// Package controller is the top-level orchestrator for hotreload. It wires
// together the file watcher, debouncer, build runner, and server manager.
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
//	             └─ debouncer          (150 ms quiet window — controls when
//	                  │                 the NEXT build starts, not cancellation)
//	                  └─ rebuildCh
//	                       └─ build goroutine (serial, one at a time)
//	                            └─ server process
//
// Key invariant: when the build goroutine calls srv.Start(), bCtx has NOT
// been cancelled. This is guaranteed by the post-build bCtx.Done() check:
// if a file event arrived while the build was running, cancelBuild() will
// have cancelled bCtx, and the check causes the goroutine to return without
// starting the server. The debounce will then fire and start a fresh build.
package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/example/hotreload/internal/debounce"
	"github.com/example/hotreload/internal/process"
	"github.com/example/hotreload/internal/watcher"
)

const (
	// debounceDuration is the quiet window after the last file event before
	// a rebuild is triggered. 150 ms absorbs typical editor write patterns
	// (write, chmod, rename-temp, rename-back) while feeling instant to the dev.
	debounceDuration = 150

	// crashThreshold: server uptime below this value counts as a crash.
	crashThreshold = 2 * time.Second

	// maxCrashStreak: consecutive crashes before back-off kicks in.
	maxCrashStreak = 3

	// crashBackoff: pause when a crash loop is detected.
	crashBackoff = 5 * time.Second
)

// Controller orchestrates the hot-reload cycle.
type Controller struct {
	root     string
	buildCmd string
	execCmd  string
}

// New creates a Controller.
func New(root, buildCmd, execCmd string) (*Controller, error) {
	return &Controller{
		root:     root,
		buildCmd: buildCmd,
		execCmd:  execCmd,
	}, nil
}

// Run starts the hot-reload loop and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	w, err := watcher.New(c.root)
	if err != nil {
		return err
	}
	defer w.Close()

	slog.Info("watching directory tree", "root", c.root, "dirs", w.WatchedCount())

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	go w.Run(watchCtx)

	// rawEvents carries file paths from the watcher to the debouncer.
	rawEvents := make(chan string, 256)

	// rebuildCh receives one signal per debounced burst — it controls when
	// the NEXT build starts. Cancellation of the current build is separate.
	rebuildCh := make(chan struct{}, 1)

	db := debounce.New(debounceDuration)
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
	// race: without this, a build that finishes inside the 150 ms debounce
	// window would see bCtx.Done() uncancelled and start the server with code
	// from BEFORE the most recent save. With this fix, the build context is
	// cancelled the instant any change is detected, and the post-build
	// bCtx.Done() check causes it to return without starting the server.
	go func() {
		for evt := range w.Events() {
			slog.Debug("file changed", "path", evt.Path, "op", evt.Op)
			cancelBuild() // immediate — don't wait for debounce
			select {
			case rawEvents <- evt.Path:
			default:
				// Channel full; debouncer only needs a signal, not every path.
			}
		}
	}()

	// Fire an initial build immediately — don't wait for a file change.
	slog.Info("triggering initial build")
	select {
	case rebuildCh <- struct{}{}:
	default:
	}

	srv := process.NewServer(c.execCmd)
	crashStreak := 0

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down…")
			stopInFlight()
			srv.Stop()
			return nil

		case <-rebuildCh:
			// The in-flight build was already cancelled the moment a file event
			// arrived. stopInFlight just waits for the goroutine to exit so we
			// never run two builds concurrently.
			stopInFlight()

			// Allocate a fresh done channel and update inFlightDone before
			// launching the goroutine, so stopInFlight() always sees the current
			// channel regardless of scheduling order.
			done := make(chan struct{})
			inFlightDone = done

			buildCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			cancelInFlight = cancel
			mu.Unlock()

			streak := crashStreak

			go func(bCtx context.Context, streak int) {
				defer close(done)

				if err := process.RunBuild(bCtx, c.buildCmd); err != nil {
					if bCtx.Err() != nil {
						slog.Debug("build superseded by newer change")
					} else {
						slog.Error("build failed — fix errors above and save again",
							"err", err)
					}
					return
				}

				// Build succeeded. Stop the old server first so the port is freed
				// before we start the new one.
				srv.Stop()

				// A file event may have arrived while we were compiling.
				// cancelBuild() will have cancelled bCtx. Do not start a server
				// with a binary that may not reflect the latest save; the debounce
				// will fire shortly and start a fresh build.
				select {
				case <-bCtx.Done():
					slog.Debug("build superseded after completion; skipping server start")
					return
				default:
				}

				// Crash-loop back-off.
				if uptime, didExit := srv.UptimeAtExit(); didExit && uptime < crashThreshold {
					streak++
					slog.Warn("server crashed quickly",
						"streak", streak,
						"uptime", uptime.Round(time.Millisecond))
					if streak >= maxCrashStreak {
						slog.Warn("crash loop detected, pausing before restart",
							"streak", streak, "delay", crashBackoff)
						select {
						case <-time.After(crashBackoff):
						case <-bCtx.Done():
							return
						}
					}
				} else {
					streak = 0
				}

				// Write back. Safe because stopInFlight() guarantees serial
				// execution — no two goroutines reach this point at the same time.
				crashStreak = streak

				if err := srv.Start(bCtx); err != nil {
					slog.Error("failed to start server", "err", err)
				}
			}(buildCtx, streak)
		}
	}
}

// closedChan returns an already-closed channel. Used to initialise
// inFlightDone so stopInFlight() is safe before the first build starts.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
