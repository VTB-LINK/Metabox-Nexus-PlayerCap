package telemetry

// 「预期之外」事件的上报。
//
// # 这个包只报两类东西
//
//	1. 崩溃（panic → Guard，见 panic.go）
//	2. 预期之外的事件（本文件）
//
// 第二类是主动的、有意的上报点，不是错误日志的转发。**别把 ReportOnce 接到 logger 上。**
// 本仓库实测过 10 event/秒（issue #30）与 55.8 event/秒的错误刷屏 —— 任何一次都能在一分钟内
// 吃光免费版 5k/月 的配额，而服务端拦不住（Custom Rate Limits 要 Business plan）。
// 真要接日志，得先做窗口聚合；ReportOnce 的低频是**它自己保证的**，不是靠运气。
//
// # 什么算「预期之外」
//
// 判据：**这件事发生了，说明我们的某个假设在这台机器上不成立。**
//
//	✓ 网易云是 v2（我们只支持 v3+，但主播装了 v2 —— 有多少人这样？）
//	✓ QQ 音乐是偏移表里没有的版本（读出来的是垃圾，见 §5.1）
//	✓ 酷狗的补丁字节被回滚了（杀软干的，硬化笔记记录过的真实故障）
//
//	✗ 网易云没启动          —— 预期之内，主播还没打开而已
//	✗ CDP 单次连接失败      —— 预期之内，重试就好
//	✗ 内存里某个字段读不到  —— 预期之内，下轮再试
//
// 后三者会在轮询循环里反复发生，正是刷屏的来源。

import (
	"sync"

	"github.com/getsentry/sentry-go"
)

var (
	reportedMu sync.Mutex
	reported   = map[string]bool{}
)

// ReportOnce 上报一个「预期之外」的事件。**同一个 key 每个进程只报一次。**
//
// 为什么必须自带去重：这类状态几乎都活在轮询循环里 —— 网易云是 v2 这件事，每 30 秒会被
// 重新检测到一次，一场直播下来就是几百次。它对我们的信息量全在第一次。
//
// 去重**不随状态变化重置**，这是与 log.Warn 的分工：
//   - log.Warn 是给主播看的 → 状态变了该再提醒（cloudmusic.go 的 warnedUnsupported 会重置）
//   - ReportOnce 是给我们看的 → 只需知道「这台机器上出现过 v2」，一次就够
//
// key 同时是 Sentry 的 fingerprint：同 key 的事件聚成一个 issue，具体差异（版本号、路径）
// 放 extra 里。所以 key 要是**稳定的类别**（"cloudmusic.unsupported_version"），
// 别把版本号拼进去 —— 那会让每个版本裂成一个 issue。
func ReportOnce(key, message string, extra map[string]any) {
	if !Enabled() {
		return
	}

	reportedMu.Lock()
	if reported[key] {
		reportedMu.Unlock()
		return
	}
	reported[key] = true
	reportedMu.Unlock()

	sentry.WithScope(func(scope *sentry.Scope) {
		// Warning 而不是 Error：这些事件本身不是故障 —— 程序按设计降级了，
		// 主播那边也有明确提示。它们是**给我们的情报**，不是待修的崩溃。
		scope.SetLevel(sentry.LevelWarning)

		// 这个 tag 让「预期之外的事件」能作为一个整体被筛出来，也能按类别分面。
		scope.SetTag("unexpected", key)

		// fingerprint 显式钉死，不让 sentry 按 message 文本分组 —— message 里迟早会有人
		// 拼进版本号之类的变量，那时同一类事件会裂成一堆 issue。
		scope.SetFingerprint([]string{key})

		if len(extra) > 0 {
			// 用 context 而不是 tag 装细节：tag 是分面用的、只能是 string、且怕高基数
			// （版本号、路径都属于高基数）。context 是给人读的，没这些限制。
			scope.SetContext(key, extra)
		}

		sentry.CaptureMessage(message)
	})
}
