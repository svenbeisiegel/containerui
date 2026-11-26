package web

import (
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestMarkImageUse(t *testing.T) {
	imgs := func() []runtime.Image {
		return []runtime.Image{
			{ID: "sha256:aaa111aaa111aaa1", Repository: "ghcr.io/x/openkca", Tag: "0.13.0", Containers: 1},
			{ID: "sha256:aaa111aaa111aaa1", Repository: "ghcr.io/x/openkca", Tag: "latest", Containers: 1},
			{ID: "sha256:bbb222bbb222bbb2", Repository: "vaultwarden/server", Tag: "1.37.5-alpine", Containers: 1},
			{ID: "sha256:ccc333ccc333ccc3", Repository: "nginx", Tag: "latest", Containers: 1},
			{ID: "sha256:ddd444ddd444ddd4", Repository: "alpine", Tag: "3", Containers: 2},
			{ID: "sha256:ddd444ddd444ddd4", Repository: "alpine", Tag: "latest", Containers: 2},
			{ID: "sha256:eee555eee555eee5", Repository: "<none>", Tag: "<none>", Containers: 1},
			{ID: "sha256:fff666fff666fff6", Repository: "redis", Tag: "7", Digest: "sha256:0123456789abcdef0123456789abcdef", Containers: 1},
			{ID: "sha256:fff666fff666fff6", Repository: "redis", Tag: "latest", Digest: "sha256:0123456789abcdef0123456789abcdef", Containers: 1},
		}
	}
	cs := []runtime.Container{
		{Name: "openkca", Image: "ghcr.io/x/openkca:0.13.0"},
		{Name: "vaultwarden", Image: "docker.io/vaultwarden/server:1.37.5-alpine"},
		{Name: "web", Image: "nginx"},
		{Name: "old", Image: "eee555eee555"},
		{Name: "cache", Image: "redis@sha256:0123456789abcdef0123456789abcdef"},
		// alpine: the engine counts 2, but no container names it (the
		// fallback keeps both rows used).
	}
	got := imgs()
	markImageUse(got, cs)
	want := []int{1, 0, 1, 1, 2, 2, 1, 1, 1}
	for i, img := range got {
		if img.InUse != want[i] {
			t.Errorf("%s (%s): InUse = %d, want %d", img.Ref(), img.ID, img.InUse, want[i])
		}
	}

	// Without a container list, the engine's count stands.
	got = imgs()
	markImageUse(got, nil)
	for _, img := range got {
		if img.InUse != img.Containers {
			t.Errorf("no containers: %s InUse = %d, want %d", img.Ref(), img.InUse, img.Containers)
		}
	}
}

// Two tags of one image: only the tag the container runs is marked.
func TestImagesInUseOnlyUsedTag(t *testing.T) {
	e := newEnv(t)
	e.login()
	// nginx:1.27 (sha256:bbb222) runs as "web"; latest is the same image.
	e.rt.AddImage(runtime.Image{ID: "sha256:bbb222", Repository: "nginx", Tag: "latest", Size: "190MB"})
	page := e.do("GET", "/images", nil).Body.String()
	row := func(ref string) string {
		for _, r := range strings.Split(page, "<tr>") {
			if strings.Contains(r, `name="ref" value="`+ref+`"`) {
				return r
			}
		}
		t.Fatalf("no row for %s:\n%s", ref, page)
		return ""
	}
	if r := row("nginx:1.27"); !strings.Contains(r, `class="badge ok wide-only" title="In use by`) || !strings.Contains(r, "Warning: this image is in use") {
		t.Errorf("nginx:1.27 not marked in use:\n%s", r)
	}
	if r := row("nginx:latest"); strings.Contains(r, `class="badge ok wide-only" title="In use by`) || strings.Contains(r, "Warning: this image is in use") {
		t.Errorf("nginx:latest marked in use:\n%s", r)
	}
}
