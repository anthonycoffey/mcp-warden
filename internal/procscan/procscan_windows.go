//go:build windows

package procscan

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a running process.
const stillActive = 259

func snapshot() ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, err
	}
	var procs []Process
	for {
		p := Process{
			PID:  int(entry.ProcessID),
			PPID: int(entry.ParentProcessID),
			Name: windows.UTF16ToString(entry.ExeFile[:]),
		}
		fill(&p)
		procs = append(procs, p)
		if err := windows.Process32Next(snap, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return procs, nil
			}
			return procs, err
		}
	}
}

// fill adds the fields Toolhelp does not provide. Failure marks the process
// unreadable rather than failing the whole snapshot.
func fill(p *Process) {
	if p.PID == 0 {
		p.Unreadable = true // System Idle Process
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
	if err != nil {
		p.Unreadable = true
		return
	}
	defer windows.CloseHandle(h)

	ct, err := handleCreateTime(h)
	if err != nil {
		p.Unreadable = true
		return
	}
	p.CreateTime = ct
	cmd, err := commandLine(h)
	if err != nil {
		p.Unreadable = true
		return
	}
	p.Cmdline = cmd
	p.RSS = workingSet(h)
}

func handleCreateTime(h windows.Handle) (time.Time, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, created.Nanoseconds()), nil
}

// commandLine uses ProcessCommandLineInformation (Windows 8.1+), which only
// needs PROCESS_QUERY_LIMITED_INFORMATION — no PEB reads, no VM_READ.
func commandLine(h windows.Handle) (string, error) {
	size := uint32(1024)
	for range 4 {
		// []uint64 guarantees the alignment UNICODE_STRING needs.
		buf := make([]uint64, (size+7)/8)
		var ret uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation,
			unsafe.Pointer(&buf[0]), uint32(len(buf)*8), &ret)
		if err == nil {
			us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
			return us.String(), nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) &&
			!errors.Is(err, windows.STATUS_BUFFER_TOO_SMALL) &&
			!errors.Is(err, windows.STATUS_BUFFER_OVERFLOW) {
			return "", err
		}
		size = max(ret, size*2)
	}
	return "", errors.New("procscan: command line too large")
}

// vmCounters mirrors VM_COUNTERS (winternl.h), which x/sys does not define.
type vmCounters struct {
	PeakVirtualSize            uintptr
	VirtualSize                uintptr
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

func workingSet(h windows.Handle) uint64 {
	var vm vmCounters
	if err := windows.NtQueryInformationProcess(h, windows.ProcessVmCounters,
		unsafe.Pointer(&vm), uint32(unsafe.Sizeof(vm)), nil); err != nil {
		return 0
	}
	return uint64(vm.WorkingSetSize)
}

func createTime(pid int) (time.Time, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, ErrNotFound
	}
	defer windows.CloseHandle(h)
	if !running(h) {
		return time.Time{}, ErrNotFound
	}
	return handleCreateTime(h)
}

// running guards against handles to exited processes, which stay openable
// while anyone still holds a handle to them.
func running(h windows.Handle) bool {
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

func kill(pid int, created time.Time) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return ErrNotFound
		}
		return err
	}
	defer windows.CloseHandle(h)
	if !running(h) {
		return ErrNotFound
	}
	// Identity is checked on the same handle we kill through, so there is
	// no window for the PID to be reused in between.
	if !created.IsZero() {
		ct, err := handleCreateTime(h)
		if err != nil {
			return err
		}
		if !SameTime(ct, created) {
			return ErrIdentityMismatch
		}
	}
	return windows.TerminateProcess(h, 1)
}
