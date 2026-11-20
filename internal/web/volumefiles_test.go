package web

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVolumeFilesMenuEntry(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, p := range []string{"/volumes", "/volumes/data"} {
		if w := e.do("GET", p, nil); !strings.Contains(w.Body.String(), `href="/volumes/files/data"`) {
			t.Errorf("%s has no Browse files entry", p)
		}
	}
	e = newEnvWith(t, nil)
	e.rt.NoVolumeFiles = true
	e.login()
	if w := e.do("GET", "/volumes", nil); strings.Contains(w.Body.String(), "Browse files") {
		t.Error("Browse files offered without the capability")
	}
	if w := e.do("GET", "/volumes/files/data", nil); w.Code != http.StatusNotFound {
		t.Errorf("browser without the capability = %d", w.Code)
	}
}

func TestVolumeFilesList(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.SetVolumeEntry("data", tar.Header{Name: "conf/run.sh", Typeflag: tar.TypeReg, Mode: 0o4750, Uid: 1234, Gid: 5678, ModTime: time.Now()}, "#!/bin/sh\n")
	e.rt.SetVolumeEntry("data", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "conf/site.conf", Mode: 0o777, ModTime: time.Now()}, "")
	w := e.do("GET", "/volumes/files/data", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("root = %d", w.Code)
	}
	for _, want := range []string{`href="/volumes/files/data?path=conf"`, `/volumes/files/data/edit?path=index.html`, "→ conf/site.conf", "lrwxrwxrwx"} {
		if !strings.Contains(body, want) {
			t.Errorf("root lacks %q", want)
		}
	}
	// Folders come first.
	if strings.Index(body, ">conf<") > strings.Index(body, ">index.html<") {
		t.Error("folder not listed first")
	}
	w = e.do("GET", "/volumes/files/data?path=conf", nil)
	body = w.Body.String()
	for _, want := range []string{"-rwsr-x---", `title="4750"`, "1234:5678", `href="/volumes/files/data"`} {
		if !strings.Contains(body, want) {
			t.Errorf("conf lacks %q", want)
		}
	}
	// .. does not leave the volume.
	w = e.do("GET", "/volumes/files/data?path=../../etc", nil)
	if !strings.Contains(w.Body.String(), "No such file or folder") || !strings.Contains(e.rt.Calls[len(e.rt.Calls)-1], "list-volume-files data etc") {
		t.Errorf("traversal: %s / %v", w.Body.String(), e.rt.Calls[len(e.rt.Calls)-1])
	}
}

func TestVolumeFileRawAndDownload(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/volumes/files/data/raw?path=conf/site.conf", nil)
	h := w.Header()
	if w.Code != http.StatusOK || w.Body.String() != "listen 80;\n" || h.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox") || h.Get("X-File-Mtime") == "" || h.Get("X-File-Mode") != "0644" ||
		h.Get("Content-Disposition") != "" {
		t.Errorf("raw = %d %v %q", w.Code, h, w.Body.String())
	}
	w = e.do("GET", "/volumes/files/data/raw?path=conf/site.conf&download=1", nil)
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename=site.conf` || w.Header().Get("Content-Length") != "11" {
		t.Errorf("download headers = %v", w.Header())
	}
	big := strings.Repeat("x", editLimit+5)
	e.rt.SetVolumeFile("data", "big.log", big)
	w = e.do("GET", "/volumes/files/data/raw?path=big.log", nil)
	if w.Header().Get("X-File-Truncated") != "1" || w.Body.Len() != editLimit {
		t.Errorf("big = %v, %d bytes", w.Header(), w.Body.Len())
	}
	if w := e.do("GET", "/volumes/files/data/raw?path=conf", nil); w.Code != http.StatusBadRequest {
		t.Errorf("raw of a folder = %d", w.Code)
	}
	if w := e.do("GET", "/volumes/files/data/raw?path=nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("raw of a missing file = %d", w.Code)
	}
}

func TestVolumeFileEditPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/volumes/files/data/edit?path=conf/site.conf", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `data-raw="/volumes/files/data/raw?path=conf%2Fsite.conf"`) ||
		!strings.Contains(body, `/static/vendor/ace/ace.js`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "style-src 'self' 'unsafe-inline'") {
		t.Errorf("edit page = %d %v\n%s", w.Code, w.Header(), body)
	}
}

func (e *env) save(p string, body string, mtime string) (*http.Response, map[string]any) {
	e.t.Helper()
	w := e.doRaw("POST", "/volumes/files/data/save?path="+url.QueryEscape(p), strings.NewReader(body), func(r *http.Request) {
		r.Header.Set("X-CSRF-Token", e.csrf)
		if mtime != "" {
			r.Header.Set("X-Expect-Mtime", mtime)
		}
	})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Result(), out
}

func TestVolumeFileSave(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.SetVolumeEntry("data", tar.Header{Name: "app.ini", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1234, Gid: 5678, ModTime: time.Unix(1700000000, 0)}, "a=1\n")

	// Without the CSRF token nothing is written.
	w := e.doRaw("POST", "/volumes/files/data/save?path=app.ini", strings.NewReader("evil"))
	if w.Code != http.StatusForbidden {
		t.Errorf("save without CSRF = %d", w.Code)
	}
	// A stale mtime is refused.
	res, out := e.save("app.ini", "a=2\n", "1600000000")
	if res.StatusCode != http.StatusConflict || out["changed"] != true {
		t.Errorf("stale save = %d %v", res.StatusCode, out)
	}
	// The right one saves, keeping owner and mode.
	res, out = e.save("app.ini", "a=2\n", "1700000000")
	if res.StatusCode != http.StatusOK || out["mode"] != "0640" || out["owner"] != "1234:5678" || out["size"] != float64(4) {
		t.Errorf("save = %d %v", res.StatusCode, out)
	}
	h, _ := e.rt.VolumeEntryHeader("data", "app.ini")
	if h.Uid != 1234 || h.Gid != 5678 || h.Mode != 0o640 || e.rt.VolumeFiles("data")["app.ini"] != "a=2\n" {
		t.Errorf("after save: %+v %q", h, e.rt.VolumeFiles("data")["app.ini"])
	}
	// Forcing (no mtime) overwrites.
	if res, out = e.save("app.ini", "a=3\n", ""); res.StatusCode != http.StatusOK {
		t.Errorf("forced save = %d %v", res.StatusCode, out)
	}
	// Save never creates a file.
	if res, _ = e.save("new.ini", "x", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("save of a missing file = %d", res.StatusCode)
	}
}

func TestVolumeFileActions(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.SetVolumeEntry("data", tar.Header{Name: "conf", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 70, Gid: 71, ModTime: time.Now()}, "")

	w := e.post("/volumes/files/data/mkdir", url.Values{"path": {"conf"}, "name": {"sub"}}, htmx)
	if !strings.Contains(w.Body.String(), "Created folder sub.") {
		t.Errorf("mkdir: %s", w.Body.String())
	}
	if h, _ := e.rt.VolumeEntryHeader("data", "conf/sub"); h.Typeflag != tar.TypeDir || h.Uid != 70 || h.Mode != 0o755 {
		t.Errorf("sub = %+v", h)
	}
	w = e.post("/volumes/files/data/newfile", url.Values{"path": {"conf"}, "name": {"new.yaml"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/volumes/files/data/edit?path=conf%2Fnew.yaml" {
		t.Errorf("newfile = %v %s", w.Header(), w.Body.String())
	}
	if h, _ := e.rt.VolumeEntryHeader("data", "conf/new.yaml"); h.Uid != 70 || h.Mode != 0o644 {
		t.Errorf("new.yaml = %+v", h)
	}
	w = e.post("/volumes/files/data/newfile", url.Values{"path": {"conf"}, "name": {"new.yaml"}}, htmx)
	if !strings.Contains(w.Body.String(), "already exists") {
		t.Errorf("newfile over existing: %s", w.Body.String())
	}
	w = e.post("/volumes/files/data/newfile", url.Values{"path": {"conf"}, "name": {"../x"}}, htmx)
	if !strings.Contains(w.Body.String(), "may not contain /") {
		t.Errorf("newfile with a slash: %s", w.Body.String())
	}
	w = e.post("/volumes/files/data/rename", url.Values{"path": {"conf/new.yaml"}, "name": {"renamed.yaml"}}, htmx)
	if !strings.Contains(w.Body.String(), "Renamed new.yaml to renamed.yaml.") || e.rt.VolumeFiles("data")["conf/renamed.yaml"] != "" {
		t.Errorf("rename: %s %v", w.Body.String(), e.rt.VolumeFiles("data"))
	}
	if _, ok := e.rt.VolumeFiles("data")["conf/renamed.yaml"]; !ok {
		t.Error("renamed file missing")
	}
	w = e.post("/volumes/files/data/delete", url.Values{"path": {"conf"}}, htmx)
	if !strings.Contains(w.Body.String(), "Deleted conf.") {
		t.Errorf("delete: %s", w.Body.String())
	}
	for k := range e.rt.VolumeFiles("data") {
		if strings.HasPrefix(k, "conf") {
			t.Errorf("left after delete: %s", k)
		}
	}
	// The root cannot be deleted.
	w = e.post("/volumes/files/data/delete", url.Values{"path": {"/"}}, htmx)
	if !strings.Contains(w.Body.String(), "cannot be deleted") {
		t.Errorf("delete root: %s", w.Body.String())
	}
}

func (e *env) uploadFiles(query string, files map[string]string) (int, map[string]any) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, data := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write([]byte(data))
	}
	mw.Close()
	w := e.doRaw("POST", "/volumes/files/data/upload"+query, &body, func(r *http.Request) {
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("X-CSRF-Token", e.csrf)
	})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestVolumeFileUpload(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.SetVolumeEntry("data", tar.Header{Name: "conf/site.conf", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 33, Gid: 33, ModTime: time.Now()}, "old")

	code, out := e.uploadFiles("?path=conf", map[string]string{"a.txt": "A", "site.conf": "new"})
	if code != http.StatusOK || out["uploaded"] != float64(1) || !strings.Contains(out["error"].(string), "site.conf: exists already") {
		t.Errorf("upload = %d %v", code, out)
	}
	if e.rt.VolumeFiles("data")["conf/a.txt"] != "A" || e.rt.VolumeFiles("data")["conf/site.conf"] != "old" {
		t.Errorf("content = %v", e.rt.VolumeFiles("data"))
	}
	code, out = e.uploadFiles("?path=conf&overwrite=1", map[string]string{"site.conf": "new"})
	if code != http.StatusOK || out["error"] != "" {
		t.Errorf("overwrite = %d %v", code, out)
	}
	h, _ := e.rt.VolumeEntryHeader("data", "conf/site.conf")
	if e.rt.VolumeFiles("data")["conf/site.conf"] != "new" || h.Uid != 33 || fs.FileMode(h.Mode) != 0o600 {
		t.Errorf("overwritten = %+v %q", h, e.rt.VolumeFiles("data")["conf/site.conf"])
	}
	// A Windows path in the file name is reduced to the name.
	if code, _ = e.uploadFiles("", map[string]string{`C:\Users\me\notes.txt`: "n"}); code != http.StatusOK || e.rt.VolumeFiles("data")["notes.txt"] != "n" {
		t.Errorf("windows name = %d %v", code, e.rt.VolumeFiles("data"))
	}
	// Nothing uploaded is an error.
	if code, _ = e.uploadFiles("?path=missing", map[string]string{"x": "x"}); code != http.StatusUnprocessableEntity {
		t.Errorf("upload into a missing folder = %d", code)
	}
}

func TestUnixPerm(t *testing.T) {
	for mode, want := range map[fs.FileMode]string{
		fs.ModeDir | 0o755:                 "drwxr-xr-x",
		fs.ModeSetuid | 0o4755&0o777:       "-rwsr-xr-x",
		fs.ModeSetgid | 0o640:              "-rw-r-S---",
		fs.ModeDir | fs.ModeSticky | 0o777: "drwxrwxrwt",
		fs.ModeSymlink | 0o777:             "lrwxrwxrwx",
	} {
		if got := unixPerm(mode); got != want {
			t.Errorf("%v = %s, want %s", mode, got, want)
		}
	}
}
