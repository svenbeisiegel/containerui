package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.VolumeBrowser = (*Driver)(nil)

// filesLabel marks the short-lived containers of the volume file browser.
const filesLabel = "containerui.helper=volume-files"

// Exit codes of the file scripts; anything else is a failure of the
// runtime or of a command, reported with its stderr.
const (
	exitChanged     = 75 // the file's mtime is not the expected one
	exitNotFound    = 76 // the path does not exist
	exitExists      = 77 // the target exists
	exitOutside     = 78 // the path resolves outside the volume
	exitNotRegular  = 79 // not a regular file (or a link where none is allowed)
	exitNotFolder   = 80 // not a folder
	exitShort       = 81 // stdin ended before the expected size
	statFormat      = "%f %s %u %g %Y"
	filesScriptHead = `set -e
cd /data
inside() {
  [ -e "$1" ] || exit 76
  r=$(realpath "$1" 2>/dev/null) || exit 76
  case "$r" in /data|/data/*) ;; *) exit 78;; esac
}
`
)

// The scripts take their paths as positional arguments ($1, $2...), never
// in the script text. Paths are "." or "./rel" (see volumeRel), so they never
// look like options. inside() checks the folder an operation works in after
// resolving symbolic links: a link may point anywhere within the volume, but
// not out of it.
const (
	listScript = filesScriptHead + `inside "$1"
[ -d "$1" ] || exit 80
cd "$1"
find . -mindepth 1 -maxdepth 1 -exec stat -c '` + statFormat + ` %n' {} + || :
printf '\0'
find . -mindepth 1 -maxdepth 1 -type l -exec sh -c 'for l; do printf "%s\0%s\0" "$l" "$(readlink "$l")"; done' sh {} + || :
`
	readScript = filesScriptHead + `inside "$1"
[ -f "$1" ] || exit 79
stat -L -c '` + statFormat + `' "$1"
if [ "$2" -gt 0 ]; then exec head -c "$2" "$1"; fi
exec cat "$1"
`
	// writeScript replaces the file atomically through a temporary file in
	// the same folder, which gets the old file's owner and mode (setuid,
	// setgid and sticky bits included: chmod runs after chown, which would
	// clear them), or the folder's owner and 0644 for a new file.
	writeScript = filesScriptHead + `d=$(dirname "$1")
inside "$d"
[ -d "$d" ] || exit 80
[ ! -L "$1" ] || exit 79
if [ -e "$1" ]; then
  [ "$3" != mustnot ] || exit 77
  [ -f "$1" ] || exit 79
  if [ -n "$2" ] && [ "$(stat -c %Y "$1")" != "$2" ]; then exit 75; fi
  own=$(stat -c %u:%g "$1")
  mode=$(stat -c %a "$1")
else
  [ "$3" != must ] || exit 76
  own=$(stat -c %u:%g "$d")
  mode=644
fi
t=$(mktemp "$d/.containerui.XXXXXX")
trap 'rm -f "$t"' EXIT
cat >"$t"
[ "$(stat -c %s "$t")" = "$4" ] || exit 81
chown "$own" "$t"
chmod "$mode" "$t"
mv -f "$t" "$1"
trap - EXIT
stat -c '` + statFormat + `' "$1"
`
	mkdirScript = filesScriptHead + `d=$(dirname "$1")
inside "$d"
[ -d "$d" ] || exit 80
if [ -e "$1" ] || [ -L "$1" ]; then exit 77; fi
mkdir "$1"
chown "$(stat -c %u:%g "$d")" "$1"
chmod 755 "$1"
`
	renameScript = filesScriptHead + `d=$(dirname "$1")
inside "$d"
[ -e "$1" ] || [ -L "$1" ] || exit 76
t="$d/$2"
if [ -e "$t" ] || [ -L "$t" ]; then exit 77; fi
mv "$1" "$t"
`
	removeScript = filesScriptHead + `d=$(dirname "$1")
inside "$d"
[ -e "$1" ] || [ -L "$1" ] || exit 76
rm -rf "$1"
`
)

// scriptPath is how a script gets a path: "." or "./rel".
func scriptPath(p string) (string, error) {
	rel, err := runtime.VolumeRel(p)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return ".", nil
	}
	return "./" + rel, nil
}

// filesArgs runs the helper image with the volume at /data and the script's
// positional arguments.
func (d *Driver) filesArgs(volume string, readOnly bool, script string, args ...string) []string {
	return append(d.helperRunArgs(filesLabel, volume, readOnly, script), append([]string{"sh"}, args...)...)
}

// filesRun runs a file script, mapping its own exit codes to errors.
func (d *Driver) filesRun(ctx context.Context, op, volume string, readOnly bool, stdin io.Reader, w io.Writer, script string, args ...string) error {
	if err := d.archiveCheck(ctx, volume); err != nil {
		return err
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stderr, code, err := d.runner.Exchange(ctx, d.args(d.filesArgs(volume, readOnly, script, args...)...), stdin, w)
	if err != nil {
		return fmt.Errorf("%s %s: %w", d.flavor.Name, op, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s: %w", d.flavor.Name, op, ctx.Err())
	}
	return d.filesError(op, code, stderr)
}

// filesError maps a file script's exit code.
func (d *Driver) filesError(op string, code int, stderr []byte) error {
	fail := func(kind error, msg string) error {
		return &runtime.CommandError{Op: op, ExitCode: code, Output: msg, Kind: kind}
	}
	switch code {
	case 0:
		return nil
	case exitChanged:
		return fail(runtime.ErrChanged, "The file was changed by someone else since it was opened.")
	case exitNotFound:
		return fail(runtime.ErrNotFound, "No such file or folder.")
	case exitExists:
		return fail(runtime.ErrConflict, "A file or folder with that name already exists.")
	case exitOutside:
		return fail(runtime.ErrInvalid, "The path leads outside the volume.")
	case exitNotRegular:
		return fail(runtime.ErrInvalid, "Not a regular file.")
	case exitNotFolder:
		return fail(runtime.ErrInvalid, "Not a folder.")
	case exitShort:
		return fail(runtime.ErrInvalid, "The content arrived incomplete; the file was not changed.")
	}
	return d.commandError(op, code, nil, stderr)
}

var statLine = regexp.MustCompile(`^([0-9a-f]+) (\d+) (\d+) (\d+) (\d+)(?: \./(.*))?$`)

// parseStat parses a line of statFormat, optionally followed by " ./name".
func parseStat(line string) (runtime.VolumeEntry, bool) {
	m := statLine.FindStringSubmatch(line)
	if m == nil {
		return runtime.VolumeEntry{}, false
	}
	raw, _ := strconv.ParseUint(m[1], 16, 32)
	size, _ := strconv.ParseInt(m[2], 10, 64)
	uid, _ := strconv.Atoi(m[3])
	gid, _ := strconv.Atoi(m[4])
	mtime, _ := strconv.ParseInt(m[5], 10, 64)
	return runtime.VolumeEntry{Name: m[6], Mode: unixMode(uint32(raw)), Size: size, UID: uid, GID: gid, ModTime: time.Unix(mtime, 0)}, true
}

// unixMode converts a raw st_mode to an fs.FileMode.
func unixMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	}
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// parseList parses listScript's output: stat lines (a line that is not one
// continues the name before, which had a newline in it), a NUL, then pairs
// of link path and target, each ended by a NUL.
func parseList(out []byte) []runtime.VolumeEntry {
	stats, links, _ := bytes.Cut(out, []byte{0})
	var entries []runtime.VolumeEntry
	for _, line := range strings.Split(strings.TrimSuffix(string(stats), "\n"), "\n") {
		if e, ok := parseStat(line); ok && e.Name != "" {
			entries = append(entries, e)
		} else if len(entries) > 0 {
			entries[len(entries)-1].Name += "\n" + line
		}
	}
	targets := map[string]string{}
	parts := strings.Split(string(links), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		targets[strings.TrimPrefix(parts[i], "./")] = parts[i+1]
	}
	for i := range entries {
		if entries[i].Mode&fs.ModeSymlink != 0 {
			entries[i].Link = targets[entries[i].Name]
		}
	}
	return entries
}

func (d *Driver) ListVolumeDir(ctx context.Context, volume, dir string) ([]runtime.VolumeEntry, error) {
	p, err := scriptPath(dir)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := d.filesRun(ctx, "volume list files", volume, true, nil, &out, listScript, p); err != nil {
		return nil, err
	}
	return parseList(out.Bytes()), nil
}

// headerWriter passes the first line, the file's stat line, to open and
// what follows to the writer open returns.
type headerWriter struct {
	name string
	open func(runtime.VolumeEntry) (io.Writer, error)
	head []byte
	w    io.Writer
}

func (h *headerWriter) Write(p []byte) (int, error) {
	if h.w != nil {
		return h.w.Write(p)
	}
	n := len(p)
	h.head = append(h.head, p...)
	i := bytes.IndexByte(h.head, '\n')
	if i < 0 {
		if len(h.head) > 256 {
			return 0, errors.New("unexpected output of the helper container")
		}
		return n, nil
	}
	e, ok := parseStat(string(h.head[:i]))
	if !ok {
		return 0, errors.New("unexpected output of the helper container")
	}
	e.Name = h.name
	w, err := h.open(e)
	if err != nil {
		return 0, err
	}
	h.w = w
	if rest := h.head[i+1:]; len(rest) > 0 {
		if _, err := w.Write(rest); err != nil {
			return 0, err
		}
	}
	h.head = nil
	return n, nil
}

func (d *Driver) ReadVolumeFile(ctx context.Context, volume, file string, limit int64, open func(runtime.VolumeEntry) (io.Writer, error)) error {
	p, err := scriptPath(file)
	if err != nil {
		return err
	}
	if limit < 0 {
		limit = 0
	}
	h := &headerWriter{name: path.Base(p), open: open}
	if err := d.filesRun(ctx, "volume read file", volume, true, nil, h, readScript, p, strconv.FormatInt(limit, 10)); err != nil {
		return err
	}
	if h.w == nil {
		return errors.New("unexpected output of the helper container")
	}
	return nil
}

func (d *Driver) WriteVolumeFile(ctx context.Context, volume, file string, r io.Reader, size int64, o runtime.WriteFileOpts) (runtime.VolumeEntry, error) {
	p, err := scriptPath(file)
	if err != nil {
		return runtime.VolumeEntry{}, err
	}
	if p == "." {
		return runtime.VolumeEntry{}, fmt.Errorf("%w: not a file", runtime.ErrInvalid)
	}
	if size < 0 {
		return runtime.VolumeEntry{}, fmt.Errorf("%w: unknown size", runtime.ErrInvalid)
	}
	// Never more than size bytes: the helper checks the count it got.
	r = io.LimitReader(r, size)
	expect := ""
	if !o.ExpectModTime.IsZero() {
		expect = strconv.FormatInt(o.ExpectModTime.Unix(), 10)
	}
	rule := map[runtime.ExistRule]string{runtime.ExistAny: "any", runtime.ExistMustExist: "must", runtime.ExistMustNotExist: "mustnot"}[o.Exist]
	var out bytes.Buffer
	if err := d.filesRun(ctx, "volume write file", volume, false, r, &out, writeScript, p, expect, rule, strconv.FormatInt(size, 10)); err != nil {
		return runtime.VolumeEntry{}, err
	}
	e, ok := parseStat(strings.TrimSpace(out.String()))
	if !ok {
		return runtime.VolumeEntry{}, errors.New("unexpected output of the helper container")
	}
	e.Name = path.Base(p)
	return e, nil
}

func (d *Driver) MakeVolumeDir(ctx context.Context, volume, dir string) error {
	p, err := scriptPath(dir)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("%w: the folder exists", runtime.ErrConflict)
	}
	return d.filesRun(ctx, "volume create folder", volume, false, nil, io.Discard, mkdirScript, p)
}

func (d *Driver) RenameVolumePath(ctx context.Context, volume, from, newName string) error {
	p, err := scriptPath(from)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("%w: the volume's root cannot be renamed", runtime.ErrInvalid)
	}
	if err := runtime.ValidVolumeName(newName); err != nil {
		return err
	}
	return d.filesRun(ctx, "volume rename", volume, false, nil, io.Discard, renameScript, p, newName)
}

func (d *Driver) RemoveVolumePath(ctx context.Context, volume, target string) error {
	p, err := scriptPath(target)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("%w: the volume's root cannot be deleted", runtime.ErrInvalid)
	}
	return d.filesRun(ctx, "volume delete", volume, false, nil, io.Discard, removeScript, p)
}
