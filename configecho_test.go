package main

// 本文件只测 /service-status 配置三件套的组装（buildConfigEcho）：config（写下的值）与
// config_effective（实际生效的值）键集合与顺序一致，播放器层轮询下限计入 config_effective，
// config_overwritten 按同一键序筛出显式设置的键。
//
// 配置层钳制如何记下原值由 config/writtenvalues_test.go 守；本文件直接构造钳制后的 Config。

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/server"
)

var echoPlayers = []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic"}

// echoEntries 按序列化顺序取出 OrderedMap 的键与值（数值解为 float64）。
func echoEntries(t *testing.T, om *server.OrderedMap) ([]string, map[string]any) {
	t.Helper()
	b, err := json.Marshal(om)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	vals := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		vals[tok.(string)] = v
	}
	return keys, vals
}

func defaultEchoConfig() config.Config {
	cfg := config.DefaultConfig()
	for _, n := range echoPlayers {
		cfg.Players[n] = &config.PlayerConfig{}
	}
	return cfg
}

// TestConfigEchoKeysMatch 两张表键集合与顺序完全相同，且是约定的键序。
func TestConfigEchoKeysMatch(t *testing.T) {
	cfg := defaultEchoConfig()
	written, effective, _ := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	wk, _ := echoEntries(t, written)
	ek, _ := echoEntries(t, effective)

	want := []string{"addr", "offset", "poll", "prior-player", "prior-player-expire", "per-player-idle-hide",
		"audio-normalize", "audio-normalize-target", "audio-normalize-max-gain", "cloudmusicv3-effect-strategy"}
	for _, n := range echoPlayers {
		want = append(want, n+"-offset", n+"-poll", n+"-idle-hide", n+"-audio-normalize")
	}
	if !reflect.DeepEqual(wk, want) {
		t.Errorf("config 键序\n得 %v\n期望 %v", wk, want)
	}
	if !reflect.DeepEqual(ek, wk) {
		t.Errorf("config_effective 键序与 config 不同\n得 %v\n期望 %v", ek, wk)
	}
}

// TestConfigEchoPlayerPollFloors 默认配置（poll 30）下，播放器层下限只进 config_effective。
func TestConfigEchoPlayerPollFloors(t *testing.T) {
	cfg := defaultEchoConfig()
	written, effective, _ := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)

	for key, want := range map[string][2]float64{
		"poll":              {30, 30},
		"wesing-poll":       {30, 30},
		"cloudmusicv3-poll": {30, 100},
		"qqmusic-poll":      {30, 30},
		"kugou-poll":        {30, 30},
		"sodamusic-poll":    {30, 200},
	} {
		if w[key] != want[0] || e[key] != want[1] {
			t.Errorf("%s 写下/生效=%v/%v，期望 %v/%v", key, w[key], e[key], want[0], want[1])
		}
	}
}

// TestConfigEchoClampedPoll 写 poll: 5（配置层钳为 10）：全局 5/10，未覆盖的播放器继承写下的 5，
// 生效值再经各自下限。
func TestConfigEchoClampedPoll(t *testing.T) {
	cfg := defaultEchoConfig()
	cfg.Poll = 10
	cfg.WrittenValues = map[string]any{"poll": 5}
	cfg.ExplicitKeys["poll"] = true

	written, effective, overwritten := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)

	for key, want := range map[string][2]float64{
		"poll":              {5, 10},
		"wesing-poll":       {5, 10},
		"cloudmusicv3-poll": {5, 100},
		"qqmusic-poll":      {5, 50},
		"kugou-poll":        {5, 10},
		"sodamusic-poll":    {5, 200},
	} {
		if w[key] != want[0] || e[key] != want[1] {
			t.Errorf("%s 写下/生效=%v/%v，期望 %v/%v", key, w[key], e[key], want[0], want[1])
		}
	}
	if !reflect.DeepEqual(overwritten, []string{"poll"}) {
		t.Errorf("config_overwritten=%v，期望 [poll]", overwritten)
	}
}

// TestConfigEchoPriorPlayerNeverNull prior-player 为 nil（`prior-player: []` 或 `-prior-player ""`）时，
// 两表都必须序列化为 []，不得为 null。
func TestConfigEchoPriorPlayerNeverNull(t *testing.T) {
	cfg := defaultEchoConfig()
	cfg.PriorPlayer = nil
	written, effective, _ := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	for name, om := range map[string]*server.OrderedMap{"config": written, "config_effective": effective} {
		_, v := echoEntries(t, om)
		if arr, ok := v["prior-player"].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s 的 prior-player=%#v，期望空数组 []", name, v["prior-player"])
		}
	}
}

// TestConfigEchoAudioAndStrategy audio 键回显写下值与生效值、显式标记按键序进 config_overwritten；
// 效果策略的写下值与生效值（如 Win11 降级后的 fadeout）分开回显。
func TestConfigEchoAudioAndStrategy(t *testing.T) {
	cfg := defaultEchoConfig()
	cfg.AudioNormalizeTarget = 0 // 写 10，钳为 0
	cfg.WrittenValues = map[string]any{"audio-normalize-target": 10}
	off := false
	cfg.Players["qqmusic"].AudioNormalize = &off
	cfg.EffectStrategy = "park"
	for _, k := range []string{"audio-normalize", "audio-normalize-target", "cloudmusicv3-effect-strategy", "qqmusic-audio-normalize"} {
		cfg.ExplicitKeys[k] = true
	}

	written, effective, overwritten := buildConfigEcho(&cfg, echoPlayers, "fadeout")
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)

	for key, want := range map[string][2]any{
		"audio-normalize":              {true, true},
		"audio-normalize-target":       {float64(10), float64(0)},
		"audio-normalize-max-gain":     {float64(30), float64(30)},
		"cloudmusicv3-effect-strategy": {"park", "fadeout"},
		"qqmusic-audio-normalize":      {false, false},
		"kugou-audio-normalize":        {true, true},
	} {
		if w[key] != want[0] || e[key] != want[1] {
			t.Errorf("%s 写下/生效=%v/%v，期望 %v/%v", key, w[key], e[key], want[0], want[1])
		}
	}
	wantOver := []string{"audio-normalize", "audio-normalize-target", "cloudmusicv3-effect-strategy", "qqmusic-audio-normalize"}
	if !reflect.DeepEqual(overwritten, wantOver) {
		t.Errorf("config_overwritten=%v，期望 %v", overwritten, wantOver)
	}
}
