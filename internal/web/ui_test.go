package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/fake"
)

// listCounter counts ListContainers calls, the call that starts a cold
// wslc session.
type listCounter struct {
	*fake.Runtime
	n atomic.Int32
}

func (c *listCounter) ListContainers(ctx context.Context, o runtime.ListContainersOpts) ([]runtime.Container, error) {
	c.n.Add(1)
	return c.Runtime.ListContainers(ctx, o)
}

func newTestServer(t *testing.T, rt runtime.Runtime, warm time.Duration) *Server {
	t.Helper()
	users, _, err := auth.NewUsers([]config.User{{Username: "admin", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Runtime: rt, Users: users, Sessions: auth.NewSessions(time.Hour, time.Hour),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), WarmInterval: warm,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestLoginPageWarmsRuntimeOncePerInterval(t *testing.T) {
	rt := &listCounter{Runtime: fake.NewSeeded()}
	s := newTestServer(t, rt, time.Hour)
	h := s.Handler()
	for range 3 {
		if w := get(h, "/login"); w.Code != http.StatusOK {
			t.Fatalf("GET /login = %d", w.Code)
		}
		s.warm.wait()
	}
	if n := rt.n.Load(); n != 1 {
		t.Errorf("warm-ups = %d, want 1 within the interval", n)
	}

	s.warm.mu.Lock()
	s.warm.last = time.Now().Add(-2 * time.Hour) // interval elapsed
	s.warm.mu.Unlock()
	get(h, "/login")
	s.warm.wait()
	if n := rt.n.Load(); n != 2 {
		t.Errorf("warm-ups after interval = %d, want 2", n)
	}
}

func TestWarmerDisabled(t *testing.T) {
	rt := &listCounter{Runtime: fake.NewSeeded()}
	s := newTestServer(t, rt, -1)
	get(s.Handler(), "/login")
	s.warm.wait()
	if n := rt.n.Load(); n != 0 {
		t.Errorf("warm-ups = %d with warming disabled", n)
	}
}

func TestFavicon(t *testing.T) {
	s := newTestServer(t, fake.NewSeeded(), -1)
	if s.favicon != "" {
		t.Skipf("a favicon is embedded (%s); the no-favicon case is not testable", s.favicon)
	}
	if w := get(s.Handler(), "/favicon.ico"); w.Code != http.StatusNoContent {
		t.Errorf("no favicon: /favicon.ico = %d, want 204", w.Code)
	}
	if body := get(s.Handler(), "/login").Body.String(); !strings.Contains(body, `<link rel="icon" href="data:,">`) {
		t.Error("no favicon: page should declare an empty icon")
	}

	s.favicon, s.favType = "/static/favicon.png", "image/png"
	h := s.Handler()
	w := get(h, "/favicon.ico")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/static/favicon.png" {
		t.Errorf("favicon redirect = %d %q", w.Code, w.Header().Get("Location"))
	}
	if body := get(h, "/login").Body.String(); !strings.Contains(body, `<link rel="icon" href="/static/favicon.png" type="image/png">`) {
		t.Error("page does not link the favicon")
	}
}

func TestLayoutThemeAndBrand(t *testing.T) {
	e := newEnv(t)
	login := e.do("GET", "/login", nil).Body.String()
	if !strings.Contains(login, "data-theme-toggle") {
		t.Error("login page has no theme toggle")
	}
	e.login()
	page := e.do("GET", "/", nil).Body.String()
	for _, want := range []string{`<span class="brand">ContainerUI</span>`, "data-theme-toggle"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(page, `<a class="brand"`) {
		t.Error("brand text must not be a link")
	}
	// theme.js must load synchronously and before the stylesheet's first paint.
	theme := strings.Index(page, `<script src="/static/theme.js"></script>`)
	css := strings.Index(page, `href="/static/app.css"`)
	if theme < 0 || css < 0 || theme > css {
		t.Errorf("theme.js (at %d) must be a blocking script before app.css (at %d)", theme, css)
	}
}

func TestCreateFormLayout(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/containers/new/scratch", nil).Body.String()
	// The required marker must sit inside the label text, not as a flex
	// sibling of the input (the misalignment from the screenshot).
	if !strings.Contains(body, `<span class="field-label">Image <span class="req" aria-hidden="true">*</span></span>`) {
		t.Error("image label markup changed")
	}
	for _, want := range []string{"<legend>Networking</legend>", `class="form-actions"`, `class="switch"`, "hostPort:containerPort"} {
		if !strings.Contains(body, want) {
			t.Errorf("create form missing %q", want)
		}
	}
}

func TestWarmEndpoint(t *testing.T) {
	rt := &listCounter{Runtime: fake.NewSeeded()}
	s := newTestServer(t, rt, time.Hour)
	h := s.Handler()
	for range 2 {
		if w := get(h, "/login/warm"); w.Code != http.StatusNoContent {
			t.Fatalf("GET /login/warm = %d, want 204 without a session", w.Code)
		}
		s.warm.wait()
	}
	if n := rt.n.Load(); n != 1 {
		t.Errorf("warm-ups = %d, want 1 (rate limited)", n)
	}
	if body := get(h, "/login").Body.String(); !strings.Contains(body, "data-keep-warm") {
		t.Error("login page does not opt into keep-warm pings")
	}
}

// List pages show no visible title or sub line, only their buttons; the h1
// stays for screen readers. Detail pages keep the object's name.
func TestPageHeaders(t *testing.T) {
	e := newEnv(t)
	e.login()
	for path, title := range map[string]string{
		"/": "Dashboard", "/containers": "Containers", "/images": "Images", "/volumes": "Volumes",
		"/networks": "Networks", "/compose": "Compose", "/system": "System",
	} {
		body := e.do("GET", path, nil).Body.String()
		if !strings.Contains(body, `<h1 class="sr-only">`+title+`</h1>`) {
			t.Errorf("GET %s: no screen-reader title %q", path, title)
		}
		if n := strings.Count(body, "<h1"); n != 1 {
			t.Errorf("GET %s: %d h1 elements, want 1", path, n)
		}
	}
	// Each list page has its actions top right, Refresh always last; the
	// tables carry no title or buttons of their own.
	for path, labels := range map[string][]string{
		"/":           {"Refresh"},
		"/containers": {"Create container", "Refresh"},
		"/compose":    {"Create compose", "Refresh"},
		"/images":     {"Pull image", "Refresh"},
		"/volumes":    {"Prune volumes", "Create volume", "Refresh"},
		"/networks":   {"Prune networks", "Create network", "Refresh"},
	} {
		body := e.do("GET", path, nil).Body.String()
		checkHeadActions(t, path, body, labels)
		if path == "/" && strings.Contains(body, "Create container") {
			t.Error("GET /: the dashboard still offers Create container")
		}
		if path != "/" && strings.Contains(body, `class="panel-head"`) {
			t.Errorf("GET %s: the table still has its own head", path)
		}
	}
	for _, gone := range []string{"Projects deployed with", "how it is installed and the host"} {
		for _, path := range []string{"/compose", "/system"} {
			if strings.Contains(e.do("GET", path, nil).Body.String(), gone) {
				t.Errorf("GET %s still shows %q", path, gone)
			}
		}
	}
	if body := e.do("GET", "/compose/shop", nil).Body.String(); !strings.Contains(body, `<h1 class="wrap">shop</h1>`) {
		t.Error("compose detail lost its name")
	}
}

// Sub-pages have a Back button left of their title, in one row.
func TestBackButtons(t *testing.T) {
	e := newEnv(t)
	e.login()
	for path, back := range map[string]string{
		"/images/pull":            "/images",
		"/containers/new":         "/containers",
		"/containers/new/scratch": "/containers/new",
		"/containers/new/cli":     "/containers/new",
		"/containers/web/edit":    "/containers/c0ffee000001",
		"/containers/web":         "/containers",
		"/volumes/new":            "/volumes",
		"/volumes/data":           "/volumes",
		"/networks/new":           "/networks",
		"/networks/backend":       "/networks",
		"/compose/new":            "/compose",
		"/compose/shop":           "/compose",
		"/compose/shop/edit":      "/compose/shop",
	} {
		body := e.do("GET", path, nil).Body.String()
		want := `<div class="title-row"><a class="button" href="` + back + `">Back</a>`
		if i := strings.Index(body, `<div class="title-row">`); i < 0 || !strings.HasPrefix(strings.Join(strings.Fields(body[i:]), ""), strings.Join(strings.Fields(want), "")) {
			t.Errorf("GET %s: no Back button to %s left of the title", path, back)
		}
		if strings.Contains(body, `class="back"`) || strings.Contains(body, "Back to containers") {
			t.Errorf("GET %s: still has the old back link", path)
		}
	}
}

// Table rows offer their actions in a ⋮ menu; volume and network detail
// pages keep buttons, the container detail page has no actions.
func TestRowMenus(t *testing.T) {
	e := newEnv(t)
	e.login()
	for path, items := range map[string][]string{
		"/partials/containers": {`data-menu="c-c0ffee000001"`, `aria-label="Actions for web"`, ">Stop<", ">Restart<", ">Recreate<", ">Connect<", ">Delete<"},
		"/partials/images":     {`data-menu="i-nginx:1.27"`, "Run…", "Tag…", `<details class="row-menu-sub">`, ">Delete<"},
		"/partials/volumes":    {`data-menu="v-data"`, ">Delete<", ">Force delete<"},
		"/partials/networks":   {`data-menu="n-backend"`, ">Delete<"},
		"/partials/compose":    {`data-menu="p-shop"`, ">Edit<"},
		"/compose/shop":        {`<details class="row-menu"`, ">Stop<"},
		"/networks/bridge":     {`<details class="row-menu"`, ">Disconnect<"},
	} {
		body := e.do("GET", path, nil, htmx).Body.String()
		i := strings.Index(body, `<details class="row-menu"`)
		if i < 0 {
			t.Errorf("GET %s: no row menu", path)
			continue
		}
		for _, want := range items {
			if !strings.Contains(body[i:], want) && !strings.Contains(body, want) {
				t.Errorf("GET %s: row menu misses %q", path, want)
			}
		}
		if !strings.Contains(body, `class="row-menu-panel"`) || !strings.Contains(body, `class="button kebab"`) {
			t.Errorf("GET %s: row menu without ⋮ button or panel", path)
		}
	}
	if body := e.do("GET", "/containers/web", nil).Body.String(); strings.Contains(body, `class="row-menu"`) || strings.Contains(body, `<div class="actions">`) || strings.Contains(body, `hx-post="/containers/`) {
		t.Error("GET /containers/web: the detail page offers actions")
	}
	for _, path := range []string{"/volumes/data", "/networks/backend"} {
		body := e.do("GET", path, nil).Body.String()
		i := strings.Index(body, `<section class="panel">`)
		j := strings.Index(body[max(i, 0):], "</section>")
		if i < 0 || j < 0 || strings.Contains(body[i:i+j], `class="row-menu"`) || !strings.Contains(body[i:i+j], `<div class="actions">`) {
			t.Errorf("GET %s: the detail panel's actions are not plain buttons", path)
		}
	}
}

// checkHeadActions checks that the page head holds the buttons labels in
// order and nothing after the last one.
func checkHeadActions(t *testing.T, path, body string, labels []string) {
	t.Helper()
	start := strings.Index(body, `<div class="page-head">`)
	end := strings.Index(body, "<section")
	if start < 0 || end < start {
		t.Errorf("GET %s: no page head before the table", path)
		return
	}
	head := body[start:end]
	at := 0
	for _, l := range labels {
		i := strings.Index(head[at:], ">"+l+"<")
		if i < 0 {
			t.Errorf("GET %s: page head misses %q after the previous button:\n%s", path, l, head)
			return
		}
		at += i + len(l) + 2
	}
	if rest := head[at:]; strings.Contains(rest, "<button") || strings.Contains(rest, `class="button`) {
		t.Errorf("GET %s: a button follows %q:\n%s", path, labels[len(labels)-1], rest)
	}
}

// Empty lists keep the table headers and show the hint in the body.
func TestEmptyTables(t *testing.T) {
	e := newEnv(t)
	e.login()
	ctx := context.Background()
	cs, _ := e.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	for _, c := range cs {
		if err := e.rt.RemoveContainer(ctx, c.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	imgs, _ := e.rt.ListImages(ctx)
	for _, img := range imgs {
		if err := e.rt.RemoveImage(ctx, img.Ref(), true); err != nil {
			t.Fatal(err)
		}
	}
	vols, _ := e.rt.ListVolumes(ctx)
	for _, v := range vols {
		if err := e.rt.RemoveVolume(ctx, v.Name, true); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		"/partials/dashboard":  `class="empty">No containers are running.<`,
		"/partials/containers": `<td colspan="6" class="empty">No containers yet. Use <em>Create container</em> to add one.`,
		"/partials/images":     `<td colspan="6" class="empty">No images yet. Use <em>Pull image</em> to get one.`,
		"/partials/volumes":    `<td colspan="6" class="empty">No volumes yet. Use <em>Create volume</em> to add one.`,
	} {
		body := e.do("GET", path, nil, htmx).Body.String()
		if !strings.Contains(body, "<thead>") || !strings.Contains(body, want) {
			t.Errorf("GET %s: want headers and %q:\n%s", path, want, body)
		}
	}

	// A failed listing shows the error above the still headed table.
	e.rt.SetErr(io.ErrUnexpectedEOF)
	for path, want := range map[string]string{
		"/partials/containers": "Could not load containers.",
		"/partials/images":     "Could not load images.",
		"/partials/volumes":    "Could not load volumes.",
		"/partials/networks":   "Could not load networks.",
		"/partials/compose":    "Could not load compose projects.",
	} {
		body := e.do("GET", path, nil, htmx).Body.String()
		if !strings.Contains(body, "<thead>") || !strings.Contains(body, `class="flash error"`) || !strings.Contains(body, want) {
			t.Errorf("GET %s with a runtime error: want headers, flash and %q:\n%s", path, want, body)
		}
	}
}
