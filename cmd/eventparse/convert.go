package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The formats convert reads; "auto" detects one from the first line.
const (
	formatJSON    = "json"    // json, json-seq, json-sse: passed on as they are
	formatExport  = "export"  // KEY=VALUE blocks, all fields
	formatVerbose = "verbose" // a header with the cursor, then indented KEY=VALUE
	formatShort   = "short"   // short, -precise, -iso, -iso-precise, -full, -unix, -monotonic, -delta, with-unit
	formatCat     = "cat"     // the message only
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
	// verboseHeader matches the first line of an entry in --output=verbose.
	verboseHeader = regexp.MustCompile(`^\S.* \[(s=[0-9a-f]+;i=[0-9a-f]+;b=[0-9a-f]+;m=[0-9a-f]+;t=[0-9a-f]+;x=[0-9a-f]+)\]$`)
	// verboseField matches a field line in --output=verbose.
	verboseField = regexp.MustCompile(`^    ([A-Z0-9_]+)=(.*)$`)
	// marker matches journalctl's own lines, such as "-- Boot ... --".
	marker = regexp.MustCompile(`^-- .* --$`)
)

// convertStats counts what convert read.
type convertStats struct {
	Format  string
	Lines   int // lines read
	Entries int // entries written
	Skipped int // journalctl's own lines, such as -- Boot ... --
}

// converter turns journalctl output into --output=json lines.
type converter struct {
	r        *bufio.Reader
	w        io.Writer
	format   string
	priority int
	now      func() time.Time
	st       convertStats

	entry   map[string]string // being read
	last    string            // its field read last (verbose)
	prefix  int               // length of its prefix (short), for continued lines
	pending bool              // entry has fields not written yet
}

// convert reads journalctl output in format, or detects it with "auto",
// and writes each entry to w as a line of --output=json. Formats without
// a priority get priority; formats without a time get the time read. The
// short formats and cat do not say where a line comes from: their lines
// are taken as the output stream of host, identifier and PID, so the
// lines of a message are merged as for a container.
func convert(r io.Reader, w io.Writer, format string, priority int, now func() time.Time) (convertStats, error) {
	c := &converter{r: bufio.NewReaderSize(r, 64<<10), w: w, format: format, priority: priority, now: now}
	for {
		line, err := c.r.ReadString('\n')
		if line != "" {
			c.st.Lines++
			if err := c.line(strings.TrimSuffix(line, "\n")); err != nil {
				return c.st, err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return c.st, err
		}
	}
	return c.st, c.flush()
}

func (c *converter) line(line string) error {
	if c.format == "" || c.format == "auto" {
		if strings.TrimSpace(line) == "" {
			return nil
		}
		if marker.MatchString(line) {
			c.st.Skipped++
			return nil
		}
		c.format = detect(line)
	}
	c.st.Format = c.format
	switch c.format {
	case formatJSON:
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "\x1e"), "data:"))
		if line == "" {
			return nil
		}
		c.st.Entries++
		_, err := io.WriteString(c.w, line+"\n")
		return err
	case formatExport:
		return c.export(line)
	case formatVerbose:
		return c.verbose(line)
	case formatShort:
		return c.short(line)
	case formatCat:
		c.start()
		c.entry["MESSAGE"] = strings.TrimSuffix(line, "\r")
		return c.flush()
	}
	return fmt.Errorf("unknown format %q", c.format)
}

// detect names the format of the first line.
func detect(line string) string {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "{"), strings.HasPrefix(t, "\x1e"), strings.HasPrefix(t, "data:"):
		return formatJSON
	case strings.HasPrefix(line, "__CURSOR="):
		return formatExport
	case verboseHeader.MatchString(line):
		return formatVerbose
	case textPrefix.MatchString(line):
		return formatShort
	}
	return formatCat
}

// export reads a line of --output=export: KEY=VALUE, or a KEY followed by
// the length and bytes of a binary value; a blank line ends the entry.
func (c *converter) export(line string) error {
	if line == "" {
		return c.flush()
	}
	if !c.pending {
		c.start()
	}
	if k, v, ok := strings.Cut(line, "="); ok {
		c.entry[k] = v
		return nil
	}
	var n uint64
	if err := binary.Read(c.r, binary.LittleEndian, &n); err != nil {
		return fmt.Errorf("binary field %s: %w", line, err)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return fmt.Errorf("binary field %s: %w", line, err)
	}
	_, _ = c.r.ReadByte() // the newline after the value
	c.entry[line] = string(data)
	return nil
}

// verbose reads a line of --output=verbose. The cursor in the header holds
// the boot and the time; a line that is no field continues the value
// before, as a message with newlines is printed.
func (c *converter) verbose(line string) error {
	if m := verboseHeader.FindStringSubmatch(line); m != nil {
		if err := c.flush(); err != nil {
			return err
		}
		c.start()
		c.entry["__CURSOR"] = m[1]
		for _, part := range strings.Split(m[1], ";") {
			k, v, _ := strings.Cut(part, "=")
			switch k {
			case "b":
				c.entry["_BOOT_ID"] = v
			case "t":
				if usec, err := strconv.ParseInt(v, 16, 64); err == nil {
					c.entry["__REALTIME_TIMESTAMP"] = strconv.FormatInt(usec, 10)
				}
			}
		}
		return nil
	}
	if !c.pending {
		return nil
	}
	if m := verboseField.FindStringSubmatch(line); m != nil {
		c.entry[m[1]], c.last = m[2], m[1]
	} else if c.last != "" {
		c.entry[c.last] += "\n" + line
	}
	return nil
}

// short reads a line of a short format. A line without the prefix
// continues the message before, as journalctl prints a message with
// newlines.
func (c *converter) short(line string) error {
	line = strings.TrimSuffix(line, "\r")
	if marker.MatchString(line) {
		c.st.Skipped++
		return nil
	}
	m := textPrefix.FindStringSubmatch(line)
	if m == nil {
		if c.pending {
			pad := min(c.prefix, len(line)-len(strings.TrimLeft(line, " ")))
			c.entry["MESSAGE"] += "\n" + line[pad:]
		}
		return nil
	}
	if err := c.flush(); err != nil {
		return err
	}
	c.start()
	g := func(name string) string { return m[textPrefix.SubexpIndex(name)] }
	if t, ok := shortTime(g, c.now()); ok {
		c.entry["__REALTIME_TIMESTAMP"] = strconv.FormatInt(t.UnixMicro(), 10)
	}
	c.entry["_HOSTNAME"] = g("host")
	c.entry["SYSLOG_IDENTIFIER"] = g("ident")
	if pid := g("pid"); pid != "" {
		c.entry["_PID"] = pid
	}
	c.entry["_STREAM_ID"] = g("host") + "/" + g("ident") + "/" + g("pid")
	c.entry["MESSAGE"] = g("msg")
	c.prefix = len(line) - len(g("msg"))
	return nil
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

// start begins an entry with the fields every format needs.
func (c *converter) start() {
	c.entry = map[string]string{
		"__CURSOR":             fmt.Sprintf("eventparse;n=%d", c.st.Lines),
		"__REALTIME_TIMESTAMP": strconv.FormatInt(c.now().UnixMicro(), 10),
	}
	if c.format == formatShort || c.format == formatCat {
		c.entry["PRIORITY"] = strconv.Itoa(c.priority)
		c.entry["_TRANSPORT"] = "stdout"
		c.entry["_STREAM_ID"] = "stdin"
	}
	c.last, c.pending = "", true
}

// flush writes the entry read, if any.
func (c *converter) flush() error {
	if !c.pending {
		return nil
	}
	c.pending = false
	b, err := json.Marshal(c.entry)
	if err != nil {
		return err
	}
	c.st.Entries++
	_, err = c.w.Write(append(b, '\n'))
	return err
}
