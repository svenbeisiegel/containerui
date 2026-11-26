package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
)

// Driver is the docker or podman CLI driver with the calls that are made
// often or run long going through the Engine API: Info, the lists, inspect
// (docker; podman's CLI prints its own inspect format, which the UI reads),
// logs, pulls, terminals, attaching to containers with a TTY and resource
// usage. Everything else, and every call while the socket cannot be used,
// goes to the embedded CLI driver.
type Driver struct {
	*cli.Driver
	api    *Client
	fb     *fallback
	podman bool
	creds  registry.Credentials

	commandTimeout time.Duration
	pullTimeout    time.Duration
}

var (
	_ runtime.Runtime        = (*Driver)(nil)
	_ runtime.Stater         = (*Driver)(nil)
	_ runtime.Composer       = (*Driver)(nil)
	_ runtime.VolumeArchiver = (*Driver)(nil)
	_ runtime.VolumeBrowser  = (*Driver)(nil)
)

// Options configure Wrap.
type Options struct {
	// Credentials are the registry logins sent with a pull. Pulls from a
	// registry whose login is kept by a credential helper use the CLI.
	// nil pulls anonymously.
	Credentials registry.Credentials
	Logger      *slog.Logger
}

// Wrap returns d reading through the API on the Unix socket at socket. It
// returns nil if the API is not used for d: wslc, no socket, or a Windows
// named pipe (not supported by the client).
func Wrap(d *cli.Driver, socket string, o Options) *Driver {
	if d == nil || socket == "" || strings.HasPrefix(socket, `\\`) {
		return nil
	}
	if n := d.Name(); n != cli.Docker.Name && n != cli.Podman.Name {
		return nil
	}
	cmd, pull := d.Timeouts()
	return &Driver{
		Driver:         d,
		api:            New(socket),
		fb:             newFallback(socket, d.Name()+" lists, logs, pulls, terminals and resource usage", o.Logger),
		podman:         d.Name() == cli.Podman.Name,
		creds:          o.Credentials,
		commandTimeout: cmd,
		pullTimeout:    pull,
	}
}

// CLI is the wrapped CLI driver.
func (d *Driver) CLI() *cli.Driver { return d.Driver }

// Socket is the socket the API is read through.
func (d *Driver) Socket() string { return d.api.Socket() }

// Source reports where the last answer came from: "api", "cli", or ""
// before the first call.
func (d *Driver) Source() string { return d.fb.Source() }

// opError turns an error answer of the daemon into the runtime's
// CommandError, as the CLI driver reports it. Other errors are returned
// unchanged, so the fallback still recognizes them.
func opError(op string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return &runtime.CommandError{Op: op, ExitCode: e.Status, Output: e.Message, Kind: e.Kind()}
	}
	return err
}

// read runs a read-only call: through the API, bounded by the command
// timeout, and through the CLI while the API cannot be used.
func read[T any](d *Driver, ctx context.Context, api, cli func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d.commandTimeout)
	defer cancel()
	return try(d.fb, ctx, onTransport, api, cli)
}

// esc escapes a name or ID for a URL path.
func esc(s string) string { return url.PathEscape(s) }

func (d *Driver) Info(ctx context.Context) (runtime.Info, error) {
	return read(d, ctx, func(ctx context.Context) (runtime.Info, error) {
		var v struct{ Version string }
		if err := d.api.get(ctx, "/version", &v); err != nil {
			return runtime.Info{Driver: d.Name()}, opError("version", err)
		}
		return runtime.Info{Driver: d.Name(), Version: v.Version}, nil
	}, d.Driver.Info)
}

func (d *Driver) Stats(ctx context.Context) ([]runtime.ContainerStats, error) {
	return try(d.fb, ctx, anyError, d.api.Stats, d.Driver.Stats)
}

func (d *Driver) DiskUsage(ctx context.Context) (runtime.DiskUsage, error) {
	return try(d.fb, ctx, anyError, d.api.DiskUsage, d.Driver.DiskUsage)
}

func (d *Driver) ListContainers(ctx context.Context, opts runtime.ListContainersOpts) ([]runtime.Container, error) {
	return read(d, ctx, func(ctx context.Context) ([]runtime.Container, error) {
		list, err := d.listContainers(ctx, opts.All)
		if err != nil {
			return nil, err
		}
		return convertContainers(list, time.Now()), nil
	}, func(ctx context.Context) ([]runtime.Container, error) { return d.Driver.ListContainers(ctx, opts) })
}

func (d *Driver) listContainers(ctx context.Context, all bool) ([]listContainerJSON, error) {
	path := "/containers/json"
	if all {
		path += "?all=1"
	}
	var list []listContainerJSON
	if err := d.api.get(ctx, path, &list); err != nil {
		return nil, opError("container list", err)
	}
	return list, nil
}

func (d *Driver) ListImages(ctx context.Context) ([]runtime.Image, error) {
	return read(d, ctx, func(ctx context.Context) ([]runtime.Image, error) {
		var list []listImageJSON
		if err := d.api.get(ctx, "/images/json", &list); err != nil {
			return nil, opError("image list", err)
		}
		var counts map[string]int
		for _, img := range list {
			if img.Containers < 0 {
				// The daemon did not count the containers: count them.
				cs, err := d.listContainers(ctx, true)
				if err != nil {
					return nil, err
				}
				counts = map[string]int{}
				for _, c := range cs {
					counts[c.ImageID]++
				}
				break
			}
		}
		return convertImages(list, counts, d.podman), nil
	}, d.Driver.ListImages)
}

func (d *Driver) ListVolumes(ctx context.Context) ([]runtime.Volume, error) {
	return read(d, ctx, func(ctx context.Context) ([]runtime.Volume, error) {
		var v struct{ Volumes []listVolumeJSON }
		if err := d.api.get(ctx, "/volumes", &v); err != nil {
			return nil, opError("volume list", err)
		}
		return convertVolumes(v.Volumes), nil
	}, d.Driver.ListVolumes)
}

func (d *Driver) ListNetworks(ctx context.Context) ([]runtime.Network, error) {
	return read(d, ctx, func(ctx context.Context) ([]runtime.Network, error) {
		var list []listNetworkJSON
		if err := d.api.get(ctx, "/networks", &list); err != nil {
			return nil, opError("network list", err)
		}
		return convertNetworks(list), nil
	}, d.Driver.ListNetworks)
}

// inspect reads an inspect document through the API (docker only: it is
// the same the CLI prints). Podman's CLI prints its own format.
func (d *Driver) inspect(ctx context.Context, op, path string, cli func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if d.podman {
		return cli(ctx)
	}
	return read(d, ctx, func(ctx context.Context) (json.RawMessage, error) {
		doc, err := d.api.raw(ctx, path)
		return doc, opError(op, err)
	}, cli)
}

func (d *Driver) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	if err := runtime.ValidateContainerID(id); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "container inspect", "/containers/"+esc(id)+"/json", func(ctx context.Context) (json.RawMessage, error) {
		return d.Driver.InspectContainer(ctx, id)
	})
}

func (d *Driver) InspectImage(ctx context.Context, ref string) (json.RawMessage, error) {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "image inspect", "/images/"+imagePath(ref)+"/json", func(ctx context.Context) (json.RawMessage, error) {
		return d.Driver.InspectImage(ctx, ref)
	})
}

func (d *Driver) InspectVolume(ctx context.Context, name string) (json.RawMessage, error) {
	if err := runtime.ValidateVolumeName(name); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "volume inspect", "/volumes/"+esc(name), func(ctx context.Context) (json.RawMessage, error) {
		return d.Driver.InspectVolume(ctx, name)
	})
}

func (d *Driver) InspectNetwork(ctx context.Context, name string) (json.RawMessage, error) {
	if err := runtime.ValidateNetworkName(name); err != nil {
		return nil, err
	}
	return d.inspect(ctx, "network inspect", "/networks/"+esc(name), func(ctx context.Context) (json.RawMessage, error) {
		return d.Driver.InspectNetwork(ctx, name)
	})
}

// imagePath escapes an image reference for /images/{name}: the daemon
// takes the slashes of a repository as they are.
func imagePath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// notModified reports whether err is a 304 of the daemon (e.g. starting a
// container that is already running).
func notModified(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotModified
}
