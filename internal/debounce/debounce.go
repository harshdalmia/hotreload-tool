package debounce

import (
	"context"
	"log/slog"
	"time"
)

type Debouncer struct {
	quietMs int
}

func New(quietMs int) *Debouncer {
	return &Debouncer{quietMs: quietMs}
}

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
