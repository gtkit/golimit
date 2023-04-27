// @Author xiaozhaofu 2023/4/21 14:23:00
package golimit

import (
	"sync"
	"testing"
	"time"
)

func TestNewLimiter(t *testing.T) {
	var wg sync.WaitGroup
	l := NewLimiter("127.0.0.1", 3)
	wg.Add(500)
	n := time.Now()
	for i := 0; i < 500; i++ {
		go func() {
			b := l.Allow()
			if b {
				t.Log("------b:", b)
			}

			wg.Done()
		}()

	}

	sub := time.Now().Sub(n)
	t.Log("------sub:", sub)
	wg.Wait()
	t.Log("------end------")

}
