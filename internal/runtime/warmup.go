package runtime

import (
	"context"
	"errors"
	"sync"
)

// WarmUp runs the list queries the dashboard needs so that a backend with a
// slow cold start (wslc starts its session/VM on first use) is ready before
// the first user request. Containers are listed first because that call
// starts the session; the remaining lists then run concurrently.
func WarmUp(ctx context.Context, rt Runtime) error {
	if _, err := rt.ListContainers(ctx, ListContainersOpts{All: true}); err != nil {
		return err
	}
	caps := rt.Capabilities()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	run(func() error { _, err := rt.ListImages(ctx); return err })
	if caps.Volumes {
		run(func() error { _, err := rt.ListVolumes(ctx); return err })
	}
	if caps.Networks {
		run(func() error { _, err := rt.ListNetworks(ctx); return err })
	}
	wg.Wait()
	return errors.Join(errs...)
}
