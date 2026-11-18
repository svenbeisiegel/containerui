package web

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Charts are rendered on the server as inline SVG (templates "linechart"
// and "donut" in partials.html), so they refresh with the htmx swap of the
// dashboard and need no chart library. Colors come from CSS classes
// (s1..s6, so), never inline styles, which the Content-Security-Policy
// blocks; metrics.js adds the hover crosshair from the Data attribute.

const (
	chartW = 600 // SVG user units; the plot is stretched to the panel width
	chartH = 160
	// maxSeries containers get their own color; the rest are summed as "other".
	maxSeries  = 6
	otherClass = "so"
	otherName  = "other"
	// minSliceShare: smaller donut slices are merged into "other".
	minSliceShare = 0.02
)

type legendItem struct {
	Class, Name, Value, Pct string
}

type chartPath struct {
	Class, D, Title string
}

type lineChart struct {
	Label  string   // accessible name
	YTicks []string // top to bottom
	XTicks []string // left to right
	Grid   []string // y of the horizontal grid lines, as SVG coordinates
	Paths  []chartPath
	Data   string // hover data (JSON) for metrics.js
}

type donutArc struct {
	Class, Dash, Offset, Title string
}

type donutChart struct {
	Label  string
	Total  string
	Arcs   []donutArc
	Legend []legendItem
}

// chartSeries is one line: values at the given times (NaN = no value).
type chartSeries struct {
	Name, Class string
	T           []time.Time
	V           []float64
	Tip         []string // optional tooltip text per point; "" formats V
}

// buildLineChart lays out series over [end-window, end]. Points further
// apart than gap break the line (the sampler was not running).
func buildLineChart(label string, series []chartSeries, end time.Time, window, gap time.Duration, scale func(float64) float64, format func(float64) string) lineChart {
	c := lineChart{Label: label}
	start := end.Add(-window)
	maxV := 0.0
	for _, s := range series {
		for _, v := range s.V {
			if !math.IsNaN(v) && v > maxV {
				maxV = v
			}
		}
	}
	top := scale(maxV)
	const ticks = 4
	for i := ticks; i >= 0; i-- {
		c.YTicks = append(c.YTicks, format(top*float64(i)/ticks))
		c.Grid = append(c.Grid, num(float64(chartH)*float64(ticks-i)/ticks))
	}
	mins := int(window / time.Minute)
	for i := 0; i <= 3; i++ {
		m := mins - i*mins/3
		if m == 0 {
			c.XTicks = append(c.XTicks, "now")
		} else {
			c.XTicks = append(c.XTicks, fmt.Sprintf("−%d min", m))
		}
	}
	x := func(t time.Time) float64 { return float64(chartW) * t.Sub(start).Seconds() / window.Seconds() }
	y := func(v float64) float64 { return float64(chartH) * (1 - v/top) }

	// hover data: the union of all sample times
	type hoverSeries struct {
		N string   `json:"n"`
		C string   `json:"c"`
		V []string `json:"v"`
	}
	var hover struct {
		W int           `json:"w"`
		X []float64     `json:"x"`
		T []string      `json:"t"`
		S []hoverSeries `json:"s"`
	}
	hover.W = chartW
	var times []time.Time
	seen := map[int64]bool{}
	for _, s := range series {
		for i, t := range s.T {
			if !t.Before(start) && !math.IsNaN(s.V[i]) && !seen[t.UnixNano()] {
				seen[t.UnixNano()] = true
				times = append(times, t)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	index := make(map[int64]int, len(times))
	for i, t := range times {
		index[t.UnixNano()] = i
		hover.X = append(hover.X, round1(x(t)))
		hover.T = append(hover.T, t.Local().Format("15:04:05"))
	}

	for _, s := range series {
		var d strings.Builder
		var prev time.Time
		hs := hoverSeries{N: s.Name, C: s.Class, V: make([]string, len(times))}
		last := math.NaN()
		for i, t := range s.T {
			v := s.V[i]
			if t.Before(start) || math.IsNaN(v) {
				prev = time.Time{}
				continue
			}
			cmd := "L"
			if prev.IsZero() || t.Sub(prev) > gap {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%s %s", cmd, num(x(t)), num(y(v)))
			prev, last = t, v
			tip := format(v)
			if i < len(s.Tip) && s.Tip[i] != "" {
				tip = s.Tip[i]
			}
			hs.V[index[t.UnixNano()]] = tip
		}
		if d.Len() == 0 {
			continue
		}
		title := s.Name
		if !math.IsNaN(last) {
			title += ": " + format(last)
		}
		c.Paths = append(c.Paths, chartPath{Class: s.Class, D: d.String(), Title: title})
		hover.S = append(hover.S, hs)
	}
	if b, err := json.Marshal(hover); err == nil {
		c.Data = string(b)
	}
	return c
}

type slice struct {
	Name  string
	Value float64
}

// buildDonut lays out at most maxSeries slices by size; the rest and
// slices under minSliceShare are merged into "other".
func buildDonut(label string, slices []slice, format func(float64) string) donutChart {
	c := donutChart{Label: label}
	sort.SliceStable(slices, func(i, j int) bool { return slices[i].Value > slices[j].Value })
	total := 0.0
	for _, s := range slices {
		total += s.Value
	}
	if total <= 0 {
		return c
	}
	var kept []slice
	other := 0.0
	for _, s := range slices {
		if s.Value <= 0 {
			continue
		}
		if len(kept) < maxSeries && s.Value/total >= minSliceShare {
			kept = append(kept, s)
		} else {
			other += s.Value
		}
	}
	if other > 0 {
		kept = append(kept, slice{Name: otherName, Value: other})
	}
	c.Total = format(total)
	gap := 0.0
	if len(kept) > 1 {
		gap = 0.8 // circumference is 100: a visible surface gap between slices
	}
	pos := 0.0
	for i, s := range kept {
		class := "s" + strconv.Itoa(i+1)
		if s.Name == otherName && i == len(kept)-1 && other > 0 {
			class = otherClass
		}
		share := s.Value / total
		p := share * 100
		dash := math.Max(p-gap, 0.3)
		c.Arcs = append(c.Arcs, donutArc{
			Class:  class,
			Dash:   num(dash) + " " + num(100-dash),
			Offset: num(-pos),
			Title:  fmt.Sprintf("%s: %s (%s)", s.Name, format(s.Value), pct(share)),
		})
		c.Legend = append(c.Legend, legendItem{Class: class, Name: s.Name, Value: format(s.Value), Pct: pct(share)})
		pos += p
	}
	return c
}

// niceCeil rounds v up to 1, 2, 2.5, 4 or 5 times a power of ten, so four
// grid intervals get readable labels.
func niceCeil(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 4, 5, 8, 10} {
		if m*exp >= v*(1-1e-9) {
			return m * exp
		}
	}
	return 10 * exp
}

// niceBytes rounds a byte value up to a nice number in its binary unit.
func niceBytes(v float64) float64 {
	unit := 1.0
	for v/unit >= 1024 {
		unit *= 1024
	}
	if v < 1024 {
		return math.Max(niceCeil(v), 4)
	}
	return niceCeil(v/unit) * unit
}

// nicePercent keeps 100% as the top while usage is below it, so a busy
// core reads as full; above it (multi-core) the scale grows.
func nicePercent(v float64) float64 {
	if v <= 100 {
		if v <= 10 {
			return 10
		}
		return 100
	}
	return niceCeil(v)
}

func formatPercent(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	if v >= 10 || v == 0 {
		return strconv.FormatFloat(math.Round(v), 'f', -1, 64) + "%"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + "%"
}

// formatBytes is humanBytes for chart labels: compact, no ".0".
func formatBytes(v float64) string {
	if v < 0 || math.IsNaN(v) {
		return "–"
	}
	s := humanBytes(uint64(math.Round(v)))
	return strings.Replace(s, ".0 ", " ", 1)
}

func formatRate(v float64) string { return formatBytes(v) + "/s" }

func pct(share float64) string {
	p := share * 100
	if p < 1 {
		return "<1%"
	}
	return strconv.FormatFloat(math.Round(p), 'f', -1, 64) + "%"
}

func num(f float64) string { return strconv.FormatFloat(round1(f), 'f', -1, 64) }

func round1(f float64) float64 { return math.Round(f*10) / 10 }
