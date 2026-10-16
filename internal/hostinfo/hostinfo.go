// Package hostinfo describes the host ContainerUI runs on: operating
// system, resources and what matters for containers. Every field is best
// effort; an empty or zero field is unknown and not shown.
package hostinfo

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Info struct {
	Hostname       string
	OSName         string // "Ubuntu 24.04.1 LTS", "Windows 11 Enterprise"
	OSVersion      string // "24.04", "24H2"
	Build          string // Windows: "26200.1234"
	Kernel         string // Linux
	Arch           string
	Virtualization string

	CPUModel     string
	CPUs         int
	MemTotal     uint64 // bytes
	MemAvailable uint64
	BootTime     time.Time
	Uptime       time.Duration
	Load         [3]float64 // Linux
	HasLoad      bool

	SELinux string // Linux: enforcing, permissive, disabled
	Cgroup  string // Linux
	Systemd string // Linux
	WSL     string // Windows
	HyperV  string // Windows

	StorageMount string
	StorageTotal uint64
	StorageFree  uint64
}

// cmdTimeout bounds each helper command.
const cmdTimeout = 2 * time.Second

var (
	staticOnce sync.Once
	static     Info
)

// Collect returns the host information. What does not change while the
// host runs is gathered once; uptime, memory, load and storage every time.
func Collect(ctx context.Context) Info {
	staticOnce.Do(func() {
		static.Arch = goruntime.GOARCH
		static.CPUs = goruntime.NumCPU()
		static.Hostname, _ = os.Hostname()
		collectStatic(ctx, &static)
	})
	info := static
	collectDynamic(&info)
	if !info.BootTime.IsZero() && info.Uptime == 0 {
		info.Uptime = time.Since(info.BootTime)
	}
	if info.Uptime > 0 && info.BootTime.IsZero() {
		info.BootTime = time.Now().Add(-info.Uptime)
	}
	info.Uptime = info.Uptime.Truncate(time.Second)
	return info
}

// output runs a command with cmdTimeout and returns its trimmed output, or
// "" if it failed.
func output(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	prepare(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// parseOSRelease parses /etc/os-release (KEY=value, values maybe quoted).
func parseOSRelease(b []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		m[k] = v
	}
	return m
}

// parseMeminfo returns MemTotal and MemAvailable of /proc/meminfo in bytes.
func parseMeminfo(b []byte) (total, avail uint64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 2 && f[2] == "kB" {
			n *= 1024
		}
		switch f[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail = n
		}
	}
	return total, avail
}

// parseCPUInfo returns the first model name and the processor count of
// /proc/cpuinfo.
func parseCPUInfo(b []byte) (model string, count int) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			count++
		case "model name", "Model", "cpu model":
			if model == "" {
				model = strings.Join(strings.Fields(v), " ")
			}
		}
	}
	return model, count
}

// parseLoadavg returns the three load averages of /proc/loadavg.
func parseLoadavg(b []byte) ([3]float64, bool) {
	var l [3]float64
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return l, false
	}
	for i := range l {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return l, false
		}
		l[i] = v
	}
	return l, true
}

// parseUptime returns the first value of /proc/uptime.
func parseUptime(b []byte) time.Duration {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

// windowsProductName fixes the registry's ProductName, which still says
// "Windows 10" on Windows 11 (build 22000 and later).
func windowsProductName(name string, build int) string {
	if build >= 22000 && strings.HasPrefix(name, "Windows 10") {
		return "Windows 11" + strings.TrimPrefix(name, "Windows 10")
	}
	return name
}

// vmMarkers identify a virtual machine in the BIOS manufacturer or product.
var vmMarkers = []string{"virtual", "vmware", "qemu", "kvm", "xen", "parallels", "bochs"}

// virtualization names the hypervisor from the BIOS system manufacturer
// and product, or "bare metal (<maker> <product>)".
func virtualization(maker, product string) string {
	both := strings.TrimSpace(maker + " " + product)
	l := strings.ToLower(both)
	for _, m := range vmMarkers {
		if strings.Contains(l, m) {
			return both
		}
	}
	if both == "" {
		return ""
	}
	return "bare metal (" + both + ")"
}

// wslLine turns the localized first line of `wsl --version` ("WSL version:
// 2.3.26.0", "WSL-Version: 2.3.26.0") into the version.
func wslLine(s string) string {
	if _, v, ok := strings.Cut(s, ":"); ok {
		return strings.TrimSpace(v)
	}
	return s
}
