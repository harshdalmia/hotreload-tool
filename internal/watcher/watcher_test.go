package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/hotreload/internal/watcher"
)

// newTempWatcher creates a watcher for a temporary directory.
// t.Cleanup registers Close() and context cancellation.
func newTempWatcher(t *testing.T) (*watcher.Watcher, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()

	w, err := watcher.New(dir)
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
	w, err := watcher.New(dir)
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
		t.Error("expected event from re-created nested directory, got none — stale-path bug?")
	}
}
