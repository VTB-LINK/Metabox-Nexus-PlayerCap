package config

// 本文件只测 song-info-keep-first 配置键：DefaultConfig 默认值、mergeYAML 的解析与类型拒收、
// 以及内置模板中包含该键（C1，#229）。命令行覆盖与 flag.Visit 赋值由根包 bannerconfig_test.go 守，
// 五处齐备由 keyplaces_test.go 守，模板与 DefaultConfig 一致由 templatedefaults_test.go 守。

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMergeYAMLSongInfoKeepFirst 校验 mergeYAML 读取 song-info-keep-first 的四种路径：
// false（关闭并标显式）、true（开启并标显式）、不写（保持默认 true 且不标显式）、
// 非布尔值（拒收：保持 true、不标显式、进 RejectedValues、不进 WrittenValues 并告警）。
//
// 变异自证（隔离副本，实测）：删去 mergeYAML 分支 → 本用例、TestConfigKeyPlaces 与
// TestLoadFileRejectedYAML 红；DefaultConfig 的 SongInfoKeepFirst 改为 false →
// 「不写保持默认开启且不标显式」「非布尔值拒收」与 TestTemplateMatchesDefaultConfig 红。
func TestMergeYAMLSongInfoKeepFirst(t *testing.T) {
	const key = "song-info-keep-first"

	t.Run("读 false 关且标显式", func(t *testing.T) {
		cfg := DefaultConfig()
		out := captureLog(t, func() {
			mergeYAML(&cfg, map[string]interface{}{key: false})
		})
		if cfg.SongInfoKeepFirst {
			t.Error("song-info-keep-first: false 未落地")
		}
		if !cfg.ExplicitKeys[key] {
			t.Error("song-info-keep-first: false 未标记为显式")
		}
		if len(cfg.RejectedValues) != 0 || len(cfg.WrittenValues) != 0 || out != "" {
			t.Errorf("合法值不应留拒收/改写记录或告警：rejected=%v written=%v log=%q",
				cfg.RejectedValues, cfg.WrittenValues, out)
		}
	})

	t.Run("读 true 开且标显式", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SongInfoKeepFirst = false
		out := captureLog(t, func() {
			mergeYAML(&cfg, map[string]interface{}{key: true})
		})
		if !cfg.SongInfoKeepFirst {
			t.Error("song-info-keep-first: true 未落地")
		}
		if !cfg.ExplicitKeys[key] {
			t.Error("song-info-keep-first: true 未标记为显式")
		}
		if len(cfg.RejectedValues) != 0 || len(cfg.WrittenValues) != 0 || out != "" {
			t.Errorf("合法值不应留拒收/改写记录或告警：rejected=%v written=%v log=%q",
				cfg.RejectedValues, cfg.WrittenValues, out)
		}
	})

	t.Run("不写保持默认开启且不标显式", func(t *testing.T) {
		cfg := DefaultConfig()
		out := captureLog(t, func() {
			mergeYAML(&cfg, map[string]interface{}{"poll": 50})
		})
		if !cfg.SongInfoKeepFirst {
			t.Error("未写 song-info-keep-first 时默认应为 true")
		}
		if cfg.ExplicitKeys[key] {
			t.Error("未写 song-info-keep-first 时不应标记为显式")
		}
		if out != "" {
			t.Errorf("未写该键不应告警：%q", out)
		}
	})

	t.Run("非布尔值拒收", func(t *testing.T) {
		for _, bad := range []interface{}{"false", "true", 0, 1, 1.5, nil, []interface{}{false}} {
			cfg := DefaultConfig()
			out := captureLog(t, func() {
				mergeYAML(&cfg, map[string]interface{}{key: bad})
			})
			if !cfg.SongInfoKeepFirst {
				t.Errorf("song-info-keep-first: %#v 不应改写默认值 true", bad)
			}
			if cfg.ExplicitKeys[key] {
				t.Errorf("song-info-keep-first: %#v 被拒后不应标为显式", bad)
			}
			if got, ok := cfg.RejectedValues[key]; !ok || !reflect.DeepEqual(got, bad) {
				t.Errorf("song-info-keep-first: %#v 应进 RejectedValues，实得 %#v", bad, cfg.RejectedValues)
			}
			if len(cfg.WrittenValues) != 0 {
				t.Errorf("song-info-keep-first: %#v 不得进 WrittenValues，实得 %#v", bad, cfg.WrittenValues)
			}
			if !strings.Contains(out, key) {
				t.Errorf("song-info-keep-first: %#v 拒收必须告警并点名键，实得 %q", bad, out)
			}
		}
	})
}

// TestTemplateHasSongInfoKeepFirst 校验模板中显式写有 song-info-keep-first: true。
//
// 变异自证（隔离副本，实测）：删去模板行 → 本用例、TestConfigKeyPlaces 与
// TestConfigReadmeMatchesTemplate 红；模板写成 false → 本用例、TestTemplateMatchesDefaultConfig
// 与 TestConfigReadmeMatchesTemplate 红。
func TestTemplateHasSongInfoKeepFirst(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(defaultConfigContent), &m); err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	v, ok := m["song-info-keep-first"].(bool)
	if !ok {
		t.Fatalf("模板缺 song-info-keep-first，或其值不是布尔：%#v", m["song-info-keep-first"])
	}
	if !v {
		t.Errorf("模板的 song-info-keep-first = %v，期望 true", v)
	}
}
