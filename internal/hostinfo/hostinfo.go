// Package hostinfo describes the host ContainerUI runs on: operating
// system, hardware, current usage and what matters for containers. Every
// field is best effort; an empty or zero field is unknown and not shown.
package hostinfo

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	goruntime "runtime"
	"sort"
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

	CPUModel    string
	CPUs        int // logical processors
	Cores       int // physical cores
	Sockets     int
	MaxMHz      int
	CPUUsage    float64 // percent of all logical processors, 0–100
	HasCPUUsage bool

	MemTotal     uint64 // bytes
	MemAvailable uint64
	SwapTotal    uint64 // Windows: the page file
	SwapFree     uint64

	BootTime time.Time
	Uptime   time.Duration
	Load     [3]float64 // Linux
	HasLoad  bool

	Procs        int // processes
	ProcsRunning int // Linux: runnable tasks
	Threads      int
	TopCPU       []Process // the processes using the most CPU
	TopMem       []Process // the processes using the most memory

	Disks []Disk

	SELinux string // Linux: enforcing, permissive, disabled
	Cgroup  string // Linux
	Systemd string // Linux
	WSL     string // Windows
	HyperV  string // Windows

	// StorageMount is where the container runtime keeps its data; the disk
	// that holds it is marked Containers.
	StorageMount string
}

// MemUsed is the memory in use: total minus available.
func (i Info) MemUsed() uint64 { return sub(i.MemTotal, i.MemAvailable) }

// SwapUsed is the swap (page file) in use.
func (i Info) SwapUsed() uint64 { return sub(i.SwapTotal, i.SwapFree) }

// ContainerDisk is the disk that holds the container storage, or nil.
func (i Info) ContainerDisk() *Disk {
	for k := range i.Disks {
		if i.Disks[k].Containers {
			return &i.Disks[k]
		}
	}
	return nil
}

// Disk is a mounted filesystem (Linux) or a fixed drive (Windows).
type Disk struct {
	Device     string // "/dev/sda2", "C:"
	Mount      string // "/var", `C:\`
	FSType     string // "xfs", "NTFS"
	Label      string // Windows: volume label
	Total      uint64 // bytes
	Free       uint64 // available to unprivileged users
	Containers bool   // holds the container storage
}

// Used is the space in use.
func (d Disk) Used() uint64 { return sub(d.Total, d.Free) }

// Process is one of the busiest processes.
type Process struct {
	PID     int
	Name    string
	Command string // command line, if readable
	User    string
	CPU     float64 // percent of one logical processor, like top
	RSS     uint64  // resident memory in bytes
	Threads int
}

func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// cmdTimeout bounds each helper command.
const cmdTimeout = 2 * time.Second

const (
	// topCount is how many processes each top list holds.
	topCount = 10
	// cacheFor lets several open pages share one reading.
	cacheFor = 2 * time.Second
	// A previous reading between minGap and maxGap old is the base for
	// CPU usage; otherwise Collect reads twice, shortSample apart.
	minGap      = 500 * time.Millisecond
	maxGap      = time.Minute
	shortSample = 250 * time.Millisecond
)

var (
	staticOnce sync.Once
	static     Info
)

// sampler keeps the previous reading, so CPU usage is measured over the
// time between two page refreshes.
var sampler struct {
	mu       sync.Mutex
	prev     *snapshot
	cached   Info
	cachedAt time.Time
	users    map[string]string // user key (uid, SID) → name
}

// Collect returns the host information. What does not change while the
// host runs is gathered once; usage numbers on every call, at most every
// cacheFor.
func Collect(ctx context.Context) Info {
	staticOnce.Do(func() {
		static.Arch = goruntime.GOARCH
		static.CPUs = goruntime.NumCPU()
		static.Hostname, _ = os.Hostname()
		collectStatic(ctx, &static)
	})
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	if !sampler.cachedAt.IsZero() && time.Since(sampler.cachedAt) < cacheFor {
		return sampler.cached
	}
	info := static
	collectDynamic(&info)
	sample(&info)
	markContainers(info.Disks, info.StorageMount)
	if !info.BootTime.IsZero() && info.Uptime == 0 {
		info.Uptime = time.Since(info.BootTime)
	}
	if info.Uptime > 0 && info.BootTime.IsZero() {
		info.BootTime = time.Now().Add(-info.Uptime)
	}
	info.Uptime = info.Uptime.Truncate(time.Second)
	sampler.cached, sampler.cachedAt = info, time.Now()
	return info
}

// snapshot is one reading of the CPU counters and the processes.
type snapshot struct {
	at      time.Time
	cpu     cpuTimes
	hasCPU  bool
	running int // Linux: runnable tasks
	procs   []procSample
}

// cpuTimes are the host's CPU counters, in any unit.
type cpuTimes struct{ total, idle uint64 }

type procSample struct {
	pid     int
	name    string
	cpu     time.Duration // user + system time so far
	rss     uint64
	threads int
	start   uint64 // start time, tells a reused PID apart
}

// sample fills the CPU usage and the process numbers. It must be called
// with sampler.mu held.
func sample(info *Info) {
	cur := takeSnapshot()
	if !cur.hasCPU && len(cur.procs) == 0 {
		return // nothing to measure on this platform
	}
	prev := sampler.prev
	if gap := cur.at.Sub(prevAt(prev)); prev == nil || gap < minGap || gap > maxGap {
		time.Sleep(shortSample)
		first := cur
		prev, cur = &first, takeSnapshot()
	}
	sampler.prev = &cur

	if cur.hasCPU && prev.hasCPU {
		info.CPUUsage, info.HasCPUUsage = cpuPercent(prev.cpu, cur.cpu)
	}
	info.Procs = len(cur.procs)
	info.ProcsRunning = cur.running
	for _, p := range cur.procs {
		info.Threads += p.threads
	}
	procs := processUsage(prev.procs, cur.procs, cur.at.Sub(prev.at))
	info.TopCPU = topN(procs, topCount, byCPU)
	info.TopMem = topN(procs, topCount, byMem)
	details := map[int]Process{}
	for _, list := range [][]Process{info.TopCPU, info.TopMem} {
		for k := range list {
			p := &list[k]
			d, ok := details[p.PID]
			if !ok {
				var key string
				d.Command, key = procDetails(p.PID)
				d.User = userName(key)
				details[p.PID] = d
			}
			p.Command, p.User = d.Command, d.User
		}
	}
}

func prevAt(s *snapshot) time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.at
}

// userName turns a platform user key into a name, cached.
func userName(key string) string {
	if key == "" {
		return ""
	}
	if n, ok := sampler.users[key]; ok {
		return n
	}
	if sampler.users == nil {
		sampler.users = map[string]string{}
	}
	n := lookupUser(key)
	sampler.users[key] = n
	return n
}

// cpuPercent is the busy share of the time between two readings, 0–100.
func cpuPercent(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	total := float64(cur.total - prev.total)
	idle := float64(cur.idle - prev.idle)
	return clamp((total-idle)/total*100, 0, 100), true
}

// processUsage turns two readings into processes with their CPU usage
// over elapsed. A process that is not in prev started in between, so all
// its CPU time falls into the interval.
func processUsage(prev, cur []procSample, elapsed time.Duration) []Process {
	before := make(map[int]procSample, len(prev))
	for _, p := range prev {
		before[p.pid] = p
	}
	out := make([]Process, 0, len(cur))
	for _, p := range cur {
		used := p.cpu
		if b, ok := before[p.pid]; ok && b.start == p.start {
			used -= b.cpu
		}
		cpu := 0.0
		if elapsed > 0 && used > 0 {
			cpu = float64(used) / float64(elapsed) * 100
		}
		out = append(out, Process{PID: p.pid, Name: p.name, CPU: cpu, RSS: p.rss, Threads: p.threads})
	}
	return out
}

func byCPU(a, b Process) bool {
	if a.CPU != b.CPU {
		return a.CPU > b.CPU
	}
	if a.RSS != b.RSS {
		return a.RSS > b.RSS
	}
	return a.PID < b.PID
}

func byMem(a, b Process) bool {
	if a.RSS != b.RSS {
		return a.RSS > b.RSS
	}
	if a.CPU != b.CPU {
		return a.CPU > b.CPU
	}
	return a.PID < b.PID
}

// topN returns the first n processes in the order of less, as a copy.
func topN(procs []Process, n int, less func(a, b Process) bool) []Process {
	s := append([]Process(nil), procs...)
	sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func clamp(v, lo, hi float64) float64 {
	return min(max(v, lo), hi)
}

// markContainers marks the disk whose mount point holds dir, the longest
// match winning.
func markContainers(disks []Disk, dir string) {
	if dir == "" {
		return
	}
	best, bestLen := -1, -1
	for k, d := range disks {
		if under(dir, d.Mount) && len(d.Mount) > bestLen {
			best, bestLen = k, len(d.Mount)
		}
	}
	if best >= 0 {
		disks[best].Containers = true
	}
}

// under reports whether path is mount or below it. Windows drive letters
// compare without case.
func under(path, mount string) bool {
	m := strings.TrimRight(mount, `/\`)
	if m == "" {
		return strings.HasPrefix(path, "/")
	}
	p, ml := strings.ToLower(path), strings.ToLower(m)
	if p == ml {
		return true
	}
	return strings.HasPrefix(p, ml) && (p[len(ml)] == '/' || p[len(ml)] == '\\')
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

// memInfo is what the page shows of /proc/meminfo, in bytes.
type memInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

// parseMeminfo parses /proc/meminfo.
func parseMeminfo(b []byte) memInfo {
	var m memInfo
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
			m.Total = n
		case "MemAvailable:":
			m.Available = n
		case "SwapTotal:":
			m.SwapTotal = n
		case "SwapFree:":
			m.SwapFree = n
		}
	}
	return m
}

// cpuInfo is what the page shows of /proc/cpuinfo.
type cpuInfo struct {
	Model          string
	Logical        int
	Cores, Sockets int // 0: unknown (most ARM hosts)
	MHz            int // current clock of the first processor
}

// parseCPUInfo parses /proc/cpuinfo. Cores are the distinct (physical
// id, core id) pairs, sockets the distinct physical ids.
func parseCPUInfo(b []byte) cpuInfo {
	var c cpuInfo
	sockets := map[string]bool{}
	cores := map[string]bool{}
	var phys, perSocket string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "processor":
			c.Logical++
		case "model name", "Model", "cpu model":
			if c.Model == "" {
				c.Model = strings.Join(strings.Fields(v), " ")
			}
		case "physical id":
			phys = v
			sockets[v] = true
		case "core id":
			cores[phys+"/"+v] = true
		case "cpu cores":
			perSocket = v
		case "cpu MHz":
			if c.MHz == 0 {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					c.MHz = int(f + 0.5)
				}
			}
		}
	}
	c.Sockets = len(sockets)
	c.Cores = len(cores)
	if c.Cores == 0 {
		if n, err := strconv.Atoi(perSocket); err == nil {
			c.Cores = n * max(c.Sockets, 1)
		}
	}
	return c
}

// procStat is what the page uses of /proc/stat.
type procStat struct {
	CPU     cpuTimes
	HasCPU  bool
	Running int
}

// parseProcStat parses /proc/stat: the aggregate cpu line (guest time is
// already part of user time) and procs_running.
func parseProcStat(b []byte) procStat {
	var s procStat
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "cpu":
			var v [8]uint64
			n := 0
			for i := 1; i < len(f) && n < len(v); i++ {
				x, err := strconv.ParseUint(f[i], 10, 64)
				if err != nil {
					break
				}
				v[n] = x
				n++
			}
			if n < 4 {
				continue
			}
			for _, x := range v[:n] {
				s.CPU.total += x
			}
			s.CPU.idle = v[3] + v[4] // idle + iowait
			s.HasCPU = true
		case "procs_running":
			s.Running, _ = strconv.Atoi(f[1])
		}
	}
	return s
}

// pidStat is what the page uses of /proc/<pid>/stat.
type pidStat struct {
	Name     string
	Ticks    uint64 // utime + stime, in clock ticks
	Threads  int
	RSSPages uint64
	Start    uint64 // starttime, in clock ticks since boot
}

// parsePIDStat parses /proc/<pid>/stat. The name may hold spaces and
// parentheses, so it ends at the last ')'.
func parsePIDStat(b []byte) (pidStat, bool) {
	s := string(b)
	open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return pidStat{}, false
	}
	// f[0] is field 3 (state) of proc(5).
	f := strings.Fields(s[close+1:])
	if len(f) < 22 {
		return pidStat{}, false
	}
	num := func(i int) uint64 {
		n, _ := strconv.ParseUint(f[i], 10, 64)
		return n
	}
	return pidStat{
		Name:     s[open+1 : close],
		Ticks:    num(11) + num(12),
		Threads:  int(num(17)),
		Start:    num(19),
		RSSPages: num(21),
	}, true
}

// mountEntry is a line of /proc/self/mounts.
type mountEntry struct{ Device, Mount, FSType string }

// skipFS are filesystem types on block devices that are not disks to
// show: read-only images such as snaps.
var skipFS = map[string]bool{"squashfs": true, "iso9660": true, "udf": true}

// parseMounts returns the disks of /proc/self/mounts: filesystems on a
// /dev device (and ZFS datasets), one per device, at its shortest mount
// path, sorted by mount path. Pseudo filesystems, overlay and tmpfs are
// left out.
func parseMounts(b []byte) []mountEntry {
	byDev := map[string]mountEntry{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		e := mountEntry{Device: unescapeMount(f[0]), Mount: unescapeMount(f[1]), FSType: f[2]}
		if skipFS[e.FSType] || !(strings.HasPrefix(e.Device, "/dev/") || e.FSType == "zfs") {
			continue
		}
		if old, ok := byDev[e.Device]; ok && len(old.Mount) <= len(e.Mount) {
			continue
		}
		byDev[e.Device] = e
	}
	out := make([]mountEntry, 0, len(byDev))
	for _, e := range byDev {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// unescapeMount undoes the octal escapes of /proc/self/mounts ("\040" for
// a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
