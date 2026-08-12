package telemetry

// 本文件只测一件事：**隐私提示与代码实际采集的东西是否一致**。
//
// 这段文案的风险不在措辞，在于**它迟早会和代码分叉**：有人加了一类新采集，忘了改文案 ——
// 于是我们对主播的承诺变成了假的，而四道门禁全绿、谁也不会发现。
//
// 所以下面不测「有没有打印」（那是同义反复），只测「文案有没有漏掉实际采集面」。

import (
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/config"
)

// noticeCoverage 把「实际会上报的 context / tag」映射到「文案里对应的那句话」。
//
// **新增采集面时，这张表和文案都要改** —— 下面的测试会先一步红。
// 值是文案里必须出现的关键词，用它反查那句话还在不在。
var noticeCoverage = map[string]string{
	// ── context ──
	"os":                  "Windows 版本",
	"device":              "CPU 架构", // arch / cpu_cores，归在「系统」那行
	"playercap":           "程序",
	"windows_compat_shim": "Windows 版本", // 只在检出兼容模式时出现，报的仍是系统版本

	// ── tag ──
	"os.build":               "Windows 版本",
	"os.edition":             "edition",
	"locale":                 "语言",
	"timezone":               "时区",
	"arch":                   "CPU 架构",
	"windows.compat_shim":    "Windows 版本",
	"config.customized":      "config.yml",
	"config.effect_strategy": "config.yml", // 是 config.yml 里的一项
	"unexpected":             "异常类型",
}

// noticeExempt 是**不必**写进隐私提示的 context —— 它们由 sentry 自动附加，且不含任何
// 用户数据。豁免必须写明理由：这张表是「我们承认它会被发出去，但它不是用户的东西」的
// 唯一出口，不写理由就成了后门。
var noticeExempt = map[string]string{
	// 实测：即使 EnableTracing: false，sentry-go 仍会给每个事件挂一个 trace context
	// （Sentry UI 里显示为「Trace: bc9ce361…」）。内容只有随机生成的 trace_id 与 span_id，
	// 用于把同一次操作的多个事件串起来 —— 与主播的任何信息无关。
	"trace": "sentry 自动生成的随机关联 ID（trace_id/span_id），不含用户数据",
}

// TestPrivacyNoticeCoversEveryContextAndTag 钉死文案覆盖每一项实际采集。
//
// 手法：把 Init 会挂的东西全挂到一个 scope 上，生成事件，然后逐个 context/tag 检查
// noticeCoverage 里有没有它、且对应的关键词还在文案里。
//
// 变异自证：给 applyOSInfo 加一个新 context（或新 tag）而不改文案，即红。
func TestPrivacyNoticeCoversEveryContextAndTag(t *testing.T) {
	scope := sentry.NewScope()
	// 用「有 shim」的 osInfo，好把 windows_compat_shim 这个条件 context 也覆盖到 ——
	// 它平时不出现，但出现时同样会发出去，文案得管它。
	applyOSInfo(scope, osInfo{
		Major: 10, Minor: 0, Build: 22631,
		ShimMajor: 6, ShimMinor: 2, ShimBuild: 9200, Shimmed: true,
		Edition: "Pro", Locale: "zh-CN", TimeZone: "China Standard Time",
		Arch: "x64", CPUCores: 8,
	})
	applyAppInfo(scope, collectAppInfo("1.0.0", config.DefaultConfig(), []string{"wesing"}))
	applyUserIP(scope)

	ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}

	check := func(kind, name string) {
		if reason, exempt := noticeExempt[name]; exempt {
			t.Logf("%s %q 已豁免：%s", kind, name, reason)
			return
		}
		keyword, known := noticeCoverage[name]
		if !known {
			t.Errorf("会上报 %s %q，但 noticeCoverage 与 noticeExempt 里都没有它 —— "+
				"要么把它写进隐私提示并登记在 noticeCoverage，要么在 noticeExempt 里写明"+
				"「它不含用户数据」的理由，要么别采它。"+
				"我们对主播的承诺不能落后于代码。", kind, name)
			return
		}
		if !strings.Contains(privacyNotice, keyword) {
			t.Errorf("%s %q 对应的文案关键词 %q 已经从隐私提示里消失了", kind, name, keyword)
		}
	}

	for name := range ev.Contexts {
		check("context", name)
	}
	for name := range ev.Tags {
		check("tag", name)
	}

	// IP 是单独一条路径（User 而非 context/tag），单独核对。
	if ev.User.IPAddress != "" && !strings.Contains(privacyNotice, "公网 IP") {
		t.Error("会上报 IP，但隐私提示里没有「公网 IP」那句")
	}
}

// TestPrivacyNoticeStatesPurposeAndRecipient 钉死「干什么用」和「给谁」还在。
//
// 「收集了什么」只是一半，主播真正关心的是另一半。这两句被删掉的话，
// 剩下的就只是一张吓人的清单。
func TestPrivacyNoticeStatesPurposeAndRecipient(t *testing.T) {
	for _, must := range []string{
		"用途",        // 干什么用
		"sentry.io", // 给谁 —— 断言域名而非品牌名「Sentry」：域名更具体，
		// 且文案可以用「遥测服务端」这类中性措辞而不必点名品牌。
		"30 天", // 存多久
		"不上报",  // 边界：哪些不碰
	} {
		if !strings.Contains(privacyNotice, must) {
			t.Errorf("隐私提示里缺少 %q —— 只说「收集了什么」而不说「干什么用、给谁、存多久」，"+
				"等于只把人吓一跳", must)
		}
	}
}

// TestPrivacyNoticeSkippedWhenDisabled 钉死**两段都不适用时不打印、不等待**。
//
// 这条是给开发者的：本地 go build 既不上报遥测、也不查版本（非 semver 版本号会让
// checkAndUpdate 直接返回），一个字节都不会外发。这时候摆一段「我们会上报…」的告示
// 是在撒谎，还要白等 10 秒 —— 每次调试都等。
//
// 变异自证：删掉 PrintPrivacyNotice 里的 `if text == "" { return }` 即红（会真等 10 秒）。
func TestPrivacyNoticeSkippedWhenDisabled(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = ""

	start := time.Now()
	PrintPrivacyNotice(10*time.Second, false)

	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("遥测未启用、也不查版本时 PrintPrivacyNotice 等了 %v（应当立即返回）—— "+
			"本地每次调试都要白等这么久", elapsed)
	}
}

// TestNoticeSectionsFollowTheirOwnSwitch 钉死两段各归各的开关。
//
// 这是加入版本检查上报时**两个方向的谎**：印多了（遥测关着却说会上报崩溃栈到
// sentry.io）与印少了（发布版把设备标识发出去却只字不提）。整块文案共用一个开关时
// 两种都会发生，而且都不会有人发现——没人会去核对一段没打印出来的文字。
func TestNoticeSectionsFollowTheirOwnSwitch(t *testing.T) {
	cases := []struct {
		name                      string
		telemetryOn, versionCheck bool
		wantTelemetry, wantUpdate bool
	}{
		{"两个都开：正式发布版的常态", true, true, true, true},
		{"只有遥测：注入了 DSN 的本地构建", true, false, true, false},
		{"只查版本：未注入 DSN 的发布版", false, true, false, true},
		{"两个都关：本地 go build 的常态", false, false, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := noticeFor(c.telemetryOn, c.versionCheck)

			if !c.wantTelemetry && !c.wantUpdate {
				if text != "" {
					t.Fatalf("两段都不适用时仍拼出了文案：\n%s", text)
				}
				return
			}
			if text == "" {
				t.Fatal("有段落适用，却拼出了空文案")
			}

			// 用各段独有的关键词判在不在，别拿整段做子串比较——那样改一个字就红，
			// 门禁会变成「谁也不敢动文案」而不是「文案不许落后于代码」。
			if got := strings.Contains(text, "sentry.io"); got != c.wantTelemetry {
				t.Errorf("遥测段出现=%v，期望 %v", got, c.wantTelemetry)
			}
			if got := strings.Contains(text, "gateway.vtb.link"); got != c.wantUpdate {
				t.Errorf("更新段出现=%v，期望 %v", got, c.wantUpdate)
			}
		})
	}
}

// TestCountdownZeroReturnsImmediately wait 为 0 时不能卡住。
func TestCountdownZeroReturnsImmediately(t *testing.T) {
	start := time.Now()
	countdown(0)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("countdown(0) 花了 %v", elapsed)
	}
}
