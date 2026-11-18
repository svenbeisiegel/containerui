// Package diskwalk measures the host paths that containers bind-mount, the
// way du does: the space allocated on disk, each file once even when it is
// hard-linked or reached through several mounts, without leaving the file
// system the path is on. System paths are not measured.
package diskwalk

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// skipExact are system folders that are skipped themselves; a folder below
// them (/srv/app, /home/user/data) is measured.
var skipExact = []string{
	"/", "/bin", "/boot", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32", "/media", "/mnt",
	"/opt", "/root", "/sbin", "/srv", "/tmp", "/usr", "/var", "/var/lib", "/var/log",
	// macOS (Docker Desktop, colima)
	"/Users", "/Volumes", "/private", "/private/var", "/private/tmp", "/System", "/Library",
}

// skipTree are skipped with everything below: kernel and runtime state, and
// the runtimes' own data, which is already counted as images, containers
// and volumes.
var skipTree = []string{
	"/proc", "/sys", "/dev", "/run", "/var/run", "/private/var/run",
	"/var/lib/docker", "/var/lib/containers",
}

// skipExactWindows and skipTreeWindows are the same for Windows paths with
// a drive (C:\Users), matched on any drive and ignoring case. A drive root
// is always skipped.
var (
	skipExactWindows = []string{"/Users", "/Program Files", "/Program Files (x86)", "/ProgramData"}
	skipTreeWindows  = []string{"/Windows", "/ProgramData/Docker", "/ProgramData/DockerDesktop"}
)

// Skipped reports whether path is a system path that is not measured.
// extra lists further paths skipped with everything below them.
func Skipped(path string, extra []string) bool {
	for _, p := range candidates(path) {
		vol, rest := split(p)
		exact, tree := skipExact, skipTree
		if vol != "" {
			if rest == "/" {
				return true // a drive or share root
			}
			exact, tree = skipExactWindows, skipTreeWindows
		}
		for _, s := range exact {
			if rest == key(s) {
				return true
			}
		}
		for _, s := range tree {
			if within(rest, key(s)) {
				return true
			}
		}
		for _, s := range extra {
			if s == "" {
				continue
			}
			if xv, xr := split(s); strings.EqualFold(vol, xv) && within(rest, xr) {
				return true
			}
		}
	}
	return false
}

// candidates are path as given and with symlinks resolved (on macOS /var
// is /private/var).
func candidates(path string) []string {
	path = filepath.Clean(path)
	out := []string{path}
	if r, err := filepath.EvalSymlinks(path); err == nil && r != path {
		out = append(out, r)
	}
	return out
}

// split cleans path and splits it into its volume name (C:, \\host\share;
// only on Windows) and the rest as a key.
func split(path string) (vol, rest string) {
	path = filepath.Clean(path)
	vol = filepath.VolumeName(path)
	rest = key(path[len(vol):])
	if rest == "" || rest == "." {
		rest = "/"
	}
	return vol, rest
}

// key is path with forward slashes, lower case on Windows, where file
// names ignore case, so that paths can be compared as strings.
func key(path string) string {
	path = filepath.ToSlash(path)
	if goruntime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// within reports whether path is dir or below it; both are keys.
func within(path, dir string) bool {
	if dir == "/" {
		return true
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// Measure measures every path once. Deeper paths are measured first, so
// when one bind-mounted path contains another, the inner one gets its own
// size and the outer one the rest. A cancelled ctx leaves the paths not
// done yet marked partial.
func Measure(ctx context.Context, paths []string, extraSkip []string) map[string]runtime.BindSize {
	out := make(map[string]runtime.BindSize, len(paths))
	var todo []string
	for _, p := range paths {
		if _, ok := out[p]; ok || p == "" {
			continue
		}
		if Skipped(p, extraSkip) {
			out[p] = runtime.BindSize{Note: runtime.BindSkipped}
			continue
		}
		out[p] = runtime.BindSize{}
		todo = append(todo, p)
	}
	sort.SliceStable(todo, func(i, j int) bool {
		di, dj := strings.Count(filepath.Clean(todo[i]), string(filepath.Separator)), strings.Count(filepath.Clean(todo[j]), string(filepath.Separator))
		if di != dj {
			return di > dj
		}
		return todo[i] < todo[j]
	})
	w := &walker{seen: map[fileID]bool{}, roots: map[string]bool{}}
	for _, p := range todo {
		out[p] = w.measure(ctx, p)
	}
	return out
}

type walker struct {
	// seen holds the directories and hard-linked files counted so far.
	// Plain files cannot be reached twice, so they are not kept.
	seen map[fileID]bool
	// roots holds the keys of the folders measured so far, so that an
	// outer path skips an inner one also where there are no file IDs.
	roots map[string]bool
}

func (w *walker) measure(ctx context.Context, path string) runtime.BindSize {
	if ctx.Err() != nil {
		return runtime.BindSize{Note: runtime.BindPartial}
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return runtime.BindSize{Note: runtime.BindMissing}
		}
		return runtime.BindSize{Note: runtime.BindPartial}
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return runtime.BindSize{Note: runtime.BindPartial}
	}
	rootDev := stat(fi).id.dev
	if fi.IsDir() {
		defer func() { w.roots[key(root)] = true }()
	}
	var res runtime.BindSize
	n := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if n++; n%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			res.Note = runtime.BindPartial
			return nil // an unreadable directory is skipped
		}
		info, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) { // removed meanwhile
				res.Note = runtime.BindPartial
			}
			return nil
		}
		st := stat(info)
		if d.IsDir() && st.id.dev != rootDev {
			return filepath.SkipDir // another file system mounted below
		}
		if d.IsDir() && w.roots[key(p)] {
			return filepath.SkipDir // measured already, as a deeper path or the same one
		}
		if d.IsDir() || st.links > 1 {
			if st.id.known() {
				if w.seen[st.id] {
					if d.IsDir() {
						return filepath.SkipDir // measured through another path
					}
					return nil
				}
				w.seen[st.id] = true
			}
		}
		res.Size += st.alloc
		return nil
	})
	if err != nil {
		res.Note = runtime.BindPartial
	}
	return res
}

// fileID identifies a file on the host; zero when the platform has none.
type fileID struct{ dev, ino uint64 }

func (id fileID) known() bool { return id.ino != 0 }

type fileStat struct {
	id    fileID
	links uint64
	alloc uint64 // bytes allocated on disk
}
