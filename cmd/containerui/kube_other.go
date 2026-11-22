//go:build !linux

package main

import (
	"errors"
	"log/slog"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/kube"
)

func openKube(*config.Config, *slog.Logger) (*kube.Cluster, string, error) {
	return nil, "", errors.New("runtime.driver kubernetes is supported on Linux only")
}

func requireKubeconfig(*auth.System, string) {}
