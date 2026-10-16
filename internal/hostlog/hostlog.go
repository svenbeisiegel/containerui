// Package hostlog reads the host's system log: the systemd journal on Linux,
// the Event Log on Windows. Output is plain text, one entry per line, so it
// can be streamed and downloaded like container logs.
package hostlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Option is one choice of a selector, priority or time range.
type Option struct {
	Value string
	Label string
}

// Query selects log entries. All fields hold values from the source's
// option lists; Validate checks that before anything runs.
type Query struct {
	Selector string // unit (journal) or channel (Event Log); "" = all units
	Priority string // minimum severity; "" = all
	Since    string // time range; "" = everything
	Tail     int    // with Follow: earlier entries to start with
	Follow   bool   // keep reading new entries until ctx ends
}

// Source is a host's system log.
type Source interface {
	// Kind is "journal" or "eventlog".
	Kind() string
	// Label names the log for people: "Journal", "Event Log".
	Label() string
	// SelectorLabel names what Selectors offers: "Unit", "Channel".
	SelectorLabel() string
	// Available reports why the log cannot be read, or nil.
	Available() error
	// Selectors lists the units or channels that can be read.
	Selectors(ctx context.Context) ([]Option, error)
	// Priorities and Ranges list the allowed values; the first is the default.
	Priorities() []Option
	Ranges() []Option
	// Read writes the selected entries to w. With q.Follow it starts with
	// the last q.Tail entries and keeps writing new ones until ctx ends.
	Read(ctx context.Context, q Query, w io.Writer) error
}

// Exporter is a Source that can also export entries in the log's native
// file format (Event Log .evtx).
type Exporter interface {
	ExportExt() string
	Export(ctx context.Context, q Query, w io.Writer) error
}

// ErrInvalid marks a query with a value that is not offered.
var ErrInvalid = errors.New("invalid log query")

// Validate checks that q only holds offered values. Nothing from a request
// reaches a command line without passing this.
func Validate(ctx context.Context, src Source, q Query) error {
	if !hasValue(src.Priorities(), q.Priority) {
		return fmt.Errorf("%w: unknown priority %q", ErrInvalid, q.Priority)
	}
	if !hasValue(src.Ranges(), q.Since) {
		return fmt.Errorf("%w: unknown time range %q", ErrInvalid, q.Since)
	}
	sels, err := src.Selectors(ctx)
	if err != nil {
		return err
	}
	if !hasValue(sels, q.Selector) {
		return fmt.Errorf("%w: unknown %s %q", ErrInvalid, src.SelectorLabel(), q.Selector)
	}
	return nil
}

// Label returns the label of value in opts, or value itself.
func Label(opts []Option, value string) string {
	for _, o := range opts {
		if o.Value == value {
			return o.Label
		}
	}
	return value
}

func hasValue(opts []Option, v string) bool {
	return slices.ContainsFunc(opts, func(o Option) bool { return o.Value == v })
}
