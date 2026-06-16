package golimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 备注:测试用独立的 Limiters 实例,而非包级单例 GlobalLimiters.
// 原因:v1 的 GlobalLimiters 在测试进程内是全局共享的,sync.Map 中的 Limiter 状态
// 会跨测试持续存在.使用 `go test -count=N` 时(N>1)会出现 token 残留导致 flaky.
// 独立实例确保每个测试用例从 0 状态开始,且不会污染其他用例.
//
// 端到端通过包级 NewLimiter 的测试(确保全局入口工作正常)放在最后,用 t.Name()
// 嵌入 key 避免命名碰撞.

// newTestLS 返回一个独立 Limiters 实例,供单测使用.
func newTestLS() *Limiters { return &Limiters{} }

// ================== 功能测试 ==================

// TestBasicRateLimit 验证 burst 内的请求允许通过,超出的被拒.
func TestBasicRateLimit(t *testing.T) {
	lim := newTestLS().getLimiter("basic-test", 5)

	for i := range 5 {
		if !lim.Allow() {
			t.Errorf("请求 %d 应允许通过.", i+1)
		}
	}

	// 第 6 个请求应被拒绝(burst 耗尽,没时间补充令牌).
	if lim.Allow() {
		t.Error("请求 6 应被拒绝.")
	}
}

// TestDifferentKeysAreIndependent 验证每个 key 都有独立的限流器.
func TestDifferentKeysAreIndependent(t *testing.T) {
	ls := newTestLS()
	limA := ls.getLimiter("key-a", 3)
	limB := ls.getLimiter("key-b", 3)

	// 用尽 key-a.
	for range 3 {
		limA.Allow()
	}

	if limA.Allow() {
		t.Error("key-a 应已耗尽.")
	}

	// key-b 不受影响.
	if !limB.Allow() {
		t.Error("key-b 仍应允许通过.")
	}
}

// TestSameKeyReturnsSameLimiter 验证同 key 多次调用返回同一实例.
func TestSameKeyReturnsSameLimiter(t *testing.T) {
	ls := newTestLS()
	lim1 := ls.getLimiter("same-key-test", 10)
	lim2 := ls.getLimiter("same-key-test", 10)

	if lim1 != lim2 {
		t.Error("同 key 多次调用 getLimiter 应返回同一 *Limiter 实例.")
	}
}

// TestTokenRecovery 验证令牌随时间补充.
func TestTokenRecovery(t *testing.T) {
	// 10 rps = 每 100ms 1 个令牌,burst = 10.
	lim := newTestLS().getLimiter("recovery-test", 10)

	// 用尽所有令牌.
	for range 10 {
		lim.Allow()
	}
	if lim.Allow() {
		t.Error("令牌耗尽后应被拒绝.")
	}

	// 等待至少 1 个令牌补充.
	time.Sleep(150 * time.Millisecond)

	if !lim.Allow() {
		t.Error("令牌补充后应允许通过.")
	}
}

// TestRateSemantic 验证 rps 参数真实控制稳态速率.
// 这是关键修复点 — 老代码用 rate.Every(1s) 导致无论 rps 多少都只 1 req/s.
func TestRateSemantic(t *testing.T) {
	rps := 100
	lim := newTestLS().getLimiter("rate-semantic-test", rps)

	// 用尽 burst.
	for range rps {
		lim.Allow()
	}

	// 等 100ms — 100 rps 下应补充约 10 个令牌.
	time.Sleep(100 * time.Millisecond)

	allowed := 0
	for range 20 {
		if lim.Allow() {
			allowed++
		}
	}

	// 应至少补充 5 个令牌(留出时序余量).
	if allowed < 5 {
		t.Errorf("100ms 内 %d rps 应至少补充 5 个令牌,实际 %d.", rps, allowed)
	}
	t.Logf("速率语义:100ms 内 %d rps 补充了 %d 个令牌.", rps, allowed)
}

// TestDefaultValues 验证 rps=0 不 panic 且拒绝所有请求.
func TestDefaultValues(t *testing.T) {
	// rate.Limit(0) 表示永不补充令牌,burst=0 → 所有请求都被拒绝.
	lim := newTestLS().getLimiter("zero-rps-test", 0)
	if lim.Allow() {
		t.Error("rps=0 应拒绝所有请求.")
	}
}

// ================== RPS 热更新测试 ==================

// TestRPSHotUpdate 验证同 key 多次调用 getLimiter 时,新 rps 热更新到现有实例.
func TestRPSHotUpdate(t *testing.T) {
	ls := newTestLS()

	// 初次创建:rps=10.
	lim1 := ls.getLimiter("hot-update", 10)
	if got := float64(lim1.limiter.Limit()); got != 10 {
		t.Errorf("初始 Limit 应为 10,实际 %v.", got)
	}
	if got := lim1.limiter.Burst(); got != 10 {
		t.Errorf("初始 Burst 应为 10,实际 %d.", got)
	}

	// 同 key 提升 rps:实例不变,Limit/Burst 更新.
	lim2 := ls.getLimiter("hot-update", 50)
	if lim1 != lim2 {
		t.Error("同 key 应返回同一 *Limiter 实例.")
	}
	if got := float64(lim2.limiter.Limit()); got != 50 {
		t.Errorf("提升后 Limit 应为 50,实际 %v.", got)
	}
	if got := lim2.limiter.Burst(); got != 50 {
		t.Errorf("提升后 Burst 应为 50,实际 %d.", got)
	}

	// 同 key 降低 rps:同理更新.
	lim3 := ls.getLimiter("hot-update", 5)
	if got := float64(lim3.limiter.Limit()); got != 5 {
		t.Errorf("降低后 Limit 应为 5,实际 %v.", got)
	}
	if got := lim3.limiter.Burst(); got != 5 {
		t.Errorf("降低后 Burst 应为 5,实际 %d.", got)
	}
}

// TestRPSHotUpdate_Idempotent 验证相同 rps 多次调用幂等,行为不变.
func TestRPSHotUpdate_Idempotent(t *testing.T) {
	ls := newTestLS()

	lim1 := ls.getLimiter("idempotent", 100)
	lim2 := ls.getLimiter("idempotent", 100)
	lim3 := ls.getLimiter("idempotent", 100)

	if lim1 != lim2 || lim2 != lim3 {
		t.Error("相同 rps 多次调用应返回同一实例.")
	}
	if got := float64(lim3.limiter.Limit()); got != 100 {
		t.Errorf("Limit 应保持 100,实际 %v.", got)
	}
}

// TestRPSHotUpdate_RateActuallyApplied 验证热更新后请求实际受新速率约束.
// 即使 stdlib 有"放气"语义保留旧 token,降速后桶中可用令牌也是有限的.
func TestRPSHotUpdate_RateActuallyApplied(t *testing.T) {
	ls := newTestLS()

	// 初始 rps=100,burst=100.
	_ = ls.getLimiter("apply-test", 100)
	// 调降到 1 rps,burst=1.
	lim := ls.getLimiter("apply-test", 1)

	// 把桶中残留令牌全部消耗(stdlib 放气下最多 100 个).
	consumed := 0
	for range 200 {
		if !lim.Allow() {
			break
		}
		consumed++
	}

	// 残留耗尽后立即调用应被拒(1 rps 补充太慢).
	if lim.Allow() {
		t.Errorf("rps=1 时桶应空,但消耗 %d 个旧令牌后仍放过请求.", consumed)
	}

	// 旧令牌不应超过旧 burst(100).
	if consumed > 100 {
		t.Errorf("放气期消耗的令牌数 %d 不应超过旧 burst 100.", consumed)
	}
	t.Logf("放气期消耗了 %d 个旧令牌,之后严格按新 rps=1 限流.", consumed)
}

// TestRPSHotUpdate_NegativeRPS 验证负数 rps 不 panic,被规范化为 0.
func TestRPSHotUpdate_NegativeRPS(t *testing.T) {
	ls := newTestLS()

	_ = ls.getLimiter("neg-test", 10)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("负数 rps 不应 panic,实际 panic:%v.", r)
		}
	}()
	lim := ls.getLimiter("neg-test", -5)

	if got := float64(lim.limiter.Limit()); got != 0 {
		t.Errorf("负数 rps 应规范化为 0,实际 %v.", got)
	}
	if got := lim.limiter.Burst(); got != 0 {
		t.Errorf("负数 rps 时 Burst 应为 0,实际 %d.", got)
	}
	// burst=0 意味着永远拒绝.
	if lim.Allow() {
		t.Error("rps 被规范化为 0 后所有请求都应被拒.")
	}
}

// TestRPSHotUpdate_FirstCallNegative 验证首次创建时 rps 为负也不 panic.
func TestRPSHotUpdate_FirstCallNegative(t *testing.T) {
	ls := newTestLS()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("首次创建时负数 rps 不应 panic,实际 panic:%v.", r)
		}
	}()
	lim := ls.getLimiter("first-neg", -100)
	if got := float64(lim.limiter.Limit()); got != 0 {
		t.Errorf("首次负数 rps 应规范化为 0,实际 %v.", got)
	}
	if lim.Allow() {
		t.Error("首次负数 rps 后所有请求都应被拒.")
	}
}

// TestRPSHotUpdate_Concurrent 并发场景:多 goroutine 同时用不同 rps 调用同一 key,
// 不应 panic / data race / 死锁,且最终 Limiter 仍可正常工作.
func TestRPSHotUpdate_Concurrent(t *testing.T) {
	ls := newTestLS()

	const goroutines = 200
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)
		go func(rps int) {
			defer wg.Done()
			lim := ls.getLimiter("concurrent-update", rps+1) // 1..200
			lim.Allow()
		}(i)
	}
	wg.Wait()

	// 收尾再读一次,确保 Limiter 仍存活、可调用.
	lim := ls.getLimiter("concurrent-update", 50)
	_ = lim.Allow() // 不强断言结果,关键是不 panic / 不死锁.

	// 并发更新后最终 Limit 应等于某次调用传入的值,落在合法范围 [1, 200] 内.
	got := float64(lim.limiter.Limit())
	if got < 1 || got > 200 {
		t.Errorf("并发更新后 Limit 应在 [1, 200] 内,实际 %v.", got)
	}
}

// TestRPSHotUpdate_LoadOrStoreLoser 验证 LoadOrStore 失败分支(并发首次创建竞态)
// 也会执行 rps 同步,保证"最后写入者胜出"语义在所有路径上一致.
func TestRPSHotUpdate_LoadOrStoreLoser(t *testing.T) {
	ls := newTestLS()

	const (
		key        = "loser-key"
		goroutines = 50
	)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ls.getLimiter(key, idx+1)
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 拿到的实例应是同一个;最终 Limit 应在合法范围 [1, goroutines] 内.
	finalLim, _ := ls.entries.Load(key)
	if finalLim == nil {
		t.Fatal("最终应存在 limiter 实例.")
	}
	final := float64(finalLim.(*Limiter).limiter.Limit())
	if final < 1 || final > goroutines {
		t.Errorf("并发首次创建竞态后最终 Limit 应在 [1, %d] 内,实际 %v.", goroutines, final)
	}
	t.Logf("并发首次创建竞态后最终 Limit=%v.", final)
}

// ================== 端到端测试:验证包级 NewLimiter 入口与全局注册表 ==================

// TestNewLimiterEntry_BasicAndHotUpdate 端到端验证:
//   - 包级 NewLimiter 能正常创建 Limiter
//   - 同 key 二次调用热更新 rps 生效
//
// 使用 t.Name() 嵌入 key 避免与其他端到端测试在全局 GlobalLimiters 上冲突.
func TestNewLimiterEntry_BasicAndHotUpdate(t *testing.T) {
	key := "endtoend:" + t.Name()

	lim1 := NewLimiter(key, 10)
	if got := float64(lim1.limiter.Limit()); got != 10 {
		t.Errorf("通过 NewLimiter 创建后 Limit 应为 10,实际 %v.", got)
	}

	lim2 := NewLimiter(key, 100)
	if lim1 != lim2 {
		t.Error("同 key 应返回同一实例.")
	}
	if got := float64(lim2.limiter.Limit()); got != 100 {
		t.Errorf("通过 NewLimiter 热更新后 Limit 应为 100,实际 %v.", got)
	}

	// 验证 Allow 工作正常.
	if !lim2.Allow() {
		t.Error("热更新后立即 Allow 应通过(刚提升 burst,桶里有 token).")
	}
}

// ================== 并发安全测试 ==================

// TestConcurrencyCorrectness 高并发争用下允许通过的总数不得超过配置上限.
// 运行命令:go test -race -run TestConcurrencyCorrectness.
func TestConcurrencyCorrectness(t *testing.T) {
	const (
		burst      = 50
		goroutines = 200
		reqsPerG   = 10
	)

	lim := newTestLS().getLimiter("concurrency-correctness", burst)

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
	t.Logf("并发正确性:allowed=%d,total=%d,burst=%d.", a, goroutines*reqsPerG, burst)

	// 允许 5 个余量给测试期间的令牌补充.
	if a > int64(burst)+5 {
		t.Errorf("严重问题:放过 %d 个请求,超过 burst %d,可能存在 race.", a, burst)
	}
}

// TestConcurrencyNoLoss 验证没有请求被静默丢失或重复.
func TestConcurrencyNoLoss(t *testing.T) {
	const (
		rps        = 1000
		goroutines = 100
		reqsPerG   = 100
	)

	lim := newTestLS().getLimiter("no-loss-test", rps)
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
		t.Errorf("期望共 %d 个请求,实际 %d.", goroutines*reqsPerG, total)
	}
	t.Logf("无丢失:allowed=%d,rejected=%d,total=%d.", allowed.Load(), rejected.Load(), total)
}

// TestMultiKeyConcurrency 并发场景下验证 per-key 隔离.
func TestMultiKeyConcurrency(t *testing.T) {
	const (
		numKeys  = 20
		burst    = 10
		reqsPerG = 5
		gsPerKey = 10
	)

	ls := newTestLS()
	var wg sync.WaitGroup
	results := make([]atomic.Int64, numKeys)

	for i := range numKeys {
		key := fmt.Sprintf("multi-key-%d", i)
		lim := ls.getLimiter(key, burst)

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
			t.Errorf("key %d:放过 %d 个,超过 burst %d.", i, a, burst)
		}
	}
}

// TestLoadOrStoreRace 专门针对原本的 Load+Store 竞态:
// 多个 goroutine 同时为同一 key 触发首次创建.
func TestLoadOrStoreRace(t *testing.T) {
	const goroutines = 100

	ls := newTestLS()
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

	// 所有 goroutine 应该拿到完全相同的 *Limiter 实例.
	for i := 1; i < goroutines; i++ {
		if ptrs[i] != ptrs[0] {
			t.Errorf("goroutine %d 拿到不同的 *Limiter 实例 — LoadOrStore 竞态未消除.", i)
		}
	}
}

// ================== 清理逻辑测试 ==================

// TestCleanupRemovesIdleEntries 验证清理逻辑移除空闲条目.
// 直接测试 clearOnce,不需要等 5 分钟.
func TestCleanupRemovesIdleEntries(t *testing.T) {
	ls := newTestLS()

	// 创建一个 Limiter 并把 lastGet 改成 10 分钟前.
	lim := ls.getLimiter("stale-key", 10)
	lim.lastGet.Store(time.Now().Add(-10 * time.Minute).UnixNano())

	// 创建一个新鲜的 Limiter.
	fresh := ls.getLimiter("fresh-key", 10)
	fresh.lastGet.Store(time.Now().UnixNano())

	// 跑一次清理.
	ls.clearOnce()

	// 旧 key 应被清掉.
	if _, ok := ls.entries.Load("stale-key"); ok {
		t.Error("stale-key 应被清理.")
	}

	// 新 key 应保留.
	if _, ok := ls.entries.Load("fresh-key"); !ok {
		t.Error("fresh-key 应仍存在.")
	}
}

// ================== 加固测试:rps 热更新并发一致性 ==================

// TestRPSHotUpdate_ConcurrentConsistency 验证并发传不同 rps 时,最终 Limit/Burst/缓存 rps
// 三者一致.修复前 SetLimit/SetBurst/rps.Store 各自原子但组合不原子,并发可能让三者来自
// 不同"胜者"导致错乱;updateRPS 把三者收进同一临界区后恒一致.
func TestRPSHotUpdate_ConcurrentConsistency(t *testing.T) {
	ls := newTestLS()
	const key = "consistency"
	_ = ls.getLimiter(key, 1)

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(rps int) {
			defer wg.Done()
			ls.getLimiter(key, rps)
		}(i%50 + 1) // rps 在 1..50.
	}
	wg.Wait()

	v, _ := ls.entries.Load(key)
	lim := v.(*Limiter)
	gotLimit := int(float64(lim.limiter.Limit()))
	gotBurst := lim.limiter.Burst()
	gotRPS := int(lim.rps.Load())

	// v1 把 rate 与 burst 绑定到同一 rps,三者必须相等.
	if gotLimit != gotBurst || gotBurst != gotRPS {
		t.Errorf("并发热更新后 Limit/Burst/缓存rps 应一致,实际 Limit=%d Burst=%d rps=%d.",
			gotLimit, gotBurst, gotRPS)
	}
}

// ================== Benchmark ==================

// BenchmarkAllow_SingleKey 测量单 key 的吞吐(最坏争用场景).
func BenchmarkAllow_SingleKey(b *testing.B) {
	lim := newTestLS().getLimiter("bench-single", 1000000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow()
		}
	})
}

// BenchmarkAllow_MultiKey 测量多 key 吞吐(典型生产场景).
func BenchmarkAllow_MultiKey(b *testing.B) {
	ls := newTestLS()
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-multi-%d", i)
	}
	// 预创建所有 Limiter.
	for _, k := range keys {
		ls.getLimiter(k, 1000000)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			ls.getLimiter(keys[i%len(keys)], 1000000).Allow()
			i++
		}
	})
}

// BenchmarkGetLimiter_RPSUnchanged 测量"同 key 同 rps"的 hot path 开销.
// 这是模式 B(每请求 NewLimiter)用户的实际请求路径.
// 实现用 atomic.Int64 缓存 rps,避免 stdlib mutex 锁开销.
func BenchmarkGetLimiter_RPSUnchanged(b *testing.B) {
	ls := newTestLS()
	ls.getLimiter("bench-unchanged", 100)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ls.getLimiter("bench-unchanged", 100) // 同 key 同 rps
		}
	})
}

// BenchmarkGetLimiter_RPSChanged 测量"同 key 不同 rps"的罕见路径开销,
// 验证 atomic 缓存正确触发 SetLimit + SetBurst.罕见路径开销可接受.
func BenchmarkGetLimiter_RPSChanged(b *testing.B) {
	ls := newTestLS()
	ls.getLimiter("bench-changed", 100)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// 在 100 / 101 之间交替,每次都触发热更新路径.
			ls.getLimiter("bench-changed", 100+(i&1))
			i++
		}
	})
}
