package crash

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets the sliding-window tests run instantly and deterministically
// instead of sleeping through real durations.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestDetector wires a Detector to a fake clock.
func newTestDetector(window time.Duration, threshold int, backoff time.Duration) (*Detector, *fakeClock) {
	clk := newFakeClock()
	d := New(window, threshold, backoff)
	d.now = clk.Now
	return d, clk
}

func TestDetector_NoCrashesIsNotLooping(t *testing.T) {
	d, _ := newTestDetector(10*time.Second, 3, time.Second)
	if d.IsLooping() {
		t.Error("a detector with no recorded crashes must not report a loop")
	}
	if got := d.Backoff(); got != 0 {
		t.Errorf("Backoff() = %v, want 0", got)
	}
}

func TestDetector_BelowThresholdIsNotLooping(t *testing.T) {
	d, _ := newTestDetector(10*time.Second, 3, time.Second)
	d.Record()
	d.Record()
	if d.IsLooping() {
		t.Errorf("2 crashes with threshold 3 must not report a loop (count=%d)", d.Count())
	}
}

func TestDetector_ReachingThresholdReportsLoop(t *testing.T) {
	d, _ := newTestDetector(10*time.Second, 3, 2*time.Second)
	d.Record()
	d.Record()
	d.Record()
	if !d.IsLooping() {
		t.Errorf("3 crashes with threshold 3 must report a loop (count=%d)", d.Count())
	}
	if got := d.Backoff(); got != 2*time.Second {
		t.Errorf("Backoff() = %v, want 2s", got)
	}
}

// TestDetector_CrashesOutsideWindowAgeOut is the core of the sliding-window
// behaviour: three crashes spread far enough apart are three isolated
// incidents, not a loop.
func TestDetector_CrashesOutsideWindowAgeOut(t *testing.T) {
	d, clk := newTestDetector(5*time.Second, 3, time.Second)

	d.Record()
	clk.Advance(3 * time.Second)
	d.Record()
	clk.Advance(3 * time.Second) // first crash is now 6s old, outside the 5s window
	d.Record()

	if got := d.Count(); got != 2 {
		t.Errorf("Count() = %d, want 2 (oldest crash should have aged out)", got)
	}
	if d.IsLooping() {
		t.Error("crashes spread beyond the window must not report a loop")
	}
}

func TestDetector_LoopClearsAfterWindowPasses(t *testing.T) {
	d, clk := newTestDetector(5*time.Second, 3, time.Second)
	d.Record()
	d.Record()
	d.Record()
	if !d.IsLooping() {
		t.Fatal("expected a loop after 3 rapid crashes")
	}

	clk.Advance(6 * time.Second) // every crash is now outside the window

	if d.IsLooping() {
		t.Errorf("loop should clear once all crashes age out (count=%d)", d.Count())
	}
	if got := d.Count(); got != 0 {
		t.Errorf("Count() = %d, want 0", got)
	}
}

func TestDetector_ResetClearsHistory(t *testing.T) {
	d, _ := newTestDetector(10*time.Second, 3, time.Second)
	d.Record()
	d.Record()
	d.Record()
	if !d.IsLooping() {
		t.Fatal("expected a loop before Reset")
	}

	d.Reset()

	if d.IsLooping() {
		t.Error("IsLooping() must be false after Reset")
	}
	if got := d.Count(); got != 0 {
		t.Errorf("Count() = %d after Reset, want 0", got)
	}
}

func TestDetector_NewClampsInvalidValues(t *testing.T) {
	d := New(0, 0, -1)
	if d.window != DefaultWindow {
		t.Errorf("window = %v, want default %v", d.window, DefaultWindow)
	}
	if d.threshold != DefaultThreshold {
		t.Errorf("threshold = %d, want default %d", d.threshold, DefaultThreshold)
	}
	if d.backoff != DefaultBackoff {
		t.Errorf("backoff = %v, want default %v", d.backoff, DefaultBackoff)
	}
}

// TestDetector_ConcurrentUse guards the mutex: the controller records crashes
// from the main loop while other goroutines may query state.
func TestDetector_ConcurrentUse(t *testing.T) {
	d := NewDefault()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			d.Record()
		}()
		go func() {
			defer wg.Done()
			_ = d.IsLooping()
			_ = d.Count()
			_ = d.Backoff()
		}()
	}
	wg.Wait()
}
