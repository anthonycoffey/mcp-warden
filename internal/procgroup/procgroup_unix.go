//go:build unix

package procgroup

import (
	"errors"
	"syscall"
)

type sysGroup struct {
	pgid int // the child is the group leader, so its PID is the PGID
}

func (g *Group) start() error {
	if g.cmd.SysProcAttr == nil {
		g.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	g.cmd.SysProcAttr.Setpgid = true
	setDeathSignal(g.cmd.SysProcAttr)
	if err := g.cmd.Start(); err != nil {
		return err
	}
	g.sys.pgid = g.cmd.Process.Pid
	return nil
}

func (g *Group) kill() error {
	// A negative PID signals every process in the group.
	err := syscall.Kill(-g.sys.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (g *Group) close() error { return nil }
