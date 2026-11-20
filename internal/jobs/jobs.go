// Package jobs runs long operations (image pulls) in the background so they
// outlive the HTTP request that started them, and records their progress.
// Short operations that finish within the request can be recorded as
// finished jobs, so the UI shows them in the same activity list.
//
// The manager keeps the newest MaxHistory jobs; with Persist, the finished
// ones are saved to a file and loaded again on the next start.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

const maxLines = 200

// MaxHistory is how many jobs a manager keeps. When there are more, the
// oldest finished jobs are dropped; running jobs are always kept.
const MaxHistory = 100

// Job is a snapshot of a background operation.
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`   // e.g. "pull"
	Target   string    `json:"target"` // e.g. the image reference
	Owner    string    `json:"owner"`  // user who started it
	Status   Status    `json:"status"`
	Lines    []string  `json:"lines,omitempty"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`

	seq uint64 // creation order; clock resolution can make Started equal
}

func (j Job) Done() bool { return j.Status != Running }

// Seq is the job's creation order within its manager: a later job has a
// higher Seq even when the clock gives both the same Started time.
func (j Job) Seq() uint64 { return j.seq }

// Func is the work a job performs; it reports progress lines via progress.
type Func func(ctx context.Context, progress func(line string)) error

type Manager struct {
	ctx  context.Context
	mu   sync.Mutex
	jobs map[string]*Job
	seq  uint64
	wg   sync.WaitGroup

	// Persistence (see Persist): path is the file, gen counts the changes
	// of finished jobs under mu, saved is the gen last written under saveMu.
	path   string
	onErr  func(error)
	gen    uint64
	saveMu sync.Mutex
	saved  uint64
}

// NewManager creates a manager whose jobs are cancelled when ctx is done.
func NewManager(ctx context.Context) *Manager {
	return &Manager{ctx: ctx, jobs: map[string]*Job{}}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) Start(kind, target, owner string, fn Func) Job {
	j := &Job{ID: newID(), Kind: kind, Target: target, Owner: owner, Status: Running, Started: time.Now()}

	m.mu.Lock()
	m.seq++
	j.seq = m.seq
	m.jobs[j.ID] = j
	m.prune()
	snapshot := j.copy()
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := fn(m.ctx, func(line string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			j.Lines = append(j.Lines, line)
			if len(j.Lines) > maxLines {
				j.Lines = j.Lines[len(j.Lines)-maxLines:]
			}
		})
		m.mu.Lock()
		j.Finished = time.Now()
		if err != nil {
			j.Status, j.Error = Failed, err.Error()
		} else {
			j.Status = Succeeded
		}
		m.prune()
		gen, done := m.changed()
		m.mu.Unlock()
		m.save(gen, done)
	}()
	return snapshot
}

// Record stores an operation that already finished: it failed with err, or
// succeeded if err is nil. lines are kept as its output.
func (m *Manager) Record(kind, target, owner string, lines []string, err error) Job {
	now := time.Now()
	j := &Job{ID: newID(), Kind: kind, Target: target, Owner: owner, Status: Succeeded, Started: now, Finished: now}
	if err != nil {
		j.Status, j.Error = Failed, err.Error()
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	j.Lines = append([]string(nil), lines...)

	m.mu.Lock()
	m.seq++
	j.seq = m.seq
	m.jobs[j.ID] = j
	m.prune()
	c := j.copy()
	gen, done := m.changed()
	m.mu.Unlock()
	m.save(gen, done)
	return c
}

func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.copy(), true
}

// List returns jobs of the given kinds, newest first.
func (m *Manager) List(kinds ...string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if slices.Contains(kinds, j.Kind) {
			out = append(out, j.copy())
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].seq > out[b].seq })
	return out
}

// Wait blocks until all running jobs have returned.
func (m *Manager) Wait() { m.wg.Wait() }

// prune drops the oldest finished jobs beyond MaxHistory; m.mu is held.
func (m *Manager) prune() {
	if len(m.jobs) <= MaxHistory {
		return
	}
	var finished []*Job
	for _, j := range m.jobs {
		if j.Done() {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].seq < finished[b].seq })
	for _, j := range finished {
		if len(m.jobs) <= MaxHistory {
			break
		}
		delete(m.jobs, j.ID)
	}
}

func (j *Job) copy() Job {
	c := *j
	c.Lines = append([]string(nil), j.Lines...)
	return c
}

// ---- persistence ----

const fileVersion = 1

type file struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"` // oldest first
}

// Persist loads the finished jobs saved in path, if it exists, and saves
// them there from now on, after each job that finishes or is recorded;
// onErr (may be nil) is called when saving fails. Call it before starting
// jobs. A file that cannot be read or parsed is an error; the manager then
// starts empty but still saves to path.
func (m *Manager) Persist(path string, onErr func(error)) error {
	m.mu.Lock()
	m.path, m.onErr = path, onErr
	m.mu.Unlock()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != fileVersion {
		return fmt.Errorf("%s: unknown version %d", path, f.Version)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range f.Jobs {
		j := f.Jobs[i]
		if j.ID == "" || !j.Done() {
			continue // only finished jobs are saved
		}
		if _, dup := m.jobs[j.ID]; dup {
			continue
		}
		m.seq++
		j.seq = m.seq
		m.jobs[j.ID] = &j
	}
	m.prune()
	return nil
}

// changed counts a change of the finished jobs; m.mu is held. It returns
// the change and the finished jobs to save, oldest first, or no jobs
// without a file.
func (m *Manager) changed() (uint64, []Job) {
	if m.path == "" {
		return 0, nil
	}
	m.gen++
	done := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.Done() {
			done = append(done, j.copy())
		}
	}
	sort.Slice(done, func(a, b int) bool { return done[a].seq < done[b].seq })
	return m.gen, done
}

// save writes the jobs of change gen unless a later change was written.
func (m *Manager) save(gen uint64, done []Job) {
	if gen == 0 {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if gen <= m.saved {
		return
	}
	m.saved = gen
	m.mu.Lock()
	path, onErr := m.path, m.onErr
	m.mu.Unlock()
	if err := writeFile(path, file{Version: fileVersion, Jobs: done}); err != nil && onErr != nil {
		onErr(err)
	}
}

// writeFile replaces path atomically: a reader sees the old or the new
// file. CreateTemp makes it readable by the owner only; it holds user
// names and command output.
func writeFile(path string, f file) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after a successful rename it is gone
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
