package runtime

import (
	"fmt"
	"sort"
	"sync"

	"github.com/svenbeisiegel/containerui/internal/config"
)

// Factory builds a driver from the runtime section of the config.
type Factory func(cfg config.Runtime) (Runtime, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a driver available under name. Drivers call it from init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("runtime: driver registered twice: " + name)
	}
	factories[name] = f
}

// Open creates the driver selected by cfg.Driver.
func Open(cfg config.Runtime) (Runtime, error) {
	mu.RLock()
	f, ok := factories[cfg.Driver]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown runtime driver %q (available: %v)", cfg.Driver, Drivers())
	}
	return f(cfg)
}

// Drivers lists registered driver names.
func Drivers() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
