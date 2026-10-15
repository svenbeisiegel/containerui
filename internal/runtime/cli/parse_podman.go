package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// podman's "--format json" prints JSON arrays with its own field names
// (and numbers where docker prints strings), so it needs separate parsers.
// Volumes use the same names as docker and share parseVolumes.

type podmanPort struct {
	HostIP        string `json:"host_ip"`
	ContainerPort uint16 `json:"container_port"`
	HostPort      uint16 `json:"host_port"`
	Range         uint16 `json:"range"`
	Protocol      string `json:"protocol"`
}

func (p podmanPort) String() string {
	cport, hport := fmt.Sprint(p.ContainerPort), fmt.Sprint(p.HostPort)
	if p.Range > 1 {
		cport += fmt.Sprintf("-%d", p.ContainerPort+p.Range-1)
		hport += fmt.Sprintf("-%d", p.HostPort+p.Range-1)
	}
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	if p.HostPort == 0 {
		return cport + "/" + proto
	}
	if p.HostIP != "" {
		hport = p.HostIP + ":" + hport
	}
	return hport + "->" + cport + "/" + proto
}

type podmanContainerJSON struct {
	ID        string       `json:"Id"`
	Names     []string     `json:"Names"`
	Image     string       `json:"Image"`
	Command   []string     `json:"Command"`
	State     string       `json:"State"`
	Status    string       `json:"Status"`
	Ports     []podmanPort `json:"Ports"`
	Networks  []string     `json:"Networks"`
	Mounts    []string     `json:"Mounts"`
	Created   int64        `json:"Created"`
	CreatedAt string       `json:"CreatedAt"` // human-readable, e.g. "5 hours ago"
	Labels    labelsJSON   `json:"Labels"`
}

func parsePodmanContainers(data []byte) ([]runtime.Container, error) {
	rows, err := decodeNDJSON[podmanContainerJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Container, 0, len(rows))
	for _, r := range rows {
		ports := make([]string, len(r.Ports))
		for i, p := range r.Ports {
			ports[i] = p.String()
		}
		name := ""
		if len(r.Names) > 0 {
			name = r.Names[0]
		}
		out = append(out, runtime.Container{
			ID:         r.ID,
			Name:       name,
			Image:      r.Image,
			Command:    strings.Join(r.Command, " "),
			State:      runtime.ParseState(strings.ToLower(r.State)),
			Status:     r.Status,
			Ports:      strings.Join(ports, ", "),
			Networks:   r.Networks,
			Mounts:     r.Mounts,
			Created:    unixTime(r.Created),
			RunningFor: r.CreatedAt,
			Labels:     r.Labels,
		})
	}
	return out, nil
}

type podmanImageJSON struct {
	ID         string   `json:"Id"`
	RepoTags   []string `json:"RepoTags"`
	Names      []string `json:"Names"` // the tags; podman 5 leaves RepoTags null
	Digest     string   `json:"Digest"`
	Size       int64    `json:"Size"`
	Created    int64    `json:"Created"`
	Containers int      `json:"Containers"`
}

// parsePodmanImages returns one row per tag, like docker (podman lists an
// image once with all its tags).
func parsePodmanImages(data []byte) ([]runtime.Image, error) {
	rows, err := decodeNDJSON[podmanImageJSON](data)
	if err != nil {
		return nil, err
	}
	var out []runtime.Image
	for _, r := range rows {
		img := runtime.Image{
			ID:         r.ID,
			Repository: "<none>",
			Tag:        "<none>",
			Digest:     r.Digest,
			Size:       humanSize(r.Size),
			Created:    unixTime(r.Created),
			Containers: r.Containers,
		}
		tags := r.RepoTags
		if len(tags) == 0 {
			tags = r.Names
		}
		if len(tags) == 0 {
			out = append(out, img)
			continue
		}
		for _, ref := range tags {
			img.Repository, img.Tag = runtime.SplitRef(ref)
			out = append(out, img)
		}
	}
	if out == nil {
		out = []runtime.Image{}
	}
	return out, nil
}

type podmanNetworkJSON struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Driver   string     `json:"driver"`
	Created  time.Time  `json:"created"`
	Internal bool       `json:"internal"`
	IPv6     bool       `json:"ipv6_enabled"`
	Labels   labelsJSON `json:"labels"`
}

func parsePodmanNetworks(data []byte) ([]runtime.Network, error) {
	rows, err := decodeNDJSON[podmanNetworkJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Network, 0, len(rows))
	for _, r := range rows {
		out = append(out, runtime.Network{
			ID:       r.ID,
			Name:     r.Name,
			Driver:   r.Driver,
			Scope:    "local",
			Internal: r.Internal,
			IPv6:     r.IPv6,
			Created:  r.Created,
			Labels:   r.Labels,
		})
	}
	return out, nil
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// humanSize formats bytes like the docker CLI: decimal units, three
// significant digits ("7.8MB", "123kB").
func humanSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.3g%s", f, units[i])
}
