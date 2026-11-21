package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
)

// Two-factor sign-in: after the password, accounts with an authenticator
// app are asked for a code. Until then they only hold a pending login (its
// own cookie, limited to /login), never a session.

const (
	pendingCookie = "cui_mfa"
	// pendingTTL is how long the code may take after the password.
	pendingTTL = 5 * time.Minute
	// maxCodeFailures wrong codes end a pending login: sign in again.
	maxCodeFailures = 5

	sessRecovery    = "mfa_recovery"     // fresh recovery codes, shown once
	sessSetupSecret = "mfa_setup_secret" // the secret being set up on /account
)

type pendingLogin struct {
	username, account string
	created           time.Time
	setup             bool   // no factor yet: set one up (server.mfa: required)
	secret            string // setup: the secret being set up
	failures          int
}

type pendingLogins struct {
	mu  sync.Mutex
	m   map[string]*pendingLogin
	now func() time.Time
}

func newPendingLogins() *pendingLogins {
	return &pendingLogins{m: map[string]*pendingLogin{}, now: time.Now}
}

func (p *pendingLogins) create(pl pendingLogin) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, o := range p.m {
		if now.Sub(o.created) > pendingTTL {
			delete(p.m, id)
		}
	}
	pl.created = now
	id := auth.RandomToken()
	p.m[id] = &pl
	return id
}

// get returns a copy of a live pending login.
func (p *pendingLogins) get(id string) (pendingLogin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.m[id]
	if !ok {
		return pendingLogin{}, false
	}
	if p.now().Sub(pl.created) > pendingTTL {
		delete(p.m, id)
		return pendingLogin{}, false
	}
	return *pl, true
}

// fail counts a wrong code and ends the pending login after too many.
func (p *pendingLogins) fail(id string) (ended bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.m[id]
	if !ok {
		return true
	}
	pl.failures++
	if pl.failures >= maxCodeFailures {
		delete(p.m, id)
		return true
	}
	return false
}

func (p *pendingLogins) delete(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, id)
}

func (s *Server) mfaOn() bool { return s.mfa != nil && s.mfaMode != config.MFAOff }

func (s *Server) setPendingCookie(w http.ResponseWriter, id string) {
	c := &http.Cookie{
		Name: pendingCookie, Value: id, Path: "/login",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	}
	if id == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func (s *Server) pendingFrom(r *http.Request) (string, pendingLogin, bool) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		return "", pendingLogin{}, false
	}
	pl, ok := s.pending.get(c.Value)
	return c.Value, pl, ok
}

// passwordAccepted continues a login whose password was right: with the
// second factor when the account has one or must set one up, else straight
// into a session.
func (s *Server) passwordAccepted(w http.ResponseWriter, r *http.Request, username, account string) {
	if !s.mfaOn() {
		s.startSession(w, r, username, account, "")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	enrolled, err := s.mfa.Enrolled(account)
	if err != nil {
		s.log.Error("login refused: two-factor store unreadable", "user", username, "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		s.render(w, "login", page{Title: "Sign in", Flash: &flash{Error: true, Msg: "Sign-in is unavailable: the two-factor settings cannot be read. See the server log."}, Data: username})
		return
	}
	if !enrolled && s.mfaMode != config.MFARequired {
		s.startSession(w, r, username, account, "")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pl := pendingLogin{username: username, account: account, setup: !enrolled}
	to := "/login/mfa"
	if pl.setup {
		pl.secret = auth.NewTOTPSecret()
		to = "/login/mfa/setup"
	}
	s.setPendingCookie(w, s.pending.create(pl))
	s.log.Info("password accepted, second factor pending", "user", username, "setup", pl.setup, "remote", r.RemoteAddr)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// startSession signs the user in: a new session (never a reused ID).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, username, account, flashMsg string) *auth.Session {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	sess := s.sessions.CreateFor(username, account)
	if flashMsg != "" {
		sess.SetFlash(flashMsg)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.ID, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
	s.log.Info("login", "user", username, "remote", r.RemoteAddr)
	return sess
}

// setupView is the QR code and key for adding ContainerUI to an app.
type setupView struct {
	QR     template.URL // data: URI of a PNG
	Secret string       // grouped for typing
	URI    string
}

func (s *Server) newSetupView(name, secret string) setupView {
	uri := auth.TOTPURI(s.mfaIssuer, name, secret)
	v := setupView{Secret: auth.FormatSecret(secret), URI: uri}
	if c, err := qr.Encode(uri, qr.M); err == nil {
		c.Scale = 5
		v.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG()))
	} else {
		s.log.Warn("QR code not generated", "error", err)
	}
	return v
}

type mfaLoginView struct {
	Username string
	Setup    setupView
}

func (s *Server) renderMFALogin(w http.ResponseWriter, status int, pl pendingLogin, msg string) {
	name, v := "login_mfa", mfaLoginView{Username: pl.username}
	if pl.setup {
		name, v.Setup = "login_mfa_setup", s.newSetupView(pl.username, pl.secret)
	}
	p := page{Title: "Two-factor sign-in", Data: v}
	if msg != "" {
		p.Flash = &flash{Error: true, Msg: msg}
	}
	w.WriteHeader(status)
	s.render(w, name, p)
}

func (s *Server) handleMFAPage(w http.ResponseWriter, r *http.Request) {
	_, pl, ok := s.pendingFrom(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	setup := r.URL.Path == "/login/mfa/setup"
	if pl.setup != setup {
		http.Redirect(w, r, map[bool]string{true: "/login/mfa/setup", false: "/login/mfa"}[pl.setup], http.StatusSeeOther)
		return
	}
	s.renderMFALogin(w, http.StatusOK, pl, "")
}

// handleMFALogin checks the code of a pending login (POST /login/mfa) or the
// first code of an app being set up (POST /login/mfa/setup).
func (s *Server) handleMFALogin(w http.ResponseWriter, r *http.Request) {
	id, pl, ok := s.pendingFrom(r)
	setup := r.URL.Path == "/login/mfa/setup"
	if !ok || pl.setup != setup {
		s.setPendingCookie(w, "")
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", page{Title: "Sign in", Flash: &flash{Error: true, Msg: "The sign-in took too long. Sign in again."}})
		return
	}
	key := clientIP(r) + "|" + pl.account + "|mfa"
	if ok, wait := s.throttle.Allowed(key); !ok {
		s.renderMFALogin(w, http.StatusTooManyRequests, pl, fmt.Sprintf("Too many wrong codes. Try again in %s.", wait.Round(time.Second)))
		return
	}
	code := r.PostFormValue("code")
	var (
		recovery bool
		codes    []string
		err      error
	)
	if setup {
		codes, err = s.mfa.Enroll(pl.account, pl.username, pl.secret, code)
	} else {
		recovery, err = s.mfa.Check(pl.account, pl.username, code)
	}
	switch {
	case errors.Is(err, auth.ErrBadCode):
		s.throttle.Failure(key)
		s.log.Warn("login failed: wrong second factor", "user", pl.username, "remote", r.RemoteAddr)
		if s.pending.fail(id) {
			s.setPendingCookie(w, "")
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "login", page{Title: "Sign in", Flash: &flash{Error: true, Msg: "Too many wrong codes. Sign in again."}, Data: pl.username})
			return
		}
		s.renderMFALogin(w, http.StatusUnauthorized, pl, "Invalid code.")
		return
	case errors.Is(err, auth.ErrNotEnrolled), errors.Is(err, auth.ErrEnrolled):
		// The second factor was reset or set up meanwhile: start over.
		s.pending.delete(id)
		s.setPendingCookie(w, "")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case err != nil:
		s.log.Error("login refused: two-factor store", "user", pl.username, "error", err)
		s.renderMFALogin(w, http.StatusServiceUnavailable, pl, "Sign-in is unavailable: the two-factor settings cannot be saved. See the server log.")
		return
	}
	s.throttle.Success(key)
	s.pending.delete(id)
	s.setPendingCookie(w, "")
	msg := ""
	if recovery {
		left, _ := s.mfa.RecoveryLeft(pl.account)
		msg = fmt.Sprintf("You signed in with a recovery code; %d left. New ones are on your Account page.", left)
		s.log.Warn("recovery code used", "user", pl.username, "left", left, "remote", r.RemoteAddr)
	}
	sess := s.startSession(w, r, pl.username, pl.account, msg)
	if setup {
		s.log.Info("two-factor sign-in set up", "user", pl.username)
		sess.Set(sessRecovery, codes)
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// accountView is the Account page.
type accountView struct {
	On, Required bool
	Enrolled     bool
	RecoveryLeft int
	Recovery     []string // fresh recovery codes, shown this once
	Setup        setupView
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, nil)
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, f *flash) {
	sess := sessionFrom(r)
	v := accountView{On: s.mfaOn(), Required: s.mfaMode == config.MFARequired}
	if v.On {
		var err error
		if v.Enrolled, err = s.mfa.Enrolled(sess.Account); err != nil {
			s.log.Error("two-factor store", "error", err)
			f, status = errFlash(err), http.StatusInternalServerError
		} else if v.Enrolled {
			v.RecoveryLeft, _ = s.mfa.RecoveryLeft(sess.Account)
			v.Recovery, _ = sess.Pop(sessRecovery).([]string)
		} else {
			secret, _ := sess.Get(sessSetupSecret).(string)
			if secret == "" {
				secret = auth.NewTOTPSecret()
				sess.Set(sessSetupSecret, secret)
			}
			v.Setup = s.newSetupView(sess.Username, secret)
		}
	}
	p := s.newPage(r, "Account", "account", v)
	if f != nil {
		p.Flash = f
	}
	w.WriteHeader(status)
	s.render(w, "account", p)
}

// handleAccountMFA changes the second factor of the signed-in account:
// enroll, recovery (new recovery codes) or disable. The last two need a
// current code from the app.
func (s *Server) handleAccountMFA(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	action := r.PathValue("action")
	if !s.mfaOn() {
		http.Error(w, "two-factor sign-in is off", http.StatusNotFound)
		return
	}
	if action != "enroll" && action != "recovery" && action != "disable" {
		http.NotFound(w, r)
		return
	}
	if action == "disable" && s.mfaMode == config.MFARequired {
		s.renderAccount(w, r, http.StatusForbidden, &flash{Error: true, Msg: "Two-factor sign-in is required on this server and cannot be turned off."})
		return
	}
	key := clientIP(r) + "|" + sess.Account + "|mfa"
	if ok, wait := s.throttle.Allowed(key); !ok {
		s.renderAccount(w, r, http.StatusTooManyRequests, &flash{Error: true, Msg: fmt.Sprintf("Too many wrong codes. Try again in %s.", wait.Round(time.Second))})
		return
	}
	code := r.PostFormValue("code")
	var err error
	switch action {
	case "enroll":
		secret, _ := sess.Get(sessSetupSecret).(string)
		var codes []string
		if codes, err = s.mfa.Enroll(sess.Account, sess.Username, secret, code); err == nil {
			sess.Pop(sessSetupSecret)
			sess.Set(sessRecovery, codes)
			sess.SetFlash("Two-factor sign-in is set up.")
			s.log.Info("two-factor sign-in set up", "user", sess.Username)
		}
	case "recovery":
		if err = s.mfa.CheckApp(sess.Account, code); err == nil {
			var codes []string
			if codes, err = s.mfa.NewRecoveryCodes(sess.Account); err == nil {
				sess.Set(sessRecovery, codes)
				sess.SetFlash("New recovery codes made; the old ones no longer work.")
				s.log.Info("new recovery codes", "user", sess.Username)
			}
		}
	case "disable":
		if err = s.mfa.CheckApp(sess.Account, code); err == nil {
			if err = s.mfa.Disable(sess.Account); err == nil {
				sess.SetFlash("Two-factor sign-in is off.")
				s.log.Warn("two-factor sign-in turned off", "user", sess.Username, "remote", r.RemoteAddr)
			}
		}
	}
	switch {
	case errors.Is(err, auth.ErrBadCode):
		s.throttle.Failure(key)
		s.log.Warn("wrong second factor on the account page", "user", sess.Username, "action", action, "remote", r.RemoteAddr)
		s.renderAccount(w, r, http.StatusUnprocessableEntity, &flash{Error: true, Msg: "Invalid code."})
		return
	case err != nil:
		s.renderAccount(w, r, http.StatusConflict, errFlash(err))
		return
	}
	s.throttle.Success(key)
	redirect(w, r, "/account")
}

// mfaIssuer names this server in authenticator apps.
func mfaIssuer() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "ContainerUI (" + h + ")"
	}
	return "ContainerUI"
}
