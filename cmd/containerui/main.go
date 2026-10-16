// Command containerui serves a web UI for managing containers.
//
//	containerui [serve] [-config config.yaml] [-driver name] [-debug] [-logfile path]
//	containerui version
//	containerui init-config [-listen addr] [-compose-dir dir] <path|->
//	containerui hash-password
//
// Without -config it uses config.yaml next to the executable and writes the
// default configuration there if the file does not exist.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/logfile"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
	_ "github.com/svenbeisiegel/containerui/internal/runtime/fake"
	"github.com/svenbeisiegel/containerui/internal/tlsutil"
	"github.com/svenbeisiegel/containerui/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=...": the
// Makefile stamps the release version, build.ps1 a dev version.
var version = "dev"

// packaging names the package a build is made for, rpm or msi, stamped with
// -ldflags "-X main.packaging=...". The package then owns the executable and
// the service. Empty for development builds.
var packaging string

// errorOutput is where the error that ends the program is written: standard
// error, or the log file when -logfile names one, since a Windows service has
// no standard error that anybody reads.
var errorOutput io.Writer = os.Stderr

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "serve" && len(args) > 0 && (args[0] == "-version" || args[0] == "--version") {
		cmd = "version"
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "version":
		fmt.Println("containerui", version)
	case "init-config":
		err = initConfig(args)
	case "hash-password":
		err = hashPassword()
	case "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  containerui [serve] [-config config.yaml] [-driver name] [-debug] [-logfile path]
      Start the web UI. Without -config, config.yaml next to the executable
      is used (and created with the defaults if missing).
  containerui version
      Print the version.
  containerui init-config [-listen addr] [-compose-dir dir] <path|->
      Write the default configuration to a new file (- for standard output).
  containerui hash-password
      Read a password and print its bcrypt hash for the config file.
`)
}

type serveOptions struct {
	configPath, driver, logFile string
	debug                       bool
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var o serveOptions
	fs.StringVar(&o.configPath, "config", "", "path to the YAML config file (default: "+config.FileName+" next to the executable, created if missing)")
	fs.StringVar(&o.driver, "driver", "", "override runtime.driver from the config (available: "+strings.Join(runtime.Drivers(), ", ")+")")
	fs.BoolVar(&o.debug, "debug", false, "enable debug logging")
	fs.StringVar(&o.logFile, "logfile", "", "write the log to this file, rotated by size, instead of standard error")
	_ = fs.Parse(args)
	// start runs run, under the service manager when Windows started
	// ContainerUI as a service
	return start(func(stop <-chan struct{}) error { return run(o, stop) })
}

// run serves until a signal or stop ends it. stop is closed by the
// Windows service manager when it stops the service, and nil otherwise.
func run(o serveOptions, stop <-chan struct{}) error {
	level := slog.LevelInfo
	if o.debug {
		level = slog.LevelDebug
	}
	var output io.Writer = os.Stderr
	if o.logFile != "" {
		f, err := logfile.OpenFile(o.logFile)
		if err != nil {
			return err
		}
		// left open: it takes every record up to the last one, and the
		// error that ends the program after that
		output, errorOutput = f, f
	}
	log := slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("ContainerUI starting", "version", version, "os", goruntime.GOOS, "arch", goruntime.GOARCH,
		"pid", os.Getpid(), "installer", packaging, "service", stop != nil)

	cfg, path, err := loadConfig(o.configPath, log)
	if err != nil {
		return err
	}
	if o.driver != "" {
		cfg.Runtime.Driver = o.driver
	}

	// Linux signs in with system accounts (PAM), Windows with Windows
	// accounts unless server.system_accounts is off; the config's users
	// sign in on Windows too. Without system accounts the first start
	// creates a config user.
	linuxAuth := goruntime.GOOS == "linux"
	windowsAuth := goruntime.GOOS == "windows" && cfg.Server.SystemAccountsOn()
	var users auth.Verifier
	if !linuxAuth {
		if !windowsAuth {
			if cfg, err = ensureUser(cfg, path); err != nil {
				return err
			}
		}
		u, plaintext, err := auth.NewUsers(cfg.Users)
		if err != nil {
			return err
		}
		for _, name := range plaintext {
			log.Warn("user has a plaintext password in the config; use password_hash (see: containerui hash-password)", "user", name)
		}
		users = u
	}

	rt, err := runtime.Open(cfg.Runtime)
	if err != nil {
		return err
	}
	if d, ok := rt.(*cli.Driver); ok {
		log.Info("container runtime selected", "driver", d.Name(), "path", d.Path())
	}

	if linuxAuth || windowsAuth {
		socket := ""
		if d, ok := rt.(interface{ SocketPath() string }); ok {
			socket = d.SocketPath()
		}
		sys, err := auth.NewSystem(cfg.Server.PAMService, socket, log)
		if err != nil {
			return err
		}
		if linuxAuth {
			if len(cfg.Users) > 0 {
				log.Warn("users in the config are ignored on Linux; sign in with a system account")
			}
			log.Info("sign-in with system accounts", "pam_service", cfg.Server.PAMService, "runtime_socket", socket)
			users = sys
		} else {
			log.Info("sign-in with Windows accounts and config users", "runtime_pipe", socket, "config_users", len(cfg.Users))
			users = auth.Any{users, sys}
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stopped := false
	go func() {
		select {
		case <-stop:
			stopped = true
			cancel()
		case <-ctx.Done():
		}
	}()

	if info, err := rt.Info(ctx); err != nil {
		log.Warn("container runtime not reachable; the UI will show errors until it is", "driver", rt.Name(), "error", err)
	} else {
		log.Info("container runtime", "driver", info.Driver, "version", info.Version)
		warmUp(ctx, rt, log)
	}
	if ctx.Err() != nil {
		return nil // interrupted during warm-up
	}
	jm := jobs.NewManager(ctx)
	if caps := rt.Capabilities(); caps.Compose {
		log.Info("compose available", "tool", caps.ComposeVersion, "dir", cfg.Resolve(cfg.Runtime.ComposeDir))
	} else {
		log.Debug("compose not available (needs docker or podman with a compose tool)")
	}

	s, err := web.New(web.Options{
		Runtime:  rt,
		Users:    users,
		Sessions: auth.NewSessions(cfg.Server.SessionIdleTimeout, cfg.Server.SessionAbsoluteTimeout),
		Jobs:     jm,
		Secure:   cfg.Server.TLS.On(),
		Logger:   log,

		ComposeDir: cfg.Resolve(cfg.Runtime.ComposeDir),

		Version:     version,
		InstallType: installType(stop != nil),
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	scheme := "http"
	if cfg.Server.TLS.On() {
		scheme = "https"
		cert, err := loadCertificate(cfg, log)
		if err != nil {
			return err
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	} else if !cfg.Server.ListenIsLoopback() {
		log.Warn("serving plain HTTP on a non-loopback address: passwords and session cookies travel unencrypted; enable server.tls")
	}

	errc := make(chan error, 1)
	go func() {
		var err error
		if srv.TLSConfig != nil {
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		errc <- err
	}()
	log.Info("ContainerUI listening", "url", displayURL(scheme, cfg.Server.Listen))

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		if stopped {
			log.Info("shutting down, the service is stopped")
		} else {
			log.Info("shutting down")
		}
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "error", err)
	}
	jm.Wait()
	return nil
}

// installType describes how this ContainerUI runs, for the System page.
func installType(service bool) string {
	switch {
	case packaging != "":
		return packaging + " package"
	case service:
		return "Windows service"
	case os.Getenv("INVOCATION_ID") != "":
		return "systemd service"
	default:
		return "standalone"
	}
}

// warmUp loads all dashboard data once before the listener starts, so the
// first login doesn't pay for the runtime's cold start (wslc starts its
// session on first use, which takes seconds).
func warmUp(ctx context.Context, rt runtime.Runtime, log *slog.Logger) {
	log.Info("warming up container runtime")
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := runtime.WarmUp(ctx, rt); err != nil {
		log.Warn("runtime warm-up failed; starting anyway", "error", err, "took", time.Since(start).Round(time.Millisecond))
		return
	}
	log.Info("container runtime ready", "took", time.Since(start).Round(time.Millisecond))
}

func loadCertificate(cfg *config.Config, log *slog.Logger) (tls.Certificate, error) {
	t := cfg.Server.TLS
	certFile, keyFile := cfg.Resolve(t.CertFile), cfg.Resolve(t.KeyFile)
	if !t.IsSelfSigned() {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	if certFile == "" {
		certFile = cfg.Resolve("containerui-cert.pem")
	}
	if keyFile == "" {
		keyFile = cfg.Resolve("containerui-key.pem")
	}
	cert, created, err := tlsutil.LoadOrCreate(certFile, keyFile, tlsutil.DefaultHosts())
	if err != nil {
		return cert, fmt.Errorf("self-signed certificate: %w", err)
	}
	if created {
		log.Info("generated self-signed certificate", "cert", certFile, "key", keyFile)
	}
	return cert, nil
}

func displayURL(scheme, listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return scheme + "://" + listen + "/"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, port))
}

// loadConfig loads the config at path. An empty path means config.yaml
// next to the executable, which is created with the defaults if missing.
func loadConfig(path string, log *slog.Logger) (*config.Config, string, error) {
	if path != "" {
		cfg, err := config.Load(path)
		return cfg, path, err
	}
	path, err := config.DefaultPath()
	if err != nil {
		return nil, "", err
	}
	cfg, created, err := config.LoadOrCreate(path)
	if err != nil {
		return nil, "", err
	}
	if created {
		log.Info("wrote default config", "path", path)
	} else {
		log.Info("using config", "path", path)
	}
	return cfg, path, nil
}

// initConfig writes the default configuration, with the listen address and
// compose folder a package chooses, to a new file or standard output.
func initConfig(args []string) error {
	fs := flag.NewFlagSet("init-config", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address (default 0.0.0.0:8443)")
	composeDir := fs.String("compose-dir", "", "folder for compose projects deployed through the UI (default: compose, next to the config)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: containerui init-config [-listen addr] [-compose-dir dir] <path|->")
	}
	data, err := config.Template(*listen, *composeDir)
	if err != nil {
		return err
	}
	if path := fs.Arg(0); path != "-" {
		if err := config.WriteNew(path, data); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "wrote", path)
		return nil
	}
	_, err = os.Stdout.Write(data)
	return err
}

// ensureUser makes sure the config has at least one account. If it has
// none, it asks for a username and password on the terminal and adds the
// account to the config file.
func ensureUser(cfg *config.Config, path string) (*config.Config, error) {
	if len(cfg.Users) > 0 {
		return cfg, nil
	}
	if !isTerminal(os.Stdin) {
		return nil, fmt.Errorf("%s has no users: start containerui once in a terminal to create one, or add one under users (see: containerui hash-password)", path)
	}
	fmt.Fprintf(os.Stderr, "No user is configured yet. Create the account for signing in to ContainerUI.\nIt is saved in %s.\n", path)
	var name string
	in := bufio.NewReader(os.Stdin)
	for name == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("read username: %w", err)
		}
		name = strings.TrimSpace(line)
		if strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			fmt.Fprintln(os.Stderr, "The username must not contain spaces or control characters.")
			name = ""
		}
	}
	var pw string
	for pw == "" {
		p, err := readPasswordTwice()
		if err != nil && !errors.Is(err, errMismatch) {
			return nil, err
		}
		if err != nil || p == "" {
			fmt.Fprintln(os.Stderr, "The passwords are empty or do not match, please try again.")
			continue
		}
		pw = p
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return nil, err
	}
	if err := config.AddUser(path, name, hash); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "User %s added to %s.\n", name, path)
	return config.Load(path)
}

var errMismatch = errors.New("passwords do not match")

// readPasswordTwice reads a password and its repetition from the terminal
// without echo.
func readPasswordTwice() (string, error) {
	fmt.Fprint(os.Stderr, "Password: ")
	p1, err := readPassword(os.Stdin)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	p2, err := readPassword(os.Stdin)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", errMismatch
	}
	return string(p1), nil
}

func hashPassword() error {
	var pw string
	if isTerminal(os.Stdin) {
		p, err := readPasswordTwice()
		if err != nil {
			return err
		}
		pw = p
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read password from stdin: %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if pw == "" {
		return errors.New("empty password")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}
