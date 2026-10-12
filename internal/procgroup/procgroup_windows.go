//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/anthonycoffey/mcp-warden/internal/procscan"
)

type sysGroup struct {
	job windows.Handle
}

func (g *Group) start() error {
	job, err := newKillOnCloseJob()
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	if g.cmd.SysProcAttr == nil {
		g.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	g.cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := g.cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	g.sys.job = job

	pid := uint32(g.cmd.Process.Pid)
	if err := assign(job, pid); err != nil {
		// Nested jobs are supported since Windows 8, so this is rare: keep
		// going and fall back to tree-walking kills.
		g.Degraded = fmt.Errorf("assign to job object: %w", err)
	}
	if err := resume(pid); err != nil {
		_ = g.cmd.Process.Kill()
		return fmt.Errorf("resume child: %w", err)
	}
	return nil
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func assign(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(job, h)
}

// resume wakes every thread of a CREATE_SUSPENDED process. exec.Cmd does not
// expose the primary thread handle, so the threads are found via Toolhelp.
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if resumed == 0 {
		return errors.New("no threads found for child process")
	}
	return nil
}

func (g *Group) kill() error {
	if g.Degraded != nil {
		return killTree(g.cmd.Process.Pid)
	}
	if g.sys.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.sys.job, 1)
}

func (g *Group) close() error {
	if g.sys.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.sys.job)
	g.sys.job = 0
	return err
}

func killTree(pid int) error {
	procs, err := procscan.Snapshot()
	if err != nil {
		return err
	}
	idx := procscan.NewIndex(procs)
	root, ok := idx.ByPID[pid]
	if !ok {
		return nil
	}
	_, err = procscan.KillTree(idx, root)
	return err
}
