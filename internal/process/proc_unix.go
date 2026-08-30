//go:build !windows

package process

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// shellCommand builds a command that runs `command` through the OS shell, so
// pipes, redirects, globs, and env vars in a user's --build or --exec string
// behave the way they would at a prompt.
func shellCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command)
}

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup terminates the process tree rooted at pid: SIGTERM first, then
// SIGKILL if the process is still alive after grace.
//
// A grace of zero or less skips the graceful step and goes straight to
// SIGKILL. That is what build cancellation wants: a compiler has nothing to
// clean up, and waiting on it would delay every rebuild.
//
// It reports whether the process was confirmed reaped. A false return means
// the process survived SIGKILL, which on Unix normally indicates it is stuck
// in an uninterruptible wait.
func killGroup(pid int, label string, grace time.Duration, waitCh <-chan error) bool {
	if grace > 0 {
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			slog.Warn("SIGTERM on process group failed", "label", label, "pid", pid, "err", err)
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}

		select {
		case <-waitCh:
			slog.Debug("process stopped after SIGTERM", "label", label, "pid", pid)
			return true
		case <-time.After(grace):
		}

		slog.Warn("process did not stop after SIGTERM, sending SIGKILL",
			"label", label, "pid", pid, "grace_period", grace)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		slog.Warn("SIGKILL on process group failed", "label", label, "pid", pid, "err", err)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after SIGKILL", "label", label, "pid", pid)
		return true
	case <-time.After(forceKillTimeout):
		slog.Error("process could not be killed - possible zombie process", "label", label, "pid", pid)
		return false
	}
}
