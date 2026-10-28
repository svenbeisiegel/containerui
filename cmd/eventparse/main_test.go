package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/events"
)

// replay returns the events of in as "summary +more", and what it read.
func replay(t *testing.T, in string) ([]string, []events.Event, events.ReplayStats) {
	t.Helper()
	now := time.Date(2026, 10, 27, 23, 30, 0, 0, time.Local)
	var got []string
	var evs []events.Event
	st, err := events.Replay(strings.NewReader(in), now, func(e events.Event) {
		got = append(got, fmt.Sprintf("%s +%d", e.Summary(), e.More()))
		evs = append(evs, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, evs, st
}

func TestAppliance(t *testing.T) {
	// captured on appliance-1, a CET host, on 27 Oct 2026
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.FixedZone("CET", 3600)
	lines := []struct{ time, msg string }{
		{"23:01:27.553612", "[27.10.2026 - 23:01:27.553] [0] [error ] [opt/osemapp/backend/classes/events.js:185] Error: connect ECONNREFUSED 192.168.21.10:443"},
		{"23:01:27.553671", "    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:2083:16) {"},
		{"23:01:27.553690", "  errno: -111,"},
		{"23:01:27.553702", "  code: 'ECONNREFUSED',"},
		{"23:01:27.553714", "  syscall: 'connect',"},
		{"23:01:27.553725", "  address: '192.168.21.10',"},
		{"23:01:27.553737", "  port: 443"},
		{"23:01:27.553748", "}"},
		{"23:01:27.555301", "[27.10.2026 - 23:01:27.555] [0] [log   ] [opt/osemapp/backend/classes/events.js:194] stop() connection to API server url[192.168.21.10]"},
	}
	formats := map[string]func(string) string{
		"short-precise":     func(tm string) string { return "Oct 27 " + tm },
		"short-iso-precise": func(tm string) string { return "2026-10-27T" + tm + "+01:00" },
	}
	for name, prefix := range formats {
		var in strings.Builder
		for _, l := range lines {
			fmt.Fprintf(&in, "%s appliance-1 discovery[962]: %s\n", prefix(l.time), l.msg)
		}
		got, evs, st := replay(t, in.String())
		if len(evs) != 1 || st.Entries != 9 || st.Dropped != 1 {
			t.Errorf("%s: events %q, %+v", name, got, st)
			continue
		}
		e := evs[0]
		if want := time.Date(2026, 10, 27, 22, 1, 27, 553612000, time.UTC); !e.Time.Equal(want) {
			t.Errorf("%s: time %v, want %v", name, e.Time, want)
		}
		if e.More() != 7 || !strings.Contains(e.Message, "Error: connect ECONNREFUSED 192.168.21.10:443\n    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:2083:16) {\n  errno: -111,") ||
			!strings.HasSuffix(e.Message, "  port: 443\n}") {
			t.Errorf("%s: message %q", name, e.Message)
		}
		if e.Component != "discovery" || e.PID != 962 || e.Host != "appliance-1" || e.PriorityName != "err" {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

func TestShortFormats(t *testing.T) {
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.FixedZone("CET", 3600)
	want := []string{"Error: probe A +2"}
	cases := []struct{ name, in string }{
		{"short", `-- Boot fc000899846a450d9a37cba81a853732 --
Oct 27 22:40:20 appliance-1 discovery[962]: Error: probe A
Oct 27 22:40:20 appliance-1 discovery[962]:     at [eval]:2:11
Oct 27 22:40:20 appliance-1 discovery[962]: }
Oct 27 22:40:20 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-precise", `Oct 27 22:40:20.805888 appliance-1 discovery[962]: Error: probe A
Oct 27 22:40:20.806439 appliance-1 discovery[962]:     at [eval]:2:11
Oct 27 22:40:20.806501 appliance-1 discovery[962]: }
Oct 27 22:40:20.806552 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-iso", `2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: Error: probe A
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]:     at [eval]:2:11
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: }
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-iso-precise", `2026-10-27T22:40:20.805888+0100 appliance-1 discovery[962]: Error: probe A
2026-10-27T22:40:20.806439+0100 appliance-1 discovery[962]:     at [eval]:2:11
2026-10-27T22:40:20.806501+0100 appliance-1 discovery[962]: }
2026-10-27T22:40:20.806552+0100 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-full", `Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: Error: probe A
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]:     at [eval]:2:11
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: }
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-unix", `1793137220.805888 appliance-1 discovery[962]: Error: probe A
1793137220.806439 appliance-1 discovery[962]:     at [eval]:2:11
1793137220.806501 appliance-1 discovery[962]: }
1793137220.806552 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-monotonic", `[2204800.813220] appliance-1 discovery[962]: Error: probe A
[2204800.813771] appliance-1 discovery[962]:     at [eval]:2:11
[2204800.813833] appliance-1 discovery[962]: }
[2204800.813884] appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
	}
	for _, c := range cases {
		got, evs, st := replay(t, c.in)
		if !slices.Equal(got, want) || st.Dropped != 1 || st.Sample != "" {
			t.Errorf("%s: events %q, %+v", c.name, got, st)
			continue
		}
		if c.name != "short-monotonic" {
			if tm := evs[0].Time.Local(); tm.Year() != 2026 || tm.Hour() != 22 || tm.Minute() != 40 {
				t.Errorf("%s: time %v", c.name, tm)
			}
		}
		if evs[0].Component != "discovery" || evs[0].PID != 962 || evs[0].Host != "appliance-1" {
			t.Errorf("%s: %+v", c.name, evs[0])
		}
	}
}

func TestMultiLineEntry(t *testing.T) {
	// journalctl indents the further lines of a message to its prefix
	got, evs, _ := replay(t, `2026-10-27T22:40:20.805888+01:00 appliance-1 app[7]: Error: boom
                                                         at main
2026-10-27T22:40:20.806001+01:00 appliance-1 app[7]: [x] next
`)
	if !slices.Equal(got, []string{"Error: boom +1", "[x] next +0"}) || evs[0].Message != "Error: boom\n    at main" {
		t.Errorf("events %q, %q", got, evs[0].Message)
	}
	if _, _, st := replay(t, "Error: probe A\n    at x\n"); st.Events != 0 || st.Sample != "Error: probe A" {
		t.Errorf("cat output: %+v", st)
	}
}
