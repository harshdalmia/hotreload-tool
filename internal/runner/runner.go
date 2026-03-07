package runner

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const (
	gracePeriod = 3 * time.Second

	crashWindow = 5 * time.Second

	crashThreshold = 3

	crashBackoff = 3 * time.Second
)

type Runner struct {
	buildCmd string
	execCmd  string

	mu         sync.Mutex
	serverProc *exec.Cmd
	serverDone chan struct{}
	crashTimes []time.Time
}

func New(buildCmd, execCmd string) *Runner {
	return &Runner{buildCmd: buildCmd, execCmd: execCmd}
}

func (r *Runner) BuildAndRun(ctx context.Context) {
	slog.Info("building", "cmd", r.buildCmd)

	if err := r.runBuild(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("build failed", "err", err)
		return
	}

	slog.Info("build succeeded")
	r.killServer()

	if r.isCrashLooping() {
		slog.Warn("crash loop detected, waiting before restarting", "backoff", crashBackoff)
		select {
		case <-time.After(crashBackoff):
		case <-ctx.Done():
			return
		}
	}

	r.startServer(ctx)
}

func (r *Runner) runBuild(ctx context.Context) error {
	cmd := shellCommandContext(ctx, r.buildCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)
	return cmd.Run()
}

func (r *Runner) startServer(ctx context.Context) {
	cmd := shellCommand(r.execCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		slog.Error("failed to start server", "err", err)
		return
	}

	slog.Info("server started", "pid", cmd.Process.Pid, "cmd", r.execCmd)
	done := make(chan struct{})

	r.mu.Lock()
	r.serverProc = cmd
	r.serverDone = done
	r.mu.Unlock()

	startTime := time.Now()

	go func() {
		defer close(done)
		err := cmd.Wait()
		elapsed := time.Since(startTime)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Error("server exited with error", "err", err, "uptime", elapsed.Round(time.Millisecond))
		} else {
			slog.Warn("server exited cleanly (unexpected)", "uptime", elapsed.Round(time.Millisecond))
		}
		if elapsed < crashWindow {
			r.recordCrash()
		}
	}()

	select {
	case <-ctx.Done():
		r.killServer()
	case <-done:
	}
}

func (r *Runner) killServer() {
	r.mu.Lock()
	proc := r.serverProc
	done := r.serverDone
	r.serverProc = nil
	r.serverDone = nil
	r.mu.Unlock()

	if proc == nil || proc.Process == nil {
		return
	}

	pid := proc.Process.Pid
	slog.Info("stopping server", "pid", pid)
	killProcessTree(pid, done)
}

func (r *Runner) recordCrash() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.crashTimes = append(r.crashTimes, time.Now())
	if len(r.crashTimes) > crashThreshold+1 {
		r.crashTimes = r.crashTimes[len(r.crashTimes)-(crashThreshold+1):]
	}
	slog.Warn("server crashed quickly", "crash_count", len(r.crashTimes))
}

func (r *Runner) isCrashLooping() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.crashTimes) < crashThreshold {
		return false
	}
	recent := r.crashTimes[len(r.crashTimes)-crashThreshold:]
	cutoff := time.Now().Add(-crashWindow)
	for _, t := range recent {
		if t.Before(cutoff) {
			return false
		}
	}
	return true
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command)
	}
	return exec.Command("sh", "-c", command)
}

func shellCommandContext(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}
