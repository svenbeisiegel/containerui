package fake

import (
	"archive/tar"
	"context"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.VolumeArchiver = (*Runtime)(nil)

// volumeFile is an entry of a fake volume's content.
type volumeFile struct {
	hdr  tar.Header
	data []byte
}

// SetVolumeFile puts a regular file (rel path, parent folders implied)
// into a volume's content.
func (f *Runtime) SetVolumeFile(volume, rel, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setFile(volume, volumeFile{hdr: tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now()}, data: []byte(data)})
}

// VolumeFiles returns a volume's content: rel path -> data ("" and a
// trailing / for folders).
func (f *Runtime) VolumeFiles(volume string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for rel, vf := range f.content[volume] {
		if vf.hdr.Typeflag == tar.TypeDir {
			rel += "/"
		}
		out[rel] = string(vf.data)
	}
	return out
}

func (f *Runtime) setFile(volume string, vf volumeFile) {
	if f.content == nil {
		f.content = map[string]map[string]volumeFile{}
	}
	if f.content[volume] == nil {
		f.content[volume] = map[string]volumeFile{}
	}
	f.content[volume][vf.hdr.Name] = vf
}

func (f *Runtime) ExportVolume(ctx context.Context, name string, w io.Writer) error {
	f.mu.Lock()
	err := f.record("export-volume %s", name)
	if err == nil && f.findVolume(name) < 0 {
		err = &runtime.CommandError{Op: "volume export", Output: "no such volume: " + name, Kind: runtime.ErrNotFound}
	}
	files := maps.Clone(f.content[name])
	f.mu.Unlock()
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}); err != nil {
		return err
	}
	dirs := map[string]bool{}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		vf := files[rel]
		// Parent folders first, like tar does.
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			if !dirs[d] && files[d].hdr.Typeflag != tar.TypeDir {
				dirs[d] = true
				if err := tw.WriteHeader(&tar.Header{Name: "./" + d + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}); err != nil {
					return err
				}
			}
		}
		h := vf.hdr
		h.Name = "./" + rel
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
		if _, err := tw.Write(vf.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

func (f *Runtime) ImportVolume(_ context.Context, name string, r io.Reader, replace bool) error {
	f.mu.Lock()
	err := f.record("import-volume %s replace=%t", name, replace)
	if err == nil && f.findVolume(name) < 0 {
		err = &runtime.CommandError{Op: "volume import", Output: "no such volume: " + name, Kind: runtime.ErrNotFound}
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	files := map[string]volumeFile{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(path.Clean(h.Name), "/")
		if rel == "." {
			continue
		}
		h.Name = rel
		files[rel] = volumeFile{hdr: *h, data: data}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replace {
		delete(f.content, name)
	}
	for _, vf := range files {
		f.setFile(name, vf)
	}
	return nil
}
