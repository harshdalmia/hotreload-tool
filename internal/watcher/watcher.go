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

type Event struct {
	Path string
	Op   fsnotify.Op
}

type Watcher struct {
	root    string
	fw      *fsnotify.Watcher
	events  chan Event
	mu      sync.Mutex
	watched map[string]struct{}
}

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

func (w *Watcher) Events() <-chan Event {
	return w.events
}

func (w *Watcher) Close() error {
	return w.fw.Close()
}

func (w *Watcher) WatchedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.watched)
}

func (w *Watcher) handleEvent(ctx context.Context, evt fsnotify.Event) {
	path := evt.Name

	if evt.Has(fsnotify.Create) {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			slog.Debug("new directory detected, adding watches", "path", path)
			if err := w.addDirRecursive(path); err != nil {
				slog.Warn("failed to watch new directory", "path", path, "err", err)
			}

			return
		}
	}

	if evt.Has(fsnotify.Remove) || evt.Has(fsnotify.Rename) {
		w.mu.Lock()
		prefix := path + string(filepath.Separator)
		for k := range w.watched {
			if k == path || strings.HasPrefix(k, prefix) {
				delete(w.watched, k)
			}
		}
		w.mu.Unlock()

	}

	if filter.ShouldIgnoreFile(path) {
		return
	}

	select {
	case <-ctx.Done():
	case w.events <- Event{Path: path, Op: evt.Op}:
	default:
		slog.Debug("event channel full, dropping event", "path", path)
	}
}

func (w *Watcher) addDirRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {

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
