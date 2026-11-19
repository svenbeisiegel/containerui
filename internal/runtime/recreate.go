package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RecreateSource describes an existing container as the options that
// create it again.
type RecreateSource struct {
	ID         string
	Name       string
	Running    bool
	AutoRemove bool
	// Project is the compose project managing the container, if any.
	Project string
	Opts    CreateContainerOpts
	// Lost lists settings of the container that Opts cannot reproduce.
	Lost []string
}

// IsDefaultNetwork reports whether name is the runtime's default network.
func IsDefaultNetwork(name string) bool { return name == "bridge" || name == "podman" }

// OptsFromInspect turns container inspect output into create options.
// image is the inspect output of the container's image (by image ID); the
// command, entrypoint, environment, labels, user and working directory are
// only kept where the container differs from it, so that a newer image's
// own defaults apply after an update. image may be nil.
func OptsFromInspect(container, image json.RawMessage) (RecreateSource, error) {
	var c ContainerInspect
	if err := json.Unmarshal(container, &c); err != nil {
		return RecreateSource{}, fmt.Errorf("decode container inspect: %w", err)
	}
	var img ImageInspect
	if len(image) > 0 {
		if err := json.Unmarshal(image, &img); err != nil {
			return RecreateSource{}, fmt.Errorf("decode image inspect: %w", err)
		}
	}
	src := RecreateSource{
		ID:         c.ID,
		Name:       strings.TrimPrefix(c.Name, "/"),
		Running:    c.State.Running || ParseState(c.State.Status).IsRunning(),
		AutoRemove: c.HostConfig.AutoRemove,
		Project:    c.Config.Labels[LabelProject],
	}
	lost := func(format string, args ...any) { src.Lost = append(src.Lost, fmt.Sprintf(format, args...)) }
	o := &src.Opts
	o.Image, o.Name, o.Pull = c.Config.Image, src.Name, PullMissing
	if p, ok := ParsePullPolicy(c.Config.Labels[LabelPull]); ok {
		o.Pull = p
	}

	// Overriding the entrypoint resets the image's command, so the
	// container's command is kept as it is then.
	if !slices.Equal(c.Config.Entrypoint, img.Config.Entrypoint) {
		o.Entrypoint = append([]string{}, c.Config.Entrypoint...)
		o.Cmd = c.Config.Cmd
	} else if !slices.Equal(c.Config.Cmd, img.Config.Cmd) {
		o.Cmd = c.Config.Cmd
	}
	o.Env = Without(c.Config.Env, img.Config.Env)
	labels := OwnLabels(c.Config.Labels, img.Config.Labels)
	for _, k := range SortedKeys(labels) {
		o.Labels = append(o.Labels, k+"="+labels[k])
	}
	if c.Config.User != img.Config.User {
		o.User = c.Config.User
	}
	if c.Config.WorkingDir != img.Config.WorkingDir {
		o.WorkDir = c.Config.WorkingDir
	}
	if h := c.Config.Hostname; h != "" && !strings.HasPrefix(c.ID, h) {
		o.Hostname = h
	}
	if c.Config.Domainname != "" {
		lost("domain name %s", c.Config.Domainname)
	}

	for _, spec := range PortSpecs(c.HostConfig.PortBindings) {
		p, err := ParsePortSpec(spec)
		if err != nil {
			lost("published port %s", spec)
			continue
		}
		o.Ports = append(o.Ports, p)
	}
	for i, m := range c.Mounts {
		v := VolumeMount{Target: m.Destination, ReadOnly: c.ReadOnly(i)}
		switch m.Type {
		case "bind":
			v.Source = m.Source
			if m.Propagation != "" && m.Propagation != "rprivate" {
				lost("mount propagation %s of %s", m.Propagation, m.Destination)
			}
		case "volume":
			// Anonymous volumes too, so their data is kept.
			v.Source = m.Name
			if v.Source == "" {
				v.Source = m.Source
			}
		default:
			lost("%s mount at %s", m.Type, m.Destination)
			continue
		}
		o.Volumes = append(o.Volumes, v)
	}

	inspectNetworks(&c, src.Name, o, lost)

	o.TTY, o.Interactive, o.AutoRemove = c.Config.Tty, c.Config.OpenStdin, c.HostConfig.AutoRemove
	if h := c.Config.Healthcheck; h != nil && len(h.Test) > 0 {
		switch {
		case h.Test[0] == "NONE":
			o.NoHealthcheck = true
		case !h.Equal(img.Config.Healthcheck):
			lost("health check %s", strings.Join(h.Test, " "))
		}
	}
	if n := c.HostConfig.NanoCpus; n > 0 {
		o.CPUs = strconv.FormatFloat(float64(n)/1e9, 'f', -1, 64)
	}
	if m := c.HostConfig.Memory; m > 0 {
		o.Memory = formatBytes(m)
	}
	o.DNS, o.DNSSearch, o.DNSOptions = c.HostConfig.Dns, c.HostConfig.DnsSearch, c.HostConfig.DnsOptions
	for _, u := range c.HostConfig.Ulimits {
		o.Ulimits = append(o.Ulimits, fmt.Sprintf("%s=%d:%d", u.Name, u.Soft, u.Hard))
	}
	if r := c.HostConfig.RestartPolicy; r.Name != "" && r.Name != "no" {
		o.RestartPolicy = r.Name
		if r.Name == "on-failure" && r.MaximumRetryCount > 0 {
			o.RestartPolicy += ":" + strconv.Itoa(r.MaximumRetryCount)
		}
	}
	o.CapAdd, o.CapDrop, o.Privileged, o.ExtraHosts = c.HostConfig.CapAdd, c.HostConfig.CapDrop, c.HostConfig.Privileged, c.HostConfig.ExtraHosts

	// What CreateContainerOpts has no field for.
	hc := c.HostConfig
	for _, d := range hc.Devices {
		lost("device %s", d.PathOnHost)
	}
	for _, s := range hc.SecurityOpt {
		lost("security option %s", s)
	}
	for _, k := range SortedKeys(hc.Sysctls) {
		lost("sysctl %s", k)
	}
	for _, k := range SortedKeys(hc.Tmpfs) {
		lost("tmpfs %s", k)
	}
	for _, g := range hc.GroupAdd {
		lost("additional group %s", g)
	}
	// 64 MiB is docker's default, 62.5 MiB podman's.
	if hc.ShmSize > 0 && hc.ShmSize != 64<<20 && hc.ShmSize != 65536000 {
		lost("shared memory size %s", formatBytes(hc.ShmSize))
	}
	if m := hc.PidMode; m == "host" || strings.HasPrefix(m, "container:") {
		lost("PID namespace %s", m)
	}
	if m := hc.IpcMode; m == "host" || strings.HasPrefix(m, "container:") {
		lost("IPC namespace %s", m)
	}
	// podman sets a limit of 2048 by default.
	if p := hc.PidsLimit; p != nil && *p > 0 && *p != 2048 {
		lost("PIDs limit %d", *p)
	}
	if s := c.Config.StopSignal; s != "" && s != img.Config.StopSignal {
		lost("stop signal %s", s)
	}
	if mac := c.Config.MacAddress; mac != "" {
		lost("MAC address %s", mac)
	}
	return src, nil
}

// inspectNetworks fills o.Networks: the network of the network mode first, then
// the others. The runtime's default network alone is left implicit.
func inspectNetworks(c *ContainerInspect, name string, o *CreateContainerOpts, lost func(string, ...any)) {
	mode := c.HostConfig.NetworkMode
	switch {
	case mode == "host" || mode == "none":
		o.Networks = []NetworkAttachment{{Name: mode}}
		return
	case strings.HasPrefix(mode, "container:"), strings.HasPrefix(mode, "ns:"), mode == "slirp4netns" || strings.HasPrefix(mode, "slirp4netns:") || mode == "pasta" || strings.HasPrefix(mode, "pasta:"):
		lost("network mode %s", mode)
		return
	}
	names := SortedKeys(c.NetworkSettings.Networks)
	if i := slices.Index(names, mode); i > 0 {
		names = append(append([]string{mode}, names[:i]...), names[i+1:]...)
	}
	for _, n := range names {
		ep := c.NetworkSettings.Networks[n]
		a := NetworkAttachment{Name: n}
		if !IsDefaultNetwork(n) {
			for _, al := range ep.Aliases {
				// The runtime adds the short ID (and podman the name).
				if al != name && !strings.HasPrefix(c.ID, al) && !slices.Contains(a.Aliases, al) {
					a.Aliases = append(a.Aliases, al)
				}
			}
		}
		if ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address != "" {
			a.IP = ep.IPAMConfig.IPv4Address
		}
		o.Networks = append(o.Networks, a)
	}
	if len(o.Networks) == 1 && IsDefaultNetwork(o.Networks[0].Name) && o.Networks[0].IP == "" {
		o.Networks = nil
	}
}

// formatBytes renders a byte count in the --memory format.
func formatBytes(n int64) string {
	for _, u := range []struct {
		unit string
		size int64
	}{{"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}} {
		if n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.unit
		}
	}
	return strconv.FormatInt(n, 10) + "b"
}

// DropUnsupported clears the options caps does not support and returns
// their descriptions.
func (o *CreateContainerOpts) DropUnsupported(caps Capabilities) []string {
	var dropped []string
	for _, opt := range caps.Unsupported(o.UsedOptions()) {
		dropped = append(dropped, OptionLabels[opt])
		switch opt {
		case OptRestart:
			o.RestartPolicy = ""
		case OptLabel:
			o.Labels = nil
		case OptEntrypoint:
			o.Entrypoint = nil
		case OptWorkDir:
			o.WorkDir = ""
		case OptAddHost:
			o.ExtraHosts = nil
		case OptCapAdd:
			o.CapAdd = nil
		case OptCapDrop:
			o.CapDrop = nil
		case OptPrivileged:
			o.Privileged = false
		case OptNetworkAlias:
			o.Networks[0].Aliases, o.Networks[0].IP = nil, ""
		case OptMultiNetwork:
			o.Networks = o.Networks[:1]
		}
	}
	return dropped
}

// Recreate replaces container id by a new one created from opts. The old
// container is renamed and stopped first; if the new one cannot be
// created or started, it is removed and the old one is restored (unless
// it removed itself on stop because of --rm). With opts.Pull ==
// PullAlways the image is pulled before the old container is touched.
// Volumes are kept: the old container is removed without its volumes.
func Recreate(ctx context.Context, rt Runtime, id string, opts CreateContainerOpts, progress func(string)) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if err := opts.Validate(); err != nil {
		return "", err
	}
	raw, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return "", err
	}
	old, err := OptsFromInspect(raw, nil)
	if err != nil {
		return "", err
	}
	if opts.Pull == PullAlways {
		progress("Pulling " + opts.Image + " …")
		if err := rt.PullImage(ctx, opts.Image, progress); err != nil {
			return "", fmt.Errorf("pull %s: %w", opts.Image, err)
		}
		opts.Pull = PullMissing
	}

	b := make([]byte, 3)
	_, _ = rand.Read(b)
	tmp := old.Name + "-old-" + hex.EncodeToString(b)
	if len(tmp) > 128 {
		tmp = "old-" + hex.EncodeToString(b) + "-" + old.ID[:min(12, len(old.ID))]
	}
	progress(fmt.Sprintf("Renaming %s to %s", old.Name, tmp))
	if err := rt.RenameContainer(ctx, old.ID, tmp); err != nil {
		return "", fmt.Errorf("rename the old container: %w", err)
	}
	restore := func(cause error) error {
		progress("Restoring the old container")
		if _, err := rt.InspectContainer(ctx, old.ID); errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w; the old container removed itself when it stopped and could not be restored", cause)
		}
		var errs []error
		if err := rt.RenameContainer(ctx, old.ID, old.Name); err != nil {
			errs = append(errs, fmt.Errorf("rename back: %w", err))
		}
		if old.Running {
			if err := rt.StartContainer(ctx, old.ID); err != nil {
				errs = append(errs, fmt.Errorf("start: %w", err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("%w; restoring the old container %s failed: %v", cause, tmp, err)
		}
		return fmt.Errorf("%w; the old container was restored", cause)
	}
	if old.Running {
		progress("Stopping " + old.Name)
		if err := rt.StopContainer(ctx, old.ID, nil); err != nil {
			return "", restore(fmt.Errorf("stop the old container: %w", err))
		}
	}

	progress("Creating the new container")
	newID, err := rt.CreateContainer(ctx, opts)
	if err != nil {
		if newID != "" {
			if rerr := rt.RemoveContainer(ctx, newID, true); rerr != nil {
				progress("Could not remove the new container: " + rerr.Error())
			}
		}
		return "", restore(err)
	}
	progress("Removing the old container " + tmp)
	if err := rt.RemoveContainer(ctx, old.ID, false); err != nil && !errors.Is(err, ErrNotFound) {
		progress("Could not remove the old container " + tmp + ": " + err.Error())
	}
	progress("Done")
	return newID, nil
}
