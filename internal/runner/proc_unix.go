//go:build !windows

package runner

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessTree(pid int, done <-chan struct{}) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		slog.Warn("SIGTERM failed", "pid", pid, "err", err)
	}

	if done != nil {
		select {
		case <-done:
			slog.Debug("server exited after SIGTERM", "pid", pid)
			return
		case <-time.After(gracePeriod):
			slog.Warn("server did not exit after SIGTERM, sending SIGKILL", "pid", pid)
		}
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		slog.Warn("SIGKILL failed", "pid", pid, "err", err)
	}

	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Warn("timed out waiting for server to exit after SIGKILL", "pid", pid)
		}
	}
}
