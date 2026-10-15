package runtime_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/fake"
)

// countingRuntime records which list methods were called.
type countingRuntime struct {
	runtime.Runtime
	caps  runtime.Capabilities
	mu    sync.Mutex
	calls []string
	err   error
}

func (c *countingRuntime) note(s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, s)
	return c.err
}

func (c *countingRuntime) Capabilities() runtime.Capabilities { return c.caps }
func (c *countingRuntime) ListContainers(context.Context, runtime.ListContainersOpts) ([]runtime.Container, error) {
	return nil, c.note("containers")
}
func (c *countingRuntime) ListImages(context.Context) ([]runtime.Image, error) {
	return nil, c.note("images")
}
func (c *countingRuntime) ListVolumes(context.Context) ([]runtime.Volume, error) {
	return nil, c.note("volumes")
}
func (c *countingRuntime) ListNetworks(context.Context) ([]runtime.Network, error) {
	return nil, c.note("networks")
}

func TestWarmUp(t *testing.T) {
	rt := &countingRuntime{Runtime: fake.New(), caps: runtime.Capabilities{Volumes: true, Networks: true}}
	if err := runtime.WarmUp(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if rt.calls[0] != "containers" {
		t.Errorf("first call = %q, want containers", rt.calls[0])
	}
	got := slices.Sorted(slices.Values(rt.calls))
	if want := []string{"containers", "images", "networks", "volumes"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestWarmUpRespectsCapabilities(t *testing.T) {
	rt := &countingRuntime{Runtime: fake.New()}
	if err := runtime.WarmUp(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(rt.calls)); !slices.Equal(got, []string{"containers", "images"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestWarmUpStopsOnContainerError(t *testing.T) {
	boom := errors.New("boom")
	rt := &countingRuntime{Runtime: fake.New(), caps: runtime.Capabilities{Volumes: true}, err: boom}
	if err := runtime.WarmUp(context.Background(), rt); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(rt.calls) != 1 {
		t.Errorf("calls after failing container list = %v", rt.calls)
	}
}
