// Command eventparse shows how ContainerUI parses and merges journal
// output into events, for trying out journalctl options:
//
//	journalctl --priority=err --output=json --since=-1h | eventparse [-text] [-format f] [-priority p]
//
// It reads journalctl output from stdin and writes each event as the
// events-YYYY-MM-DD.jsonl files store it, or with -text as a readable
// block. What it read is summed up on stderr.
//
// ContainerUI reads --output=json. The other formats are converted to it
// first: export and verbose have all fields; the short formats have the
// time, host, identifier and PID; cat only the message. Formats without a
// priority get -priority.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/events"
)

func main() {
	text := flag.Bool("text", false, "write each event as a readable block instead of a JSON line")
	format := flag.String("format", "auto", "the journalctl output format: auto, json, export, verbose, short (any short-*) or cat")
	priority := flag.Int("priority", 3, "the priority of entries of formats without one (short, cat)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: journalctl --priority=err --output=json ... | eventparse [-text] [-format f] [-priority p]")
		flag.PrintDefaults()
	}
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	pr, pw := io.Pipe()
	var cst convertStats
	converted := make(chan error, 1)
	go func() {
		var err error
		cst, err = convert(os.Stdin, pw, *format, *priority, time.Now)
		pw.CloseWithError(err)
		converted <- err
	}()
	st, err := events.Replay(pr, func(e events.Event) {
		if !*text {
			_ = enc.Encode(e)
			return
		}
		source := e.Component
		if e.Container != "" {
			source += " · " + e.Container
		}
		fmt.Fprintf(out, "── %s  %s  %s  %s  (+%d lines)\n", e.Time.Local().Format("2006-01-02 15:04:05.000"), e.PriorityName, source, e.ID, e.More())
		for line := range strings.Lines(e.Message) {
			fmt.Fprintf(out, "   %s", strings.TrimSuffix(line, "\n")+"\n")
		}
	})
	out.Flush()
	if cerr := <-converted; err == nil {
		err = cerr
	}
	fmt.Fprintf(os.Stderr, "\nformat %s: %d lines read, %d entries, %d journalctl lines skipped\n", cst.Format, cst.Lines, cst.Entries, cst.Skipped)
	fmt.Fprintf(os.Stderr, "%d lines, %d journal entries, %d of priority err or worse, %d events after merging\n",
		st.Lines, st.Entries, st.Kept, st.Events)
	if st.Sample != "" {
		fmt.Fprintf(os.Stderr, "not a journal entry: %.200s\n", st.Sample)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "eventparse:", err)
		os.Exit(1)
	}
}
