package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/platform/clock"
)

func TestBuckets(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	b := &Buckets{PerMinute: 6, Burst: 3, Now: c.Now} // one token per 10s
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow("ip"); !ok {
			t.Fatalf("burst token %d denied", i)
		}
	}
	ok, wait := b.Allow("ip")
	if ok || wait != 10*time.Second {
		t.Fatalf("over burst: %v %v", ok, wait)
	}
	if ok, _ := b.Allow("other"); !ok {
		t.Fatal("keys not independent")
	}
	c.Advance(10 * time.Second)
	if ok, _ := b.Allow("ip"); !ok {
		t.Fatal("not refilled")
	}
	c.Advance(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow("ip"); !ok {
			t.Fatal("refill exceeded burst or did not happen")
		}
	}
	if ok, _ := b.Allow("ip"); ok {
		t.Fatal("refill not capped at burst")
	}
}

func TestBucketsBoundedMemory(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	b := &Buckets{PerMinute: 60, Burst: 1, MaxKeys: 10, Now: c.Now}
	for i := 0; i < 10; i++ {
		b.Allow(fmt.Sprint(i))
	}
	if ok, _ := b.Allow("new"); ok {
		t.Fatal("new key admitted while full of active buckets")
	}
	c.Advance(time.Second) // all refilled: the sweep frees them
	if ok, _ := b.Allow("new"); !ok {
		t.Fatal("refilled buckets not swept")
	}
	if len(b.buckets) != 1 {
		t.Fatalf("%d buckets", len(b.buckets))
	}
}

func TestBucketsConcurrent(t *testing.T) {
	b := &Buckets{PerMinute: 1, Burst: 50}
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := b.Allow("k"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("allowed %d, want 50", allowed)
	}
}

func TestBackoff(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	b := &Backoff{Free: 2, Base: time.Second, Max: 8 * time.Second, Now: c.Now}
	b.Fail("alice")
	b.Fail("alice")
	if blocked, _ := b.Blocked("alice"); blocked {
		t.Fatal("blocked within free failures")
	}
	for i, want := range []time.Duration{1, 2, 4, 8, 8} {
		b.Fail("alice")
		blocked, wait := b.Blocked("alice")
		if !blocked || wait != want*time.Second {
			t.Fatalf("failure %d: %v %v, want %v", i+3, blocked, wait, want*time.Second)
		}
	}
	c.Advance(8 * time.Second)
	if blocked, _ := b.Blocked("alice"); blocked {
		t.Fatal("still blocked after the delay")
	}
	b.Succeed("alice")
	b.Fail("alice")
	if blocked, _ := b.Blocked("alice"); blocked {
		t.Fatal("success did not reset")
	}
}
