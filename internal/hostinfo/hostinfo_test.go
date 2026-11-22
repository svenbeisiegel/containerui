package hostinfo

import (
	"context"
	"fmt"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestParseOSRelease(t *testing.T) {
	m := parseOSRelease([]byte("# comment\nNAME=\"Rocky Linux\"\nVERSION_ID='9.4'\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"\nID=rocky\n"))
	if m["PRETTY_NAME"] != "Rocky Linux 9.4 (Blue Onyx)" || m["VERSION_ID"] != "9.4" || m["ID"] != "rocky" {
		t.Errorf("got %q", m)
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo([]byte("MemTotal:       16303404 kB\nMemFree:  100 kB\nMemAvailable:    8151702 kB\nSwapTotal: 2097148 kB\nSwapFree: 1048574 kB\n"))
	want := memInfo{Total: 16303404 * 1024, Available: 8151702 * 1024, SwapTotal: 2097148 * 1024, SwapFree: 1048574 * 1024}
	if m != want {
		t.Errorf("got %+v, want %+v", m, want)
	}
}

func TestParseCPUInfo(t *testing.T) {
	// Two sockets with two cores each, hyper-threaded: 8 logical CPUs.
	var b strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, "processor\t: %d\nmodel name\t: Intel(R) Xeon(R)   CPU\ncpu MHz\t\t: 2199.998\nphysical id\t: %d\ncore id\t\t: %d\ncpu cores\t: 2\n\n", i, i/4, i%2)
	}
	c := parseCPUInfo([]byte(b.String()))
	want := cpuInfo{Model: "Intel(R) Xeon(R) CPU", Logical: 8, Cores: 4, Sockets: 2, MHz: 2200}
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
	// ARM: no topology lines.
	c = parseCPUInfo([]byte("processor\t: 0\nBogoMIPS\t: 48.00\n\nprocessor\t: 1\nBogoMIPS\t: 48.00\n"))
	if c.Logical != 2 || c.Cores != 0 || c.Sockets != 0 {
		t.Errorf("arm: %+v", c)
	}
}

func TestParseProcStat(t *testing.T) {
	s := parseProcStat([]byte("cpu  100 5 50 800 20 3 2 0 7 0\ncpu0 50 2 25 400 10 1 1 0 0 0\nintr 1 2 3\nprocs_running 3\nprocs_blocked 0\n"))
	if !s.HasCPU || s.CPU.total != 980 || s.CPU.idle != 820 || s.Running != 3 {
		t.Errorf("got %+v", s)
	}
	if p, ok := cpuPercent(cpuTimes{total: 1000, idle: 800}, cpuTimes{total: 1400, idle: 1100}); !ok || p != 25 {
		t.Errorf("cpuPercent = %v, %v", p, ok)
	}
	if _, ok := cpuPercent(cpuTimes{total: 1000}, cpuTimes{total: 1000}); ok {
		t.Error("no elapsed time accepted")
	}
}

func TestParsePIDStat(t *testing.T) {
	line := "1234 ((sd-pam) x)) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 3 0 98765 12345678 2048 18446744073709551615"
	s, ok := parsePIDStat([]byte(line))
	want := pidStat{Name: "(sd-pam) x)", Ticks: 300, Threads: 3, Start: 98765, RSSPages: 2048}
	if !ok || s != want {
		t.Errorf("got %+v, %v; want %+v", s, ok, want)
	}
	if _, ok := parsePIDStat([]byte("1 (init")); ok {
		t.Error("truncated stat accepted")
	}
}

func TestParseMounts(t *testing.T) {
	in := `/dev/mapper/rl-root / xfs rw,seclabel,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sda1 /boot xfs rw 0 0
/dev/mapper/rl-root /var/lib/containers/storage/overlay xfs rw 0 0
overlay /var/lib/containers/storage/overlay/abc/merged overlay rw 0 0
/dev/loop0 /snap/core/1 squashfs ro 0 0
/dev/sdb1 /mnt/my\040disk ext4 rw 0 0
tank/data /tank/data zfs rw 0 0
`
	got := parseMounts([]byte(in))
	want := []mountEntry{
		{"/dev/mapper/rl-root", "/", "xfs"},
		{"/dev/sda1", "/boot", "xfs"},
		{"/dev/sdb1", "/mnt/my disk", "ext4"},
		{"tank/data", "/tank/data", "zfs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestMarkContainers(t *testing.T) {
	disks := []Disk{{Mount: "/"}, {Mount: "/var"}, {Mount: "/var/lib/docker2"}}
	markContainers(disks, "/var/lib/docker")
	if disks[0].Containers || !disks[1].Containers || disks[2].Containers {
		t.Errorf("linux: %+v", disks)
	}
	win := []Disk{{Mount: `C:\`}, {Mount: `D:\`}}
	markContainers(win, `c:\ProgramData\docker`)
	if !win[0].Containers || win[1].Containers {
		t.Errorf("windows: %+v", win)
	}
}

func TestProcessUsageTop(t *testing.T) {
	prev := []procSample{
		{pid: 1, name: "a", cpu: time.Second, start: 10},
		{pid: 2, name: "b", cpu: 5 * time.Second, start: 20},
		{pid: 3, name: "old", cpu: 9 * time.Second, start: 30},
	}
	cur := []procSample{
		{pid: 1, name: "a", cpu: 1500 * time.Millisecond, rss: 100, start: 10}, // 0.5s of 1s: 50%
		{pid: 2, name: "b", cpu: 5 * time.Second, rss: 900, start: 20},         // idle
		{pid: 3, name: "new", cpu: 200 * time.Millisecond, rss: 50, start: 31}, // PID reused: 20%
		{pid: 4, name: "c", cpu: 2 * time.Second, rss: 10, start: 40},          // new, multi-threaded: 200%
	}
	procs := processUsage(prev, cur, time.Second)
	top := topN(procs, 3, byCPU)
	if len(top) != 3 || top[0].PID != 4 || top[0].CPU != 200 || top[1].PID != 1 || top[1].CPU != 50 || top[2].PID != 3 || top[2].CPU != 20 {
		t.Errorf("by CPU: %+v", top)
	}
	mem := topN(procs, 2, byMem)
	if len(mem) != 2 || mem[0].PID != 2 || mem[1].PID != 1 {
		t.Errorf("by memory: %+v", mem)
	}
}

func TestParseLoadavgUptime(t *testing.T) {
	l, ok := parseLoadavg([]byte("0.52 0.58 0.59 1/389 12345\n"))
	if !ok || l != [3]float64{0.52, 0.58, 0.59} {
		t.Errorf("load = %v, %v", l, ok)
	}
	if _, ok := parseLoadavg([]byte("x")); ok {
		t.Error("bad loadavg accepted")
	}
	if d := parseUptime([]byte("3600.50 7000.00\n")); d != 3600*time.Second+500*time.Millisecond {
		t.Errorf("uptime = %v", d)
	}
}

func TestWindowsProductName(t *testing.T) {
	cases := []struct {
		name  string
		build int
		want  string
	}{
		{"Windows 10 Enterprise", 26200, "Windows 11 Enterprise"},
		{"Windows 10 Pro", 19045, "Windows 10 Pro"},
		{"Windows Server 2022 Datacenter", 20348, "Windows Server 2022 Datacenter"},
	}
	for _, c := range cases {
		if got := windowsProductName(c.name, c.build); got != c.want {
			t.Errorf("windowsProductName(%q, %d) = %q, want %q", c.name, c.build, got, c.want)
		}
	}
}

func TestVirtualizationWSL(t *testing.T) {
	if got := virtualization("Microsoft Corporation", "Virtual Machine"); got != "Microsoft Corporation Virtual Machine" {
		t.Errorf("hyper-v: %q", got)
	}
	if got := virtualization("Dell Inc.", "Latitude"); got != "bare metal (Dell Inc. Latitude)" {
		t.Errorf("dell: %q", got)
	}
	if got := wslLine("WSL-Version: 2.3.26.0"); got != "2.3.26.0" {
		t.Errorf("wsl: %q", got)
	}
}

func TestCollect(t *testing.T) {
	info := Collect(context.Background())
	if info.Hostname == "" || info.OSName == "" || info.CPUs == 0 {
		t.Errorf("incomplete: %+v", info)
	}
	if goruntime.GOOS == "linux" || goruntime.GOOS == "windows" {
		if len(info.Disks) == 0 || info.Procs == 0 || !info.HasCPUUsage || len(info.TopCPU) == 0 || len(info.TopMem) == 0 {
			t.Errorf("no usage: %+v", info)
		}
	}
	t.Logf("%+v", info)
}
