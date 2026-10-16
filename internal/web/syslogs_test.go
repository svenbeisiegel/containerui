package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/hostinfo"
	"github.com/svenbeisiegel/containerui/internal/hostlog"
)

func fakeHostInfo(context.Context) hostinfo.Info {
	return hostinfo.Info{
		Hostname: "testhost", OSName: "Rocky Linux 9.4 (Blue Onyx)", Kernel: "5.14.0-427.el9.x86_64", Arch: "x86_64",
		CPUModel: "Test CPU", CPUs: 4, MemTotal: 16 << 30, MemAvailable: 8 << 30,
		Uptime: 50 * time.Hour, BootTime: time.Date(2026, 10, 14, 8, 0, 0, 0, time.Local),
		Load: [3]float64{0.5, 0.25, 0.125}, HasLoad: true, SELinux: "enforcing", Cgroup: "v2 (unified)",
		StorageMount: "/var/lib/containers/storage", StorageTotal: 100 << 30, StorageFree: 40 << 30,
	}
}

// fakeSysLog is a journal-like system log. Read writes "entry N" lines;
// following writes the tail and then waits for stop or the context.
type fakeSysLog struct {
	unavailable error
	stop        chan struct{}

	mu   sync.Mutex
	last hostlog.Query
}

func (f *fakeSysLog) lastQuery() hostlog.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeSysLog) Kind() string          { return "journal" }
func (f *fakeSysLog) Label() string         { return "Journal" }
func (f *fakeSysLog) SelectorLabel() string { return "Unit" }
func (f *fakeSysLog) Available() error      { return f.unavailable }
func (f *fakeSysLog) Selectors(context.Context) ([]hostlog.Option, error) {
	return []hostlog.Option{{Value: "", Label: "All units"}, {Value: "docker.service", Label: "docker.service"}}, nil
}
func (f *fakeSysLog) Priorities() []hostlog.Option {
	return []hostlog.Option{{Value: "", Label: "All priorities"}, {Value: "err", Label: "Error and worse"}}
}
func (f *fakeSysLog) Ranges() []hostlog.Option {
	return []hostlog.Option{{Value: "boot", Label: "Current boot"}, {Value: "", Label: "Entire journal"}}
}
func (f *fakeSysLog) ExportExt() string { return ".evtx" }
func (f *fakeSysLog) Export(_ context.Context, _ hostlog.Query, w io.Writer) error {
	_, err := io.WriteString(w, "ElfFile\x00")
	return err
}

func (f *fakeSysLog) Read(ctx context.Context, q hostlog.Query, w io.Writer) error {
	f.mu.Lock()
	f.last = q
	f.mu.Unlock()
	n := 5
	if q.Follow {
		n = q.Tail
	}
	for i := 1; i <= n; i++ {
		if _, err := fmt.Fprintf(w, "entry %d %s\n", i, q.Selector); err != nil {
			return err
		}
	}
	if !q.Follow {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.stop:
		return nil
	}
}

func TestSystemPageHost(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/system", nil).Body.String()
	for _, want := range []string{"testhost", "Rocky Linux 9.4", "5.14.0-427.el9.x86_64", "2 days 2 hours", "8.0 GiB available of 16.0 GiB",
		"0.50 0.25 0.12", "40.0 GiB free of 100.0 GiB", "enforcing", "v2 (unified)",
		"System Logs", `<option value="docker.service">`, `formaction="/system/logs/watch" formtarget="_blank"`, `value="native"`, "Download .evtx"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	if strings.Contains(body, "Build") || strings.Contains(body, "WSL") {
		t.Error("the page shows empty host fields")
	}
}

func TestSystemPageLogsUnavailable(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.HostLog = &fakeSysLog{unavailable: errors.New("journalctl was not found")} })
	e.login()
	body := e.do("GET", "/system", nil).Body.String()
	if !strings.Contains(body, "journalctl was not found") || strings.Contains(body, "/system/logs/watch") {
		t.Error("an unavailable log should show its error instead of the form")
	}
	if w := e.do("GET", "/system/logs/download", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("download status = %d", w.Code)
	}
}

func TestSysLogsWatchPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/system/logs/watch?sel=docker.service&prio=err&since=boot", nil)
	body := w.Body.String()
	for _, want := range []string{"<title>Logs · Journal · docker.service · ContainerUI</title>", "Error and worse", "Current boot",
		`data-stream="/system/logs/stream?prio=err&amp;sel=docker.service&amp;since=boot"`, "/static/logs.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("watch page lacks %q", want)
		}
	}
	for _, bad := range []string{"sel=-f", "sel=sshd.service", "prio=emerg", "since=-1h"} {
		if w := e.do("GET", "/system/logs/watch?"+bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", bad, w.Code)
		}
	}
}

func TestSysLogsDownload(t *testing.T) {
	src := &fakeSysLog{}
	e := newEnvWith(t, func(o *Options) { o.HostLog = src })
	e.login()

	w := e.do("GET", "/system/logs/download?sel=docker.service&prio=&since=", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	cd := w.Header().Get("Content-Disposition")
	if !regexp.MustCompile(`^attachment; filename="[A-Za-z0-9_.-]+_journal_docker.service_\d{8}-\d{6}\.log"$`).MatchString(cd) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if body := w.Body.String(); !strings.HasPrefix(body, "entry 1 docker.service\n") || !strings.Contains(body, "entry 5") {
		t.Errorf("body = %q", body)
	}
	if src.lastQuery().Follow || src.lastQuery().Since != "" {
		t.Errorf("query = %+v", src.lastQuery())
	}

	// Without a time range the default (first) range applies.
	e.do("GET", "/system/logs/download", nil)
	if src.lastQuery().Since != "boot" {
		t.Errorf("default range = %q", src.lastQuery().Since)
	}

	w = e.do("GET", "/system/logs/download?format=native", nil)
	if cd := w.Header().Get("Content-Disposition"); !strings.HasSuffix(cd, `.evtx"`) || w.Body.String() != "ElfFile\x00" {
		t.Errorf("native: disposition %q, body %q", cd, w.Body)
	}
}

func TestSysLogsStream(t *testing.T) {
	src := &fakeSysLog{stop: make(chan struct{})}
	e := newEnvWith(t, func(o *Options) { o.HostLog = src })
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	resp, r := openLogStream(t, e, srv, "/system/logs/stream?sel=docker.service&prio=err&since=boot&tail=3")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	readEventsUntil(t, r, "data: entry 3 docker.service\n")
	close(src.stop)
	readEventsUntil(t, r, "event: end\ndata: The log stream ended.\n\n")
	if !src.lastQuery().Follow || src.lastQuery().Tail != 3 || src.lastQuery().Priority != "err" {
		t.Errorf("query = %+v", src.lastQuery())
	}
}
