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

// --- RunBuild tests ---------------------------------------------------------

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

// --- Server tests -----------------------------------------------------------

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
	if elapsed > DefaultKillDelay+3*time.Second {
		t.Errorf("stubborn process not killed within grace period: %v", elapsed)
	}
}

// --- Exit attribution -------------------------------------------------------
//
// These tests cover the distinction that makes crash detection work: an exit
// caused by Stop must never look like a crash, and an exit the process chose
// on its own must always be reported.

func TestServer_StopIsNotReportedAsCrash(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	srv.Stop()

	// Give the reaper a moment to publish, if it were going to.
	select {
	case info := <-srv.Exits():
		t.Errorf("Stop() must not publish an exit notification, got %+v", info)
	case <-time.After(300 * time.Millisecond):
	}

	info, ok := srv.LastExit()
	if !ok {
		t.Fatal("LastExit() should report an exit after Stop()")
	}
	if !info.Intentional {
		t.Error("an exit caused by Stop() must be marked Intentional")
	}
}

func TestServer_UnexpectedExitIsReported(t *testing.T) {
	srv := NewServer(exitCmd(1))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	select {
	case info := <-srv.Exits():
		if info.Intentional {
			t.Error("a self-inflicted exit must not be marked Intentional")
		}
		if info.Err == nil {
			t.Error("a non-zero exit status should surface a non-nil Err")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected an exit notification for a process that died on its own")
	}
}

func TestServer_CleanSelfExitIsReportedWithoutError(t *testing.T) {
	srv := NewServer(immediateExitCmd())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	select {
	case info := <-srv.Exits():
		if info.Intentional {
			t.Error("a self-inflicted exit must not be marked Intentional")
		}
		if info.Err != nil {
			t.Errorf("a zero exit status should have a nil Err, got %v", info.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected an exit notification for a cleanly self-exiting process")
	}
}

// TestServer_ExitsIsSafeToSelectWhenIdle guards the controller's main loop:
// it selects on Exits() unconditionally, so an idle server must not make that
// case fire.
func TestServer_ExitsIsSafeToSelectWhenIdle(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	select {
	case info := <-srv.Exits():
		t.Errorf("Exits() fired on a server that was never started: %+v", info)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServer_RestartAfterCrashResetsExitState(t *testing.T) {
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	srv.Stop()

	if _, ok := srv.LastExit(); !ok {
		t.Fatal("expected LastExit to be set after the first Stop")
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("second Start() failed: %v", err)
	}
	defer srv.Stop()

	if _, ok := srv.LastExit(); ok {
		t.Error("LastExit should be cleared once the server is started again")
	}
	if !srv.IsRunning() {
		t.Error("server should be running after the second Start()")
	}
}

// --- Context handling -------------------------------------------------------

// TestServer_StartHonoursContext covers the contract that the server dies with
// the context it was started under, without the caller calling Stop.
func TestServer_StartHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(sleepCmd(60))
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if !srv.IsRunning() {
		t.Fatal("server should be running after Start()")
	}

	cancel()

	deadline := time.After(5 * time.Second)
	for srv.IsRunning() {
		select {
		case <-deadline:
			t.Fatal("server still running 5s after its context was cancelled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestServer_StartRefusesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := NewServer(sleepCmd(60))
	if err := srv.Start(ctx); err == nil {
		srv.Stop()
		t.Error("expected Start() to refuse an already-cancelled context")
	}
}

func TestServer_EmptyExecCommand(t *testing.T) {
	srv := NewServer("")
	if err := srv.Start(context.Background()); err == nil {
		t.Error("expected an error for an empty exec command")
	}
}

// --- Builder ----------------------------------------------------------------

func TestBuilder_Success(t *testing.T) {
	b := NewBuilder("echo build ok")
	if err := b.Build(context.Background()); err != nil {
		t.Errorf("expected build to succeed, got: %v", err)
	}
}

func TestBuilder_Failure(t *testing.T) {
	b := NewBuilder(exitCmd(1))
	if err := b.Build(context.Background()); err == nil {
		t.Error("expected build to fail for a non-zero exit code")
	}
}

// TestBuilder_CancellationIsImmediate is the reason build cancellation skips
// the graceful signal: this happens on every save that lands mid-compile, so
// it must not cost the server's kill delay.
func TestBuilder_CancellationIsImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBuilder(sleepCmd(30))

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = b.Build(ctx)
		done <- time.Since(start)
	}()

	time.Sleep(300 * time.Millisecond) // let the process actually get going
	cancel()

	select {
	case elapsed := <-done:
		if elapsed > DefaultKillDelay {
			t.Errorf("build cancellation took %v; it must not wait out the %v kill delay",
				elapsed, DefaultKillDelay)
		}
	case <-time.After(DefaultKillDelay + 5*time.Second):
		t.Fatal("build did not return after cancellation")
	}
}

func TestWithKillDelay_ClampsInvalidValues(t *testing.T) {
	if got := NewServer("x").killDelay; got != DefaultKillDelay {
		t.Errorf("default killDelay = %v, want %v", got, DefaultKillDelay)
	}
	if got := NewServer("x", WithKillDelay(0)).killDelay; got != DefaultKillDelay {
		t.Errorf("killDelay = %v, want default %v", got, DefaultKillDelay)
	}
	if got := NewServer("x", WithKillDelay(-time.Second)).killDelay; got != DefaultKillDelay {
		t.Errorf("killDelay = %v, want default %v", got, DefaultKillDelay)
	}
	if got := NewServer("x", WithKillDelay(2*time.Second)).killDelay; got != 2*time.Second {
		t.Errorf("killDelay = %v, want 2s", got)
	}
}

// TestBuilder_EmptyCommandIsANoOp is the interpreted-language path: no build
// step, so Build reports success immediately and the controller goes straight
// to restarting the server.
func TestBuilder_EmptyCommandIsANoOp(t *testing.T) {
	for _, cmd := range []string{"", "   "} {
		b := NewBuilder(cmd)
		start := time.Now()
		if err := b.Build(context.Background()); err != nil {
			t.Errorf("Build() with command %q = %v, want nil", cmd, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("a no-op build took %v; it should not spawn a process", elapsed)
		}
	}
}

// TestRunBuild_EmptyCommandStillErrors keeps the low-level helper strict. Only
// Builder treats an absent command as a deliberate choice.
func TestRunBuild_EmptyCommandStillErrors(t *testing.T) {
	if err := RunBuild(context.Background(), ""); err == nil {
		t.Error("RunBuild with an empty command should still report an error")
	}
}
