package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/harshdalmia/hotreload-tool/internal/config"
	"github.com/harshdalmia/hotreload-tool/internal/crash"
	"github.com/harshdalmia/hotreload-tool/internal/process"
	"github.com/harshdalmia/hotreload-tool/internal/watcher"
)

// The controller holds every tricky invariant in hotreload: serial builds,
// latest-state-wins supersede, "a failed build must not disturb a working
// server", and crash restart. These tests drive it with fakes so those
// invariants are checked directly instead of inferred from a live reload.

const (
	testDebounce = 15 * time.Millisecond
	// waitTimeout is generous: these tests assert ordering, not latency, and a
	// loaded CI machine should not turn into a false failure.
	waitTimeout = 5 * time.Second
)

// --- fakes ------------------------------------------------------------------

type fakeWatcher struct {
	mu     sync.Mutex
	events chan watcher.Event
	closed bool
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan watcher.Event, 128)}
}

func (f *fakeWatcher) Run(ctx context.Context) {
	<-ctx.Done()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
}

func (f *fakeWatcher) Events() <-chan watcher.Event { return f.events }
func (f *fakeWatcher) Close() error                 { return nil }
func (f *fakeWatcher) WatchedCount() int            { return 1 }

// emit simulates a file change. It is a no-op after the watcher has shut down,
// so a test that cancels while emitting cannot panic.
func (f *fakeWatcher) emit(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- watcher.Event{Path: path}
}

type fakeBuilder struct {
	mu    sync.Mutex
	calls int

	// fn decides the outcome of each build. call is 1-based. A nil fn always
	// succeeds.
	fn func(ctx context.Context, call int) error

	// started receives the call number as each build begins.
	started chan int
}

func newFakeBuilder(fn func(ctx context.Context, call int) error) *fakeBuilder {
	return &fakeBuilder{fn: fn, started: make(chan int, 64)}
}

func (f *fakeBuilder) Build(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	fn := f.fn
	f.mu.Unlock()

	select {
	case f.started <- call:
	default:
	}

	if fn == nil {
		return nil
	}
	return fn(ctx, call)
}

func (f *fakeBuilder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeServer struct {
	mu        sync.Mutex
	starts    int
	stops     int
	running   bool
	startCtxs []context.Context
	startErr  error

	exits     chan process.ExitInfo
	startedCh chan int
	stoppedCh chan int

	// events, when set, records start and stop so ordering can be asserted
	// against the pre/build/post stages.
	events *recorder
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		exits:     make(chan process.ExitInfo, 8),
		startedCh: make(chan int, 64),
		stoppedCh: make(chan int, 64),
	}
}

func (f *fakeServer) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.startErr != nil {
		err := f.startErr
		f.mu.Unlock()
		return err
	}
	f.starts++
	n := f.starts
	f.running = true
	f.startCtxs = append(f.startCtxs, ctx)
	rec := f.events
	f.mu.Unlock()

	if rec != nil {
		rec.add("start")
	}
	select {
	case f.startedCh <- n:
	default:
	}
	return nil
}

func (f *fakeServer) Stop() {
	f.mu.Lock()
	f.stops++
	n := f.stops
	f.running = false
	rec := f.events
	f.mu.Unlock()

	if rec != nil {
		rec.add("stop")
	}
	select {
	case f.stoppedCh <- n:
	default:
	}
}

func (f *fakeServer) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeServer) Exits() <-chan process.ExitInfo { return f.exits }

func (f *fakeServer) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func (f *fakeServer) lastStartCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.startCtxs) == 0 {
		return nil
	}
	return f.startCtxs[len(f.startCtxs)-1]
}

// simulateCrash makes the server look like it died on its own.
func (f *fakeServer) simulateCrash(err error, uptime time.Duration) {
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
	f.exits <- process.ExitInfo{Uptime: uptime, Err: err, Intentional: false}
}

// --- harness ----------------------------------------------------------------

// recorder captures the order in which pipeline stages ran, so tests can assert
// that pre runs before the build and post only after the server is up.
type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, name)
}

func (r *recorder) sequence() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seq...)
}

// indexOf returns the position of the first occurrence of name, or -1.
func indexOf(seq []string, name string) int {
	for i, s := range seq {
		if s == name {
			return i
		}
	}
	return -1
}

type harness struct {
	ctrl    *Controller
	watcher *fakeWatcher
	builder *fakeBuilder
	preCmd  *fakeBuilder
	postCmd *fakeBuilder
	server  *fakeServer
	events  *recorder
	cancel  context.CancelFunc
	runErr  chan error

	waitOnce  sync.Once
	runResult error
	returned  bool
}

type harnessOpts struct {
	builderFn  func(ctx context.Context, call int) error
	preFn      func(ctx context.Context, call int) error
	postFn     func(ctx context.Context, call int) error
	detector   *crash.Detector
	cfg        *config.Config
	watcherErr error
}

func startHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Build = "fake-build"
	cfg.Exec = "fake-exec"
	cfg.Debounce = testDebounce
	if opts.cfg != nil {
		cfg = *opts.cfg
	}

	detector := opts.detector
	if detector == nil {
		detector = crash.NewDefault()
	}

	rec := &recorder{}

	// Wrap each stage so it records when it ran, then defers to the test's hook.
	trace := func(name string, fn func(ctx context.Context, call int) error) func(context.Context, int) error {
		return func(ctx context.Context, call int) error {
			rec.add(name)
			if fn == nil {
				return nil
			}
			return fn(ctx, call)
		}
	}

	fw := newFakeWatcher()
	fb := newFakeBuilder(trace("build", opts.builderFn))
	fpre := newFakeBuilder(trace("pre", opts.preFn))
	fpost := newFakeBuilder(trace("post", opts.postFn))
	fsrv := newFakeServer()
	fsrv.events = rec

	c := &Controller{
		cfg: cfg,
		newWatcher: func() (Watcher, error) {
			if opts.watcherErr != nil {
				return nil, opts.watcherErr
			}
			return fw, nil
		},
		preCmd:   fpre,
		builder:  fb,
		postCmd:  fpost,
		server:   fsrv,
		detector: detector,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	h := &harness{
		ctrl: c, watcher: fw, builder: fb, preCmd: fpre, postCmd: fpost,
		server: fsrv, events: rec, cancel: cancel, runErr: runErr,
	}
	t.Cleanup(func() { h.shutdown(t) })
	return h
}

// wait collects Run's return value. Safe to call more than once — a test that
// asserts on the shutdown result and the automatic cleanup both go through
// here, and only the first call actually reads the channel.
func (h *harness) wait(t *testing.T) error {
	t.Helper()
	h.waitOnce.Do(func() {
		select {
		case err := <-h.runErr:
			h.runResult = err
			h.returned = true
		case <-time.After(waitTimeout):
		}
	})
	if !h.returned {
		t.Error("controller.Run did not return after context cancellation")
	}
	return h.runResult
}

// shutdown cancels the controller and waits for Run to return.
func (h *harness) shutdown(t *testing.T) {
	t.Helper()
	h.cancel()
	h.wait(t)
}

// awaitInt waits for a value on ch, failing the test on timeout.
func awaitInt(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		return 0
	}
}

// awaitStable waits until check() holds, then confirms it still holds after a
// settle period. Used for "nothing further happened" assertions.
func awaitStable(t *testing.T, settle time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last error
	for time.Now().Before(deadline) {
		if last = check(); last == nil {
			time.Sleep(settle)
			if last = check(); last == nil {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never became stable: %v", last)
}

// --- tests ------------------------------------------------------------------

// TestRun_InitialBuildHappensWithoutFileChange covers the documented promise
// that hotreload compiles and serves immediately on startup.
func TestRun_InitialBuildHappensWithoutFileChange(t *testing.T) {
	h := startHarness(t, harnessOpts{})

	if got := awaitInt(t, h.builder.started, "initial build"); got != 1 {
		t.Errorf("first build call = %d, want 1", got)
	}
	if got := awaitInt(t, h.server.startedCh, "initial server start"); got != 1 {
		t.Errorf("first server start = %d, want 1", got)
	}
}

func TestRun_FileChangeTriggersRebuildAndRestart(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.watcher.emit("main.go")

	if got := awaitInt(t, h.builder.started, "rebuild"); got != 2 {
		t.Errorf("second build call = %d, want 2", got)
	}
	if got := awaitInt(t, h.server.startedCh, "server restart"); got != 2 {
		t.Errorf("second server start = %d, want 2", got)
	}
	// The old server must be stopped before the new one binds the port.
	if starts, stops := h.server.counts(); stops < 1 {
		t.Errorf("expected the previous server to be stopped (starts=%d stops=%d)", starts, stops)
	}
}

// TestRun_BurstOfEventsCausesOneRebuild is the debounce contract seen from the
// outside: a formatter touching ten files is one reload, not ten.
func TestRun_BurstOfEventsCausesOneRebuild(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	for i := 0; i < 10; i++ {
		h.watcher.emit(fmt.Sprintf("file%d.go", i))
	}

	if got := awaitInt(t, h.builder.started, "rebuild after burst"); got != 2 {
		t.Errorf("build call = %d, want 2", got)
	}
	awaitInt(t, h.server.startedCh, "server restart after burst")

	// No third build should follow from the same burst.
	awaitStable(t, 150*time.Millisecond, func() error {
		if n := h.builder.count(); n != 2 {
			return fmt.Errorf("build count = %d, want 2", n)
		}
		return nil
	})
}

// TestRun_FailedBuildLeavesRunningServerAlone is the single most important
// safety property: a typo must not take your server down.
func TestRun_FailedBuildLeavesRunningServerAlone(t *testing.T) {
	h := startHarness(t, harnessOpts{
		builderFn: func(ctx context.Context, call int) error {
			if call == 1 {
				return nil // initial build succeeds
			}
			return errors.New("syntax error")
		},
	})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")
	_, stopsBefore := h.server.counts()

	h.watcher.emit("broken.go")
	awaitInt(t, h.builder.started, "failing rebuild")

	awaitStable(t, 200*time.Millisecond, func() error {
		starts, stops := h.server.counts()
		if starts != 1 {
			return fmt.Errorf("server starts = %d, want 1 (failed build must not start a server)", starts)
		}
		if stops != stopsBefore {
			return fmt.Errorf("server stops = %d, want %d (failed build must not stop the server)", stops, stopsBefore)
		}
		return nil
	})
}

// TestRun_SupersededBuildDoesNotTouchServer is the kill-then-decide
// regression: a build that finishes after a newer change arrived must leave
// the current server running rather than stopping it and then declining to
// start a replacement.
func TestRun_SupersededBuildDoesNotTouchServer(t *testing.T) {
	release := make(chan struct{})
	h := startHarness(t, harnessOpts{
		builderFn: func(ctx context.Context, call int) error {
			switch call {
			case 1:
				return nil // initial build succeeds, server starts
			case 2:
				// Block, then report success even though a newer change has
				// landed in the meantime. This is the exact race: the compile
				// won, the code lost.
				<-release
				return nil
			default:
				// Keep later builds from muddying the assertion.
				return errors.New("later build fails")
			}
		},
	})

	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")
	_, stopsBefore := h.server.counts()

	h.watcher.emit("a.go")
	awaitInt(t, h.builder.started, "second build")

	// A newer save arrives while build 2 is still compiling.
	h.watcher.emit("b.go")
	time.Sleep(50 * time.Millisecond) // let the cancellation register
	close(release)

	awaitStable(t, 250*time.Millisecond, func() error {
		starts, stops := h.server.counts()
		if starts != 1 {
			return fmt.Errorf("server starts = %d, want 1 (stale build must not start a server)", starts)
		}
		if stops != stopsBefore {
			return fmt.Errorf("server stops = %d, want %d (stale build must not stop the live server)", stops, stopsBefore)
		}
		return nil
	})
}

// TestRun_ServerStartedUnderRootContext guards a subtle lifetime bug: if the
// server were started under the per-build context, the very next file save
// would cancel it out from under itself.
func TestRun_ServerStartedUnderRootContext(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	startCtx := h.server.lastStartCtx()
	if startCtx == nil {
		t.Fatal("no context recorded for Start")
	}

	// This cancels the in-flight build context.
	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "rebuild")

	if err := startCtx.Err(); err != nil {
		t.Errorf("context passed to Start was cancelled by a file event: %v", err)
	}
}

// --- crash handling ---------------------------------------------------------

func TestRun_CrashedServerIsRestarted(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.server.simulateCrash(errors.New("panic: nil map"), 3*time.Second)

	if got := awaitInt(t, h.server.startedCh, "restart after crash"); got != 2 {
		t.Errorf("server start after crash = %d, want 2", got)
	}
	// A crash restarts the binary; it does not need a recompile.
	if n := h.builder.count(); n != 1 {
		t.Errorf("build count = %d, want 1 (a crash should not trigger a rebuild)", n)
	}
}

// TestRun_CleanSelfExitIsNotRestarted keeps a one-shot --exec command from
// spinning forever.
func TestRun_CleanSelfExitIsNotRestarted(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.server.simulateCrash(nil, time.Second) // exit status 0

	awaitStable(t, 250*time.Millisecond, func() error {
		if starts, _ := h.server.counts(); starts != 1 {
			return fmt.Errorf("server starts = %d, want 1 (clean exit must not be restarted)", starts)
		}
		return nil
	})
}

// TestRun_CrashDuringBuildDefersToTheBuild avoids two goroutines racing to
// start a server.
func TestRun_CrashDuringBuildDefersToTheBuild(t *testing.T) {
	release := make(chan struct{})
	h := startHarness(t, harnessOpts{
		builderFn: func(ctx context.Context, call int) error {
			if call == 2 {
				<-release
			}
			return nil
		},
	})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "second build")

	// Crash while build 2 is compiling.
	h.server.simulateCrash(errors.New("boom"), time.Second)
	time.Sleep(100 * time.Millisecond)

	if starts, _ := h.server.counts(); starts != 1 {
		t.Errorf("server starts = %d during in-flight build, want 1", starts)
	}

	close(release)

	// The build finishing is what brings the server back.
	if got := awaitInt(t, h.server.startedCh, "server start from completed build"); got != 2 {
		t.Errorf("server start = %d, want 2", got)
	}
}

// TestRun_CrashLoopBacksOff checks the backoff is actually applied rather than
// just computed.
func TestRun_CrashLoopBacksOff(t *testing.T) {
	const backoff = 400 * time.Millisecond
	// threshold 1: the very first crash counts as a loop, which keeps the test
	// short while exercising the same code path.
	detector := crash.New(10*time.Second, 1, backoff)

	h := startHarness(t, harnessOpts{detector: detector})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	start := time.Now()
	h.server.simulateCrash(errors.New("boom"), 50*time.Millisecond)
	awaitInt(t, h.server.startedCh, "restart after backoff")
	elapsed := time.Since(start)

	if elapsed < backoff {
		t.Errorf("restart took %v, expected at least the %v backoff", elapsed, backoff)
	}
}

// TestRun_SuccessfulBuildResetsCrashHistory: new code deserves a clean slate,
// otherwise the fix for a crash loop would still be penalised by the backoff.
func TestRun_SuccessfulBuildResetsCrashHistory(t *testing.T) {
	detector := crash.New(time.Minute, 2, time.Second)
	detector.Record()
	detector.Record()
	if !detector.IsLooping() {
		t.Fatal("precondition: detector should report a loop")
	}

	h := startHarness(t, harnessOpts{detector: detector})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	awaitStable(t, 50*time.Millisecond, func() error {
		if detector.IsLooping() {
			return errors.New("crash history should be cleared by a successful build")
		}
		return nil
	})
}

// --- lifecycle --------------------------------------------------------------

func TestRun_ShutdownStopsServerAndReturnsNil(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.cancel()

	if err := h.wait(t); err != nil {
		t.Errorf("Run returned %v, want nil on clean shutdown", err)
	}

	if _, stops := h.server.counts(); stops < 1 {
		t.Error("shutdown should stop the server")
	}
}

func TestRun_WatcherCreationErrorIsReturned(t *testing.T) {
	sentinel := errors.New("cannot watch")
	c := &Controller{
		cfg:        config.Default(),
		newWatcher: func() (Watcher, error) { return nil, sentinel },
		builder:    newFakeBuilder(nil),
		server:     newFakeServer(),
		detector:   crash.NewDefault(),
	}

	if err := c.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("Run() error = %v, want %v", err, sentinel)
	}
}

// TestRun_ServerStartErrorDoesNotKillTheLoop: a failed start must be reported
// and survived, not fatal.
func TestRun_ServerStartErrorDoesNotKillTheLoop(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.server.mu.Lock()
	h.server.startErr = errors.New("port already in use")
	h.server.mu.Unlock()

	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "rebuild with failing start")

	// The loop must still be alive: clear the error and confirm it recovers.
	h.server.mu.Lock()
	h.server.startErr = nil
	h.server.mu.Unlock()

	h.watcher.emit("main2.go")
	awaitInt(t, h.builder.started, "rebuild after recovery")
	awaitInt(t, h.server.startedCh, "server start after recovery")
}

// TestNew_WiresRealDependencies is a smoke test for the production constructor.
func TestNew_WiresRealDependencies(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Build = "echo build"
	cfg.Exec = "echo exec"

	c := New(cfg)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.builder == nil || c.server == nil || c.detector == nil || c.newWatcher == nil {
		t.Error("New left a dependency unset")
	}

	w, err := c.newWatcher()
	if err != nil {
		t.Fatalf("newWatcher() failed: %v", err)
	}
	defer w.Close()
	if w.WatchedCount() < 1 {
		t.Errorf("WatchedCount() = %d, want at least 1", w.WatchedCount())
	}
}

// --- pre and post hooks -----------------------------------------------------

// TestRun_HookOrdering pins the pipeline order. pre must run before the build,
// because code generators produce source the build then compiles, and post must
// run only once the server is actually up.
func TestRun_HookOrdering(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	// Wait for post, which is the last stage.
	awaitStable(t, 50*time.Millisecond, func() error {
		if h.postCmd.count() < 1 {
			return errors.New("post-cmd has not run yet")
		}
		return nil
	})

	seq := h.events.sequence()
	pre, build, start, post := indexOf(seq, "pre"), indexOf(seq, "build"), indexOf(seq, "start"), indexOf(seq, "post")

	for name, idx := range map[string]int{"pre": pre, "build": build, "start": start, "post": post} {
		if idx < 0 {
			t.Fatalf("%s never ran; sequence was %v", name, seq)
		}
	}
	if pre >= build || build >= start || start >= post {
		t.Errorf("wrong order: want pre < build < start < post, got %v", seq)
	}
}

// TestRun_PreCmdFailureAbortsWithoutTouchingServer: a failing code generator
// means the build would compile stale or missing sources, so the reload should
// stop dead and leave the working server alone.
func TestRun_PreCmdFailureAbortsWithoutTouchingServer(t *testing.T) {
	h := startHarness(t, harnessOpts{
		preFn: func(_ context.Context, call int) error {
			if call == 1 {
				return nil // let the initial reload succeed
			}
			return errors.New("templ generate failed")
		},
	})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")
	buildsBefore := h.builder.count()
	_, stopsBefore := h.server.counts()

	h.watcher.emit("main.go")

	awaitStable(t, 250*time.Millisecond, func() error {
		if h.preCmd.count() < 2 {
			return errors.New("pre-cmd has not run a second time yet")
		}
		if n := h.builder.count(); n != buildsBefore {
			return fmt.Errorf("build count = %d, want %d: a failed pre-cmd must not build", n, buildsBefore)
		}
		starts, stops := h.server.counts()
		if starts != 1 {
			return fmt.Errorf("server starts = %d, want 1", starts)
		}
		if stops != stopsBefore {
			return fmt.Errorf("server stops = %d, want %d: the live server must be left alone", stops, stopsBefore)
		}
		return nil
	})
}

// TestRun_PostCmdFailureIsNonFatal: the server is already serving by the time
// post runs, so a failure there must not tear it down.
func TestRun_PostCmdFailureIsNonFatal(t *testing.T) {
	h := startHarness(t, harnessOpts{
		postFn: func(_ context.Context, _ int) error {
			return errors.New("health check failed")
		},
	})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	awaitStable(t, 250*time.Millisecond, func() error {
		if h.postCmd.count() < 1 {
			return errors.New("post-cmd has not run yet")
		}
		if !h.server.IsRunning() {
			return errors.New("server should still be running after a failed post-cmd")
		}
		if _, stops := h.server.counts(); stops > 1 {
			return fmt.Errorf("server was stopped %d times; a failed post-cmd must not stop it", stops)
		}
		return nil
	})

	// And the loop is still healthy.
	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "rebuild after a failed post-cmd")
}

// TestRun_PostCmdRunsUnderRootContext: post-cmd is started after the server, so
// it must not inherit a build context that the next save cancels.
func TestRun_PostCmdRunsUnderRootContext(t *testing.T) {
	captured := make(chan context.Context, 4)
	h := startHarness(t, harnessOpts{
		postFn: func(ctx context.Context, _ int) error {
			select {
			case captured <- ctx:
			default:
			}
			return nil
		},
	})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	var postCtx context.Context
	select {
	case postCtx = <-captured:
	case <-time.After(waitTimeout):
		t.Fatal("post-cmd never ran")
	}

	// This cancels the build context.
	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "rebuild")

	if err := postCtx.Err(); err != nil {
		t.Errorf("post-cmd context was cancelled by a file event: %v", err)
	}
}

// TestRun_HooksRunOnEveryReload guards against the hooks only firing at startup.
func TestRun_HooksRunOnEveryReload(t *testing.T) {
	h := startHarness(t, harnessOpts{})
	awaitInt(t, h.builder.started, "initial build")
	awaitInt(t, h.server.startedCh, "initial server start")

	h.watcher.emit("main.go")
	awaitInt(t, h.builder.started, "rebuild")
	awaitInt(t, h.server.startedCh, "restart")

	awaitStable(t, 100*time.Millisecond, func() error {
		if n := h.preCmd.count(); n < 2 {
			return fmt.Errorf("pre-cmd ran %d times, want at least 2", n)
		}
		if n := h.postCmd.count(); n < 2 {
			return fmt.Errorf("post-cmd ran %d times, want at least 2", n)
		}
		return nil
	})
}

// TestNew_WiresHooksAndPoller checks the production constructor builds the full
// pipeline, including choosing the poller when asked.
func TestNew_WiresHooksAndPoller(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Exec = "echo exec"
	cfg.PreCmd = "echo pre"
	cfg.PostCmd = "echo post"
	cfg.Poll = true

	c := New(cfg)
	if c.preCmd == nil || c.postCmd == nil {
		t.Fatal("New left a hook unset")
	}

	w, err := c.newWatcher()
	if err != nil {
		t.Fatalf("newWatcher(): %v", err)
	}
	defer w.Close()

	if _, ok := w.(*watcher.Poller); !ok {
		t.Errorf("with Poll set, newWatcher returned %T, want *watcher.Poller", w)
	}
}

// TestNew_UsesFsnotifyByDefault is the complement: polling is opt-in.
func TestNew_UsesFsnotifyByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Exec = "echo exec"

	c := New(cfg)
	w, err := c.newWatcher()
	if err != nil {
		t.Fatalf("newWatcher(): %v", err)
	}
	defer w.Close()

	if _, ok := w.(*watcher.Watcher); !ok {
		t.Errorf("without Poll, newWatcher returned %T, want *watcher.Watcher", w)
	}
}
