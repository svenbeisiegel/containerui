package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRef(t *testing.T) {
	cases := map[string]string{
		"nginx":                                    "docker.io/library/nginx:latest",
		"nginx:1.27":                               "docker.io/library/nginx:1.27",
		"docker.io/library/nginx:1.27":             "docker.io/library/nginx:1.27",
		"index.docker.io/bitnami/redis":            "docker.io/bitnami/redis:latest",
		"ghcr.io/org/app:v2.1.0":                   "ghcr.io/org/app:v2.1.0",
		"localhost:5000/app":                       "localhost:5000/app:latest",
		"registry.local:5000/a/b:1-x":              "registry.local:5000/a/b:1-x",
		"nginx@sha256:" + strings.Repeat("ab", 32): "docker.io/library/nginx@sha256:" + strings.Repeat("ab", 32),
	}
	for in, want := range cases {
		r, err := ParseRef(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseRef(%q) = %q, %v; want %q", in, r.String(), err, want)
		}
	}
	for _, bad := range []string{"sha256:" + strings.Repeat("a", 64), "a1b2c3d4e5f6", "UPPER/case", "nginx:", "nginx@md5:xyz"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) should fail", bad)
		}
	}
}

func TestNewerTags(t *testing.T) {
	tags := []string{"latest", "1.26", "1.27", "1.27.3", "1.27.4", "1.28.0", "2.0.0", "1.28.0-alpine", "1.29.1-alpine", "1.27.3-alpine",
		"v1.27.9", "1.30.0-rc1", "1.27.5", "1.27.6", "1.27.7", "mainline"}
	got := NewerTags("1.27.3", tags)
	want := []Tag{{"2.0.0", "major"}, {"1.28.0", "minor"}, {"1.27.7", "patch"}, {"1.27.6", "patch"}, {"1.27.5", "patch"}}
	if !slices.Equal(got, want) {
		t.Errorf("NewerTags(1.27.3) = %v; want %v", got, want)
	}
	if got := NewerTags("1.27.3-alpine", tags); !slices.Equal(got, []Tag{{"1.29.1-alpine", "minor"}, {"1.28.0-alpine", "minor"}}) {
		t.Errorf("alpine = %v", got)
	}
	if got := NewerTags("1.26", tags); !slices.Equal(got, []Tag{{"1.27", "minor"}}) {
		t.Errorf("1.26 = %v", got)
	}
	if got := NewerTags("latest", tags); got != nil {
		t.Errorf("latest = %v", got)
	}
}

// fakeRegistry serves one repository behind a bearer token service.
type fakeRegistry struct {
	srv      *httptest.Server
	digest   string
	tags     []string
	user     string // required basic login at the token service, "" = anonymous
	pass     string
	noHead   bool // HEAD without Docker-Content-Digest
	tokens   atomic.Int32
	requests atomic.Int32
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{digest: "sha256:" + strings.Repeat("1", 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:team/app:pull" || r.URL.Query().Get("service") != "fake" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		if u, p, _ := r.BasicAuth(); f.user != "" && (u != f.user || p != f.pass) {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		f.tokens.Add(1)
		fmt.Fprint(w, `{"token":"tok","expires_in":300}`)
	})
	mux.HandleFunc("/v2/team/app/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:team/app:pull"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/team/app/manifests/1.0":
			if !strings.Contains(r.Header.Get("Accept"), "manifest.list.v2") {
				http.Error(w, "accept", http.StatusBadRequest)
				return
			}
			if !(f.noHead && r.Method == http.MethodHead) {
				w.Header().Set("Docker-Content-Digest", f.digest)
			}
			if f.noHead {
				w.Header().Del("Docker-Content-Digest")
			}
			fmt.Fprint(w, `{"manifest":true}`)
		case r.URL.Path == "/v2/team/app/tags/list" && r.URL.Query().Get("last") == "":
			w.Header().Set("Link", `</v2/team/app/tags/list?n=1000&last=1.0>; rel="next"`)
			fmt.Fprintf(w, `{"name":"team/app","tags":["0.9","1.0"]}`)
		case r.URL.Path == "/v2/team/app/tags/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "team/app", "tags": f.tags})
		default:
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func TestClientTokenFlowAndPagination(t *testing.T) {
	f := newFakeRegistry(t)
	f.tags = []string{"1.1", "2.0"}
	c := NewClient(5*time.Second, nil, []string{f.host()})
	ref, err := ParseRef(f.host() + "/team/app:1.0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if d, err := c.Digest(ctx, ref); err != nil || d != f.digest {
		t.Fatalf("Digest = %q, %v", d, err)
	}
	tags, err := c.Tags(ctx, ref)
	if err != nil || !slices.Equal(tags, []string{"0.9", "1.0", "1.1", "2.0"}) {
		t.Fatalf("Tags = %q, %v", tags, err)
	}
	if n := f.tokens.Load(); n != 1 {
		t.Errorf("%d tokens fetched; the token should be reused", n)
	}

	// The manifest is hashed when HEAD has no digest header.
	f.noHead = true
	sum := sha256.Sum256([]byte(`{"manifest":true}`))
	if d, err := c.Digest(ctx, ref); err != nil || d != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("hashed digest = %q, %v", d, err)
	}

	// Unknown repositories and refused logins are reported.
	missing, _ := ParseRef(f.host() + "/team/app:9.9")
	if _, err := c.Digest(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag err = %v", err)
	}
	f.user, f.pass = "bob", "secret"
	c2 := NewClient(5*time.Second, nil, []string{f.host()})
	if _, err := c2.Digest(ctx, ref); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("anonymous err = %v", err)
	}
}

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientWithAuthFile(t *testing.T) {
	f := newFakeRegistry(t)
	f.user, f.pass = "bob", "s3cret:x"
	auth := base64.StdEncoding.EncodeToString([]byte("bob:s3cret:x"))
	file := writeAuthFile(t, fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, f.host(), auth))
	c := NewClient(5*time.Second, FileCredentials([]string{"/does/not/exist", file}), []string{f.host()})
	ref, _ := ParseRef(f.host() + "/team/app:1.0")
	if d, err := c.Digest(context.Background(), ref); err != nil || d != f.digest {
		t.Fatalf("Digest = %q, %v", d, err)
	}
}

func TestFileCredentials(t *testing.T) {
	file := writeAuthFile(t, `{
		"auths": {"https://index.docker.io/v1/": {}, "ghcr.io": {"auth": "`+base64.StdEncoding.EncodeToString([]byte("me:pw"))+`"}, "quay.io": {"username": "u", "password": "p"}},
		"credsStore": "desktop",
		"credHelpers": {"123.dkr.ecr.eu-west-1.amazonaws.com": "ecr-login"}
	}`)
	creds := FileCredentials([]string{file})
	check := func(host, wantUser, wantPass string, wantErr error) {
		t.Helper()
		u, p, err := creds(host)
		if u != wantUser || p != wantPass || !errors.Is(err, wantErr) {
			t.Errorf("creds(%s) = %q, %q, %v", host, u, p, err)
		}
	}
	check("ghcr.io", "me", "pw", nil)
	check("quay.io", "u", "p", nil)
	check(DockerHub, "", "", ErrCredentialHelper)
	check("123.dkr.ecr.eu-west-1.amazonaws.com", "", "", ErrCredentialHelper)
	check("other.example.com", "", "", nil)
}

func TestChecker(t *testing.T) {
	f := newFakeRegistry(t)
	f.tags = []string{"1.1", "2.0", "latest"}
	c := NewChecker(NewClient(5*time.Second, nil, []string{f.host()}), time.Hour)
	ref := f.host() + "/team/app:1.0"
	ctx := context.Background()

	s := c.Check(ctx, ref, []string{f.host() + "/team/app@" + f.digest}, false)
	if s.Err != "" || s.DigestChanged || s.DigestUnknown || !slices.Equal(s.NewerTags, []Tag{{"2.0", "major"}, {"1.1", "minor"}}) || !s.UpdateAvailable() {
		t.Errorf("status = %+v", s)
	}
	before := f.requests.Load()
	s = c.Check(ctx, ref, []string{f.host() + "/team/app@sha256:" + strings.Repeat("2", 64), "other/repo@" + f.digest}, false)
	if !s.DigestChanged || s.DigestUnknown {
		t.Errorf("rebuilt tag not detected: %+v", s)
	}
	if f.requests.Load() != before {
		t.Error("cached answer was not used")
	}
	if s = c.Check(ctx, ref, nil, true); !s.DigestUnknown || s.DigestChanged || f.requests.Load() == before {
		t.Errorf("forced check without local digests = %+v", s)
	}
	if s = c.Check(ctx, "nginx@sha256:"+strings.Repeat("a", 64), nil, false); !s.Pinned || s.UpdateAvailable() {
		t.Errorf("pinned = %+v", s)
	}
	if s = c.Check(ctx, "sha256:"+strings.Repeat("a", 64), nil, false); s.Err == "" {
		t.Errorf("image ID = %+v", s)
	}
	if s = c.Check(ctx, f.host()+"/team/app:9.9", nil, false); !strings.Contains(s.Err, "not found") {
		t.Errorf("missing tag = %+v", s)
	}
	// Failures are only reused for a minute.
	now := time.Now()
	c.now = func() time.Time { return now.Add(30 * time.Second) }
	before = f.requests.Load()
	c.Check(ctx, f.host()+"/team/app:9.9", nil, false)
	if f.requests.Load() != before {
		t.Error("failure not reused within a minute")
	}
	c.now = func() time.Time { return now.Add(2 * time.Minute) }
	c.Check(ctx, f.host()+"/team/app:9.9", nil, false)
	if f.requests.Load() == before {
		t.Error("failure reused after a minute")
	}
}

func TestUnreachableRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient(50*time.Millisecond, nil, []string{host})
	ref, _ := ParseRef(host + "/team/app:1.0")
	if _, err := c.Digest(context.Background(), ref); err == nil || err.Error() != host+" did not answer within 50ms" {
		t.Errorf("timeout err = %v", err)
	}
	srv.Close()
	if _, err := c.Digest(context.Background(), ref); err == nil || !strings.HasPrefix(err.Error(), "cannot reach "+host+": ") {
		t.Errorf("closed err = %v", err)
	}
}
