//go:build linux

package procgroup

import "syscall"

// setDeathSignal asks the kernel to SIGKILL the child when its parent thread
// exits. It covers the direct child only; grandchildren in the process group
// are handled by Kill and by `mcp-warden reap`.
func setDeathSignal(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
