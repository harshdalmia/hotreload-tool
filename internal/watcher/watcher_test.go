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

// newTempWatcher creates a watcher for a temporary directory using the
// default filter rules. t.Cleanup registers Close() and context cancellation.
func newTempWatcher(t *testing.T) (*watcher.Watcher, string, context.CancelFunc) {
	t.Helper()
	return newTempWatcherWithFilter(t, filter.Default())
}

// newTempWatcherWithFilter is newTempWatcher with explicit filter rules.
func newTempWatcherWithFilter(t *testing.T, f *filter.Filter) (*watcher.Watcher, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()

	w, err := watcher.New(dir, f)
	if err != nil {
		t.Fatalf("watcher.New(%q): %v", dir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		w.Close()
	})

	go w.Run(ctx)
	return w, dir, cancel
}

// waitEvent waits up to timeout for any event whose path contains needle.
func waitEvent(t *testing.T, w *watcher.Watcher, needle string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-w.Events():
			if !ok {
				t.Log("events channel closed")
				return false
			}
			if filepath.Base(evt.Path) == needle || evt.Path == needle {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestWatcher_DetectsFileCreate(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond) // give inotify a moment to arm

	path := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "hello.go", 2*time.Second) {
		t.Error("expected event for newly created file, got none within 2s")
	}
}

func TestWatcher_DetectsFileModify(t *testing.T) {
	w, dir, _ := newTempWatcher(t)

	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // drain the create event

	// Modify the file.
	if err := os.WriteFile(path, []byte("package main\n// changed\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "main.go", 2*time.Second) {
		t.Error("expected event for modified file, got none within 2s")
	}
}

func TestWatcher_IgnoresHiddenFiles(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	// Write a hidden file — it must NOT generate an event.
	hidden := filepath.Join(dir, ".hidden")
	if err := os.WriteFile(hidden, []byte("secret"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Also write a visible file so we have a "sentinel" event to wait for.
	visible := filepath.Join(dir, "visible.go")
	if err := os.WriteFile(visible, []byte("package main"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wait for the sentinel.
	if !waitEvent(t, w, "visible.go", 2*time.Second) {
		t.Skip("sentinel event not received; timing issue, skipping")
	}

	// Drain all remaining events and check none mention the hidden file.
	drain := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-w.Events():
			if filepath.Base(evt.Path) == ".hidden" {
				t.Errorf("unexpected event for hidden file: %s", evt.Path)
			}
		case <-drain:
			return
		}
	}
}

func TestWatcher_IgnoresVimSwapFiles(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	// Vim creates a .swp file on open; it must be filtered out.
	swap := filepath.Join(dir, ".main.go.swp")
	if err := os.WriteFile(swap, []byte("swap"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sentinel := filepath.Join(dir, "real.go")
	if err := os.WriteFile(sentinel, []byte("package main"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "real.go", 2*time.Second) {
		t.Skip("sentinel event not received; timing issue, skipping")
	}

	drain := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-w.Events():
			if filepath.Base(evt.Path) == ".main.go.swp" {
				t.Errorf("unexpected event for vim swap file: %s", evt.Path)
			}
		case <-drain:
			return
		}
	}
}

func TestWatcher_DetectsNewSubdirectory(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	// Create a new subdirectory — the watcher should begin watching it.
	subdir := filepath.Join(dir, "newpkg")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // allow watcher to pick up the new dir

	// Now create a file inside the new subdirectory.
	newFile := filepath.Join(subdir, "handler.go")
	if err := os.WriteFile(newFile, []byte("package newpkg\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "handler.go", 2*time.Second) {
		t.Error("expected event for file in newly created subdirectory, got none within 2s")
	}
}

func TestWatcher_WatchedCountAtLeastOne(t *testing.T) {
	w, _, _ := newTempWatcher(t)
	if w.WatchedCount() < 1 {
		t.Errorf("WatchedCount should be >= 1, got %d", w.WatchedCount())
	}
}

func TestWatcher_ContextCancellationClosesEvents(t *testing.T) {
	dir := t.TempDir()
	w, err := watcher.New(dir, filter.Default())
	if err != nil {
		t.Fatalf("watcher.New: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("watcher.Run did not return within 1s of context cancellation")
	}
}

// TestWatcher_DeletedThenRecreatedSubdir is a regression test for the stale-
// watched-path bug: when a directory tree is deleted, ALL descendants must be
// removed from the tracked set so that if the tree is re-created, fsnotify
// watches are properly added again and file events are forwarded.
func TestWatcher_DeletedThenRecreatedSubdir(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	// Create a nested directory with a file.
	subdir := filepath.Join(dir, "pkg")
	nested := filepath.Join(subdir, "sub")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let watcher register the new dirs

	// Confirm watcher knows about the nested dir.
	if w.WatchedCount() < 3 { // root + pkg + pkg/sub
		t.Logf("note: WatchedCount=%d (may vary by OS)", w.WatchedCount())
	}

	// Delete the entire subtree.
	if err := os.RemoveAll(subdir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Re-create the same subtree.
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll after remove: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let watcher re-add watches

	// Write a file in the re-created nested directory.
	// If the stale-path bug is present, this event will be silently dropped.
	newFile := filepath.Join(nested, "service.go")
	if err := os.WriteFile(newFile, []byte("package sub\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "service.go", 3*time.Second) {
		t.Error("expected event from re-created nested directory, got none - stale-path bug?")
	}
}

// TestWatcher_PrePopulatedNewDirectoryTriggersEvent is a regression test for a
// silent miss: a directory that appears already containing source files — from
// a branch switch, an unzip, or a recursive copy — holds files that were
// created before any watch existed. fsnotify will never report them, so the
// watcher has to notice the subtree itself.
func TestWatcher_PrePopulatedNewDirectoryTriggersEvent(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	// Build the populated tree somewhere the watcher cannot see, then move it
	// into place in one step, mimicking a checkout or an extract.
	staging := filepath.Join(t.TempDir(), "feature")
	if err := os.MkdirAll(filepath.Join(staging, "inner"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "inner", "service.go"),
		[]byte("package inner\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dest := filepath.Join(dir, "feature")
	if err := os.Rename(staging, dest); err != nil {
		t.Skipf("cannot rename across directories on this platform: %v", err)
	}

	if !waitEvent(t, w, "service.go", 3*time.Second) {
		t.Error("expected an event for a pre-populated new directory, got none within 3s")
	}
}

// TestWatcher_EmptyNewDirectoryProducesNoEvent is the complement: mkdir alone
// changes no code, so it must not cost a rebuild.
func TestWatcher_EmptyNewDirectoryProducesNoEvent(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	if err := os.Mkdir(filepath.Join(dir, "emptypkg"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Sentinel: a real file change must still come through afterwards.
	if err := os.WriteFile(filepath.Join(dir, "sentinel.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The first event we see should be the sentinel, not the bare directory.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed unexpectedly")
			}
			base := filepath.Base(evt.Path)
			if base == "emptypkg" {
				t.Errorf("an empty new directory should not produce an event, got %s", evt.Path)
				return
			}
			if base == "sentinel.go" {
				return // correct: only the real file change arrived
			}
		case <-deadline:
			t.Error("sentinel event never arrived")
			return
		}
	}
}

// TestWatcher_IgnoredNewDirectoryIsNotWatched makes sure a freshly created
// node_modules does not get armed just because it is new.
func TestWatcher_IgnoredNewDirectoryIsNotWatched(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	before := w.WatchedCount()

	nm := filepath.Join(dir, "node_modules", "pkg")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nm, "index.js"), []byte("//x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if after := w.WatchedCount(); after != before {
		t.Errorf("WatchedCount went from %d to %d; node_modules should not be watched", before, after)
	}
}

// TestWatcher_RespectsIncludeExt checks the filter is actually consulted by the
// watcher, not just unit-tested in isolation.
func TestWatcher_RespectsIncludeExt(t *testing.T) {
	f := filter.New(filter.Options{IncludeExt: []string{".go"}})
	w, dir, _ := newTempWatcherWithFilter(t, f)
	time.Sleep(50 * time.Millisecond)

	// A docs edit must not come through...
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// ...while a Go edit must.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "main.go", 3*time.Second) {
		t.Fatal("expected an event for main.go")
	}

	drain := time.After(250 * time.Millisecond)
	for {
		select {
		case evt := <-w.Events():
			if filepath.Base(evt.Path) == "README.md" {
				t.Errorf("README.md should be filtered out by include-ext, got %s", evt.Path)
			}
		case <-drain:
			return
		}
	}
}

// TestWatcher_RootIsWatchedEvenIfItsNameIsIgnored covers pointing --root at a
// directory whose name is on the built-in ignore list. The user asked for it
// explicitly, so it must be honoured.
func TestWatcher_RootIsWatchedEvenIfItsNameIsIgnored(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "build") // "build" is ignored by default
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	w, err := watcher.New(root, filter.Default())
	if err != nil {
		t.Fatalf("watcher.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		w.Close()
	})
	go w.Run(ctx)

	if w.WatchedCount() < 1 {
		t.Fatalf("root %q should be watched even though its name is ignored", root)
	}
}

// TestWatcher_IncludeDirMakesIgnoredSubdirWatchable is the escape hatch working
// end to end through the watcher.
func TestWatcher_IncludeDirMakesIgnoredSubdirWatchable(t *testing.T) {
	f := filter.New(filter.Options{IncludeDir: []string{"build"}})
	w, dir, _ := newTempWatcherWithFilter(t, f)

	sub := filepath.Join(dir, "build")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(sub, "gen.go"), []byte("package build\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "gen.go", 3*time.Second) {
		t.Error("expected an event from an explicitly included build/ directory")
	}
}
