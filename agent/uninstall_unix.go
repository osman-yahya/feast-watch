//go:build unix

package agent

import "syscall"

// detachedProcAttr puts the uninstaller in its own session, so that killing
// the agent's process group — which is exactly what stopping its service does
// — does not take the removal down with it. Only the no-systemd fallback path
// depends on this; a transient unit is already outside our group.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
