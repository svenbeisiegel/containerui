package events

import (
	"context"
	"log/slog"
	"time"
)

// Source is the system log the error events come from.
type Source interface {
	// Follow calls line with each line journalctl
	// --output=short-iso-precise writes for the error entries from since
	// on. It keeps following new entries until ctx ends or the reader
	// fails.
	Follow(ctx context.Context, since time.Time, line func([]byte)) error
}

const (
	batchSize     = 100
	flushInterval = time.Second
	pruneInterval = time.Hour
	minBackoff    = time.Second
	maxBackoff    = time.Minute
)

// Collector follows a Source and adds its events to a Store.
type Collector struct {
	Store  *Store
	Source Source
	Log    *slog.Logger
}

// Run collects until ctx ends. The first run reads the last Retention of
// the log; later ones resume at the stored journal time, so nothing logged
// while ContainerUI was stopped is missed. A reader that ends is restarted
// with a growing delay.
func (c *Collector) Run(ctx context.Context) {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()
	backoff := minBackoff
	for ctx.Err() == nil {
		since := c.Store.Since()
		if since.IsZero() {
			since = time.Now().Add(-Retention)
			log.Info("importing error events from the journal", "since", Retention, "dir", c.Store.Dir())
		} else {
			log.Debug("following the journal for error events", "since", since, "dir", c.Store.Dir())
		}
		started := time.Now()
		err := c.follow(ctx, since, prune.C, log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		log.Warn("journal reader for error events ended; restarting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// follow runs one reader and stores its events in batches until it ends.
func (c *Collector) follow(ctx context.Context, since time.Time, prune <-chan time.Time, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan []byte, batchSize)
	done := make(chan error, 1)
	go func() {
		done <- c.Source.Follow(ctx, since, func(line []byte) {
			select {
			case lines <- line:
			case <-ctx.Done():
			}
		})
	}()

	// The time stored is that of the oldest message that may still get
	// lines, so a restart reads it again instead of losing it; the events
	// read again have the same ids and are skipped.
	m := newMerger()
	var batch []Event
	take := func(line []byte) {
		batch = append(batch, m.add(string(line), time.Now())...)
	}
	flush := func() {
		pos := m.resume()
		if len(batch) == 0 && (pos.IsZero() || pos.Equal(c.Store.Since())) {
			return
		}
		if err := c.Store.Add(batch, pos); err != nil {
			log.Error("store error events", "error", err)
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	for {
		select {
		case line := <-lines:
			take(line)
			if len(batch) >= batchSize {
				flush()
			}
		case <-tick.C:
			batch = append(batch, m.expire(time.Now())...)
			flush()
		case <-prune:
			flush()
			if err := c.Store.Prune(); err != nil {
				log.Warn("prune error events", "error", err)
			}
		case err := <-done:
			// the reader has stopped sending; take what it left
		drain:
			for {
				select {
				case line := <-lines:
					take(line)
				default:
					break drain
				}
			}
			batch = append(batch, m.close()...)
			flush()
			return err
		}
	}
}
