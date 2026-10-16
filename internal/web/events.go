package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/events"
)

// eventsPageSize is how many events the Events page shows at first and
// how many more each "Show more" adds.
const eventsPageSize = 200

// eventsView is the list of the Events page.
type eventsView struct {
	Filter     events.Filter
	Components []events.Component
	Events     []events.Event
	Total      int    // events matching the filter
	Unread     int    // unread events of all
	Unmarked   int    // unread events matching the filter
	Limit      int    // events shown at most
	Query      string // the filter as query parameters
	Flash      *flash
}

// More is the number of events the "Show more" link shows.
func (v eventsView) More() int { return v.Limit + eventsPageSize }

// eventsFilter reads the filter of an events request: component, q
// (search text) and unread, and the number of events to show, n.
func eventsFilter(r *http.Request) (events.Filter, int) {
	v := r.URL.Query()
	if r.Method == http.MethodPost {
		v = url.Values{"component": {r.PostFormValue("component")}, "q": {r.PostFormValue("q")}, "unread": {r.PostFormValue("unread")}}
	}
	f := events.Filter{
		Component: v.Get("component"),
		Text:      strings.TrimSpace(v.Get("q")),
		Unread:    v.Get("unread") != "",
	}
	if len(f.Text) > 200 {
		f.Text = f.Text[:200]
	}
	n, err := strconv.Atoi(v.Get("n"))
	if err != nil || n < eventsPageSize {
		n = eventsPageSize
	}
	return f, n
}

func eventsQuery(f events.Filter) string {
	v := url.Values{}
	if f.Component != "" {
		v.Set("component", f.Component)
	}
	if f.Text != "" {
		v.Set("q", f.Text)
	}
	if f.Unread {
		v.Set("unread", "1")
	}
	return v.Encode()
}

func (s *Server) loadEvents(f events.Filter, limit int) eventsView {
	list, total := s.events.List(f, limit)
	uf := f
	uf.Unread = true
	_, unmarked := s.events.List(uf, 1)
	return eventsView{
		Filter: f, Components: s.events.Components(), Events: list, Total: total,
		Unread: s.events.Unread(), Unmarked: unmarked, Limit: limit, Query: eventsQuery(f),
	}
}

// requireEvents answers 404 when no events are collected.
func (s *Server) requireEvents(w http.ResponseWriter) bool {
	if s.events == nil {
		http.Error(w, "Error events are only collected on Linux with the systemd journal.", http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	f, n := eventsFilter(r)
	s.render(w, "events", s.newPage(r, "Events", "events", s.loadEvents(f, n)))
}

func (s *Server) handleEventsPartial(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	f, n := eventsFilter(r)
	s.renderPartial(w, "events_list", s.loadEvents(f, n))
}

// handleEventsUnread renders the unread count of the navigation.
func (s *Server) handleEventsUnread(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	s.renderPartial(w, "events_unread", s.events.Unread())
}

// handleEventOpen marks an event read as its details are opened and
// renders it again, open and read.
func (s *Server) handleEventOpen(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.events.Get(id); !ok {
		http.Error(w, "unknown event", http.StatusNotFound)
		return
	}
	user := sessionFrom(r).Username
	if n, err := s.events.MarkRead(user, id); err != nil {
		s.log.Warn("save event read state", "error", err)
	} else if n > 0 {
		w.Header().Set("HX-Trigger", "events-changed")
	}
	e, _ := s.events.Get(id)
	s.renderPartial(w, "event_item", map[string]any{"E": e, "Open": true})
}

// handleEventsRead marks the selected events read: those in the ids
// field, or with all=1 every unread event matching the filter. It renders
// the list again.
func (s *Server) handleEventsRead(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	f, n := eventsFilter(r)
	user := sessionFrom(r).Username
	var marked int
	var err error
	if r.PostFormValue("all") != "" {
		marked, err = s.events.MarkAllRead(user, f)
	} else {
		_ = r.ParseForm()
		marked, err = s.events.MarkRead(user, r.PostForm["ids"]...)
	}
	v := s.loadEvents(f, n)
	if err != nil {
		s.log.Warn("save event read state", "error", err)
		v.Flash = errFlash(err)
	} else {
		s.log.Info("events marked read", "user", user, "count", marked)
		v.Flash = okFlash("%d %s marked as read.", marked, pluralWord(marked, "event"))
	}
	w.Header().Set("HX-Trigger", "events-changed")
	if !isHTMX(r) {
		redirect(w, r, "/events?"+v.Query)
		return
	}
	s.renderPartial(w, "events_list", v)
}

func pluralWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
