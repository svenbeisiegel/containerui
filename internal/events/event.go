// Package events keeps the host's error events: it follows the systemd
// journal for entries of priority err and worse, stores them on disk for
// Retention and remembers which of them an administrator has read.
//
// On disk, in the events folder:
//
//	events-YYYY-MM-DD.jsonl  the events of one UTC day, one JSON object per line
//	reads-YYYY-MM-DD.jsonl   who read which event of that day, and when
//	state.json               the journal time to resume from
//
// The journal is read as journalctl --output=short-iso-precise (see
// short.go), which has the exact time, the identifier and the PID of each
// line. Containers and the stdout of services log one journal entry per
// line; the lines of one message, such as a stack trace, are merged into
// one event as they are collected, and messages whose log level is below
// error are dropped (see merge.go).
//
// Day files make the retention a matter of deleting files, and JSON Lines
// can be searched with grep or jq, e.g. by component:
//
//	jq -c 'select(.component=="discovery")' events-*.jsonl
package events

import (
	"strconv"
	"strings"
	"time"
)

// Retention is how long events are kept.
const Retention = 7 * 24 * time.Hour

// Event is one error entry of the system log.
type Event struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Priority     int       `json:"priority"`
	PriorityName string    `json:"priority_name"`
	// Component is what the event is filtered by: the syslog identifier,
	// which is the process or, for a container, the tag of its log driver.
	Component  string `json:"component"`
	Identifier string `json:"identifier,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Host       string `json:"host,omitempty"`
	Message    string `json:"message"`

	// source is the stream whose lines are merged into messages: host,
	// identifier and PID.
	source string

	// Who read the event and when; kept in the reads files.
	ReadBy string    `json:"-"`
	ReadAt time.Time `json:"-"`
}

// Read reports whether somebody has read the event.
func (e Event) Read() bool { return !e.ReadAt.IsZero() }

// Summary is the first line of the message that is not blank.
func (e Event) Summary() string {
	s, _ := e.summary()
	return s
}

// More is the number of lines of the message after the summary.
func (e Event) More() int {
	_, more := e.summary()
	return more
}

func (e Event) summary() (line string, more int) {
	lines := strings.Split(e.Message, "\n")
	for i, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			return s, len(lines) - i - 1
		}
	}
	return "", 0
}

// day is the UTC day the event is filed under.
func (e Event) day() string { return e.Time.UTC().Format(dayLayout) }

const dayLayout = "2006-01-02"

// priorityNames are the syslog names of the priorities kept.
var priorityNames = []string{"emerg", "alert", "crit", "err"}

// MaxPriority is the least severe priority kept: err.
const MaxPriority = 3

// PriorityName returns the syslog name of p.
func PriorityName(p int) string {
	if p >= 0 && p < len(priorityNames) {
		return priorityNames[p]
	}
	return strconv.Itoa(p)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
