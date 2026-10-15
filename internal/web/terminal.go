package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	// maxTerminals bounds concurrently open terminals (each runs a CLI
	// process on a pseudo console).
	maxTerminals = 8
	// terminalReadLimit bounds one browser message (e.g. a large paste).
	terminalReadLimit = 1 << 20
)

// terminalSessionCheck is how often an open terminal re-checks the login
// session, so logout or expiry closes it. Tests shorten it.
var terminalSessionCheck = 30 * time.Second

// terminalView is rendered by the terminal_connect partial.
type terminalView struct {
	ID    string // container ID (URL path segment)
	Name  string
	State string // "unsupported", "stopped", "checking", "unavailable", "ready"
	Shell string
	Err   string
	CSRF  string
}

// findContainer looks a container up by ID or name. On failure it returns
// the HTTP status and the message to show.
func (s *Server) findContainer(ctx context.Context, id string) (*runtime.Container, int, string) {
	cs, err := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	if err != nil {
		return nil, http.StatusBadGateway, userError(err)
	}
	for i := range cs {
		if cs[i].ID == id || cs[i].Name == id {
			return &cs[i], http.StatusOK, ""
		}
	}
	return nil, http.StatusNotFound, "Container " + id + " not found."
}

// terminalTarget runs the pre-checks for opening a terminal: driver
// support, a running container and a usable shell. On failure it returns
// the HTTP status and the message to show.
func (s *Server) terminalTarget(ctx context.Context, id string) (c *runtime.Container, shell string, status int, msg string) {
	if !s.caps().Terminal {
		return nil, "", http.StatusNotImplemented, "The " + s.rt.Name() + " runtime does not support terminals."
	}
	c, status, msg = s.findContainer(ctx, id)
	if c == nil {
		return nil, "", status, msg
	}
	if c.State != runtime.StateRunning {
		return c, "", http.StatusConflict, "Start the container to open a terminal."
	}
	shell, err := s.rt.CheckTerminal(ctx, c.ID)
	if err != nil {
		return c, "", http.StatusConflict, userError(err)
	}
	return c, shell, http.StatusOK, ""
}

// handleTerminalCheck renders the Connect button after checking whether a
// shell can be opened. The detail page loads it asynchronously because the
// check runs a command in the container.
func (s *Server) handleTerminalCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := terminalView{ID: id, Name: id, CSRF: sessionFrom(r).CSRF}
	c, shell, status, msg := s.terminalTarget(r.Context(), id)
	if c != nil {
		v.ID, v.Name = c.ID, c.Name
	}
	switch {
	case status == http.StatusNotImplemented:
		v.State = "unsupported"
	case c != nil && c.State != runtime.StateRunning:
		v.State = "stopped"
	case msg != "":
		v.State, v.Err = "unavailable", msg
	default:
		v.State, v.Shell = "ready", shell
	}
	s.renderPartial(w, "terminal_connect", v)
}

// terminalPageView is rendered by the standalone terminal page.
type terminalPageView struct {
	ID          string
	C           *runtime.Container // nil if the container was not found
	Shell       string
	Err         string // why no terminal can be opened
	WSURL       string
	CSRF        string
	Opened      time.Time
	Favicon     string
	FaviconType string
}

// handleTerminalPage renders the terminal page, which opens in its own tab;
// its script connects to the WebSocket.
func (s *Server) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := terminalPageView{ID: id, CSRF: sessionFrom(r).CSRF, Opened: time.Now(), Favicon: s.favicon, FaviconType: s.favType}
	c, shell, status, msg := s.terminalTarget(r.Context(), id)
	v.C, v.Shell, v.Err = c, shell, msg
	if c != nil {
		v.ID = c.ID
	}
	if msg == "" {
		v.WSURL = "/containers/" + c.ID + "/terminal/ws"
	}
	allowInlineStyles(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.terminalPage.ExecuteTemplate(w, "terminal_page", v); err != nil {
		s.log.Error("render page", "page", "terminal_page", "error", err)
	}
}

// terminalControl is a JSON text message. Browser to server: resize.
// Server to browser: exit (with Code) and error (with Message).
type terminalControl struct {
	Type    string `json:"type"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    *int   `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// handleTerminalWS connects a browser terminal (xterm.js) to a shell in the
// container. Binary messages carry raw terminal bytes in both directions;
// text messages carry terminalControl JSON.
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	q := r.URL.Query()
	// WebSocket requests are GETs, so checkCSRF does not cover them;
	// websocket.Accept additionally rejects foreign Origins.
	if !sess.ValidCSRF(q.Get("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	// Re-check server-side; the shell is never taken from the client.
	c, shell, status, msg := s.terminalTarget(r.Context(), r.PathValue("id"))
	if msg != "" {
		http.Error(w, msg, status)
		return
	}
	if s.terminals.Add(1) > maxTerminals {
		s.terminals.Add(-1)
		http.Error(w, "Too many open terminals. Close one and try again.", http.StatusServiceUnavailable)
		return
	}
	defer s.terminals.Add(-1)

	// The server's read/write timeouts would cut a long-lived connection.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("terminal upgrade failed", "id", c.ID, "user", sess.Username, "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminalReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sendControl := func(m terminalControl) {
		b, _ := json.Marshal(m)
		_ = conn.Write(ctx, websocket.MessageText, b)
	}

	cols := sizeParam(q.Get("cols"), 80, 10, 500)
	rows := sizeParam(q.Get("rows"), 24, 5, 300)
	term, err := s.rt.OpenTerminal(ctx, c.ID, runtime.TerminalOpts{Shell: shell, Cols: cols, Rows: rows})
	if err != nil {
		s.log.Warn("terminal open failed", "id", c.ID, "user", sess.Username, "error", err)
		sendControl(terminalControl{Type: "error", Message: userError(err)})
		conn.Close(websocket.StatusInternalError, "open failed")
		return
	}
	start := time.Now()
	s.log.Info("terminal opened", "id", c.ID, "name", c.Name, "shell", shell, "user", sess.Username)
	var closeOnce sync.Once
	closeTerm := func() {
		closeOnce.Do(func() {
			if err := term.Close(); err != nil {
				s.log.Warn("terminal close", "id", c.ID, "error", err)
			}
		})
	}
	defer closeTerm()

	// Terminal output -> browser.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Browser input -> terminal.
	var lastInput atomic.Int64
	go func() {
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				lastInput.Store(time.Now().UnixNano())
				if _, err := term.Write(data); err != nil {
					return
				}
				continue
			}
			var m terminalControl
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				_ = term.Resize(clamp(m.Cols, 10, 500), clamp(m.Rows, 5, 300))
			}
		}
	}()

	exited := make(chan int, 1)
	go func() {
		code, _ := term.Wait()
		exited <- code
	}()

	ticker := time.NewTicker(terminalSessionCheck)
	defer ticker.Stop()
	lastCheck := time.Now()
	reason := ""
	code := -1
loop:
	for {
		select {
		case code = <-exited:
			reason = "exited"
			break loop
		case <-ctx.Done():
			reason = "disconnected"
			break loop
		case <-ticker.C:
			// Typing counts as activity for the login session; an idle
			// terminal does not keep it alive.
			alive := s.sessions.Alive(sess.ID)
			if alive && lastInput.Load() > lastCheck.UnixNano() {
				_, alive = s.sessions.Get(sess.ID)
			}
			lastCheck = time.Now()
			if !alive {
				reason = "session ended"
				sendControl(terminalControl{Type: "error", Message: "Your session has ended. Sign in again to reconnect."})
				break loop
			}
		}
	}

	// Closing the terminal flushes remaining output and ends the reader.
	closeTerm()
	select {
	case <-outDone:
	case <-time.After(2 * time.Second):
	}
	if reason == "exited" {
		sendControl(terminalControl{Type: "exit", Code: &code})
		conn.Close(websocket.StatusNormalClosure, "shell exited")
	} else {
		conn.Close(websocket.StatusNormalClosure, reason)
	}
	s.log.Info("terminal closed", "id", c.ID, "name", c.Name, "user", sess.Username, "reason", reason,
		"exit_code", code, "duration", time.Since(start).Round(time.Second))
}

// sizeParam parses a terminal dimension, falling back to def.
func sizeParam(s string, def, lo, hi int) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil {
		n = def
	}
	return clamp(n, lo, hi)
}

func clamp(n, lo, hi int) uint16 {
	return uint16(max(lo, min(n, hi)))
}
