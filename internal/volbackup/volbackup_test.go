package volbackup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// file is an entry of a test volume.
type file struct {
	typ      byte
	mode     int64
	uid, gid int
	data     string
	link     string
}

type volumes map[string]map[string]file // volume -> rel path ("" = root) -> file

// exporter writes a volume like "tar -C /data -cf - ." does.
func (vs volumes) export(_ context.Context, name string, w io.Writer) error {
	files, ok := vs[name]
	if !ok {
		return errors.New("no such volume")
	}
	tw := tar.NewWriter(w)
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		f := files[rel]
		h := &tar.Header{Name: "./" + rel, Typeflag: f.typ, Mode: f.mode, Uid: f.uid, Gid: f.gid, Uname: "someone", Gname: "group", Linkname: f.link, ModTime: time.Unix(1700000000, 0)}
		if f.typ == tar.TypeDir {
			h.Name = strings.TrimSuffix(h.Name, "/") + "/"
		}
		if f.typ == tar.TypeReg {
			h.Size = int64(len(f.data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, f.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// importer records what a restore extracts.
type importer struct {
	got      volumes
	replaced map[string]bool
	fail     string // volume whose import fails
}

func (im *importer) imp(_ context.Context, name string, r io.Reader, replace bool) error {
	if name == im.fail {
		return errors.New("disk full")
	}
	if im.got == nil {
		im.got, im.replaced = volumes{}, map[string]bool{}
	}
	im.replaced[name] = replace
	files := map[string]file{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h.Name, "./") {
			return errors.New("name not relative: " + h.Name)
		}
		if h.Uname != "" || h.Gname != "" {
			return errors.New("user names are kept")
		}
		data, _ := io.ReadAll(tr)
		rel := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		files[rel] = file{typ: h.Typeflag, mode: h.Mode, uid: h.Uid, gid: h.Gid, data: string(data), link: h.Linkname}
	}
	im.got[name] = files
	return nil
}

func sample() volumes {
	return volumes{
		"data": {
			"":           {typ: tar.TypeDir, mode: 0o755},
			"db":         {typ: tar.TypeDir, mode: 0o700, uid: 999, gid: 999},
			"db/pg.conf": {typ: tar.TypeReg, mode: 0o600, uid: 999, gid: 999, data: "max_connections = 100\n"},
			"db/link":    {typ: tar.TypeSymlink, mode: 0o777, link: "pg.conf"},
			"db/hard":    {typ: tar.TypeLink, mode: 0o600, link: "./db/pg.conf"},
		},
		"empty": {"": {typ: tar.TypeDir, mode: 0o777}},
	}
}

func manifest(c Compression) Manifest {
	return Manifest{Compression: c, Host: "box", Runtime: "docker", Volumes: []Volume{
		{Name: "data", Driver: "local", Labels: map[string]string{"com.docker.compose.project": "app"}, Options: map[string]string{"type": "none"}},
		{Name: "empty", Driver: "local"},
	}}
}

func backup(t *testing.T, c Compression) []byte {
	t.Helper()
	var buf bytes.Buffer
	stats, err := Write(context.Background(), &buf, manifest(c), sample().export)
	if err != nil {
		t.Fatal(err)
	}
	if s := stats["data"]; s.Entries != 5 || s.Bytes != int64(len("max_connections = 100\n")) {
		t.Errorf("stats = %+v", s)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	for _, c := range []Compression{Zstd, None} {
		t.Run(string(c), func(t *testing.T) {
			b := backup(t, c)
			if got := bytes.HasPrefix(b, zstdMagic); got != (c == Zstd) {
				t.Fatalf("zstd magic = %v", got)
			}
			m, stats, err := Scan(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if m.Format != Format || m.Version != Version || m.Host != "box" || m.Compression != c || len(m.Volumes) != 2 || m.Volumes[0].Labels["com.docker.compose.project"] != "app" {
				t.Errorf("manifest = %+v", m)
			}
			if stats["empty"].Entries != 1 || stats["data"].Entries != 5 {
				t.Errorf("stats = %+v", stats)
			}

			var im importer
			var progress []string
			_, err = Restore(context.Background(), bytes.NewReader(b), map[string]Target{
				"data":  {Name: "data2", Replace: true},
				"empty": {Name: "empty"},
			}, im.imp, func(l string) { progress = append(progress, l) })
			if err != nil {
				t.Fatal(err)
			}
			want := sample()
			want["data"]["db/hard"] = file{typ: tar.TypeLink, mode: 0o600, link: "./db/pg.conf"}
			if !equal(im.got["data2"], want["data"]) || !equal(im.got["empty"], want["empty"]) {
				t.Errorf("restored %+v", im.got)
			}
			if !im.replaced["data2"] || im.replaced["empty"] {
				t.Errorf("replace = %v", im.replaced)
			}
			if len(progress) != 2 || !strings.Contains(progress[0], "data into data2") {
				t.Errorf("progress = %q", progress)
			}
		})
	}
}

func equal(a, b map[string]file) bool { return maps.Equal(a, b) }

func TestRestoreSkips(t *testing.T) {
	b := backup(t, Zstd)
	var im importer
	if _, err := Restore(context.Background(), bytes.NewReader(b), map[string]Target{"empty": {Name: "e"}}, im.imp, nil); err != nil {
		t.Fatal(err)
	}
	if len(im.got) != 1 || im.got["e"] == nil {
		t.Errorf("restored %v", im.got)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(b), map[string]Target{"nope": {Name: "e"}}, im.imp, nil); err == nil {
		t.Error("restore of a volume not in the file succeeded")
	}
}

func TestRestoreImportFails(t *testing.T) {
	b := backup(t, None)
	im := importer{fail: "x"}
	_, err := Restore(context.Background(), bytes.NewReader(b), map[string]Target{"data": {Name: "x"}}, im.imp, nil)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteExportFails(t *testing.T) {
	m := manifest(Zstd)
	m.Volumes = append(m.Volumes, Volume{Name: "missing"})
	_, err := Write(context.Background(), io.Discard, m, sample().export)
	if err == nil || !strings.Contains(err.Error(), "missing: no such volume") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteRejectsUnsafePaths(t *testing.T) {
	vs := volumes{"data": {"../etc/passwd": {typ: tar.TypeReg, data: "x"}}}
	m := Manifest{Volumes: []Volume{{Name: "data"}}}
	if _, err := Write(context.Background(), io.Discard, m, vs.export); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("err = %v", err)
	}
	vs = volumes{"data": {"x": {typ: tar.TypeLink, link: "../../etc/shadow"}}}
	if _, err := Write(context.Background(), io.Discard, m, vs.export); err == nil || !strings.Contains(err.Error(), "unsafe hard link") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteValidatesManifest(t *testing.T) {
	for _, vols := range [][]Volume{nil, {{Name: "a/b"}}, {{Name: "a"}, {Name: "a"}}, {{Name: ".."}}} {
		if _, err := Write(context.Background(), io.Discard, Manifest{Volumes: vols}, sample().export); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: err = %v", vols, err)
		}
	}
}

// rawTar builds an uncompressed file from entries (name, content).
func rawTar(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(e[0], "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Typeflag: typ, Size: int64(len(e[1])), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e[1])
	}
	tw.Close()
	return buf.Bytes()
}

const goodManifest = `{"format":"containerui-volume-backup","version":1,"volumes":[{"name":"a"},{"name":"b"}]}`

func TestScanRejects(t *testing.T) {
	full := backup(t, Zstd)
	cases := map[string][]byte{
		"empty":           nil,
		"not tar":         []byte("hello world, this is no tar file at all"),
		"truncated zstd":  full[:len(full)/2],
		"no manifest":     rawTar(t, [2]string{"volumes/a/", ""}),
		"other format":    rawTar(t, [2]string{ManifestName, `{"format":"x","version":1,"volumes":[{"name":"a"}]}`}),
		"newer version":   rawTar(t, [2]string{ManifestName, `{"format":"containerui-volume-backup","version":99,"volumes":[{"name":"a"}]}`}),
		"bad json":        rawTar(t, [2]string{ManifestName, `{`}),
		"unknown volume":  rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/", ""}, [2]string{"volumes/b/", ""}, [2]string{"volumes/c/x", "1"}),
		"outside volumes": rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/", ""}, [2]string{"etc/passwd", "x"}),
		"dot dot":         rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/../../x", "x"}),
		"split volume":    rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/", ""}, [2]string{"volumes/b/", ""}, [2]string{"volumes/a/x", "1"}),
		"missing content": rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/", ""}),
	}
	for name, b := range cases {
		if _, _, err := Scan(bytes.NewReader(b)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, _, err := Scan(bytes.NewReader(rawTar(t, [2]string{ManifestName, goodManifest}, [2]string{"volumes/a/", ""}, [2]string{"volumes/b/", ""}))); err != nil {
		t.Errorf("valid file: %v", err)
	}
}

func TestParseCompression(t *testing.T) {
	for in, want := range map[string]Compression{"": Zstd, "zstd": Zstd, "none": None} {
		if got, err := ParseCompression(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseCompression("gzip"); err == nil {
		t.Error("gzip accepted")
	}
	if Zstd.Ext() != ".tar.zst" || None.Ext() != ".tar" {
		t.Error("extensions")
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}
