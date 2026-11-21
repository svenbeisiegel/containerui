package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// stubStater returns scripted samples.
type stubStater struct {
	mu        sync.Mutex
	stats     []runtime.ContainerStats
	du        runtime.DiskUsage
	err       error
	calls     int
	diskCalls int
}

func (s *stubStater) Stats(context.Context) ([]runtime.ContainerStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return append([]runtime.ContainerStats(nil), s.stats...), s.err
}

func (s *stubStater) DiskUsage(context.Context) (runtime.DiskUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diskCalls++
	du := s.du
	du.Containers = append([]runtime.ContainerDisk(nil), s.du.Containers...)
	if s.du.Binds != nil {
		du.Binds = maps.Clone(s.du.Binds)
	}
	return du, s.err
}

func (s *stubStater) counts() (stats, disk int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.diskCalls
}

func newTestSampler(st runtime.Stater, now *time.Time) *metricsSampler {
	m := newMetricsSampler(st, slog.New(slog.NewTextHandler(io.Discard, nil)), config.Metrics{})
	m.now = func() time.Time { return *now }
	return m
}

// clock is a settable time for samplers whose loops run.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSamplerRatesAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	st := &stubStater{stats: []runtime.ContainerStats{{ID: "a", Name: "web", CPUPercent: 10, MemUsage: 100, BlockRead: 1000, BlockWrite: 0}}}
	m := newTestSampler(st, &now)

	m.sample()
	now = now.Add(5 * time.Second)
	st.stats[0].BlockRead, st.stats[0].BlockWrite = 6000, 500
	m.sample()
	now = now.Add(5 * time.Second)
	st.stats[0].BlockRead = 10 // restarted: counters reset
	m.sample()

	snap := m.snapshot()
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
	p := snap.Series[0].Points
	if p[0].Read != -1 {
		t.Errorf("first rate should be unknown, got %v", p[0].Read)
	}
	if p[1].Read != 1000 || p[1].Write != 100 {
		t.Errorf("rate = %v/%v, want 1000/100", p[1].Read, p[1].Write)
	}
	if p[2].Read != -1 {
		t.Errorf("reset counter should give unknown rate, got %v", p[2].Read)
	}

	// A container that is gone disappears once its points leave the window.
	st.stats = nil
	now = now.Add(metricsWindow + time.Second)
	m.sample()
	if snap := m.snapshot(); len(snap.Series) != 0 {
		t.Errorf("stale series kept: %+v", snap.Series)
	}

	st.err = errors.New("boom")
	m.sample()
	if snap := m.snapshot(); snap.Err == "" {
		t.Error("stats error not reported")
	}
}

func TestSamplerIntervals(t *testing.T) {
	st := &stubStater{stats: []runtime.ContainerStats{{ID: "a", Name: "web"}}}
	clk := &clock{t: time.Now()}
	m := newMetricsSampler(st, slog.New(slog.NewTextHandler(io.Discard, nil)),
		config.Metrics{Interval: time.Hour, DashboardInterval: 5 * time.Millisecond, DiskInterval: time.Hour})
	m.now = clk.now
	m.start()
	defer m.close()

	// The background samples once at start, then waits for the interval.
	waitFor(t, "the first sample", func() bool { n, d := st.counts(); return n == 1 && d == 1 })
	time.Sleep(20 * time.Millisecond)
	if n, _ := st.counts(); n != 1 {
		t.Fatalf("%d samples without a viewer, want 1", n)
	}

	// A viewer: a sample right away, then every dashboard interval.
	m.touch()
	waitFor(t, "fast sampling", func() bool { n, _ := st.counts(); return n >= 5 })

	// The viewer left: back to the interval after the current wait.
	clk.add(time.Minute)
	time.Sleep(20 * time.Millisecond)
	before, _ := st.counts()
	time.Sleep(30 * time.Millisecond)
	if after, _ := st.counts(); after != before {
		t.Errorf("still sampling fast without a viewer: %d -> %d", before, after)
	}
}

func TestRefreshDisk(t *testing.T) {
	bind := t.TempDir()
	if err := os.WriteFile(filepath.Join(bind, "data"), make([]byte, 50_000), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &stubStater{du: runtime.DiskUsage{
		Containers: []runtime.ContainerDisk{
			{Name: "db", Binds: []string{bind, "/var/run/docker.sock", "/demo"}},
		},
		Binds: map[string]runtime.BindSize{"/demo": {Size: 7}}, // sized by the runtime already
	}}
	clk := &clock{t: time.Now()}
	m := newMetricsSampler(st, slog.New(slog.NewTextHandler(io.Discard, nil)), config.Metrics{})
	m.now = clk.now
	m.start()
	defer m.close()
	waitFor(t, "the first disk usage", func() bool { return !m.snapshot().DiskAt.IsZero() })

	snap := m.snapshot()
	binds := snap.Disk.Binds
	if binds[bind].Size < 50_000 || binds[bind].Note != "" {
		t.Errorf("bind = %+v, want at least the file", binds[bind])
	}
	if binds["/var/run/docker.sock"].Note != runtime.BindSkipped || binds["/demo"].Size != 7 {
		t.Errorf("binds = %+v", binds)
	}

	// Opening the dashboard right after a measurement does not measure again.
	m.refreshDisk()
	time.Sleep(20 * time.Millisecond)
	if _, d := st.counts(); d != 1 {
		t.Errorf("disk measured %d times, want 1", d)
	}
	clk.add(2 * diskMinGap)
	m.refreshDisk()
	// Wait for the run to finish, not just start: refreshDisk skips while
	// one is busy (sizing the binds takes a while on slow runners).
	waitFor(t, "the second disk usage", func() bool { _, d := st.counts(); return d == 2 && !m.snapshot().DiskBusy })

	// A failure keeps the previous result.
	st.mu.Lock()
	st.err = errors.New("df failed")
	st.mu.Unlock()
	clk.add(2 * diskMinGap)
	m.refreshDisk()
	waitFor(t, "the failed disk usage", func() bool { return m.snapshot().DiskErr != "" })
	if snap := m.snapshot(); snap.Disk == nil || snap.DiskBusy {
		t.Errorf("snapshot after failure = %+v", snap)
	}
}

func TestUnknownCPU(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	m := newTestSampler(&stubStater{}, &now)
	var series []metricSeries
	for i := 0; i < maxSeries+2; i++ {
		series = append(series, metricSeries{ID: string(rune('a' + i)), Name: string(rune('a' + i)),
			Points: []metricPoint{{T: now.Add(-time.Minute), CPU: math.NaN(), Mem: 10, Read: -1, Write: -1}, {T: now, CPU: float64(i), Mem: 10, Read: -1, Write: -1}}})
	}
	v := m.buildMetricsView(metricsSnapshot{Series: series}, now)
	if v.Collecting || len(v.Legend) != maxSeries+1 {
		t.Fatalf("view = %+v", v)
	}
	if strings.Contains(v.CPU.Data, "NaN") {
		t.Errorf("unknown CPU leaked into the chart: %s", v.CPU.Data)
	}
	cur := currentFor([]runtime.Container{{ID: "a"}}, metricsSnapshot{Series: []metricSeries{{ID: "a", Points: []metricPoint{{T: now, CPU: math.NaN(), Read: -1, Write: -1}}}}}, now, time.Minute)
	if cur["a"].CPU != "–" {
		t.Errorf("unknown CPU shown as %q", cur["a"].CPU)
	}
}

func TestNiceScales(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, 3: 4, 7: 8, 12: 20, 23: 25, 99: 100, 101: 200, 0.3: 0.4} {
		if got := niceCeil(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("niceCeil(%v) = %v, want %v", in, got, want)
		}
	}
	if got := nicePercent(55); got != 100 {
		t.Errorf("nicePercent(55) = %v", got)
	}
	if got := nicePercent(3); got != 10 {
		t.Errorf("nicePercent(3) = %v", got)
	}
	if got := nicePercent(180); got != 200 {
		t.Errorf("nicePercent(180) = %v", got)
	}
	if got := niceBytes(300 << 20); got != 400<<20 {
		t.Errorf("niceBytes(300MiB) = %v", got)
	}
	if got := formatBytes(1 << 30); got != "1 GiB" {
		t.Errorf("formatBytes = %q", got)
	}
}

func TestLineChart(t *testing.T) {
	end := time.Date(2026, 11, 14, 10, 15, 0, 0, time.UTC)
	t0 := end.Add(-10 * time.Minute)
	s := chartSeries{Name: "web", Class: "s1",
		T: []time.Time{t0, t0.Add(5 * time.Second), t0.Add(5 * time.Minute), t0.Add(5*time.Minute + 5*time.Second)},
		V: []float64{10, 20, math.NaN(), 40}}
	c := buildLineChart("CPU", []chartSeries{s}, end, 15*time.Minute, 15*time.Second, nicePercent, formatPercent)
	if len(c.YTicks) != 5 || c.YTicks[0] != "100%" || c.YTicks[4] != "0%" {
		t.Errorf("y ticks = %v", c.YTicks)
	}
	if c.XTicks[0] != "−15 min" || c.XTicks[3] != "now" {
		t.Errorf("x ticks = %v", c.XTicks)
	}
	if len(c.Paths) != 1 {
		t.Fatalf("paths = %+v", c.Paths)
	}
	// x of t0 is 5/15 of the width; the NaN and the long gap start a new segment
	if d := c.Paths[0].D; !strings.HasPrefix(d, "M200 144") || strings.Count(d, "M") != 2 {
		t.Errorf("path = %q", d)
	}
	if c.Paths[0].Title != "web: 40%" {
		t.Errorf("title = %q", c.Paths[0].Title)
	}
	if !strings.Contains(c.Data, `"n":"web"`) || !strings.Contains(c.Data, `"40%"`) {
		t.Errorf("hover data = %s", c.Data)
	}
}

func TestDonut(t *testing.T) {
	var slices []slice
	for i, v := range []float64{50, 20, 10, 8, 5, 3, 2, 1, 1} {
		slices = append(slices, slice{Name: string(rune('a' + i)), Value: v})
	}
	c := buildDonut("Disk", slices, formatBytes)
	if len(c.Arcs) != 7 || c.Legend[6].Name != otherName || c.Legend[6].Class != otherClass {
		t.Fatalf("legend = %+v", c.Legend)
	}
	if c.Legend[0].Pct != "50%" || c.Legend[6].Pct != "4%" {
		t.Errorf("pcts = %+v", c.Legend)
	}
	if empty := buildDonut("x", nil, formatBytes); len(empty.Arcs) != 0 {
		t.Error("empty donut has arcs")
	}
}

func TestDiskChartsVolumes(t *testing.T) {
	du := runtime.DiskUsage{
		Containers: []runtime.ContainerDisk{
			{Name: "db", Size: 10, Mounts: []string{"dbdata", "shared"}},
			{Name: "web", Size: 5, Mounts: []string{"shared", "4701ab…"}},
		},
		Volumes:    map[string]uint64{"dbdata": 1000, "shared": 300, "4701abcdef": 85, "unused": 9999},
		Categories: []runtime.DiskCategory{{Name: "Images", Size: 100}, {Name: "Build cache"}},
	}
	per, cat := diskCharts(du)
	got := map[string]string{}
	for _, l := range per.Legend {
		got[l.Name] = l.Value
	}
	if got["db"] != "1010 B" || got["web"] != "90 B" || got["shared mounts"] != "300 B" {
		t.Errorf("per container = %v", got)
	}
	if len(cat.Legend) != 1 || cat.Legend[0].Name != "Images" {
		t.Errorf("categories = %+v", cat.Legend)
	}
}

func TestDiskChartsBinds(t *testing.T) {
	du := runtime.DiskUsage{
		Containers: []runtime.ContainerDisk{
			{Name: "db", Size: 10, Mounts: []string{"data"}, Binds: []string{"/srv/db", "/srv/common"}},
			{Name: "web", Size: 200, Mounts: []string{"data"}, Binds: []string{"/srv/common", "/var/run/docker.sock"}},
			{Name: "app", Size: 100, Binds: []string{"data"}}, // a host path named like the volume
		},
		Volumes: map[string]uint64{"data": 100},
		Binds: map[string]runtime.BindSize{
			"/srv/db": {Size: 600}, "/srv/common": {Size: 400}, "data": {Size: 30},
			"/var/run/docker.sock": {Note: runtime.BindSkipped},
		},
		Categories: []runtime.DiskCategory{{Name: "Images", Size: 100}},
	}
	per, cat := diskCharts(du)
	got := map[string]string{}
	for _, l := range per.Legend {
		got[l.Name] = l.Value
	}
	// shared: the volume (100) and /srv/common (400)
	if got["db"] != "610 B" || got["web"] != "200 B" || got["app"] != "130 B" || got["shared mounts"] != "500 B" {
		t.Errorf("per container = %v", got)
	}
	cats := map[string]string{}
	for _, l := range cat.Legend {
		cats[l.Name] = l.Value
	}
	if cats["Bind mounts"] != "1 KiB" || cats["Images"] != "100 B" {
		t.Errorf("categories = %v", cats)
	}
	if u := unmeasured(du); len(u) != 1 || u[0] != "/var/run/docker.sock (skipped: system path)" {
		t.Errorf("unmeasured = %v", u)
	}
}

func TestChooseSeriesAndStableColors(t *testing.T) {
	now := time.Now()
	m := newTestSampler(&stubStater{}, &now)
	var series []metricSeries
	for i := 0; i < 8; i++ {
		series = append(series, metricSeries{ID: string(rune('a' + i)), Name: string(rune('a' + i)),
			Points: []metricPoint{{T: now, CPU: float64(i), Mem: uint64(i), Read: -1, Write: -1}, {T: now, CPU: float64(i), Mem: uint64(i), Read: -1, Write: -1}}})
	}
	v := m.buildMetricsView(metricsSnapshot{Series: series}, now)
	if len(v.Legend) != maxSeries+1 || v.Legend[maxSeries].Name != "other (2)" {
		t.Fatalf("legend = %+v", v.Legend)
	}
	first := map[string]string{}
	for _, l := range v.Legend {
		first[l.Name] = l.Class
	}
	if _, ok := first["a"]; ok {
		t.Error("the idlest container should be in other")
	}
	// Same containers again: same colors.
	v2 := m.buildMetricsView(metricsSnapshot{Series: series}, now)
	for _, l := range v2.Legend {
		if first[l.Name] != l.Class {
			t.Errorf("%s changed color %s -> %s", l.Name, first[l.Name], l.Class)
		}
	}
}

func TestDashboardMetrics(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.do("GET", "/", nil).Body.String()
	for _, want := range []string{"Container Metrics", "<th class=\"num\">CPU</th>"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The fake samples immediately; after two samples the charts render.
	m := e.srv.metrics
	m.sample()
	time.Sleep(10 * time.Millisecond)
	m.sample()
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = e.do("GET", "/partials/dashboard", nil, htmx).Body.String()
		if strings.Contains(body, `class="chart"`) && strings.Contains(body, `class="donut"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{`class="chart"`, "CPU usage", "RAM usage", "Disk I/O", `class="donut"`, "Images", `class="swatch s1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard partial missing %q", want)
		}
	}
	if strings.Contains(body, "style=") {
		t.Error("charts must not use inline styles (CSP)")
	}
}

func TestDashboardWithoutStats(t *testing.T) {
	e := newEnv(t)
	e.rt.NoStats = true
	e.login()
	w := e.do("GET", "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "Container Metrics") || strings.Contains(body, ">CPU<") {
		t.Error("metrics shown although the runtime reports no stats")
	}
}
