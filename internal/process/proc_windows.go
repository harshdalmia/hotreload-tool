//go:build windows

package process

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

// killGroup uses taskkill on Windows to terminate the process tree.
func killGroup(pid int, label string, waitCh <-chan error) {
	pidStr := strconv.Itoa(pid)

	// Try graceful tree termination first.
	if err := exec.Command("taskkill", "/PID", pidStr, "/T").Run(); err != nil {
		slog.Debug("taskkill graceful attempt failed", "label", label, "pid", pid, "err", err)
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after graceful taskkill", "label", label, "pid", pid)
		return
	case <-time.After(gracePeriod):
	}

	slog.Warn("process did not stop after graceful taskkill, forcing kill",
		"label", label, "pid", pid, "grace_period", gracePeriod)

	if err := exec.Command("taskkill", "/PID", pidStr, "/T", "/F").Run(); err != nil {
		slog.Warn("forced taskkill failed", "label", label, "pid", pid, "err", err)
		if p, findErr := os.FindProcess(pid); findErr == nil {
			_ = p.Kill()
		}
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after forced taskkill", "label", label, "pid", pid)
	case <-time.After(2 * time.Second):
		slog.Error("process could not be killed on windows", "label", label, "pid", pid)
	}
}
