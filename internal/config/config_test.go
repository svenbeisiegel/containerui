package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	for _, doc := range []string{"", "# only a comment\n"} {
		cfg, err := Parse([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Listen != "127.0.0.1:8443" || !cfg.Server.TLS.On() || !cfg.Server.TLS.IsSelfSigned() {
			t.Errorf("server = %+v", cfg.Server)
		}
		if cfg.Runtime.Driver != "auto" || cfg.Runtime.PullTimeout != 30*time.Minute || cfg.Runtime.CommandTimeout != 30*time.Second {
			t.Errorf("runtime = %+v", cfg.Runtime)
		}
		if cfg.Server.PAMService != "login" || len(cfg.Users) != 0 {
			t.Errorf("cfg = %+v", cfg)
		}
		if !cfg.Server.ListenIsLoopback() {
			t.Error("default listen should be loopback")
		}
	}
}

func TestDefaultYAMLMatchesDefaults(t *testing.T) {
	cfg, err := Parse(DefaultYAML)
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if cfg.Server.Listen != d.Server.Listen || cfg.Runtime != d.Runtime ||
		cfg.Server.SessionIdleTimeout != d.Server.SessionIdleTimeout || !cfg.Server.TLS.IsSelfSigned() {
		t.Errorf("default.yaml = %+v, Default() = %+v", cfg, d)
	}
}

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  listen: "0.0.0.0:8443"
  tls: { enabled: true, self_signed: true }
  session_idle_timeout: 10m
runtime:
  driver: docker
  path: /usr/local/bin/docker
  command_timeout: 5s
users:
  - { username: a, password: secret }
  - { username: b, password_hash: "$2a$10$x" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.SessionIdleTimeout != 10*time.Minute || !cfg.Server.TLS.IsSelfSigned() {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Runtime.Driver != "docker" || cfg.Runtime.Path != "/usr/local/bin/docker" || cfg.Runtime.CommandTimeout != 5*time.Second {
		t.Errorf("runtime = %+v", cfg.Runtime)
	}
	if cfg.Server.ListenIsLoopback() {
		t.Error("0.0.0.0 is not loopback")
	}
}

func TestParseTLSOff(t *testing.T) {
	cfg, err := Parse([]byte(`server: { listen: "127.0.0.1:8080", tls: { enabled: false } }`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.On() {
		t.Error("tls should be off")
	}
	cfg, err = Parse([]byte(`server: { tls: { cert_file: c.pem, key_file: k.pem } }`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.TLS.On() || cfg.Server.TLS.IsSelfSigned() {
		t.Errorf("own certificate: tls = %+v", cfg.Server.TLS)
	}
}

func TestParseLegacyWSLC(t *testing.T) {
	cfg, err := Parse([]byte(`
runtime:
  wslc: { path: 'C:\wslc.exe', session: "s1", command_timeout: 5s }
`))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Runtime
	if r.Driver != "wslc" || r.Path != `C:\wslc.exe` || r.Session != "s1" || r.CommandTimeout != 5*time.Second || r.PullTimeout != 30*time.Minute || r.WSLC != nil {
		t.Errorf("runtime = %+v", r)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no password":     "users: [{username: a}]",
		"both passwords":  "users: [{username: a, password: x, password_hash: y}]",
		"duplicate user":  "users: [{username: a, password: x}, {username: a, password: y}]",
		"bad listen":      "server: { listen: nope }",
		"tls needs files": "server: { tls: { enabled: true, self_signed: false } }",
		"unknown field":   "bogus: 1",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("expected error for %s", strings.TrimSpace(doc))
			}
		})
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	cfg, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if cfg.Runtime.Driver != "auto" || cfg.Dir != filepath.Dir(path) {
		t.Errorf("cfg = %+v", cfg)
	}
	if err := os.WriteFile(path, []byte("server: { listen: \"127.0.0.1:9000\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, created, err = LoadOrCreate(path)
	if err != nil || created || cfg.Server.Listen != "127.0.0.1:9000" {
		t.Fatalf("second: created=%v err=%v cfg=%+v", created, err, cfg)
	}
}

func TestAddUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, DefaultYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddUser(path, "admin", "$2a$12$hash"); err != nil {
		t.Fatal(err)
	}
	if err := AddUser(path, "bob", "$2a$12$other"); err != nil {
		t.Fatal(err)
	}
	if err := AddUser(path, "bob", "$2a$12$again"); err == nil {
		t.Error("duplicate user accepted")
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"# Container runtime: auto", "# Generate with: containerui hash-password", "listen: \"127.0.0.1:8443\""} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lost %q:\n%s", want, data)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Users) != 2 || cfg.Users[0] != (User{Username: "admin", PasswordHash: "$2a$12$hash"}) || cfg.Users[1].Username != "bob" {
		t.Errorf("users = %+v\n%s", cfg.Users, data)
	}
}

func TestAddUserNoUsersKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("# mine\nserver:\n  listen: \"127.0.0.1:9000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddUser(path, "admin", "$2a$12$hash"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Users) != 1 || cfg.Server.Listen != "127.0.0.1:9000" {
		t.Fatalf("err=%v cfg=%+v", err, cfg)
	}
}

func TestTemplate(t *testing.T) {
	data, err := Template("0.0.0.0:9443", "/var/lib/containerui/compose")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("template does not parse: %v\n%s", err, data)
	}
	if cfg.Server.Listen != "0.0.0.0:9443" || cfg.Runtime.ComposeDir != "/var/lib/containerui/compose" {
		t.Errorf("listen %q, compose_dir %q", cfg.Server.Listen, cfg.Runtime.ComposeDir)
	}
	if !strings.Contains(string(data), "# deployed compose files") {
		t.Error("the comment of compose_dir was dropped")
	}
	same, err := Template("", "")
	if err != nil || string(same) != string(DefaultYAML) {
		t.Errorf("empty values changed the template: %v", err)
	}
}
