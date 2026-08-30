// Package e2e_test exercises hotreload against a real Go toolchain and real
// processes: an actual `go build`, an actual server process, an actual file
// save. The unit tests elsewhere use fakes to pin down ordering and edge
// cases; these tests answer the blunter question of whether the thing
// reloads.
//
// They need `go` on PATH and take a few seconds each, so they are skipped
// under -short.
package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/harshdalmia/hotreload-tool/internal/config"
	"github.com/harshdalmia/hotreload-tool/internal/controller"
)

// serverSource is the program hotreload builds and runs. It appends one line
// to the log on every start, which is how the tests count restarts, and
// rewrites the heartbeat file continuously, which is how they tell a live
// server from a dead one.
//
// %[1]s is the version marker, %[2]s the exit behaviour.
const serverSource = `package main

import (
	"fmt"
	"os"
	"time"
)

const version = %[1]q

func main() {
	logPath := os.Args[1]
	beatPath := os.Args[2]

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintf(f, "%%s start\n", version)
	f.Close()

	%[2]s

	for i := 0; ; i++ {
		os.WriteFile(beatPath, []byte(fmt.Sprintf("%%s %%d", version, i)), 0o644)
		time.Sleep(50 * time.Millisecond)
	}
}
`

// keepRunning and crashImmediately are the two exit behaviours substituted
// into serverSource.
const (
	keepRunning      = ""
	crashImmediately = `os.Exit(1)`
)

type fixture struct {
	root     string // watched source tree
	srcPath  string // root/main.go
	binPath  string // built binary, deliberately outside root
	logPath  string // append-only start log
	beatPath string // heartbeat file
	cfg      config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGoToolchain(t)

	root := t.TempDir()
	out := t.TempDir() // outside root so build output cannot trigger events

	binName := "e2eserver"
	if isWindows() {
		binName += ".exe"
	}

	f := &fixture{
		root:     root,
		srcPath:  filepath.Join(root, "main.go"),
		binPath:  filepath.Join(out, binName),
		logPath:  filepath.Join(out, "starts.log"),
		beatPath: filepath.Join(out, "heartbeat.txt"),
	}

	// A go.mod makes `go build .` work inside root.
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module e2eserver\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	f.writeServer(t, "V1", keepRunning)

	cfg := config.Default()
	cfg.Root = root
	// go build -C avoids shell-specific `cd` syntax entirely.
	cfg.Build = fmt.Sprintf("go build -C %s -o %s .", shellQuote(root), shellQuote(f.binPath))
	cfg.Exec = fmt.Sprintf("%s %s %s",
		shellQuote(f.binPath), shellQuote(f.logPath), shellQuote(f.beatPath))
	cfg.Debounce = 50 * time.Millisecond
	cfg.KillDelay = 2 * time.Second
	cfg.IncludeExt = []string{".go"}

	if err := cfg.Normalize(); err != nil {
		t.Fatalf("cfg.Normalize: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	f.cfg = cfg
	return f
}

// writeServer replaces the watched source with a given version and behaviour.
func (f *fixture) writeServer(t *testing.T, version, behaviour string) {
	t.Helper()
	body := fmt.Sprintf(serverSource, version, behaviour)
	if err := os.WriteFile(f.srcPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write server source: %v", err)
	}
}

// writeBrokenServer replaces the watched source with something that will not
// compile.
func (f *fixture) writeBrokenServer(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.srcPath,
		[]byte("package main\n\nfunc main() { this is not go }\n"), 0o644); err != nil {
		t.Fatalf("write broken source: %v", err)
	}
}

// run starts the controller and returns a function that shuts it down.
func (f *fixture) run(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.New(f.cfg).Run(ctx) }()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("controller.Run returned %v, want nil", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("controller.Run did not return after cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

// heartbeat returns the version and counter the running server last wrote.
func (f *fixture) heartbeat() (version string, counter int, ok bool) {
	b, err := os.ReadFile(f.beatPath)
	if err != nil {
		return "", 0, false
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false
	}
	return parts[0], n, true
}

// startCount returns how many times a server process has started.
func (f *fixture) startCount() int {
	b, err := os.ReadFile(f.logPath)
	if err != nil {
		return 0
	}
	return len(strings.Fields(strings.ReplaceAll(string(b), " start", "")))
}

// awaitVersion waits for the heartbeat to report the given version.
func (f *fixture) awaitVersion(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got, _, ok := f.heartbeat(); ok && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _, _ := f.heartbeat()
	t.Fatalf("timed out after %v waiting for version %q; heartbeat reports %q", timeout, want, got)
}

// awaitStartCount waits for at least n server starts.
func (f *fixture) awaitStartCount(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.startCount() >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %d server starts, saw %d", timeout, n, f.startCount())
}

// sampleHeartbeat polls until it gets one readable heartbeat sample.
//
// The server rewrites the file every 50 ms, so an unlucky read lands between
// the truncate and the write and comes back empty or half-written. That says
// nothing about whether the server is alive, so it must be retried rather than
// believed.
func (f *fixture) sampleHeartbeat(timeout time.Duration) (version string, counter int, ok bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, n, good := f.heartbeat(); good {
			return v, n, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", 0, false
}

// requireAlive confirms the server is still ticking by watching the heartbeat
// counter advance.
func (f *fixture) requireAlive(t *testing.T, wantVersion string) {
	t.Helper()

	version, first, ok := f.sampleHeartbeat(10 * time.Second)
	if !ok {
		t.Fatal("no readable heartbeat; the server is not running")
	}
	if version != wantVersion {
		t.Fatalf("heartbeat version = %q, want %q", version, wantVersion)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, n, good := f.heartbeat(); good && n > first {
			if v != wantVersion {
				t.Fatalf("heartbeat version changed to %q, want %q", v, wantVersion)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("heartbeat counter stuck at %d; the server is not alive", first)
}

// --- tests ------------------------------------------------------------------

// TestE2E_InitialBuildAndReloadOnSave is the whole product in one test: start,
// serve, edit, serve the new code.
func TestE2E_InitialBuildAndReloadOnSave(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)
	f.run(t)

	// Startup: build and serve without any file change.
	f.awaitVersion(t, "V1", 90*time.Second)
	if got := f.startCount(); got != 1 {
		t.Errorf("server start count = %d, want 1", got)
	}

	// Save a change: rebuild and serve the new binary.
	f.writeServer(t, "V2", keepRunning)
	f.awaitVersion(t, "V2", 90*time.Second)

	f.awaitStartCount(t, 2, 10*time.Second)
	if got := f.startCount(); got != 2 {
		t.Errorf("server start count = %d, want exactly 2 (one reload, not several)", got)
	}
}

// TestE2E_BuildFailureLeavesServerRunning is the safety property that matters
// most in daily use: a broken save must not cost you your running server.
func TestE2E_BuildFailureLeavesServerRunning(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)
	f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)
	startsBefore := f.startCount()

	f.writeBrokenServer(t)
	time.Sleep(3 * time.Second) // let the failing build run to completion

	f.requireAlive(t, "V1")
	if got := f.startCount(); got != startsBefore {
		t.Errorf("server start count = %d, want %d; a failed build must not restart anything",
			got, startsBefore)
	}

	// Fixing the code recovers without intervention.
	f.writeServer(t, "V3", keepRunning)
	f.awaitVersion(t, "V3", 90*time.Second)
}

// TestE2E_CrashedServerIsRestarted covers the behaviour that was missing
// entirely: a server that dies on its own comes back without a file change.
func TestE2E_CrashedServerIsRestarted(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)
	// Start from a version that exits non-zero as soon as it has logged.
	f.writeServer(t, "V1", crashImmediately)
	f.run(t)

	// The first start plus at least two restarts prove the loop is reacting to
	// the crash rather than to a file event.
	f.awaitStartCount(t, 3, 90*time.Second)

	// And a real fix ends the loop.
	f.writeServer(t, "V2", keepRunning)
	f.awaitVersion(t, "V2", 90*time.Second)
	f.requireAlive(t, "V2")
}

// TestE2E_IgnoredFileDoesNotRebuild pins the --include-ext behaviour to an
// observable outcome: editing docs costs nothing.
func TestE2E_IgnoredFileDoesNotRebuild(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)
	f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)
	startsBefore := f.startCount()

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf("# docs revision %d\n", i)
		if err := os.WriteFile(filepath.Join(f.root, "README.md"), []byte(body), 0o644); err != nil {
			t.Fatalf("write README: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)

	if got := f.startCount(); got != startsBefore {
		t.Errorf("server start count = %d, want %d; README edits must not trigger a rebuild",
			got, startsBefore)
	}
	f.requireAlive(t, "V1")
}

// TestE2E_ShutdownLeavesNoProcess checks the server really goes away on
// shutdown rather than being orphaned.
func TestE2E_ShutdownLeavesNoProcess(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)
	stop := f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)
	_, before, ok := f.sampleHeartbeat(5 * time.Second)
	if !ok {
		t.Fatal("no readable heartbeat before shutdown")
	}

	stop()

	// Give any surviving process a chance to prove it is still writing. Nothing
	// is rewriting the file now, so these reads cannot race.
	time.Sleep(1 * time.Second)
	_, afterStop, ok := f.heartbeat()
	if !ok {
		t.Fatal("heartbeat unreadable after shutdown")
	}
	time.Sleep(1 * time.Second)
	_, later, _ := f.heartbeat()

	if later != afterStop {
		t.Errorf("heartbeat advanced from %d to %d after shutdown; the server was orphaned",
			afterStop, later)
	}
	if afterStop < before {
		t.Errorf("heartbeat went backwards (%d -> %d)", before, afterStop)
	}
}

// --- helpers ----------------------------------------------------------------

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("end-to-end tests need the go toolchain on PATH")
	}
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}

// shellQuote wraps a path in double quotes for the OS shell. It deliberately
// avoids %q, whose Go-style escaping turns C:\Users\x into C:\\Users\\x and
// leaves the shell looking for a directory that does not exist.
func shellQuote(s string) string {
	return `"` + s + `"`
}

// TestE2E_NoBuildCommandStillRestarts is the interpreted-language workflow end
// to end: Python and Node have nothing to compile, so a save should restart the
// process directly rather than requiring a placeholder build command.
//
// The server binary is compiled once up front, outside hotreload, and the build
// command is left empty. Editing a watched file must therefore restart the
// process without producing a new binary, which is exactly what an interpreted
// project looks like from hotreload's point of view.
func TestE2E_NoBuildCommandStillRestarts(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)

	// Compile once ourselves, so hotreload has something to run.
	build := exec.Command("go", "build", "-C", f.root, "-o", f.binPath, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("pre-building the server failed: %v", err)
	}

	f.cfg.Build = "" // the whole point of this test
	if err := f.cfg.Validate(); err != nil {
		t.Fatalf("an empty build command should be valid: %v", err)
	}
	f.run(t)

	f.awaitVersion(t, "V1", 30*time.Second)
	if got := f.startCount(); got != 1 {
		t.Fatalf("server start count = %d, want 1", got)
	}

	// A save must restart the process even though nothing is compiled.
	f.writeServer(t, "V2", keepRunning)
	f.awaitStartCount(t, 2, 30*time.Second)

	// The binary was never rebuilt, so the running code is still V1. That is
	// correct: without a build step hotreload restarts, it does not compile.
	if version, _, ok := f.sampleHeartbeat(5 * time.Second); ok && version != "V1" {
		t.Errorf("heartbeat version = %q, want V1; no build step should mean no new binary", version)
	}
	f.requireAlive(t, "V1")
}

// TestE2E_PollModeReloads exercises the polling watcher against a real build and
// a real process. Polling is an entirely separate detection path, so the basic
// promise has to be proved for it independently: it is the mode people will fall
// back to inside Docker, where notifications never arrive.
func TestE2E_PollModeReloads(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)

	f.cfg.Poll = true
	f.cfg.PollInterval = 100 * time.Millisecond
	if err := f.cfg.Validate(); err != nil {
		t.Fatalf("poll config should be valid: %v", err)
	}
	f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)
	if got := f.startCount(); got != 1 {
		t.Errorf("server start count = %d, want 1", got)
	}

	f.writeServer(t, "V2", keepRunning)
	f.awaitVersion(t, "V2", 90*time.Second)
	f.requireAlive(t, "V2")
}

// TestE2E_PollModeIgnoresBuildOutput is the rebuild-loop guard for poll mode.
// Polling rescans everything, so if the filter were not consulted the build's
// own artefacts would retrigger it forever.
func TestE2E_PollModeIgnoresBuildOutput(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)

	f.cfg.Poll = true
	f.cfg.PollInterval = 100 * time.Millisecond
	f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)
	startsBefore := f.startCount()

	// Write into directories a build tool would own.
	for _, rel := range []string{
		filepath.Join("target", "debug", "app"),
		filepath.Join("bin", "app"),
		filepath.Join("dist", "bundle.js"),
	} {
		full := filepath.Join(f.root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte("artefact"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	time.Sleep(2 * time.Second) // several poll ticks

	if got := f.startCount(); got != startsBefore {
		t.Errorf("server start count = %d, want %d; build output must not retrigger a poll reload",
			got, startsBefore)
	}
	f.requireAlive(t, "V1")
}

// TestE2E_HooksRunAroundTheReload proves the hooks actually execute real shell
// commands in the right order, rather than only being wired up correctly.
func TestE2E_HooksRunAroundTheReload(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)

	// Hook output goes outside the watched tree, so writing it cannot itself
	// trigger another reload.
	scratch := t.TempDir()
	preMarker := filepath.Join(scratch, "pre.txt")
	postMarker := filepath.Join(scratch, "post.txt")

	// `echo x > file` behaves the same under sh and cmd.
	f.cfg.PreCmd = "echo pre > " + shellQuote(preMarker)
	f.cfg.PostCmd = "echo post > " + shellQuote(postMarker)
	f.run(t)

	f.awaitVersion(t, "V1", 90*time.Second)

	// post-cmd runs after the server is up, so both markers should appear.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, preErr := os.Stat(preMarker)
		_, postErr := os.Stat(postMarker)
		if preErr == nil && postErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, err := os.Stat(preMarker); err != nil {
		t.Errorf("pre-cmd did not run: %v", err)
	}
	if _, err := os.Stat(postMarker); err != nil {
		t.Errorf("post-cmd did not run: %v", err)
	}

	// The post marker must not predate the pre marker.
	preInfo, err1 := os.Stat(preMarker)
	postInfo, err2 := os.Stat(postMarker)
	if err1 == nil && err2 == nil && postInfo.ModTime().Before(preInfo.ModTime()) {
		t.Errorf("post-cmd (%v) ran before pre-cmd (%v)", postInfo.ModTime(), preInfo.ModTime())
	}
}

// TestE2E_FailingPreCmdNeverBuilds is the pre-hook equivalent of the
// build-failure guarantee. A failing code generator means the build would
// compile missing or stale sources, so the reload must stop before it starts.
//
// The failure semantics with a live server to protect are covered precisely by
// TestRun_PreCmdFailureAbortsWithoutTouchingServer; this checks the real shell
// path, that a non-zero hook genuinely aborts the pipeline.
func TestE2E_FailingPreCmdNeverBuilds(t *testing.T) {
	skipIfShort(t)
	f := newFixture(t)

	f.cfg.PreCmd = exitFailureCmd()
	f.run(t)

	// Nothing should ever come up: the pre hook fails on the initial reload.
	time.Sleep(5 * time.Second)

	if got := f.startCount(); got != 0 {
		t.Errorf("server started %d times; a failing pre-cmd must abort before the build", got)
	}
	if _, _, ok := f.heartbeat(); ok {
		t.Error("a server is running despite the pre-cmd failing")
	}
}

// exitFailureCmd is a shell command that always exits non-zero.
func exitFailureCmd() string {
	if isWindows() {
		return "exit /b 1"
	}
	return "exit 1"
}
