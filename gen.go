package loadgenerater

import (
	"bytes"
	"context"
	"fmt"
	"load-generater/helper/log"
	"load-generater/lib"
	"math"
	"sync/atomic"
	"time"
)

// 日志记录器。
var logger = log.DLogger()

// myGenerator 代表载荷发生器的实现类型。
type myGenerator struct {
	caller lib.Caller // 调用器。

	// 载荷参数
	timeoutNS  time.Duration // 处理超时时间，单位：纳秒。
	lps        uint32        // 每秒载荷量。
	durationNS time.Duration // 负载持续时间，单位：纳秒。

	// 控制作用的字段
	concurrency uint32             // 载荷并发量，用于约束tickets票数。并发量 ～= 单个载荷的响应超时时间 / 载荷的发送间隔时间 = timeoutNS /（1e9 / lps）+ 1
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
	if err := pset.Check(); err != nil {
		return nil, err
	}
	gen := &myGenerator{
		caller:     pset.Caller,
		timeoutNS:  pset.TimeoutNS,
		lps:        pset.LPS,
		durationNS: pset.DurationNS,
		status:     lib.STATUS_ORIGINAL,
		resultCh:   pset.ResultCh,
	}
	if err := gen.init(); err != nil {
		return nil, err
	}
	return gen, nil
}

// 初始化载荷发生器。计算并发量、初始化票池
func (gen *myGenerator) init() error {
	var buf bytes.Buffer
	buf.WriteString("Initializing the load generator...")
	// 载荷的并发量 ≈ 载荷的响应超时时间 / 载荷的发送间隔时间
	var total64 = int64(gen.timeoutNS)/int64(1e9/gen.lps) + 1
	if total64 > math.MaxInt32 {
		total64 = math.MaxInt32
	}
	gen.concurrency = uint32(total64)
	tickets, err := lib.NewGoTickets(gen.concurrency)
	if err != nil {
		return err
	}
	gen.tickets = tickets

	buf.WriteString(fmt.Sprintf("Done. (concurrency=%d)", gen.concurrency))
	logger.Infoln(buf.String())
	return nil
}

// 异步调用承受方接口
func (gen *myGenerator) asyncCall() {
	gen.tickets.Take()
	go func() {
		defer func() {
			gen.tickets.Return()
		}()
		// 构造请求
		rawReq := gen.caller.BuildReq()
		// 发送请求，加入超时判断，用time.afterfuncl来修改状态，从而避免使用更多的goroutine实现
		var callStatus uint32
		timer := time.AfterFunc(gen.timeoutNS, func() {
			if !atomic.CompareAndSwapUint32(&callStatus, 0, 2) {
				return
			}
			// 构造超时请求结果
			result := &lib.CallResult{
				ID:     rawReq.ID,
				Req:    rawReq,
				Code:   lib.RET_CODE_WARNING_CALL_TIMEOUT,
				Msg:    fmt.Sprintf("Timeout! (expected: < %v)", gen.timeoutNS),
				Elapse: gen.timeoutNS,
			}
			gen.sendResult(result)
		})
		rawResp := gen.callOne()
		timer.Stop()
		if !atomic.CompareAndSwapUint32(&callStatus, 0, 1) {
			return
		}
		var result *lib.CallResult
		if rawResp.Err != nil {
			result = &lib.CallResult{
				ID:     rawResp.ID,
				Req:    rawReq,
				Code:   lib.RET_CODE_ERROR_CALL,
				Msg:    rawResp.Err.Error(),
				Elapse: rawResp.Elapse}
		} else {
			result = gen.caller.CheckResp(rawReq, *rawResp)
			result.Elapse = rawResp.Elapse
		}
		gen.sendResult(result)
	}()
}

// 用于为停止载荷发生器做准备。
func (gen *myGenerator) prepareToStop(ctxError error) {
	logger.Infof("Prepare to stop load generator (cause: %s)...", ctxError)
	// 停止时进行两次变更状态
	atomic.CompareAndSwapUint32(
		&gen.status, lib.STATUS_STARTED, lib.STATUS_STOPPING)
	logger.Infof("Closing result channel...")
	close(gen.resultCh)
	atomic.StoreUint32(&gen.status, lib.STATUS_STOPPED)
}

// 生成载荷并向生产方发送
func (gen *myGenerator) genload(throttle <-chan time.Time) {
	for {
		select {
		case <-gen.ctx.Done():
			gen.prepareToStop(gen.ctx.Err())
			return
		default:
		}
		gen.asyncCall()
		select {
		case <-throttle:
		case <-gen.ctx.Done():
			gen.prepareToStop(gen.ctx.Err())
			return
		}
	}
}

// 启动载荷发生器
func (gen *myGenerator) Start() bool {
	logger.Infoln("Starting load generator...")

	// 检查是否具备可启动的状态，顺便设置状态为正在启动
	if !atomic.CompareAndSwapUint32(
		&gen.status, lib.STATUS_ORIGINAL, lib.STATUS_STARTING) {
		if !atomic.CompareAndSwapUint32(
			&gen.status, lib.STATUS_STOPPED, lib.STATUS_STARTING) {
			return false
		}
	}

	// 通过每秒发送载荷数设定节流阀
	var throttle <-chan time.Time
	if gen.lps > 0 {
		interval := time.Duration(1e9 / gen.lps)
		logger.Infof("Setting throttle (%v)...", interval)
		throttle = time.Tick(interval)
	}

	// 初始化上下文用于中断goroutine
	gen.ctx, gen.cancelFunc = context.WithTimeout(context.Background(), gen.durationNS)

	// 初始化计数器
	gen.callCount = 0

	// 初始化发生器状态，用原子操作
	atomic.StoreUint32(&gen.status, lib.STATUS_STARTED)

	// 生成并发生载荷
	go func() {
		logger.Info("Generating loads...")
		gen.genload(throttle)
		logger.Infof("Stopped. (call count: %d)", gen.callCount)
	}()

	return true
}
