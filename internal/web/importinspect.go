package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Filling the create form from pasted "docker inspect" output, e.g. to turn
// a compose container into a standalone one. Nothing is created here: the
// filled form is shown for review and submitted to the normal create path.

// maxInspectPaste caps the pasted inspect output; one container's is a few KiB.
const maxInspectPaste = 2 << 20

// containerImport describes the container the create form was filled from.
type containerImport struct {
	Name, Project, Service string
	Notes                  []string // what to check before creating
	Lost                   []string // settings that cannot be carried over
}

func (s *Server) handleContainerImport(w http.ResponseWriter, r *http.Request) {
	text := r.PostFormValue("inspect")
	imp, err := s.importInspect(r, text)
	if err != nil {
		s.log.Info("container inspect import rejected", "user", sessionFrom(r).Username, "error", err)
		v := s.newContainerView(r.Context(), defaultContainerForm(), "")
		msg := userError(err)
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		v.ImportText, v.ImportErr = text, msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "container_new", s.newPage(r, "Create container", "containers", v))
		return
	}
	f := formFromOpts(imp.Opts)
	f.Start = imp.Opts.Start
	v := s.newContainerView(r.Context(), f, "")
	v.Import = &containerImport{Name: imp.Name, Project: imp.Project, Service: imp.Service, Notes: imp.Notes, Lost: imp.Lost}
	s.render(w, "container_new", s.newPage(r, "Create container", "containers", v))
}

// importInspect validates the pasted inspect output and maps it to create
// options the runtime supports.
func (s *Server) importInspect(r *http.Request, text string) (runtime.ImportSource, error) {
	if len(text) > maxInspectPaste {
		return runtime.ImportSource{}, fmt.Errorf("%w: the inspect output is larger than %d MiB", runtime.ErrInvalid, maxInspectPaste>>20)
	}
	doc, err := runtime.ParseContainerInspect([]byte(text))
	if err != nil {
		return runtime.ImportSource{}, err
	}
	// The output may come from another host: a missing image is no error.
	img, _ := s.containerImageInspect(r.Context(), doc)
	imp, err := runtime.ImportFromInspect(doc, img)
	if err != nil {
		return imp, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	s.dropUnsupported(&imp.RecreateSource)
	return imp, nil
}
