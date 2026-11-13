package relay

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// BandwidthOpts configure app-level splice rate limits. Zero Mbps means unlimited.
// Average soft-cap: when combined ingress+egress average over AvgWindow exceeds
// AvgCapMbps, both directions are limited to ThrottleMbps until the average falls.
type BandwidthOpts struct {
	IngressMbps float64
	EgressMbps  float64
	AvgWindow   time.Duration
	AvgCapMbps  float64
	ThrottleMbps float64
}

// limiter is a shared token bucket (bytes per second).
type limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes/sec; 0 = unlimited
	tokens float64
	burst  float64
	last   time.Time
}

func newLimiter(mbps float64) *limiter {
	if mbps <= 0 {
		return &limiter{}
	}
	bps := mbps * 1e6 / 8
	return &limiter{rate: bps, tokens: bps, burst: bps, last: time.Now()}
}

func (l *limiter) setRate(mbps float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if mbps <= 0 {
		l.rate = 0
		return
	}
	bps := mbps * 1e6 / 8
	l.rate = bps
	if l.burst < bps {
		l.burst = bps
	}
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

func (l *limiter) wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		if l.rate <= 0 {
			l.mu.Unlock()
			return nil
		}
		now := time.Now()
		elapsed := now.Sub(l.last).Seconds()
		l.last = now
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		need := float64(n)
		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()
			return nil
		}
		deficit := need - l.tokens
		wait := time.Duration(deficit / l.rate * float64(time.Second))
		l.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Bandwidth tracks caps, rolling average, and throttle state for metrics/TUI.
type Bandwidth struct {
	ingress, egress *limiter
	cfgIngress      float64
	cfgEgress       float64
	avgWindow       time.Duration
	avgCap          float64
	throttleMbps    float64

	mu       sync.Mutex
	samples  []bwSample
	throttled atomic.Bool
	inRate    atomic.Uint64 // recent bytes/sec estimate
	outRate   atomic.Uint64
}

type bwSample struct {
	at   time.Time
	in   uint64
	out  uint64
}

func newBandwidth(o BandwidthOpts) *Bandwidth {
	return &Bandwidth{
		ingress:      newLimiter(o.IngressMbps),
		egress:       newLimiter(o.EgressMbps),
		cfgIngress:   o.IngressMbps,
		cfgEgress:    o.EgressMbps,
		avgWindow:    o.AvgWindow,
		avgCap:       o.AvgCapMbps,
		throttleMbps: o.ThrottleMbps,
	}
}

// enabled is true when any hard or soft cap is configured.
func (b *Bandwidth) enabled() bool {
	return b != nil && (b.cfgIngress > 0 || b.cfgEgress > 0 ||
		(b.avgWindow > 0 && b.avgCap > 0 && b.throttleMbps > 0))
}

// startAvg runs the rolling-average soft-cap loop until ctx ends.
func (b *Bandwidth) startAvg(ctx context.Context) {
	if b == nil || b.avgWindow <= 0 || b.avgCap <= 0 || b.throttleMbps <= 0 {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.recomputeAvg()
		}
	}
}

func (b *Bandwidth) note(in, out uint64) {
	if b == nil || b.avgWindow <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.samples = append(b.samples, bwSample{at: now, in: in, out: out})
	cut := now.Add(-b.avgWindow)
	i := 0
	for i < len(b.samples) && b.samples[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		b.samples = append([]bwSample{}, b.samples[i:]...)
	}
}

func (b *Bandwidth) recomputeAvg() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.samples) < 2 {
		b.applyThrottle(false)
		return
	}
	first, last := b.samples[0], b.samples[len(b.samples)-1]
	sec := last.at.Sub(first.at).Seconds()
	if sec < 1 {
		return
	}
	bytes := float64((last.in - first.in) + (last.out - first.out))
	mbps := bytes * 8 / sec / 1e6
	b.applyThrottle(mbps > b.avgCap)
}

func (b *Bandwidth) applyThrottle(on bool) {
	b.throttled.Store(on)
	if on {
		b.ingress.setRate(minMbps(b.cfgIngress, b.throttleMbps))
		b.egress.setRate(minMbps(b.cfgEgress, b.throttleMbps))
		return
	}
	b.ingress.setRate(b.cfgIngress)
	b.egress.setRate(b.cfgEgress)
}

func minMbps(configured, throttle float64) float64 {
	if configured <= 0 {
		return throttle
	}
	if configured < throttle {
		return configured
	}
	return throttle
}

// Throttled reports whether the rolling-average soft cap is engaged.
func (b *Bandwidth) Throttled() bool {
	return b != nil && b.throttled.Load()
}

// Rates returns approximate recent ingress/egress bytes per second.
func (b *Bandwidth) Rates() (inBps, outBps uint64) {
	if b == nil {
		return 0, 0
	}
	return b.inRate.Load(), b.outRate.Load()
}

func (b *Bandwidth) waitIn(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	return b.ingress.wait(ctx, n)
}

func (b *Bandwidth) waitOut(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	return b.egress.wait(ctx, n)
}

// throttledConn rate-limits Read (ingress) and Write (egress) and counts bytes.
type throttledConn struct {
	net.Conn
	bw            *Bandwidth
	read, written *atomic.Uint64
	ctx           context.Context
	lastNote      time.Time
	windowIn      uint64
	windowOut     uint64
}

func (c *throttledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Add(uint64(n))
		_ = c.bw.waitIn(c.ctx, n)
		c.track(uint64(n), 0)
	}
	return n, err
}

func (c *throttledConn) Write(p []byte) (int, error) {
	if err := c.bw.waitOut(c.ctx, len(p)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.written.Add(uint64(n))
		c.track(0, uint64(n))
	}
	return n, err
}

func (c *throttledConn) track(in, out uint64) {
	c.windowIn += in
	c.windowOut += out
	now := time.Now()
	if c.lastNote.IsZero() {
		c.lastNote = now
		return
	}
	if now.Sub(c.lastNote) < time.Second {
		return
	}
	sec := now.Sub(c.lastNote).Seconds()
	if sec > 0 {
		c.bw.inRate.Store(uint64(float64(c.windowIn) / sec))
		c.bw.outRate.Store(uint64(float64(c.windowOut) / sec))
	}
	totalIn := c.read.Load()
	totalOut := c.written.Load()
	c.bw.note(totalIn, totalOut)
	c.windowIn, c.windowOut = 0, 0
	c.lastNote = now
}

var _ io.ReadWriter = (*throttledConn)(nil)
