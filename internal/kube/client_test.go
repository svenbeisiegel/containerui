package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// clientCert returns a CA pool and a PEM client certificate and key it
// signed, like RKE2's client-ca and admin certificate.
func clientCert(t *testing.T) (*x509.CertPool, []byte, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "rke2-client-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "system:admin", Organization: []string{"system:masters"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func serverCA(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestClientCertificateAuth(t *testing.T) {
	pool, certPEM, keyPEM := clientCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cn := r.TLS.PeerCertificates[0].Subject.CommonName; cn != "system:admin" {
			t.Errorf("client CN = %q", cn)
		}
		if ua := r.Header.Get("User-Agent"); ua != "containerui/test" {
			t.Errorf("user agent = %q", ua)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("no bearer token expected")
		}
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"major":"1","minor":"36","gitVersion":"v1.36.6+rke2r1","platform":"linux/amd64"}`)
		case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			fmt.Fprint(w, `{"status":{"userInfo":{"username":"system:admin","groups":["system:masters","system:authenticated"]}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	defer srv.Close()

	c, err := New(&Config{Server: srv.URL, CAData: serverCA(srv), CertData: certPEM, KeyData: keyPEM}, Options{UserAgent: "containerui/test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.GitVersion != "v1.36.6+rke2r1" || v.Minor != "36" {
		t.Errorf("version = %+v", v)
	}
	u, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "system:admin" || len(u.Groups) != 2 {
		t.Errorf("user = %+v", u)
	}

	// Without the client certificate the handshake fails.
	c2, _ := New(&Config{Server: srv.URL, CAData: serverCA(srv)}, Options{})
	if _, err := c2.Version(context.Background()); err == nil {
		t.Error("expected a TLS error without a client certificate")
	}
	// An unknown server CA is rejected.
	if _, err := New(&Config{Server: srv.URL, CAData: []byte("junk")}, Options{}); err == nil {
		t.Error("expected an error for a CA without PEM")
	}
}

func TestBearerTokenFile(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := New(&Config{Server: srv.URL, CAData: serverCA(srv), TokenFile: tokFile}, Options{})
	var v map[string]any
	if err := c.Get(context.Background(), "/x", nil, &v); err != nil {
		t.Fatal(err)
	}
	// Rotated on disk: reused until tokenTTL passes, then re-read.
	if err := os.WriteFile(tokFile, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), "/x", nil, &v)
	c.tokenRead = time.Now().Add(-2 * tokenTTL)
	_ = c.Get(context.Background(), "/x", nil, &v)
	want := []string{"Bearer first", "Bearer first", "Bearer second"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("authorization = %q, want %q", got, want)
	}
}

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	c, err := New(&Config{Server: srv.URL, CAData: serverCA(srv), Token: "t"}, Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestErrorMapping(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		code := map[string]int{"/nf": 404, "/forbidden": 403, "/conflict": 409, "/plain": 500}[r.URL.Path]
		w.WriteHeader(code)
		if r.URL.Path == "/plain" {
			fmt.Fprint(w, "upstream broke\n")
			return
		}
		fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pods \"x\" is %d","reason":"R%d","code":%d}`, code, code, code)
	})
	for path, want := range map[string]error{"/nf": runtime.ErrNotFound, "/forbidden": ErrForbidden, "/conflict": runtime.ErrConflict} {
		var v any
		err := c.Get(context.Background(), path, nil, &v)
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", path, err, want)
		}
		var e *Error
		if !errors.As(err, &e) || !strings.HasPrefix(e.Reason, "R") || !strings.Contains(e.Message, `pods "x"`) {
			t.Errorf("%s: error = %#v", path, err)
		}
	}
	var v any
	err := c.Get(context.Background(), "/plain", nil, &v)
	var e *Error
	if !errors.As(err, &e) || e.Status != 500 || e.Message != "upstream broke" || errors.Unwrap(err) != nil {
		t.Errorf("plain error = %#v", err)
	}
}

func TestPathEscapes(t *testing.T) {
	if got := Path("api", "v1", "namespaces", "a/b", "pods", "x y"); got != "/api/v1/namespaces/a%2Fb/pods/x%20y" {
		t.Errorf("Path = %q", got)
	}
}

func TestListPages(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "500" || q.Get("labelSelector") != "app=wpi" {
			t.Errorf("query = %v", q)
		}
		switch q.Get("continue") {
		case "":
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"100","continue":"p2"},"items":[{"n":1},{"n":2}]}`)
		case "p2":
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"100"},"items":[{"n":3}]}`)
		}
	})
	items, rv, err := c.List(context.Background(), Path("api", "v1", "pods"), map[string][]string{"labelSelector": {"app=wpi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || rv != "100" || string(items[2]) != `{"n":3}` {
		t.Errorf("items = %s, rv = %q", items, rv)
	}
}

func TestWatch(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("watch") != "1" || q.Get("allowWatchBookmarks") != "true" {
			t.Errorf("query = %v", q)
		}
		f := w.(http.Flusher)
		switch q.Get("resourceVersion") {
		case "100":
			fmt.Fprintln(w, `{"type":"ADDED","object":{"metadata":{"name":"wpi-0","resourceVersion":"101"}}}`)
			f.Flush()
			fmt.Fprintln(w, `{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"105"}}}`)
			f.Flush()
			fmt.Fprintln(w, `{"type":"DELETED","object":{"metadata":{"name":"wpi-0","resourceVersion":"106"}}}`)
			// The server ends the watch: Watch returns nil.
		case "1":
			fmt.Fprintln(w, `{"type":"ERROR","object":{"kind":"Status","status":"Failure","message":"too old resource version: 1 (90)","reason":"Expired","code":410}}`)
		case "block":
			f.Flush()
			<-r.Context().Done()
		}
	})
	var types []string
	err := c.Watch(context.Background(), "/api/v1/pods", map[string][]string{"resourceVersion": {"100"}}, func(ev WatchEvent) error {
		var o struct {
			Metadata struct{ ResourceVersion string } `json:"metadata"`
		}
		if err := json.Unmarshal(ev.Object, &o); err != nil {
			return err
		}
		types = append(types, ev.Type+"@"+o.Metadata.ResourceVersion)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(types, " ") != "ADDED@101 BOOKMARK@105 DELETED@106" {
		t.Errorf("events = %v", types)
	}

	err = c.Watch(context.Background(), "/api/v1/pods", map[string][]string{"resourceVersion": {"1"}}, func(WatchEvent) error { return nil })
	if !errors.Is(err, ErrGone) {
		t.Errorf("err = %v, want ErrGone", err)
	}

	stop := errors.New("stop")
	err = c.Watch(context.Background(), "/api/v1/pods", map[string][]string{"resourceVersion": {"100"}}, func(WatchEvent) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("err = %v, want the callback's error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = c.Watch(ctx, "/api/v1/pods", map[string][]string{"resourceVersion": {"block"}}, func(WatchEvent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's error", err)
	}
}

func TestStream(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("container") != "couchdb" {
			t.Errorf("query = %v", r.URL.Query())
		}
		fmt.Fprint(w, "line 1\nline 2\n")
	})
	rc, err := c.Stream(context.Background(), Path("api", "v1", "namespaces", "osem", "pods", "wpi-0", "log"), map[string][]string{"container": {"couchdb"}})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "line 1\nline 2\n" {
		t.Errorf("body = %q", b)
	}
}
