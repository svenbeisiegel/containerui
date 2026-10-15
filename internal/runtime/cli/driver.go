// Package cli implements runtime.Runtime on top of a docker-compatible
// container CLI: the WSL container CLI (wslc.exe), docker or podman. The
// differences between them are described by a Flavor.
//
// For WSL the CLI is used instead of the wslcsdk.dll C API because the SDK
// (3.0.x) cannot list containers, volumes or networks and cannot attach to
// the CLI's default session, which is where the user's existing containers
// live.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func init() {
	for _, f := range Flavors {
		runtime.Register(f.Name, func(cfg config.Runtime) (runtime.Runtime, error) {
			return New(f, cfg)
		})
	}
	runtime.Register("auto", Detect)
}

type Driver struct {
	flavor         *Flavor
	runner         Runner
	path           string // resolved executable, for logging
	session        string
	commandTimeout time.Duration
	pullTimeout    time.Duration
	composeTimeout time.Duration
	compose        *composeTool // nil: no compose tool found
}

var _ runtime.Runtime = (*Driver)(nil)

// New creates a driver for flavor f using the executable at cfg.Path, or
// the flavor's default binary on PATH.
func New(f *Flavor, cfg config.Runtime) (*Driver, error) {
	path, err := lookPath(f, cfg.Path)
	if err != nil {
		return nil, err
	}
	d := NewWithRunner(execRunner{path: path}, f, cfg)
	d.path = path
	d.compose = findCompose(f, cfg, execRunner{path: path}, func(bin string) (Runner, bool) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return nil, false
		}
		return execRunner{path: p}, true
	})
	return d, nil
}

func lookPath(f *Flavor, path string) (string, error) {
	if path == "" {
		path = f.Binary
	}
	p, err := exec.LookPath(path)
	if err != nil {
		hint := ""
		if f == WSLC {
			hint = " (requires WSL 2.9.3 or newer)"
		}
		return "", fmt.Errorf("%s: cannot find %q%s: %w", f.Name, path, hint, err)
	}
	return p, nil
}

// NewWithRunner creates a driver with a custom Runner (used by tests).
func NewWithRunner(r Runner, f *Flavor, cfg config.Runtime) *Driver {
	d := &Driver{flavor: f, runner: r, commandTimeout: cfg.CommandTimeout, pullTimeout: cfg.PullTimeout, composeTimeout: cfg.ComposeTimeout}
	if f.session {
		d.session = cfg.Session
	}
	if d.commandTimeout == 0 {
		d.commandTimeout = 30 * time.Second
	}
	if d.pullTimeout == 0 {
		d.pullTimeout = 30 * time.Minute
	}
	if d.composeTimeout == 0 {
		d.composeTimeout = 30 * time.Minute
	}
	return d
}

func (d *Driver) Name() string { return d.flavor.Name }

// Path is the executable the driver runs ("" for a custom Runner).
func (d *Driver) Path() string { return d.path }

func (d *Driver) Capabilities() runtime.Capabilities {
	c := runtime.Capabilities{Restart: true, Inspect: true, Pull: true, Tag: true, Volumes: true, Networks: true, Terminal: true, Logs: true,
		VolumeDrivers: d.flavor.volumeDrivers, PruneAnonymous: d.flavor.pruneAnonymous,
		NetworkDrivers: d.flavor.networkDrivers, NetworkIPv6: d.flavor.networkIPv6}
	if d.compose != nil {
		c.Compose, c.ComposeVersion = true, d.compose.name+" "+d.compose.version
	}
	return c
}

// args prefixes global options (they must precede the command).
func (d *Driver) args(a ...string) []string {
	if d.session == "" {
		return a
	}
	return append([]string{"--session", d.session}, a...)
}

// run executes a CLI command and returns stdout, mapping failures to
// *runtime.CommandError. op names the command for error messages.
func (d *Driver) run(ctx context.Context, op string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.commandTimeout)
	defer cancel()
	stdout, stderr, code, err := d.runner.Run(ctx, d.args(args...))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", d.flavor.Name, op, err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s %s: %w", d.flavor.Name, op, ctx.Err())
	}
	if code != 0 {
		return stdout, d.commandError(op, code, stdout, stderr)
	}
	return stdout, nil
}

func (d *Driver) Info(ctx context.Context) (runtime.Info, error) {
	out, err := d.run(ctx, "version", d.flavor.versionArgs...)
	if err != nil {
		return runtime.Info{Driver: d.Name()}, err
	}
	v := strings.TrimSpace(string(normalize(out)))
	v = strings.TrimSpace(strings.TrimPrefix(v, "wslc"))
	return runtime.Info{Driver: d.Name(), Version: v}, nil
}

func (d *Driver) ListContainers(ctx context.Context, opts runtime.ListContainersOpts) ([]runtime.Container, error) {
	args := []string{"container", d.flavor.verb("list"), "--format", "json", "--no-trunc"}
	if opts.All {
		args = append(args, "--all")
	}
	out, err := d.run(ctx, "container list", args...)
	if err != nil {
		return nil, err
	}
	if d.flavor.podmanJSON {
		return parsePodmanContainers(out)
	}
	return parseContainers(out)
}

func (d *Driver) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	if err := runtime.ValidateContainerID(id); err != nil {
		return nil, err
	}
	out, err := d.run(ctx, "container inspect", "container", "inspect", id)
	var list []json.RawMessage
	if jerr := json.Unmarshal(normalize(out), &list); jerr == nil && len(list) == 0 {
		// The CLIs print "[]" and exit non-zero for unknown IDs (wslc
		// without an error code).
		return nil, &runtime.CommandError{Op: "container inspect", Output: "container " + id + " not found", Kind: runtime.ErrNotFound}
	}
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s container inspect: unexpected output", d.flavor.Name)
	}
	return list[0], nil
}

func (d *Driver) CreateContainer(ctx context.Context, opts runtime.CreateContainerOpts) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	args := []string{"container", "create"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	for _, p := range opts.Ports {
		args = append(args, "-p", p.PortSpec())
	}
	for _, v := range opts.Volumes {
		args = append(args, "-v", v.String())
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.Pull != "" {
		args = append(args, "--pull", string(opts.Pull))
	}
	// Long flag forms only: wslc uses -h for --hostname, not for help.
	if opts.TTY {
		args = append(args, "--tty")
	}
	if opts.Interactive {
		args = append(args, "--interactive")
	}
	if opts.AutoRemove {
		args = append(args, "--rm")
	}
	if opts.NoHealthcheck {
		args = append(args, "--no-healthcheck")
	}
	for _, kv := range []struct{ flag, val string }{
		{"--cpus", opts.CPUs}, {"--memory", opts.Memory}, {"--hostname", opts.Hostname}, {"--user", opts.User},
	} {
		if kv.val != "" {
			args = append(args, kv.flag, kv.val)
		}
	}
	for _, multi := range []struct {
		flag string
		vals []string
	}{
		{"--dns", opts.DNS}, {"--dns-search", opts.DNSSearch}, {"--dns-option", opts.DNSOptions}, {"--ulimit", opts.Ulimits},
	} {
		for _, v := range multi.vals {
			args = append(args, multi.flag, v)
		}
	}
	// Everything after the image is passed to the container verbatim.
	args = append(args, opts.Image)
	args = append(args, opts.Cmd...)

	// Creating may implicitly pull the image, so allow the pull timeout.
	ctx, cancel := context.WithTimeout(ctx, d.pullTimeout)
	defer cancel()
	stdout, stderr, code, err := d.runner.Run(ctx, d.args(args...))
	if err != nil {
		return "", fmt.Errorf("%s container create: %w", d.flavor.Name, err)
	}
	if code != 0 {
		return "", d.commandError("container create", code, stdout, stderr)
	}
	id := lastLine(stdout)
	if id == "" {
		id = opts.Name
	}
	if opts.Start {
		if err := d.StartContainer(ctx, id); err != nil {
			return id, fmt.Errorf("container created but not started: %w", err)
		}
	}
	return id, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(normalize(b))), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (d *Driver) StartContainer(ctx context.Context, id string) error {
	return d.containerOp(ctx, "start", id)
}

func (d *Driver) StopContainer(ctx context.Context, id string, timeout *time.Duration) error {
	return d.containerOp(ctx, "stop", id, timeoutArgs(timeout)...)
}

func (d *Driver) RestartContainer(ctx context.Context, id string, timeout *time.Duration) error {
	return d.containerOp(ctx, "restart", id, timeoutArgs(timeout)...)
}

func (d *Driver) RemoveContainer(ctx context.Context, id string, force bool) error {
	var flags []string
	if force {
		flags = append(flags, "--force")
	}
	return d.containerOp(ctx, "remove", id, flags...)
}

func timeoutArgs(t *time.Duration) []string {
	if t == nil {
		return nil
	}
	return []string{"-t", strconv.Itoa(int(t.Seconds()))}
}

func (d *Driver) containerOp(ctx context.Context, verb, id string, flags ...string) error {
	if err := runtime.ValidateContainerID(id); err != nil {
		return err
	}
	args := append([]string{"container", d.flavor.verb(verb)}, flags...)
	args = append(args, id)
	_, err := d.run(ctx, "container "+verb, args...)
	return err
}

// shellProbe prints the path of the best available shell. It runs under
// /bin/sh, so a container without /bin/sh has no usable shell either.
const shellProbe = "command -v bash || command -v sh"

func (d *Driver) CheckTerminal(ctx context.Context, id string) (string, error) {
	if err := runtime.ValidateContainerID(id); err != nil {
		return "", err
	}
	out, err := d.run(ctx, "terminal check", "container", "exec", id, "/bin/sh", "-c", shellProbe)
	var ce *runtime.CommandError
	if errors.As(err, &ce) && (strings.HasPrefix(ce.Code, "WSLC_E_CONTAINER") ||
		(ce.Code == "" && (errors.Is(err, runtime.ErrNotFound) || errors.Is(err, runtime.ErrConflict)))) {
		// The container itself is the problem (not found, not running).
		return "", err
	}
	shell := ""
	if err == nil {
		shell = lastLine(out)
	}
	if runtime.ValidateShellPath(shell) != nil {
		noShell := &runtime.CommandError{Op: "terminal check", Output: "No shell (bash or sh) found in the container.", Kind: runtime.ErrUnsupported}
		if ce != nil {
			noShell.Code, noShell.ExitCode = ce.Code, ce.ExitCode
		}
		return "", noShell
	}
	return shell, nil
}

func (d *Driver) ContainerLogs(ctx context.Context, id string, opts runtime.LogsOpts, w io.Writer) error {
	if err := runtime.ValidateContainerID(id); err != nil {
		return err
	}
	args := []string{"container", "logs"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	args = append(args, id)
	// The CLI's own error message arrives on the same stream as the logs;
	// keep the end of the output to report it.
	tw := &tailWriter{w: w}
	code, err := d.runner.Pipe(ctx, d.args(args...), tw)
	if err != nil {
		return fmt.Errorf("%s container logs: %w", d.flavor.Name, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s container logs: %w", d.flavor.Name, ctx.Err())
	}
	if code != 0 {
		return d.commandError("container logs", code, nil, tw.tail)
	}
	if tw.err != nil {
		return tw.err
	}
	return nil
}

// tailLimit is how much of the end of the log output tailWriter keeps.
const tailLimit = 4 << 10

// tailWriter forwards to w and keeps the last tailLimit bytes. After a write
// to w fails it discards further output (so the process can finish) and
// remembers the error.
type tailWriter struct {
	w    io.Writer
	tail []byte
	err  error
}

func (t *tailWriter) Write(p []byte) (int, error) {
	if t.err == nil {
		_, t.err = t.w.Write(p)
	}
	t.tail = append(t.tail, p...)
	if over := len(t.tail) - tailLimit; over > 0 {
		t.tail = append(t.tail[:0], t.tail[over:]...)
	}
	return len(p), nil
}

func (d *Driver) ListImages(ctx context.Context) ([]runtime.Image, error) {
	out, err := d.run(ctx, "image list", "image", d.flavor.verb("list"), "--format", "json", "--no-trunc")
	if err != nil {
		return nil, err
	}
	if d.flavor.podmanJSON {
		return parsePodmanImages(out)
	}
	return parseImages(out)
}

func (d *Driver) PullImage(ctx context.Context, ref string, progress func(string)) error {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	ctx, cancel := context.WithTimeout(ctx, d.pullTimeout)
	defer cancel()
	var stdout bytes.Buffer
	stderr, code, err := d.runner.Stream(ctx, d.args("image", "pull", ref), func(line string) {
		stdout.WriteString(line + "\n")
		progress(line)
	})
	if err != nil {
		return fmt.Errorf("%s image pull: %w", d.flavor.Name, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s image pull: %w", d.flavor.Name, ctx.Err())
	}
	if code != 0 {
		return d.commandError("image pull", code, stdout.Bytes(), stderr)
	}
	return nil
}

func (d *Driver) RemoveImage(ctx context.Context, ref string, force bool) error {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return err
	}
	args := []string{"image", d.flavor.verb("remove")}
	if force {
		args = append(args, "--force")
	}
	_, err := d.run(ctx, "image remove", append(args, ref)...)
	return err
}

func (d *Driver) TagImage(ctx context.Context, source, target string) error {
	if err := errors.Join(runtime.ValidateImageRef(source), runtime.ValidateImageRef(target)); err != nil {
		return err
	}
	_, err := d.run(ctx, "image tag", "image", "tag", source, target)
	return err
}

func (d *Driver) ListVolumes(ctx context.Context) ([]runtime.Volume, error) {
	out, err := d.run(ctx, "volume list", "volume", d.flavor.verb("list"), "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseVolumes(out)
}

func (d *Driver) InspectVolume(ctx context.Context, name string) (json.RawMessage, error) {
	if err := runtime.ValidateVolumeName(name); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "volume", name)
}

func (d *Driver) InspectImage(ctx context.Context, ref string) (json.RawMessage, error) {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "image", ref)
}

// inspect runs "<kind> inspect name" and returns the single document.
func (d *Driver) inspect(ctx context.Context, kind, name string) (json.RawMessage, error) {
	op := kind + " inspect"
	out, err := d.run(ctx, op, kind, "inspect", name)
	var list []json.RawMessage
	if jerr := json.Unmarshal(normalize(out), &list); jerr == nil && len(list) == 0 {
		// Like container inspect: "[]" and a non-zero exit.
		return nil, &runtime.CommandError{Op: op, Output: kind + " " + name + " not found", Kind: runtime.ErrNotFound}
	}
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s %s: unexpected output", d.flavor.Name, op)
	}
	return list[0], nil
}

func (d *Driver) CreateVolume(ctx context.Context, opts runtime.CreateVolumeOpts) (string, error) {
	if err := opts.Validate(d.flavor.volumeDrivers); err != nil {
		return "", err
	}
	args := []string{"volume", "create"}
	if opts.Driver != "" {
		args = append(args, "--driver", opts.Driver)
	}
	for _, o := range opts.Options {
		args = append(args, "--opt", o)
	}
	for _, l := range opts.Labels {
		args = append(args, "--label", l)
	}
	if opts.Name != "" {
		args = append(args, opts.Name)
	}
	out, err := d.run(ctx, "volume create", args...)
	if err != nil {
		return "", err
	}
	if name := lastLine(out); name != "" {
		return name, nil
	}
	return opts.Name, nil
}

func (d *Driver) RemoveVolume(ctx context.Context, name string, force bool) error {
	if err := runtime.ValidateVolumeName(name); err != nil {
		return err
	}
	args := []string{"volume", d.flavor.verb("remove")}
	if force {
		args = append(args, "--force")
	}
	_, err := d.run(ctx, "volume remove", append(args, name)...)
	return err
}

func (d *Driver) PruneVolumes(ctx context.Context, all bool) (string, error) {
	// --force skips the interactive confirmation.
	args := []string{"volume", "prune", "--force"}
	if all && d.flavor.pruneAnonymous {
		args = append(args, "--all")
	}
	out, err := d.run(ctx, "volume prune", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(normalize(out))), nil
}

func (d *Driver) ListNetworks(ctx context.Context) ([]runtime.Network, error) {
	out, err := d.run(ctx, "network list", "network", d.flavor.verb("list"), "--format", "json", "--no-trunc")
	if err != nil {
		return nil, err
	}
	if d.flavor.podmanJSON {
		return parsePodmanNetworks(out)
	}
	return parseNetworks(out)
}
