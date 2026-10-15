package cli

import (
	"fmt"
	"sync"
	"time"

	"github.com/aymanbagabas/go-pty"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// closeGrace is how long Close waits for the CLI to exit on its own after its
// pseudo console was closed before the process is killed.
const closeGrace = 3 * time.Second

// StartPTY runs the CLI attached to a new pseudo terminal (ConPTY on Windows).
// wslc only allocates a TTY in the container when its own stdio is a
// console, so pipes are not enough here.
func (r execRunner) StartPTY(args []string, cols, rows uint16) (runtime.Terminal, error) {
	p, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("create pseudo terminal: %w", err)
	}
	if err := p.Resize(int(cols), int(rows)); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("resize pseudo terminal: %w", err)
	}
	cmd := p.Command(r.path, args...)
	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return nil, err
	}
	t := &ptyTerminal{pty: p, cmd: cmd, done: make(chan struct{})}
	go func() {
		t.code, t.err = exitStatus(cmd.Wait())
		close(t.done)
	}()
	return t, nil
}

type ptyTerminal struct {
	pty pty.Pty
	cmd *pty.Cmd

	done      chan struct{} // closed when the process has exited
	code      int
	err       error
	closeOnce sync.Once
}

func (t *ptyTerminal) Read(b []byte) (int, error)  { return t.pty.Read(b) }
func (t *ptyTerminal) Write(b []byte) (int, error) { return t.pty.Write(b) }

func (t *ptyTerminal) Resize(cols, rows uint16) error {
	return t.pty.Resize(int(cols), int(rows))
}

func (t *ptyTerminal) Wait() (int, error) {
	<-t.done
	return t.code, t.err
}

// Close closes the pseudo terminal, which hangs up the CLI like closing a
// console window does, and kills it if it does not exit in time. Pending
// Reads return an error once the terminal is closed.
func (t *ptyTerminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		err = t.pty.Close()
		select {
		case <-t.done:
		case <-time.After(closeGrace):
			_ = t.cmd.Process.Kill()
			<-t.done
		}
	})
	return err
}
