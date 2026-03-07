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
	gracePeriod = 5 * time.Second
)

func RunBuild(ctx context.Context, buildCmd string) error {
	if buildCmd == "" {
		return fmt.Errorf("empty build command")
	}

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

		killGroup(cmd.Process.Pid, "build", waitCh)
		return fmt.Errorf("build cancelled: %w", ctx.Err())
	}
}

type Server struct {
	execCmd string

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	startedAt time.Time
	exitedAt  time.Time
}

func NewServer(execCmd string) *Server {

	ch := make(chan struct{})
	close(ch)
	return &Server{
		execCmd: execCmd,
		done:    ch,
	}
}

func (s *Server) Start(ctx context.Context) error {
	if s.execCmd == "" {
		return fmt.Errorf("empty exec command")
	}

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
	s.mu.Unlock()

	slog.Info("server started", "pid", cmd.Process.Pid, "cmd", s.execCmd)

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

	_ = ctx
	return nil
}

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

	waitCh := make(chan error, 1)
	go func() {
		<-done
		waitCh <- nil
	}()

	killGroup(pid, "server", waitCh)
}

func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

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
