package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Runner executes the container CLI. It is an interface so tests can substitute
// canned output for the real CLI.
type Runner interface {
	// Run executes the CLI with args and returns its captured output. exitCode
	// is -1 if the process could not be started (err is then non-nil).
	Run(ctx context.Context, args []string) (stdout, stderr []byte, exitCode int, err error)
	// Stream is like Run but passes each stdout line to onLine as it arrives.
	Stream(ctx context.Context, args []string, onLine func(string)) (stderr []byte, exitCode int, err error)
	// Pipe is like Run but copies stdout and stderr, merged in arrival
	// order, to w unchanged as they arrive.
	Pipe(ctx context.Context, args []string, w io.Writer) (exitCode int, err error)
	// StartPTY starts the CLI on a pseudo terminal of the given size and
	// returns without waiting for it to exit.
	StartPTY(args []string, cols, rows uint16) (runtime.Terminal, error)
}

type execRunner struct {
	path string
}

func (r execRunner) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.path, args...)
	hideWindow(cmd)
	return cmd
}

func (r execRunner) Run(ctx context.Context, args []string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := r.command(ctx, args)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code, err := exitStatus(err)
	return stdout.Bytes(), stderr.Bytes(), code, err
}

func (r execRunner) Stream(ctx context.Context, args []string, onLine func(string)) ([]byte, int, error) {
	var stderr bytes.Buffer
	cmd := r.command(ctx, args)
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, -1, err
	}
	if err := cmd.Start(); err != nil {
		return nil, -1, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(scanLinesOrCR)
	for sc.Scan() {
		if line := string(bytes.TrimSpace(sc.Bytes())); line != "" {
			onLine(line)
		}
	}
	_, _ = io.Copy(io.Discard, out)
	code, err := exitStatus(cmd.Wait())
	return stderr.Bytes(), code, err
}

func (r execRunner) Pipe(ctx context.Context, args []string, w io.Writer) (int, error) {
	cmd := r.command(ctx, args)
	// The same writer for both makes exec use a single pipe, which keeps the
	// order and calls w from one goroutine only.
	cmd.Stdout, cmd.Stderr = w, w
	// Do not wait forever for the output pipe after the context ended.
	cmd.WaitDelay = 2 * time.Second
	return exitStatus(cmd.Run())
}

// exitStatus turns a cmd error into (exitCode, err). A non-zero exit is not
// an error at this level: err is only set if the process failed to run.
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// scanLinesOrCR splits on \n or \r so progress output that redraws a line
// with carriage returns still produces separate updates.
func scanLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
