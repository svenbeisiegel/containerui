package events

import (
	"bufio"
	"io"
	"time"
)

// ReplayStats counts what Replay read.
type ReplayStats struct {
	Lines   int    // lines read
	Entries int    // journal entries: lines with a time, host and identifier
	Dropped int    // messages dropped for a level below error
	Events  int    // events after merging
	Sample  string // the first line that is not journalctl output
}

// Replay reads the output of journalctl --output=short-iso-precise (or
// another short format) from r and calls fn with the events the collector
// would store, parsed and merged the same way. Formats without a year are
// read as of now. It is for trying out what a journal gives.
func Replay(r io.Reader, now time.Time, fn func(Event)) (ReplayStats, error) {
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
		line, err := br.ReadString('\n')
		if line != "" {
			st.Lines++
			// lines are merged by their journal times, so no time passes
			emit(m.add(line, now))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
	}
	emit(m.close())
	st.Entries, st.Dropped, st.Sample = m.seq, m.dropped, m.sample
	return st, nil
}
