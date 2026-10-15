package fake

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func (f *Runtime) CheckTerminal(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("terminal check %s", id); err != nil {
		return "", err
	}
	c, err := f.find(id)
	if err != nil {
		return "", err
	}
	if !c.State.IsRunning() {
		return "", &runtime.CommandError{Op: "terminal check", Output: "container " + c.Name + " is not running", Kind: runtime.ErrConflict}
	}
	if f.NoShell {
		return "", &runtime.CommandError{Op: "terminal check", Output: "No shell (bash or sh) found in the container.", Kind: runtime.ErrUnsupported}
	}
	return "/bin/sh", nil
}

func (f *Runtime) OpenTerminal(ctx context.Context, id string, opts runtime.TerminalOpts) (runtime.Terminal, error) {
	if err := runtime.ValidateShellPath(opts.Shell); err != nil {
		return nil, err
	}
	if _, err := f.CheckTerminal(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.Calls = append(f.Calls, fmt.Sprintf("terminal open %s %s %dx%d", id, opts.Shell, opts.Cols, opts.Rows))
	f.mu.Unlock()
	t := newShell(opts.Cols, opts.Rows)
	go t.print(t.prompt())
	return t, nil
}

// shell is a tiny line-editing pseudo shell for demos and tests. It knows
// echo, stty size, exit [code] and help.
type shell struct {
	out  *io.PipeReader
	outW *io.PipeWriter

	mu         sync.Mutex // serializes output and guards the fields below
	line       []rune
	cols, rows uint16
	exited     bool

	done      chan struct{}
	code      int
	closeOnce sync.Once
}

func newShell(cols, rows uint16) *shell {
	r, w := io.Pipe()
	return &shell{out: r, outW: w, cols: cols, rows: rows, done: make(chan struct{})}
}

func (s *shell) prompt() string { return "/ # " }

func (s *shell) print(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.outW, text)
}

func (s *shell) Read(b []byte) (int, error) { return s.out.Read(b) }

func (s *shell) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return 0, io.ErrClosedPipe
	}
	var echo strings.Builder
	for _, r := range string(b) {
		switch {
		case r == '\r' || r == '\n':
			echo.WriteString("\r\n")
			cmd := strings.TrimSpace(string(s.line))
			s.line = s.line[:0]
			out, exit, code := s.run(cmd)
			echo.WriteString(out)
			if exit {
				_, _ = io.WriteString(s.outW, echo.String())
				s.exit(code)
				return len(b), nil
			}
			echo.WriteString(s.prompt())
		case r == 0x7f || r == '\b':
			if len(s.line) > 0 {
				s.line = s.line[:len(s.line)-1]
				echo.WriteString("\b \b")
			}
		case r == 0x03: // Ctrl+C
			s.line = s.line[:0]
			echo.WriteString("^C\r\n" + s.prompt())
		case r >= ' ':
			s.line = append(s.line, r)
			echo.WriteRune(r)
		}
	}
	if _, err := io.WriteString(s.outW, echo.String()); err != nil {
		return 0, err
	}
	return len(b), nil
}

// run executes one command line; it is called with s.mu held.
func (s *shell) run(cmd string) (out string, exit bool, code int) {
	name, arg, _ := strings.Cut(cmd, " ")
	switch name {
	case "":
		return "", false, 0
	case "echo":
		return arg + "\r\n", false, 0
	case "stty":
		if arg == "size" {
			return fmt.Sprintf("%d %d\r\n", s.rows, s.cols), false, 0
		}
	case "exit":
		code, _ := strconv.Atoi(strings.TrimSpace(arg))
		return "", true, code
	case "help":
		return "fake shell: echo TEXT, stty size, exit [CODE]\r\n", false, 0
	}
	return "sh: " + name + ": not found\r\n", false, 0
}

// exit ends the shell; it is called with s.mu held.
func (s *shell) exit(code int) {
	s.exited, s.code = true, code
	_ = s.outW.Close()
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *shell) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cols, s.rows = cols, rows
	return nil
}

func (s *shell) Wait() (int, error) {
	<-s.done
	return s.code, nil
}

func (s *shell) Close() error {
	// Unblock a pending output write first so the lock can be taken.
	_ = s.outW.CloseWithError(io.ErrClosedPipe)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exited {
		s.exit(-1)
	}
	return nil
}
