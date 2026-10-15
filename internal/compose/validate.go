// Package compose holds the runtime-independent parts of the Compose
// support: checking a compose file before it is handed to the native
// compose tool, storing the files deployed through the UI, grouping
// containers, volumes and networks into projects by their labels, and
// rebuilding a compose file from running containers.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// MaxFileSize is the largest compose file accepted.
const MaxFileSize = 512 << 10

// Problem is something wrong with a compose file. Line and Col are 1-based;
// 0 means the problem has no position.
type Problem struct {
	Line, Col int
	Msg       string
	Warning   bool // the file is still deployable
}

func (p Problem) String() string {
	if p.Line > 0 {
		return fmt.Sprintf("line %d: %s", p.Line, p.Msg)
	}
	return p.Msg
}

// File is what the UI needs to know about a valid compose file.
type File struct {
	Name     string // top-level name:, "" if not set
	Services []Service
	Networks []string // top-level networks (keys)
	Volumes  []string // top-level volumes (keys)
}

// Service is a service of a compose file.
type Service struct {
	Name     string
	Image    string
	Replicas int
}

// HasErrors reports whether any problem is not a warning.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if !p.Warning {
			return true
		}
	}
	return false
}

var (
	serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	yamlLineRe    = regexp.MustCompile(`line (\d+)(?:, column (\d+))?:\s*`)
	namedVolumeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

	topLevelKeys = map[string]bool{
		"name": true, "version": true, "services": true, "networks": true,
		"volumes": true, "configs": true, "secrets": true, "include": true,
	}
)

// Parse checks the syntax and structure of a compose file. Deeper checks
// (every service option) are left to the compose tool. The returned File
// is only meaningful when there are no errors.
func Parse(src []byte) (*File, []Problem) {
	c := &checker{}
	f := &File{}
	if len(src) > MaxFileSize {
		c.add(nil, false, "the file is larger than %d KiB", MaxFileSize>>10)
		return f, c.problems
	}
	if len(bytes.TrimSpace(src)) == 0 {
		c.add(nil, false, "the file is empty")
		return f, c.problems
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		c.syntax(src, err)
		return f, c.problems
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		c.add(&extra, false, "only one YAML document is allowed (remove the extra ---)")
	} else if !errors.Is(err, io.EOF) {
		c.syntax(src, err)
	}
	if len(doc.Content) == 0 {
		c.add(nil, false, "the file is empty")
		return f, c.problems
	}
	root := doc.Content[0]
	c.duplicates(root)
	if root.Kind != yaml.MappingNode {
		c.add(root, false, "the top level must be a mapping with a services: key")
		return f, c.problems
	}

	top := map[string]*yaml.Node{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		top[k.Value] = v
		switch {
		case k.Value == "version":
			c.add(k, true, "version is obsolete and ignored by Compose v2")
		case strings.HasPrefix(k.Value, "x-"), topLevelKeys[k.Value]:
		default:
			c.add(k, false, "unknown top-level key %q", k.Value)
		}
	}

	if n := top["name"]; n != nil {
		if n.Kind != yaml.ScalarNode {
			c.add(n, false, "name must be a string")
		} else if err := runtime.ValidateProjectName(n.Value); err != nil && !strings.Contains(n.Value, "$") {
			c.add(n, false, "name %q: use lowercase letters, digits, '-' and '_'", n.Value)
		} else {
			f.Name = n.Value
		}
	}
	for _, key := range []string{"networks", "volumes", "configs", "secrets"} {
		if n := top[key]; n != nil && !isNull(n) && n.Kind != yaml.MappingNode {
			c.add(n, false, "%s must be a mapping", key)
		}
	}
	networks := mapKeys(top["networks"])
	volumes := mapKeys(top["volumes"])
	f.Networks, f.Volumes = keyList(top["networks"]), keyList(top["volumes"])

	services := top["services"]
	switch {
	case services == nil && top["include"] != nil:
		return f, c.sorted()
	case services == nil:
		c.add(root, false, "services: is missing")
		return f, c.sorted()
	case services.Kind != yaml.MappingNode:
		c.add(services, false, "services must be a mapping of service names")
		return f, c.sorted()
	case len(services.Content) == 0:
		c.add(services, false, "services is empty")
		return f, c.sorted()
	}
	names := mapKeys(services)
	for i := 0; i+1 < len(services.Content); i += 2 {
		k, v := services.Content[i], services.Content[i+1]
		if !serviceNameRe.MatchString(k.Value) {
			c.add(k, false, "service name %q: use letters, digits, '.', '-' and '_'", k.Value)
		}
		if s, ok := c.service(k, v, names, networks, volumes); ok {
			f.Services = append(f.Services, s)
		}
	}
	return f, c.sorted()
}

type checker struct{ problems []Problem }

func (c *checker) add(n *yaml.Node, warning bool, format string, args ...any) {
	p := Problem{Msg: fmt.Sprintf(format, args...), Warning: warning}
	if n != nil {
		p.Line, p.Col = n.Line, n.Column
	}
	c.problems = append(c.problems, p)
}

func (c *checker) sorted() []Problem {
	sort.SliceStable(c.problems, func(i, j int) bool { return c.problems[i].Line < c.problems[j].Line })
	return c.problems
}

// syntax turns a yaml.v3 error ("yaml: line 3: mapping values are not
// allowed in this context") into a positioned problem.
func (c *checker) syntax(src []byte, err error) {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	p := Problem{Msg: msg}
	if m := yamlLineRe.FindStringSubmatchIndex(msg); m != nil {
		p.Line, _ = strconv.Atoi(msg[m[2]:m[3]])
		if m[4] >= 0 {
			p.Col, _ = strconv.Atoi(msg[m[4]:m[5]])
		}
		p.Msg = msg[:m[0]] + msg[m[1]:]
	}
	// yaml.v3 often names the line where the enclosing block starts, or no
	// line at all. The first line at which the file stops parsing is more
	// useful; find it by bisecting over prefixes of whole lines.
	if l := firstBadLine(src); l > p.Line {
		p.Line, p.Col = l, 0
	}
	c.problems = append(c.problems, p)
}

// firstBadLine returns the smallest n such that the first n lines of src do
// not parse, or 0.
func firstBadLine(src []byte) int {
	lines := bytes.SplitAfter(src, []byte("\n"))
	parses := func(n int) bool {
		var v yaml.Node
		return yaml.Unmarshal(bytes.Join(lines[:n], nil), &v) == nil
	}
	if parses(len(lines)) {
		return 0
	}
	lo, hi := 0, len(lines) // parses(lo), !parses(hi)
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if parses(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// duplicates reports keys that appear twice in the same mapping.
func (c *checker) duplicates(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.ScalarNode && k.Value != "<<" {
				if seen[k.Value] {
					c.add(k, false, "duplicate key %q", k.Value)
				}
				seen[k.Value] = true
			}
		}
	}
	for _, ch := range n.Content {
		c.duplicates(ch)
	}
}

// service checks one service definition.
func (c *checker) service(key, n *yaml.Node, services, networks, volumes map[string]bool) (Service, bool) {
	name := key.Value
	s := Service{Name: name, Replicas: 1}
	if n.Kind != yaml.MappingNode {
		c.add(n, false, "service %q must be a mapping", name)
		return s, false
	}
	opts := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		opts[n.Content[i].Value] = n.Content[i+1]
	}
	if img := opts["image"]; img != nil {
		if img.Kind != yaml.ScalarNode || img.Value == "" {
			c.add(img, false, "service %q: image must be a string", name)
		}
		s.Image = img.Value
	}
	if opts["image"] == nil && opts["build"] == nil && opts["extends"] == nil {
		c.add(key, false, "service %q needs image or build", name)
	}
	if b := opts["build"]; b != nil {
		c.add(b, true, "service %q: build contexts are resolved on the server, relative to the project folder", name)
	}

	// Options that are a list, or a list or a mapping.
	for _, key := range []string{"ports", "volumes", "expose", "cap_add", "cap_drop", "dns", "tmpfs", "env_file", "devices", "secrets", "configs"} {
		if v := opts[key]; v != nil && v.Kind != yaml.SequenceNode && !(v.Kind == yaml.ScalarNode && (key == "dns" || key == "tmpfs" || key == "env_file")) {
			c.add(v, false, "service %q: %s must be a list", name, key)
		}
	}
	for _, key := range []string{"environment", "labels", "depends_on", "networks", "extra_hosts", "sysctls", "ulimits"} {
		if v := opts[key]; v != nil && v.Kind != yaml.SequenceNode && v.Kind != yaml.MappingNode && !isNull(v) {
			c.add(v, false, "service %q: %s must be a list or a mapping", name, key)
		}
	}
	for _, key := range []string{"command", "entrypoint"} {
		if v := opts[key]; v != nil && v.Kind != yaml.SequenceNode && v.Kind != yaml.ScalarNode {
			c.add(v, false, "service %q: %s must be a string or a list", name, key)
		}
	}

	for _, ref := range listOrKeys(opts["depends_on"]) {
		if !services[ref.Value] {
			c.add(ref, false, "service %q depends on undefined service %q", name, ref.Value)
		}
	}
	for _, ref := range listOrKeys(opts["networks"]) {
		if ref.Value != "default" && !networks[ref.Value] && !strings.Contains(ref.Value, "$") {
			c.add(ref, false, "service %q uses undefined network %q (add it under networks:)", name, ref.Value)
		}
	}
	if v := opts["volumes"]; v != nil && v.Kind == yaml.SequenceNode {
		for _, m := range v.Content {
			if src := namedVolume(m); src != "" && !volumes[src] {
				c.add(m, false, "service %q uses undefined volume %q (add it under volumes:)", name, src)
			}
		}
	}
	if d := opts["deploy"]; d != nil && d.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(d.Content); i += 2 {
			if d.Content[i].Value == "replicas" {
				if r, err := strconv.Atoi(d.Content[i+1].Value); err == nil {
					s.Replicas = r
				}
			}
		}
	}
	return s, true
}

// namedVolume returns the volume name of a short-syntax mount
// ("data:/var/lib/x") or of a long-syntax volume mount, "" for binds and
// anonymous volumes.
func namedVolume(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		src, _, ok := strings.Cut(n.Value, ":")
		// A single letter is a Windows drive (C:\data:/data).
		if ok && len(src) > 1 && namedVolumeRe.MatchString(src) {
			return src
		}
	case yaml.MappingNode:
		var typ, src string
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "type":
				typ = n.Content[i+1].Value
			case "source":
				src = n.Content[i+1].Value
			}
		}
		if typ == "volume" && namedVolumeRe.MatchString(src) {
			return src
		}
	}
	return ""
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func mapKeys(n *yaml.Node) map[string]bool {
	out := map[string]bool{}
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			out[n.Content[i].Value] = true
		}
	}
	return out
}

// keyList returns the keys of a mapping in file order.
func keyList(n *yaml.Node) []string {
	var out []string
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
	}
	return out
}

// listOrKeys returns the entries of a list or the keys of a mapping.
func listOrKeys(n *yaml.Node) []*yaml.Node {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.SequenceNode:
		var out []*yaml.Node
		for _, e := range n.Content {
			if e.Kind == yaml.ScalarNode {
				out = append(out, e)
			}
		}
		return out
	case yaml.MappingNode:
		var out []*yaml.Node
		for i := 0; i < len(n.Content); i += 2 {
			out = append(out, n.Content[i])
		}
		return out
	}
	return nil
}
