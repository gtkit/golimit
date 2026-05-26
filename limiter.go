// Package golimit 提供轻量、并发安全的限流封装,
// 基于 golang.org/x/time/rate(令牌桶算法),支持 per-key 隔离与自动清理.
//
// Deprecated: v1 已停止新功能开发,仅接受关键 bug 修复.
// 新项目请使用 github.com/gtkit/golimit/v2 — 修复了 v1 的若干设计缺陷:
//   - 从全局单例改为可实例化的 *Limiter,支持多个独立限流器.
//   - 新增 Close() 优雅关闭 cleanup goroutine,不再泄漏.
//   - 支持 fractional rps、键基数上限 (WithMaxKeys) 防 DoS、拒绝回调 (WithOnReject).
package golimit

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Limiters 是 per-key 限流器的并发安全注册表.
type Limiters struct {
	limiters sync.Map // map[string]*Limiter.
}

// Limiter 包装 rate.Limiter,额外记录最后访问时间用于自动清理.
type Limiter struct {
	limiter *rate.Limiter
	lastGet atomic.Int64 // Unix 纳秒时间戳,原子更新,避免 data race.
}

// GlobalLimiters 是包级单例注册表,所有 key 共享同一注册表与同一后台清理 goroutine.
var GlobalLimiters = &Limiters{}

var once sync.Once

// NewLimiter 返回指定 key 的限流器,允许每秒 rps 个请求,初始突发容量等于 rps.
//
// 首次调用会通过 sync.Once 启动后台清理 goroutine.
//
// 🔄 行为说明(自 v1.0.5):同一 key 多次调用 NewLimiter 时,新传入的 rps **会热更新**
// 到现有 Limiter(底层调用 stdlib rate.Limiter.SetLimit/SetBurst,并发安全).
// 此前版本(<= v1.0.4)会静默忽略新 rps,只返回首次创建的实例 — 这是隐性 bug,
// 现已修复.
//
// stdlib 的"放气"语义注意:把 rps 从高调到低(例如 100 → 5)的瞬间,桶里已有的
// token 不会立即被砍到新 burst,要到下次 Allow 调用时才会 cap.因此切换过渡期
// 可能放过最多 (旧 burst) 个请求,之后才严格按新 rps 限流.如需立即生效,
// 请额外等待至少 1 / (旧 rps) 秒,让 stdlib 自然回填.
//
// rps < 0 会被规范化为 0(等价于"完全禁止"),避免触发 stdlib SetBurst 的 panic.
//
// Deprecated: 请改用 github.com/gtkit/golimit/v2 的 New() + Allow(key).
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
// 命中已有 key 时,若 rps 与当前 Limit() 不同则调用 stdlib SetLimit/SetBurst
// 热更新,保证调速后续生效.LoadOrStore 用于消除多 goroutine 同时首次创建的竞态.
func (ls *Limiters) getLimiter(key string, rps int) *Limiter {
	// 负数 rps 会触发 rate.Limiter.SetBurst 内部 panic("burst < 0"),规范化为 0.
	if rps < 0 {
		rps = 0
	}

	newLimit := rate.Limit(rps)

	if v, ok := ls.limiters.Load(key); ok {
		l := v.(*Limiter)
		// Limit() 内部加锁;先比较再 Set 可以省去 rps 不变时的两次额外 Lock.
		if l.limiter.Limit() != newLimit {
			l.limiter.SetLimit(newLimit)
			l.limiter.SetBurst(rps)
		}
		return l
	}

	l := &Limiter{
		// 第一个参数 r Limit:每秒向桶中补充的 token 数(rate.Limit 即 float64).
		// 第二个参数 b int:桶容量,即允许的最大突发并发量.
		limiter: rate.NewLimiter(newLimit, rps),
	}
	l.lastGet.Store(time.Now().UnixNano())

	if actual, loaded := ls.limiters.LoadOrStore(key, l); loaded {
		// 输给了另一个 goroutine 的 LoadOrStore — 拿到的实例可能是别人刚塞进去的,
		// 其 rps 可能与本次入参不同.同步一次 SetLimit/SetBurst 以保证"最后写入者胜出"
		// 的语义,行为与命中已有 key 时一致.
		existing := actual.(*Limiter)
		if existing.limiter.Limit() != newLimit {
			existing.limiter.SetLimit(newLimit)
			existing.limiter.SetBurst(rps)
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
	ls.limiters.Range(func(key, value any) bool {
		lim := value.(*Limiter)
		if now-lim.lastGet.Load() > int64(idleThreshold) {
			ls.limiters.Delete(key)
		}
		return true // 始终返回 true,遍历所有 key.
	})
}
