package telemetry

// 本文件只测一件事：**ReportOnce 的去重与事件形状**。
// panic 上报在 panicrepanic_test.go。
//
// 去重是这个 API 的全部意义所在：它上报的那类状态都活在轮询循环里（网易云是 v2 这件事
// 每 30 秒被重新检测一次），不去重就是刷屏，而刷屏能在一分钟内吃光免费版 5k/月 的配额。

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
		ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持", map[string]any{"version": "2.10.13"})
	}

	if n := len(events()); n != 1 {
		t.Errorf("同一个 key 报了 %d 次，want 1 —— 这类状态每 30 秒被重新检测一次，"+
			"不去重的话一场直播就能吃掉大半个月的配额", n)
	}
}

// TestReportOnceDistinctKeys 不同 key 各报一次 —— 去重不能过度。
func TestReportOnceDistinctKeys(t *testing.T) {
	events := captureEvents(t)

	ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持", nil)
	ReportOnce("qqmusic.unknown_version", "QQ 音乐版本未知", nil)
	ReportOnce("cloudmusic.unsupported_version", "网易云版本不支持", nil) // 重复的
	ReportOnce("kugou.patch_reverted", "酷狗补丁被回滚", nil)

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

	ReportOnce("some.key", "不该被报出去", nil)

	if n := len(events()); n != 0 {
		t.Errorf("未注入 DSN 时报了 %d 个事件，want 0", n)
	}
}

// TestReportOnceEventShape 钉死事件的形状：level / tag / fingerprint / context。
//
// fingerprint 那条最要紧：不显式钉的话 sentry 按 message 文本分组，而 message 里迟早会有人
// 拼进版本号 —— 那时同一类事件会裂成一堆 issue，「有多少主播在用 v2」这个问题就没法回答了。
//
// 变异自证：删掉 SetFingerprint / SetTag / SetLevel 任一即红。
func TestReportOnceEventShape(t *testing.T) {
	events := captureEvents(t)

	const key = "cloudmusic.unsupported_version"
	ReportOnce(key, "网易云音乐版本不支持 CDP（需 v3+）", map[string]any{
		"version":  "2.10.13.202675",
		"exe_path": `G:\CloudMusic\cloudmusic.exe`,
	})

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
	ctx, ok := ev.Contexts[key]
	if !ok {
		t.Fatalf("没有名为 %q 的 context —— 版本号和路径全丢了，只剩一句「不支持」", key)
	}
	if ctx["version"] != "2.10.13.202675" {
		t.Errorf("context.version = %v，want \"2.10.13.202675\"", ctx["version"])
	}
	if ctx["exe_path"] == nil {
		t.Error("context 里没有 exe_path")
	}
}

// TestReportOnceNilExtra extra 为 nil 时不该炸，也不该塞个空 context。
func TestReportOnceNilExtra(t *testing.T) {
	events := captureEvents(t)

	ReportOnce("some.key", "没有额外信息的事件", nil)

	evs := events()
	if len(evs) != 1 {
		t.Fatalf("报了 %d 个事件，want 1", len(evs))
	}
	if _, ok := evs[0].Contexts["some.key"]; ok {
		t.Error("extra 为 nil 时不该塞一个空 context —— 空 context 在 UI 里是噪音")
	}
}
