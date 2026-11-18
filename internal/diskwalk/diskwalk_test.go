package diskwalk

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// du is the expected size of paths measured on their own.
func du(t *testing.T, paths ...string) uint64 {
	t.Helper()
	var total uint64
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += stat(fi).alloc
	}
	return total
}

func TestMeasure(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	data := filepath.Join(app, "data")
	write(t, filepath.Join(app, "config.yaml"), 3000)
	write(t, filepath.Join(data, "db.bin"), 200_000)
	write(t, filepath.Join(data, "sub", "x"), 5000)
	single := filepath.Join(root, "single.conf")
	write(t, single, 100)

	got := Measure(context.Background(), []string{app, data, single, data, filepath.Join(root, "nope"), "/proc/1", "/"}, nil)

	wantData := du(t, data, filepath.Join(data, "db.bin"), filepath.Join(data, "sub"), filepath.Join(data, "sub", "x"))
	wantApp := du(t, app, filepath.Join(app, "config.yaml")) // data is counted for the deeper bind
	if got[data] != (runtime.BindSize{Size: wantData}) {
		t.Errorf("data = %+v, want %d", got[data], wantData)
	}
	if got[app] != (runtime.BindSize{Size: wantApp}) {
		t.Errorf("app = %+v, want %d (without data)", got[app], wantApp)
	}
	if got[single] != (runtime.BindSize{Size: du(t, single)}) || got[single].Size == 0 {
		t.Errorf("single file = %+v", got[single])
	}
	if got[filepath.Join(root, "nope")].Note != runtime.BindMissing {
		t.Errorf("missing = %+v", got[filepath.Join(root, "nope")])
	}
	for _, p := range []string{"/proc/1", "/"} {
		if got[p].Note != runtime.BindSkipped || got[p].Size != 0 {
			t.Errorf("%s = %+v, want skipped", p, got[p])
		}
	}
	if len(got) != 6 {
		t.Errorf("results = %v", got)
	}
}

func TestHardLinksCountOnce(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("no file IDs: hard links count per link")
	}
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	write(t, filepath.Join(a, "big"), 100_000)
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(a, "big"), filepath.Join(b, "big")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(a, "big"), filepath.Join(a, "big2")); err != nil {
		t.Fatal(err)
	}
	got := Measure(context.Background(), []string{a, b}, nil)
	file := du(t, filepath.Join(a, "big"))
	if got[a].Size != du(t, a)+file || got[b].Size != du(t, b) {
		t.Errorf("a = %+v, b = %+v; the file (%d) must count once", got[a], got[b], file)
	}
}

func TestUnreadableIsPartial(t *testing.T) {
	if goruntime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions and a non-root user")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "ok"), 1000)
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "secret"), 1000)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	got := Measure(context.Background(), []string{root}, nil)[root]
	if got.Note != runtime.BindPartial || got.Size == 0 {
		t.Errorf("got %+v, want a partial size", got)
	}
}

func TestCancelled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "f"), 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Measure(ctx, []string{root}, nil)[root]; got.Note != runtime.BindPartial {
		t.Errorf("got %+v", got)
	}
}

func TestSkipped(t *testing.T) {
	for path, want := range map[string]bool{
		"/": true, "/etc": true, "/etc/localtime": false, "/home": true, "/home/u/data": false,
		"/var/run/docker.sock": true, "/run/podman/podman.sock": true, "/proc": true, "/sys/fs/cgroup": true,
		"/dev/null": true, "/var/lib/docker/volumes/x/_data": true, "/var/lib/dockerish": false,
		"/srv": true, "/srv/app": false, "/var/log": true, "/var/log/app": false, "/data": false,
	} {
		if got := Skipped(path, nil); got != want {
			t.Errorf("Skipped(%s) = %v, want %v", path, got, want)
		}
	}
	if !Skipped("/data/docker-root/overlay", []string{"/data/docker-root"}) {
		t.Error("extra skip path not applied")
	}
}

func TestSkippedWindows(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows paths")
	}
	for path, want := range map[string]bool{
		`C:\`: true, `d:\`: true, `C:\Windows`: true, `C:\Windows\System32\drivers`: true,
		`C:\Users`: true, `c:\users`: true, `C:\Users\u\data`: false, `C:\Program Files`: true,
		`C:\ProgramData`: true, `C:\ProgramData\Docker\volumes\x`: true, `C:\ProgramData\app`: false,
		`D:\data`: false, `D:\Users`: true, `\\host\share`: true, `\\host\share\data`: false,
		`C:\windowsish`: false,
	} {
		if got := Skipped(path, nil); got != want {
			t.Errorf("Skipped(%s) = %v, want %v", path, got, want)
		}
	}
	if !Skipped(`D:\Docker-Root\overlay`, []string{`d:\docker-root`}) {
		t.Error("extra skip path not applied")
	}
	if Skipped(`C:\docker-root\overlay`, []string{`D:\docker-root`}) {
		t.Error("extra skip path applied on another drive")
	}
}
