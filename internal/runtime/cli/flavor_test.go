package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestDockerArgsAndParsing(t *testing.T) {
	r := &fakeRunner{stdout: fixture(t, "docker_containers.ndjson")}
	d := NewWithRunner(r, Docker, config.Runtime{Session: "ignored"})
	cs, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "container ls --format json --no-trunc --all" {
		t.Errorf("args = %q", got)
	}
	if len(cs) != 2 || cs[0].Name != "web" || cs[0].State != runtime.StateRunning || cs[1].State != runtime.StateExited {
		t.Fatalf("containers = %+v", cs)
	}
	if cs[0].Created.IsZero() || cs[1].Mounts[0] != "data" {
		t.Errorf("container = %+v", cs[1])
	}

	_ = d.RemoveContainer(context.Background(), "web", true)
	if got := r.last(); got != "container rm --force web" {
		t.Errorf("remove args = %q", got)
	}
	_ = d.RemoveImage(context.Background(), "alpine", false)
	_ = d.RemoveVolume(context.Background(), "data", false)
	if got := r.last(); got != "volume rm data" {
		t.Errorf("volume remove args = %q", got)
	}
	_, _ = d.PruneVolumes(context.Background(), true)
	if got := r.last(); got != "volume prune --force --all" {
		t.Errorf("prune args = %q", got)
	}

	r.stdout = "27.3.1\n"
	info, err := d.Info(context.Background())
	if err != nil || info.Driver != "docker" || info.Version != "27.3.1" {
		t.Errorf("info = %+v, %v", info, err)
	}
	if got := r.last(); got != "version --format {{.Server.Version}}" {
		t.Errorf("version args = %q", got)
	}
	caps := d.Capabilities()
	if !reflect.DeepEqual(caps.VolumeDrivers, []string{"local"}) || !caps.PruneAnonymous {
		t.Errorf("caps = %+v", caps)
	}
	if _, err := d.CreateVolume(context.Background(), runtime.CreateVolumeOpts{Driver: "vhd"}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("wslc volume driver accepted by docker: %v", err)
	}
}

func TestPodmanParsing(t *testing.T) {
	r := &fakeRunner{stdout: fixture(t, "podman_containers.json")}
	d := NewWithRunner(r, Podman, config.Runtime{})
	cs, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.Container{
		ID:         "4f1c2b3a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a",
		Name:       "web",
		Image:      "docker.io/library/nginx:1.27",
		Command:    "nginx -g daemon off;",
		State:      runtime.StateRunning,
		Status:     "Up 26 hours",
		Ports:      "127.0.0.1:8080->80/tcp, 5000-5001->5000-5001/udp",
		Networks:   []string{"podman"},
		Mounts:     []string{"/data"},
		Created:    time.Unix(1792134723, 0),
		RunningFor: "26 hours ago",
	}
	if len(cs) != 2 || !reflect.DeepEqual(cs[0], want) {
		t.Fatalf("container = %+v\nwant        %+v", cs[0], want)
	}
	if cs[1].Ports != "" || cs[1].State != runtime.StateExited || cs[1].Command != "" {
		t.Errorf("stopped container = %+v", cs[1])
	}

	r.stdout = fixture(t, "podman_images.json")
	imgs, err := d.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "image ls --format json --no-trunc" {
		t.Errorf("args = %q", got)
	}
	if len(imgs) != 4 {
		t.Fatalf("images = %+v", imgs)
	}
	if imgs[0].Repository != "docker.io/library/nginx" || imgs[0].Tag != "1.27" || imgs[1].Tag != "latest" ||
		imgs[0].Size != "192MB" || imgs[0].Containers != 1 || imgs[0].Created.IsZero() {
		t.Errorf("tagged image = %+v / %+v", imgs[0], imgs[1])
	}
	if imgs[2].Ref() != "docker.io/library/alpine:latest" || imgs[2].Size != "8.72MB" {
		t.Errorf("podman 5 image (tags in Names) = %+v", imgs[2])
	}
	if imgs[3].Ref() != "sha256-2" || imgs[3].Size != "512B" {
		t.Errorf("dangling image = %+v", imgs[3])
	}

	r.stdout = fixture(t, "podman_networks.json")
	nets, err := d.ListNetworks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 1 || nets[0].Name != "podman" || nets[0].Driver != "bridge" || nets[0].Created.IsZero() {
		t.Errorf("networks = %+v", nets)
	}

	_, _ = d.PruneVolumes(context.Background(), true)
	if got := r.last(); got != "volume prune --force" {
		t.Errorf("prune args = %q", got)
	}
	if d.Capabilities().PruneAnonymous {
		t.Error("podman cannot limit prune to anonymous volumes")
	}
}

func TestEnglishErrorMapping(t *testing.T) {
	cases := []struct {
		stderr string
		want   error
	}{
		{"Error response from daemon: No such container: nope", runtime.ErrNotFound},
		{"Error: no such container nope", runtime.ErrNotFound},
		{"Error: nope: image not known", runtime.ErrNotFound},
		{"Error response from daemon: conflict: unable to remove repository reference", runtime.ErrConflict},
		{"Error response from daemon: remove data: volume is in use - [4f1c]", runtime.ErrConflict},
		{"Error response from daemon: container 4f1c is not running", runtime.ErrConflict},
		{"invalid reference format", runtime.ErrInvalid},
		{"something else", nil},
	}
	for _, c := range cases {
		d := NewWithRunner(&fakeRunner{stderr: c.stderr, code: 1}, Docker, config.Runtime{})
		err := d.StartContainer(context.Background(), "abc")
		var ce *runtime.CommandError
		if !errors.As(err, &ce) || ce.Kind != c.want || ce.Code != "" {
			t.Errorf("%q: err = %#v, want kind %v", c.stderr, err, c.want)
		}
		if !strings.Contains(err.Error(), "container start failed") {
			t.Errorf("%q: message = %q", c.stderr, err)
		}
	}
}

func TestCheckTerminalDockerContainerErrors(t *testing.T) {
	d := NewWithRunner(&fakeRunner{stderr: "Error response from daemon: container abc is not running", code: 1}, Docker, config.Runtime{})
	_, err := d.CheckTerminal(context.Background(), "abc")
	if !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("not running: %v", err)
	}
	d = NewWithRunner(&fakeRunner{stderr: `OCI runtime exec failed: exec: "/bin/sh": stat /bin/sh: no such file or directory`, code: 126}, Docker, config.Runtime{})
	_, err = d.CheckTerminal(context.Background(), "abc")
	if !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("no shell: %v", err)
	}
}

func TestSessionOnlyForWSLC(t *testing.T) {
	r := &fakeRunner{}
	_, _ = NewWithRunner(r, Podman, config.Runtime{Session: "s1"}).ListVolumes(context.Background())
	if got := r.last(); got != "volume ls --format json" {
		t.Errorf("args = %q", got)
	}
}

func TestDetect(t *testing.T) {
	answering := map[*Flavor]bool{Docker: false, Podman: true}
	installed := map[*Flavor]bool{Docker: true, Podman: true}
	open := func(f *Flavor) (*Driver, error) {
		if !installed[f] {
			return nil, errors.New(f.Name + ": not installed")
		}
		r := &fakeRunner{stdout: "1.0\n"}
		if !answering[f] {
			r = &fakeRunner{stderr: "Cannot connect to the Docker daemon", code: 1}
		}
		return NewWithRunner(r, f, config.Runtime{}), nil
	}
	cands := []*Flavor{WSLC, Docker, Podman}

	rt, err := detect(config.Runtime{}, cands, open)
	if err != nil || rt.Name() != "podman" {
		t.Errorf("answering podman preferred: %v, %v", rt, err)
	}
	answering[Podman] = false
	rt, err = detect(config.Runtime{}, cands, open)
	if err != nil || rt.Name() != "docker" {
		t.Errorf("first installed when none answers: %v, %v", rt, err)
	}
	installed = map[*Flavor]bool{}
	if _, err := detect(config.Runtime{}, cands, open); err == nil || !strings.Contains(err.Error(), "wslc: not installed") {
		t.Errorf("none installed: %v", err)
	}
}

func TestSocketPath(t *testing.T) {
	for _, tc := range []struct {
		flavor     *Flavor
		goos, host string
		euid       int
		want       string
	}{
		{Docker, "linux", "", 1000, "/var/run/docker.sock"},
		{Docker, "linux", "unix:///run/user/1000/docker.sock", 1000, "/run/user/1000/docker.sock"},
		{Podman, "linux", "", 0, "/run/podman/podman.sock"},
		{Podman, "linux", "", 1000, ""},
		{Docker, "windows", "", -1, `\\.\pipe\docker_engine`},
		{Docker, "windows", "npipe:////./pipe/dockerDesktopLinuxEngine", -1, `\\.\pipe\dockerDesktopLinuxEngine`},
		{Podman, "windows", "", -1, `\\.\pipe\podman-machine-default`},
		{WSLC, "windows", "", -1, ""},
	} {
		if got := socketPath(tc.flavor, tc.goos, tc.host, tc.euid); got != tc.want {
			t.Errorf("socketPath(%s, %s, %q) = %q, want %q", tc.flavor.Name, tc.goos, tc.host, got, tc.want)
		}
	}
}
