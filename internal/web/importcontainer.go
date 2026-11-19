package web

import (
	"fmt"
	"html/template"
	"path"
	"net/http"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/compose"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Creating a container starts with choosing how: from scratch, or by
// filling the create form from pasted "docker inspect" output, a docker
// run / create command or a compose file. Nothing is created by an
// import: the filled form is shown for review and submitted to the normal
// create path.

// maxImportPaste caps the pasted text; one container's inspect output is a
// few KiB.
const maxImportPaste = 2 << 20

// importSource describes one way of filling the create form.
type importSource struct {
	Key, Title, Lead    string
	Hint                template.HTML
	Label, DropHint     string
	Placeholder, Accept string
}

var importSources = map[string]importSource{
	"inspect": {
		Key: "inspect", Title: "From docker inspect",
		Lead:        "Fill the form from an existing container, e.g. to run a compose service as a standalone container.",
		Hint:        "Paste the output of <code>docker inspect &lt;container&gt;</code> (or <code>podman inspect</code>) or drop a saved <code>.json</code> file below.",
		Label:       "docker inspect output (JSON)",
		DropHint:    "Drop the inspect output here",
		Placeholder: "[\n  {\n    \"Id\": \"3f6e2a9c1b7d…\",\n    \"Name\": \"/shop-web-1\",\n    …",
		Accept:      ".json,application/json,text/plain",
	},
	"cli": {
		Key: "cli", Title: "From a docker command",
		Lead:        "Fill the form from a docker run or docker create command, e.g. from a project's README.",
		Hint:        "Paste a <code>docker run …</code> or <code>docker create …</code> command (<code>podman</code> works too). Line continuations with <code>\\</code>, <code>`</code> or <code>^</code> are fine.",
		Label:       "docker run or docker create command",
		DropHint:    "Drop the script here",
		Placeholder: "docker run -d --name web \\\n  -p 8080:80 \\\n  -v /srv/www:/usr/share/nginx/html:ro \\\n  -e TZ=Europe/Berlin \\\n  nginx:1.27",
		Accept:      ".sh,.ps1,.cmd,.bat,.txt,text/plain",
	},
	"compose": {
		Key: "compose", Title: "From a compose file",
		Lead:        "Fill the form from one service of a compose file, run as a standalone container.",
		Hint:        "Paste a compose file or just the part that defines the service (it needs an <code>image:</code>), or drop a <code>compose.yaml</code> below.",
		Label:       "Compose file or service (YAML)",
		DropHint:    "Drop the compose file here",
		Placeholder: "services:\n  web:\n    image: nginx:1.27\n    ports:\n      - \"8080:80\"",
		Accept:      ".yml,.yaml,application/yaml,application/x-yaml,text/yaml,text/plain",
	},
}

// containerImportView is the paste page of one import source.
type containerImportView struct {
	Source importSource
	Text   string
	Err    string
	// Services to choose from (compose files with several services) and
	// the one chosen.
	Services []compose.Service
	Service  string
}

// containerImport describes what the create form was filled from.
type containerImport struct {
	Source                 string // inspect, cli or compose
	Name, Project, Service string
	Notes                  []string // what to check before creating
	Lost                   []string // settings that cannot be carried over
}

// handleContainerNew shows the choice of how to create a container; with
// ?image= (Run… on the Images page) it goes straight to the form.
func (s *Server) handleContainerNew(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("image") != "" {
		s.handleContainerScratch(w, r)
		return
	}
	s.render(w, "container_choose", s.newPage(r, "Create container", "containers", nil))
}

// handleContainerScratch shows the empty create form.
func (s *Server) handleContainerScratch(w http.ResponseWriter, r *http.Request) {
	f := defaultContainerForm()
	f.Image = r.URL.Query().Get("image")
	s.render(w, "container_new", s.newPage(r, "Create container", "containers", s.newContainerView(r.Context(), f, "")))
}

func (s *Server) handleContainerImportPage(w http.ResponseWriter, r *http.Request) {
	src, ok := importSources[path.Base(r.URL.Path)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, "container_import", s.newPage(r, "Create container", "containers", containerImportView{Source: src}))
}

func (s *Server) handleContainerImport(w http.ResponseWriter, r *http.Request) {
	src, ok := importSources[path.Base(r.URL.Path)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	text, service := r.PostFormValue("text"), r.PostFormValue("service")
	var imp runtime.ImportSource
	var choices []compose.Service
	var err error
	switch {
	case len(text) > maxImportPaste:
		err = fmt.Errorf("%w: the pasted text is larger than %d MiB", runtime.ErrInvalid, maxImportPaste>>20)
	case src.Key == "inspect":
		imp, err = s.importInspect(r, text)
	case src.Key == "cli":
		imp, err = runtime.ImportFromCLI(text)
	case src.Key == "compose":
		imp, choices, err = compose.ImportCompose([]byte(text), service)
	}
	v := containerImportView{Source: src, Text: text, Services: choices, Service: service}
	if err != nil {
		s.log.Info("container import rejected", "source", src.Key, "user", sessionFrom(r).Username, "error", err)
		v.Err = userError(err)
		if v.Err != "" {
			v.Err = strings.ToUpper(v.Err[:1]) + v.Err[1:]
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "container_import", s.newPage(r, "Create container", "containers", v))
		return
	}
	if choices != nil {
		// Several services: ask which one.
		s.render(w, "container_import", s.newPage(r, "Create container", "containers", v))
		return
	}
	s.dropUnsupported(&imp.RecreateSource)
	cv := s.newContainerView(r.Context(), formFromOpts(imp.Opts), "")
	cv.Import = &containerImport{Source: src.Key, Name: imp.Name, Project: imp.Project, Service: imp.Service, Notes: imp.Notes, Lost: imp.Lost}
	s.render(w, "container_new", s.newPage(r, "Create container", "containers", cv))
}

// importInspect validates the pasted inspect output and maps it to create
// options.
func (s *Server) importInspect(r *http.Request, text string) (runtime.ImportSource, error) {
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
	return imp, nil
}
