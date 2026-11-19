// Package updates finds local images that have a newer version in their
// registry, and the containers that run an outdated image.
//
// A Scanner checks every tagged local image in the background (every
// interval, or right away on request) and keeps the result. Pages read it
// without asking a registry: the Images page offers to pull the newer
// image, the container list marks outdated containers, and the container
// page offers to recreate one with the newer image.
package updates

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const (
	// scanTimeout bounds one scan of all images.
	scanTimeout = 15 * time.Minute
	// parallel registry checks per scan.
	parallel = 4
)

// Image is what the last check found for a local image reference.
type Image struct {
	Ref    string // as the runtime lists it, e.g. nginx:1.27
	ID     string // local image ID
	Status registry.Status
	// NewerLocal is a newer version of the image that is already local
	// (one of Status.NewerTags); then this is not the latest local image.
	NewerLocal string
}

// DigestChanged: the registry serves a different image for the tag.
func (i Image) DigestChanged() bool { return i.Status.DigestChanged }

// NewerTags are newer versions in the registry, offered only for the
// latest local version of a repository.
func (i Image) NewerTags() []registry.Tag {
	if i.NewerLocal != "" {
		return nil
	}
	return i.Status.NewerTags
}

// Available reports whether pulling would bring a newer image.
func (i Image) Available() bool { return i.DigestChanged() || len(i.NewerTags()) > 0 }

// Repo is the image's repository as the runtime lists it.
func (i Image) Repo() string {
	repo, _ := runtime.SplitRef(i.Ref)
	return repo
}

// Candidate is an image a container can be updated to.
type Candidate struct {
	Ref   string
	Label string
	// Pull: the registry has a newer image for Ref than the local one
	// (or there is none yet), so pull it before recreating.
	Pull bool
}

// State describes the scanner itself.
type State struct {
	Scanning bool
	Last     time.Time // when the last scan finished
	Err      string    // why the last scan failed
	Interval time.Duration
}

type containerImage struct {
	ref, imageID string
}

// Scanner checks local images for updates and keeps the results.
type Scanner struct {
	rt       runtime.Runtime
	checker  registry.UpdateChecker
	interval time.Duration
	log      *slog.Logger

	mu         sync.Mutex
	ctx        context.Context // of Run, for scans started by CheckNow
	images     map[string]*Image
	containers map[string]containerImage // by container ID, at the last scan
	scanning   bool
	last       time.Time
	lastErr    string
	wg         sync.WaitGroup
}

func New(rt runtime.Runtime, checker registry.UpdateChecker, interval time.Duration, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{rt: rt, checker: checker, interval: interval, log: log, ctx: context.Background(),
		images: map[string]*Image{}, containers: map[string]containerImage{}}
}

// Run scans right away and then every interval until ctx ends.
func (s *Scanner) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.Scan(ctx, false)
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
			s.Scan(ctx, true)
		}
	}
}

// CheckNow starts a scan that asks every registry again, unless one is
// running. It reports whether it started one.
func (s *Scanner) CheckNow() bool {
	run, ok := s.BeginCheck()
	if !ok {
		return false
	}
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = run(ctx)
	}()
	return true
}

// BeginCheck reserves a scan that asks every registry again, unless one
// is running: State reports it as running from now on. The caller must
// call run, which scans and returns why the scan failed.
func (s *Scanner) BeginCheck() (run func(ctx context.Context) error, ok bool) {
	if !s.begin() {
		return nil, false
	}
	return func(ctx context.Context) error { return s.scan(ctx, true) }, true
}

// Wait waits for scans started by CheckNow.
func (s *Scanner) Wait() { s.wg.Wait() }

// Scan checks all local images; force skips the registry answers cached
// since the last scan. It does nothing while another scan runs.
func (s *Scanner) Scan(ctx context.Context, force bool) {
	if s.begin() {
		_ = s.scan(ctx, force)
	}
}

func (s *Scanner) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanning {
		return false
	}
	s.scanning = true
	return true
}

func (s *Scanner) scan(ctx context.Context, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	started := time.Now()
	images, containers, err := s.collect(ctx, force)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanning = false
	if err != nil {
		s.lastErr = err.Error()
		s.log.Warn("image update check failed", "error", err)
		return err
	}
	s.images, s.containers, s.last, s.lastErr = images, containers, time.Now(), ""
	s.markNewerLocal()
	n := 0
	for _, img := range s.images {
		if img.Available() {
			n++
		}
	}
	s.log.Info("image update check finished", "images", len(s.images), "updates", n, "duration", time.Since(started).Round(time.Millisecond))
	return nil
}

func (s *Scanner) collect(ctx context.Context, force bool) (map[string]*Image, map[string]containerImage, error) {
	list, err := s.rt.ListImages(ctx)
	if err != nil {
		return nil, nil, err
	}
	var refs []string
	seen := map[string]bool{}
	for _, img := range list {
		if untagged(img.Repository) || untagged(img.Tag) {
			continue
		}
		ref := img.Ref()
		if !checkable(ref) || seen[key(ref)] {
			continue
		}
		seen[key(ref)] = true
		refs = append(refs, ref)
	}

	images := map[string]*Image{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if img := s.check(ctx, ref, force); img != nil {
				mu.Lock()
				images[key(ref)] = img
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// The image each container runs: after a newer image was pulled for
	// its tag, the container still runs the old one.
	containers := map[string]containerImage{}
	cs, err := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cs {
		raw, err := s.rt.InspectContainer(ctx, c.ID)
		if err != nil {
			continue
		}
		var ci runtime.ContainerInspect
		if json.Unmarshal(raw, &ci) == nil && ci.Config.Image != "" {
			containers[c.ID] = containerImage{ref: ci.Config.Image, imageID: normID(ci.Image)}
		}
	}
	return images, containers, ctx.Err()
}

// check asks the registry about one local image; nil if it is gone.
func (s *Scanner) check(ctx context.Context, ref string, force bool) *Image {
	raw, err := s.rt.InspectImage(ctx, ref)
	if err != nil {
		return nil
	}
	var ii runtime.ImageInspect
	_ = json.Unmarshal(raw, &ii)
	return &Image{Ref: ref, ID: normID(ii.ID), Status: s.checker.Check(ctx, ref, ii.RepoDigests, force)}
}

// Refresh checks these references again after they were pulled, tagged
// or removed, reusing the registry answers of the last scan.
func (s *Scanner) Refresh(ctx context.Context, refs ...string) {
	for _, ref := range refs {
		if !checkable(ref) {
			continue
		}
		img := s.check(ctx, ref, false)
		s.mu.Lock()
		if img == nil {
			delete(s.images, key(ref))
		} else {
			s.images[key(ref)] = img
		}
		s.markNewerLocal()
		s.mu.Unlock()
	}
}

// markNewerLocal notes for each image the newest local newer version.
// Callers hold s.mu.
func (s *Scanner) markNewerLocal() {
	local := s.localTags()
	for _, img := range s.images {
		img.NewerLocal = ""
		if newer := local.newer(img.Ref); len(newer) > 0 {
			img.NewerLocal = newer[0].Name
		}
	}
}

// localTags indexes the tags of the local images by repository.
// Callers hold s.mu.
func (s *Scanner) localTags() localTags {
	l := localTags{}
	for _, img := range s.images {
		if r, err := registry.ParseRef(img.Ref); err == nil {
			l[r.Name()] = append(l[r.Name()], r.Tag)
		}
	}
	return l
}

// localTags are the tags of local images by repository name.
type localTags map[string][]string

// newer lists the local versions of ref's repository that are newer than
// its tag, newest first: they need no registry to be found.
func (l localTags) newer(ref string) []registry.Tag {
	r, err := registry.ParseRef(ref)
	if err != nil {
		return nil
	}
	return registry.NewerTags(r.Tag, l[r.Name()])
}

// State reports whether a scan runs and how the last one went.
func (s *Scanner) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{Scanning: s.scanning, Last: s.last, Err: s.lastErr, Interval: s.interval}
}

// Updates lists the local images that have a newer image, by reference.
func (s *Scanner) Updates() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Image
	for _, img := range s.images {
		if img.Available() {
			out = append(out, *img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// Image returns what is known about a local image reference.
func (s *Scanner) Image(ref string) (Image, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, ok := s.images[key(ref)]
	if !ok {
		return Image{}, false
	}
	return *img, true
}

// ForContainer lists the images container c can be updated to: its own
// tag when that has a newer image (in the registry, or already pulled),
// and newer versions, newest first: those the registry has and those
// already pulled, which are offered even when the registry cannot be
// asked or no longer has the container's tag.
func (s *Scanner) ForContainer(c runtime.Container) []Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, imageID := c.Image, ""
	if ci, ok := s.containers[c.ID]; ok {
		ref, imageID = ci.ref, ci.imageID
	}
	if !checkable(ref) {
		return nil
	}
	repo, tag := runtime.SplitRef(ref)
	if tag == "" {
		tag = "latest"
	}
	var out []Candidate
	// The container's image may have lost its tag locally (removed or
	// retagged): newer local versions still count.
	img, ok := s.images[key(ref)]
	if ok {
		switch {
		case img.Status.DigestChanged:
			out = append(out, Candidate{Ref: ref, Label: tag + " (rebuilt in the registry)", Pull: true})
		case imageID != "" && img.ID != "" && imageID != img.ID:
			out = append(out, Candidate{Ref: ref, Label: tag + " (newer image already pulled)"})
		}
	}
	var names []string
	if ok {
		for _, t := range img.Status.NewerTags {
			names = append(names, t.Name)
		}
	}
	for _, t := range s.localTags().newer(ref) {
		names = append(names, t.Name)
	}
	for _, t := range registry.NewerTags(tag, names) {
		r := repo + ":" + t.Name
		local, ok := s.images[key(r)]
		out = append(out, Candidate{Ref: r, Label: t.Name + " (" + t.Kind + ")", Pull: !ok || local.Status.DigestChanged})
	}
	return out
}

// checkable: a tagged reference of a registry image.
func checkable(ref string) bool {
	r, err := registry.ParseRef(ref)
	return err == nil && r.Digest == "" && !strings.Contains(ref, "<none>")
}

// key normalizes a reference: "nginx" and "docker.io/library/nginx:latest"
// are the same image.
func key(ref string) string {
	if r, err := registry.ParseRef(ref); err == nil {
		return r.String()
	}
	return ref
}

func untagged(s string) bool { return s == "" || s == "<none>" }

func normID(id string) string { return strings.TrimPrefix(id, "sha256:") }
