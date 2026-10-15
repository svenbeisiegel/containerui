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
	body := e.do("GET", "/containers/new", nil).Body.String()
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
