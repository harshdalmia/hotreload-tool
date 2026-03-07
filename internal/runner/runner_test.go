package runner

import (
	"testing"
	"time"
)

func TestCrashLoopDetection_NotLooping(t *testing.T) {
	r := New("", "")

	r.mu.Lock()
	r.crashTimes = []time.Time{time.Now()}
	r.mu.Unlock()

	if r.isCrashLooping() {
		t.Error("expected not crash-looping with only 1 crash")
	}
}

func TestCrashLoopDetection_LoopingWhenRapid(t *testing.T) {
	r := New("", "")
	now := time.Now()
	r.mu.Lock()
	r.crashTimes = []time.Time{
		now.Add(-1 * time.Second),
		now.Add(-500 * time.Millisecond),
		now,
	}
	r.mu.Unlock()

	if !r.isCrashLooping() {
		t.Error("expected crash-looping with 3 rapid crashes")
	}
}

func TestCrashLoopDetection_NotLoopingWhenOld(t *testing.T) {
	r := New("", "")
	now := time.Now()
	r.mu.Lock()
	r.crashTimes = []time.Time{
		now.Add(-30 * time.Second),
		now.Add(-1 * time.Second),
		now,
	}
	r.mu.Unlock()

	if r.isCrashLooping() {
		t.Error("expected not crash-looping when some crashes are old")
	}
}

func TestRecordCrash_TrimsOldEntries(t *testing.T) {
	r := New("", "")
	for i := 0; i < 10; i++ {
		r.recordCrash()
	}
	r.mu.Lock()
	count := len(r.crashTimes)
	r.mu.Unlock()
	if count > crashThreshold+1 {
		t.Errorf("crash times should be trimmed to %d, got %d", crashThreshold+1, count)
	}
}
