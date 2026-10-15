package jobs

import (
	"context"
	"errors"
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
