package runtime

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Importing a container from a pasted "docker run" or "docker create"
// command: the command is split like a shell would and its flags are
// turned into create options. Flags the create form has no field for are
// reported in Lost; nothing is created here.

// MaxPortRange is the largest port range expanded into single mappings.
const MaxPortRange = 100

// ImportFromCLI turns a pasted docker (or podman) run / create command into
// options for a new container.
func ImportFromCLI(text string) (ImportSource, error) {
	cmds, err := shellCommands(text)
	if err != nil {
		return ImportSource{}, err
	}
	var found []cliCommand
	compose := false
	for _, words := range cmds {
		c, ok := dockerCommand(words)
		if !ok {
			continue
		}
		switch c.sub {
		case "run", "create":
			found = append(found, c)
		case "compose":
			compose = true
		}
	}
	switch {
	case len(found) == 0 && compose:
		return ImportSource{}, fmt.Errorf("%w: this is a docker compose command; choose \"From a compose file\" and paste the compose file instead", ErrInvalid)
	case len(found) == 0:
		return ImportSource{}, fmt.Errorf("%w: paste a docker run or docker create command, e.g. docker run -d -p 8080:80 nginx", ErrInvalid)
	case len(found) > 1:
		return ImportSource{}, fmt.Errorf("%w: the text contains %d docker run or create commands; paste one", ErrInvalid, len(found))
	}
	return found[0].importSource()
}

// cliCommand is a docker run / create command split into words.
type cliCommand struct {
	program string // docker, podman, …
	sub     string // run, create, compose, …
	args    []string
}

// dockerCommand recognizes [sudo] [VAR=x] docker|podman [global flags]
// [container] <sub> args….
func dockerCommand(words []string) (cliCommand, bool) {
	i := 0
	for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") {
		i++ // FOO=bar docker run …
	}
	if i < len(words) && (words[i] == "sudo" || words[i] == "doas") {
		i++
		for i < len(words) && strings.HasPrefix(words[i], "-") {
			if words[i] == "-u" || words[i] == "-g" {
				i++
			}
			i++
		}
	}
	if i >= len(words) {
		return cliCommand{}, false
	}
	prog := strings.ToLower(path.Base(strings.ReplaceAll(words[i], `\`, "/")))
	prog = strings.TrimSuffix(prog, ".exe")
	switch prog {
	case "docker", "podman", "nerdctl":
	default:
		return cliCommand{}, false
	}
	i++
	// Global flags before the subcommand.
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		f := words[i]
		i++
		if strings.Contains(f, "=") {
			continue
		}
		switch f {
		case "-H", "--host", "-c", "--context", "--config", "-l", "--log-level", "--tlscacert", "--tlscert", "--tlskey",
			"--connection", "--url", "--identity", "--root", "--runroot", "--storage-driver", "--namespace", "-n", "--address":
			i++
		}
	}
	if i < len(words) && words[i] == "container" {
		i++
	}
	if i >= len(words) {
		return cliCommand{prog, "", nil}, true
	}
	return cliCommand{prog, words[i], words[i+1:]}, true
}

// cliFlag describes a docker run flag the importer knows.
type cliFlag struct {
	value bool // takes a value
	set   func(b *cliBuilder, v string)
}

var cliShort = map[byte]string{
	'i': "interactive", 't': "tty", 'd': "detach", 'e': "env", 'p': "publish", 'v': "volume",
	'l': "label", 'm': "memory", 'u': "user", 'w': "workdir", 'h': "hostname", 'P': "publish-all",
	'a': "attach", 'q': "quiet",
}

var cliAliases = map[string]string{
	"net": "network", "net-alias": "network-alias", "dns-opt": "dns-option",
}

// cliBoolFlags are docker / podman run flags without a value that the form
// has no field for; every other unknown flag is assumed to take a value.
var cliBoolFlags = map[string]bool{
	"init": true, "read-only": true, "publish-all": true, "sig-proxy": true, "oom-kill-disable": true,
	"disable-content-trust": true, "quiet": true, "help": true, "use-api-socket": true, "replace": true,
	"rmi": true, "read-only-tmpfs": true, "env-host": true, "no-hosts": true, "http-proxy": true,
	"passwd": true, "rootfs": true, "tls-verify": true,
}

var cliFlags = map[string]cliFlag{
	"interactive":    {false, func(b *cliBuilder, v string) { b.o.Interactive = isTrue(v) }},
	"tty":            {false, func(b *cliBuilder, v string) { b.o.TTY = isTrue(v) }},
	"rm":             {false, func(b *cliBuilder, v string) { b.o.AutoRemove = isTrue(v) }},
	"privileged":     {false, func(b *cliBuilder, v string) { b.o.Privileged = isTrue(v) }},
	"no-healthcheck": {false, func(b *cliBuilder, v string) { b.o.NoHealthcheck = isTrue(v) }},
	"detach":         {false, func(*cliBuilder, string) {}},
	"attach":         {true, func(*cliBuilder, string) {}},
	"detach-keys":    {true, func(*cliBuilder, string) {}},
	"name":           {true, func(b *cliBuilder, v string) { b.o.Name = v }},
	"publish":        {true, func(b *cliBuilder, v string) { b.port(v) }},
	"volume":         {true, func(b *cliBuilder, v string) { b.volume(v) }},
	"mount":          {true, func(b *cliBuilder, v string) { b.mount(v) }},
	"env": {true, func(b *cliBuilder, v string) {
		if !strings.Contains(v, "=") {
			b.note("Environment variable %s takes its value from the shell docker runs in; enter it as %s=value.", v, v)
		}
		b.o.Env = append(b.o.Env, v)
	}},
	"label": {true, func(b *cliBuilder, v string) {
		if !strings.Contains(v, "=") {
			v += "="
		}
		b.o.Labels = append(b.o.Labels, v)
	}},
	"network":       {true, func(b *cliBuilder, v string) { b.network(v) }},
	"network-alias": {true, func(b *cliBuilder, v string) { b.aliases = append(b.aliases, splitList(v)...) }},
	"ip":            {true, func(b *cliBuilder, v string) { b.ip = v }},
	"restart":       {true, func(b *cliBuilder, v string) { b.o.RestartPolicy = v }},
	"entrypoint": {true, func(b *cliBuilder, v string) {
		if v == "" {
			b.lost("--entrypoint \"\" (no entrypoint)")
			return
		}
		b.o.Entrypoint = []string{v}
	}},
	"workdir":    {true, func(b *cliBuilder, v string) { b.o.WorkDir = v }},
	"user":       {true, func(b *cliBuilder, v string) { b.o.User = v }},
	"hostname":   {true, func(b *cliBuilder, v string) { b.o.Hostname = v }},
	"cpus":       {true, func(b *cliBuilder, v string) { b.o.CPUs = v }},
	"memory":     {true, func(b *cliBuilder, v string) { b.o.Memory = NormalizeMemory(v) }},
	"dns":        {true, func(b *cliBuilder, v string) { b.o.DNS = append(b.o.DNS, splitList(v)...) }},
	"dns-search": {true, func(b *cliBuilder, v string) { b.o.DNSSearch = append(b.o.DNSSearch, splitList(v)...) }},
	"dns-option": {true, func(b *cliBuilder, v string) { b.o.DNSOptions = append(b.o.DNSOptions, v) }},
	"ulimit":     {true, func(b *cliBuilder, v string) { b.o.Ulimits = append(b.o.Ulimits, v) }},
	"add-host":   {true, func(b *cliBuilder, v string) { b.o.ExtraHosts = append(b.o.ExtraHosts, ExtraHost(v)) }},
	"cap-add":    {true, func(b *cliBuilder, v string) { b.o.CapAdd = append(b.o.CapAdd, splitList(v)...) }},
	"cap-drop":   {true, func(b *cliBuilder, v string) { b.o.CapDrop = append(b.o.CapDrop, splitList(v)...) }},
	"pull": {true, func(b *cliBuilder, v string) {
		if p, ok := ParsePullPolicy(v); ok {
			b.o.Pull = p
		} else {
			b.lost("--pull %s", v)
		}
	}},
}

func isTrue(v string) bool { return v == "" || v == "true" || v == "1" }

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
}

// cliBuilder collects the options of one command.
type cliBuilder struct {
	imp     ImportSource
	o       *CreateContainerOpts
	aliases []string
	ip      string
}

func (b *cliBuilder) note(format string, args ...any) {
	b.imp.Notes = append(b.imp.Notes, fmt.Sprintf(format, args...))
}

func (b *cliBuilder) lost(format string, args ...any) {
	b.imp.Lost = append(b.imp.Lost, fmt.Sprintf(format, args...))
}

func (c cliCommand) importSource() (ImportSource, error) {
	b := &cliBuilder{}
	b.o = &b.imp.Opts
	b.o.Pull = PullMissing
	b.o.Start = c.sub == "run"
	args := c.args
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = args[i+1:]
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = args[i:]
			break
		}
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		if strings.HasPrefix(a, "--") {
			name, val, hasVal := strings.Cut(a[2:], "=")
			if err := b.flag("--"+name, name, val, hasVal, next); err != nil {
				return ImportSource{}, err
			}
			continue
		}
		// Short flags, possibly combined: -dit, -p8080:80, -e KEY=v.
		for j := 1; j < len(a); j++ {
			name, ok := cliShort[a[j]]
			if !ok {
				name = string(a[j])
			}
			f, known := cliFlags[name]
			if !known && !cliBoolFlags[name] && len(a) > 2 {
				// An unknown flag inside a group such as -xyz: no value to take.
				b.lost("-%c", a[j])
				continue
			}
			if takesValue := (known && f.value) || !known && !cliBoolFlags[name]; takesValue {
				val, hasVal := a[j+1:], j+1 < len(a)
				if err := b.flag("-"+string(a[j]), name, strings.TrimPrefix(val, "="), hasVal, next); err != nil {
					return ImportSource{}, err
				}
				break
			}
			if err := b.flag("-"+string(a[j]), name, "", false, next); err != nil {
				return ImportSource{}, err
			}
		}
	}
	if len(rest) == 0 {
		return ImportSource{}, fmt.Errorf("%w: the %s %s command names no image", ErrInvalid, c.program, c.sub)
	}
	b.o.Image, b.o.Cmd = rest[0], rest[1:]
	if len(b.o.Cmd) == 0 {
		b.o.Cmd = nil
	}
	b.finishNetworks()
	return b.imp, nil
}

// flag applies one flag. display is the flag as written, name its long
// name; next consumes the following argument as the value.
func (b *cliBuilder) flag(display, name, val string, hasVal bool, next func() (string, bool)) error {
	if n, ok := cliAliases[name]; ok {
		name = n
	}
	f, known := cliFlags[name]
	if !known {
		if cliBoolFlags[name] {
			if !hasVal || isTrue(val) {
				b.lost("%s", display)
			}
			return nil
		}
		if !hasVal {
			var ok bool
			if val, ok = next(); !ok {
				b.lost("%s", display)
				return nil
			}
		}
		b.lost("%s %s", display, val)
		return nil
	}
	if f.value && !hasVal {
		var ok bool
		if val, ok = next(); !ok {
			return fmt.Errorf("%w: %s needs a value", ErrInvalid, display)
		}
	}
	if !f.value && hasVal && !isTrue(val) && val != "false" && val != "0" {
		return fmt.Errorf("%w: %s=%s: expected true or false", ErrInvalid, display, val)
	}
	if !f.value && hasVal && (val == "false" || val == "0") {
		f.set(b, "false")
		return nil
	}
	f.set(b, val)
	return nil
}

func (b *cliBuilder) port(spec string) {
	ps, err := ExpandPortSpec(spec)
	if err != nil {
		b.lost("published port %s (%v)", spec, plainError(err))
		return
	}
	b.o.Ports = append(b.o.Ports, ps...)
}

var volumeOptRe = regexp.MustCompile(`^[a-zA-Z,]+$`)

func (b *cliBuilder) volume(spec string) {
	v, lost, ok := ImportVolumeSpec(spec)
	b.imp.Lost = append(b.imp.Lost, lost...)
	if ok {
		b.o.Volumes = append(b.o.Volumes, v)
	}
}

// ImportVolumeSpec parses a -v / compose short-syntax volume
// "source:/target[:options]". Options other than ro and rw are returned
// as lost; ok is false for specs the form cannot hold (anonymous volumes),
// which are described in lost too.
func ImportVolumeSpec(spec string) (v VolumeMount, lost []string, ok bool) {
	s, opts := spec, ""
	if i := strings.LastIndex(s, ":"); i > 0 && volumeOptRe.MatchString(s[i+1:]) && strings.Contains(s[:i], ":") {
		s, opts = s[:i], s[i+1:]
	}
	if !strings.Contains(s, ":") || strings.LastIndex(s, ":/") <= 0 {
		if strings.HasPrefix(s, "/") && !strings.Contains(s, ":") {
			return v, []string{"anonymous volume " + spec}, false
		}
		return v, []string{"volume " + spec}, false
	}
	v, err := ParseVolumeSpec(s)
	if err != nil {
		return v, []string{"volume " + spec}, false
	}
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "", "rw":
		case "ro":
			v.ReadOnly = true
		default:
			lost = append(lost, fmt.Sprintf("volume option %s of %s", o, v.Target))
		}
	}
	return v, lost, true
}

// mount handles --mount type=bind,source=…,target=…[,readonly].
func (b *cliBuilder) mount(spec string) {
	typ := "volume"
	var v VolumeMount
	var extra []string
	for _, kv := range strings.Split(spec, ",") {
		k, val, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "type":
			typ = val
		case "source", "src":
			v.Source = val
		case "target", "destination", "dst":
			v.Target = val
		case "readonly", "ro":
			v.ReadOnly = val == "" || val == "true" || val == "1"
		case "":
		default:
			extra = append(extra, kv)
		}
	}
	switch {
	case typ != "bind" && typ != "volume":
		b.lost("%s mount %s", typ, spec)
		return
	case v.Source == "":
		b.lost("anonymous volume %s", v.Target)
		return
	case v.Target == "":
		b.lost("mount %s", spec)
		return
	}
	for _, e := range extra {
		b.lost("mount option %s of %s", e, v.Target)
	}
	b.o.Volumes = append(b.o.Volumes, v)
}

// network handles --network name and the long form
// --network name=x,alias=a,ip=1.2.3.4.
func (b *cliBuilder) network(v string) {
	n := NetworkAttachment{Name: v}
	if strings.Contains(v, "=") {
		n = NetworkAttachment{}
		for _, kv := range strings.Split(v, ",") {
			k, val, _ := strings.Cut(kv, "=")
			switch k {
			case "name":
				n.Name = val
			case "alias":
				n.Aliases = append(n.Aliases, val)
			case "ip":
				n.IP = val
			default:
				b.lost("network option %s of %s", kv, v)
			}
		}
	}
	if n.Name == "" {
		b.lost("network %s", v)
		return
	}
	switch n.Name {
	case "default", "bridge":
		if len(n.Aliases) == 0 && n.IP == "" {
			return // the default network
		}
	}
	if strings.HasPrefix(n.Name, "container:") {
		b.lost("network %s", n.Name)
		return
	}
	b.o.Networks = append(b.o.Networks, n)
}

// finishNetworks applies --network-alias and --ip to the first network.
func (b *cliBuilder) finishNetworks() {
	if len(b.aliases) == 0 && b.ip == "" {
		return
	}
	if len(b.o.Networks) == 0 {
		if len(b.aliases) > 0 {
			b.lost("network aliases %s (only possible in a network other than the default one)", strings.Join(b.aliases, ", "))
		}
		if b.ip != "" {
			b.lost("IP address %s (only possible in a network other than the default one)", b.ip)
		}
		return
	}
	n := &b.o.Networks[0]
	n.Aliases = append(n.Aliases, b.aliases...)
	if b.ip != "" {
		n.IP = b.ip
	}
}

// plainError strips the ErrInvalid prefix of an error message.
func plainError(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// ExtraHost converts host=ip (newer docker) to the host:ip form.
func ExtraHost(v string) string {
	if h, ip, ok := strings.Cut(v, "="); ok {
		return h + ":" + ip
	}
	return v
}

var memSuffixRe = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([bkmg])(?:i?b)?$`)

// NormalizeMemory turns sizes like 512mb, 1GiB or 1 g into the form the
// create form accepts (512m, 1G).
func NormalizeMemory(v string) string {
	v = strings.TrimSpace(v)
	if m := memSuffixRe.FindStringSubmatch(v); m != nil {
		return m[1] + m[2]
	}
	return v
}

// ExpandPortSpec parses a port spec that may contain ranges, e.g.
// 8000-8002:8000-8002 or 127.0.0.1:5000-5001:5000-5001/udp, into single
// mappings.
func ExpandPortSpec(spec string) ([]PortMapping, error) {
	s := strings.TrimSpace(spec)
	if !strings.Contains(s, "-") {
		p, err := ParsePortSpec(s)
		if err != nil {
			return nil, err
		}
		return []PortMapping{p}, nil
	}
	base, proto, _ := strings.Cut(s, "/")
	parts := strings.Split(base, ":")
	var ip, host, ctr string
	switch len(parts) {
	case 1:
		ctr = parts[0]
	case 2:
		host, ctr = parts[0], parts[1]
	case 3:
		ip, host, ctr = parts[0], parts[1], parts[2]
	default:
		return nil, fmt.Errorf("%w: port spec %q is not supported", ErrInvalid, spec)
	}
	cFrom, cTo, err := portRange(ctr)
	if err != nil {
		return nil, fmt.Errorf("%w: port spec %q: %v", ErrInvalid, spec, err)
	}
	hFrom, hTo := 0, 0
	if host != "" {
		if hFrom, hTo, err = portRange(host); err != nil {
			return nil, fmt.Errorf("%w: port spec %q: %v", ErrInvalid, spec, err)
		}
	}
	n := cTo - cFrom + 1
	switch {
	case n > MaxPortRange:
		return nil, fmt.Errorf("%w: port range %q has more than %d ports", ErrInvalid, spec, MaxPortRange)
	case host != "" && hTo-hFrom+1 != n && hFrom != hTo:
		return nil, fmt.Errorf("%w: port spec %q: the host and container ranges differ in size", ErrInvalid, spec)
	case host != "" && hFrom == hTo && n > 1:
		return nil, fmt.Errorf("%w: port spec %q: a range of container ports needs a host port range", ErrInvalid, spec)
	}
	var out []PortMapping
	for k := 0; k < n; k++ {
		one := strconv.Itoa(cFrom + k)
		if host != "" {
			one = strconv.Itoa(hFrom+k) + ":" + one
			if ip != "" {
				one = ip + ":" + one
			}
		}
		if proto != "" {
			one += "/" + proto
		}
		p, err := ParsePortSpec(one)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func portRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, "-")
	from, err := parsePort(a)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return int(from), int(from), nil
	}
	to, err := parsePort(b)
	if err != nil {
		return 0, 0, err
	}
	if to < from {
		return 0, 0, fmt.Errorf("range %s ends before it starts", s)
	}
	return int(from), int(to), nil
}

// shellCommands splits text like a POSIX shell into commands (separated
// by newlines, ;, &&, ||, | and &) of words. Line continuations of sh
// (\), PowerShell (`) and cmd (^) are joined. A backslash only escapes
// characters a shell treats specially, so Windows paths survive.
func shellCommands(text string) ([][]string, error) {
	return shellSplit(text, true)
}

// SplitWords splits a command line such as a compose command: string into
// words, honoring quotes; ;, &, | and # have no special meaning.
func SplitWords(text string) ([]string, error) {
	cmds, err := shellSplit(text, false)
	if err != nil || len(cmds) == 0 {
		return nil, err
	}
	return cmds[0], nil
}

func shellSplit(text string, commands bool) ([][]string, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if commands {
		// Line continuations, also with trailing blanks after them.
		text = continuationRe.ReplaceAllString(text, " ")
	}

	var cmds [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			// A line that starts with a flag continues a command whose
			// line continuation was forgotten.
			if commands && len(cmds) > 0 && strings.HasPrefix(words[0], "-") {
				cmds[len(cmds)-1] = append(cmds[len(cmds)-1], words...)
			} else {
				cmds = append(cmds, words)
			}
		}
		words = nil
	}
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs) && strings.ContainsRune(" \t\"'\\$`;&|<>()!#*?", rs[i+1]):
			cur.WriteRune(rs[i+1])
			i++
			inWord = true
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("%w: unterminated ' quote", ErrInvalid)
			}
			cur.WriteString(string(rs[i+1 : j]))
			i, inWord = j, true
		case r == '"':
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[j+1]) {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("%w: unterminated \" quote", ErrInvalid)
			}
			i, inWord = j, true
		case commands && r == '#' && !inWord:
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case r == ' ' || r == '\t' || !commands && (r == '\n' || r == '\r'):
			endWord()
		case commands && (r == '\n' || r == ';' || r == '&' || r == '|'):
			endCmd()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	endCmd()
	// A copied prompt: "$ docker run …".
	for i, c := range cmds {
		if commands && len(c) > 1 && (c[0] == "$" || c[0] == ">" || c[0] == "PS>") {
			cmds[i] = c[1:]
		}
	}
	return cmds, nil
}

var continuationRe = regexp.MustCompile("[\\\\`^][ \t]*\n")
