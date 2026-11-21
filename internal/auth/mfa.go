package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// TOTP parameters (RFC 6238) as every authenticator app expects them.
const (
	totpPeriod = 30 * time.Second
	totpDigits = 6
	// totpSkew is how many steps before and after the current one are
	// accepted, for clocks that are a little off.
	totpSkew = 1
	// RecoveryCodes is how many one-time recovery codes an account gets.
	RecoveryCodes = 10
)

var (
	ErrBadCode     = errors.New("the code is not valid")
	ErrNotEnrolled = errors.New("two-factor sign-in is not set up for this account")
	ErrEnrolled    = errors.New("two-factor sign-in is already set up for this account")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// MFA keeps each account's second sign-in factor: a TOTP secret and
// one-time recovery codes, in a JSON file only its owner may read.
//
// Every operation reads the file afresh and writes it back atomically, so a
// reset from the command line (containerui mfa-reset) takes effect at once
// in the running server.
type MFA struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

type mfaFile struct {
	Accounts map[string]*mfaAccount `json:"accounts"`
}

type mfaAccount struct {
	Name     string    `json:"name"`      // username as last typed, for mfa-reset
	Secret   string    `json:"secret"`    // base32
	LastStep int64     `json:"last_step"` // newest time step used: a code works once
	Recovery []string  `json:"recovery"`  // SHA-256 of the unused recovery codes
	Enrolled time.Time `json:"enrolled"`
}

// OpenMFA uses the store at path, which need not exist yet. It fails when
// the file exists but cannot be read.
func OpenMFA(path string) (*MFA, error) {
	m := &MFA{path: path, now: time.Now}
	if _, err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Path is the store's file.
func (m *MFA) Path() string { return m.path }

func (m *MFA) load() (*mfaFile, error) {
	f := &mfaFile{Accounts: map[string]*mfaAccount{}}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read two-factor store: %w", err)
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("read two-factor store %s: %w", m.path, err)
	}
	if f.Accounts == nil {
		f.Accounts = map[string]*mfaAccount{}
	}
	return f, nil
}

func (m *MFA) save(f *mfaFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("save two-factor store: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mfa-*.json")
	if err != nil {
		return fmt.Errorf("save two-factor store: %w", err)
	}
	defer os.Remove(tmp.Name())
	// CreateTemp makes the file 0600; Chmod in case the umask did not.
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("save two-factor store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("save two-factor store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save two-factor store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save two-factor store: %w", err)
	}
	if err := os.Rename(tmp.Name(), m.path); err != nil {
		return fmt.Errorf("save two-factor store: %w", err)
	}
	return nil
}

// update runs fn on the store under the lock and saves it when fn returns
// save = true.
func (m *MFA) update(fn func(f *mfaFile) (save bool, err error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.load()
	if err != nil {
		return err
	}
	save, err := fn(f)
	if save {
		if serr := m.save(f); serr != nil {
			return serr
		}
	}
	return err
}

// Enrolled reports whether account has a second factor.
func (m *MFA) Enrolled(account string) (bool, error) {
	var ok bool
	err := m.update(func(f *mfaFile) (bool, error) {
		_, ok = f.Accounts[account]
		return false, nil
	})
	return ok, err
}

// RecoveryLeft is the number of unused recovery codes of account.
func (m *MFA) RecoveryLeft(account string) (int, error) {
	var n int
	err := m.update(func(f *mfaFile) (bool, error) {
		a, ok := f.Accounts[account]
		if !ok {
			return false, ErrNotEnrolled
		}
		n = len(a.Recovery)
		return false, nil
	})
	return n, err
}

// NewTOTPSecret returns a random 160-bit secret, base32 as apps take it.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPURI is the otpauth:// URI that a QR code hands to authenticator apps.
func TOTPURI(issuer, name, secret string) string {
	q := url.Values{
		"secret": {secret}, "issuer": {issuer},
		"algorithm": {"SHA1"}, "digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(int(totpPeriod.Seconds()))},
	}
	label := url.PathEscape(issuer) + ":" + url.PathEscape(name)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// FormatSecret groups a secret in blocks of four for typing it in.
func FormatSecret(secret string) string {
	var parts []string
	for len(secret) > 4 {
		parts = append(parts, secret[:4])
		secret = secret[4:]
	}
	return strings.Join(append(parts, secret), " ")
}

func totpCode(key []byte, step int64) string {
	mac := hmac.New(sha1.New, key)
	_ = binary.Write(mac, binary.BigEndian, step)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// TOTPCode is the code for secret at time t, for tests and tools.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(secret)
	if err != nil {
		return "", err
	}
	return totpCode(key, t.Unix()/int64(totpPeriod.Seconds())), nil
}

// matchTOTP returns the time step code belongs to, if it is one near now and
// newer than after (each code works once).
func matchTOTP(secret, code string, now time.Time, after int64) (int64, bool) {
	key, err := b32.DecodeString(secret)
	if err != nil || len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / int64(totpPeriod.Seconds())
	for step := cur - totpSkew; step <= cur+totpSkew; step++ {
		if step > after && subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// normalizeCode drops what people type around a code: spaces and dashes.
func normalizeCode(code string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(code)))
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no look-alikes

// newRecoveryCodes returns codes like "k7m2p-x9qrt" (about 49 bits each)
// and the hashes to keep.
func newRecoveryCodes() (codes, hashes []string) {
	for range RecoveryCodes {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var sb strings.Builder
		for i, c := range b {
			if i == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		codes = append(codes, sb.String())
		hashes = append(hashes, hashRecovery(sb.String()))
	}
	return codes, hashes
}

func hashRecovery(code string) string {
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// Enroll sets up the second factor of account with secret, once code shows
// that the user's app has it. It returns the recovery codes, which are
// shown once and kept only as hashes.
func (m *MFA) Enroll(account, name, secret, code string) ([]string, error) {
	var codes []string
	err := m.update(func(f *mfaFile) (bool, error) {
		if _, ok := f.Accounts[account]; ok {
			return false, ErrEnrolled
		}
		step, ok := matchTOTP(secret, normalizeCode(code), m.now(), 0)
		if !ok {
			return false, ErrBadCode
		}
		var hashes []string
		codes, hashes = newRecoveryCodes()
		f.Accounts[account] = &mfaAccount{Name: name, Secret: secret, LastStep: step, Recovery: hashes, Enrolled: m.now().UTC()}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// Check verifies a code from the authenticator app or an unused recovery
// code, which it then uses up. recovery reports which of the two it was.
func (m *MFA) Check(account, name, code string) (recovery bool, err error) {
	code = normalizeCode(code)
	err = m.update(func(f *mfaFile) (bool, error) {
		a, ok := f.Accounts[account]
		if !ok {
			return false, ErrNotEnrolled
		}
		if isDigits(code) {
			step, ok := matchTOTP(a.Secret, code, m.now(), a.LastStep)
			if !ok {
				return false, ErrBadCode
			}
			a.LastStep, a.Name = step, name
			return true, nil
		}
		h := hashRecovery(code)
		i := slices.IndexFunc(a.Recovery, func(r string) bool { return subtle.ConstantTimeCompare([]byte(r), []byte(h)) == 1 })
		if i < 0 {
			return false, ErrBadCode
		}
		a.Recovery = slices.Delete(a.Recovery, i, i+1)
		a.Name = name
		recovery = true
		return true, nil
	})
	return recovery, err
}

// CheckApp verifies a code from the authenticator app only (no recovery
// codes), to confirm changes to the second factor itself.
func (m *MFA) CheckApp(account, code string) error {
	code = normalizeCode(code)
	return m.update(func(f *mfaFile) (bool, error) {
		a, ok := f.Accounts[account]
		if !ok {
			return false, ErrNotEnrolled
		}
		step, ok := matchTOTP(a.Secret, code, m.now(), a.LastStep)
		if !ok {
			return false, ErrBadCode
		}
		a.LastStep = step
		return true, nil
	})
}

// NewRecoveryCodes replaces the recovery codes of account.
func (m *MFA) NewRecoveryCodes(account string) ([]string, error) {
	var codes []string
	err := m.update(func(f *mfaFile) (bool, error) {
		a, ok := f.Accounts[account]
		if !ok {
			return false, ErrNotEnrolled
		}
		codes, a.Recovery = newRecoveryCodes()
		return true, nil
	})
	return codes, err
}

// Disable removes the second factor of account.
func (m *MFA) Disable(account string) error {
	return m.update(func(f *mfaFile) (bool, error) {
		if _, ok := f.Accounts[account]; !ok {
			return false, ErrNotEnrolled
		}
		delete(f.Accounts, account)
		return true, nil
	})
}

// Reset removes the second factor of the accounts that are who (an account
// key such as a Windows SID) or were last signed in to as who (a username,
// ignoring case). It returns the removed accounts.
func (m *MFA) Reset(who string) ([]string, error) {
	var removed []string
	err := m.update(func(f *mfaFile) (bool, error) {
		for key, a := range f.Accounts {
			if key == who || strings.EqualFold(a.Name, who) {
				removed = append(removed, key)
				delete(f.Accounts, key)
			}
		}
		slices.Sort(removed)
		return len(removed) > 0, nil
	})
	return removed, err
}
