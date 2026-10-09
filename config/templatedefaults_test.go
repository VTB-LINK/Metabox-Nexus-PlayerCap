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
