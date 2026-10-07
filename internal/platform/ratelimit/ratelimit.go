// Package ratelimit provides in-memory keyed limiters for a single instance
// (spec §6.1, §7.1): token buckets per key and exponential backoff after
// repeated failures.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// DefaultMaxKeys bounds the memory of a limiter (roughly 100 bytes per key).
const DefaultMaxKeys = 100_000

// Buckets is a token bucket per key: Burst tokens, refilled at PerMinute.
// It is safe for concurrent use.
type Buckets struct {
	PerMinute int
	Burst     int
	// MaxKeys bounds the number of tracked keys; 0 means DefaultMaxKeys.
	// When full even after dropping refilled buckets, new keys are denied:
	// failing closed is the safe choice for login throttling.
	MaxKeys int
	Now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *Buckets) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Buckets) perSecond() float64 { return float64(b.PerMinute) / 60 }

// Allow takes one token for key. If none is left it reports how long until
// the next token.
func (b *Buckets) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buckets == nil {
		b.buckets = map[string]*bucket{}
	}
	bk, found := b.buckets[key]
	if !found {
		max := b.MaxKeys
		if max == 0 {
			max = DefaultMaxKeys
		}
		if len(b.buckets) >= max {
			b.sweep(now)
		}
		if len(b.buckets) >= max {
			return false, time.Minute
		}
		bk = &bucket{tokens: float64(b.Burst), at: now}
		b.buckets[key] = bk
	}
	bk.tokens = math.Min(float64(b.Burst), bk.tokens+now.Sub(bk.at).Seconds()*b.perSecond())
	bk.at = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true, 0
	}
	wait := time.Duration((1 - bk.tokens) / b.perSecond() * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled completely; they carry no state.
func (b *Buckets) sweep(now time.Time) {
	for k, bk := range b.buckets {
		if bk.tokens+now.Sub(bk.at).Seconds()*b.perSecond() >= float64(b.Burst) {
			delete(b.buckets, k)
		}
	}
}

// Backoff blocks a key for an exponentially growing time after repeated
// failures: after Free failures, Base, then 2×Base, 4×Base, … up to Max. A
// success resets the key. It is safe for concurrent use.
type Backoff struct {
	Free int
	Base time.Duration
	Max  time.Duration
	// MaxKeys bounds memory like Buckets.MaxKeys. When full, failures of new
	// keys are not tracked; the token buckets still limit them.
	MaxKeys int
	Now     func() time.Time

	mu    sync.Mutex
	fails map[string]*failures
}

type failures struct {
	n    int
	last time.Time
}

func (b *Backoff) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Backoff) delay(n int) time.Duration {
	if n <= b.Free {
		return 0
	}
	d := b.Base << min(n-b.Free-1, 30)
	if d > b.Max || d <= 0 {
		d = b.Max
	}
	return d
}

// Blocked reports whether key must wait, and for how long.
func (b *Backoff) Blocked(key string) (bool, time.Duration) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.fails[key]
	if !ok {
		return false, 0
	}
	if until := f.last.Add(b.delay(f.n)); now.Before(until) {
		return true, until.Sub(now)
	}
	return false, 0
}

// Fail records a failure for key.
func (b *Backoff) Fail(key string) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fails == nil {
		b.fails = map[string]*failures{}
	}
	f, ok := b.fails[key]
	if !ok {
		max := b.MaxKeys
		if max == 0 {
			max = DefaultMaxKeys
		}
		if len(b.fails) >= max {
			// Forget keys whose block expired long ago (one Max period).
			for k, v := range b.fails {
				if now.Sub(v.last) > b.Max {
					delete(b.fails, k)
				}
			}
			if len(b.fails) >= max {
				return
			}
		}
		f = &failures{}
		b.fails[key] = f
	}
	f.n++
	f.last = now
}

// Succeed clears key.
func (b *Backoff) Succeed(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.fails, key)
}
