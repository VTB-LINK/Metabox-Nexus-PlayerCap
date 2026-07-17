package telemetry

// 把系统信息挂到 sentry 的 scope 上（context 用于阅读，tag 用于筛选/聚合）。

import (
	"strconv"

	"github.com/getsentry/sentry-go"
)

// applyOSInfo 把一次系统快照写进 scope。
//
// context 与 tag 的分工不是随便定的：
//   - context：给人读的详情，不可筛选，payload 大小无所谓
//   - tag：可筛选、可聚合、可做 issue 的分面。**只能是 string**，且数量有限（别往里塞高基数的值）
//
// 默认的 Environment 集成只在 key 不存在时才填，所以我们预写的 contexts["os"] 不会被它
// 用 runtime.GOOS（那个字符串 "windows"）覆盖掉。
func applyOSInfo(scope *sentry.Scope, o osInfo) {
	// Sentry 对 contexts["os"] 有约定字段（name/version/build/kernel_version），
	// 用对了它会在 UI 里渲染成结构化的 OS 卡片而不是一坨 JSON。edition 是我们加的。
	scope.SetContext("os", map[string]any{
		"name":    "Windows",
		"version": o.version(),
		"build":   strconv.FormatUint(uint64(o.Build), 10),
		"edition": o.Edition,
	})
	scope.SetContext("device", map[string]any{
		"arch":      o.Arch,
		"cpu_cores": o.CPUCores,
	})

	scope.SetTag("os.build", strconv.FormatUint(uint64(o.Build), 10))
	setTagIfNotEmpty(scope, "os.edition", o.Edition)
	setTagIfNotEmpty(scope, "locale", o.Locale)
	setTagIfNotEmpty(scope, "timezone", o.TimeZone)
	setTagIfNotEmpty(scope, "arch", o.Arch)

	// 这个 tag **无论真假都设**，不是只在 true 时设：我们要能回答「有多少主播开着兼容模式」，
	// 那需要一个能分面的布尔，而不是「有的机器有这个 tag、有的没有」。
	scope.SetTag("windows.compat_shim", strconv.FormatBool(o.Shimmed))

	if o.Shimmed {
		scope.SetContext("windows_compat_shim", map[string]any{
			"real_version":     o.version(),
			"reported_version": o.shimVersion(),
			"why_it_matters": "RtlGetVersion 受兼容层影响，RtlGetNtVersionNumbers 不受。" +
				"park.IsWindows11() 建立在前者之上：真实是 Win11 但被 shim 报成低版本时，" +
				"park 的强制降级不会触发，而 Win11 的 DWM 不合成不可见窗口 —— 主播画面会黑掉。",
		})
	}
}

// setTagIfNotEmpty 空值不设 tag。
//
// 空字符串的 tag 在 Sentry 里会变成一个真实存在的、值为空的分面，比「没有这个 tag」更难查：
// 前者看着像「采到了，是空的」，后者才是「没采到」。这几项确实可能采不到（API 失败时留空）。
func setTagIfNotEmpty(scope *sentry.Scope, key, value string) {
	if value != "" {
		scope.SetTag(key, value)
	}
}

// logOSInfo 把采到的系统信息打一行出来，并在检测到兼容性 shim 时告警。
//
// 那条 Warn 对主播有直接价值（「关掉兼容模式」是他自己能做的），且与 Sentry 通不通无关。
// **它其实不该住在 telemetry 里** —— 受害者是 park，理应由 park 自检。但那是另一条线的改动，
// 这里先把已经采到的信息用起来，别浪费。
func logOSInfo(o osInfo) {
	log.Info("系统: Windows %s (%s) %s / %s / %s / %d 核",
		o.version(), nonEmpty(o.Edition, "edition 未知"), o.Arch,
		nonEmpty(o.Locale, "locale 未知"), nonEmpty(o.TimeZone, "时区未知"), o.CPUCores)

	if o.Shimmed {
		log.Warn("检测到兼容性 shim：系统真实版本 %s，但 RtlGetVersion 报的是 %s。"+
			"park 的 Win11 判据读的正是后者 —— 若这台机器其实是 Win11，屏外保活不会被降级，"+
			"网易云特效可能黑屏。建议右键 exe →「属性」→「兼容性」，取消勾选兼容模式。",
			o.version(), o.shimVersion())
	}
}

// nonEmpty 给空值一个可读的替代，免得日志里出现「Windows 10.0.19045 () x64 //」这种。
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
