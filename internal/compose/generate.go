package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// The parts of docker/podman inspect output Generate uses.
type containerInspect struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Image       string
		Cmd         strSlice
		Entrypoint  strSlice
		Env         []string
		Labels      map[string]string
		User        string
		WorkingDir  string
		Hostname    string
		Tty         bool
		OpenStdin   bool
		StopSignal  string
		Healthcheck *healthcheck
	}
	HostConfig struct {
		PortBindings  map[string][]portBinding
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		NetworkMode string
		Privileged  bool
		CapAdd      []string
		CapDrop     []string
		Dns         []string
		ExtraHosts  []string
		Memory      int64
		NanoCpus    int64
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
		RW          *bool
		ReadWrite   *bool // wslc
	}
	NetworkSettings struct {
		Networks map[string]json.RawMessage
	}
}

type imageInspect struct {
	Config struct {
		Cmd         strSlice
		Entrypoint  strSlice
		Env         []string
		Labels      map[string]string
		User        string
		WorkingDir  string
		Healthcheck *healthcheck
	}
}

type healthcheck struct {
	Test        []string
	Interval    int64 // nanoseconds
	Timeout     int64
	StartPeriod int64
	Retries     int
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

// strSlice accepts a JSON string or array of strings.
type strSlice []string

func (s *strSlice) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = strSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

var anonymousVolumeRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Generate rebuilds a compose file for a project from the inspect output of
// its containers. images maps an image reference (Config.Image) to its
// inspect output; values the container only inherits from its image are
// left out. The result is a starting point to review: what the runtime
// does not record (build contexts, env_file, profiles, variables) is lost.
func Generate(project string, containers []json.RawMessage, images map[string]json.RawMessage) ([]byte, error) {
	type replica struct {
		c   containerInspect
		num int
	}
	services := map[string][]replica{}
	for _, raw := range containers {
		var c containerInspect
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("decode container inspect: %w", err)
		}
		l := c.Config.Labels
		if l[runtime.LabelProject] != project || strings.EqualFold(l[runtime.LabelOneOff], "true") {
			continue
		}
		svc := l[runtime.LabelService]
		if svc == "" {
			continue
		}
		num, _ := strconv.Atoi(l[runtime.LabelNumber])
		services[svc] = append(services[svc], replica{c, num})
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("project %q has no containers to rebuild the compose file from", project)
	}

	g := &generator{project: project, volumes: map[string]bool{}, extVolumes: map[string]bool{}, networks: map[string]bool{}, extNetworks: map[string]bool{}}
	svcNodes := mapping()
	for _, name := range sortedKeys(services) {
		reps := services[name]
		sort.Slice(reps, func(i, j int) bool { return reps[i].num < reps[j].num })
		var img imageInspect
		if raw, ok := images[reps[0].c.Config.Image]; ok {
			_ = json.Unmarshal(raw, &img)
		}
		addPair(svcNodes, name, g.service(name, reps[0].c, reps[0].num, len(reps), img))
	}

	root := mapping()
	root.HeadComment = "Rebuilt by ContainerUI from the running containers of project " + project + ".\n" +
		"Review before applying: build contexts, env_file, profiles and variables\n" +
		"are not recorded by the runtime and were not restored."
	addPair(root, "name", scalar(project))
	addPair(root, "services", svcNodes)
	if len(g.networks)+len(g.extNetworks) > 0 {
		nets := mapping()
		for _, n := range sortedKeys(union(g.networks, g.extNetworks)) {
			if g.extNetworks[n] {
				addPair(nets, n, external())
			} else {
				addPair(nets, n, mapping())
			}
		}
		addPair(root, "networks", nets)
	}
	if len(g.volumes)+len(g.extVolumes) > 0 {
		vols := mapping()
		for _, v := range sortedKeys(union(g.volumes, g.extVolumes)) {
			if g.extVolumes[v] {
				addPair(vols, v, external())
			} else {
				addPair(vols, v, mapping())
			}
		}
		addPair(root, "volumes", vols)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type generator struct {
	project               string
	volumes, extVolumes   map[string]bool
	networks, extNetworks map[string]bool
}

func (g *generator) service(name string, c containerInspect, num, replicas int, img imageInspect) *yaml.Node {
	s := mapping()
	addPair(s, "image", scalar(c.Config.Image))

	cname := strings.TrimPrefix(c.Name, "/")
	if cname != "" && replicas == 1 && cname != fmt.Sprintf("%s-%s-%d", g.project, name, num) && cname != fmt.Sprintf("%s_%s_%d", g.project, name, num) {
		addPair(s, "container_name", scalar(cname))
	}
	if len(c.Config.Entrypoint) > 0 && !slices.Equal(c.Config.Entrypoint, img.Config.Entrypoint) {
		addPair(s, "entrypoint", flowList(c.Config.Entrypoint))
	}
	if len(c.Config.Cmd) > 0 && !slices.Equal(c.Config.Cmd, img.Config.Cmd) {
		addPair(s, "command", command(c.Config.Cmd))
	}
	if c.Config.User != "" && c.Config.User != img.Config.User {
		addPair(s, "user", quoted(c.Config.User))
	}
	if c.Config.WorkingDir != "" && c.Config.WorkingDir != img.Config.WorkingDir {
		addPair(s, "working_dir", scalar(c.Config.WorkingDir))
	}
	if h := c.Config.Hostname; h != "" && !strings.HasPrefix(c.ID, h) {
		addPair(s, "hostname", scalar(h))
	}

	if env := without(c.Config.Env, img.Config.Env); len(env) > 0 {
		m := mapping()
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			addPair(m, k, quoted(v))
		}
		addPair(s, "environment", m)
	}
	if ports := portList(c.HostConfig.PortBindings); len(ports) > 0 {
		addPair(s, "ports", list(ports, true))
	}
	if vols := g.mounts(c); len(vols) > 0 {
		addPair(s, "volumes", list(vols, false))
	}
	switch mode := c.HostConfig.NetworkMode; {
	case mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") || strings.HasPrefix(mode, "service:"):
		addPair(s, "network_mode", scalar(mode))
	default:
		if nets := g.serviceNetworks(c); len(nets) > 0 {
			addPair(s, "networks", list(nets, false))
		}
	}
	if dep := dependsOn(c.Config.Labels[runtime.LabelDependsOn]); dep != nil {
		addPair(s, "depends_on", dep)
	}
	if r := c.HostConfig.RestartPolicy; r.Name != "" && r.Name != "no" {
		policy := r.Name
		if r.Name == "on-failure" && r.MaximumRetryCount > 0 {
			policy += ":" + strconv.Itoa(r.MaximumRetryCount)
		}
		addPair(s, "restart", scalar(policy))
	}
	if h := c.Config.Healthcheck; h != nil && len(h.Test) > 0 && !healthEqual(h, img.Config.Healthcheck) {
		addPair(s, "healthcheck", healthNode(h))
	}
	if c.Config.Tty {
		addPair(s, "tty", boolean(true))
	}
	if c.Config.OpenStdin {
		addPair(s, "stdin_open", boolean(true))
	}
	if c.HostConfig.Privileged {
		addPair(s, "privileged", boolean(true))
	}
	if len(c.HostConfig.CapAdd) > 0 {
		addPair(s, "cap_add", list(c.HostConfig.CapAdd, false))
	}
	if len(c.HostConfig.CapDrop) > 0 {
		addPair(s, "cap_drop", list(c.HostConfig.CapDrop, false))
	}
	if len(c.HostConfig.Dns) > 0 {
		addPair(s, "dns", list(c.HostConfig.Dns, false))
	}
	if len(c.HostConfig.ExtraHosts) > 0 {
		addPair(s, "extra_hosts", list(c.HostConfig.ExtraHosts, true))
	}
	if c.HostConfig.Memory > 0 {
		addPair(s, "mem_limit", scalar(strconv.FormatInt(c.HostConfig.Memory, 10)))
	}
	if c.HostConfig.NanoCpus > 0 {
		addPair(s, "cpus", scalar(strconv.FormatFloat(float64(c.HostConfig.NanoCpus)/1e9, 'f', -1, 64)))
	}
	if labels := ownLabels(c.Config.Labels, img.Config.Labels); len(labels) > 0 {
		m := mapping()
		for _, k := range sortedKeys(labels) {
			addPair(m, k, quoted(labels[k]))
		}
		addPair(s, "labels", m)
	}
	if replicas > 1 {
		d := mapping()
		addPair(d, "replicas", scalar(strconv.Itoa(replicas)))
		addPair(s, "deploy", d)
	}
	return s
}

// mounts returns the service's volumes in short syntax and records the
// named volumes it uses.
func (g *generator) mounts(c containerInspect) []string {
	var out []string
	for _, m := range c.Mounts {
		ro := (m.RW != nil && !*m.RW) || (m.ReadWrite != nil && !*m.ReadWrite)
		suffix := ""
		if ro {
			suffix = ":ro"
		}
		switch m.Type {
		case "volume":
			switch {
			case m.Name == "" || anonymousVolumeRe.MatchString(m.Name):
				out = append(out, m.Destination)
				continue
			case strings.HasPrefix(m.Name, g.project+"_"):
				key := strings.TrimPrefix(m.Name, g.project+"_")
				g.volumes[key] = true
				out = append(out, key+":"+m.Destination+suffix)
			default:
				g.extVolumes[m.Name] = true
				out = append(out, m.Name+":"+m.Destination+suffix)
			}
		case "bind":
			out = append(out, m.Source+":"+m.Destination+suffix)
		}
	}
	return out
}

// serviceNetworks returns the networks the container is attached to and
// records them. Only the project's default network means "no networks:".
func (g *generator) serviceNetworks(c containerInspect) []string {
	var out []string
	for _, n := range sortedKeys(c.NetworkSettings.Networks) {
		switch {
		case n == g.project+"_default":
			out = append(out, "default")
		case strings.HasPrefix(n, g.project+"_"):
			key := strings.TrimPrefix(n, g.project+"_")
			g.networks[key] = true
			out = append(out, key)
		case n == "bridge" || n == "podman":
			// The runtime's default network: compose would create its own.
		default:
			g.extNetworks[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 1 && out[0] == "default" {
		return nil
	}
	return out
}

// portList formats PortBindings as "[ip:]host:container[/udp]", skipping
// the IPv6 duplicate of each IPv4 binding.
func portList(b map[string][]portBinding) []string {
	var out []string
	seen := map[string]bool{}
	for _, cport := range sortedKeys(b) {
		port, proto, _ := strings.Cut(cport, "/")
		if proto == "udp" || proto == "sctp" {
			port += "/" + proto
		}
		bindings := b[cport]
		if len(bindings) == 0 {
			continue
		}
		for _, h := range bindings {
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

// dependsOn parses the depends_on label ("db:service_started:false,...").
func dependsOn(label string) *yaml.Node {
	if label == "" {
		return nil
	}
	type dep struct{ name, cond string }
	var deps []dep
	simple := true
	for _, d := range strings.Split(label, ",") {
		parts := strings.Split(d, ":")
		if parts[0] == "" {
			continue
		}
		cond := "service_started"
		if len(parts) > 1 && parts[1] != "" {
			cond = parts[1]
		}
		if cond != "service_started" {
			simple = false
		}
		deps = append(deps, dep{parts[0], cond})
	}
	if len(deps) == 0 {
		return nil
	}
	if simple {
		names := make([]string, len(deps))
		for i, d := range deps {
			names[i] = d.name
		}
		return list(names, false)
	}
	m := mapping()
	for _, d := range deps {
		c := mapping()
		addPair(c, "condition", scalar(d.cond))
		addPair(m, d.name, c)
	}
	return m
}

func healthEqual(a, b *healthcheck) bool {
	if b == nil {
		return false
	}
	return slices.Equal(a.Test, b.Test) && a.Interval == b.Interval && a.Timeout == b.Timeout &&
		a.StartPeriod == b.StartPeriod && a.Retries == b.Retries
}

func healthNode(h *healthcheck) *yaml.Node {
	m := mapping()
	addPair(m, "test", flowList(h.Test))
	for _, d := range []struct {
		key string
		ns  int64
	}{{"interval", h.Interval}, {"timeout", h.Timeout}, {"start_period", h.StartPeriod}} {
		if d.ns > 0 {
			addPair(m, d.key, scalar(time.Duration(d.ns).String()))
		}
	}
	if h.Retries > 0 {
		addPair(m, "retries", scalar(strconv.Itoa(h.Retries)))
	}
	return m
}

// without returns the entries of a that are not in b.
func without(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// ownLabels drops compose's own labels and labels inherited from the image.
func ownLabels(labels, image map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, runtime.LabelPrefix) || strings.HasPrefix(k, "io.podman.compose.") || strings.HasPrefix(k, "com.microsoft.wsl.") {
			continue
		}
		if iv, ok := image[k]; ok && iv == v {
			continue
		}
		out[k] = v
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// yaml.Node helpers.

func mapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode} }

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// quoted always quotes, so values like "yes", "1.0" or "" stay strings.
func quoted(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func boolean(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
}

func addPair(m *yaml.Node, key string, v *yaml.Node) {
	m.Content = append(m.Content, scalar(key), v)
}

func list(items []string, quote bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	for _, it := range items {
		if quote {
			n.Content = append(n.Content, quoted(it))
		} else {
			n.Content = append(n.Content, scalar(it))
		}
	}
	return n
}

// command keeps a single shell-form string as a string (compose splits it
// like a shell); otherwise it is an exec-form list.
func command(cmd []string) *yaml.Node {
	if len(cmd) == 1 && strings.ContainsAny(cmd[0], " \t") {
		return quoted(cmd[0])
	}
	return flowList(cmd)
}

func flowList(items []string) *yaml.Node {
	n := list(items, true)
	n.Style = yaml.FlowStyle
	return n
}

func external() *yaml.Node {
	m := mapping()
	addPair(m, "external", boolean(true))
	return m
}
