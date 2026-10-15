//go:build !linux && !windows

package auth

import (
	"errors"
	"log/slog"
)

// System verifies system accounts; only implemented on Linux and Windows.
type System struct{}

func NewSystem(service, socket string, log *slog.Logger) (*System, error) {
	return nil, errors.New("system accounts are only supported on Linux and Windows")
}

func (*System) Verify(string, string) bool { return false }
