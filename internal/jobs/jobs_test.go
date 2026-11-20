package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

func TestJobLifecycle(t *testing.T) {
	m := NewManager(context.Background())
	release := make(chan struct{})
	j := m.Start("pull", "alpine", "admin", func(ctx context.Context, progress func(string)) error {
		progress("one")
		<-release
		progress("two")
		return nil
	})
	if j.Status != Running || j.ID == "" {
		t.Fatalf("job = %+v", j)
	}
	close(release)
	m.Wait()
	got, ok := m.Get(j.ID)
	if !ok || got.Status != Succeeded || len(got.Lines) != 2 || got.Finished.IsZero() {
		t.Fatalf("job = %+v", got)
	}

	bad := m.Start("pull", "nope", "admin", func(context.Context, func(string)) error { return errors.New("boom") })
	m.Wait()
	got, _ = m.Get(bad.ID)
	if got.Status != Failed || got.Error != "boom" {
		t.Fatalf("job = %+v", got)
	}
	if l := m.List("pull"); len(l) != 2 || l[0].ID != bad.ID {
		t.Errorf("list = %+v", l)
	}
}

func TestJobCancelledWithManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx)
	j := m.Start("pull", "x", "", func(ctx context.Context, _ func(string)) error {
		<-ctx.Done()
		return ctx.Err()
	})
	cancel()
	m.Wait()
	if got, _ := m.Get(j.ID); got.Status != Failed {
		t.Errorf("status = %s", got.Status)
	}
}

func TestLinesCapped(t *testing.T) {
	m := NewManager(context.Background())
	j := m.Start("pull", "x", "", func(_ context.Context, p func(string)) error {
		for i := 0; i < maxLines+50; i++ {
			p("line")
		}
		return nil
	})
	m.Wait()
	if got, _ := m.Get(j.ID); len(got.Lines) != maxLines {
		t.Errorf("lines = %d", len(got.Lines))
	}
}

func TestRecordAndListKinds(t *testing.T) {
	m := NewManager(context.Background())
	pull := m.Start("pull", "alpine", "admin", func(context.Context, func(string)) error { return nil })
	m.Wait()
	ok := m.Record("image", "tag alpine", "admin", []string{"done"}, nil)
	bad := m.Record("image", "delete alpine", "admin", nil, errors.New("in use"))
	m.Record("volume", "create data", "admin", nil, nil)
	if ok.Status != Succeeded || ok.Finished.IsZero() || len(ok.Lines) != 1 {
		t.Errorf("ok = %+v", ok)
	}
	if bad.Status != Failed || bad.Error != "in use" {
		t.Errorf("bad = %+v", bad)
	}
	if !(pull.Seq() < ok.Seq() && ok.Seq() < bad.Seq()) {
		t.Errorf("seq = %d %d %d", pull.Seq(), ok.Seq(), bad.Seq())
	}
	l := m.List("pull", "image")
	if len(l) != 3 || l[0].ID != bad.ID || l[1].ID != ok.ID || l[2].ID != pull.ID {
		t.Errorf("list = %+v", l)
	}
}

func TestHistoryCapped(t *testing.T) {
	m := NewManager(context.Background())
	release := make(chan struct{})
	running := m.Start("pull", "slow", "", func(context.Context, func(string)) error { <-release; return nil })
	var first, last Job
	for i := 0; i < MaxHistory+20; i++ {
		j := m.Record("image", "x", "", nil, nil)
		if i == 0 {
			first = j
		}
		last = j
	}
	l := m.List("pull", "image")
	if len(l) != MaxHistory {
		t.Fatalf("kept %d jobs, want %d", len(l), MaxHistory)
	}
	if _, ok := m.Get(running.ID); !ok {
		t.Error("running job dropped")
	}
	if _, ok := m.Get(first.ID); ok {
		t.Error("oldest finished job kept")
	}
	if l[0].ID != last.ID {
		t.Errorf("newest = %+v", l[0])
	}
	close(release)
	m.Wait()
}

func TestPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	m := NewManager(context.Background())
	if err := m.Persist(path, func(err error) { t.Error(err) }); err != nil {
		t.Fatal(err)
	}
	m.Start("pull", "alpine", "admin", func(_ context.Context, p func(string)) error { p("pulled"); return nil })
	m.Wait()
	m.Record("image", "delete alpine", "admin", nil, errors.New("in use"))
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if goruntime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}

	n := NewManager(context.Background())
	if err := n.Persist(path, nil); err != nil {
		t.Fatal(err)
	}
	l := n.List("pull", "image")
	if len(l) != 2 || l[0].Target != "delete alpine" || l[0].Error != "in use" || l[1].Target != "alpine" ||
		len(l[1].Lines) != 1 || l[1].Status != Succeeded || l[0].Seq() <= l[1].Seq() {
		t.Fatalf("loaded = %+v", l)
	}
	j := n.Record("volume", "create data", "admin", nil, nil)
	if j.Seq() <= l[0].Seq() {
		t.Errorf("new seq %d after %d", j.Seq(), l[0].Seq())
	}
}

func TestPersistBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background())
	if err := m.Persist(path, nil); err == nil {
		t.Fatal("no error for a corrupt file")
	}
	m.Record("image", "x", "", nil, nil) // still saves, replacing the bad file
	n := NewManager(context.Background())
	if err := n.Persist(path, nil); err != nil || len(n.List("image")) != 1 {
		t.Errorf("err = %v, jobs = %+v", err, n.List("image"))
	}
}
