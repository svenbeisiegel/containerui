//go:build !linux && !windows

package hostlog

// New returns nil: there is no supported system log on this platform.
func New() Source { return nil }
