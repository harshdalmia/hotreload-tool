// Package watcher wraps fsnotify to provide recursive directory watching
// with dynamic detection of newly created subdirectories and graceful
// handling of directory deletions.
//
// Design notes:
//   - A single fsnotify.Watcher is shared for all directories; watches are
//     added and removed dynamically as the directory tree changes.
//   - When a CREATE event arrives for a directory, we recursively add watches
//     so newly created packages are covered without restarting hotreload.
//   - File-descriptor exhaustion (ENOSPC / EMFILE on Linux) is handled
//     gracefully: we log a warning and continue rather than crashing.
//     To raise the limit: sudo sysctl fs.inotify.max_user_watches=524288
//   - Events are filtered (via the filter package) before being forwarded
//     to avoid spurious rebuilds from editor temp files, .git/*, etc.
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/example/hotreload/internal/filter"
)

// Event is a single meaningful file-system event after filtering.
type Event struct {
	Path string
	Op   fsnotify.Op
}

// Watcher recursively watches a directory tree.
type Watcher struct {
	root    string
	fw      *fsnotify.Watcher
	events  chan Event
	mu      sync.Mutex
	watched map[string]struct{} // set of currently watched dirs
}

// New creates a Watcher for the given root directory and performs an
// initial recursive walk to add all non-ignored subdirectories.
// Call Run(ctx) in a goroutine to start forwarding events, and
// Close() to release resources.
func New(root string) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		root:    root,
		fw:      fw,
		events:  make(chan Event, 256),
		watched: make(map[string]struct{}),
	}

	if err := w.addDirRecursive(root); err != nil {
		fw.Close()
		return nil, err
	}

	return w, nil
}

// Run reads raw fsnotify events and forwards filtered ones to Events().
// It exits when ctx is cancelled or the underlying watcher is closed.
// Run closes the Events() channel when it returns.
func (w *Watcher) Run(ctx context.Context) {
	defer close(w.events)

	for {
		select {
		case <-ctx.Done():
			return

		case evt, ok := <-w.fw.Events:
			if !ok {
				return
			}
			w.handleEvent(ctx, evt)

		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			slog.Warn("fsnotify error", "err", err)
		}
	}
}

// Events returns the channel on which filtered events are delivered.
// The channel is closed when Run returns.
func (w *Watcher) Events() <-chan Event {
	return w.events
}

// Close releases the underlying fsnotify watcher.
func (w *Watcher) Close() error {
	return w.fw.Close()
}

// WatchedCount returns the number of directories currently being watched.
// Useful for diagnostics and logging.
func (w *Watcher) WatchedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.watched)
}

// handleEvent processes one raw fsnotify event.
func (w *Watcher) handleEvent(ctx context.Context, evt fsnotify.Event) {
	path := evt.Name

	// Dynamically watch newly created directories so new packages are covered.
	if evt.Has(fsnotify.Create) {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			slog.Debug("new directory detected, adding watches", "path", path)
			if err := w.addDirRecursive(path); err != nil {
				slog.Warn("failed to watch new directory", "path", path, "err", err)
			}
			// Directory creation alone is not a meaningful rebuild trigger.
			return
		}
	}

	// Clean up our tracking set when directories are removed.
	// We must remove the path itself AND all descendants: if /a/b is deleted,
	// /a/b/c is also gone. If we only removed /a/b, then when /a/b/c is
	// later re-created its path would still appear in watched, causing
	// addDirRecursive to skip calling fw.Add() on it and silently miss events.
	if evt.Has(fsnotify.Remove) || evt.Has(fsnotify.Rename) {
		w.mu.Lock()
		prefix := path + string(filepath.Separator)
		for k := range w.watched {
			if k == path || strings.HasPrefix(k, prefix) {
				delete(w.watched, k)
			}
		}
		w.mu.Unlock()
		// fsnotify automatically removes its inotify watch for a deleted path.
	}

	// Apply filter — skip temp files, hidden files, .git contents, etc.
	if filter.ShouldIgnoreFile(path) {
		return
	}

	// Forward to the debouncer. Drop if the buffer is full — the debouncer
	// only needs one signal per burst, not one per event.
	select {
	case <-ctx.Done():
	case w.events <- Event{Path: path, Op: evt.Op}:
	default:
		slog.Debug("event channel full, dropping event", "path", path)
	}
}

// addDirRecursive walks path and adds an fsnotify watch for each
// non-ignored subdirectory. Errors from inaccessible paths are tolerated
// so a transiently missing directory doesn't abort the whole walk.
func (w *Watcher) addDirRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Tolerate errors (e.g. directory deleted between Walk and visit).
			slog.Debug("skipping path during walk", "path", path, "err", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if filter.ShouldIgnoreDir(path) {
			slog.Debug("ignoring directory", "path", path)
			return filepath.SkipDir
		}

		w.mu.Lock()
		_, already := w.watched[path]
		w.mu.Unlock()
		if already {
			return nil
		}

		if err := w.fw.Add(path); err != nil {
			// Log and continue — hitting the inotify fd limit shouldn't crash.
			// To raise the limit: sudo sysctl fs.inotify.max_user_watches=524288
			slog.Warn("could not add watch (fd limit?)", "path", path, "err", err)
			return nil
		}

		w.mu.Lock()
		w.watched[path] = struct{}{}
		w.mu.Unlock()

		slog.Debug("watching directory", "path", path)
		return nil
	})
}
