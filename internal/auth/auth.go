// Package auth implements username/password login, server-side sessions,
// CSRF tokens and login throttling.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/svenbeisiegel/containerui/internal/config"
)

// HashCost is the bcrypt cost used by HashPassword.
const HashCost = 12

func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), HashCost)
	return string(h), err
}

// Users verifies credentials against the configured accounts.
type Users struct {
	hashes map[string][]byte
	dummy  []byte // compared against for unknown users to keep timing uniform
}

// NewUsers builds the user store. Plaintext passwords are hashed in memory
// so every login takes the same bcrypt path. It returns the names of users
// configured with plaintext passwords so the caller can warn about them.
func NewUsers(users []config.User) (*Users, []string, error) {
	u := &Users{hashes: map[string][]byte{}}
	var plaintext []string
	for _, cu := range users {
		hash := []byte(cu.PasswordHash)
		if cu.Password != "" {
			h, err := bcrypt.GenerateFromPassword([]byte(cu.Password), bcrypt.MinCost+2)
			if err != nil {
				return nil, nil, err
			}
			hash = h
			plaintext = append(plaintext, cu.Username)
		} else if _, err := bcrypt.Cost(hash); err != nil {
			return nil, nil, fmt.Errorf("user %s: password_hash is not a bcrypt hash: %w", cu.Username, err)
		}
		u.hashes[cu.Username] = hash
	}
	d, err := bcrypt.GenerateFromPassword([]byte("dummy-password"), HashCost)
	if err != nil {
		return nil, nil, err
	}
	u.dummy = d
	return u, plaintext, nil
}

// Verify reports whether username/password are valid.
func (u *Users) Verify(username, password string) bool {
	hash, ok := u.hashes[username]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(u.dummy, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type Session struct {
	ID       string
	Username string
	CSRF     string
	Created  time.Time
	LastSeen time.Time

	mu    sync.Mutex
	flash string
}

// SetFlash stores a one-time message shown on the next full page render.
func (s *Session) SetFlash(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flash = msg
}

// PopFlash returns and clears the pending flash message.
func (s *Session) PopFlash() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := s.flash
	s.flash = ""
	return msg
}

// ValidCSRF compares a submitted token in constant time.
func (s *Session) ValidCSRF(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRF)) == 1
}

// Sessions is an in-memory session store. Sessions do not survive a
// restart, which simply forces users to log in again.
type Sessions struct {
	mu       sync.Mutex
	byID     map[string]*Session
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

func NewSessions(idle, absolute time.Duration) *Sessions {
	return &Sessions{byID: map[string]*Session{}, idle: idle, absolute: absolute, now: time.Now}
}

// Create starts a new session (always with a fresh ID).
func (s *Sessions) Create(username string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, sess := range s.byID {
		if s.expired(sess, now) {
			delete(s.byID, id)
		}
	}
	sess := &Session{ID: randomToken(), Username: username, CSRF: randomToken(), Created: now, LastSeen: now}
	s.byID[sess.ID] = sess
	return sess
}

// Get returns a live session and refreshes its idle timer.
func (s *Sessions) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	now := s.now()
	if s.expired(sess, now) {
		delete(s.byID, id)
		return nil, false
	}
	sess.LastSeen = now
	return sess, true
}

// Alive reports whether a session is still valid without refreshing its idle
// timer, for long-lived connections that must end with the session.
func (s *Sessions) Alive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	return ok && !s.expired(sess, s.now())
}

func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *Sessions) expired(sess *Session, now time.Time) bool {
	return now.Sub(sess.LastSeen) > s.idle || now.Sub(sess.Created) > s.absolute
}

// Throttle slows down password guessing: after maxFree failures for a key
// (client IP + username), further attempts are refused for an exponentially
// growing lockout period.
type Throttle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
	now     func() time.Time
}

type throttleEntry struct {
	failures int
	until    time.Time
	last     time.Time
}

const (
	maxFree    = 5
	maxLockout = 15 * time.Minute
)

func NewThrottle() *Throttle {
	return &Throttle{entries: map[string]*throttleEntry{}, now: time.Now}
}

// Allowed reports whether an attempt for key may proceed, and if not, how
// long to wait.
func (t *Throttle) Allowed(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok {
		return true, 0
	}
	if wait := e.until.Sub(t.now()); wait > 0 {
		return false, wait
	}
	return true, 0
}

func (t *Throttle) Failure(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, e := range t.entries {
		if now.Sub(e.last) > maxLockout*2 {
			delete(t.entries, k)
		}
	}
	e, ok := t.entries[key]
	if !ok {
		e = &throttleEntry{}
		t.entries[key] = e
	}
	e.failures++
	e.last = now
	if e.failures >= maxFree {
		lock := time.Second << min(e.failures-maxFree, 10)
		e.until = now.Add(min(lock, maxLockout))
	}
}

func (t *Throttle) Success(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}
