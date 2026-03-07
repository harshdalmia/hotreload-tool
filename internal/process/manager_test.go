package process

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func exitCmd(code int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("exit /b %d", code)
	}
	return fmt.Sprintf("exit %d", code)
}

func sleepCmd(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("powershell -NoProfile -Command \"Start-Sleep -Seconds %d\"", seconds)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

func compilerErrCmd() string {
	if runtime.GOOS == "windows" {
		return "echo syntax error 1>&2 & exit /b 2"
	}
	return "echo 'syntax error' >&2; exit 2"
}

func immediateExitCmd() string {
	if runtime.GOOS == "windows" {
		return "exit /b 0"
	}
	return "exit 0"
}

func TestRunBuild_Success(t *testing.T) {
	if err := RunBuild(context.Background(), "echo build ok"); err != nil {
		t.Errorf("expected build to succeed, got: %v", err)
	}
}

func TestRunBuild_Failure(t *testing.T) {
	err := RunBuild(context.Background(), exitCmd(1))
	if err == nil {
		t.Error("expected build to fail for non-zero exit code, got nil")
	}
}

func TestRunBuild_CompilerError(t *testing.T) {
	err := RunBuild(context.Background(), compilerErrCmd())
	if err == nil {
		t.Error("expected build to fail for compiler-like error, got nil")
	}
}

func TestRunBuild_EmptyCommand(t *testing.T) {
	if err := RunBuild(context.Background(), ""); err == nil {
		t.Error("expected error for empty build command")
	}
}

func TestRunBuild_CancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunBuild(ctx, sleepCmd(30))
	if err == nil {
		t.Error("expected error when context is cancelled before build")
	}
}

func TestRunBuild_CancelledMidFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := RunBuild(ctx, sleepCmd(10))
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected error when build is cancelled mid-flight")
	}
	if elapsed > 3*time.Second {
		t.Errorf("RunBuild took too long after cancellation: %v", elapsed)
	}
}

func TestServer_StartStop(t *testing.T) {
	srv := NewServer(sleepCmd(60))

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if !srv.IsRunning() {
		t.Error("server should be running after Start()")
	}

	srv.Stop()

	if srv.IsRunning() {
		t.Error("server should not be running after Stop()")
	}
}

func TestServer_StopIsIdempotent(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	srv.Stop()
	srv.Stop()
}

func TestServer_StopWhenNeverStarted(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	srv.Stop()
}

func TestServer_StopReleasesResources(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	start := time.Now()
	srv.Stop()
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Errorf("Stop() took too long (%v), process may not have been killed", elapsed)
	}
}

func TestServer_UptimeAtExit_WhileRunning(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer srv.Stop()

	_, exited := srv.UptimeAtExit()
	if exited {
		t.Error("UptimeAtExit should report exited=false while server is still running")
	}
}

func TestServer_UptimeAtExit_AfterExit(t *testing.T) {
	srv := NewServer(immediateExitCmd())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit within 3 seconds")
	}

	uptime, exited := srv.UptimeAtExit()
	if !exited {
		t.Error("UptimeAtExit should report exited=true after process exits")
	}
	if uptime < 0 {
		t.Errorf("uptime should be non-negative, got %v", uptime)
	}
}

func TestServer_StubbornProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stubborn-process test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("stubborn-process trap test is Unix-specific")
	}

	srv := NewServer("trap '' TERM; tail -f /dev/null")

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	srv.Stop()
	elapsed := time.Since(start)

	if srv.IsRunning() {
		t.Error("server should not be running after Stop()")
	}
	if elapsed > gracePeriod+3*time.Second {
		t.Errorf("stubborn process not killed within grace period: %v", elapsed)
	}
}
