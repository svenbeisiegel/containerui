package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// stubStater returns scripted samples.
type stubStater struct {
	mu    sync.Mutex
	stats []runtime.ContainerStats
	du    runtime.DiskUsage
	err   error
	calls int
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
	return s.du, s.err
}

func newTestSampler(st runtime.Stater, now *time.Time) *metricsSampler {
	m := newMetricsSampler(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return *now }
	return m
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

func TestSamplerStopsWhenIdle(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	st := &stubStater{stats: []runtime.ContainerStats{{ID: "a", Name: "web"}}}
	m := newTestSampler(st, &now)
	m.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m.interval, m.idle = 5*time.Millisecond, 20*time.Millisecond
	m.touch()
	time.Sleep(15 * time.Millisecond)
	mu.Lock()
	now = now.Add(time.Second) // long past idle
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		running := m.running
		m.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sampler did not stop when idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.close()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.calls == 0 {
		t.Error("sampler never sampled")
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
	if got["db"] != "1010 B" || got["web"] != "90 B" || got["shared volumes"] != "300 B" {
		t.Errorf("per container = %v", got)
	}
	if len(cat.Legend) != 1 || cat.Legend[0].Name != "Images" {
		t.Errorf("categories = %+v", cat.Legend)
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
