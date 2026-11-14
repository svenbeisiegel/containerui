package cli

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// scriptRunner answers Run by the first script key contained in the args.
type scriptRunner struct {
	fakeRunner
	script map[string]string
}

func (s *scriptRunner) Run(_ context.Context, args []string) ([]byte, []byte, int, error) {
	s.calls = append(s.calls, args)
	joined := strings.Join(args, " ")
	best := ""
	for k := range s.script {
		if strings.Contains(joined, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, []byte("unexpected command " + joined), 1, nil
	}
	return []byte(s.script[best]), nil, 0, nil
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"0B": 0, "4.1kB": 4100, "5.24MB": 5240000, "1.914GiB": 2055141851, "496KiB": 507904,
		"4.348GB (89%)": 4348000000, "N/A": 0, "--": 0, "": 0, "12": 12, "2.316MiB": 2428502,
		"1.5 GB": 1500000000, "7x": 0,
	} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	if got := parsePercent("100.69%"); got != 100.69 {
		t.Errorf("parsePercent = %v", got)
	}
	if got := parsePercent("--"); got != 0 {
		t.Errorf("parsePercent(--) = %v", got)
	}
}

func TestDockerStats(t *testing.T) {
	r := &scriptRunner{script: map[string]string{"container stats": testdata(t, "docker_stats.ndjson")}}
	d := NewWithRunner(r, Docker, config.Runtime{})
	if !d.Capabilities().Stats {
		t.Fatal("docker should report Stats")
	}
	got, err := d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d stats", len(got))
	}
	busy := got[1]
	if busy.Name != "cuitest-busy" || busy.ID != "4b4141452e5fd908d97c443089a9bdede04f7ce6d7202225efa45dab62c20d30" {
		t.Errorf("busy = %+v", busy)
	}
	if busy.CPUPercent != 100.69 || busy.MemUsage != 2428502 || busy.MemLimit != 2055141851 ||
		busy.BlockRead != 1490000 || busy.BlockWrite != 21000000 {
		t.Errorf("busy values = %+v", busy)
	}
	if want := []string{"container", "stats", "--no-stream", "--format", "json"}; !reflect.DeepEqual(r.calls[0], want) {
		t.Errorf("args = %v", r.calls[0])
	}
}

func TestDockerDiskUsage(t *testing.T) {
	r := &scriptRunner{script: map[string]string{
		"system df --format":           testdata(t, "docker_df.ndjson"),
		"system df --verbose --format": testdata(t, "docker_df_v.json"),
	}}
	du, err := NewWithRunner(r, Docker, config.Runtime{}).DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCat := []runtime.DiskCategory{
		{Name: "Images", Size: 4854000000, Reclaimable: 4348000000},
		{Name: "Containers", Size: 5263000, Reclaimable: 4096},
		{Name: "Volumes", Size: 30460000, Reclaimable: 9484000},
		{Name: "Build cache"},
	}
	if !reflect.DeepEqual(du.Categories, wantCat) {
		t.Errorf("categories = %+v", du.Categories)
	}
	if len(du.Containers) != 3 {
		t.Fatalf("containers = %+v", du.Containers)
	}
	busy := du.Containers[2]
	if busy.Name != "cuitest-busy" || busy.Size != 8190 || !busy.Running || !reflect.DeepEqual(busy.Mounts, []string{"cuitest-data"}) {
		t.Errorf("busy = %+v", busy)
	}
	if du.Containers[0].Running {
		t.Error("exited container reported running")
	}
	if du.Volumes["cuitest-data"] != 20970000 || len(du.Volumes) != 3 {
		t.Errorf("volumes = %v", du.Volumes)
	}
}

func TestPodmanStats(t *testing.T) {
	r := &scriptRunner{script: map[string]string{
		"container stats":           testdata(t, "podman_stats.json"),
		"system df":                 testdata(t, "podman_df.json"),
		"container ls --all --size": testdata(t, "podman_ps_size.json"),
	}}
	d := NewWithRunner(r, Podman, config.Runtime{})
	st, err := d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0].Name != "web" || st[0].CPUPercent != 12.5 || st[0].MemUsage != 24580000 ||
		st[0].MemLimit != 8232000000 || st[0].BlockRead != 8192 || st[0].BlockWrite != 1049000 {
		t.Errorf("stats = %+v", st)
	}
	du, err := d.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(du.Categories) != 3 || du.Categories[0].Size != 524288000 || du.Categories[2].Name != "Volumes" {
		t.Errorf("categories = %+v", du.Categories)
	}
	if len(du.Containers) != 3 || du.Containers[1].Size != 5242880 || du.Containers[2].Size != 0 || du.Containers[2].Running {
		t.Errorf("containers = %+v", du.Containers)
	}
	if du.Volumes != nil {
		t.Errorf("podman volumes = %v", du.Volumes)
	}
}

func TestWSLCNoStats(t *testing.T) {
	d := NewWithRunner(&fakeRunner{}, WSLC, config.Runtime{})
	if d.Capabilities().Stats {
		t.Error("wslc should not report Stats")
	}
	if _, err := d.Stats(context.Background()); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}
