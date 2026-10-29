package engine

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// A limit set while running holds: going from unlimited to 300 KB/s, the
// limiter hands out about 300 KB a second, not the huge burst an
// "unlimited" limiter had (the library sizes a zero burst from the rate).
func TestSetLimitHoldsAfterUnlimited(t *testing.T) {
	l := rate.NewLimiter(unlimited, 0)
	setLimit(l, 0)
	// What the library does to a fresh limiter's burst (rate.go): based on
	// the rate, which is huge while unlimited. Ours is already set.
	if l.Burst() == 0 {
		t.Fatal("burst left at 0: the library would size it from the (huge) rate")
	}
	// The bucket starts empty and, unlimited, refills within a microsecond.
	if !l.AllowN(time.Now().Add(time.Millisecond), minBurst) {
		t.Error("unlimited: a 1 MiB read was refused")
	}

	setLimit(l, 300<<10)
	if l.Limit() != rate.Limit(300<<10) || l.Burst() != minBurst {
		t.Fatalf("300 KB/s: limit %v, burst %d", l.Limit(), l.Burst())
	}
	// From now on, at most the burst plus 300 KB per second gets through.
	now := time.Now()
	got := 0
	for i := 0; i < 1000 && l.AllowN(now, 64<<10); i++ {
		got += 64 << 10
	}
	if got > minBurst {
		t.Errorf("%d bytes allowed at once at 300 KB/s (burst %d)", got, minBurst)
	}
	if l.AllowN(now.Add(100*time.Millisecond), 64<<10) {
		t.Error("64 KiB allowed 0.1 s later: more than 300 KB/s")
	}

	// A high limit gets a burst of one second's worth.
	setLimit(l, 50<<20)
	if l.Burst() != 50<<20 {
		t.Errorf("50 MB/s: burst %d", l.Burst())
	}
}
