// Command eventparse shows how ContainerUI parses and merges journal
// output into events, for trying out what a journal gives:
//
//	journalctl --priority=err --output=short-iso-precise --since=-1h | eventparse [-text]
//
// It reads journalctl output from stdin and writes each event as the
// events-YYYY-MM-DD.jsonl files store it, or with -text as a readable
// block. What it read is summed up on stderr.
//
// ContainerUI reads --output=short-iso-precise. The other short formats
// are read as well: short and short-precise without a year are taken as
// of now, the monotonic ones have no time. Every line is taken as an
// error, as with --priority=err; a message that names a level below error
// is dropped.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/events"
)

func main() {
	text := flag.Bool("text", false, "write each event as a readable block instead of a JSON line")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: journalctl --priority=err --output=short-iso-precise ... | eventparse [-text]")
		flag.PrintDefaults()
	}
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	st, err := events.Replay(os.Stdin, time.Now(), func(e events.Event) {
		if !*text {
			_ = enc.Encode(e)
			return
		}
		fmt.Fprintf(out, "── %s  %s  %s  %s  (+%d lines)\n", e.Time.Local().Format("2006-01-02 15:04:05.000000"), e.PriorityName, e.Component, e.ID, e.More())
		for line := range strings.Lines(e.Message) {
			fmt.Fprintf(out, "   %s", strings.TrimSuffix(line, "\n")+"\n")
		}
	})
	out.Flush()
	fmt.Fprintf(os.Stderr, "\n%d lines, %d journal entries, %d events after merging (dropped for a level below error: %d)\n",
		st.Lines, st.Entries, st.Events, st.Dropped)
	if st.Sample != "" {
		fmt.Fprintf(os.Stderr, "not journalctl short output: %.200s\n", st.Sample)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "eventparse:", err)
		os.Exit(1)
	}
}
