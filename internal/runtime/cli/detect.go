package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// probeTimeout bounds the version call that checks a candidate answers.
const probeTimeout = 15 * time.Second

// Service is set when ContainerUI runs as a Windows service. Detection then
// skips wslc, whose sessions belong to the user who starts them: the service
// account would see a session of its own, not the user's containers.
var Service bool

// Candidates are the CLIs Detect tries, in order of preference. wslc only
// exists on Windows.
func Candidates() []*Flavor {
	if goruntime.GOOS == "windows" {
		if Service {
			slog.Info("running as a Windows service: wslc is not detected, its sessions are per user")
			return []*Flavor{Docker, Podman}
		}
		return []*Flavor{WSLC, Docker, Podman}
	}
	return []*Flavor{Docker, Podman}
}

// Detect implements the "auto" driver: it returns a driver for the first
// installed CLI that answers a version call. If none answers, it uses the
// first installed one (the UI shows errors until it is reachable). A
// runtime.path in the config applies to every candidate.
func Detect(cfg config.Runtime) (runtime.Runtime, error) {
	return detect(cfg, Candidates(), func(f *Flavor) (*Driver, error) { return New(f, cfg) })
}

func detect(cfg config.Runtime, candidates []*Flavor, open func(*Flavor) (*Driver, error)) (runtime.Runtime, error) {
	var first *Driver
	var errs []error
	for _, f := range candidates {
		d, err := open(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		timeout := probeTimeout
		if cfg.CommandTimeout > 0 {
			timeout = min(timeout, cfg.CommandTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, err = d.Info(ctx)
		cancel()
		if err == nil {
			return d, nil
		}
		slog.Debug("container runtime installed but not answering", "driver", f.Name, "error", err)
		if first == nil {
			first = d
		}
	}
	if first != nil {
		return first, nil
	}
	return nil, fmt.Errorf("no container runtime found: %w", errors.Join(errs...))
}
