package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// activity returns a tab's drawer body.
func (e *env) activity(tab string) string {
	e.t.Helper()
	w := e.do("GET", "/partials/activity/"+tab, nil, htmx)
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET activity %s = %d", tab, w.Code)
	}
	return w.Body.String()
}

// drawerOpen reports whether a page renders its activity drawer open.
func drawerOpen(body string) bool {
	i := strings.Index(body, `<aside id="activity"`)
	if i < 0 {
		return false
	}
	tag := body[i : i+strings.Index(body[i:], ">")]
	return !strings.Contains(tag, " hidden")
}

func TestActivityDrawer(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, tab := range []string{"images", "volumes", "networks"} {
		if a := e.activity(tab); !strings.Contains(a, "No recent activity.") || !strings.Contains(a, `id="activity-state" hx-swap-oob="true"`) {
			t.Errorf("%s: empty drawer = %s", tab, a)
		}
	}
	if w := e.do("GET", "/partials/activity/system", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown tab = %d", w.Code)
	}

	// An htmx action reloads and opens the drawer in place.
	w := e.post("/volumes/prune", nil, htmx)
	if w.Header().Get("HX-Trigger") != "activity-changed" {
		t.Errorf("prune trigger = %v", w.Header())
	}
	if a := e.activity("volumes"); !strings.Contains(a, "Pruned unused volumes.") || !strings.Contains(a, "status-dot succeeded") || !strings.Contains(a, "admin") {
		t.Errorf("after prune: %s", a)
	}
	if drawerOpen(e.do("GET", "/volumes", nil).Body.String()) {
		t.Error("an htmx action opens the drawer on the next page too")
	}
	// Tabs keep their own entries.
	if strings.Contains(e.activity("networks"), "Pruned") {
		t.Error("volume entry in the network drawer")
	}

	// Clear hides the entries for this user until something new happens.
	w = e.post("/activity/volumes/clear", nil, htmx)
	if !strings.Contains(w.Body.String(), "No recent activity.") {
		t.Errorf("clear: %s", w.Body.String())
	}
	e.post("/volumes/delete", url.Values{"name": {"cache"}}, htmx)
	if a := e.activity("volumes"); strings.Contains(a, "Pruned") || !strings.Contains(a, "Deleted volume cache.") {
		t.Errorf("after clear: %s", a)
	}
}

func TestActivityHiddenWithoutSupport(t *testing.T) {
	e := newEnv(t)
	e.rt.NoNetworks = true
	e.login()
	for _, path := range []string{"/partials/activity/networks", "/networks/new"} {
		if w := e.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
	if w := e.post("/activity/networks/clear", nil); w.Code != http.StatusNotFound {
		t.Errorf("clear = %d", w.Code)
	}
}
