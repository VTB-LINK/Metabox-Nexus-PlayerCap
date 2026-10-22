package telemetry_test

// 本文件只测一件事：音频采集的三个上报事件（audiocap/manager）在隐私提示里有交代（issue #120）。
//
// privacynotice_test.go 只遍历用 applyOSInfo / applyAppInfo / applyProcess / applyUserIP 构造出的 scope，碰不到
// manager 经 ReportOnce 发出的事件。这里取 manager.SampleTelemetryEvents()——它与真实上报共用同一批构造函数、
// 可选字段都填上——逐个核对每个 tag 键与 extra 键：登记表里要有它，且登记的关键词同时出现在中文与英文遥测段。
// 构造函数新增任何键而不改文案与登记表，即红。
//
// 放在外部测试包：包内测试导入 manager 会因 manager 的 Windows 文件导入 telemetry 而成环。

import (
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/audiocap/manager"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// audioNoticeCoverage 把音频采集事件的 tag / extra 键映射到中英文遥测段里必须出现的关键词。
var audioNoticeCoverage = map[string]struct{ zh, en string }{
	// ── tag ──
	"exit_code": {"退出码", "exit code"},
	"cause":     {"放弃", "give-up"},
	"player":    {"播放器名", "player names"},
	// ── extra ──
	"alive_ms":       {"运行时长", "run time"},
	"quick_fails":    {"失败次数", "failure count"},
	"failures":       {"失败次数", "failure count"},
	"last_exit_code": {"退出码", "exit code"},
	"spawn_error":    {"系统错误码", "system error codes"},
	"stderr_tail":    {"诊断日志末几行", "last lines of its diagnostic log"},
}

// TestPrivacyNoticeCoversAudioCaptureEvents 逐个核对样本事件的每个 tag 与 extra 键。
//
// 变异自证（实测）：给 CaptureGaveUpEvent 加一个 extra 键、给 HelperExitedEvent 加一个 tag 而不登记，均红；
// 删掉中文或英文遥测段里的音频采集那行，均红。
func TestPrivacyNoticeCoversAudioCaptureEvents(t *testing.T) {
	samples := manager.SampleTelemetryEvents()
	if len(samples) == 0 {
		t.Fatal("manager.SampleTelemetryEvents 没有样本：本门禁空转")
	}
	used := map[string]bool{}
	check := func(key, kind, name string) {
		used[name] = true
		kw, ok := audioNoticeCoverage[name]
		if !ok {
			t.Errorf("事件 %s 会上报 %s %q，但 audioNoticeCoverage 里没有它：要么写进隐私提示（中英文）并登记，要么别采它", key, kind, name)
			return
		}
		if !strings.Contains(telemetry.NoticeTelemetrySection, kw.zh) {
			t.Errorf("事件 %s 的 %s %q 对应的中文关键词 %q 不在遥测段里", key, kind, name, kw.zh)
		}
		if !strings.Contains(telemetry.NoticeTelemetrySectionEN, kw.en) {
			t.Errorf("事件 %s 的 %s %q 对应的英文关键词 %q 不在英文遥测段里", key, kind, name, kw.en)
		}
	}
	for _, ev := range samples {
		for name := range ev.Tags {
			check(ev.Key, "tag", name)
		}
		for name := range ev.Extra {
			check(ev.Key, "extra", name)
		}
	}
	for name := range audioNoticeCoverage {
		if !used[name] {
			t.Errorf("audioNoticeCoverage 登记了 %q，但样本事件里已没有这个键：清掉它，免得登记表与代码分叉", name)
		}
	}
}

// TestPrivacyNoticeHasAudioCaptureLine 断言中英文遥测段都有音频采集那一行（以条目开头的词认）。
//
// 变异自证（实测）：删掉任一语言的那一行即红。
func TestPrivacyNoticeHasAudioCaptureLine(t *testing.T) {
	for _, c := range []struct{ lang, section, head string }{
		{"中文", telemetry.NoticeTelemetrySection, "· 音频采集："},
		{"英文", telemetry.NoticeTelemetrySectionEN, "· Audio capture:"},
	} {
		if !strings.Contains(c.section, c.head) {
			t.Errorf("%s遥测段里没有以 %q 开头的音频采集条目", c.lang, c.head)
		}
	}
}
