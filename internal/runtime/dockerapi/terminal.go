package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
)

// detachGrace is how long Close waits for the daemon to end an attach
// after typing the detach keys.
const detachGrace = time.Second

// cleanupTimeout bounds the exec that ends a closed terminal's processes.
const cleanupTimeout = 10 * time.Second

// execConfig is the body of POST /containers/{id}/exec.
type execConfig struct {
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
	Env          []string `json:",omitempty"`
	Cmd          []string
	ConsoleSize  *[2]uint16 `json:",omitempty"` // height, width
}

// consoleSize is the [height, width] of a new TTY, nil if unknown.
func consoleSize(cols, rows uint16) *[2]uint16 {
	if cols == 0 || rows == 0 {
		return nil
	}
	return &[2]uint16{rows, cols}
}

func (d *Driver) execCreate(ctx context.Context, op, id string, cfg execConfig) (string, error) {
	var r struct {
		ID string `json:"Id"`
	}
	if err := d.api.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/exec", cfg, &r); err != nil {
		return "", opError(op, err)
	}
	return r.ID, nil
}

// execStart starts an exec and returns its hijacked stream.
func (d *Driver) execStart(ctx context.Context, op, execID string, tty bool, size *[2]uint16) (net.Conn, *bufio.Reader, error) {
	body := map[string]any{"Detach": false, "Tty": tty}
	if size != nil {
		body["ConsoleSize"] = size
	}
	conn, br, err := d.api.hijack(ctx, http.MethodPost, "/exec/"+esc(execID)+"/start", body)
	return conn, br, opError(op, err)
}

// execExitCode waits briefly for the exec to be reported as finished and
// returns its exit code (-1 if it does not finish in time).
func (d *Driver) execExitCode(ctx context.Context, execID string) (int, error) {
	for i := 0; ; i++ {
		var v struct {
			Running  bool
			ExitCode *int
		}
		if err := d.api.get(ctx, "/exec/"+esc(execID)+"/json", &v); err != nil {
			return -1, err
		}
		if !v.Running && v.ExitCode != nil {
			return *v.ExitCode, nil
		}
		if i == 20 {
			return -1, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// execRun runs cmd in the container without a TTY and returns its output
// and exit code, like "container exec" without -it.
func (d *Driver) execRun(ctx context.Context, op, id string, cmd []string) (stdout, stderr []byte, code int, err error) {
	execID, err := d.execCreate(ctx, op, id, execConfig{AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return nil, nil, -1, err
	}
	conn, br, err := d.execStart(ctx, op, execID, false, nil)
	if err != nil {
		return nil, nil, -1, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	var out, errOut bytes.Buffer
	err = demux(br, &out, &errOut)
	stop()
	conn.Close()
	if ctx.Err() != nil {
		return out.Bytes(), errOut.Bytes(), -1, ctx.Err()
	}
	if err != nil {
		return out.Bytes(), errOut.Bytes(), -1, &runtime.CommandError{Op: op, Output: err.Error()}
	}
	code, err = d.execExitCode(ctx, execID)
	if err == nil && code != 0 {
		text := strings.TrimSpace(errOut.String())
		if text == "" {
			text = strings.TrimSpace(out.String())
		}
		err = &runtime.CommandError{Op: op, ExitCode: code, Output: text}
	}
	return out.Bytes(), errOut.Bytes(), code, err
}

// lastLine returns the last non-empty line of b, trimmed.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (d *Driver) CheckTerminal(ctx context.Context, id string) (string, error) {
	if err := runtime.ValidateContainerID(id); err != nil {
		return "", err
	}
	return read(d, ctx, func(ctx context.Context) (string, error) {
		const op = "terminal check"
		out, _, _, err := d.execRun(ctx, op, id, []string{"/bin/sh", "-c", cli.ShellProbe})
		if isTransport(err) || ctx.Err() != nil {
			return "", err
		}
		var ce *runtime.CommandError
		if errors.As(err, &ce) && (errors.Is(err, runtime.ErrNotFound) || errors.Is(err, runtime.ErrConflict)) {
			// The container itself is the problem (not found, not running).
			return "", err
		}
		shell := ""
		if err == nil {
			shell = lastLine(out)
		}
		if runtime.ValidateShellPath(shell) != nil {
			noShell := &runtime.CommandError{Op: op, Output: "No shell (bash or sh) found in the container.", Kind: runtime.ErrUnsupported}
			if ce != nil {
				noShell.ExitCode = ce.ExitCode
			}
			return "", noShell
		}
		return shell, nil
	}, func(ctx context.Context) (string, error) { return d.Driver.CheckTerminal(ctx, id) })
}

func (d *Driver) OpenTerminal(ctx context.Context, id string, opts runtime.TerminalOpts) (runtime.Terminal, error) {
	if err := errors.Join(runtime.ValidateContainerID(id), runtime.ValidateShellPath(opts.Shell)); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.commandTimeout)
	defer cancel()
	return try(d.fb, ctx, onDial, func(ctx context.Context) (runtime.Terminal, error) {
		return d.apiTerminal(ctx, id, opts)
	}, func(ctx context.Context) (runtime.Terminal, error) { return d.Driver.OpenTerminal(ctx, id, opts) })
}

func (d *Driver) apiTerminal(ctx context.Context, id string, opts runtime.TerminalOpts) (runtime.Terminal, error) {
	const op = "container exec"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tag := cli.TerminalTagVar + "=" + hex.EncodeToString(b)
	size := consoleSize(opts.Cols, opts.Rows)
	execID, err := d.execCreate(ctx, op, id, execConfig{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true,
		Env: []string{"TERM=xterm-256color", tag}, Cmd: []string{opts.Shell}, ConsoleSize: size,
	})
	if err != nil {
		return nil, err
	}
	conn, br, err := d.execStart(ctx, op, execID, true, size)
	if err != nil {
		return nil, err
	}
	t := newStreamTerminal(conn, br)
	t.resize = func(cols, rows uint16) error {
		return d.resize(op, "/exec/"+esc(execID)+"/resize", cols, rows)
	}
	t.exitCode = func() (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
		defer cancel()
		return d.execExitCode(ctx, execID)
	}
	t.afterClose = func(ended bool) error {
		if ended {
			return nil
		}
		// The daemon leaves the shell running when the stream closes: end
		// it and what it started, like the CLI driver does.
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _, _, err := d.execRun(ctx, "terminal cleanup", id, []string{"/bin/sh", "-c", cli.TerminalKill, "sh", tag})
		return err
	}
	go t.initSize(opts.Cols, opts.Rows)
	return t, nil
}

// OpenAttach attaches to a container with a TTY through the API. Without
// a TTY the CLI driver attaches: its pseudo terminal supplies the line
// editing, echo and line endings such a container does not.
func (d *Driver) OpenAttach(ctx context.Context, id string, opts runtime.AttachOpts) (runtime.Terminal, error) {
	if !opts.TTY {
		return d.Driver.OpenAttach(ctx, id, opts)
	}
	if err := runtime.ValidateContainerID(id); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.commandTimeout)
	defer cancel()
	return try(d.fb, ctx, onDial, func(ctx context.Context) (runtime.Terminal, error) {
		return d.apiAttach(ctx, id, opts)
	}, func(ctx context.Context) (runtime.Terminal, error) { return d.Driver.OpenAttach(ctx, id, opts) })
}

func (d *Driver) apiAttach(ctx context.Context, id string, opts runtime.AttachOpts) (runtime.Terminal, error) {
	op := "container attach"
	if opts.Start {
		op = "container start --attach"
	}
	tty, err := d.containerTTY(ctx, id)
	if err != nil {
		return nil, opError(op, err)
	}
	if !tty {
		return d.Driver.OpenAttach(ctx, id, opts)
	}
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "detachKeys": {runtime.DetachKeys}}
	if opts.Stdin {
		q.Set("stdin", "1")
	}
	conn, br, err := d.api.hijack(ctx, http.MethodPost, "/containers/"+esc(id)+"/attach?"+q.Encode(), nil)
	if err != nil {
		return nil, opError(op, err)
	}
	if opts.Start {
		if err := d.api.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/start", nil, nil); err != nil && !notModified(err) {
			conn.Close()
			return nil, opError(op, err)
		}
	}
	t := newStreamTerminal(conn, br)
	t.resize = func(cols, rows uint16) error {
		return d.resize(op, "/containers/"+esc(id)+"/resize", cols, rows)
	}
	t.exitCode = func() (int, error) {
		// The stream ends when the container stops or on the detach keys.
		ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
		defer cancel()
		var c struct {
			State struct {
				Running  bool
				ExitCode int
			}
		}
		if err := d.api.get(ctx, "/containers/"+esc(id)+"/json", &c); err != nil {
			return -1, opError(op, err)
		}
		if c.State.Running {
			return 0, nil
		}
		return c.State.ExitCode, nil
	}
	if opts.Stdin {
		// Detach with the keys, so the daemon does not take the closed
		// connection for the end of the container's input.
		t.beforeClose = func() {
			if _, err := conn.Write([]byte(runtime.DetachSequence)); err == nil {
				select {
				case <-t.done:
				case <-time.After(detachGrace):
				}
			}
		}
	}
	go t.initSize(opts.Cols, opts.Rows)
	return t, nil
}

func (d *Driver) resize(op, path string, cols, rows uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
	defer cancel()
	q := url.Values{"h": {strconv.Itoa(int(rows))}, "w": {strconv.Itoa(int(cols))}}
	return opError(op+" resize", d.api.do(ctx, http.MethodPost, path+"?"+q.Encode(), nil, nil))
}

// streamTerminal is a runtime.Terminal on a hijacked API connection with a
// TTY: reads and writes carry raw terminal bytes.
type streamTerminal struct {
	conn net.Conn
	r    io.Reader

	resize      func(cols, rows uint16) error
	exitCode    func() (int, error)
	beforeClose func()                 // before the connection closes, unless the stream ended
	afterClose  func(ended bool) error // after it closed

	done      chan struct{} // closed when the stream ended
	doneOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

func newStreamTerminal(conn net.Conn, r io.Reader) *streamTerminal {
	return &streamTerminal{conn: conn, r: r, done: make(chan struct{})}
}

func (t *streamTerminal) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if err != nil {
		t.end()
	}
	return n, err
}

func (t *streamTerminal) Write(b []byte) (int, error) { return t.conn.Write(b) }

func (t *streamTerminal) Resize(cols, rows uint16) error { return t.resize(cols, rows) }

// Wait blocks until the stream ended (the process exited, or the terminal
// was closed) and returns the exit code.
func (t *streamTerminal) Wait() (int, error) {
	<-t.done
	return t.exitCode()
}

func (t *streamTerminal) end() { t.doneOnce.Do(func() { close(t.done) }) }

func (t *streamTerminal) ended() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

func (t *streamTerminal) Close() error {
	t.closeOnce.Do(func() {
		if t.beforeClose != nil && !t.ended() {
			t.beforeClose()
		}
		ended := t.ended()
		t.closeErr = t.conn.Close()
		t.end()
		if t.afterClose != nil {
			t.closeErr = errors.Join(t.closeErr, t.afterClose(ended))
		}
	})
	return t.closeErr
}

// initSize sets the terminal's size once the process runs; right after the
// start the daemon may not accept it yet.
func (t *streamTerminal) initSize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	for i := 1; i <= 10; i++ {
		if t.resize(cols, rows) == nil || t.ended() {
			return
		}
		time.Sleep(time.Duration(i) * 20 * time.Millisecond)
	}
}
