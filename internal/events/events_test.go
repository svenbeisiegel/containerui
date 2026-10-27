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

// journalLine is a journalctl --output=json line.
func journalLine(cursor string, t time.Time, prio int, fields string) string {
	return fmt.Sprintf(`{"__CURSOR":%q,"__REALTIME_TIMESTAMP":"%d","PRIORITY":"%d","_BOOT_ID":"b1","_HOSTNAME":"host"%s}`,
		cursor, t.UnixMicro(), prio, fields)
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

// ids are the IDs of the groups, joined with + within a group.
func ids(gs []Group) []string {
	var out []string
	for _, g := range gs {
		out = append(out, strings.Join(g.IDs(), "+"))
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

func TestStoreGroups(t *testing.T) {
	now := time.Now()
	s, err := open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Hour)
	line := func(id string, after time.Duration, container string, pid int, msg string) Event {
		e := ev(id, at.Add(after), "docker.service", msg)
		e.Container, e.PID, e.Boot = container, pid, "b1"
		return e
	}
	ms := time.Millisecond
	err = s.Add([]Event{
		line("t1", 0, "web", 7, "Error: boom"),
		line("t2", 2*ms, "web", 7, "    at main (/app/index.js:1:1)"),
		line("o1", 3*ms, "db", 7, "other container"),
		ev("k1", at.Add(4*ms), "kernel", "interleaved"),
		line("t3", 5*ms, "web", 7, ""),
		line("p1", 6*ms, "web", 8, "other pid"),
		line("t4", 900*ms, "web", 7, "Node.js v26"),
	}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	// a later batch continues the group, a gap starts a new one
	_ = s.Add([]Event{line("t5", 1800*ms, "web", 7, "exit"), line("n1", 2900*ms, "web", 7, "restarted")}, "c2")
	got, total := s.List(Filter{}, 0)
	if want := []string{"n1", "p1", "k1", "o1", "t1+t2+t3+t4+t5"}; !slices.Equal(ids(got), want) || total != 5 {
		t.Fatalf("List = %v (%d), want %v", ids(got), total, want)
	}
	g := got[4]
	if g.Message() != "Error: boom\n    at main (/app/index.js:1:1)\n\nNode.js v26\nexit" || g.Summary() != "Error: boom" || g.Lines() != 5 {
		t.Errorf("group: %q, summary %q", g.Message(), g.Summary())
	}
	if got, _ := s.List(Filter{Text: "index.js"}, 0); !slices.Equal(ids(got), []string{"t1+t2+t3+t4+t5"}) {
		t.Errorf("text filter: %v", ids(got))
	}
	if n, _ := s.MarkAllRead("u", Filter{Text: "index.js"}); n != 1 || s.Unread() != 4 {
		t.Errorf("MarkAllRead marked %d, %d unread", n, s.Unread())
	}
	if e, _ := s.Get("t5"); !e.Read() {
		t.Error("marking by text did not mark the whole group")
	}
	if n, _ := s.MarkRead("u", "o1", "t3"); n != 1 {
		t.Errorf("MarkRead of an event in a read group marked %d groups", n)
	}
	if g, ok := s.GroupOf("t4"); !ok || g.ID() != "t1" || !g.Read() {
		t.Errorf("GroupOf(t4) = %v %v", g.IDs(), ok)
	}
	if c := s.Components(); c[0] != (Component{"docker.service", 4, 2}) {
		t.Errorf("Components = %+v", c)
	}

	// the groups are rebuilt from the day files
	s2, err := open(s.Dir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.List(Filter{Unread: true}, 0); !slices.Equal(ids(got), []string{"n1", "p1", "k1"}) {
		t.Errorf("after reopen: %v", ids(got))
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
		if _, n := s.List(Filter{}, 0); n == 2 && s.Cursor() == "c3" {
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
	src.mu.Lock()
	defer src.mu.Unlock()
	if want := []string{"since -168h0m0s", "c2"}; !slices.Equal(src.calls, want) {
		t.Errorf("Follow calls = %q, want %q (resume after the last cursor, even of a skipped entry)", src.calls, want)
	}
}
