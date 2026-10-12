// Package procgroup starts a command so that it and every process it spawns
// can be killed as a unit — including, on Windows, when mcp-warden itself is
// killed without warning.
//
//   - Windows: the child is created suspended, assigned to a Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, then resumed. The kernel closes the
//     job handle when mcp-warden exits for any reason, killing the tree.
//   - Linux: a new process group, plus Pdeathsig so the direct child dies
//     with mcp-warden.
//   - macOS: a new process group. A hard-killed mcp-warden can leave the
//     group behind; `mcp-warden reap` and `scan --kill` clean that up.
package procgroup

import (
	"os/exec"
)

// Group owns a started command and its descendants.
type Group struct {
	cmd *exec.Cmd
	sys sysGroup

	// Degraded is set when the platform mechanism could not be applied
	// (e.g. job assignment refused) and Kill falls back to walking the
	// process tree.
	Degraded error
}

// Start configures cmd for group ownership and starts it. cmd must not have
// been started.
func Start(cmd *exec.Cmd) (*Group, error) {
	g := &Group{cmd: cmd}
	if err := g.start(); err != nil {
		return nil, err
	}
	return g, nil
}

// Pid returns the direct child's PID.
func (g *Group) Pid() int { return g.cmd.Process.Pid }

// Kill terminates the child and all of its descendants. It is safe to call
// more than once and after the child has exited.
func (g *Group) Kill() error { return g.kill() }

// Close releases OS resources. On Windows this also kills anything still
// running in the job.
func (g *Group) Close() error { return g.close() }
