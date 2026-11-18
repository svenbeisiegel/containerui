package web

import (
	"context"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/diskwalk"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	metricsWindow  = 15 * time.Minute // history kept and charted
	metricsTimeout = 30 * time.Second // one stats sample
	// diskTimeout bounds one disk usage run, measuring the bind-mounted
	// host paths included.
	diskTimeout = 10 * time.Minute
	// diskMinGap: opening the dashboard measures the disk usage again
	// unless the last result is newer than this.
	diskMinGap = time.Minute
	// viewerMin is how long a viewer counts as present after the dashboard
	// was last rendered (it polls every 5s), at least 3 dashboard
	// intervals.
	viewerMin = 30 * time.Second
)

// metricPoint is one sample of a container. CPU is NaN when unknown. Read
// and Write are rates in bytes/s, negative when unknown (the first sample
// after the container appeared or its counters were reset).
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
	Series   []metricSeries // sorted by name
	Err      string
	Disk     *runtime.DiskUsage // nil until the first disk sample
	DiskErr  string
	DiskAt   time.Time // when Disk was measured
	DiskBusy bool      // a disk usage run is in progress
}

// metricsSampler monitors the containers in the background from the start:
// CPU, RAM and disk I/O every interval, every fast interval while the
// dashboard is shown, keeping metricsWindow of history in memory; the disk
// usage every diskEvery and when the dashboard is opened. Nothing is
// written to disk.
type metricsSampler struct {
	st  runtime.Stater
	log *slog.Logger
	now func() time.Time

	interval, fast, window, diskEvery time.Duration

	ctx    context.Context // cancelled by close
	cancel context.CancelFunc
	wake   chan struct{} // a viewer arrived: sample now
	disk   chan struct{} // measure the disk usage now

	mu        sync.Mutex
	series    map[string]*metricSeries
	err       string
	du        *runtime.DiskUsage
	diskErr   string
	diskAt    time.Time
	diskBusy  bool
	lastTouch time.Time
	slots     map[string]int // color slot by series ID, see assignSlots
	started   bool
	closed    bool
	wg        sync.WaitGroup
}

func newMetricsSampler(st runtime.Stater, log *slog.Logger, cfg config.Metrics) *metricsSampler {
	d := config.Default().Metrics
	ctx, cancel := context.WithCancel(context.Background())
	return &metricsSampler{
		st: st, log: log, now: time.Now,
		interval:  cmpOr(cfg.Interval, d.Interval),
		fast:      cmpOr(cfg.DashboardInterval, d.DashboardInterval),
		window:    metricsWindow,
		diskEvery: cmpOr(cfg.DiskInterval, d.DiskInterval),
		ctx:       ctx, cancel: cancel,
		wake: make(chan struct{}, 1), disk: make(chan struct{}, 1),
		series: map[string]*metricSeries{},
	}
}

func cmpOr(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// start begins the background sampling.
func (m *metricsSampler) start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	m.wg.Add(2)
	go m.statsLoop()
	go m.diskLoop()
}

// viewing reports whether the dashboard is being looked at; the caller
// holds m.mu.
func (m *metricsSampler) viewing() bool {
	return !m.lastTouch.IsZero() && m.now().Sub(m.lastTouch) <= max(3*m.fast, viewerMin)
}

// touch records that the dashboard was rendered. A viewer arriving makes
// the sampler take a sample right away and then sample every fast
// interval.
func (m *metricsSampler) touch() {
	m.mu.Lock()
	arrived := !m.viewing()
	m.lastTouch = m.now()
	m.mu.Unlock()
	if arrived {
		signal(m.wake)
	}
}

// refreshDisk asks for one more disk usage run, unless one is running or
// the last result is recent.
func (m *metricsSampler) refreshDisk() {
	m.mu.Lock()
	skip := m.diskBusy || (!m.diskAt.IsZero() && m.now().Sub(m.diskAt) < diskMinGap)
	m.mu.Unlock()
	if !skip {
		signal(m.disk)
	}
}

// signal wakes a loop without blocking; a pending signal is enough.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (m *metricsSampler) statsLoop() {
	defer m.wg.Done()
	for {
		m.sample()
		m.mu.Lock()
		next := m.interval
		if m.viewing() {
			next = m.fast
		}
		m.mu.Unlock()
		t := time.NewTimer(next)
		select {
		case <-m.ctx.Done():
			t.Stop()
			return
		case <-m.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

func (m *metricsSampler) diskLoop() {
	defer m.wg.Done()
	for {
		m.sampleDisk()
		t := time.NewTimer(m.diskEvery)
		select {
		case <-m.ctx.Done():
			t.Stop()
			return
		case <-m.disk:
			t.Stop()
		case <-t.C:
		}
	}
}

func (m *metricsSampler) sample() {
	ctx, cancel := context.WithTimeout(m.ctx, metricsTimeout)
	defer cancel()
	stats, err := m.st.Stats(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if err != nil && m.ctx.Err() != nil {
		return // closing
	}
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

// sampleDisk measures the disk usage: the runtime's, then the host paths
// the containers bind-mount. The previous result stays shown until the new
// one is complete.
func (m *metricsSampler) sampleDisk() {
	m.mu.Lock()
	m.diskBusy = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, diskTimeout)
	defer cancel()
	du, err := m.st.DiskUsage(ctx)
	if err == nil {
		measureBinds(ctx, &du)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diskBusy = false
	if m.ctx.Err() != nil {
		return // closing
	}
	if err != nil {
		m.diskErr = userError(err)
		m.log.Debug("disk usage failed", "error", err)
		return
	}
	m.du, m.diskErr, m.diskAt = &du, "", m.now()
}

// measureBinds adds the sizes of the bind-mounted host paths that du does
// not size yet.
func measureBinds(ctx context.Context, du *runtime.DiskUsage) {
	var paths []string
	for _, c := range du.Containers {
		for _, b := range c.Binds {
			if _, ok := du.Binds[b]; !ok {
				paths = append(paths, b)
			}
		}
	}
	if len(paths) == 0 {
		return
	}
	if du.Binds == nil {
		du.Binds = map[string]runtime.BindSize{}
	}
	maps.Copy(du.Binds, diskwalk.Measure(ctx, paths, nil))
}

func (m *metricsSampler) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := metricsSnapshot{Err: m.err, DiskErr: m.diskErr, Disk: m.du, DiskAt: m.diskAt, DiskBusy: m.diskBusy}
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
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}
