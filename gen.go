package loadgenerater

import (
	"bytes"
	"context"
	"errors"
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

// 打印被忽略的结果
func (gen *myGenerator) printIgnoredResult(result *lib.CallResult, cause string) {
	resultMsg := fmt.Sprintf(
		"ID=%d, Code=%d, Msg=%s, Elapse=%v",
		result.ID, result.Code, result.Msg, result.Elapse)
	logger.Warnf("Ignored result: %s. (cause: %s)\n", resultMsg, cause)
}

// 发送调用结果
func (gen *myGenerator) sendResult(result *lib.CallResult) bool {
	if atomic.LoadUint32(&gen.status) != lib.STATUS_STARTED {
		gen.printIgnoredResult(result, "stopped load generator")
		return false
	}
	select {
	case gen.resultCh <- result:
		return true
	default:
		gen.printIgnoredResult(result, "result channel full")
		return false
	}
}

// 向载荷承受方发起一次调用
func (gen *myGenerator) callOne(rawReq *lib.RawReq) *lib.RawResp {
	// 调用次数加1
	atomic.AddInt64(&gen.callCount, 1)

	// 合法性检查
	if rawReq == nil {
		return &lib.RawResp{Err: fmt.Errorf("invalid raw request"), ID: -1}
	}

	// 计时并调用
	start := time.Now().UnixNano()
	resp, err := gen.caller.Call(rawReq.Req, gen.timeoutNS)
	end := time.Now().UnixNano()
	elapsedTime := time.Duration(end - start)
	var rawResp lib.RawResp
	if err != nil {
		errMsg := fmt.Sprintf("Sync Call Error: %s.", err)
		rawResp = lib.RawResp{Err: errors.New(errMsg), Elapse: elapsedTime, ID: rawReq.ID}
	} else {
		rawResp = lib.RawResp{Resp: resp, Elapse: elapsedTime, ID: rawReq.ID}
	}
	return &rawResp
}

// 异步调用承受方接口
func (gen *myGenerator) asyncCall() {
	gen.tickets.Take()
	go func() {
		defer func() {
			// 捕获来自调用方的panic
			if p := recover(); p != nil {
				err, ok := interface{}(p).(error)
				var errMsg string
				if ok {
					errMsg = fmt.Sprintf("Async Call Panic! (error: %s)", err)
				} else {
					errMsg = fmt.Sprintf("Async Call Panic! (clue: %#v)", p)
				}
				logger.Errorln(errMsg)
				// 调用异常结果也要收集
				result := &lib.CallResult{
					ID:   -1,
					Code: lib.RET_CODE_FATAL_CALL,
					Msg:  errMsg}
				gen.sendResult(result)
			}

			gen.tickets.Return()
		}()

		// 构造请求
		rawReq := gen.caller.BuildReq()

		// 发送请求并接收响应，加入超时判断，用time.afterfuncl来修改标识符状态实现，从而避免使用更多的goroutine
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
		rawResp := gen.callOne(&rawReq)
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
		// 由于select是随机的，在开头检查保证能及时停止
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

	// 初始化上下文用于中断goroutine。代替自己设置定时器，保证不会有其他程序向该通道发送元素，保证信号有效性
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

func (gen *myGenerator) Stop() bool {
	// 检查防止重复停止
	if !atomic.CompareAndSwapUint32(&gen.status, lib.STATUS_STARTED, lib.STATUS_STARTING) {
		return false
	}

	// 发送停止信号
	gen.cancelFunc()

	// 不断检查是否停止
	for {
		if atomic.LoadUint32(&gen.status) == lib.STATUS_STOPPED {
			break
		}
		time.Sleep(time.Microsecond)
	}

	return true
}

func (gen *myGenerator) Status() uint32 {
	return atomic.LoadUint32(&gen.status)
}

func (gen *myGenerator) CallCount() int64 {
	return atomic.LoadInt64(&gen.callCount)
}
