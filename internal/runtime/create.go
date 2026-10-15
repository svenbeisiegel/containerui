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
	Network string // optional
	Pull    PullPolicy
	Start   bool // start the container right after creating it

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
	if o.Network != "" && !netNameRe.MatchString(o.Network) {
		errs = append(errs, fmt.Errorf("%w: invalid network name %q", ErrInvalid, o.Network))
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
