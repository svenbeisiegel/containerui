package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	// maxLogStreams bounds concurrently followed logs (each runs a CLI
	// process).
	maxLogStreams = 16
	// logWatchTail is how many earlier lines a log watch starts with.
	logWatchTail = 100
	// downloadHold is how much download output is held back before the
	// response is committed, so an immediate runtime error can still be
	// answered with an error status instead of a log file containing it.
	downloadHold = 4 << 10
	// sseMaxLine forces out a line that has no newline after this many bytes.
	sseMaxLine = 64 << 10
)

// logStreamTick is how often a followed log sends a keep-alive comment and
// re-checks the login session. Tests shorten it.
var logStreamTick = 15 * time.Second

// logsTarget runs the pre-checks shared by the log endpoints.
func (s *Server) logsTarget(ctx context.Context, id string) (*runtime.Container, int, string) {
	if !s.caps().Logs {
		return nil, http.StatusNotImplemented, "The " + s.rt.Name() + " runtime does not provide container logs."
	}
	return s.findContainer(ctx, id)
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// logFileName is "<container-name>_<timestamp>.log".
func logFileName(c *runtime.Container, now time.Time) string {
	name := unsafeFileChars.ReplaceAllString(c.Name, "_")
	if name == "" || name == "_" {
		name = shortID(c.ID)
	}
	return name + "_" + now.Format("20060102-150405") + ".log"
}

// handleLogsDownload sends the container's complete log as a text file.
func (s *Server) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	c, status, msg := s.logsTarget(r.Context(), r.PathValue("id"))
	if msg != "" {
		http.Error(w, msg, status)
		return
	}
	user := sessionFrom(r).Username
	// A large log can take longer than the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	hw := &heldWriter{w: w, header: func(h http.Header) {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Disposition", `attachment; filename="`+logFileName(c, time.Now())+`"`)
	}}
	err := s.rt.ContainerLogs(r.Context(), c.ID, runtime.LogsOpts{}, hw)
	if err != nil && !hw.committed {
		s.log.Warn("logs download failed", "id", c.ID, "name", c.Name, "user", user, "error", err)
		status := http.StatusBadGateway
		if errors.Is(err, runtime.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, userError(err), status)
		return
	}
	if ferr := hw.flush(); err == nil {
		err = ferr
	}
	if err != nil {
		s.log.Warn("logs download incomplete", "id", c.ID, "name", c.Name, "user", user, "bytes", hw.n, "error", err)
		return
	}
	s.log.Info("logs downloaded", "id", c.ID, "name", c.Name, "user", user, "bytes", hw.n)
}

// heldWriter holds back the first downloadHold bytes; the response headers
// are only sent once that much arrived or flush is called.
type heldWriter struct {
	w         http.ResponseWriter
	header    func(http.Header)
	buf       []byte
	committed bool
	n         int64 // bytes written to w
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if !h.committed {
		h.buf = append(h.buf, p...)
		if len(h.buf) < downloadHold {
			return len(p), nil
		}
		return len(p), h.flush()
	}
	n, err := h.w.Write(p)
	h.n += int64(n)
	return n, err
}

func (h *heldWriter) flush() error {
	if !h.committed {
		h.committed = true
		h.header(h.w.Header())
		h.w.WriteHeader(http.StatusOK)
	}
	if len(h.buf) == 0 {
		return nil
	}
	n, err := h.w.Write(h.buf)
	h.n += int64(n)
	h.buf = nil
	return err
}

// logWatchView is the data of the logs_watch page.
type logWatchView struct {
	C           *runtime.Container
	StreamURL   string
	Opened      time.Time
	Favicon     string
	FaviconType string
}

// handleLogsWatch renders the live log page; its script reads the stream.
func (s *Server) handleLogsWatch(w http.ResponseWriter, r *http.Request) {
	c, status, msg := s.logsTarget(r.Context(), r.PathValue("id"))
	if msg != "" {
		http.Error(w, msg, status)
		return
	}
	v := logWatchView{C: c, StreamURL: "/containers/" + c.ID + "/logs/stream", Opened: time.Now(), Favicon: s.favicon, FaviconType: s.favType}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.logsPage.ExecuteTemplate(w, "logs_watch", v); err != nil {
		s.log.Error("render page", "page", "logs_watch", "error", err)
	}
}

// handleLogsStream follows the container's output as Server-Sent Events:
// unnamed events carry log lines (one data field per line), a final "end"
// event carries the reason the stream stopped.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	c, status, msg := s.logsTarget(r.Context(), r.PathValue("id"))
	if msg != "" {
		http.Error(w, msg, status)
		return
	}
	tail := logWatchTail
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = max(0, min(n, 10000))
		}
	}
	if s.logStreams.Add(1) > maxLogStreams {
		s.logStreams.Add(-1)
		http.Error(w, "Too many open log streams. Close one and try again.", http.StatusServiceUnavailable)
		return
	}
	defer s.logStreams.Add(-1)

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w, rc: rc}
	// The log goroutine may outlive the handler by a moment; it must not
	// write to a finished response.
	defer sw.close()
	if err := sw.comment("stream " + c.ID); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	start := time.Now()
	s.log.Info("log watch opened", "id", c.ID, "name", c.Name, "user", sess.Username)
	done := make(chan error, 1)
	go func() {
		done <- s.rt.ContainerLogs(ctx, c.ID, runtime.LogsOpts{Follow: true, Tail: tail}, sw)
	}()

	ticker := time.NewTicker(logStreamTick)
	defer ticker.Stop()
	reason, endMsg := "", ""
	finished := false
loop:
	for {
		select {
		case err := <-done:
			finished = true
			switch {
			case ctx.Err() != nil:
				reason = "disconnected"
			case err != nil:
				reason, endMsg = "error", userError(err)
			default:
				reason, endMsg = "ended", "The log stream ended; the container is no longer running."
			}
			break loop
		case <-ctx.Done():
			reason = "disconnected"
			break loop
		case <-ticker.C:
			if !s.sessions.Alive(sess.ID) {
				reason, endMsg = "session ended", "Your session has ended. Sign in again to keep watching."
				break loop
			}
			if sw.comment("ping") != nil {
				reason = "disconnected"
				break loop
			}
		}
	}
	// Cancelling ends the runtime's log process; wait for its last output
	// before the end event.
	cancel()
	if !finished {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if endMsg != "" {
		_ = sw.end(endMsg)
	}
	s.log.Info("log watch closed", "id", c.ID, "name", c.Name, "user", sess.Username, "reason", reason,
		"duration", time.Since(start).Round(time.Second))
}

// sseWriter turns raw log output into Server-Sent Events. Complete lines
// are sent as they arrive; a trailing partial line waits for its newline.
// It is safe for concurrent use.
type sseWriter struct {
	mu      sync.Mutex
	w       io.Writer
	rc      *http.ResponseController
	partial []byte
	err     error
}

func (s *sseWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.partial = append(s.partial, p...)
	var chunk []byte
	switch cut := bytes.LastIndexByte(s.partial, '\n'); {
	case cut >= 0:
		chunk = bytes.TrimSuffix(s.partial[:cut], []byte("\r"))
		s.partial = append([]byte(nil), s.partial[cut+1:]...)
	case len(s.partial) >= sseMaxLine:
		chunk, s.partial = s.partial, nil
	default:
		return len(p), nil
	}
	if err := s.send("", chunk); err != nil {
		return 0, err
	}
	return len(p), nil
}

// end sends any held partial line and then the end event.
func (s *sseWriter) end(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if partial := bytes.TrimRight(s.partial, "\r"); len(partial) > 0 {
		if err := s.send("", partial); err != nil {
			return err
		}
	}
	s.partial = nil
	return s.send("end", []byte(reason))
}

// close makes all further writes fail.
func (s *sseWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = errStreamClosed
	}
}

var errStreamClosed = errors.New("log stream closed")

// comment sends an SSE comment line (ignored by the browser).
func (s *sseWriter) comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return s.flush(fmt.Appendf(nil, ": %s\n\n", text))
}

// send writes one event; every line of data becomes its own data field.
// SSE treats CR, LF and CRLF as line ends alike, so all three split lines.
// Must be called with mu held.
func (s *sseWriter) send(event string, data []byte) error {
	var b bytes.Buffer
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	for _, line := range bytes.Split(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.flush(b.Bytes())
}

// flush writes b and flushes it to the client. Must be called with mu held.
func (s *sseWriter) flush(b []byte) error {
	if _, err := s.w.Write(b); err != nil {
		s.err = err
		return err
	}
	if err := s.rc.Flush(); err != nil {
		s.err = err
		return err
	}
	return nil
}
