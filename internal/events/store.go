package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// MaxInMemory bounds the events held for the UI on a very noisy host; the
// oldest are dropped first. The day files on disk keep all of them.
const MaxInMemory = 100_000

const (
	eventsPrefix = "events-"
	readsPrefix  = "reads-"
	fileExt      = ".jsonl"
	stateFile    = "state.json"
)

// Store holds the events of the last Retention in memory, oldest first, and
// in day files on disk. It is safe for concurrent use.
type Store struct {
	dir string
	now func() time.Time

	mu     sync.RWMutex
	events []*Event // sorted by time, oldest first
	byID   map[string]*Event
	cursor string
}

// state is what state.json holds.
type state struct {
	Cursor string `json:"cursor"`
}

// readMark is one line of a reads file.
type readMark struct {
	ID   string    `json:"id"`
	User string    `json:"user"`
	At   time.Time `json:"at"`
}

// Open opens the store in dir, creating the folder if needed, and loads
// the events of the last Retention. Older day files are deleted.
func Open(dir string) (*Store, error) {
	return open(dir, time.Now)
}

func open(dir string, now func() time.Time) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("events folder: %w", err)
	}
	s := &Store{dir: dir, now: now, byID: map[string]*Event{}}
	if data, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		var st state
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("%s: %w", stateFile, err)
		}
		s.cursor = st.Cursor
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := s.pruneFiles(); err != nil {
		return nil, err
	}
	cutoff := s.cutoff()
	days, err := s.days(eventsPrefix)
	if err != nil {
		return nil, err
	}
	for _, day := range days {
		err := readLines(s.path(eventsPrefix, day), func(line []byte) {
			var e Event
			if json.Unmarshal(line, &e) != nil || e.ID == "" || e.Time.Before(cutoff) || s.byID[e.ID] != nil {
				return
			}
			s.byID[e.ID] = &e
			s.events = append(s.events, &e)
		})
		if err != nil {
			return nil, err
		}
	}
	s.sort()
	s.trim()
	days, err = s.days(readsPrefix)
	if err != nil {
		return nil, err
	}
	for _, day := range days {
		err := readLines(s.path(readsPrefix, day), func(line []byte) {
			var m readMark
			if json.Unmarshal(line, &m) != nil {
				return
			}
			if e := s.byID[m.ID]; e != nil && !e.Read() {
				e.ReadBy, e.ReadAt = m.User, m.At
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Dir is the folder of the store.
func (s *Store) Dir() string { return s.dir }

// Cursor is the journal position after the last event added, "" before
// the first.
func (s *Store) Cursor() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursor
}

// Add stores new events and the journal cursor after them. Events already
// stored or older than Retention are skipped. When writing fails the
// events are still kept in memory, but the cursor is not saved, so a
// restart reads them from the journal again.
func (s *Store) Add(evs []Event, cursor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.cutoff()
	byDay := map[string][]*Event{}
	var days []string
	unsorted := false
	for i := range evs {
		e := evs[i]
		if e.ID == "" || e.Time.Before(cutoff) || s.byID[e.ID] != nil {
			continue
		}
		e.Time, e.ReadBy, e.ReadAt = e.Time.UTC(), "", time.Time{}
		p := &e
		s.byID[e.ID] = p
		if n := len(s.events); n > 0 && e.Time.Before(s.events[n-1].Time) {
			unsorted = true
		}
		s.events = append(s.events, p)
		if byDay[e.day()] == nil {
			days = append(days, e.day())
		}
		byDay[e.day()] = append(byDay[e.day()], p)
	}
	if unsorted {
		s.sort()
	}
	s.trim()
	var errs []error
	for _, day := range days {
		var lines [][]byte
		for _, e := range byDay[day] {
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			lines = append(lines, b)
		}
		errs = append(errs, appendLines(s.path(eventsPrefix, day), lines))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if cursor == "" || cursor == s.cursor {
		return nil
	}
	if err := writeAtomic(filepath.Join(s.dir, stateFile), state{Cursor: cursor}); err != nil {
		return err
	}
	s.cursor = cursor
	return nil
}

// Filter selects events. The zero Filter selects all.
type Filter struct {
	Component string // exact component, "" = all
	Text      string // case-insensitive substring of message, component or identifier
	Unread    bool   // only events nobody has read
}

func (f Filter) match(e *Event) bool {
	if f.Component != "" && e.Component != f.Component {
		return false
	}
	if f.Unread && e.Read() {
		return false
	}
	if f.Text != "" {
		t := strings.ToLower(f.Text)
		if !strings.Contains(strings.ToLower(e.Message), t) &&
			!strings.Contains(strings.ToLower(e.Component), t) &&
			!strings.Contains(strings.ToLower(e.Identifier), t) {
			return false
		}
	}
	return true
}

// List returns up to limit events matching f, newest first, and how many
// match in all. limit <= 0 means no limit.
func (s *Store) List(f Filter, limit int) ([]Event, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	total := 0
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		if !f.match(e) {
			continue
		}
		total++
		if limit <= 0 || len(out) < limit {
			out = append(out, *e)
		}
	}
	return out, total
}

// Get returns the event with the id.
func (s *Store) Get(id string) (Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byID[id]
	if e == nil {
		return Event{}, false
	}
	return *e, true
}

// Component is a component and how many of its events there are.
type Component struct {
	Name   string
	Total  int
	Unread int
}

// Components lists the components of the stored events by name.
func (s *Store) Components() []Component {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := map[string]int{}
	var out []Component
	for _, e := range s.events {
		i, ok := idx[e.Component]
		if !ok {
			i = len(out)
			idx[e.Component] = i
			out = append(out, Component{Name: e.Component})
		}
		out[i].Total++
		if !e.Read() {
			out[i].Unread++
		}
	}
	slices.SortFunc(out, func(a, b Component) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// HasComponent reports whether any stored event has the component.
func (s *Store) HasComponent(name string) bool {
	return slices.ContainsFunc(s.Components(), func(c Component) bool { return c.Name == name })
}

// Unread counts the events nobody has read.
func (s *Store) Unread() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.events {
		if !e.Read() {
			n++
		}
	}
	return n
}

// MarkRead records that user read the events with the ids. Unknown and
// already read events are skipped; it returns how many were marked.
func (s *Store) MarkRead(user string, ids ...string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []*Event
	for _, id := range ids {
		if e := s.byID[id]; e != nil && !e.Read() && !slices.Contains(evs, e) {
			evs = append(evs, e)
		}
	}
	return len(evs), s.markRead(user, evs)
}

// MarkAllRead records that user read every unread event matching f.
func (s *Store) MarkAllRead(user string, f Filter) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []*Event
	for _, e := range s.events {
		if !e.Read() && f.match(e) {
			evs = append(evs, e)
		}
	}
	return len(evs), s.markRead(user, evs)
}

// markRead marks evs read in memory and appends them to the reads file of
// their day. The caller holds the lock.
func (s *Store) markRead(user string, evs []*Event) error {
	at := s.now().UTC()
	byDay := map[string][][]byte{}
	var days []string
	for _, e := range evs {
		e.ReadBy, e.ReadAt = user, at
		b, err := json.Marshal(readMark{ID: e.ID, User: user, At: at})
		if err != nil {
			return err
		}
		if byDay[e.day()] == nil {
			days = append(days, e.day())
		}
		byDay[e.day()] = append(byDay[e.day()], b)
	}
	var errs []error
	for _, day := range days {
		errs = append(errs, appendLines(s.path(readsPrefix, day), byDay[day]))
	}
	return errors.Join(errs...)
}

// Prune drops the events older than Retention and deletes their files.
func (s *Store) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.cutoff()
	n := 0
	for n < len(s.events) && s.events[n].Time.Before(cutoff) {
		delete(s.byID, s.events[n].ID)
		n++
	}
	s.events = slices.Delete(s.events, 0, n)
	return s.pruneFiles()
}

func (s *Store) cutoff() time.Time { return s.now().Add(-Retention) }

// pruneFiles deletes the day files of days that ended before the cutoff.
func (s *Store) pruneFiles() error {
	keep := s.cutoff().UTC().Format(dayLayout)
	var errs []error
	for _, prefix := range []string{eventsPrefix, readsPrefix} {
		days, err := s.days(prefix)
		if err != nil {
			return err
		}
		for _, day := range days {
			if day < keep {
				if err := os.Remove(s.path(prefix, day)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// days lists the days that have a file with the prefix, oldest first.
func (s *Store) days(prefix string) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var days []string
	for _, ent := range entries {
		day, ok := strings.CutPrefix(ent.Name(), prefix)
		if !ok || ent.IsDir() {
			continue
		}
		day, ok = strings.CutSuffix(day, fileExt)
		if _, err := time.Parse(dayLayout, day); ok && err == nil {
			days = append(days, day)
		}
	}
	slices.Sort(days)
	return days, nil
}

func (s *Store) path(prefix, day string) string {
	return filepath.Join(s.dir, prefix+day+fileExt)
}

func (s *Store) sort() {
	slices.SortStableFunc(s.events, func(a, b *Event) int { return a.Time.Compare(b.Time) })
}

// trim drops the oldest events beyond MaxInMemory.
func (s *Store) trim() {
	if n := len(s.events) - MaxInMemory; n > 0 {
		for _, e := range s.events[:n] {
			delete(s.byID, e.ID)
		}
		s.events = slices.Delete(s.events, 0, n)
	}
}

// readLines calls fn with each line of the file. A missing file has none.
func readLines(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return nil // EOF, or a torn last line
		}
	}
}

// appendLines appends each line and a newline to the file in one write.
func appendLines(path string, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	var buf []byte
	for _, l := range lines {
		buf = append(append(buf, l...), '\n')
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, werr := f.Write(buf)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// writeAtomic writes v as JSON to path through a temporary file, so a
// crash leaves either the old or the new content.
func writeAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
