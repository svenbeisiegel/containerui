package web

import (
	"net/http"
	"os"
	goruntime "runtime"
)

// systemData is the System page: what runs and how it was installed.
type systemData struct {
	Version     string
	OS, Arch    string
	GoVersion   string
	InstallType string
	Executable  string
	PID         int
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	d := systemData{
		Version:     s.version,
		OS:          goruntime.GOOS,
		Arch:        goruntime.GOARCH,
		GoVersion:   goruntime.Version(),
		InstallType: s.installType,
		PID:         os.Getpid(),
	}
	if exe, err := os.Executable(); err == nil {
		d.Executable = exe
	}
	s.render(w, "system", s.newPage(r, "System", "system", d))
}
