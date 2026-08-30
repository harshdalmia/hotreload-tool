// Package process manages the lifecycle of build and server sub-processes.
//
// Design:
//   - Each process is started in its own process group / equivalent so we can
//     terminate the whole tree (parent + spawned children) on rebuild.
//   - Graceful termination first, then force-kill after the kill delay.
//   - After killing, we wait for Wait() completion so resources are fully reaped.
//   - Server and build logs are streamed directly to stdout/stderr.
//
// Exit attribution: Server distinguishes an exit that hotreload caused (a
// rebuild or shutdown calling Stop) from one the process chose on its own (a
// panic, a fatal error, an OOM kill). Only the latter is reported on Exits(),
// which is what lets the controller restart a crashed server without
// mistaking its own restarts for crashes.
package process

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// DefaultKillDelay is how long a process gets to shut down gracefully before
// it is force-killed.
const DefaultKillDelay = 5 * time.Second

// forceKillTimeout is the additional grace given after the forced kill signal
// before we declare the process unkillable.
const forceKillTimeout = 2 * time.Second

// Builder runs a project's build command.
type Builder struct {
	cmd string
}

// NewBuilder returns a Builder for the given shell command.
func NewBuilder(cmd string) *Builder {
	return &Builder{cmd: cmd}
}

// Build runs the build command synchronously, streaming its output to
// stdout/stderr. It returns nil on success, and a non-nil error if the build
// fails or ctx is cancelled mid-build.
func (b *Builder) Build(ctx context.Context) error {
	return runBuild(ctx, b.cmd)
}

// RunBuild runs buildCmd, streaming its output to stdout/stderr.
func RunBuild(ctx context.Context, buildCmd string) error {
	return runBuild(ctx, buildCmd)
}

func runBuild(ctx context.Context, buildCmd string) error {
	if buildCmd == "" {
		return fmt.Errorf("empty build command")
	}
	// Don't spawn a process just to kill it if we were already superseded.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("build cancelled before start: %w", err)
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
		// Context cancelled: kill the build process tree and wait for reap.
		//
		// Grace of zero means "force-kill immediately". A compiler has nothing
		// to flush and no connections to drain, so a graceful signal would buy
		// nothing and cost the whole kill delay. Cancellation happens on every
		// keystroke-triggered save, so it has to be instant.
		killGroup(cmd.Process.Pid, "build", 0, waitCh)
		return fmt.Errorf("build cancelled: %w", ctx.Err())
	}
}

// ExitInfo describes how and when a server process exited.
type ExitInfo struct {
	// Uptime is how long the process ran before exiting.
	Uptime time.Duration

	// Err is the error from Wait: nil for a zero exit status, non-nil for a
	// non-zero status or a signal.
	Err error

	// Intentional reports whether hotreload caused this exit by calling Stop
	// (a rebuild or a shutdown) rather than the process ending on its own.
	Intentional bool
}

// Server manages a single running server process.
type Server struct {
	execCmd   string
	killDelay time.Duration

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{} // closed when the process exits
	startedAt time.Time
	exitedAt  time.Time
	stopping  bool // set by Stop so the reaper can attribute the exit
	lastExit  ExitInfo
	hasExited bool

	// exits carries unexpected exits only. Capacity 1: the controller reads
	// it on every loop iteration, and a queue of stale crash notices would be
	// no more useful than the newest one.
	exits chan ExitInfo
}

// NewServer creates a Server for the given exec command using the default
// kill delay.
func NewServer(execCmd string) *Server {
	return NewServerWithKillDelay(execCmd, DefaultKillDelay)
}

// NewServerWithKillDelay creates a Server with an explicit kill delay.
func NewServerWithKillDelay(execCmd string, killDelay time.Duration) *Server {
	if killDelay <= 0 {
		killDelay = DefaultKillDelay
	}
	// Initialise with an already-closed done channel so callers that check
	// Done() before Start() is ever called do not block.
	ch := make(chan struct{})
	close(ch)
	return &Server{
		execCmd:   execCmd,
		killDelay: killDelay,
		done:      ch,
		exits:     make(chan ExitInfo, 1),
	}
}

// Start launches the server process. Server logs stream directly to
// stdout/stderr with no buffering.
//
// The process tree is terminated when ctx is cancelled, so callers should
// pass a context that lives as long as they want the server to: the root
// context, not a per-build one.
func (s *Server) Start(ctx context.Context) error {
	if s.execCmd == "" {
		return fmt.Errorf("empty exec command")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("not starting server, context already done: %w", err)
	}

	// Use OS shell for the same reasons as the build command.
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
	s.exitedAt = time.Time{}
	s.stopping = false
	s.hasExited = false
	s.mu.Unlock()

	slog.Info("server started", "pid", cmd.Process.Pid, "cmd", s.execCmd)

	go s.reap(cmd, done, now)

	// Honour ctx: tear the process tree down if the caller's context ends.
	// Exits once the process is gone, so this goroutine never outlives it.
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-done:
		}
	}()

	return nil
}

// reap waits for the process to exit, records timing and attribution, and
// publishes unexpected exits on Exits().
func (s *Server) reap(cmd *exec.Cmd, done chan struct{}, startedAt time.Time) {
	err := cmd.Wait()
	exitTime := time.Now()

	s.mu.Lock()
	s.exitedAt = exitTime
	s.hasExited = true
	info := ExitInfo{
		Uptime:      exitTime.Sub(startedAt),
		Err:         err,
		Intentional: s.stopping,
	}
	s.lastExit = info
	s.mu.Unlock()

	close(done)

	uptime := info.Uptime.Round(time.Millisecond)
	switch {
	case info.Intentional:
		slog.Debug("server stopped by hotreload", "uptime", uptime)
	case err != nil:
		slog.Warn("server exited on its own with an error", "err", err, "uptime", uptime)
	default:
		slog.Info("server exited on its own cleanly", "uptime", uptime)
	}

	if info.Intentional {
		return
	}
	select {
	case s.exits <- info:
	default:
		slog.Debug("unread exit notification pending, dropping older one")
	}
}

// Exits returns a channel of exits that hotreload did not cause. Exits
// triggered by Stop are never reported here, so a consumer can treat every
// value as a genuine crash.
//
// The channel is never closed, which makes it safe to select on even when no
// server is running.
func (s *Server) Exits() <-chan ExitInfo {
	return s.exits
}

// Stop terminates the server process and all of its children by signalling
// the process group. Blocks until the process has been fully reaped.
// Safe to call when no server is running, and safe to call repeatedly.
func (s *Server) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	if cmd != nil {
		// Tell the reaper this exit is our doing, before we signal anything.
		s.stopping = true
		s.cmd = nil
	}
	killDelay := s.killDelay
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

	if !killGroup(pid, "server", killDelay, waitCh) {
		// The process survived even a forced kill. Put it back so a later
		// Stop retries instead of silently leaking it, and so IsRunning
		// keeps reporting the truth.
		s.mu.Lock()
		if s.cmd == nil {
			s.cmd = cmd
		}
		s.mu.Unlock()
		slog.Error("server process could not be stopped; it may still hold its port", "pid", pid)
	}
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

// LastExit returns information about the most recent exit. ok is false if the
// server has not exited since it was last started.
func (s *Server) LastExit() (ExitInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastExit, s.hasExited
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
