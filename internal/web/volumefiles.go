package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// ---- volume file browser and editor ----

// editLimit is the most the editor reads and saves of a file.
const editLimit = 10 << 20

// volumeBrowser returns the runtime's file browser, or answers 404.
func (s *Server) volumeBrowser(w http.ResponseWriter, r *http.Request) (runtime.VolumeBrowser, bool) {
	vb, ok := s.rt.(runtime.VolumeBrowser)
	if !ok || !s.caps().VolumeFiles || !s.caps().Volumes {
		http.NotFound(w, r)
		return nil, false
	}
	return vb, true
}

// fileRow is an entry of the browsed folder.
type fileRow struct {
	runtime.VolumeEntry
	Path string // relative to the volume's root
}

func (e fileRow) IsDir() bool     { return e.Mode.IsDir() }
func (e fileRow) IsLink() bool    { return e.Mode&fs.ModeSymlink != 0 }
func (e fileRow) IsRegular() bool { return e.Mode.IsRegular() }
func (e fileRow) Owner() string   { return fmt.Sprintf("%d:%d", e.UID, e.GID) }
func (e fileRow) Perm() string    { return unixPerm(e.Mode) }
func (e fileRow) Octal() string   { return fmt.Sprintf("%04o", unixOctal(e.Mode)) }
func (e fileRow) HumanSize() string {
	if e.Size < 0 {
		return ""
	}
	return humanBytes(uint64(e.Size))
}

// unixOctal is the permission bits with setuid, setgid and sticky as chmod
// takes them.
func unixOctal(m fs.FileMode) uint32 {
	o := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		o |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		o |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		o |= 0o1000
	}
	return o
}

// unixPerm renders a mode as ls -l does: "drwxr-sr-x".
func unixPerm(m fs.FileMode) string {
	b := []byte("----------")
	switch {
	case m.IsDir():
		b[0] = 'd'
	case m&fs.ModeSymlink != 0:
		b[0] = 'l'
	case m&fs.ModeNamedPipe != 0:
		b[0] = 'p'
	case m&fs.ModeSocket != 0:
		b[0] = 's'
	case m&fs.ModeCharDevice != 0:
		b[0] = 'c'
	case m&fs.ModeDevice != 0:
		b[0] = 'b'
	}
	const rwx = "rwx"
	for i := 0; i < 9; i++ {
		if m.Perm()&(1<<uint(8-i)) != 0 {
			b[1+i] = rwx[i%3]
		}
	}
	special := func(set bool, i int, x, noX byte) {
		if !set {
			return
		}
		if b[i] == 'x' {
			b[i] = x
		} else {
			b[i] = noX
		}
	}
	special(m&fs.ModeSetuid != 0, 3, 's', 'S')
	special(m&fs.ModeSetgid != 0, 6, 's', 'S')
	special(m&fs.ModeSticky != 0, 9, 't', 'T')
	return string(b)
}

type crumb struct{ Name, Path string }

type volumeFilesView struct {
	Volume  string
	Dir     string // "" for the root
	Crumbs  []crumb
	Entries []fileRow
	Err     string
	Flash   *flash
	CSRF    string
}

// PartialURL refreshes the table.
func (v volumeFilesView) PartialURL() string {
	u := "/partials/volume-files/" + url.PathEscape(v.Volume)
	if v.Dir != "" {
		u += "?path=" + url.QueryEscape(v.Dir)
	}
	return u
}

// UploadURL is where the upload form posts, for the folder.
func (v volumeFilesView) UploadURL() string {
	return filesURL(v.Volume, "upload", v.Dir)
}

// Parent is the folder above Dir.
func (v volumeFilesView) Parent() string { return parentDir(v.Dir) }

func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// filesURL is the browser page (or one of its actions) for a folder.
func filesURL(volume, action, dir string) string {
	u := "/volumes/files/" + url.PathEscape(volume)
	if action != "" {
		u += "/" + action
	}
	if dir != "" {
		u += "?path=" + url.QueryEscape(dir)
	}
	return u
}

func crumbsOf(dir string) []crumb {
	var out []crumb
	if dir == "" {
		return nil
	}
	acc := ""
	for _, part := range strings.Split(dir, "/") {
		acc = path.Join(acc, part)
		out = append(out, crumb{Name: part, Path: acc})
	}
	return out
}

// pathParam reads and cleans the path within the volume.
func pathParam(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if r.Method == http.MethodPost && p == "" {
		p = r.PostFormValue("path")
	}
	return runtime.VolumeRel(p)
}

func (s *Server) loadVolumeFiles(r *http.Request, vb runtime.VolumeBrowser, volume, dir string, f *flash) volumeFilesView {
	v := volumeFilesView{Volume: volume, Dir: dir, Crumbs: crumbsOf(dir), Flash: f, CSRF: sessionFrom(r).CSRF}
	entries, err := vb.ListVolumeDir(r.Context(), volume, dir)
	if err != nil {
		v.Err = userError(err)
		return v
	}
	for _, e := range entries {
		v.Entries = append(v.Entries, fileRow{VolumeEntry: e, Path: path.Join(dir, e.Name)})
	}
	// Folders first, then by name.
	slices.SortFunc(v.Entries, func(a, b fileRow) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return v
}

func (s *Server) handleVolumeFiles(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	dir, err := pathParam(r)
	if err != nil {
		http.Error(w, userError(err), http.StatusBadRequest)
		return
	}
	v := s.loadVolumeFiles(r, vb, volume, dir, nil)
	title := "Files · " + volume
	s.render(w, "volume_files", s.newPage(r, title, "volumes", v))
}

func (s *Server) handleVolumeFilesPartial(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	dir, err := pathParam(r)
	if err != nil {
		http.Error(w, userError(err), http.StatusBadRequest)
		return
	}
	s.renderPartial(w, "volume_files_table", s.loadVolumeFiles(r, vb, r.PathValue("name"), dir, nil))
}

// fileStatus is the HTTP status for a file error.
func fileStatus(err error) int {
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, runtime.ErrChanged), errors.Is(err, runtime.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, runtime.ErrInvalid):
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// handleVolumeFileRaw sends a file's bytes: to the editor (at most
// editLimit) or, with download=1, as a download. They are never shown as a
// page of this origin.
func (s *Server) handleVolumeFileRaw(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	file, err := pathParam(r)
	if err != nil || file == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	download := r.URL.Query().Get("download") == "1"
	limit := int64(editLimit)
	if download {
		limit = 0
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}
	started := false
	err = vb.ReadVolumeFile(r.Context(), volume, file, limit, func(e runtime.VolumeEntry) (io.Writer, error) {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
		h.Set("X-File-Mtime", strconv.FormatInt(e.ModTime.Unix(), 10))
		h.Set("X-File-Size", strconv.FormatInt(e.Size, 10))
		h.Set("X-File-Mode", fmt.Sprintf("%04o", unixOctal(e.Mode)))
		h.Set("X-File-Owner", fmt.Sprintf("%d:%d", e.UID, e.GID))
		h.Set("Last-Modified", e.ModTime.UTC().Format(http.TimeFormat))
		if download {
			h.Set("Content-Length", strconv.FormatInt(e.Size, 10))
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(file)}))
		} else if e.Size > limit {
			h.Set("X-File-Truncated", "1")
		}
		started = true
		w.WriteHeader(http.StatusOK)
		return w, nil
	})
	if err != nil {
		if started {
			s.log.Warn("volume file read aborted", "volume", volume, "path", file, "error", err)
			return
		}
		http.Error(w, userError(err), fileStatus(err))
	}
}

type volumeEditView struct {
	Volume, Path, Name, Dir string
	CSRF                    string
}

func (v volumeEditView) RawURL() string {
	return filesURL(v.Volume, "raw", v.Path)
}
func (v volumeEditView) DownloadURL() string { return v.RawURL() + "&download=1" }
func (v volumeEditView) SaveURL() string     { return filesURL(v.Volume, "save", v.Path) }
func (v volumeEditView) BackURL() string     { return filesURL(v.Volume, "", v.Dir) }

func (s *Server) handleVolumeFileEdit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.volumeBrowser(w, r); !ok {
		return
	}
	file, err := pathParam(r)
	if err != nil || file == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	v := volumeEditView{Volume: r.PathValue("name"), Path: file, Name: path.Base(file), Dir: parentDir(file), CSRF: sessionFrom(r).CSRF}
	// Ace injects <style> elements for its themes and layout.
	allowInlineStyles(w)
	s.render(w, "volume_edit", s.newPage(r, "Edit "+v.Name+" · "+v.Volume, "volumes", v))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleVolumeFileSave writes the editor's text back. Unless the editor
// says to overwrite, the file must not have changed since it was read
// (X-Expect-Mtime, seconds); it keeps its owner and permissions.
func (s *Server) handleVolumeFileSave(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	file, err := pathParam(r)
	if err != nil || file == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, editLimit))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "The file is larger than 10 MiB."})
		return
	}
	o := runtime.WriteFileOpts{Exist: runtime.ExistMustExist}
	if v := r.Header.Get("X-Expect-Mtime"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid X-Expect-Mtime"})
			return
		}
		o.ExpectModTime = time.Unix(sec, 0)
	}
	user := sessionFrom(r).Username
	e, err := vb.WriteVolumeFile(r.Context(), volume, file, bytes.NewReader(body), int64(len(body)), o)
	target := "edit " + volume + ":/" + file
	if err != nil {
		s.log.Warn("volume file save failed", "volume", volume, "path", file, "error", err)
		if !errors.Is(err, runtime.ErrChanged) {
			s.recordActivity(w, r, volumeJobKind, target, "", userError(err), nil, true, false)
		}
		writeJSON(w, fileStatus(err), map[string]any{"error": userError(err), "changed": errors.Is(err, runtime.ErrChanged)})
		return
	}
	s.log.Info("volume file saved", "volume", volume, "path", file, "bytes", len(body), "user", user)
	s.recordActivity(w, r, volumeJobKind, target, fmt.Sprintf("Saved %s (%s).", file, humanBytes(uint64(len(body)))), "", nil, true, false)
	writeJSON(w, http.StatusOK, map[string]any{
		"mtime": e.ModTime.Unix(), "size": e.Size,
		"mode": fmt.Sprintf("%04o", unixOctal(e.Mode)), "owner": fmt.Sprintf("%d:%d", e.UID, e.GID),
	})
}

// filesResult records a file action and shows the folder again: the table
// for htmx, the page otherwise.
func (s *Server) filesResult(w http.ResponseWriter, r *http.Request, vb runtime.VolumeBrowser, volume, dir, target, msg string, err error) {
	errMsg := ""
	f := okFlash("%s", msg)
	if err != nil {
		errMsg = userError(err)
		f = errFlash(err)
		s.log.Warn("volume file action failed", "volume", volume, "action", target, "error", err)
	} else {
		s.log.Info("volume file action", "volume", volume, "action", target, "user", sessionFrom(r).Username)
	}
	s.recordActivity(w, r, volumeJobKind, volume+": "+target, msg, errMsg, nil, !isHTMX(r), false)
	if !isHTMX(r) {
		sessionFrom(r).SetFlash(f.Msg)
		http.Redirect(w, r, filesURL(volume, "", dir), http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "volume_files_table", s.loadVolumeFiles(r, vb, volume, dir, f))
}

// formPaths reads the folder (path) and the entry name (name) of a form.
func formPaths(r *http.Request) (dir, name string, err error) {
	if dir, err = runtime.VolumeRel(r.PostFormValue("path")); err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(r.PostFormValue("name"))
	return dir, name, runtime.ValidVolumeName(name)
}

func (s *Server) handleVolumeMkdir(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	dir, name, err := formPaths(r)
	p := path.Join(dir, name)
	if err == nil {
		err = vb.MakeVolumeDir(r.Context(), volume, p)
	}
	s.filesResult(w, r, vb, volume, dir, "mkdir /"+p, "Created folder "+name+".", err)
}

// handleVolumeNewFile creates an empty file and opens it in the editor.
func (s *Server) handleVolumeNewFile(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	dir, name, err := formPaths(r)
	p := path.Join(dir, name)
	if err == nil {
		_, err = vb.WriteVolumeFile(r.Context(), volume, p, strings.NewReader(""), 0, runtime.WriteFileOpts{Exist: runtime.ExistMustNotExist})
	}
	if err != nil {
		s.filesResult(w, r, vb, volume, dir, "create /"+p, "", err)
		return
	}
	s.recordActivity(w, r, volumeJobKind, volume+": create /"+p, "Created file "+name+".", "", nil, true, false)
	redirect(w, r, filesURL(volume, "edit", p))
}

func (s *Server) handleVolumeRename(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	from, err := runtime.VolumeRel(r.PostFormValue("path"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if err == nil {
		err = runtime.ValidVolumeName(name)
	}
	if err == nil {
		err = vb.RenameVolumePath(r.Context(), volume, from, name)
	}
	s.filesResult(w, r, vb, volume, parentDir(from), "rename /"+from+" to "+name, "Renamed "+path.Base(from)+" to "+name+".", err)
}

func (s *Server) handleVolumeRemovePath(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	volume := r.PathValue("name")
	p, err := runtime.VolumeRel(r.PostFormValue("path"))
	if err == nil {
		err = vb.RemoveVolumePath(r.Context(), volume, p)
	}
	s.filesResult(w, r, vb, volume, parentDir(p), "delete /"+p, "Deleted "+path.Base(p)+".", err)
}

// handleVolumeUpload stores uploaded files in a folder. It is sent by
// script with the CSRF token as a header, so the body is read here, after
// lifting the read deadline, and not by the CSRF check. Files are spooled
// to disk by ParseMultipartForm, so each one's size is known: the helper
// only replaces a file once all of it arrived. An existing file is only
// replaced with overwrite=1, and keeps its owner and permissions.
func (s *Server) handleVolumeUpload(w http.ResponseWriter, r *http.Request) {
	vb, ok := s.volumeBrowser(w, r)
	if !ok {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	volume := r.PathValue("name")
	dir, err := runtime.VolumeRel(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userError(err)})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The upload could not be read: " + err.Error()})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	rule := runtime.ExistMustNotExist
	if r.URL.Query().Get("overwrite") == "1" {
		rule = runtime.ExistAny
	}
	var done, failed []string
	for _, fh := range r.MultipartForm.File["files"] {
		name := path.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		p := path.Join(dir, name)
		err := runtime.ValidVolumeName(name)
		if err == nil {
			var f io.ReadCloser
			if f, err = fh.Open(); err == nil {
				_, err = vb.WriteVolumeFile(r.Context(), volume, p, f, fh.Size, runtime.WriteFileOpts{Exist: rule})
				f.Close()
			}
		}
		if err != nil {
			s.log.Warn("volume file upload failed", "volume", volume, "path", p, "error", err)
			msg := userError(err)
			if errors.Is(err, runtime.ErrConflict) {
				msg = "exists already"
			}
			failed = append(failed, name+": "+msg)
			continue
		}
		done = append(done, name)
	}
	user := sessionFrom(r).Username
	s.log.Info("volume files uploaded", "volume", volume, "dir", dir, "files", len(done), "failed", len(failed), "user", user)
	msg := fmt.Sprintf("Uploaded %d file(s) to /%s.", len(done), dir)
	errMsg := ""
	if len(failed) > 0 {
		errMsg = "Not uploaded: " + strings.Join(failed, "; ")
		if len(done) > 0 {
			errMsg = msg + " " + errMsg
		}
	}
	s.recordActivity(w, r, volumeJobKind, volume+": upload to /"+dir, msg, errMsg, nil, true, false)
	if errMsg == "" {
		// The page shows it after the script reloads; errors the script
		// shows itself.
		sessionFrom(r).SetFlash(msg)
	}
	status := http.StatusOK
	if len(done) == 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"redirect": filesURL(volume, "", dir), "error": errMsg, "uploaded": len(done)})
}
