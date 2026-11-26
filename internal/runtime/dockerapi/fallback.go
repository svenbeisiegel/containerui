package dockerapi

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// retryAPI is how long the CLI is used after the API failed before the API
// is tried again.
const retryAPI = 5 * time.Minute

// fallback remembers whether the API can be used and logs when that
// changes. After the API failed, the CLI is used for retryAPI.
type fallback struct {
	socket string
	what   string // what is read through the API, for logging
	log    *slog.Logger
	now    func() time.Time

	mu        sync.Mutex
	downUntil time.Time // use the CLI until then
	source    string    // "api" or "cli" after the first call
}

func newFallback(socket, what string, log *slog.Logger) *fallback {
	if log == nil {
		log = slog.Default()
	}
	return &fallback{socket: socket, what: what, log: log, now: time.Now}
}

// useAPI reports whether the API is to be tried.
func (f *fallback) useAPI() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.now().Before(f.downUntil)
}

// up records that the daemon answered on the socket.
func (f *fallback) up() { f.switchTo("api", nil) }

// down records that the API cannot be used: the CLI is used for retryAPI.
func (f *fallback) down(err error) {
	f.mu.Lock()
	f.downUntil = f.now().Add(retryAPI)
	f.mu.Unlock()
	f.switchTo("cli", err)
}

func (f *fallback) switchTo(source string, err error) {
	f.mu.Lock()
	changed := f.source != source
	f.source = source
	f.mu.Unlock()
	if !changed {
		return
	}
	if source == "api" {
		f.log.Info(f.what+" via the runtime API", "socket", f.socket)
	} else {
		f.log.Info(f.what+" via the CLI: the runtime API cannot be used", "socket", f.socket, "error", err, "retry_in", retryAPI)
	}
}

// Source reports where the last answer came from: "api", "cli", or ""
// before the first call.
func (f *fallback) Source() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.source
}

// When the CLI takes over after the API failed: a read can be repeated
// whenever the daemon did not answer; a change only when the request never
// left (it may have been applied otherwise). anyError is the resource
// usage's rule: the CLI computes it differently, so any failure counts.
var (
	onTransport = isTransport
	onDial      = isDial
	anyError    = func(error) bool { return true }
)

// try calls api unless the API is down, and cli when it is down or the
// API fails in a way retry accepts. Errors the daemon answered with are
// returned as they are.
func try[T any](f *fallback, ctx context.Context, retry func(error) bool, api, cli func(context.Context) (T, error)) (T, error) {
	if f.useAPI() {
		v, err := api(ctx)
		if err == nil || ctx.Err() != nil || !retry(err) {
			if err == nil || !isTransport(err) {
				f.up()
			}
			return v, err
		}
		f.down(err)
	}
	return cli(ctx)
}

// Stater reads resource usage through the API and falls back to another
// Stater (the CLI driver) while the API cannot be used.
type Stater struct {
	api *Client
	cli runtime.Stater
	*fallback
}

var _ runtime.Stater = (*Stater)(nil)

// NewStater returns a Stater that asks the API on the Unix socket at path
// first and cli when that fails. Without a socket path, and for a Windows
// named pipe, it returns cli itself.
func NewStater(path string, cli runtime.Stater, log *slog.Logger) runtime.Stater {
	if path == "" || strings.HasPrefix(path, `\\`) {
		return cli
	}
	return &Stater{api: New(path), cli: cli, fallback: newFallback(path, "resource usage", log)}
}

func (s *Stater) Stats(ctx context.Context) ([]runtime.ContainerStats, error) {
	return try(s.fallback, ctx, anyError, s.api.Stats, s.cli.Stats)
}

func (s *Stater) DiskUsage(ctx context.Context) (runtime.DiskUsage, error) {
	return try(s.fallback, ctx, anyError, s.api.DiskUsage, s.cli.DiskUsage)
}
