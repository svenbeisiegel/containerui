package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// netSpec holds what the fake remembers about a network beyond
// runtime.Network.
type netSpec struct {
	Subnet, Gateway string
	Options         map[string]string
	Aliases         map[string][]string // container ID -> aliases
	IPs             map[string]string   // container ID -> fixed IP
}

// networkDrivers mimic docker's.
var networkDrivers = []string{"bridge", "macvlan", "ipvlan"}

// findNetwork returns the index of the network with this name or ID, or -1
// (call with f.mu held).
func (f *Runtime) findNetwork(name string) int {
	for i, n := range f.networks {
		if n.Name == name || n.ID == name {
			return i
		}
	}
	return -1
}

func notFoundNetwork(op, name string) error {
	return &runtime.CommandError{Op: op, Output: "network " + name + " not found", Kind: runtime.ErrNotFound}
}

// networkContainers returns the containers attached to a network (call
// with f.mu held), sorted by name.
func (f *Runtime) networkContainers(name string) []*runtime.Container {
	var out []*runtime.Container
	for _, c := range f.containers {
		if slices.Contains(c.Networks, name) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *runtime.Container) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (f *Runtime) spec(name string) *netSpec {
	s := f.netSpecs[name]
	if s == nil {
		s = &netSpec{Options: map[string]string{}, Aliases: map[string][]string{}, IPs: map[string]string{}}
		f.netSpecs[name] = s
	}
	return s
}

func (f *Runtime) InspectNetwork(_ context.Context, name string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("inspect-network %s", name); err != nil {
		return nil, err
	}
	i := f.findNetwork(name)
	if i < 0 {
		return nil, notFoundNetwork("network inspect", name)
	}
	n := f.networks[i]
	s := f.spec(n.Name)
	containers := map[string]any{}
	for k, c := range f.networkContainers(n.Name) {
		ip := s.IPs[c.ID]
		if ip == "" {
			ip = fmt.Sprintf("172.18.0.%d", k+2)
		}
		containers[c.ID] = map[string]any{"Name": c.Name, "IPv4Address": ip + "/16", "IPv6Address": "", "Aliases": s.Aliases[c.ID]}
	}
	var ipam []map[string]string
	if s.Subnet != "" {
		ipam = append(ipam, map[string]string{"Subnet": s.Subnet, "Gateway": s.Gateway})
	}
	labels := n.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return json.Marshal(map[string]any{
		"Name": n.Name, "Id": n.ID, "Created": n.Created, "Scope": n.Scope, "Driver": n.Driver,
		"EnableIPv6": n.IPv6, "Internal": n.Internal, "IPAM": map[string]any{"Driver": "default", "Config": ipam},
		"Containers": containers, "Options": s.Options, "Labels": labels,
	})
}

func (f *Runtime) CreateNetwork(_ context.Context, opts runtime.CreateNetworkOpts) (string, error) {
	if err := opts.Validate(networkDrivers, true); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("create-network %s", opts.Name); err != nil {
		return "", err
	}
	if f.findNetwork(opts.Name) >= 0 {
		return "", &runtime.CommandError{Op: "network create", Output: "network with name " + opts.Name + " already exists", Kind: runtime.ErrConflict}
	}
	driver := opts.Driver
	if driver == "" {
		driver = networkDrivers[0]
	}
	id := randomHex(32)
	n := runtime.Network{ID: id, Name: opts.Name, Driver: driver, Scope: "local", Internal: opts.Internal, IPv6: opts.IPv6, Created: time.Now()}
	for _, l := range opts.Labels {
		k, v, _ := strings.Cut(l, "=")
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[k] = v
	}
	f.networks = append(f.networks, n)
	s := f.spec(opts.Name)
	s.Subnet, s.Gateway = opts.Subnet, opts.Gateway
	for _, o := range opts.Options {
		k, v, _ := strings.Cut(o, "=")
		s.Options[k] = v
	}
	return id, nil
}

func (f *Runtime) RemoveNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("rmn %s", name); err != nil {
		return err
	}
	i := f.findNetwork(name)
	if i < 0 {
		return notFoundNetwork("network remove", name)
	}
	n := f.networks[i]
	if runtime.IsPredefinedNetwork(n.Name) {
		return &runtime.CommandError{Op: "network remove", Output: n.Name + " is a pre-defined network and cannot be removed", Kind: runtime.ErrConflict}
	}
	if cs := f.networkContainers(n.Name); len(cs) > 0 {
		return &runtime.CommandError{Op: "network remove", Output: "network " + n.Name + " has active endpoints (" + cs[0].Name + ")", Kind: runtime.ErrConflict}
	}
	f.networks = slices.Delete(f.networks, i, i+1)
	delete(f.netSpecs, n.Name)
	return nil
}

func (f *Runtime) PruneNetworks(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("prune-networks"); err != nil {
		return "", err
	}
	var removed []string
	f.networks = slices.DeleteFunc(f.networks, func(n runtime.Network) bool {
		prune := !runtime.IsPredefinedNetwork(n.Name) && len(f.networkContainers(n.Name)) == 0
		if prune {
			removed = append(removed, n.Name)
			delete(f.netSpecs, n.Name)
		}
		return prune
	})
	if len(removed) == 0 {
		return "", nil
	}
	return "Deleted Networks:\n" + strings.Join(removed, "\n"), nil
}

func (f *Runtime) ConnectNetwork(_ context.Context, network, container string, opts runtime.ConnectNetworkOpts) error {
	if err := errors.Join(runtime.ValidateNetworkName(network), runtime.ValidateContainerID(container), opts.Validate()); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("connect %s %s", network, container); err != nil {
		return err
	}
	i := f.findNetwork(network)
	if i < 0 {
		return notFoundNetwork("network connect", network)
	}
	c, err := f.find(container)
	if err != nil {
		return err
	}
	name := f.networks[i].Name
	if slices.Contains(c.Networks, name) {
		return &runtime.CommandError{Op: "network connect", Output: "endpoint with name " + c.Name + " already exists in network " + name, Kind: runtime.ErrConflict}
	}
	c.Networks = append(c.Networks, name)
	s := f.spec(name)
	s.Aliases[c.ID] = opts.Aliases
	if opts.IP != "" {
		s.IPs[c.ID] = opts.IP
	}
	return nil
}

func (f *Runtime) DisconnectNetwork(_ context.Context, network, container string) error {
	if err := errors.Join(runtime.ValidateNetworkName(network), runtime.ValidateContainerID(container)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("disconnect %s %s", network, container); err != nil {
		return err
	}
	i := f.findNetwork(network)
	if i < 0 {
		return notFoundNetwork("network disconnect", network)
	}
	c, err := f.find(container)
	if err != nil {
		return err
	}
	name := f.networks[i].Name
	j := slices.Index(c.Networks, name)
	if j < 0 {
		return &runtime.CommandError{Op: "network disconnect", Output: "container " + c.Name + " is not connected to network " + name, Kind: runtime.ErrConflict}
	}
	c.Networks = slices.Delete(c.Networks, j, j+1)
	s := f.spec(name)
	delete(s.Aliases, c.ID)
	delete(s.IPs, c.ID)
	return nil
}
