package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// fakeRunner returns scripted results and records the args it was called with.
type fakeRunner struct {
	calls  [][]string
	stdout string
	stderr string
	code   int
	lines  []string

	ptySize [2]uint16
	pty     *stubTerminal
}

func (f *fakeRunner) Run(_ context.Context, args []string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, args)
	return []byte(f.stdout), []byte(f.stderr), f.code, nil
}

func (f *fakeRunner) Stream(_ context.Context, args []string, onLine func(string)) ([]byte, int, error) {
	f.calls = append(f.calls, args)
	for _, l := range f.lines {
		onLine(l)
	}
	return []byte(f.stderr), f.code, nil
}

func (f *fakeRunner) Pipe(_ context.Context, args []string, w io.Writer) (int, error) {
	f.calls = append(f.calls, args)
	_, _ = io.WriteString(w, f.stdout+f.stderr)
	return f.code, nil
}

func (f *fakeRunner) StartPTY(args []string, cols, rows uint16) (runtime.Terminal, error) {
	f.calls = append(f.calls, args)
	f.ptySize = [2]uint16{cols, rows}
	f.pty = &stubTerminal{done: make(chan struct{})}
	return f.pty, nil
}

// stubTerminal stands in for a wslc process on a pseudo terminal.
type stubTerminal struct {
	done chan struct{}
	once sync.Once
}

func (s *stubTerminal) exit()                       { s.once.Do(func() { close(s.done) }) }
func (s *stubTerminal) Read([]byte) (int, error)    { <-s.done; return 0, io.EOF }
func (s *stubTerminal) Write(b []byte) (int, error) { return len(b), nil }
func (s *stubTerminal) Resize(uint16, uint16) error { return nil }
func (s *stubTerminal) Wait() (int, error)          { <-s.done; return 0, nil }
func (s *stubTerminal) Close() error                { s.exit(); return nil }

func (f *fakeRunner) last() string { return strings.Join(f.calls[len(f.calls)-1], " ") }

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestListContainers(t *testing.T) {
	r := &fakeRunner{stdout: fixture(t, "containers.ndjson")}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	got, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.last() != "container list --format json --no-trunc --all" {
		t.Errorf("args = %q", r.last())
	}
	if len(got) != 1 {
		t.Fatalf("got %d containers", len(got))
	}
	c := got[0]
	if c.Name != "couchdb" || c.State != runtime.StateRunning || !strings.HasPrefix(c.ID, "65ad6ac603a6") {
		t.Errorf("container = %+v", c)
	}
	if c.Ports != "127.0.0.1:5984->5984/tcp" || !reflect.DeepEqual(c.Networks, []string{"bridge"}) || !reflect.DeepEqual(c.Mounts, []string{"couchdb"}) {
		t.Errorf("ports/networks/mounts = %q %v %v", c.Ports, c.Networks, c.Mounts)
	}
	if c.Created.IsZero() || c.Created.UTC().Hour() != 6 {
		t.Errorf("created = %v", c.Created)
	}
	if strings.HasPrefix(c.Command, `"`) {
		t.Errorf("command not unquoted: %q", c.Command)
	}
}

func TestListImagesVolumesNetworks(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{stdout: fixture(t, "images.ndjson")}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	imgs, err := d.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) == 0 {
		t.Fatal("no images parsed")
	}
	var couch runtime.Image
	for _, i := range imgs {
		if strings.Contains(i.Repository, "couchdb") {
			couch = i
		}
	}
	if couch.Tag != "3.5.2-r2-tumbleweed" || couch.Size != "202MB" || couch.Containers != 1 || couch.Digest != "" || couch.Created.IsZero() {
		t.Errorf("image = %+v", couch)
	}

	r.stdout = fixture(t, "volumes.ndjson")
	vols, err := d.ListVolumes(ctx)
	if err != nil || len(vols) != 1 || vols[0].Name != "couchdb" || vols[0].Driver != "guest" {
		t.Errorf("volumes = %+v, %v", vols, err)
	}

	r.stdout = fixture(t, "networks.ndjson")
	nets, err := d.ListNetworks(ctx)
	if err != nil || len(nets) != 3 {
		t.Fatalf("networks = %+v, %v", nets, err)
	}
	names := []string{nets[0].Name, nets[1].Name, nets[2].Name}
	if !reflect.DeepEqual(names, []string{"bridge", "host", "none"}) || nets[0].Created.IsZero() {
		t.Errorf("networks = %+v", nets)
	}
}

func TestEmptyAndUTF16Output(t *testing.T) {
	d := NewWithRunner(&fakeRunner{stdout: "\n"}, WSLC, config.Runtime{})
	got, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{})
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty output = %v, %v", got, err)
	}

	utf16 := []byte{0xFF, 0xFE}
	for _, r := range `{"Name":"v1","Driver":"local"}` {
		utf16 = append(utf16, byte(r), 0)
	}
	vols, err := parseVolumes(utf16)
	if err != nil || len(vols) != 1 || vols[0].Name != "v1" {
		t.Errorf("utf16 = %+v, %v", vols, err)
	}
}

func TestSessionFlag(t *testing.T) {
	r := &fakeRunner{}
	d := NewWithRunner(r, WSLC, config.Runtime{Session: "mysession"})
	_ = d.StartContainer(context.Background(), "web")
	if r.last() != "--session mysession container start web" {
		t.Errorf("args = %q", r.last())
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		stderr string
		code   string
		kind   error
	}{
		{"Der Container „x“ wurde nicht gefunden.\nFehlercode: WSLC_E_CONTAINER_NOT_FOUND\nWenn dieser Fehler unerwartet war, ... https://github.com/microsoft/WSL/issues.\n",
			"WSLC_E_CONTAINER_NOT_FOUND", runtime.ErrNotFound},
		{"Der Container wird ausgeführt.\nFehlercode: WSLC_E_CONTAINER_IS_RUNNING\n", "WSLC_E_CONTAINER_IS_RUNNING", runtime.ErrConflict},
		{"conflict: unable to remove repository reference\nFehlercode: ERROR_SHARING_VIOLATION\n", "ERROR_SHARING_VIOLATION", runtime.ErrConflict},
		{"name already in use\nError code: ERROR_ALREADY_EXISTS\n", "ERROR_ALREADY_EXISTS", runtime.ErrConflict},
		{"something odd happened\n", "", nil},
	}
	for _, c := range cases {
		r := &fakeRunner{stderr: c.stderr, code: 1}
		d := NewWithRunner(r, WSLC, config.Runtime{})
		err := d.StopContainer(context.Background(), "x", nil)
		var ce *runtime.CommandError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v; want CommandError", err)
		}
		if ce.Code != c.code || (c.kind != nil && !errors.Is(err, c.kind)) {
			t.Errorf("stderr %q: code=%q kind=%v err=%v", c.stderr, ce.Code, ce.Kind, err)
		}
		if strings.Contains(ce.Output, "github.com") || (c.code != "" && strings.Contains(ce.Output, c.code)) {
			t.Errorf("output not cleaned: %q", ce.Output)
		}
	}
}

func TestInspectNotFound(t *testing.T) {
	d := NewWithRunner(&fakeRunner{stdout: "[]\n", stderr: "nicht gefunden", code: 1}, WSLC, config.Runtime{})
	if _, err := d.InspectContainer(context.Background(), "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("err = %v; want ErrNotFound", err)
	}
	d = NewWithRunner(&fakeRunner{stdout: `[{"Id":"abc"}]`}, WSLC, config.Runtime{})
	raw, err := d.InspectContainer(context.Background(), "abc")
	if err != nil || string(raw) != `{"Id":"abc"}` {
		t.Errorf("inspect = %s, %v", raw, err)
	}
}

func TestVolumeInspectNotFound(t *testing.T) {
	d := NewWithRunner(&fakeRunner{stdout: "[]\n", stderr: "Volume nicht gefunden: 'nope'", code: 1}, WSLC, config.Runtime{})
	if _, err := d.InspectVolume(context.Background(), "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("err = %v; want ErrNotFound", err)
	}
	r := &fakeRunner{stdout: "[\n  {\n    \"Name\": \"data\"\n  }\n]\n"}
	d = NewWithRunner(r, WSLC, config.Runtime{})
	raw, err := d.InspectVolume(context.Background(), "data")
	if err != nil || !strings.Contains(string(raw), `"data"`) || r.last() != "volume inspect data" {
		t.Errorf("inspect = %s, %v, args %q", raw, err, r.last())
	}
}

func TestCreateVolumeArgs(t *testing.T) {
	r := &fakeRunner{stdout: "data\n"}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	name, err := d.CreateVolume(context.Background(), runtime.CreateVolumeOpts{
		Name: "data", Driver: "vhd", Options: []string{"size=1G"}, Labels: []string{"app=web", "tier=db"},
	})
	if err != nil || name != "data" {
		t.Fatalf("create = %q, %v", name, err)
	}
	if want := "volume create --driver vhd --opt size=1G --label app=web --label tier=db data"; r.last() != want {
		t.Errorf("args = %q; want %q", r.last(), want)
	}
	// Without a name the runtime's generated name is returned.
	r.stdout = "3f2a9c\n"
	if name, err := d.CreateVolume(context.Background(), runtime.CreateVolumeOpts{}); err != nil || name != "3f2a9c" || r.last() != "volume create" {
		t.Errorf("anonymous create = %q, %v, args %q", name, err, r.last())
	}
}

func TestCreateContainerArgs(t *testing.T) {
	r := &fakeRunner{stdout: "ee71a64f0869\n"}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	id, err := d.CreateContainer(context.Background(), runtime.CreateContainerOpts{
		Image:   "alpine:latest",
		Name:    "web",
		Cmd:     []string{"echo", "-n", "hi"},
		Env:     []string{"A=1"},
		Ports:   []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80}},
		Volumes: []runtime.VolumeMount{{Source: "data", Target: "/data", ReadOnly: true}},
		Network: "bridge",
		Pull:    runtime.PullMissing,
		Start:   true,
	})
	if err != nil || id != "ee71a64f0869" {
		t.Fatalf("create = %q, %v", id, err)
	}
	want := "container create --name web -e A=1 -p 127.0.0.1:8080:80 -v data:/data:ro --network bridge --pull missing alpine:latest echo -n hi"
	if got := strings.Join(r.calls[0], " "); got != want {
		t.Errorf("create args =\n %q\nwant\n %q", got, want)
	}
	if r.last() != "container start ee71a64f0869" {
		t.Errorf("start args = %q", r.last())
	}
}

func TestValidationBlocksFlagInjection(t *testing.T) {
	r := &fakeRunner{}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	ctx := context.Background()
	errs := []error{
		d.StopContainer(ctx, "--all", nil),
		d.RemoveContainer(ctx, "-f", true),
		d.RemoveImage(ctx, "--force", false),
		d.TagImage(ctx, "alpine", "-x"),
		d.PullImage(ctx, "--all-tags", nil),
		d.RemoveVolume(ctx, "--force", false),
	}
	for i, err := range errs {
		if !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("case %d: err = %v; want ErrInvalid", i, err)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("runner was called: %v", r.calls)
	}
}

func TestMiscArgs(t *testing.T) {
	r := &fakeRunner{}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	ctx := context.Background()
	timeout := 5 * time.Second
	steps := []struct {
		do   func() error
		want string
	}{
		{func() error { return d.RestartContainer(ctx, "web", &timeout) }, "container restart -t 5 web"},
		{func() error { return d.StopContainer(ctx, "web", nil) }, "container stop web"},
		{func() error { return d.RemoveContainer(ctx, "web", true) }, "container remove --force web"},
		{func() error { return d.RemoveImage(ctx, "ghcr.io/a/b:1", true) }, "image remove --force ghcr.io/a/b:1"},
		{func() error { return d.TagImage(ctx, "alpine:latest", "alpine:test") }, "image tag alpine:latest alpine:test"},
		{func() error { return d.RemoveVolume(ctx, "data", false) }, "volume remove data"},
		{func() error { return d.RemoveVolume(ctx, "data", true) }, "volume remove --force data"},
		{func() error { _, err := d.PruneVolumes(ctx, false); return err }, "volume prune --force"},
		{func() error { _, err := d.PruneVolumes(ctx, true); return err }, "volume prune --force --all"},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatal(err)
		}
		if r.last() != s.want {
			t.Errorf("args = %q; want %q", r.last(), s.want)
		}
	}
}

func TestPullStreamsProgress(t *testing.T) {
	r := &fakeRunner{lines: []string{"latest: Pulling from library/alpine", "Status: Downloaded newer image"}}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	var got []string
	if err := d.PullImage(context.Background(), "alpine:latest", func(l string) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || r.last() != "image pull alpine:latest" {
		t.Errorf("progress = %v, args = %q", got, r.last())
	}

	r = &fakeRunner{stderr: "Fehlercode: WSLC_E_IMAGE_NOT_FOUND\n", code: 1}
	d = NewWithRunner(r, WSLC, config.Runtime{})
	if err := d.PullImage(context.Background(), "nope:x", nil); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("pull err = %v", err)
	}
}

func TestContainerLogs(t *testing.T) {
	r := &fakeRunner{stdout: "line 1\n\n  indented\r\n"}
	d := NewWithRunner(r, WSLC, config.Runtime{Session: "s1"})
	var buf strings.Builder
	if err := d.ContainerLogs(context.Background(), "web", runtime.LogsOpts{}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != r.stdout || r.last() != "--session s1 container logs web" {
		t.Errorf("output = %q, args = %q", buf.String(), r.last())
	}

	if err := d.ContainerLogs(context.Background(), "web", runtime.LogsOpts{Follow: true, Tail: 100}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if r.last() != "--session s1 container logs --follow --tail 100 web" {
		t.Errorf("follow args = %q", r.last())
	}

	d = NewWithRunner(&fakeRunner{stderr: "Der Container wurde nicht gefunden.\nFehlercode: WSLC_E_CONTAINER_NOT_FOUND\n", code: 1}, WSLC, config.Runtime{})
	err := d.ContainerLogs(context.Background(), "nope", runtime.LogsOpts{}, io.Discard)
	if !errors.Is(err, runtime.ErrNotFound) || !strings.Contains(err.Error(), "nicht gefunden") {
		t.Errorf("logs err = %v", err)
	}

	if err := d.ContainerLogs(context.Background(), "-rf", runtime.LogsOpts{}, io.Discard); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("invalid id err = %v", err)
	}
}

func TestTailWriterKeepsEnd(t *testing.T) {
	var out strings.Builder
	tw := &tailWriter{w: &out}
	chunk := strings.Repeat("x", 1000)
	for range 10 {
		_, _ = tw.Write([]byte(chunk))
	}
	_, _ = tw.Write([]byte("END"))
	if len(tw.tail) != tailLimit || !strings.HasSuffix(string(tw.tail), "END") || out.Len() != 10003 {
		t.Errorf("tail len = %d, out len = %d", len(tw.tail), out.Len())
	}
}

func TestScanLinesOrCR(t *testing.T) {
	data := []byte("a\rb\r\nc")
	var parts []string
	for len(data) > 0 {
		n, tok, _ := scanLinesOrCR(data, true)
		parts = append(parts, string(tok))
		data = data[n:]
	}
	if strings.Join(parts, "|") != "a|b||c" {
		t.Errorf("parts = %q", parts)
	}
}

func TestCreateContainerAdvancedArgs(t *testing.T) {
	r := &fakeRunner{stdout: "ee71a64f0869\n"}
	d := NewWithRunner(r, WSLC, config.Runtime{})
	_, err := d.CreateContainer(context.Background(), runtime.CreateContainerOpts{
		Image: "alpine", Cmd: []string{"sleep", "300"},
		TTY: true, Interactive: true, AutoRemove: true, NoHealthcheck: true,
		CPUs: "0.5", Memory: "64M", Hostname: "cui-host", User: "1000:1000",
		DNS: []string{"1.1.1.1", "8.8.8.8"}, DNSSearch: []string{"example.com"},
		DNSOptions: []string{"ndots:2"}, Ulimits: []string{"nofile=1024:2048", "nproc=512"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "container create --tty --interactive --rm --no-healthcheck --cpus 0.5 --memory 64M --hostname cui-host --user 1000:1000" +
		" --dns 1.1.1.1 --dns 8.8.8.8 --dns-search example.com --dns-option ndots:2 --ulimit nofile=1024:2048 --ulimit nproc=512" +
		" alpine sleep 300"
	if got := strings.Join(r.calls[0], " "); got != want {
		t.Errorf("create args =\n %q\nwant\n %q", got, want)
	}
}

func TestCheckTerminal(t *testing.T) {
	r := &fakeRunner{stdout: "/usr/bin/bash\n"}
	d := NewWithRunner(r, WSLC, config.Runtime{Session: "s1"})
	shell, err := d.CheckTerminal(context.Background(), "web")
	if err != nil || shell != "/usr/bin/bash" {
		t.Fatalf("shell = %q, %v", shell, err)
	}
	if want := "--session s1 container exec web /bin/sh -c " + shellProbe; r.last() != want {
		t.Errorf("args = %q; want %q", r.last(), want)
	}

	// Neither bash nor sh: the probe exits non-zero without an error code.
	d = NewWithRunner(&fakeRunner{code: 1}, WSLC, config.Runtime{})
	if _, err := d.CheckTerminal(context.Background(), "web"); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("no shell: err = %v; want ErrUnsupported", err)
	}
	// Unexpected output is not used as a shell path.
	d = NewWithRunner(&fakeRunner{stdout: "sh -c rm\n"}, WSLC, config.Runtime{})
	if _, err := d.CheckTerminal(context.Background(), "web"); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("bad output: err = %v; want ErrUnsupported", err)
	}
	// Container errors pass through unchanged.
	d = NewWithRunner(&fakeRunner{stderr: "Fehlercode: WSLC_E_CONTAINER_NOT_FOUND\n", code: 1}, WSLC, config.Runtime{})
	if _, err := d.CheckTerminal(context.Background(), "web"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("not found: err = %v; want ErrNotFound", err)
	}
	r = &fakeRunner{}
	d = NewWithRunner(r, WSLC, config.Runtime{})
	if _, err := d.CheckTerminal(context.Background(), "--privileged"); !errors.Is(err, runtime.ErrInvalid) || len(r.calls) != 0 {
		t.Errorf("flag as id: err = %v, calls = %v", err, r.calls)
	}
}

func TestOpenTerminal(t *testing.T) {
	r := &fakeRunner{}
	d := NewWithRunner(r, WSLC, config.Runtime{Session: "s1"})
	term, err := d.OpenTerminal(context.Background(), "web", runtime.TerminalOpts{Shell: "/bin/sh", Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	args := r.calls[0]
	tag := args[len(args)-3]
	want := "--session s1 container exec --interactive --tty --env TERM=xterm-256color --env " + tag + " web /bin/sh"
	if r.last() != want || r.ptySize != [2]uint16{120, 40} || !regexp.MustCompile(`^CONTAINERUI_TERMINAL=[0-9a-f]{32}$`).MatchString(tag) {
		t.Errorf("args = %q size = %v; want %q 120x40", r.last(), r.ptySize, want)
	}

	// Closing a live terminal kills its processes in the container by tag.
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || !strings.HasPrefix(r.last(), "--session s1 container exec web /bin/sh -c ") || !strings.HasSuffix(r.last(), " sh "+tag) {
		t.Errorf("cleanup calls = %q", r.calls)
	}
	_ = term.Close()
	if len(r.calls) != 2 {
		t.Errorf("second Close ran cleanup again: %q", r.calls)
	}

	// A shell that exited by itself needs no cleanup.
	r = &fakeRunner{}
	d = NewWithRunner(r, WSLC, config.Runtime{})
	term, err = d.OpenTerminal(context.Background(), "web", runtime.TerminalOpts{Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	r.pty.exit()
	for deadline := time.Now().Add(2 * time.Second); !term.(*execTerminal).exited.Load() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	_ = term.Close()
	if len(r.calls) != 1 {
		t.Errorf("calls after exit = %q; want no cleanup", r.calls)
	}

	for _, shell := range []string{"sh", "/bin/sh -i", "/bin/$(id)", ""} {
		if _, err := d.OpenTerminal(context.Background(), "web", runtime.TerminalOpts{Shell: shell}); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("shell %q: err = %v; want ErrInvalid", shell, err)
		}
	}
}
