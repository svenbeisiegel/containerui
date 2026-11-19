package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/fake"
)

func inspectFixture(t *testing.T, i int) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("../compose/testdata/inspect_containers.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	return list[i]
}

func TestOptsFromInspect(t *testing.T) {
	img, err := os.ReadFile("../compose/testdata/inspect_image_nginx.json")
	if err != nil {
		t.Fatal(err)
	}
	src, err := runtime.OptsFromInspect(inspectFixture(t, 0), img)
	if err != nil {
		t.Fatal(err)
	}
	o := src.Opts
	if src.Name != "shop-web-1" || src.Project != "shop" || o.Image != "nginx:1.27" || o.Name != "shop-web-1" {
		t.Errorf("source = %+v", src)
	}
	// Command, entrypoint, PATH and the maintainer label come from the image.
	if o.Cmd != nil || o.Entrypoint != nil || !slices.Equal(o.Env, []string{"MODE=prod"}) || !slices.Equal(o.Labels, []string{"traefik.enable=true"}) {
		t.Errorf("cmd %q entrypoint %q env %q labels %q", o.Cmd, o.Entrypoint, o.Env, o.Labels)
	}
	var ports []string
	for _, p := range o.Ports {
		ports = append(ports, p.PortSpec())
	}
	if !slices.Equal(ports, []string{"8443:443", "5353:53/udp", "127.0.0.1:8080:80"}) {
		t.Errorf("ports = %q", ports)
	}
	wantVols := []runtime.VolumeMount{{Source: "/srv/shop/html", Target: "/usr/share/nginx/html", ReadOnly: true}, {Source: "shop_cache", Target: "/cache"}}
	if !reflect.DeepEqual(o.Volumes, wantVols) {
		t.Errorf("volumes = %+v", o.Volumes)
	}
	wantNets := []runtime.NetworkAttachment{{Name: "shop_default", Aliases: []string{"web"}}, {Name: "shop_front"}}
	if !reflect.DeepEqual(o.Networks, wantNets) {
		t.Errorf("networks = %+v", o.Networks)
	}
	if o.Hostname != "" || o.RestartPolicy != "unless-stopped" || len(src.Lost) != 0 {
		t.Errorf("hostname %q restart %q lost %q", o.Hostname, o.RestartPolicy, src.Lost)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("opts do not validate: %v", err)
	}

	// Without the image everything the container has is kept; anonymous
	// volumes are re-attached by name.
	src, err = runtime.OptsFromInspect(inspectFixture(t, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	o = src.Opts
	if o.User != "999" || o.Hostname != "dbhost" || len(o.Env) != 2 || !slices.Equal(o.Cmd, []string{"postgres"}) || len(o.Volumes) != 3 ||
		o.Volumes[1].Source != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || o.RestartPolicy != "" {
		t.Errorf("db opts = %+v", o)
	}
	if !slices.Equal(src.Lost, []string{"health check CMD-SHELL pg_isready"}) {
		t.Errorf("lost = %q", src.Lost)
	}
}

func TestOptsFromInspectHostSettings(t *testing.T) {
	raw := json.RawMessage(`{"Id":"abc123","Name":"/tool","State":{"Status":"exited"},
		"Config":{"Image":"alpine","Cmd":["-c","echo hi"],"Entrypoint":["/bin/sh"],"Hostname":"abc123","Tty":true,"OpenStdin":true,
		  "Healthcheck":{"Test":["NONE"]},"StopSignal":"SIGINT"},
		"HostConfig":{"NetworkMode":"host","AutoRemove":true,"Memory":536870912,"NanoCpus":1500000000,
		  "Ulimits":[{"Name":"nofile","Soft":1024,"Hard":2048}],"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":3},
		  "CapAdd":["NET_ADMIN"],"Privileged":true,"ExtraHosts":["db:10.0.0.5"],"Dns":["1.1.1.1"],
		  "Devices":[{"PathOnHost":"/dev/ttyUSB0","PathInContainer":"/dev/ttyUSB0"}],"PidMode":"host","Tmpfs":{"/run":""},"ShmSize":67108864},
		"Mounts":[{"Type":"tmpfs","Destination":"/cache"}],
		"NetworkSettings":{"Networks":{"host":{}}}}`)
	src, err := runtime.OptsFromInspect(raw, json.RawMessage(`{"Config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	o := src.Opts
	if src.Running || !src.AutoRemove || !o.AutoRemove || !o.TTY || !o.Interactive || !o.NoHealthcheck || !o.Privileged {
		t.Errorf("flags = %+v", o)
	}
	if !slices.Equal(o.Entrypoint, []string{"/bin/sh"}) || !slices.Equal(o.Cmd, []string{"-c", "echo hi"}) {
		t.Errorf("entrypoint %q cmd %q", o.Entrypoint, o.Cmd)
	}
	if o.Memory != "512m" || o.CPUs != "1.5" || o.RestartPolicy != "on-failure:3" || !slices.Equal(o.Ulimits, []string{"nofile=1024:2048"}) {
		t.Errorf("memory %q cpus %q restart %q ulimits %q", o.Memory, o.CPUs, o.RestartPolicy, o.Ulimits)
	}
	if !reflect.DeepEqual(o.Networks, []runtime.NetworkAttachment{{Name: "host"}}) {
		t.Errorf("networks = %+v", o.Networks)
	}
	want := []string{"tmpfs mount at /cache", "device /dev/ttyUSB0", "tmpfs /run", "PID namespace host", "stop signal SIGINT"}
	if !slices.Equal(src.Lost, want) {
		t.Errorf("lost =\n %q\nwant\n %q", src.Lost, want)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("opts do not validate: %v", err)
	}
}

func TestDropUnsupported(t *testing.T) {
	o := runtime.CreateContainerOpts{Image: "a", RestartPolicy: "always", Networks: []runtime.NetworkAttachment{{Name: "a", Aliases: []string{"x"}}, {Name: "b"}}}
	dropped := o.DropUnsupported(runtime.Capabilities{CreateOptions: map[string]bool{runtime.OptMultiNetwork: true}})
	if !slices.Equal(dropped, []string{"restart policy", "network aliases and fixed IPs"}) || o.RestartPolicy != "" || o.Networks[0].Aliases != nil || len(o.Networks) != 2 {
		t.Errorf("dropped %q, opts %+v", dropped, o)
	}
}

// created creates a running container "app" in the fake and returns its ID.
func created(t *testing.T, rt *fake.Runtime) (string, runtime.CreateContainerOpts) {
	t.Helper()
	opts := runtime.CreateContainerOpts{
		Image: "nginx:1.27", Name: "app", Env: []string{"MODE=prod"}, Start: true,
		Ports:         []runtime.PortMapping{{HostPort: 8095, ContainerPort: 80}},
		Volumes:       []runtime.VolumeMount{{Source: "data", Target: "/data"}},
		Networks:      []runtime.NetworkAttachment{{Name: "backend", Aliases: []string{"api"}}, {Name: "bridge"}},
		RestartPolicy: "always", Labels: []string{"team=web"}, CapAdd: []string{"NET_ADMIN"},
	}
	id, err := rt.CreateContainer(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return id, opts
}

func TestRecreateRoundTrip(t *testing.T) {
	rt := fake.NewSeeded()
	ctx := context.Background()
	id, want := created(t, rt)
	raw, _ := rt.InspectContainer(ctx, id)
	img, _ := rt.InspectImage(ctx, "nginx:1.27")
	src, err := runtime.OptsFromInspect(raw, img)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Running || len(src.Lost) != 0 {
		t.Errorf("running %v lost %q", src.Running, src.Lost)
	}
	want.Pull, want.Start = runtime.PullMissing, false
	if !reflect.DeepEqual(src.Opts, want) {
		t.Errorf("opts =\n %+v\nwant\n %+v", src.Opts, want)
	}

	// Update: same settings, newer image.
	opts := src.Opts
	opts.Image, opts.Pull, opts.Start = "nginx:1.28", runtime.PullAlways, true
	var lines []string
	rt.Calls = nil
	newID, err := runtime.Recreate(ctx, rt, id, opts, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.InspectContainer(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("old container still there: %v", err)
	}
	cs, _ := rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	i := slices.IndexFunc(cs, func(c runtime.Container) bool { return c.ID == newID })
	if i < 0 || cs[i].Name != "app" || cs[i].Image != "nginx:1.28" || cs[i].State != runtime.StateRunning {
		t.Fatalf("new container = %+v", cs)
	}
	if rt.LastCreate.Pull != runtime.PullMissing || !slices.Equal(rt.LastCreate.CapAdd, []string{"NET_ADMIN"}) {
		t.Errorf("create opts = %+v", rt.LastCreate)
	}
	calls := strings.Join(rt.Calls, "\n")
	for _, c := range []string{"pull nginx:1.28", "rename " + id + " app-old-", "stop " + id, "create app nginx:1.28", "remove " + id + " force=false"} {
		if !strings.Contains(calls, c) {
			t.Errorf("calls miss %q:\n%s", c, calls)
		}
	}
	if lines[len(lines)-1] != "Done" {
		t.Errorf("progress = %q", lines)
	}
}

func TestRecreateRollsBack(t *testing.T) {
	rt := fake.NewSeeded()
	ctx := context.Background()
	id, opts := created(t, rt)
	// shop-web-1 publishes 8081, so starting the new container fails.
	opts.Ports = []runtime.PortMapping{{HostPort: 8081, ContainerPort: 80}}
	_, err := runtime.Recreate(ctx, rt, id, opts, nil)
	if !errors.Is(err, runtime.ErrConflict) || !strings.Contains(err.Error(), "old container was restored") {
		t.Fatalf("err = %v", err)
	}
	cs, _ := rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	i := slices.IndexFunc(cs, func(c runtime.Container) bool { return c.ID == id })
	if i < 0 || cs[i].Name != "app" || cs[i].State != runtime.StateRunning {
		t.Errorf("old container = %+v", cs)
	}
	if n := len(slices.DeleteFunc(cs, func(c runtime.Container) bool { return !strings.HasPrefix(c.Name, "app") })); n != 1 {
		t.Errorf("%d app containers", n)
	}
}

// The pull policy ContainerUI records in a label comes back as the
// container's pull policy and is not one of its own labels.
func TestOptsFromInspectPullLabel(t *testing.T) {
	doc := func(labels map[string]string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"Id": "abc", "Name": "/web", "Config": map[string]any{"Image": "nginx:latest", "Labels": labels}})
		return b
	}
	for label, want := range map[string]runtime.PullPolicy{"always": runtime.PullAlways, "never": runtime.PullNever, "missing": runtime.PullMissing, "bogus": runtime.PullMissing, "": runtime.PullMissing} {
		labels := map[string]string{"team": "web"}
		if label != "" {
			labels[runtime.LabelPull] = label
		}
		src, err := runtime.OptsFromInspect(doc(labels), nil)
		if err != nil {
			t.Fatal(err)
		}
		if src.Opts.Pull != want || !slices.Equal(src.Opts.Labels, []string{"team=web"}) {
			t.Errorf("label %q: pull = %q, labels = %v", label, src.Opts.Pull, src.Opts.Labels)
		}
	}
}

func TestSetPullLabel(t *testing.T) {
	o := runtime.CreateContainerOpts{Labels: []string{"team=web", runtime.LabelPull + "=never"}}
	o.SetPullLabel(runtime.PullAlways)
	if want := []string{"team=web", "containerui.pull=always"}; !slices.Equal(o.Labels, want) {
		t.Errorf("labels = %v; want %v", o.Labels, want)
	}
	o.SetPullLabel("")
	if want := []string{"team=web", "containerui.pull=missing"}; !slices.Equal(o.Labels, want) {
		t.Errorf("labels = %v; want %v", o.Labels, want)
	}
}
