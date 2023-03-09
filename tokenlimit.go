package golimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limiters struct {
	limiters *sync.Map
}

type Limiter struct {
	limiter *rate.Limiter
	lastGet time.Time // 上一次获取token的时间
	key     string
	r       rate.Limit
	b       int
}

var GlobalLimiters = &Limiters{
	limiters: &sync.Map{},
}

var once = sync.Once{}

func Allow(param string, num int) bool {
	return NewLimiter(param, num).allow()
}

func NewLimiter(key string, b int) *Limiter {

	once.Do(func() {

		go GlobalLimiters.clearLimiter()
	})

	keyLimiter := GlobalLimiters.getLimiter(key, b)

	return keyLimiter

}

func (l *Limiter) allow() bool {

	l.lastGet = time.Now()

	return l.limiter.Allow()

}

func (ls *Limiters) getLimiter(key string, b int) *Limiter {

	limiter, ok := ls.limiters.Load(key)

	if ok {

		return limiter.(*Limiter)
	}

	l := &Limiter{
		// 实例化一个限流器，桶的容量是1，每秒生成一个令牌
		// 1.第一个参数是 r Limit。代表每秒可以向 Token 桶中产生多少 token。Limit 实际上是 float64 的别名
		// 2.第二个参数是 b int。b 代表 初始并发量,看做是桶的容量。
		limiter: rate.NewLimiter(rate.Every(1*time.Second), b),
		lastGet: time.Now(),
		key:     key,
	}

	ls.limiters.Store(key, l)

	return l
}

// 清除过期的限流器
func (ls *Limiters) clearLimiter() {

	for {

		time.Sleep(1 * time.Minute)

		ls.limiters.Range(func(key, value interface{}) bool {
			// 超过5分钟
			if time.Now().Unix()-value.(*Limiter).lastGet.Unix() > 5*60 {

				ls.limiters.Delete(key)
			}

			return true
		})

	}

}
