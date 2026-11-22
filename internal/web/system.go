package web

import (
	"net/http"
	goruntime "runtime"

	"github.com/svenbeisiegel/containerui"
	"github.com/svenbeisiegel/containerui/internal/hostinfo"
)

// systemData is the System page: the host ContainerUI runs on, its
// hardware and current usage, and which ContainerUI this is.
type systemData struct {
	Version  string
	Platform string       // GOOS/GOARCH of this build
	Logs     *sysLogsView // nil: no system log on this platform
	Live     systemLive
}

// systemLive is the part of the System page that refreshes itself: usage
// numbers, disks and the busiest processes.
type systemLive struct {
	Host hostinfo.Info
	Sort string // "cpu" or "mem": the order of the process table
}

// Procs returns the process table in the chosen order.
func (v systemLive) Procs() []hostinfo.Process {
	if v.Sort == "mem" {
		return v.Host.TopMem
	}
	return v.Host.TopCPU
}

func (s *Server) systemLive(r *http.Request) systemLive {
	v := systemLive{Host: s.hostinfo(r.Context()), Sort: "cpu"}
	if r.URL.Query().Get("sort") == "mem" {
		v.Sort = "mem"
	}
	return v
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	d := systemData{
		Version:  s.version,
		Platform: goruntime.GOOS + "/" + goruntime.GOARCH,
		Logs:     s.sysLogsPanel(r.Context()),
		Live:     s.systemLive(r),
	}
	s.render(w, "system", s.newPage(r, "System", "system", d))
}

// handleSystemPartial serves the self-refreshing part of the System page.
func (s *Server) handleSystemPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "system_live", s.systemLive(r))
}

// handleLicenses serves the third-party notices, the licenses of the
// open-source components in this binary, as plain text.
func (s *Server) handleLicenses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(containerui.Notices))
}
