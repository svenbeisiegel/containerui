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
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/updates"
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
	imgRaw, err := s.containerImageInspect(ctx, raw)
	if err != nil {
		return runtime.RecreateSource{}, err
	}
	src, err := runtime.OptsFromInspect(raw, imgRaw)
	if err != nil {
		return src, err
	}
	s.dropUnsupported(&src)
	return src, nil
}

// containerImageInspect returns the inspect output of the image of the
// container inspected in raw, nil if the runtime does not have it.
func (s *Server) containerImageInspect(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var c runtime.ContainerInspect
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode container inspect: %w", err)
	}
	// The image the container runs (by ID), not what its tag points to now.
	for _, ref := range []string{c.Image, c.Config.Image} {
		if runtime.ValidateImageRef(ref) != nil {
			continue
		}
		if img, err := s.rt.InspectImage(ctx, ref); err == nil && len(img) > 0 {
			return img, nil
		}
	}
	return nil, nil
}

// dropUnsupported clears the options the runtime cannot set and adds them
// to src.Lost.
func (s *Server) dropUnsupported(src *runtime.RecreateSource) {
	for _, d := range src.Opts.DropUnsupported(s.caps()) {
		src.Lost = append(src.Lost, fmt.Sprintf("%s (not supported by %s)", d, s.rt.Name()))
	}
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
		if s.updates != nil {
			// A pulled image is no longer outdated.
			s.updates.Refresh(ctx, opts.Image)
		}
		return nil
	})
	return target, nil
}

// setPullLabel records the container's pull policy p in a label, so
// recreating it later pulls the same way; runtimes without labels
// (wslc) do not record it and recreate with PullMissing.
func (s *Server) setPullLabel(opts *runtime.CreateContainerOpts, p runtime.PullPolicy) {
	if s.caps().CreateOptions[runtime.OptLabel] {
		opts.SetPullLabel(p)
	}
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
		http.Redirect(w, r, "/containers", http.StatusSeeOther)
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
		http.Redirect(w, r, "/containers", http.StatusSeeOther)
		return
	}
	f := parseContainerForm(r)
	opts, err := f.toOpts()
	if err == nil {
		s.setPullLabel(&opts, opts.Pull)
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

// handleContainerUpdate recreates a container with one of the newer
// images the update check found for it (the list's Update menu items).
func (s *Server) handleContainerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	image := strings.TrimSpace(r.PostFormValue("image"))
	src, err := s.recreateSource(r.Context(), id)
	if err == nil {
		err = managedError(src)
	}
	var cand *updates.Candidate
	if err == nil {
		if s.updates == nil {
			err = runtime.ErrUnsupported
		} else {
			for _, c := range s.updates.ForContainer(runtime.Container{ID: src.ID, Image: src.Opts.Image}) {
				if c.Ref == image {
					cand = &c
				}
			}
			if cand == nil {
				err = fmt.Errorf("%w: no newer image %s is known for container %s", runtime.ErrInvalid, image, src.Name)
			}
		}
	}
	to := "/containers"
	if err == nil {
		opts := src.Opts
		// The container keeps its pull policy; this update only pulls
		// the new image if needed (or always, if the registry has a newer one).
		s.setPullLabel(&opts, src.Opts.Pull)
		opts.Image, opts.Pull, opts.Start = image, runtime.PullMissing, src.Running
		if cand.Pull {
			opts.Pull = runtime.PullAlways
		}
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

// ---- recreate ----

// handleContainerRecreate replaces a container with one created from the
// same settings and image tag. Its pull policy decides whether the image
// is pulled first (always), only if missing, or never; with "always" a
// newer image of a tag such as latest is picked up.
func (s *Server) handleContainerRecreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, err := s.recreateSource(r.Context(), id)
	if err == nil {
		err = managedError(src)
	}
	to := "/containers"
	if err == nil {
		opts := src.Opts
		opts.Start = src.Running
		s.setPullLabel(&opts, opts.Pull)
		var target string
		if target, err = s.startRecreate(r, src, opts, "recreate"); err == nil {
			sessionFrom(r).SetFlash(fmt.Sprintf("Recreating container %s…", src.Name))
			to = "/containers/" + target
		}
	}
	if err != nil {
		s.log.Warn("container recreate refused", "id", id, "user", sessionFrom(r).Username, "error", err)
		sessionFrom(r).SetFlash(userError(err))
	}
	redirect(w, r, to)
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
