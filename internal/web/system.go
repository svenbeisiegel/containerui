package web

import (
	"net/http"
	"os"
	goruntime "runtime"

	"github.com/svenbeisiegel/containerui"
	"github.com/svenbeisiegel/containerui/internal/hostinfo"
)

// systemData is the System page: what runs, how it was installed and the
// host it runs on.
type systemData struct {
	Version     string
	OS, Arch    string
	GoVersion   string
	InstallType string
	Executable  string
	PID         int
	Host        hostinfo.Info
	Logs        *sysLogsView // nil: no system log on this platform
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	d := systemData{
		Version:     s.version,
		OS:          goruntime.GOOS,
		Arch:        goruntime.GOARCH,
		GoVersion:   goruntime.Version(),
		InstallType: s.installType,
		PID:         os.Getpid(),
		Host:        s.hostinfo(r.Context()),
		Logs:        s.sysLogsPanel(r.Context()),
	}
	if exe, err := os.Executable(); err == nil {
		d.Executable = exe
	}
	s.render(w, "system", s.newPage(r, "System", "system", d))
}

// handleLicenses serves the third-party notices, the licenses of the
// open-source components in this binary, as plain text.
func (s *Server) handleLicenses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(containerui.Notices))
}
