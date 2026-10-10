package config

// 本文件只测 audio-frame-rate 配置键：mergeYAML 的解析（类型不符须告警且不改值）、钳制边界，
// 以及模板里有这一行。越界时写下的原值保留见 writtenvalues_test，模板取值与 DefaultConfig() 一致见
// templatedefaults_test。Load() 里 clampAudioFrameRate 的调用点与 flag.Visit 分支里的赋值不在本包测
// （原因同 clamppoll_test 的说明），由根包 bannerconfig_test.go 经真实的 Load 守；该分支在不在由
// keyplaces_test 守。

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 整数被采纳并标记为显式；越界的整数照原样搬入（钳制另由 clampAudioFrameRate 做，写下的原值才留得住）；
// 字符串、小数、布尔与空值被忽略，值保持默认，并告警点名键。
//
// 变异自证（隔离副本，实测）：删掉 mergeYAML 的 audio-frame-rate 分支 → 30 未落地，红；分支里去掉
// mark → 未标记显式，红；类型不符时不告警（删 else）→ 红；改用 float64 断言 → 整数被拒、30.0 被采纳，红。
func TestMergeYAMLAudioFrameRate(t *testing.T) {
	for _, v := range []int{30, 15, 60, 0, -5, 100} {
		cfg := DefaultConfig()
		mergeYAML(&cfg, map[string]interface{}{"audio-frame-rate": v})
		if cfg.AudioFrameRate != v {
			t.Errorf("audio-frame-rate: %d 未落地，实得 %d", v, cfg.AudioFrameRate)
		}
		if !cfg.ExplicitKeys["audio-frame-rate"] {
			t.Errorf("audio-frame-rate: %d 未被标记为显式设置", v)
		}
	}

	for _, bad := range []interface{}{"30", 30.0, 29.97, true, nil, []interface{}{30}} {
		cfg := DefaultConfig()
		out := captureLog(t, func() {
			mergeYAML(&cfg, map[string]interface{}{"audio-frame-rate": bad})
		})
		if cfg.AudioFrameRate != 25 {
			t.Errorf("audio-frame-rate: %#v 不该被采纳，实得 %d", bad, cfg.AudioFrameRate)
		}
		if cfg.ExplicitKeys["audio-frame-rate"] {
			t.Errorf("audio-frame-rate: %#v 未被采纳，不该标记为显式设置", bad)
		}
		if !strings.Contains(out, "audio-frame-rate") {
			t.Errorf("audio-frame-rate: %#v 类型不符必须告警并点名键，实际输出 %q", bad, out)
		}
	}

	// 没写这个键：值与标记都不动。
	cfg := DefaultConfig()
	mergeYAML(&cfg, map[string]interface{}{"audio-normalize-target": -20})
	if cfg.AudioFrameRate != 25 || cfg.ExplicitKeys["audio-frame-rate"] {
		t.Errorf("未写 audio-frame-rate 时应保持默认且不标记，实得 %d / %v", cfg.AudioFrameRate, cfg.ExplicitKeys["audio-frame-rate"])
	}
}

// 钳到 [15,60]：越界（含 0 与负数）钳到最近的边界并告警，界内（含两端）不动、不告警。
//
// 变异自证（隔离副本，实测）：下限改为 10 → 14、0、-5 三例红；上限改为 120 → 61、1000 两例红；
// 钳制后不写回 cfg.AudioFrameRate → 越界各例红；去掉告警 → 越界各例红。
func TestClampAudioFrameRate(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{25, 25}, {15, 15}, {60, 60}, {30, 30},
		{14, 15}, {0, 15}, {-5, 15}, {61, 60}, {1000, 60},
	} {
		cfg := Config{AudioFrameRate: c.in}
		out := captureLog(t, func() { clampAudioFrameRate(&cfg) })
		if cfg.AudioFrameRate != c.want {
			t.Errorf("audio-frame-rate=%d 应为 %d，实得 %d", c.in, c.want, cfg.AudioFrameRate)
		}
		if warned := strings.Contains(out, "audio-frame-rate"); warned != (c.in != c.want) {
			t.Errorf("audio-frame-rate=%d：告警=%v，期望 %v（输出 %q）", c.in, warned, c.in != c.want, out)
		}
	}
}

// 模板里有 audio-frame-rate 一行，取值在钳制范围内。templatedefaults_test 只在模板写了这个键时才比对
// 它与 DefaultConfig() 是否一致，模板漏了这一行它照样绿。
//
// 变异自证（隔离副本，实测）：删掉模板中的 audio-frame-rate 行 → 红。
func TestTemplateHasAudioFrameRate(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(defaultConfigContent), &m); err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	v, ok := m["audio-frame-rate"].(int)
	if !ok {
		t.Fatalf("模板缺 audio-frame-rate，或其值不是整数: %#v", m["audio-frame-rate"])
	}
	if v < audioFrameRateMin || v > audioFrameRateMax {
		t.Errorf("模板的 audio-frame-rate=%d 不在 [%d,%d] 内", v, audioFrameRateMin, audioFrameRateMax)
	}
}
