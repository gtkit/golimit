// Package golimit 提供轻量、并发安全的限流封装,
// 基于 golang.org/x/time/rate(令牌桶算法),支持 per-key 隔离与自动清理.
//
// 同仓库另有 github.com/gtkit/golimit/v2,设计上的不同:
//   - 单一 Limiter 类型(内含 sync.Map),不再有 Limiters / Limiter 两层结构.
//   - 可实例化,支持多个独立限流器.
//   - Close() 可优雅关闭 cleanup goroutine.
//   - 支持 fractional rps、键基数上限(WithMaxKeys)、拒绝回调(WithOnReject).
//
// v1 与 v2 是两套独立 API,各有适用场景:
//   - v1 适合"一个进程一个全局注册表"的简单场景,API 稳定.
//   - v2 适合需要"多实例隔离 / 生命周期管理 / DoS 防护"的复杂场景.
package golimit

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Limiters 是 per-key 限流器的并发安全注册表(registry / pool).
//
// 备注:Limiters 与 Limiter 的命名只差一个 s,语义不同 —
// Limiters 管理多个 Limiter.这是 v1 的两层 API 设计,v2 已合并为单一 Limiter.
type Limiters struct {
	entries sync.Map // map[string]*Limiter.
}

// Limiter 包装单个令牌桶,记录最后访问时间用于自动清理.
type Limiter struct {
	limiter *rate.Limiter
	// rps 缓存当前配置的速率,使 getLimiter 命中已有 key 时的比较无需调用
	// rate.Limiter.Limit()(后者加 mutex).atomic.Load + 整数比较 ~1 ns,
	// 在"同 key 同 rps"的高频路径(如 Gin middleware 每请求 NewLimiter)
	// 上避免 mutex 争用.
	rps     atomic.Int64
	lastGet atomic.Int64 // Unix 纳秒时间戳,原子更新,避免 data race.
}

// GlobalLimiters 是包级单例注册表,所有 key 共享同一注册表与同一后台清理 goroutine.
var GlobalLimiters = &Limiters{}

var once sync.Once

// NewLimiter 返回指定 key 的限流器,允许每秒 rps 个请求,初始突发容量等于 rps.
//
// 首次调用会通过 sync.Once 启动后台清理 goroutine.
//
// 同一 key 多次调用 NewLimiter 时,新传入的 rps 会**热更新**到现有 Limiter
// (底层调用 stdlib rate.Limiter.SetLimit/SetBurst,并发安全).
//
// stdlib 的"放气"语义注意:把 rps 从高调到低(例如 100 → 5)的瞬间,桶里已有的
// token 不会立即被砍到新 burst,要到下次 Allow 调用时才会 cap.因此切换过渡期
// 可能放过最多 (旧 burst) 个请求,之后才严格按新 rps 限流.如需立即生效,
// 请额外等待至少 1 / (旧 rps) 秒,让 stdlib 自然回填.
//
// rps < 0 会被规范化为 0(等价于"完全禁止"),避免触发 stdlib SetBurst 的 panic.
//
// 用法:
//
//	lim := golimit.NewLimiter("ip:192.168.1.1:/api/v1", 100)
//	if !lim.Allow() {
//	    // 被限流了.
//	}
func NewLimiter(key string, rps int) *Limiter {
	once.Do(func() {
		go GlobalLimiters.clearLimiter()
	})
	return GlobalLimiters.getLimiter(key, rps)
}

// Allow 判断当前请求是否在限流额度内.
// 并发安全,可以从任意数量的 goroutine 同时调用.
func (l *Limiter) Allow() bool {
	l.lastGet.Store(time.Now().UnixNano())
	return l.limiter.Allow()
}

// getLimiter 查找已有 Limiter 或原子地创建新实例.
//
// 命中已有 key 时,通过 atomic.Int64.Load 比较 rps,免锁.仅当 rps 实际变化
// 时才调 SetLimit + SetBurst(各加一次 mutex).LoadOrStore 用于消除多 goroutine
// 同时首次创建同一 key 的竞态;输家分支也走相同的 rps 同步逻辑,保证语义一致.
func (ls *Limiters) getLimiter(key string, rps int) *Limiter {
	// 负数 rps 会触发 rate.Limiter.SetBurst 内部 panic("burst < 0"),规范化为 0.
	if rps < 0 {
		rps = 0
	}

	if v, ok := ls.entries.Load(key); ok {
		l := v.(*Limiter)
		if int(l.rps.Load()) != rps {
			// rps 真正变化才付 mutex 代价.SetLimit 与 SetBurst 都要 —
			// v1 API 把 rate 与 burst 绑定到同一 rps 参数.
			l.limiter.SetLimit(rate.Limit(rps))
			l.limiter.SetBurst(rps)
			l.rps.Store(int64(rps))
		}
		return l
	}

	l := &Limiter{
		// 第一个参数 r Limit:每秒向桶中补充的 token 数(rate.Limit 即 float64).
		// 第二个参数 b int:桶容量,即允许的最大突发并发量.
		limiter: rate.NewLimiter(rate.Limit(rps), rps),
	}
	// rps 必须在 LoadOrStore 之前 Store — 否则其他 goroutine 看到这个 visitor 时
	// rps=0,会误以为需要热更新.
	l.rps.Store(int64(rps))
	l.lastGet.Store(time.Now().UnixNano())

	if actual, loaded := ls.entries.LoadOrStore(key, l); loaded {
		// 输给了另一个 goroutine 的 LoadOrStore — 拿到的实例可能是别人刚塞进去的,
		// 其 rps 可能与本次入参不同.同步一次以保证"最后写入者胜出"的语义.
		existing := actual.(*Limiter)
		if int(existing.rps.Load()) != rps {
			existing.limiter.SetLimit(rate.Limit(rps))
			existing.limiter.SetBurst(rps)
			existing.rps.Store(int64(rps))
		}
		return existing
	}
	return l
}

// idleThreshold 是 Limiter 被回收前允许的最大空闲时长.
const idleThreshold = 5 * time.Minute

// clearLimiter 在后台 goroutine 中周期性地清理空闲 Limiter.
func (ls *Limiters) clearLimiter() {
	for {
		time.Sleep(1 * time.Minute)
		ls.clearOnce()
	}
}

// clearOnce 执行一次清理,移除超过 idleThreshold 没被访问的 Limiter.
// 抽成独立函数便于测试.
func (ls *Limiters) clearOnce() {
	now := time.Now().UnixNano()
	ls.entries.Range(func(key, value any) bool {
		lim := value.(*Limiter)
		if now-lim.lastGet.Load() > int64(idleThreshold) {
			ls.entries.Delete(key)
		}
		return true // 始终返回 true,遍历所有 key.
	})
}
