package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// journalLine is a journalctl --output=json line.
func journalLine(cursor string, t time.Time, prio int, fields string) string {
	return fmt.Sprintf(`{"__CURSOR":%q,"__REALTIME_TIMESTAMP":"%d","PRIORITY":"%d","_BOOT_ID":"b1","_HOSTNAME":"host"%s}`,
		cursor, t.UnixMicro(), prio, fields)
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

func TestParseJournal(t *testing.T) {
	at := time.Date(2026, 10, 16, 8, 30, 0, 123000, time.UTC)
	cases := []struct {
		name, line         string
		ok                 bool
		component, message string
		priority, pid      int
	}{
		{"unit", journalLine("c1", at, 3, `,"_SYSTEMD_UNIT":"docker.service","SYSLOG_IDENTIFIER":"dockerd","_PID":"42","MESSAGE":"boom\n"`),
			true, "docker.service", "boom", 3, 42},
		{"container", journalLine("c8", at, 3, `,"_SYSTEMD_UNIT":"docker.service","CONTAINER_NAME":"web","_PID":"42","MESSAGE":"at x"`),
			true, "docker.service", "at x", 3, 42},
		{"multi-line", journalLine("c9", at, 3, `,"SYSLOG_IDENTIFIER":"app","MESSAGE":"Error: boom\n    at main\n"`),
			true, "app", "Error: boom\n    at main", 3, 0},
		{"identifier", journalLine("c2", at, 2, `,"SYSLOG_IDENTIFIER":"myapp","MESSAGE":"crit"`), true, "myapp", "crit", 2, 0},
		{"kernel", journalLine("c3", at, 3, `,"_TRANSPORT":"kernel","MESSAGE":"oops"`), true, "kernel", "oops", 3, 0},
		{"comm", journalLine("c4", at, 0, `,"_COMM":"bash","MESSAGE":"x"`), true, "bash", "x", 0, 0},
		{"bytes message", journalLine("c5", at, 3, `,"SYSLOG_IDENTIFIER":"a","MESSAGE":[104,105,255]`), true, "a", "hi�", 3, 0},
		{"repeated field", journalLine("c6", at, 3, `,"SYSLOG_IDENTIFIER":["first","second"],"MESSAGE":"m"`), true, "first", "m", 3, 0},
		{"warning skipped", journalLine("c7", at, 4, `,"MESSAGE":"w"`), false, "", "", 0, 0},
		{"no cursor", `{"PRIORITY":"3","MESSAGE":"m"}`, false, "", "", 0, 0},
		{"not json", `nope`, false, "", "", 0, 0},
	}
	for _, c := range cases {
		e, _, ok := ParseJournal([]byte(c.line))
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if e.Component != c.component || e.Message != c.message || e.Priority != c.priority || e.PID != c.pid {
			t.Errorf("%s: got component %q message %q priority %d pid %d", c.name, e.Component, e.Message, e.Priority, e.PID)
		}
		if (c.name == "container") != (e.Container == "web") {
			t.Errorf("%s: container %q", c.name, e.Container)
		}
		if !e.Time.Equal(at) || e.Boot != "b1" || e.Host != "host" || len(e.ID) != 16 || e.PriorityName != PriorityName(c.priority) {
			t.Errorf("%s: got %+v", c.name, e)
		}
	}
	a, cur, _ := ParseJournal([]byte(journalLine("same", at, 3, "")))
	b, _, _ := ParseJournal([]byte(journalLine("same", at.Add(time.Hour), 3, "")))
	if cur != "same" || a.ID != b.ID {
		t.Errorf("the id must only depend on the cursor: %q %q (cursor %q)", a.ID, b.ID, cur)
	}
	if _, cur, ok := ParseJournal([]byte(journalLine("c7", at, 6, ""))); ok || cur != "c7" {
		t.Errorf("an entry that is not kept still returns its cursor: %q %v", cur, ok)
	}
}

func TestJournalFollowArgs(t *testing.T) {
	got := strings.Join(journalFollowArgs("s=abc;i=1", time.Time{}), " ")
	if !strings.HasSuffix(got, " --after-cursor=s=abc;i=1") || !strings.Contains(got, "--priority=err --output=json --follow") {
		t.Errorf("with cursor: %q", got)
	}
	since := time.Date(2026, 10, 9, 8, 0, 0, 0, time.Local)
	if got := journalFollowArgs("", since); got[len(got)-1] != "--since=2026-10-09 08:00:00" {
		t.Errorf("without cursor: %q", got)
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
	if s.Cursor() != "" {
		t.Fatalf("new store has cursor %q", s.Cursor())
	}
	day := 24 * time.Hour
	err = s.Add([]Event{
		ev("a", clk.t.Add(-2*day), "docker.service", "pull failed"),
		ev("b", clk.t.Add(-time.Hour), "kernel", "Out of memory"),
		ev("old", clk.t.Add(-8*day), "kernel", "too old"),
		ev("c", clk.t.Add(-3*day), "sshd.service", "auth failure"), // out of order
	}, "cur1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add([]Event{ev("b", clk.t, "kernel", "duplicate")}, "cur2"); err != nil {
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
	if s2.Cursor() != "cur2" {
		t.Errorf("cursor after reopen = %q", s2.Cursor())
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
	}, "c")
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
	calls []string // cursor of each call, "since" without one
	runs  [][]string
}

func (f *fakeSource) Follow(ctx context.Context, cursor string, since time.Time, line func([]byte)) error {
	f.mu.Lock()
	if cursor == "" {
		cursor = "since " + time.Until(since).Round(time.Hour).String()
	}
	f.calls = append(f.calls, cursor)
	var lines []string
	if len(f.runs) > 0 {
		lines, f.runs = f.runs[0], f.runs[1:]
	}
	last := len(f.runs) == 0
	f.mu.Unlock()
	for _, l := range lines {
		line([]byte(l))
	}
	if last {
		<-ctx.Done()
		return ctx.Err()
	}
	return fmt.Errorf("journalctl ended")
}

func TestCollector(t *testing.T) {
	now := time.Now()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{runs: [][]string{
		{journalLine("c1", now.Add(-time.Hour), 3, `,"SYSLOG_IDENTIFIER":"a","MESSAGE":"one"`),
			journalLine("c1a", now, 3, `,"CONTAINER_ID_FULL":"x","MESSAGE":"Error: boom"`),
			journalLine("c1b", now, 3, `,"CONTAINER_ID_FULL":"x","MESSAGE":"    at main"`),
			journalLine("c2", now, 5, `,"MESSAGE":"notice"`)},
		{journalLine("c3", now, 2, `,"SYSLOG_IDENTIFIER":"b","MESSAGE":"two"`)},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Collector{Store: s, Source: src}).Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, n := s.List(Filter{}, 0); n == 3 && s.Cursor() == "c3" {
			break
		}
		if time.Now().After(deadline) {
			_, n := s.List(Filter{}, 0)
			t.Fatalf("collected %d events, cursor %q", n, s.Cursor())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got, _ := s.List(Filter{Text: "boom"}, 0); len(got) != 1 || got[0].Message != "Error: boom\n    at main" {
		t.Errorf("the lines of a container message are one event: %+v", got)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if want := []string{"since -168h0m0s", "c2"}; !slices.Equal(src.calls, want) {
		t.Errorf("Follow calls = %q, want %q (resume after the last cursor, even of a skipped entry)", src.calls, want)
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
		msg, _ := json.Marshal(l)
		line := journalLine(fmt.Sprintf("c%d", f.n), f.at.Add(time.Duration(f.n)*50*time.Microsecond), 3,
			fmt.Sprintf(`,"_SYSTEMD_UNIT":"docker.service","_TRANSPORT":"journal","CONTAINER_NAME":%q,"CONTAINER_ID_FULL":"id-%s","MESSAGE":%s`, container, container, msg))
		f.got = append(f.got, f.m.add([]byte(line), f.at)...)
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
		got = append(got, fmt.Sprintf("%s %s +%d", e.Container, e.Summary(), e.More()))
	}
	want := []string{
		"discovery [27.10.2026 - 22:30:43.482] [0] [error ] [events.js:185] Error: connect ECONNREFUSED 192.0.2.10:443 +4",
		"discovery [27.10.2026 - 22:30:43.484] [0] [error ] [events.js:185] reconnect failed +0",
		"discovery [27.10.2026 - 22:30:43.484] [0] [error ] [startup.js:186] TZ= is not a valid timezone +0",
		"couchdb [info] 2026-10-27T10:35:58.447313Z couchdb@couchdb-0.local <0.43.0> -------- SIGTERM received - shutting down +0",
		"couchdb [info] 2026-10-27T10:35:58.447410Z couchdb@couchdb-0.local <0.43.0> -------- SIGTERM received - shutting down +0",
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
	if len(f.m.open) != 0 {
		t.Errorf("%d messages still open", len(f.m.open))
	}
}

func TestMergerBounds(t *testing.T) {
	at := time.Date(2026, 10, 27, 22, 30, 43, 0, time.UTC)
	m := newMerger()
	cont := func(cursor string, after time.Duration, msg string) []byte {
		return []byte(journalLine(cursor, at.Add(after), 3, `,"CONTAINER_ID_FULL":"x","MESSAGE":`+strconv.Quote(msg)))
	}
	// entries that are whole messages are never merged
	if evs := m.add([]byte(journalLine("w1", at, 3, `,"SYSLOG_IDENTIFIER":"app","MESSAGE":"boom"`)), at); len(evs) != 1 {
		t.Fatalf("whole message: %v", evs)
	}
	if evs := m.add([]byte(journalLine("w2", at, 3, `,"SYSLOG_IDENTIFIER":"app","MESSAGE":"  detail"`)), at); len(evs) != 1 || evs[0].Message != "  detail" {
		t.Fatalf("whole message: %v", evs)
	}
	// an open message holds the cursor back until it is complete
	m.add(cont("x1", 0, "Error: boom"), at)
	m.add(cont("x2", time.Millisecond, "    at main"), at)
	if m.resume() != "w2" {
		t.Errorf("resume with an open message = %q, want w2", m.resume())
	}
	// a line after more than mergeGap starts a new message, however it looks
	if evs := m.add(cont("x3", 2*time.Second, "    at late"), at); len(evs) != 1 || evs[0].Message != "Error: boom\n    at main" {
		t.Errorf("after the gap: %v", evs)
	}
	if m.resume() != "x2" {
		t.Errorf("resume = %q, want x2", m.resume())
	}
	if evs := m.expire(at.Add(mergeGap)); len(evs) != 0 {
		t.Errorf("expired too early: %v", evs)
	}
	if evs := m.expire(at.Add(2 * mergeGap)); len(evs) != 1 || m.resume() != "x3" {
		t.Errorf("expire: %v, resume %q", evs, m.resume())
	}
	// a message ends after maxMergeLines
	m.add(cont("y0", 3*time.Second, "Error: endless"), at)
	var evs []Event
	for i := 1; i < maxMergeLines; i++ {
		evs = append(evs, m.add(cont(fmt.Sprintf("y%d", i), 3*time.Second, "  more"), at)...)
	}
	if len(evs) != 1 || evs[0].More() != maxMergeLines-1 || len(m.open) != 0 {
		t.Errorf("long message: %d events, open %d", len(evs), len(m.open))
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
	_ = s.Add([]Event{a, b}, "cur1")
	_, _ = s.MarkRead("alice", "a")
	// as stored by a version before merging
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"cursor":"cur1"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, n := s.List(Filter{}, 0); n != 0 || s.Cursor() != "" {
		t.Fatalf("after a merge version change: %d events, cursor %q", n, s.Cursor())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, eventsPrefix+"*")); len(files) != 0 {
		t.Errorf("event files kept: %v", files)
	}
	_ = s.Add([]Event{a, b}, "cur2")
	if got, _ := s.Get("a"); got.ReadBy != "alice" {
		t.Errorf("read mark of an imported event: %+v", got)
	}
	if got, _ := s.Get("b"); got.Read() {
		t.Errorf("b is read: %+v", got)
	}
	s, _ = open(dir, clk.now)
	if got, _ := s.Get("a"); s.Cursor() != "cur2" || got.ReadBy != "alice" {
		t.Errorf("after reopening: cursor %q, a %+v", s.Cursor(), got)
	}
}
