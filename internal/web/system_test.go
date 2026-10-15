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
	for _, want := range []string{"1.2.3", "rpm package", "dnf upgrade", `href="/system" aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}
