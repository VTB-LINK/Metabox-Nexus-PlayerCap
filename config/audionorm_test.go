package config

// 本文件测音量归一化配置的三件事，与 clamppoll_test / priorplayerwarn_test 同套路：
//  1. GetPlayerAudioNormalize 的 per-player bool 覆盖优先、nil 回退全局（对照 GetPlayerIdleHide）；
//  2. 全局 target/max-gain 的钳制边界（对照 clampPolls）；
//  3. mergeYAML 对全局与 per-player 键的解析，含类型断言失败必须告警且不改值（AGENTS.md §3.3）。
//
// 第 3 项尤其要紧：全局字段的 yaml tag 是装饰性的（yaml.Unmarshal 解到 map 再手工搬运，
// 见 config.go 的 Load 与 mergeYAML），只加字段+tag 会静默失效。本文件的 TestMergeYAMLAudioNormalizeGlobal
// 就是钉死「手动分支真的把值搬进来了」——删掉分支它当场红。

import (
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

// TestGetPlayerAudioNormalize per-player *bool 覆盖优先、nil 回退全局，双向都测。
func TestGetPlayerAudioNormalize(t *testing.T) {
	cases := []struct {
		name   string
		global bool
		player map[string]*PlayerConfig
		query  string
		want   bool
	}{
		// 全局 on：某家显式 off → 该家 false
		{"全局on+某家override off → false", true,
			map[string]*PlayerConfig{"wesing": {AudioNormalize: boolPtr(false)}}, "wesing", false},
		// 全局 on：未覆盖的家回退全局 → true
		{"全局on+不在map → true", true,
			map[string]*PlayerConfig{"wesing": {AudioNormalize: boolPtr(false)}}, "kugou", true},
		{"全局on+map里nil → true", true,
			map[string]*PlayerConfig{"qqmusic": {AudioNormalize: nil}}, "qqmusic", true},
		// 全局 off：某家显式 on → 该家 true
		{"全局off+某家override on → true", false,
			map[string]*PlayerConfig{"wesing": {AudioNormalize: boolPtr(true)}}, "wesing", true},
		// 全局 off：未覆盖的家回退全局 → false
		{"全局off+不在map → false", false,
			map[string]*PlayerConfig{"wesing": {AudioNormalize: boolPtr(true)}}, "kugou", false},
		{"全局off+map里nil → false", false,
			map[string]*PlayerConfig{"qqmusic": {AudioNormalize: nil}}, "qqmusic", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{AudioNormalize: c.global, Players: c.player}
			if got := cfg.GetPlayerAudioNormalize(c.query); got != c.want {
				t.Errorf("GetPlayerAudioNormalize(%q)=%v，期望 %v", c.query, got, c.want)
			}
		})
	}
}

// TestClampAudioNormalize 全局 target 钳到 [-60,0]、max-gain 钳到 [0,45]，含越界反例与边界。
func TestClampAudioNormalize(t *testing.T) {
	cases := []struct {
		name         string
		target, gain int
		wantTarget   int
		wantGain     int
	}{
		{"默认值不动", -14, 30, -14, 30},
		{"target 正值越界 → 0", 10, 30, 0, 30},
		{"target 过低越界 → -60", -100, 30, -60, 30},
		{"max-gain 过高越界 → 45", -14, 99, -14, 45},
		{"max-gain 负值越界 → 0", -14, -5, -14, 0},
		{"target 恰好上界 0", 0, 30, 0, 30},
		{"target 恰好下界 -60", -60, 30, -60, 30},
		{"max-gain 恰好下界 0", -14, 0, -14, 0},
		{"max-gain 恰好上界 45", -14, 45, -14, 45},
		{"两头同时越界", 50, 200, 0, 45},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{AudioNormalizeTarget: c.target, AudioNormalizeMaxGain: c.gain}
			clampAudioNormalize(&cfg)
			if cfg.AudioNormalizeTarget != c.wantTarget {
				t.Errorf("target=%d 应钳为 %d，实得 %d", c.target, c.wantTarget, cfg.AudioNormalizeTarget)
			}
			if cfg.AudioNormalizeMaxGain != c.wantGain {
				t.Errorf("max-gain=%d 应钳为 %d，实得 %d", c.gain, c.wantGain, cfg.AudioNormalizeMaxGain)
			}
		})
	}
}

// TestMergeYAMLAudioNormalizeGlobal 钉死全局三字段真的被 mergeYAML 手动搬进来了。
// 若有人按「yaml tag 自动处理」删掉手动分支，valid 子用例当场红（值仍是入参默认）。
func TestMergeYAMLAudioNormalizeGlobal(t *testing.T) {
	// 合法值：bool/int 正常落地 + mark
	cfg := Config{Players: map[string]*PlayerConfig{}, ExplicitKeys: map[string]bool{}}
	mergeYAML(&cfg, map[string]interface{}{
		"audio-normalize":          true,
		"audio-normalize-target":   -18,
		"audio-normalize-max-gain": 20,
	})
	if !cfg.AudioNormalize {
		t.Error("audio-normalize: true 未落地 —— 全局字段缺手动解析分支（§3.3 静默失效）")
	}
	if cfg.AudioNormalizeTarget != -18 {
		t.Errorf("audio-normalize-target 未落地，实得 %d", cfg.AudioNormalizeTarget)
	}
	if cfg.AudioNormalizeMaxGain != 20 {
		t.Errorf("audio-normalize-max-gain 未落地，实得 %d", cfg.AudioNormalizeMaxGain)
	}
	for _, k := range []string{"audio-normalize", "audio-normalize-target", "audio-normalize-max-gain"} {
		if !cfg.ExplicitKeys[k] {
			t.Errorf("%s 未被 mark 为显式设置", k)
		}
	}

	// 类型断言失败：字符串 "yes" 必须被忽略、不改值、且告警点名键
	cfg2 := Config{AudioNormalize: false, Players: map[string]*PlayerConfig{}, ExplicitKeys: map[string]bool{}}
	out := captureLog(t, func() {
		mergeYAML(&cfg2, map[string]interface{}{"audio-normalize": "yes"})
	})
	if cfg2.AudioNormalize {
		t.Error("audio-normalize: \"yes\"（字符串）不该被接受")
	}
	if !strings.Contains(out, "audio-normalize") {
		t.Errorf("类型断言失败必须告警并点名键，实际输出 %q", out)
	}
}

// TestMergeYAMLAudioNormalizePerPlayer per-player bool 覆盖解析 + 类型断言失败忽略。
func TestMergeYAMLAudioNormalizePerPlayer(t *testing.T) {
	// 用独有名字追加注册，避免依赖注册表为空、与同包其它测试解耦（见 priorplayerwarn_test）。
	registerForTest("anorm-player")

	// 合法 bool：false 必须可与 nil 区分（displaylike idle-hide 的 *bool 语义）
	cfg := Config{Players: map[string]*PlayerConfig{}, ExplicitKeys: map[string]bool{}}
	mergeYAML(&cfg, map[string]interface{}{"anorm-player-audio-normalize": false})
	pc := cfg.Players["anorm-player"]
	if pc == nil || pc.AudioNormalize == nil {
		t.Fatal("anorm-player-audio-normalize: false 未被解析成非 nil 覆盖")
	}
	if *pc.AudioNormalize != false {
		t.Errorf("per-player 覆盖应为 false，实得 %v", *pc.AudioNormalize)
	}
	if !cfg.ExplicitKeys["anorm-player-audio-normalize"] {
		t.Error("per-player 键未被 mark")
	}

	// 类型断言失败：字符串 → 忽略、保持 nil、告警
	cfg2 := Config{Players: map[string]*PlayerConfig{}, ExplicitKeys: map[string]bool{}}
	out := captureLog(t, func() {
		mergeYAML(&cfg2, map[string]interface{}{"anorm-player-audio-normalize": "yes"})
	})
	if pc2 := cfg2.Players["anorm-player"]; pc2 != nil && pc2.AudioNormalize != nil {
		t.Error("字符串值不该被接受，覆盖应保持 nil")
	}
	if !strings.Contains(out, "anorm-player-audio-normalize") {
		t.Errorf("per-player 类型断言失败必须告警并点名键，实际输出 %q", out)
	}
}
