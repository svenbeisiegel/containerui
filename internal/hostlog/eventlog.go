package hostlog

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

var eventPriorities = []Option{
	{"", "All levels"},
	{"1", "Critical"},
	{"2", "Error and worse"},
	{"3", "Warning and worse"},
	{"4", "Information and worse"},
}

var eventRanges = []Option{
	{"24h", "Last 24 hours"},
	{"boot", "Since boot"},
	{"1h", "Last hour"},
	{"7d", "Last 7 days"},
	{"", "Entire channel"},
}

var rangeDurations = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

// eventChannelMatch selects the container-related channels offered besides
// System and Application.
var eventChannelMatch = []string{"hyper-v-compute", "containers-", "lxss", "wsl", "host-network-service"}

// eventChannels turns `wevtutil el` output into options. Debug and analytic
// channels are left out: they cannot be queried while enabled.
func eventChannels(out string) []Option {
	opts := []Option{{"System", "System"}, {"Application", "Application"}}
	var extra []string
	for _, ch := range strings.Split(out, "\n") {
		ch = strings.TrimSpace(ch)
		l := strings.ToLower(ch)
		if ch == "" || strings.HasSuffix(l, "/debug") || strings.HasSuffix(l, "/analytic") || strings.HasSuffix(l, "-analytic") {
			continue
		}
		for _, m := range eventChannelMatch {
			if strings.Contains(l, m) {
				extra = append(extra, ch)
				break
			}
		}
	}
	slices.Sort(extra)
	for _, ch := range slices.Compact(extra) {
		opts = append(opts, Option{ch, ch})
	}
	return opts
}

// eventXPath builds the wevtutil query for a validated q. boot is the time
// since boot; after and upTo bound the record IDs when non-zero.
func eventXPath(q Query, boot time.Duration, after, upTo uint64) string {
	var conds []string
	switch q.Priority {
	case "":
	case "4":
		conds = append(conds, "Level<=4") // 0 is "log always", shown as information
	default:
		conds = append(conds, "Level>=1 and Level<="+q.Priority)
	}
	since := rangeDurations[q.Since]
	if q.Since == "boot" {
		since = boot
	}
	if since > 0 {
		conds = append(conds, fmt.Sprintf("TimeCreated[timediff(@SystemTime)<=%d]", since.Milliseconds()))
	}
	if after > 0 {
		conds = append(conds, "EventRecordID>"+strconv.FormatUint(after, 10))
	}
	if upTo > 0 {
		conds = append(conds, "EventRecordID<="+strconv.FormatUint(upTo, 10))
	}
	if len(conds) == 0 {
		return "*"
	}
	return "*[System[" + strings.Join(conds, " and ") + "]]"
}

// event is the part of wevtutil's RenderedXml that is shown.
type event struct {
	System struct {
		Provider struct {
			Name string `xml:"Name,attr"`
		}
		EventID     string
		Level       int
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		}
		EventRecordID uint64
	}
	RenderingInfo struct {
		Message string
		Level   string
	}
}

var levelNames = map[int]string{1: "Critical", 2: "Error", 3: "Warning", 4: "Information", 5: "Verbose"}

// line formats e like journalctl's short-iso output:
// "2026-10-16T08:51:23+0200 Error Provider[7031]: message". Further message
// lines are indented.
func (e *event) line(loc *time.Location) string {
	ts := e.System.TimeCreated.SystemTime
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.In(loc).Format("2006-01-02T15:04:05-0700")
	}
	level := strings.TrimSpace(e.RenderingInfo.Level)
	if level == "" {
		level = levelNames[e.System.Level]
		if level == "" {
			level = "Information"
		}
	}
	var msg []string
	for _, l := range strings.Split(strings.ReplaceAll(e.RenderingInfo.Message, "\r", ""), "\n") {
		if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) != "" {
			msg = append(msg, l)
		}
	}
	return fmt.Sprintf("%s %s %s[%s]: %s", ts, level, e.System.Provider.Name, strings.TrimSpace(e.System.EventID), strings.Join(msg, "\n    "))
}

// decodeEvents calls fn for each Event element in r (wevtutil /e:Events
// output, UTF-8 or UTF-16 with BOM).
func decodeEvents(r io.Reader, fn func(*event) error) error {
	d := xml.NewDecoder(newBOMReader(r))
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Event" {
			continue
		}
		var e event
		if err := d.DecodeElement(&e, &se); err != nil {
			return fmt.Errorf("read event: %w", err)
		}
		if err := fn(&e); err != nil {
			return err
		}
	}
}

// newBOMReader returns r as UTF-8: UTF-16LE input (wevtutil /uni:true)
// is recognised by its byte order mark and converted.
func newBOMReader(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if b, err := br.Peek(2); err == nil && b[0] == 0xFF && b[1] == 0xFE {
		_, _ = br.Discard(2)
		return &utf16Reader{r: br}
	}
	return br
}

// utf16Reader converts UTF-16LE to UTF-8.
type utf16Reader struct {
	r    *bufio.Reader
	high rune // pending high surrogate
	out  []byte
}

func (u *utf16Reader) Read(p []byte) (int, error) {
	for len(u.out) == 0 {
		var b [2]byte
		if _, err := io.ReadFull(u.r, b[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			return 0, err
		}
		c := rune(b[0]) | rune(b[1])<<8
		switch {
		case utf16.IsSurrogate(c) && c < 0xDC00:
			u.high = c
			continue
		case utf16.IsSurrogate(c):
			c = utf16.DecodeRune(u.high, c)
			u.high = 0
		}
		u.out = utf8.AppendRune(u.out, c)
	}
	n := copy(p, u.out)
	u.out = u.out[n:]
	return n, nil
}
