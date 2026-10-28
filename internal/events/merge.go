package events

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Docker's journald log driver, and journald for what a service writes to
// stdout or stderr, store one journal entry per line. A merger joins the
// lines of one message, such as a stack trace, into one event. The lines
// of one host, identifier and PID are taken as one stream.
//
// The further lines of an entry with newlines, which journalctl prints
// indented below it, always belong to its message. An entry continues the
// open message of its stream when it comes at most mergeGap after the
// line before and looks like a continuation: indented, a closing bracket,
// blank, a known stack trace line, the text after a blank line (as in
// Node's report of an uncaught exception) or the exception line that ends
// a Python traceback. A line that starts like a log record (a bracket, a
// JSON object, a date or a level) always starts a new message, so separate
// messages logged at the same instant stay apart.
//
// The journal has everything a container writes to stderr at err, also
// its warnings and log lines, so the level a message gives itself decides:
// a message whose first line names a level below error ([warn ], [log   ],
// INFO:root, level=info) is dropped with all its lines. A message that
// names no level is kept as an error.

// mergeVersion is the version of the merge rules. When the stored events
// were merged by another version, they are imported again.
const mergeVersion = 2

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
	// levelWord matches an upper case word at the start of a record, after
	// an optional time: ERROR:root:..., WARN ...
	levelWord = regexp.MustCompile(`^(?:\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(?:[.,]\d+)?(?:Z|[+-]\d\d:?\d\d)?\s+)?([A-Z]+)\b`)
	// levelField matches a level field: level=info, "level":"info".
	levelField = regexp.MustCompile(`(?:^|[\s{,"])(?:level|lvl|severity)"?\s*[=:]\s*"?([A-Za-z]+)`)
)

// levels are the syslog priorities of the level names messages use.
var levels = map[string]int{
	"emerg": 0, "emergency": 0, "alert": 1,
	"crit": 2, "critical": 2, "fatal": 2, "panic": 2,
	"err": 3, "error": 3, "severe": 3,
	"warn": 4, "warning": 4, "notice": 5,
	"info": 6, "log": 6, "debug": 7, "trace": 7, "verbose": 7,
}

// messageLevel returns the priority of the level a record names in its
// first line: in one of its leading [...] groups, as an upper case word at
// its start, or in a level field.
func messageLevel(line string) (int, bool) {
	rest := line
	for strings.HasPrefix(rest, "[") {
		group, after, ok := strings.Cut(rest[1:], "]")
		if !ok {
			break
		}
		if p, ok := levels[strings.ToLower(strings.TrimSpace(group))]; ok {
			return p, true
		}
		rest = strings.TrimLeft(after, " \t")
	}
	for _, re := range []*regexp.Regexp{levelWord, levelField} {
		if m := re.FindStringSubmatch(rest); m != nil {
			if p, ok := levels[strings.ToLower(m[1])]; ok {
				return p, true
			}
		}
	}
	return 0, false
}

// merger joins the journal entries of one message into one event. It
// follows one run of the journal reader and is not safe for concurrent
// use.
type merger struct {
	open    map[string]*message // by source
	entry   *message            // the message of the entry read last, for its further lines
	pad     int                 // the prefix length of that entry
	seq     int                 // entries read
	read    time.Time           // the latest journal time read
	dropped int                 // messages dropped for their level
	sample  string              // the first line that is not journalctl output
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
}

func newMerger() *merger { return &merger{open: map[string]*message{}} }

// add takes a line of journalctl --output=short-iso-precise, read at now,
// and returns the events it completes: the message of its source it does
// not continue, or its own once it has maxMergeLines.
func (m *merger) add(line string, now time.Time) []Event {
	line = strings.TrimRight(line, "\r\n")
	e, prefix, ok := parseShort(line, now)
	if !ok {
		if m.entry == nil {
			if m.sample == "" && strings.TrimSpace(line) != "" && !marker.MatchString(line) {
				m.sample = line
			}
			return nil
		}
		if msg := m.entry; m.open[msg.ev.source] == msg {
			pad := min(m.pad, len(line)-len(strings.TrimLeft(line, " ")))
			return m.extend(msg, line[pad:], msg.last, now)
		}
		return nil
	}
	m.seq++
	if e.Time.After(m.read) {
		m.read = e.Time
	}
	m.pad = prefix
	var done []Event
	if msg := m.open[e.source]; msg != nil {
		if msg.continues(e) {
			m.entry = msg
			return m.extend(msg, e.Message, e.Time, now)
		}
		delete(m.open, e.source)
		done = m.events(msg)
	}
	msg := &message{ev: e, seq: m.seq}
	msg.add(e.Message, e.Time, now)
	m.open[e.source] = msg
	m.entry = msg
	return done
}

// extend adds a line to an open message and returns it when it is full.
func (m *merger) extend(msg *message, line string, t, now time.Time) []Event {
	msg.add(line, t, now)
	if len(msg.lines) < maxMergeLines {
		return nil
	}
	delete(m.open, msg.ev.source)
	return m.events(msg)
}

// events returns the events of the messages that are errors by their
// level.
func (m *merger) events(msgs ...*message) []Event {
	var evs []Event
	for _, msg := range msgs {
		if e, ok := msg.event(); ok {
			evs = append(evs, e)
		} else {
			m.dropped++
		}
	}
	return evs
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
	return m.events(msgs...)
}

// resume is the journal time to read on from so nothing is lost: the
// first line of the oldest open message, else just after the latest line
// read. It is zero before the first line.
func (m *merger) resume() time.Time {
	var oldest time.Time
	for _, msg := range m.open {
		if oldest.IsZero() || msg.ev.Time.Before(oldest) {
			oldest = msg.ev.Time
		}
	}
	if !oldest.IsZero() || m.read.IsZero() {
		return oldest
	}
	return m.read.Add(time.Microsecond)
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

func (msg *message) add(line string, t, now time.Time) {
	msg.lines = append(msg.lines, line)
	msg.last, msg.read = t, now
	msg.blank = strings.TrimSpace(line) == ""
	switch {
	case strings.HasPrefix(line, "Traceback (most recent call last):"):
		msg.traceback = true
	case msg.traceback && !msg.blank && !strings.ContainsAny(line[:1], " \t"):
		msg.traceback = false
	}
}

// event returns the message as an event, and false when the level it
// names is below error.
func (msg *message) event() (Event, bool) {
	e := msg.ev
	e.Message = strings.TrimRight(strings.Join(msg.lines, "\n"), " \t\n")
	if p, ok := messageLevel(msg.lines[0]); ok {
		e.Priority = p
	}
	e.PriorityName = PriorityName(e.Priority)
	return e, e.Priority <= MaxPriority
}
