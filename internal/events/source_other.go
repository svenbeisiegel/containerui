//go:build !linux

package events

// NewSource returns nil: error events come from the systemd journal, which
// only Linux has.
func NewSource() Source { return nil }
