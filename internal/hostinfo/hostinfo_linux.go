package hostinfo

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

func prepare(*exec.Cmd) {}

func collectStatic(ctx context.Context, info *Info) {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		m := parseOSRelease(b)
		info.OSName = m["PRETTY_NAME"]
		if info.OSName == "" {
			info.OSName = strings.TrimSpace(m["NAME"] + " " + m["VERSION"])
		}
		info.OSVersion = m["VERSION_ID"]
	}
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		info.Kernel = unix.ByteSliceToString(u.Release[:])
		if m := unix.ByteSliceToString(u.Machine[:]); m != "" {
			info.Arch = m
		}
	}
	switch v := output(ctx, "systemd-detect-virt"); v {
	case "":
	case "none":
		info.Virtualization = "bare metal"
	default:
		info.Virtualization = v
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		model, n := parseCPUInfo(b)
		info.CPUModel = model
		if n > 0 {
			info.CPUs = n
		}
	}
	switch b, err := os.ReadFile("/sys/fs/selinux/enforce"); {
	case err != nil:
		info.SELinux = "disabled"
	case strings.TrimSpace(string(b)) == "1":
		info.SELinux = "enforcing"
	default:
		info.SELinux = "permissive"
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		info.Cgroup = "v2 (unified)"
	} else if _, err := os.Stat("/sys/fs/cgroup"); err == nil {
		info.Cgroup = "v1"
	}
	info.Systemd = firstLine(output(ctx, "systemctl", "--version"))
	for _, p := range []string{"/var/lib/docker", "/var/lib/containers/storage", "/"} {
		if _, err := os.Stat(p); err == nil {
			info.StorageMount = p
			break
		}
	}
}

func collectDynamic(info *Info) {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		info.Uptime = parseUptime(b)
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		info.MemTotal, info.MemAvailable = parseMeminfo(b)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		info.Load, info.HasLoad = parseLoadavg(b)
	}
	var st unix.Statfs_t
	if info.StorageMount != "" && unix.Statfs(info.StorageMount, &st) == nil {
		info.StorageTotal = st.Blocks * uint64(st.Bsize)
		info.StorageFree = st.Bavail * uint64(st.Bsize)
	}
}
