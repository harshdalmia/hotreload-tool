// Package watcher wraps fsnotify to provide recursive directory watching
// with dynamic detection of newly created subdirectories and graceful
// handling of directory deletions.
//
// Design notes:
//   - A single fsnotify.Watcher is shared for all directories; watches are
//     added and removed dynamically as the directory tree changes.
//   - When a CREATE event arrives for a directory, we recursively add watches
//     so newly created packages are covered without restarting hotreload.
//     If that directory already contains source files (a branch switch, an
//     unzip, a cp -r), those files existed before the watch did and would
//     otherwise never be seen, so we synthesise one event for the subtree.
//   - File-descriptor exhaustion (ENOSPC / EMFILE on Linux) is handled
//     gracefully: we log a warning and continue rather than crashing.
//     To raise the limit: sudo sysctl fs.inotify.max_user_watches=524288
//   - Events are filtered before being forwarded to avoid spurious rebuilds
//     from editor temp files, .git/*, build output, and so on.
//   - Sends to the event channel block until the consumer catches up or the
//     context ends. Events are never silently dropped.
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

	"github.com/harshdalmia/hotreload-tool/internal/filter"
)

// eventBuffer is the capacity of the outgoing event channel. Large enough to
// absorb a formatter rewriting a whole package without stalling the fsnotify
// reader.
const eventBuffer = 256

// Event is a single meaningful file-system event after filtering.
type Event struct {
	Path string
	Op   fsnotify.Op
}

// Watcher recursively watches a directory tree.
type Watcher struct {
	root    string
	filter  *filter.Filter
	fw      *fsnotify.Watcher
	events  chan Event
	mu      sync.Mutex
	watched map[string]struct{} // set of currently watched dirs
}

// New creates a Watcher for the given root directory and performs an
// initial recursive walk to add all non-ignored subdirectories.
// A nil f means the default filter rules.
// Call Run(ctx) in a goroutine to start forwarding events, and
// Close() to release resources.
func New(root string, f *filter.Filter) (*Watcher, error) {
	if f == nil {
		f = filter.Default()
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		root:    root,
		filter:  f,
		fw:      fw,
		events:  make(chan Event, eventBuffer),
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
			w.handleNewDir(ctx, path, evt.Op)
			return
		}
	}

	// Clean up our tracking set when directories are removed.
	// We must remove the path itself AND all descendants: if /a/b is deleted,
	// /a/b/c is also gone. If we only removed /a/b, then when /a/b/c is
	// later re-created its path would still appear in watched, causing
	// addDirRecursive to skip calling fw.Add() on it and silently miss events.
	if evt.Has(fsnotify.Remove) || evt.Has(fsnotify.Rename) {
		w.forgetSubtree(path)
		// fsnotify automatically removes its inotify watch for a deleted path.
	}

	// Apply filter — skip temp files, hidden files, .git contents, etc.
	if w.filter.ShouldIgnoreFile(path) {
		return
	}

	w.send(ctx, Event{Path: path, Op: evt.Op})
}

// handleNewDir arms watches for a directory that has just appeared and, if it
// already holds source files, emits one event so the change is not missed.
//
// A bare mkdir is not a meaningful rebuild trigger, so an empty new directory
// produces no event. But a directory that arrives already populated — from a
// branch switch, an archive extraction, or a recursive copy — contains files
// created before any watch existed, and fsnotify will never report them.
func (w *Watcher) handleNewDir(ctx context.Context, dir string, op fsnotify.Op) {
	// A new node_modules or .git must not be watched just because it is new.
	// (addDirRecursive deliberately always watches its own root, so the check
	// has to happen here rather than inside the walk.)
	if w.filter.ShouldIgnoreDir(dir) {
		slog.Debug("ignoring new directory", "path", dir)
		return
	}

	slog.Debug("new directory detected, adding watches", "path", dir)
	if err := w.addDirRecursive(dir); err != nil {
		slog.Warn("failed to watch new directory", "path", dir, "err", err)
	}

	// One event is enough: the debouncer coalesces a burst into a single
	// rebuild anyway, so walking the whole subtree to emit hundreds of events
	// would buy nothing.
	if seed, ok := w.firstRelevantFile(dir); ok {
		slog.Debug("new directory already contains source files, triggering rebuild",
			"path", dir, "seed", seed)
		w.send(ctx, Event{Path: seed, Op: op})
	}
}

// send forwards evt to the consumer. It blocks until the consumer accepts the
// event or ctx ends, so no change is ever silently discarded.
func (w *Watcher) send(ctx context.Context, evt Event) {
	select {
	case w.events <- evt:
	case <-ctx.Done():
	}
}

// forgetSubtree removes path and every descendant from the tracked set.
func (w *Watcher) forgetSubtree(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prefix := path + string(filepath.Separator)
	for k := range w.watched {
		if k == path || strings.HasPrefix(k, prefix) {
			delete(w.watched, k)
		}
	}
}

// firstRelevantFile returns the first non-ignored file inside root, stopping
// the walk as soon as one is found.
func (w *Watcher) firstRelevantFile(root string) (string, bool) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate races with a tree still being written
		}
		if d.IsDir() {
			if path != root && w.filter.ShouldIgnoreDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if w.filter.ShouldIgnoreFile(path) {
			return nil
		}
		found = path
		return filepath.SkipAll
	})
	if err != nil {
		slog.Debug("scan of new directory failed", "path", root, "err", err)
	}
	return found, found != ""
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
		// The root itself is always watched even if its name would otherwise
		// be ignored: the user asked for it explicitly.
		if path != root && w.filter.ShouldIgnoreDir(path) {
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
