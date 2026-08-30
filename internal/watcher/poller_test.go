package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/harshdalmia/hotreload-tool/internal/filter"
	"github.com/harshdalmia/hotreload-tool/internal/watcher"
)

// pollInterval is short so the tests stay quick. Real use defaults to 500ms.
const pollInterval = 40 * time.Millisecond

// newTempPoller starts a Poller over a fresh temp directory.
func newTempPoller(t *testing.T, f *filter.Filter) (*watcher.Poller, string) {
	t.Helper()
	dir := t.TempDir()

	p, err := watcher.NewPoller(dir, f, pollInterval)
	if err != nil {
		t.Fatalf("NewPoller(%q): %v", dir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		p.Close()
	})
	go p.Run(ctx)

	return p, dir
}

// awaitPollEvent waits for an event whose base name matches needle.
func awaitPollEvent(t *testing.T, p *watcher.Poller, needle string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-p.Events():
			if !ok {
				return false
			}
			if filepath.Base(evt.Path) == needle {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestPoller_DetectsCreate(t *testing.T) {
	p, dir := newTempPoller(t, filter.Default())

	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !awaitPollEvent(t, p, "hello.go", 3*time.Second) {
		t.Error("expected an event for a newly created file")
	}
}

func TestPoller_DetectsModify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The file exists before the Poller starts, so the baseline scan should
	// record it without reporting anything.
	p, err := watcher.NewPoller(dir, filter.Default(), pollInterval)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.Close() })
	go p.Run(ctx)

	select {
	case evt := <-p.Events():
		t.Fatalf("baseline scan should emit nothing, got %v", evt)
	case <-time.After(150 * time.Millisecond):
	}

	// Change size as well as content, so the test does not depend on modtime
	// granularity, which can be coarse on some filesystems.
	if err := os.WriteFile(path, []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !awaitPollEvent(t, p, "main.go", 3*time.Second) {
		t.Error("expected an event for a modified file")
	}
}

func TestPoller_DetectsDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p, err := watcher.NewPoller(dir, filter.Default(), pollInterval)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.Close() })
	go p.Run(ctx)

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !awaitPollEvent(t, p, "gone.go", 3*time.Second) {
		t.Error("expected an event for a deleted file")
	}
}

func TestPoller_IgnoresFilteredFiles(t *testing.T) {
	p, dir := newTempPoller(t, filter.Default())

	// None of these may be reported.
	for _, name := range []string{".hidden", "main.go.swp", "notes.go~"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	// Build output must not be reported either; this is the rebuild-loop guard.
	target := filepath.Join(dir, "target", "debug")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "app"), []byte("binary"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Sentinel proving the poller is alive.
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt, ok := <-p.Events():
			if !ok {
				t.Fatal("events channel closed unexpectedly")
			}
			base := filepath.Base(evt.Path)
			if base == "real.go" {
				return // got the sentinel with nothing filtered leaking through
			}
			t.Errorf("filtered path produced an event: %s", evt.Path)
		case <-deadline:
			t.Error("sentinel event never arrived")
			return
		}
	}
}

// TestPoller_SeesPrePopulatedDirectory is a case the fsnotify path has to work
// around explicitly. Polling gets it for free: a scan simply sees whatever is on
// disk, so there is no window during which a new subtree is invisible.
func TestPoller_SeesPrePopulatedDirectory(t *testing.T) {
	p, dir := newTempPoller(t, filter.Default())

	staging := filepath.Join(t.TempDir(), "feature")
	if err := os.MkdirAll(filepath.Join(staging, "inner"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "inner", "service.go"),
		[]byte("package inner\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Rename(staging, filepath.Join(dir, "feature")); err != nil {
		t.Skipf("cannot rename across directories here: %v", err)
	}

	if !awaitPollEvent(t, p, "service.go", 3*time.Second) {
		t.Error("expected an event for a directory that arrived already populated")
	}
}

func TestPoller_RespectsIncludeExt(t *testing.T) {
	p, dir := newTempPoller(t, filter.New(filter.Options{IncludeExt: []string{".go"}}))

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt, ok := <-p.Events():
			if !ok {
				t.Fatal("events channel closed unexpectedly")
			}
			switch filepath.Base(evt.Path) {
			case "README.md":
				t.Error("README.md should be excluded by include-ext")
			case "main.go":
				return
			}
		case <-deadline:
			t.Error("no event for main.go")
			return
		}
	}
}

func TestPoller_ContextCancellationClosesEvents(t *testing.T) {
	p, err := watcher.NewPoller(t.TempDir(), filter.Default(), pollInterval)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Poller.Run did not return after cancellation")
	}

	if _, open := <-p.Events(); open {
		t.Error("Events() should be closed once Run returns")
	}
}

func TestPoller_WatchedCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p, err := watcher.NewPoller(dir, filter.Default(), pollInterval)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	defer p.Close()

	if got := p.WatchedCount(); got < 3 { // root + pkg + pkg/sub
		t.Errorf("WatchedCount() = %d, want at least 3", got)
	}
}

func TestPoller_RejectsMissingRoot(t *testing.T) {
	if _, err := watcher.NewPoller(filepath.Join(t.TempDir(), "absent"), nil, pollInterval); err == nil {
		t.Error("expected an error for a root that does not exist")
	}
}

func TestPoller_RejectsFileAsRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := watcher.NewPoller(path, nil, pollInterval); err == nil {
		t.Error("expected an error when the root is a file")
	}
}

// TestPoller_RootUnderIgnoredDirectory is the scoping rule again: a project
// living under a path component named tmp or build must still be scanned.
func TestPoller_RootUnderIgnoredDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp", "myproject")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	p, err := watcher.NewPoller(root, filter.Default(), pollInterval)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.Close() })
	go p.Run(ctx)

	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !awaitPollEvent(t, p, "main.go", 3*time.Second) {
		t.Error("ignore rules are leaking above the root in poll mode")
	}
}
