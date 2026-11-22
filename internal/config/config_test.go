package config

import (
	"os"
	"path/filepath"
	"reflect"
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
		if cfg.Server.Listen != "0.0.0.0:8443" || !cfg.Server.TLS.On() || !cfg.Server.TLS.IsSelfSigned() {
			t.Errorf("server = %+v", cfg.Server)
		}
		if cfg.Runtime.Driver != "auto" || cfg.Runtime.PullTimeout != 30*time.Minute || cfg.Runtime.CommandTimeout != 30*time.Second {
			t.Errorf("runtime = %+v", cfg.Runtime)
		}
		if cfg.Server.PAMService != "login" || cfg.Server.MFA != MFAOptional || len(cfg.Users) != 0 {
			t.Errorf("cfg = %+v", cfg)
		}
		if cfg.Server.ListenIsLoopback() {
			t.Error("default listen should serve every interface")
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
		cfg.Server.SessionIdleTimeout != d.Server.SessionIdleTimeout || !cfg.Server.TLS.IsSelfSigned() ||
		!reflect.DeepEqual(cfg.Registry, d.Registry) || cfg.Metrics != d.Metrics {
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

func TestDataDir(t *testing.T) {
	for _, c := range []struct{ goos, programData, want string }{
		{"linux", "", "/var/lib/containerui"},
		{"windows", `D:\Data`, `D:\Data\ContainerUI`},
		{"windows", "", `C:\ProgramData\ContainerUI`},
	} {
		if got := DataDir(c.goos, c.programData); got != c.want {
			t.Errorf("DataDir(%q, %q) = %q, want %q", c.goos, c.programData, got, c.want)
		}
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
	for _, want := range []string{"# Container runtime: auto", "# Generate with: containerui hash-password", "listen: \"0.0.0.0:8443\""} {
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
	data, err := Template("0.0.0.0:9443", "/var/lib/containerui/compose", "/var/lib/containerui/events")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("template does not parse: %v\n%s", err, data)
	}
	if cfg.Server.Listen != "0.0.0.0:9443" || cfg.Runtime.ComposeDir != "/var/lib/containerui/compose" || cfg.Events.Dir != "/var/lib/containerui/events" {
		t.Errorf("listen %q, compose_dir %q, events.dir %q", cfg.Server.Listen, cfg.Runtime.ComposeDir, cfg.Events.Dir)
	}
	if !strings.Contains(string(data), "# deployed compose files") {
		t.Error("the comment of compose_dir was dropped")
	}
	same, err := Template("", "", "")
	if err != nil || string(same) != string(DefaultYAML) {
		t.Errorf("empty values changed the template: %v", err)
	}
}

func TestRuntimeSocket(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"/run/docker.sock":              "/run/docker.sock",
		"unix:///run/user/1000/d.sock":  "/run/user/1000/d.sock",
		" unix:///var/run/docker.sock ": "/var/run/docker.sock",
		"npipe:////./pipe/docker_x":     `\\.\pipe\docker_x`,
	} {
		cfg, err := Parse([]byte("runtime: { socket: \"" + in + "\" }"))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if cfg.Runtime.Socket != want {
			t.Errorf("socket %q = %q, want %q", in, cfg.Runtime.Socket, want)
		}
	}
	if _, err := Parse([]byte(`runtime: { socket: "tcp://10.0.0.1:2375" }`)); err == nil || !strings.Contains(err.Error(), "runtime.socket") {
		t.Errorf("tcp socket accepted: %v", err)
	}
}

func TestMetricsDefaultsAndLimits(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := Default().Metrics; cfg.Metrics != want || want.Interval != time.Minute ||
		want.DashboardInterval != 10*time.Second || want.DiskInterval != time.Hour {
		t.Errorf("metrics = %+v", cfg.Metrics)
	}
	for doc, field := range map[string]string{
		"metrics: { interval: 5s, dashboard_interval: 2s }":  "metrics.interval",
		"metrics: { dashboard_interval: 1s }":                "metrics.dashboard_interval",
		"metrics: { interval: 30s, dashboard_interval: 1m }": "must not exceed",
		"metrics: { disk_interval: 1m }":                     "metrics.disk_interval",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want %s", doc, err, field)
		}
	}
	cfg, err = Parse([]byte("metrics: { interval: 30s, dashboard_interval: 5s, disk_interval: 2h }"))
	if err != nil || cfg.Metrics != (Metrics{Interval: 30 * time.Second, DashboardInterval: 5 * time.Second, DiskInterval: 2 * time.Hour}) {
		t.Errorf("metrics = %+v, %v", cfg.Metrics, err)
	}
}

func TestActivityFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etc")
	cases := []struct {
		compose, file, want string
	}{
		{"compose", "", filepath.Join(dir, "activity.json")},
		{filepath.Join(dir, "..", "lib", "compose"), "", filepath.Join(dir, "..", "lib", "activity.json")},
		{"compose", "state/history.json", filepath.Join(dir, "state", "history.json")},
		{"", "", filepath.Join(dir, "activity.json")},
	}
	for _, c := range cases {
		cfg := &Config{Dir: dir, Runtime: Runtime{ComposeDir: c.compose}, Activity: Activity{File: c.file}}
		if got := cfg.ActivityFile(); got != filepath.Clean(c.want) {
			t.Errorf("compose %q file %q: got %q, want %q", c.compose, c.file, got, c.want)
		}
	}
}

func TestMFASetting(t *testing.T) {
	if _, err := Parse([]byte("server:\n  mfa: sometimes\n")); err == nil || !strings.Contains(err.Error(), "server.mfa") {
		t.Errorf("invalid server.mfa: %v", err)
	}
	cfg, err := Parse([]byte("server:\n  mfa: required\n"))
	if err != nil || cfg.Server.MFA != MFARequired {
		t.Fatalf("mfa: required = %v, %v", cfg, err)
	}
	cfg.Dir = "/etc/containerui"
	if got := cfg.MFAPath(); got != filepath.Join(filepath.Dir(cfg.ActivityFile()), "mfa.json") {
		t.Errorf("MFAPath = %s", got)
	}
	cfg.Server.MFAFile = "secrets/mfa.json"
	if got := cfg.MFAPath(); got != cfg.Resolve("secrets/mfa.json") {
		t.Errorf("MFAPath with mfa_file = %s", got)
	}
}

func TestKubernetesSection(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if k := cfg.Kubernetes; k.Kubeconfig != "" || k.Context != "" || len(k.Namespaces) != 0 || k.RequestTimeout != 30*time.Second {
		t.Errorf("defaults = %+v", k)
	}
	cfg, err = Parse([]byte(`
runtime: { driver: kubernetes }
kubernetes:
  kubeconfig: /etc/containerui/kubeconfig
  context: kube-1
  namespaces: [osem, longhorn-system]
  request_timeout: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	if k := cfg.Kubernetes; cfg.Runtime.Driver != "kubernetes" || k.Kubeconfig != "/etc/containerui/kubeconfig" ||
		k.Context != "kube-1" || strings.Join(k.Namespaces, ",") != "osem,longhorn-system" || k.RequestTimeout != time.Minute {
		t.Errorf("parsed = %+v", k)
	}
	for doc, field := range map[string]string{
		"kubernetes: { namespaces: [Osem] }":       "kubernetes.namespaces[0]",
		"kubernetes: { namespaces: [a_b] }":        "kubernetes.namespaces[0]",
		"kubernetes: { request_timeout: 100ms }":   "kubernetes.request_timeout",
		"kubernetes: { namespaces: [ok, '-bad'] }": "kubernetes.namespaces[1]",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want %s", doc, err, field)
		}
	}
}
