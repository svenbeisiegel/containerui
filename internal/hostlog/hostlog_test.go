package hostlog

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestJournalArgs(t *testing.T) {
	cases := []struct {
		q    Query
		want string
	}{
		{Query{}, "--no-pager --output=short-iso"},
		{Query{Selector: "docker.service", Priority: "err", Since: "boot"},
			"--no-pager --output=short-iso --unit=docker.service --priority=err --boot"},
		{Query{Since: "24h", Follow: true, Tail: 100},
			"--no-pager --output=short-iso --since=-24h --follow --lines=100"},
		{Query{Follow: true}, "--no-pager --output=short-iso --follow --lines=all"},
	}
	for _, c := range cases {
		if got := strings.Join(journalArgs(c.q), " "); got != c.want {
			t.Errorf("journalArgs(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestJournalUnits(t *testing.T) {
	got := journalUnits("sshd.service\ndocker.service\n\ncron.service\ncontainerui.service\nsshd.service\n")
	var vals []string
	for _, o := range got {
		vals = append(vals, o.Value)
	}
	want := []string{"", "containerui.service", "docker.service", "cron.service", "sshd.service"}
	if !slices.Equal(vals, want) {
		t.Errorf("units = %q, want %q", vals, want)
	}
}

func TestEventXPath(t *testing.T) {
	cases := []struct {
		q           Query
		after, upTo uint64
		want        string
	}{
		{Query{}, 0, 0, "*"},
		{Query{Priority: "2", Since: "1h"}, 0, 0, "*[System[Level>=1 and Level<=2 and TimeCreated[timediff(@SystemTime)<=3600000]]]"},
		{Query{Priority: "4", Since: "boot"}, 0, 50, "*[System[Level<=4 and TimeCreated[timediff(@SystemTime)<=90000] and EventRecordID<=50]]"},
		{Query{}, 7, 0, "*[System[EventRecordID>7]]"},
	}
	for _, c := range cases {
		if got := eventXPath(c.q, 90*time.Second, c.after, c.upTo); got != c.want {
			t.Errorf("eventXPath(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestEventChannels(t *testing.T) {
	out := "Application\r\nMicrosoft-Windows-Containers-Wcifs/Debug\r\nMicrosoft-Windows-Containers-Wcifs/Operational\r\n" +
		"Microsoft-Windows-Hyper-V-Compute-Analytic\r\nMicrosoft-Windows-Hyper-V-Compute-Operational\r\n" +
		"Microsoft-Windows-Security-LessPrivilegedAppContainer/Operational\r\nSystem\r\n"
	var vals []string
	for _, o := range eventChannels(out) {
		vals = append(vals, o.Value)
	}
	want := []string{"System", "Application", "Microsoft-Windows-Containers-Wcifs/Operational", "Microsoft-Windows-Hyper-V-Compute-Operational"}
	if !slices.Equal(vals, want) {
		t.Errorf("channels = %q, want %q", vals, want)
	}
}

const renderedXML = "<Events>\r\n" +
	`<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System><Provider Name='Service Control Manager'/>` +
	`<EventID Qualifiers='16384'>7036</EventID><Level>4</Level><TimeCreated SystemTime='2026-10-16T06:51:23.0863832Z'/>` +
	`<EventRecordID>3442</EventRecordID></System><RenderingInfo Culture='de-DE'><Message>Der Dienst wurde gestartet: äöü.` + "\r\n\r\n" +
	`Zweite Zeile</Message><Level>Informationen</Level></RenderingInfo></Event>` +
	`<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System><Provider Name='Disk'/>` +
	`<EventID>7</EventID><Level>2</Level><TimeCreated SystemTime='2026-10-16T07:00:00Z'/>` +
	`<EventRecordID>3443</EventRecordID></System></Event>` + "\r\n</Events>\r\n"

func decodeLines(t *testing.T, r io.Reader) ([]string, []uint64) {
	t.Helper()
	var lines []string
	var ids []uint64
	err := decodeEvents(r, func(e *event) error {
		lines = append(lines, e.line(time.UTC))
		ids = append(ids, e.System.EventRecordID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines, ids
}

func TestDecodeEvents(t *testing.T) {
	want := []string{
		"2026-10-16T06:51:23+0000 Informationen Service Control Manager[7036]: Der Dienst wurde gestartet: äöü.\n    Zweite Zeile",
		"2026-10-16T07:00:00+0000 Error Disk[7]: ",
	}
	// UTF-8 as is, and UTF-16LE with BOM as wevtutil /uni:true writes it.
	u16 := []byte{0xFF, 0xFE}
	for _, c := range utf16.Encode([]rune(renderedXML + "😀")) {
		u16 = append(u16, byte(c), byte(c>>8))
	}
	for name, r := range map[string]io.Reader{"utf8": strings.NewReader(renderedXML), "utf16": strings.NewReader(string(u16))} {
		lines, ids := decodeLines(t, r)
		if !slices.Equal(lines, want) {
			t.Errorf("%s: lines = %q, want %q", name, lines, want)
		}
		if !slices.Equal(ids, []uint64{3442, 3443}) {
			t.Errorf("%s: ids = %v", name, ids)
		}
	}
}

func TestUTF16Surrogates(t *testing.T) {
	in := []byte{0xFF, 0xFE}
	for _, c := range utf16.Encode([]rune("a😀b")) {
		in = append(in, byte(c), byte(c>>8))
	}
	got, err := io.ReadAll(newBOMReader(strings.NewReader(string(in))))
	if err != nil || string(got) != "a😀b" {
		t.Errorf("got %q, %v", got, err)
	}
}

type stubSource struct{}

func (stubSource) Kind() string          { return "journal" }
func (stubSource) Label() string         { return "Journal" }
func (stubSource) SelectorLabel() string { return "Unit" }
func (stubSource) Available() error      { return nil }
func (stubSource) Selectors(context.Context) ([]Option, error) {
	return journalUnits("docker.service\n"), nil
}
func (stubSource) Priorities() []Option                         { return journalPriorities }
func (stubSource) Ranges() []Option                             { return journalRanges }
func (stubSource) Read(context.Context, Query, io.Writer) error { return nil }

func TestValidate(t *testing.T) {
	ok := []Query{{}, {Selector: "docker.service", Priority: "err", Since: "boot"}}
	for _, q := range ok {
		if err := Validate(context.Background(), stubSource{}, q); err != nil {
			t.Errorf("Validate(%+v) = %v", q, err)
		}
	}
	bad := []Query{
		{Selector: "--output=json"},
		{Selector: "-f"},
		{Selector: "sshd.service"},
		{Priority: "emerg"},
		{Since: "-1h"},
	}
	for _, q := range bad {
		if err := Validate(context.Background(), stubSource{}, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", q, err)
		}
	}
}
