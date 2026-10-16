package hostlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// New returns the systemd journal.
func New() Source {
	path, err := exec.LookPath("journalctl")
	return &journal{path: path, lookErr: err}
}

// unitsTTL is how long the unit list is reused.
const unitsTTL = time.Minute

type journal struct {
	path    string
	lookErr error

	mu      sync.Mutex
	units   []Option
	unitsAt time.Time
}

func (j *journal) Kind() string          { return "journal" }
func (j *journal) Label() string         { return "Journal" }
func (j *journal) SelectorLabel() string { return "Unit" }
func (j *journal) Priorities() []Option  { return journalPriorities }
func (j *journal) Ranges() []Option      { return journalRanges }

func (j *journal) Available() error {
	if j.lookErr != nil {
		return errors.New("journalctl was not found; system logs need the systemd journal")
	}
	return nil
}

func (j *journal) Selectors(ctx context.Context) ([]Option, error) {
	if err := j.Available(); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.units != nil && time.Since(j.unitsAt) < unitsTTL {
		return j.units, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, j.path, "--field=_SYSTEMD_UNIT").Output()
	if err != nil {
		return nil, fmt.Errorf("list journal units: %w", err)
	}
	j.units, j.unitsAt = journalUnits(string(out)), time.Now()
	return j.units, nil
}

func (j *journal) Read(ctx context.Context, q Query, w io.Writer) error {
	if err := j.Available(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, j.path, journalArgs(q)...)
	// One writer for both keeps the order and a single writing goroutine.
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("journalctl exited with status %d", ee.ExitCode())
	}
	return err
}
