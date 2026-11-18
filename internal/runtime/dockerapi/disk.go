package dockerapi

import (
	"context"
	"slices"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

type dfJSON struct {
	LayersSize int64
	Images     []imageJSON
	Containers []containerJSON
	Volumes    []volumeJSON
	BuildCache []buildCacheJSON

	// Newer daemons (API 1.52+) also summarise each category; the lists
	// above may then be empty.
	ImageUsage      *usageJSON[imageJSON]
	ContainerUsage  *usageJSON[containerJSON]
	VolumeUsage     *usageJSON[volumeJSON]
	BuildCacheUsage *usageJSON[buildCacheJSON]
}

type usageJSON[T any] struct {
	TotalSize   *int64
	Reclaimable *int64
	Items       []T
}

func (u *usageJSON[T]) items(legacy []T) []T {
	if len(legacy) == 0 && u != nil {
		return u.Items
	}
	return legacy
}

func (u *usageJSON[T]) reclaimable(computed int64) uint64 {
	if u != nil && u.Reclaimable != nil {
		return nonNeg(*u.Reclaimable)
	}
	return nonNeg(computed)
}

type imageJSON struct {
	Size, SharedSize int64
	Containers       int64
}

type volumeJSON struct {
	Name      string
	UsageData *struct {
		Size, RefCount int64
	}
}

type buildCacheJSON struct {
	Size          int64
	Shared, InUse bool
}

func nonNeg(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// DiskUsage reports the daemon's disk usage ("system df"): the categories,
// every container's writable layer and mounts, and the volume sizes.
// Reclaimable amounts come from the daemon where it reports them, else
// they are computed as docker system df does.
func (c *Client) DiskUsage(ctx context.Context) (runtime.DiskUsage, error) {
	var df dfJSON
	if err := c.get(ctx, "/system/df", &df); err != nil {
		return runtime.DiskUsage{}, err
	}
	return convertDF(df), nil
}

func convertDF(df dfJSON) runtime.DiskUsage {
	var du runtime.DiskUsage

	// Images: the shared layers count once (LayersSize); the space of an
	// image in use by a container is not reclaimable.
	images := df.ImageUsage.items(df.Images)
	size := df.LayersSize
	if df.ImageUsage != nil && df.ImageUsage.TotalSize != nil {
		size = *df.ImageUsage.TotalSize
	}
	var used int64
	for _, im := range images {
		if im.Containers > 0 && im.Size >= 0 && im.SharedSize >= 0 {
			used += im.Size - im.SharedSize
		}
	}
	du.Categories = append(du.Categories, runtime.DiskCategory{Name: "Images", Size: nonNeg(size), Reclaimable: df.ImageUsage.reclaimable(size - used)})

	var csize, creclaim int64
	for _, ct := range df.ContainerUsage.items(df.Containers) {
		running := runtime.ParseState(strings.ToLower(ct.State)).IsRunning()
		cd := runtime.ContainerDisk{ID: ct.ID, Name: ct.name(), Size: nonNeg(ct.SizeRw), Running: running}
		for _, m := range ct.Mounts {
			switch {
			case strings.EqualFold(m.Type, "volume") && m.Name != "":
				cd.Mounts = append(cd.Mounts, m.Name)
			case strings.EqualFold(m.Type, "bind") && m.Source != "" && !slices.Contains(cd.Binds, m.Source):
				cd.Binds = append(cd.Binds, m.Source)
			}
		}
		du.Containers = append(du.Containers, cd)
		if ct.SizeRw > 0 {
			csize += ct.SizeRw
			if !running {
				creclaim += ct.SizeRw
			}
		}
	}
	du.Categories = append(du.Categories, runtime.DiskCategory{Name: "Containers", Size: nonNeg(csize), Reclaimable: df.ContainerUsage.reclaimable(creclaim)})

	var vsize, vreclaim int64
	du.Volumes = map[string]uint64{}
	for _, v := range df.VolumeUsage.items(df.Volumes) {
		if v.UsageData == nil || v.UsageData.Size < 0 { // -1: not measured
			continue
		}
		du.Volumes[v.Name] = uint64(v.UsageData.Size)
		vsize += v.UsageData.Size
		if v.UsageData.RefCount == 0 {
			vreclaim += v.UsageData.Size
		}
	}
	du.Categories = append(du.Categories, runtime.DiskCategory{Name: "Volumes", Size: nonNeg(vsize), Reclaimable: df.VolumeUsage.reclaimable(vreclaim)})

	var bsize, breclaim int64
	for _, b := range df.BuildCacheUsage.items(df.BuildCache) {
		if b.Shared {
			continue
		}
		bsize += b.Size
		if !b.InUse {
			breclaim += b.Size
		}
	}
	du.Categories = append(du.Categories, runtime.DiskCategory{Name: "Build cache", Size: nonNeg(bsize), Reclaimable: df.BuildCacheUsage.reclaimable(breclaim)})
	return du
}
