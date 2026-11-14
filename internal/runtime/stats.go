package runtime

import "context"

// ContainerStats is one sample of a running container's resource usage.
type ContainerStats struct {
	ID   string // full ID where the runtime reports one, else the short ID
	Name string
	// CPUPercent is relative to one core, so it exceeds 100 on multi-core
	// hosts (as docker stats reports it).
	CPUPercent float64
	MemUsage   uint64 // bytes
	MemLimit   uint64 // bytes; the host memory without a limit
	// BlockRead and BlockWrite are cumulative since the container started.
	BlockRead  uint64
	BlockWrite uint64
}

// DiskCategory is one row of the runtime's disk usage summary.
type DiskCategory struct {
	Name        string // Images, Containers, Volumes, Build cache
	Size        uint64
	Reclaimable uint64
}

// ContainerDisk is a container's own disk usage: its writable layer and the
// volumes it mounts.
type ContainerDisk struct {
	ID      string
	Name    string
	Size    uint64   // writable layer
	Mounts  []string // volume names as the runtime lists them (may be truncated)
	Running bool
}

// DiskUsage is the runtime's disk usage, as "system df" reports it.
type DiskUsage struct {
	Categories []DiskCategory
	Containers []ContainerDisk
	// Volumes maps volume names to their size; nil when the runtime does
	// not report volume sizes.
	Volumes map[string]uint64
}

// Stater is implemented by drivers that report resource usage.
// Capabilities.Stats reports whether it works for this runtime.
type Stater interface {
	// Stats samples all running containers once.
	Stats(ctx context.Context) ([]ContainerStats, error)
	DiskUsage(ctx context.Context) (DiskUsage, error)
}
