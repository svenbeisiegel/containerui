package cli

import (
	"context"
	"os"
	goruntime "runtime"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Flavor describes what differs between the container CLIs the driver
// supports. Their commands and flags are otherwise the same (wslc copies
// the docker CLI, podman is docker compatible).
type Flavor struct {
	Name   string
	Binary string // default executable, looked up on PATH

	versionArgs []string
	// verbs renames subcommand verbs ("list", "remove"); nil keeps them.
	verbs map[string]string
	// session reports whether the global --session option exists.
	session bool
	// podmanJSON: list commands print JSON arrays with podman's own field
	// names instead of docker-style NDJSON.
	podmanJSON bool
	// textKind classifies an error by its message when the CLI prints no
	// stable error code (docker and podman messages are English only).
	textKind func(text string) error

	volumeDrivers  []string
	pruneAnonymous bool
	// volumeArchive: "container run --rm -i" pipes stdin and stdout, which
	// volume backup and restore use with a helper container.
	volumeArchive bool

	networkDrivers []string
	networkIPv6    bool
	// aliasFlag is the network connect option for a container alias.
	aliasFlag string

	// createOptions are the optional create options (runtime.Opt*) the
	// CLI's "container create" accepts.
	createOptions map[string]bool

	// stats: "container stats" and "system df" print JSON (not wslc).
	stats bool
	// attach: "container attach" and "container start --attach" with
	// --detach-keys exist, and "container attach" takes --sig-proxy.
	attach bool
	// startSigProxy: "container start --attach" takes --sig-proxy too
	// (podman; docker start has no such flag and proxies no signals with a
	// TTY).
	startSigProxy bool

	// compose lists the compose tools to try, in order. None for wslc,
	// which has no compose.
	compose []composeCandidate
}

var (
	WSLC = &Flavor{
		Name:           "wslc",
		Binary:         "wslc.exe",
		versionArgs:    []string{"version"},
		session:        true,
		volumeDrivers:  []string{"guest", "vhd"},
		pruneAnonymous: true,
		networkDrivers: []string{"bridge"},
		aliasFlag:      "--network-alias",
		// TODO: verify the other create flags with "wslc container create
		// --help" and enable the ones wslc accepts. Additional networks
		// only need "network connect".
		createOptions: map[string]bool{runtime.OptMultiNetwork: true},
		// TODO: verify "wslc container attach" and "wslc container start
		// --attach" (with --sig-proxy and --detach-keys) and enable attach.
		// TODO: verify "wslc container run --rm -i" with piped stdin and
		// stdout and enable volumeArchive.
	}
	Docker = &Flavor{
		Name:   "docker",
		Binary: "docker",
		// Asks the daemon, so it also fails when the daemon is down.
		versionArgs:    []string{"version", "--format", "{{.Server.Version}}"},
		verbs:          map[string]string{"list": "ls", "remove": "rm"},
		textKind:       englishKind,
		volumeDrivers:  []string{"local"},
		pruneAnonymous: true,
		networkDrivers: []string{"bridge", "macvlan", "ipvlan", "overlay"},
		networkIPv6:    true,
		aliasFlag:      "--alias",
		createOptions:  runtime.AllOptions(),
		stats:          true,
		attach:         true,
		volumeArchive:  true,
		compose:        []composeCandidate{{plugin: true}, {binary: "docker-compose"}},
	}
	Podman = &Flavor{
		Name:          "podman",
		Binary:        "podman",
		versionArgs:   []string{"version", "--format", "{{.Client.Version}}"},
		verbs:         map[string]string{"list": "ls", "remove": "rm"},
		podmanJSON:    true,
		textKind:      englishKind,
		volumeDrivers: []string{"local"},
		// podman volume prune has no --all: it always removes named volumes too.
		networkDrivers: []string{"bridge", "macvlan", "ipvlan"},
		networkIPv6:    true,
		aliasFlag:      "--alias",
		createOptions:  runtime.AllOptions(),
		stats:          true,
		attach:         true,
		startSigProxy:  true,
		volumeArchive:  true,
		compose:        []composeCandidate{{plugin: true}, {binary: "podman-compose"}},
	}
)

// Flavors lists the supported CLIs.
var Flavors = []*Flavor{WSLC, Docker, Podman}

// verb maps a subcommand verb to this CLI's spelling.
func (f *Flavor) verb(v string) string {
	if w, ok := f.verbs[v]; ok {
		return w
	}
	return v
}

// englishKind classifies docker and podman error messages.
func englishKind(text string) error {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "no such container"), strings.Contains(t, "no such image"),
		strings.Contains(t, "no such volume"), strings.Contains(t, "no such object"),
		strings.Contains(t, "no such network"), strings.Contains(t, "image not known"),
		strings.Contains(t, "not found: manifest unknown"), strings.Contains(t, "pull access denied"):
		return runtime.ErrNotFound
	case strings.Contains(t, "conflict"), strings.Contains(t, "already in use"),
		strings.Contains(t, "is in use"), strings.Contains(t, "is not running"),
		strings.Contains(t, "is running"), strings.Contains(t, "already exists"),
		strings.Contains(t, "active endpoints"), strings.Contains(t, "has associated containers"):
		return runtime.ErrConflict
	case strings.Contains(t, "invalid reference format"), strings.Contains(t, "invalid argument"):
		return runtime.ErrInvalid
	}
	return nil
}

// SocketPath returns the Unix socket (Linux, macOS) or named pipe (Windows)
// through which the runtime is reached, or "" if there is none (wslc,
// rootless podman): runtime.socket if set, else the flavor's default. Who
// may use the socket decides who may use the runtime.
func (d *Driver) SocketPath() string {
	if d.socket != "" {
		return d.socket
	}
	return socketPath(d.flavor, goruntime.GOOS, os.Getenv("DOCKER_HOST"), os.Geteuid())
}

// socketEnv points the CLI of flavor f at a configured socket: docker reads
// DOCKER_HOST, podman CONTAINER_HOST. nil keeps the CLI's own choice.
func socketEnv(f *Flavor, socket string) []string {
	if socket == "" {
		return nil
	}
	host := "unix://" + socket
	if strings.HasPrefix(socket, `\\`) {
		host = "npipe://" + strings.ReplaceAll(socket, `\`, "/")
	}
	switch f {
	case Docker:
		return []string{"DOCKER_HOST=" + host}
	case Podman:
		return []string{"CONTAINER_HOST=" + host}
	}
	return nil
}

func socketPath(f *Flavor, goos, dockerHost string, euid int) string {
	if goos == "windows" {
		switch f {
		case Docker:
			if p, ok := strings.CutPrefix(dockerHost, "npipe://"); ok {
				return strings.ReplaceAll(p, "/", `\`)
			}
			return `\\.\pipe\docker_engine`
		case Podman:
			return `\\.\pipe\podman-machine-default`
		}
		return ""
	}
	switch f {
	case Docker:
		if h, ok := strings.CutPrefix(dockerHost, "unix://"); ok {
			return h
		}
		return "/var/run/docker.sock"
	case Podman:
		if euid == 0 {
			return "/run/podman/podman.sock"
		}
	}
	return ""
}

// APISocket returns the Unix socket through which the runtime's API can be
// read (the resource monitoring prefers it to the CLI), or "" if none is
// known: runtime.socket, $DOCKER_HOST, the default socket, or the one the
// CLI itself uses (docker's current context, podman's remote socket).
func (d *Driver) APISocket(ctx context.Context) string {
	if d.socket != "" {
		return d.socket
	}
	if goruntime.GOOS == "windows" {
		return "" // named pipes are not supported by the API client
	}
	host := os.Getenv("DOCKER_HOST")
	switch d.flavor {
	case Docker:
		if host != "" {
			p, _ := strings.CutPrefix(host, "unix://")
			if p == host {
				return "" // tcp:// or ssh://
			}
			return p
		}
	case Podman:
		if h := os.Getenv("CONTAINER_HOST"); h != "" {
			p, _ := strings.CutPrefix(h, "unix://")
			if p == h {
				return ""
			}
			return p
		}
	default:
		return ""
	}
	if p := d.SocketPath(); p != "" && exists(p) {
		return p
	}
	args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
	if d.flavor == Podman {
		args = []string{"info", "--format", "{{.Host.RemoteSocket.Path}}"}
	}
	out, err := d.run(ctx, "socket lookup", args...)
	if err != nil {
		return ""
	}
	p, _ := strings.CutPrefix(strings.TrimSpace(string(normalize(out))), "unix://")
	if p == "" || !strings.HasPrefix(p, "/") || !exists(p) {
		return ""
	}
	return p
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
