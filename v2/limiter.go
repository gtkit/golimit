// Package golimit 提供生产级、per-key 的限流能力,
// 基于 golang.org/x/time/rate(令牌桶算法).
//
// 设计原则:**零第三方框架依赖**.本包仅依赖 stdlib + golang.org/x/time.
// Web 框架集成(Gin / echo / chi 等)通过独立的 sub-module 提供,例如
// github.com/gtkit/golimit/v2/gin.
//
// v2 重设计:Limiter 改为实例化对象(不再是全局单例),通过 Close() 优雅关闭.
package golimit

import (
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ================== 类型与常量 ==================

// RejectReason 标识 Allow / Check 拒绝的原因.
type RejectReason string

const (
	// RejectRate 因令牌不足被拒(超过 rate / burst 配置).
	RejectRate RejectReason = "rate"
	// RejectMaxKeys 因键基数达到 WithMaxKeys 上限而拒绝新建.
	RejectMaxKeys RejectReason = "max_keys"
)

// RejectFunc 在限流拒绝时被调用,用于接入 metrics / log.
// 实现必须非阻塞且并发安全 — 它会在请求关键路径上同步执行.
type RejectFunc func(key string, reason RejectReason)

// Result 是 Check 的返回值,包含限流决策所需的全部信息.
//
// 这是**框架无关**的接口契约 — Web 中间件 / RPC 拦截器 / 任意调度逻辑
// 都可基于此 struct 写响应头、告警、降级等.
type Result struct {
	// Allowed 当前请求是否被允许.
	Allowed bool

	// Limit 是配置的 burst 上限(即 X-RateLimit-Limit 头的值).
	Limit int

	// Remaining 是当前剩余的令牌数(允许时为消耗 1 个之后的值).
	Remaining float64

	// ResetAt 是桶下一次能补满 burst 的预估时间(仅当 Allowed=true 时填充).
	// 用于 X-RateLimit-Reset 头(Unix 时间戳).
	ResetAt time.Time

	// RetryAfter 当 Allowed=false 时,客户端建议等待多久后重试.
	// 用于 Retry-After 头(秒).
	RetryAfter time.Duration

	// Reason 当 Allowed=false 时的拒绝原因.允许时为空.
	Reason RejectReason
}

// Limiter 管理 per-key 令牌桶限流器,后台自动清理空闲 key.
// 并发安全,可以从任意数量的 goroutine 同时调用.
//
// 每个 key 都拥有独立的 rate.Limiter,存放在 sync.Map 中,
// 不同 key(IP / 用户 / 路径)之间不会争用同一把锁.
type Limiter struct {
	rate  float64 // 每秒补充的 token 数.
	burst int     // 最大突发容量.

	visitors sync.Map     // map[string]*visitor.
	size     atomic.Int64 // 当前 sync.Map 中 key 数量,用于 WithMaxKeys 上限判定与 Size() 监控.
	stopCh   chan struct{}
	wg       sync.WaitGroup

	cleanupInterval time.Duration
	maxIdleTime     time.Duration
	maxKeys         int        // 0 = 无上限.
	onReject        RejectFunc // nil = 不通知.

	// retryAfter 在 New 中根据 rate 一次性算出,Check 拒绝路径直接用,免去每次重算.
	retryAfter time.Duration
}

// visitor 跟踪 per-key 的 rate.Limiter 与最近访问时间.
type visitor struct {
	lim      *rate.Limiter
	lastSeen atomic.Int64 // Unix 纳秒,原子更新,免锁.
}

// Option 用于配置 Limiter.
type Option func(*Limiter)

// ================== Options ==================

// WithBurst 设置最大突发容量.
// 默认为 max(1, ceil(rate)).
func WithBurst(burst int) Option {
	return func(l *Limiter) {
		l.burst = burst
	}
}

// WithCleanupInterval 设置后台清理 goroutine 的扫描间隔.
// 默认为 1 分钟.
func WithCleanupInterval(d time.Duration) Option {
	return func(l *Limiter) {
		l.cleanupInterval = d
	}
}

// WithMaxIdleTime 设置 key 空闲多久后被清理.
// 默认为 5 分钟.
func WithMaxIdleTime(d time.Duration) Option {
	return func(l *Limiter) {
		l.maxIdleTime = d
	}
}

// WithMaxKeys 限制 Limiter 内最多保存的 key 数量,达到上限后拒绝新建.
// 用于防止键基数爆炸攻击(攻击者伪造大量 IP / 路径耗光内存).
// 默认 0 = 无上限.
//
// 推荐线上配置:预估正常业务峰值的 5~10 倍.例如日活 10w 用户可配 100w.
func WithMaxKeys(n int) Option {
	return func(l *Limiter) {
		l.maxKeys = n
	}
}

// WithOnReject 注册拒绝回调,在 Allow / AllowN / Check 返回 false 时触发.
// 用于接入 metrics 计数或结构化日志.回调必须非阻塞.
//
// reason 区分两种拒绝:
//   - RejectRate     — 正常的限流拒绝(令牌不足).
//   - RejectMaxKeys  — 键基数达到 WithMaxKeys 上限.
func WithOnReject(fn RejectFunc) Option {
	return func(l *Limiter) {
		l.onReject = fn
	}
}

// ================== 构造 ==================

// New 创建一个 Limiter,每个 key 每秒允许 rps 个请求.
// 同时启动后台清理 goroutine,需调用 Close() 停止.
//
// 用法:
//
//	lim := golimit.New(100)                              // 100 req/s,burst=100.
//	lim := golimit.New(100, golimit.WithBurst(200))      // 100 req/s,burst=200.
//	lim := golimit.New(100, golimit.WithMaxKeys(100000)) // 100 req/s,最多缓存 10w key.
//	defer lim.Close()
//
// 所有 Option 的非法值都会被规范化为默认值,确保 New 永不 panic:
//   - rps <= 0 → 100
//   - burst <= 0 → max(1, ceil(rps))
//   - cleanupInterval <= 0 → 1 分钟(避免 time.NewTicker(0) panic)
//   - maxIdleTime <= 0 → 5 分钟
//   - maxKeys < 0 → 0(等价于无上限)
func New(rps float64, opts ...Option) *Limiter {
	if rps <= 0 {
		rps = 100
	}

	// burst 至少为 1 — 避免 rps<1 时 int(rps)=0 导致全部请求被拒.
	defaultBurst := max(1, int(math.Ceil(rps)))

	l := &Limiter{
		rate:            rps,
		burst:           defaultBurst,
		stopCh:          make(chan struct{}),
		cleanupInterval: time.Minute,
		maxIdleTime:     5 * time.Minute,
	}
	for _, opt := range opts {
		opt(l)
	}

	// 配置兜底 — 任何 Option 传入非法值都不应让 Limiter 进入不合理状态.
	if l.burst <= 0 {
		l.burst = defaultBurst
	}
	if l.cleanupInterval <= 0 {
		// time.NewTicker 要求 d > 0,否则 panic("non-positive interval").
		l.cleanupInterval = time.Minute
	}
	if l.maxIdleTime <= 0 {
		l.maxIdleTime = 5 * time.Minute
	}
	if l.maxKeys < 0 {
		// 负数 maxKeys 在 getOrCreate 中会让上限检查永远不命中,等价于静默关闭防护.
		// 规范化为 0(显式无上限),避免用户误以为配了防护其实没生效.
		l.maxKeys = 0
	}

	// 预计算 retryAfter,Check 拒绝路径直接用.
	l.retryAfter = computeRetryAfter(l.rate)

	l.wg.Add(1)
	go l.cleanupLoop()

	return l
}

// ================== 主 API ==================

// Allow 判断指定 key 的当前请求是否允许.
// 并发安全,可以从任意数量的 goroutine 同时调用.
//
// 若 Limiter 设置了 WithMaxKeys 且已达上限,新 key 直接被拒(返回 false).
//
// Allow 是 hot path 的"零开销"版本 — 仅返回 bool,无 Result 构造.
// 如需要响应头元信息(Limit / Remaining / RetryAfter),改用 Check.
func (l *Limiter) Allow(key string) bool {
	v := l.getOrCreate(key)
	if v == nil {
		l.notify(key, RejectMaxKeys)
		return false
	}
	v.lastSeen.Store(time.Now().UnixNano())
	if !v.lim.Allow() {
		l.notify(key, RejectRate)
		return false
	}
	return true
}

// AllowN 判断指定 key 的 n 个请求是否允许.
// n <= 0 时直接返回 true(零请求恒允许,符合 token bucket 直觉).
func (l *Limiter) AllowN(key string, n int) bool {
	if n <= 0 {
		return true
	}
	v := l.getOrCreate(key)
	if v == nil {
		l.notify(key, RejectMaxKeys)
		return false
	}
	v.lastSeen.Store(time.Now().UnixNano())
	if !v.lim.AllowN(time.Now(), n) {
		l.notify(key, RejectRate)
		return false
	}
	return true
}

// Check 判断请求是否允许,并返回信息丰富的 Result(含 Limit / Remaining / RetryAfter / Reason).
//
// 这是**框架无关**的接口契约,供 Web 中间件 / RPC 拦截器 / 任意调度层使用:
//
//	r := lim.Check("user:123")
//	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(r.Limit))
//	w.Header().Set("X-RateLimit-Remaining", strconv.FormatFloat(r.Remaining, 'f', 0, 64))
//	if !r.Allowed {
//	    w.Header().Set("Retry-After", strconv.Itoa(int(r.RetryAfter.Seconds())))
//	    return // 拒绝.
//	}
//
// Check 比 Allow 多一个 Result 构造的开销(数十 ns),热路径仍推荐 Allow.
func (l *Limiter) Check(key string) Result {
	v := l.getOrCreate(key)
	if v == nil {
		l.notify(key, RejectMaxKeys)
		return Result{
			Allowed:    false,
			Limit:      l.burst,
			Remaining:  0,
			RetryAfter: l.retryAfter,
			Reason:     RejectMaxKeys,
		}
	}
	v.lastSeen.Store(time.Now().UnixNano())

	now := time.Now()
	if !v.lim.AllowN(now, 1) {
		l.notify(key, RejectRate)
		return Result{
			Allowed:    false,
			Limit:      l.burst,
			Remaining:  v.lim.Tokens(),
			RetryAfter: l.retryAfter,
			Reason:     RejectRate,
		}
	}

	return Result{
		Allowed:   true,
		Limit:     l.burst,
		Remaining: v.lim.Tokens(),
		ResetAt:   now.Add(time.Second),
	}
}

// Tokens 返回指定 key 当前的近似可用令牌数.
// 若 key 此前从未见过,则返回 burst.
func (l *Limiter) Tokens(key string) float64 {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor).lim.Tokens()
	}
	return float64(l.burst)
}

// Reset 移除指定 key 的限流状态.
// 下一次请求会从满 burst 开始计数.
func (l *Limiter) Reset(key string) {
	if _, loaded := l.visitors.LoadAndDelete(key); loaded {
		l.size.Add(-1)
	}
}

// Close 停止后台清理 goroutine 并等待其退出.
// Close 返回后 Limiter 不应再被使用.
func (l *Limiter) Close() {
	close(l.stopCh)
	l.wg.Wait()
}

// ================== 元信息读取 ==================

// Rate 返回配置的每秒请求数.
func (l *Limiter) Rate() float64 {
	return l.rate
}

// Burst 返回配置的突发容量.
func (l *Limiter) Burst() int {
	return l.burst
}

// Size 返回当前 Limiter 内缓存的 key 数量(便于运维监控 / 告警).
// 注意:cleanup 删除是 lazy 的(每 cleanupInterval 一次),
// Size 反映上一次清理后的状态加上之后新增,可能略高于实际活跃 key 数.
func (l *Limiter) Size() int64 {
	return l.size.Load()
}

// FormatRate 把 Rate 格式化为字符串(便于写响应头).
func (l *Limiter) FormatRate() string {
	return strconv.FormatFloat(l.rate, 'f', -1, 64)
}

// FormatBurst 把 Burst 格式化为字符串(便于写响应头).
func (l *Limiter) FormatBurst() string {
	return strconv.Itoa(l.burst)
}

// ================== Helpers(供框架适配层使用) ==================

// RetryAfterSeconds 根据 rps 计算合理的 Retry-After 秒数.
// rps >= 1 → 1 秒(下一秒一定会补一个令牌);rps < 1 → ceil(1/rps),如 rps=0.1 返回 10.
// rps <= 0(异常)→ 1 秒兜底.
//
// 这是**框架适配层公用**的辅助函数,exported 出来避免每个中间件重新发明.
// 参数命名为 rps 而非 rate,避免遮蔽 golang.org/x/time/rate 包名.
func RetryAfterSeconds(rps float64) int {
	if rps <= 0 || rps >= 1 {
		return 1
	}
	return int(math.Ceil(1.0 / rps))
}

// computeRetryAfter 把 RetryAfterSeconds 的结果转为 time.Duration,在 Check 拒绝路径用.
func computeRetryAfter(rps float64) time.Duration {
	return time.Duration(RetryAfterSeconds(rps)) * time.Second
}

// ================== 内部实现 ==================

// getOrCreate 查找已有 visitor 或原子地创建新实例.
// 返回 nil 表示因 WithMaxKeys 上限被拒.
func (l *Limiter) getOrCreate(key string) *visitor {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor)
	}

	// 上限检查 — 先于实例化,避免无效分配.
	// 注意:maxKeys 边界附近存在轻微竞态(多 goroutine 同时通过 size 检查后才 LoadOrStore),
	// 但偏差有界(最多 N 个并发新建 goroutine),对 DoS 防护够用.
	if l.maxKeys > 0 && l.size.Load() >= int64(l.maxKeys) {
		return nil
	}

	v := &visitor{
		lim: rate.NewLimiter(rate.Limit(l.rate), l.burst),
	}
	// 关键:创建时立即 Store lastSeen,否则 lastSeen=0 < threshold,
	// 在首次 Allow 调用 v.lastSeen.Store 之前若 cleanup goroutine 抢先 Range,
	// 新创建的 visitor 会被立即删除 → 后续请求重新创建,burst 重置,绕过限流.
	v.lastSeen.Store(time.Now().UnixNano())

	if actual, loaded := l.visitors.LoadOrStore(key, v); loaded {
		return actual.(*visitor)
	}
	l.size.Add(1)
	return v
}

// notify 在 onReject 已注册时安全调用回调.
func (l *Limiter) notify(key string, reason RejectReason) {
	if l.onReject != nil {
		l.onReject(key, reason)
	}
}

// cleanupLoop 周期性清理空闲的 visitor.
func (l *Limiter) cleanupLoop() {
	defer l.wg.Done()
	ticker := time.NewTicker(l.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stopCh:
			return
		}
	}
}

// cleanup 移除空闲超过 maxIdleTime 的 visitor.
func (l *Limiter) cleanup() {
	threshold := time.Now().Add(-l.maxIdleTime).UnixNano()

	l.visitors.Range(func(key, value any) bool {
		v := value.(*visitor)
		if v.lastSeen.Load() < threshold {
			if _, loaded := l.visitors.LoadAndDelete(key); loaded {
				l.size.Add(-1)
			}
		}
		return true
	})
}
