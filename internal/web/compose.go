package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/compose"
	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Compose pages exist only when the runtime found a native compose tool
// (docker compose, podman compose, docker-compose, podman-compose) and a
// folder for the deployed compose files is configured.

// reservedProjects are path segments of the compose routes.
var reservedProjects = []string{"new", "validate"}

const composeJobKind = "compose"

// composer returns the runtime's compose support, or false if there is none.
func (s *Server) composer() (runtime.Composer, bool) {
	c, ok := s.rt.(runtime.Composer)
	if !ok || !s.rt.Capabilities().Compose || s.store.Dir == "" {
		return nil, false
	}
	return c, true
}

// caps returns the runtime's capabilities as the UI offers them: compose
// also needs a folder for the compose files.
func (s *Server) caps() runtime.Capabilities {
	c := s.rt.Capabilities()
	if c.Compose && s.store.Dir == "" {
		c.Compose = false
	}
	return c
}

// requireCompose answers 404 when compose is unavailable.
func (s *Server) requireCompose(w http.ResponseWriter, r *http.Request) (runtime.Composer, bool) {
	c, ok := s.composer()
	if !ok {
		http.NotFound(w, r)
	}
	return c, ok
}

// loadProjects groups the runtime's labelled objects and the stored files
// into projects. Only a failing container list is an error.
func (s *Server) loadProjects(ctx context.Context) ([]compose.Project, error) {
	caps := s.caps()
	cs, err := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	if err != nil {
		return nil, err
	}
	var vs []runtime.Volume
	if caps.Volumes {
		if vs, err = s.rt.ListVolumes(ctx); err != nil {
			s.log.Debug("compose: list volumes", "error", err)
		}
	}
	var ns []runtime.Network
	if caps.Networks {
		if ns, err = s.rt.ListNetworks(ctx); err != nil {
			s.log.Debug("compose: list networks", "error", err)
		}
	}
	stored, err := s.store.List()
	if err != nil {
		s.log.Warn("compose: list stored files", "dir", s.store.Dir, "error", err)
	}
	return compose.Group(cs, vs, ns, stored), nil
}

// ---- list ----

type composeListView struct {
	Projects []compose.Project
	Err      string
	Flash    *flash
	CSRF     string
}

type composePage struct {
	List composeListView
	Jobs composeJobsView
}

func (s *Server) loadComposeList(r *http.Request, f *flash) composeListView {
	v := composeListView{Flash: f, CSRF: sessionFrom(r).CSRF}
	ps, err := s.loadProjects(r.Context())
	if err != nil {
		v.Err = userError(err)
	}
	v.Projects = ps
	return v
}

func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	data := composePage{List: s.loadComposeList(r, nil), Jobs: s.loadComposeJobs("")}
	s.render(w, "compose", s.newPage(r, "Compose", "compose", data))
}

func (s *Server) handleComposePartial(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	s.renderPartial(w, "compose_table", s.loadComposeList(r, nil))
}

// ---- jobs ----

type composeJobsView struct {
	Project    string // "" = all projects
	Jobs       []jobs.Job
	AnyRunning bool
}

// Compose jobs have the target "<project> <action>".
func jobProject(j jobs.Job) string {
	p, _, _ := strings.Cut(j.Target, " ")
	return p
}

func (s *Server) loadComposeJobs(project string) composeJobsView {
	v := composeJobsView{Project: project}
	for _, j := range s.jobs.List(composeJobKind) {
		if project != "" && jobProject(j) != project {
			continue
		}
		if !j.Done() {
			v.AnyRunning = true
		}
		if len(v.Jobs) < 5 {
			v.Jobs = append(v.Jobs, j)
		}
	}
	return v
}

func (s *Server) handleComposeJobsPartial(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	project := r.URL.Query().Get("project")
	if project != "" && runtime.ValidateProjectName(project) != nil {
		http.NotFound(w, r)
		return
	}
	v := s.loadComposeJobs(project)
	if !v.AnyRunning && isHTMX(r) {
		// The polling panel stops after this response; let the project
		// views reload once to show the result.
		w.Header().Set("HX-Trigger", "compose-changed")
	}
	s.renderPartial(w, "compose_jobs", v)
}

// composeBusy reports whether a job is running for the project.
func (s *Server) composeBusy(project string) bool {
	for _, j := range s.jobs.List(composeJobKind) {
		if jobProject(j) == project && !j.Done() {
			return true
		}
	}
	return false
}

// startComposeJob runs fn as a background job unless one is already
// running for the project.
func (s *Server) startComposeJob(r *http.Request, project, action string, fn func(ctx context.Context, progress func(string)) error) error {
	if s.composeBusy(project) {
		return fmt.Errorf("an operation is already running for project %s; wait for it to finish", project)
	}
	user := sessionFrom(r).Username
	s.log.Info("compose job started", "project", project, "action", action, "user", user)
	s.jobs.Start(composeJobKind, project+" "+action, user, func(ctx context.Context, progress func(string)) error {
		if err := fn(ctx, progress); err != nil {
			s.log.Warn("compose job failed", "project", project, "action", action, "user", user, "error", err)
			return errors.New(userError(err))
		}
		s.log.Info("compose job finished", "project", project, "action", action, "user", user)
		return nil
	})
	return nil
}

// ---- create / edit ----

type composeCheck struct {
	Empty     bool
	Problems  []compose.Problem
	Services  []string
	Native    bool   // the compose tool checked the file
	NativeErr string // its complaint
}

type composeForm struct {
	Edit     bool   // updating an existing project
	Project  string // project name
	YAML     string
	Source   string // where an edited file came from
	Err      string
	Check    composeCheck
	Deployed bool
}

func (s *Server) handleComposeNew(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	f := composeForm{Check: composeCheck{Empty: true}}
	s.render(w, "compose_new", s.newPage(r, "Create Compose", "compose", f))
}

// readComposeForm reads the project name and file of a create/edit form.
func readComposeForm(r *http.Request) (project, src string) {
	src = strings.ReplaceAll(r.PostFormValue("yaml"), "\r\n", "\n")
	return strings.TrimSpace(r.PostFormValue("project")), src
}

// checkCompose runs our own checks and fills in the project name from the
// file's name: if the form left it empty.
func checkCompose(project *string, src string, edit bool) composeCheck {
	if strings.TrimSpace(src) == "" {
		return composeCheck{Empty: true}
	}
	file, problems := compose.Parse([]byte(src))
	c := composeCheck{Problems: problems}
	for _, svc := range file.Services {
		c.Services = append(c.Services, svc.Name)
	}
	switch {
	case *project == "" && file.Name != "":
		*project = file.Name
	case file.Name != "" && file.Name != *project:
		msg := fmt.Sprintf("name: %s is ignored; the project is deployed as %s", file.Name, *project)
		if *project == "" || edit {
			msg = fmt.Sprintf("name: %s is ignored; this project is %s", file.Name, *project)
		}
		c.Problems = append(c.Problems, compose.Problem{Msg: msg, Warning: true})
	}
	return c
}

func validateNewProject(project string, existing []compose.Project) error {
	if project == "" {
		return fmt.Errorf("%w: enter a project name, or set name: in the file", runtime.ErrInvalid)
	}
	if err := runtime.ValidateProjectName(project); err != nil {
		return err
	}
	if slices.Contains(reservedProjects, project) {
		return fmt.Errorf("%w: %q is reserved; choose another project name", runtime.ErrInvalid, project)
	}
	if _, ok := compose.Find(existing, project); ok {
		return fmt.Errorf("project %s already exists; edit it instead", project)
	}
	return nil
}

// nativeCheck lets the compose tool validate src as the project's file.
func (s *Server) nativeCheck(ctx context.Context, c runtime.Composer, project, src string) error {
	tmp, cleanup, err := s.store.WriteTemp(project, []byte(src))
	if err != nil {
		return err
	}
	defer cleanup()
	return c.ComposeValidate(ctx, runtime.ComposeTarget{Project: project, File: tmp})
}

// handleComposeValidate answers the editor's live check (and the "Check"
// button, which adds the compose tool's own validation).
func (s *Server) handleComposeValidate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCompose(w, r)
	if !ok {
		return
	}
	project, src := readComposeForm(r)
	edit := r.PostFormValue("edit") == "true"
	check := checkCompose(&project, src, edit)
	if r.PostFormValue("native") != "" && !check.Empty && !compose.HasErrors(check.Problems) {
		check.Native = true
		name := project
		if runtime.ValidateProjectName(name) != nil {
			name = "containerui-check"
		}
		if err := s.nativeCheck(r.Context(), c, name, src); err != nil {
			check.NativeErr = userError(err)
		}
	}
	s.renderPartial(w, "compose_problems", check)
}

// handleComposeCreate deploys a new project.
func (s *Server) handleComposeCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCompose(w, r)
	if !ok {
		return
	}
	project, src := readComposeForm(r)
	f := composeForm{YAML: src}
	f.Check = checkCompose(&project, src, false)
	f.Project = project
	fail := func(err error) {
		if err != nil {
			f.Err = userError(err)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "compose_new", s.newPage(r, "Create Compose", "compose", f))
	}
	existing, err := s.loadProjects(r.Context())
	if err != nil {
		fail(err)
		return
	}
	if err := validateNewProject(project, existing); err != nil {
		fail(err)
		return
	}
	s.deployCompose(w, r, c, f, fail)
}

// handleComposeEdit shows a project's compose file for editing: the stored
// file, else the file it was deployed from (if readable here), else one
// rebuilt from its containers.
func (s *Server) handleComposeEdit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	p, ok := s.findProject(w, r)
	if !ok {
		return
	}
	f := composeForm{Edit: true, Project: p.Name, Deployed: p.Deployed()}
	if b, err := s.store.Read(p.Name); err == nil {
		f.YAML = string(b)
	} else if path := readableConfigFile(p); path != "" {
		b, _ := os.ReadFile(path)
		f.YAML = string(b)
		f.Source = "Loaded from " + path + ". Applying stores a copy in ContainerUI and deploys the project from there."
	} else {
		src, err := s.regenerate(r.Context(), p)
		if err != nil {
			f.Err = "The compose file could not be rebuilt: " + userError(err)
		}
		f.YAML = string(src)
		f.Source = "The original compose file is not available, so this one was rebuilt from the running containers. Review it before applying: " +
			"build contexts, env_file, profiles and variables are not recorded by the runtime."
	}
	f.Check = checkCompose(&f.Project, f.YAML, true)
	s.render(w, "compose_new", s.newPage(r, "Edit "+p.Name, "compose", f))
}

// handleComposeUpdate stores the edited file and applies it.
func (s *Server) handleComposeUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCompose(w, r)
	if !ok {
		return
	}
	p, ok := s.findProject(w, r)
	if !ok {
		return
	}
	_, src := readComposeForm(r)
	f := composeForm{Edit: true, Project: p.Name, YAML: src, Deployed: p.Deployed()}
	f.Check = checkCompose(&f.Project, src, true)
	fail := func(err error) {
		if err != nil {
			f.Err = userError(err)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "compose_new", s.newPage(r, "Edit "+p.Name, "compose", f))
	}
	s.deployCompose(w, r, c, f, fail)
}

// deployCompose checks f, stores the file and starts "compose up".
func (s *Server) deployCompose(w http.ResponseWriter, r *http.Request, c runtime.Composer, f composeForm, fail func(error)) {
	if f.Check.Empty {
		fail(fmt.Errorf("%w: the compose file is empty", runtime.ErrInvalid))
		return
	}
	if compose.HasErrors(f.Check.Problems) {
		fail(fmt.Errorf("%w: fix the problems below first", runtime.ErrInvalid))
		return
	}
	if err := s.nativeCheck(r.Context(), c, f.Project, f.YAML); err != nil {
		f.Check.Native, f.Check.NativeErr = true, userError(err)
		fail(errors.New("The compose tool rejected the file."))
		return
	}
	if s.composeBusy(f.Project) {
		fail(fmt.Errorf("an operation is already running for project %s; wait for it to finish", f.Project))
		return
	}
	path, err := s.store.Write(f.Project, []byte(f.YAML))
	if err != nil {
		fail(fmt.Errorf("store the compose file: %w", err))
		return
	}
	target := runtime.ComposeTarget{Project: f.Project, File: path}
	if err := s.startComposeJob(r, f.Project, "up", func(ctx context.Context, progress func(string)) error {
		return c.ComposeUp(ctx, target, progress)
	}); err != nil {
		fail(err)
		return
	}
	verb := "Deploying"
	if f.Edit {
		verb = "Applying changes to"
	}
	sessionFrom(r).SetFlash(fmt.Sprintf("%s project %s…", verb, f.Project))
	redirect(w, r, "/compose/"+f.Project)
}

// regenerate rebuilds a compose file from the project's containers.
func (s *Server) regenerate(ctx context.Context, p compose.Project) ([]byte, error) {
	var containers []json.RawMessage
	images := map[string]json.RawMessage{}
	for _, svc := range p.Services {
		for _, c := range svc.Containers {
			raw, err := s.rt.InspectContainer(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			containers = append(containers, raw)
			if _, seen := images[c.Image]; !seen && c.Image != "" {
				// Without the image, inherited values are kept in the file.
				if img, err := s.rt.InspectImage(ctx, c.Image); err == nil {
					images[c.Image] = img
				}
			}
		}
	}
	return compose.Generate(p.Name, containers, images)
}

// readableConfigFile returns the project's compose file from its labels if
// it is a single absolute path readable on this host.
func readableConfigFile(p compose.Project) string {
	if len(p.ConfigFiles) != 1 || !filepath.IsAbs(p.ConfigFiles[0]) {
		return ""
	}
	if st, err := os.Stat(p.ConfigFiles[0]); err != nil || !st.Mode().IsRegular() || st.Size() > compose.MaxFileSize {
		return ""
	}
	return p.ConfigFiles[0]
}

// ---- detail ----

type composeDetailView struct {
	Project compose.Project
	Source  string // stored, file path or ""
	Caps    runtime.Capabilities
	CSRF    string
	Err     string
}

// findProject loads the project named in the path, answering 404 if there
// is none.
func (s *Server) findProject(w http.ResponseWriter, r *http.Request) (compose.Project, bool) {
	name := r.PathValue("project")
	if runtime.ValidateProjectName(name) != nil {
		http.NotFound(w, r)
		return compose.Project{}, false
	}
	ps, err := s.loadProjects(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		s.render(w, "compose_detail", s.newPage(r, name, "compose", composeDetailPage{Detail: composeDetailView{Project: compose.Project{Name: name}, Err: userError(err)}}))
		return compose.Project{}, false
	}
	p, ok := compose.Find(ps, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "compose_detail", s.newPage(r, name, "compose", composeDetailPage{Detail: composeDetailView{Project: compose.Project{Name: name}, Err: "Project " + name + " not found."}}))
		return compose.Project{}, false
	}
	return p, true
}

func (s *Server) composeDetail(r *http.Request, p compose.Project) composeDetailView {
	v := composeDetailView{Project: p, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	if p.Stored {
		v.Source = "stored"
	} else if len(p.ConfigFiles) > 0 {
		v.Source = strings.Join(p.ConfigFiles, ", ")
	}
	return v
}

type composeDetailPage struct {
	Detail composeDetailView
	Jobs   composeJobsView
}

func (s *Server) handleComposeDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	p, ok := s.findProject(w, r)
	if !ok {
		return
	}
	data := composeDetailPage{Detail: s.composeDetail(r, p), Jobs: s.loadComposeJobs(p.Name)}
	s.render(w, "compose_detail", s.newPage(r, p.Name, "compose", data))
}

func (s *Server) handleComposeProjectPartial(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCompose(w, r); !ok {
		return
	}
	name := r.PathValue("project")
	if runtime.ValidateProjectName(name) != nil {
		http.NotFound(w, r)
		return
	}
	v := composeDetailView{Project: compose.Project{Name: name}, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	ps, err := s.loadProjects(r.Context())
	if err != nil {
		v.Err = userError(err)
	} else if p, ok := compose.Find(ps, name); ok {
		v = s.composeDetail(r, p)
	} else {
		v.Err = "Project " + name + " no longer exists."
	}
	s.renderPartial(w, "compose_project", v)
}

// ---- actions ----

// target returns how to address the project: its stored file, else its
// own compose file if readable here, else the name alone.
func (s *Server) target(p compose.Project) runtime.ComposeTarget {
	t := runtime.ComposeTarget{Project: p.Name}
	if p.Stored {
		if path, err := s.store.Path(p.Name); err == nil {
			t.File = path
			return t
		}
	}
	t.File = readableConfigFile(p)
	return t
}

func (s *Server) handleComposeAction(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCompose(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if !slices.Contains(append(slices.Clone(runtime.ComposeActions), "up", "down", "delete"), action) {
		http.NotFound(w, r)
		return
	}
	p, ok := s.findProject(w, r)
	if !ok {
		return
	}
	t := s.target(p)
	var f *flash
	var err error
	switch action {
	case "delete":
		if err = s.store.Delete(p.Name); errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: no compose file is stored for project %s", runtime.ErrNotFound, p.Name)
		}
		if err == nil {
			s.log.Info("compose file deleted", "project", p.Name, "user", sessionFrom(r).Username)
			f = okFlash("The compose file of project %s was deleted.", p.Name)
		}
	case "up":
		if !p.Stored {
			err = fmt.Errorf("%w: no compose file is stored for project %s; edit it first", runtime.ErrInvalid, p.Name)
			break
		}
		err = s.startComposeJob(r, p.Name, action, func(ctx context.Context, progress func(string)) error {
			return c.ComposeUp(ctx, t, progress)
		})
		f = okFlash("Deploying project %s…", p.Name)
	case "down":
		volumes := r.PostFormValue("volumes") == "true"
		err = s.startComposeJob(r, p.Name, action, func(ctx context.Context, progress func(string)) error {
			return c.ComposeDown(ctx, t, volumes, progress)
		})
		f = okFlash("Taking project %s down…", p.Name)
	default:
		if action == "pull" && t.File == "" {
			err = fmt.Errorf("%w: pulling needs the compose file; edit the project first", runtime.ErrInvalid)
			break
		}
		err = s.startComposeJob(r, p.Name, action, func(ctx context.Context, progress func(string)) error {
			return c.ComposeAction(ctx, t, action, progress)
		})
		f = okFlash("%s project %s…", map[string]string{"start": "Starting", "stop": "Stopping", "restart": "Restarting", "pull": "Pulling images of"}[action], p.Name)
	}
	if err != nil {
		f = errFlash(err)
	}
	if action != "delete" && err == nil {
		w.Header().Set("HX-Trigger", "compose-job-started")
	}

	if !isHTMX(r) || r.PostFormValue("view") == "detail" {
		sessionFrom(r).SetFlash(f.Msg)
		to := "/compose/" + p.Name
		if r.PostFormValue("view") != "detail" || (action == "delete" && err == nil && !p.Deployed()) {
			to = "/compose"
		}
		redirect(w, r, to)
		return
	}
	s.renderPartial(w, "compose_table", s.loadComposeList(r, f))
}

// composeReturnRe matches the pages a container action may return to.
var composeReturnRe = regexp.MustCompile(`^/compose/[a-z0-9][a-z0-9_-]*$`)
