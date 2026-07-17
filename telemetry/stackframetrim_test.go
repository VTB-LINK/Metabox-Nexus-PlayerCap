package telemetry

// 本文件只测一件事：**Guard 自己那一帧被裁掉，且只裁它**。
// re-panic 的语义在 panicrepanic_test.go。
//
// 背景（实测，sentry-go v0.48.0）：sentry 算栈时只过滤它自己包内的帧，所以官方
// `defer sentry.Recover()` 的最内层正好是 panic 现场，而我们的 Guard 无论写成闭包还是具名
// 函数，最内层永远是它自己。这一帧靠调用形状消不掉，只能在事件出门前裁。
//
// 它不只是难看：Sentry 的 issue 分组吃 stacktrace，最内层恒定意味着不同位置的 panic 有塌进
// 同一个 issue 的风险——「崩了」看得见，「哪崩的」看不见。分组算法在客户端验证不了，所以不赌。

import (
	"errors"
	"testing"

	"github.com/getsentry/sentry-go"
)

// frames 造一个栈（sentry 的 Frames 由外向内排，最后一个是最内层）。
func frames(fs ...sentry.Frame) *sentry.Event {
	return &sentry.Event{Exception: []sentry.Exception{{
		Stacktrace: &sentry.Stacktrace{Frames: fs},
	}}}
}

// TestInitWiresBeforeSendTrim 钉死**接线**：Init 真的把裁帧挂到了 sentry 的 BeforeSend 上。
//
// 这条比单测 trimGuardFrame 值钱得多——纯函数正确但没挂上去，等于没做，而且四道门禁全绿。
// 手法：Init 之后取 sentry **实际生效**的 BeforeSend，喂给它一个带 Guard 帧的事件，看它裁没裁。
//
// 变异自证：删掉 Init 里的 BeforeSend 字段即红；把 trimGuardFrame 改成空函数也红。
func TestInitWiresBeforeSendTrim(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	Init(Options{Version: "9.9.9", IsRelease: true})

	client := sentry.CurrentHub().Client()
	if client == nil {
		t.Fatal("Init 之后没有 client")
	}
	before := client.Options().BeforeSend
	if before == nil {
		t.Fatal("BeforeSend 没挂上 —— Guard 帧不会被裁，每个 panic 的最内层都是 Guard")
	}

	ev := before(frames(
		sentry.Frame{Module: "main", Function: "realPanicSite"},
		sentry.Frame{Module: guardModule, Function: "Guard"},
	), nil)

	if ev == nil {
		t.Fatal("BeforeSend 把事件整个丢了 —— 那就一条都报不出去")
	}
	got := ev.Exception[0].Stacktrace.Frames
	if len(got) != 1 {
		t.Fatalf("裁帧后应剩 1 帧，实得 %d: %+v", len(got), got)
	}
	if got[0].Function != "realPanicSite" {
		t.Errorf("最内层帧 = %q，want \"realPanicSite\"（真正的 panic 现场）", got[0].Function)
	}
}

// TestTrimGuardFrameOnlyTrimsOwnFrame 钉死**只裁自己那帧**。
//
// 裁多了比不裁更糟：真正的 panic 现场被抹掉，上报指向调用者，排查直接跑偏。
func TestTrimGuardFrameOnlyTrimsOwnFrame(t *testing.T) {
	cases := []struct {
		name      string
		in        []sentry.Frame
		wantCount int
		wantInner string
	}{
		{
			name: "最内层是别的包里同名的 Guard —— 不许裁",
			in: []sentry.Frame{
				{Module: "main", Function: "caller"},
				{Module: "some/other/pkg", Function: "Guard"},
			},
			wantCount: 2,
			wantInner: "Guard",
		},
		{
			name: "最内层是本包的别的函数 —— 不许裁",
			in: []sentry.Frame{
				{Module: "main", Function: "caller"},
				{Module: guardModule, Function: "Init"},
			},
			wantCount: 2,
			wantInner: "Init",
		},
		{
			name: "Guard 不在最内层（正常 panic 现场在内）—— 不许裁",
			in: []sentry.Frame{
				{Module: guardModule, Function: "Guard"},
				{Module: "main", Function: "realPanicSite"},
			},
			wantCount: 2,
			wantInner: "realPanicSite",
		},
		{
			name: "最内层正是本包的 Guard —— 裁",
			in: []sentry.Frame{
				{Module: "main", Function: "realPanicSite"},
				{Module: guardModule, Function: "Guard"},
			},
			wantCount: 1,
			wantInner: "realPanicSite",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := frames(c.in...)
			trimGuardFrame(ev)
			got := ev.Exception[0].Stacktrace.Frames
			if len(got) != c.wantCount {
				t.Fatalf("帧数 = %d，want %d: %+v", len(got), c.wantCount, got)
			}
			if got[len(got)-1].Function != c.wantInner {
				t.Errorf("最内层 = %q，want %q", got[len(got)-1].Function, c.wantInner)
			}
		})
	}
}

// TestTrimGuardFrameSurvivesEmptyStacks 空栈/nil 栈不能让它炸。
//
// 上报路径上 panic 是不可接受的：BeforeSend 在 sentry 的调用链里，它 panic 会污染一个
// 本来只是想报告 panic 的流程。
func TestTrimGuardFrameSurvivesEmptyStacks(t *testing.T) {
	for _, ev := range []*sentry.Event{
		{},
		{Exception: []sentry.Exception{{}}}, // Stacktrace 为 nil
		{Exception: []sentry.Exception{{Stacktrace: &sentry.Stacktrace{}}}}, // Frames 为空
		frames(sentry.Frame{Module: guardModule, Function: "Guard"}),        // 只有 Guard 一帧
	} {
		trimGuardFrame(ev) // 不 panic 即通过
	}
}

// TestRealPanicStackHasNoGuardFrame 端到端：真的 panic 一次，确认裁帧在**真实的 runtime 栈**
// 上生效。
//
// 上面几条喂的都是手工构造的 Frame，它们验证不了本条要验证的那个假设：
//
//	**reflect 取到的 guardModule，与 runtime 栈里 Guard 帧的 Module 字段，是同一个字符串。**
//
// 两者对不上的话，判据永远不命中 → 裁帧静默失效 → 每个 panic 的最内层又变回 Guard，
// 而上面所有测试照样全绿。这正是「测纯函数便宜、测接线才值钱」的又一处。
//
// 手法：把一个带拦截 BeforeSend 的 client 绑到当前 hub 上，让 report 走进去，
// 事件在 BeforeSend 里被扣下（返回 nil），零出网零配额。
func TestRealPanicStackHasNoGuardFrame(t *testing.T) {
	got := captureGuardEvent(t, panicHere)

	if got == nil {
		t.Fatal("panic 没有产生任何上报事件")
	}
	if len(got.Exception) == 0 {
		t.Fatal("事件里没有 Exception —— 没有 type/value/栈，等于没报")
	}
	st := got.Exception[0].Stacktrace
	if st == nil || len(st.Frames) == 0 {
		t.Fatal("事件里没有栈")
	}

	inner := st.Frames[len(st.Frames)-1]
	if inner.Function == "Guard" && inner.Module == guardModule {
		t.Errorf("最内层帧仍是 Guard（Module=%q）—— 裁帧没生效。"+
			"多半是 guardModule 与 runtime 栈里的 Module 对不上", inner.Module)
	}
	if inner.Function != "panicHere" {
		t.Errorf("最内层帧 = %q（Module=%q），want \"panicHere\"（真正的 panic 现场）",
			inner.Function, inner.Module)
	}
}

// panicHere 是上面那条端到端测试里**真正的 panic 现场**，栈的最内层应当指向它。
func panicHere() {
	panic(errors.New("real panic for stack trim check"))
}

// captureGuardEvent 让 fn 在 Guard 底下 panic，返回 Guard 实际交给 sentry 的那个 event。
//
// 本包共享的测试工具（nonerrorpanic_test.go 也用它）。要点：
//   - 把一个带拦截 BeforeSend 的 client 绑到当前 hub 上，事件在 BeforeSend 里被扣下
//     （返回 nil）→ 零出网、零配额。
//   - 读到的是 sentry **实际收到**的东西，不是我们自己的副本 —— 这是它比手工构造 Frame
//     值钱的地方。
//   - Guard 会 re-panic（那是它的正确行为），所以外层要接住，否则测试进程直接死。
func captureGuardEvent(t *testing.T, fn func()) *sentry.Event {
	t.Helper()

	origDSN := dsn
	dsn = probeDSN
	t.Cleanup(func() { dsn = origDSN })

	var got *sentry.Event
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:              probeDSN,
		AttachStacktrace: true,
		BeforeSend: func(ev *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			trimGuardFrame(ev)
			got = ev
			return nil // 扣下，不发
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	hub := sentry.CurrentHub()
	prev := hub.Client()
	hub.BindClient(client)
	t.Cleanup(func() { hub.BindClient(prev) })

	func() {
		defer func() { _ = recover() }() // 接住 Guard 抛回来的
		defer Guard()
		fn()
	}()
	return got
}
