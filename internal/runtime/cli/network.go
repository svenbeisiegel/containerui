package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func (d *Driver) InspectNetwork(ctx context.Context, name string) (json.RawMessage, error) {
	if err := runtime.ValidateNetworkName(name); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "network", name)
}

func (d *Driver) CreateNetwork(ctx context.Context, opts runtime.CreateNetworkOpts) (string, error) {
	if err := opts.Validate(d.flavor.networkDrivers, d.flavor.networkIPv6); err != nil {
		return "", err
	}
	args := []string{"network", "create"}
	for _, kv := range []struct{ flag, val string }{
		{"--driver", opts.Driver}, {"--subnet", opts.Subnet}, {"--gateway", opts.Gateway}, {"--ip-range", opts.IPRange},
	} {
		if kv.val != "" {
			args = append(args, kv.flag, kv.val)
		}
	}
	if opts.Internal {
		args = append(args, "--internal")
	}
	if opts.IPv6 {
		args = append(args, "--ipv6")
	}
	for _, o := range opts.Options {
		args = append(args, "--opt", o)
	}
	for _, l := range opts.Labels {
		args = append(args, "--label", l)
	}
	out, err := d.run(ctx, "network create", append(args, opts.Name)...)
	if err != nil {
		return "", err
	}
	if id := lastLine(out); id != "" {
		return id, nil
	}
	return opts.Name, nil
}

func (d *Driver) RemoveNetwork(ctx context.Context, name string) error {
	if err := runtime.ValidateNetworkName(name); err != nil {
		return err
	}
	_, err := d.run(ctx, "network remove", "network", d.flavor.verb("remove"), name)
	return err
}

func (d *Driver) PruneNetworks(ctx context.Context) (string, error) {
	// --force skips the interactive confirmation.
	out, err := d.run(ctx, "network prune", "network", "prune", "--force")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(normalize(out))), nil
}

func (d *Driver) ConnectNetwork(ctx context.Context, network, container string, opts runtime.ConnectNetworkOpts) error {
	if err := errors.Join(runtime.ValidateNetworkName(network), runtime.ValidateContainerID(container), opts.Validate()); err != nil {
		return err
	}
	args := []string{"network", "connect"}
	if opts.IP != "" {
		args = append(args, "--ip", opts.IP)
	}
	for _, a := range opts.Aliases {
		args = append(args, d.flavor.aliasFlag, a)
	}
	_, err := d.run(ctx, "network connect", append(args, network, container)...)
	return err
}

func (d *Driver) DisconnectNetwork(ctx context.Context, network, container string) error {
	if err := errors.Join(runtime.ValidateNetworkName(network), runtime.ValidateContainerID(container)); err != nil {
		return err
	}
	_, err := d.run(ctx, "network disconnect", "network", "disconnect", network, container)
	return err
}
