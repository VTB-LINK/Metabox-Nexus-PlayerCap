package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTemplateMatchesDefaultConfig 钉住模板中全局键的取值与 DefaultConfig() 一致。
// 没有 config.yml 时 Load 写出模板再读；已有 config.yml 缺某键、或模板写入失败时，该键落到
// DefaultConfig()。两处不一致，新装与升级用户的实际默认值就不同（AGENTS.md「陷阱三」）。
// 只比全局字段：本测试二进制不注册播放器，mergeYAML 会跳过模板中的专属键；模板的专属偏移与
// DefaultConfig() 的回退值本就不同（同一陷阱的已知差异），不在此覆盖。
func TestTemplateMatchesDefaultConfig(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(defaultConfigContent), &m); err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	got, want := DefaultConfig(), DefaultConfig()
	mergeYAML(&got, m)
	got.Players, want.Players = nil, nil
	got.ExplicitKeys, want.ExplicitKeys = nil, nil // 只记录来源，不属于配置值
	if !reflect.DeepEqual(got, want) {
		t.Errorf("模板全局键与 DefaultConfig() 不一致:\n模板  %+v\n内置  %+v", got, want)
	}
}

// TestTemplateOffsetsNegateLegacyValues 钉住 offset 改为延迟语义（issue #54）之后模板里各 offset 的取值：
// 都是 3.x 模板取值（提前量）的相反数，同一份歌词的观感不变。下表左列照 v3.3.4 的模板抄录。
// 全局 offset 另由上一条与 DefaultConfig() 对齐；各播放器的键不在那一条之内（本测试二进制不注册播放器），
// 这里直接读模板的解析结果。
//
// 变异自证（隔离的 worktree，实测）：模板的 kugou-offset 改回 430 → 红；模板的 offset 改回 200 → 本条与
// 上一条都红。
func TestTemplateOffsetsNegateLegacyValues(t *testing.T) {
	legacy := map[string]int{
		"offset":              200,
		"cloudmusicv3-offset": 500,
		"qqmusic-offset":      400,
		"kugou-offset":        430,
		"sodamusic-offset":    340,
		"applemusic-offset":   400,
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(defaultConfigContent), &m); err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	for key, old := range legacy {
		if got, ok := m[key].(int); !ok || got != -old {
			t.Errorf("模板的 %s = %v，期望 %d（3.x 模板取值 %d 的相反数）", key, m[key], -old, old)
		}
	}
	// wesing-offset 在模板里是注释掉的示例，取值 0，不进解析结果。
	if _, ok := m["wesing-offset"]; ok {
		t.Errorf("模板的 wesing-offset 应是注释掉的示例，实际解析出 %v", m["wesing-offset"])
	}
}
