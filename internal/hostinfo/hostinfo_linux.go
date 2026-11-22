package hostinfo

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat times. It is 100 on
// every Linux architecture's user ABI.
const clockTicks = 100

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
		c := parseCPUInfo(b)
		info.CPUModel = c.Model
		if c.Logical > 0 {
			info.CPUs = c.Logical
		}
		info.Cores, info.Sockets, info.MaxMHz = c.Cores, c.Sockets, c.MHz
	}
	if b, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq"); err == nil {
		if khz, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && khz > 0 {
			info.MaxMHz = (khz + 500) / 1000
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
		m := parseMeminfo(b)
		info.MemTotal, info.MemAvailable = m.Total, m.Available
		info.SwapTotal, info.SwapFree = m.SwapTotal, m.SwapFree
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		info.Load, info.HasLoad = parseLoadavg(b)
	}
	if b, err := os.ReadFile("/proc/self/mounts"); err == nil {
		for _, m := range parseMounts(b) {
			var st unix.Statfs_t
			if unix.Statfs(m.Mount, &st) != nil || st.Blocks == 0 {
				continue
			}
			bs := uint64(st.Frsize)
			if bs == 0 {
				bs = uint64(st.Bsize)
			}
			info.Disks = append(info.Disks, Disk{
				Device: m.Device, Mount: m.Mount, FSType: m.FSType,
				Total: st.Blocks * bs, Free: st.Bavail * bs,
			})
		}
	}
}

func takeSnapshot() snapshot {
	s := snapshot{at: time.Now()}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		ps := parseProcStat(b)
		s.cpu, s.hasCPU, s.running = ps.CPU, ps.HasCPU, ps.Running
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return s
	}
	page := uint64(os.Getpagesize())
	s.procs = make([]procSample, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		st, ok := parsePIDStat(b)
		if !ok {
			continue
		}
		s.procs = append(s.procs, procSample{
			pid: pid, name: st.Name,
			cpu:     time.Duration(st.Ticks) * time.Second / clockTicks,
			rss:     st.RSSPages * page,
			threads: st.Threads, start: st.Start,
		})
	}
	return s
}

// maxCommand bounds the command line shown as a tooltip.
const maxCommand = 500

// procDetails returns the command line of a process and its owner's uid.
// Kernel threads have no command line.
func procDetails(pid int) (command, userKey string) {
	dir := "/proc/" + strconv.Itoa(pid)
	if b, err := os.ReadFile(dir + "/cmdline"); err == nil {
		command = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if len(command) > maxCommand {
			command = command[:maxCommand] + "…"
		}
	}
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			userKey = strconv.FormatUint(uint64(st.Uid), 10)
		}
	}
	return command, userKey
}

// lookupUser names a uid, or returns the uid if it has no name.
func lookupUser(uid string) string {
	if u, err := user.LookupId(uid); err == nil {
		return u.Username
	}
	return uid
}
