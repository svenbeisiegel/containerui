package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.VolumeBrowser = (*Runtime)(nil)

// SetVolumeEntry puts an entry with a full tar header (owner, mode, type,
// link target, mtime) into a volume's content.
func (f *Runtime) SetVolumeEntry(volume string, hdr tar.Header, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hdr.Size = int64(len(data))
	f.setFile(volume, volumeFile{hdr: hdr, data: []byte(data)})
}

// VolumeEntryHeader returns the tar header of an entry of a volume.
func (f *Runtime) VolumeEntryHeader(volume, rel string) (tar.Header, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vf, ok := f.content[volume][rel]
	return vf.hdr, ok
}

func filesErr(op string, kind error, msg string) error {
	return &runtime.CommandError{Op: op, Output: msg, Kind: kind}
}

// filesStart records the call and checks the volume; it returns with f.mu
// held on success.
func (f *Runtime) filesStart(op, volume string, format string, args ...any) error {
	f.mu.Lock()
	if err := f.record(format, args...); err != nil {
		f.mu.Unlock()
		return err
	}
	if f.findVolume(volume) < 0 {
		f.mu.Unlock()
		return filesErr(op, runtime.ErrNotFound, "no such volume: "+volume)
	}
	return nil
}

// kind is what rel is in a volume: "" missing, "dir" (also implied by a
// deeper entry), or the entry's tar type.
func (f *Runtime) kind(volume, rel string) byte {
	if rel == "" {
		return tar.TypeDir
	}
	if vf, ok := f.content[volume][rel]; ok {
		return vf.hdr.Typeflag
	}
	for k := range f.content[volume] {
		if strings.HasPrefix(k, rel+"/") {
			return tar.TypeDir
		}
	}
	return 0
}

// owner of a folder (root and implied folders: 0:0).
func (f *Runtime) owner(volume, rel string) (int, int) {
	vf := f.content[volume][rel]
	return vf.hdr.Uid, vf.hdr.Gid
}

func parentOf(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

func entryOf(name string, h tar.Header) runtime.VolumeEntry {
	mode := fs.FileMode(h.Mode & 0o777)
	switch h.Typeflag {
	case tar.TypeDir:
		mode |= fs.ModeDir
	case tar.TypeSymlink:
		mode |= fs.ModeSymlink
	}
	if h.Mode&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if h.Mode&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if h.Mode&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return runtime.VolumeEntry{Name: name, Mode: mode, Size: h.Size, UID: h.Uid, GID: h.Gid, ModTime: h.ModTime.Truncate(time.Second), Link: h.Linkname}
}

func (f *Runtime) ListVolumeDir(_ context.Context, volume, dir string) ([]runtime.VolumeEntry, error) {
	const op = "volume list files"
	rel, err := runtime.VolumeRel(dir)
	if err != nil {
		return nil, err
	}
	if err := f.filesStart(op, volume, "list-volume-files %s %s", volume, rel); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	switch f.kind(volume, rel) {
	case 0:
		return nil, filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	case tar.TypeDir:
	default:
		return nil, filesErr(op, runtime.ErrInvalid, "Not a folder.")
	}
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	seen := map[string]bool{}
	var out []runtime.VolumeEntry
	for k, vf := range f.content[volume] {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name, deeper, _ := strings.Cut(strings.TrimPrefix(k, prefix), "/")
		if seen[name] {
			continue
		}
		if deeper != "" {
			if _, explicit := f.content[volume][prefix+name]; explicit {
				continue // listed with its own header
			}
			seen[name] = true
			out = append(out, runtime.VolumeEntry{Name: name, Mode: fs.ModeDir | 0o755, ModTime: time.Now().Truncate(time.Second)})
			continue
		}
		seen[name] = true
		out = append(out, entryOf(name, vf.hdr))
	}
	slices.SortFunc(out, func(a, b runtime.VolumeEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *Runtime) ReadVolumeFile(_ context.Context, volume, file string, limit int64, open func(runtime.VolumeEntry) (io.Writer, error)) error {
	const op = "volume read file"
	rel, err := runtime.VolumeRel(file)
	if err != nil {
		return err
	}
	if err := f.filesStart(op, volume, "read-volume-file %s %s", volume, rel); err != nil {
		return err
	}
	vf, ok := f.content[volume][rel]
	k := f.kind(volume, rel)
	f.mu.Unlock()
	switch {
	case k == 0:
		return filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	case !ok || vf.hdr.Typeflag != tar.TypeReg:
		return filesErr(op, runtime.ErrInvalid, "Not a regular file.")
	}
	w, err := open(entryOf(path.Base(rel), vf.hdr))
	if err != nil {
		return err
	}
	data := vf.data
	if limit > 0 && int64(len(data)) > limit {
		data = data[:limit]
	}
	_, err = w.Write(data)
	return err
}

func (f *Runtime) WriteVolumeFile(_ context.Context, volume, file string, r io.Reader, size int64, o runtime.WriteFileOpts) (runtime.VolumeEntry, error) {
	const op = "volume write file"
	rel, err := runtime.VolumeRel(file)
	if err != nil {
		return runtime.VolumeEntry{}, err
	}
	if rel == "" {
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrInvalid, "Not a regular file.")
	}
	// Read before locking: r may be a slow upload.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, size)); err != nil {
		return runtime.VolumeEntry{}, err
	}
	if int64(buf.Len()) != size {
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrInvalid, "The content arrived incomplete; the file was not changed.")
	}
	if err := f.filesStart(op, volume, "write-volume-file %s %s", volume, rel); err != nil {
		return runtime.VolumeEntry{}, err
	}
	defer f.mu.Unlock()
	dir := parentOf(rel)
	switch f.kind(volume, dir) {
	case 0:
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	case tar.TypeDir:
	default:
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrInvalid, "Not a folder.")
	}
	old, exists := f.content[volume][rel]
	hdr := tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644}
	switch k := f.kind(volume, rel); {
	case k == tar.TypeSymlink:
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrInvalid, "Not a regular file.")
	case k != 0 && o.Exist == runtime.ExistMustNotExist:
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrConflict, "A file or folder with that name already exists.")
	case k != 0 && (!exists || k != tar.TypeReg):
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrInvalid, "Not a regular file.")
	case k != 0:
		if !o.ExpectModTime.IsZero() && old.hdr.ModTime.Unix() != o.ExpectModTime.Unix() {
			return runtime.VolumeEntry{}, filesErr(op, runtime.ErrChanged, "The file was changed by someone else since it was opened.")
		}
		hdr.Mode, hdr.Uid, hdr.Gid = old.hdr.Mode, old.hdr.Uid, old.hdr.Gid
	case o.Exist == runtime.ExistMustExist:
		return runtime.VolumeEntry{}, filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	default:
		hdr.Uid, hdr.Gid = f.owner(volume, dir)
	}
	hdr.ModTime = time.Now()
	hdr.Size = int64(buf.Len())
	f.setFile(volume, volumeFile{hdr: hdr, data: buf.Bytes()})
	return entryOf(path.Base(rel), hdr), nil
}

func (f *Runtime) MakeVolumeDir(_ context.Context, volume, dir string) error {
	const op = "volume create folder"
	rel, err := runtime.VolumeRel(dir)
	if err != nil {
		return err
	}
	if err := f.filesStart(op, volume, "make-volume-dir %s %s", volume, rel); err != nil {
		return err
	}
	defer f.mu.Unlock()
	parent := parentOf(rel)
	if f.kind(volume, parent) != tar.TypeDir {
		return filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	}
	if rel == "" || f.kind(volume, rel) != 0 {
		return filesErr(op, runtime.ErrConflict, "A file or folder with that name already exists.")
	}
	uid, gid := f.owner(volume, parent)
	f.setFile(volume, volumeFile{hdr: tar.Header{Name: rel, Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: gid, ModTime: time.Now()}})
	return nil
}

func (f *Runtime) RenameVolumePath(_ context.Context, volume, from, newName string) error {
	const op = "volume rename"
	rel, err := runtime.VolumeRel(from)
	if err != nil {
		return err
	}
	if err := runtime.ValidVolumeName(newName); err != nil {
		return err
	}
	if err := f.filesStart(op, volume, "rename-volume-path %s %s %s", volume, rel, newName); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if rel == "" {
		return filesErr(op, runtime.ErrInvalid, "The volume's root cannot be renamed.")
	}
	if f.kind(volume, rel) == 0 {
		return filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	}
	to := path.Join(parentOf(rel), newName)
	if f.kind(volume, to) != 0 {
		return filesErr(op, runtime.ErrConflict, "A file or folder with that name already exists.")
	}
	for k, vf := range f.content[volume] {
		if k == rel || strings.HasPrefix(k, rel+"/") {
			delete(f.content[volume], k)
			vf.hdr.Name = to + strings.TrimPrefix(k, rel)
			f.content[volume][vf.hdr.Name] = vf
		}
	}
	return nil
}

func (f *Runtime) RemoveVolumePath(_ context.Context, volume, target string) error {
	const op = "volume delete"
	rel, err := runtime.VolumeRel(target)
	if err != nil {
		return err
	}
	if err := f.filesStart(op, volume, "remove-volume-path %s %s", volume, rel); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if rel == "" {
		return filesErr(op, runtime.ErrInvalid, "The volume's root cannot be deleted.")
	}
	if f.kind(volume, rel) == 0 {
		return filesErr(op, runtime.ErrNotFound, "No such file or folder.")
	}
	for k := range f.content[volume] {
		if k == rel || strings.HasPrefix(k, rel+"/") {
			delete(f.content[volume], k)
		}
	}
	return nil
}
