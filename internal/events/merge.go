package events

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Docker's journald log driver, and journald for what a service writes to
// stdout or stderr, store one journal entry per line. A merger joins the
// lines of one message, such as a stack trace, into one event.
//
// A line continues the open message of its source when it comes at most
// mergeGap after the line before and looks like a continuation: indented,
// a closing bracket, blank, a known stack trace line, the text after a
// blank line (as in Node's report of an uncaught exception) or the
// exception line that ends a Python traceback. A line that starts like a
// log record (a bracket, a JSON object, a date or a level) always starts
// a new message, so separate messages logged at the same instant stay
// apart.

// mergeVersion is the version of the merge rules. When the stored events
// were merged by another version, they are imported again.
const mergeVersion = 1

// mergeGap is how long after the line before a line can still continue a
// message. The lines of one message come within milliseconds; the gap
// only bounds a merge, whether a line continues is decided by its text.
const mergeGap = time.Second

// maxMergeLines bounds a message, so a source that never stops writing
// continuation lines still has its events stored.
const maxMergeLines = 1000

var (
	// recordStart matches the lines that begin a log record.
	recordStart = regexp.MustCompile(`^(\[|\{|<\d|\d{4}-\d\d-\d\d|\d\d[./-]\d\d[./-]\d{2,4}|(ERROR|WARN(ING)?|INFO|DEBUG|CRITICAL|FATAL|TRACE)\b)`)
	// stackLine matches the lines of stack traces that are not indented.
	stackLine = regexp.MustCompile(`^(Caused by\b|\.\.\. \d+ (more|common frames omitted)|Traceback \(most recent call last\):|During handling of the above exception|The above exception was the direct cause|Node\.js v\d)`)
)

// merger joins the journal entries of one message into one event. It
// follows one run of the journal reader and is not safe for concurrent
// use.
type merger struct {
	open   map[string]*message // by source
	seq    int                 // entries read
	cursor string              // of the last entry read
}

// message is a message of which more lines may come.
type message struct {
	ev        Event // the first line
	lines     []string
	last      time.Time // journal time of the last line
	read      time.Time // when the last line was read
	blank     bool      // the last line is blank
	traceback bool      // in a Python traceback, before its exception line
	seq       int       // of the first entry
	before    string    // the cursor before the first entry
}

func newMerger() *merger { return &merger{open: map[string]*message{}} }

// add takes a line of journalctl --output=json, read at now, and returns
// the events it completes: its own when it cannot be continued, and the
// message of its source it does not continue.
func (m *merger) add(line []byte, now time.Time) []Event {
	e, cursor, ok := ParseJournal(line)
	before := m.cursor
	if cursor != "" {
		m.cursor = cursor
		m.seq++
	}
	if !ok {
		return nil
	}
	if e.source == "" {
		return []Event{e}
	}
	var done []Event
	if msg := m.open[e.source]; msg != nil {
		if msg.continues(e) {
			msg.add(e, now)
			if len(msg.lines) < maxMergeLines {
				return nil
			}
			delete(m.open, e.source)
			return []Event{msg.event()}
		}
		done = append(done, msg.event())
	}
	msg := &message{ev: e, seq: m.seq, before: before}
	msg.add(e, now)
	m.open[e.source] = msg
	return done
}

// expire returns the messages whose last line was read more than mergeGap
// before now.
func (m *merger) expire(now time.Time) []Event {
	return m.take(func(msg *message) bool { return now.Sub(msg.read) > mergeGap })
}

// close returns all open messages.
func (m *merger) close() []Event {
	return m.take(func(*message) bool { return true })
}

func (m *merger) take(done func(*message) bool) []Event {
	var msgs []*message
	for src, msg := range m.open {
		if done(msg) {
			msgs = append(msgs, msg)
			delete(m.open, src)
		}
	}
	slices.SortFunc(msgs, func(a, b *message) int { return a.seq - b.seq })
	evs := make([]Event, len(msgs))
	for i, msg := range msgs {
		evs[i] = msg.event()
	}
	return evs
}

// resume is the cursor to read on from so nothing is lost: the one before
// the oldest open message, else the last. It is "" when there is none yet.
func (m *merger) resume() string {
	var oldest *message
	for _, msg := range m.open {
		if oldest == nil || msg.seq < oldest.seq {
			oldest = msg
		}
	}
	if oldest != nil {
		return oldest.before
	}
	return m.cursor
}

// continues reports whether e is a further line of the message.
func (msg *message) continues(e Event) bool {
	line := e.Message
	switch {
	case e.Time.Sub(msg.last) > mergeGap:
		return false
	case strings.TrimSpace(line) == "", strings.ContainsAny(line[:1], " \t}])"):
		return true
	case recordStart.MatchString(line):
		return false
	}
	return stackLine.MatchString(line) || msg.blank || msg.traceback
}

func (msg *message) add(e Event, now time.Time) {
	line := e.Message
	msg.lines = append(msg.lines, line)
	msg.ev.Priority = min(msg.ev.Priority, e.Priority)
	msg.last, msg.read = e.Time, now
	msg.blank = strings.TrimSpace(line) == ""
	switch {
	case strings.HasPrefix(line, "Traceback (most recent call last):"):
		msg.traceback = true
	case msg.traceback && !msg.blank && !strings.ContainsAny(line[:1], " \t"):
		msg.traceback = false
	}
}

func (msg *message) event() Event {
	e := msg.ev
	e.Message = strings.TrimRight(strings.Join(msg.lines, "\n"), " \t\n")
	e.PriorityName = PriorityName(e.Priority)
	return e
}
