package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// detachGrace is how long Close waits for the CLI to exit after sending the
// detach keys, before it hangs up the pseudo terminal.
const detachGrace = time.Second

// killer is a Terminal whose process can be ended without a hangup.
type killer interface{ Kill() error }

// sigProxyOff reports whether attachArgs can pass --sig-proxy=false (docker
// start has no such flag).
func (d *Driver) sigProxyOff(opts runtime.AttachOpts) bool {
	return !opts.Start || d.flavor.startSigProxy
}

// attachArgs builds the CLI arguments for OpenAttach. --sig-proxy=false
// keeps the hangup of a closing terminal away from the container's process.
// Without it, Close kills the CLI instead of hanging it up.
func (d *Driver) attachArgs(id string, opts runtime.AttachOpts) []string {
	var a []string
	if opts.Start {
		a = []string{"container", "start", "--attach"}
		if opts.Stdin {
			a = append(a, "--interactive")
		}
	} else {
		a = []string{"container", "attach"}
		if !opts.Stdin {
			a = append(a, "--no-stdin")
		}
	}
	if d.sigProxyOff(opts) {
		a = append(a, "--sig-proxy=false")
	}
	a = append(a, "--detach-keys", runtime.DetachKeys, id)
	return d.args(a...)
}

func (d *Driver) OpenAttach(_ context.Context, id string, opts runtime.AttachOpts) (runtime.Terminal, error) {
	if !d.flavor.attach {
		return nil, fmt.Errorf("%w: %s cannot attach to containers", runtime.ErrUnsupported, d.flavor.Name)
	}
	if err := runtime.ValidateContainerID(id); err != nil {
		return nil, err
	}
	op := "container attach"
	if opts.Start {
		op = "container start --attach"
	}
	t, err := d.runner.StartPTY(d.attachArgs(id, opts), opts.Cols, opts.Rows)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", d.flavor.Name, op, err)
	}
	at := &attachTerminal{
		Terminal:   t,
		detachKeys: opts.Stdin && opts.TTY,
		kill:       !d.sigProxyOff(opts),
		done:       make(chan struct{}),
	}
	go func() {
		_, _ = t.Wait()
		close(at.done)
	}()
	return at, nil
}

// attachTerminal detaches cleanly on Close: it types the detach keys first
// (they only work with a TTY and input attached), and hangs up the pseudo
// terminal if the CLI is still there after detachGrace. With kill set, it
// kills the CLI before the hangup, because the CLI would pass the hangup on
// to the container (docker start --attach with a container without a TTY).
type attachTerminal struct {
	runtime.Terminal
	detachKeys bool
	kill       bool
	done       chan struct{} // closed when the CLI has exited

	closeOnce sync.Once
	closeErr  error
}

func (t *attachTerminal) Close() error {
	t.closeOnce.Do(func() {
		if t.detachKeys {
			select {
			case <-t.done:
			default:
				if _, err := t.Terminal.Write([]byte(runtime.DetachSequence)); err == nil {
					select {
					case <-t.done:
					case <-time.After(detachGrace):
					}
				}
			}
		}
		if k, ok := t.Terminal.(killer); ok && t.kill {
			select {
			case <-t.done:
			default:
				_ = k.Kill()
			}
		}
		t.closeErr = t.Terminal.Close()
	})
	return t.closeErr
}
