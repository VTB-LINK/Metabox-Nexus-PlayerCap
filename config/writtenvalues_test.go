package config

// 本文件只测「写下的值」的保留：钳制与校验回退改写运行值时，原值记入 WrittenValues，
// 供 /service-status 的 config 回显；运行值与改写前的行为一致，未被改写的键不留记录。
//
// 钳制本身的边界由 clamppoll_test / audionorm_test 守，效果策略的采纳判据由 effectstrategy_test 守；
// 本文件只断言「原值没丢」与「没改写就没记录」。CLI 侧（Load 里 flag.Visit 的效果策略分支）测不到，
// 原因同 clamppoll_test 的说明。

import "testing"

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

// TestMergeYAMLEffectStrategyKeepsWritten 非法字符串不被采纳（运行值保持默认），但写下的值保留；
// 合法值无记录；非字符串类型属于解析不出值的情况，维持现状、不留记录。
func TestMergeYAMLEffectStrategyKeepsWritten(t *testing.T) {
	const key = "cloudmusicv3-effect-strategy"
	cases := []struct {
		name        string
		v           interface{}
		wantWritten interface{}
		wantRecord  bool
	}{
		{"合法值无记录", "park", "park", false},
		{"非法字符串保留写下值", "foo", "foo", true},
		{"空串保留写下值", "", "", true},
		{"非字符串类型维持现状", 1, "fadeout", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			captureLog(t, func() { mergeYAML(&cfg, map[string]interface{}{key: c.v}) })
			if got := cfg.Written(key, cfg.EffectStrategy); got != c.wantWritten {
				t.Errorf("写下的值=%v，期望 %v", got, c.wantWritten)
			}
			if _, recorded := cfg.WrittenValues[key]; recorded != c.wantRecord {
				t.Errorf("记录=%v，期望 %v", recorded, c.wantRecord)
			}
		})
	}
}
