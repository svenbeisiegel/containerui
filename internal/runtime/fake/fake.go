// Package fake provides an in-memory runtime.Runtime for tests and for
// developing the UI without a real container runtime (driver: fake).
package fake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func init() {
	runtime.Register("fake", func(config.Runtime) (runtime.Runtime, error) { return NewSeeded(), nil })
}

type Runtime struct {
	mu         sync.Mutex
	containers map[string]*runtime.Container // by ID
	images     map[string]*runtime.Image     // by Ref()
	volumes    []runtime.Volume
	networks   []runtime.Network
	env        map[string][]string // container environment by ID
	netSpecs   map[string]*netSpec // by network name

	// Err, if set, is returned by every call (to test error rendering).
	Err error
	// PullDelay slows down PullImage per progress line.
	PullDelay time.Duration
	// Calls records operations, e.g. "stop web".
	Calls []string
	// LastCreate holds the options of the most recent CreateContainer call.
	LastCreate runtime.CreateContainerOpts
	// NoShell makes CheckTerminal report that no shell is available.
	NoShell bool
	// NoNetworks hides network support (Capabilities.Networks).
	NoNetworks bool
	// LogInterval is how often a followed log gets a new line (default 1s).
	LogInterval time.Duration
	// ComposeEnabled reports a compose tool (Capabilities.Compose).
	ComposeEnabled bool
}

var _ runtime.Runtime = (*Runtime)(nil)

func New() *Runtime {
	return &Runtime{containers: map[string]*runtime.Container{}, images: map[string]*runtime.Image{}, env: map[string][]string{}, netSpecs: map[string]*netSpec{}}
}

// NewSeeded returns a fake with a few images, containers, volumes and networks.
func NewSeeded() *Runtime {
	f := New()
	now := time.Now()
	f.AddImage(runtime.Image{ID: "sha256:aaa111", Repository: "alpine", Tag: "latest", Size: "8.3MB", Created: now.Add(-48 * time.Hour)})
	f.AddImage(runtime.Image{ID: "sha256:bbb222", Repository: "nginx", Tag: "1.27", Size: "190MB", Created: now.Add(-72 * time.Hour)})
	f.AddContainer(runtime.Container{ID: "c0ffee000001", Name: "web", Image: "nginx:1.27", State: runtime.StateRunning, Status: "Up 2 hours", Ports: "127.0.0.1:8080->80/tcp", Networks: []string{"bridge"}, Mounts: []string{"data"}, Created: now.Add(-2 * time.Hour)})
	f.AddContainer(runtime.Container{ID: "c0ffee000002", Name: "job", Image: "alpine:latest", State: runtime.StateExited, Status: "Exited (0) 1 hour ago", Networks: []string{"bridge"}, Created: now.Add(-3 * time.Hour)})
	f.volumes = []runtime.Volume{volume("data", "guest"), volume("cache", "guest")}
	f.networks = []runtime.Network{
		{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local"},
		{ID: "n2", Name: "host", Driver: "host", Scope: "local"},
		{ID: "n3", Name: "none", Driver: "null", Scope: "local"},
	}
	f.networks = append(f.networks, runtime.Network{ID: "n5", Name: "backend", Driver: "bridge", Scope: "local", Created: now.Add(-24 * time.Hour),
		Labels: map[string]string{"team": "platform"}})
	f.spec("backend").Subnet, f.spec("backend").Gateway = "172.30.0.0/16", "172.30.0.1"
	f.seedCompose(now)
	f.ComposeEnabled = true
	return f
}

func (f *Runtime) AddImage(img runtime.Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[img.Ref()] = &img
}

func (f *Runtime) AddContainer(c runtime.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = &c
}

func (f *Runtime) record(format string, args ...any) error {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
	return f.Err
}

func (f *Runtime) Name() string { return "fake" }

func (f *Runtime) Capabilities() runtime.Capabilities {
	c := runtime.Capabilities{Restart: true, Inspect: true, Pull: true, Tag: true, Volumes: true, Networks: true, Terminal: true, Logs: true,
		VolumeDrivers: volumeDrivers, PruneAnonymous: true, NetworkDrivers: networkDrivers, NetworkIPv6: true}
	if f.NoNetworks {
		c.Networks = false
	}
	if f.ComposeEnabled {
		c.Compose, c.ComposeVersion = true, "fake compose 2.0.0"
	}
	return c
}

func (f *Runtime) Info(context.Context) (runtime.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.Info{Driver: "fake", Version: "0.0.0"}, f.Err
}

func (f *Runtime) find(id string) (*runtime.Container, error) {
	if c, ok := f.containers[id]; ok {
		return c, nil
	}
	for _, c := range f.containers {
		if c.Name == id || strings.HasPrefix(c.ID, id) {
			return c, nil
		}
	}
	return nil, &runtime.CommandError{Op: "container", Output: "container " + id + " not found", Kind: runtime.ErrNotFound}
}

func (f *Runtime) ListContainers(_ context.Context, opts runtime.ListContainersOpts) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	out := []runtime.Container{}
	for _, c := range f.containers {
		if opts.All || c.State.IsRunning() {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (f *Runtime) InspectContainer(_ context.Context, id string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("inspect %s", id); err != nil {
		return nil, err
	}
	c, err := f.find(id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(f.inspectDoc(c))
}

func (f *Runtime) CreateContainer(_ context.Context, opts runtime.CreateContainerOpts) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastCreate = opts
	if err := f.record("create %s %s", opts.Name, opts.Image); err != nil {
		return "", err
	}
	if opts.Name != "" {
		if _, err := f.find(opts.Name); err == nil {
			return "", &runtime.CommandError{Op: "container create", Output: "name " + opts.Name + " already in use", Kind: runtime.ErrConflict}
		}
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	name := opts.Name
	if name == "" {
		name = "fake_" + id[:4]
	}
	var ports []string
	for _, p := range opts.Ports {
		ports = append(ports, p.String())
	}
	c := &runtime.Container{ID: id, Name: name, Image: opts.Image, Command: strings.Join(opts.Cmd, " "), State: runtime.StateCreated,
		Status: "Created", Ports: strings.Join(ports, ", "), Created: time.Now(), Networks: []string{"bridge"}}
	if opts.Network != "" {
		c.Networks = []string{opts.Network}
	}
	for _, v := range opts.Volumes {
		c.Mounts = append(c.Mounts, v.Source)
	}
	if opts.Start {
		c.State, c.Status = runtime.StateRunning, "Up Less than a second"
	}
	f.containers[id] = c
	f.env[id] = opts.Env
	return id, nil
}

func (f *Runtime) setState(op, id string, st runtime.State, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("%s %s", op, id); err != nil {
		return err
	}
	c, err := f.find(id)
	if err != nil {
		return err
	}
	c.State, c.Status = st, status
	return nil
}

func (f *Runtime) StartContainer(_ context.Context, id string) error {
	return f.setState("start", id, runtime.StateRunning, "Up Less than a second")
}

func (f *Runtime) StopContainer(_ context.Context, id string, _ *time.Duration) error {
	return f.setState("stop", id, runtime.StateExited, "Exited (0) Less than a second ago")
}

func (f *Runtime) RestartContainer(_ context.Context, id string, _ *time.Duration) error {
	return f.setState("restart", id, runtime.StateRunning, "Up Less than a second")
}

func (f *Runtime) RemoveContainer(_ context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("remove %s force=%t", id, force); err != nil {
		return err
	}
	c, err := f.find(id)
	if err != nil {
		return err
	}
	if c.State.IsRunning() && !force {
		return &runtime.CommandError{Op: "container remove", Output: "container is running", Kind: runtime.ErrConflict}
	}
	delete(f.containers, c.ID)
	return nil
}

// fakeLogLines is how many lines a fake container has logged so far.
const fakeLogLines = 5

func (f *Runtime) ContainerLogs(ctx context.Context, id string, opts runtime.LogsOpts, w io.Writer) error {
	f.mu.Lock()
	err := f.record("logs %s follow=%t", id, opts.Follow)
	var c *runtime.Container
	if err == nil {
		c, err = f.find(id)
	}
	var name string
	if c != nil {
		name = c.Name
	}
	interval := f.LogInterval
	f.mu.Unlock()
	if err != nil {
		return err
	}
	first := 1
	if opts.Tail > 0 && opts.Tail < fakeLogLines {
		first = fakeLogLines - opts.Tail + 1
	}
	n := first
	for ; n <= fakeLogLines; n++ {
		if _, err := fmt.Fprintf(w, "[fake] %s log line %d\n", name, n); err != nil {
			return err
		}
	}
	if !opts.Follow {
		return nil
	}
	if interval <= 0 {
		interval = time.Second
	}
	for ; ; n++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		f.mu.Lock()
		_, exists := f.containers[c.ID]
		running := exists && c.State.IsRunning()
		f.mu.Unlock()
		if !running {
			return nil
		}
		if _, err := fmt.Fprintf(w, "[fake] %s log line %d\n", name, n); err != nil {
			return err
		}
	}
}

func (f *Runtime) ListImages(context.Context) ([]runtime.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	out := []runtime.Image{}
	for _, img := range f.images {
		i := *img
		i.Containers = 0
		for _, c := range f.containers {
			if c.Image == i.Ref() {
				i.Containers++
			}
		}
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref() < out[j].Ref() })
	return out, nil
}

func (f *Runtime) PullImage(ctx context.Context, ref string, progress func(string)) error {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return err
	}
	f.mu.Lock()
	err := f.record("pull %s", ref)
	delay := f.PullDelay
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	for _, l := range []string{"Pulling from " + ref, "Downloading", "Extracting", "Pull complete", "Status: Downloaded newer image for " + ref} {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		progress(l)
	}
	repo, tag := runtime.SplitRef(ref)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	f.AddImage(runtime.Image{ID: "sha256:" + hex.EncodeToString(b), Repository: repo, Tag: tag, Size: "5MB", Created: time.Now()})
	return nil
}

func (f *Runtime) RemoveImage(_ context.Context, ref string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("rmi %s force=%t", ref, force); err != nil {
		return err
	}
	if _, ok := f.images[ref]; !ok {
		return &runtime.CommandError{Op: "image remove", Output: "no such image: " + ref, Kind: runtime.ErrNotFound}
	}
	if !force {
		for _, c := range f.containers {
			if c.Image == ref {
				return &runtime.CommandError{Op: "image remove", Output: "image is in use by container " + c.Name, Kind: runtime.ErrConflict}
			}
		}
	}
	delete(f.images, ref)
	return nil
}

func (f *Runtime) TagImage(_ context.Context, source, target string) error {
	if err := runtime.ValidateImageRef(target); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("tag %s %s", source, target); err != nil {
		return err
	}
	src, ok := f.images[source]
	if !ok {
		return &runtime.CommandError{Op: "image tag", Output: "no such image: " + source, Kind: runtime.ErrNotFound}
	}
	img := *src
	img.Repository, img.Tag = runtime.SplitRef(target)
	f.images[img.Ref()] = &img
	return nil
}

func (f *Runtime) ListVolumes(context.Context) ([]runtime.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Volume{}, f.volumes...), f.Err
}

func volume(name, driver string) runtime.Volume {
	return runtime.Volume{Name: name, Driver: driver, Mountpoint: "/var/lib/docker/volumes/" + name + "/_data", Scope: "local"}
}

// findVolume returns the index of the named volume or -1.
func (f *Runtime) findVolume(name string) int {
	for i, v := range f.volumes {
		if v.Name == name {
			return i
		}
	}
	return -1
}

// volumeUser returns the name of a container mounting the volume, or "".
func (f *Runtime) volumeUser(name string) string {
	for _, c := range f.containers {
		if slices.Contains(c.Mounts, name) {
			return c.Name
		}
	}
	return ""
}

func (f *Runtime) InspectVolume(_ context.Context, name string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("inspect-volume %s", name); err != nil {
		return nil, err
	}
	i := f.findVolume(name)
	if i < 0 {
		return nil, &runtime.CommandError{Op: "volume inspect", Output: "volume " + name + " not found", Kind: runtime.ErrNotFound}
	}
	v := f.volumes[i]
	return json.Marshal(map[string]any{"Name": v.Name, "Driver": v.Driver, "Mountpoint": v.Mountpoint, "Scope": v.Scope, "Labels": nil, "Options": nil})
}

// volumeDrivers mimic wslc's.
var volumeDrivers = []string{"guest", "vhd"}

func (f *Runtime) CreateVolume(_ context.Context, opts runtime.CreateVolumeOpts) (string, error) {
	if err := opts.Validate(volumeDrivers); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("create-volume %s", opts.Name); err != nil {
		return "", err
	}
	name := opts.Name
	if name == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		name = hex.EncodeToString(b)
	}
	if f.findVolume(name) >= 0 {
		return "", &runtime.CommandError{Op: "volume create", Output: "volume " + name + " already exists", Kind: runtime.ErrConflict}
	}
	driver := opts.Driver
	if driver == "" {
		driver = volumeDrivers[0]
	}
	f.volumes = append(f.volumes, volume(name, driver))
	return name, nil
}

func (f *Runtime) RemoveVolume(_ context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("rmv %s force=%t", name, force); err != nil {
		return err
	}
	i := f.findVolume(name)
	if i < 0 {
		if force {
			return nil
		}
		return &runtime.CommandError{Op: "volume remove", Output: "volume " + name + " not found", Kind: runtime.ErrNotFound}
	}
	// Like wslc, force does not remove a volume that is in use.
	if c := f.volumeUser(name); c != "" {
		return &runtime.CommandError{Op: "volume remove", Output: "volume is in use by container " + c, Kind: runtime.ErrConflict}
	}
	f.volumes = slices.Delete(f.volumes, i, i+1)
	return nil
}

// anonymousVolumeRe matches the generated names of anonymous volumes.
var anonymousVolumeRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (f *Runtime) PruneVolumes(_ context.Context, all bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("prune-volumes all=%t", all); err != nil {
		return "", err
	}
	var removed []string
	f.volumes = slices.DeleteFunc(f.volumes, func(v runtime.Volume) bool {
		prune := f.volumeUser(v.Name) == "" && (all || anonymousVolumeRe.MatchString(v.Name))
		if prune {
			removed = append(removed, v.Name)
		}
		return prune
	})
	return "Deleted Volumes: " + strings.Join(removed, ", "), nil
}

func (f *Runtime) ListNetworks(context.Context) ([]runtime.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Network{}, f.networks...), f.Err
}
