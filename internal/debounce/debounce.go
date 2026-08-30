// Package debounce coalesces bursts of file-system events into a single
// rebuild trigger.
//
// Editors rarely produce one event per save. A single write in vim can look
// like write → chmod → rename-temp → rename-back, and formatters or code
// generators can touch dozens of files at once. Rebuilding for each of those
// would waste a compile per event and could start a server from a
// half-written tree.
//
// Run waits for a quiet window after the most recent event before emitting a
// single signal. The window resets on every event, so a continuous stream of
// changes produces exactly one trigger once it settles.
//
// Note that debouncing controls only when the NEXT build starts. Cancelling
// an in-flight build happens immediately in the controller, without waiting
// for the quiet window.
package debounce

import (
	"context"
	"log/slog"
	"time"
)

// Debouncer emits one signal per burst of input events.
type Debouncer struct {
	quiet time.Duration
}

// New returns a Debouncer that waits for quiet with no input before emitting.
func New(quiet time.Duration) *Debouncer {
	if quiet < 0 {
		quiet = 0
	}
	return &Debouncer{quiet: quiet}
}

// Quiet returns the configured quiet window.
func (d *Debouncer) Quiet() time.Duration { return d.quiet }

// Run reads changed paths from in and emits one struct{} on out per burst.
// It returns when ctx is cancelled or in is closed.
//
// Sends on out are non-blocking: out is expected to have capacity 1 and
// carries no data, so if a trigger is already pending there is nothing to
// gain by queueing a second one. Dropping in that case is coalescing, not
// lost work.
func (d *Debouncer) Run(ctx context.Context, in <-chan string, out chan<- struct{}) {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false

	send := func() {
		select {
		case out <- struct{}{}:
		default:
			slog.Debug("rebuild already pending, coalescing trigger")
		}
	}

	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return

		case path, ok := <-in:
			if !ok {
				timer.Stop()
				return
			}
			slog.Debug("file changed, resetting debounce timer", "path", path)
			if timerActive {
				if !timer.Stop() {
					// Drain a value the timer may have already delivered so
					// the next Reset starts from a clean channel.
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(d.quiet)
			timerActive = true

		case <-timer.C:
			timerActive = false
			slog.Debug("debounce timer fired, triggering rebuild")
			send()
		}
	}
}
