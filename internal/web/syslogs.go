package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/svenbeisiegel/containerui/internal/hostlog"
)

// sysLogsView is the System logs panel of the System page.
type sysLogsView struct {
	Label         string // "Journal", "Event Log"
	SelectorLabel string // "Unit", "Channel"
	Selectors     []hostlog.Option
	Priorities    []hostlog.Option
	Ranges        []hostlog.Option
	ExportExt     string // ".evtx" when the native format can be downloaded
	Err           string
}

// sysLogsPanel describes the host's system log for the System page, or nil
// if the platform has none.
func (s *Server) sysLogsPanel(ctx context.Context) *sysLogsView {
	src := s.hostlog
	if src == nil {
		return nil
	}
	v := &sysLogsView{Label: src.Label(), SelectorLabel: src.SelectorLabel(), Priorities: src.Priorities(), Ranges: src.Ranges()}
	if ex, ok := src.(hostlog.Exporter); ok {
		v.ExportExt = ex.ExportExt()
	}
	if err := src.Available(); err != nil {
		v.Err = err.Error()
		return v
	}
	sels, err := src.Selectors(ctx)
	if err != nil {
		s.log.Warn("list system log selectors", "error", err)
		v.Err = userError(err)
		return v
	}
	v.Selectors = sels
	return v
}

// sysLogTarget reads and checks the query of a system log request. On
// failure it has answered the request and returns ok false.
func (s *Server) sysLogTarget(w http.ResponseWriter, r *http.Request) (hostlog.Source, hostlog.Query, bool) {
	src := s.hostlog
	if src == nil {
		http.Error(w, "This platform has no supported system log.", http.StatusNotFound)
		return nil, hostlog.Query{}, false
	}
	if err := src.Available(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return nil, hostlog.Query{}, false
	}
	v := r.URL.Query()
	q := hostlog.Query{Selector: v.Get("sel"), Priority: v.Get("prio"), Since: v.Get("since")}
	if !v.Has("since") {
		q.Since = src.Ranges()[0].Value
	}
	if err := hostlog.Validate(r.Context(), src, q); err != nil {
		if errors.Is(err, hostlog.ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, userError(err), http.StatusBadGateway)
		}
		return nil, hostlog.Query{}, false
	}
	return src, q, true
}

// sysLogParams is q as query parameters.
func sysLogParams(q hostlog.Query) string {
	return url.Values{"sel": {q.Selector}, "prio": {q.Priority}, "since": {q.Since}}.Encode()
}

func sysLogAttrs(src hostlog.Source, q hostlog.Query) []any {
	return []any{"log", src.Kind(), "selector", q.Selector, "priority", q.Priority, "since", q.Since}
}

// sysLogFileName is "<host>_journal[_<unit>]_<timestamp><ext>" or
// "<host>_<channel>_<timestamp><ext>".
func sysLogFileName(host string, src hostlog.Source, q hostlog.Query, ext string, now time.Time) string {
	name := host
	if name == "" {
		name = "host"
	}
	switch {
	case src.Kind() == "journal":
		name += "_journal"
		if q.Selector != "" {
			name += "_" + q.Selector
		}
	case q.Selector != "":
		name += "_" + q.Selector
	}
	return unsafeFileChars.ReplaceAllString(name, "_") + "_" + now.Format("20060102-150405") + ext
}

// handleSysLogsWatch renders the live page of the host's system log.
func (s *Server) handleSysLogsWatch(w http.ResponseWriter, r *http.Request) {
	src, q, ok := s.sysLogTarget(w, r)
	if !ok {
		return
	}
	sels, _ := src.Selectors(r.Context())
	title := src.Label()
	if q.Selector != "" {
		title += " · " + q.Selector
	}
	host, _ := os.Hostname()
	s.renderLogWatch(w, logWatchView{
		Title: title,
		Meta: []metaItem{
			{"Host", host},
			{src.SelectorLabel(), hostlog.Label(sels, q.Selector)},
			{"Priority", hostlog.Label(src.Priorities(), q.Priority)},
			{"Starting from", hostlog.Label(src.Ranges(), q.Since)},
		},
		StreamURL: "/system/logs/stream?" + sysLogParams(q),
	})
}

// handleSysLogsStream follows the host's system log as Server-Sent Events.
func (s *Server) handleSysLogsStream(w http.ResponseWriter, r *http.Request) {
	src, q, ok := s.sysLogTarget(w, r)
	if !ok {
		return
	}
	q.Follow, q.Tail = true, watchTail(r)
	s.serveLogStream(w, r, "stream "+src.Kind(), "The log stream ended.", sysLogAttrs(src, q),
		func(ctx context.Context, w io.Writer) error { return src.Read(ctx, q, w) })
}

// handleSysLogsDownload sends the selected system log entries as a text
// file, or with format=native in the log's own file format.
func (s *Server) handleSysLogsDownload(w http.ResponseWriter, r *http.Request) {
	src, q, ok := s.sysLogTarget(w, r)
	if !ok {
		return
	}
	host, _ := os.Hostname()
	if r.URL.Query().Get("format") == "native" {
		ex, ok := src.(hostlog.Exporter)
		if !ok {
			http.Error(w, "This log has no native file format.", http.StatusBadRequest)
			return
		}
		s.serveLogDownload(w, r, sysLogFileName(host, src, q, ex.ExportExt(), time.Now()), "application/octet-stream",
			append(sysLogAttrs(src, q), "format", "native"),
			func(ctx context.Context, w io.Writer) error { return ex.Export(ctx, q, w) })
		return
	}
	s.serveLogDownload(w, r, sysLogFileName(host, src, q, ".log", time.Now()), "text/plain; charset=utf-8", sysLogAttrs(src, q),
		func(ctx context.Context, w io.Writer) error { return src.Read(ctx, q, w) })
}
