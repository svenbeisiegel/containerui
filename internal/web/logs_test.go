package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestLogsButtonsOnDetailPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	body := e.do("GET", "/containers/c0ffee000001", nil).Body.String()
	for _, want := range []string{`href="/containers/c0ffee000001/logs/download"`, `href="/containers/c0ffee000001/logs/watch" target="_blank"`} {
		if !strings.Contains(body, want) {
			t.Errorf("running container page lacks %q", want)
		}
	}
	body = e.do("GET", "/containers/c0ffee000002", nil).Body.String()
	if !strings.Contains(body, "/logs/download") || strings.Contains(body, "/logs/watch") {
		t.Error("stopped container should offer the download only")
	}
}

func TestLogsDownload(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/web/logs/download", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	cd := w.Header().Get("Content-Disposition")
	if !regexp.MustCompile(`^attachment; filename="web_\d{8}-\d{6}\.log"$`).MatchString(cd) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if body := w.Body.String(); !strings.HasPrefix(body, "[fake] web log line 1\n") || !strings.Contains(body, "log line 5\n") {
		t.Errorf("body = %q", body)
	}

	// Stopped containers still have logs.
	if w := e.do("GET", "/containers/job/logs/download", nil); w.Code != http.StatusOK {
		t.Errorf("stopped container status = %d", w.Code)
	}

	w = e.do("GET", "/containers/nope/logs/download", nil)
	if w.Code != http.StatusNotFound || w.Header().Get("Content-Disposition") != "" {
		t.Errorf("unknown container: status = %d, disposition = %q", w.Code, w.Header().Get("Content-Disposition"))
	}

	e.cookie = nil
	if w := e.do("GET", "/containers/web/logs/download", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("anonymous download: status = %d", w.Code)
	}
}

func TestLogFileName(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 25, 30, 0, time.Local)
	for _, c := range []struct {
		c    runtime.Container
		want string
	}{
		{runtime.Container{ID: "abc", Name: "couchdb"}, "couchdb_20261007-142530.log"},
		{runtime.Container{ID: "abc", Name: "my app/1"}, "my_app_1_20261007-142530.log"},
		{runtime.Container{ID: "0123456789abcdef", Name: ""}, "0123456789ab_20261007-142530.log"},
	} {
		if got := logFileName(&c.c, now); got != c.want {
			t.Errorf("logFileName(%q) = %q, want %q", c.c.Name, got, c.want)
		}
	}
}

func TestLogsWatchPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/web/logs/watch", nil)
	body := w.Body.String()
	for _, want := range []string{"<title>Logs · web · ContainerUI</title>", "nginx:1.27", "127.0.0.1:8080-&gt;80/tcp",
		`data-stream="/containers/c0ffee000001/logs/stream"`, "/static/logs.js", "Watching since"} {
		if !strings.Contains(body, want) {
			t.Errorf("watch page lacks %q", want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") {
		t.Errorf("watch page CSP = %q", csp)
	}
	if w := e.do("GET", "/containers/nope/logs/watch", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown container status = %d", w.Code)
	}
}

// openLogStream starts a log stream request against a test server.
func openLogStream(t *testing.T, e *env, srv *httptest.Server, path string) (*http.Response, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	req.AddCookie(e.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body)
}

// readEventsUntil reads the SSE stream until it contains want.
func readEventsUntil(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		line, err := r.ReadString('\n')
		out.WriteString(line)
		if err != nil {
			t.Fatalf("reading for %q: %v (got %q)", want, err, out.String())
		}
	}
	return out.String()
}

func TestLogsStream(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.LogInterval = 20 * time.Millisecond
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	resp, r := openLogStream(t, e, srv, "/containers/web/logs/stream?tail=2")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := readEventsUntil(t, r, "data: [fake] web log line 7\n")
	if strings.Contains(out, "line 3\n") || !strings.Contains(out, "data: [fake] web log line 4\n\n") {
		t.Errorf("tail=2 stream = %q", out)
	}

	if err := e.rt.StopContainer(context.Background(), "web", nil); err != nil {
		t.Fatal(err)
	}
	readEventsUntil(t, r, "event: end\ndata: The log stream ended; the container is no longer running.\n\n")
}

func TestLogsStreamEndsWithSession(t *testing.T) {
	old := logStreamTick
	logStreamTick = 20 * time.Millisecond
	defer func() { logStreamTick = old }()
	e := newEnv(t)
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	_, r := openLogStream(t, e, srv, "/containers/web/logs/stream")
	readEventsUntil(t, r, "line 5")
	e.post("/logout", nil)
	readEventsUntil(t, r, "event: end\ndata: Your session has ended.")
}

func TestSSEWriterFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, rc: http.NewResponseController(rec)}
	for _, chunk := range []string{"one\r\ntw", "o\r", "\nthree\rfour\n\nfive"} {
		if _, err := sw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.end("bye"); err != nil {
		t.Fatal(err)
	}
	// One event per completed write; the CR of the split CRLF is held back.
	want := "data: one\n\n" + "data: two\ndata: three\ndata: four\ndata: \n\n" + "data: five\n\n" + "event: end\ndata: bye\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("events =\n%q\nwant\n%q", got, want)
	}
	sw.close()
	if _, err := sw.Write([]byte("late\n")); err == nil {
		t.Error("write after close succeeded")
	}
}
