package golimit

import "time"

// Option 用于配置 Limiter.
type Option func(*Limiter)

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
