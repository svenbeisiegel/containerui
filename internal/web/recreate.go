package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Editing a container and updating its image both recreate it: the old
// container is replaced by one created from the (changed) settings read
// back from inspect. This runs as a background job ("recreate") whose
// target is the container's name, shown on the container page.

const (
	jobRecreate = "recreate"
	// recentJob is how long a finished recreate job stays on the page.
	recentJob = 10 * time.Minute
)

// recreateSource reads a container's settings for recreating it. Options
// the runtime cannot set are dropped and listed in Lost.
func (s *Server) recreateSource(ctx context.Context, id string) (runtime.RecreateSource, error) {
	if !s.caps().Inspect {
		return runtime.RecreateSource{}, runtime.ErrUnsupported
	}
	raw, err := s.rt.InspectContainer(ctx, id)
	if err != nil {
		return runtime.RecreateSource{}, err
	}
	var c runtime.ContainerInspect
	if err := json.Unmarshal(raw, &c); err != nil {
		return runtime.RecreateSource{}, fmt.Errorf("decode container inspect: %w", err)
	}
	// The image the container runs (by ID), not what its tag points to now.
	img := c.Image
	if runtime.ValidateImageRef(img) != nil {
		img = c.Config.Image
	}
	imgRaw, _ := s.rt.InspectImage(ctx, img)
	src, err := runtime.OptsFromInspect(raw, imgRaw)
	if err != nil {
		return src, err
	}
	for _, d := range src.Opts.DropUnsupported(s.caps()) {
		src.Lost = append(src.Lost, fmt.Sprintf("%s (not supported by %s)", d, s.rt.Name()))
	}
	return src, nil
}

// recreateJob returns the running or recently finished recreate job of
// the container with this name.
func (s *Server) recreateJob(name string) *jobs.Job {
	for _, j := range s.jobs.List(jobRecreate) {
		if j.Target == name && (!j.Done() || time.Since(j.Finished) < recentJob) {
			return &j
		}
	}
	return nil
}

func (s *Server) recreateBusy(names ...string) bool {
	for _, n := range names {
		if j := s.recreateJob(n); j != nil && !j.Done() {
			return true
		}
	}
	return false
}

// managedError refuses to recreate a compose container.
func managedError(src runtime.RecreateSource) error {
	if src.Project == "" {
		return nil
	}
	return fmt.Errorf("%w: container %s belongs to compose project %s; change it in the project's compose file instead", runtime.ErrInvalid, src.Name, src.Project)
}

// startRecreate starts the job that replaces src by a container created
// from opts and returns the name the new container is shown under.
func (s *Server) startRecreate(r *http.Request, src runtime.RecreateSource, opts runtime.CreateContainerOpts, what string) (string, error) {
	target := opts.Name
	if target == "" {
		target = src.Name
	}
	if s.recreateBusy(src.Name, target) {
		return "", fmt.Errorf("%w: container %s is already being recreated", runtime.ErrConflict, src.Name)
	}
	user := sessionFrom(r).Username
	s.log.Info("container recreate started", "container", src.Name, "image", opts.Image, "reason", what, "user", user)
	s.jobs.Start(jobRecreate, target, user, func(ctx context.Context, progress func(string)) error {
		id, err := runtime.Recreate(ctx, s.rt, src.ID, opts, progress)
		if err != nil {
			s.log.Warn("container recreate failed", "container", src.Name, "image", opts.Image, "user", user, "error", err)
			return errors.New(userError(err))
		}
		s.log.Info("container recreated", "container", target, "id", id, "image", opts.Image, "reason", what, "user", user)
		return nil
	})
	return target, nil
}

// ---- edit ----

func (s *Server) editView(ctx context.Context, src runtime.RecreateSource, f containerForm, err string) containerNewView {
	v := s.newContainerView(ctx, f, err)
	v.Edit = &containerEdit{ID: src.ID, Name: src.Name, Running: src.Running, AutoRemove: src.AutoRemove, Lost: src.Lost}
	return v
}

func (s *Server) handleContainerEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, err := s.recreateSource(r.Context(), id)
	if err == nil {
		err = managedError(src)
	}
	if err != nil {
		sessionFrom(r).SetFlash(userError(err))
		http.Redirect(w, r, "/containers/"+id, http.StatusSeeOther)
		return
	}
	f := formFromOpts(src.Opts)
	f.Start = src.Running
	s.render(w, "container_new", s.newPage(r, "Edit container "+src.Name, "containers", s.editView(r.Context(), src, f, "")))
}

func (s *Server) handleContainerEditSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, err := s.recreateSource(r.Context(), id)
	if err == nil {
		err = managedError(src)
	}
	if err != nil {
		sessionFrom(r).SetFlash(userError(err))
		http.Redirect(w, r, "/containers/"+id, http.StatusSeeOther)
		return
	}
	f := parseContainerForm(r)
	opts, err := f.toOpts()
	if err == nil {
		var target string
		if target, err = s.startRecreate(r, src, opts, "edit"); err == nil {
			sessionFrom(r).SetFlash(fmt.Sprintf("Recreating container %s…", target))
			http.Redirect(w, r, "/containers/"+target, http.StatusSeeOther)
			return
		}
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, "container_new", s.newPage(r, "Edit container "+src.Name, "containers", s.editView(r.Context(), src, f, userError(err))))
}

// ---- image update ----

func (s *Server) handleContainerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	image := strings.TrimSpace(r.PostFormValue("image"))
	src, err := s.recreateSource(r.Context(), id)
	if err == nil {
		err = managedError(src)
	}
	if err == nil {
		err = runtime.ValidateImageRef(image)
	}
	if err == nil {
		if repo, _ := runtime.SplitRef(image); repo != imageRepo(src.Opts.Image) {
			err = fmt.Errorf("%w: %s is not an image of %s", runtime.ErrInvalid, image, imageRepo(src.Opts.Image))
		}
	}
	to := "/containers/" + id
	if err == nil {
		opts := src.Opts
		opts.Image, opts.Pull, opts.Start = image, runtime.PullAlways, src.Running
		var target string
		if target, err = s.startRecreate(r, src, opts, "update"); err == nil {
			sessionFrom(r).SetFlash(fmt.Sprintf("Updating container %s to %s…", src.Name, image))
			to = "/containers/" + target
		}
	}
	if err != nil {
		sessionFrom(r).SetFlash(userError(err))
	}
	redirect(w, r, to)
}

// imageRepo is the repository part of an image reference.
func imageRepo(ref string) string {
	repo, _ := runtime.SplitRef(ref)
	return repo
}

type updateTag struct {
	Ref, Name, Kind string
}

type containerUpdateView struct {
	ID, Name, CSRF string
	Ref            string
	Project        string // compose project managing the container
	AutoRemove     bool
	CanRecreate    bool
	Status         registry.Status
	Tags           []updateTag
}

func (s *Server) handleContainerUpdatePartial(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	raw, err := s.rt.InspectContainer(ctx, id)
	if err != nil {
		s.renderPartial(w, "container_update", containerUpdateView{ID: id, Status: registry.Status{Err: userError(err)}})
		return
	}
	var c runtime.ContainerInspect
	_ = json.Unmarshal(raw, &c)
	v := containerUpdateView{
		ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), CSRF: sessionFrom(r).CSRF, Ref: c.Config.Image,
		Project: c.Config.Labels[runtime.LabelProject], AutoRemove: c.HostConfig.AutoRemove, CanRecreate: s.caps().Pull,
	}
	var digests []string
	if c.Image != "" {
		if imgRaw, err := s.rt.InspectImage(ctx, c.Image); err == nil {
			var img runtime.ImageInspect
			if json.Unmarshal(imgRaw, &img) == nil {
				digests = img.RepoDigests
			}
		}
	}
	v.Status = s.updates.Check(ctx, v.Ref, digests, r.URL.Query().Get("refresh") == "1")
	repo := imageRepo(v.Ref)
	for _, t := range v.Status.NewerTags {
		v.Tags = append(v.Tags, updateTag{Ref: repo + ":" + t.Name, Name: t.Name, Kind: t.Kind})
	}
	s.renderPartial(w, "container_update", v)
}

// ---- job progress ----

type recreateJobView struct {
	Name string
	Job  *jobs.Job
}

func (s *Server) handleRecreateJobPartial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	j := s.recreateJob(name)
	if j != nil && j.Done() && r.URL.Query().Get("poll") == "1" && isHTMX(r) {
		// The job just finished: reload the page to show the new container.
		w.Header().Set("HX-Refresh", "true")
	}
	s.renderPartial(w, "recreate_job", recreateJobView{Name: name, Job: j})
}
