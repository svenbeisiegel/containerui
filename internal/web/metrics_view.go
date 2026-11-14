package web

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// metricsView is the metrics part of the dashboard.
type metricsView struct {
	Err        string
	Collecting bool // fewer than two samples so far
	Legend     []legendItem
	CPU        lineChart
	Mem        lineChart
	IO         lineChart

	DiskErr        string
	DiskLoading    bool
	DiskContainers donutChart
	DiskCategories donutChart
}

// currentStats are a running container's latest values for the table.
type currentStats struct {
	CPU, Mem, IO string
}

// assignSlots gives each chosen series a color slot (1..maxSeries) and keeps
// it while the series stays chosen, so a container keeps its color across
// refreshes. New series take the free slots in the order of ids.
func (m *metricsSampler) assignSlots(ids []string) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slots == nil {
		m.slots = map[string]int{}
	}
	chosen := map[string]bool{}
	for _, id := range ids {
		chosen[id] = true
	}
	used := map[int]bool{}
	for id, s := range m.slots {
		if chosen[id] {
			used[s] = true
		} else {
			delete(m.slots, id)
		}
	}
	for _, id := range ids {
		if _, ok := m.slots[id]; ok {
			continue
		}
		for s := 1; s <= maxSeries; s++ {
			if !used[s] {
				m.slots[id], used[s] = s, true
				break
			}
		}
	}
	out := make(map[string]int, len(m.slots))
	for id, s := range m.slots {
		out[id] = s
	}
	return out
}

func lastPoint(s metricSeries) metricPoint { return s.Points[len(s.Points)-1] }

func ioRate(p metricPoint) float64 {
	if p.Read < 0 || p.Write < 0 {
		return math.NaN()
	}
	return p.Read + p.Write
}

// chooseSeries picks the maxSeries containers with the largest share of
// CPU, memory or disk I/O at the latest sample.
func chooseSeries(series []metricSeries) (chosen, rest []metricSeries) {
	if len(series) <= maxSeries {
		return series, nil
	}
	var cpu, mem, io float64
	for _, s := range series {
		p := lastPoint(s)
		cpu += p.CPU
		mem += float64(p.Mem)
		if r := ioRate(p); !math.IsNaN(r) {
			io += r
		}
	}
	share := func(v, total float64) float64 {
		if total <= 0 || math.IsNaN(v) {
			return 0
		}
		return v / total
	}
	score := map[string]float64{}
	for _, s := range series {
		p := lastPoint(s)
		score[s.ID] = max(share(p.CPU, cpu), share(float64(p.Mem), mem), share(ioRate(p), io))
	}
	ranked := append([]metricSeries(nil), series...)
	sort.SliceStable(ranked, func(i, j int) bool { return score[ranked[i].ID] > score[ranked[j].ID] })
	chosen, rest = ranked[:maxSeries], ranked[maxSeries:]
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Name < chosen[j].Name })
	return chosen, rest
}

// buildMetricsView renders the snapshot's charts as of now.
func (m *metricsSampler) buildMetricsView(snap metricsSnapshot, now time.Time) *metricsView {
	v := &metricsView{Err: snap.Err, DiskErr: snap.DiskErr, DiskLoading: snap.Disk == nil && snap.DiskErr == ""}
	if snap.Disk != nil {
		v.DiskContainers, v.DiskCategories = diskCharts(*snap.Disk)
	}
	enough := false
	for _, s := range snap.Series {
		if len(s.Points) >= 2 {
			enough = true
		}
	}
	v.Collecting = !enough
	if !enough {
		return v
	}

	chosen, rest := chooseSeries(snap.Series)
	ids := make([]string, len(chosen)) // in legend (name) order, so new slots follow it
	for i, s := range chosen {
		ids[i] = s.ID
	}
	slots := m.assignSlots(ids)

	var cpu, mem, io []chartSeries
	for _, s := range chosen {
		class := "s" + strconv.Itoa(slots[s.ID])
		v.Legend = append(v.Legend, legendItem{Class: class, Name: s.Name})
		c := chartSeries{Name: s.Name, Class: class}
		mm, ii := c, c
		for _, p := range s.Points {
			c.T, c.V = append(c.T, p.T), append(c.V, p.CPU)
			mm.T, mm.V = append(mm.T, p.T), append(mm.V, float64(p.Mem))
			ii.T, ii.V = append(ii.T, p.T), append(ii.V, ioRate(p))
			ii.Tip = append(ii.Tip, ioTip(p))
		}
		cpu, mem, io = append(cpu, c), append(mem, mm), append(io, ii)
	}
	if len(rest) > 0 {
		name := otherName + " (" + strconv.Itoa(len(rest)) + ")"
		v.Legend = append(v.Legend, legendItem{Class: otherClass, Name: name})
		c, mm, ii := sumSeries(rest, name)
		cpu, mem, io = append(cpu, c), append(mem, mm), append(io, ii)
	}
	gap := 3 * m.interval
	v.CPU = buildLineChart("CPU usage", cpu, now, m.window, gap, nicePercent, formatPercent)
	v.Mem = buildLineChart("RAM usage", mem, now, m.window, gap, niceBytes, formatBytes)
	v.IO = buildLineChart("Disk I/O", io, now, m.window, gap, niceBytes, formatRate)
	return v
}

func ioTip(p metricPoint) string {
	if p.Read < 0 || p.Write < 0 {
		return ""
	}
	return "read " + formatRate(p.Read) + " · write " + formatRate(p.Write)
}

// sumSeries adds up the containers without their own color per sample
// time (all containers of one sample share its time).
func sumSeries(rest []metricSeries, name string) (cpu, mem, io chartSeries) {
	type acc struct {
		cpu, mem, read, write float64
		ioKnown               bool
	}
	byT := map[int64]*acc{}
	var times []time.Time
	for _, s := range rest {
		for _, p := range s.Points {
			a := byT[p.T.UnixNano()]
			if a == nil {
				a = &acc{}
				byT[p.T.UnixNano()] = a
				times = append(times, p.T)
			}
			a.cpu += p.CPU
			a.mem += float64(p.Mem)
			if p.Read >= 0 && p.Write >= 0 {
				a.read, a.write, a.ioKnown = a.read+p.Read, a.write+p.Write, true
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	cpu = chartSeries{Name: name, Class: otherClass}
	mem, io = cpu, cpu
	for _, t := range times {
		a := byT[t.UnixNano()]
		cpu.T, cpu.V = append(cpu.T, t), append(cpu.V, a.cpu)
		mem.T, mem.V = append(mem.T, t), append(mem.V, a.mem)
		p := metricPoint{Read: -1, Write: -1}
		if a.ioKnown {
			p.Read, p.Write = a.read, a.write
		}
		io.T, io.V, io.Tip = append(io.T, t), append(io.V, ioRate(p)), append(io.Tip, ioTip(p))
	}
	return cpu, mem, io
}

// diskCharts builds the per-container and per-category donuts. A container's
// slice is its writable layer plus the volumes only it mounts; volumes that
// several containers mount are one "shared volumes" slice.
func diskCharts(du runtime.DiskUsage) (perContainer, byCategory donutChart) {
	resolve := func(mount string) (string, bool) {
		mount = strings.TrimSuffix(mount, "…")
		if _, ok := du.Volumes[mount]; ok {
			return mount, true
		}
		match := ""
		for name := range du.Volumes {
			if strings.HasPrefix(name, mount) {
				if match != "" {
					return "", false // ambiguous
				}
				match = name
			}
		}
		return match, match != ""
	}
	users := map[string]int{}
	mounts := make([][]string, len(du.Containers))
	for i, c := range du.Containers {
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			if name, ok := resolve(m); ok && !seen[name] {
				seen[name] = true
				users[name]++
				mounts[i] = append(mounts[i], name)
			}
		}
	}
	var slices []slice
	shared := 0.0
	for name, n := range users {
		if n > 1 {
			shared += float64(du.Volumes[name])
		}
	}
	for i, c := range du.Containers {
		size := float64(c.Size)
		for _, name := range mounts[i] {
			if users[name] == 1 {
				size += float64(du.Volumes[name])
			}
		}
		slices = append(slices, slice{Name: c.Name, Value: size})
	}
	if shared > 0 {
		slices = append(slices, slice{Name: "shared volumes", Value: shared})
	}
	perContainer = buildDonut("Disk usage per container", slices, formatBytes)

	var cats []slice
	for _, c := range du.Categories {
		cats = append(cats, slice{Name: c.Name, Value: float64(c.Size)})
	}
	byCategory = buildDonut("Disk usage by category", cats, formatBytes)
	return perContainer, byCategory
}

// currentFor maps the running containers to their latest sample. Stats IDs
// may be short (podman), so a prefix of the container ID matches too.
func currentFor(running []runtime.Container, snap metricsSnapshot, now time.Time, fresh time.Duration) map[string]currentStats {
	out := map[string]currentStats{}
	for _, c := range running {
		for _, s := range snap.Series {
			if s.ID == "" || !(strings.HasPrefix(c.ID, s.ID) || strings.HasPrefix(s.ID, c.ID)) {
				continue
			}
			p := lastPoint(s)
			if now.Sub(p.T) > fresh {
				break
			}
			cur := currentStats{CPU: formatPercent(p.CPU), Mem: formatBytes(float64(p.Mem)), IO: "–"}
			if r := ioRate(p); !math.IsNaN(r) {
				cur.IO = formatRate(r)
			}
			out[c.ID] = cur
			break
		}
	}
	return out
}
