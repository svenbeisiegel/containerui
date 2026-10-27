package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/events"
)

// newEventsEnv is a signed-in env with an event store holding three
// events: two of docker.service and one of the kernel.
func newEventsEnv(t *testing.T) (*env, *events.Store) {
	t.Helper()
	st, err := events.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = st.Add([]events.Event{
		{ID: "e1", Time: now.Add(-3 * time.Hour), Priority: 3, PriorityName: "err", Component: "docker.service", Unit: "docker.service", Message: "failed to pull <image>"},
		{ID: "e2", Time: now.Add(-2 * time.Hour), Priority: 2, PriorityName: "crit", Component: "kernel", Transport: "kernel", Message: "Out of memory: Killed process 1234"},
		{ID: "e3", Time: now.Add(-1 * time.Hour), Priority: 3, PriorityName: "err", Component: "docker.service", Message: "network error"},
	}, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, func(o *Options) { o.Events = st })
	e.login()
	return e, st
}

func TestEventsHiddenWithoutStore(t *testing.T) {
	e := newEnv(t)
	e.login()
	if body := e.do("GET", "/", nil).Body.String(); strings.Contains(body, `href="/events"`) {
		t.Error("the navigation links the Events page without events")
	}
	if w := e.do("GET", "/events", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET /events = %d", w.Code)
	}
}

func TestEventsPage(t *testing.T) {
	e, _ := newEventsEnv(t)
	body := e.do("GET", "/", nil).Body.String()
	if !strings.Contains(body, `href="/events"`) || !strings.Contains(body, `title="3 unread"`) {
		t.Errorf("navigation lacks the Events link with 3 unread:\n%s", body)
	}
	w := e.do("GET", "/events", nil)
	body = w.Body.String()
	if w.Code != http.StatusOK || strings.Count(body, `<details class="event unread"`) != 3 {
		t.Fatalf("GET /events = %d:\n%s", w.Code, body)
	}
	if strings.Index(body, "network error") > strings.Index(body, "failed to pull") {
		t.Error("events are not listed newest first")
	}
	if !strings.Contains(body, "failed to pull &lt;image&gt;") {
		t.Error("the message is not escaped")
	}
	if !strings.Contains(body, `<option value="docker.service" >docker.service (2, 2 unread)</option>`) {
		t.Errorf("component options missing:\n%s", body)
	}

	cases := []struct{ query, has, hasNot string }{
		{"component=kernel", "Out of memory", "network error"},
		{"q=PULL", "failed to pull", "Out of memory"},
		{"component=nope", "No events match the filter.", "network error"},
	}
	for _, c := range cases {
		body := e.do("GET", "/partials/events?"+c.query, nil, htmx).Body.String()
		if !strings.Contains(body, c.has) || strings.Contains(body, c.hasNot) {
			t.Errorf("%s: want %q and not %q:\n%s", c.query, c.has, c.hasNot, body)
		}
	}
}

func TestEventOpenMarksRead(t *testing.T) {
	e, st := newEventsEnv(t)
	if w := e.do("POST", "/events/e2/open", url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("open without CSRF token = %d", w.Code)
	}
	w := e.post("/events/e2/open", nil, htmx)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `<details class="event" id="event-e2" open`) || !strings.Contains(body, "by admin at") {
		t.Fatalf("open = %d:\n%s", w.Code, body)
	}
	if w.Header().Get("HX-Trigger") != "events-changed" {
		t.Error("opening did not announce the change")
	}
	if ev, _ := st.Get("e2"); ev.ReadBy != "admin" {
		t.Errorf("e2 not read: %+v", ev)
	}
	if body := e.do("GET", "/partials/events/unread", nil, htmx).Body.String(); !strings.Contains(body, `title="2 unread"`) {
		t.Errorf("unread badge: %s", body)
	}
	if w := e.post("/events/e2/open", nil, htmx); w.Header().Get("HX-Trigger") != "" {
		t.Error("opening a read event announced a change")
	}
	if w := e.post("/events/nope/open", nil, htmx); w.Code != http.StatusNotFound {
		t.Errorf("open unknown = %d", w.Code)
	}
}

func TestEventsMarkAllRead(t *testing.T) {
	e, st := newEventsEnv(t)
	w := e.post("/events/read", url.Values{"all": {"1"}, "component": {"docker.service"}}, htmx)
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, "2 events marked as read.") {
		t.Fatalf("mark all = %d:\n%s", w.Code, body)
	}
	if st.Unread() != 1 {
		t.Errorf("unread = %d, want 1 (the kernel event)", st.Unread())
	}
	w = e.post("/events/read", url.Values{"ids": {"e2"}}, htmx)
	if !strings.Contains(w.Body.String(), "1 event marked as read.") || st.Unread() != 0 {
		t.Errorf("mark by id: %d unread:\n%s", st.Unread(), w.Body.String())
	}
	if body := e.do("GET", "/", nil).Body.String(); regexp.MustCompile(`title="\d+ unread"`).MatchString(body) {
		t.Error("the navigation still shows an unread count")
	}
}

func TestEventGroup(t *testing.T) {
	e, st := newEventsEnv(t)
	at := time.Now().Add(-time.Minute)
	line := func(id string, after time.Duration, msg string) events.Event {
		return events.Event{ID: id, Time: at.Add(after), Priority: 3, PriorityName: "err", Component: "docker.service",
			Container: "web", PID: 7, Message: msg}
	}
	_ = st.Add([]events.Event{
		line("s1", 0, ""),
		line("s2", time.Millisecond, "Error: boom"),
		line("s3", 2*time.Millisecond, "    at main (/app/index.js:1:1)"),
	}, "cursor2")
	body := e.do("GET", "/events", nil).Body.String()
	if strings.Count(body, `<details class="event unread"`) != 4 || !strings.Contains(body, `<span class="event-text">Error: boom</span><span class="event-more">+2 lines</span>`) ||
		!strings.Contains(body, "docker.service · web") {
		t.Fatalf("the trace is not one event:\n%s", body)
	}
	if !strings.Contains(body, "\nError: boom\n    at main") {
		t.Error("the trace is not in order in the details")
	}
	if w := e.post("/events/s1/open", nil, htmx); !strings.Contains(w.Body.String(), `<details class="event" id="event-s1" open`) {
		t.Fatalf("open:\n%s", w.Body.String())
	}
	if ev, _ := st.Get("s3"); !ev.Read() || st.Unread() != 3 {
		t.Errorf("opening did not mark the whole group read: %+v, %d unread", ev, st.Unread())
	}
}
