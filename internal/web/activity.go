package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/jobs"
)

// ---- the activity side bar ----
//
// The Compose, Containers, Images, Volumes and Networks pages share a
// drawer on the right edge that lists what was done through ContainerUI
// recently, by any user: image pulls, compose actions and container
// recreates run as background jobs, the other actions finish within the
// request and are recorded as finished jobs. The jobs manager keeps the
// newest jobs.MaxHistory of them, also across restarts.

// Job kinds of the recorded actions.
const (
	pullJobKind      = "pull"
	imageJobKind     = "image"
	volumeJobKind    = "volume"
	networkJobKind   = "network"
	containerJobKind = "container"
	// composeJobKind (compose.go) and jobRecreate (recreate.go) are jobs too.
)

// activityKinds are the job kinds the side bar lists, with their labels.
var activityKinds = []struct{ Kind, Label string }{
	{pullJobKind, "Image pull"},
	{imageJobKind, "Image"},
	{volumeJobKind, "Volume"},
	{networkJobKind, "Network"},
	{containerJobKind, "Container"},
	{jobRecreate, "Recreate"},
	{composeJobKind, "Compose"},
}

func allActivityKinds() []string {
	out := make([]string, len(activityKinds))
	for i, k := range activityKinds {
		out[i] = k.Kind
	}
	return out
}

// activityPages are the nav items (page.Active) whose pages show the side
// bar; the Dashboard, System and Events pages do not.
var activityPages = []string{"compose", "containers", "images", "volumes", "networks"}

// activityEntry is a job shown in the side bar.
type activityEntry struct {
	jobs.Job
	Label string // what kind of action it is
}

// activityView is the side bar.
type activityView struct {
	Entries    []activityEntry // newest first
	AnyRunning bool            // an entry runs (keeps the body polling)
	Open       bool            // render the drawer open
	Partial    bool            // rendered as an htmx partial: update the toggle badge out of band
	Return     string          // page to return to after Clear without JavaScript
	CSRF       string
}

// Newest is the latest entry, for the status dot on the edge handle.
func (v activityView) Newest() *activityEntry {
	if len(v.Entries) == 0 {
		return nil
	}
	return &v.Entries[0]
}

// Session keys of the side bar.
const (
	// sessActivityCleared (uint64) holds the Seq of the newest entry when
	// the user cleared the list; older entries are not shown to them. A
	// sequence rather than a time, since the clock can give an action
	// recorded right after the clear the same time (Windows).
	sessActivityCleared = "activity-cleared"
	// sessActivityOpen: open the drawer on the next page, set when an
	// action that opens it redirects.
	sessActivityOpen = "activity-open"
)

// loadActivity builds the side bar; it renders open once after an action
// redirected to the page.
func (s *Server) loadActivity(r *http.Request) activityView {
	sess := sessionFrom(r)
	v := activityView{CSRF: sess.CSRF}
	v.Open = sess.Pop(sessActivityOpen) != nil
	cleared, _ := sess.Get(sessActivityCleared).(uint64)
	for _, j := range s.jobs.List(allActivityKinds()...) { // newest first
		if !j.Done() {
			v.AnyRunning = true
		}
		if j.Seq() > cleared {
			v.Entries = append(v.Entries, activityEntry{Job: j, Label: activityLabel(j.Kind)})
		}
	}
	return v
}

func activityLabel(kind string) string {
	for _, k := range activityKinds {
		if k.Kind == kind {
			return k.Label
		}
	}
	return kind
}

// pageActivity is the side bar of a full page, nil on pages without one.
func (s *Server) pageActivity(r *http.Request, active string) *activityView {
	if sessionFrom(r) == nil || !slices.Contains(activityPages, active) {
		return nil
	}
	v := s.loadActivity(r)
	if r.Method == http.MethodGet {
		v.Return = r.URL.RequestURI()
	}
	return &v
}

// recordActivity adds a finished action to the side bar. msg is shown on
// success, errMsg (already user-facing) marks it failed; out holds output
// lines. With open, the side bar opens: for htmx requests right away (set
// redirecting when the response is a redirect, so the next page opens it).
// Without, an htmx page only refreshes the list.
func (s *Server) recordActivity(w http.ResponseWriter, r *http.Request, kind, target, msg, errMsg string, out []string, redirecting, open bool) {
	var err error
	lines := out
	if errMsg != "" {
		err = errors.New(errMsg)
	} else if msg != "" {
		lines = append([]string{msg}, out...)
	}
	s.jobs.Record(kind, target, sessionFrom(r).Username, lines, err)
	switch {
	case open:
		s.openActivity(w, r, redirecting)
	case isHTMX(r) && !redirecting:
		w.Header().Set("HX-Trigger", "activity-recorded")
	}
}

// openActivity opens the side bar: right away for an htmx partial
// response, on the next page for a redirect.
func (s *Server) openActivity(w http.ResponseWriter, r *http.Request, redirecting bool) {
	if isHTMX(r) && !redirecting {
		w.Header().Set("HX-Trigger", "activity-changed")
		return
	}
	sessionFrom(r).Set(sessActivityOpen, true)
}

// handleActivityPartial renders the list. The body polls with running=1
// while an entry runs; the poll that finds them all finished lets the
// image and compose views reload once to show the results.
func (s *Server) handleActivityPartial(w http.ResponseWriter, r *http.Request) {
	v := s.loadActivity(r)
	v.Open = false // the open state lives in the page, not the polled body
	v.Partial = true
	if !v.AnyRunning && isHTMX(r) && r.URL.Query().Get("running") == "1" {
		w.Header().Set("HX-Trigger", "images-changed, compose-changed")
	}
	s.renderPartial(w, "activity_body", v)
}

// handleActivityClear hides the shown entries for this user until newer
// actions happen.
func (s *Server) handleActivityClear(w http.ResponseWriter, r *http.Request) {
	var newest uint64
	if l := s.jobs.List(allActivityKinds()...); len(l) > 0 {
		newest = l[0].Seq()
	}
	sessionFrom(r).Set(sessActivityCleared, newest)
	if !isHTMX(r) {
		redirect(w, r, localReturn(r.PostFormValue("return"), "/containers"))
		return
	}
	v := s.loadActivity(r)
	v.Open = false
	v.Partial = true
	s.renderPartial(w, "activity_body", v)
}

// localReturn is to if it is a path of this site, else def.
func localReturn(to, def string) string {
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.ContainsAny(to, "\\\r\n") {
		return def
	}
	return to
}
