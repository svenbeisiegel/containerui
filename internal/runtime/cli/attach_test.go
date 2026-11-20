package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestOpenAttachArgs(t *testing.T) {
	cases := []struct {
		flavor *Flavor
		opts   runtime.AttachOpts
		want   string
	}{
		{Docker, runtime.AttachOpts{Stdin: true, TTY: true}, "container attach --sig-proxy=false --detach-keys ctrl-p,ctrl-q abc"},
		{Docker, runtime.AttachOpts{}, "container attach --no-stdin --sig-proxy=false --detach-keys ctrl-p,ctrl-q abc"},
		{Podman, runtime.AttachOpts{Start: true, Stdin: true, TTY: true}, "container start --attach --interactive --sig-proxy=false --detach-keys ctrl-p,ctrl-q abc"},
		{Docker, runtime.AttachOpts{Start: true}, "container start --attach --detach-keys ctrl-p,ctrl-q abc"},
		{Docker, runtime.AttachOpts{Start: true, Stdin: true, TTY: true}, "container start --attach --interactive --detach-keys ctrl-p,ctrl-q abc"},
	}
	for _, c := range cases {
		r := &fakeRunner{}
		d := NewWithRunner(r, c.flavor, config.Runtime{})
		term, err := d.OpenAttach(context.Background(), "abc", runtime.AttachOpts{Start: c.opts.Start, Stdin: c.opts.Stdin, TTY: c.opts.TTY, Cols: 90, Rows: 20})
		if err != nil {
			t.Fatal(err)
		}
		if r.last() != c.want {
			t.Errorf("%+v: args = %q; want %q", c.opts, r.last(), c.want)
		}
		if r.ptySize != [2]uint16{90, 20} {
			t.Errorf("pty size = %v", r.ptySize)
		}
		_ = term.Close()
	}
}

// Close types the detach keys first when they can work, then hangs up.
func TestAttachCloseDetaches(t *testing.T) {
	r := &fakeRunner{}
	d := NewWithRunner(r, Docker, config.Runtime{})
	term, err := d.OpenAttach(context.Background(), "abc", runtime.AttachOpts{Stdin: true, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if string(r.pty.written) != runtime.DetachSequence {
		t.Errorf("written = %q; want the detach keys", r.pty.written)
	}
	if len(r.calls) != 1 {
		t.Errorf("calls = %v; detaching must not run anything in the container", r.calls)
	}
}

// Close kills a docker start --attach CLI instead of hanging it up, as it
// could not get --sig-proxy=false; everything else is hung up.
func TestAttachCloseKillsWithoutSigProxyFlag(t *testing.T) {
	cases := []struct {
		flavor *Flavor
		opts   runtime.AttachOpts
		kill   bool
	}{
		{Docker, runtime.AttachOpts{Start: true}, true},
		{Docker, runtime.AttachOpts{Start: true, Stdin: true}, true},
		{Docker, runtime.AttachOpts{}, false},
		{Podman, runtime.AttachOpts{Start: true}, false},
	}
	for _, c := range cases {
		r := &fakeRunner{}
		d := NewWithRunner(r, c.flavor, config.Runtime{})
		term, err := d.OpenAttach(context.Background(), "abc", c.opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := term.Close(); err != nil {
			t.Fatal(err)
		}
		if r.pty.killed != c.kill {
			t.Errorf("%s %+v: killed = %v; want %v", c.flavor.Name, c.opts, r.pty.killed, c.kill)
		}
	}
}

func TestAttachUnsupported(t *testing.T) {
	d := NewWithRunner(&fakeRunner{}, WSLC, config.Runtime{})
	if d.Capabilities().Attach {
		t.Error("wslc reports attach support")
	}
	if _, err := d.OpenAttach(context.Background(), "abc", runtime.AttachOpts{}); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
	if !NewWithRunner(&fakeRunner{}, Docker, config.Runtime{}).Capabilities().Attach {
		t.Error("docker lacks attach support")
	}
}
