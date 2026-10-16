package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestSystemPage(t *testing.T) {
	e := newEnvWith(t, func(o *Options) {
		o.Version = "1.2.3"
		o.InstallType = "rpm package"
	})
	e.login()
	w := e.do("GET", "/system", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /system: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"1.2.3", "rpm package", "dnf upgrade", `href="/system" aria-current="page"`, `href="/system/licenses"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

func TestLicensesPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/system/licenses", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /system/licenses: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q, want text/plain", ct)
	}
	for _, want := range []string{"htmx", "xterm.js", "gopkg.in/yaml.v3", "Apache License"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the notices do not name %q", want)
		}
	}
}
