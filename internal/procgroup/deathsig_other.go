//go:build unix && !linux

package procgroup

import "syscall"

// setDeathSignal is a no-op: Pdeathsig is Linux-only.
func setDeathSignal(*syscall.SysProcAttr) {}
