package golimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ================== 核心 Limiter 测试 ==================

func TestAllow_Basic(t *testing.T) {
	l := New(5)
	defer l.Close()

	for i := range 5 {
		if !l.Allow("k") {
			t.Errorf("request %d should be allowed.", i+1)
		}
	}
	if l.Allow("k") {
		t.Error("request 6 should be rejected.")
	}
}

func TestAllow_IndependentKeys(t *testing.T) {
	l := New(3)
	defer l.Close()

	for range 3 {
		l.Allow("a")
	}
	if l.Allow("a") {
		t.Error("key a should be exhausted.")
	}
	if !l.Allow("b") {
		t.Error("key b should be independent and allowed.")
	}
}

func TestAllowN(t *testing.T) {
	l := New(10)
	defer l.Close()

	if !l.AllowN("k", 5) {
		t.Error("AllowN(5) should succeed with 10 burst.")
	}
	if l.AllowN("k", 6) {
		t.Error("AllowN(6) should fail with only 5 tokens left.")
	}
}

func TestTokenRecovery(t *testing.T) {
	l := New(10, WithBurst(1))
	defer l.Close()

	if !l.Allow("k") {
		t.Fatal("first request should pass.")
	}
	if l.Allow("k") {
		t.Fatal("second request should be rejected.")
	}

	// 10 rps, burst=1 → 1 token recovered in 100ms.
	time.Sleep(150 * time.Millisecond)

	if !l.Allow("k") {
		t.Error("should be allowed after token recovery.")
	}
}

func TestRateSemantic(t *testing.T) {
	l := New(100) // 100 token/秒.
	defer l.Close()

	// 用尽 burst.
	for range 100 {
		l.Allow("k")
	}
	// 等 100ms → 应补充 ~10 个令牌.
	time.Sleep(100 * time.Millisecond)

	allowed := 0
	for range 20 {
		if l.Allow("k") {
			allowed++
		}
	}
	if allowed < 5 {
		t.Errorf("expected >= 5 tokens recovered at 100 rps after 100ms, got %d.", allowed)
	}
	t.Logf("rate semantic: %d tokens recovered after 100ms.", allowed)
}

func TestWithBurst(t *testing.T) {
	l := New(10, WithBurst(20))
	defer l.Close()

	allowed := 0
	for range 25 {
		if l.Allow("k") {
			allowed++
		}
	}
	if allowed != 20 {
		t.Errorf("expected 20 allowed (burst=20), got %d.", allowed)
	}
}

func TestReset(t *testing.T) {
	l := New(5)
	defer l.Close()

	for range 5 {
		l.Allow("k")
	}
	if l.Allow("k") {
		t.Error("should be rejected before reset.")
	}

	l.Reset("k")

	if !l.Allow("k") {
		t.Error("should be allowed after reset.")
	}
}

func TestTokens(t *testing.T) {
	l := New(10)
	defer l.Close()

	// 未见过的 key 应返回满 burst.
	if tok := l.Tokens("new"); tok != 10 {
		t.Errorf("unseen key should have burst tokens, got %f.", tok)
	}

	l.Allow("k")
	tok := l.Tokens("k")
	if tok < 8 || tok > 10 {
		t.Errorf("after 1 Allow, tokens should be ~9, got %f.", tok)
	}
}

func TestClose(t *testing.T) {
	l := New(100)
	l.Allow("k")
	l.Close()
	// Close 后清理 goroutine 应已退出.
	// 这里仅验证不 panic 不死锁.
}

func TestZeroRate(t *testing.T) {
	l := New(0) // 应使用默认 rate=100.
	defer l.Close()

	if !l.Allow("k") {
		t.Error("rps=0 应回退到默认 100,首次请求应通过.")
	}
}

// TestNewVisitorNotImmediatelyCleaned 是关键回归测试:新建 visitor 不应被立即清理.
//
// 隐患场景(v2.0.1 及之前):新建 visitor 时未 Store lastSeen → 默认 0 →
// cleanup goroutine 抢先看到 lastSeen=0 < threshold → 立即删除 → 后续 Allow
// 重新创建,burst 重置 → 绕过限流.
//
// 修复后:getOrCreate 创建时立即 Store lastSeen=Now,cleanup 不再误删新 visitor.
func TestNewVisitorNotImmediatelyCleaned(t *testing.T) {
	// 极激进配置:每 5ms 清理一次,空闲超 5ms 即过期.
	l := New(100,
		WithCleanupInterval(5*time.Millisecond),
		WithMaxIdleTime(5*time.Millisecond),
	)
	defer l.Close()

	// 创建 visitor 后立即查 Size — 应该 = 1,不应该被 cleanup 抢先删除.
	l.Allow("k")
	if got := l.Size(); got != 1 {
		t.Fatalf("新建 visitor 后 Size 应为 1,实际 %d(可能被 cleanup 立即删除).", got)
	}

	// 紧密自旋:在多个 cleanup 周期内反复创建新 key,确保每个新 key 都不被即删.
	for i := range 100 {
		key := fmt.Sprintf("k%d", i)
		l.Allow(key)
		// 用 Tokens 间接验证 visitor 仍在(若被删除,会返回满 burst=100;若仍在,小于满 burst).
		if tok := l.Tokens(key); tok >= float64(l.Burst()) {
			t.Errorf("新建 visitor %s 在 Allow 后应已消耗 1 个 token,实际 Tokens=%v 等于 burst,可能被即删.", key, tok)
		}
	}
}

// TestNewVisitorNotImmediatelyCleaned_Concurrent 并发版本:多 goroutine 同时创建
// + 短 cleanup 周期,确保 visitor 不会被 cleanup 与新建之间的 race 误删.
func TestNewVisitorNotImmediatelyCleaned_Concurrent(t *testing.T) {
	l := New(100,
		WithCleanupInterval(1*time.Millisecond),
		WithMaxIdleTime(1*time.Millisecond),
	)
	defer l.Close()

	const goroutines = 100
	var wg sync.WaitGroup
	var brokenCount atomic.Int64

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-k%d", idx)
			l.Allow(key)
			// visitor 应至少存在到 cleanup 下一周期前 — 这里立即查,期望未被即删.
			if tok := l.Tokens(key); tok >= float64(l.Burst()) {
				brokenCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := brokenCount.Load(); got > 0 {
		t.Errorf("%d / %d 个新建 visitor 被 cleanup 立即误删(Tokens 返回满 burst).", got, goroutines)
	}
}

// TestWithCleanupInterval_ZeroSafe 验证 WithCleanupInterval(0) 不会让 New 内部
// time.NewTicker(0) panic — 应被规范化为默认 1 分钟.
func TestWithCleanupInterval_ZeroSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithCleanupInterval(0) 不应导致 panic,实际 panic:%v.", r)
		}
	}()
	l := New(10, WithCleanupInterval(0))
	defer l.Close()

	// 正常 Allow 应工作.
	if !l.Allow("k") {
		t.Error("修复 cleanupInterval=0 后基本功能应正常.")
	}
}

// TestWithCleanupInterval_NegativeSafe 验证负数 cleanupInterval 也安全.
func TestWithCleanupInterval_NegativeSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithCleanupInterval(<0) 不应 panic,实际:%v.", r)
		}
	}()
	l := New(10, WithCleanupInterval(-1*time.Second))
	defer l.Close()
	l.Allow("k")
}

// TestWithMaxIdleTime_ZeroSafe 验证 WithMaxIdleTime(0) 不让 Limiter 进入异常状态.
// 0 会让所有 visitor 在每次 cleanup 时被删除 — 这是用户预期吗?
// 修复策略:规范化为默认值,避免用户误配.
func TestWithMaxIdleTime_ZeroSafe(t *testing.T) {
	l := New(10, WithMaxIdleTime(0), WithCleanupInterval(50*time.Millisecond))
	defer l.Close()

	l.Allow("k")
	// 等待 1 个 cleanup 周期 + 余量.
	time.Sleep(150 * time.Millisecond)

	// 因为 maxIdleTime 被规范化为 5 分钟,visitor 不应被清理.
	if got := l.Size(); got == 0 {
		t.Error("WithMaxIdleTime(0) 应被规范化为默认 5 分钟,visitor 不应被清理.")
	}
}

// TestWithMaxKeys_NegativeSafe 验证负数 maxKeys 被规范化为 0(无上限),
// 而不是静默等价于"配了防护但永远不生效".
func TestWithMaxKeys_NegativeSafe(t *testing.T) {
	l := New(10, WithMaxKeys(-100))
	defer l.Close()

	// 负数被规范化为 0(无上限)— 应可以无限创建 key.
	for i := range 50 {
		if !l.Allow(fmt.Sprintf("k%d", i)) {
			t.Errorf("WithMaxKeys(-100) 应等价于无上限,key %d 不应被拒.", i)
		}
	}
}

// TestFractionalRate 验证 rps < 1 时 burst 至少为 1,不再因 int(rps)=0 全拒.
func TestFractionalRate(t *testing.T) {
	l := New(0.5)
	defer l.Close()

	if l.Burst() < 1 {
		t.Fatalf("burst should be >= 1 for fractional rps, got %d.", l.Burst())
	}
	if !l.Allow("k") {
		t.Error("first request should be allowed (burst >= 1 with fractional rps).")
	}
}

// TestAllowN_NonPositive 验证 n <= 0 时 AllowN 直接返回 true.
func TestAllowN_NonPositive(t *testing.T) {
	l := New(10, WithBurst(1))
	defer l.Close()

	if !l.AllowN("k", 0) {
		t.Error("AllowN(_, 0) should always return true.")
	}
	if !l.AllowN("k", -5) {
		t.Error("AllowN(_, -5) should always return true.")
	}
}

// TestWithMaxKeys 验证键基数上限达到后,新 key 被拒.
func TestWithMaxKeys(t *testing.T) {
	l := New(1000, WithMaxKeys(2))
	defer l.Close()

	if !l.Allow("a") {
		t.Fatal("key a should be allowed (1st new key).")
	}
	if !l.Allow("b") {
		t.Fatal("key b should be allowed (2nd new key).")
	}
	if l.Allow("c") {
		t.Error("key c should be rejected (over WithMaxKeys=2).")
	}
	// 已存在的 key 仍可访问.
	if !l.Allow("a") {
		t.Error("existing key a should still be allowed after maxKeys reached.")
	}
	if got := l.Size(); got != 2 {
		t.Errorf("Size() should be 2, got %d.", got)
	}
}

// TestWithOnReject 验证拒绝回调按 reason 正确触发.
func TestWithOnReject(t *testing.T) {
	var rateRejects, maxKeyRejects atomic.Int64
	l := New(1, WithBurst(1), WithMaxKeys(1), WithOnReject(func(key string, reason RejectReason) {
		switch reason {
		case RejectRate:
			rateRejects.Add(1)
		case RejectMaxKeys:
			maxKeyRejects.Add(1)
		}
	}))
	defer l.Close()

	// 第 1 个 key 占满 burst.
	l.Allow("a")
	l.Allow("a") // 触发 rate reject(令牌不足).
	l.Allow("b") // 触发 max_keys reject(已达上限).

	if rateRejects.Load() != 1 {
		t.Errorf("expected 1 rate reject, got %d.", rateRejects.Load())
	}
	if maxKeyRejects.Load() != 1 {
		t.Errorf("expected 1 max_keys reject, got %d.", maxKeyRejects.Load())
	}
}

// TestSize 验证 Size 在 Allow / Reset / cleanup 之后正确更新.
func TestSize(t *testing.T) {
	l := New(100, WithCleanupInterval(50*time.Millisecond), WithMaxIdleTime(100*time.Millisecond))
	defer l.Close()

	l.Allow("a")
	l.Allow("b")
	l.Allow("c")
	if got := l.Size(); got != 3 {
		t.Errorf("Size() after 3 Allow should be 3, got %d.", got)
	}

	l.Reset("a")
	if got := l.Size(); got != 2 {
		t.Errorf("Size() after Reset should be 2, got %d.", got)
	}

	// 等待 idle + cleanup 触发.
	time.Sleep(250 * time.Millisecond)
	if got := l.Size(); got != 0 {
		t.Errorf("Size() after cleanup should be 0, got %d.", got)
	}
}

// TestCheck_RetryAfterFractional 验证 rate<1 时 Check 返回的 RetryAfter 按 1/rate 计算.
// 这是给上层中间件写 Retry-After 响应头用的 — 之前在 middleware 层耦合测试,
// v2 拆 Check 接口后,核心库直接测 Result.RetryAfter,框架无关.
func TestCheck_RetryAfterFractional(t *testing.T) {
	l := New(0.1) // rate=0.1/s → RetryAfter 应为 10s.
	defer l.Close()

	l.Allow("k") // 用尽桶.
	r := l.Check("k")

	if r.Allowed {
		t.Fatal("用尽后 Check 应返回 Allowed=false.")
	}
	if r.RetryAfter != 10*time.Second {
		t.Errorf("rate=0.1 时 RetryAfter 应为 10s,实际 %v.", r.RetryAfter)
	}
	if r.Reason != RejectRate {
		t.Errorf("拒绝原因应为 RejectRate,实际 %v.", r.Reason)
	}
}

// TestCheck_Allowed 验证 Check 在允许路径返回的字段都被填充.
func TestCheck_Allowed(t *testing.T) {
	l := New(100, WithBurst(50))
	defer l.Close()

	r := l.Check("k")
	if !r.Allowed {
		t.Fatal("首次请求应允许.")
	}
	if r.Limit != 50 {
		t.Errorf("Limit 应为 burst=50,实际 %d.", r.Limit)
	}
	if r.Remaining < 48 || r.Remaining > 50 {
		t.Errorf("Remaining 应在 [48,50] 内(刚消耗 1 个),实际 %v.", r.Remaining)
	}
	if r.ResetAt.IsZero() {
		t.Error("允许路径 ResetAt 应被填充.")
	}
	if r.RetryAfter != 0 {
		t.Errorf("允许路径 RetryAfter 应为 0,实际 %v.", r.RetryAfter)
	}
	if r.Reason != "" {
		t.Errorf("允许路径 Reason 应为空,实际 %v.", r.Reason)
	}
}

// TestCheck_MaxKeysRejected 验证 Check 在 maxKeys 上限被拒时 Reason=RejectMaxKeys.
func TestCheck_MaxKeysRejected(t *testing.T) {
	l := New(10, WithMaxKeys(1))
	defer l.Close()

	l.Allow("a") // 占满 1 个 key.

	r := l.Check("b")
	if r.Allowed {
		t.Fatal("超过 maxKeys 上限应被拒.")
	}
	if r.Reason != RejectMaxKeys {
		t.Errorf("拒绝原因应为 RejectMaxKeys,实际 %v.", r.Reason)
	}
	if r.RetryAfter <= 0 {
		t.Errorf("MaxKeys 拒绝时 RetryAfter 应 > 0,实际 %v.", r.RetryAfter)
	}
}

// TestRetryAfterSeconds_Helper 验证 exported helper 的边界行为.
func TestRetryAfterSeconds_Helper(t *testing.T) {
	cases := []struct {
		rate float64
		want int
	}{
		{rate: 100, want: 1},  // rate >= 1 → 1 秒.
		{rate: 1, want: 1},    // rate = 1 → 1 秒.
		{rate: 0.5, want: 2},  // rate = 0.5 → 2 秒.
		{rate: 0.1, want: 10}, // rate = 0.1 → 10 秒.
		{rate: 0.05, want: 20},
		{rate: 0, want: 1}, // 异常 → 1 秒兜底.
		{rate: -1, want: 1},
	}
	for _, c := range cases {
		if got := RetryAfterSeconds(c.rate); got != c.want {
			t.Errorf("RetryAfterSeconds(%v) = %d,期望 %d.", c.rate, got, c.want)
		}
	}
}

// ================== Wait(阻塞式整流)测试 ==================

// TestWait_Basic 验证有令牌时 Wait 立即返回 nil.
func TestWait_Basic(t *testing.T) {
	l := New(100)
	defer l.Close()

	if err := l.Wait(t.Context(), "k"); err != nil {
		t.Errorf("有令牌时 Wait 应返回 nil,实际 %v.", err)
	}
}

// TestWait_BlocksUntilToken 验证桶用尽后 Wait 阻塞等待令牌补充,而非立即拒绝.
func TestWait_BlocksUntilToken(t *testing.T) {
	// 10 rps, burst=1 → 用尽后约 100ms 补 1 个令牌.
	l := New(10, WithBurst(1))
	defer l.Close()

	if err := l.Wait(t.Context(), "k"); err != nil {
		t.Fatalf("首个请求应立即通过,实际 %v.", err)
	}

	start := time.Now()
	if err := l.Wait(t.Context(), "k"); err != nil {
		t.Fatalf("第二个请求应在等待后通过,实际 %v.", err)
	}
	waited := time.Since(start)

	// 第二个请求被迫等待约 100ms(留余量,至少 50ms);若几乎没等,说明没整流.
	if waited < 50*time.Millisecond {
		t.Errorf("Wait 应阻塞约 100ms 等待令牌,实际只等了 %v.", waited)
	}
	t.Logf("Wait 阻塞了 %v 等待令牌补充.", waited)
}

// TestWait_DeadlineTooShort 验证 ctx deadline 早于令牌补充时间时,Wait 不傻等到
// deadline,而是(由底层 rate 包)提前返回非 nil 错误.
func TestWait_DeadlineTooShort(t *testing.T) {
	// 1 rps, burst=1 → 用尽后需等 1s,但 ctx 只有 50ms,根本来不及补令牌.
	l := New(1, WithBurst(1))
	defer l.Close()

	_ = l.Wait(t.Context(), "k") // 用尽桶.

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := l.Wait(ctx, "k"); err == nil {
		t.Error("deadline 内补不到令牌时 Wait 应返回非 nil 错误.")
	}
}

// TestWait_Canceled 验证等待过程中 ctx 被取消时,Wait 返回 context.Canceled.
func TestWait_Canceled(t *testing.T) {
	// 1 rps, burst=1 → 用尽后需等 1s;ctx 无 deadline,等待中手动 cancel.
	l := New(1, WithBurst(1))
	defer l.Close()

	_ = l.Wait(t.Context(), "k") // 用尽桶.

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	err := l.Wait(ctx, "k")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("等待中被 cancel 时 Wait 应返回 context.Canceled,实际 %v.", err)
	}
}

// TestWait_MaxKeys 验证 WithMaxKeys 上限后,Wait 新 key 立即返回 ErrMaxKeys.
func TestWait_MaxKeys(t *testing.T) {
	l := New(1000, WithMaxKeys(1))
	defer l.Close()

	if err := l.Wait(t.Context(), "a"); err != nil {
		t.Fatalf("首个 key 应允许,实际 %v.", err)
	}

	err := l.Wait(t.Context(), "b")
	if !errors.Is(err, ErrMaxKeys) {
		t.Errorf("超过 WithMaxKeys 上限时 Wait 应返回 ErrMaxKeys,实际 %v.", err)
	}
}

// ================== 并发测试 ==================

func TestConcurrency_Correctness(t *testing.T) {
	const burst = 50
	l := New(float64(burst)) // High rate = burst, so no refill interference.
	defer l.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for range 200 {
		wg.Go(func() {
			for range 10 {
				if l.Allow("strict") {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	a := allowed.Load()
	t.Logf("concurrency: allowed=%d / total=2000 (burst=%d).", a, burst)
	// 留 5 个余量给测试期间的令牌补充.
	if a > int64(burst)+5 {
		t.Errorf("CRITICAL: allowed %d > burst %d. Race condition.", a, burst)
	}
}

func TestConcurrency_NoLoss(t *testing.T) {
	l := New(1000)
	defer l.Close()

	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup

	for range 100 {
		wg.Go(func() {
			for range 100 {
				if l.Allow("k") {
					allowed.Add(1)
				} else {
					rejected.Add(1)
				}
			}
		})
	}
	wg.Wait()

	total := allowed.Load() + rejected.Load()
	if total != 10000 {
		t.Errorf("expected 10000 total, got %d.", total)
	}
}

func TestConcurrency_MultiKey(t *testing.T) {
	const burst = 10
	l := New(float64(burst))
	defer l.Close()

	var wg sync.WaitGroup
	results := make([]atomic.Int64, 20)

	for i := range 20 {
		key := fmt.Sprintf("key-%d", i)
		for range 10 {
			wg.Go(func() {
				for range 5 {
					if l.Allow(key) {
						results[i].Add(1)
					}
				}
			})
		}
	}
	wg.Wait()

	for i := range results {
		if a := results[i].Load(); a > int64(burst)+3 {
			t.Errorf("key %d: allowed %d > burst %d.", i, a, burst)
		}
	}
}

func TestConcurrency_LoadOrStore(t *testing.T) {
	l := New(10)
	defer l.Close()

	var ptrs [100]*visitor
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Go(func() {
			ptrs[i] = l.getOrCreate("race-key")
		})
	}
	wg.Wait()

	for i := 1; i < 100; i++ {
		if ptrs[i] != ptrs[0] {
			t.Fatalf("goroutine %d got a different visitor — LoadOrStore race.", i)
		}
	}
}

// ================== 清理测试 ==================

func TestCleanup(t *testing.T) {
	l := New(10, WithCleanupInterval(50*time.Millisecond), WithMaxIdleTime(100*time.Millisecond))
	defer l.Close()

	l.Allow("should-expire")
	// 等待 idle + 一次清理循环.
	time.Sleep(250 * time.Millisecond)

	// 过期 key 应已被清理.
	// 再触发一次查询 — 若清理生效,Tokens 返回满 burst.
	if tok := l.Tokens("should-expire"); tok != 10 {
		t.Errorf("expected full burst after cleanup, got %f.", tok)
	}
}

func TestCleanup_ActiveKeysSurvive(t *testing.T) {
	// rate=2/s + burst=100 → 在 250ms 内最多补 0.5 个令牌,active 桶仍处于"被消耗"状态,
	// 避免出现"rate ≥ burst 时令牌瞬间补满"导致断言永远失败的测试设计 bug.
	l := New(2, WithBurst(100), WithCleanupInterval(50*time.Millisecond), WithMaxIdleTime(200*time.Millisecond))
	defer l.Close()

	// 保持一个 key 活跃,让另一个过期.
	l.Allow("active")
	l.Allow("idle")

	time.Sleep(100 * time.Millisecond)
	l.Allow("active") // Refresh active key.
	time.Sleep(150 * time.Millisecond)

	// 活跃 key 应保留状态(不是满 burst).
	activeTok := l.Tokens("active")
	idleTok := l.Tokens("idle")

	if activeTok >= float64(l.Burst()) {
		t.Errorf("active key should have consumed tokens, got %f / burst=%d.", activeTok, l.Burst())
	}
	if idleTok != float64(l.Burst()) {
		t.Errorf("idle key should have been cleaned up and return full burst, got %f.", idleTok)
	}
}

// ================== Benchmark ==================

func BenchmarkAllow_SingleKey(b *testing.B) {
	l := New(1000000)
	defer l.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow("bench")
		}
	})
}

func BenchmarkAllow_MultiKey(b *testing.B) {
	l := New(1000000)
	defer l.Close()

	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow(keys[i%len(keys)])
			i++
		}
	})
}
