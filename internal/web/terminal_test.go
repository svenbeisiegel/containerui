package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTerminalConnectOnDetailPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/c0ffee000001", nil)
	body := w.Body.String()
	for _, want := range []string{`id="terminal-connect"`, `hx-get="/containers/c0ffee000001/terminal/check"`, `class="unavailable" disabled>Connect`} {
		if !strings.Contains(body, want) {
			t.Errorf("running container page lacks %q", want)
		}
	}
	// xterm only loads on the terminal page.
	if strings.Contains(w.Header().Get("Content-Security-Policy"), "unsafe-inline") || strings.Contains(body, "xterm") {
		t.Error("detail page must not relax the CSP or load xterm")
	}

	body = e.do("GET", "/containers/c0ffee000002", nil).Body.String()
	if !strings.Contains(body, `title="Start the container to open a terminal."`) || strings.Contains(body, "terminal/check") ||
		!strings.Contains(body, `class="unavailable" disabled>Connect`) {
		t.Error("stopped container should show a disabled Connect button without a check")
	}
}

func TestTerminalCheck(t *testing.T) {
	e := newEnv(t)
	e.login()

	body := e.do("GET", "/containers/c0ffee000001/terminal/check", nil, htmx).Body.String()
	if !strings.Contains(body, `href="/containers/c0ffee000001/terminal" target="_blank"`) || !strings.Contains(body, "/bin/sh") ||
		!strings.Contains(body, `class="button primary"`) || strings.Contains(body, "disabled") {
		t.Errorf("ready button:\n%s", body)
	}

	body = e.do("GET", "/containers/job/terminal/check", nil, htmx).Body.String()
	if !strings.Contains(body, "Start the container") || strings.Contains(body, "data-terminal-connect") || !strings.Contains(body, "disabled") {
		t.Errorf("stopped button:\n%s", body)
	}

	e.rt.NoShell = true
	body = e.do("GET", "/containers/web/terminal/check", nil, htmx).Body.String()
	if !strings.Contains(body, "No shell (bash or sh) found") || strings.Contains(body, "data-terminal-connect") || !strings.Contains(body, "disabled") {
		t.Errorf("no-shell button:\n%s", body)
	}

	body = e.do("GET", "/containers/nope/terminal/check", nil, htmx).Body.String()
	if !strings.Contains(body, "not found") {
		t.Errorf("unknown container button:\n%s", body)
	}
}

func TestTerminalPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/web/terminal", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, want := range []string{"<title>Terminal · web", `data-url="/containers/c0ffee000001/terminal/ws"`, `data-csrf="` + e.csrf + `"`,
		"/bin/sh", "/static/terminal.js", "/static/vendor/xterm/xterm.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("terminal page lacks %q", want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src") {
		t.Errorf("terminal page CSP = %q", csp)
	}

	w = e.do("GET", "/containers/job/terminal", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Start the container") || strings.Contains(w.Body.String(), "terminal.js") {
		t.Errorf("stopped container: %d\n%s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/containers/nope/terminal", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown container: %d", w.Code)
	}
}

// dialTerminal opens the terminal WebSocket of a running test server.
func dialTerminal(t *testing.T, e *env, srv *httptest.Server, path string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if header == nil {
		header = http.Header{}
	}
	if e.cookie != nil {
		header.Add("Cookie", e.cookie.Name+"="+e.cookie.Value)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: header})
}

// readUntil reads terminal output until it contains want.
func readUntil(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("reading for %q: %v (got %q)", want, err, out.String())
		}
		if typ == websocket.MessageBinary {
			out.Write(data)
		}
	}
	return out.String()
}

func TestTerminalWebSocket(t *testing.T) {
	e := newEnv(t)
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ctx := context.Background()

	c, _, err := dialTerminal(t, e, srv, "/containers/web/terminal/ws?cols=100&rows=30&csrf="+url.QueryEscape(e.csrf), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "/ # ")

	if err := c.Write(ctx, websocket.MessageBinary, []byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "30 100")

	resize, _ := json.Marshal(terminalControl{Type: "resize", Cols: 132, Rows: 43})
	if err := c.Write(ctx, websocket.MessageText, resize); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("echo hello\rstty size\r")); err != nil {
		t.Fatal(err)
	}
	if out := readUntil(t, c, "43 132"); !strings.Contains(out, "\nhello\r") {
		t.Errorf("echo output = %q", out)
	}

	if err := c.Write(ctx, websocket.MessageBinary, []byte("exit 3\r")); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(rctx)
		if err != nil {
			t.Fatalf("no exit message: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m terminalControl
		if err := json.Unmarshal(data, &m); err != nil || m.Type != "exit" || m.Code == nil || *m.Code != 3 {
			t.Fatalf("control = %s", data)
		}
		break
	}
	if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("close = %v; want normal closure", err)
	}

	var opened bool
	for _, call := range e.rt.Calls {
		opened = opened || call == "terminal open c0ffee000001 /bin/sh 100x30"
	}
	if !opened {
		t.Errorf("calls = %v", e.rt.Calls)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(e.logs.String(), "terminal closed") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.logs.String(), "exit_code=3") {
		t.Errorf("close not logged:\n%s", e.logs.String())
	}
}

func TestTerminalWebSocketRejected(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	// Not signed in.
	if _, resp, err := dialTerminal(t, e, srv, "/containers/web/terminal/ws?csrf=x", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: resp = %v, err = %v", resp, err)
	}

	e.login()
	csrf := "&csrf=" + url.QueryEscape(e.csrf)
	cases := []struct {
		name, path string
		header     http.Header
		status     int
	}{
		{"missing csrf", "/containers/web/terminal/ws?cols=80", nil, http.StatusForbidden},
		{"wrong csrf", "/containers/web/terminal/ws?csrf=nope", nil, http.StatusForbidden},
		{"foreign origin", "/containers/web/terminal/ws?x=1" + csrf, http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
		{"stopped container", "/containers/job/terminal/ws?x=1" + csrf, nil, http.StatusConflict},
		{"unknown container", "/containers/nope/terminal/ws?x=1" + csrf, nil, http.StatusNotFound},
	}
	for _, c := range cases {
		conn, resp, err := dialTerminal(t, e, srv, c.path, c.header)
		if err == nil {
			conn.CloseNow()
			t.Errorf("%s: connected", c.name)
			continue
		}
		if resp == nil || resp.StatusCode != c.status {
			t.Errorf("%s: resp = %v, err = %v; want %d", c.name, resp, err, c.status)
		}
	}
}

func TestTerminalClosedOnClientDisconnect(t *testing.T) {
	e := newEnv(t)
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	c, _, err := dialTerminal(t, e, srv, "/containers/web/terminal/ws?csrf="+url.QueryEscape(e.csrf), nil)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "/ # ")
	c.Close(websocket.StatusNormalClosure, "bye")

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(e.logs.String(), "reason=disconnected") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.logs.String(), "reason=disconnected") {
		t.Errorf("disconnect not handled:\n%s", e.logs.String())
	}
}

func TestTerminalClosedOnLogout(t *testing.T) {
	old := terminalSessionCheck
	terminalSessionCheck = 50 * time.Millisecond
	t.Cleanup(func() { terminalSessionCheck = old })

	e := newEnv(t)
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	c, _, err := dialTerminal(t, e, srv, "/containers/web/terminal/ws?csrf="+url.QueryEscape(e.csrf), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "/ # ")
	e.post("/logout", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no session-ended message: %v", err)
		}
		if typ == websocket.MessageText {
			if !strings.Contains(string(data), "session has ended") {
				t.Errorf("control = %s", data)
			}
			break
		}
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("close = %v", err)
	}
}

func TestSizeParam(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint16
	}{{"", 80}, {"abc", 80}, {"120", 120}, {"1", 10}, {"99999", 500}, {"-5", 10}} {
		if got := sizeParam(c.in, 80, 10, 500); got != c.want {
			t.Errorf("sizeParam(%q) = %d; want %d", c.in, got, c.want)
		}
	}
}
