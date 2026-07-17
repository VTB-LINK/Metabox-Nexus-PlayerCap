package telemetry

// panic 捕获。设计目标只有一条：**让崩溃照常崩，只是顺便报一声。**
//
// 本文件里每一个反直觉的决定都来自实测（sentry-go v0.48.0，2026-07-17），下面逐条写明。

import (
	"fmt"
	"reflect"
	"time"

	"github.com/getsentry/sentry-go"
)

// flushTimeout 是 re-panic 之前等待事件送达的上限。
//
// 崩溃的进程马上就没了，异步 worker 来不及 POST —— 不 Flush 就等于不报。
// 实测：DSN 指向不可解析的主机（模拟被墙）时 Flush 离线 142ms 返回，不会真的干等 2 秒。
// 未注入 DSN 时 report 直接早退，连 Flush 都不调，零延迟。
const flushTimeout = 2 * time.Second

// guardModule 是本包的导入路径，用于在 BeforeSend 里认出 Guard 自己的栈帧。
//
// 用 reflect 取而不是硬编码字符串：包挪目录、module 改名，它都自动跟上。
var guardModule = reflect.TypeOf(Options{}).PkgPath()

// Guard 捕获 panic → 上报 → Flush → **原样 re-panic**。
//
// 用法（每个 goroutine 入口一行）：
//
//	go func() {
//		defer telemetry.Guard()
//		...
//	}()
//
// # 必须直接 defer，不能包闭包
//
// `defer telemetry.Guard()` ✓
// `defer func() { telemetry.Guard() }()` ✗ —— 多包一层后，Guard 内部的 recover() 返回 nil，
// panic 照常向上跑，**上报静默失效而门禁全绿**。Go 的 recover 只在被 defer 直接调用的函数里有效。
//
// # 为什么不用官方的 defer sentry.Recover()
//
// 它捕获之后**不 re-panic**（源码 + 实测双证）。那会让 goroutine 带着未知的损坏状态继续跑：
// 把错位的歌词持续推上 OBS，而且骗过所有人——进程还活着，没有任何信号说该重启它。
// 本仓库的价值排序是「稳定 > 正确 > 简洁 > 功能完整度」，而「假装没崩」比「崩掉」更坏。
//
// 我们要的是纯增量可观测性：进程该死还是死，exit code、panic 输出、`[recovered, repanicked]`
// 标记、完整栈，一样不少（实测子进程 exit status 2，与不加 Guard 时逐字节同构）。
// 上报逻辑**故意内联在这里**，没有抽成 report() 之类的辅助函数。
//
// 原因是栈：辅助函数会作为一帧出现在 Guard 与 sentry 之间，于是栈的最内层变成那个辅助函数
// 而不是 Guard，trimGuardFrame 的判据就落空了。这不是推测——第一版正是抽了个 report()，
// 端到端测试立刻报出「最内层帧 = report」，一帧都没裁掉。
//
// 给判据补上 report 这个名字也能修，但那样每加一个中间函数就要再补一次，且漏了不会有人发现。
// 保持这里只有一帧，判据就永远只需要认一个名字。
func Guard() {
	r := recover()
	if r == nil {
		return
	}

	if Enabled() { // 未注入 DSN：零开销，不 Flush、不等待，直接抛回
		err, ok := r.(error)
		if !ok {
			// **非 error 的 panic 值必须包一层，否则上报里根本没有 Exception。**
			// 实测 sentry-go v0.48.0：panic("字符串") 与 panic(42) 产生的 event 只有一行
			// Message，Exception 整个为空——没有 type、没有 value、**没有栈**，等于没报。
			//
			// 最常见的那类天然安全：nil 解引用、数组越界、nil map 写入，它们的 panic 值是
			// runtime.Error，本身就实现了 error（实测 nil map 写入 → runtime.plainError
			// "assignment to entry in nil map"，栈完整）。这里兜的是代码里裸写 panic("...")。
			//
			// 注意包装**只用于上报**：下面 re-panic 抛的仍是原始的 r，见 panicrepanic_test.go。
			err = fmt.Errorf("%v", r)
		}
		sentry.CurrentHub().Recover(err)
		sentry.Flush(flushTimeout)
	}

	panic(r) // 原样 re-panic —— 保持「进程死」的语义分毫不变
}

// trimGuardFrame 裁掉栈最内层的 Guard 帧。
//
// # 为什么这帧非裁不可
//
// sentry-go 算栈时只过滤**它自己包内**的帧。官方的 sentry.Recover() 因此能让最内层正好落在
// panic 现场；而任何住在我们包里的 recover 函数——闭包、具名函数，两种都实测过——最内层永远
// 是它自己。**这一帧靠调用形状消不掉**，只能在事件出门前裁。
//
// 实测对比（探针用 BeforeSend 拦截 event 读到的最内层帧）：
//
//	defer sentry.Recover()              → level2        ← panic 现场
//	defer func(){ recover(); ... }()    → xxx.func1     ← 闭包自己
//	defer guard()                       → guard         ← 具名函数自己
//
// 代价不只是难看：Sentry 的 issue 分组吃 stacktrace，最内层帧恒定意味着**不同位置的 panic 有
// 塌进同一个 issue 的风险**——那样遥测就废了一半，我们看得到「崩了」却看不出「哪崩的」。
// 分组算法在客户端无法验证（要真 DSN + 真 event），所以不赌，直接把噪音源掐掉。
func trimGuardFrame(event *sentry.Event) {
	for i := range event.Exception {
		st := event.Exception[i].Stacktrace
		if st == nil || len(st.Frames) == 0 {
			continue
		}
		last := st.Frames[len(st.Frames)-1]
		if last.Module == guardModule && last.Function == "Guard" {
			st.Frames = st.Frames[:len(st.Frames)-1]
		}
	}
}
