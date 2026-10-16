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
		k.Close()
	}
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
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
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
	}
	if p, err := windows.UTF16PtrFromString(info.StorageMount); err == nil {
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) == nil {
			info.StorageTotal, info.StorageFree = total, free
		}
	}
}
