//go:build windows

package process

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// shellCommand builds a command that runs `command` through cmd.exe.
//
// It sets CmdLine explicitly rather than letting os/exec assemble the command
// line from Args. os/exec quotes arguments using the C runtime's rules, which
// cmd.exe does not follow: a command containing quotes — say
//
//	go build -o "C:\Program Files\app.exe" .
//
// would have those quotes escaped as \" and arrive at cmd.exe as literal
// characters, breaking the path. Passing the line verbatim with /S (strip the
// outermost quote pair, take the rest as-is) preserves the user's quoting.
//
// CmdLine begins with the program name because that is what CreateProcess
// expects; the executable actually launched comes from cmd.Path.
func shellCommand(command string) *exec.Cmd {
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `cmd /S /C "` + command + `"`,
	}
	return cmd
}

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A new process group gives us a handle on the whole tree: the group id
	// equals the child's pid, children inherit it, and CTRL_BREAK can then be
	// addressed to the group without touching hotreload itself.
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// signalBreak sends CTRL_BREAK to the process group whose id is pid.
//
// This is the closest Windows equivalent of SIGTERM for a console
// application. It reaches every process in the group, including grandchildren
// spawned by the cmd.exe wrapper, and a Go server receives it as os.Interrupt
// so its graceful-shutdown path runs.
func signalBreak(pid int) error {
	// Group 0 means "every process sharing this console", which would include
	// hotreload itself. Never send that.
	if pid <= 0 {
		return fmt.Errorf("refusing to signal process group %d", pid)
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

// killGroup terminates the process tree rooted at pid: CTRL_BREAK first, then
// taskkill /T /F if the process is still alive after grace.
//
// A grace of zero or less skips the graceful step. That is what build
// cancellation wants: a compiler has nothing to clean up, and a console
// control event sent milliseconds after spawn can be missed entirely while the
// child is still attaching to the console — which would then cost the full
// grace period on every superseded build.
//
// It reports whether the process was confirmed reaped.
//
// Note that plain `taskkill /T` (no /F) is deliberately not used as the
// graceful step. It posts a window-close message, which a console application
// with no message loop never acts on, so it does nothing except burn the whole
// grace period before the forced kill.
func killGroup(pid int, label string, grace time.Duration, waitCh <-chan error) bool {
	if grace > 0 {
		if err := signalBreak(pid); err != nil {
			slog.Debug("could not send CTRL_BREAK to process group",
				"label", label, "pid", pid, "err", err)
		}

		select {
		case <-waitCh:
			slog.Debug("process stopped after CTRL_BREAK", "label", label, "pid", pid)
			return true
		case <-time.After(grace):
		}

		slog.Warn("process did not stop after CTRL_BREAK, forcing kill",
			"label", label, "pid", pid, "grace_period", grace)
	}

	pidStr := strconv.Itoa(pid)
	if err := exec.Command("taskkill", "/PID", pidStr, "/T", "/F").Run(); err != nil {
		slog.Warn("forced taskkill failed", "label", label, "pid", pid, "err", err)
		// Last resort: this only reaches the direct child, not the tree.
		if p, findErr := os.FindProcess(pid); findErr == nil {
			_ = p.Kill()
		}
	}

	select {
	case <-waitCh:
		slog.Debug("process stopped after forced taskkill", "label", label, "pid", pid)
		return true
	case <-time.After(forceKillTimeout):
		slog.Error("process could not be killed on windows", "label", label, "pid", pid)
		return false
	}
}
