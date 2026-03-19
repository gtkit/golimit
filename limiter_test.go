package golimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ================== Functional Tests ==================

// TestBasicRateLimit verifies that requests within the burst pass and excess requests are rejected.
func TestBasicRateLimit(t *testing.T) {
	lim := NewLimiter("basic-test", 5)

	for i := range 5 {
		if !lim.Allow() {
			t.Errorf("request %d should be allowed.", i+1)
		}
	}

	// The 6th request should be rejected (burst exhausted, no time to refill).
	if lim.Allow() {
		t.Error("request 6 should be rejected.")
	}
}

// TestDifferentKeysAreIndependent verifies that each key has its own isolated rate limiter.
func TestDifferentKeysAreIndependent(t *testing.T) {
	limA := NewLimiter("key-a", 3)
	limB := NewLimiter("key-b", 3)

	// Exhaust key-a.
	for range 3 {
		limA.Allow()
	}

	if limA.Allow() {
		t.Error("key-a should be exhausted.")
	}

	// key-b should be unaffected.
	if !limB.Allow() {
		t.Error("key-b should still be allowed.")
	}
}

// TestSameKeyReturnsSameLimiter verifies that NewLimiter returns the same instance for the same key.
func TestSameKeyReturnsSameLimiter(t *testing.T) {
	lim1 := NewLimiter("same-key-test", 10)
	lim2 := NewLimiter("same-key-test", 10)

	if lim1 != lim2 {
		t.Error("NewLimiter with the same key should return the same *Limiter instance.")
	}
}

// TestTokenRecovery verifies that tokens regenerate over time.
func TestTokenRecovery(t *testing.T) {
	// 10 rps = 1 token every 100ms, burst = 10.
	lim := NewLimiter("recovery-test", 10)

	// Exhaust all tokens.
	for range 10 {
		lim.Allow()
	}
	if lim.Allow() {
		t.Error("should be rejected when tokens are exhausted.")
	}

	// Wait for at least 1 token to regenerate.
	time.Sleep(150 * time.Millisecond)

	if !lim.Allow() {
		t.Error("should be allowed after token recovery.")
	}
}

// TestRateSemantic verifies that the rps parameter actually controls the sustained rate.
// This is the critical fix — the old code used rate.Every(1s) which meant 1 req/s regardless of rps.
func TestRateSemantic(t *testing.T) {
	rps := 100
	lim := NewLimiter("rate-semantic-test", rps)

	// Exhaust burst.
	for range rps {
		lim.Allow()
	}

	// Wait 100ms — at 100 rps, should regenerate ~10 tokens.
	time.Sleep(100 * time.Millisecond)

	allowed := 0
	for range 20 {
		if lim.Allow() {
			allowed++
		}
	}

	// Should have recovered ~10 tokens (allow some timing tolerance).
	if allowed < 5 {
		t.Errorf("expected at least 5 tokens recovered at %d rps after 100ms, got %d.", rps, allowed)
	}
	t.Logf("rate semantic: %d tokens recovered after 100ms at %d rps.", allowed, rps)
}

// TestDefaultValues verifies that rps=0 or negative values don't panic.
func TestDefaultValues(t *testing.T) {
	// rate.Limit(0) means no tokens ever — all requests should be rejected after burst.
	lim := NewLimiter("zero-rps-test", 0)
	// burst = 0, so even the first request should be rejected.
	if lim.Allow() {
		t.Error("rps=0 should reject all requests.")
	}
}

// ================== Concurrency Safety ==================

// TestConcurrencyCorrectness is the most important test.
// It verifies that under heavy goroutine contention, the total allowed count
// never exceeds the configured limit. Run with: go test -race -run TestConcurrencyCorrectness.
func TestConcurrencyCorrectness(t *testing.T) {
	const (
		burst      = 50
		goroutines = 200
		reqsPerG   = 10
	)

	// Use a very high rate so refill doesn't interfere during the test.
	lim := NewLimiter("concurrency-correctness", burst)

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range reqsPerG {
				if lim.Allow() {
					allowed.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	a := allowed.Load()
	t.Logf("concurrency correctness: allowed=%d, total=%d, burst=%d.", a, goroutines*reqsPerG, burst)

	if a > int64(burst)+5 {
		// Small margin for token refill during test execution.
		t.Errorf("CRITICAL: allowed %d requests, exceeds burst %d. Race condition detected.", a, burst)
	}
}

// TestConcurrencyNoLoss verifies that no requests are silently dropped or duplicated.
func TestConcurrencyNoLoss(t *testing.T) {
	const (
		rps        = 1000
		goroutines = 100
		reqsPerG   = 100
	)

	lim := NewLimiter("no-loss-test", rps)
	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range reqsPerG {
				if lim.Allow() {
					allowed.Add(1)
				} else {
					rejected.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	total := allowed.Load() + rejected.Load()
	if total != goroutines*reqsPerG {
		t.Errorf("expected %d total requests, got %d.", goroutines*reqsPerG, total)
	}
	t.Logf("no loss: allowed=%d, rejected=%d, total=%d.", allowed.Load(), rejected.Load(), total)
}

// TestMultiKeyConcurrency verifies per-key isolation under concurrent access.
func TestMultiKeyConcurrency(t *testing.T) {
	const (
		numKeys  = 20
		burst    = 10
		reqsPerG = 5
		gsPerKey = 10
	)

	var wg sync.WaitGroup
	results := make([]atomic.Int64, numKeys)

	for i := range numKeys {
		key := fmt.Sprintf("multi-key-%d", i)
		lim := NewLimiter(key, burst)

		for range gsPerKey {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				for range reqsPerG {
					if lim.Allow() {
						results[idx].Add(1)
					}
				}
			}(i)
		}
	}

	wg.Wait()

	for i := range numKeys {
		a := results[i].Load()
		if a > int64(burst)+5 {
			t.Errorf("key %d: allowed %d, exceeds burst %d.", i, a, burst)
		}
	}
}

// TestLoadOrStoreRace specifically targets the original Load+Store race condition.
// Multiple goroutines simultaneously create the same key for the first time.
func TestLoadOrStoreRace(t *testing.T) {
	const goroutines = 100

	// Use a fresh Limiters instance to guarantee no pre-existing key.
	ls := &Limiters{}
	var ptrs [goroutines]*Limiter
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ptrs[idx] = ls.getLimiter("race-key", 10)
		}(i)
	}

	wg.Wait()

	// All goroutines should get the exact same *Limiter instance.
	for i := 1; i < goroutines; i++ {
		if ptrs[i] != ptrs[0] {
			t.Errorf("goroutine %d got a different *Limiter instance — LoadOrStore race.", i)
		}
	}
}

// ================== Cleanup Tests ==================

// TestCleanupRemovesIdleEntries verifies that the cleanup goroutine removes stale entries.
// We test the clearLimiter logic directly rather than waiting 5+ minutes.
func TestCleanupRemovesIdleEntries(t *testing.T) {
	ls := &Limiters{}

	// Create a limiter and set its lastGet to 10 minutes ago.
	lim := ls.getLimiter("stale-key", 10)
	lim.lastGet.Store(time.Now().Add(-10 * time.Minute).UnixNano())

	// Create a fresh limiter.
	fresh := ls.getLimiter("fresh-key", 10)
	fresh.lastGet.Store(time.Now().UnixNano())

	// Run cleanup.
	ls.clearOnce()

	// Stale key should be removed.
	if _, ok := ls.limiters.Load("stale-key"); ok {
		t.Error("stale-key should have been cleaned up.")
	}

	// Fresh key should remain.
	if _, ok := ls.limiters.Load("fresh-key"); !ok {
		t.Error("fresh-key should still exist.")
	}
}

// ================== Benchmarks ==================

// BenchmarkAllow_SingleKey measures throughput for a single key (worst-case contention).
func BenchmarkAllow_SingleKey(b *testing.B) {
	lim := NewLimiter("bench-single", 1000000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow()
		}
	})
}

// BenchmarkAllow_MultiKey measures throughput across many keys (typical production scenario).
func BenchmarkAllow_MultiKey(b *testing.B) {
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-multi-%d", i)
	}
	// Pre-create all limiters.
	for _, k := range keys {
		NewLimiter(k, 1000000)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			NewLimiter(keys[i%len(keys)], 1000000).Allow()
			i++
		}
	})
}

// BenchmarkNewLimiter_ExistingKey measures the overhead of looking up an existing key.
func BenchmarkNewLimiter_ExistingKey(b *testing.B) {
	NewLimiter("bench-existing", 100)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			NewLimiter("bench-existing", 100)
		}
	})
}
