package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Importing one service of a pasted compose file as a standalone
// container: the file may be complete or just the part of it that defines
// the service (its block, with or without the service name and the
// services: key). The service is turned into create options for the
// create form; nothing is deployed.

// ImportCompose reads a compose file or fragment. With service empty and
// more than one service to choose from, it returns the choices and no
// options; with one service, or service naming one, it returns its
// options.
func ImportCompose(src []byte, service string) (runtime.ImportSource, []Service, error) {
	f, err := readFragment(src)
	if err != nil {
		return runtime.ImportSource{}, nil, err
	}
	var choices []Service
	for _, s := range f.services {
		if img, ok := s.body.get("image"); ok && scalarText(img) != "" {
			choices = append(choices, Service{Name: s.name, Image: interpolate(scalarText(img), nil), Replicas: 1})
		}
	}
	var pick *fragmentService
	switch {
	case service != "":
		i := slices.IndexFunc(f.services, func(s fragmentService) bool { return s.name == service })
		if i < 0 {
			return runtime.ImportSource{}, nil, fmt.Errorf("%w: the compose file has no service %q", runtime.ErrInvalid, service)
		}
		pick = &f.services[i]
	case len(f.services) == 1:
		pick = &f.services[0]
	case len(choices) == 1:
		pick = &f.services[slices.IndexFunc(f.services, func(s fragmentService) bool { return s.name == choices[0].Name })]
	case len(choices) == 0:
		return runtime.ImportSource{}, nil, fmt.Errorf("%w: no service of the compose file names an image (build: is not supported here; build the image first)", runtime.ErrInvalid)
	default:
		return runtime.ImportSource{}, choices, nil
	}
	imp, err := f.importService(*pick)
	return imp, nil, err
}

// fragment is a parsed compose file or part of one.
type fragment struct {
	project  string
	services []fragmentService
	volumes  map[string]string // top-level volume key -> real name, if known
	networks map[string]string // top-level network key -> real name, if known
}

type fragmentService struct {
	name string // "" for a bare service body
	body omap
}

// Keys that mark a mapping as a service body.
var serviceKeys = []string{"image", "build", "ports", "environment", "volumes", "command", "container_name", "restart", "networks"}

func readFragment(src []byte) (*fragment, error) {
	if len(src) > MaxFileSize {
		return nil, fmt.Errorf("%w: the compose file is larger than %d KiB", runtime.ErrInvalid, MaxFileSize>>10)
	}
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, fmt.Errorf("%w: paste a compose file or the part of it that defines the service", runtime.ErrInvalid)
	}
	src = dedent(src)
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(src)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: not valid YAML: %s", runtime.ErrInvalid, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("%w: paste a compose file or the part of it that defines the service", runtime.ErrInvalid)
	}
	root, ok := toValue(doc.Content[0]).(omap)
	if !ok {
		return nil, fmt.Errorf("%w: the compose file must be a mapping (services: or a service with image:)", runtime.ErrInvalid)
	}
	f := &fragment{volumes: map[string]string{}, networks: map[string]string{}}
	if n, ok := root.get("name"); ok {
		f.project = interpolate(scalarText(n), nil)
	}
	for _, top := range []struct {
		key   string
		names map[string]string
	}{{"volumes", f.volumes}, {"networks", f.networks}} {
		m, _ := root.getMap(top.key)
		for _, kv := range m {
			def, _ := kv.v.(omap)
			if name, ok := def.get("name"); ok && scalarText(name) != "" {
				top.names[kv.k] = interpolate(scalarText(name), nil)
			} else if ext, ok := def.get("external"); ok && isTrueValue(ext) {
				top.names[kv.k] = kv.k
			}
		}
	}

	switch services, ok := root.get("services"); {
	case ok:
		m, isMap := services.(omap)
		if !isMap || len(m) == 0 {
			return nil, fmt.Errorf("%w: services: must be a mapping of service names", runtime.ErrInvalid)
		}
		for _, kv := range m {
			body, _ := kv.v.(omap)
			f.services = append(f.services, fragmentService{kv.k, body})
		}
	case root.hasAny("image", "build"):
		// The body of one service, without its name.
		f.services = []fragmentService{{"", root}}
	default:
		// Service entries without the services: key.
		for _, kv := range root {
			if body, ok := kv.v.(omap); ok && body.hasAny(serviceKeys...) {
				f.services = append(f.services, fragmentService{kv.k, body})
			}
		}
	}
	if len(f.services) == 0 {
		return nil, fmt.Errorf("%w: no service found; paste a compose file or the service's block with its image:", runtime.ErrInvalid)
	}
	return f, nil
}

// dedent removes the indentation all lines share, so that a block copied
// from the middle of a file parses.
func dedent(src []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	indent := -1
	for _, l := range lines {
		t := strings.TrimLeft(l, " ")
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if n := len(l) - len(t); indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return []byte(strings.Join(lines, "\n"))
	}
	for i, l := range lines {
		if len(l) >= indent && strings.TrimSpace(l[:indent]) == "" {
			lines[i] = l[indent:]
		} else {
			lines[i] = strings.TrimLeft(l, " ")
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// importer turns one service into create options.
type importer struct {
	f    *fragment
	name string
	imp  runtime.ImportSource
	vars []string // variables without a default
}

func (im *importer) note(format string, args ...any) {
	im.imp.Notes = append(im.imp.Notes, fmt.Sprintf(format, args...))
}

func (im *importer) lost(format string, args ...any) {
	im.imp.Lost = append(im.imp.Lost, fmt.Sprintf(format, args...))
}

// str returns a scalar with variables substituted.
func (im *importer) str(v any) string { return interpolate(scalarText(v), &im.vars) }

// strs returns a list (or a single scalar) of strings.
func (im *importer) strs(v any) []string {
	switch v := v.(type) {
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, im.str(x))
		}
		return out
	case nil:
		return nil
	default:
		return []string{im.str(v)}
	}
}

// keyValues returns a list or mapping as key=value lines; a mapping
// entry without a value is returned as the bare key.
func (im *importer) keyValues(v any) []string {
	if m, ok := v.(omap); ok {
		var out []string
		for _, kv := range m {
			if kv.v == nil {
				out = append(out, kv.k)
				continue
			}
			out = append(out, kv.k+"="+im.str(kv.v))
		}
		return out
	}
	return im.strs(v)
}

// args returns a command or entrypoint given as a string or a list.
func (im *importer) args(key string, v any) []string {
	if l, ok := v.([]any); ok {
		out := []string{}
		for _, x := range l {
			out = append(out, im.str(x))
		}
		return out
	}
	words, err := runtime.SplitWords(im.str(v))
	if err != nil {
		im.lost("%s: %s", key, im.str(v))
		return nil
	}
	return words
}

func (f *fragment) importService(s fragmentService) (runtime.ImportSource, error) {
	im := &importer{f: f, name: s.name}
	im.imp.Service, im.imp.Project = s.name, f.project
	o := &im.imp.Opts
	o.Start, o.Pull = true, runtime.PullMissing
	what := "the service"
	if s.name != "" {
		what = "service " + s.name
	}

	img, _ := s.body.get("image")
	o.Image = im.str(img)
	if o.Image == "" {
		if s.body.hasAny("build") {
			return im.imp, fmt.Errorf("%w: %s has no image: build: is not supported here; build the image first and paste the service with its image:", runtime.ErrInvalid, what)
		}
		return im.imp, fmt.Errorf("%w: %s has no image:", runtime.ErrInvalid, what)
	}
	if runtime.ValidateContainerName(s.name) == nil {
		o.Name = s.name
	}

	var limits omap
	for _, kv := range s.body {
		k, v := kv.k, kv.v
		switch k {
		case "image", "profiles", "scale":
		case "container_name":
			o.Name = im.str(v)
		case "build":
			im.note("build: is ignored; image %s is used, so it must exist locally or in its registry.", o.Image)
		case "command":
			o.Cmd = im.args(k, v)
		case "entrypoint":
			o.Entrypoint = im.args(k, v)
		case "environment":
			for _, e := range im.keyValues(v) {
				if !strings.Contains(e, "=") {
					im.note("Environment variable %s takes its value from the environment compose runs in; enter it as %s=value.", e, e)
				}
				o.Env = append(o.Env, e)
			}
		case "env_file":
			for _, e := range im.envFiles(v) {
				im.lost("env_file %s (copy its variables into Environment variables)", e)
			}
		case "ports":
			im.ports(v)
		case "expose":
			im.lost("expose %s (only documents the ports)", strings.Join(im.strs(v), ", "))
		case "volumes":
			im.volumes(v)
		case "networks":
			im.networks(v)
		case "network_mode":
			switch m := im.str(v); m {
			case "bridge", "default":
			case "host", "none":
				o.Networks = []runtime.NetworkAttachment{{Name: m}}
			default:
				im.lost("network_mode: %s", m)
			}
		case "restart":
			if r := im.str(v); r != "no" {
				o.RestartPolicy = r
			}
		case "labels":
			for _, l := range im.keyValues(v) {
				if !strings.Contains(l, "=") {
					l += "="
				}
				o.Labels = append(o.Labels, l)
			}
		case "hostname":
			o.Hostname = im.str(v)
		case "user":
			o.User = im.str(v)
		case "working_dir":
			o.WorkDir = im.str(v)
		case "cap_add":
			o.CapAdd = im.strs(v)
		case "cap_drop":
			o.CapDrop = im.strs(v)
		case "privileged":
			o.Privileged = isTrueValue(v)
		case "tty":
			o.TTY = isTrueValue(v)
		case "stdin_open":
			o.Interactive = isTrueValue(v)
		case "dns":
			o.DNS = im.strs(v)
		case "dns_search":
			o.DNSSearch = im.strs(v)
		case "dns_opt":
			o.DNSOptions = im.strs(v)
		case "extra_hosts":
			for _, h := range im.keyValues(v) {
				o.ExtraHosts = append(o.ExtraHosts, runtime.ExtraHost(h))
			}
		case "ulimits":
			im.ulimits(v)
		case "cpus":
			o.CPUs = im.str(v)
		case "mem_limit":
			o.Memory = runtime.NormalizeMemory(im.str(v))
		case "healthcheck":
			hc, _ := v.(omap)
			test, _ := hc.get("test")
			if d, ok := hc.get("disable"); ok && isTrueValue(d) || slices.Equal(im.strs(test), []string{"NONE"}) {
				o.NoHealthcheck = true
			} else {
				im.lost("healthcheck (the image's own health check is used)")
			}
		case "pull_policy":
			switch p := im.str(v); p {
			case "if_not_present":
				o.Pull = runtime.PullMissing
			default:
				if pp, ok := runtime.ParsePullPolicy(p); ok {
					o.Pull = pp
				} else {
					im.lost("pull_policy: %s", p)
				}
			}
		case "depends_on":
			var deps []string
			if m, ok := v.(omap); ok {
				for _, kv := range m {
					deps = append(deps, kv.k)
				}
			} else {
				deps = im.strs(v)
			}
			im.note("The service depends on %s; those containers are not created.", strings.Join(deps, ", "))
		case "deploy":
			limits = im.deploy(v)
		default:
			if sc := scalarText(v); sc != "" {
				im.lost("%s: %s", k, im.str(v))
			} else {
				im.lost("%s", k)
			}
		}
	}
	if c, ok := limits.get("cpus"); ok && o.CPUs == "" {
		o.CPUs = im.str(c)
	}
	if m, ok := limits.get("memory"); ok && o.Memory == "" {
		o.Memory = runtime.NormalizeMemory(im.str(m))
	}
	if len(im.vars) > 0 {
		slices.Sort(im.vars)
		im.note("The compose file uses %s, which compose fills in from the environment or an .env file; replace them with their values.", strings.Join(slices.Compact(im.vars), ", "))
	}
	return im.imp, nil
}

func (im *importer) envFiles(v any) []string {
	if l, ok := v.([]any); ok {
		var out []string
		for _, x := range l {
			if m, ok := x.(omap); ok {
				p, _ := m.get("path")
				out = append(out, im.str(p))
			} else {
				out = append(out, im.str(x))
			}
		}
		return out
	}
	return im.strs(v)
}

func (im *importer) ports(v any) {
	l, _ := v.([]any)
	for _, p := range l {
		spec := im.str(p)
		if m, ok := p.(omap); ok {
			// Long syntax.
			get := func(k string) string { x, _ := m.get(k); return im.str(x) }
			spec = get("target")
			if pub := get("published"); pub != "" {
				spec = pub + ":" + spec
				if ip := get("host_ip"); ip != "" {
					spec = ip + ":" + spec
				}
			}
			if proto := get("protocol"); proto != "" {
				spec += "/" + proto
			}
		}
		ps, err := runtime.ExpandPortSpec(spec)
		if err != nil {
			im.lost("published port %s", spec)
			continue
		}
		im.imp.Opts.Ports = append(im.imp.Opts.Ports, ps...)
	}
}

var relativePathRe = regexp.MustCompile(`^(\.|~)`)

func (im *importer) volumes(v any) {
	l, _ := v.([]any)
	o := &im.imp.Opts
	for _, x := range l {
		var vm runtime.VolumeMount
		named := false
		if m, ok := x.(omap); ok {
			get := func(k string) string { y, _ := m.get(k); return im.str(y) }
			typ := get("type")
			vm = runtime.VolumeMount{Source: get("source"), Target: get("target")}
			if ro, ok := m.get("read_only"); ok {
				vm.ReadOnly = isTrueValue(ro)
			}
			switch {
			case typ != "bind" && typ != "volume":
				im.lost("%s mount at %s", typ, vm.Target)
				continue
			case vm.Source == "":
				im.lost("anonymous volume %s", vm.Target)
				continue
			}
			named = typ == "volume"
		} else {
			var lost []string
			var ok bool
			vm, lost, ok = runtime.ImportVolumeSpec(im.str(x))
			im.imp.Lost = append(im.imp.Lost, lost...)
			if !ok {
				continue
			}
			named = !strings.ContainsAny(vm.Source, `/\`) && !relativePathRe.MatchString(vm.Source)
		}
		switch {
		case relativePathRe.MatchString(vm.Source):
			im.note("Bind mount %s is relative to the compose file's folder; enter the absolute path on this host.", vm.Source)
		case named:
			vm.Source = im.resourceName("Volume", vm.Source, im.f.volumes)
		}
		o.Volumes = append(o.Volumes, vm)
	}
}

// resourceName returns the name compose gives a project's volume or
// network: the name: or external one, else <project>_<key>.
func (im *importer) resourceName(kind, key string, names map[string]string) string {
	if n, ok := names[key]; ok {
		return n
	}
	if im.f.project != "" {
		n := im.f.project + "_" + key
		im.note("%s %s is named %s by compose project %s; that one is used, so the container shares its data with the project.", kind, key, n, im.f.project)
		return n
	}
	im.note("%s %s: compose prefixes its name with the project name (usually the folder name), e.g. myapp_%s. To share it with a deployed project, use that name (see the %ss page).", kind, key, key, strings.ToLower(kind))
	return key
}

func (im *importer) networks(v any) {
	o := &im.imp.Opts
	add := func(key string, cfg omap) {
		var name string
		if key == "default" {
			if im.f.project == "" {
				im.note("Network default is the compose project's default network; choose a network.")
				return
			}
			name = im.f.project + "_default"
			im.note("Network default is %s, the default network of compose project %s.", name, im.f.project)
		} else {
			name = im.resourceName("Network", key, im.f.networks)
		}
		n := runtime.NetworkAttachment{Name: name}
		if a, ok := cfg.get("aliases"); ok {
			n.Aliases = im.strs(a)
		}
		if ip, ok := cfg.get("ipv4_address"); ok {
			n.IP = im.str(ip)
		}
		for _, kv := range cfg {
			if kv.k != "aliases" && kv.k != "ipv4_address" {
				im.lost("network %s option %s", key, kv.k)
			}
		}
		o.Networks = append(o.Networks, n)
	}
	switch v := v.(type) {
	case omap:
		for _, kv := range v {
			cfg, _ := kv.v.(omap)
			add(kv.k, cfg)
		}
	default:
		for _, n := range im.strs(v) {
			add(n, nil)
		}
	}
}

func (im *importer) ulimits(v any) {
	m, _ := v.(omap)
	for _, kv := range m {
		if lim, ok := kv.v.(omap); ok {
			soft, _ := lim.get("soft")
			hard, _ := lim.get("hard")
			im.imp.Opts.Ulimits = append(im.imp.Opts.Ulimits, kv.k+"="+im.str(soft)+":"+im.str(hard))
			continue
		}
		im.imp.Opts.Ulimits = append(im.imp.Opts.Ulimits, kv.k+"="+im.str(kv.v))
	}
}

// deploy handles deploy: and returns its resources.limits.
func (im *importer) deploy(v any) omap {
	d, _ := v.(omap)
	var limits omap
	for _, kv := range d {
		switch kv.k {
		case "replicas":
			if r := im.str(kv.v); r != "1" {
				im.note("The service runs %s replicas; one container is created.", r)
			}
		case "resources":
			res, _ := kv.v.(omap)
			for _, r := range res {
				if r.k == "limits" {
					limits, _ = r.v.(omap)
					for _, l := range limits {
						if l.k != "cpus" && l.k != "memory" {
							im.lost("deploy.resources.limits.%s", l.k)
						}
					}
				} else {
					im.lost("deploy.resources.%s", r.k)
				}
			}
		case "restart_policy":
			rp, _ := kv.v.(omap)
			cond, _ := rp.get("condition")
			switch c := im.str(cond); c {
			case "", "any":
				if im.imp.Opts.RestartPolicy == "" {
					im.imp.Opts.RestartPolicy = "always"
				}
			case "on-failure":
				if im.imp.Opts.RestartPolicy == "" {
					im.imp.Opts.RestartPolicy = "on-failure"
				}
			case "none":
			default:
				im.lost("deploy.restart_policy.condition: %s", c)
			}
		default:
			im.lost("deploy.%s", kv.k)
		}
	}
	return limits
}

// ---------- YAML values ----------

// omap is a YAML mapping with its order kept.
type omap []omapEntry

type omapEntry struct {
	k string
	v any
}

func (m omap) get(k string) (any, bool) {
	for _, e := range m {
		if e.k == k {
			return e.v, true
		}
	}
	return nil, false
}

func (m omap) getMap(k string) (omap, bool) {
	v, ok := m.get(k)
	om, isMap := v.(omap)
	return om, ok && isMap
}

func (m omap) hasAny(keys ...string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { _, ok := m.get(k); return ok })
}

// toValue converts a YAML node into omap, []any, string (scalars) or nil
// (null), following aliases and merge keys (<<).
func toValue(n *yaml.Node) any {
	switch n.Kind {
	case yaml.AliasNode:
		return toValue(n.Alias)
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return toValue(n.Content[0])
	case yaml.SequenceNode:
		out := []any{}
		for _, c := range n.Content {
			out = append(out, toValue(c))
		}
		return out
	case yaml.MappingNode:
		var m omap
		var merged []omap
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "<<" && k.Tag == "!!merge" {
				switch x := toValue(v).(type) {
				case omap:
					merged = append(merged, x)
				case []any:
					for _, y := range x {
						if ym, ok := y.(omap); ok {
							merged = append(merged, ym)
						}
					}
				}
				continue
			}
			m = append(m, omapEntry{k.Value, toValue(v)})
		}
		for _, mm := range merged {
			for _, e := range mm {
				if _, ok := m.get(e.k); !ok {
					m = append(m, e)
				}
			}
		}
		if m == nil {
			m = omap{}
		}
		return m
	default:
		if n.Tag == "!!null" {
			return nil
		}
		return n.Value
	}
}

// scalarText returns a scalar's text, "" for anything else.
func scalarText(v any) string {
	s, _ := v.(string)
	return s
}

func isTrueValue(v any) bool {
	switch strings.ToLower(scalarText(v)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

var interpolationRe = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// interpolate resolves compose variables as if none were set: defaults
// (${VAR:-x}, ${VAR-x}) are used, ${VAR:+x} is empty, $$ is $. Other
// variables are kept and their names added to vars.
func interpolate(s string, vars *[]string) string {
	return interpolationRe.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		sub := interpolationRe.FindStringSubmatch(m)
		name, op, arg := sub[1], sub[2], sub[3]
		if name == "" {
			name = sub[4]
		}
		switch op {
		case ":-", "-":
			return arg
		case ":+", "+":
			return ""
		}
		if vars != nil {
			*vars = append(*vars, name)
		}
		return m
	})
}
