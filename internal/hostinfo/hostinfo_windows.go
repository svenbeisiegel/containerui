package hostinfo

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// prepare keeps a console window from flashing up for a helper command.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func collectStatic(ctx context.Context, info *Info) {
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		product := regString(k, "ProductName")
		build := regString(k, "CurrentBuild")
		n, _ := strconv.Atoi(build)
		info.OSName = windowsProductName(product, n)
		info.OSVersion = regString(k, "DisplayVersion")
		if ubr, _, err := k.GetIntegerValue("UBR"); err == nil && build != "" {
			build += "." + strconv.FormatUint(ubr, 10)
		}
		info.Build = build
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE); err == nil {
		info.Virtualization = virtualization(regString(k, "SystemManufacturer"), regString(k, "SystemProductName"))
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		info.CPUModel = strings.Join(strings.Fields(regString(k, "ProcessorNameString")), " ")
		if mhz, _, err := k.GetIntegerValue("~MHz"); err == nil {
			info.MaxMHz = int(mhz)
		}
		k.Close()
	}
	info.Cores, info.Sockets = coresSockets()
	info.WSL = wslVersion(ctx)
	info.HyperV = serviceState("vmcompute")
	if d := os.Getenv("SystemDrive"); d != "" {
		info.StorageMount = d + `\`
	} else {
		info.StorageMount = `C:\`
	}
}

func regString(k registry.Key, name string) string {
	s, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// wslVersion is the first line of `wsl --version`, e.g. "WSL version:
// 2.3.26.0". WSL_UTF8 makes wsl.exe write UTF-8 instead of UTF-16.
func wslVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", "--version")
	prepare(cmd)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	s := strings.ReplaceAll(string(out), "\x00", "")
	return wslLine(firstLine(strings.TrimPrefix(s, "\uFEFF")))
}

// serviceState describes the Host Compute Service that Windows containers
// and WSL run on, or "" if it is not installed.
func serviceState(name string) string {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return ""
	}
	defer windows.CloseServiceHandle(m)
	n, _ := windows.UTF16PtrFromString(name)
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return ""
	}
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS
	if windows.QueryServiceStatus(s, &st) != nil {
		return "Host Compute Service installed"
	}
	if st.CurrentState == windows.SERVICE_RUNNING {
		return "Host Compute Service running"
	}
	return "Host Compute Service installed, not running"
}

var (
	kernel32                           = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64                 = kernel32.NewProc("GetTickCount64")
	procGlobalMemoryStatusEx           = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes                 = kernel32.NewProc("GetSystemTimes")
	procK32GetProcessMemoryInfo        = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGetLogicalProcessorInformation = kernel32.NewProc("GetLogicalProcessorInformation")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func collectDynamic(info *Info) {
	if ms, _, _ := procGetTickCount64.Call(); ms > 0 {
		info.Uptime = time.Duration(ms) * time.Millisecond
	}
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); ok != 0 {
		info.MemTotal, info.MemAvailable = m.TotalPhys, m.AvailPhys
		// The commit limit is RAM plus the page files.
		info.SwapTotal = sub(m.TotalPageFile, m.TotalPhys)
		info.SwapFree = min(sub(m.AvailPageFile, m.AvailPhys), info.SwapTotal)
	}
	info.Disks = fixedDrives()
}

// fixedDrives returns the local fixed drives (no removable, network or
// optical drives).
func fixedDrives() []Disk {
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil || n == 0 || int(n) > len(buf) {
		return nil
	}
	var disks []Disk
	// The buffer holds NUL-terminated roots, `C:\` NUL `D:\` NUL; n counts both NULs.
	for start, i := 0, 0; i < int(n); i++ {
		if buf[i] != 0 {
			continue
		}
		root := windows.UTF16ToString(buf[start:i])
		start = i + 1
		if root == "" {
			continue
		}
		p, err := windows.UTF16PtrFromString(root)
		if err != nil || windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		d := Disk{Device: strings.TrimRight(root, `\`), Mount: root}
		label := make([]uint16, windows.MAX_PATH+1)
		fs := make([]uint16, windows.MAX_PATH+1)
		if windows.GetVolumeInformation(p, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))) == nil {
			d.Label, d.FSType = windows.UTF16ToString(label), windows.UTF16ToString(fs)
		}
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) != nil || total == 0 {
			continue
		}
		d.Total, d.Free = total, free
		disks = append(disks, d)
	}
	return disks
}

// logicalProcessorInfo is SYSTEM_LOGICAL_PROCESSOR_INFORMATION; the union
// is two ULONGLONGs, aligned like them.
type logicalProcessorInfo struct {
	ProcessorMask uintptr
	Relationship  uint32
	_             [2]uint64
}

const (
	relationProcessorCore    = 0
	relationProcessorPackage = 3
)

// coresSockets counts the physical cores and processor packages.
func coresSockets() (cores, sockets int) {
	var size uint32
	procGetLogicalProcessorInformation.Call(0, uintptr(unsafe.Pointer(&size)))
	one := uint32(unsafe.Sizeof(logicalProcessorInfo{}))
	if size < one {
		return 0, 0
	}
	buf := make([]logicalProcessorInfo, size/one)
	if ok, _, _ := procGetLogicalProcessorInformation.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); ok == 0 {
		return 0, 0
	}
	for _, e := range buf[:size/one] {
		switch e.Relationship {
		case relationProcessorCore:
			cores++
		case relationProcessorPackage:
			sockets++
		}
	}
	return cores, sockets
}

func filetime(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func takeSnapshot() snapshot {
	s := snapshot{at: time.Now()}
	var idle, kernel, user windows.Filetime
	if ok, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok != 0 {
		// Kernel time includes idle time.
		s.cpu = cpuTimes{total: filetime(kernel) + filetime(user), idle: filetime(idle)}
		s.hasCPU = true
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return s
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == 0 {
			continue // System Idle Process: its CPU time is idle time
		}
		p := procSample{pid: int(e.ProcessID), name: windows.UTF16ToString(e.ExeFile[:]), threads: int(e.Threads)}
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID); err == nil {
			var created, exited, k, u windows.Filetime
			if windows.GetProcessTimes(h, &created, &exited, &k, &u) == nil {
				p.start = filetime(created)
				p.cpu = time.Duration(filetime(k)+filetime(u)) * 100
			}
			mc := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
			if ok, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); ok != 0 {
				p.rss = uint64(mc.WorkingSetSize)
			}
			windows.CloseHandle(h)
		}
		s.procs = append(s.procs, p)
	}
	return s
}

// procDetails returns the executable path of a process and its owner's
// SID. Reading another process's command line needs more rights than the
// path.
func procDetails(pid int) (command, userKey string) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
		command = windows.UTF16ToString(buf[:n])
	}
	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) == nil {
		if tu, err := tok.GetTokenUser(); err == nil {
			userKey = tu.User.Sid.String()
		}
		tok.Close()
	}
	return command, userKey
}

// lookupUser names a SID as DOMAIN\account, or returns the SID.
func lookupUser(sid string) string {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return sid
	}
	account, domain, _, err := s.LookupAccount("")
	if err != nil {
		return sid
	}
	if domain != "" {
		return domain + `\` + account
	}
	return account
}
