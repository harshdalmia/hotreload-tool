//go:build windows

package runner

import (
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func killProcessTree(pid int, done <-chan struct{}) {
	pidStr := strconv.Itoa(pid)

	if err := exec.Command("taskkill", "/PID", pidStr, "/T").Run(); err != nil {
		slog.Debug("taskkill graceful attempt failed", "pid", pid, "err", err)
	}

	if done != nil {
		select {
		case <-done:
			slog.Debug("server exited after graceful taskkill", "pid", pid)
			return
		case <-time.After(gracePeriod):
			slog.Warn("server did not exit after graceful taskkill, forcing", "pid", pid)
		}
	}

	if err := exec.Command("taskkill", "/PID", pidStr, "/T", "/F").Run(); err != nil {
		slog.Warn("forced taskkill failed", "pid", pid, "err", err)
		if p, findErr := os.FindProcess(pid); findErr == nil {
			_ = p.Kill()
		}
	}

	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Warn("timed out waiting for server to exit after forced kill", "pid", pid)
		}
	}
}
