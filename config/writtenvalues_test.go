package config

// 本文件只测「写下的值」的保留，分两处记：
//   - 钳制与取值校验改写运行值时，原值记入 WrittenValues，供 /service-status 的 config 回显；这些原值的
//     类型与该键的正常类型相同（钳制前的整数、取值非法的特效策略字符串）。
//   - 值因类型不符而被拒时，原值记入 RejectedValues，只供启动横幅；它们不得进 WrittenValues，否则会让
//     /service-status 里该字段的类型随写法变化。
//
// 运行值与此前的行为一致，按原样采纳的键两处都不留记录。钳制本身的边界由 clamppoll_test /
// audionorm_test 守，效果策略的采纳判据由 effectstrategy_test 守。CLI 一层（Load 里的 flag.Visit）不在
// 本包测：命令行覆盖后两处记录如何作废，由根包 bannerconfig_test.go 经真实的 Load 驱动。

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestClampPollsKeepsWritten 越界的全局与 per-player poll：运行值照旧钳制，写下的值保留原值；
// 未设覆盖的播放器继承全局写下的值（不是全局钳制后的值）。
func TestClampPollsKeepsWritten(t *testing.T) {
	cfg := Config{
		Poll: 5,
		Players: map[string]*PlayerConfig{
			"kugou":   {Poll: pollPtr(99999)},
			"wesing":  {Poll: pollPtr(500)},
			"qqmusic": {},
		},
	}
	clampPolls(&cfg)

	if cfg.Poll != pollMin {
		t.Errorf("全局 poll=5 的运行值应钳为 %d，实得 %d", pollMin, cfg.Poll)
	}
	if got := cfg.Written("poll", cfg.Poll); got != 5 {
		t.Errorf("全局 poll 写下的值应为 5，实得 %v", got)
	}

	if got := cfg.GetPlayerPoll("kugou"); got != pollMax {
		t.Errorf("kugou-poll=99999 的运行值应钳为 %d，实得 %d", pollMax, got)
	}
	if got := cfg.WrittenPlayerPoll("kugou"); got != 99999 {
		t.Errorf("kugou-poll 写下的值应为 99999，实得 %d", got)
	}

	if got := cfg.GetPlayerPoll("qqmusic"); got != pollMin {
		t.Errorf("qqmusic 无覆盖，运行值应继承全局钳制后的 %d，实得 %d", pollMin, got)
	}
	if got := cfg.WrittenPlayerPoll("qqmusic"); got != 5 {
		t.Errorf("qqmusic 无覆盖，写下的值应继承全局写下的 5，实得 %d", got)
	}

	if got := cfg.WrittenPlayerPoll("wesing"); got != 500 || cfg.GetPlayerPoll("wesing") != 500 {
		t.Errorf("wesing-poll=500 未越界，写下值与运行值都应为 500，实得 %d / %d", got, cfg.GetPlayerPoll("wesing"))
	}
	if _, ok := cfg.WrittenValues["wesing-poll"]; ok {
		t.Error("wesing-poll 未被钳制，不该留下写下值记录")
	}
}

// TestClampPollsInRangeLeavesNoRecord 全部在界内时不留任何记录，写下值即运行值。
func TestClampPollsInRangeLeavesNoRecord(t *testing.T) {
	cfg := Config{Poll: 30, Players: map[string]*PlayerConfig{"kugou": {Poll: pollPtr(pollMin)}}}
	clampPolls(&cfg)
	if len(cfg.WrittenValues) != 0 {
		t.Errorf("无越界却留下记录 %v", cfg.WrittenValues)
	}
	if got := cfg.Written("poll", cfg.Poll); got != 30 {
		t.Errorf("全局 poll 写下值应为 30，实得 %v", got)
	}
	if got := cfg.WrittenPlayerPoll("kugou"); got != pollMin {
		t.Errorf("kugou-poll 写下值应为 %d，实得 %d", pollMin, got)
	}
}

// TestClampAudioNormalizeKeepsWritten 越界的 target / max-gain：运行值照旧钳制，写下的值保留原值。
func TestClampAudioNormalizeKeepsWritten(t *testing.T) {
	cfg := Config{AudioNormalizeTarget: 10, AudioNormalizeMaxGain: -5}
	clampAudioNormalize(&cfg)

	if cfg.AudioNormalizeTarget != audioNormTargetMax || cfg.AudioNormalizeMaxGain != audioNormMaxGainMin {
		t.Errorf("运行值应钳为 %d / %d，实得 %d / %d",
			audioNormTargetMax, audioNormMaxGainMin, cfg.AudioNormalizeTarget, cfg.AudioNormalizeMaxGain)
	}
	if got := cfg.Written("audio-normalize-target", cfg.AudioNormalizeTarget); got != 10 {
		t.Errorf("audio-normalize-target 写下的值应为 10，实得 %v", got)
	}
	if got := cfg.Written("audio-normalize-max-gain", cfg.AudioNormalizeMaxGain); got != -5 {
		t.Errorf("audio-normalize-max-gain 写下的值应为 -5，实得 %v", got)
	}

	inRange := Config{AudioNormalizeTarget: -14, AudioNormalizeMaxGain: 30}
	clampAudioNormalize(&inRange)
	if len(inRange.WrittenValues) != 0 {
		t.Errorf("无越界却留下记录 %v", inRange.WrittenValues)
	}
}

// TestClampAudioFrameRateKeepsWritten 越界的 audio-frame-rate：运行值钳到边界，写下的值保留原值；
// 界内不留记录。
//
// 变异自证（隔离副本，实测）：clampAudioFrameRate 去掉 noteWritten → 写下的值变成钳制后的 60，红；
// noteWritten 挪到赋值之后（记下的是钳制后的值）→ 红。
func TestClampAudioFrameRateKeepsWritten(t *testing.T) {
	cfg := Config{AudioFrameRate: 100}
	captureLog(t, func() { clampAudioFrameRate(&cfg) })
	if cfg.AudioFrameRate != audioFrameRateMax {
		t.Errorf("运行值应钳为 %d，实得 %d", audioFrameRateMax, cfg.AudioFrameRate)
	}
	if got := cfg.Written("audio-frame-rate", cfg.AudioFrameRate); got != 100 {
		t.Errorf("audio-frame-rate 写下的值应为 100，实得 %v", got)
	}

	inRange := Config{AudioFrameRate: 30}
	clampAudioFrameRate(&inRange)
	if len(inRange.WrittenValues) != 0 {
		t.Errorf("无越界却留下记录 %v", inRange.WrittenValues)
	}
	if got := inRange.Written("audio-frame-rate", inRange.AudioFrameRate); got != 30 {
		t.Errorf("界内时写下的值即运行值 30，实得 %v", got)
	}
}

// TestMergeYAMLEffectStrategyKeepsWritten 非法字符串不被采纳（运行值保持默认），但写下的值保留在
// WrittenValues（类型仍是字符串，进 config 回显）；合法值无记录。非字符串类型属于类型不符：不进
// WrittenValues，对 config 回显而言视同没写，原值记在 RejectedValues（只上横幅）。
//
// 变异自证（隔离的 worktree，实测）：非字符串取值也记进 WrittenValues → 后两例红；非法字符串改记进
// RejectedValues → 第二、三例红。
func TestMergeYAMLEffectStrategyKeepsWritten(t *testing.T) {
	const key = "cloudmusicv3-effect-strategy"
	cases := []struct {
		name         string
		v            interface{}
		wantWritten  interface{}
		wantRecord   bool // 记在 WrittenValues
		wantRejected bool // 记在 RejectedValues
	}{
		{"合法值无记录", "park", "park", false, false},
		{"非法字符串保留写下值", "foo", "foo", true, false},
		{"空串保留写下值", "", "", true, false},
		{"非字符串类型不进 WrittenValues", 1, "fadeout", false, true},
		{"空值不进 WrittenValues", nil, "fadeout", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			captureLog(t, func() { mergeYAML(&cfg, map[string]interface{}{key: c.v}) })
			if got := cfg.Written(key, cfg.EffectStrategy); got != c.wantWritten {
				t.Errorf("写下的值=%v，期望 %v", got, c.wantWritten)
			}
			if _, recorded := cfg.WrittenValues[key]; recorded != c.wantRecord {
				t.Errorf("WrittenValues 有记录=%v，期望 %v", recorded, c.wantRecord)
			}
			got, rejected := cfg.RejectedValues[key]
			if rejected != c.wantRejected || (rejected && got != c.v) {
				t.Errorf("RejectedValues 有记录=%v（%#v），期望 %v（%#v）", rejected, got, c.wantRejected, c.v)
			}
		})
	}
}

// TestMergeYAMLRejectedValues mergeYAML 认得的每个键，写成它认不了的类型时：不采纳（运行值与显式标记都
// 不变）、告警点名该键、原值记入 RejectedValues，且 WrittenValues 里不留任何东西——后者会进 /service-status
// 的 config，原值一旦记进去，该字段的类型就随写法变了。键集合取自 config.go 源码里 mergeYAML 对参数 m 的
// 下标读取（与 keyplaces_test 同一个扫描），测试里没有键清单：新增一个键而拒收时不记原值、记错地方或不
// 告警，本用例即红。小数、空值、键值映射三种取值没有哪个键认。
//
// 变异自证（隔离的 worktree，实测）：以下分支拒收时各自改记进 WrittenValues，均红——offset、poll、
// prior-player、audio-frame-rate、cloudmusicv3-effect-strategy、每播放器 poll、每播放器 audio-normalize；
// offset、每播放器 offset 拒收时不记原值 → 红；删掉 offset 或每播放器 poll 拒收时的告警 → 红；
// noteRejected 整个改为记进 WrittenValues → 红。
func TestMergeYAMLRejectedValues(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读取 config.go 失败: %v", err)
	}
	keys, _, _ := scanKeys(t, string(src), t.Errorf)
	if len(keys) == 0 {
		t.Fatal("没有从 mergeYAML 扫出任何键，本用例失效")
	}
	const player = "zzrejected"
	registerForTest(player)

	for _, k := range keys {
		key := strings.Replace(k, playerKey, player, 1)
		for _, bad := range []any{3.5, nil, map[string]any{"a": 1}} {
			cfg, want := DefaultConfig(), DefaultConfig()
			mergeYAML(&want, nil) // 只为与 cfg 一样带上各播放器的空 PlayerConfig
			out := captureLog(t, func() { mergeYAML(&cfg, map[string]any{key: bad}) })

			if got, recorded := cfg.RejectedValues[key]; !recorded || !reflect.DeepEqual(got, bad) || len(cfg.RejectedValues) != 1 {
				t.Errorf("%s: %#v 被拒后 RejectedValues 应只有这一条原值，实得 %#v", key, bad, cfg.RejectedValues)
			}
			if len(cfg.WrittenValues) != 0 {
				t.Errorf("%s: %#v 被拒的原值不得进 WrittenValues，实得 %#v", key, bad, cfg.WrittenValues)
			}
			if !strings.Contains(out, key) {
				t.Errorf("%s: %#v 被拒必须告警并点名键，实际输出 %q", key, bad, out)
			}
			cfg.RejectedValues = nil
			if !reflect.DeepEqual(cfg, want) {
				t.Errorf("%s: %#v 不该被采纳，运行值或显式标记变了\n得 %+v\n期望 %+v", key, bad, cfg, want)
			}
		}
	}
}

// TestMergeYAMLPriorPlayerDroppedItems prior-player 列表里不是字符串的项被丢弃：其余项照常采纳、键算显式设置，
// 写下的原列表记入 RejectedValues（只上横幅）并告警；config 回显的是采纳后的列表，WrittenValues 里没有它。
// 全是字符串（含空列表）时两处都不留记录、不告警。
//
// 变异自证（隔离的 worktree，实测）：丢弃非字符串项时不记原列表 → 红；把原列表记进 WrittenValues → 红；
// 不告警 → 红；全是字符串时也记 → 红。
func TestMergeYAMLPriorPlayerDroppedItems(t *testing.T) {
	const key = "prior-player"
	raw := []any{"kugou", 1, "wesing"}
	cfg := DefaultConfig()
	out := captureLog(t, func() { mergeYAML(&cfg, map[string]any{key: raw}) })
	if !reflect.DeepEqual(cfg.PriorPlayer, []string{"kugou", "wesing"}) || !cfg.ExplicitKeys[key] {
		t.Errorf("字符串项应照常采纳并标为显式，实得 %v / %v", cfg.PriorPlayer, cfg.ExplicitKeys[key])
	}
	if got := cfg.RejectedValues[key]; !reflect.DeepEqual(got, raw) {
		t.Errorf("RejectedValues 里应是原列表 %v，实得 %v", raw, got)
	}
	if len(cfg.WrittenValues) != 0 {
		t.Errorf("原列表不得进 WrittenValues，实得 %v", cfg.WrittenValues)
	}
	if !strings.Contains(out, key) {
		t.Errorf("丢弃列表项必须告警，实际输出 %q", out)
	}

	for _, clean := range [][]any{{"kugou", "wesing"}, {}} {
		cfg := DefaultConfig()
		out := captureLog(t, func() { mergeYAML(&cfg, map[string]any{key: clean}) })
		if len(cfg.WrittenValues) != 0 || len(cfg.RejectedValues) != 0 || out != "" || !cfg.ExplicitKeys[key] {
			t.Errorf("%v 全部采纳，不该留记录或告警：记录 %v / %v，输出 %q，显式=%v",
				clean, cfg.WrittenValues, cfg.RejectedValues, out, cfg.ExplicitKeys[key])
		}
	}
}

// TestLoadFileRejectedYAML 真实的 YAML 文本经 loadFile：各种类型不符的写法都按 YAML 解出的原值记进
// RejectedValues，WrittenValues 里一条也没有，运行值保持默认、不标记为显式。
//
// 变异自证（隔离的 worktree，实测）：mergeYAML 的 offset、prior-player、audio-frame-rate、每播放器 poll、
// 每播放器 audio-normalize 分支拒收时改记进 WrittenValues → 各自红；offset、每播放器 offset 拒收时不记
// 原值 → 红。
func TestLoadFileRejectedYAML(t *testing.T) {
	const player = "zzyaml"
	registerForTest(player)
	exeDir := t.TempDir()
	writeConfig(t, filepath.Join(exeDir, configName), `addr: 8765
offset: "300"
poll: 30.5
prior-player: wesing
prior-player-expire: true
per-player-idle-hide:
audio-normalize: "false"
audio-normalize-target: [-14]
audio-normalize-max-gain: {gain: 30}
audio-frame-rate: "30"
cloudmusicv3-effect-strategy: 1
zzyaml-offset: "0"
zzyaml-poll: fast
zzyaml-idle-hide: 1.5
zzyaml-audio-normalize: 0
`)
	cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), t.TempDir(), nil)

	want := map[string]any{
		"addr":                         8765,
		"offset":                       "300",
		"poll":                         30.5,
		"prior-player":                 "wesing",
		"prior-player-expire":          true,
		"per-player-idle-hide":         nil,
		"audio-normalize":              "false",
		"audio-normalize-target":       []any{-14},
		"audio-normalize-max-gain":     map[string]any{"gain": 30},
		"audio-frame-rate":             "30",
		"cloudmusicv3-effect-strategy": 1,
		"zzyaml-offset":                "0",
		"zzyaml-poll":                  "fast",
		"zzyaml-idle-hide":             1.5,
		"zzyaml-audio-normalize":       0,
	}
	if !reflect.DeepEqual(cfg.RejectedValues, want) {
		t.Errorf("RejectedValues\n得 %#v\n期望 %#v", cfg.RejectedValues, want)
	}
	if len(cfg.WrittenValues) != 0 {
		t.Errorf("类型不符的原值不得进 WrittenValues，实得 %#v", cfg.WrittenValues)
	}
	if len(cfg.ExplicitKeys) != 0 {
		t.Errorf("被拒的键不该标为显式: %v", cfg.ExplicitKeys)
	}
	def := DefaultConfig()
	if cfg.Addr != def.Addr || cfg.Offset != def.Offset || cfg.Poll != def.Poll || !reflect.DeepEqual(cfg.PriorPlayer, def.PriorPlayer) ||
		cfg.PriorPlayerExpire != def.PriorPlayerExpire || cfg.PerPlayerIdleHide != def.PerPlayerIdleHide ||
		cfg.AudioNormalize != def.AudioNormalize || cfg.AudioNormalizeTarget != def.AudioNormalizeTarget ||
		cfg.AudioNormalizeMaxGain != def.AudioNormalizeMaxGain || cfg.AudioFrameRate != def.AudioFrameRate ||
		cfg.EffectStrategy != def.EffectStrategy || !reflect.DeepEqual(cfg.Players[player], &PlayerConfig{}) {
		t.Errorf("被拒的键不该改动运行值: %+v（%s: %+v）", cfg, player, cfg.Players[player])
	}
	if cfg.ConfigFileState != FileLoaded {
		t.Errorf("文件本身解析成功，加载结果应为 FileLoaded，实得 %d", cfg.ConfigFileState)
	}
}
