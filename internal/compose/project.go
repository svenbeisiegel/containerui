package compose

import (
	"sort"
	"strconv"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Project is a compose project as found on the runtime (by labels) and/or
// in the Store.
type Project struct {
	Name        string
	Services    []ProjectService
	Volumes     []runtime.Volume
	Networks    []runtime.Network
	ConfigFiles []string // compose files the project was deployed from
	WorkingDir  string
	Stored      bool // a compose file is in the Store
	Running     int  // running containers
	Total       int  // all containers
}

// ProjectService is a service with its containers (replicas, sorted by
// container number).
type ProjectService struct {
	Name       string
	Containers []runtime.Container
}

// Deployed reports whether the project has any containers.
func (p Project) Deployed() bool { return p.Total > 0 }

// Status summarizes the project's state: running, partial, stopped or
// "not deployed".
func (p Project) Status() string {
	switch {
	case p.Total == 0:
		return "not deployed"
	case p.Running == p.Total:
		return "running"
	case p.Running == 0:
		return "stopped"
	}
	return "partial"
}

// ServiceNames lists the services, sorted.
func (p Project) ServiceNames() []string {
	out := make([]string, len(p.Services))
	for i, s := range p.Services {
		out[i] = s.Name
	}
	return out
}

// Group collects the containers, volumes and networks carrying compose
// project labels into projects, adds stored projects that are not
// deployed, and sorts the result by name.
func Group(containers []runtime.Container, volumes []runtime.Volume, networks []runtime.Network, stored []string) []Project {
	byName := map[string]*Project{}
	get := func(name string) *Project {
		p := byName[name]
		if p == nil {
			p = &Project{Name: name}
			byName[name] = p
		}
		return p
	}
	for _, c := range containers {
		name := c.Labels[runtime.LabelProject]
		if name == "" {
			continue
		}
		p := get(name)
		p.Total++
		if c.State.IsRunning() {
			p.Running++
		}
		if p.WorkingDir == "" {
			p.WorkingDir = c.Labels[runtime.LabelWorkingDir]
		}
		if len(p.ConfigFiles) == 0 {
			p.ConfigFiles = splitFiles(c.Labels[runtime.LabelConfigFiles])
		}
		svc := c.Labels[runtime.LabelService]
		i := sort.Search(len(p.Services), func(i int) bool { return p.Services[i].Name >= svc })
		if i == len(p.Services) || p.Services[i].Name != svc {
			p.Services = append(p.Services, ProjectService{})
			copy(p.Services[i+1:], p.Services[i:])
			p.Services[i] = ProjectService{Name: svc}
		}
		p.Services[i].Containers = append(p.Services[i].Containers, c)
	}
	for _, v := range volumes {
		if name := v.Labels[runtime.LabelProject]; name != "" {
			p := get(name)
			p.Volumes = append(p.Volumes, v)
		}
	}
	for _, n := range networks {
		if name := n.Labels[runtime.LabelProject]; name != "" {
			p := get(name)
			p.Networks = append(p.Networks, n)
		}
	}
	for _, name := range stored {
		get(name).Stored = true
	}

	out := make([]Project, 0, len(byName))
	for _, p := range byName {
		for _, s := range p.Services {
			sort.Slice(s.Containers, func(i, j int) bool { return containerNumber(s.Containers[i]) < containerNumber(s.Containers[j]) })
		}
		sort.Slice(p.Volumes, func(i, j int) bool { return p.Volumes[i].Name < p.Volumes[j].Name })
		sort.Slice(p.Networks, func(i, j int) bool { return p.Networks[i].Name < p.Networks[j].Name })
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the named project from Group's result.
func Find(projects []Project, name string) (Project, bool) {
	for _, p := range projects {
		if p.Name == name {
			return p, true
		}
	}
	return Project{}, false
}

func containerNumber(c runtime.Container) int {
	n, _ := strconv.Atoi(c.Labels[runtime.LabelNumber])
	return n
}

// splitFiles splits the config_files label. Docker Compose joins the files
// with commas, podman-compose too.
func splitFiles(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
