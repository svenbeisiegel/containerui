package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
)

// fakeRunner stands in for the CLI and records what it was asked.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string // stdout by "verb noun" (e.g. "container ls")
}

func (r *fakeRunner) record(args []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := strings.Join(args, " ")
	r.calls = append(r.calls, line)
	if len(args) >= 2 {
		return r.out[args[0]+" "+args[1]]
	}
	return ""
}

func (r *fakeRunner) called() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *fakeRunner) Run(_ context.Context, args []string) ([]byte, []byte, int, error) {
	return []byte(r.record(args)), nil, 0, nil
}

func (r *fakeRunner) Stream(_ context.Context, args []string, onLine func(string)) ([]byte, int, error) {
	r.record(args)
	return nil, 0, nil
}

func (r *fakeRunner) Pipe(_ context.Context, args []string, w io.Writer) (int, error) {
	_, err := io.WriteString(w, r.record(args))
	return 0, err
}

func (r *fakeRunner) Exchange(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, int, error) {
	r.record(args)
	return nil, 0, nil
}

func (r *fakeRunner) StartPTY(args []string, _, _ uint16) (runtime.Terminal, error) {
	r.record(args)
	return nil, errors.New("no pseudo terminal in tests")
}

// newDriver returns a Driver for flavor on a fake daemon serving handler
// (nil: no daemon listens on the socket).
func newDriver(t *testing.T, flavor *cli.Flavor, handler http.Handler, o Options) (*Driver, *fakeRunner) {
	t.Helper()
	sock := filepath.Join(socketDir(t), "d.sock")
	if handler != nil {
		fakeDaemon(t, sock, handler)
	}
	r := &fakeRunner{out: map[string]string{}}
	d := Wrap(cli.NewWithRunner(r, flavor, config.Runtime{}), sock, o)
	if d == nil {
		t.Fatal("Wrap returned nil")
	}
	return d, r
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// frame encodes one frame of a multiplexed stream.
func frame(stream byte, s string) []byte {
	b := make([]byte, 8, 8+len(s))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	return append(b, s...)
}

// hijack takes over the connection of r and answers the upgrade; serve
// then talks the raw stream.
func hijack(t *testing.T, w http.ResponseWriter, serve func(conn net.Conn, rw *bufio.ReadWriter)) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	_ = rw.Flush()
	serve(conn, rw)
}

func TestWrap(t *testing.T) {
	r := &fakeRunner{}
	if Wrap(cli.NewWithRunner(r, cli.WSLC, config.Runtime{}), "/x.sock", Options{}) != nil {
		t.Error("wslc wrapped")
	}
	if Wrap(cli.NewWithRunner(r, cli.Docker, config.Runtime{}), "", Options{}) != nil {
		t.Error("wrapped without a socket")
	}
	if Wrap(cli.NewWithRunner(r, cli.Docker, config.Runtime{}), `\\.\pipe\docker_engine`, Options{}) != nil {
		t.Error("named pipe wrapped")
	}
}

func TestListContainers(t *testing.T) {
	created := time.Now().Add(-26 * time.Hour).Unix()
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = fmt.Fprintf(w, `[
 {"Id":"4f1c","Names":["/web"],"Image":"nginx:1.27","ImageID":"sha256:aa","Command":"/docker-entrypoint.sh nginx -g 'daemon off;'",
  "Created":%d,"Ports":[{"IP":"::","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}],
  "Labels":{"maintainer":"NGINX Docker Maintainers"},"State":"running","Status":"Up 26 hours",
  "NetworkSettings":{"Networks":{"frontend":{"NetworkID":"n2"},"bridge":{"NetworkID":"n1"}}},"Mounts":[]},
 {"Id":"9a8b","Names":["/tmp"],"Image":"alpine:latest","Command":"sh","Created":%d,"Ports":[],"Labels":{},
  "State":"exited","Status":"Exited (0) 40 hours ago","NetworkSettings":{"Networks":{"bridge":{}}},
  "Mounts":[{"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data"},{"Type":"bind","Source":"/srv/conf"}]}
]`, created, created)
	})
	d, r := newDriver(t, cli.Docker, mux, Options{})
	cs, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if query != "all=1" {
		t.Errorf("query %q", query)
	}
	want := []runtime.Container{{
		ID: "4f1c", Name: "web", Image: "nginx:1.27", Command: "/docker-entrypoint.sh nginx -g 'daemon off;'",
		State: runtime.StateRunning, Status: "Up 26 hours",
		// As the docker CLI prints it (testdata/docker_containers.ndjson of package cli).
		Ports:    "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp",
		Networks: []string{"bridge", "frontend"}, Created: time.Unix(created, 0), RunningFor: "26 hours ago",
		Labels: map[string]string{"maintainer": "NGINX Docker Maintainers"},
	}, {
		ID: "9a8b", Name: "tmp", Image: "alpine:latest", Command: "sh", State: runtime.StateExited,
		Status: "Exited (0) 40 hours ago", Networks: []string{"bridge"}, Mounts: []string{"/srv/conf", "data"},
		Created: time.Unix(created, 0), RunningFor: "26 hours ago",
	}}
	if !reflect.DeepEqual(cs, want) {
		t.Errorf("containers =\n%+v\nwant\n%+v", cs, want)
	}
	if calls := r.called(); len(calls) != 0 {
		t.Errorf("CLI called: %v", calls)
	}
	if d.Source() != "api" {
		t.Errorf("source %q", d.Source())
	}
}

func TestDisplayPorts(t *testing.T) {
	for _, tc := range []struct {
		ports []portJSON
		want  string
	}{
		{nil, ""},
		{[]portJSON{{PrivatePort: 443, Type: "tcp"}, {PrivatePort: 80, Type: "tcp"}}, "80/tcp, 443/tcp"},
		{[]portJSON{{PrivatePort: 53, Type: "udp"}, {PrivatePort: 53}}, "53/tcp, 53/udp"},
		{[]portJSON{
			{IP: "0.0.0.0", PrivatePort: 8000, PublicPort: 8000, Type: "tcp"},
			{IP: "0.0.0.0", PrivatePort: 8001, PublicPort: 8001, Type: "tcp"},
			{IP: "0.0.0.0", PrivatePort: 8002, PublicPort: 8002, Type: "tcp"},
		}, "0.0.0.0:8000-8002->8000-8002/tcp"},
		{[]portJSON{
			{IP: "127.0.0.1", PrivatePort: 5432, PublicPort: 15432, Type: "tcp"},
			{PrivatePort: 9000, Type: "tcp"},
		}, "9000/tcp, 127.0.0.1:15432->5432/tcp"},
	} {
		if got := displayPorts(tc.ports); got != tc.want {
			t.Errorf("displayPorts(%+v) = %q, want %q", tc.ports, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                    "Less than a second",
		time.Second:          "1 second",
		45 * time.Second:     "45 seconds",
		90 * time.Second:     "About a minute",
		10 * time.Minute:     "10 minutes",
		59 * time.Minute:     "59 minutes",
		70 * time.Minute:     "About an hour",
		26 * time.Hour:       "26 hours",
		3 * 24 * time.Hour:   "3 days",
		21 * 24 * time.Hour:  "3 weeks",
		90 * 24 * time.Hour:  "3 months",
		800 * 24 * time.Hour: "2 years",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestListImages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[
 {"Id":"sha256:a1","RepoTags":["alpine:latest","alpine:3.22"],"RepoDigests":["alpine@sha256:d1"],"Created":1760000000,"Size":8420000,"Containers":-1},
 {"Id":"sha256:b2","RepoTags":[],"RepoDigests":["ghcr.io/x/app@sha256:d2"],"Created":1760000000,"Size":202000000,"Containers":-1},
 {"Id":"sha256:c3","RepoTags":["<none>:<none>"],"RepoDigests":["<none>@<none>"],"Created":1760000000,"Size":999,"Containers":-1}
]`)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "all=1" {
			t.Errorf("container count without all: %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `[{"Id":"c1","ImageID":"sha256:a1"},{"Id":"c2","ImageID":"sha256:a1"},{"Id":"c3","ImageID":"sha256:b2"}]`)
	})
	created := time.Unix(1760000000, 0)
	tagged := []runtime.Image{
		{ID: "sha256:a1", Repository: "alpine", Tag: "latest", Size: "8.42MB", Created: created, Containers: 2},
		{ID: "sha256:a1", Repository: "alpine", Tag: "3.22", Size: "8.42MB", Created: created, Containers: 2},
	}
	untagged := []runtime.Image{
		{ID: "sha256:b2", Repository: "ghcr.io/x/app", Tag: "<none>", Size: "202MB", Created: created, Containers: 1},
		{ID: "sha256:c3", Repository: "<none>", Tag: "<none>", Size: "999B", Created: created},
	}
	// docker's CLI hides untagged images, podman's lists them.
	for _, tc := range []struct {
		flavor *cli.Flavor
		want   []runtime.Image
	}{
		{cli.Docker, tagged},
		{cli.Podman, append(append([]runtime.Image(nil), tagged...), untagged...)},
	} {
		d, _ := newDriver(t, tc.flavor, mux, Options{})
		imgs, err := d.ListImages(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(imgs, tc.want) {
			t.Errorf("%s images =\n%+v\nwant\n%+v", tc.flavor.Name, imgs, tc.want)
		}
	}
}

func TestListVolumesAndNetworks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Volumes":[
 {"Name":"data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/data/_data","Scope":"local","Labels":null,"Options":null},
 {"Name":"nfs","Driver":"local","Mountpoint":"/var/lib/docker/volumes/nfs/_data","Scope":"local","Labels":{"a":"b"},"Options":{"device":":/export","type":"nfs"}}
],"Warnings":null}`)
	})
	mux.HandleFunc("GET /networks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"Name":"bridge","Id":"f19f","Created":"2026-10-05T06:34:22.488226783Z","Scope":"local","Driver":"bridge","EnableIPv6":false,"Internal":false,"Labels":{}},
 {"Name":"back","Id":"0c16","Created":"2026-10-04T18:26:02Z","Scope":"local","Driver":"bridge","EnableIPv6":true,"Internal":true,"Labels":{"x":"y"}}]`)
	})
	d, _ := newDriver(t, cli.Docker, mux, Options{})
	vols, err := d.ListVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantVols := []runtime.Volume{
		{Name: "data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/data/_data", Scope: "local", Options: map[string]string{}},
		{Name: "nfs", Driver: "local", Mountpoint: "/var/lib/docker/volumes/nfs/_data", Scope: "local", Labels: map[string]string{"a": "b"},
			Options: map[string]string{"device": ":/export", "type": "nfs"}},
	}
	if !reflect.DeepEqual(vols, wantVols) {
		t.Errorf("volumes =\n%+v\nwant\n%+v", vols, wantVols)
	}
	if vols[1].DataLocation() != ":/export" {
		t.Errorf("data location %q", vols[1].DataLocation())
	}
	nets, err := d.ListNetworks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantNets := []runtime.Network{
		{ID: "f19f", Name: "bridge", Driver: "bridge", Scope: "local", Created: time.Date(2026, 10, 5, 6, 34, 22, 488226783, time.UTC)},
		{ID: "0c16", Name: "back", Driver: "bridge", Scope: "local", Internal: true, IPv6: true,
			Created: time.Date(2026, 10, 4, 18, 26, 2, 0, time.UTC), Labels: map[string]string{"x": "y"}},
	}
	if !reflect.DeepEqual(nets, wantNets) {
		t.Errorf("networks =\n%+v\nwant\n%+v", nets, wantNets)
	}
}

func TestInspect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "abc" {
			writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
			return
		}
		_, _ = io.WriteString(w, `{"Id":"abc","Name":"/web"}`)
	})
	mux.HandleFunc("GET /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ref") != "ghcr.io/x/app:1.0/json" {
			t.Errorf("image path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"Id":"sha256:1"}`)
	})

	d, r := newDriver(t, cli.Docker, mux, Options{})
	ctx := context.Background()
	doc, err := d.InspectContainer(ctx, "abc")
	if err != nil || string(doc) != `{"Id":"abc","Name":"/web"}` {
		t.Errorf("inspect = %s, %v", doc, err)
	}
	if doc, err := d.InspectImage(ctx, "ghcr.io/x/app:1.0"); err != nil || string(doc) != `{"Id":"sha256:1"}` {
		t.Errorf("image inspect = %s, %v", doc, err)
	}

	// The daemon's answer is final: a 404 is not asked again on the CLI.
	_, err = d.InspectContainer(ctx, "gone")
	var ce *runtime.CommandError
	if !errors.Is(err, runtime.ErrNotFound) || !errors.As(err, &ce) || ce.Output != "No such container: gone" || ce.Op != "container inspect" {
		t.Errorf("unknown container: %v", err)
	}
	if calls := r.called(); len(calls) != 0 {
		t.Errorf("CLI called: %v", calls)
	}
	if d.Source() != "api" {
		t.Errorf("source %q after a daemon error", d.Source())
	}

	// Podman's CLI prints its own inspect format: the CLI answers.
	p, pr := newDriver(t, cli.Podman, mux, Options{})
	pr.out["container inspect"] = `[{"Id":"abc","from":"cli"}]`
	if doc, err := p.InspectContainer(ctx, "abc"); err != nil || !strings.Contains(string(doc), `"from":"cli"`) {
		t.Errorf("podman inspect = %s, %v", doc, err)
	}
}

func TestErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		status int
		msg    string
		want   error
	}{
		{404, "No such image: x", runtime.ErrNotFound},
		{409, "container is running", runtime.ErrConflict},
		{400, "bad parameter", runtime.ErrInvalid},
		{400, "invalid reference format", runtime.ErrInvalid},
		{500, "volume is in use", runtime.ErrConflict},
		{500, "something broke", nil},
	} {
		e := &Error{Status: tc.status, Message: tc.msg}
		if got := e.Kind(); got != tc.want {
			t.Errorf("%d %q: kind %v, want %v", tc.status, tc.msg, got, tc.want)
		}
	}
}

func TestFallbackToCLI(t *testing.T) {
	// No daemon listens: the CLI answers, and keeps answering for retryAPI.
	d, r := newDriver(t, cli.Docker, nil, Options{})
	r.out["container ls"] = `{"ID":"c1","Names":"from-cli","State":"running"}`
	now := time.Unix(1000, 0)
	d.fb.now = func() time.Time { return now }
	ctx := context.Background()
	for range 2 {
		cs, err := d.ListContainers(ctx, runtime.ListContainersOpts{})
		if err != nil || len(cs) != 1 || cs[0].Name != "from-cli" {
			t.Fatalf("containers = %+v, %v", cs, err)
		}
	}
	if d.Source() != "cli" || len(r.called()) != 2 {
		t.Errorf("source %q, CLI calls %v", d.Source(), r.called())
	}
}

func TestNoRetryOfChanges(t *testing.T) {
	// The connection breaks after the pull reached the daemon: the CLI
	// must not pull again.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	d, r := newDriver(t, cli.Docker, mux, Options{})
	if err := d.PullImage(context.Background(), "alpine:latest", nil); err == nil {
		t.Error("broken pull succeeded")
	}
	if calls := r.called(); len(calls) != 0 {
		t.Errorf("CLI called: %v", calls)
	}

	// The socket cannot be reached at all: nothing was sent, the CLI pulls.
	d, r = newDriver(t, cli.Docker, nil, Options{})
	if err := d.PullImage(context.Background(), "alpine:latest", nil); err != nil {
		t.Fatal(err)
	}
	if calls := r.called(); len(calls) != 1 || calls[0] != "image pull alpine:latest" {
		t.Errorf("CLI calls %v", calls)
	}
}

func TestLogs(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprint("tty=", tty), func(t *testing.T) {
			var query string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /containers/c1/json", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]any{"Config": map[string]any{"Tty": tty}})
			})
			mux.HandleFunc("GET /containers/c1/logs", func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.RawQuery
				if tty {
					_, _ = io.WriteString(w, "raw out\r\nraw err\r\n")
					return
				}
				_, _ = w.Write(append(frame(1, "out 1\n"), append(frame(2, "err 1\n"), frame(1, "out 2\n")...)...))
			})
			d, _ := newDriver(t, cli.Docker, mux, Options{})
			var buf bytes.Buffer
			if err := d.ContainerLogs(context.Background(), "c1", runtime.LogsOpts{Follow: true, Tail: 50}, &buf); err != nil {
				t.Fatal(err)
			}
			want := "out 1\nerr 1\nout 2\n"
			if tty {
				want = "raw out\r\nraw err\r\n"
			}
			if buf.String() != want {
				t.Errorf("logs %q, want %q", buf.String(), want)
			}
			if query != "follow=1&stderr=1&stdout=1&tail=50" {
				t.Errorf("query %q", query)
			}
		})
	}
}

func TestDemux(t *testing.T) {
	var out, errOut bytes.Buffer
	in := append(frame(1, "a"), append(frame(2, "b"), frame(1, strings.Repeat("c", 40000))...)...)
	if err := demux(bytes.NewReader(in), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a"+strings.Repeat("c", 40000) || errOut.String() != "b" {
		t.Errorf("out %d bytes, err %q", out.Len(), errOut.String())
	}
	if err := demux(bytes.NewReader(frame(1, "abc")[:9]), &out, &errOut); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated frame: %v", err)
	}
	if err := demux(bytes.NewReader(frame(3, "exec failed")), &out, &errOut); err == nil || err.Error() != "exec failed" {
		t.Errorf("error frame: %v", err)
	}
}

func TestPull(t *testing.T) {
	var query, auth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		query, auth = r.URL.RawQuery, r.Header.Get("X-Registry-Auth")
		for _, m := range []string{
			`{"status":"Pulling from x/app","id":"1.0"}`,
			`{"status":"Pulling fs layer","progressDetail":{},"id":"abc"}`,
			`{"status":"Downloading","progressDetail":{"current":1,"total":9},"progress":"[=>   ] 1B/9B","id":"abc"}`,
			`{"status":"Pull complete","progressDetail":{},"id":"abc"}`,
			`{"status":"Status: Downloaded newer image for ghcr.io/x/app:1.0"}`,
		} {
			_, _ = io.WriteString(w, m+"\n")
		}
	})
	creds := func(reg string) (string, string, error) {
		if reg == "ghcr.io" {
			return "me", "pw", nil
		}
		return "", "", nil
	}
	d, _ := newDriver(t, cli.Docker, mux, Options{Credentials: creds})
	var lines []string
	if err := d.PullImage(context.Background(), "ghcr.io/x/app:1.0", func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"1.0: Pulling from x/app", "abc: Pulling fs layer", "abc: Pull complete", "Status: Downloaded newer image for ghcr.io/x/app:1.0"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("progress %q", lines)
	}
	if query != "fromImage=ghcr.io%2Fx%2Fapp&tag=1.0" {
		t.Errorf("query %q", query)
	}
	b, err := base64.URLEncoding.DecodeString(auth)
	var a map[string]string
	if err != nil || json.Unmarshal(b, &a) != nil || a["username"] != "me" || a["password"] != "pw" || a["serveraddress"] != "ghcr.io" {
		t.Errorf("auth %q (%s)", auth, b)
	}
}

func TestPullError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"Pulling from library/nope","id":"latest"}`+"\n"+
			`{"errorDetail":{"message":"pull access denied for nope, repository does not exist"},"error":"pull access denied for nope, repository does not exist"}`+"\n")
	})
	d, _ := newDriver(t, cli.Docker, mux, Options{})
	err := d.PullImage(context.Background(), "nope", nil)
	var ce *runtime.CommandError
	if !errors.As(err, &ce) || !errors.Is(err, runtime.ErrNotFound) || !strings.Contains(ce.Output, "pull access denied") {
		t.Errorf("pull error: %v", err)
	}
}

func TestPullCredentialHelper(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		t.Error("API pull despite a credential helper")
	})
	creds := func(string) (string, string, error) { return "", "", registry.ErrCredentialHelper }
	d, r := newDriver(t, cli.Docker, mux, Options{Credentials: creds})
	if err := d.PullImage(context.Background(), "private.example/app:1", nil); err != nil {
		t.Fatal(err)
	}
	if calls := r.called(); len(calls) != 1 || calls[0] != "image pull private.example/app:1" {
		t.Errorf("CLI calls %v", calls)
	}
}

func TestPullQuery(t *testing.T) {
	for ref, want := range map[string]string{
		"alpine":                         "fromImage=alpine&tag=latest",
		"localhost:5000/app":             "fromImage=localhost%3A5000%2Fapp&tag=latest",
		"localhost:5000/app:2":           "fromImage=localhost%3A5000%2Fapp&tag=2",
		"alpine@sha256:abc":              "fromImage=alpine&tag=sha256%3Aabc",
		"ghcr.io/x/app:1.0@sha256:def12": "fromImage=ghcr.io%2Fx%2Fapp&tag=sha256%3Adef12",
	} {
		if got := pullQuery(ref).Encode(); got != want {
			t.Errorf("pullQuery(%q) = %q, want %q", ref, got, want)
		}
	}
}

// execDaemon fakes the exec endpoints: the terminal exec echoes its input
// until it reads "q", other execs print stdout and exit with 0.
type execDaemon struct {
	t       *testing.T
	mu      sync.Mutex
	creates []execConfig
	resizes []string
	stdout  string
	codes   map[string]int // exit code by exec ID (default 0)
}

func (e *execDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "stopped" {
			writeError(w, http.StatusConflict, "container stopped is not running")
			return
		}
		var cfg execConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		e.mu.Lock()
		e.creates = append(e.creates, cfg)
		id := fmt.Sprint("e", len(e.creates))
		e.mu.Unlock()
		writeJSON(w, map[string]string{"Id": id})
	})
	mux.HandleFunc("POST /exec/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Tty bool }
		_ = json.NewDecoder(r.Body).Decode(&body)
		hijack(e.t, w, func(conn net.Conn, rw *bufio.ReadWriter) {
			if !body.Tty {
				_, _ = rw.Write(frame(1, e.stdout))
				_ = rw.Flush()
				return
			}
			buf := make([]byte, 64)
			for {
				n, err := rw.Read(buf)
				if n > 0 {
					if strings.Contains(string(buf[:n]), "q") {
						return
					}
					_, _ = rw.Write(buf[:n])
					_ = rw.Flush()
				}
				if err != nil {
					return
				}
			}
		})
	})
	mux.HandleFunc("POST /exec/{id}/resize", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.resizes = append(e.resizes, r.PathValue("id")+" "+r.URL.RawQuery)
		e.mu.Unlock()
	})
	mux.HandleFunc("GET /exec/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		code := e.codes[r.PathValue("id")]
		e.mu.Unlock()
		writeJSON(w, map[string]any{"Running": false, "ExitCode": code})
	})
	return mux
}

func (e *execDaemon) state() ([]execConfig, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]execConfig(nil), e.creates...), append([]string(nil), e.resizes...)
}

func TestTerminal(t *testing.T) {
	e := &execDaemon{t: t, codes: map[string]int{"e1": 3}}
	d, r := newDriver(t, cli.Docker, e.handler(), Options{})
	term, err := d.OpenTerminal(context.Background(), "c1", runtime.TerminalOpts{Shell: "/bin/bash", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := term.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	if err := term.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	_, _ = term.Write([]byte("q"))
	if _, err := io.ReadAll(term); err != nil {
		t.Fatal(err)
	}
	if code, err := term.Wait(); code != 3 || err != nil {
		t.Errorf("wait = %d, %v", code, err)
	}
	if err := term.Close(); err != nil {
		t.Error(err)
	}
	creates, resizes := e.state()
	if len(creates) != 1 {
		t.Fatalf("execs %+v (cleanup after the shell exited?)", creates)
	}
	c := creates[0]
	if !c.Tty || !c.AttachStdin || !reflect.DeepEqual(c.Cmd, []string{"/bin/bash"}) || len(c.Env) != 2 ||
		c.Env[0] != "TERM=xterm-256color" || !strings.HasPrefix(c.Env[1], cli.TerminalTagVar+"=") ||
		c.ConsoleSize == nil || *c.ConsoleSize != [2]uint16{30, 100} {
		t.Errorf("exec %+v", c)
	}
	if !contains(resizes, "e1 h=40&w=120") {
		t.Errorf("resizes %v", resizes)
	}
	if calls := r.called(); len(calls) != 0 {
		t.Errorf("CLI called: %v", calls)
	}
}

func TestTerminalCloseEndsShell(t *testing.T) {
	e := &execDaemon{t: t}
	d, _ := newDriver(t, cli.Docker, e.handler(), Options{})
	term, err := d.OpenTerminal(context.Background(), "c1", runtime.TerminalOpts{Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	creates, _ := e.state()
	if len(creates) != 2 {
		t.Fatalf("execs %+v, want the shell and its cleanup", creates)
	}
	tag := creates[0].Env[1]
	if want := []string{"/bin/sh", "-c", cli.TerminalKill, "sh", tag}; !reflect.DeepEqual(creates[1].Cmd, want) || creates[1].Tty {
		t.Errorf("cleanup exec %+v", creates[1])
	}
}

func TestCheckTerminal(t *testing.T) {
	e := &execDaemon{t: t, stdout: "/usr/bin/bash\n"}
	d, _ := newDriver(t, cli.Docker, e.handler(), Options{})
	ctx := context.Background()
	if shell, err := d.CheckTerminal(ctx, "c1"); shell != "/usr/bin/bash" || err != nil {
		t.Errorf("shell %q, %v", shell, err)
	}
	if creates, _ := e.state(); len(creates) != 1 || creates[0].Cmd[2] != cli.ShellProbe {
		t.Errorf("execs %+v", creates)
	}
	if _, err := d.CheckTerminal(ctx, "stopped"); !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("stopped container: %v", err)
	}
	e.stdout = ""
	if _, err := d.CheckTerminal(ctx, "c1"); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("no shell: %v", err)
	}
}

func TestAttach(t *testing.T) {
	var (
		mu      sync.Mutex
		query   string
		started bool
		gotKeys bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"Config": map[string]any{"Tty": r.PathValue("id") == "tty"}, "State": map[string]any{"Running": true}})
	})
	mux.HandleFunc("POST /containers/tty/start", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started = true
		mu.Unlock()
		w.WriteHeader(http.StatusNotModified) // already running
	})
	mux.HandleFunc("POST /containers/tty/resize", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /containers/tty/attach", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.RawQuery
		mu.Unlock()
		hijack(t, w, func(conn net.Conn, rw *bufio.ReadWriter) {
			_, _ = rw.WriteString("$ ")
			_ = rw.Flush()
			buf := make([]byte, 16)
			for {
				n, err := rw.Read(buf)
				if strings.Contains(string(buf[:n]), runtime.DetachSequence) {
					mu.Lock()
					gotKeys = true
					mu.Unlock()
					return // the daemon detaches
				}
				if err != nil {
					return
				}
			}
		})
	})
	d, r := newDriver(t, cli.Docker, mux, Options{})
	ctx := context.Background()
	term, err := d.OpenAttach(ctx, "tty", runtime.AttachOpts{Start: true, Stdin: true, TTY: true, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if n, err := term.Read(buf); err != nil || string(buf[:n]) != "$ " {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	if err := term.Close(); err != nil {
		t.Error(err)
	}
	if code, err := term.Wait(); code != 0 || err != nil {
		t.Errorf("wait = %d, %v (still running: detached)", code, err)
	}
	mu.Lock()
	if !started || !gotKeys || query != "detachKeys=ctrl-p%2Cctrl-q&stderr=1&stdin=1&stdout=1&stream=1" {
		t.Errorf("started %v, detach keys %v, query %q", started, gotKeys, query)
	}
	mu.Unlock()
	if calls := r.called(); len(calls) != 0 {
		t.Errorf("CLI called: %v", calls)
	}

	// Without a TTY the CLI attaches on its pseudo terminal.
	if _, err := d.OpenAttach(ctx, "plain", runtime.AttachOpts{Stdin: true}); err == nil {
		t.Error("the fake CLI has no pseudo terminal")
	}
	if calls := r.called(); len(calls) != 1 || !strings.HasPrefix(calls[0], "container attach") {
		t.Errorf("CLI calls %v", calls)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
