package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/updates"
)

// imageInfo is what the image detail page shows of an image's inspect
// document. Docker and Podman use the same field names for these.
type imageInfo struct {
	ID           string    `json:"Id"`
	RepoTags     []string  `json:"RepoTags"`
	RepoDigests  []string  `json:"RepoDigests"`
	Created      time.Time `json:"Created"`
	Size         uint64    `json:"Size"`
	Author       string    `json:"Author"`
	Architecture string    `json:"Architecture"`
	Os           string    `json:"Os"`
	Variant      string    `json:"Variant"`
	Config       struct {
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Labels       map[string]string   `json:"Labels"`
		StopSignal   string              `json:"StopSignal"`
		Healthcheck  *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	RootFS struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// Platform is os/arch[/variant], "" if unknown.
func (i imageInfo) Platform() string {
	if i.Os == "" && i.Architecture == "" {
		return ""
	}
	p := i.Os + "/" + i.Architecture
	if i.Variant != "" {
		p += "/" + i.Variant
	}
	return p
}

// Env splits the image's environment into name and value.
func (i imageInfo) Env() map[string]string {
	if len(i.Config.Env) == 0 {
		return nil
	}
	m := make(map[string]string, len(i.Config.Env))
	for _, kv := range i.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func (i imageInfo) Ports() []string   { return sortedKeys(i.Config.ExposedPorts) }
func (i imageInfo) Volumes() []string { return sortedKeys(i.Config.Volumes) }

// Healthcheck is the check's command, "" if the image has none.
func (i imageInfo) Healthcheck() string {
	h := i.Config.Healthcheck
	if h == nil || len(h.Test) == 0 || h.Test[0] == "NONE" {
		return ""
	}
	if h.Test[0] == "CMD" || h.Test[0] == "CMD-SHELL" {
		return strings.Join(h.Test[1:], " ")
	}
	return strings.Join(h.Test, " ")
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type imageDetailView struct {
	Ref   string
	Image *runtime.Image // nil if the list does not have it
	// Update is what the update check found (zero without one).
	Update  updates.Image
	Info    imageInfo
	HasInfo bool
	Users   []runtime.Container // containers created from the image
	Inspect string
	Err     string
	Caps    runtime.Capabilities
	CSRF    string
}

func (s *Server) handleImageDetail(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	list := s.loadImages(r, nil)
	v := imageDetailView{Ref: ref, Caps: list.Caps, CSRF: list.CSRF}
	for i := range list.Images {
		if list.Images[i].Ref() == ref {
			v.Image = &list.Images[i]
			break
		}
	}
	if v.Image == nil {
		// An untagged image, addressed by (a prefix of) its ID.
		id := strings.TrimPrefix(ref, "sha256:")
		for i, img := range list.Images {
			if len(id) >= 6 && strings.HasPrefix(strings.TrimPrefix(img.ID, "sha256:"), id) {
				v.Image = &list.Images[i]
				break
			}
		}
	}
	if v.Image != nil {
		v.Update = list.Updates[v.Image.Ref()]
		if cs, err := s.rt.ListContainers(r.Context(), runtime.ListContainersOpts{All: true}); err == nil {
			v.Users = imageUsers(*v.Image, cs)
		}
	}
	if raw, err := s.rt.InspectImage(r.Context(), ref); err != nil {
		if v.Image != nil || !errors.Is(err, runtime.ErrNotFound) {
			v.Err = userError(err)
		}
	} else {
		v.Inspect = prettyJSON(raw)
		v.HasInfo = json.Unmarshal(raw, &v.Info) == nil
	}
	if v.Image == nil && v.Inspect == "" {
		w.WriteHeader(http.StatusNotFound)
		if v.Err == "" {
			v.Err = "Image " + ref + " not found."
		}
	}
	s.render(w, "image_detail", s.newPage(r, "Image "+ref, "images", v))
}

// imageDetailURL is the detail page of an image reference.
func imageDetailURL(ref string) string {
	return "/images/" + (&url.URL{Path: ref}).EscapedPath()
}

// imageUsers lists the containers created from img, matched as
// markImageUse counts them.
func imageUsers(img runtime.Image, cs []runtime.Container) []runtime.Container {
	var out []runtime.Container
	for _, c := range cs {
		if containerUsesImage(c, img) {
			out = append(out, c)
		}
	}
	return out
}

// containerUsesImage reports whether c was created from img: by image ID
// (its tag has moved on), by repository and digest, or by reference.
func containerUsesImage(c runtime.Container, img runtime.Image) bool {
	r, err := registry.ParseRef(c.Image)
	switch {
	case errors.Is(err, registry.ErrNotRegistry):
		id := strings.TrimPrefix(c.Image, "sha256:")
		return strings.HasPrefix(strings.TrimPrefix(img.ID, "sha256:"), id)
	case err != nil:
		return false
	case r.Digest != "":
		if img.Digest != r.Digest {
			return false
		}
		ir, err := registry.ParseRef(img.Repository)
		return err == nil && ir.Name() == r.Name()
	}
	if img.Repository == "" || img.Repository == "<none>" || img.Tag == "" || img.Tag == "<none>" {
		return false
	}
	ir, err := registry.ParseRef(img.Ref())
	return err == nil && ir.String() == r.String()
}
