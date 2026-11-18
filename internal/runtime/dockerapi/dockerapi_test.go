package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Captured from Docker 29.5.2 (API 1.54): a container writing to a volume
// and one with a bind mount and the same volume.
const (
	busyID = "a0b1ed6e575b364a98ec184693e8a5db96285743ba083922b5cbe4c6abfe9474"
	bindID = "2f99a52f3283d3aaa49d3042f47e46d20dbf45e6650d4b75419425db850e9812"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// socketDir returns a directory short enough for a Unix socket path
// (macOS allows 104 bytes).
func socketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) > 80 {
		var err error
		if dir, err = os.MkdirTemp("", "dapi"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	return dir
}

// fakeDaemon serves handler on a Unix socket at path.
func fakeDaemon(t *testing.T, path string, handler http.Handler) *httptest.Server {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// daemon answers like the captured Docker: the busy container's stats come
// from the two samples in turn, the bind container is "removed".
type daemon struct {
	t     *testing.T
	mu    sync.Mutex
	calls int
	df    []byte
	paths []string
}

func (d *daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, r.URL.RequestURI())
	switch {
	case r.URL.Path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case r.URL.Path == "/containers/json":
		_, _ = w.Write(readTestdata(d.t, "containers.json"))
	case r.URL.Path == "/containers/"+busyID+"/stats":
		d.calls++
		name := "stats_busy_1.json"
		if d.calls%2 == 0 {
			name = "stats_busy_2.json"
		}
		_, _ = w.Write(readTestdata(d.t, name))
	case strings.HasSuffix(r.URL.Path, "/stats"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: x"}`))
	case r.URL.Path == "/system/df":
		df := d.df
		if df == nil {
			df = readTestdata(d.t, "system_df.json")
		}
		_, _ = w.Write(df)
	default:
		http.NotFound(w, r)
	}
}

func TestStats(t *testing.T) {
	sock := filepath.Join(socketDir(t), "d.sock")
	d := &daemon{t: t}
	fakeDaemon(t, sock, d)
	c := New(sock)
	defer c.Close()
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := c.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("stats = %+v (the removed container must be skipped)", first)
	}
	s := first[0]
	if s.ID != busyID || s.Name != "cui-api-busy" || !math.IsNaN(s.CPUPercent) {
		t.Errorf("first sample = %+v, CPU must be unknown", s)
	}
	// usage 18542592 minus inactive_file 8388608
	if s.MemUsage != 10153984 || s.MemLimit != 2054774784 || s.BlockRead != 0 || s.BlockWrite != 9202302976 {
		t.Errorf("first sample values = %+v", s)
	}

	second, err := c.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s = second[0]
	// (17175867000-12717568000) / (1265019340000000-1265009110000000) * 2 CPUs
	if want := 4458299000.0 / 10230000000 * 2 * 100; math.Abs(s.CPUPercent-want) > 1e-9 {
		t.Errorf("CPU = %v, want %v", s.CPUPercent, want)
	}
	if s.MemUsage != 17465344-8388608 || s.BlockWrite != 12507414528 {
		t.Errorf("second sample values = %+v", s)
	}
	for _, p := range d.paths {
		if strings.Contains(p, "/stats") && !strings.Contains(p, "stream=false") || strings.Contains(p, "/stats") && !strings.Contains(p, "one-shot=true") {
			t.Errorf("stats request %s is not one-shot", p)
		}
	}
}

func TestCPUPercent(t *testing.T) {
	t0 := time.Unix(1000, 0)
	for _, tc := range []struct {
		name      string
		prev, cur cpuSample
		ncpu      float64
		want      float64
	}{
		{"host counters", cpuSample{100, 1000, t0}, cpuSample{150, 1100, t0.Add(time.Second)}, 4, 200},
		{"wall clock without host counters", cpuSample{0, 0, t0}, cpuSample{5e8, 0, t0.Add(2 * time.Second)}, 4, 25},
		{"counters reset", cpuSample{500, 1000, t0}, cpuSample{100, 1100, t0.Add(time.Second)}, 1, math.NaN()},
		{"no time passed", cpuSample{0, 0, t0}, cpuSample{10, 0, t0}, 1, math.NaN()},
	} {
		got := cpuPercent(tc.prev, tc.cur, tc.ncpu)
		if math.IsNaN(tc.want) != math.IsNaN(got) || !math.IsNaN(got) && math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := memUsage(100, map[string]uint64{"total_inactive_file": 30, "inactive_file": 10}); got != 70 {
		t.Errorf("cgroup v1 mem = %d", got)
	}
	if got := memUsage(100, map[string]uint64{"inactive_file": 200}); got != 100 {
		t.Errorf("cache above usage = %d", got)
	}
}

func TestDiskUsage(t *testing.T) {
	legacy := readTestdata(t, "system_df.json")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(legacy, &doc); err != nil {
		t.Fatal(err)
	}
	// Only the summaries of newer daemons, and only the lists of older ones.
	summaries, lists := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for k, v := range doc {
		if strings.HasSuffix(k, "Usage") {
			summaries[k] = v
		} else {
			lists[k] = v
		}
	}
	marshal := func(m map[string]json.RawMessage) []byte {
		b, _ := json.Marshal(m)
		return b
	}

	for _, tc := range []struct {
		name   string
		df     []byte
		images runtime.DiskCategory
		vols   runtime.DiskCategory
	}{
		{"both", legacy,
			runtime.DiskCategory{Name: "Images", Size: 5007489894, Reclaimable: 4406418136},
			runtime.DiskCategory{Name: "Volumes", Size: 15969792, Reclaimable: 7581184}},
		{"summaries only", marshal(summaries),
			runtime.DiskCategory{Name: "Images", Size: 5007489894, Reclaimable: 4406418136},
			runtime.DiskCategory{Name: "Volumes", Size: 15969792, Reclaimable: 7581184}},
		{"lists only", marshal(lists),
			// computed: the layers minus what the image in use adds
			runtime.DiskCategory{Name: "Images", Size: 5007489894, Reclaimable: 5007489894 - (13030721 - 12942697)},
			runtime.DiskCategory{Name: "Volumes", Size: 15969792, Reclaimable: 7581184}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(socketDir(t), "d.sock")
			fakeDaemon(t, sock, &daemon{t: t, df: tc.df})
			du, err := New(sock).DiskUsage(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []runtime.DiskCategory{
				tc.images,
				{Name: "Containers", Size: 20480},
				tc.vols,
				{Name: "Build cache", Size: 4098000, Reclaimable: 4096000},
			}
			if !reflect.DeepEqual(du.Categories, want) {
				t.Errorf("categories = %+v\nwant %+v", du.Categories, want)
			}
			if len(du.Containers) != 2 {
				t.Fatalf("containers = %+v", du.Containers)
			}
			bind := du.Containers[0]
			if bind.ID != bindID || bind.Name != "cui-api-bind" || bind.Size != 12288 || !bind.Running ||
				!reflect.DeepEqual(bind.Mounts, []string{"cui-api-data"}) || !reflect.DeepEqual(bind.Binds, []string{"/srv/cuitest/config"}) {
				t.Errorf("bind container = %+v", bind)
			}
			if busy := du.Containers[1]; len(busy.Binds) != 0 || !reflect.DeepEqual(busy.Mounts, []string{"cui-api-data"}) {
				t.Errorf("busy container = %+v", busy)
			}
			if du.Volumes["cui-api-data"] != 8388608 || len(du.Volumes) != 3 {
				t.Errorf("volumes = %v", du.Volumes)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	sock := filepath.Join(socketDir(t), "d.sock")
	fakeDaemon(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"disk on fire"}`))
	}))
	_, err := New(sock).DiskUsage(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Status != 500 || e.Message != "disk on fire" || IsNotFound(err) {
		t.Errorf("err = %v", err)
	}
}

type stubCLI struct {
	mu    sync.Mutex
	calls int
}

func (s *stubCLI) Stats(context.Context) ([]runtime.ContainerStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return []runtime.ContainerStats{{ID: "cli", Name: "from-cli", CPUPercent: 1}}, nil
}

func (s *stubCLI) DiskUsage(context.Context) (runtime.DiskUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return runtime.DiskUsage{Categories: []runtime.DiskCategory{{Name: "cli"}}}, nil
}

func TestFallback(t *testing.T) {
	cli := &stubCLI{}
	if got := NewStater("", cli, nil); got != runtime.Stater(cli) {
		t.Error("no socket must use the CLI directly")
	}
	if got := NewStater(`\\.\pipe\docker_engine`, cli, nil); got != runtime.Stater(cli) {
		t.Error("a named pipe must use the CLI directly")
	}

	sock := filepath.Join(socketDir(t), "d.sock")
	now := time.Unix(1000, 0)
	s := NewStater(sock, cli, nil).(*Stater)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	// No daemon yet: the CLI answers, and keeps answering without the API
	// being tried again until retryAPI passed.
	st, err := s.Stats(ctx)
	if err != nil || len(st) != 1 || st[0].Name != "from-cli" || s.Source() != "cli" {
		t.Fatalf("stats = %+v, %v, source %s", st, err, s.Source())
	}
	d := &daemon{t: t}
	fakeDaemon(t, sock, d)
	now = now.Add(retryAPI - time.Second)
	if _, err := s.DiskUsage(ctx); err != nil || cli.calls != 2 || len(d.paths) != 0 {
		t.Errorf("API tried again too early: cli calls %d, api %v", cli.calls, d.paths)
	}

	now = now.Add(2 * time.Second)
	st, err = s.Stats(ctx)
	if err != nil || len(st) != 1 || st[0].ID != busyID || s.Source() != "api" || cli.calls != 2 {
		t.Errorf("after retry: %+v, %v, source %s, cli calls %d", st, err, s.Source(), cli.calls)
	}

	// A cancelled call is not a reason to switch.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Stats(cctx); err == nil || s.Source() != "api" || cli.calls != 2 {
		t.Errorf("cancelled: err %v, source %s, cli calls %d", err, s.Source(), cli.calls)
	}
}
