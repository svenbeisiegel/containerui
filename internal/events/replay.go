package events

import (
	"bufio"
	"bytes"
	"io"
	"time"
)

// ReplayStats counts what Replay read.
type ReplayStats struct {
	Lines   int    // lines that are not blank
	Entries int    // journal entries: lines with a __CURSOR
	Kept    int    // entries of priority err or worse
	Events  int    // events after merging
	Sample  string // the first line that is not a journal entry
}

// Replay reads the output of journalctl --output=json from r and calls fn
// with the events the collector would store, parsed and merged the same
// way. It is for trying out what a journal gives.
func Replay(r io.Reader, fn func(Event)) (ReplayStats, error) {
	var st ReplayStats
	m := newMerger()
	emit := func(evs []Event) {
		for _, e := range evs {
			st.Events++
			fn(e)
		}
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			st.Lines++
			_, cursor, ok := ParseJournal(line)
			switch {
			case ok:
				st.Entries++
				st.Kept++
			case cursor != "":
				st.Entries++
			case st.Sample == "":
				st.Sample = string(bytes.TrimSpace(line))
			}
			// lines are merged by their journal times, so no time passes
			emit(m.add(line, time.Time{}))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
	}
	emit(m.close())
	return st, nil
}
