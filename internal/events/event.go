// Package events keeps the host's error events: it follows the systemd
// journal for entries of priority err and worse, stores them on disk for
// Retention and remembers which of them an administrator has read.
//
// On disk, in the events folder:
//
//	events-YYYY-MM-DD.jsonl  the events of one UTC day, one JSON object per line
//	reads-YYYY-MM-DD.jsonl   who read which event of that day, and when
//	state.json               the journal cursor to resume from
//
// The lines a process writes together, such as a stack trace on stderr,
// are journal entries of their own. The store groups them: entries of the
// same boot, component, container and PID, each at most groupGap after the
// one before, make one Group, which the Events page shows as one event.
//
// Day files make the retention a matter of deleting files, and JSON Lines
// can be searched with grep or jq, e.g. by component:
//
//	jq -c 'select(.component=="docker.service")' events-*.jsonl
package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Boot         string    `json:"boot,omitempty"`
	Priority     int       `json:"priority"`
	PriorityName string    `json:"priority_name"`
	// Component is what the event is filtered by: the systemd unit, else
	// the syslog identifier, else the command.
	Component  string `json:"component"`
	Unit       string `json:"unit,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	// Container is the container name the Docker journald log driver
	// sets; its entries all have the PID of dockerd.
	Container string `json:"container,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Host      string `json:"host,omitempty"`
	Transport string `json:"transport,omitempty"`
	Message   string `json:"message"`

	// Who read the event and when; kept in the reads files.
	ReadBy string    `json:"-"`
	ReadAt time.Time `json:"-"`
}

// Read reports whether somebody has read the event.
func (e Event) Read() bool { return !e.ReadAt.IsZero() }

// groupGap is how long after the previous entry of the same source an
// entry still belongs to its group.
const groupGap = time.Second

// groupKey is the source of an event, for grouping.
type groupKey struct {
	boot, component, container string
	pid                        int
}

func (e *Event) groupKey() groupKey {
	return groupKey{e.Boot, e.Component, e.Container, e.PID}
}

// Group is the events one source logged together, oldest first, such as
// the lines of a stack trace. It has at least one event.
type Group struct {
	Events []*Event
}

// ID is the ID of the first event.
func (g Group) ID() string { return g.Events[0].ID }

// First is the oldest event.
func (g Group) First() Event { return *g.Events[0] }

// Last is the newest event.
func (g Group) Last() Event { return *g.Events[len(g.Events)-1] }

// Lines is the number of events.
func (g Group) Lines() int { return len(g.Events) }

// More is the number of events after the first.
func (g Group) More() int { return len(g.Events) - 1 }

// IDs are the IDs of the events.
func (g Group) IDs() []string {
	ids := make([]string, len(g.Events))
	for i, e := range g.Events {
		ids[i] = e.ID
	}
	return ids
}

// Message is the messages of the events, one per line.
func (g Group) Message() string {
	msgs := make([]string, len(g.Events))
	for i, e := range g.Events {
		msgs[i] = e.Message
	}
	return strings.Join(msgs, "\n")
}

// Summary is the first line of the message that is not blank.
func (g Group) Summary() string {
	for _, e := range g.Events {
		for line := range strings.Lines(e.Message) {
			if s := strings.TrimSpace(line); s != "" {
				return s
			}
		}
	}
	return ""
}

// Priority is the most severe priority of the events.
func (g Group) Priority() int {
	p := g.Events[0].Priority
	for _, e := range g.Events[1:] {
		p = min(p, e.Priority)
	}
	return p
}

// PriorityName is the syslog name of the group's priority.
func (g Group) PriorityName() string { return PriorityName(g.Priority()) }

// Read reports whether somebody has read all events of the group.
func (g Group) Read() bool {
	for _, e := range g.Events {
		if !e.Read() {
			return false
		}
	}
	return true
}

// clone copies the events, so the group can be used outside the lock.
func (g *Group) clone() Group {
	evs := make([]*Event, len(g.Events))
	for i, e := range g.Events {
		c := *e
		evs[i] = &c
	}
	return Group{Events: evs}
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

// ParseJournal turns one line of `journalctl --output=json` into an event.
// It returns the entry's cursor, which is set even when the entry is not
// kept, and ok false for entries that are not error events.
func ParseJournal(line []byte) (e Event, cursor string, ok bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, "", false
	}
	field := func(name string) string { return journalValue(raw[name]) }
	cursor = field("__CURSOR")
	if cursor == "" {
		return Event{}, "", false
	}
	prio, err := strconv.Atoi(field("PRIORITY"))
	if err != nil || prio < 0 || prio > MaxPriority {
		return Event{}, cursor, false
	}
	usec, err := strconv.ParseInt(field("__REALTIME_TIMESTAMP"), 10, 64)
	if err != nil {
		return Event{}, cursor, false
	}
	sum := sha256.Sum256([]byte(cursor))
	e = Event{
		ID:           hex.EncodeToString(sum[:8]),
		Time:         time.UnixMicro(usec).UTC(),
		Boot:         field("_BOOT_ID"),
		Priority:     prio,
		PriorityName: PriorityName(prio),
		Unit:         firstNonEmpty(field("_SYSTEMD_UNIT"), field("_SYSTEMD_USER_UNIT")),
		Identifier:   field("SYSLOG_IDENTIFIER"),
		Container:    field("CONTAINER_NAME"),
		Host:         field("_HOSTNAME"),
		Transport:    field("_TRANSPORT"),
		Message:      strings.TrimRight(field("MESSAGE"), "\n"),
	}
	e.PID, _ = strconv.Atoi(firstNonEmpty(field("_PID"), field("SYSLOG_PID")))
	e.Component = component(e, field("_COMM"))
	return e, cursor, true
}

// component names what an event came from, for filtering.
func component(e Event, comm string) string {
	switch {
	case e.Unit != "":
		return e.Unit
	case e.Identifier != "":
		return e.Identifier
	case e.Transport == "kernel":
		return "kernel"
	case comm != "":
		return comm
	}
	return "unknown"
}

// journalValue decodes a journal JSON field: a string, an array of bytes
// for values that are not valid UTF-8, or an array of those for a field set
// more than once (the first value is used). Null and missing are "".
func journalValue(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var b []byte
	var nums []int
	if json.Unmarshal(v, &nums) == nil {
		for _, n := range nums {
			b = append(b, byte(n))
		}
		return strings.ToValidUTF8(string(b), "�")
	}
	var many []json.RawMessage
	if json.Unmarshal(v, &many) == nil && len(many) > 0 {
		return journalValue(many[0])
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
