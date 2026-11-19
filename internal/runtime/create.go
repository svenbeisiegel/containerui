package runtime

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// PullPolicy controls whether CreateContainer pulls the image first.
type PullPolicy string

const (
	PullMissing PullPolicy = "missing"
	PullAlways  PullPolicy = "always"
	PullNever   PullPolicy = "never"
)

// LabelPull records the pull policy a container was created with, so
// that recreating it applies the same policy: the runtimes do not keep
// --pull. Labels starting with LabelOwnPrefix are ContainerUI's own.
const (
	LabelPull      = "containerui.pull"
	LabelOwnPrefix = "containerui."
)

// ParsePullPolicy returns the policy s names, ok false if it names none.
func ParsePullPolicy(s string) (PullPolicy, bool) {
	switch p := PullPolicy(s); p {
	case PullMissing, PullAlways, PullNever:
		return p, true
	}
	return "", false
}

// SetPullLabel records policy p (default PullMissing) in o.Labels,
// replacing an earlier record.
func (o *CreateContainerOpts) SetPullLabel(p PullPolicy) {
	if p == "" {
		p = PullMissing
	}
	o.Labels = slices.DeleteFunc(slices.Clone(o.Labels), func(l string) bool { return strings.HasPrefix(l, LabelPull+"=") })
	o.Labels = append(o.Labels, LabelPull+"="+string(p))
}

// VolumeMount binds a host path or named volume into the container.
type VolumeMount struct {
	Source   string // host path (e.g. C:\data) or volume name
	Target   string // absolute path inside the container
	ReadOnly bool
}

func (v VolumeMount) String() string {
	s := v.Source + ":" + v.Target
	if v.ReadOnly {
		s += ":ro"
	}
	return s
}

type CreateContainerOpts struct {
	Image   string
	Name    string   // optional
	Cmd     []string // optional command + arguments
	Env     []string // KEY=VALUE
	Ports   []PortMapping
	Volumes []VolumeMount
	// Networks the container joins; the first one at creation (host and
	// none only alone), the others are connected before it starts. Empty
	// means the runtime's default network.
	Networks []NetworkAttachment
	Pull     PullPolicy
	Start    bool // start the container right after creating it

	// Advanced options. Zero values mean "not passed to the runtime".
	TTY           bool // allocate a pseudo-TTY
	Interactive   bool // keep STDIN open
	AutoRemove    bool // remove the container when it exits
	NoHealthcheck bool // disable the image's health check

	CPUs       string   // decimal number of CPUs, e.g. "1.5"
	Memory     string   // memory limit: <number>[b|k|m|g], e.g. "512M"
	Hostname   string   // container hostname
	User       string   // name | uid | uid:gid
	DNS        []string // nameserver IP addresses
	DNSSearch  []string // DNS search domains
	DNSOptions []string // resolver options, e.g. ndots:2
	Ulimits    []string // name=soft[:hard], -1 = unlimited

	// Options not every runtime supports (see Capabilities.CreateOptions
	// and UsedOptions).
	RestartPolicy string   // no | always | unless-stopped | on-failure[:N]
	Labels        []string // key=value
	Entrypoint    []string // nil = the image's; empty = none
	WorkDir       string   // absolute working directory
	ExtraHosts    []string // host:ip entries for /etc/hosts
	CapAdd        []string // Linux capabilities to add, e.g. NET_ADMIN
	CapDrop       []string // Linux capabilities to drop
	Privileged    bool
}

// NetworkAttachment is a network a new container joins.
type NetworkAttachment struct {
	Name    string
	Aliases []string // extra DNS names in this network
	IP      string   // fixed IPv4 address
}

// Optional create options, keys of Capabilities.CreateOptions.
const (
	OptRestart      = "restart"
	OptLabel        = "label"
	OptEntrypoint   = "entrypoint"
	OptWorkDir      = "workdir"
	OptAddHost      = "add-host"
	OptCapAdd       = "cap-add"
	OptCapDrop      = "cap-drop"
	OptPrivileged   = "privileged"
	OptNetworkAlias = "network-alias" // aliases or IP on the first network
	OptMultiNetwork = "multi-network" // more than one network
)

// AllCreateOptions lists every optional create option.
var AllCreateOptions = []string{OptRestart, OptLabel, OptEntrypoint, OptWorkDir, OptAddHost, OptCapAdd, OptCapDrop, OptPrivileged, OptNetworkAlias, OptMultiNetwork}

// OptionLabels names the optional create options for messages.
var OptionLabels = map[string]string{
	OptRestart: "restart policy", OptLabel: "labels", OptEntrypoint: "entrypoint", OptWorkDir: "working directory",
	OptAddHost: "extra hosts", OptCapAdd: "added capabilities", OptCapDrop: "dropped capabilities",
	OptPrivileged: "privileged mode", OptNetworkAlias: "network aliases and fixed IPs", OptMultiNetwork: "additional networks",
}

// UsedOptions returns the optional create options o sets.
func (o *CreateContainerOpts) UsedOptions() []string {
	var out []string
	add := func(used bool, opt string) {
		if used {
			out = append(out, opt)
		}
	}
	add(o.RestartPolicy != "" && o.RestartPolicy != "no", OptRestart)
	add(len(o.Labels) > 0, OptLabel)
	add(o.Entrypoint != nil, OptEntrypoint)
	add(o.WorkDir != "", OptWorkDir)
	add(len(o.ExtraHosts) > 0, OptAddHost)
	add(len(o.CapAdd) > 0, OptCapAdd)
	add(len(o.CapDrop) > 0, OptCapDrop)
	add(o.Privileged, OptPrivileged)
	add(len(o.Networks) > 0 && (len(o.Networks[0].Aliases) > 0 || o.Networks[0].IP != ""), OptNetworkAlias)
	add(len(o.Networks) > 1, OptMultiNetwork)
	return out
}

// Network returns the name of the first network, or "".
func (o *CreateContainerOpts) Network() string {
	if len(o.Networks) == 0 {
		return ""
	}
	return o.Networks[0].Name
}

var (
	containerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// Permissive OCI reference check: registry[:port]/path[:tag][@digest] or
	// a hex image ID. It deliberately forbids a leading '-' and whitespace.
	imageRefRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/:@]{0,511}$`)
	envKeyRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	netNameRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

	// Compose's own rule for project names.
	projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

	volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	volumeKeyRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./-]*$`)

	cpusRe      = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	memoryRe    = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([bkmgBKMG]?)$`)
	dnsLabelRe  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	userRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,31})?$`)
	dnsOptionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(:[0-9]+)?$`)
	ulimitRe    = regexp.MustCompile(`^([a-z]+)=(-1|[0-9]+)(?::(-1|[0-9]+))?$`)
	restartRe   = regexp.MustCompile(`^(no|always|unless-stopped|on-failure(:[0-9]+)?)$`)
	capRe       = regexp.MustCompile(`^[A-Za-z][A-Za-z_]*$`)
)

// knownUlimits are the resource names accepted by --ulimit.
var knownUlimits = map[string]bool{
	"core": true, "cpu": true, "data": true, "fsize": true, "locks": true,
	"memlock": true, "msgqueue": true, "nice": true, "nofile": true, "nproc": true,
	"rss": true, "rtprio": true, "rttime": true, "sigpending": true, "stack": true,
}

// validDomain checks an RFC 1123 host or domain name.
func validDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !dnsLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}

// validateUlimit checks name=soft[:hard]; -1 means unlimited.
func validateUlimit(u string) error {
	m := ulimitRe.FindStringSubmatch(u)
	if m == nil {
		return fmt.Errorf("%w: ulimit %q must be name=soft[:hard]", ErrInvalid, u)
	}
	if !knownUlimits[m[1]] {
		return fmt.Errorf("%w: unknown ulimit %q", ErrInvalid, m[1])
	}
	if m[3] != "" && m[3] != "-1" {
		hard, _ := strconv.ParseInt(m[3], 10, 64)
		soft, _ := strconv.ParseInt(m[2], 10, 64)
		if m[2] == "-1" || soft > hard {
			return fmt.Errorf("%w: ulimit %q: soft limit exceeds hard limit", ErrInvalid, u)
		}
	}
	return nil
}

func ValidateContainerName(name string) error {
	if !containerNameRe.MatchString(name) {
		return fmt.Errorf("%w: container name %q must match %s", ErrInvalid, name, containerNameRe)
	}
	return nil
}

// ValidateContainerID accepts a container name, full ID or ID prefix.
func ValidateContainerID(id string) error {
	if !containerNameRe.MatchString(id) {
		return fmt.Errorf("%w: invalid container id %q", ErrInvalid, id)
	}
	return nil
}

// ValidateProjectName checks a compose project name.
func ValidateProjectName(name string) error {
	if !projectNameRe.MatchString(name) {
		return fmt.Errorf("%w: project name %q must be lowercase letters, digits, '-' and '_', starting with a letter or digit", ErrInvalid, name)
	}
	return nil
}

// ValidateVolumeName accepts named and anonymous (hex) volume names.
func ValidateVolumeName(name string) error {
	if !volumeNameRe.MatchString(name) {
		return fmt.Errorf("%w: volume name %q must match %s", ErrInvalid, name, volumeNameRe)
	}
	return nil
}

type CreateVolumeOpts struct {
	Name    string   // optional; the runtime generates one if empty
	Driver  string   // optional; one of Capabilities.VolumeDrivers
	Options []string // driver options, key=value
	Labels  []string // metadata, key=value
}

// Validate checks the options; drivers are the volume drivers the runtime
// accepts (Capabilities.VolumeDrivers).
func (o *CreateVolumeOpts) Validate(drivers []string) error {
	var errs []error
	if o.Name != "" {
		if err := ValidateVolumeName(o.Name); err != nil {
			errs = append(errs, err)
		}
	}
	if o.Driver != "" && !slices.Contains(drivers, o.Driver) {
		errs = append(errs, fmt.Errorf("%w: volume driver %q must be one of %s", ErrInvalid, o.Driver, strings.Join(drivers, ", ")))
	}
	errs = append(errs, validateKeyValues("volume", "option", o.Options), validateKeyValues("volume", "label", o.Labels))
	return errors.Join(errs...)
}

func ValidateImageRef(ref string) error {
	if !imageRefRe.MatchString(ref) || strings.Contains(ref, "//") || strings.HasSuffix(ref, ":") {
		return fmt.Errorf("%w: invalid image reference %q", ErrInvalid, ref)
	}
	return nil
}

func (o *CreateContainerOpts) Validate() error {
	var errs []error
	if err := ValidateImageRef(o.Image); err != nil {
		errs = append(errs, err)
	}
	if o.Name != "" {
		if err := ValidateContainerName(o.Name); err != nil {
			errs = append(errs, err)
		}
	}
	for _, e := range o.Env {
		key, _, ok := strings.Cut(e, "=")
		if !ok || !envKeyRe.MatchString(key) {
			errs = append(errs, fmt.Errorf("%w: environment variable %q must be KEY=VALUE", ErrInvalid, e))
		}
	}
	for _, p := range o.Ports {
		if p.ContainerPort == 0 {
			errs = append(errs, fmt.Errorf("%w: container port is required", ErrInvalid))
		}
		if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
			errs = append(errs, fmt.Errorf("%w: protocol %q must be tcp or udp", ErrInvalid, p.Protocol))
		}
		if p.HostIP != "" && net.ParseIP(p.HostIP) == nil {
			errs = append(errs, fmt.Errorf("%w: host IP %q is not an IP address", ErrInvalid, p.HostIP))
		}
	}
	for _, v := range o.Volumes {
		if v.Source == "" || strings.HasPrefix(v.Source, "-") {
			errs = append(errs, fmt.Errorf("%w: volume source %q is invalid", ErrInvalid, v.Source))
		}
		if !strings.HasPrefix(v.Target, "/") {
			errs = append(errs, fmt.Errorf("%w: volume target %q must be an absolute container path", ErrInvalid, v.Target))
		}
	}
	for i, n := range o.Networks {
		if !netNameRe.MatchString(n.Name) {
			errs = append(errs, fmt.Errorf("%w: invalid network name %q", ErrInvalid, n.Name))
		}
		if (n.Name == "host" || n.Name == "none") && len(o.Networks) > 1 {
			errs = append(errs, fmt.Errorf("%w: network %s cannot be combined with other networks", ErrInvalid, n.Name))
		}
		if slices.ContainsFunc(o.Networks[:i], func(m NetworkAttachment) bool { return m.Name == n.Name }) {
			errs = append(errs, fmt.Errorf("%w: network %s is listed twice", ErrInvalid, n.Name))
		}
		ep := ConnectNetworkOpts{Aliases: n.Aliases, IP: n.IP}
		if err := ep.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if o.RestartPolicy != "" && !restartRe.MatchString(o.RestartPolicy) {
		errs = append(errs, fmt.Errorf("%w: restart policy %q must be no, always, unless-stopped or on-failure[:N]", ErrInvalid, o.RestartPolicy))
	}
	errs = append(errs, validateKeyValues("container", "label", o.Labels))
	if len(o.Entrypoint) > 0 && (o.Entrypoint[0] == "" || strings.HasPrefix(o.Entrypoint[0], "-")) {
		errs = append(errs, fmt.Errorf("%w: entrypoint %q is invalid", ErrInvalid, o.Entrypoint[0]))
	}
	if o.WorkDir != "" && !strings.HasPrefix(o.WorkDir, "/") {
		errs = append(errs, fmt.Errorf("%w: working directory %q must be an absolute container path", ErrInvalid, o.WorkDir))
	}
	for _, h := range o.ExtraHosts {
		host, ip, ok := strings.Cut(h, ":")
		if !ok || !validDomain(host) || (ip != "host-gateway" && net.ParseIP(ip) == nil) {
			errs = append(errs, fmt.Errorf("%w: extra host %q must be host:ip", ErrInvalid, h))
		}
	}
	for _, c := range slices.Concat(o.CapAdd, o.CapDrop) {
		if !capRe.MatchString(c) {
			errs = append(errs, fmt.Errorf("%w: capability %q must be a name like NET_ADMIN or ALL", ErrInvalid, c))
		}
	}
	switch o.Pull {
	case "", PullMissing, PullAlways, PullNever:
	default:
		errs = append(errs, fmt.Errorf("%w: invalid pull policy %q", ErrInvalid, o.Pull))
	}
	if o.CPUs != "" {
		if n, err := strconv.ParseFloat(o.CPUs, 64); !cpusRe.MatchString(o.CPUs) || err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%w: CPUs %q must be a number greater than 0, e.g. 1.5", ErrInvalid, o.CPUs))
		}
	}
	if o.Memory != "" {
		m := memoryRe.FindStringSubmatch(o.Memory)
		if n, err := strconv.ParseFloat(firstGroup(m), 64); m == nil || err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%w: memory %q must be a size like 512M or 1G", ErrInvalid, o.Memory))
		}
	}
	if o.Hostname != "" && !validDomain(o.Hostname) {
		errs = append(errs, fmt.Errorf("%w: hostname %q is not a valid host name", ErrInvalid, o.Hostname))
	}
	if o.User != "" && !userRe.MatchString(o.User) {
		errs = append(errs, fmt.Errorf("%w: user %q must be name, uid or uid:gid", ErrInvalid, o.User))
	}
	for _, d := range o.DNS {
		if net.ParseIP(d) == nil {
			errs = append(errs, fmt.Errorf("%w: DNS server %q is not an IP address", ErrInvalid, d))
		}
	}
	for _, d := range o.DNSSearch {
		if !validDomain(d) {
			errs = append(errs, fmt.Errorf("%w: DNS search domain %q is not a valid domain", ErrInvalid, d))
		}
	}
	for _, d := range o.DNSOptions {
		if !dnsOptionRe.MatchString(d) {
			errs = append(errs, fmt.Errorf("%w: DNS option %q must look like ndots:2 or rotate", ErrInvalid, d))
		}
	}
	for _, u := range o.Ulimits {
		if err := validateUlimit(u); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func firstGroup(m []string) string {
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ParsePortSpec parses docker-style port specs:
//
//	80            container port only (runtime picks the host port)
//	8080:80       host port -> container port
//	127.0.0.1:8080:80/udp
func ParsePortSpec(spec string) (PortMapping, error) {
	var pm PortMapping
	s := strings.TrimSpace(spec)
	if s == "" {
		return pm, fmt.Errorf("%w: empty port spec", ErrInvalid)
	}
	if base, proto, ok := strings.Cut(s, "/"); ok {
		pm.Protocol = strings.ToLower(proto)
		s = base
	}
	parts := strings.Split(s, ":")
	var hostPort, ctrPort string
	switch len(parts) {
	case 1:
		ctrPort = parts[0]
	case 2:
		hostPort, ctrPort = parts[0], parts[1]
	case 3:
		pm.HostIP, hostPort, ctrPort = parts[0], parts[1], parts[2]
	default:
		return pm, fmt.Errorf("%w: port spec %q (IPv6 host IPs are not supported here)", ErrInvalid, spec)
	}
	var err error
	if pm.ContainerPort, err = parsePort(ctrPort); err != nil {
		return pm, fmt.Errorf("%w: port spec %q: %v", ErrInvalid, spec, err)
	}
	if hostPort != "" {
		if pm.HostPort, err = parsePort(hostPort); err != nil {
			return pm, fmt.Errorf("%w: port spec %q: %v", ErrInvalid, spec, err)
		}
	}
	return pm, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(n), nil
}

// ParseVolumeSpec parses "source:/target[:ro|:rw]". The source may be a
// Windows path containing a drive colon (C:\data:/data).
func ParseVolumeSpec(spec string) (VolumeMount, error) {
	var v VolumeMount
	s := strings.TrimSpace(spec)
	if strings.HasSuffix(s, ":ro") {
		v.ReadOnly = true
		s = strings.TrimSuffix(s, ":ro")
	} else {
		s = strings.TrimSuffix(s, ":rw")
	}
	i := strings.LastIndex(s, ":/")
	if i <= 0 {
		return v, fmt.Errorf("%w: volume spec %q must be source:/container/path", ErrInvalid, spec)
	}
	v.Source, v.Target = s[:i], s[i+1:]
	return v, nil
}

// PortSpec renders a mapping in the CLI -p format.
func (p PortMapping) PortSpec() string {
	s := strconv.Itoa(int(p.ContainerPort))
	if p.HostPort != 0 {
		s = strconv.Itoa(int(p.HostPort)) + ":" + s
		if p.HostIP != "" {
			s = p.HostIP + ":" + s
		}
	}
	if p.Protocol != "" {
		s += "/" + p.Protocol
	}
	return s
}
