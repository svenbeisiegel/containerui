package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// terminalTagVar marks every process of one terminal (the shell and its
// children inherit it), so they can be found again when the terminal closes.
const terminalTagVar = "CONTAINERUI_TERMINAL"

// terminalKill ends all processes whose environment contains "$1" (the tag
// assignment): SIGHUP first like a closed terminal, SIGKILL a second later.
// The script's own process does not carry the tag.
const terminalKill = `found=
for p in /proc/[0-9]*; do
  if tr '\0' '\n' < "$p/environ" 2>/dev/null | grep -qxF "$1"; then found="$found ${p#/proc/}"; fi
done
[ -z "$found" ] && exit 0
kill -HUP $found 2>/dev/null
sleep 1
kill -KILL $found 2>/dev/null
exit 0`

func (d *Driver) OpenTerminal(_ context.Context, id string, opts runtime.TerminalOpts) (runtime.Terminal, error) {
	if err := errors.Join(runtime.ValidateContainerID(id), runtime.ValidateShellPath(opts.Shell)); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tag := terminalTagVar + "=" + hex.EncodeToString(b)
	args := d.args("container", "exec", "--interactive", "--tty",
		"--env", "TERM=xterm-256color", "--env", tag, id, opts.Shell)
	t, err := d.runner.StartPTY(args, opts.Cols, opts.Rows)
	if err != nil {
		return nil, fmt.Errorf("%s container exec: %w", d.flavor.Name, err)
	}
	et := &execTerminal{Terminal: t, d: d, id: id, tag: tag}
	go func() {
		_, _ = t.Wait()
		et.exited.Store(true)
	}()
	return et, nil
}

// execTerminal makes Close also end the shell inside the container: the CLI
// exits when its console closes, but does not stop the process it started.
type execTerminal struct {
	runtime.Terminal
	d       *Driver
	id, tag string

	exited    atomic.Bool // the shell exited on its own
	closeOnce sync.Once
	closeErr  error
}

func (t *execTerminal) Close() error {
	t.closeOnce.Do(func() {
		shellGone := t.exited.Load()
		t.closeErr = t.Terminal.Close()
		if shellGone {
			return
		}
		_, err := t.d.run(context.Background(), "terminal cleanup", "container", "exec", t.id, "/bin/sh", "-c", terminalKill, "sh", t.tag)
		if err != nil {
			t.closeErr = errors.Join(t.closeErr, err)
		}
	})
	return t.closeErr
}
