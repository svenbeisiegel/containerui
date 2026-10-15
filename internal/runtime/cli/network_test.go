package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestNetworkCommands(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		flavor                         *Flavor
		create, remove, connect, prune string
	}{
		{WSLC,
			"network create --subnet 172.30.0.0/16 --gateway 172.30.0.1 --internal --opt mtu=1400 --label team=x backend",
			"network remove backend",
			"network connect --ip 172.30.0.5 --network-alias db --network-alias db2 backend web",
			"network prune --force"},
		{Docker,
			"network create --subnet 172.30.0.0/16 --gateway 172.30.0.1 --internal --opt mtu=1400 --label team=x backend",
			"network rm backend",
			"network connect --ip 172.30.0.5 --alias db --alias db2 backend web",
			"network prune --force"},
		{Podman,
			"network create --subnet 172.30.0.0/16 --gateway 172.30.0.1 --internal --opt mtu=1400 --label team=x backend",
			"network rm backend",
			"network connect --ip 172.30.0.5 --alias db --alias db2 backend web",
			"network prune --force"},
	} {
		r := &fakeRunner{stdout: "3f2a9c0e\n"}
		d := NewWithRunner(r, tc.flavor, config.Runtime{})
		id, err := d.CreateNetwork(ctx, runtime.CreateNetworkOpts{Name: "backend", Subnet: "172.30.0.0/16", Gateway: "172.30.0.1", Internal: true,
			Options: []string{"mtu=1400"}, Labels: []string{"team=x"}})
		if err != nil || id != "3f2a9c0e" {
			t.Fatalf("%s create = %q, %v", tc.flavor.Name, id, err)
		}
		if got := r.last(); got != tc.create {
			t.Errorf("%s create = %q", tc.flavor.Name, got)
		}
		if err := d.RemoveNetwork(ctx, "backend"); err != nil || r.last() != tc.remove {
			t.Errorf("%s remove = %q, %v", tc.flavor.Name, r.last(), err)
		}
		if err := d.ConnectNetwork(ctx, "backend", "web", runtime.ConnectNetworkOpts{Aliases: []string{"db", "db2"}, IP: "172.30.0.5"}); err != nil || r.last() != tc.connect {
			t.Errorf("%s connect = %q, %v", tc.flavor.Name, r.last(), err)
		}
		if err := d.DisconnectNetwork(ctx, "backend", "web"); err != nil || r.last() != "network disconnect backend web" {
			t.Errorf("%s disconnect = %q, %v", tc.flavor.Name, r.last(), err)
		}
		if _, err := d.PruneNetworks(ctx); err != nil || r.last() != tc.prune {
			t.Errorf("%s prune = %q, %v", tc.flavor.Name, r.last(), err)
		}
		r.stdout = `[{"Name":"backend","Id":"3f2a"}]`
		if raw, err := d.InspectNetwork(ctx, "backend"); err != nil || !strings.Contains(string(raw), "3f2a") || r.last() != "network inspect backend" {
			t.Errorf("%s inspect = %s, %v (%q)", tc.flavor.Name, raw, err, r.last())
		}
	}
}

func TestNetworkCapabilities(t *testing.T) {
	c := NewWithRunner(&fakeRunner{}, WSLC, config.Runtime{}).Capabilities()
	if c.NetworkIPv6 || len(c.NetworkDrivers) != 1 {
		t.Errorf("wslc caps = %+v", c)
	}
	if c := NewWithRunner(&fakeRunner{}, Docker, config.Runtime{}).Capabilities(); !c.NetworkIPv6 || c.NetworkDrivers[0] != "bridge" {
		t.Errorf("docker caps = %+v", c)
	}
	// IPv6 only where supported.
	w := NewWithRunner(&fakeRunner{}, WSLC, config.Runtime{})
	if _, err := w.CreateNetwork(context.Background(), runtime.CreateNetworkOpts{Name: "n", IPv6: true}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("wslc ipv6: %v", err)
	}
	r := &fakeRunner{}
	d := NewWithRunner(r, Docker, config.Runtime{})
	if _, err := d.CreateNetwork(context.Background(), runtime.CreateNetworkOpts{Name: "n", Driver: "macvlan", IPv6: true}); err != nil || r.last() != "network create --driver macvlan --ipv6 n" {
		t.Errorf("docker ipv6: %q %v", r.last(), err)
	}
}

func TestNetworkErrors(t *testing.T) {
	ctx := context.Background()
	d := NewWithRunner(&fakeRunner{stderr: "Error response from daemon: error while removing network: network backend id 3f2a has active endpoints", code: 1}, Docker, config.Runtime{})
	if err := d.RemoveNetwork(ctx, "backend"); !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("in use: %v", err)
	}
	d = NewWithRunner(&fakeRunner{stdout: "[]", stderr: "Error: No such network: nope", code: 1}, Docker, config.Runtime{})
	if _, err := d.InspectNetwork(ctx, "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("inspect missing: %v", err)
	}
	r := &fakeRunner{}
	d = NewWithRunner(r, Docker, config.Runtime{})
	for _, err := range []error{
		d.RemoveNetwork(ctx, "-f"),
		d.ConnectNetwork(ctx, "backend", "--privileged", runtime.ConnectNetworkOpts{}),
		d.ConnectNetwork(ctx, "backend", "web", runtime.ConnectNetworkOpts{IP: "nope"}),
		d.DisconnectNetwork(ctx, "-x", "web"),
	} {
		if !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("want ErrInvalid, got %v", err)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("invalid input reached the CLI: %v", r.calls)
	}
}
