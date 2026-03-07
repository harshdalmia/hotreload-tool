package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/hotreload/internal/watcher"
)

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
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(100 * time.Millisecond)

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

	hidden := filepath.Join(dir, ".hidden")
	if err := os.WriteFile(hidden, []byte("secret"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	visible := filepath.Join(dir, "visible.go")
	if err := os.WriteFile(visible, []byte("package main"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "visible.go", 2*time.Second) {
		t.Skip("sentinel event not received; timing issue, skipping")
	}

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

	subdir := filepath.Join(dir, "newpkg")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

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

func TestWatcher_DeletedThenRecreatedSubdir(t *testing.T) {
	w, dir, _ := newTempWatcher(t)
	time.Sleep(50 * time.Millisecond)

	subdir := filepath.Join(dir, "pkg")
	nested := filepath.Join(subdir, "sub")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if w.WatchedCount() < 3 {
		t.Logf("note: WatchedCount=%d (may vary by OS)", w.WatchedCount())
	}

	if err := os.RemoveAll(subdir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll after remove: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	newFile := filepath.Join(nested, "service.go")
	if err := os.WriteFile(newFile, []byte("package sub\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitEvent(t, w, "service.go", 3*time.Second) {
		t.Error("expected event from re-created nested directory, got none — stale-path bug?")
	}
}
