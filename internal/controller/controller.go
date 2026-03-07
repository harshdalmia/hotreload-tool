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
	debounceDuration = 150

	crashThreshold = 2 * time.Second

	maxCrashStreak = 3

	crashBackoff = 5 * time.Second
)

type Controller struct {
	root     string
	buildCmd string
	execCmd  string
}

func New(root, buildCmd, execCmd string) (*Controller, error) {
	return &Controller{
		root:     root,
		buildCmd: buildCmd,
		execCmd:  execCmd,
	}, nil
}

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

	rawEvents := make(chan string, 256)

	rebuildCh := make(chan struct{}, 1)

	db := debounce.New(debounceDuration)
	go db.Run(ctx, rawEvents, rebuildCh)

	var (
		mu             sync.Mutex
		cancelInFlight context.CancelFunc = func() {}
		inFlightDone                      = closedChan()
	)

	cancelBuild := func() {
		mu.Lock()
		cancelInFlight()
		mu.Unlock()
	}

	stopInFlight := func() {
		cancelBuild()
		<-inFlightDone
	}

	go func() {
		for evt := range w.Events() {
			slog.Debug("file changed", "path", evt.Path, "op", evt.Op)
			cancelBuild()
			select {
			case rawEvents <- evt.Path:
			default:

			}
		}
	}()

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

			stopInFlight()

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

				srv.Stop()

				select {
				case <-bCtx.Done():
					slog.Debug("build superseded after completion; skipping server start")
					return
				default:
				}

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

				crashStreak = streak

				if err := srv.Start(bCtx); err != nil {
					slog.Error("failed to start server", "err", err)
				}
			}(buildCtx, streak)
		}
	}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
