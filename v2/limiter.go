// Package golimit 提供生产级、per-key 的限流能力,
// 基于 golang.org/x/time/rate(令牌桶算法).
//
// 设计原则:**零第三方框架依赖**.本包仅依赖 stdlib + golang.org/x/time.
// Web 框架集成(Gin / echo / chi 等)以文档示例形式提供(见仓库 docs/gin.md),
// 基于框架无关的 Check + Result 契约,用户复制即用,不引入任何框架依赖.
//
// v2 重设计:Limiter 改为实例化对象(不再是全局单例),通过 Close() 优雅关闭.
package golimit

import (
	"context"
	"errors"
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

// ErrMaxKeys 是 Wait 在 key 基数达到 WithMaxKeys 上限、无法为新 key 创建限流器时
// 返回的错误.可用 errors.Is(err, ErrMaxKeys) 判定,以区分"键基数被拒"与 ctx 取消.
var ErrMaxKeys = errors.New("golimit: max keys limit reached")

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
	// createMu 仅串行化"新 key 创建"路径,使 WithMaxKeys 成为硬上限;已有 key 走无锁 Load.
	createMu  sync.Mutex
	stopCh    chan struct{}
	closeOnce sync.Once // 保证 Close 幂等,重复调用不 panic.
	wg        sync.WaitGroup

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
	// activeWaits 是当前阻塞在 Wait/WaitN 中的调用数,原子更新.
	// >0 时 cleanup 必须跳过该 key:等待时长可能超过 maxIdleTime,若被回收,
	// 同 key 的新请求会重建满桶,绕过"同 key 平滑整流"语义.
	activeWaits atomic.Int64
}

// ================== 构造 ==================

// maxBurst 是 burst 的内部上限,防止极大 rps 推导出溢出 int 或荒谬的 burst.
const maxBurst = 1 << 30 // ~10.7 亿,远超任何真实限流场景.

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
//   - rps 非有限(NaN / ±Inf)或 <= 0 → 100
//   - burst <= 0 → max(1, ceil(rps))
//   - cleanupInterval <= 0 → 1 分钟(避免 time.NewTicker(0) panic)
//   - maxIdleTime <= 0 → 5 分钟
//   - maxKeys < 0 → 0(等价于无上限)
func New(rps float64, opts ...Option) *Limiter {
	// 非有限(NaN / ±Inf)或非正数都回退默认,保证 New 永不 panic / 不进入异常状态.
	if math.IsNaN(rps) || math.IsInf(rps, 0) || rps <= 0 {
		rps = 100
	}

	// burst 至少为 1(避免 rps<1 时 int=0 全拒);对极大 rps 封顶,避免 int 溢出 / 荒谬 burst.
	ceil := math.Ceil(rps)
	if ceil > maxBurst {
		ceil = maxBurst
	}
	defaultBurst := max(1, int(ceil))

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
	now := time.Now()
	v.lastSeen.Store(now.UnixNano())
	if !v.lim.AllowN(now, 1) {
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
	now := time.Now()
	v.lastSeen.Store(now.UnixNano())
	if !v.lim.AllowN(now, n) {
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
	now := time.Now()
	v.lastSeen.Store(now.UnixNano())
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

	remaining := v.lim.Tokens()
	return Result{
		Allowed:   true,
		Limit:     l.burst,
		Remaining: remaining,
		ResetAt:   now.Add(l.refillDuration(remaining)),
	}
}

// Wait 阻塞直到 key 的限流器放行 1 个请求,或 ctx 被取消 / 超时.
//
// 这是 Allow 的"整流"对应物:Allow 超额立即拒绝(返回 false),Wait 超额则排队等待——
// 适合主动调用下游(API / DB / MQ / 第三方接口)时把发送速率平滑摊开,而非直接丢弃请求.
// ctx 取消 / 超时可随时中断等待.
//
// 返回值:
//   - nil:已获得令牌,可继续.
//   - ErrMaxKeys:设置了 WithMaxKeys 且已达上限,新 key 被拒(不阻塞,立即返回).
//   - 其他非 nil:未获得令牌 —— ctx 在等待中被取消/超时(context.Canceled /
//     context.DeadlineExceeded),或所需等待时间已超出 ctx deadline 时底层 rate 包
//     提前返回的错误(不会傻等到 deadline).可用 errors.Is 判定具体类型.
//
// 在途等待期间,cleanup 不会回收本 key(activeWaits 计数在 createMu 锁内维护、与 cleanup
// 删除互斥),保证"同 key 平滑整流"不被自动清理打断.因 Wait 是"等待而非拒绝"语义,令牌
// 不足不触发 WithOnReject 回调;仅 ErrMaxKeys 这种硬拒绝会触发(reason=RejectMaxKeys).
func (l *Limiter) Wait(ctx context.Context, key string) error {
	v := l.acquireForWait(key) // 锁内 activeWaits++,cleanup 不会再回收本 key.
	if v == nil {
		l.notify(key, RejectMaxKeys)
		return ErrMaxKeys
	}
	defer l.releaseWait(v)
	v.lastSeen.Store(time.Now().UnixNano())
	return v.lim.Wait(ctx) // 注意:不持锁阻塞.
}

// WaitN 阻塞直到 key 的限流器放行 n 个请求,或 ctx 被取消 / 超时.语义同 Wait.
// n <= 0 时立即返回 nil(不阻塞).
//
// 注意:n 超过配置的 burst 时,底层 rate 包会立即返回错误(再久也放行不了 n 个),
// 不会阻塞——调用方应保证 n <= burst.
func (l *Limiter) WaitN(ctx context.Context, key string, n int) error {
	if n <= 0 {
		return nil
	}
	v := l.acquireForWait(key) // 锁内 activeWaits++,cleanup 不会再回收本 key.
	if v == nil {
		l.notify(key, RejectMaxKeys)
		return ErrMaxKeys
	}
	defer l.releaseWait(v)
	v.lastSeen.Store(time.Now().UnixNano())
	return v.lim.WaitN(ctx, n) // 注意:不持锁阻塞.
}

// Tokens 返回指定 key 当前的近似可用令牌数.
// 若 key 此前从未见过,则返回 burst.
func (l *Limiter) Tokens(key string) float64 {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor).lim.Tokens()
	}
	return float64(l.burst)
}

// Reset 移除指定 key 的限流状态.下一次请求会从满 burst 开始计数.
//
// 走 createMu,与创建 / cleanup 在 size 维护上互斥,避免短暂计数不一致.
// 注:Reset 是显式重置,即使该 key 有在途 Wait 也会清除(显式操作语义高于 cleanup 的自动保护).
func (l *Limiter) Reset(key string) {
	l.createMu.Lock()
	defer l.createMu.Unlock()
	if _, loaded := l.visitors.LoadAndDelete(key); loaded {
		l.size.Add(-1)
	}
}

// Close 停止后台清理 goroutine 并等待其退出.
// Close 返回后 Limiter 不应再被使用.
func (l *Limiter) Close() {
	l.closeOnce.Do(func() {
		close(l.stopCh)
	})
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

// maxRetryAfterSeconds 是 Retry-After 的内部上限(1 天).
//  1. 防止极小正 rps 让 ceil(1/rps) 溢出 int —— float→int 越界在 Go 里是实现相关的
//     (amd64 给 MinInt64,会得到负的 Retry-After);
//  2. 超过一天的 Retry-After 对客户端已无实际意义.
const maxRetryAfterSeconds = 24 * 60 * 60

// RetryAfterSeconds 根据 rps 计算合理的 Retry-After 秒数,返回值恒为正整数.
// rps >= 1(含 +Inf)→ 1 秒(下一秒一定会补一个令牌);rps < 1 → ceil(1/rps),如 rps=0.1 返回 10;
// rps <= 0 / NaN(异常)→ 1 秒兜底;极小正 rps 致 ceil 超上限 → 钳制为 maxRetryAfterSeconds.
//
// 这是**框架适配层公用**的辅助函数,exported 出来避免每个中间件重新发明.
// 参数命名为 rps 而非 rate,避免遮蔽 golang.org/x/time/rate 包名.
func RetryAfterSeconds(rps float64) int {
	// NaN 的所有比较都为 false,必须显式拦,否则会落到下面 int(Ceil) 触发越界负值.
	if math.IsNaN(rps) || rps <= 0 || rps >= 1 {
		return 1
	}
	secs := math.Ceil(1.0 / rps)
	if secs > maxRetryAfterSeconds {
		return maxRetryAfterSeconds
	}
	return int(secs)
}

// computeRetryAfter 把 RetryAfterSeconds 的结果转为 time.Duration,在 Check 拒绝路径用.
func computeRetryAfter(rps float64) time.Duration {
	return time.Duration(RetryAfterSeconds(rps)) * time.Second
}

// refillDuration 估算桶从当前剩余令牌补满到 burst 所需时间,用于 Result.ResetAt.
// rate<=0 或已满时返回 0.
func (l *Limiter) refillDuration(remaining float64) time.Duration {
	if l.rate <= 0 {
		return 0
	}
	deficit := float64(l.burst) - remaining
	if deficit <= 0 {
		return 0
	}
	secs := deficit / l.rate
	// 钳制:极小 rate 会让 secs 巨大,float→Duration(int64 纳秒)越界会回绕成负值
	// (ResetAt 落到过去).复用 Retry-After 的 1 天上限.
	if secs > maxRetryAfterSeconds {
		secs = maxRetryAfterSeconds
	}
	return time.Duration(secs * float64(time.Second))
}

// ================== 内部实现 ==================

// getOrCreate 查找已有 visitor 或创建新实例.返回 nil 表示因 WithMaxKeys 上限被拒.
//
// 已有 key 走无锁 sync.Map.Load 快路径;仅"新 key 创建"经 createMu 串行,把 maxKeys
// 检查 + size 自增 + Store 收进同一临界区,使 WithMaxKeys 成为**硬上限**(而非
// check-then-act 竞态下可被并发冲破的软上限).
func (l *Limiter) getOrCreate(key string) *visitor {
	// 快路径:已有 key,无锁.
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor)
	}
	// 慢路径:新 key 创建串行化.
	l.createMu.Lock()
	defer l.createMu.Unlock()
	return l.getOrCreateLocked(key)
}

// getOrCreateLocked 假定调用方已持有 createMu.查找已有 visitor 或创建新实例,把 maxKeys
// 检查 + Store + size.Add 收进锁内,使 WithMaxKeys 成为硬上限.返回 nil 表示因上限被拒.
//
// 抽出"已持锁"版本是为了让 getOrCreate(快路径无锁)与 acquireForWait(全程持锁)复用同一
// 创建逻辑,又不会因重复 Lock 同一把不可重入的 createMu 而死锁.
func (l *Limiter) getOrCreateLocked(key string) *visitor {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor)
	}
	if l.maxKeys > 0 && l.size.Load() >= int64(l.maxKeys) {
		return nil
	}
	v := &visitor{
		lim: rate.NewLimiter(rate.Limit(l.rate), l.burst),
	}
	// 创建时立即 Store lastSeen,否则 lastSeen=0 < threshold 会被 cleanup 误删.
	v.lastSeen.Store(time.Now().UnixNano())
	l.visitors.Store(key, v)
	l.size.Add(1)
	return v
}

// acquireForWait 供 Wait / WaitN 使用:在 createMu 锁内获取/创建 visitor 并 activeWaits++.
// 把"标记在途等待"与 cleanup 的删除收进同一把锁,杜绝 check-then-delete 竞态——cleanup
// 锁内会二次确认 activeWaits==0,故此处 ++ 之后它绝不会回收该 key.返回 nil 表示因上限被拒.
func (l *Limiter) acquireForWait(key string) *visitor {
	l.createMu.Lock()
	defer l.createMu.Unlock()
	v := l.getOrCreateLocked(key)
	if v != nil {
		v.activeWaits.Add(1)
	}
	return v
}

// releaseWait 在 Wait / WaitN 结束时调用:先刷新 lastSeen 再减 activeWaits.
// 顺序关键 —— 任何观察到 activeWaits==0 的时刻 lastSeen 都已刷新为 now,cleanup 的
// "lastSeen 过期"二次确认因此不会误删一个刚结束等待的 key.无需持锁(均为原子操作).
func (l *Limiter) releaseWait(v *visitor) {
	v.lastSeen.Store(time.Now().UnixNano())
	v.activeWaits.Add(-1)
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

// cleanup 移除空闲超过 maxIdleTime 的 visitor.有在途 Wait/WaitN 的 key 一律不回收
// (等待可能长于 maxIdleTime,删除会让同 key 新请求重建满桶、绕过整流).
func (l *Limiter) cleanup() {
	threshold := time.Now().Add(-l.maxIdleTime).UnixNano()

	l.visitors.Range(func(key, value any) bool {
		v := value.(*visitor)
		// 无锁预筛:绝大多数 key 不满足回收条件,直接跳过,避免无谓加锁.
		if v.activeWaits.Load() != 0 || v.lastSeen.Load() >= threshold {
			return true
		}
		// 候选回收:进 createMu 二次确认后再删,与 acquireForWait / getOrCreate 互斥.
		// 三个条件缺一不可:
		//   - 当前 map value 仍是同一个 v(否则会误删刚被 Reset + 重建的新 visitor);
		//   - activeWaits 仍为 0(否则有 Wait 在预筛后、加锁前进来了);
		//   - lastSeen 仍过期(否则有请求 / Wait 结束刚刷新了它).
		l.createMu.Lock()
		if cur, ok := l.visitors.Load(key); ok && cur == value &&
			v.activeWaits.Load() == 0 && v.lastSeen.Load() < threshold {
			l.visitors.Delete(key)
			l.size.Add(-1)
		}
		l.createMu.Unlock()
		return true
	})
}
