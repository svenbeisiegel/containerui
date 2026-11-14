package fake

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.Stater = (*Runtime)(nil)

const fakeMemLimit = 8 << 30

// seed gives every container stable, distinct base values.
func seed(id string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return float64(h.Sum32()%1000) / 1000
}

// Stats returns slowly varying values for the running containers, so the
// dashboard charts move while developing the UI.
func (f *Runtime) Stats(context.Context) ([]runtime.ContainerStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	t := float64(time.Now().UnixNano()) / 1e9
	out := []runtime.ContainerStats{}
	for _, c := range f.containers {
		if !c.State.IsRunning() {
			continue
		}
		s := seed(c.ID)
		wave := (math.Sin(t/(40+60*s)+s*6) + 1) / 2 // 0..1
		started := c.Created
		if started.IsZero() {
			started = time.Now().Add(-time.Hour)
		}
		up := time.Since(started).Seconds()
		out = append(out, runtime.ContainerStats{
			ID:         c.ID,
			Name:       c.Name,
			CPUPercent: 2 + 60*s*wave,
			MemUsage:   uint64((20 + 300*s + 80*wave) * (1 << 20)),
			MemLimit:   fakeMemLimit,
			BlockRead:  uint64(up * (2000 + 50000*s) * (0.5 + wave)),
			BlockWrite: uint64(up * (8000 + 120000*s*wave)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DiskUsage derives sizes from the fake's containers, images and volumes.
func (f *Runtime) DiskUsage(context.Context) (runtime.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return runtime.DiskUsage{}, f.Err
	}
	du := runtime.DiskUsage{Volumes: map[string]uint64{}}
	var images, containers, volumes uint64
	for range f.images {
		images += 120 << 20
	}
	for _, c := range f.containers {
		size := uint64(seed(c.ID)*400) << 20
		containers += size
		du.Containers = append(du.Containers, runtime.ContainerDisk{
			ID: c.ID, Name: c.Name, Size: size, Mounts: c.Mounts, Running: c.State.IsRunning(),
		})
	}
	sort.Slice(du.Containers, func(i, j int) bool { return du.Containers[i].Name < du.Containers[j].Name })
	for _, v := range f.volumes {
		size := uint64(seed(v.Name)*900) << 20
		volumes += size
		du.Volumes[v.Name] = size
	}
	du.Categories = []runtime.DiskCategory{
		{Name: "Images", Size: images, Reclaimable: images / 3},
		{Name: "Containers", Size: containers},
		{Name: "Volumes", Size: volumes, Reclaimable: volumes / 4},
		{Name: "Build cache", Size: 64 << 20, Reclaimable: 64 << 20},
	}
	return du, nil
}
