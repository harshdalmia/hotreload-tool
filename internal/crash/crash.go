// Package crash detects server crash loops using a sliding time window.
//
// A single crash is normal: you saved a file with a nil-pointer bug, the
// server died, you will fix it. A crash *loop* is different — restarting
// immediately and repeatedly floods the terminal and burns CPU without
// giving the developer a chance to read the stack trace. The detector
// answers one question: "have there been enough crashes recently that I
// should slow down?"
//
// The window is sliding rather than a consecutive-failure counter, so a
// server that limps along for a while between crashes eventually clears
// itself without needing an explicit success signal.
package crash

import (
	"sync"
	"time"
)

// Defaults chosen so that a genuinely broken server backs off quickly while
// an occasional crash during normal development never triggers a delay.
const (
	DefaultWindow    = 10 * time.Second
	DefaultThreshold = 3
	DefaultBackoff   = 3 * time.Second
)

// Detector tracks recent crash timestamps. It is safe for concurrent use.
type Detector struct {
	window    time.Duration
	threshold int
	backoff   time.Duration

	mu      sync.Mutex
	crashes []time.Time
	now     func() time.Time // injectable for tests
}

// New returns a Detector that reports a loop once threshold crashes have
// been recorded within window.
func New(window time.Duration, threshold int, backoff time.Duration) *Detector {
	if window <= 0 {
		window = DefaultWindow
	}
	if threshold < 1 {
		threshold = DefaultThreshold
	}
	if backoff < 0 {
		backoff = DefaultBackoff
	}
	return &Detector{
		window:    window,
		threshold: threshold,
		backoff:   backoff,
		now:       time.Now,
	}
}

// NewDefault returns a Detector using the package defaults.
func NewDefault() *Detector {
	return New(DefaultWindow, DefaultThreshold, DefaultBackoff)
}

// Record notes that the server just crashed.
func (d *Detector) Record() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.crashes = append(d.crashes, d.now())
	d.pruneLocked()
}

// Count returns how many crashes fall inside the current window.
func (d *Detector) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
	return len(d.crashes)
}

// IsLooping reports whether the crash count within the window has reached
// the threshold.
func (d *Detector) IsLooping() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
	return len(d.crashes) >= d.threshold
}

// Backoff returns how long to wait before the next restart attempt: the
// configured backoff while looping, otherwise zero.
func (d *Detector) Backoff() time.Duration {
	if d.IsLooping() {
		return d.backoff
	}
	return 0
}

// Reset clears all recorded crashes. Called when a fresh build succeeds,
// since new code invalidates the crash history of the old binary.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.crashes = nil
}

// pruneLocked drops timestamps that have aged out of the window.
// Callers must hold d.mu.
func (d *Detector) pruneLocked() {
	if len(d.crashes) == 0 {
		return
	}
	cutoff := d.now().Add(-d.window)
	keep := d.crashes[:0]
	for _, t := range d.crashes {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	d.crashes = keep
}
