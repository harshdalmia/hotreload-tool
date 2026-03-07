// Package process manages the lifecycle of build and server sub-processes.
//
// Design:
//   - Each process is started in its own process group / equivalent so we can
//     terminate the whole tree (parent + spawned children) on rebuild.
//   - Graceful termination first, then force-kill after gracePeriod if needed.
//   - After killing, we wait for Wait() completion so resources are fully reaped.
//   - Server and build logs are streamed directly to stdout/stderr.
package process

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const (
	// gracePeriod is how long we wait after graceful stop before force-killing.
	gracePeriod = 5 * time.Second
)

// RunBuild runs the build command synchronously, streaming its output directly
// to stdout/stderr. Returns nil on success; a non-nil error if the build
// fails or if ctx is cancelled mid-build.
func RunBuild(ctx context.Context, buildCmd string) error {
	if buildCmd == "" {
		return fmt.Errorf("empty build command")
	}

	// Run via OS shell so the command can contain pipes, redirects, env vars, etc.
	cmd := shellCommand(buildCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)

	slog.Info("build starting", "cmd", buildCmd)
	start := time.Now()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start build: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err := <-waitCh:
		if err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		slog.Info("build succeeded", "elapsed", time.Since(start).Round(time.Millisecond))
		return nil

	case <-ctx.Done():
		// Context cancelled � kill build process tree and wait for reap.
		killGroup(cmd.Process.Pid, "build", waitCh)
		return fmt.Errorf("build cancelled: %w", ctx.Err())
	}
}

// Server manages a single running server process.
type Server struct {
	execCmd string

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{} // closed when the process exits
	startedAt time.Time
	exitedAt  time.Time
}

// NewServer creates a Server for the given exec command.
func NewServer(execCmd string) *Server {
	// Initialise with an already-closed done channel so callers that check
	// Done() before Start() is ever called do not block.
	ch := make(chan struct{})
	close(ch)
	return &Server{
		execCmd: execCmd,
		done:    ch,
	}
}

// Start launches the server process. Server logs stream directly to
// stdout/stderr with no buffering.
func (s *Server) Start(ctx context.Context) error {
	if s.execCmd == "" {
		return fmt.Errorf("empty exec command")
	}

	// Use OS shell for the same reasons as RunBuild.
	cmd := shellCommand(s.execCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	done := make(chan struct{})
	now := time.Now()

	s.mu.Lock()
	s.cmd = cmd
	s.done = done
	s.startedAt = now
	s.exitedAt = time.Time{} // reset
	s.mu.Unlock()

	slog.Info("server started", "pid", cmd.Process.Pid, "cmd", s.execCmd)

	// Reap the process and record timing.
	go func() {
		err := cmd.Wait()
		exitTime := time.Now()

		s.mu.Lock()
		s.exitedAt = exitTime
		s.mu.Unlock()

		close(done)

		if err != nil {
			slog.Warn("server exited with error", "err", err)
		} else {
			slog.Info("server exited cleanly")
		}
	}()

	_ = ctx // kept for API symmetry with prior implementation.
	return nil
}

// Stop terminates the server process and all of its children by signalling
// the process group. Blocks until the process has been fully reaped.
// Safe to call when no server is running.
func (s *Server) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	s.cmd = nil
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid
	slog.Debug("stopping server", "pid", pid)

	// Convert done (chan struct{}) into the chan error shape killGroup expects.
	waitCh := make(chan error, 1)
	go func() {
		<-done
		waitCh <- nil
	}()

	killGroup(pid, "server", waitCh)
}

// Done returns a channel that is closed when the server process exits.
func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// IsRunning reports whether the server process is currently alive.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// UptimeAtExit returns how long the server ran and whether it has exited.
// If the server is still running, the second return value is false.
func (s *Server) UptimeAtExit() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exitedAt.IsZero() {
		return 0, false
	}
	return s.exitedAt.Sub(s.startedAt), true
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command)
	}
	return exec.Command("sh", "-c", command)
}
