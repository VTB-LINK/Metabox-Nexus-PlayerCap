package config

// 本文件只测 cloudmusicv3-effect-strategy 的取值校验：非法值必须告警、不改值、不标记 ExplicitKeys。
//
// 它守的是**配置假象**：server.SetDefaultEffectStrategy 对非法值静默忽略（运行时仍是 fadeout），
// 而 /service-status 的 config_effective 与 config_overwritten 读的是 cfg 的运行值。值若在 config 层
// 被放过，回显显示已生效、实际并未生效。（被拦下的写下值只进 config 回显，见 writtenvalues_test。）
//
// 只覆盖 mergeYAML 一侧。CLI 覆盖（Load 里 flag.Visit 的同名分支）共用 validEffectStrategy，
// 不在本包测（同 clamppoll_test 的说明）：CLI 侧「非法值撤销显式标记、config.yml 的合法标记保留」
// 由根包 bannerconfig_test.go 的 TestBannerCommandLineOverride 经真实的 Load 守。

import (
	"strings"
	"testing"
)

func TestMergeYAMLEffectStrategy(t *testing.T) {
	const key = "cloudmusicv3-effect-strategy"
	cases := []struct {
		name     string
		v        interface{}
		want     string
		wantMark bool
	}{
		{"park 生效", "park", "park", true},
		{"fadeout 生效", "fadeout", "fadeout", true},
		{"非法字符串保留默认", "foo", "fadeout", false},
		{"大小写不同视为非法", "Park", "fadeout", false},
		{"空串视为非法", "", "fadeout", false},
		{"非字符串类型保留默认", 1, "fadeout", false},
		{"空值保留默认", nil, "fadeout", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			out := captureLog(t, func() {
				mergeYAML(&cfg, map[string]interface{}{key: c.v})
			})
			if cfg.EffectStrategy != c.want {
				t.Errorf("EffectStrategy=%q，期望 %q", cfg.EffectStrategy, c.want)
			}
			if cfg.ExplicitKeys[key] != c.wantMark {
				t.Errorf("ExplicitKeys[%s]=%v，期望 %v —— 未采纳的值不得进 config_overwritten",
					key, cfg.ExplicitKeys[key], c.wantMark)
			}
			if warned := strings.Contains(out, key); warned == c.wantMark {
				t.Errorf("告警=%v，期望 %v；实际输出 %q", warned, !c.wantMark, out)
			}
		})
	}
}
