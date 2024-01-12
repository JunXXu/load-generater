package loadgenerater

import (
	"load-generater/lib"
	"time"
)

// ParamSet 代表了载荷发生器参数的集合。
type ParamSet struct {
	Caller lib.Caller // 调用器。

	TimeoutNS  time.Duration // 响应超时时间，单位：纳秒。
	LPS        uint32        // 每秒载荷量。
	DurationNS time.Duration // 负载持续时间，单位：纳秒。

	// 这里用指针的好处：方便零值判断、减少元素值复制带来的开销
	ResultCh chan *lib.CallResult // 调用结果通道，需要用并发安全的数据结构
}
