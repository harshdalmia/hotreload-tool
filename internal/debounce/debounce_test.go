package debounce

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func runAndCount(t *testing.T, quietMs int, duration time.Duration, feed func(in chan<- string)) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	in := make(chan string, 16)
	out := make(chan struct{}, 16)
	d := New(quietMs)
	go d.Run(ctx, in, out)

	go feed(in)

	var count int32
	go func() {
		for range out {
			atomic.AddInt32(&count, 1)
		}
	}()

	<-ctx.Done()
	time.Sleep(10 * time.Millisecond)
	return atomic.LoadInt32(&count)
}

func TestDebouncer_SingleFire(t *testing.T) {
	count := runAndCount(t, 30, 150*time.Millisecond, func(in chan<- string) {
		in <- "main.go"
	})
	if count != 1 {
		t.Errorf("expected 1 trigger, got %d", count)
	}
}

func TestDebouncer_CoalescesBurst(t *testing.T) {

	count := runAndCount(t, 50, 300*time.Millisecond, func(in chan<- string) {
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

	count := runAndCount(t, 30, 500*time.Millisecond, func(in chan<- string) {

		for i := 0; i < 3; i++ {
			in <- "main.go"
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)

		for i := 0; i < 3; i++ {
			in <- "handler.go"
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
	})
	if count != 2 {
		t.Errorf("expected 2 triggers for 2 separate bursts, got %d", count)
	}
}

func TestDebouncer_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan string, 4)
	out := make(chan struct{}, 4)
	d := New(200)

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
