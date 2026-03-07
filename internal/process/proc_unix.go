//go:build !windows

package process

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup sends SIGTERM to the process group identified by pid,
// waits for exit (via waitCh), and escalates to SIGKILL if gracePeriod elapses.
func killGroup(pid int, label string, waitCh <-chan error) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		slog.Warn("SIGTERM on process group failed", "label", label, "pid", pid, "err", err)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after SIGTERM", "label", label, "pid", pid)
		return
	case <-time.After(gracePeriod):
	}

	slog.Warn("process did not stop after SIGTERM, sending SIGKILL",
		"label", label, "pid", pid, "grace_period", gracePeriod)

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		slog.Warn("SIGKILL on process group failed", "label", label, "pid", pid, "err", err)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after SIGKILL", "label", label, "pid", pid)
	case <-time.After(2 * time.Second):
		slog.Error("process could not be killed - possible zombie process", "label", label, "pid", pid)
	}
}
