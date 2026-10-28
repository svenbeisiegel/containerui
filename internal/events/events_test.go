package events

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortLine is a journalctl --output=short-iso-precise line of ident.
func shortLine(t time.Time, ident, msg string) string {
	return t.In(time.FixedZone("CET", 3600)).Format("2006-01-02T15:04:05.000000-07:00") + " host " + ident + ": " + msg
}

func TestEventSummary(t *testing.T) {
	cases := []struct {
		message, summary string
		more             int
	}{
		{"boom", "boom", 0},
		{"", "", 0},
		{"Error: boom\n    at main (/app/index.js:1:1)\n\nNode.js v26", "Error: boom", 3},
		{"\n  \n  Traceback:\n  x", "Traceback:", 1},
	}
	for _, c := range cases {
		e := Event{Message: c.message}
		if e.Summary() != c.summary || e.More() != c.more {
			t.Errorf("%q: summary %q more %d, want %q %d", c.message, e.Summary(), e.More(), c.summary, c.more)
		}
	}
}

func TestParseShort(t *testing.T) {
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.FixedZone("CET", 3600)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 27, 22, 1, 27, 553124000, time.UTC)
	cases := []struct {
		name, line         string
		at                 time.Time
		component, message string
		pid                int
	}{
		{"short-iso-precise", "2026-10-27T23:01:27.553124+01:00 appliance-1 discovery[962]: Error: connect ECONNREFUSED",
			at, "discovery", "Error: connect ECONNREFUSED", 962},
		{"short-precise", "Oct 27 23:01:27.553124 appliance-1 discovery[962]:   errno: -111,",
			at, "discovery", "  errno: -111,", 962},
		{"short", "Oct 27 23:01:27 appliance-1 discovery[962]: }",
			at.Truncate(time.Second), "discovery", "}", 962},
		{"kernel", "2026-10-27T23:01:27.553124+0100 appliance-1 kernel: Out of memory",
			at, "kernel", "Out of memory", 0},
		{"empty message", "2026-10-27T23:01:27.553124+01:00 appliance-1 app[7]:", at, "app", "", 7},
	}
	for _, c := range cases {
		e, prefix, ok := parseShort(c.line, now)
		if !ok {
			t.Errorf("%s: not parsed", c.name)
			continue
		}
		if !e.Time.Equal(c.at) || e.Component != c.component || e.Identifier != c.component || e.Message != c.message || e.PID != c.pid ||
			e.Host != "appliance-1" || e.Priority != 3 || e.PriorityName != "err" || len(e.ID) != 16 {
			t.Errorf("%s: got %+v", c.name, e)
		}
		if !strings.HasSuffix(c.line[:prefix], ":") && !strings.HasSuffix(c.line[:prefix], ": ") || c.line[prefix:] != c.message {
			t.Errorf("%s: prefix %q", c.name, c.line[:prefix])
		}
	}
	for _, line := range []string{"                                                      at main", "-- Boot fc000899846a450d9a37cba81a853732 --", "plain text", ""} {
		if _, _, ok := parseShort(line, now); ok {
			t.Errorf("%q parsed as an entry", line)
		}
	}
	a, _, _ := parseShort(cases[0].line, now)
	b, _, _ := parseShort(cases[0].line, now.Add(time.Hour))
	c, _, _ := parseShort(strings.Replace(cases[0].line, "553124", "553125", 1), now)
	if a.ID != b.ID || a.ID == c.ID {
		t.Errorf("the id must depend on the line only: %q %q %q", a.ID, b.ID, c.ID)
	}
}

func TestMessageLevel(t *testing.T) {
	cases := []struct {
		line string
		prio int
		ok   bool
	}{
		{"[27.10.2026 - 23:01:27.553] [0] [error ] [events.js:185] Error: connect ECONNREFUSED", 3, true},
		{"[27.10.2026 - 23:01:27.555] [0] [log   ] [events.js:194] stop() connection", 6, true},
		{"[27.10.2026 - 00:00:00.000] [0] [warn ] probe C", 4, true},
		{"[info] 2026-10-27T10:35:58.447313Z couchdb@couchdb-0.local <0.43.0> -------- SIGTERM received", 6, true},
		{"[error] 2026-10-27T11:00:37.297932Z couchdb@couchdb-0.local gen_server terminated", 3, true},
		{"ERROR:root:probe E", 3, true},
		{"INFO:root:started", 6, true},
		{"2026-10-27 22:40:20,123 WARNING something", 4, true},
		{"FATAL: database is gone", 2, true},
		{`{"level":"error","msg":"probe G json"}`, 3, true},
		{`{"msg":"x","level":"debug"}`, 7, true},
		{"time=2026-10-27T22:40:20Z level=info msg=started", 6, true},
		{"Error: probe A", 0, false},
		{"TypeError: fetch failed", 0, false},
		{"Log in failed for admin", 0, false},
		{"[x] next", 0, false},
		{"loglevel=info is not supported", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		if p, ok := messageLevel(c.line); p != c.prio || ok != c.ok {
			t.Errorf("%q: %d %v, want %d %v", c.line, p, ok, c.prio, c.ok)
		}
	}
}

func TestJournalFollowArgs(t *testing.T) {
	since := time.Date(2026, 10, 27, 23, 1, 27, 553124000, time.FixedZone("CET", 3600))
	got := strings.Join(journalFollowArgs(since), " ")
	if !strings.HasPrefix(got, "--priority=err --output=short-iso-precise ") || !strings.Contains(got, " --follow ") ||
		!strings.HasSuffix(got, " --since=2026-10-27 22:01:27.553124 UTC") {
		t.Errorf("args: %q", got)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func ev(id string, t time.Time, component, msg string) Event {
	return Event{ID: id, Time: t, Priority: 3, PriorityName: "err", Component: component, Message: msg}
}

func ids(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, err := open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Since().IsZero() {
		t.Fatalf("new store has since %v", s.Since())
	}
	day := 24 * time.Hour
	err = s.Add([]Event{
		ev("a", clk.t.Add(-2*day), "docker.service", "pull failed"),
		ev("b", clk.t.Add(-time.Hour), "kernel", "Out of memory"),
		ev("old", clk.t.Add(-8*day), "kernel", "too old"),
		ev("c", clk.t.Add(-3*day), "sshd.service", "auth failure"), // out of order
	}, clk.t.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add([]Event{ev("b", clk.t, "kernel", "duplicate")}, clk.t); err != nil {
		t.Fatal(err)
	}
	all, total := s.List(Filter{}, 0)
	if want := []string{"b", "a", "c"}; !slices.Equal(ids(all), want) || total != 3 {
		t.Fatalf("List = %v (%d), want %v", ids(all), total, want)
	}
	if n, err := s.MarkRead("alice", "a", "a", "unknown"); n != 1 || err != nil {
		t.Fatalf("MarkRead = %d, %v", n, err)
	}
	if n, _ := s.MarkRead("bob", "a"); n != 0 {
		t.Errorf("marking a read event again marked %d", n)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	want := []string{"events-2026-10-13.jsonl", "events-2026-10-14.jsonl", "events-2026-10-16.jsonl", "reads-2026-10-14.jsonl", "state.json"}
	if !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "events-2026-10-16.jsonl"))
	if !strings.Contains(string(data), `"component":"kernel"`) || strings.Count(string(data), "\n") != 1 {
		t.Errorf("day file: %s", data)
	}

	s2, err := open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Since().Equal(clk.t) {
		t.Errorf("since after reopen = %v", s2.Since())
	}
	a, ok := s2.Get("a")
	if !ok || !a.Read() || a.ReadBy != "alice" || !a.ReadAt.Equal(clk.t) {
		t.Errorf("read state after reopen: %+v", a)
	}
	if s2.Unread() != 2 {
		t.Errorf("Unread = %d", s2.Unread())
	}

	// six days later a and c are past the retention, b is not
	clk.t = clk.t.Add(6 * day)
	if err := s2.Prune(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.List(Filter{}, 0); !slices.Equal(ids(got), []string{"b"}) {
		t.Errorf("after prune: %v", ids(got))
	}
	for _, f := range []string{"events-2026-10-13.jsonl", "events-2026-10-14.jsonl", "reads-2026-10-14.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s not deleted: %v", f, err)
		}
	}
}

func TestStoreFilter(t *testing.T) {
	now := time.Now()
	s, err := open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Add([]Event{
		ev("1", now.Add(-4*time.Minute), "docker.service", "Container failed"),
		ev("2", now.Add(-3*time.Minute), "kernel", "Out of memory: killed process"),
		ev("3", now.Add(-2*time.Minute), "docker.service", "network error"),
		ev("4", now.Add(-1*time.Minute), "sshd.service", "error: kex"),
	}, now)
	_, _ = s.MarkRead("u", "3")
	cases := []struct {
		f     Filter
		limit int
		want  []string
		total int
	}{
		{Filter{Component: "docker.service"}, 0, []string{"3", "1"}, 2},
		{Filter{Text: "ERROR"}, 0, []string{"4", "3"}, 2},
		{Filter{Text: "docker"}, 0, []string{"3", "1"}, 2},
		{Filter{Unread: true}, 0, []string{"4", "2", "1"}, 3},
		{Filter{Component: "docker.service", Unread: true}, 0, []string{"1"}, 1},
		{Filter{}, 2, []string{"4", "3"}, 4},
	}
	for _, c := range cases {
		got, total := s.List(c.f, c.limit)
		if !slices.Equal(ids(got), c.want) || total != c.total {
			t.Errorf("List(%+v, %d) = %v (%d), want %v (%d)", c.f, c.limit, ids(got), total, c.want, c.total)
		}
	}
	comps := s.Components()
	if len(comps) != 3 || comps[0] != (Component{"docker.service", 2, 1}) || comps[1].Name != "kernel" {
		t.Errorf("Components = %+v", comps)
	}
	if !s.HasComponent("kernel") || s.HasComponent("nope") {
		t.Error("HasComponent")
	}
	if n, _ := s.MarkAllRead("u", Filter{Component: "docker.service"}); n != 1 || s.Unread() != 2 {
		t.Errorf("MarkAllRead marked %d, %d unread left", n, s.Unread())
	}
}

// fakeSource replays lines, then ends or waits for the context.
type fakeSource struct {
	mu    sync.Mutex
	calls []time.Time // since of each call
	runs  [][]string
}

func (f *fakeSource) Follow(ctx context.Context, since time.Time, line func([]byte)) error {
	f.mu.Lock()
	f.calls = append(f.calls, since)
	var lines []string
	if len(f.runs) > 0 {
		lines, f.runs = f.runs[0], f.runs[1:]
	}
	last := len(f.runs) == 0
	f.mu.Unlock()
	for _, l := range lines {
		line([]byte(l + "\n"))
	}
	if last {
		<-ctx.Done()
		return ctx.Err()
	}
	return fmt.Errorf("journalctl ended")
}

func TestCollector(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{runs: [][]string{
		{shortLine(now.Add(-time.Hour), "a[1]", "one"),
			shortLine(now, "x[2]", "Error: boom"),
			shortLine(now, "x[2]", "    at main"),
			shortLine(now, "x[2]", "[warn ] only a warning")},
		// read again after a restart: stored already
		{shortLine(now, "x[2]", "Error: boom"),
			shortLine(now, "x[2]", "    at main"),
			shortLine(now.Add(time.Second), "b[3]", "two")},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Collector{Store: s, Source: src}).Run(ctx)
		close(done)
	}()
	end := now.Add(time.Second + time.Microsecond)
	deadline := time.Now().Add(10 * time.Second)
	for !s.Since().Equal(end) {
		if time.Now().After(deadline) {
			_, n := s.List(Filter{}, 0)
			t.Fatalf("collected %d events, since %v", n, s.Since())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got, n := s.List(Filter{}, 0); n != 3 {
		t.Errorf("events: %+v", got)
	}
	if got, _ := s.List(Filter{Text: "boom"}, 0); len(got) != 1 || got[0].Message != "Error: boom\n    at main" {
		t.Errorf("the lines of a container message are one event: %+v", got)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.calls) != 2 || time.Until(src.calls[0]).Round(time.Hour) != -Retention || !src.calls[1].Equal(now.Add(time.Microsecond)) {
		t.Errorf("Follow calls = %v (resume just after the last line, even of a dropped message)", src.calls)
	}
}

// containerFeed adds lines a container wrote to m, each 50µs after the one
// before, and collects the events completed.
type containerFeed struct {
	m   *merger
	at  time.Time
	n   int
	got []Event
}

func (f *containerFeed) lines(container string, lines ...string) {
	for _, l := range lines {
		f.n++
		line := shortLine(f.at.Add(time.Duration(f.n)*50*time.Microsecond), container, l)
		f.got = append(f.got, f.m.add(line+"\n", f.at)...)
	}
}

func TestMerger(t *testing.T) {
	at := time.Date(2026, 10, 27, 22, 30, 43, 0, time.UTC)
	f := &containerFeed{m: newMerger(), at: at}
	// a Node error object, and the next record a few µs later
	f.lines("discovery",
		"[27.10.2026 - 22:30:43.482] [0] [error ] [events.js:185] Error: connect ECONNREFUSED 192.0.2.10:443",
		"    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:2083:16) {",
		"  errno: -111,")
	// another container in between
	f.lines("osem",
		"node:internal/process/promises:324",
		"    triggerUncaughtException(err, true /* fromPromise */);",
		"    ^",
		"",
		"TypeError: fetch failed",
		"    at async find (/opt/app/index.js:334:10) {",
		"  [cause]: SocketError: other side closed",
		"}",
		"",
		"Node.js v26.11.1")
	f.lines("discovery",
		"  port: 443",
		"}",
		"[27.10.2026 - 22:30:43.484] [0] [error ] [events.js:185] reconnect failed",
		"[27.10.2026 - 22:30:43.484] [0] [error ] [startup.js:186] TZ= is not a valid timezone",
		"Exiting now due to failing startup conditions")
	f.lines("couchdb",
		"[info] 2026-10-27T10:35:58.447313Z couchdb@couchdb-0.local <0.43.0> -------- SIGTERM received - shutting down",
		"",
		"[info] 2026-10-27T10:35:58.447410Z couchdb@couchdb-0.local <0.43.0> -------- SIGTERM received - shutting down",
		"",
		"[error] 2026-10-27T11:00:37.297932Z couchdb@couchdb-0.local <0.1900.0> -------- gen_server terminated with reason: killed",
		"  last msg: redacted",
		"     state: {st,<0.1897.0>,couch_mrview_index,nil}",
		"    extra: []",
		"Kernel pid terminated (application_controller) (\"{application_terminated,couch_log,shutdown}\")",
		"",
		"Crash dump is being written to: erl_crash.dump...done")
	f.lines("node",
		"Error: probe A",
		"    at [eval]:2:11",
		"Error: probe B",
		"    at [eval]:3:15")
	f.lines("python",
		"ERROR:root:probe E",
		"Traceback (most recent call last):",
		`  File "<string>", line 3, in <module>`,
		"    try: 1/0",
		"         ~^~",
		"ZeroDivisionError: division by zero",
		"ERROR:root:probe F",
		`{"level":"error","msg":"probe G json"}`)
	f.got = append(f.got, f.m.expire(at.Add(2*time.Second))...)

	var got []string
	for _, e := range f.got {
		got = append(got, fmt.Sprintf("%s %s +%d", e.Component, e.Summary(), e.More()))
	}
	want := []string{
		"discovery [27.10.2026 - 22:30:43.482] [0] [error ] [events.js:185] Error: connect ECONNREFUSED 192.0.2.10:443 +4",
		"discovery [27.10.2026 - 22:30:43.484] [0] [error ] [events.js:185] reconnect failed +0",
		"discovery [27.10.2026 - 22:30:43.484] [0] [error ] [startup.js:186] TZ= is not a valid timezone +0",
		"couchdb [error] 2026-10-27T11:00:37.297932Z couchdb@couchdb-0.local <0.1900.0> -------- gen_server terminated with reason: killed +3",
		"node Error: probe A +1",
		"python ERROR:root:probe E +5",
		"python ERROR:root:probe F +0",
		"osem node:internal/process/promises:324 +9",
		"discovery Exiting now due to failing startup conditions +0",
		`couchdb Kernel pid terminated (application_controller) ("{application_terminated,couch_log,shutdown}") +2`,
		"node Error: probe B +1",
		`python {"level":"error","msg":"probe G json"} +0`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if e := f.got[0]; !strings.HasSuffix(e.Message, "\n  errno: -111,\n  port: 443\n}") {
		t.Errorf("merged message: %q", e.Message)
	}
	if len(f.m.open) != 0 || f.m.dropped != 2 {
		t.Errorf("%d messages still open, %d dropped", len(f.m.open), f.m.dropped)
	}
}

func TestMergerBounds(t *testing.T) {
	at := time.Date(2026, 10, 27, 22, 30, 43, 0, time.UTC)
	m := newMerger()
	cont := func(after time.Duration, msg string) string {
		return shortLine(at.Add(after), "x[1]", msg)
	}
	if !m.resume().IsZero() {
		t.Errorf("resume before the first line = %v", m.resume())
	}
	// lines before the first entry are no journal output
	m.add("Error: not journalctl\n", at)
	m.add(cont(0, "Error: boom"), at)
	if m.sample != "Error: not journalctl" || m.seq != 1 {
		t.Errorf("sample %q, %d entries", m.sample, m.seq)
	}
	// the further lines of an entry are indented to its prefix and always
	// continue it, even when they look like a new record
	pad := strings.Repeat(" ", len(cont(0, "")))
	m.add(pad+"  at main\n", at)
	m.add(pad+"[error ] still the same entry\n", at)
	m.add(cont(time.Millisecond, "    at more"), at)
	// an open message holds the position back until it is complete
	if !m.resume().Equal(at) {
		t.Errorf("resume with an open message = %v, want %v", m.resume(), at)
	}
	// a line after more than mergeGap starts a new message, however it looks
	evs := m.add(cont(2*time.Second, "    at late"), at)
	if len(evs) != 1 || evs[0].Message != "Error: boom\n  at main\n[error ] still the same entry\n    at more" {
		t.Errorf("after the gap: %+v", evs)
	}
	if !m.resume().Equal(at.Add(2 * time.Second)) {
		t.Errorf("resume = %v", m.resume())
	}
	if evs := m.expire(at.Add(mergeGap)); len(evs) != 0 {
		t.Errorf("expired too early: %v", evs)
	}
	if evs := m.expire(at.Add(2 * mergeGap)); len(evs) != 1 || !m.resume().Equal(at.Add(2*time.Second+time.Microsecond)) {
		t.Errorf("expire: %v, resume %v", evs, m.resume())
	}
	// a message ends after maxMergeLines
	m.add(cont(3*time.Second, "Error: endless"), at)
	evs = nil
	for i := 1; i < maxMergeLines; i++ {
		evs = append(evs, m.add(cont(3*time.Second, "  more"), at)...)
	}
	if len(evs) != 1 || evs[0].More() != maxMergeLines-1 || len(m.open) != 0 {
		t.Errorf("long message: %d events, open %d", len(evs), len(m.open))
	}
	// the further lines of an entry whose message ended are dropped
	if evs := m.add(pad+"  more\n", at); len(evs) != 0 || len(m.open) != 0 {
		t.Errorf("line after a full message: %v", evs)
	}
	// a message below error is dropped with its lines
	m.add(cont(5*time.Second, "[warn ] slow"), at)
	m.add(cont(5*time.Second, "  detail"), at)
	if evs := m.close(); len(evs) != 0 || m.dropped != 1 {
		t.Errorf("warning: %v, %d dropped", evs, m.dropped)
	}
}

func TestStoreReimport(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, err := open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	a, b := ev("a", clk.t.Add(-time.Hour), "kernel", "one"), ev("b", clk.t.Add(-time.Minute), "kernel", "two")
	_ = s.Add([]Event{a, b}, clk.t.Add(-time.Second))
	_, _ = s.MarkRead("alice", "a")
	// as stored by a version before merging
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"cursor":"cur1"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, n := s.List(Filter{}, 0); n != 0 || !s.Since().IsZero() {
		t.Fatalf("after a merge version change: %d events, since %v", n, s.Since())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, eventsPrefix+"*")); len(files) != 0 {
		t.Errorf("event files kept: %v", files)
	}
	_ = s.Add([]Event{a, b}, clk.t)
	if got, _ := s.Get("a"); got.ReadBy != "alice" {
		t.Errorf("read mark of an imported event: %+v", got)
	}
	if got, _ := s.Get("b"); got.Read() {
		t.Errorf("b is read: %+v", got)
	}
	s, _ = open(dir, clk.now)
	if got, _ := s.Get("a"); !s.Since().Equal(clk.t) || got.ReadBy != "alice" {
		t.Errorf("after reopening: since %v, a %+v", s.Since(), got)
	}
}
