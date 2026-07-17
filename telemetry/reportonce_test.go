package telemetry

// 本文件只测两件事：**ReportOnce 的去重与事件形状**（含「低基数进 tag、高基数进 context」
// 的分工），以及 **SetPlayerVersion 设全局版本 tag**。panic 上报在 panicrepanic_test.go。
//
// 去重是 ReportOnce 的全部意义所在：它上报的那类状态都活在轮询循环里（网易云是 v2 这件事
// 每 30 秒被重新检测一次），不去重就是刷屏，而刷屏能在一分钟内吃光免费版 5k/月 的配额。
//
// tag vs context 的分工是「能不能筛选野外版本分布」的前提：版本号是低基数、进 tag 才能在
// Sentry 里聚合「最多的是哪个版本」；exe 路径、歌名样本是高基数，进 tag 会把面炸开。

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

// captureEvents 装一个拦截 client，返回一个取已捕获事件的函数。
// 事件在 BeforeSend 里被扣下（返回 nil），零出网零配额。
func captureEvents(t *testing.T) func() []*sentry.Event {
	t.Helper()

	origDSN := dsn
	dsn = probeDSN
	t.Cleanup(func() { dsn = origDSN })

	// 每个用例从干净的去重表开始，否则测试之间会互相「已经报过了」。
	reportedMu.Lock()
	reported = map[string]bool{}
	reportedMu.Unlock()

	var got []*sentry.Event
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: probeDSN,
		BeforeSend: func(ev *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			got = append(got, ev)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	hub := sentry.CurrentHub()
	prev := hub.Client()
	hub.BindClient(client)
	t.Cleanup(func() { hub.BindClient(prev) })

	return func() []*sentry.Event { return got }
}

// TestReportOnceDeduplicates 钉死同 key 只报一次。
//
// 100 次调用模拟的是真实节奏：v2 检测每 30 秒一轮，一场 8 小时的直播就是约 960 轮。
// 不去重的话，一个主播一场直播能自己吃掉 1/5 的月配额。
//
// 变异自证：删掉 reported[key] 的检查即红。
func TestReportOnceDeduplicates(t *testing.T) {
	events := captureEvents(t)

	for i := 0; i < 100; i++ {
		ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持",
			map[string]string{"cloudmusicv3.version": "2.10.13"}, nil)
	}

	if n := len(events()); n != 1 {
		t.Errorf("同一个 key 报了 %d 次，want 1 —— 这类状态每 30 秒被重新检测一次，"+
			"不去重的话一场直播就能吃掉大半个月的配额", n)
	}
}

// TestReportOnceDistinctKeys 不同 key 各报一次 —— 去重不能过度。
func TestReportOnceDistinctKeys(t *testing.T) {
	events := captureEvents(t)

	ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持", nil, nil)
	ReportOnce("qqmusic.unknown_version", "QQ 音乐版本未知", nil, nil)
	ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持", nil, nil) // 重复的
	ReportOnce("kugou.patch_reverted", "酷狗补丁被回滚", nil, nil)

	if n := len(events()); n != 3 {
		t.Errorf("三个不同 key 报了 %d 个事件，want 3 —— 去重只该按 key 生效", n)
	}
}

// TestReportOnceDisabledWhenNoDSN 未注入 DSN 时什么都不做。
//
// 变异自证：删掉 ReportOnce 开头的 `if !Enabled()` 即红。
func TestReportOnceDisabledWhenNoDSN(t *testing.T) {
	events := captureEvents(t)

	orig := dsn
	dsn = "" // captureEvents 设过了，这里覆盖成禁用
	t.Cleanup(func() { dsn = orig })

	ReportOnce("some.key", "不该被报出去", nil, nil)

	if n := len(events()); n != 0 {
		t.Errorf("未注入 DSN 时报了 %d 个事件，want 0", n)
	}
}

// TestReportOnceEventShape 钉死事件的形状：level / fingerprint / **tag vs context 分工**。
//
// 两条最要紧：
//   - fingerprint：不显式钉的话 sentry 按 message 文本分组，message 里迟早会拼进版本号 ——
//     那时同一类事件会裂成一堆 issue，「有多少主播在用 v2」这个问题就没法回答了。
//   - 版本号进 tag：低基数、可分面，这是「筛选野外最多哪个版本」的前提。放进 context 就只能
//     一个个点开看，无法聚合。而高基数的 exe 路径必须留 context，不能进 tag。
//
// 变异自证：删掉 SetFingerprint / SetLevel / tags 的 SetTag 循环任一即红。
func TestReportOnceEventShape(t *testing.T) {
	events := captureEvents(t)

	const key = "cloudmusic.unsupported_version"
	ReportOnce(key, "网易云音乐版本不支持 CDP（需 v3+）",
		map[string]string{"cloudmusicv3.version": "2.10.13.202675"}, // 低基数 → tag，可筛选
		map[string]any{"exe_path": `G:\CloudMusic\cloudmusic.exe`})  // 高基数 → context

	evs := events()
	if len(evs) != 1 {
		t.Fatalf("报了 %d 个事件，want 1", len(evs))
	}
	ev := evs[0]

	if ev.Level != sentry.LevelWarning {
		t.Errorf("Level = %v，want warning —— 这类事件不是待修的崩溃，是给我们的情报", ev.Level)
	}
	if got := ev.Tags["unexpected"]; got != key {
		t.Errorf("tag unexpected = %q，want %q —— 少了它就没法把「预期之外的事件」整体筛出来", got, key)
	}
	if len(ev.Fingerprint) != 1 || ev.Fingerprint[0] != key {
		t.Errorf("Fingerprint = %v，want [%q] —— 不钉死的话 sentry 按 message 文本分组，"+
			"message 里一旦拼进版本号，同类事件就裂成一堆 issue", ev.Fingerprint, key)
	}

	// 版本号必须进 tag —— 这是「能在 Sentry 里聚合筛选野外最多哪个版本」的前提。
	if got := ev.Tags["cloudmusicv3.version"]; got != "2.10.13.202675" {
		t.Errorf("tag cloudmusicv3.version = %q，want \"2.10.13.202675\" —— 版本号不进 tag 就没法聚合筛选", got)
	}
	// 高基数的路径留 context，绝不进 tag（否则 tag 面被路径炸开）。
	if _, ok := ev.Tags["exe_path"]; ok {
		t.Error("exe_path 进了 tag —— 高基数的路径必须留 context")
	}
	ctx, ok := ev.Contexts[key]
	if !ok {
		t.Fatalf("没有名为 %q 的 context —— exe_path 丢了", key)
	}
	if ctx["exe_path"] == nil {
		t.Error("context 里没有 exe_path")
	}
}

// TestReportOnceNilExtra extra 为 nil 时不该炸，也不该塞个空 context。
func TestReportOnceNilExtra(t *testing.T) {
	events := captureEvents(t)

	ReportOnce("some.key", "没有额外信息的事件", nil, nil)

	evs := events()
	if len(evs) != 1 {
		t.Fatalf("报了 %d 个事件，want 1", len(evs))
	}
	if _, ok := evs[0].Contexts["some.key"]; ok {
		t.Error("extra 为 nil 时不该塞一个空 context —— 空 context 在 UI 里是噪音")
	}
}

// TestSetPlayerVersion 钉死全局版本 tag：设过之后，这台机器上报的任何事件都带上它。
// 这是「异常上报只覆盖出问题的版本、而全量版本分布靠它」的机制。
func TestSetPlayerVersion(t *testing.T) {
	events := captureEvents(t)
	// 全局 scope 是 hub 级、跨用例共享，用后必须清掉，否则污染别的用例。
	t.Cleanup(func() {
		sentry.ConfigureScope(func(scope *sentry.Scope) { scope.RemoveTag("qqmusic.version") })
	})

	SetPlayerVersion("qqmusic", "22.41")
	sentry.CaptureMessage("probe") // 触发一次上报，看全局 tag 有没有带上

	evs := events()
	if len(evs) != 1 {
		t.Fatalf("报了 %d 个事件，want 1", len(evs))
	}
	if got := evs[0].Tags["qqmusic.version"]; got != "22.41" {
		t.Errorf("tag qqmusic.version = %q，want \"22.41\" —— 全局版本 tag 没生效，就聚合不了野外版本分布", got)
	}
}

// TestSetPlayerVersionSkipsEmpty 空版本不设 tag —— 读不到版本时别塞个空值污染分面。
func TestSetPlayerVersionSkipsEmpty(t *testing.T) {
	events := captureEvents(t)
	t.Cleanup(func() {
		sentry.ConfigureScope(func(scope *sentry.Scope) { scope.RemoveTag("kugou.version") })
	})

	SetPlayerVersion("kugou", "")
	sentry.CaptureMessage("probe")

	evs := events()
	if len(evs) != 1 {
		t.Fatalf("报了 %d 个事件，want 1", len(evs))
	}
	if _, ok := evs[0].Tags["kugou.version"]; ok {
		t.Error("空版本竟然设了 kugou.version tag —— 该 early return")
	}
}
