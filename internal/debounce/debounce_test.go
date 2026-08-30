package debounce

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func runAndCount(t *testing.T, quiet time.Duration, duration time.Duration, feed func(in chan<- string)) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	in := make(chan string, 16)
	out := make(chan struct{}, 16)
	d := New(quiet)
	go d.Run(ctx, in, out)

	go feed(in)

	var count int32
	go func() {
		for range out {
			atomic.AddInt32(&count, 1)
		}
	}()

	<-ctx.Done()
	time.Sleep(10 * time.Millisecond) // allow goroutines to finish
	return atomic.LoadInt32(&count)
}

func TestDebouncer_SingleFire(t *testing.T) {
	count := runAndCount(t, 30*time.Millisecond, 150*time.Millisecond, func(in chan<- string) {
		in <- "main.go"
	})
	if count != 1 {
		t.Errorf("expected 1 trigger, got %d", count)
	}
}

func TestDebouncer_CoalescesBurst(t *testing.T) {
	// 10 rapid events should result in exactly one trigger.
	count := runAndCount(t, 50*time.Millisecond, 300*time.Millisecond, func(in chan<- string) {
		for i := 0; i < 10; i++ {
			in <- "main.go"
			time.Sleep(5 * time.Millisecond)
		}
	})
	if count != 1 {
		t.Errorf("expected 1 trigger for a burst of 10 events, got %d", count)
	}
}

func TestDebouncer_TwoBursts(t *testing.T) {
	// Two bursts separated by a quiet period should each fire once.
	count := runAndCount(t, 30*time.Millisecond, 500*time.Millisecond, func(in chan<- string) {
		// First burst
		for i := 0; i < 3; i++ {
			in <- "main.go"
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // quiet period
		// Second burst
		for i := 0; i < 3; i++ {
			in <- "handler.go"
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // let timer fire
	})
	if count != 2 {
		t.Errorf("expected 2 triggers for 2 separate bursts, got %d", count)
	}
}

func TestDebouncer_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan string, 4)
	out := make(chan struct{}, 4)
	d := New(200 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, in, out)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("debouncer did not stop after context cancellation")
	}
}

func TestDebouncer_ClosedInputStopsRun(t *testing.T) {
	in := make(chan string)
	out := make(chan struct{}, 1)
	d := New(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(context.Background(), in, out)
	}()

	close(in)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("debouncer did not stop after the input channel was closed")
	}
}

// TestDebouncer_CoalescesWhenOutputFull documents the capacity-1 contract:
// a second trigger is dropped rather than queued, because a pending rebuild
// already covers the newer change.
func TestDebouncer_CoalescesWhenOutputFull(t *testing.T) {
	count := runAndCount(t, 20*time.Millisecond, 300*time.Millisecond, func(in chan<- string) {
		in <- "a.go"
		time.Sleep(80 * time.Millisecond) // let the first trigger fire
		in <- "b.go"
		time.Sleep(80 * time.Millisecond) // and the second
	})
	if count != 2 {
		t.Errorf("expected 2 triggers from 2 well-separated events, got %d", count)
	}
}

func TestNew_ClampsNegativeQuietWindow(t *testing.T) {
	if got := New(-time.Second).Quiet(); got != 0 {
		t.Errorf("New(-1s).Quiet() = %v, want 0", got)
	}
}
