package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/jobs"
)

// activityTitle marks a page that renders the activity side bar.
const activityTitle = `<h2 id="activity-title">Activity</h2>`

// activity returns the side bar's list.
func (e *env) activity() string {
	e.t.Helper()
	w := e.do("GET", "/partials/activity", nil, htmx)
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET activity = %d", w.Code)
	}
	return w.Body.String()
}

// drawerOpen reports whether a page renders the side bar open.
func drawerOpen(body string) bool {
	i := strings.Index(body, `<aside id="activity"`)
	if i < 0 {
		return false
	}
	tag := body[i : i+strings.Index(body[i:], ">")]
	return !strings.Contains(tag, " hidden")
}

func TestActivityOnPages(t *testing.T) {
	e := newEnv(t)
	e.login()
	with := []string{
		"/compose", "/compose/shop", "/compose/new",
		"/containers", "/containers/new", "/containers/new/scratch", "/containers/web",
		"/images", "/images/pull",
		"/volumes", "/volumes/new", "/volumes/data",
		"/networks", "/networks/new", "/networks/bridge",
	}
	for _, path := range with {
		w := e.do("GET", path, nil)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, activityTitle) || !strings.Contains(body, "/static/drawer.js") ||
			strings.Count(body, `<aside id="activity"`) != 1 {
			t.Errorf("GET %s = %d: no single side bar", path, w.Code)
		}
		if strings.Contains(body, "hx-swap-oob") && strings.Contains(body, `id="activity-state" hx-swap-oob`) {
			t.Errorf("GET %s: out-of-band badge on a full page", path)
		}
	}
	for _, path := range []string{"/", "/system", "/system/licenses", "/events"} {
		w := e.do("GET", path, nil)
		if w.Code == http.StatusNotFound {
			continue // no Events page without a journal
		}
		if body := w.Body.String(); strings.Contains(body, `id="activity"`) || strings.Contains(body, "/static/drawer.js") {
			t.Errorf("GET %s shows the side bar", path)
		}
	}
	if w := e.do("GET", "/partials/activity/images", nil, htmx); w.Code != http.StatusNotFound {
		t.Errorf("old per-tab route = %d", w.Code)
	}
}

func TestActivityShared(t *testing.T) {
	e := newEnv(t)
	e.login()
	if a := e.activity(); !strings.Contains(a, "No recent activity.") || !strings.Contains(a, `id="activity-state" hx-swap-oob="true"`) {
		t.Errorf("empty side bar = %s", a)
	}

	// An htmx action reloads and opens the side bar in place.
	w := e.post("/volumes/prune", nil, htmx)
	if w.Header().Get("HX-Trigger") != "activity-changed" {
		t.Errorf("prune trigger = %v", w.Header())
	}
	if a := e.activity(); !strings.Contains(a, "Pruned unused volumes.") || !strings.Contains(a, "status-dot succeeded") ||
		!strings.Contains(a, "admin") || !strings.Contains(a, `<span class="job-kind">Volume</span>`) {
		t.Errorf("after prune: %s", a)
	}
	if drawerOpen(e.do("GET", "/volumes", nil).Body.String()) {
		t.Error("an htmx action opens the side bar on the next page too")
	}
	// Every page shows the same list.
	e.post("/networks/prune", nil, htmx)
	for _, path := range []string{"/containers", "/images", "/compose/shop"} {
		body := e.do("GET", path, nil).Body.String()
		if !strings.Contains(body, "Pruned unused volumes.") || !strings.Contains(body, "Pruned unused networks.") {
			t.Errorf("GET %s misses an entry", path)
		}
	}

	// Clear hides the entries for this user until something new happens.
	w = e.post("/activity/clear", nil, htmx)
	if !strings.Contains(w.Body.String(), "No recent activity.") {
		t.Errorf("clear: %s", w.Body.String())
	}
	mine, myCSRF := e.cookie, e.csrf
	e.cookie = nil
	e.login() // another session still sees them
	if a := e.activity(); !strings.Contains(a, "Pruned unused volumes.") {
		t.Errorf("other session lost the entries: %s", a)
	}
	e.cookie, e.csrf = mine, myCSRF
	e.post("/volumes/delete", url.Values{"name": {"cache"}}, htmx)
	if a := e.activity(); strings.Contains(a, "Pruned") || !strings.Contains(a, "Deleted volume cache.") {
		t.Errorf("after clear: %s", a)
	}

	// Without JavaScript, clear goes back to the page it was sent from,
	// but only to a page of this site.
	if w := e.post("/activity/clear", url.Values{"return": {"/images?x=1"}}); w.Header().Get("Location") != "/images?x=1" {
		t.Errorf("clear return = %v", w.Header())
	}
	if w := e.post("/activity/clear", url.Values{"return": {"//evil.example/"}}); w.Header().Get("Location") != "/containers" {
		t.Errorf("foreign clear return = %v", w.Header())
	}
	if w := e.do("POST", "/activity/clear", url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("clear without csrf = %d", w.Code)
	}
}

func TestActivityContainerActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	// From the list: recorded, the side bar only refreshes.
	w := e.post("/containers/web/stop", url.Values{"name": {"web"}}, htmx)
	if w.Header().Get("HX-Trigger") != "activity-recorded" || !strings.Contains(w.Body.String(), "Container web stopped.") {
		t.Errorf("stop = %v\n%s", w.Header(), w.Body.String())
	}
	if a := e.activity(); !strings.Contains(a, "stop web") || !strings.Contains(a, `<span class="job-kind">Container</span>`) {
		t.Errorf("after stop: %s", a)
	}
	// From the detail page: recorded, the next page keeps it closed.
	e.post("/containers/web/start", url.Values{"name": {"web"}, "view": {"detail"}})
	if body := e.do("GET", "/containers/web", nil).Body.String(); drawerOpen(body) || !strings.Contains(body, "start web") {
		t.Errorf("detail after start: open = %v", drawerOpen(body))
	}
	// A failed action is recorded as failed.
	e.post("/containers/nope/start", url.Values{"name": {"nope"}}, htmx)
	if a := e.activity(); !strings.Contains(a, "start nope") || !strings.Contains(a, `class="job failed"`) {
		t.Errorf("failed start: %s", a)
	}

	// Creating a container is recorded, also when it fails.
	e.post("/containers", url.Values{"image": {"alpine:latest"}, "name": {"made"}})
	if a := e.activity(); !strings.Contains(a, "create made") || !strings.Contains(a, "Container made created.") {
		t.Errorf("create: %s", a)
	}
	if body := e.do("GET", "/containers", nil).Body.String(); drawerOpen(body) {
		t.Error("create opens the side bar")
	}
}

var activityClosedRe = regexp.MustCompile(`<aside id="activity"[^>]* hidden>`)

func TestActivityCompose(t *testing.T) {
	e := newEnv(t)
	e.login()

	// An htmx action on the list opens the side bar in the browser (event);
	// the next page load still starts closed.
	w := e.post("/compose/shop/stop", url.Values{"view": {"list"}}, htmx)
	if w.Header().Get("HX-Trigger") != "activity-changed" {
		t.Errorf("stop trigger = %v", w.Header())
	}
	e.jobs.Wait()
	if a := e.activity(); !strings.Contains(a, "shop stop") || !strings.Contains(a, "succeeded") || !strings.Contains(a, `<span class="job-kind">Compose</span>`) {
		t.Errorf("after stop: %s", a)
	}
	if body := e.do("GET", "/compose", nil).Body.String(); !activityClosedRe.MatchString(body) || !strings.Contains(body, "shop stop") {
		t.Error("list page after an htmx action should start closed and show the entry")
	}

	// The poll that sees the job done lets the compose views reload.
	if w := e.do("GET", "/partials/activity?running=1", nil, htmx); !strings.Contains(w.Header().Get("HX-Trigger"), "compose-changed") {
		t.Errorf("finished poll trigger = %v", w.Header())
	}
	if w := e.do("GET", "/partials/activity", nil, htmx); w.Header().Get("HX-Trigger") != "" {
		t.Errorf("idle reload trigger = %v", w.Header())
	}

	// A detail-view action redirects; the reload opens the side bar once.
	e.post("/compose/shop/start", url.Values{"view": {"detail"}}, htmx)
	e.jobs.Wait()
	if body := e.do("GET", "/compose/shop", nil).Body.String(); activityClosedRe.MatchString(body) || !strings.Contains(body, "shop start") {
		t.Error("detail after action: side bar not open")
	}
	if body := e.do("GET", "/compose/shop", nil).Body.String(); !activityClosedRe.MatchString(body) {
		t.Error("side bar should open only once after the action")
	}
	for _, path := range []string{"/partials/compose/jobs", "/compose/jobs/clear"} {
		if w := e.do("GET", path, nil, htmx); w.Code == http.StatusOK && strings.Contains(w.Body.String(), "compose-jobs") {
			t.Errorf("%s still serves the old drawer", path)
		}
	}
}

func TestActivityKeepsHistory(t *testing.T) {
	e := newEnv(t)
	e.login()
	for i := 0; i < jobs.MaxHistory+5; i++ {
		e.post("/volumes/prune", nil, htmx)
	}
	if n := strings.Count(e.activity(), `<details class="job`); n != jobs.MaxHistory {
		t.Errorf("side bar lists %d entries, want %d", n, jobs.MaxHistory)
	}
}
