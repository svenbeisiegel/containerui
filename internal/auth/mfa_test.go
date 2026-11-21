package auth

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 (the last six digits of the eight shown).
func TestTOTPCodeRFC6238(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		got, err := TOTPCode(secret, time.Unix(tc.unix, 0))
		if err != nil || got != tc.want {
			t.Errorf("TOTPCode(%d) = %q, %v; want %q", tc.unix, got, err, tc.want)
		}
	}
}

func newTestMFA(t *testing.T) (*MFA, *time.Time) {
	t.Helper()
	m, err := OpenMFA(filepath.Join(t.TempDir(), "mfa.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	m.now = func() time.Time { return now }
	return m, &now
}

func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := TOTPCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMFAEnrollAndCheck(t *testing.T) {
	m, now := newTestMFA(t)
	secret := NewTOTPSecret()
	if ok, err := m.Enrolled("alice"); ok || err != nil {
		t.Fatalf("Enrolled before setup = %v, %v", ok, err)
	}
	if _, err := m.Enroll("alice", "alice", secret, "000000"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("Enroll with a wrong code: %v", err)
	}
	enrollCode := code(t, secret, *now)
	recovery, err := m.Enroll("alice", "alice", secret, enrollCode[:3]+" "+enrollCode[3:])
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if len(recovery) != RecoveryCodes {
		t.Fatalf("%d recovery codes", len(recovery))
	}
	if _, err := m.Enroll("alice", "alice", secret, enrollCode); !errors.Is(err, ErrEnrolled) {
		t.Errorf("second Enroll: %v", err)
	}
	// The code used to enroll does not work again.
	if _, err := m.Check("alice", "alice", enrollCode); !errors.Is(err, ErrBadCode) {
		t.Errorf("replayed code: %v", err)
	}
	*now = now.Add(30 * time.Second)
	c := code(t, secret, *now)
	if rec, err := m.Check("alice", "alice", c); err != nil || rec {
		t.Errorf("Check(current code) = %v, %v", rec, err)
	}
	if _, err := m.Check("alice", "alice", c); !errors.Is(err, ErrBadCode) {
		t.Errorf("replayed code: %v", err)
	}
	// One step of clock skew either way is accepted, two are not.
	*now = now.Add(time.Minute)
	if _, err := m.Check("alice", "alice", code(t, secret, now.Add(30*time.Second))); err != nil {
		t.Errorf("code one step ahead: %v", err)
	}
	*now = now.Add(5 * time.Minute)
	if _, err := m.Check("alice", "alice", code(t, secret, now.Add(-time.Minute))); !errors.Is(err, ErrBadCode) {
		t.Errorf("code two steps old: %v", err)
	}
	if _, err := m.Check("bob", "bob", c); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("unknown account: %v", err)
	}
}

func TestMFARecoveryCodes(t *testing.T) {
	m, now := newTestMFA(t)
	secret := NewTOTPSecret()
	recovery, err := m.Enroll("alice", "alice", secret, code(t, secret, *now))
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := m.Check("alice", "alice", strings.ToUpper(recovery[3])); err != nil || !rec {
		t.Fatalf("Check(recovery code) = %v, %v", rec, err)
	}
	if _, err := m.Check("alice", "alice", recovery[3]); !errors.Is(err, ErrBadCode) {
		t.Errorf("recovery code used twice: %v", err)
	}
	if n, _ := m.RecoveryLeft("alice"); n != RecoveryCodes-1 {
		t.Errorf("RecoveryLeft = %d", n)
	}
	if err := m.CheckApp("alice", recovery[4]); !errors.Is(err, ErrBadCode) {
		t.Errorf("CheckApp took a recovery code: %v", err)
	}
	fresh, err := m.NewRecoveryCodes("alice")
	if err != nil || len(fresh) != RecoveryCodes {
		t.Fatalf("NewRecoveryCodes = %d, %v", len(fresh), err)
	}
	if _, err := m.Check("alice", "alice", recovery[0]); !errors.Is(err, ErrBadCode) {
		t.Errorf("old recovery code still works: %v", err)
	}
	if _, err := m.Check("alice", "alice", fresh[0]); err != nil {
		t.Errorf("new recovery code: %v", err)
	}
}

func TestMFAPersistsAndResets(t *testing.T) {
	m, now := newTestMFA(t)
	secret := NewTOTPSecret()
	if _, err := m.Enroll("S-1-5-21-1", `CORP\Alice`, secret, code(t, secret, *now)); err != nil {
		t.Fatal(err)
	}
	if goruntime.GOOS != "windows" {
		fi, err := os.Stat(m.Path())
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("store mode = %v", fi.Mode().Perm())
		}
	}
	data, _ := os.ReadFile(m.Path())
	if strings.Contains(string(data), "\"recovery\": null") {
		t.Error("recovery codes not stored")
	}
	// A second store on the same file (the CLI) sees and resets it.
	other, err := OpenMFA(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	removed, err := other.Reset(`corp\alice`)
	if err != nil || len(removed) != 1 || removed[0] != "S-1-5-21-1" {
		t.Fatalf("Reset = %v, %v", removed, err)
	}
	if ok, _ := m.Enrolled("S-1-5-21-1"); ok {
		t.Error("the running store still has the reset account")
	}
	if removed, _ := other.Reset("nobody"); len(removed) != 0 {
		t.Errorf("Reset(nobody) = %v", removed)
	}
}

func TestMFADisable(t *testing.T) {
	m, now := newTestMFA(t)
	secret := NewTOTPSecret()
	if _, err := m.Enroll("alice", "alice", secret, code(t, secret, *now)); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("alice"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.Enrolled("alice"); ok {
		t.Error("still enrolled")
	}
	if err := m.Disable("alice"); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("second Disable: %v", err)
	}
}

func TestOpenMFARejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mfa.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMFA(path); err == nil {
		t.Error("corrupt store accepted")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("ContainerUI (host)", `CORP\alice`, "ABC")
	if !strings.HasPrefix(u, "otpauth://totp/ContainerUI%20%28host%29:CORP%5Calice?") || !strings.Contains(u, "secret=ABC") || !strings.Contains(u, "issuer=ContainerUI+%28host%29") {
		t.Errorf("TOTPURI = %s", u)
	}
	if FormatSecret("ABCDEFGHIJ") != "ABCD EFGH IJ" {
		t.Errorf("FormatSecret = %q", FormatSecret("ABCDEFGHIJ"))
	}
}
