package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/events"
)

// replay converts in and returns the events as "summary +more" and the
// format detected.
func replay(t *testing.T, in string) ([]string, []events.Event, string) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 27, 23, 0, 0, 0, time.Local) }
	var json bytes.Buffer
	st, err := convert(strings.NewReader(in), &json, "auto", 3, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var evs []events.Event
	if _, err := events.Replay(&json, func(e events.Event) {
		got = append(got, fmt.Sprintf("%s +%d", e.Summary(), e.More()))
		evs = append(evs, e)
	}); err != nil {
		t.Fatal(err)
	}
	return got, evs, st.Format
}

func TestConvertFormats(t *testing.T) {
	want := []string{"Error: probe A +2", "[27.10.2026 - 00:00:00.000] [0] [warn ] probe C +0"}
	cases := []struct{ name, format, in string }{
		{"short", formatShort, `-- Boot fc000899846a450d9a37cba81a853732 --
Oct 27 22:40:20 appliance-1 discovery[962]: Error: probe A
Oct 27 22:40:20 appliance-1 discovery[962]:     at [eval]:2:11
Oct 27 22:40:20 appliance-1 discovery[962]: }
Oct 27 22:40:20 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-precise", formatShort, `Oct 27 22:40:20.805888 appliance-1 discovery[962]: Error: probe A
Oct 27 22:40:20.806439 appliance-1 discovery[962]:     at [eval]:2:11
Oct 27 22:40:20.806501 appliance-1 discovery[962]: }
Oct 27 22:40:20.806552 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-iso", formatShort, `2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: Error: probe A
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]:     at [eval]:2:11
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: }
2026-10-27T22:40:20+01:00 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-iso-precise", formatShort, `2026-10-27T22:40:20.805888+0100 appliance-1 discovery[962]: Error: probe A
2026-10-27T22:40:20.806439+0100 appliance-1 discovery[962]:     at [eval]:2:11
2026-10-27T22:40:20.806501+0100 appliance-1 discovery[962]: }
2026-10-27T22:40:20.806552+0100 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-full", formatShort, `Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: Error: probe A
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]:     at [eval]:2:11
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: }
Tue 2026-10-27 22:40:20 CET appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-unix", formatShort, `1793137220.805888 appliance-1 discovery[962]: Error: probe A
1793137220.806439 appliance-1 discovery[962]:     at [eval]:2:11
1793137220.806501 appliance-1 discovery[962]: }
1793137220.806552 appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"short-monotonic", formatShort, `[2204800.813220] appliance-1 discovery[962]: Error: probe A
[2204800.813771] appliance-1 discovery[962]:     at [eval]:2:11
[2204800.813833] appliance-1 discovery[962]: }
[2204800.813884] appliance-1 discovery[962]: [27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
		{"cat", formatCat, "Error: probe A\n    at [eval]:2:11\n}\n[27.10.2026 - 00:00:00.000] [0] [warn ] probe C\n"},
		{"json-seq", formatJSON, "\x1e" + `{"__CURSOR":"a","__REALTIME_TIMESTAMP":"1793137220805888","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"Error: probe A"}` + "\n" +
			"\x1e" + `{"__CURSOR":"b","__REALTIME_TIMESTAMP":"1793137220806439","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"    at [eval]:2:11"}` + "\n" +
			"\x1e" + `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1793137220806501","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"}"}` + "\n" +
			"\x1e" + `{"__CURSOR":"d","__REALTIME_TIMESTAMP":"1793137220806552","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"[27.10.2026 - 00:00:00.000] [0] [warn ] probe C"}` + "\n"},
		{"json-sse", formatJSON, `data: {"__CURSOR":"a","__REALTIME_TIMESTAMP":"1793137220805888","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"Error: probe A"}

data: {"__CURSOR":"b","__REALTIME_TIMESTAMP":"1793137220806439","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"    at [eval]:2:11"}

data: {"__CURSOR":"c","__REALTIME_TIMESTAMP":"1793137220806501","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"}"}

data: {"__CURSOR":"d","__REALTIME_TIMESTAMP":"1793137220806552","PRIORITY":"3","CONTAINER_ID_FULL":"x","MESSAGE":"[27.10.2026 - 00:00:00.000] [0] [warn ] probe C"}

`},
		{"export", formatExport, "__CURSOR=a\n__REALTIME_TIMESTAMP=1793137220805888\nPRIORITY=3\nCONTAINER_ID_FULL=x\nMESSAGE=Error: probe A\n\n" +
			"__CURSOR=b\n__REALTIME_TIMESTAMP=1793137220806439\nPRIORITY=3\nCONTAINER_ID_FULL=x\nMESSAGE=    at [eval]:2:11\n\n" +
			"__CURSOR=c\n__REALTIME_TIMESTAMP=1793137220806501\nPRIORITY=3\nCONTAINER_ID_FULL=x\nMESSAGE\n\x01\x00\x00\x00\x00\x00\x00\x00}\n\n" +
			"__CURSOR=d\n__REALTIME_TIMESTAMP=1793137220806552\nPRIORITY=3\nCONTAINER_ID_FULL=x\nMESSAGE=[27.10.2026 - 00:00:00.000] [0] [warn ] probe C\n\n"},
		{"verbose", formatVerbose, `Tue 2026-10-27 22:40:20.805888 CET [s=1873;i=507aac;b=fc00;m=201585e88a4;t=65ed94b146500;x=9cce]
    PRIORITY=3
    CONTAINER_ID_FULL=x
    MESSAGE=Error: probe A
Tue 2026-10-27 22:40:20.806439 CET [s=1873;i=507aad;b=fc00;m=201585e88a5;t=65ed94b146501;x=9ccf]
    PRIORITY=3
    CONTAINER_ID_FULL=x
    MESSAGE=    at [eval]:2:11
Tue 2026-10-27 22:40:20.806501 CET [s=1873;i=507aae;b=fc00;m=201585e88a6;t=65ed94b146502;x=9cd0]
    PRIORITY=3
    CONTAINER_ID_FULL=x
    MESSAGE=}
Tue 2026-10-27 22:40:20.806552 CET [s=1873;i=507aaf;b=fc00;m=201585e88a7;t=65ed94b146503;x=9cd1]
    PRIORITY=3
    CONTAINER_ID_FULL=x
    MESSAGE=[27.10.2026 - 00:00:00.000] [0] [warn ] probe C
`},
	}
	for _, c := range cases {
		got, evs, format := replay(t, c.in)
		if format != c.format || !slices.Equal(got, want) {
			t.Errorf("%s: format %s, events %q", c.name, format, got)
			continue
		}
		if c.format == formatShort && c.name != "short-monotonic" {
			if tm := evs[0].Time.Local(); tm.Year() != 2026 || tm.Hour() != 22 || tm.Minute() != 40 {
				t.Errorf("%s: time %v", c.name, tm)
			}
			if evs[0].Component != "discovery" || evs[0].PID != 962 || evs[0].Host != "appliance-1" {
				t.Errorf("%s: %+v", c.name, evs[0])
			}
		}
	}
}

func TestConvertMultiLine(t *testing.T) {
	// journalctl indents the further lines of a message to its prefix
	got, evs, _ := replay(t, `Oct 27 22:40:20 appliance-1 app[7]: Error: boom
                                        at main
Oct 27 22:40:20 appliance-1 app[7]: [x] next
`)
	if !slices.Equal(got, []string{"Error: boom +1", "[x] next +0"}) || evs[0].Message != "Error: boom\n    at main" {
		t.Errorf("short: %q, %q", got, evs[0].Message)
	}
	got, evs, _ = replay(t, `Tue 2026-10-27 22:40:20.805888 CET [s=1;i=1;b=fc;m=1;t=65ed94b146500;x=1]
    PRIORITY=3
    MESSAGE=Error: boom
    at main
    _PID=7
`)
	if !slices.Equal(got, []string{"Error: boom +1"}) || evs[0].PID != 7 {
		t.Errorf("verbose: %q, %+v", got, evs)
	}
}
