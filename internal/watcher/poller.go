package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/harshdalmia/hotreload-tool/internal/filter"
)

// Poller detects changes by rescanning the tree on an interval instead of
// subscribing to filesystem notifications.
//
// This exists because notifications are not always available. inotify does not
// fire for writes made on the host side of a Docker bind mount, nor reliably
// across several network filesystems or some WSL configurations. In those
// environments a notification-based watcher starts cleanly, reports the
// directories it is watching, and then simply never reloads: the worst kind of
// failure, because nothing looks wrong. Polling trades a little CPU for
// working everywhere.
//
// It also sidesteps two problems the fsnotify path has to work around. There is
// no per-directory watch to register, so the file-descriptor limits that make
// large trees expensive do not apply, and there is no window between a
// directory being created and its watch being armed during which changes are
// lost. A scan sees whatever is on disk at the time.
//
// The trade is latency, bounded by the interval, and a walk per tick.
type Poller struct {
	root     string
	filter   *filter.Filter
	interval time.Duration
	events   chan Event

	mu       sync.Mutex
	snapshot map[string]fileState
	dirCount int
}

// fileState is the fingerprint used to decide whether a file changed.
//
// Modification time plus size catches everything a developer's editor does. A
// content hash would also catch a write that preserves both, but it would mean
// reading every file on every tick, which is precisely the cost polling is
// trying to keep small.
type fileState struct {
	modTime time.Time
	size    int64
}

// NewPoller creates a Poller for root. A nil f means the default filter rules.
// The interval defaults to 500ms when not positive.
func NewPoller(root string, f *filter.Filter, interval time.Duration) (*Poller, error) {
	if f == nil {
		f = filter.Default()
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "watch", Path: root, Err: fs.ErrInvalid}
	}

	p := &Poller{
		root: root,
		// Scope rules to the tree, as the fsnotify watcher does: components
		// above the root must not trigger ignore rules.
		filter:   f.WithRoot(root),
		interval: interval,
		events:   make(chan Event, eventBuffer),
	}

	// Establish a baseline without emitting anything. The controller triggers
	// an initial build on its own, so reporting every existing file as new
	// would only cause a duplicate.
	initial, dirs := p.scan()
	p.snapshot = initial
	p.dirCount = dirs

	return p, nil
}

// Run rescans the tree until ctx is cancelled, forwarding what changed.
// It closes Events() when it returns.
func (p *Poller) Run(ctx context.Context) {
	defer close(p.events)

	slog.Info("polling for changes", "root", p.root, "interval", p.interval)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !p.tick(ctx) {
				return
			}
		}
	}
}

// tick performs one scan and emits the differences. It reports false if ctx
// ended while events were being delivered.
func (p *Poller) tick(ctx context.Context) bool {
	current, dirs := p.scan()

	p.mu.Lock()
	previous := p.snapshot
	p.snapshot = current
	p.dirCount = dirs
	p.mu.Unlock()

	for path, now := range current {
		before, existed := previous[path]
		switch {
		case !existed:
			if !p.send(ctx, Event{Path: path, Op: fsnotify.Create}) {
				return false
			}
		case now.modTime != before.modTime || now.size != before.size:
			if !p.send(ctx, Event{Path: path, Op: fsnotify.Write}) {
				return false
			}
		}
	}

	for path := range previous {
		if _, still := current[path]; !still {
			if !p.send(ctx, Event{Path: path, Op: fsnotify.Remove}) {
				return false
			}
		}
	}

	return true
}

// scan walks the tree and returns the state of every file that matters, along
// with the number of directories visited.
func (p *Poller) scan() (files map[string]fileState, dirs int) {
	files = make(map[string]fileState)

	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory may vanish between being listed and being visited.
			// Tolerate it: the next tick will show the correct picture.
			slog.Debug("skipping path during poll scan", "path", path, "err", err)
			return nil
		}

		if d.IsDir() {
			// The root is always scanned even if its own name would be ignored;
			// the user asked for it explicitly.
			if path != p.root && p.filter.ShouldIgnoreDir(path) {
				return filepath.SkipDir
			}
			dirs++
			return nil
		}

		if p.filter.ShouldIgnoreFile(path) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			// Being deleted while we look at it is normal during a build.
			// Skip it this tick; the next scan reports the settled state.
			slog.Debug("could not stat file during poll scan", "path", path, "err", err)
			return nil
		}
		files[path] = fileState{modTime: info.ModTime(), size: info.Size()}
		return nil
	})
	if err != nil {
		slog.Warn("poll scan failed", "root", p.root, "err", err)
	}

	return files, dirs
}

// send forwards evt, blocking until the consumer takes it or ctx ends, so no
// change is silently dropped. It reports false once ctx is done.
func (p *Poller) send(ctx context.Context, evt Event) bool {
	select {
	case p.events <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// Events returns the channel of detected changes. Closed when Run returns.
func (p *Poller) Events() <-chan Event { return p.events }

// Close releases resources. Polling holds none, so this only exists to satisfy
// the same interface as the fsnotify-backed watcher.
func (p *Poller) Close() error { return nil }

// WatchedCount returns the number of directories seen by the last scan.
func (p *Poller) WatchedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirCount
}
