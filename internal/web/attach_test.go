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

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// readControl reads messages until a control message arrives.
func readControl(t *testing.T, c *websocket.Conn) terminalControl {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no control message: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m terminalControl
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("control = %s", data)
		}
		return m
	}
}

// Attach is offered for running and stopped containers.
func TestAttachInMenu(t *testing.T) {
	e := newEnv(t)
	e.login()
	list := e.do("GET", "/partials/containers", nil, htmx).Body.String()
	for _, id := range []string{"c0ffee000001", "c0ffee000002"} {
		if !strings.Contains(list, `href="/containers/`+id+`/attach" target="_blank"`) {
			t.Errorf("menu of %s lacks Attach", id)
		}
	}
}

func TestAttachPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/web/attach", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, want := range []string{"<title>Attach · web", `data-url="/containers/c0ffee000001/attach/ws"`, `data-mode="attach"`,
		`data-start="false"`, `data-autoconnect="true"`, "/static/terminal.js", ">Detach<"} {
		if !strings.Contains(body, want) {
			t.Errorf("attach page lacks %q", want)
		}
	}

	// Stopped: waits for "Start and attach" unless asked to start.
	body = e.do("GET", "/containers/job/attach", nil).Body.String()
	for _, want := range []string{`data-start="true"`, `data-autoconnect="false"`, "Start and attach", "not running"} {
		if !strings.Contains(body, want) {
			t.Errorf("stopped attach page lacks %q", want)
		}
	}
	if body := e.do("GET", "/containers/job/attach?start=1", nil).Body.String(); !strings.Contains(body, `data-autoconnect="true"`) {
		t.Error("start=1 does not connect right away")
	}
	if w := e.do("GET", "/containers/nope/attach", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown container: %d", w.Code)
	}
}

func TestAttachWebSocket(t *testing.T) {
	e := newEnv(t)
	e.login()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ctx := context.Background()
	csrf := "&csrf=" + url.QueryEscape(e.csrf)

	// A stopped container needs start=1.
	if _, resp, err := dialTerminal(t, e, srv, "/containers/job/attach/ws?x=1"+csrf, nil); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("stopped without start: resp = %v, err = %v", resp, err)
	}

	// Seeded containers keep no STDIN open: output only, input dropped.
	c, _, err := dialTerminal(t, e, srv, "/containers/web/attach/ws?cols=100&rows=30"+csrf, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The earlier output comes first, then the live output.
	got := readUntil(t, c, "attached to web")
	logs, marker := strings.Index(got, "[fake] web log line 5\r\n"), strings.Index(got, "── attached ──")
	if !strings.Contains(got, "── earlier output (last 500 lines) ──") || logs < 0 || marker < logs || strings.Index(got, "attached to web") < marker {
		t.Errorf("attach output = %q; want the log lines, then the marker, then the attach output", got)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("exit 3\r"))
	c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	cs, _ := e.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	for _, ct := range cs {
		if ct.Name == "web" && ct.State != runtime.StateRunning {
			t.Errorf("web is %s after detaching; input must be dropped and detach keep it running", ct.State)
		}
	}

	// Create with TTY and STDIN, then start attached.
	w := e.post("/containers", url.Values{"image": {"alpine:latest"}, "name": {"setup"}, "tty": {"on"}, "interactive": {"on"}, "start": {"on"}, "attach": {"1"}})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/containers/") || !strings.HasSuffix(loc, "/attach?start=1") {
		t.Fatalf("create and attach: %d %q", w.Code, loc)
	}
	if e.rt.LastCreate.Start {
		t.Error("create and attach started the container on create")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(loc, "/containers/"), "/attach?start=1")
	c, _, err = dialTerminal(t, e, srv, "/containers/"+id+"/attach/ws?start=1"+csrf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readUntil(t, c, "first start of setup"); !strings.Contains(got, "── starting ──") {
		t.Errorf("start attached output = %q; want the starting marker", got)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("echo configured\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "configured\r\n")
	c.Close(websocket.StatusNormalClosure, "")

	// Attach again to the running container; start=1 is ignored now.
	c, _, err = dialTerminal(t, e, srv, "/containers/"+id+"/attach/ws?start=1"+csrf, nil)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "attached to setup")
	// The main process exits: the container stops.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("exit 4\r"))
	if m := readControl(t, c); m.Type != "exit" || m.Code == nil || *m.Code != 4 {
		t.Errorf("control = %+v; want exit 4", m)
	}

	var started, attached bool
	for _, call := range e.rt.Calls {
		started = started || call == "attach-start "+id+" stdin=true tty=true 80x24"
		attached = attached || call == "attach "+id+" stdin=true tty=true 80x24"
	}
	if !started || !attached {
		t.Errorf("calls = %v", e.rt.Calls)
	}
}

// The create form offers "Create and attach" only with TTY and STDIN.
func TestCreateAndAttachButton(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/containers/new/scratch", nil).Body.String()
	if !strings.Contains(body, `name="attach" value="1" formtarget="_blank" data-attach-create`) {
		t.Fatal("create form lacks the attach button")
	}
	if strings.Contains(body, "data-attach-create hidden") {
		t.Error("attach button hidden although TTY and STDIN are on by default")
	}
	w := e.post("/containers", url.Values{"image": {"alpine:latest"}, "name": {"bad name!"}, "tty": {"on"}})
	if !strings.Contains(w.Body.String(), "data-attach-create hidden") {
		t.Error("attach button shown without STDIN")
	}
}

func TestHistoryFrame(t *testing.T) {
	got := string(historyFrame([]byte("one\ntwo\r\nthree"), false))
	want := "\x1b[2m── earlier output (last 500 lines) ──\x1b[0m\r\n" +
		"one\r\ntwo\r\nthree\r\n" +
		"\x1b[2m── attached ──\x1b[0m\r\n"
	if got != want {
		t.Errorf("historyFrame = %q\nwant %q", got, want)
	}
	if got := string(historyFrame([]byte("ready\n"), true)); !strings.HasSuffix(got, "ready\r\n\x1b[2m── starting ──\x1b[0m\r\n") {
		t.Errorf("historyFrame start = %q", got)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 5}
	for _, p := range []string{"abc", "def", "g"} {
		_, _ = b.Write([]byte(p))
	}
	if got := b.b.String(); got != "cdefg" {
		t.Errorf("tail = %q; want cdefg", got)
	}
	_, _ = b.Write([]byte("0123456789"))
	if got := b.b.String(); got != "56789" {
		t.Errorf("tail = %q; want 56789", got)
	}
}
