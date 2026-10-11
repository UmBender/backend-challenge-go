package app

import (
	"math/rand/v2"
	"time"
)

// Backoff returns the delay before retry number attempt (1-based):
// exponential, capped, with equal jitter (random in [step/2, step]). Unlike
// full jitter it keeps a minimum wait, so a missing reference never causes a
// hot loop, while still spreading instances apart. Doubling stops at cap, so
// large attempts cannot overflow.
func Backoff(attempt int, base, cap time.Duration) time.Duration {
	if base <= 0 {
		base = time.Millisecond
	}
	if cap < base {
		cap = base
	}
	step := base
	for i := 1; i < attempt && step < cap; i++ {
		step *= 2
	}
	if step > cap {
		step = cap
	}
	half := step / 2
	return half + rand.N(step-half+1)
}
