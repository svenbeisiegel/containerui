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

// Stater reads resource usage through the API and falls back to another
// Stater (the CLI driver) while the API cannot be used.
type Stater struct {
	api *Client
	cli runtime.Stater
	log *slog.Logger
	now func() time.Time

	mu        sync.Mutex
	downUntil time.Time // use the CLI until then
	source    string    // "api" or "cli" after the first call, for logging
}

var _ runtime.Stater = (*Stater)(nil)

// NewStater returns a Stater that asks the API on the Unix socket at path
// first and cli when that fails. Without a socket path, and for a Windows
// named pipe, it returns cli itself.
func NewStater(path string, cli runtime.Stater, log *slog.Logger) runtime.Stater {
	if path == "" || strings.HasPrefix(path, `\\`) {
		return cli
	}
	if log == nil {
		log = slog.Default()
	}
	return &Stater{api: New(path), cli: cli, log: log, now: time.Now}
}

func (s *Stater) Stats(ctx context.Context) ([]runtime.ContainerStats, error) {
	return try(s, ctx, s.api.Stats, s.cli.Stats)
}

func (s *Stater) DiskUsage(ctx context.Context) (runtime.DiskUsage, error) {
	return try(s, ctx, s.api.DiskUsage, s.cli.DiskUsage)
}

// try calls api unless it failed within retryAPI, and cli when api is not
// used or fails.
func try[T any](s *Stater, ctx context.Context, api, cli func(context.Context) (T, error)) (T, error) {
	s.mu.Lock()
	useAPI := !s.now().Before(s.downUntil)
	s.mu.Unlock()
	if useAPI {
		v, err := api(ctx)
		if err == nil {
			s.switchTo("api", nil)
			return v, nil
		}
		if ctx.Err() != nil {
			return v, err
		}
		s.mu.Lock()
		s.downUntil = s.now().Add(retryAPI)
		s.mu.Unlock()
		s.switchTo("cli", err)
	}
	return cli(ctx)
}

// switchTo logs when the source of the resource usage changes.
func (s *Stater) switchTo(source string, err error) {
	s.mu.Lock()
	changed := s.source != source
	s.source = source
	s.mu.Unlock()
	if !changed {
		return
	}
	if source == "api" {
		s.log.Info("resource usage via the runtime API", "socket", s.api.Socket())
	} else {
		s.log.Info("resource usage via the CLI: the runtime API cannot be used", "socket", s.api.Socket(), "error", err, "retry_in", retryAPI)
	}
}

// Source reports where the last resource usage came from: "api", "cli", or
// "" before the first call.
func (s *Stater) Source() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source
}
