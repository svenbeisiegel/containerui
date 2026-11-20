package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// attachTarget is a container that can be attached to, with how it was
// created: input is only forwarded when it keeps STDIN open, and the detach
// keys only work with a TTY.
type attachTarget struct {
	C          *runtime.Container
	TTY, Stdin bool
}

// findAttachTarget runs the pre-checks for attaching. On failure it returns
// the HTTP status and the message to show (with the container if found).
func (s *Server) findAttachTarget(ctx context.Context, id string) (attachTarget, int, string) {
	if !s.caps().Attach {
		return attachTarget{}, http.StatusNotImplemented, "The " + s.rt.Name() + " runtime does not support attaching to containers."
	}
	c, status, msg := s.findContainer(ctx, id)
	if c == nil {
		return attachTarget{}, status, msg
	}
	t := attachTarget{C: c}
	raw, err := s.rt.InspectContainer(ctx, c.ID)
	if err != nil {
		return t, http.StatusBadGateway, userError(err)
	}
	var in runtime.ContainerInspect
	if err := json.Unmarshal(raw, &in); err != nil {
		return t, http.StatusBadGateway, "Cannot read the container's settings: " + err.Error()
	}
	t.TTY, t.Stdin = in.Config.Tty, in.Config.OpenStdin
	return t, http.StatusOK, ""
}

// attachPageView is rendered by the standalone attach page.
type attachPageView struct {
	terminalPageView
	TTY, Stdin bool
	Running    bool
	// Start: the first connect starts the stopped container attached.
	Start bool
	// AutoConnect: connect when the page opens (a stopped container waits
	// for "Start and attach" unless it came from "Create and attach").
	AutoConnect bool
	DetachKeys  string
}

// handleAttachPage renders the attach page, which opens in its own tab.
// ?start=1 (from "Create and attach") starts a stopped container right
// away.
func (s *Server) handleAttachPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := attachPageView{terminalPageView: terminalPageView{ID: id, CSRF: sessionFrom(r).CSRF, Opened: time.Now(),
		Favicon: s.favicon, FaviconType: s.favType}, DetachKeys: runtime.DetachKeys}
	t, status, msg := s.findAttachTarget(r.Context(), id)
	v.C, v.Err, v.TTY, v.Stdin = t.C, msg, t.TTY, t.Stdin
	if t.C != nil {
		v.ID = t.C.ID
		v.Running = t.C.State.IsRunning()
	}
	if msg == "" {
		v.WSURL = "/containers/" + t.C.ID + "/attach/ws"
		v.Start = !v.Running
		v.AutoConnect = v.Running || r.URL.Query().Get("start") == "1"
	}
	allowInlineStyles(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.attachPage.ExecuteTemplate(w, "attach_page", v); err != nil {
		s.log.Error("render page", "page", "attach_page", "error", err)
	}
}

// handleAttachWS attaches a browser terminal to the container's main
// process (see pumpTerminal for the protocol). With start=1 a stopped
// container is started attached. Closing the connection detaches; the
// container keeps running.
func (s *Server) handleAttachWS(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	q := r.URL.Query()
	// WebSocket requests are GETs, so checkCSRF does not cover them;
	// websocket.Accept additionally rejects foreign Origins.
	if !sess.ValidCSRF(q.Get("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	t, status, msg := s.findAttachTarget(r.Context(), r.PathValue("id"))
	if msg != "" {
		http.Error(w, msg, status)
		return
	}
	c := t.C
	// Starting is only asked for; a container that already runs is
	// attached to instead.
	start := !c.State.IsRunning()
	if start && q.Get("start") != "1" {
		http.Error(w, "Start the container to attach to it.", http.StatusConflict)
		return
	}
	if s.terminals.Add(1) > maxTerminals {
		s.terminals.Add(-1)
		http.Error(w, "Too many open terminals. Close one and try again.", http.StatusServiceUnavailable)
		return
	}
	defer s.terminals.Add(-1)

	conn, err := s.acceptTerminal(w, r)
	if err != nil {
		s.log.Warn("attach upgrade failed", "id", c.ID, "user", sess.Username, "error", err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.caps().Logs {
		// Attach only shows output written from now on, so a prompt the
		// container already printed would be invisible: show the recent
		// output first. Output written between this read and the start of
		// the attach below can be missed.
		if out := s.attachHistory(ctx, c.ID); len(out) > 0 {
			_ = conn.Write(ctx, websocket.MessageBinary, historyFrame(out, start))
		}
	}
	opts := runtime.AttachOpts{Start: start, Stdin: t.Stdin, TTY: t.TTY,
		Cols: sizeParam(q.Get("cols"), 80, 10, 500), Rows: sizeParam(q.Get("rows"), 24, 5, 300)}
	term, err := s.rt.OpenAttach(ctx, c.ID, opts)
	if err != nil {
		s.log.Warn("attach failed", "id", c.ID, "start", start, "user", sess.Username, "error", err)
		sendControl(ctx, conn, terminalControl{Type: "error", Message: userError(err)})
		conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	begin := time.Now()
	s.log.Info("container attached", "id", c.ID, "name", c.Name, "start", start, "stdin", t.Stdin, "tty", t.TTY, "user", sess.Username)
	reason, code := s.pumpTerminal(ctx, cancel, conn, term, sess, t.Stdin, c.ID)
	if reason == "exited" {
		// The CLI ends when the container stops or on the detach keys.
		if now, _, _ := s.findContainer(ctx, c.ID); now != nil && now.State.IsRunning() {
			reason = "detached"
			sendControl(ctx, conn, terminalControl{Type: "detached"})
		} else {
			sendControl(ctx, conn, terminalControl{Type: "exit", Code: &code})
		}
	}
	conn.Close(websocket.StatusNormalClosure, reason)
	s.log.Info("container detached", "id", c.ID, "name", c.Name, "user", sess.Username, "reason", reason,
		"duration", time.Since(begin).Round(time.Second))
}

// attachHistory is how many lines of earlier output the attach terminal
// shows before the live output.
const attachHistory = 500

// attachHistoryMax caps the earlier output (its end is kept).
const attachHistoryMax = 1 << 20

// attachHistory returns the container's last attachHistory lines of output.
// Errors are only logged: the attach goes on without them.
func (s *Server) attachHistory(ctx context.Context, id string) []byte {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	buf := &tailBuffer{max: attachHistoryMax}
	if err := s.rt.ContainerLogs(ctx, id, runtime.LogsOpts{Tail: attachHistory}, buf); err != nil {
		s.log.Warn("attach history failed", "id", id, "error", err)
	}
	return buf.b.Bytes()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	b   bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.b.Reset()
		t.b.Write(p[len(p)-t.max:])
		return n, nil
	}
	if over := t.b.Len() + len(p) - t.max; over > 0 {
		rest := append([]byte(nil), t.b.Bytes()[over:]...)
		t.b.Reset()
		t.b.Write(rest)
	}
	t.b.Write(p)
	return n, nil
}

// historyFrame frames the earlier output for the terminal: a dim header,
// the output with lone \n turned into \r\n (logs come without a TTY's
// line discipline) and a dim marker where the live output begins.
func historyFrame(out []byte, start bool) []byte {
	const dim, reset = "\x1b[2m", "\x1b[0m"
	var b bytes.Buffer
	b.WriteString(dim + "── earlier output (last " + strconv.Itoa(attachHistory) + " lines) ──" + reset + "\r\n")
	for i, ch := range out {
		if ch == '\n' && (i == 0 || out[i-1] != '\r') {
			b.WriteByte('\r')
		}
		b.WriteByte(ch)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		b.WriteString("\r\n")
	}
	marker := "attached"
	if start {
		marker = "starting"
	}
	b.WriteString(dim + "── " + marker + " ──" + reset + "\r\n")
	return b.Bytes()
}
