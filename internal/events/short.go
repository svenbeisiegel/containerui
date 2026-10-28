package events

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// textPrefix matches the prefix journalctl's short formats put before
	// a message: a time, the host and the identifier with its PID.
	textPrefix = regexp.MustCompile(`^(?:` +
		`(?P<iso>\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d))` +
		`|(?P<full>[A-Z][a-z]{2} \d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)? \S+)` +
		`|(?P<short>[A-Z][a-z]{2} [ \d]\d \d\d:\d\d:\d\d(?:\.\d+)?)` +
		`|(?P<unix>\d{9,}\.\d+)` +
		`|(?P<mono>\[\s*\d+\.\d+(?:\s*<\s*\d+\.\d+>)?\])` +
		`) (?P<host>\S+) (?P<ident>[^\s\[]+?)(?:\[(?P<pid>\d+)\])?:(?: (?P<msg>.*))?$`)
	// marker matches journalctl's own lines, such as "-- Boot ... --".
	marker = regexp.MustCompile(`^-- .* --$`)
)

// parseShort reads a line of journalctl's short formats; ContainerUI reads
// short-iso-precise. It returns the entry and the length of its prefix, to
// which journalctl indents the further lines of a message with newlines.
// ok is false for lines without the prefix: those further lines and
// journalctl's own lines.
//
// journalctl ran with --priority=err, so every entry is an error; its
// level is decided by its message (see messageLevel). Formats without a
// year take the latest one not after now; the monotonic ones have no time
// and get now.
func parseShort(line string, now time.Time) (e Event, prefix int, ok bool) {
	m := textPrefix.FindStringSubmatch(line)
	if m == nil {
		return Event{}, 0, false
	}
	g := func(name string) string { return m[textPrefix.SubexpIndex(name)] }
	t, ok := shortTime(g, now)
	if !ok {
		t = now
	}
	e = Event{
		Time:         t.UTC(),
		Priority:     MaxPriority,
		PriorityName: PriorityName(MaxPriority),
		Component:    firstNonEmpty(g("ident"), "unknown"),
		Identifier:   g("ident"),
		Host:         g("host"),
		Message:      g("msg"),
		source:       g("host") + "/" + g("ident") + "/" + g("pid"),
	}
	e.PID, _ = strconv.Atoi(g("pid"))
	// the same line read again, after a restart, gets the same id
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", e.source, e.Time.UnixMicro(), e.Message))
	e.ID = hex.EncodeToString(sum[:8])
	return e, len(line) - len(e.Message), true
}

// shortTime reads the time of a short format; the monotonic ones have
// none.
func shortTime(g func(string) string, now time.Time) (time.Time, bool) {
	parse := func(s string, layouts ...string) (time.Time, bool) {
		for _, l := range layouts {
			if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	switch {
	case g("iso") != "":
		return parse(g("iso"), "2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z0700")
	case g("full") != "":
		return parse(g("full"), "Mon 2006-01-02 15:04:05.999999999 MST")
	case g("short") != "":
		t, ok := parse(g("short"), "Jan 02 15:04:05.999999999", "Jan _2 15:04:05.999999999")
		if !ok {
			return t, false
		}
		// the year is not printed: the latest one not in the future
		t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
		if t.After(now.Add(24 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		return t, true
	case g("unix") != "":
		sec, frac, _ := strings.Cut(g("unix"), ".")
		s, err := strconv.ParseInt(sec, 10, 64)
		frac = (frac + "000000")[:6]
		us, err2 := strconv.ParseInt(frac, 10, 64)
		return time.Unix(s, us*1000), err == nil && err2 == nil
	}
	return time.Time{}, false
}
