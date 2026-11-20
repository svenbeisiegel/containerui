package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/volbackup"
)

// backup downloads a backup of the volumes.
func (e *env) backup(form url.Values) *bytes.Buffer {
	e.t.Helper()
	w := e.post("/volumes/backup", form)
	if w.Code != http.StatusOK {
		e.t.Fatalf("backup = %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
	return w.Body
}

// upload sends a backup file like the upload script does and returns the
// staged upload's id.
func (e *env) upload(name string, data []byte) (string, *httptest.ResponseRecorder) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	w := e.doRaw("POST", "/volumes/restore/upload", &body, func(r *http.Request) {
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("Accept", "application/json")
		r.Header.Set("X-CSRF-Token", e.csrf)
	})
	m := regexp.MustCompile(`"/volumes/restore/([0-9a-f]{32})"`).FindStringSubmatch(w.Body.String())
	if m == nil {
		return "", w
	}
	return m[1], w
}

// doRaw sends a request with a raw body.
func (e *env) doRaw(method, path string, body io.Reader, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, body)
	if e.cookie != nil {
		r.AddCookie(e.cookie)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestVolumeBackupDownload(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/volumes/backup", url.Values{"volume": {"data", "cache", "data"}, "compression": {"zstd"}})
	if w.Code != http.StatusOK {
		t.Fatalf("backup = %d\n%s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !regexp.MustCompile(`^attachment; filename="volumes_[A-Za-z0-9._-]+_\d{8}-\d{6}\.tar\.zst"$`).MatchString(cd) || w.Header().Get("Content-Type") != "application/zstd" {
		t.Errorf("headers = %v", w.Header())
	}
	m, stats, err := volbackup.Scan(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Volumes) != 2 || m.Volumes[0].Name != "data" || m.Volumes[1].Name != "cache" || m.Runtime != "fake" || m.Compression != volbackup.Zstd {
		t.Errorf("manifest = %+v", m)
	}
	if v := m.Volumes[0]; v.Driver != "guest" || v.Mountpoint == "" || !slices.Equal(v.UsedBy, []string{"web"}) {
		t.Errorf("data = %+v", v)
	}
	if stats["data"].Bytes == 0 || stats["cache"].Entries != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if a := e.activity(); !strings.Contains(a, "backup data, cache") || !strings.Contains(a, "Downloaded volumes_") {
		t.Errorf("activity: %s", a)
	}

	// A single volume, uncompressed, is named after the volume.
	w = e.post("/volumes/backup", url.Values{"volume": {"cache"}, "compression": {"none"}})
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="cache_`) || !strings.HasSuffix(cd, `.tar"`) {
		t.Errorf("single: %q", cd)
	}
	if _, _, err := volbackup.Scan(w.Body); err != nil {
		t.Errorf("single: %v", err)
	}

	// Errors before the download go to the activity side bar.
	for _, form := range []url.Values{{}, {"volume": {"nope"}}, {"volume": {"data"}, "compression": {"gzip"}}} {
		w = e.post("/volumes/backup", form)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/volumes" || w.Header().Get("Content-Disposition") != "" {
			t.Errorf("%v: %d %v", form, w.Code, w.Header())
		}
	}
	if a := e.activity(); !strings.Contains(a, "select at least one volume") || !strings.Contains(a, "volume nope not found") || !strings.Contains(a, "unknown compression") {
		t.Errorf("errors: %s", a)
	}
}

func TestVolumeBackupStopsUsers(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.backup(url.Values{"volume": {"data"}, "stop": {"on"}})
	want := []string{"stop c0ffee000001", "export-volume data", "start c0ffee000001"}
	var got []string
	for _, c := range e.rt.Calls {
		if strings.HasPrefix(c, "stop ") || strings.HasPrefix(c, "start ") || strings.HasPrefix(c, "export-volume") {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls = %q", e.rt.Calls)
	}
	if a := e.activity(); !strings.Contains(a, "Stopped container web.") || !strings.Contains(a, "Started container web again.") {
		t.Errorf("activity: %s", a)
	}
	if c := e.container("web"); c.State != runtime.StateRunning {
		t.Errorf("web is %s", c.State)
	}
}

func TestVolumeRestore(t *testing.T) {
	e := newEnv(t)
	e.login()
	file := e.backup(url.Values{"volume": {"data", "cache"}}).Bytes()

	id, w := e.upload("backup.tar.zst", file)
	if id == "" {
		t.Fatalf("upload = %d %s", w.Code, w.Body.String())
	}
	page := e.do("GET", "/volumes/restore/"+id, nil).Body.String()
	for _, s := range []string{"backup.tar.zst", `name="name_0" value="data"`, `name="name_1" value="cache"`, "exists", "used by web", `action="/volumes/restore/` + id + `/discard"`} {
		if !strings.Contains(page, s) {
			t.Errorf("choose page misses %q:\n%s", s, page)
		}
	}

	// Duplicate and invalid targets are rejected with the form kept.
	w2 := e.post("/volumes/restore/"+id, url.Values{"sel_0": {"on"}, "name_0": {"x"}, "sel_1": {"on"}, "name_1": {"x"}})
	if w2.Code != http.StatusUnprocessableEntity || !strings.Contains(w2.Body.String(), "Another volume is restored into x.") {
		t.Errorf("duplicate = %d %s", w2.Code, w2.Body.String())
	}
	w2 = e.post("/volumes/restore/"+id, url.Values{"sel_0": {"on"}, "name_0": {"-bad"}})
	if w2.Code != http.StatusUnprocessableEntity || !strings.Contains(w2.Body.String(), `value="-bad"`) {
		t.Errorf("invalid = %d", w2.Code)
	}

	// data into a new volume, cache replaces the existing one.
	e.rt.SetVolumeFile("cache", "stale.txt", "old")
	w2 = e.post("/volumes/restore/"+id, url.Values{"sel_0": {"on"}, "name_0": {"data-copy"}, "sel_1": {"on"}, "name_1": {"cache"}, "mode_1": {"replace"}, "stop": {"on"}})
	if w2.Code != http.StatusSeeOther || w2.Header().Get("Location") != "/volumes" {
		t.Fatalf("restore = %d %s", w2.Code, w2.Body.String())
	}
	waitFor(t, "restore job", func() bool { return strings.Contains(e.activity(), "Restored volume cache into cache") })
	got := e.rt.VolumeFiles("data-copy")
	if got["index.html"] != "<h1>Hello from the data volume</h1>\n" || got["conf/site.conf"] != "listen 80;\n" {
		t.Errorf("data-copy = %v", got)
	}
	if _, stale := e.rt.VolumeFiles("cache")["stale.txt"]; stale {
		t.Error("replace kept stale.txt")
	}
	if a := e.activity(); !strings.Contains(a, "Created volume data-copy.") {
		t.Errorf("activity: %s", a)
	}
	// The upload is gone after the restore.
	if w := e.do("GET", "/volumes/restore/"+id, nil); w.Code != http.StatusNotFound {
		t.Errorf("after restore = %d", w.Code)
	}
}

func TestVolumeRestoreSkipAndMerge(t *testing.T) {
	e := newEnv(t)
	e.login()
	file := e.backup(url.Values{"volume": {"data"}}).Bytes()
	e.rt.SetVolumeFile("data", "index.html", "changed")
	e.rt.SetVolumeFile("data", "extra.txt", "keep me")

	id, _ := e.upload("data.tar.zst", file)
	e.post("/volumes/restore/"+id, url.Values{"sel_0": {"on"}, "name_0": {"data"}}) // mode defaults to skip
	waitFor(t, "skip job", func() bool { return strings.Contains(e.activity(), "Skipped data: volume data exists.") })
	if e.rt.VolumeFiles("data")["index.html"] != "changed" {
		t.Error("skip changed the volume")
	}

	id, _ = e.upload("data.tar.zst", file)
	e.post("/volumes/restore/"+id, url.Values{"sel_0": {"on"}, "name_0": {"data"}, "mode_0": {"merge"}})
	waitFor(t, "merge job", func() bool { return strings.Contains(e.activity(), "Restored volume data into data") })
	if f := e.rt.VolumeFiles("data"); f["index.html"] != "<h1>Hello from the data volume</h1>\n" || f["extra.txt"] != "keep me" {
		t.Errorf("merge = %v", f)
	}
}

func TestVolumeRestoreUploadChecks(t *testing.T) {
	e := newEnv(t)
	e.login()
	if _, w := e.upload("junk.tar", []byte("this is not a backup")); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid volume backup") {
		t.Errorf("junk = %d %s", w.Code, w.Body.String())
	}
	entries, _ := os.ReadDir(e.srv.uploads.dir)
	if len(entries) != 0 {
		t.Errorf("rejected upload left %d files", len(entries))
	}

	// Without the upload script: a plain form post, CSRF token in the form.
	file := e.backup(url.Values{"volume": {"cache"}, "compression": {"none"}}).Bytes()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("csrf", e.csrf)
	fw, _ := mw.CreateFormFile("file", "cache.tar")
	fw.Write(file)
	mw.Close()
	w := e.doRaw("POST", "/volumes/restore/upload", &body, func(r *http.Request) { r.Header.Set("Content-Type", mw.FormDataContentType()) })
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/volumes/restore/") {
		t.Errorf("form upload = %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	// Without a CSRF token nothing is accepted.
	w = e.doRaw("POST", "/volumes/restore/upload", bytes.NewReader(file), func(r *http.Request) { r.Header.Set("Content-Type", "application/octet-stream") })
	if w.Code != http.StatusForbidden {
		t.Errorf("no csrf = %d", w.Code)
	}

	// Another user's upload is not shown; old uploads are swept.
	st, err := e.srv.uploads.stage(bytes.NewReader(file), "x.tar", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/volumes/restore/"+st.ID, nil); w.Code != http.StatusNotFound {
		t.Errorf("other user's upload = %d", w.Code)
	}
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(e.srv.uploads.dataPath(st.ID), old, old)
	os.Chtimes(e.srv.uploads.metaPath(st.ID), old, old)
	e.srv.uploads.sweep(time.Now())
	if _, err := os.Stat(e.srv.uploads.dataPath(st.ID)); !os.IsNotExist(err) {
		t.Errorf("old upload kept: %v", err)
	}

	// Discard removes an upload.
	id, _ := e.upload("cache.tar", file)
	if w := e.post("/volumes/restore/"+id+"/discard", nil); w.Code != http.StatusSeeOther {
		t.Errorf("discard = %d", w.Code)
	}
	if m, _ := filepath.Glob(filepath.Join(e.srv.uploads.dir, id+"*")); len(m) != 0 {
		t.Errorf("discard left %v", m)
	}
}

func TestVolumeBackupUnsupported(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.VolumeUploadDir = "" })
	e.login()
	if w := e.do("GET", "/volumes/restore", nil); w.Code != http.StatusNotFound {
		t.Errorf("restore without upload dir = %d", w.Code)
	}
	if body := e.do("GET", "/volumes", nil).Body.String(); strings.Contains(body, `href="/volumes/restore"`) || !strings.Contains(body, `id="volume-backup-form"`) {
		t.Error("restore button without upload dir")
	}
	e.rt.NoVolumeBackup = true
	if w := e.post("/volumes/backup", url.Values{"volume": {"data"}}); w.Code != http.StatusNotFound {
		t.Errorf("backup unsupported = %d", w.Code)
	}
	body := e.do("GET", "/volumes", nil).Body.String()
	if strings.Contains(body, "volume-backup-form") || strings.Contains(body, ">Backup<") {
		t.Error("backup controls shown without support")
	}
}
