package dockerapi

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// The list answers of the Engine API (docker and podman's compat API),
// converted to what the CLI driver returns for the same objects: the same
// human-readable strings ("Up 4 hours", "0.0.0.0:8080->80/tcp", "8.42MB")
// the docker CLI prints.

type listContainerJSON struct {
	ID              string `json:"Id"`
	Names           []string
	Image           string
	ImageID         string
	Command         string
	Created         int64
	Ports           []portJSON
	Labels          map[string]string
	State           string
	Status          string
	NetworkSettings struct {
		Networks map[string]struct{}
	}
	Mounts []mountJSON
}

type portJSON struct {
	IP          string
	PrivatePort uint16
	PublicPort  uint16
	Type        string
}

func convertContainers(list []listContainerJSON, now time.Time) []runtime.Container {
	out := make([]runtime.Container, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		var networks []string
		for n := range c.NetworkSettings.Networks {
			networks = append(networks, n)
		}
		sort.Strings(networks)
		var mounts []string
		for _, m := range c.Mounts {
			if m.Name != "" {
				mounts = append(mounts, m.Name)
			} else if m.Source != "" {
				mounts = append(mounts, m.Source)
			}
		}
		sort.Strings(mounts) // the daemon lists them in no stable order
		created := unixTime(c.Created)
		runningFor := ""
		if !created.IsZero() {
			runningFor = humanDuration(now.Sub(created)) + " ago"
		}
		labels := c.Labels
		if len(labels) == 0 {
			labels = nil
		}
		out = append(out, runtime.Container{
			ID:         c.ID,
			Name:       name,
			Image:      c.Image,
			Command:    c.Command,
			State:      runtime.ParseState(strings.ToLower(c.State)),
			Status:     c.Status,
			Ports:      displayPorts(c.Ports),
			Networks:   networks,
			Mounts:     mounts,
			Created:    created,
			RunningFor: runningFor,
			Labels:     labels,
		})
	}
	return out
}

// portGroup is a run of consecutive container ports with the same address
// and protocol, published on the same port numbers (or not at all).
type portGroup struct {
	ip, proto   string
	first, last uint16
}

func (g *portGroup) String() string {
	s := strconv.Itoa(int(g.first))
	if g.last != g.first {
		s += "-" + strconv.Itoa(int(g.last))
	}
	if g.ip != "" {
		s = net.JoinHostPort(g.ip, s) + "->" + s
	}
	return s + "/" + g.proto
}

// displayPorts formats ports like the docker CLI's PORTS column: sorted,
// runs of consecutive ports with the same address and protocol grouped
// into ranges, mappings to another host port listed last.
func displayPorts(ports []portJSON) string {
	ps := append([]portJSON(nil), ports...)
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.PrivatePort != b.PrivatePort {
			return a.PrivatePort < b.PrivatePort
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		if a.PublicPort != b.PublicPort {
			return a.PublicPort < b.PublicPort
		}
		return a.Type < b.Type
	})
	var (
		result   []string
		mappings []string
		groups   = map[string]*portGroup{}
		keys     []string
	)
	for _, p := range ps {
		proto := p.Type
		if proto == "" {
			proto = "tcp"
		}
		key := proto
		if p.IP != "" {
			if p.PublicPort != p.PrivatePort {
				mappings = append(mappings, fmt.Sprintf("%s->%d/%s", net.JoinHostPort(p.IP, strconv.Itoa(int(p.PublicPort))), p.PrivatePort, proto))
				continue
			}
			key = p.IP + "/" + proto
		}
		g := groups[key]
		if g == nil {
			groups[key] = &portGroup{ip: p.IP, proto: proto, first: p.PrivatePort, last: p.PrivatePort}
			keys = append(keys, key)
			continue
		}
		if p.PrivatePort == g.last+1 {
			g.last = p.PrivatePort
			continue
		}
		result = append(result, g.String())
		g.first, g.last = p.PrivatePort, p.PrivatePort
	}
	for _, k := range keys {
		result = append(result, groups[k].String())
	}
	return strings.Join(append(result, mappings...), ", ")
}

// humanDuration is the docker CLI's (go-units) rendering of a duration,
// as in "26 hours ago".
func humanDuration(d time.Duration) string {
	if seconds := int(d.Seconds()); seconds < 1 {
		return "Less than a second"
	} else if seconds == 1 {
		return "1 second"
	} else if seconds < 60 {
		return fmt.Sprintf("%d seconds", seconds)
	} else if minutes := int(d.Minutes()); minutes == 1 {
		return "About a minute"
	} else if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	} else if hours := int(d.Hours() + 0.5); hours == 1 {
		return "About an hour"
	} else if hours < 48 {
		return fmt.Sprintf("%d hours", hours)
	} else if hours < 24*7*2 {
		return fmt.Sprintf("%d days", hours/24)
	} else if hours < 24*30*2 {
		return fmt.Sprintf("%d weeks", hours/24/7)
	} else if hours < 24*365*2 {
		return fmt.Sprintf("%d months", hours/24/30)
	}
	return fmt.Sprintf("%d years", int(d.Hours())/24/365)
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

type listImageJSON struct {
	ID          string `json:"Id"`
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
	Containers  int // -1: not counted by the daemon
}

// convertImages returns one row per tag like the docker CLI; an image
// without tags gets one row per repository it has a digest in, or a
// "<none>" row, unless untagged is false (the docker CLI hides untagged
// images since 29; podman lists them). counts holds the containers per
// image ID where the daemon did not count them.
func convertImages(list []listImageJSON, counts map[string]int, untagged bool) []runtime.Image {
	out := []runtime.Image{}
	for _, r := range list {
		img := runtime.Image{
			ID:         r.ID,
			Repository: "<none>",
			Tag:        "<none>",
			Size:       runtime.HumanSize(r.Size),
			Created:    unixTime(r.Created),
			Containers: r.Containers,
		}
		if img.Containers < 0 {
			img.Containers = counts[r.ID]
		}
		var tags []string
		for _, t := range r.RepoTags {
			if t != "<none>:<none>" {
				tags = append(tags, t)
			}
		}
		if len(tags) > 0 {
			for _, ref := range tags {
				img.Repository, img.Tag = runtime.SplitRef(ref)
				out = append(out, img)
			}
			continue
		}
		if !untagged {
			continue
		}
		seen := map[string]bool{}
		for _, ref := range r.RepoDigests {
			repo, _, _ := strings.Cut(ref, "@")
			if repo == "" || repo == "<none>" || seen[repo] {
				continue
			}
			seen[repo] = true
			img.Repository = repo
			out = append(out, img)
		}
		if len(seen) == 0 {
			out = append(out, img)
		}
	}
	return out
}

type listVolumeJSON struct {
	Name       string
	Driver     string
	Mountpoint string
	Scope      string
	Labels     map[string]string
	Options    map[string]string
}

func convertVolumes(list []listVolumeJSON) []runtime.Volume {
	out := make([]runtime.Volume, 0, len(list))
	for _, v := range list {
		labels := v.Labels
		if len(labels) == 0 {
			labels = nil
		}
		opts := v.Options
		if opts == nil {
			// The list has the options (unlike docker's CLI list): none.
			opts = map[string]string{}
		}
		out = append(out, runtime.Volume{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Scope: v.Scope, Labels: labels, Options: opts})
	}
	return out
}

type listNetworkJSON struct {
	Name       string
	ID         string `json:"Id"`
	Created    time.Time
	Scope      string
	Driver     string
	EnableIPv6 bool
	Internal   bool
	Labels     map[string]string
}

func convertNetworks(list []listNetworkJSON) []runtime.Network {
	out := make([]runtime.Network, 0, len(list))
	for _, n := range list {
		labels := n.Labels
		if len(labels) == 0 {
			labels = nil
		}
		scope := n.Scope
		if scope == "" {
			scope = "local"
		}
		out = append(out, runtime.Network{
			ID:       n.ID,
			Name:     n.Name,
			Driver:   n.Driver,
			Scope:    scope,
			Internal: n.Internal,
			IPv6:     n.EnableIPv6,
			Created:  n.Created,
			Labels:   labels,
		})
	}
	return out
}
