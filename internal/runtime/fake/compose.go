package fake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/compose"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.Composer = (*Runtime)(nil)

// seedCompose adds the deployed project "shop" (web + db).
func (f *Runtime) seedCompose(now time.Time) {
	f.AddImage(runtime.Image{ID: "sha256:ccc333", Repository: "postgres", Tag: "16", Size: "430MB", Created: now.Add(-96 * time.Hour)})
	labels := func(service string) map[string]string {
		return map[string]string{
			runtime.LabelProject: "shop", runtime.LabelService: service, runtime.LabelNumber: "1", runtime.LabelOneOff: "False",
			runtime.LabelWorkingDir: "/srv/shop", runtime.LabelConfigFiles: "/srv/shop/compose.yaml",
		}
	}
	web := labels("web")
	web[runtime.LabelDependsOn] = "db:service_started:false"
	f.AddContainer(runtime.Container{ID: "5b0b00000001", Name: "shop-web-1", Image: "nginx:1.27", State: runtime.StateRunning, Status: "Up 5 hours",
		Ports: "0.0.0.0:8081->80/tcp", Networks: []string{"shop_default"}, Created: now.Add(-5 * time.Hour), Labels: web})
	f.AddContainer(runtime.Container{ID: "5b0b00000002", Name: "shop-db-1", Image: "postgres:16", State: runtime.StateRunning, Status: "Up 5 hours",
		Networks: []string{"shop_default"}, Mounts: []string{"shop_dbdata"}, Created: now.Add(-5 * time.Hour), Labels: labels("db")})
	f.env["5b0b00000002"] = []string{"POSTGRES_PASSWORD=example"}
	f.volumes = append(f.volumes, runtime.Volume{Name: "shop_dbdata", Driver: "local", Mountpoint: "/var/lib/docker/volumes/shop_dbdata/_data", Scope: "local",
		Labels: map[string]string{runtime.LabelProject: "shop", runtime.LabelVolume: "dbdata"}})
	f.networks = append(f.networks, runtime.Network{ID: "n4", Name: "shop_default", Driver: "bridge", Scope: "local", Created: now.Add(-5 * time.Hour),
		Labels: map[string]string{runtime.LabelProject: "shop", runtime.LabelNetwork: "default"}})
}

// portRe matches a fake port summary entry: [ip:]host->container/proto.
var portRe = regexp.MustCompile(`^(?:(.*):)?(\d+)->(\d+)/(\w+)$`)

// inspectDoc builds a docker-style inspect document for c (call with f.mu held).
func (f *Runtime) inspectDoc(c *runtime.Container) map[string]any {
	bindings := map[string]any{}
	for _, p := range strings.Split(c.Ports, ", ") {
		if m := portRe.FindStringSubmatch(p); m != nil {
			bindings[m[3]+"/"+m[4]] = []map[string]string{{"HostIp": m[1], "HostPort": m[2]}}
		}
	}
	var mounts []map[string]any
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]any{"Type": "volume", "Name": m, "Destination": "/data/" + m, "RW": true})
	}
	nets := map[string]any{}
	for _, n := range c.Networks {
		nets[n] = map[string]any{}
	}
	var cmd []string
	if c.Command != "" {
		cmd = strings.Fields(c.Command)
	}
	mode := "bridge"
	if len(c.Networks) > 0 {
		mode = c.Networks[0]
	}
	return map[string]any{
		"Id":   c.ID,
		"Name": "/" + c.Name,
		"Config": map[string]any{
			"Image": c.Image, "Cmd": cmd, "Env": append(slices.Clone(f.env[c.ID]), imageEnv...), "Labels": c.Labels, "Hostname": c.ID,
		},
		"HostConfig":      map[string]any{"PortBindings": bindings, "NetworkMode": mode, "RestartPolicy": map[string]any{"Name": "no"}},
		"Mounts":          mounts,
		"NetworkSettings": map[string]any{"Networks": nets},
		"State":           map[string]any{"Status": c.State},
	}
}

// imageEnv is the environment every fake image sets.
var imageEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

func (f *Runtime) InspectImage(_ context.Context, ref string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("inspect-image %s", ref); err != nil {
		return nil, err
	}
	img, ok := f.images[ref]
	if !ok {
		return nil, &runtime.CommandError{Op: "image inspect", Output: "no such image: " + ref, Kind: runtime.ErrNotFound}
	}
	return json.Marshal(map[string]any{"Id": img.ID, "RepoTags": []string{img.Ref()}, "Config": map[string]any{"Env": imageEnv}})
}

func (f *Runtime) composeCheck(op string, t runtime.ComposeTarget) error {
	if err := f.record("compose %s %s", op, t.Project); err != nil {
		return err
	}
	if !f.ComposeEnabled {
		return fmt.Errorf("compose: %w", runtime.ErrUnsupported)
	}
	return runtime.ValidateProjectName(t.Project)
}

// readCompose loads and checks the target's compose file.
func readCompose(t runtime.ComposeTarget) (*compose.File, error) {
	if t.File == "" {
		return nil, fmt.Errorf("%w: no compose file", runtime.ErrInvalid)
	}
	src, err := os.ReadFile(t.File)
	if err != nil {
		return nil, err
	}
	file, problems := compose.Parse(src)
	if compose.HasErrors(problems) {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.String())
		}
		return nil, &runtime.CommandError{Op: "compose config", ExitCode: 15, Output: strings.Join(msgs, "; "), Kind: runtime.ErrInvalid}
	}
	return file, nil
}

func (f *Runtime) ComposeValidate(_ context.Context, t runtime.ComposeTarget) error {
	f.mu.Lock()
	err := f.composeCheck("validate", t)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = readCompose(t)
	return err
}

// emit reports progress lines, pausing PullDelay before each.
func (f *Runtime) emit(ctx context.Context, progress func(string), lines []string) error {
	f.mu.Lock()
	delay := f.PullDelay
	f.mu.Unlock()
	if progress == nil {
		progress = func(string) {}
	}
	for _, l := range lines {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		progress(l)
	}
	return nil
}

// projectContainers returns the containers of a project (call with f.mu held).
func (f *Runtime) projectContainers(project string) []*runtime.Container {
	var out []*runtime.Container
	for _, c := range f.containers {
		if c.Labels[runtime.LabelProject] == project {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *runtime.Container) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (f *Runtime) ComposeUp(ctx context.Context, t runtime.ComposeTarget, progress func(string)) error {
	f.mu.Lock()
	err := f.composeCheck("up", t)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	file, err := readCompose(t)
	if err != nil {
		return err
	}

	f.mu.Lock()
	var lines []string
	withLabels := func(extra map[string]string) map[string]string {
		m := map[string]string{runtime.LabelProject: t.Project}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, n := range append([]string{"default"}, file.Networks...) {
		name := t.Project + "_" + n
		if !slices.ContainsFunc(f.networks, func(x runtime.Network) bool { return x.Name == name }) {
			f.networks = append(f.networks, runtime.Network{ID: randomHex(6), Name: name, Driver: "bridge", Scope: "local", Created: time.Now(),
				Labels: withLabels(map[string]string{runtime.LabelNetwork: n})})
			lines = append(lines, "Network "+name+"  Created")
		}
	}
	for _, v := range file.Volumes {
		name := t.Project + "_" + v
		if f.findVolume(name) < 0 {
			vol := volume(name, "local")
			vol.Labels = withLabels(map[string]string{runtime.LabelVolume: v})
			f.volumes = append(f.volumes, vol)
			lines = append(lines, "Volume "+name+"  Created")
		}
	}
	want := map[string]bool{}
	for _, s := range file.Services {
		image := s.Image
		if image == "" {
			image = t.Project + "-" + s.Name // built image
		}
		for n := 1; n <= max(s.Replicas, 1); n++ {
			name := fmt.Sprintf("%s-%s-%d", t.Project, s.Name, n)
			want[name] = true
			if c, err := f.find(name); err == nil {
				if c.Image == image {
					if !c.State.IsRunning() {
						c.State, c.Status = runtime.StateRunning, "Up Less than a second"
						lines = append(lines, "Container "+name+"  Started")
					} else {
						lines = append(lines, "Container "+name+"  Running")
					}
					continue
				}
				delete(f.containers, c.ID)
				lines = append(lines, "Container "+name+"  Recreate")
			}
			labels := withLabels(map[string]string{runtime.LabelService: s.Name, runtime.LabelNumber: strconv.Itoa(n), runtime.LabelOneOff: "False",
				runtime.LabelConfigFiles: t.File})
			id := randomHex(6)
			f.containers[id] = &runtime.Container{ID: id, Name: name, Image: image, State: runtime.StateRunning, Status: "Up Less than a second",
				Networks: []string{t.Project + "_default"}, Created: time.Now(), Labels: labels}
			lines = append(lines, "Container "+name+"  Started")
		}
	}
	for _, c := range f.projectContainers(t.Project) {
		if !want[c.Name] {
			delete(f.containers, c.ID)
			lines = append(lines, "Container "+c.Name+"  Removed")
		}
	}
	f.mu.Unlock()
	return f.emit(ctx, progress, lines)
}

func (f *Runtime) ComposeAction(ctx context.Context, t runtime.ComposeTarget, action string, progress func(string)) error {
	f.mu.Lock()
	err := f.composeCheck(action, t)
	if err == nil && !slices.Contains(runtime.ComposeActions, action) {
		err = fmt.Errorf("%w: compose action %q", runtime.ErrInvalid, action)
	}
	if err != nil {
		f.mu.Unlock()
		return err
	}
	var lines []string
	for _, c := range f.projectContainers(t.Project) {
		switch action {
		case "start":
			c.State, c.Status = runtime.StateRunning, "Up Less than a second"
			lines = append(lines, "Container "+c.Name+"  Started")
		case "stop":
			c.State, c.Status = runtime.StateExited, "Exited (0) Less than a second ago"
			lines = append(lines, "Container "+c.Name+"  Stopped")
		case "restart":
			c.State, c.Status = runtime.StateRunning, "Up Less than a second"
			lines = append(lines, "Container "+c.Name+"  Restarted")
		case "pull":
			lines = append(lines, c.Labels[runtime.LabelService]+" Pulled")
		}
	}
	f.mu.Unlock()
	return f.emit(ctx, progress, lines)
}

func (f *Runtime) ComposeDown(ctx context.Context, t runtime.ComposeTarget, volumes bool, progress func(string)) error {
	f.mu.Lock()
	if err := f.composeCheck("down", t); err != nil {
		f.mu.Unlock()
		return err
	}
	f.Calls[len(f.Calls)-1] += fmt.Sprintf(" volumes=%t", volumes)
	var lines []string
	for _, c := range f.projectContainers(t.Project) {
		delete(f.containers, c.ID)
		lines = append(lines, "Container "+c.Name+"  Removed")
	}
	f.networks = slices.DeleteFunc(f.networks, func(n runtime.Network) bool {
		if n.Labels[runtime.LabelProject] == t.Project {
			lines = append(lines, "Network "+n.Name+"  Removed")
			return true
		}
		return false
	})
	if volumes {
		f.volumes = slices.DeleteFunc(f.volumes, func(v runtime.Volume) bool {
			if v.Labels[runtime.LabelProject] == t.Project {
				lines = append(lines, "Volume "+v.Name+"  Removed")
				return true
			}
			return false
		})
	}
	f.mu.Unlock()
	return f.emit(ctx, progress, lines)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
