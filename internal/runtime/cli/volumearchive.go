package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var _ runtime.VolumeArchiver = (*Driver)(nil)

// helperLabel marks the short-lived containers of volume backup and restore.
const helperLabel = "containerui.helper=volume-archive"

// The helper's scripts. find -delete empties the volume (hidden files
// too) without removing its root; tar keeps owners and modes as root.
const (
	exportScript  = "cd /data && exec tar -cf - ."
	importScript  = "cd /data && exec tar -xf -"
	replaceScript = "cd /data && find . -mindepth 1 -delete && exec tar -xf -"
)

// helperArgs runs the helper image with the volume mounted at /data. The
// tar stream must not end up in the container's log, so logging is off.
func (d *Driver) helperArgs(volume string, readOnly bool, script string) []string {
	mount := volume + ":/data"
	if readOnly {
		mount += ":ro"
	}
	return []string{"container", "run", "--rm", "--interactive", "--quiet",
		"--network", "none", "--log-driver", "none", "--user", "0:0",
		"--label", helperLabel, "--volume", mount,
		"--entrypoint", "sh", d.helperImage, "-c", script}
}

// archiveCheck makes sure the volume exists: "run --volume" would
// otherwise create it.
func (d *Driver) archiveCheck(ctx context.Context, name string) error {
	if !d.flavor.volumeArchive {
		return fmt.Errorf("volume backup: %w", runtime.ErrUnsupported)
	}
	_, err := d.InspectVolume(ctx, name)
	return err
}

func (d *Driver) ExportVolume(ctx context.Context, name string, w io.Writer) error {
	if err := d.archiveCheck(ctx, name); err != nil {
		return err
	}
	return d.exchange(ctx, "volume export", d.helperArgs(name, true, exportScript), nil, w)
}

func (d *Driver) ImportVolume(ctx context.Context, name string, r io.Reader, replace bool) error {
	if err := d.archiveCheck(ctx, name); err != nil {
		return err
	}
	script := importScript
	if replace {
		script = replaceScript
	}
	return d.exchange(ctx, "volume import", d.helperArgs(name, false, script), r, io.Discard)
}

// exchange runs a CLI command that reads stdin and writes stdout without a
// command timeout (it may move gigabytes); ctx bounds it.
func (d *Driver) exchange(ctx context.Context, op string, args []string, stdin io.Reader, w io.Writer) error {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stderr, code, err := d.runner.Exchange(ctx, d.args(args...), stdin, w)
	if err != nil {
		return fmt.Errorf("%s %s: %w", d.flavor.Name, op, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s: %w", d.flavor.Name, op, ctx.Err())
	}
	if code != 0 {
		return d.commandError(op, code, nil, stderr)
	}
	return nil
}
