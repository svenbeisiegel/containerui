package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
)

// mfaEnv is a test server with two-factor sign-in in the given mode.
func mfaEnv(t *testing.T, mode string) (*env, *auth.MFA) {
	t.Helper()
	m, err := auth.OpenMFA(filepath.Join(t.TempDir(), "mfa.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, func(o *Options) { o.MFA, o.MFAMode = m, mode })
	return e, m
}

// enroll sets up admin's app with a known secret, using up the current
// time step; the next code to work is one step ahead.
func enroll(t *testing.T, m *auth.MFA) (secret string, recovery []string) {
	t.Helper()
	secret = auth.NewTOTPSecret()
	recovery, err := m.Enroll("admin", "admin", secret, totp(t, secret, 0))
	if err != nil {
		t.Fatal(err)
	}
	return secret, recovery
}

// totp is the code for secret, steps time steps from now.
func totp(t *testing.T, secret string, steps int) string {
	t.Helper()
	c, err := auth.TOTPCode(secret, time.Now().Add(time.Duration(steps)*30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// req sends a request with the given cookies (no session of the env).
func (e *env) req(method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// passwordStep signs in with the password and expects the second factor.
func passwordStep(t *testing.T, e *env, want string) *http.Cookie {
	t.Helper()
	w := e.req("POST", "/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Fatalf("password step = %d %s, want %s", w.Code, w.Header().Get("Location"), want)
	}
	if cookie(w, sessionCookie) != nil {
		t.Fatal("a session was started before the second factor")
	}
	p := cookie(w, pendingCookie)
	if p == nil || !p.HttpOnly || p.SameSite != http.SameSiteStrictMode || p.Path != "/login" {
		t.Fatalf("pending cookie = %+v", p)
	}
	return p
}

func TestMFAOptionalNotEnrolledSignsInWithPassword(t *testing.T) {
	e, _ := mfaEnv(t, config.MFAOptional)
	e.login()
}

func TestMFAOffIgnoresEnrolledAccount(t *testing.T) {
	e, m := mfaEnv(t, config.MFAOff)
	enroll(t, m)
	e.login()
	if body := e.do("GET", "/account", nil).Body.String(); !strings.Contains(body, "turned off on this server") {
		t.Error("the account page does not say two-factor sign-in is off")
	}
}

func TestMFALogin(t *testing.T) {
	e, m := mfaEnv(t, config.MFAOptional)
	secret, _ := enroll(t, m)
	pending := passwordStep(t, e, "/login/mfa")

	// The pending login is no session.
	if w := e.req("GET", "/containers", nil, pending); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("GET /containers with a pending login = %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := e.req("GET", "/login/mfa", nil, pending); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="code"`) {
		t.Fatalf("code page = %d", w.Code)
	}
	if w := e.req("GET", "/login/mfa", nil); w.Code != http.StatusSeeOther {
		t.Errorf("code page without a pending login = %d", w.Code)
	}
	if w := e.req("POST", "/login/mfa", url.Values{"code": {"000000"}}, pending); w.Code != http.StatusUnauthorized || cookie(w, sessionCookie) != nil {
		t.Errorf("wrong code = %d", w.Code)
	}
	// The code that enrolled was used up.
	if w := e.req("POST", "/login/mfa", url.Values{"code": {totp(t, secret, 0)}}, pending); w.Code != http.StatusUnauthorized {
		t.Errorf("replayed code = %d", w.Code)
	}
	w := e.req("POST", "/login/mfa", url.Values{"code": {totp(t, secret, 1)}}, pending)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("right code = %d %s", w.Code, w.Header().Get("Location"))
	}
	sess := cookie(w, sessionCookie)
	if sess == nil {
		t.Fatal("no session after the second factor")
	}
	if w := e.req("GET", "/containers", nil, sess); w.Code != http.StatusOK {
		t.Errorf("GET /containers after sign-in = %d", w.Code)
	}
	// The pending login is gone.
	if w := e.req("POST", "/login/mfa", url.Values{"code": {totp(t, secret, -1)}}, pending); cookie(w, sessionCookie) != nil {
		t.Error("the pending login was used twice")
	}
	if !strings.Contains(e.logs.String(), "second factor pending") || !strings.Contains(e.logs.String(), "wrong second factor") {
		t.Errorf("logs:\n%s", e.logs.String())
	}
}

func TestMFARecoveryCodeLogin(t *testing.T) {
	e, m := mfaEnv(t, config.MFAOptional)
	_, recovery := enroll(t, m)
	pending := passwordStep(t, e, "/login/mfa")
	w := e.req("POST", "/login/mfa", url.Values{"code": {recovery[0]}}, pending)
	sess := cookie(w, sessionCookie)
	if w.Code != http.StatusSeeOther || sess == nil {
		t.Fatalf("recovery code = %d", w.Code)
	}
	if body := e.req("GET", "/", nil, sess).Body.String(); !strings.Contains(body, "recovery code; 9 left") {
		t.Error("no notice that a recovery code was used")
	}
}

func TestMFATooManyWrongCodesEndPendingLogin(t *testing.T) {
	e, m := mfaEnv(t, config.MFAOptional)
	secret, _ := enroll(t, m)
	pending := passwordStep(t, e, "/login/mfa")
	for range maxCodeFailures {
		e.req("POST", "/login/mfa", url.Values{"code": {"000000"}}, pending)
	}
	w := e.req("POST", "/login/mfa", url.Values{"code": {totp(t, secret, 1)}}, pending)
	if cookie(w, sessionCookie) != nil {
		t.Fatal("signed in after too many wrong codes")
	}
}

var totpKeyRe = regexp.MustCompile(`<code class="totp-key">([A-Z2-7 ]+)</code>`)

func TestMFARequiredSetsUpAtSignIn(t *testing.T) {
	e, m := mfaEnv(t, config.MFARequired)
	pending := passwordStep(t, e, "/login/mfa/setup")
	if w := e.req("GET", "/login/mfa", nil, pending); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login/mfa/setup" {
		t.Errorf("code page while setting up = %d %s", w.Code, w.Header().Get("Location"))
	}
	w := e.req("GET", "/login/mfa/setup", nil, pending)
	key := totpKeyRe.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || key == nil || !strings.Contains(w.Body.String(), `src="data:image/png;base64,`) {
		t.Fatalf("setup page = %d, key %v", w.Code, key)
	}
	secret := strings.ReplaceAll(key[1], " ", "")
	if w := e.req("POST", "/login/mfa/setup", url.Values{"code": {"000000"}}, pending); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong setup code = %d", w.Code)
	}
	w = e.req("POST", "/login/mfa/setup", url.Values{"code": {totp(t, secret, 0)}}, pending)
	sess := cookie(w, sessionCookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/account" || sess == nil {
		t.Fatalf("setup = %d %s", w.Code, w.Header().Get("Location"))
	}
	if ok, _ := m.Enrolled("admin"); !ok {
		t.Fatal("not enrolled after setup")
	}
	body := e.req("GET", "/account", nil, sess).Body.String()
	if strings.Count(body, "<li><code>") != auth.RecoveryCodes {
		t.Error("the recovery codes are not shown after setup")
	}
	if strings.Contains(body, "/account/mfa/disable") {
		t.Error("required two-factor sign-in can be turned off")
	}
	if body := e.req("GET", "/account", nil, sess).Body.String(); strings.Contains(body, "<li><code>") {
		t.Error("the recovery codes are shown twice")
	}
}

func TestMFAAccountPage(t *testing.T) {
	e, m := mfaEnv(t, config.MFAOptional)
	e.login()
	body := e.do("GET", "/account", nil).Body.String()
	key := totpKeyRe.FindStringSubmatch(body)
	if key == nil || !strings.Contains(body, "/account/mfa/enroll") {
		t.Fatal("no setup on the account page")
	}
	secret := strings.ReplaceAll(key[1], " ", "")
	// The same secret is offered until it is set up.
	if again := totpKeyRe.FindStringSubmatch(e.do("GET", "/account", nil).Body.String()); again == nil || again[1] != key[1] {
		t.Error("the secret changed between page loads")
	}
	if w := e.post("/account/mfa/enroll", url.Values{"code": {"000000"}}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("wrong enroll code = %d", w.Code)
	}
	if w := e.do("POST", "/account/mfa/enroll", url.Values{"code": {totp(t, secret, 0)}}); w.Code != http.StatusForbidden {
		t.Errorf("enroll without CSRF token = %d", w.Code)
	}
	if w := e.post("/account/mfa/enroll", url.Values{"code": {totp(t, secret, 0)}}); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/account" {
		t.Fatalf("enroll = %d %v", w.Code, w.Header())
	}
	if ok, _ := m.Enrolled("admin"); !ok {
		t.Fatal("not enrolled")
	}
	if w := e.post("/account/mfa/recovery", url.Values{"code": {totp(t, secret, 1)}}); w.Code != http.StatusSeeOther {
		t.Errorf("new recovery codes = %d", w.Code)
	}
	if w := e.post("/account/mfa/disable", url.Values{"code": {totp(t, secret, 1)}}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("disable with a used code = %d", w.Code)
	}
	if ok, _ := m.Enrolled("admin"); !ok {
		t.Fatal("disabled with a used code")
	}
}

func TestMFARequiredCannotDisable(t *testing.T) {
	e, m := mfaEnv(t, config.MFARequired)
	secret, _ := enroll(t, m)
	pending := passwordStep(t, e, "/login/mfa")
	w := e.req("POST", "/login/mfa", url.Values{"code": {totp(t, secret, 1)}}, pending)
	e.cookie = cookie(w, sessionCookie)
	e.csrf = csrfRe.FindStringSubmatch(e.do("GET", "/account", nil).Body.String())[1]
	if w := e.post("/account/mfa/disable", url.Values{"code": {totp(t, secret, -1)}}); w.Code != http.StatusForbidden {
		t.Errorf("disable when required = %d", w.Code)
	}
	if ok, _ := m.Enrolled("admin"); !ok {
		t.Error("disabled although required")
	}
}
