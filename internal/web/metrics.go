package web

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	metricsInterval = 5 * time.Second  // one stats sample
	metricsWindow   = 15 * time.Minute // history kept and charted
	metricsIdle     = 15 * time.Minute // sampling stops this long after the dashboard was last shown
	diskInterval    = time.Minute      // "system df" is slow on large hosts
	metricsTimeout  = 30 * time.Second
)

// metricPoint is one sample of a container. Read and Write are rates in
// bytes/s, negative when unknown (the first sample after the container
// appeared or its counters were reset).
type metricPoint struct {
	T           time.Time
	CPU         float64
	Mem         uint64
	Read, Write float64
}

type metricSeries struct {
	ID, Name string
	MemLimit uint64
	Points   []metricPoint

	lastRead, lastWrite uint64
	lastT               time.Time
}

// metricsSnapshot is a copy of the sampler state for rendering.
type metricsSnapshot struct {
	Series  []metricSeries // sorted by name
	Err     string
	Disk    *runtime.DiskUsage // nil until the first disk sample
	DiskErr string
	Started time.Time // when the current sampling run started
}

// metricsSampler samples container stats in the background while the
// dashboard is being looked at, keeping metricsWindow of history in memory.
// Nothing is written to disk.
type metricsSampler struct {
	st  runtime.Stater
	log *slog.Logger
	now func() time.Time

	interval, window, idle, diskEvery time.Duration

	mu        sync.Mutex
	series    map[string]*metricSeries
	err       string
	disk      *runtime.DiskUsage
	diskErr   string
	diskAt    time.Time
	diskBusy  bool
	lastTouch time.Time
	started   time.Time
	slots     map[string]int // color slot by series ID, see assignSlots
	running   bool
	stop      chan struct{}
	closed    bool
	wg        sync.WaitGroup
}

func newMetricsSampler(st runtime.Stater, log *slog.Logger) *metricsSampler {
	return &metricsSampler{
		st: st, log: log, now: time.Now,
		interval: metricsInterval, window: metricsWindow, idle: metricsIdle, diskEvery: diskInterval,
		series: map[string]*metricSeries{}, stop: make(chan struct{}),
	}
}

// touch records that the dashboard was shown and starts sampling if it is
// not running.
func (m *metricsSampler) touch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.lastTouch = m.now()
	if m.running {
		return
	}
	m.running, m.started = true, m.now()
	m.wg.Add(1)
	go m.loop()
}

func (m *metricsSampler) loop() {
	defer m.wg.Done()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		m.sampleDisk()
		m.sample()
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		m.mu.Lock()
		if m.now().Sub(m.lastTouch) > m.idle {
			m.running = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
}

func (m *metricsSampler) sample() {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	stats, err := m.st.Stats(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if err != nil {
		m.err = userError(err)
		m.log.Debug("container stats failed", "error", err)
	} else {
		m.err = ""
		for _, s := range stats {
			m.add(s, now)
		}
	}
	m.trim(now)
}

// add appends a sample; the caller holds m.mu.
func (m *metricsSampler) add(s runtime.ContainerStats, now time.Time) {
	ser := m.series[s.ID]
	if ser == nil {
		ser = &metricSeries{ID: s.ID}
		m.series[s.ID] = ser
	}
	ser.Name, ser.MemLimit = s.Name, s.MemLimit
	p := metricPoint{T: now, CPU: s.CPUPercent, Mem: s.MemUsage, Read: -1, Write: -1}
	if dt := now.Sub(ser.lastT).Seconds(); !ser.lastT.IsZero() && dt > 0 &&
		s.BlockRead >= ser.lastRead && s.BlockWrite >= ser.lastWrite {
		p.Read = float64(s.BlockRead-ser.lastRead) / dt
		p.Write = float64(s.BlockWrite-ser.lastWrite) / dt
	}
	ser.lastRead, ser.lastWrite, ser.lastT = s.BlockRead, s.BlockWrite, now
	ser.Points = append(ser.Points, p)
}

// trim drops points older than the window and series without points; the
// caller holds m.mu.
func (m *metricsSampler) trim(now time.Time) {
	cut := now.Add(-m.window)
	for id, ser := range m.series {
		i := 0
		for i < len(ser.Points) && ser.Points[i].T.Before(cut) {
			i++
		}
		ser.Points = append(ser.Points[:0], ser.Points[i:]...)
		if len(ser.Points) == 0 {
			delete(m.series, id)
		}
	}
}

// sampleDisk refreshes the disk usage in its own goroutine at most every
// diskEvery, so a slow "system df" never delays the stats samples.
func (m *metricsSampler) sampleDisk() {
	m.mu.Lock()
	if m.diskBusy || (!m.diskAt.IsZero() && m.now().Sub(m.diskAt) < m.diskEvery) {
		m.mu.Unlock()
		return
	}
	m.diskBusy, m.diskAt = true, m.now()
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
		defer cancel()
		du, err := m.st.DiskUsage(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.diskBusy = false
		if err != nil {
			m.diskErr = userError(err)
			m.log.Debug("disk usage failed", "error", err)
			return
		}
		m.disk, m.diskErr = &du, ""
	}()
}

func (m *metricsSampler) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := metricsSnapshot{Err: m.err, DiskErr: m.diskErr, Disk: m.disk, Started: m.started}
	for _, ser := range m.series {
		c := *ser
		c.Points = append([]metricPoint(nil), ser.Points...)
		snap.Series = append(snap.Series, c)
	}
	sort.Slice(snap.Series, func(i, j int) bool { return snap.Series[i].Name < snap.Series[j].Name })
	return snap
}

// close stops sampling and waits for running samples.
func (m *metricsSampler) close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
	}
	m.mu.Unlock()
	m.wg.Wait()
}
