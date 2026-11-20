// Package config loads and validates the ContainerUI YAML configuration.
// Every setting has a default, so ContainerUI also runs without a file.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultYAML is the commented default configuration written on first start.
//
//go:embed default.yaml
var DefaultYAML []byte

// FileName is the config file looked up next to the executable.
const FileName = "config.yaml"

// Config is the root configuration document.
type Config struct {
	Server   Server   `yaml:"server"`
	Runtime  Runtime  `yaml:"runtime"`
	Events   Events   `yaml:"events"`
	Activity Activity `yaml:"activity"`
	Registry Registry `yaml:"registry"`
	Metrics  Metrics  `yaml:"metrics"`
	Users    []User   `yaml:"users"`

	// Dir is the directory the config file was loaded from. Relative paths
	// (TLS files) are resolved against it.
	Dir string `yaml:"-"`
}

type Server struct {
	Listen                 string        `yaml:"listen"`
	TLS                    TLS           `yaml:"tls"`
	SessionIdleTimeout     time.Duration `yaml:"session_idle_timeout"`
	SessionAbsoluteTimeout time.Duration `yaml:"session_absolute_timeout"`
	PAMService             string        `yaml:"pam_service"` // Linux system login
	// SystemAccounts lets Windows accounts sign in (nil = default, on).
	// Linux always signs in with system accounts.
	SystemAccounts *bool `yaml:"system_accounts"`
}

// SystemAccountsOn reports whether system accounts may sign in.
func (s Server) SystemAccountsOn() bool { return s.SystemAccounts == nil || *s.SystemAccounts }

type TLS struct {
	Enabled    *bool  `yaml:"enabled"` // nil = default (on)
	SelfSigned *bool  `yaml:"self_signed"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
}

// On reports whether HTTPS is enabled.
func (t TLS) On() bool { return t.Enabled != nil && *t.Enabled }

// IsSelfSigned reports whether a self-signed certificate is generated.
func (t TLS) IsSelfSigned() bool { return t.SelfSigned != nil && *t.SelfSigned }

type Runtime struct {
	Driver  string `yaml:"driver"`
	Path    string `yaml:"path"`    // CLI binary; empty = the driver's default
	Session string `yaml:"session"` // wslc only
	// Socket is the Unix socket of the docker or podman daemon ("/path" or
	// "unix:///path"; Windows: a named pipe); empty = $DOCKER_HOST, then the runtime's default.
	// The CLI, the API access of the resource monitoring and the Linux
	// sign-in check all use it.
	Socket         string        `yaml:"socket"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
	PullTimeout    time.Duration `yaml:"pull_timeout"`

	// ComposePath is a standalone compose binary (docker-compose,
	// podman-compose); empty = detect the runtime's compose plugin first.
	ComposePath    string        `yaml:"compose_path"`
	ComposeTimeout time.Duration `yaml:"compose_timeout"`
	// ComposeDir holds the compose files deployed through the UI, one
	// directory per project (relative to the config file).
	ComposeDir string `yaml:"compose_dir"`

	// WSLC is the pre-1.1 layout (runtime.wslc.*), still accepted and
	// folded into the fields above.
	WSLC *legacyWSLC `yaml:"wslc,omitempty"`
}

type legacyWSLC struct {
	Path           string        `yaml:"path"`
	Session        string        `yaml:"session"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
	PullTimeout    time.Duration `yaml:"pull_timeout"`
}

// Events configures the error events kept from the systemd journal (Linux).
type Events struct {
	// Dir holds the events and their read state (relative to the config
	// file).
	Dir string `yaml:"dir"`
}

// Activity configures the history of the activity side bar.
type Activity struct {
	// File keeps the history across restarts (relative to the config
	// file); empty = activity.json next to the compose folder.
	File string `yaml:"file"`
}

// Registry configures the background check of local images for updates.
type Registry struct {
	// CheckUpdates asks registries for newer images (nil = default, on).
	CheckUpdates  *bool         `yaml:"check_updates"`
	CheckInterval time.Duration `yaml:"check_interval"` // how often all local images are checked
	Timeout       time.Duration `yaml:"timeout"`        // per registry request
	// AuthFile is a docker config.json or podman auth.json with logins;
	// empty = the runtime's usual files (relative to the config file).
	AuthFile string `yaml:"auth_file"`
	// Insecure lists registries (host[:port]) reached over plain HTTP.
	Insecure []string `yaml:"insecure_registries"`
}

// CheckUpdatesOn reports whether update checks are enabled.
func (r Registry) CheckUpdatesOn() bool { return r.CheckUpdates == nil || *r.CheckUpdates }

// Metrics configures the background monitoring of the containers.
type Metrics struct {
	// Interval is how often CPU, RAM and disk I/O are sampled.
	Interval time.Duration `yaml:"interval"`
	// DashboardInterval is the sampling interval while the dashboard is
	// shown.
	DashboardInterval time.Duration `yaml:"dashboard_interval"`
	// DiskInterval is how often the disk usage is measured.
	DiskInterval time.Duration `yaml:"disk_interval"`
}

// User is a login account. Either PasswordHash (bcrypt, preferred) or
// Password (plaintext) must be set.
type User struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash,omitempty"`
	Password     string `yaml:"password,omitempty"`
}

// Default returns the configuration used when no file sets anything.
func Default() *Config {
	on := true
	return &Config{
		Server: Server{
			Listen:                 "0.0.0.0:8443",
			TLS:                    TLS{Enabled: &on, SelfSigned: &on},
			SessionIdleTimeout:     30 * time.Minute,
			SessionAbsoluteTimeout: 12 * time.Hour,
			PAMService:             "login",
			SystemAccounts:         &on,
		},
		Runtime: Runtime{
			Driver:         "auto",
			CommandTimeout: 30 * time.Second,
			PullTimeout:    30 * time.Minute,
			ComposeTimeout: 30 * time.Minute,
			ComposeDir:     "compose",
		},
		Events:   Events{Dir: "events"},
		Registry: Registry{CheckUpdates: &on, CheckInterval: 30 * time.Minute, Timeout: 10 * time.Second, Insecure: []string{}},
		Metrics:  Metrics{Interval: time.Minute, DashboardInterval: 10 * time.Second, DiskInterval: time.Hour},
	}
}

// DefaultPath is config.yaml in the directory of the running executable.
func DefaultPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return filepath.Join(filepath.Dir(exe), FileName), nil
}

// DataDir is the folder the rpm, deb and msi packages keep everything but
// the binary in: the configuration, the TLS certificate, the activity
// history and the compose, logs and events folders. programData is the
// value of %ProgramData% on Windows, C:\ProgramData if empty.
func DataDir(goos, programData string) string {
	if goos == "windows" {
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return programData + `\ContainerUI`
	}
	return "/var/lib/containerui"
}

// Template is DefaultYAML with the listen address, the compose folder and
// the events folder replaced where they are set (empty keeps the default),
// which is how the installers write a configuration that fits the folders
// of the platform.
func Template(listen, composeDir, eventsDir string) ([]byte, error) {
	out := DefaultYAML
	for _, r := range []struct{ key, value string }{
		{"listen", listen},
		{"compose_dir", composeDir},
		{"dir", eventsDir},
	} {
		if r.value == "" {
			continue
		}
		var err error
		if out, err = replaceValue(out, r.key, r.value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// replaceValue replaces the value of the one line of data that sets key,
// keeping its indentation and comment.
func replaceValue(data []byte, key, value string) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		rest, ok := strings.CutPrefix(trimmed, key+":")
		if !ok {
			continue
		}
		if found {
			return nil, fmt.Errorf("default.yaml sets %s twice", key)
		}
		found = true
		indent := line[:len(line)-len(trimmed)]
		comment := ""
		if j := strings.Index(rest, " #"); j >= 0 {
			comment = "    " + strings.TrimLeft(rest[j:], " ")
		}
		quoted, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		lines[i] = indent + key + ": " + strings.TrimSpace(string(quoted)) + comment
	}
	if !found {
		return nil, fmt.Errorf("default.yaml does not set %s", key)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// WriteNew writes data to path with mode 0600, failing if path exists.
func WriteNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// LoadOrCreate loads the config at path. If path does not exist, it writes
// DefaultYAML there first; created reports whether that happened.
func LoadOrCreate(path string) (cfg *Config, created bool, err error) {
	switch err := WriteNew(path, DefaultYAML); {
	case err == nil:
		created = true
	case !errors.Is(err, os.ErrExist):
		return nil, false, fmt.Errorf("write default config: %w", err)
	}
	cfg, err = Load(path)
	return cfg, created, err
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.Dir = filepath.Dir(abs)
	return cfg, nil
}

// Parse decodes YAML, applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if w := c.Runtime.WSLC; w != nil {
		r := &c.Runtime
		if r.Driver == "" {
			r.Driver = "wslc"
		}
		r.Path = cmp(r.Path, w.Path)
		r.Session = cmp(r.Session, w.Session)
		r.CommandTimeout = cmp(r.CommandTimeout, w.CommandTimeout)
		r.PullTimeout = cmp(r.PullTimeout, w.PullTimeout)
		r.WSLC = nil
	}
	d := Default()
	c.Server.Listen = cmp(c.Server.Listen, d.Server.Listen)
	if c.Server.TLS.Enabled == nil {
		c.Server.TLS.Enabled = d.Server.TLS.Enabled
	}
	if c.Server.TLS.SelfSigned == nil {
		// Own certificate files imply not generating one.
		c.Server.TLS.SelfSigned = new(c.Server.TLS.CertFile == "" && c.Server.TLS.KeyFile == "")
	}
	c.Server.SessionIdleTimeout = cmp(c.Server.SessionIdleTimeout, d.Server.SessionIdleTimeout)
	c.Server.SessionAbsoluteTimeout = cmp(c.Server.SessionAbsoluteTimeout, d.Server.SessionAbsoluteTimeout)
	c.Server.PAMService = cmp(c.Server.PAMService, d.Server.PAMService)
	if c.Server.SystemAccounts == nil {
		c.Server.SystemAccounts = d.Server.SystemAccounts
	}
	c.Runtime.Driver = cmp(c.Runtime.Driver, d.Runtime.Driver)
	c.Runtime.CommandTimeout = cmp(c.Runtime.CommandTimeout, d.Runtime.CommandTimeout)
	c.Runtime.PullTimeout = cmp(c.Runtime.PullTimeout, d.Runtime.PullTimeout)
	c.Runtime.ComposeTimeout = cmp(c.Runtime.ComposeTimeout, d.Runtime.ComposeTimeout)
	c.Runtime.ComposeDir = cmp(c.Runtime.ComposeDir, d.Runtime.ComposeDir)
	c.Runtime.Socket = SocketPath(c.Runtime.Socket)
	c.Metrics.Interval = cmp(c.Metrics.Interval, d.Metrics.Interval)
	c.Metrics.DashboardInterval = cmp(c.Metrics.DashboardInterval, d.Metrics.DashboardInterval)
	c.Metrics.DiskInterval = cmp(c.Metrics.DiskInterval, d.Metrics.DiskInterval)
	c.Events.Dir = cmp(c.Events.Dir, d.Events.Dir)
	if c.Registry.CheckUpdates == nil {
		c.Registry.CheckUpdates = d.Registry.CheckUpdates
	}
	c.Registry.CheckInterval = cmp(c.Registry.CheckInterval, d.Registry.CheckInterval)
	c.Registry.Timeout = cmp(c.Registry.Timeout, d.Registry.Timeout)
	if c.Registry.Insecure == nil {
		c.Registry.Insecure = []string{}
	}
}

// cmp returns v, or def if v is the zero value.
func cmp[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func (c *Config) validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen %q: %w", c.Server.Listen, err))
	}
	t := c.Server.TLS
	if t.On() && !t.IsSelfSigned() && (t.CertFile == "" || t.KeyFile == "") {
		errs = append(errs, errors.New("server.tls: cert_file and key_file are required unless self_signed is true"))
	}
	seen := map[string]bool{}
	for i, u := range c.Users {
		switch {
		case u.Username == "":
			errs = append(errs, fmt.Errorf("users[%d]: username is required", i))
		case seen[u.Username]:
			errs = append(errs, fmt.Errorf("users[%d]: duplicate username %q", i, u.Username))
		}
		seen[u.Username] = true
		if u.PasswordHash == "" && u.Password == "" {
			errs = append(errs, fmt.Errorf("users[%d] (%s): password_hash or password is required", i, u.Username))
		}
		if u.PasswordHash != "" && u.Password != "" {
			errs = append(errs, fmt.Errorf("users[%d] (%s): set only one of password_hash and password", i, u.Username))
		}
	}
	if s := c.Runtime.Socket; s != "" && strings.Contains(s, "://") {
		errs = append(errs, fmt.Errorf("runtime.socket %q: only unix:// sockets and npipe:// pipes are supported", s))
	}
	m := c.Metrics
	if m.Interval < 10*time.Second {
		errs = append(errs, fmt.Errorf("metrics.interval %s: must be at least 10s", m.Interval))
	}
	if m.DashboardInterval < 2*time.Second {
		errs = append(errs, fmt.Errorf("metrics.dashboard_interval %s: must be at least 2s", m.DashboardInterval))
	}
	if m.DashboardInterval > m.Interval {
		errs = append(errs, fmt.Errorf("metrics.dashboard_interval %s: must not exceed metrics.interval %s", m.DashboardInterval, m.Interval))
	}
	if m.DiskInterval < 5*time.Minute {
		errs = append(errs, fmt.Errorf("metrics.disk_interval %s: must be at least 5m", m.DiskInterval))
	}
	if c.Registry.CheckInterval < time.Minute {
		errs = append(errs, fmt.Errorf("registry.check_interval %s: must be at least 1m", c.Registry.CheckInterval))
	}
	return errors.Join(errs...)
}

// SocketPath turns a runtime.socket value into a path: "unix:///x" becomes
// "/x", a Windows "npipe:////./pipe/x" becomes `\\.\pipe\x`. Other schemes are
// returned unchanged (and rejected by validate).
func SocketPath(s string) string {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutPrefix(s, "unix://"); ok {
		return p
	}
	if p, ok := strings.CutPrefix(s, "npipe://"); ok {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return s
}

// ListenIsLoopback reports whether the listen address only binds loopback.
func (s Server) ListenIsLoopback() bool {
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ActivityFile is the file the activity history is kept in: activity.file,
// or activity.json in the folder that holds the compose folder, which is
// the data folder of the Linux and Windows packages (/var/lib/containerui,
// ProgramData\ContainerUI) and the config folder otherwise.
func (c *Config) ActivityFile() string {
	if c.Activity.File != "" {
		return c.Resolve(c.Activity.File)
	}
	compose := c.Runtime.ComposeDir
	if compose == "" {
		compose = "compose"
	}
	return filepath.Join(filepath.Dir(c.Resolve(compose)), "activity.json")
}

// Resolve returns p made absolute relative to the config directory.
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}
