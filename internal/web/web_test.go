package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/fake"
)

type env struct {
	t    *testing.T
	h    http.Handler
	rt   *fake.Runtime
	jobs *jobs.Manager
	logs *syncBuffer
	// composeDir holds the compose files deployed in the test.
	composeDir string

	cookie *http.Cookie
	csrf   string
}

// syncBuffer is a bytes.Buffer safe for concurrent use: handlers served by a
// real httptest.Server log from their own goroutines while tests read the logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with options changed by opt before the server is made.
func newEnvWith(t *testing.T, opt func(*Options)) *env {
	t.Helper()
	users, _, err := auth.NewUsers([]config.User{{Username: "admin", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	rt := fake.NewSeeded()
	jm := jobs.NewManager(context.Background())
	composeDir := t.TempDir()
	o := Options{
		Runtime: rt, Users: users, Sessions: auth.NewSessions(time.Hour, time.Hour), Jobs: jm, ComposeDir: composeDir,
		WarmInterval: -1, // deterministic: no background warm-ups
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		HostLog:      &fakeSysLog{},
		HostInfo:     fakeHostInfo,
	}
	if opt != nil {
		opt(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, h: s.Handler(), rt: rt, jobs: jm, logs: logs, composeDir: composeDir}
	t.Cleanup(func() {
		if strings.Contains(logs.String(), "render") {
			t.Errorf("template errors logged:\n%s", logs.String())
		}
	})
	return e
}

type reqOpt func(*http.Request)

func htmx(r *http.Request) { r.Header.Set("HX-Request", "true") }

func (e *env) do(method, path string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if e.cookie != nil {
		r.AddCookie(e.cookie)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// post sends a form with the session's CSRF token as htmx would.
func (e *env) post(path string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	return e.do("POST", path, form, append([]reqOpt{func(r *http.Request) { r.Header.Set("X-CSRF-Token", e.csrf) }}, opts...)...)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) login() {
	e.t.Helper()
	w := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		e.t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			e.cookie = c
		}
	}
	if e.cookie == nil || !e.cookie.HttpOnly || e.cookie.SameSite != http.SameSiteStrictMode {
		e.t.Fatalf("session cookie = %+v", e.cookie)
	}
	m := csrfRe.FindStringSubmatch(e.do("GET", "/", nil).Body.String())
	if m == nil {
		e.t.Fatal("no csrf token on dashboard")
	}
	e.csrf = m[1]
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "/containers", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("GET /containers = %d %s", w.Code, w.Header().Get("Location"))
	}
	w = e.do("GET", "/partials/containers", nil, htmx)
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx partial = %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/static/htmx.min.js", "/static/app.js", "/static/app.css", "/static/theme.js"} {
		if w := e.do("GET", p, nil); w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
	if w := e.do("GET", "/favicon.ico", nil); w.Code != http.StatusNoContent {
		t.Errorf("favicon = %d", w.Code)
	}
	if w := e.do("GET", "/login", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="password"`) {
		t.Errorf("login page = %d", w.Code)
	}
}

func TestLoginLogout(t *testing.T) {
	e := newEnv(t)
	w := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Invalid username or password") {
		t.Fatalf("bad login = %d", w.Code)
	}
	e.login()
	w = e.do("GET", "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if w := e.post("/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := e.do("GET", "/", nil); w.Code != http.StatusSeeOther {
		t.Errorf("after logout = %d; session still valid", w.Code)
	}
}

func TestLoginThrottle(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	}
	w := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Too many failed attempts") {
		t.Errorf("throttled login = %d", w.Code)
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	e := newEnv(t)
	e.login()
	if w := e.do("POST", "/containers/c0ffee000001/stop", url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("missing csrf = %d", w.Code)
	}
	if w := e.do("POST", "/containers/c0ffee000001/stop", url.Values{"csrf": {"nope"}}); w.Code != http.StatusForbidden {
		t.Errorf("wrong csrf = %d", w.Code)
	}
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	if w := e.post("/containers/c0ffee000001/stop", nil, evil); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin = %d", w.Code)
	}
	if len(e.rt.Calls) != 0 {
		t.Errorf("runtime called despite rejection: %v", e.rt.Calls)
	}
	// Form-field token (non-JS fallback) is accepted.
	w := e.do("POST", "/containers/c0ffee000001/stop", url.Values{"csrf": {e.csrf}})
	if w.Code != http.StatusSeeOther || !slices.Contains(e.rt.Calls, "stop c0ffee000001") {
		t.Errorf("form csrf = %d, calls %v", w.Code, e.rt.Calls)
	}
}

func TestPagesRender(t *testing.T) {
	e := newEnv(t)
	e.login()
	pages := map[string][]string{
		"/":                        {"Running Containers", "web", "nginx", "data", "bridge"},
		"/partials/dashboard":      {`id="dashboard"`},
		"/containers":              {"web", "job", "Force delete", "Start"},
		"/partials/containers":     {`id="containers"`},
		"/containers/new":          {`name="image"`, "alpine:latest", "bridge"},
		"/containers/new?image=x":  {`value="x"`},
		"/containers/c0ffee000001": {"Inspect", "nginx:1.27", "Restart"},
		"/images":                  {"Pull an Image", "alpine", "nginx", "Tag…", `class="in-use"`, "Warning: this image is in use"},
		"/partials/images":         {`id="images"`},
		"/partials/pulls":          {`id="pulls"`},
		"/volumes":                 {"Create a Volume", "Prune volumes", "cache", `href="/volumes/data"`, `href="/containers/web"`, "Force delete"},
		"/partials/volumes":        {`id="volumes"`, "cache"},
		"/volumes/data":            {"Inspect", "/var/lib/docker/volumes/data/_data", "web", "Force delete"},
	}
	for path, want := range pages {
		w := e.do("GET", path, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)
			continue
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("GET %s: missing %q", path, s)
			}
		}
	}
	if w := e.do("GET", "/containers/doesnotexist", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown container = %d", w.Code)
	}
	if w := e.do("GET", "/volumes/doesnotexist", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown volume = %d", w.Code)
	}
}

func TestRuntimeErrorShown(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.Err = &runtime.CommandError{Op: "container list", Code: "WSLC_E_SERVICE_UNAVAILABLE", Output: "service down"}
	w := e.do("GET", "/", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "service down [WSLC_E_SERVICE_UNAVAILABLE]") {
		t.Errorf("dashboard with runtime error = %d\n%s", w.Code, w.Body.String())
	}
}

func TestContainerActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/containers/c0ffee000001/stop", url.Values{"name": {"web"}, "view": {"list"}}, htmx)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Container web stopped.") || !strings.Contains(w.Body.String(), `id="containers"`) {
		t.Fatalf("stop = %d\n%s", w.Code, w.Body.String())
	}
	w = e.post("/containers/c0ffee000001/restart", url.Values{"name": {"web"}, "view": {"list"}}, htmx)
	if !strings.Contains(w.Body.String(), "Container web restarted.") {
		t.Errorf("restart: %s", w.Body.String())
	}
	// Deleting a running container without force reports a conflict.
	w = e.post("/containers/c0ffee000001/delete", url.Values{"name": {"web"}, "view": {"list"}}, htmx)
	if !strings.Contains(w.Body.String(), "Force delete") || !strings.Contains(w.Body.String(), "flash error") {
		t.Errorf("delete running: %s", w.Body.String())
	}
	// Detail view redirects back to the detail page (or the list after delete).
	w = e.post("/containers/c0ffee000001/delete", url.Values{"name": {"web"}, "view": {"detail"}, "force": {"true"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/containers" {
		t.Errorf("detail delete redirect = %q", w.Header().Get("HX-Redirect"))
	}
	if w := e.do("GET", "/containers", nil); !strings.Contains(w.Body.String(), "Container web deleted.") {
		t.Error("flash not shown after redirect")
	}
	if w := e.post("/containers/c0ffee000002/explode", nil, htmx); w.Code != http.StatusNotFound {
		t.Errorf("unknown action = %d", w.Code)
	}
	want := []string{"stop c0ffee000001", "restart c0ffee000001", "remove c0ffee000001 force=false", "remove c0ffee000001 force=true"}
	if !reflect.DeepEqual(e.rt.Calls, want) {
		t.Errorf("calls = %v", e.rt.Calls)
	}
}

func TestCreateContainer(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{
		"image": {"alpine:latest"}, "name": {"new1"}, "cmd": {`sh -c "echo hi"`},
		"ports": {"8081:80, 127.0.0.1:9000:9000/udp"}, "env": {"A=1\nB=2"},
		"volumes": {"data:/data"}, "network": {"bridge"}, "pull": {"missing"}, "start": {"on"},
	}
	w := e.post("/containers", form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/containers" {
		t.Fatalf("create = %d\n%s", w.Code, w.Body.String())
	}
	cs, _ := e.rt.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	var c *runtime.Container
	for i := range cs {
		if cs[i].Name == "new1" {
			c = &cs[i]
		}
	}
	if c == nil || c.State != runtime.StateRunning || c.Command != "sh -c echo hi" {
		t.Fatalf("created container = %+v", c)
	}
	if !strings.Contains(e.do("GET", "/containers", nil).Body.String(), "Container new1 created and started.") {
		t.Error("flash missing")
	}

	form.Set("ports", "notaport")
	form.Set("name", "new2")
	w = e.post("/containers", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid port") || strings.Contains(w.Body.String(), "invalid argument:") || !strings.Contains(w.Body.String(), `value="new2"`) {
		t.Errorf("invalid create = %d\n%s", w.Code, w.Body.String())
	}

	form.Set("ports", "")
	form.Set("name", "new1")
	w = e.post("/containers", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "already in use") {
		t.Errorf("duplicate create = %d", w.Code)
	}
}

func TestImageActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/images/pull", url.Values{"ref": {"busybox:1.36"}}, htmx)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "busybox:1.36") {
		t.Fatalf("pull = %d\n%s", w.Code, w.Body.String())
	}
	e.jobs.Wait()
	w = e.do("GET", "/partials/pulls", nil, htmx)
	if !strings.Contains(w.Body.String(), "succeeded") || w.Header().Get("HX-Trigger") != "images-changed" || strings.Contains(w.Body.String(), "every 1s") {
		t.Errorf("pulls after completion = %s / %v", w.Body.String(), w.Header())
	}
	if !strings.Contains(e.do("GET", "/partials/images", nil).Body.String(), "busybox") {
		t.Error("pulled image not listed")
	}

	w = e.post("/images/pull", url.Values{"ref": {"--all-tags"}}, htmx)
	if !strings.Contains(w.Body.String(), "invalid image reference") {
		t.Errorf("invalid pull: %s", w.Body.String())
	}

	w = e.post("/images/tag", url.Values{"source": {"alpine:latest"}, "target": {"alpine:mine"}}, htmx)
	if !strings.Contains(w.Body.String(), "Tagged alpine:latest as alpine:mine.") || !strings.Contains(w.Body.String(), "mine") {
		t.Errorf("tag: %s", w.Body.String())
	}

	w = e.post("/images/delete", url.Values{"ref": {"nginx:1.27"}}, htmx)
	if !strings.Contains(w.Body.String(), "used by a container") {
		t.Errorf("delete in-use image: %s", w.Body.String())
	}
	w = e.post("/images/delete", url.Values{"ref": {"alpine:mine"}}, htmx)
	if !strings.Contains(w.Body.String(), "Deleted image alpine:mine.") {
		t.Errorf("delete: %s", w.Body.String())
	}
}

func TestSplitArgs(t *testing.T) {
	cases := map[string][]string{
		``:                         nil,
		`echo hi`:                  {"echo", "hi"},
		`sh -c "echo hello world"`: {"sh", "-c", "echo hello world"},
		`a 'b c' "d\"e" f\ g`:      {"a", "b c", `d"e`, "f g"},
		`""`:                       {""},
		"  spaced\t out  ":         {"spaced", "out"},
		`nginx -g 'daemon off;'`:   {"nginx", "-g", "daemon off;"},
	}
	for in, want := range cases {
		got, err := splitArgs(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitArgs(`echo "oops`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestVolumeActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/volumes", url.Values{"name": {"logs"}, "driver": {"vhd"}, "labels": {"app=web\n"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/volumes" {
		t.Fatalf("create = %d\n%s", w.Code, w.Body.String())
	}
	if body := e.do("GET", "/volumes", nil).Body.String(); !strings.Contains(body, "Volume logs created.") || !strings.Contains(body, "vhd") {
		t.Errorf("created volume not shown:\n%s", body)
	}
	w = e.post("/volumes", url.Values{"name": {"bad name"}, "options": {"size=1G"}})
	if body := w.Body.String(); w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "volume name") || !strings.Contains(body, `value="bad name"`) || !strings.Contains(body, "size=1G") {
		t.Errorf("invalid create = %d\n%s", w.Code, body)
	}

	// "data" is mounted by the web container: neither delete nor force delete removes it.
	for _, force := range []string{"", "true"} {
		w = e.post("/volumes/delete", url.Values{"name": {"data"}, "view": {"list"}, "force": {force}}, htmx)
		if !strings.Contains(w.Body.String(), "delete the container first") || !strings.Contains(w.Body.String(), `id="volumes"`) {
			t.Errorf("delete in-use (force=%q): %s", force, w.Body.String())
		}
	}
	w = e.post("/volumes/delete", url.Values{"name": {"logs"}, "view": {"list"}}, htmx)
	if !strings.Contains(w.Body.String(), "Deleted volume logs.") {
		t.Errorf("delete: %s", w.Body.String())
	}
	// From the detail page a failed delete returns to the detail page.
	w = e.post("/volumes/delete", url.Values{"name": {"data"}, "view": {"detail"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/volumes/data" {
		t.Errorf("detail delete redirect = %q", w.Header().Get("HX-Redirect"))
	}

	// Without "all" prune keeps the unused named volume "cache".
	w = e.post("/volumes/prune", url.Values{}, htmx)
	if body := w.Body.String(); !strings.Contains(body, "Pruned unused volumes.") || !strings.Contains(body, "cache") {
		t.Errorf("prune: %s", body)
	}
	w = e.post("/volumes/prune", url.Values{"all": {"on"}}, htmx)
	if body := w.Body.String(); !strings.Contains(body, "Deleted Volumes: cache") || !strings.Contains(body, `href="/volumes/data"`) {
		t.Errorf("prune all: %s", body)
	}

	want := []string{"create-volume logs", "rmv data force=false", "rmv data force=true", "rmv logs force=false", "rmv data force=false", "prune-volumes all=false", "prune-volumes all=true"}
	if !reflect.DeepEqual(e.rt.Calls, want) {
		t.Errorf("calls = %v", e.rt.Calls)
	}
}
