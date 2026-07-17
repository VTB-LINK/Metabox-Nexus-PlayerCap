package telemetry

// 本文件只测一件事：**时区读的是非本地化的 TimeZoneKeyName，不是本地化的 StandardName**。
//
// # 这两个字段在同一个结构体里，长得一样，只有一个能用
//
// 实测本机（Win10 中文，2026-07-17）：
//
//	TimeZoneKeyName = "Singapore Standard Time"     ← 不随系统语言变
//	StandardName    = "马来西亚半岛标准时间"          ← **本地化的**
//
// 用 StandardName 当 key 的后果：同一个时区，中文系统的主播报「中国标准时间」、英文系统报
// "China Standard Time"、日文系统报另一个 —— 在 Sentry 里分裂成三个互不相干的值，
// 想按时区聚合就废了。而这个错误在英文机器上**看不出来**（两个字段恰好相同）。
//
// # 这个门禁的局限，说在前面
//
// 它靠「值里有没有非 ASCII 字符」判断，所以：
//   - 中文/日文/俄文等非拉丁语系的开发机上 → 能抓（当前开发机属此类）
//   - 英文机器上 → **抓不到**（StandardName 本来就是英文，与 KeyName 相同）
//   - CI 上 → 跑不了（telemetry 是 Windows-only 包，runner 是 Linux）
//
// 换句话说这是条**有条件的**门禁。写下来是因为它在当前开发机上确实有效，
// 而不是假装它到处都灵。

import (
	"testing"
	"unicode"
)

// TestTimeZoneUsesNonLocalizedKey 钉死时区值不含本地化字符。
//
// 变异自证（在中文 Windows 上）：把 sysinfo.go 里的 tz.TimeZoneKeyName 改成 tz.StandardName
// 即红 —— 值会变成「马来西亚半岛标准时间」。
func TestTimeZoneUsesNonLocalizedKey(t *testing.T) {
	o := collectOSInfo()

	if o.TimeZone == "" {
		t.Skip("这台机器读不到时区，跳过（GetDynamicTimeZoneInformation 失败）")
	}

	for _, r := range o.TimeZone {
		if r > unicode.MaxASCII {
			t.Errorf("时区 = %q，含非 ASCII 字符 %q\n"+
				"说明读的是本地化的 StandardName，而不是 TimeZoneKeyName。"+
				"实测本机：StandardName=「马来西亚半岛标准时间」、"+
				"TimeZoneKeyName=\"Singapore Standard Time\"。"+
				"用本地化的名字当 key，不同系统语言的主播会分裂成互不相干的时区值。",
				o.TimeZone, r)
			return
		}
	}

	t.Logf("时区 = %q（纯 ASCII，符合 TimeZoneKeyName 的形状）", o.TimeZone)
}

// TestCollectOSInfoReturnsPlausibleValues 真机采集的基本合理性。
//
// 这条不追求精确 —— 它守的是「采集链路整个塌了」这种粗故障：某个 API 换了签名、
// 结构体字段错位、手搓的 LazyDLL 调用写错参数个数，都会让这些值变成零值或垃圾。
func TestCollectOSInfoReturnsPlausibleValues(t *testing.T) {
	o := collectOSInfo()

	// Win10/11 的 major 都是 10。低于 6 意味着 Vista 之前 —— 本程序跑不了，一定是读错了。
	if o.Major < 6 {
		t.Errorf("Major = %d —— 低于 6 不可能（那是 Vista 之前），版本读取链路坏了", o.Major)
	}
	if o.Build == 0 {
		t.Error("Build = 0 —— 读取链路坏了")
	}
	if o.Arch == "" {
		t.Error("Arch 为空 —— GetNativeSystemInfo 没生效")
	}
	if o.CPUCores == 0 {
		t.Error("CPUCores = 0 —— 不可能，GetNativeSystemInfo 的结构体字段可能错位了")
	}
	if o.Locale == "" {
		t.Error("Locale 为空 —— GetUserDefaultLocaleName 没生效")
	}
	// 这条专防一个具体的错法：GetDynamicTimeZoneInformation 的返回值里，
	// **只有 TIME_ZONE_ID_INVALID(0xFFFFFFFF) 才是失败**；0/1/2 分别是 unknown/standard/daylight，
	// 都算成功。把判据写成 `r != 0` 会把「当前处于标准时间」当失败 —— 而本机实测正是返回 0，
	// 那样时区就静默变成空串了。上面那条时区测试遇到空串会 Skip，抓不到，只能靠这里。
	if o.TimeZone == "" {
		t.Error("TimeZone 为空 —— GetDynamicTimeZoneInformation 的返回值判据可能写错了：" +
			"只有 0xFFFFFFFF 是失败，0/1/2 都是成功")
	}

	t.Logf("本机: Windows %s (%s) %s / %s / %s / %d 核 / shim=%v",
		o.version(), o.Edition, o.Arch, o.Locale, o.TimeZone, o.CPUCores, o.Shimmed)
}
