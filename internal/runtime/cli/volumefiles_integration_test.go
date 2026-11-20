//go:build integration

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// TestVolumeFiles lists, reads and changes files of a volume with the real
// docker or podman CLI and the helper image:
//
//	go test -tags integration -run VolumeFiles ./internal/runtime/cli/
func TestVolumeFiles(t *testing.T) {
	var d *Driver
	for _, f := range []*Flavor{Docker, Podman} {
		if drv, err := New(f, config.Runtime{}); err == nil {
			if _, err := drv.Info(context.Background()); err == nil {
				d = drv
				break
			}
		}
	}
	if d == nil {
		t.Skip("neither docker nor podman answers")
	}
	ctx := context.Background()
	vol := fmt.Sprint("cui-it-files-", time.Now().UnixNano())
	if _, err := d.CreateVolume(ctx, runtime.CreateVolumeOpts{Name: vol, Labels: []string{"cui.test=1"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.RemoveVolume(ctx, vol, true) })
	content := tarOf(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./conf/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 70, Gid: 71},
		{Name: "./conf/app.ini", Typeflag: tar.TypeReg, Mode: 0o4750, Uid: 1234, Gid: 5678, Size: 6},
		{Name: "./.hidden", Typeflag: tar.TypeReg, Mode: 0o600, Size: 6},
		{Name: "./new\nline", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6},
		{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "conf/app.ini", Mode: 0o777},
		{Name: "./escape", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
	})
	if err := d.ImportVolume(ctx, vol, bytes.NewReader(content), false); err != nil {
		t.Fatal(err)
	}

	entries, err := d.ListVolumeDir(ctx, vol, "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]runtime.VolumeEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(entries) != 5 || !byName["conf"].Mode.IsDir() || byName["link"].Link != "conf/app.ini" || byName[".hidden"].Size != 6 {
		t.Fatalf("root = %+v", entries)
	}
	if _, ok := byName["new\nline"]; !ok {
		t.Errorf("name with newline missing: %+v", entries)
	}
	conf, err := d.ListVolumeDir(ctx, vol, "conf")
	if err != nil || len(conf) != 1 {
		t.Fatalf("conf = %+v, %v", conf, err)
	}
	ini := conf[0]
	if ini.UID != 1234 || ini.GID != 5678 || ini.Mode&fs.ModeSetuid == 0 || ini.Mode.Perm() != 0o750 {
		t.Errorf("app.ini = %+v (%v)", ini, ini.Mode)
	}

	// Read, then save over it: owner and mode, setuid included, stay.
	read := func(p string) (runtime.VolumeEntry, string, error) {
		var meta runtime.VolumeEntry
		var buf bytes.Buffer
		err := d.ReadVolumeFile(ctx, vol, p, 0, func(e runtime.VolumeEntry) (io.Writer, error) { meta = e; return &buf, nil })
		return meta, buf.String(), err
	}
	meta, data, err := read("conf/app.ini")
	if err != nil || data != "hello\n" || meta.UID != 1234 {
		t.Fatalf("read = %+v %q %v", meta, data, err)
	}
	if _, data, _ := read("link"); data != "hello\n" {
		t.Errorf("read through link = %q", data)
	}
	saved, err := d.WriteVolumeFile(ctx, vol, "conf/app.ini", strings.NewReader("[x]\na=1\n"), int64(len("[x]\na=1\n")), runtime.WriteFileOpts{ExpectModTime: meta.ModTime, Exist: runtime.ExistMustExist})
	if err != nil {
		t.Fatal(err)
	}
	if saved.UID != 1234 || saved.GID != 5678 || saved.Mode&fs.ModeSetuid == 0 || saved.Mode.Perm() != 0o750 || saved.Size != 8 {
		t.Errorf("after save = %+v (%v)", saved, saved.Mode)
	}
	if _, data, _ := read("conf/app.ini"); data != "[x]\na=1\n" {
		t.Errorf("saved content = %q", data)
	}
	// A stale mtime is a conflict.
	_, err = d.WriteVolumeFile(ctx, vol, "conf/app.ini", strings.NewReader("lost"), int64(len("lost")), runtime.WriteFileOpts{ExpectModTime: time.Unix(1, 0)})
	if !errors.Is(err, runtime.ErrChanged) {
		t.Errorf("stale save: %v", err)
	}

	// A short body leaves the file as it was.
	if _, err := d.WriteVolumeFile(ctx, vol, "conf/app.ini", strings.NewReader("cut"), 10, runtime.WriteFileOpts{}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("short save: %v", err)
	}
	if _, data, _ := read("conf/app.ini"); data != "[x]\na=1\n" {
		t.Errorf("after short save = %q", data)
	}

	// New files and folders belong to the folder's owner.
	created, err := d.WriteVolumeFile(ctx, vol, "conf/new.txt", strings.NewReader("n"), int64(len("n")), runtime.WriteFileOpts{Exist: runtime.ExistMustNotExist})
	if err != nil || created.UID != 70 || created.GID != 71 || created.Mode.Perm() != 0o644 {
		t.Errorf("new file = %+v, %v", created, err)
	}
	if _, err := d.WriteVolumeFile(ctx, vol, "conf/new.txt", strings.NewReader("n"), int64(len("n")), runtime.WriteFileOpts{Exist: runtime.ExistMustNotExist}); !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("create over existing: %v", err)
	}
	if err := d.MakeVolumeDir(ctx, vol, "conf/sub"); err != nil {
		t.Fatal(err)
	}
	conf, _ = d.ListVolumeDir(ctx, vol, "conf")
	for _, e := range conf {
		if e.Name == "sub" && (e.UID != 70 || e.Mode.Perm() != 0o755 || !e.Mode.IsDir()) {
			t.Errorf("sub = %+v", e)
		}
	}

	// Rename and delete.
	if err := d.RenameVolumePath(ctx, vol, "conf/new.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameVolumePath(ctx, vol, "conf/renamed.txt", "app.ini"); !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("rename onto existing: %v", err)
	}
	if err := d.RemoveVolumePath(ctx, vol, "conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListVolumeDir(ctx, vol, "conf"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("list removed folder: %v", err)
	}

	// Nothing outside the volume is reachable through a link.
	if _, err := d.ListVolumeDir(ctx, vol, "escape"); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("list through escaping link: %v", err)
	}
	if _, err := d.WriteVolumeFile(ctx, vol, "escape/passwd", strings.NewReader("x"), int64(len("x")), runtime.WriteFileOpts{}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("write through escaping link: %v", err)
	}
	if _, err := d.WriteVolumeFile(ctx, vol, "link", strings.NewReader("x"), int64(len("x")), runtime.WriteFileOpts{}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("write onto a link: %v", err)
	}
	// Deleting the link removes the link only.
	if err := d.RemoveVolumePath(ctx, vol, "escape"); err != nil {
		t.Error(err)
	}
}
