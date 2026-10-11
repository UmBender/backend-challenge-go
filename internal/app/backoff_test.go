package app

import (
	"testing"
	"time"
)

// TestBackoffStaysWithinItsWindow: attempt n waits between half and all of
// min(cap, base*2^(n-1)), never zero, never more than cap.
func TestBackoffStaysWithinItsWindow(t *testing.T) {
	base, cap := time.Second, 2*time.Minute
	for attempt := 1; attempt <= 40; attempt++ {
		step := base << (attempt - 1)
		if attempt > 8 || step > cap {
			step = cap
		}
		for i := 0; i < 200; i++ {
			d := Backoff(attempt, base, cap)
			if d < step/2 || d > step {
				t.Fatalf("attempt %d: %v outside [%v, %v]", attempt, d, step/2, step)
			}
		}
	}
}

func TestBackoffIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[Backoff(5, time.Second, time.Minute)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct delays in 50 draws: no jitter", len(seen))
	}
}

func TestBackoffHandlesNonsense(t *testing.T) {
	if d := Backoff(0, time.Second, time.Minute); d < 500*time.Millisecond || d > time.Second {
		t.Fatalf("attempt 0 = %v, want the first step", d)
	}
	if d := Backoff(3, 0, time.Minute); d <= 0 {
		t.Fatalf("zero base must still give a positive delay, got %v", d)
	}
}
