package player

import (
	"strings"
	"sync"
	"sync/atomic"
)

// RUMFlusher 由维护活动会话 RUM 状态的播放器可选实现，供主进程收到退出信号时在
// emitAppExit 与 rum.Shutdown 之前同步结算活动会话与待报曲目。
// 实现必须并发安全且幂等，且绝不在持锁期间调用 rum.Track。
type RUMFlusher interface {
	FlushRUM()
}

// SessionRUMGate 协调单个播放器或捕获器的 RUM 会话登记与退出 FlushRUM 结算：
//   - Flush 置位 flushed 后，所有新的 Register 立即返回 (zero, false)，杜绝关停后产生孤儿事件；
//   - Register 持锁期间仅执行纯内存构造与登记（严禁网络/CDP I/O 或调用 rum.Track），解锁后在锁外通过
//     attachDone 完成屏障执行 onAttached；
//   - End 与 Flush 均在锁外先等待 attachDone，再执行结算回调（内部带 endDone 完成屏障），最后清理槽位。
type SessionRUMGate[T comparable] struct {
	mu         sync.Mutex
	flushed    bool
	cur        T
	hasCur     bool
	attachDone chan struct{}
}

// Register 在未进入退出结算时纯内存构造并登记会话，随后在锁外执行 onAttached 并关闭 attachDone 屏障；
// 若 Flush 已被调用则拒绝登记，直接返回 (zero, false) 且不调用 makeSess 或 onAttached。
func (g *SessionRUMGate[T]) Register(makeSess func() T, onAttached func(T)) (T, bool) {
	var zero T
	g.mu.Lock()
	if g.flushed {
		g.mu.Unlock()
		return zero, false
	}
	sess := makeSess()
	attachDone := make(chan struct{})
	g.cur = sess
	g.hasCur = true
	g.attachDone = attachDone
	g.mu.Unlock()

	defer close(attachDone)
	if onAttached != nil {
		onAttached(sess)
	}
	return sess, true
}

// Current 返回当前登记的活动会话（若有）。
func (g *SessionRUMGate[T]) Current() (T, bool) {
	g.mu.Lock()
	cur, ok := g.cur, g.hasCur
	g.mu.Unlock()
	return cur, ok
}

// End 在会话结束时等待对应 attachDone 完成，再执行 endFn 结算，完成后从当前槽位清除该会话。
func (g *SessionRUMGate[T]) End(sess T, endFn func(T)) {
	g.mu.Lock()
	var attachDone chan struct{}
	if g.hasCur && g.cur == sess {
		attachDone = g.attachDone
	}
	g.mu.Unlock()

	if attachDone != nil {
		<-attachDone
	}
	if endFn != nil {
		endFn(sess)
	}

	g.mu.Lock()
	if g.hasCur && g.cur == sess {
		var zero T
		g.cur = zero
		g.hasCur = false
		g.attachDone = nil
	}
	g.mu.Unlock()
}

// Flush 置位退出闸门拒绝后续新登记，并在锁外等待活动会话的 attachDone 后调用 flushFn 完成结算与屏障等待。
func (g *SessionRUMGate[T]) Flush(flushFn func(T)) {
	g.mu.Lock()
	g.flushed = true
	cur := g.cur
	hasCur := g.hasCur
	attachDone := g.attachDone
	g.mu.Unlock()

	if !hasCur {
		return
	}
	if attachDone != nil {
		<-attachDone
	}
	if flushFn != nil {
		flushFn(cur)
	}

	g.mu.Lock()
	if g.hasCur && g.cur == cur {
		var zero T
		g.cur = zero
		g.hasCur = false
		g.attachDone = nil
	}
	g.mu.Unlock()
}

// CoverOutcome 描述封面下载与 800×800 规范化的脱敏处理结果（不含封面 URL 或错误原文）。
type CoverOutcome struct {
	Action         string
	FallbackReason string
	DurationMs     int64
}

// CoverOutcome.Action 的稳定枚举取值。
const (
	CoverActionPassthrough800 = "passthrough_800"
	CoverActionResized800     = "resized_800"
	CoverActionFallbackRaw    = "fallback_raw"
	CoverActionNone           = "none"
	CoverActionErr            = "err"
)

var tracksRecognizedTotal atomic.Uint64

// NoteTrackRecognized 在各播放器每首新歌首轮取词落定发出 track_recognized 时递增全局识曲计数。
func NoteTrackRecognized() {
	tracksRecognizedTotal.Add(1)
}

// TracksRecognizedTotal 返回本进程启动以来已结算识曲的总曲目数，供心跳增量与退出汇总读取。
func TracksRecognizedTotal() uint64 {
	return tracksRecognizedTotal.Load()
}

// SanitizeVersion 清洗上报给 RUM 的播放器版本号：仅保留 ASCII 字母、数字、点、横线与下划线，
// 最长 32 字符，避免异常自由文本或本地路径片段进入事件属性。
func SanitizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) > 32 {
		return "other"
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			c == '.' || c == '-' || c == '_'
		if !ok {
			return "other"
		}
	}
	return v
}

// CombineSupplementDecisions 合并歌名与歌手等多字段的 SupplementDecision 结果为曲目级决策枚举。
func CombineSupplementDecisions(decisions ...string) string {
	best := ""
	rank := func(d string) int {
		switch d {
		case "overwritten":
			return 4
		case "filled_empty":
			return 3
		case "kept":
			return 2
		case "unchanged":
			return 1
		default:
			return 0
		}
	}
	for _, d := range decisions {
		if rank(d) > rank(best) {
			best = d
		}
	}
	return best
}
