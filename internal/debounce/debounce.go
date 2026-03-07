// Package debounce coalesces rapid successive file-system events into a
// single rebuild signal after a configurable quiet period.
//
// Why debouncing matters:
// Editors do not write files atomically. A single "save" in vim, VS Code,
// or GoLand typically produces 3–5 fsnotify events (write, chmod, rename,
// write-temp, rename-back…). Without debouncing each of those events would
// trigger a separate rebuild, wasting CPU and delaying the final result.
//
// The Debouncer reads path strings from an input channel, resets a timer on
// every arrival, and sends one signal on the output channel after the quiet
// window elapses.
package debounce

import (
	"context"
	"log/slog"
	"time"
)

// Debouncer coalesces events from an input channel into a single signal on
// an output channel, waiting for quietMs milliseconds of inactivity.
type Debouncer struct {
	quietMs int
}

// New creates a Debouncer with the given quiet window in milliseconds.
// A value of 150 ms is a good default: fast enough to feel instant,
// long enough to absorb typical editor save patterns.
func New(quietMs int) *Debouncer {
	return &Debouncer{quietMs: quietMs}
}

// Run reads strings from in and, after each burst of activity, sends one
// struct{}{} on out. It returns when ctx is cancelled or in is closed.
//
// A "burst" ends when no new values arrive within d.quietMs milliseconds.
// The first event in a burst resets the timer; subsequent events keep
// resetting it until the quiet window finally expires.
func (d *Debouncer) Run(ctx context.Context, in <-chan string, out chan<- struct{}) {
	quiet := time.Duration(d.quietMs) * time.Millisecond
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false

	send := func() {
		select {
		case out <- struct{}{}:
		default:
			// out already has a pending signal; no need to add another.
		}
	}

	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return

		case path, ok := <-in:
			if !ok {
				// Input channel closed; drain timer and exit.
				timer.Stop()
				return
			}
			slog.Debug("file changed, resetting debounce timer", "path", path)
			if timerActive {
				if !timer.Stop() {
					// Timer already fired; drain the channel.
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(quiet)
			timerActive = true

		case <-timer.C:
			timerActive = false
			slog.Debug("debounce timer fired, triggering rebuild")
			send()
		}
	}
}
