package telemetry

// 英文隐私提示的门禁，与中文侧 privacynotice_test.go / gatewaynotice_test.go 同构。
//
// 面向非中文分发时，英文采集告知与中文一样是**对用户的承诺**，同样不能落后于代码。
// 中文侧核对的是 privacyNotice / noticeUpdateSection（中文 const），这里核对
// privacyNoticeEN / noticeUpdateSectionEN。新增采集面 / 请求头时，中英两侧的覆盖表都要改。

import (
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/clientid"
	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
)

// noticeCoverageEN 是 noticeCoverage 的英文对照：context/tag → 英文文案关键词。
var noticeCoverageEN = map[string]string{
	// ── context ──
	"os":                  "Windows version",
	"device":              "CPU architecture",
	"playercap":           "Program",
	"windows_compat_shim": "Windows version",

	// ── tag ──
	"os.build":               "Windows version",
	"os.edition":             "edition",
	"locale":                 "language",
	"timezone":               "time zone",
	"arch":                   "CPU architecture",
	"windows.compat_shim":    "Windows version",
	"config.customized":      "config.yml",
	"config.effect_strategy": "config.yml",
	"unexpected":             "anomaly type",
	"process":                "Audio capture",
}

// gatewayNoticeCoverageEN 是 gatewayNoticeCoverage 的英文对照：请求头 → 更新段英文关键词。
var gatewayNoticeCoverageEN = map[string]string{
	clientid.HeaderID:      "anonymous device ID",
	clientid.HeaderVersion: "Program version",
	clientid.HeaderOS:      "Windows version",
	clientid.HeaderEdition: "edition",
	clientid.HeaderArch:    "CPU architecture",
	clientid.HeaderPing:    "daily online",
	clientid.HeaderUA:      "Program version",
}

// TestPrivacyNoticeENCoversEveryContextAndTag 是中文同名门禁的英文同位物：
// 钉死英文隐私提示覆盖每一项实际采集的 context/tag。
func TestPrivacyNoticeENCoversEveryContextAndTag(t *testing.T) {
	scope := sentry.NewScope()
	applyOSInfo(scope, osInfo{
		Major: 10, Minor: 0, Build: 22631,
		ShimMajor: 6, ShimMinor: 2, ShimBuild: 9200, Shimmed: true,
		Edition: "Pro", Locale: "zh-CN", TimeZone: "China Standard Time",
		Arch: "x64", CPUCores: 8,
	})
	applyAppInfo(scope, collectAppInfo("1.0.0", config.DefaultConfig(), []string{"wesing"}))
	applyProcess(scope, "audio-helper")
	applyUserIP(scope)

	ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}

	check := func(kind, name string) {
		if _, exempt := noticeExempt[name]; exempt {
			return
		}
		keyword, known := noticeCoverageEN[name]
		if !known {
			t.Errorf("会上报 %s %q，但 noticeCoverageEN 里没有它 —— 英文隐私提示不能落后于代码", kind, name)
			return
		}
		if !strings.Contains(privacyNoticeEN, keyword) {
			t.Errorf("%s %q 对应的英文关键词 %q 已从英文隐私提示里消失", kind, name, keyword)
		}
	}
	for name := range ev.Contexts {
		check("context", name)
	}
	for name := range ev.Tags {
		check("tag", name)
	}
	if ev.User.IPAddress != "" && !strings.Contains(privacyNoticeEN, "public IP") {
		t.Error("会上报 IP，但英文隐私提示里没有 public IP 那句")
	}
}

// TestUpdateNoticeENCoversEveryRequestHeader 英文更新段覆盖每一个请求头。
func TestUpdateNoticeENCoversEveryRequestHeader(t *testing.T) {
	for _, header := range clientid.HeaderNames() {
		keyword, known := gatewayNoticeCoverageEN[header]
		if !known {
			t.Errorf("版本检查请求会带 %q，但 gatewayNoticeCoverageEN 里没有它 —— 英文更新段不能落后于代码", header)
			continue
		}
		if !strings.Contains(noticeUpdateSectionEN, keyword) {
			t.Errorf("请求头 %q 对应的英文关键词 %q 已从英文更新段里消失", header, keyword)
		}
	}
}

// TestUpdateNoticeENCoverageHasNoStaleEntries 反向：登记了却已不发的头要清掉。
func TestUpdateNoticeENCoverageHasNoStaleEntries(t *testing.T) {
	actual := make(map[string]bool, len(clientid.HeaderNames()))
	for _, header := range clientid.HeaderNames() {
		actual[header] = true
	}
	for header := range gatewayNoticeCoverageEN {
		if !actual[header] {
			t.Errorf("gatewayNoticeCoverageEN 里登记了 %q，但 clientid 已不发这个头了", header)
		}
	}
}

// TestPrivacyNoticeENStatesPurposeAndRecipient 英文版「干什么用/给谁/存多久/不碰什么」。
func TestPrivacyNoticeENStatesPurposeAndRecipient(t *testing.T) {
	for _, must := range []string{"Purpose", "sentry.io", "30 days", "Not reported"} {
		if !strings.Contains(privacyNoticeEN, must) {
			t.Errorf("英文隐私提示里缺少 %q", must)
		}
	}
}

// TestPrivacyNoticeENCoversRUMSection 钉死英文 RUM 隐私告知段已纳入全集且关键承诺完整。
//
// 变异自证：删掉 noticeRUMSectionEN 里的 "public IP" 或 "posthog.com" 即红。
func TestPrivacyNoticeENCoversRUMSection(t *testing.T) {
	for _, must := range []string{
		"Release builds",
		"MachineGuid",
		"no additional local state file",
		"administrator",
		"public IP",
		"song name",
		"artist",
		"album",
		"lyric text",
		"full local paths",
		"Windows user name",
		"full cover URLs",
		"frontend query parameters",
		"unknown config text",
		"downsampled",
		"Recipient",
		"posthog.com",
	} {
		if !strings.Contains(noticeRUMSectionEN, must) {
			t.Errorf("英文 RUM 隐私告知段缺少 %q", must)
		}
		if !strings.Contains(privacyNoticeEN, must) {
			t.Errorf("英文隐私告知全集缺少 RUM 关键说明 %q", must)
		}
	}
}

// TestNoticeForSwitchesLanguage 钉死 noticeFor 按 i18n 语言产出对应语言的段落。
func TestNoticeForSwitchesLanguage(t *testing.T) {
	defer i18n.SetLanguage(i18n.Chinese)

	i18n.SetLanguage(i18n.English)
	en := noticeFor(true, true, true)
	if !strings.Contains(en, "Reported:") || strings.Contains(en, "上报内容") || !strings.Contains(en, "MachineGuid") {
		t.Errorf("英文下 noticeFor 应产出含 RUM 的英文段落、不含中文，得到：\n%s", en)
	}
	enRUMOnly := noticeFor(false, false, true)
	if !strings.Contains(enRUMOnly, "MachineGuid") || strings.Contains(enRUMOnly, "sentry.io") || strings.Contains(enRUMOnly, "gateway.vtb.link") || strings.Contains(enRUMOnly, "上报内容") {
		t.Errorf("英文下单开 RUM 应仅产出英文 RUM 段，得到：\n%s", enRUMOnly)
	}

	i18n.SetLanguage(i18n.Chinese)
	zh := noticeFor(true, true, true)
	if !strings.Contains(zh, "上报内容") || strings.Contains(zh, "Reported:") || !strings.Contains(zh, "MachineGuid") {
		t.Errorf("中文下 noticeFor 应产出含 RUM 的中文段落、不含英文，得到：\n%s", zh)
	}
	zhRUMOnly := noticeFor(false, false, true)
	if !strings.Contains(zhRUMOnly, "MachineGuid") || strings.Contains(zhRUMOnly, "sentry.io") || strings.Contains(zhRUMOnly, "gateway.vtb.link") || strings.Contains(zhRUMOnly, "Reported:") {
		t.Errorf("中文下单开 RUM 应仅产出中文 RUM 段，得到：\n%s", zhRUMOnly)
	}
}
