package events

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// NewSource returns the systemd journal, or nil if journalctl is missing.
func NewSource() Source {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil
	}
	return &journal{path: path}
}

type journal struct{ path string }

func (j *journal) Follow(ctx context.Context, since time.Time, line func([]byte)) error {
	cmd := exec.CommandContext(ctx, j.path, journalFollowArgs(since)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(out, 64<<10)
	for {
		b, err := r.ReadBytes('\n')
		if len(b) > 0 {
			line(b)
		}
		if err != nil {
			break
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("journalctl exited with status %d: %s", ee.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	if err == nil {
		err = errors.New("journalctl ended")
	}
	return err
}
