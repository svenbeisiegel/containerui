// Package web serves the ContainerUI HTML interface (Go templates + htmx).
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/compose"
	"github.com/svenbeisiegel/containerui/internal/hostinfo"
	"github.com/svenbeisiegel/containerui/internal/hostlog"
	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

//go:embed templates static
var assets embed.FS

const sessionCookie = "cui_session"

type Options struct {
	Runtime  runtime.Runtime
	Users    auth.Verifier
	Sessions *auth.Sessions
	Jobs     *jobs.Manager
	Secure   bool // served over TLS: mark cookies Secure
	Logger   *slog.Logger
	// ComposeDir is the absolute folder for compose files deployed through
	// the UI; empty disables compose.
	ComposeDir string
	// Version and InstallType are shown on the System page; InstallType
	// says how ContainerUI was installed: rpm, msi, a service or standalone.
	Version     string
	InstallType string
	// WarmInterval limits how often serving the login page starts a
	// background runtime warm-up. Zero means 10s; negative disables it.
	WarmInterval time.Duration
	// HostLog and HostInfo describe the host on the System page; nil means
	// this platform's system log and host information.
	HostLog  hostlog.Source
	HostInfo func(context.Context) hostinfo.Info
}

// favicon is the first of these files present in static/. None is shipped;
// drop an official Mitel icon in internal/web/static/ and rebuild.
var faviconCandidates = []struct{ name, ctype string }{
	{"favicon.svg", "image/svg+xml"},
	{"favicon.png", "image/png"},
	{"favicon.ico", "image/x-icon"},
}

type Server struct {
	rt       runtime.Runtime
	users    auth.Verifier
	sessions *auth.Sessions
	throttle *auth.Throttle
	jobs     *jobs.Manager
	store    compose.Store
	secure   bool
	log      *slog.Logger
	pages    map[string]*template.Template
	partials *template.Template
	static   http.Handler
	warm     *warmer
	favicon  string // URL path, "" if none
	favType  string

	logsPage     *template.Template // standalone live log page
	terminalPage *template.Template // standalone terminal page

	version     string
	installType string
	hostlog     hostlog.Source // nil: no system log on this platform
	hostinfo    func(context.Context) hostinfo.Info

	terminals  atomic.Int32 // open terminal connections
	logStreams atomic.Int32 // open live log streams
}

func New(o Options) (*Server, error) {
	s := &Server{
		rt: o.Runtime, users: o.Users, sessions: o.Sessions, jobs: o.Jobs,
		throttle: auth.NewThrottle(), secure: o.Secure, log: o.Logger, store: compose.Store{Dir: o.ComposeDir},
		pages:   map[string]*template.Template{},
		version: o.Version, installType: o.InstallType,
		hostlog: o.HostLog, hostinfo: o.HostInfo,
	}
	if s.hostlog == nil {
		s.hostlog = hostlog.New()
	}
	if s.hostinfo == nil {
		s.hostinfo = hostinfo.Collect
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	interval := o.WarmInterval
	if interval == 0 {
		interval = defaultWarmInterval
	}
	s.warm = &warmer{rt: s.rt, log: s.log, interval: interval}
	if s.jobs == nil {
		s.jobs = jobs.NewManager(context.Background())
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.StripPrefix("/static/", http.FileServerFS(staticFS))
	for _, c := range faviconCandidates {
		if _, err := fs.Stat(staticFS, c.name); err == nil {
			s.favicon, s.favType = "/static/"+c.name, c.ctype
			break
		}
	}
	return s, nil
}

func (s *Server) parseTemplates() error {
	pages := []string{"login", "dashboard", "containers", "container_new", "container_detail", "images", "volumes", "volume_detail", "compose", "compose_new", "compose_detail", "networks", "network_detail", "system"}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/"+p+".html")
		if err != nil {
			return fmt.Errorf("parse template %s: %w", p, err)
		}
		s.pages[p] = t
	}
	t, err := template.New("partials").Funcs(funcs).ParseFS(assets, "templates/partials.html")
	if err != nil {
		return fmt.Errorf("parse partials: %w", err)
	}
	s.partials = t
	// The live log page has its own minimal document instead of the layout.
	t, err = template.New("logs_watch").Funcs(funcs).ParseFS(assets, "templates/partials.html", "templates/logs_watch.html")
	if err != nil {
		return fmt.Errorf("parse template logs_watch: %w", err)
	}
	s.logsPage = t
	t, err = template.New("terminal_page").Funcs(funcs).ParseFS(assets, "templates/partials.html", "templates/terminal.html")
	if err != nil {
		return fmt.Errorf("parse template terminal_page: %w", err)
	}
	s.terminalPage = t
	return nil
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /login/warm", s.handleWarm)
	mux.HandleFunc("POST /login", s.handleLogin)

	authed := http.NewServeMux()
	authed.HandleFunc("POST /logout", s.handleLogout)
	authed.HandleFunc("GET /{$}", s.handleDashboard)
	authed.HandleFunc("GET /partials/dashboard", s.handleDashboardPartial)
	authed.HandleFunc("GET /containers", s.handleContainers)
	authed.HandleFunc("GET /partials/containers", s.handleContainersPartial)
	authed.HandleFunc("GET /containers/new", s.handleContainerNew)
	authed.HandleFunc("POST /containers", s.handleContainerCreate)
	authed.HandleFunc("GET /containers/{id}", s.handleContainerDetail)
	authed.HandleFunc("POST /containers/{id}/{action}", s.handleContainerAction)
	authed.HandleFunc("GET /containers/{id}/terminal", s.handleTerminalPage)
	authed.HandleFunc("GET /containers/{id}/terminal/check", s.handleTerminalCheck)
	authed.HandleFunc("GET /containers/{id}/terminal/ws", s.handleTerminalWS)
	authed.HandleFunc("GET /containers/{id}/logs/download", s.handleLogsDownload)
	authed.HandleFunc("GET /containers/{id}/logs/watch", s.handleLogsWatch)
	authed.HandleFunc("GET /containers/{id}/logs/stream", s.handleLogsStream)
	authed.HandleFunc("GET /images", s.handleImages)
	authed.HandleFunc("GET /partials/images", s.handleImagesPartial)
	authed.HandleFunc("POST /images/pull", s.handleImagePull)
	authed.HandleFunc("GET /partials/pulls", s.handlePullsPartial)
	authed.HandleFunc("POST /images/tag", s.handleImageTag)
	authed.HandleFunc("POST /images/delete", s.handleImageDelete)
	authed.HandleFunc("GET /volumes", s.handleVolumes)
	authed.HandleFunc("GET /partials/volumes", s.handleVolumesPartial)
	authed.HandleFunc("POST /volumes", s.handleVolumeCreate)
	authed.HandleFunc("GET /volumes/{name}", s.handleVolumeDetail)
	authed.HandleFunc("POST /volumes/delete", s.handleVolumeDelete)
	authed.HandleFunc("POST /volumes/prune", s.handleVolumePrune)
	authed.HandleFunc("GET /networks", s.handleNetworks)
	authed.HandleFunc("GET /partials/networks", s.handleNetworksPartial)
	authed.HandleFunc("POST /networks", s.handleNetworkCreate)
	authed.HandleFunc("GET /networks/{name}", s.handleNetworkDetail)
	authed.HandleFunc("POST /networks/delete", s.handleNetworkDelete)
	authed.HandleFunc("POST /networks/prune", s.handleNetworkPrune)
	authed.HandleFunc("POST /networks/{name}/connect", s.handleNetworkConnect)
	authed.HandleFunc("POST /networks/{name}/disconnect", s.handleNetworkDisconnect)
	authed.HandleFunc("GET /compose", s.handleCompose)
	authed.HandleFunc("GET /partials/compose", s.handleComposePartial)
	authed.HandleFunc("GET /partials/compose/jobs", s.handleComposeJobsPartial)
	authed.HandleFunc("GET /partials/compose/{project}", s.handleComposeProjectPartial)
	authed.HandleFunc("GET /compose/new", s.handleComposeNew)
	authed.HandleFunc("POST /compose", s.handleComposeCreate)
	authed.HandleFunc("POST /compose/validate", s.handleComposeValidate)
	authed.HandleFunc("GET /compose/{project}", s.handleComposeDetail)
	authed.HandleFunc("GET /compose/{project}/edit", s.handleComposeEdit)
	authed.HandleFunc("POST /compose/{project}", s.handleComposeUpdate)
	authed.HandleFunc("POST /compose/{project}/{action}", s.handleComposeAction)
	authed.HandleFunc("GET /system", s.handleSystem)
	authed.HandleFunc("GET /system/licenses", s.handleLicenses)
	authed.HandleFunc("GET /system/logs/watch", s.handleSysLogsWatch)
	authed.HandleFunc("GET /system/logs/stream", s.handleSysLogsStream)
	authed.HandleFunc("GET /system/logs/download", s.handleSysLogsDownload)
	mux.Handle("/", s.requireAuth(s.checkCSRF(authed)))

	return s.recoverer(s.logRequests(securityHeaders(s.checkOrigin(mux))))
}

type ctxKey struct{}

func sessionFrom(r *http.Request) *auth.Session {
	sess, _ := r.Context().Value(ctxKey{}).(*auth.Session)
	return sess
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// redirect works for both plain and htmx requests.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		var sess *auth.Session
		if err == nil {
			sess, _ = s.sessions.Get(c.Value)
		}
		if sess == nil {
			if r.Method == http.MethodGet && !isHTMX(r) && !isWebSocket(r) {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

// checkCSRF requires the session's token on every state-changing request,
// either as form field "csrf" or as header X-CSRF-Token (sent by htmx).
func (s *Server) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if !sessionFrom(r).ValidCSRF(token) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkOrigin rejects cross-origin state-changing requests (including the
// login form) as a second line of defence next to the CSRF token.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			src := r.Header.Get("Origin")
			if src == "" {
				src = r.Header.Get("Referer")
			}
			if src != "" {
				u, err := url.Parse(src)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// allowInlineStyles relaxes the CSP of one response for xterm.js, which
// injects <style> elements for its theme and cell sizes. Scripts stay
// restricted to 'self'.
func allowInlineStyles(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy+"; style-src 'self' 'unsafe-inline'")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController and the WebSocket upgrade reach the
// underlying writer (deadlines, hijacking).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Polling endpoints are noisy; log them at debug level.
		level := slog.LevelInfo
		if r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/partials/") || strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/login/warm") {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start).Round(time.Millisecond), "remote", r.RemoteAddr)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "error", v)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// handleFavicon answers browsers' implicit /favicon.ico request.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if s.favicon == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, s.favicon, http.StatusMovedPermanently)
}

// page is the data passed to every full-page template.
type page struct {
	Favicon     string // set by render
	FaviconType string
	Title       string
	Active      string // nav item
	User        string
	CSRF        string
	Runtime     string
	Caps        runtime.Capabilities
	Flash       *flash
	Data        any
}

type flash struct {
	Error bool
	Msg   string
}

func okFlash(format string, args ...any) *flash { return &flash{Msg: fmt.Sprintf(format, args...)} }
func errFlash(err error) *flash                 { return &flash{Error: true, Msg: userError(err)} }

// userError renders an error for display: the backend's message plus code.
func userError(err error) string {
	var ce *runtime.CommandError
	if errors.As(err, &ce) {
		msg := ce.Output
		if msg == "" {
			msg = ce.Error()
		}
		if ce.Code != "" {
			msg += " [" + ce.Code + "]"
		}
		return msg
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The runtime did not respond in time."
	}
	// Validation errors read better without the sentinel's prefix.
	return strings.ReplaceAll(err.Error(), runtime.ErrInvalid.Error()+": ", "")
}

func (s *Server) newPage(r *http.Request, title, active string, data any) page {
	p := page{Title: title, Active: active, Runtime: s.rt.Name(), Caps: s.caps(), Data: data}
	if sess := sessionFrom(r); sess != nil {
		p.User, p.CSRF = sess.Username, sess.CSRF
		if f := sess.PopFlash(); f != "" {
			p.Flash = &flash{Msg: f}
		}
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	p.Favicon, p.FaviconType = s.favicon, s.favType
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("render page", "page", name, "error", err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partials.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render partial", "partial", name, "error", err)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := s.sessions.Get(c.Value); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	// The user is about to sign in: wake the runtime up while they type.
	s.warm.kick()
	s.render(w, "login", page{Title: "Sign in"})
}

// handleWarm keeps the runtime warm while the sign-in page is open. It needs
// no session; the warmer's rate limit bounds the work it can cause.
func (s *Server) handleWarm(w http.ResponseWriter, r *http.Request) {
	s.warm.kick()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	key := clientIP(r) + "|" + username
	fail := func(msg string) {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", page{Title: "Sign in", Flash: &flash{Error: true, Msg: msg}, Data: username})
	}
	if ok, wait := s.throttle.Allowed(key); !ok {
		fail(fmt.Sprintf("Too many failed attempts. Try again in %s.", wait.Round(time.Second)))
		return
	}
	if !s.users.Verify(username, password) {
		s.throttle.Failure(key)
		s.log.Warn("login failed", "user", username, "remote", r.RemoteAddr)
		fail("Invalid username or password.")
		return
	}
	s.throttle.Success(key)
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	sess := s.sessions.Create(username)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.ID, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
	s.log.Info("login", "user", username, "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(sessionFrom(r).ID)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
	redirect(w, r, "/login")
}
