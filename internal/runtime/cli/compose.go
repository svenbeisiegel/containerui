package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// composeCandidate is a compose tool a flavor may have: the CLI's own
// "compose" subcommand (plugin) or a standalone binary on PATH.
type composeCandidate struct {
	plugin bool
	binary string
}

type composeKind int

const (
	// composeV2 is Docker Compose v2, as "docker compose", docker-compose or
	// behind "podman compose".
	composeV2 composeKind = iota
	// podmanCompose is the Python podman-compose (also behind "podman compose").
	podmanCompose
)

// composeTool is the compose tool the driver found.
type composeTool struct {
	runner  Runner
	prefix  []string // "compose" for a CLI plugin
	name    string   // e.g. "docker compose", "podman-compose"
	kind    composeKind
	version string
}

var (
	semverRe        = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)[^\s,]*`)
	podmanComposeRe = regexp.MustCompile(`podman-compose version:?\s+(\S+)`)
)

// parseComposeVersion classifies the output of "<tool> version". ok is false
// for output it does not recognize and for Compose v1, which is end of life
// and names containers differently.
func parseComposeVersion(out string) (kind composeKind, version string, ok bool) {
	if m := podmanComposeRe.FindStringSubmatch(out); m != nil {
		return podmanCompose, m[1], true
	}
	m := semverRe.FindStringSubmatch(out)
	if m == nil {
		return 0, "", false
	}
	if major, _ := strconv.Atoi(m[1]); major < 2 {
		return 0, "", false
	}
	return composeV2, strings.TrimPrefix(m[0], "v"), true
}

// findCompose returns the first compose tool of flavor f that answers a
// version call, or nil. main runs the flavor's own CLI; lookup resolves a
// standalone binary. runtime.compose_path replaces the candidates.
func findCompose(f *Flavor, cfg config.Runtime, main Runner, lookup func(bin string) (Runner, bool)) *composeTool {
	candidates := f.compose
	if len(candidates) == 0 {
		return nil
	}
	if cfg.ComposePath != "" {
		candidates = []composeCandidate{{binary: cfg.ComposePath}}
	}
	timeout := probeTimeout
	if cfg.CommandTimeout > 0 {
		timeout = min(timeout, cfg.CommandTimeout)
	}
	for _, c := range candidates {
		t := &composeTool{runner: main, name: f.Binary + " compose"}
		if c.plugin {
			t.prefix = []string{"compose"}
		} else {
			r, ok := lookup(c.binary)
			if !ok {
				continue
			}
			t.runner, t.name = r, strings.TrimSuffix(filepath.Base(c.binary), ".exe")
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		stdout, stderr, code, err := t.runner.Run(ctx, append(slices.Clone(t.prefix), "version"))
		cancel()
		if err != nil || code != 0 {
			slog.Debug("compose tool not available", "tool", t.name, "exit", code, "error", err)
			continue
		}
		kind, version, ok := parseComposeVersion(string(normalize(stdout)) + "\n" + string(normalize(stderr)))
		if !ok {
			slog.Debug("compose tool not supported", "tool", t.name, "output", strings.TrimSpace(string(stdout)))
			continue
		}
		t.kind, t.version = kind, version
		slog.Debug("compose tool found", "tool", t.name, "version", version)
		return t
	}
	return nil
}

var _ runtime.Composer = (*Driver)(nil)

// composeArgs builds "[compose] [--ansi never] -p project [-f file] verb...".
func (d *Driver) composeArgs(t runtime.ComposeTarget, verb ...string) ([]string, error) {
	if d.compose == nil {
		return nil, fmt.Errorf("compose: %w", runtime.ErrUnsupported)
	}
	if err := runtime.ValidateProjectName(t.Project); err != nil {
		return nil, err
	}
	a := slices.Clone(d.compose.prefix)
	if d.compose.kind == composeV2 {
		a = append(a, "--ansi", "never")
	}
	a = append(a, "-p", t.Project)
	if t.File != "" {
		// Absolute, so it can never be taken for an option.
		if !filepath.IsAbs(t.File) {
			return nil, fmt.Errorf("%w: compose file %q must be an absolute path", runtime.ErrInvalid, t.File)
		}
		a = append(a, "-f", t.File)
	}
	return append(a, verb...), nil
}

func (d *Driver) ComposeValidate(ctx context.Context, t runtime.ComposeTarget) error {
	if t.File == "" {
		return fmt.Errorf("%w: no compose file", runtime.ErrInvalid)
	}
	verb := []string{"config", "--quiet"}
	if d.compose != nil && d.compose.kind == podmanCompose {
		verb = []string{"config"} // no --quiet; the output is discarded
	}
	args, err := d.composeArgs(t, verb...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, d.commandTimeout)
	defer cancel()
	stdout, stderr, code, err := d.compose.runner.Run(ctx, args)
	if err != nil {
		return fmt.Errorf("%s config: %w", d.compose.name, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s config: %w", d.compose.name, ctx.Err())
	}
	if code != 0 {
		ce := d.commandError("compose config", code, nil, stderr)
		if ce.Output == "" {
			ce = d.commandError("compose config", code, stdout, nil)
		}
		ce.Kind = runtime.ErrInvalid
		return ce
	}
	return nil
}

func (d *Driver) ComposeUp(ctx context.Context, t runtime.ComposeTarget, progress func(string)) error {
	if t.File == "" {
		return fmt.Errorf("%w: no compose file", runtime.ErrInvalid)
	}
	return d.composeStream(ctx, "compose up", t, progress, "up", "--detach", "--remove-orphans")
}

func (d *Driver) ComposeAction(ctx context.Context, t runtime.ComposeTarget, action string, progress func(string)) error {
	if !slices.Contains(runtime.ComposeActions, action) {
		return fmt.Errorf("%w: compose action %q", runtime.ErrInvalid, action)
	}
	return d.composeStream(ctx, "compose "+action, t, progress, action)
}

func (d *Driver) ComposeDown(ctx context.Context, t runtime.ComposeTarget, volumes bool, progress func(string)) error {
	verb := []string{"down"}
	if volumes {
		verb = append(verb, "--volumes")
	}
	return d.composeStream(ctx, "compose down", t, progress, verb...)
}

// composeStream runs a compose command and passes its output (compose
// reports progress on stderr) line by line to progress.
func (d *Driver) composeStream(ctx context.Context, op string, t runtime.ComposeTarget, progress func(string), verb ...string) error {
	args, err := d.composeArgs(t, verb...)
	if err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	ctx, cancel := context.WithTimeout(ctx, d.composeTimeout)
	defer cancel()
	lw := &lineWriter{onLine: progress}
	code, err := d.compose.runner.Pipe(ctx, args, lw)
	lw.flush()
	if err != nil {
		return fmt.Errorf("%s: %w", d.compose.name, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", d.compose.name, ctx.Err())
	}
	if code != 0 {
		return d.commandError(op, code, nil, []byte(strings.Join(lw.last, "\n")))
	}
	return nil
}

// composeErrorLines is how many trailing output lines a failed compose
// command reports.
const composeErrorLines = 8

// lineWriter splits written output into lines (on \n or \r) and keeps the
// last few for error messages.
type lineWriter struct {
	onLine func(string)
	buf    []byte
	last   []string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		w.line(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(w.buf)
		w.buf = nil
	}
}

func (w *lineWriter) line(b []byte) {
	l := strings.TrimSpace(string(b))
	if l == "" {
		return
	}
	w.onLine(l)
	w.last = append(w.last, l)
	if len(w.last) > composeErrorLines {
		w.last = w.last[1:]
	}
}
