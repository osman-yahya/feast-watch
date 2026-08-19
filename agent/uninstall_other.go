//go:build !unix

package agent

import "syscall"

// detachedProcAttr has no portable equivalent outside unix. The agent's
// install/uninstall story is systemd on Linux; elsewhere the removal simply
// runs as an ordinary child.
func detachedProcAttr() *syscall.SysProcAttr { return nil }
