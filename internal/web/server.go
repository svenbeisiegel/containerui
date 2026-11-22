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
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/events"
	"github.com/svenbeisiegel/containerui/internal/hostinfo"
	"github.com/svenbeisiegel/containerui/internal/hostlog"
	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/kube"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/updates"
)

//go:embed templates static
var assets embed.FS

const sessionCookie = "cui_session"

type Options struct {
	// Runtime is the container runtime; nil in Kubernetes mode.
	Runtime runtime.Runtime
	// Kube switches the UI to Kubernetes mode: the pages show this
	// cluster instead of a container runtime.
	Kube     *kube.Cluster
	Users    auth.Verifier
	Sessions *auth.Sessions
	// MFA keeps the accounts' second factor; nil turns two-factor sign-in
	// off. MFAMode is server.mfa (off, optional, required).
	MFA     *auth.MFA
	MFAMode string
	Jobs    *jobs.Manager
	Secure  bool // served over TLS: mark cookies Secure
	Logger  *slog.Logger
	// ComposeDir is the absolute folder for compose files deployed through
	// the UI; empty disables compose.
	ComposeDir string
	// Version is shown on the System page.
	Version string
	// WarmInterval limits how often serving the login page starts a
	// background runtime warm-up. Zero means 10s; negative disables it.
	WarmInterval time.Duration
	// HostLog and HostInfo describe the host on the System page; nil means
	// this platform's system log and host information.
	HostLog  hostlog.Source
	HostInfo func(context.Context) hostinfo.Info
	// Events are the host's error events for the Events page; nil hides
	// the page.
	Events *events.Store
	// Updates knows which local images have a newer version; nil hides
	// image updates.
	Updates *updates.Scanner
	// Stater reads the resource usage (e.g. through the runtime API); nil
	// asks Runtime when it implements runtime.Stater.
	Stater runtime.Stater
	// Metrics sets the monitoring intervals; zero values are the defaults.
	Metrics config.Metrics
	// VolumeUploadDir keeps uploaded volume backups until they are
	// restored; empty disables volume restore.
	VolumeUploadDir string
}

// favicon is the first of these files present in static/. favicon.svg ships
// as a copy of the ContainerUI logo (static/logo.svg).
var faviconCandidates = []struct{ name, ctype string }{
	{"favicon.svg", "image/svg+xml"},
	{"favicon.png", "image/png"},
	{"favicon.ico", "image/x-icon"},
}

type Server struct {
	rt        runtime.Runtime
	kube      *kube.Cluster // nil: container runtime mode
	users     auth.Verifier
	sessions  *auth.Sessions
	throttle  *auth.Throttle
	mfa       *auth.MFA
	mfaMode   string
	mfaIssuer string
	pending   *pendingLogins
	jobs      *jobs.Manager
	store     compose.Store
	secure    bool
	log       *slog.Logger
	pages     map[string]*template.Template
	partials  *template.Template
	static    http.Handler
	warm      *warmer
	metrics   *metricsSampler // nil: the runtime reports no resource usage
	favicon   string          // URL path, "" if none
	favType   string

	logsPage     *template.Template // standalone live log page
	terminalPage *template.Template // standalone terminal page
	attachPage   *template.Template // standalone attach page

	version  string
	hostlog  hostlog.Source // nil: no system log on this platform
	hostinfo func(context.Context) hostinfo.Info
	events   *events.Store    // nil: no Events page
	updates  *updates.Scanner // nil: no image updates
	uploads  *uploadStore     // nil: no volume restore

	terminals  atomic.Int32 // open terminal connections
	logStreams atomic.Int32 // open live log streams
}

func New(o Options) (*Server, error) {
	s := &Server{
		rt: o.Runtime, users: o.Users, sessions: o.Sessions, jobs: o.Jobs,
		mfa: o.MFA, mfaMode: o.MFAMode, mfaIssuer: mfaIssuer(), pending: newPendingLogins(),
		throttle: auth.NewThrottle(), secure: o.Secure, log: o.Logger, store: compose.Store{Dir: o.ComposeDir},
		pages:   map[string]*template.Template{},
		version: o.Version,
		hostlog: o.HostLog, hostinfo: o.HostInfo, events: o.Events, updates: o.Updates,
	}
	if o.Kube != nil {
		s.kube = o.Kube
		if s.rt == nil {
			s.rt = clusterRuntime{}
		}
		o.WarmInterval = -1 // nothing to warm up
		o.VolumeUploadDir = ""
		s.store = compose.Store{}
	}
	if o.VolumeUploadDir != "" {
		s.uploads = &uploadStore{dir: o.VolumeUploadDir}
		s.uploads.sweep(time.Now())
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
	st := o.Stater
	if st == nil {
		st, _ = s.rt.(runtime.Stater)
	}
	if st != nil && s.rt.Capabilities().Stats {
		s.metrics = newMetricsSampler(st, s.log, o.Metrics)
	}
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
	if s.metrics != nil {
		s.metrics.start()
	}
	return s, nil
}

// Close stops background work: the metrics sampler.
func (s *Server) Close() {
	if s.metrics != nil {
		s.metrics.close()
	}
}

func (s *Server) parseTemplates() error {
	pages := []string{"login", "login_mfa", "login_mfa_setup", "account", "dashboard", "containers", "container_choose", "container_import", "container_new", "container_detail", "images", "image_detail", "image_pull", "volumes", "volume_new", "volume_detail", "volume_restore", "volume_files", "volume_edit", "compose", "compose_new", "compose_detail", "networks", "network_new", "network_detail", "system", "events",
		"kube_dashboard", "kube_workloads", "kube_workload", "kube_pods", "kube_pod", "kube_services", "kube_service", "kube_storage", "kube_pvc", "kube_nodes", "kube_node", "kube_cluster_events"}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/kube_partials.html", "templates/"+p+".html")
		if err != nil {
			return fmt.Errorf("parse template %s: %w", p, err)
		}
		s.pages[p] = t
	}
	t, err := template.New("partials").Funcs(funcs).ParseFS(assets, "templates/partials.html", "templates/kube_partials.html")
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
	t, err = template.New("attach_page").Funcs(funcs).ParseFS(assets, "templates/partials.html", "templates/attach.html")
	if err != nil {
		return fmt.Errorf("parse template attach_page: %w", err)
	}
	s.attachPage = t
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
	mux.HandleFunc("GET /login/mfa", s.handleMFAPage)
	mux.HandleFunc("POST /login/mfa", s.handleMFALogin)
	mux.HandleFunc("GET /login/mfa/setup", s.handleMFAPage)
	mux.HandleFunc("POST /login/mfa/setup", s.handleMFALogin)

	authed := http.NewServeMux()
	authed.HandleFunc("POST /logout", s.handleLogout)
	authed.HandleFunc("GET /account", s.handleAccount)
	authed.HandleFunc("POST /account/mfa/{action}", s.handleAccountMFA)
	if s.kube != nil {
		s.registerKubeRoutes(authed)
	} else {
		s.registerRuntimeRoutes(authed)
	}
	authed.HandleFunc("GET /partials/activity", s.handleActivityPartial)
	authed.HandleFunc("POST /activity/clear", s.handleActivityClear)
	authed.HandleFunc("GET /system", s.handleSystem)
	authed.HandleFunc("GET /partials/system", s.handleSystemPartial)
	authed.HandleFunc("GET /system/licenses", s.handleLicenses)
	authed.HandleFunc("GET /system/logs/watch", s.handleSysLogsWatch)
	authed.HandleFunc("GET /system/logs/stream", s.handleSysLogsStream)
	authed.HandleFunc("GET /system/logs/download", s.handleSysLogsDownload)
	authed.HandleFunc("GET /events", s.handleEvents)
	authed.HandleFunc("GET /partials/events", s.handleEventsPartial)
	authed.HandleFunc("GET /partials/events/unread", s.handleEventsUnread)
	authed.HandleFunc("POST /events/read", s.handleEventsRead)
	authed.HandleFunc("POST /events/{id}/open", s.handleEventOpen)
	mux.Handle("/", s.requireAuth(s.checkCSRF(authed)))

	return s.recoverer(s.logRequests(securityHeaders(s.checkOrigin(mux))))
}

// registerRuntimeRoutes adds the pages of a container runtime.
func (s *Server) registerRuntimeRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /{$}", s.handleDashboard)
	m.HandleFunc("GET /partials/dashboard", s.handleDashboardPartial)
	m.HandleFunc("GET /containers", s.handleContainers)
	m.HandleFunc("GET /partials/containers", s.handleContainersPartial)
	m.HandleFunc("GET /containers/new", s.handleContainerNew)
	m.HandleFunc("POST /containers", s.handleContainerCreate)
	m.HandleFunc("GET /containers/new/scratch", s.handleContainerScratch)
	// Literal paths: a {source} wildcard would clash with /containers/{id}/edit.
	for src := range importSources {
		m.HandleFunc("GET /containers/new/"+src, s.handleContainerImportPage)
		m.HandleFunc("POST /containers/new/"+src, s.handleContainerImport)
	}
	m.HandleFunc("GET /containers/{id}", s.handleContainerDetail)
	m.HandleFunc("POST /containers/{id}/{action}", s.handleContainerAction)
	m.HandleFunc("GET /containers/{id}/edit", s.handleContainerEdit)
	m.HandleFunc("POST /containers/{id}/edit", s.handleContainerEditSave)
	m.HandleFunc("POST /containers/{id}/update", s.handleContainerUpdate)
	m.HandleFunc("POST /containers/{id}/recreate", s.handleContainerRecreate)
	m.HandleFunc("GET /partials/containers/{id}/job", s.handleRecreateJobPartial)
	m.HandleFunc("GET /containers/{id}/terminal", s.handleTerminalPage)
	m.HandleFunc("GET /containers/{id}/terminal/ws", s.handleTerminalWS)
	m.HandleFunc("GET /containers/{id}/attach", s.handleAttachPage)
	m.HandleFunc("GET /containers/{id}/attach/ws", s.handleAttachWS)
	m.HandleFunc("GET /containers/{id}/logs/download", s.handleLogsDownload)
	m.HandleFunc("GET /containers/{id}/logs/watch", s.handleLogsWatch)
	m.HandleFunc("GET /containers/{id}/logs/stream", s.handleLogsStream)
	m.HandleFunc("GET /images", s.handleImages)
	m.HandleFunc("GET /partials/images", s.handleImagesPartial)
	m.HandleFunc("GET /images/pull", s.handleImagePullPage)
	m.HandleFunc("POST /images/pull", s.handleImagePull)
	m.HandleFunc("POST /images/check-updates", s.handleImageCheckUpdates)
	m.HandleFunc("POST /images/tag", s.handleImageTag)
	m.HandleFunc("POST /images/delete", s.handleImageDelete)
	m.HandleFunc("POST /images/prune", s.handleImagePrune)
	m.HandleFunc("GET /images/{ref...}", s.handleImageDetail)
	m.HandleFunc("GET /volumes", s.handleVolumes)
	m.HandleFunc("GET /partials/volumes", s.handleVolumesPartial)
	m.HandleFunc("GET /volumes/new", s.handleVolumeNew)
	m.HandleFunc("POST /volumes", s.handleVolumeCreate)
	m.HandleFunc("GET /volumes/{name}", s.handleVolumeDetail)
	m.HandleFunc("POST /volumes/delete", s.handleVolumeDelete)
	m.HandleFunc("POST /volumes/prune", s.handleVolumePrune)
	m.HandleFunc("POST /volumes/backup", s.handleVolumeBackup)
	m.HandleFunc("GET /volumes/restore", s.handleVolumeRestorePage)
	m.HandleFunc("POST /volumes/restore/upload", s.handleVolumeRestoreUpload)
	m.HandleFunc("GET /volumes/restore/{id}", s.handleVolumeRestoreChoose)
	m.HandleFunc("POST /volumes/restore/{id}", s.handleVolumeRestore)
	m.HandleFunc("POST /volumes/restore/{id}/discard", s.handleVolumeRestoreDiscard)
	m.HandleFunc("GET /volumes/files/{name}", s.handleVolumeFiles)
	m.HandleFunc("GET /partials/volume-files/{name}", s.handleVolumeFilesPartial)
	m.HandleFunc("GET /volumes/files/{name}/raw", s.handleVolumeFileRaw)
	m.HandleFunc("GET /volumes/files/{name}/edit", s.handleVolumeFileEdit)
	m.HandleFunc("POST /volumes/files/{name}/save", s.handleVolumeFileSave)
	m.HandleFunc("POST /volumes/files/{name}/upload", s.handleVolumeUpload)
	m.HandleFunc("POST /volumes/files/{name}/mkdir", s.handleVolumeMkdir)
	m.HandleFunc("POST /volumes/files/{name}/newfile", s.handleVolumeNewFile)
	m.HandleFunc("POST /volumes/files/{name}/rename", s.handleVolumeRename)
	m.HandleFunc("POST /volumes/files/{name}/delete", s.handleVolumeRemovePath)
	m.HandleFunc("GET /networks", s.handleNetworks)
	m.HandleFunc("GET /partials/networks", s.handleNetworksPartial)
	m.HandleFunc("GET /networks/new", s.handleNetworkNew)
	m.HandleFunc("POST /networks", s.handleNetworkCreate)
	m.HandleFunc("GET /networks/{name}", s.handleNetworkDetail)
	m.HandleFunc("POST /networks/delete", s.handleNetworkDelete)
	m.HandleFunc("POST /networks/prune", s.handleNetworkPrune)
	m.HandleFunc("POST /networks/{name}/connect", s.handleNetworkConnect)
	m.HandleFunc("POST /networks/{name}/disconnect", s.handleNetworkDisconnect)
	m.HandleFunc("GET /compose", s.handleCompose)
	m.HandleFunc("GET /partials/compose", s.handleComposePartial)
	m.HandleFunc("GET /partials/compose/{project}", s.handleComposeProjectPartial)
	m.HandleFunc("GET /compose/new", s.handleComposeNew)
	m.HandleFunc("POST /compose", s.handleComposeCreate)
	m.HandleFunc("POST /compose/validate", s.handleComposeValidate)
	m.HandleFunc("GET /compose/{project}", s.handleComposeDetail)
	m.HandleFunc("GET /compose/{project}/edit", s.handleComposeEdit)
	m.HandleFunc("POST /compose/{project}", s.handleComposeUpdate)
	m.HandleFunc("POST /compose/{project}/{action}", s.handleComposeAction)
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
	Kube        bool // Kubernetes mode: the cluster navigation
	Caps        runtime.Capabilities
	Flash       *flash
	Data        any
	// HasEvents shows the Events page in the navigation, with the number
	// of unread events.
	HasEvents    bool
	EventsUnread int
	// Activity is the side bar of the pages that show it (activity.go).
	Activity *activityView
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
	p := page{Title: title, Active: active, Runtime: s.rt.Name(), Kube: s.kube != nil, Caps: s.caps(), Data: data}
	if s.events != nil {
		p.HasEvents, p.EventsUnread = true, s.events.Unread()
	}
	if sess := sessionFrom(r); sess != nil {
		p.User, p.CSRF = sess.Username, sess.CSRF
		if f := sess.PopFlash(); f != "" {
			p.Flash = &flash{Msg: f}
		}
	}
	p.Activity = s.pageActivity(r, active)
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
	account, ok := s.users.Verify(username, password)
	if !ok {
		s.throttle.Failure(key)
		s.log.Warn("login failed", "user", username, "remote", r.RemoteAddr)
		fail("Invalid username or password.")
		return
	}
	s.throttle.Success(key)
	s.passwordAccepted(w, r, username, account)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(sessionFrom(r).ID)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
	redirect(w, r, "/login")
}
