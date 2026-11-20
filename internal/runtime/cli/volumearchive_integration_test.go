//go:build integration

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/volbackup"
)

// TestVolumeArchive backs up and restores a volume with the real docker or
// podman CLI and the helper image:
//
//	go test -tags integration -run VolumeArchive ./internal/runtime/cli/
//
// It only touches volumes it creates (prefixed "cui-it-").
func TestVolumeArchive(t *testing.T) {
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
	suffix := fmt.Sprint(time.Now().UnixNano())
	src, dst := "cui-it-src-"+suffix, "cui-it-dst-"+suffix
	for _, n := range []string{src, dst} {
		if _, err := d.CreateVolume(ctx, runtime.CreateVolumeOpts{Name: n, Labels: []string{"cui.test=1"}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.RemoveVolume(ctx, n, true) })
	}

	// Fill the source volume through the helper.
	content := tarOf(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./db/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 999, Gid: 999},
		{Name: "./db/pg.conf", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 999, Gid: 998, Size: 6},
		{Name: "./db/hard", Typeflag: tar.TypeLink, Linkname: "./db/pg.conf"},
		{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "db/pg.conf", Mode: 0o777},
		{Name: "./run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Uid: 1000, Gid: 1000, Size: 6},
		{Name: "./.hidden", Typeflag: tar.TypeReg, Mode: 0o600, Size: 6},
		{Name: "./empty/", Typeflag: tar.TypeDir, Mode: 0o1777},
	})
	if err := d.ImportVolume(ctx, src, bytes.NewReader(content), false); err != nil {
		t.Fatal(err)
	}
	want := listing(t, d, src)
	if len(want) != 8 || !strings.Contains(want["db/pg.conf"], "uid=999 gid=998 mode=640") {
		t.Fatalf("source volume = %v", want)
	}

	// Back up, then restore into the other volume, which has a stale file
	// that replace removes.
	if err := d.ImportVolume(ctx, dst, bytes.NewReader(tarOf(t, []tar.Header{{Name: "./stale", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6}})), false); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	m := volbackup.Manifest{Volumes: []volbackup.Volume{{Name: src}}}
	if _, err := volbackup.Write(ctx, &file, m, d.ExportVolume); err != nil {
		t.Fatal(err)
	}
	if _, err := volbackup.Restore(ctx, bytes.NewReader(file.Bytes()), map[string]volbackup.Target{src: {Name: dst, Replace: true}}, d.ImportVolume, nil); err != nil {
		t.Fatal(err)
	}
	got := listing(t, d, dst)
	if !maps.Equal(got, want) {
		for _, k := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
			for k := range want {
				yield(k, true)
			}
			for k := range got {
				yield(k, true)
			}
		}))) {
			if want[k] != got[k] {
				t.Errorf("%s: want %q, got %q", k, want[k], got[k])
			}
		}
	}

	// Exporting a missing volume does not create it.
	missing := "cui-it-missing-" + suffix
	if err := d.ExportVolume(ctx, missing, io.Discard); err == nil {
		t.Error("export of a missing volume succeeded")
	}
	if _, err := d.InspectVolume(ctx, missing); err == nil {
		_ = d.RemoveVolume(ctx, missing, true)
		t.Error("export created the missing volume")
	}
}

// tarOf builds a tar stream; regular files contain "hello\n".
func tarOf(t *testing.T, hs []tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		h.ModTime = time.Unix(1700000000, 0)
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			io.WriteString(tw, "hello\n")
		}
	}
	tw.Close()
	return buf.Bytes()
}

// listing describes a volume's entries by exporting it.
func listing(t *testing.T, d *Driver, volume string) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := d.ExportVolume(context.Background(), volume, &buf); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" || name == "." {
			name = "."
		}
		out[name] = fmt.Sprintf("type=%c uid=%d gid=%d mode=%o link=%s data=%q", h.Typeflag, h.Uid, h.Gid, h.Mode&0o7777, h.Linkname, data)
	}
	return out
}
