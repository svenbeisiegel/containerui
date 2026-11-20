package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestListVolumeDirArgs(t *testing.T) {
	out := "41ed 4096 70 71 1700000000 ./conf\n" +
		"81a4 6 0 0 1700000001 ./.hidden\n" +
		"81a4 6 0 0 1700000002 ./new\nline\n" +
		"89e8 8 1234 5678 1700000003 ./run.sh\n" +
		"a1ff 12 0 0 1700000004 ./link\n" +
		"\x00./link\x00conf/app.ini\x00"
	r := &fakeRunner{queue: []string{inspectData}, exchangeOut: out}
	d := NewWithRunner(r, Docker, config.Runtime{})
	entries, err := d.ListVolumeDir(context.Background(), "data", "a/../b c")
	if err != nil {
		t.Fatal(err)
	}
	args := r.calls[1]
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--label containerui.helper=volume-files --volume data:/data:ro --entrypoint sh docker.io/library/busybox:stable -c ") ||
		args[len(args)-2] != "sh" || args[len(args)-1] != "./b c" {
		t.Errorf("args = %q", args)
	}
	if strings.Contains(args[len(args)-3], "b c") {
		t.Error("the path is part of the script")
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %+v", entries)
	}
	want := map[string]func(runtime.VolumeEntry) bool{
		"conf": func(e runtime.VolumeEntry) bool {
			return e.Mode.IsDir() && e.Mode.Perm() == 0o755 && e.UID == 70 && e.GID == 71
		},
		".hidden":   func(e runtime.VolumeEntry) bool { return e.Mode.IsRegular() && e.Size == 6 },
		"new\nline": func(e runtime.VolumeEntry) bool { return e.ModTime.Unix() == 1700000002 },
		"run.sh": func(e runtime.VolumeEntry) bool {
			return e.Mode&fs.ModeSetuid != 0 && e.Mode&fs.ModeSetgid == 0 && e.Mode.Perm() == 0o750
		},
		"link": func(e runtime.VolumeEntry) bool { return e.Mode&fs.ModeSymlink != 0 && e.Link == "conf/app.ini" },
	}
	for _, e := range entries {
		if ok := want[e.Name]; ok == nil || !ok(e) {
			t.Errorf("entry %+v (%v)", e, e.Mode)
		}
	}
}

func TestReadVolumeFileHeader(t *testing.T) {
	r := &fakeRunner{queue: []string{inspectData}, exchangeOut: "81a0 11 1234 5678 1700000000\nhello\nworld"}
	d := NewWithRunner(r, Podman, config.Runtime{})
	var meta runtime.VolumeEntry
	var body strings.Builder
	err := d.ReadVolumeFile(context.Background(), "data", "/conf/app.ini", 100, func(e runtime.VolumeEntry) (io.Writer, error) {
		meta = e
		return &body, nil
	})
	if err != nil || meta.Name != "app.ini" || meta.UID != 1234 || meta.Mode.Perm() != 0o640 || body.String() != "hello\nworld" {
		t.Errorf("read = %+v %q %v", meta, body.String(), err)
	}
	args := r.calls[1]
	if args[len(args)-2] != "./conf/app.ini" || args[len(args)-1] != "100" || !strings.Contains(strings.Join(args, " "), "data:/data:ro") {
		t.Errorf("args = %q", args)
	}
}

func TestWriteVolumeFileArgs(t *testing.T) {
	r := &fakeRunner{queue: []string{inspectData}, exchangeOut: "81a4 3 1 2 1700000000\n"}
	d := NewWithRunner(r, Docker, config.Runtime{})
	e, err := d.WriteVolumeFile(context.Background(), "data", "x.txt", strings.NewReader("abcdef"), 3, runtime.WriteFileOpts{Exist: runtime.ExistMustExist})
	if err != nil || e.Size != 3 || e.UID != 1 {
		t.Fatalf("write = %+v %v", e, err)
	}
	args := r.calls[1]
	if got := strings.Join(args[len(args)-5:], "|"); got != "sh|./x.txt||must|3" || string(r.exchangeIn) != "abc" {
		t.Errorf("args = %q, stdin %q", got, r.exchangeIn)
	}
	if strings.Contains(strings.Join(args, " "), "data:/data:ro") {
		t.Error("write mounts read only")
	}
}

func TestVolumeFilesExitCodes(t *testing.T) {
	d := NewWithRunner(&fakeRunner{}, Docker, config.Runtime{})
	for code, kind := range map[int]error{75: runtime.ErrChanged, 76: runtime.ErrNotFound, 77: runtime.ErrConflict, 78: runtime.ErrInvalid, 79: runtime.ErrInvalid, 80: runtime.ErrInvalid, 81: runtime.ErrInvalid} {
		if err := d.filesError("volume write file", code, nil); !errors.Is(err, kind) {
			t.Errorf("exit %d = %v", code, err)
		}
	}
	if err := d.filesError("volume list files", 125, []byte("Unable to find image")); err == nil || !strings.Contains(err.Error(), "Unable to find image") {
		t.Errorf("runtime failure = %v", err)
	}
	if err := d.RemoveVolumePath(context.Background(), "data", "../"); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("remove root = %v", err)
	}
}
