package dockerapi

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// statsConcurrency bounds the stats requests in flight.
const statsConcurrency = 8

type containerJSON struct {
	ID     string `json:"Id"`
	Names  []string
	State  string
	SizeRw int64
	Mounts []mountJSON
}

type mountJSON struct {
	Type   string
	Name   string
	Source string
}

func (c containerJSON) name() string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

type statsJSON struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Read time.Time `json:"read"`
	CPU  struct {
		Usage struct {
			Total  uint64   `json:"total_usage"`
			PerCPU []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
		Online uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Blkio struct {
		Bytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// cpuSample are the CPU counters of a container's previous sample.
type cpuSample struct {
	total, system uint64
	read          time.Time
}

// Stats samples all running containers once. Each container's stats are
// read once ("one-shot"); the CPU share is computed against the counters of
// the previous call, so it is the average since then and NaN for a
// container's first sample.
func (c *Client) Stats(ctx context.Context) ([]runtime.ContainerStats, error) {
	var list []containerJSON
	if err := c.get(ctx, "/containers/json", &list); err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		raw      = make([]*statsJSON, len(list))
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, statsConcurrency)
	)
	for i, ct := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var s statsJSON
			err := c.get(ctx, "/containers/"+url.PathEscape(ct.ID)+"/stats?stream=false&one-shot=true", &s)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				if s.ID == "" {
					s.ID = ct.ID
				}
				if s.Name == "" {
					s.Name = ct.name()
				}
				raw[i] = &s
			case IsNotFound(err): // removed meanwhile
			case firstErr == nil:
				firstErr = err
			}
		}()
	}
	wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]runtime.ContainerStats, 0, len(raw))
	seen := map[string]bool{}
	for _, s := range raw {
		if s == nil {
			continue
		}
		seen[s.ID] = true
		out = append(out, c.convert(s))
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	for id := range c.prev {
		if !seen[id] {
			delete(c.prev, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// convert turns one stats answer into a sample; the caller holds c.mu.
func (c *Client) convert(s *statsJSON) runtime.ContainerStats {
	cs := runtime.ContainerStats{
		ID: s.ID, Name: strings.TrimPrefix(s.Name, "/"),
		CPUPercent: math.NaN(),
		MemUsage:   memUsage(s.Memory.Usage, s.Memory.Stats),
		MemLimit:   s.Memory.Limit,
	}
	cur := cpuSample{total: s.CPU.Usage.Total, system: s.CPU.System, read: s.Read}
	if prev, ok := c.prev[s.ID]; ok {
		cs.CPUPercent = cpuPercent(prev, cur, cpus(s))
	}
	c.prev[s.ID] = cur
	for _, b := range s.Blkio.Bytes {
		switch strings.ToLower(b.Op) {
		case "read":
			cs.BlockRead += b.Value
		case "write":
			cs.BlockWrite += b.Value
		}
	}
	return cs
}

func cpus(s *statsJSON) float64 {
	if s.CPU.Online > 0 {
		return float64(s.CPU.Online)
	}
	if n := len(s.CPU.Usage.PerCPU); n > 0 {
		return float64(n)
	}
	return 1
}

// cpuPercent is the CPU share between two samples relative to one core, as
// docker stats computes it: the container's share of the host's CPU time
// times the number of CPUs. Without host counters (podman), the
// container's CPU time is divided by the wall-clock time instead. NaN when
// the counters were reset (the container restarted).
func cpuPercent(prev, cur cpuSample, ncpu float64) float64 {
	if cur.total < prev.total {
		return math.NaN()
	}
	d := float64(cur.total - prev.total)
	if cur.system > prev.system && prev.system > 0 {
		return d / float64(cur.system-prev.system) * ncpu * 100
	}
	if wall := cur.read.Sub(prev.read); wall > 0 {
		return d / float64(wall.Nanoseconds()) * 100
	}
	return math.NaN()
}

// memUsage leaves out the page cache the kernel can drop, as docker stats
// does: total_inactive_file (cgroup v1) or inactive_file (v2).
func memUsage(usage uint64, stats map[string]uint64) uint64 {
	if v, ok := stats["total_inactive_file"]; ok && v < usage {
		return usage - v
	}
	if v, ok := stats["inactive_file"]; ok && v < usage {
		return usage - v
	}
	return usage
}
