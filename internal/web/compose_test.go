package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const shopFile = `services:
  web:
    image: nginx:1.27
    ports: ["8080:80"]
  db:
    image: postgres:16
    volumes: [dbdata:/var/lib/postgresql/data]
volumes:
  dbdata:
`

func TestComposeHiddenWithoutTool(t *testing.T) {
	e := newEnv(t)
	e.rt.ComposeEnabled = false
	e.login()
	if body := e.do("GET", "/", nil).Body.String(); strings.Contains(body, `href="/compose"`) {
		t.Error("nav links to compose without a compose tool")
	}
	for _, path := range []string{"/compose", "/compose/new", "/compose/shop", "/compose/shop/edit", "/partials/compose"} {
		if w := e.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
	if w := e.post("/compose", url.Values{"project": {"x"}, "yaml": {shopFile}}); w.Code != http.StatusNotFound {
		t.Errorf("POST /compose = %d, want 404", w.Code)
	}
	if body := e.do("GET", "/containers", nil).Body.String(); strings.Contains(body, `href="/compose/shop"`) {
		t.Error("container list links to compose without a compose tool")
	}
}

func TestComposePagesRender(t *testing.T) {
	e := newEnv(t)
	e.login()
	pages := map[string][]string{
		"/":                       {`href="/compose"`, "compose projects"},
		"/compose":                {"Create compose", `href="/compose/shop"`, "db, web", "running", "/srv/shop/compose.yaml"},
		"/partials/compose":       {`id="compose"`, "shop"},
		"/compose/new":            {`name="yaml"`, "Open file…", "data-compose-drop", "/static/compose.js", "Deploy"},
		"/compose/shop":           {"shop-web-1", "shop-db-1", "shop_dbdata", "shop_default", `name="return" value="/compose/shop"`, "Down"},
		"/partials/compose/shop":  {`id="compose-project"`, "shop-web-1"},
		"/partials/compose/jobs":  {`id="compose-jobs"`},
		"/containers":             {`class="tag" href="/compose/shop"`},
		"/static/compose.js":      {"data-compose-yaml"},
		"/compose/shop/edit":      {"rebuilt from the running containers", "name: shop", "image: postgres:16", "POSTGRES_PASSWORD", "readonly", "Apply changes"},
		"/partials/compose/other": {"no longer exists"},
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
	for _, path := range []string{"/compose/nope", "/compose/nope/edit", "/compose/Bad..Name"} {
		if w := e.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

func TestComposeValidate(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/compose/validate", url.Values{"yaml": {"services:\n  web:\n    image: x\n   ports: 1\n"}}, htmx)
	if body := w.Body.String(); !strings.Contains(body, `data-compose-line="4"`) || !strings.Contains(body, "did not find expected key") {
		t.Errorf("syntax error: %s", body)
	}
	w = e.post("/compose/validate", url.Values{"yaml": {shopFile}}, htmx)
	if body := w.Body.String(); !strings.Contains(body, "No syntax errors") || !strings.Contains(body, "web, db") || strings.Contains(body, "compose tool") {
		t.Errorf("valid file: %s", body)
	}
	w = e.post("/compose/validate", url.Values{"yaml": {shopFile}, "project": {"blog"}, "native": {"1"}}, htmx)
	if body := w.Body.String(); !strings.Contains(body, "The compose tool accepts the file") {
		t.Errorf("native check: %s", body)
	}
	if !slices.Contains(e.rt.Calls, "compose validate blog") {
		t.Errorf("calls = %v", e.rt.Calls)
	}
	// The temporary file is gone and left no project folder behind.
	if entries, _ := os.ReadDir(e.composeDir); len(entries) != 0 {
		t.Errorf("compose dir after check: %v", entries)
	}
	w = e.post("/compose/validate", url.Values{"yaml": {""}}, htmx)
	if !strings.Contains(w.Body.String(), "Paste a compose file") {
		t.Errorf("empty: %s", w.Body.String())
	}
}

func TestComposeCreate(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Errors re-render the form with the file intact.
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"project": {"blog"}, "yaml": {"services:\n  web: {}\n"}}, `service &#34;web&#34; needs image or build`},
		{url.Values{"project": {""}, "yaml": {shopFile}}, "enter a project name"},
		{url.Values{"project": {"Blog"}, "yaml": {shopFile}}, "project name"},
		{url.Values{"project": {"shop"}, "yaml": {shopFile}}, "project shop already exists"},
		{url.Values{"project": {"new"}, "yaml": {shopFile}}, "reserved"},
	} {
		w := e.post("/compose", tc.form)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %v = %d, want 422", tc.form, w.Code)
			continue
		}
		if body := w.Body.String(); !strings.Contains(body, tc.want) || !strings.Contains(body, "image:") {
			t.Errorf("POST %v: missing %q or the file in\n%s", tc.form, tc.want, body)
		}
	}

	w := e.post("/compose", url.Values{"yaml": {"name: blog\n" + shopFile}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/compose/blog" {
		t.Fatalf("create = %d %s\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	e.jobs.Wait()
	stored, err := os.ReadFile(filepath.Join(e.composeDir, "blog", "compose.yaml"))
	if err != nil || !strings.Contains(string(stored), "postgres:16") {
		t.Errorf("stored file = %q, %v", stored, err)
	}
	if !slices.Contains(e.rt.Calls, "compose validate blog") || !slices.Contains(e.rt.Calls, "compose up blog") {
		t.Errorf("calls = %v", e.rt.Calls)
	}
	body := e.do("GET", "/compose/blog", nil).Body.String()
	for _, want := range []string{"Deploying project blog", "blog-web-1", "blog-db-1", "blog_dbdata", "succeeded", "Container blog-web-1  Started", "stored in ContainerUI"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail after create: missing %q", want)
		}
	}
	// Editing now shows the stored file verbatim.
	if body := e.do("GET", "/compose/blog/edit", nil).Body.String(); !strings.Contains(body, "name: blog\nservices:") || strings.Contains(body, "rebuilt") {
		t.Errorf("edit stored: %s", body)
	}
}

func TestComposeUpdate(t *testing.T) {
	e := newEnv(t)
	e.login()
	// shop was deployed outside ContainerUI; applying stores the file.
	updated := strings.Replace(shopFile, "nginx:1.27", "nginx:1.28", 1)
	w := e.post("/compose/shop", url.Values{"yaml": {updated}, "edit": {"true"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/compose/shop" {
		t.Fatalf("update = %d %s\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	e.jobs.Wait()
	if b, _ := os.ReadFile(filepath.Join(e.composeDir, "shop", "compose.yaml")); string(b) != updated {
		t.Errorf("stored = %q", b)
	}
	body := e.do("GET", "/compose/shop", nil).Body.String()
	if !strings.Contains(body, "nginx:1.28") || !strings.Contains(body, "Container shop-web-1  Recreate") || !strings.Contains(body, "Container shop-db-1  Running") {
		t.Errorf("detail after update:\n%s", body)
	}

	w = e.post("/compose/shop", url.Values{"yaml": {"services: [\n"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Edit shop") {
		t.Errorf("invalid update = %d", w.Code)
	}
	if w := e.post("/compose/nope", url.Values{"yaml": {shopFile}}); w.Code != http.StatusNotFound {
		t.Errorf("update unknown project = %d", w.Code)
	}
}

func TestComposeActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/compose/shop/stop", url.Values{"view": {"list"}}, htmx)
	if w.Code != http.StatusOK || w.Header().Get("HX-Trigger") != "compose-job-started" || !strings.Contains(w.Body.String(), "Stopping project shop") {
		t.Errorf("stop = %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
	e.jobs.Wait()
	if body := e.do("GET", "/partials/compose", nil).Body.String(); !strings.Contains(body, "stopped") {
		t.Errorf("after stop: %s", body)
	}
	// Without a stored file the project is addressed by name only, and
	// pulling (which needs the file) is refused.
	w = e.post("/compose/shop/pull", url.Values{"view": {"list"}}, htmx)
	if !strings.Contains(w.Body.String(), "pulling needs the compose file") {
		t.Errorf("pull without file: %s", w.Body.String())
	}

	w = e.post("/compose/shop/down", url.Values{"view": {"detail"}, "volumes": {"true"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/compose/shop" {
		t.Errorf("down redirect = %v", w.Header())
	}
	e.jobs.Wait()
	want := []string{"compose stop shop", "compose down shop volumes=true"}
	var got []string
	for _, c := range e.rt.Calls {
		if strings.HasPrefix(c, "compose ") {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("compose calls = %v, want %v", got, want)
	}
	if w := e.do("GET", "/compose/shop", nil); w.Code != http.StatusNotFound {
		t.Errorf("project after down = %d", w.Code)
	}
	if w := e.post("/compose/shop/explode", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown action = %d", w.Code)
	}
}

func TestComposeStoredNotDeployed(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.composeDir, "blog")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(shopFile), 0o600); err != nil {
		t.Fatal(err)
	}
	body := e.do("GET", "/compose", nil).Body.String()
	if !strings.Contains(body, "not deployed") || !strings.Contains(body, `action="/compose/blog/up"`) {
		t.Errorf("stored project not listed: %s", body)
	}
	w := e.post("/compose/blog/up", url.Values{"view": {"list"}}, htmx)
	if !strings.Contains(w.Body.String(), "Deploying project blog") {
		t.Errorf("deploy: %s", w.Body.String())
	}
	e.jobs.Wait()
	if !slices.Contains(e.rt.Calls, "compose up blog") {
		t.Errorf("calls = %v", e.rt.Calls)
	}
	w = e.post("/compose/blog/delete", url.Values{"view": {"detail"}})
	if w.Code != http.StatusSeeOther {
		t.Errorf("delete = %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); !os.IsNotExist(err) {
		t.Errorf("stored file still there: %v", err)
	}
}

func TestContainerActionReturnsToCompose(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/containers/shop-web-1/stop", url.Values{"view": {"detail"}, "return": {"/compose/shop"}}, htmx)
	if got := w.Header().Get("HX-Redirect"); got != "/compose/shop" {
		t.Errorf("redirect = %q", got)
	}
	w = e.post("/containers/shop-web-1/start", url.Values{"view": {"detail"}, "return": {"https://evil.example/"}}, htmx)
	if got := w.Header().Get("HX-Redirect"); got != "/containers/shop-web-1" {
		t.Errorf("foreign return = %q", got)
	}
}

var drawerClosedRe = regexp.MustCompile(`<aside id="compose-output"[^>]* hidden>`)

func TestComposeOutputDrawer(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Closed on load, with a toggle; nothing to show yet.
	for _, path := range []string{"/compose", "/compose/shop"} {
		body := e.do("GET", path, nil).Body.String()
		if !drawerClosedRe.MatchString(body) || !strings.Contains(body, "data-output-toggle") || !strings.Contains(body, "No compose output.") {
			t.Errorf("GET %s: drawer not closed/empty:\n%s", path, body)
		}
		if strings.Contains(body, "hx-swap-oob") {
			t.Errorf("GET %s: out-of-band badge on a full page", path)
		}
	}

	// An htmx action on the list opens the drawer in the browser (event);
	// the next page load still starts closed.
	e.post("/compose/shop/stop", url.Values{"view": {"list"}}, htmx)
	e.jobs.Wait()
	part := e.do("GET", "/partials/compose/jobs", nil, htmx).Body.String()
	for _, want := range []string{"shop stop", "succeeded", "by admin", "took ", `id="compose-output-state" hx-swap-oob="true"`} {
		if !strings.Contains(part, want) {
			t.Errorf("jobs partial missing %q:\n%s", want, part)
		}
	}
	if body := e.do("GET", "/compose", nil).Body.String(); !drawerClosedRe.MatchString(body) || !strings.Contains(body, "shop stop") {
		t.Error("list page after an htmx action should start closed and show the output")
	}

	// A detail-view action redirects; the reload opens the drawer once and
	// shows only the newest action.
	e.post("/compose/shop/start", url.Values{"view": {"detail"}}, htmx)
	e.jobs.Wait()
	body := e.do("GET", "/compose/shop", nil).Body.String()
	if drawerClosedRe.MatchString(body) || !strings.Contains(body, "shop start") || strings.Contains(body, "shop stop") {
		t.Errorf("detail after action: drawer not open with the newest job only:\n%s", body)
	}
	if body := e.do("GET", "/compose/shop", nil).Body.String(); !drawerClosedRe.MatchString(body) {
		t.Error("drawer should open only once after the action")
	}

	// Clear needs the CSRF token and hides the output for this session only.
	if w := e.do("POST", "/compose/jobs/clear", url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("clear without csrf = %d", w.Code)
	}
	w := e.post("/compose/jobs/clear", url.Values{"project": {"shop"}}, htmx)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No compose output.") {
		t.Errorf("clear = %d\n%s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/partials/compose/jobs", "/partials/compose/jobs?project=shop"} {
		if p := e.do("GET", path, nil, htmx).Body.String(); !strings.Contains(p, "No compose output.") {
			t.Errorf("%s after clear shows an older job:\n%s", path, p)
		}
	}
	mine, myCSRF := e.cookie, e.csrf
	e.cookie = nil
	e.login() // another session still sees it
	if p := e.do("GET", "/partials/compose/jobs", nil, htmx).Body.String(); !strings.Contains(p, "shop start") {
		t.Errorf("other session lost the output:\n%s", p)
	}
	e.cookie, e.csrf = mine, myCSRF

	// A newer action shows again.
	e.post("/compose/shop/restart", url.Values{"view": {"list"}}, htmx)
	e.jobs.Wait()
	if p := e.do("GET", "/partials/compose/jobs?project=shop", nil, htmx).Body.String(); !strings.Contains(p, "shop restart") {
		t.Errorf("new action after clear not shown:\n%s", p)
	}
	if w := e.post("/compose/jobs/clear", url.Values{"project": {"Bad Name"}}, htmx); w.Code != http.StatusNotFound {
		t.Errorf("clear with invalid project = %d", w.Code)
	}
}
