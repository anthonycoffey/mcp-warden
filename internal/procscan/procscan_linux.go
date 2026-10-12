//go:build linux

package procscan

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// clockTicks is USER_HZ. It is 100 on every mainstream Linux architecture;
// reading it properly needs sysconf(3), which would require cgo.
const clockTicks = 100

var (
	bootOnce sync.Once
	bootTime time.Time
)

func boot() time.Time {
	bootOnce.Do(func() {
		data, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "btime "); ok {
				if secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
					bootTime = time.Unix(secs, 0)
				}
			}
		}
	})
	return bootTime
}

func snapshot() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := read(pid)
		if err != nil {
			continue // exited between ReadDir and read
		}
		procs = append(procs, p)
	}
	return procs, nil
}

type stat struct {
	ppid      int
	state     byte
	startTime time.Time
	rssPages  uint64
}

// parseStat parses /proc/<pid>/stat. The comm field may contain spaces and
// parentheses, so fields are counted from the last ')'.
func parseStat(data []byte) (stat, bool) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 > len(data) {
		return stat{}, false
	}
	f := strings.Fields(string(data[end+2:]))
	// f[0]=state(3) f[1]=ppid(4) … f[19]=starttime(22) f[21]=rss(24)
	if len(f) < 22 {
		return stat{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	ticks, _ := strconv.ParseUint(f[19], 10, 64)
	rss, _ := strconv.ParseUint(f[21], 10, 64)
	start := boot().Add(time.Duration(ticks) * time.Second / clockTicks)
	return stat{ppid: ppid, state: f[0][0], startTime: start, rssPages: rss}, true
}

func read(pid int) (Process, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Process{}, err
	}
	st, ok := parseStat(data)
	if !ok {
		return Process{}, ErrNotFound
	}
	p := Process{
		PID:        pid,
		PPID:       st.ppid,
		CreateTime: st.startTime,
		RSS:        st.rssPages * uint64(os.Getpagesize()),
	}
	if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		p.Name = strings.TrimSpace(string(comm))
	}
	// An unreadable command line marks the process; it does not fail the
	// snapshot.
	if cmd, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.Cmdline = strings.TrimSpace(string(bytes.ReplaceAll(cmd, []byte{0}, []byte{' '})))
	} else {
		p.Unreadable = true
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		p.Name = filepath.Base(exe)
	}
	return p, nil
}

func createTime(pid int) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, ErrNotFound
	}
	st, ok := parseStat(data)
	if !ok || st.state == 'Z' || st.state == 'X' {
		return time.Time{}, ErrNotFound
	}
	return st.startTime, nil
}

func kill(pid int, created time.Time) error {
	// A pidfd would close the check-then-kill race entirely; the window here
	// is microseconds and PIDs on Linux are allocated sequentially, so the
	// re-check is the pragmatic guard.
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
