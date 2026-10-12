//go:build darwin

package procscan

import (
	"bytes"
	"encoding/binary"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func snapshot() ([]Process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	procs := make([]Process, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
		p := Process{
			PID:        int(kp.Proc.P_pid),
			PPID:       int(kp.Eproc.Ppid),
			Name:       commName(kp.Proc.P_comm[:]),
			CreateTime: startTime(kp),
			// RSS needs proc_pidinfo from libproc, which is only reachable
			// through cgo; it is left as 0 (unknown) on macOS.
		}
		if args, err := procArgs(p.PID); err == nil {
			p.Cmdline = args
		} else {
			p.Unreadable = true
		}
		procs = append(procs, p)
	}
	return procs, nil
}

func commName(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func startTime(kp *unix.KinfoProc) time.Time {
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*1000)
}

// procArgs reads KERN_PROCARGS2: int32 argc, the exec path, NUL padding,
// then argc NUL-terminated arguments (followed by the environment, ignored).
func procArgs(pid int) (string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(buf) < 4 {
		return "", syscall.EINVAL
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	// Skip the exec path and its NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return "", syscall.EINVAL
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			args = append(args, string(rest))
			break
		}
		args = append(args, string(rest[:j]))
		rest = rest[j+1:]
	}
	return strings.Join(args, " "), nil
}

func createTime(pid int) (time.Time, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return time.Time{}, ErrNotFound
	}
	return startTime(kp), nil
}

func kill(pid int, created time.Time) error {
	ct, err := createTime(pid)
	if err != nil {
		return err
	}
	if !created.IsZero() && !SameTime(ct, created) {
		return ErrIdentityMismatch
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return ErrNotFound
		}
		return err
	}
	return nil
}
