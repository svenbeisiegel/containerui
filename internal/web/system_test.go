package web

import (
	"net/http"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestSystemPage(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.Version = "1.2.3" })
	e.login()
	w := e.do("GET", "/system", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /system: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"1.2.3", goruntime.GOOS + "/" + goruntime.GOARCH,
		`href="/system" aria-current="page"`, `href="/system/licenses"`,
		// specs
		"Test CPU", "1 socket · 2 cores · 4 logical CPUs · 3.60 GHz", "0 B used of 2.0 GiB",
		// live usage
		`id="system-live"`, `hx-get="/partials/system?sort=cpu"`, "38%", `class="fill" width="37.5"`, "8.0 GiB of 16.0 GiB",
		"/dev/mapper/rl-root", "container storage", `class="fill" width="60.0"`, `class="usage bad"`,
		"212", "2 running · 640 threads",
		"conmon", `title="/usr/bin/conmon --api-version 1"`, "13%",
		// container platform
		"Container Platform", "enforcing",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	for _, gone := range []string{"dnf upgrade", "Installed as", "Executable", "couchdb"} {
		if strings.Contains(body, gone) {
			t.Errorf("the page still shows %q", gone)
		}
	}
}

func TestSystemPartial(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/partials/system?sort=mem", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /partials/system: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="system-live"`) || strings.Contains(body, "<html") {
		t.Errorf("not just the live block:\n%s", body)
	}
	for _, want := range []string{`hx-get="/partials/system?sort=mem" hx-trigger="every 5s"`, "couchdb", `sort=mem" hx-target="#system-live" hx-swap="outerHTML" aria-current="true"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the partial does not show %q", want)
		}
	}
	if strings.Contains(body, "conmon") {
		t.Error("ordered by memory, but shows the CPU list")
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
