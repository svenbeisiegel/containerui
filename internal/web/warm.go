package web

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	defaultWarmInterval = 10 * time.Second
	warmTimeout         = 2 * time.Minute
)

// warmer starts a background runtime warm-up when someone is about to log in,
// so a backend that went cold while idle is ready when the dashboard loads.
// wslc ends its session after roughly 25-30s without commands, so the login
// page also pings /login/warm while it is open (see app.js).
// It runs at most once per interval and never twice concurrently.
type warmer struct {
	rt       runtime.Runtime
	log      *slog.Logger
	interval time.Duration // <0 disables

	mu      sync.Mutex
	last    time.Time
	running bool
	wg      sync.WaitGroup
}

// kick starts a warm-up unless one is running or ran recently. It reports
// whether a warm-up was started.
func (w *warmer) kick() bool {
	if w.interval < 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running || (!w.last.IsZero() && time.Since(w.last) < w.interval) {
		return false
	}
	w.running, w.last = true, time.Now()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()
		start := time.Now()
		err := runtime.WarmUp(ctx, w.rt)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		if err != nil {
			w.log.Debug("runtime warm-up failed", "error", err)
			return
		}
		w.log.Debug("runtime warmed up", "took", time.Since(start).Round(time.Millisecond))
	}()
	return true
}

// wait blocks until a running warm-up finished (for tests).
func (w *warmer) wait() { w.wg.Wait() }
