// Package controller is the top-level orchestrator for hotreload. It wires
// together the file watcher, debouncer, builder, and server manager.
//
// Pipeline:
//
//	fsnotify events
//	   └─ watcher.Event{Path, Op}
//	        │
//	        ├─ cancelBuild()          ← immediate: abort any in-flight build
//	        │                           so a fast-finishing build cannot start
//	        │                           a server with stale code
//	        │
//	        └─ rawEvents chan string   ← fan-in to debouncer
//	             └─ debouncer          (quiet window — controls when the NEXT
//	                  │                 build starts, not cancellation)
//	                  └─ rebuildCh
//	                       └─ build goroutine (serial, one at a time)
//	                            └─ server process
//
// Alongside that pipeline the main loop watches the server's Exits() channel,
// so a server that dies on its own — a panic, a fatal error, an OOM kill — is
// restarted without waiting for the developer to save a file.
//
// Key invariants:
//
//   - At most one build goroutine runs at a time. stopInFlight blocks on the
//     previous goroutine's done channel before a new one is launched.
//   - A superseded build never touches the running server. The bCtx.Done()
//     check happens BEFORE Stop(), so a build that lost the race leaves the
//     current server alone instead of killing it and declining to replace it.
//   - The server is started under the root context, never a build context.
//     Build contexts are cancelled on the next file event; the server must
//     outlive that.
//   - Exits caused by hotreload itself are never treated as crashes. That
//     distinction is made in the process package, which marks an exit
//     Intentional when Stop caused it.
package controller

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/harshdalmia/hotreload-tool/internal/config"
	"github.com/harshdalmia/hotreload-tool/internal/crash"
	"github.com/harshdalmia/hotreload-tool/internal/debounce"
	"github.com/harshdalmia/hotreload-tool/internal/filter"
	"github.com/harshdalmia/hotreload-tool/internal/logstream"
	"github.com/harshdalmia/hotreload-tool/internal/process"
	"github.com/harshdalmia/hotreload-tool/internal/watcher"
)

// rawEventBuffer is the capacity of the channel feeding the debouncer. It only
// carries paths for logging; the debouncer needs a signal, not a queue.
const rawEventBuffer = 256

// Builder compiles the project.
type Builder interface {
	// Build runs the build command, returning an error if it fails or if ctx
	// is cancelled mid-build.
	Build(ctx context.Context) error
}

// Server manages the lifecycle of the built artefact.
type Server interface {
	// Start launches the server, which is terminated when ctx ends.
	Start(ctx context.Context) error
	// Stop terminates the server and blocks until it has been reaped.
	Stop()
	// IsRunning reports whether a server process is currently alive.
	IsRunning() bool
	// Exits reports exits that hotreload did not cause. Never closed.
	Exits() <-chan process.ExitInfo
}

// Watcher reports filtered file-system changes.
type Watcher interface {
	// Run forwards events until ctx ends, then closes Events().
	Run(ctx context.Context)
	// Events returns the channel of filtered events.
	Events() <-chan watcher.Event
	// Close releases the underlying watcher.
	Close() error
	// WatchedCount returns how many directories are being watched.
	WatchedCount() int
}

// Controller orchestrates the hot-reload cycle.
type Controller struct {
	cfg config.Config

	// newWatcher is a field rather than a direct call so tests can supply a
	// fake without touching the file system.
	newWatcher func() (Watcher, error)

	preCmd   Builder
	builder  Builder
	postCmd  Builder
	server   Server
	detector *crash.Detector
}

// New creates a Controller wired to the real watcher, builder, and server.
func New(cfg config.Config) *Controller {
	f := filter.New(filter.Options{
		IncludeExt: cfg.IncludeExt,
		ExcludeDir: cfg.ExcludeDir,
		IncludeDir: cfg.IncludeDir,
	})

	// One sink per underlying stream, shared by every prefixed writer, so two
	// processes cannot interleave halfway through a line.
	colour := logstream.SupportsColour(os.Stdout)
	outSink := logstream.NewSink(os.Stdout)
	errSink := logstream.NewSink(os.Stderr)

	streams := func(label string) process.Streams {
		prefix := logstream.Prefix(label, colour)
		return process.Streams{
			Stdout: outSink.Writer(prefix),
			Stderr: errSink.Writer(prefix),
		}
	}

	return &Controller{
		cfg:        cfg,
		newWatcher: watcherFactory(cfg, f),
		preCmd: process.NewBuilder(cfg.PreCmd,
			process.WithLabel("pre"), process.WithBuilderStreams(streams("pre"))),
		builder: process.NewBuilder(cfg.Build,
			process.WithLabel("build"), process.WithBuilderStreams(streams("build"))),
		postCmd: process.NewBuilder(cfg.PostCmd,
			process.WithLabel("post"), process.WithBuilderStreams(streams("post"))),
		server: process.NewServer(cfg.Exec,
			process.WithKillDelay(cfg.KillDelay), process.WithStreams(streams("app"))),
		detector: crash.NewDefault(),
	}
}

// watcherFactory chooses between filesystem notifications and polling.
//
// Polling is used when asked for explicitly, and also as a fallback when
// notifications cannot be set up at all. The fallback matters because the
// alternative is refusing to start: a developer who has hit the inotify watch
// limit is better served by a slightly slower reload than by no tool.
//
// Note that it cannot detect the other failure mode, where notifications are
// accepted but never delivered, as happens on Docker bind mounts. Nothing
// reports that condition, so --poll has to be requested.
func watcherFactory(cfg config.Config, f *filter.Filter) func() (Watcher, error) {
	newPoller := func() (Watcher, error) {
		p, err := watcher.NewPoller(cfg.Root, f, cfg.PollInterval)
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	if cfg.Poll {
		return newPoller
	}

	return func() (Watcher, error) {
		w, err := watcher.New(cfg.Root, f)
		if err != nil {
			slog.Warn("filesystem notifications unavailable, falling back to polling",
				"err", err, "interval", cfg.PollInterval)
			return newPoller()
		}
		return w, nil
	}
}

// Run starts the hot-reload loop and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	w, err := c.newWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	slog.Info("watching directory tree", "root", c.cfg.Root, "dirs", w.WatchedCount())

	// Cancelled before w.Close() runs, thanks to LIFO defer order.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	go w.Run(watchCtx)

	// rawEvents carries file paths from the watcher to the debouncer.
	rawEvents := make(chan string, rawEventBuffer)

	// rebuildCh receives one signal per debounced burst — it controls when
	// the NEXT build starts. Cancellation of the current build is separate.
	rebuildCh := make(chan struct{}, 1)

	db := debounce.New(c.cfg.Debounce)
	go db.Run(ctx, rawEvents, rebuildCh)

	// mu guards cancelInFlight so it is safe to call from both the event
	// fan-in goroutine (for immediate cancellation) and the main loop.
	var (
		mu             sync.Mutex
		cancelInFlight context.CancelFunc = func() {}
		inFlightDone                      = closedChan()
	)

	// cancelBuild immediately aborts the in-flight build (if any).
	// Safe to call from any goroutine.
	cancelBuild := func() {
		mu.Lock()
		cancelInFlight()
		mu.Unlock()
	}

	// stopInFlight cancels the in-flight build and blocks until its goroutine
	// has fully exited. Must only be called from the main event loop.
	stopInFlight := func() {
		cancelBuild()
		<-inFlightDone
	}

	// Fan watcher events into the debouncer.
	//
	// Crucially, we also call cancelBuild() on EVERY file event — not just
	// when the debounce fires. This fixes the "fast build starts stale server"
	// race: without this, a build that finishes inside the debounce window
	// would see bCtx.Done() uncancelled and start the server with code from
	// BEFORE the most recent save. With this, the build context is cancelled
	// the instant any change is detected, and the post-build bCtx.Done()
	// check causes the build to leave the running server alone.
	go func() {
		for evt := range w.Events() {
			slog.Debug("file changed", "path", evt.Path, "op", evt.Op)
			cancelBuild() // immediate — don't wait for debounce
			select {
			case rawEvents <- evt.Path:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Fire an initial build immediately — don't wait for a file change.
	slog.Info("triggering initial build")
	select {
	case rebuildCh <- struct{}{}:
	default:
	}

	// buildWatch is non-nil exactly while a build goroutine is active. A nil
	// channel blocks forever in a select, which is what makes the "no build
	// running" case fall through cleanly.
	var buildWatch chan struct{}

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down...")
			stopInFlight()
			c.server.Stop()
			return nil

		case <-buildWatch:
			// The build goroutine finished; nothing to do but stop watching it.
			buildWatch = nil

		case info := <-c.server.Exits():
			c.handleUnexpectedExit(ctx, info, buildWatch != nil)

		case <-rebuildCh:
			// The in-flight build was already cancelled the moment a file event
			// arrived. stopInFlight just waits for the goroutine to exit so we
			// never run two builds concurrently.
			stopInFlight()

			// Allocate a fresh done channel and publish it before launching the
			// goroutine, so stopInFlight() always sees the current channel
			// regardless of scheduling order.
			done := make(chan struct{})
			inFlightDone = done
			buildWatch = done

			buildCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			cancelInFlight = cancel
			mu.Unlock()

			go c.buildAndRestart(ctx, buildCtx, done)
		}
	}
}

// buildAndRestart compiles the project and, on success, swaps in a new server.
//
// rootCtx governs the server's lifetime; bCtx governs this build attempt and
// is cancelled as soon as another file event arrives.
func (c *Controller) buildAndRestart(rootCtx, bCtx context.Context, done chan struct{}) {
	defer close(done)

	// Pre hook first: code generators run here, and the build below may depend
	// on what they produce. A failure is treated exactly like a build failure,
	// leaving the running server untouched.
	if err := c.preCmd.Build(bCtx); err != nil {
		if bCtx.Err() != nil {
			slog.Debug("pre-cmd superseded by newer change")
		} else {
			slog.Error("pre-cmd failed - skipping build, server left running", "err", err)
		}
		return
	}

	if err := c.builder.Build(bCtx); err != nil {
		if bCtx.Err() != nil {
			slog.Debug("build superseded by newer change")
		} else {
			slog.Error("build failed - fix errors above and save again", "err", err)
		}
		return
	}

	// A file event may have arrived while we were compiling, in which case
	// cancelBuild() has cancelled bCtx and this binary is already stale.
	//
	// This check must happen BEFORE stopping the server. Stopping first and
	// then bailing out would kill a working server and decline to start a
	// replacement, leaving the developer with nothing running until the next
	// build finishes — or indefinitely, if that build fails.
	select {
	case <-bCtx.Done():
		slog.Debug("build superseded after completion; keeping the current server")
		return
	default:
	}

	// A fresh binary invalidates the old one's crash history.
	c.detector.Reset()

	// Free the port before we rebind it.
	c.server.Stop()

	if err := c.server.Start(rootCtx); err != nil {
		slog.Error("failed to start server", "err", err)
		return
	}

	// Post hook last. The server is already up, so a failure here is worth
	// reporting but must not tear anything down: the reload succeeded.
	if err := c.postCmd.Build(rootCtx); err != nil {
		slog.Warn("post-cmd failed; the server is running regardless", "err", err)
	}
}

// handleUnexpectedExit reacts to a server process that ended without
// hotreload asking it to.
//
// buildActive reports whether a build goroutine is currently running; if one
// is, it will start a fresh server when it finishes, so restarting here would
// race it.
func (c *Controller) handleUnexpectedExit(ctx context.Context, info process.ExitInfo, buildActive bool) {
	uptime := info.Uptime.Round(time.Millisecond)

	// A clean exit is most likely a one-shot command or a deliberate
	// shutdown. Restarting it would spin forever, so report and wait for the
	// next change instead.
	if info.Err == nil {
		slog.Info("server exited cleanly on its own; waiting for the next change", "uptime", uptime)
		return
	}

	if buildActive {
		slog.Debug("server crashed while a build is in flight; that build will start the replacement",
			"uptime", uptime)
		return
	}
	if c.server.IsRunning() {
		slog.Debug("ignoring stale crash notification; a newer server is already running")
		return
	}

	c.detector.Record()
	slog.Warn("server crashed", "err", info.Err, "uptime", uptime,
		"crashes_in_window", c.detector.Count())

	if backoff := c.detector.Backoff(); backoff > 0 {
		slog.Warn("crash loop detected, backing off before restart", "delay", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}

	slog.Info("restarting crashed server")
	if err := c.server.Start(ctx); err != nil {
		slog.Error("failed to restart crashed server", "err", err)
	}
}

// closedChan returns an already-closed channel. Used to initialise
// inFlightDone so stopInFlight() is safe before the first build starts.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
