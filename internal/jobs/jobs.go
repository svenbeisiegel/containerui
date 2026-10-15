// Package jobs runs long operations (image pulls) in the background so they
// outlive the HTTP request that started them, and records their progress.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

const (
	maxLines  = 200
	retention = time.Hour
)

// Job is a snapshot of a background operation.
type Job struct {
	ID       string
	Kind     string // e.g. "pull"
	Target   string // e.g. the image reference
	Owner    string // user who started it
	Status   Status
	Lines    []string
	Error    string
	Started  time.Time
	Finished time.Time

	seq uint64 // creation order; clock resolution can make Started equal
}

func (j Job) Done() bool { return j.Status != Running }

// Func is the work a job performs; it reports progress lines via progress.
type Func func(ctx context.Context, progress func(line string)) error

type Manager struct {
	ctx  context.Context
	mu   sync.Mutex
	jobs map[string]*Job
	seq  uint64
	wg   sync.WaitGroup
}

// NewManager creates a manager whose jobs are cancelled when ctx is done.
func NewManager(ctx context.Context) *Manager {
	return &Manager{ctx: ctx, jobs: map[string]*Job{}}
}

func (m *Manager) Start(kind, target, owner string, fn Func) Job {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	j := &Job{ID: hex.EncodeToString(b), Kind: kind, Target: target, Owner: owner, Status: Running, Started: time.Now()}

	m.mu.Lock()
	m.prune()
	m.seq++
	j.seq = m.seq
	m.jobs[j.ID] = j
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
		defer m.mu.Unlock()
		j.Finished = time.Now()
		if err != nil {
			j.Status, j.Error = Failed, err.Error()
		} else {
			j.Status = Succeeded
		}
	}()
	return snapshot
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

// List returns jobs of the given kind, newest first.
func (m *Manager) List(kind string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if j.Kind == kind {
			out = append(out, j.copy())
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].seq > out[b].seq })
	return out
}

// Wait blocks until all running jobs have returned.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) prune() {
	for id, j := range m.jobs {
		if j.Status != Running && time.Since(j.Finished) > retention {
			delete(m.jobs, id)
		}
	}
}

func (j *Job) copy() Job {
	c := *j
	c.Lines = append([]string(nil), j.Lines...)
	return c
}
