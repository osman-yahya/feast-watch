package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// DefaultUninstaller is where the installer puts the uninstall script. It is a
// path rather than an embedded routine because the same script is what an
// operator runs by hand on a host being decommissioned — one uninstaller, one
// behaviour, whoever starts it.
const DefaultUninstaller = "/usr/local/sbin/feast-watch-agent-uninstall"

// uninstallCommand builds the command that removes this agent from its host.
//
// The uninstaller stops the agent's own systemd service as its first step, so
// it must not be a child of the agent: `systemctl stop` kills the unit's whole
// cgroup, and a plain child would be killed halfway through removing the very
// files it is standing on. Under systemd it is therefore launched as a
// transient unit (`systemd-run`), which belongs to systemd rather than to us
// and survives our death. Without systemd — a container, a host running
// something else — it is launched directly and detached instead.
//
// NOTHING SECRET IS PASSED. Not as an argument (/proc is world-readable, so a
// command line is visible to every user on the host) and not through
// systemd-run's --setenv, which is itself an argument. `--report` only asks
// the uninstaller to confirm the removal to the mother; it reads the URL and
// token out of agent.conf, which is root-owned, 0600, and about to be deleted
// by the same run.
func uninstallCommand(systemdRun, uninstaller string) (name string, args []string) {
	if systemdRun != "" {
		return systemdRun, []string{
			// --collect so a failed run does not linger in `systemctl --failed`
			// on a host that is supposed to have nothing of ours left on it.
			"--unit=feast-watch-agent-uninstall",
			"--collect",
			uninstaller, "--purge", "--report",
		}
	}
	return uninstaller, []string{"--purge", "--report"}
}

// Uninstall launches the uninstaller and returns as soon as it has started.
//
// It deliberately does not wait: the first thing the uninstaller does is stop
// this agent's service, so there is no exit status for us to collect — the
// process asking is the process being removed. "Started successfully" is the
// only outcome this side can observe, which is why the CONFIRMATION that the
// host is clean comes from the uninstaller itself calling POST /v1/uninstalled
// rather than from anything here.
func (c Config) Uninstall() error {
	uninstaller := c.uninstallerPath()
	if _, err := os.Stat(uninstaller); err != nil {
		// A host installed by an older installer, or a k8s agent that was
		// never installed by a script at all. Reported to the mother on the
		// next push, so the panel shows why the row is stuck instead of just
		// showing it stuck.
		return fmt.Errorf("uninstaller %s is not on this host: %w", uninstaller, err)
	}
	systemdRun, _ := exec.LookPath("systemd-run")

	name, args := uninstallCommand(systemdRun, uninstaller)
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	// Release rather than Wait: waiting would block until a process that is
	// about to kill us finishes, and the child is either systemd's now or
	// deliberately detached.
	return cmd.Process.Release()
}

// uninstallerPath is the configured uninstaller, or the path the installer
// writes it to.
func (c Config) uninstallerPath() string {
	if c.UninstallCmd != "" {
		return c.UninstallCmd
	}
	return DefaultUninstaller
}
