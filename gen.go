package loadgenerater

import (
	"context"
	"load-generater/lib"
	"time"
)

// myGenerator 代表载荷发生器的实现类型。
type myGenerator struct {
	caller lib.Caller // 调用器。

	// 载荷参数
	timeoutNS  time.Duration // 处理超时时间，单位：纳秒。
	lps        uint32        // 每秒载荷量。
	durationNS time.Duration // 负载持续时间，单位：纳秒。

	// 控制作用的字段
	concurrency uint32             // 载荷并发量。并发量 ～= 单个载荷的响应超时时间 / 载荷的发送间隔时间
	tickets     lib.GoTickets      // Goroutine票池。用于控制goroutine数量
	ctx         context.Context    // 上下文。
	cancelFunc  context.CancelFunc // 取消函数。

	// 发生器状态
	callCount int64                // 调用计数。
	status    uint32               // 状态。非负的短小数值。uint32是原子操作支持的数值，保证并发安全。
	resultCh  chan *lib.CallResult // 调用结果通道。
}

// NewGenerator 会新建一个载荷发生器。面向接口编程，避免直接返回myGenerator*
func NewGenerator(pset ParamSet) (lib.Generator, error) {
	gen := &myGenerator{
		caller:     pset.Caller,
		timeoutNS:  pset.TimeoutNS,
		lps:        pset.LPS,
		durationNS: pset.DurationNS,
		status:     lib.STATUS_ORIGINAL,
		resultCh:   pset.ResultCh,
	}
	return gen, nil
}
