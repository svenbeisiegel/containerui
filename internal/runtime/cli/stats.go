package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.Stater = (*Driver)(nil)

// Stats samples the running containers once ("container stats
// --no-stream"). docker waits for a second sample to compute the CPU
// share, so this takes about two seconds.
func (d *Driver) Stats(ctx context.Context) ([]runtime.ContainerStats, error) {
	if !d.flavor.stats {
		return nil, runtime.ErrUnsupported
	}
	out, err := d.run(ctx, "container stats", "container", "stats", "--no-stream", "--format", "json")
	if err != nil {
		return nil, err
	}
	if d.flavor.podmanJSON {
		return parsePodmanStats(out)
	}
	return parseStats(out)
}

// DiskUsage reports the runtime's disk usage. docker's verbose "system df"
// also lists every container's writable layer, its mounts and the volume
// sizes; podman cannot combine --verbose with --format, so its containers
// come from "container ls --size" and volume sizes are not known.
func (d *Driver) DiskUsage(ctx context.Context) (runtime.DiskUsage, error) {
	if !d.flavor.stats {
		return runtime.DiskUsage{}, runtime.ErrUnsupported
	}
	out, err := d.run(ctx, "system df", "system", "df", "--format", "json")
	if err != nil {
		return runtime.DiskUsage{}, err
	}
	var du runtime.DiskUsage
	if d.flavor.podmanJSON {
		if du.Categories, err = parseDF(out); err != nil {
			return du, err
		}
		out, err = d.run(ctx, "container list", "container", d.flavor.verb("list"), "--all", "--size", "--format", "json")
		if err != nil {
			return du, err
		}
		if du.Containers, err = parsePodmanContainerSizes(out); err != nil {
			return du, err
		}
		d.addBinds(ctx, du.Containers)
		return du, nil
	}
	if du.Categories, err = parseDF(out); err != nil {
		return du, err
	}
	out, err = d.run(ctx, "system df", "system", "df", "--verbose", "--format", "json")
	if err != nil {
		return du, err
	}
	if du.Containers, du.Volumes, err = parseDFVerbose(out); err != nil {
		return du, err
	}
	d.addBinds(ctx, du.Containers)
	return du, nil
}

// addBinds fills the bind-mounted host paths of cs from one "container
// inspect" of all of them. Without them the disk usage is still useful, so
// a failure is only logged.
func (d *Driver) addBinds(ctx context.Context, cs []runtime.ContainerDisk) {
	if len(cs) == 0 {
		return
	}
	args := []string{"container", "inspect"}
	for _, c := range cs {
		if runtime.ValidateContainerID(c.ID) == nil {
			args = append(args, c.ID)
		}
	}
	// A container removed meanwhile makes inspect exit non-zero but still
	// print the others.
	out, err := d.run(ctx, "container inspect", args...)
	binds, perr := parseBinds(out)
	if perr != nil {
		slog.Debug("bind mounts unknown", "error", errors.Join(err, perr))
		return
	}
	for i := range cs {
		for id, paths := range binds {
			if cs[i].ID != "" && strings.HasPrefix(id, cs[i].ID) {
				cs[i].Binds = paths
				break
			}
		}
	}
}

// parseBinds maps full container IDs to their bind mount sources.
func parseBinds(data []byte) (map[string][]string, error) {
	var list []runtime.ContainerInspect
	data = bytes.TrimSpace(normalize(data))
	if len(data) == 0 {
		return nil, errors.New("no inspect output")
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode CLI output: %w", err)
	}
	out := make(map[string][]string, len(list))
	for _, c := range list {
		var paths []string
		for _, m := range c.Mounts {
			if strings.EqualFold(m.Type, "bind") && m.Source != "" && !slices.Contains(paths, m.Source) {
				paths = append(paths, m.Source)
			}
		}
		out[c.ID] = paths
	}
	return out, nil
}

// ---- docker ----

type statsJSON struct {
	Container string `json:"Container"` // full ID when stats lists all containers
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	CPUPerc   string `json:"CPUPerc"`
	MemUsage  string `json:"MemUsage"`
	BlockIO   string `json:"BlockIO"`
}

func parseStats(data []byte) ([]runtime.ContainerStats, error) {
	rows, err := decodeNDJSON[statsJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ContainerStats, 0, len(rows))
	for _, r := range rows {
		id := r.ID
		if len(r.Container) > len(id) && strings.HasPrefix(r.Container, id) {
			id = r.Container
		}
		out = append(out, makeStats(id, strings.TrimPrefix(r.Name, "/"), r.CPUPerc, r.MemUsage, r.BlockIO))
	}
	return out, nil
}

func makeStats(id, name, cpu, mem, block string) runtime.ContainerStats {
	s := runtime.ContainerStats{ID: id, Name: name, CPUPercent: parsePercent(cpu)}
	s.MemUsage, s.MemLimit = parsePair(mem)
	s.BlockRead, s.BlockWrite = parsePair(block)
	return s
}

type dfJSON struct {
	Type        string `json:"Type"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
	// podman prints raw numbers instead.
	RawSize        int64 `json:"RawSize"`
	RawReclaimable int64 `json:"RawReclaimable"`
}

func (r dfJSON) category() runtime.DiskCategory {
	c := runtime.DiskCategory{Name: categoryName(r.Type), Size: parseSize(r.Size), Reclaimable: parseSize(r.Reclaimable)}
	if r.RawSize > 0 {
		c.Size = uint64(r.RawSize)
	}
	if r.RawReclaimable > 0 {
		c.Reclaimable = uint64(r.RawReclaimable)
	}
	return c
}

func categoryName(t string) string {
	switch strings.ToLower(t) {
	case "local volumes", "volumes":
		return "Volumes"
	case "build cache":
		return "Build cache"
	}
	return t
}

func parseDF(data []byte) ([]runtime.DiskCategory, error) {
	rows, err := decodeNDJSON[dfJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.DiskCategory, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.category())
	}
	return out, nil
}

type dfVerboseJSON struct {
	Containers []struct {
		ID     string `json:"ID"`
		Names  string `json:"Names"`
		Size   string `json:"Size"`
		Mounts string `json:"Mounts"`
		State  string `json:"State"`
	} `json:"Containers"`
	Volumes []struct {
		Name string `json:"Name"`
		Size string `json:"Size"`
	} `json:"Volumes"`
}

func parseDFVerbose(data []byte) ([]runtime.ContainerDisk, map[string]uint64, error) {
	var v dfVerboseJSON
	if err := json.Unmarshal(normalize(data), &v); err != nil {
		return nil, nil, fmt.Errorf("decode CLI output: %w", err)
	}
	cs := make([]runtime.ContainerDisk, 0, len(v.Containers))
	for _, c := range v.Containers {
		cs = append(cs, runtime.ContainerDisk{
			ID: c.ID, Name: strings.TrimPrefix(c.Names, "/"), Size: parseSize(c.Size),
			Mounts: splitList(c.Mounts), Running: runtime.ParseState(strings.ToLower(c.State)).IsRunning(),
		})
	}
	vols := make(map[string]uint64, len(v.Volumes))
	for _, vol := range v.Volumes {
		vols[vol.Name] = parseSize(vol.Size)
	}
	return cs, vols, nil
}

// ---- podman ----

type podmanStatsJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CPU      string `json:"cpu_percent"`
	MemUsage string `json:"mem_usage"`
	BlockIO  string `json:"block_io"`
}

func parsePodmanStats(data []byte) ([]runtime.ContainerStats, error) {
	rows, err := decodeNDJSON[podmanStatsJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ContainerStats, 0, len(rows))
	for _, r := range rows {
		out = append(out, makeStats(r.ID, r.Name, r.CPU, r.MemUsage, r.BlockIO))
	}
	return out, nil
}

type podmanSizeJSON struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
	Size  *struct {
		RwSize int64 `json:"rwSize"`
	} `json:"Size"`
}

func parsePodmanContainerSizes(data []byte) ([]runtime.ContainerDisk, error) {
	rows, err := decodeNDJSON[podmanSizeJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ContainerDisk, 0, len(rows))
	for _, r := range rows {
		c := runtime.ContainerDisk{ID: r.ID, Running: runtime.ParseState(strings.ToLower(r.State)).IsRunning()}
		if len(r.Names) > 0 {
			c.Name = r.Names[0]
		}
		if r.Size != nil && r.Size.RwSize > 0 {
			c.Size = uint64(r.Size.RwSize)
		}
		out = append(out, c)
	}
	return out, nil
}

// ---- human-readable values ----

var sizeUnits = map[string]float64{
	"b":  1,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "pb": 1e15,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40, "pib": 1 << 50,
	// docker's go-units also accepts these short binary spellings
	"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40,
}

// parseSize parses sizes as the CLIs print them: "4.1kB", "1.914GiB",
// "4.348GB (89%)" (a reclaimable share), "0B". Unknown values ("N/A",
// "--") are 0.
func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	if unit == "" {
		unit = "b"
	}
	m, ok := sizeUnits[unit]
	if !ok {
		return 0
	}
	return uint64(math.Round(n * m))
}

// parsePair parses "used / limit" and "read / write" pairs.
func parsePair(s string) (uint64, uint64) {
	a, b, _ := strings.Cut(s, "/")
	return parseSize(a), parseSize(b)
}

// parsePercent parses "12.50%"; "--" and "" are 0.
func parsePercent(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	if err != nil || math.IsNaN(f) || f < 0 {
		return 0
	}
	return f
}
