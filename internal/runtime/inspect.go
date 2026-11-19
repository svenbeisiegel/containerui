package runtime

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ContainerInspect is the part of docker/podman/wslc "container inspect"
// output that ContainerUI reads back (compose file generation, recreating
// a container).
type ContainerInspect struct {
	ID    string `json:"Id"`
	Name  string
	Image string // image ID
	State struct {
		Status  string
		Running bool
	}
	Config struct {
		Image       string
		Cmd         StrSlice
		Entrypoint  StrSlice
		Env         []string
		Labels      map[string]string
		User        string
		WorkingDir  string
		Hostname    string
		Domainname  string
		Tty         bool
		OpenStdin   bool
		StopSignal  string
		MacAddress  string
		Healthcheck *Healthcheck
	}
	HostConfig struct {
		PortBindings  map[string][]PortBinding
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		NetworkMode string
		AutoRemove  bool
		Privileged  bool
		CapAdd      []string
		CapDrop     []string
		Dns         []string
		DnsSearch   []string
		DnsOptions  []string
		ExtraHosts  []string
		Memory      int64
		NanoCpus    int64
		Ulimits     []struct {
			Name string
			Soft int64
			Hard int64
		}

		// Settings ContainerUI cannot reproduce; only reported.
		Devices []struct {
			PathOnHost      string
			PathInContainer string
		}
		SecurityOpt []string
		Sysctls     map[string]string
		ShmSize     int64
		PidMode     string
		IpcMode     string
		Tmpfs       map[string]string
		GroupAdd    []string
		PidsLimit   *int64
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
		RW          *bool
		ReadWrite   *bool // wslc
		Propagation string
	}
	NetworkSettings struct {
		Networks map[string]EndpointInspect
	}
}

// EndpointInspect is a container's endpoint in one network.
type EndpointInspect struct {
	Aliases    []string
	MacAddress string
	IPAMConfig *struct {
		IPv4Address string
	}
}

// ReadOnly reports whether the i-th mount is read-only.
func (c *ContainerInspect) ReadOnly(i int) bool {
	m := c.Mounts[i]
	return (m.RW != nil && !*m.RW) || (m.ReadWrite != nil && !*m.ReadWrite)
}

// ImageInspect is the part of "image inspect" output ContainerUI reads.
type ImageInspect struct {
	ID          string `json:"Id"`
	RepoTags    []string
	RepoDigests []string
	Config      struct {
		Cmd         StrSlice
		Entrypoint  StrSlice
		Env         []string
		Labels      map[string]string
		User        string
		WorkingDir  string
		StopSignal  string
		Healthcheck *Healthcheck
	}
}

type Healthcheck struct {
	Test        []string
	Interval    int64 // nanoseconds
	Timeout     int64
	StartPeriod int64
	Retries     int
}

// Equal reports whether both health checks are the same; nil equals nothing.
func (h *Healthcheck) Equal(o *Healthcheck) bool {
	if h == nil || o == nil {
		return false
	}
	return slices.Equal(h.Test, o.Test) && h.Interval == o.Interval && h.Timeout == o.Timeout &&
		h.StartPeriod == o.StartPeriod && h.Retries == o.Retries
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

// StrSlice accepts a JSON string or array of strings.
type StrSlice []string

func (s *StrSlice) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StrSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// AnonymousVolumeRe matches the generated names of anonymous volumes.
var AnonymousVolumeRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PortSpecs formats PortBindings as "[ip:]host:container[/udp]", skipping
// the IPv6 duplicate of each IPv4 binding. IPv6 host IPs are bracketed.
func PortSpecs(b map[string][]PortBinding) []string {
	var out []string
	seen := map[string]bool{}
	for _, cport := range SortedKeys(b) {
		port, proto, _ := strings.Cut(cport, "/")
		if proto == "udp" || proto == "sctp" {
			port += "/" + proto
		}
		for _, h := range b[cport] {
			var spec string
			switch {
			case h.HostPort == "":
				spec = port
			case h.HostIP == "" || h.HostIP == "0.0.0.0" || h.HostIP == "::":
				spec = h.HostPort + ":" + port
			case strings.Contains(h.HostIP, ":"):
				spec = "[" + h.HostIP + "]:" + h.HostPort + ":" + port
			default:
				spec = h.HostIP + ":" + h.HostPort + ":" + port
			}
			if !seen[spec] {
				seen[spec] = true
				out = append(out, spec)
			}
		}
	}
	return out
}

// Without returns the entries of a that are not in b.
func Without(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// OwnLabels drops compose's, the runtimes' and ContainerUI's own labels
// and labels inherited unchanged from the image.
func OwnLabels(labels, image map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, LabelPrefix) || strings.HasPrefix(k, "io.podman.compose.") || strings.HasPrefix(k, "com.microsoft.wsl.") || strings.HasPrefix(k, LabelOwnPrefix) {
			continue
		}
		if iv, ok := image[k]; ok && iv == v {
			continue
		}
		out[k] = v
	}
	return out
}

// SortedKeys returns the keys of m in order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
