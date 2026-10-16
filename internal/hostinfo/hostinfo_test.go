package hostinfo

import (
	"context"
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
	total, avail := parseMeminfo([]byte("MemTotal:       16303404 kB\nMemFree:  100 kB\nMemAvailable:    8151702 kB\n"))
	if total != 16303404*1024 || avail != 8151702*1024 {
		t.Errorf("got %d, %d", total, avail)
	}
}

func TestParseCPUInfo(t *testing.T) {
	model, n := parseCPUInfo([]byte("processor\t: 0\nmodel name\t: Intel(R) Xeon(R)   CPU\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R)   CPU\n"))
	if model != "Intel(R) Xeon(R) CPU" || n != 2 {
		t.Errorf("got %q, %d", model, n)
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
	t.Logf("%+v", info)
}
