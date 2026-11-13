package relay

import (
	"context"
	"testing"
	"time"
)

func TestLimiterWait(t *testing.T) {
	l := newLimiter(0.008) // 0.008 Mbps = 1000 bytes/sec; burst starts full
	ctx := context.Background()
	if err := l.wait(ctx, 1000); err != nil { // drain burst
		t.Fatal(err)
	}
	start := time.Now()
	if err := l.wait(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatalf("expected rate limiting, elapsed %v", time.Since(start))
	}
}

func TestLimiterUnlimited(t *testing.T) {
	l := newLimiter(0)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := l.wait(ctx, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("unlimited limiter should not block")
	}
}

func TestAvgSoftCap(t *testing.T) {
	b := newBandwidth(BandwidthOpts{
		IngressMbps: 1000, EgressMbps: 1000,
		AvgWindow: time.Hour, AvgCapMbps: 1, ThrottleMbps: 0.1,
	})
	now := time.Now()
	b.mu.Lock()
	b.samples = []bwSample{
		{at: now.Add(-time.Minute), in: 0, out: 0},
		{at: now, in: 10_000_000, out: 10_000_000}, // ~2.67 Mbps over 60s
	}
	b.mu.Unlock()
	b.recomputeAvg()
	if !b.Throttled() {
		t.Fatal("expected soft cap engaged")
	}
}
