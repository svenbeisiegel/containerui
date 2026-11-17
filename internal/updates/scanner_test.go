package updates

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/fake"
)

type stub map[string]registry.Status

func (s stub) Check(_ context.Context, ref string, _ []string, _ bool) registry.Status {
	st := s[ref]
	st.Ref = ref
	return st
}

func TestScan(t *testing.T) {
	ctx := context.Background()
	rt := fake.New()
	now := time.Now()
	rt.AddImage(runtime.Image{ID: "sha256:a1", Repository: "nginx", Tag: "1.26", Created: now})
	rt.AddImage(runtime.Image{ID: "sha256:a2", Repository: "nginx", Tag: "1.27", Created: now})
	rt.AddImage(runtime.Image{ID: "sha256:a3", Repository: "<none>", Tag: "<none>", Created: now})
	rt.AddImage(runtime.Image{ID: "sha256:a4", Repository: "redis", Tag: "<none>", Created: now})
	rt.AddContainer(runtime.Container{ID: "c1", Name: "old", Image: "nginx:1.26"})
	rt.AddContainer(runtime.Container{ID: "c2", Name: "new", Image: "nginx:1.27"})
	checks := stub{
		"nginx:1.26": {NewerTags: []registry.Tag{{Name: "1.28", Kind: "minor"}, {Name: "1.27", Kind: "minor"}}},
		"nginx:1.27": {NewerTags: []registry.Tag{{Name: "1.28", Kind: "minor"}}},
	}
	s := New(rt, checks, time.Hour, nil)
	s.Scan(ctx, false)

	if st := s.State(); st.Scanning || st.Last.IsZero() || st.Err != "" {
		t.Fatalf("state = %+v", st)
	}
	if len(s.images) != 2 {
		t.Fatalf("checked %d images, want the two tagged ones", len(s.images))
	}
	old, _ := s.Image("nginx:1.26")
	cur, _ := s.Image("docker.io/library/nginx:1.27")
	if old.NewerLocal != "1.27" || old.Available() || !cur.Available() || cur.NewerTags()[0].Name != "1.28" {
		t.Errorf("1.26 = %+v, 1.27 = %+v", old, cur)
	}

	want := []Candidate{{Ref: "nginx:1.28", Label: "1.28 (minor)", Pull: true}, {Ref: "nginx:1.27", Label: "1.27 (minor)"}}
	if got := s.ForContainer(runtime.Container{ID: "c1", Image: "nginx:1.26"}); !reflect.DeepEqual(got, want) {
		t.Errorf("candidates =\n %+v\nwant\n %+v", got, want)
	}

	// A newer image pulled for the same tag: the container runs the old one.
	rt.AddImage(runtime.Image{ID: "sha256:b2", Repository: "nginx", Tag: "1.27", Created: now})
	checks["nginx:1.27"] = registry.Status{}
	s.Refresh(ctx, "nginx:1.27")
	want = []Candidate{{Ref: "nginx:1.27", Label: "1.27 (newer image already pulled)"}}
	if got := s.ForContainer(runtime.Container{ID: "c2", Image: "nginx:1.27"}); !reflect.DeepEqual(got, want) {
		t.Errorf("candidates =\n %+v\nwant\n %+v", got, want)
	}
	// A container created after the scan runs the current image.
	if got := s.ForContainer(runtime.Container{ID: "c9", Image: "nginx:1.27"}); got != nil {
		t.Errorf("new container candidates = %+v", got)
	}

	if err := rt.RemoveImage(ctx, "nginx:1.26", true); err != nil {
		t.Fatal(err)
	}
	s.Refresh(ctx, "nginx:1.26")
	if _, ok := s.Image("nginx:1.26"); ok {
		t.Error("removed image still known")
	}
}

func TestCheckNow(t *testing.T) {
	rt := fake.NewSeeded()
	s := New(rt, stub{}, time.Hour, nil)
	if !s.CheckNow() {
		t.Fatal("check not started")
	}
	s.Wait()
	if !s.CheckNow() {
		t.Error("second check not started after the first finished")
	}
	s.Wait()
	rt.Err = runtime.ErrUnsupported
	s.CheckNow()
	s.Wait()
	if st := s.State(); st.Err == "" || len(s.images) == 0 {
		t.Errorf("failed scan: state %+v, kept %d images", st, len(s.images))
	}
}
