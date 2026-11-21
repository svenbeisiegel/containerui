package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestImageDetail(t *testing.T) {
	e := newEnv(t)
	e.login()

	if body := e.do("GET", "/partials/images", nil, htmx).Body.String(); !strings.Contains(body, `<a href="/images/nginx:1.27">nginx</a>`) {
		t.Error("image row does not link to its detail page")
	}

	w := e.do("GET", "/images/nginx:1.27", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /images/nginx:1.27 = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"sha256:bbb222", "190MB", "linux/amd64", ">Layers<", ">3<", "PATH", `href="/containers/c0ffee000001">web</a>`, "<h2>Inspect</h2>"} {
		if !strings.Contains(body, want) {
			t.Errorf("image detail misses %q", want)
		}
	}
	if body := e.do("GET", "/images/alpine:latest", nil).Body.String(); !strings.Contains(body, `>job</a>`) {
		t.Error("alpine:latest does not list its container job")
	}
	// Untagged images are addressed by ID.
	if w := e.do("GET", "/images/sha256:aaa111", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), ">Delete<") {
		t.Errorf("GET by ID = %d", w.Code)
	}
	if w := e.do("GET", "/images/nosuch:1", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Image nosuch:1 not found.") {
		t.Errorf("unknown image = %d", w.Code)
	}
	// The pull page keeps its own route.
	if w := e.do("GET", "/images/pull", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="ref"`) {
		t.Errorf("GET /images/pull = %d", w.Code)
	}

	// Actions sent from the detail page go back to it, or to the list once
	// the image is gone.
	w = e.post("/images/tag", url.Values{"source": {"alpine:latest"}, "target": {"alpine:mine"}, "view": {"detail"}}, htmx)
	if got := w.Header().Get("HX-Redirect"); got != "/images/alpine:latest" {
		t.Errorf("tag redirect = %q", got)
	}
	if body := e.do("GET", "/images/alpine:mine", nil).Body.String(); !strings.Contains(body, "No container was created from this image.") {
		t.Error("unused tag lists containers")
	}
	w = e.post("/images/delete", url.Values{"ref": {"alpine:mine"}, "view": {"detail"}}, htmx)
	if got := w.Header().Get("HX-Redirect"); got != "/images" {
		t.Errorf("delete redirect = %q", got)
	}
	w = e.post("/images/delete", url.Values{"ref": {"nosuch:1"}, "view": {"detail"}}, htmx)
	if got := w.Header().Get("HX-Redirect"); got != "/images/nosuch:1" {
		t.Errorf("failed delete redirect = %q", got)
	}
	// From the list, the table comes back as before.
	w = e.post("/images/tag", url.Values{"source": {"alpine:latest"}, "target": {"alpine:two"}}, htmx)
	if w.Header().Get("HX-Redirect") != "" || !strings.Contains(w.Body.String(), `id="images"`) {
		t.Error("list tag does not answer with the table")
	}
}

// Detail pages share the image page's layout: summary cards, titled
// panels and the inspect document collapsed at the bottom.
func TestDetailLayout(t *testing.T) {
	e := newEnv(t)
	e.login()
	for path, items := range map[string][]string{
		"/containers/web":    {">State<", ">Restart policy<", "<h2>Overview</h2>", "<h2>Network</h2>", "<h2>Mounts</h2>", "<h2>Environment</h2>", `href="/images/nginx:1.27"`},
		"/images/nginx:1.27": {">Size<", "<h2>Overview</h2>", "<h2>Containers</h2>"},
		"/volumes/data":      {">Driver<", "<h2>Overview</h2>", "<h2>Containers</h2>", `href="/containers/c0ffee000001">web</a>`},
		"/networks/backend":  {">Driver<", "<h2>Overview</h2>", "<h2>Addressing</h2>", "<h2>Labels</h2>", "<h2>Connected Containers</h2>"},
		"/compose/shop":      {">Services<", "<h2>Overview</h2>", "<h2>Services</h2>"},
	} {
		body := e.do("GET", path, nil).Body.String()
		if !strings.Contains(body, `<div class="cards stat-cards">`) {
			t.Errorf("GET %s: no summary cards", path)
		}
		for _, want := range items {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: misses %q", path, want)
			}
		}
		if path != "/compose/shop" && (!strings.Contains(body, `<details class="inspect">`) || strings.Contains(body, `<details class="inspect" open`)) {
			t.Errorf("GET %s: inspect output is not collapsed", path)
		}
	}
}
