package auth

import (
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
)

func TestUsers(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	u, plain, err := NewUsers([]config.User{
		{Username: "admin", PasswordHash: hash},
		{Username: "dev", Password: "devpw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || plain[0] != "dev" {
		t.Errorf("plaintext users = %v", plain)
	}
	checks := []struct {
		user, pw string
		ok       bool
	}{
		{"admin", "s3cret", true},
		{"admin", "wrong", false},
		{"dev", "devpw", true},
		{"nobody", "s3cret", false},
		{"admin", "", false},
	}
	for _, c := range checks {
		if got := u.Verify(c.user, c.pw); got != c.ok {
			t.Errorf("Verify(%q, %q) = %v", c.user, c.pw, got)
		}
	}
	if _, _, err := NewUsers([]config.User{{Username: "x", PasswordHash: "not-bcrypt"}}); err == nil {
		t.Error("invalid hash accepted")
	}
}

func TestSessions(t *testing.T) {
	now := time.Now()
	s := NewSessions(10*time.Minute, time.Hour)
	s.now = func() time.Time { return now }

	sess := s.Create("admin")
	if sess.ID == "" || sess.CSRF == "" || sess.ID == sess.CSRF {
		t.Fatalf("session = %+v", sess)
	}
	if got, ok := s.Get(sess.ID); !ok || got.Username != "admin" {
		t.Fatal("session not found")
	}

	now = now.Add(9 * time.Minute)
	if _, ok := s.Get(sess.ID); !ok {
		t.Fatal("session expired too early")
	}
	now = now.Add(11 * time.Minute)
	if _, ok := s.Get(sess.ID); ok {
		t.Fatal("idle session still valid")
	}

	// Absolute timeout applies even while active.
	sess = s.Create("admin")
	for i := 0; i < 7; i++ {
		now = now.Add(9 * time.Minute)
		s.Get(sess.ID)
	}
	if _, ok := s.Get(sess.ID); ok {
		t.Fatal("session outlived absolute timeout")
	}

	sess = s.Create("admin")
	s.Delete(sess.ID)
	if _, ok := s.Get(sess.ID); ok {
		t.Fatal("deleted session still valid")
	}
}

func TestCSRF(t *testing.T) {
	sess := NewSessions(time.Hour, time.Hour).Create("a")
	if !sess.ValidCSRF(sess.CSRF) || sess.ValidCSRF("") || sess.ValidCSRF("x") {
		t.Error("csrf validation wrong")
	}
}

func TestThrottle(t *testing.T) {
	now := time.Now()
	th := NewThrottle()
	th.now = func() time.Time { return now }
	key := "1.2.3.4|admin"
	for i := 0; i < maxFree-1; i++ {
		th.Failure(key)
		if ok, _ := th.Allowed(key); !ok {
			t.Fatalf("locked after %d failures", i+1)
		}
	}
	th.Failure(key)
	if ok, wait := th.Allowed(key); ok || wait <= 0 {
		t.Fatal("not locked after maxFree failures")
	}
	if ok, _ := th.Allowed("other|admin"); !ok {
		t.Fatal("other key locked")
	}
	now = now.Add(2 * time.Second)
	if ok, _ := th.Allowed(key); !ok {
		t.Fatal("lock did not expire")
	}
	th.Success(key)
	th.Failure(key)
	if ok, _ := th.Allowed(key); !ok {
		t.Fatal("success did not reset")
	}
}
