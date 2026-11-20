package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/volbackup"
)

// ---- volume backup and restore ----

// volumeArchiver returns the runtime's volume backup support, if any.
func (s *Server) volumeArchiver() (runtime.VolumeArchiver, bool) {
	va, _ := s.rt.(runtime.VolumeArchiver)
	return va, s.caps().VolumeBackup
}

// backupSupported answers 404 if the runtime cannot back up volumes.
func (s *Server) backupSupported(w http.ResponseWriter, r *http.Request) (runtime.VolumeArchiver, bool) {
	va, ok := s.volumeArchiver()
	if !ok || !s.caps().Volumes {
		http.NotFound(w, r)
		return nil, false
	}
	return va, true
}

// restoreSupported also needs the folder uploads wait in.
func (s *Server) restoreSupported(w http.ResponseWriter, r *http.Request) (runtime.VolumeArchiver, bool) {
	if s.uploads == nil {
		http.NotFound(w, r)
		return nil, false
	}
	return s.backupSupported(w, r)
}

var fileNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// backupFileName names a backup file after the single volume or the host.
func backupFileName(names []string, host string, comp volbackup.Compression, now time.Time) string {
	base := "volumes"
	if len(names) == 1 {
		base = names[0]
	} else if h := fileNameUnsafe.ReplaceAllString(host, "-"); h != "" {
		base += "_" + h
	}
	return base + "_" + now.Format("20060102-150405") + comp.Ext()
}

// volumeUsersRunning returns the running containers that mount one of the
// volumes.
func (s *Server) volumeUsersRunning(ctx context.Context, volumes []string) ([]runtime.Container, error) {
	cs, err := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	if err != nil {
		return nil, err
	}
	var out []runtime.Container
	for _, c := range cs {
		if c.State.IsRunning() && slices.ContainsFunc(c.Mounts, func(m string) bool { return slices.Contains(volumes, m) }) {
			out = append(out, c)
		}
	}
	return out, nil
}

// stopVolumeUsers stops the running containers that mount one of the
// volumes and returns the ones it stopped, to be started again with
// startAgain (also after an error).
func (s *Server) stopVolumeUsers(ctx context.Context, volumes []string, progress func(string)) ([]runtime.Container, error) {
	users, err := s.volumeUsersRunning(ctx, volumes)
	if err != nil {
		return nil, err
	}
	var stopped []runtime.Container
	for _, c := range users {
		if err := s.rt.StopContainer(ctx, c.ID, nil); err != nil {
			return stopped, fmt.Errorf("stop container %s: %s", c.Name, userError(err))
		}
		stopped = append(stopped, c)
		progress("Stopped container " + c.Name + ".")
	}
	return stopped, nil
}

// startAgain starts the containers stopVolumeUsers stopped, even when ctx
// has ended (the browser went away).
func (s *Server) startAgain(ctx context.Context, cs []runtime.Container, progress func(string)) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	for _, c := range cs {
		if err := s.rt.StartContainer(ctx, c.ID); err != nil {
			s.log.Warn("start container after volume backup failed", "container", c.Name, "error", err)
			errs = append(errs, fmt.Errorf("start container %s again: %s", c.Name, userError(err)))
			continue
		}
		progress("Started container " + c.Name + " again.")
	}
	return errors.Join(errs...)
}

// handleVolumeBackup sends the selected volumes as one backup file.
// Errors before the download starts are shown in the activity side bar of
// the volume list.
func (s *Server) handleVolumeBackup(w http.ResponseWriter, r *http.Request) {
	va, ok := s.backupSupported(w, r)
	if !ok {
		return
	}
	user := sessionFrom(r).Username
	_ = r.ParseForm()
	var names []string
	for _, n := range r.PostForm["volume"] {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	target := "backup " + strings.Join(names, ", ")
	fail := func(err error) {
		s.log.Warn("volume backup failed", "volumes", names, "user", user, "error", err)
		s.recordActivity(w, r, volumeJobKind, target, "", userError(err), nil, true, true)
		redirect(w, r, "/volumes")
	}
	if len(names) == 0 {
		target = "backup"
		fail(fmt.Errorf("%w: select at least one volume", runtime.ErrInvalid))
		return
	}
	comp, err := volbackup.ParseCompression(r.PostFormValue("compression"))
	if err != nil {
		fail(err)
		return
	}
	m, err := s.backupManifest(r.Context(), names, comp)
	if err != nil {
		fail(err)
		return
	}

	var lines []string
	progress := func(l string) { lines = append(lines, l) }
	var stopped []runtime.Container
	if r.PostFormValue("stop") == "on" {
		stopped, err = s.stopVolumeUsers(r.Context(), names, progress)
		if err != nil {
			if serr := s.startAgain(r.Context(), stopped, progress); serr != nil {
				err = errors.Join(err, serr)
			}
			fail(err)
			return
		}
	}

	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	filename := backupFileName(names, m.Host, comp, time.Now())
	hw := &heldWriter{w: w, header: func(h http.Header) {
		h.Set("Content-Type", comp.ContentType())
		h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		h.Set("Cache-Control", "no-store")
	}}
	stats, err := volbackup.Write(r.Context(), hw, m, va.ExportVolume)
	if err == nil {
		err = hw.flush()
	}
	if serr := s.startAgain(r.Context(), stopped, progress); serr != nil {
		lines = append(lines, serr.Error())
	}
	for _, n := range names {
		if st, ok := stats[n]; ok && err == nil {
			lines = append(lines, fmt.Sprintf("%s: %s, %s", n, st.EntriesText(), volbackup.FormatBytes(st.Bytes)))
		}
	}
	if err != nil {
		s.log.Warn("volume backup failed", "volumes", names, "user", user, "bytes", hw.n, "error", err)
		if !hw.committed {
			s.recordActivity(w, r, volumeJobKind, target, "", userError(err), lines, true, true)
			redirect(w, r, "/volumes")
			return
		}
		// The download already started: the browser shows it as failed.
		s.jobs.Record(volumeJobKind, target, user, lines, fmt.Errorf("download incomplete: %s", userError(err)))
		return
	}
	s.log.Info("volume backup downloaded", "volumes", names, "compression", comp, "bytes", hw.n, "user", user)
	lines = append([]string{fmt.Sprintf("Downloaded %s (%s).", filename, volbackup.FormatBytes(hw.n))}, lines...)
	s.jobs.Record(volumeJobKind, target, user, lines, nil)
}

// backupManifest describes the named volumes for a backup file.
func (s *Server) backupManifest(ctx context.Context, names []string, comp volbackup.Compression) (volbackup.Manifest, error) {
	vols, err := s.rt.ListVolumes(ctx)
	if err != nil {
		return volbackup.Manifest{}, err
	}
	users := s.volumeUsers(ctx)
	host, _ := os.Hostname()
	m := volbackup.Manifest{Created: time.Now().UTC(), Creator: strings.TrimSpace("ContainerUI " + s.version), Host: host, Runtime: s.rt.Name(), Compression: comp}
	for _, n := range names {
		if err := runtime.ValidateVolumeName(n); err != nil {
			return m, err
		}
		i := slices.IndexFunc(vols, func(v runtime.Volume) bool { return v.Name == n })
		if i < 0 {
			return m, &runtime.CommandError{Op: "volume backup", Output: "volume " + n + " not found", Kind: runtime.ErrNotFound}
		}
		v := vols[i]
		m.Volumes = append(m.Volumes, volbackup.Volume{Name: v.Name, Driver: v.Driver, Scope: v.Scope, Options: v.Options, Labels: v.Labels, Mountpoint: v.Mountpoint, UsedBy: users[v.Name]})
	}
	return m, nil
}

// ---- staged uploads ----

// uploadMaxAge is how long an uploaded backup waits to be restored.
const uploadMaxAge = 24 * time.Hour

// uploadStore keeps uploaded backup files until they are restored: <id>.tar
// with the file and <id>.json with its description.
type uploadStore struct {
	dir string
}

// stagedUpload describes an uploaded backup file.
type stagedUpload struct {
	ID       string                     `json:"id"`
	Owner    string                     `json:"owner"`
	FileName string                     `json:"fileName"`
	Size     int64                      `json:"size"`
	Uploaded time.Time                  `json:"uploaded"`
	Manifest volbackup.Manifest         `json:"manifest"`
	Stats    map[string]volbackup.Stats `json:"stats"`
}

var uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (u *uploadStore) dataPath(id string) string { return filepath.Join(u.dir, id+".tar") }
func (u *uploadStore) metaPath(id string) string { return filepath.Join(u.dir, id+".json") }

// sweep removes uploads older than uploadMaxAge and leftovers of failed
// uploads.
func (u *uploadStore) sweep(now time.Time) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > uploadMaxAge {
			_ = os.Remove(filepath.Join(u.dir, e.Name()))
		}
	}
}

// load returns the upload id of owner.
func (u *uploadStore) load(id, owner string) (*stagedUpload, bool) {
	if !uploadIDRe.MatchString(id) {
		return nil, false
	}
	b, err := os.ReadFile(u.metaPath(id))
	if err != nil {
		return nil, false
	}
	var st stagedUpload
	if json.Unmarshal(b, &st) != nil || st.Owner != owner || time.Since(st.Uploaded) > uploadMaxAge {
		return nil, false
	}
	if _, err := os.Stat(u.dataPath(id)); err != nil {
		return nil, false
	}
	return &st, true
}

func (u *uploadStore) remove(id string) {
	_ = os.Remove(u.dataPath(id))
	_ = os.Remove(u.metaPath(id))
}

// stage copies an uploaded file to the store while checking it.
func (u *uploadStore) stage(src io.Reader, fileName, owner string) (*stagedUpload, error) {
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return nil, err
	}
	u.sweep(time.Now())
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	st := &stagedUpload{ID: hex.EncodeToString(b), Owner: owner, FileName: filepath.Base(fileName), Uploaded: time.Now()}
	f, err := os.OpenFile(u.dataPath(st.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cw := &countWriter{w: f}
	tee := io.TeeReader(src, cw)
	st.Manifest, st.Stats, err = volbackup.Scan(tee)
	if err == nil {
		// The rest of the upload (tar padding) belongs to the file too.
		_, err = io.Copy(io.Discard, tee)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	st.Size = cw.n
	if err == nil {
		var meta []byte
		if meta, err = json.Marshal(st); err == nil {
			err = os.WriteFile(u.metaPath(st.ID), meta, 0o600)
		}
	}
	if err != nil {
		u.remove(st.ID)
		return nil, err
	}
	return st, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ---- restore pages ----

// volumeRestoreView is the restore page: the upload form (Upload nil) or
// the choice of what to restore from an uploaded file.
type volumeRestoreView struct {
	Err    string
	CSRF   string
	Upload *stagedUpload
	Rows   []restoreRow
	Stop   bool
	// Drivers the runtime accepts; a volume with another driver is created
	// with the default one.
	Drivers []string
}

// restoreRow is a volume of an uploaded backup file and what to do with it.
type restoreRow struct {
	Index    int
	Source   volbackup.Volume
	Stats    volbackup.Stats
	Selected bool
	Target   string
	Mode     string   // for an existing target: skip, replace or merge
	Exists   bool     // the target volume exists
	UsedBy   []string // containers that mount the existing target
	Driver   string   // the driver it is created with, if it is created
	Err      string
}

// Restore modes for a target volume that exists.
const (
	restoreSkip    = "skip"
	restoreReplace = "replace"
	restoreMerge   = "merge"
)

func (s *Server) handleVolumeRestorePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.restoreSupported(w, r); ok {
		s.renderRestore(w, r, volumeRestoreView{})
	}
}

func (s *Server) renderRestore(w http.ResponseWriter, r *http.Request, v volumeRestoreView) {
	v.CSRF = sessionFrom(r).CSRF
	v.Drivers = s.caps().VolumeDrivers
	s.render(w, "volume_restore", s.newPage(r, "Restore volumes", "volumes", v))
}

// handleVolumeRestoreUpload stages an uploaded backup file. The upload
// script sends the CSRF token as a header and wants JSON back; without it
// (no script) the form is parsed by the CSRF check and the answer is a page.
func (s *Server) handleVolumeRestoreUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.restoreSupported(w, r); !ok {
		return
	}
	user := sessionFrom(r).Username
	wantJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	// A large file takes longer than the server's read timeout.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	answer := func(st *stagedUpload, err error) {
		if err != nil {
			s.log.Warn("volume backup upload rejected", "user", user, "error", err)
			msg := userError(err)
			if wantJSON {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			s.renderRestore(w, r, volumeRestoreView{Err: msg})
			return
		}
		s.log.Info("volume backup uploaded", "file", st.FileName, "bytes", st.Size, "volumes", len(st.Manifest.Volumes), "user", user)
		to := "/volumes/restore/" + st.ID
		if wantJSON {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"redirect": to})
			return
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
	part, name, err := uploadedFile(r)
	if err != nil {
		answer(nil, err)
		return
	}
	defer part.Close()
	st, err := s.uploads.stage(part, name, user)
	answer(st, err)
}

// uploadedFile returns the "file" part of a multipart upload, streamed
// unless the CSRF check already parsed the form.
func uploadedFile(r *http.Request) (io.ReadCloser, string, error) {
	noFile := fmt.Errorf("%w: choose a backup file", runtime.ErrInvalid)
	if r.MultipartForm != nil {
		fhs := r.MultipartForm.File["file"]
		if len(fhs) == 0 {
			return nil, "", noFile
		}
		f, err := fhs[0].Open()
		return f, fhs[0].Filename, err
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return nil, "", noFile
		}
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
		}
		if p.FormName() == "file" && p.FileName() != "" {
			return p, p.FileName(), nil
		}
		_ = p.Close()
	}
}

// restoreRows lists the volumes of an upload with the defaults (all
// selected, same name, existing ones skipped) or the posted form.
func (s *Server) restoreRows(ctx context.Context, st *stagedUpload, form func(string) string) ([]restoreRow, error) {
	vols, err := s.rt.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, v := range vols {
		existing[v.Name] = true
	}
	users := s.volumeUsers(ctx)
	drivers := s.caps().VolumeDrivers
	rows := make([]restoreRow, len(st.Manifest.Volumes))
	for i, v := range st.Manifest.Volumes {
		row := restoreRow{Index: i, Source: v, Stats: st.Stats[v.Name], Selected: true, Target: v.Name, Mode: restoreSkip}
		if form != nil {
			idx := strconv.Itoa(i)
			row.Selected = form("sel_"+idx) == "on"
			row.Target = strings.TrimSpace(form("name_" + idx))
			if m := form("mode_" + idx); m == restoreReplace || m == restoreMerge {
				row.Mode = m
			}
		}
		row.Exists = existing[row.Target]
		row.UsedBy = users[row.Target]
		row.Driver = createDriver(v.Driver, drivers)
		rows[i] = row
	}
	return rows, nil
}

// createDriver is the driver a restored volume is created with: its own,
// if the runtime has it, else the runtime's default ("").
func createDriver(driver string, drivers []string) string {
	if slices.Contains(drivers, driver) {
		return driver
	}
	return ""
}

// validateRows checks the selected targets; it reports whether all are ok.
func validateRows(rows []restoreRow) bool {
	ok, selected := true, false
	seen := map[string]bool{}
	for i := range rows {
		row := &rows[i]
		if !row.Selected {
			continue
		}
		selected = true
		switch err := runtime.ValidateVolumeName(row.Target); {
		case row.Target == "":
			row.Err = "Enter the name of the volume to restore into."
		case err != nil:
			row.Err = userError(err)
		case seen[row.Target]:
			row.Err = "Another volume is restored into " + row.Target + "."
		}
		seen[row.Target] = true
		if row.Err != "" {
			ok = false
		}
	}
	return ok && selected
}

func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request) (*stagedUpload, bool) {
	st, ok := s.uploads.load(r.PathValue("id"), sessionFrom(r).Username)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		s.renderRestore(w, r, volumeRestoreView{Err: "The uploaded backup file is gone (restored, discarded or older than 24 hours). Upload it again."})
	}
	return st, ok
}

func (s *Server) handleVolumeRestoreChoose(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.restoreSupported(w, r); !ok {
		return
	}
	st, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	v := volumeRestoreView{Upload: st}
	var err error
	if v.Rows, err = s.restoreRows(r.Context(), st, nil); err != nil {
		v.Err = userError(err)
	}
	s.renderRestore(w, r, v)
}

func (s *Server) handleVolumeRestoreDiscard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.restoreSupported(w, r); !ok {
		return
	}
	if st, ok := s.uploads.load(r.PathValue("id"), sessionFrom(r).Username); ok {
		s.uploads.remove(st.ID)
		s.log.Info("volume backup upload discarded", "file", st.FileName, "user", st.Owner)
	}
	redirect(w, r, "/volumes")
}

// handleVolumeRestore starts restoring the chosen volumes of an upload in
// the background.
func (s *Server) handleVolumeRestore(w http.ResponseWriter, r *http.Request) {
	va, ok := s.restoreSupported(w, r)
	if !ok {
		return
	}
	st, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	user := sessionFrom(r).Username
	stop := r.PostFormValue("stop") == "on"
	rows, err := s.restoreRows(r.Context(), st, r.PostFormValue)
	if err == nil && !validateRows(rows) {
		err = fmt.Errorf("%w: check the volumes to restore", runtime.ErrInvalid)
		if !slices.ContainsFunc(rows, func(row restoreRow) bool { return row.Selected }) {
			err = fmt.Errorf("%w: select at least one volume", runtime.ErrInvalid)
		}
	}
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderRestore(w, r, volumeRestoreView{Upload: st, Rows: rows, Stop: stop, Err: userError(err)})
		return
	}
	var chosen []restoreRow
	var targets []string
	for _, row := range rows {
		if row.Selected {
			chosen = append(chosen, row)
			targets = append(targets, row.Target)
		}
	}
	s.log.Info("volume restore started", "file", st.FileName, "volumes", targets, "user", user)
	s.jobs.Start(volumeJobKind, "restore "+strings.Join(targets, ", "), user, func(ctx context.Context, progress func(string)) error {
		defer s.uploads.remove(st.ID)
		err := s.restoreVolumes(ctx, va, st, chosen, stop, progress)
		if err != nil {
			s.log.Warn("volume restore failed", "file", st.FileName, "volumes", targets, "user", user, "error", err)
			return errors.New(userError(err))
		}
		s.log.Info("volume restore finished", "file", st.FileName, "volumes", targets, "user", user)
		return nil
	})
	s.openActivity(w, r, true)
	redirect(w, r, "/volumes")
}

// restoreVolumes creates the missing target volumes and extracts the
// chosen volumes of an upload into them.
func (s *Server) restoreVolumes(ctx context.Context, va runtime.VolumeArchiver, st *stagedUpload, rows []restoreRow, stop bool, progress func(string)) (err error) {
	// What exists now counts, not what the form showed.
	vols, err := s.rt.ListVolumes(ctx)
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, v := range vols {
		exists[v.Name] = true
	}
	targets := map[string]volbackup.Target{}
	var mounted []string
	for _, row := range rows {
		if !exists[row.Target] {
			continue
		}
		if row.Mode == restoreSkip {
			progress(fmt.Sprintf("Skipped %s: volume %s exists.", row.Source.Name, row.Target))
			continue
		}
		mounted = append(mounted, row.Target)
		targets[row.Source.Name] = volbackup.Target{Name: row.Target, Replace: row.Mode == restoreReplace}
	}
	if stop && len(mounted) > 0 {
		var stopped []runtime.Container
		stopped, err = s.stopVolumeUsers(ctx, mounted, progress)
		defer func() {
			if serr := s.startAgain(ctx, stopped, progress); serr != nil {
				err = errors.Join(err, serr)
			}
		}()
		if err != nil {
			return err
		}
	}
	for _, row := range rows {
		if exists[row.Target] {
			continue
		}
		opts := runtime.CreateVolumeOpts{Name: row.Target, Driver: row.Driver}
		if row.Driver != "" {
			for _, k := range slices.Sorted(maps.Keys(row.Source.Options)) {
				opts.Options = append(opts.Options, k+"="+row.Source.Options[k])
			}
		} else if row.Source.Driver != "" {
			progress(fmt.Sprintf("Driver %s is not available: %s is created with the default driver, without driver options.", row.Source.Driver, row.Target))
		}
		for _, k := range slices.Sorted(maps.Keys(row.Source.Labels)) {
			opts.Labels = append(opts.Labels, k+"="+row.Source.Labels[k])
		}
		if _, err := s.rt.CreateVolume(ctx, opts); err != nil {
			return fmt.Errorf("create volume %s: %s", row.Target, userError(err))
		}
		progress("Created volume " + row.Target + ".")
		targets[row.Source.Name] = volbackup.Target{Name: row.Target}
	}
	if len(targets) == 0 {
		progress("Nothing to restore.")
		return nil
	}
	f, err := os.Open(s.uploads.dataPath(st.ID))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = volbackup.Restore(ctx, f, targets, va.ImportVolume, progress)
	return err
}
