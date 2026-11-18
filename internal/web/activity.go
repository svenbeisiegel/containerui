package web

import (
	"errors"
	"net/http"

	"github.com/svenbeisiegel/containerui/internal/jobs"
)

// ---- activity drawers of the Images, Volumes and Networks tabs ----
//
// Each tab has a drawer on the right edge (like the compose output) that
// lists what was done there recently, by any user: image pulls run as
// background jobs, the other actions finish within the request and are
// recorded as finished jobs.

// Job kinds of the recorded actions; image pulls use pullJobKind.
const (
	pullJobKind    = "pull"
	imageJobKind   = "image"
	volumeJobKind  = "volume"
	networkJobKind = "network"
)

// maxActivity is how many entries a drawer shows.
const maxActivity = 10

type activityTab struct {
	Name  string // URL segment and session key part
	Title string
	Kinds []string
}

var activityTabs = map[string]activityTab{
	"images":   {Name: "images", Title: "Image activity", Kinds: []string{pullJobKind, imageJobKind}},
	"volumes":  {Name: "volumes", Title: "Volume activity", Kinds: []string{volumeJobKind}},
	"networks": {Name: "networks", Title: "Network activity", Kinds: []string{networkJobKind}},
}

// tabOfKind maps a job kind to the tab whose drawer shows it.
func tabOfKind(kind string) string {
	for name, t := range activityTabs {
		for _, k := range t.Kinds {
			if k == kind {
				return name
			}
		}
	}
	return ""
}

// activityView is a tab's drawer.
type activityView struct {
	Tab        string
	Title      string
	Entries    []jobs.Job // newest first
	AnyRunning bool       // an entry runs (keeps the body polling)
	Open       bool       // render the drawer open
	Partial    bool       // rendered as an htmx partial: update the toggle badge out of band
	CSRF       string
}

// Newest is the latest entry, for the status dot on the edge handle.
func (v activityView) Newest() *jobs.Job {
	if len(v.Entries) == 0 {
		return nil
	}
	return &v.Entries[0]
}

// sessActivityCleared (uint64) holds the Seq of the newest entry when the
// user cleared the drawer; older entries are not shown to them. A sequence
// rather than a time, since the clock can give an action recorded right
// after the clear the same time (Windows).
func sessActivityCleared(tab string) string { return "activity-cleared-" + tab }
func sessActivityOpen(tab string) string    { return "activity-open-" + tab }

// activityTabFor returns the tab of the request path, or answers 404 if it
// is unknown or the runtime cannot do what the tab shows.
func (s *Server) activityTabFor(w http.ResponseWriter, r *http.Request) (activityTab, bool) {
	t, ok := activityTabs[r.PathValue("tab")]
	caps := s.caps()
	if !ok || (t.Name == "volumes" && !caps.Volumes) || (t.Name == "networks" && !caps.Networks) {
		http.NotFound(w, r)
		return activityTab{}, false
	}
	return t, true
}

// loadActivity builds a tab's drawer; it renders open once after an action
// redirected to the page.
func (s *Server) loadActivity(r *http.Request, tab string) activityView {
	t := activityTabs[tab]
	sess := sessionFrom(r)
	v := activityView{Tab: t.Name, Title: t.Title, CSRF: sess.CSRF}
	v.Open = sess.Pop(sessActivityOpen(tab)) != nil
	cleared, _ := sess.Get(sessActivityCleared(tab)).(uint64)
	for _, j := range s.jobs.List(t.Kinds...) { // newest first
		if !j.Done() {
			v.AnyRunning = true
		}
		if j.Seq() > cleared && len(v.Entries) < maxActivity {
			v.Entries = append(v.Entries, j)
		}
	}
	return v
}

// recordActivity adds a finished action to its tab's drawer. msg is shown on
// success, errMsg (already user-facing) marks it failed; out holds output
// lines. For htmx requests the drawer reloads and opens via HX-Trigger; set
// redirecting when the response is a redirect, so the next page opens it.
func (s *Server) recordActivity(w http.ResponseWriter, r *http.Request, kind, target, msg, errMsg string, out []string, redirecting bool) {
	var err error
	lines := out
	if errMsg != "" {
		err = errors.New(errMsg)
	} else if msg != "" {
		lines = append([]string{msg}, out...)
	}
	s.jobs.Record(kind, target, sessionFrom(r).Username, lines, err)
	s.openActivity(w, r, tabOfKind(kind), redirecting)
}

// openActivity opens a tab's drawer: right away for an htmx partial
// response, on the next page for a redirect.
func (s *Server) openActivity(w http.ResponseWriter, r *http.Request, tab string, redirecting bool) {
	if isHTMX(r) && !redirecting {
		w.Header().Set("HX-Trigger", "activity-changed")
		return
	}
	sessionFrom(r).Set(sessActivityOpen(tab), true)
}

func (s *Server) handleActivityPartial(w http.ResponseWriter, r *http.Request) {
	t, ok := s.activityTabFor(w, r)
	if !ok {
		return
	}
	v := s.loadActivity(r, t.Name)
	v.Open = false // the open state lives in the page, not the polled body
	v.Partial = true
	if t.Name == "images" && !v.AnyRunning && isHTMX(r) {
		// The polling body stops after this response; let the image table
		// reload once to show newly pulled images.
		w.Header().Set("HX-Trigger", "images-changed")
	}
	s.renderPartial(w, "activity_body", v)
}

// handleActivityClear hides the shown entries for this user until newer
// actions happen.
func (s *Server) handleActivityClear(w http.ResponseWriter, r *http.Request) {
	t, ok := s.activityTabFor(w, r)
	if !ok {
		return
	}
	var newest uint64
	if l := s.jobs.List(t.Kinds...); len(l) > 0 {
		newest = l[0].Seq()
	}
	sessionFrom(r).Set(sessActivityCleared(t.Name), newest)
	if !isHTMX(r) {
		redirect(w, r, "/"+t.Name)
		return
	}
	v := s.loadActivity(r, t.Name)
	v.Open = false
	v.Partial = true
	s.renderPartial(w, "activity_body", v)
}
