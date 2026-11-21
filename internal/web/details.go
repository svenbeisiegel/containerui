package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// containerInfo is what the container detail page shows of the
// container's inspect document. Docker and Podman use the same field
// names for these; anything missing stays empty.
type containerInfo struct {
	Path   string   `json:"Path"`
	Args   []string `json:"Args"`
	Config struct {
		Hostname   string            `json:"Hostname"`
		User       string            `json:"User"`
		WorkingDir string            `json:"WorkingDir"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
		ExitCode   int       `json:"ExitCode"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	HostConfig   struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		Privileged bool `json:"Privileged"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress         string   `json:"IPAddress"`
			GlobalIPv6Address string   `json:"GlobalIPv6Address"`
			MacAddress        string   `json:"MacAddress"`
			Aliases           []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// parseContainerInfo reads the inspect document; ok is false if it is
// not a JSON object.
func parseContainerInfo(raw []byte) (containerInfo, bool) {
	var i containerInfo
	return i, json.Unmarshal(raw, &i) == nil
}

// Restart is the restart policy as given to --restart, "" for none.
func (i containerInfo) Restart() string {
	p := i.HostConfig.RestartPolicy
	switch p.Name {
	case "", "no":
		return ""
	case "on-failure":
		if p.MaximumRetryCount > 0 {
			return "on-failure:" + strconv.Itoa(p.MaximumRetryCount)
		}
	}
	return p.Name
}

// Health is the health check's status, "" without a check.
func (i containerInfo) Health() string {
	if i.State.Health == nil {
		return ""
	}
	return i.State.Health.Status
}

// Env splits the environment into name and value.
func (i containerInfo) Env() map[string]string { return envMap(i.Config.Env) }

// containerNet is one network the container is attached to.
type containerNet struct {
	Name, IPv4, IPv6, MAC string
	Aliases               []string
}

// Nets lists the attached networks by name.
func (i containerInfo) Nets() []containerNet {
	var out []containerNet
	for name, n := range i.NetworkSettings.Networks {
		out = append(out, containerNet{Name: name, IPv4: n.IPAddress, IPv6: n.GlobalIPv6Address, MAC: n.MacAddress, Aliases: n.Aliases})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// volumeInfo is what the volume detail page adds from the inspect
// document.
type volumeInfo struct {
	CreatedAt time.Time `json:"CreatedAt"`
}

func parseVolumeInfo(raw []byte) volumeInfo {
	var i volumeInfo
	_ = json.Unmarshal(raw, &i)
	return i
}

func envMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// containersNamed lists the containers with the given names, in list
// order; nil if the list fails.
func containersNamed(r *http.Request, rt runtime.Runtime, names []string) []runtime.Container {
	cs, err := rt.ListContainers(r.Context(), runtime.ListContainersOpts{All: true})
	if err != nil {
		return nil
	}
	var out []runtime.Container
	for _, c := range cs {
		if slices.Contains(names, c.Name) {
			out = append(out, c)
		}
	}
	return out
}
