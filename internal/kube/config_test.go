package kube

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// rke2Kubeconfig mirrors the admin kubeconfig RKE2 writes (inline data,
// one context named default).
func rke2Kubeconfig() string {
	return `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ` + b64("CA PEM") + `
    server: https://127.0.0.1:6443/
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: default
  user:
    client-certificate-data: ` + b64("CERT PEM") + `
    client-key-data: ` + b64("KEY PEM") + `
`
}

func TestParseRKE2Kubeconfig(t *testing.T) {
	cfg, err := parseKubeconfig([]byte(rke2Kubeconfig()), "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://127.0.0.1:6443" {
		t.Errorf("server = %q (trailing slash must go)", cfg.Server)
	}
	if cfg.Context != "default" || cfg.Namespace != "default" {
		t.Errorf("context/namespace = %q/%q", cfg.Context, cfg.Namespace)
	}
	if string(cfg.CAData) != "CA PEM" || string(cfg.CertData) != "CERT PEM" || string(cfg.KeyData) != "KEY PEM" {
		t.Errorf("data = %q %q %q", cfg.CAData, cfg.CertData, cfg.KeyData)
	}
	if cfg.Token != "" || cfg.TokenFile != "" || cfg.Insecure {
		t.Errorf("unexpected token/insecure: %+v", cfg)
	}
}

func TestParseContextFilesAndToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("FILE CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := `
current-context: a
clusters:
- name: c1
  cluster: {server: "https://a:6443"}
- name: c2
  cluster:
    server: https://b:6443
    certificate-authority: ca.crt
    tls-server-name: kubernetes
contexts:
- name: a
  context: {cluster: c1, user: u1}
- name: b
  context: {cluster: c2, user: u2, namespace: osem}
users:
- name: u1
  user: {token: abc}
- name: u2
  user: {tokenFile: tok}
`
	cfg, err := parseKubeconfig([]byte(doc), dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://b:6443" || cfg.Namespace != "osem" || cfg.ServerName != "kubernetes" {
		t.Errorf("cfg = %+v", cfg)
	}
	if string(cfg.CAData) != "FILE CA" {
		t.Errorf("relative CA file not resolved: %q", cfg.CAData)
	}
	if cfg.TokenFile != filepath.Join(dir, "tok") {
		t.Errorf("token file = %q", cfg.TokenFile)
	}

	cfg, err = parseKubeconfig([]byte(doc), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context != "a" || cfg.Token != "abc" || cfg.CAData != nil {
		t.Errorf("current context: %+v", cfg)
	}
}

func TestParseRejects(t *testing.T) {
	base := `
current-context: x
clusters: [{name: c, cluster: {server: "https://h"}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
`
	for name, tc := range map[string]struct{ doc, ctx, want string }{
		"exec":       {base + "users: [{name: u, user: {exec: {command: aws}}}]", "", "exec credential plugins"},
		"provider":   {base + "users: [{name: u, user: {auth-provider: {name: oidc}}}]", "", "auth-provider"},
		"basic":      {base + "users: [{name: u, user: {username: a, password: b}}]", "", "basic authentication"},
		"no context": {base + "users: [{name: u, user: {}}]", "nope", `context "nope" not found`},
		"no user":    {base, "", `user "u" not found`},
		"cert only":  {base + "users: [{name: u, user: {client-certificate-data: " + b64("x") + "}}]", "", "must both be set"},
		"bad b64":    {base + "users: [{name: u, user: {client-certificate-data: '%%%', client-key-data: '%%%'}}]", "", "invalid base64"},
		"not https":  {strings.Replace(base, "https://h", "h:6443", 1) + "users: [{name: u, user: {}}]", "", "not an http(s) URL"},
		"proxy":      {strings.Replace(base, `server: "https://h"`, `server: "https://h", proxy-url: "http://p"`, 1) + "users: [{name: u, user: {}}]", "", "proxy-url"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseKubeconfig([]byte(tc.doc), "/", tc.ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigSearchOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	envFile := filepath.Join(t.TempDir(), "env.yaml")
	t.Setenv("KUBECONFIG", filepath.Join(home, "missing")+string(os.PathListSeparator)+envFile)

	homeCfg := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(homeCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeCfg, []byte(rke2Kubeconfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != homeCfg {
		t.Errorf("source = %q, want ~/.kube/config while $KUBECONFIG files are missing", cfg.Source)
	}

	other := strings.Replace(rke2Kubeconfig(), "127.0.0.1", "10.0.0.1", 1)
	if err := os.WriteFile(envFile, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig("", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Source != envFile || cfg.Server != "https://10.0.0.1:6443" {
		t.Errorf("$KUBECONFIG must win: %q %q", cfg.Source, cfg.Server)
	}

	if _, err := LoadConfig(filepath.Join(home, "nope.yaml"), ""); err == nil {
		t.Error("an explicit missing path must fail")
	}
}

func TestInClusterConfig(t *testing.T) {
	dir := t.TempDir()
	old := serviceAccountDir
	serviceAccountDir = dir
	t.Cleanup(func() { serviceAccountDir = old })
	for name, data := range map[string]string{"token": "tok\n", "ca.crt": "CA", "namespace": "osem\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	cfg, err := inClusterConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://[fd00::1]:443" || cfg.Namespace != "osem" || cfg.Source != InCluster ||
		!bytes.Equal(cfg.CAData, []byte("CA")) || cfg.TokenFile != filepath.Join(dir, "token") {
		t.Errorf("cfg = %+v", cfg)
	}
}
