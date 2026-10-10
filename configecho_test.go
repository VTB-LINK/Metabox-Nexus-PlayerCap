package main

// 本文件只测 /service-status 配置三件套的组装（buildConfigEcho）：config（写下的值）与
// config_effective（实际生效的值）键集合与顺序一致，播放器层轮询下限计入 config_effective，
// config_overwritten 按同一键序筛出显式设置的键。
//
// 配置层钳制如何记下原值由 config/writtenvalues_test.go 守；本文件直接构造钳制后的 Config。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/server"
)

var echoPlayers = []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic", "applemusic"}

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
		"audio-normalize", "audio-normalize-target", "audio-normalize-max-gain", "audio-frame-rate", "cloudmusicv3-effect-strategy"}
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
		"applemusic-poll":   {30, 50},
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
		"applemusic-poll":   {5, 50},
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

// jsonKind 返回一个回显值按 JSON 解出后的类型：字符串、数值、布尔，或字符串数组。
func jsonKind(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return fmt.Sprintf("%T", v)
	}
	for _, item := range arr {
		if _, ok := item.(string); !ok {
			return fmt.Sprintf("含 %T 的数组", item)
		}
	}
	return "字符串数组"
}

// wantEchoTypes 断言三件套里每个键的类型不随配置的写法变化：config 与 config_effective 的 Go 类型相同，
// 且都与默认配置下的相同；按 JSON 解出后的类型也与默认配置下的相同（按字段类型解码的客户端看到的就是它）。
// 默认配置下各键的类型即 doc/openapi.yaml 给 config 各字段声明的类型：字符串、整数、布尔、字符串数组。
func wantEchoTypes(t *testing.T, desc string, written, effective *server.OrderedMap) {
	t.Helper()
	base := defaultEchoConfig()
	baseWritten, _, _ := buildConfigEcho(&base, echoPlayers, base.EffectStrategy)
	_, baseJSON := echoEntries(t, baseWritten)
	_, gotJSON := echoEntries(t, written)
	for _, k := range baseWritten.Keys() {
		b, _ := baseWritten.Get(k)
		w, _ := written.Get(k)
		e, _ := effective.Get(k)
		if reflect.TypeOf(w) != reflect.TypeOf(b) || reflect.TypeOf(e) != reflect.TypeOf(b) {
			t.Errorf("%s：%s 的类型变了，config 为 %T、config_effective 为 %T，应为 %T", desc, k, w, e, b)
		}
		if got, want := jsonKind(gotJSON[k]), jsonKind(baseJSON[k]); got != want {
			t.Errorf("%s：%s 在 config 里按 JSON 是%s，应为%s", desc, k, got, want)
		}
	}
}

// TestConfigEchoTypesNeverChange /service-status 的 config 与 config_effective 是公开接口，各字段的类型不得随
// config.yml 的写法变化。回显的每个键逐个写成字符串、小数、布尔、空值、列表、映射六种取值（其中该键认不了的
// 被拒，认得的被采纳），经真实的 config.Load 后，三件套里所有键的类型都与默认配置下的相同。键集合取自
// buildConfigEcho 的输出，没有键清单；config 包一侧按 mergeYAML 的源码扫描逐键核对被拒的原值不进
// WrittenValues（config/writtenvalues_test.go 的 TestMergeYAMLRejectedValues）。另以命令行覆盖、钳制、
// 取值非法的特效策略字符串、列表里混有非字符串项作对照。
//
// 变异自证（隔离的 worktree，实测）：mergeYAML 的 audio-frame-rate、poll 分支拒收时改记进 WrittenValues、
// cloudmusicv3-effect-strategy 的非字符串取值改记进 WrittenValues、noteRejected 整个改为记进
// WrittenValues → 红；buildConfigEcho 的 offset 或每播放器 offset 的写下值改读被拒原值 → 红。offset、
// prior-player 与每播放器键的分支改记进 WrittenValues 时本用例不红（这些键的写下值不读 WrittenValues，
// 或读时按整数取），由 config 包的 TestMergeYAMLRejectedValues 拦。
func TestConfigEchoTypesNeverChange(t *testing.T) {
	check := func(desc string, cfg config.Config) {
		t.Helper()
		w, e, _ := buildConfigEcho(&cfg, echoPlayers, effectiveEffectStrategy(cfg.EffectStrategy, false))
		wantEchoTypes(t, desc, w, e)
	}

	base := defaultEchoConfig()
	baseWritten, _, _ := buildConfigEcho(&base, echoPlayers, base.EffectStrategy)
	keys := baseWritten.Keys()
	if len(keys) == 0 {
		t.Fatal("buildConfigEcho 没有给出任何键，本用例失效")
	}
	for _, k := range keys {
		for _, literal := range []string{`"abc"`, `3.5`, `true`, ``, `[1, x]`, `{a: 1}`} {
			yml := k + ": " + literal
			check(yml, loadConfig(t, yml+"\n"))
		}
	}

	check("命令行覆盖被拒的键", loadConfig(t, "offset: \"300\"\naudio-frame-rate: \"30\"\nwesing-poll: fast\n",
		"-offset", "350", "-audio-frame-rate", "40", "-wesing-poll", "50"))
	check("钳制", loadConfig(t, "poll: 5\naudio-normalize-target: 10\naudio-normalize-max-gain: 99\naudio-frame-rate: 100\nkugou-poll: 99999\n"))
	check("命令行的值被钳制", loadConfig(t, "poll: abc\n", "-poll", "5", "-kugou-poll", "1"))
	check("取值非法的特效策略字符串", loadConfig(t, "cloudmusicv3-effect-strategy: foo\n"))
	check("命令行的特效策略取值非法", loadConfig(t, "cloudmusicv3-effect-strategy: 1\n", "-cloudmusicv3-effect-strategy", "foo"))
	check("列表里混有非字符串项", loadConfig(t, "prior-player: [kugou, 1, wesing]\n"))
}

// TestConfigEchoRejectedShowsAdopted 类型不符而被拒的键在三件套里视同未设置：config 与 config_effective
// 都是被采纳的值（默认值，或列表里被采纳的那几项），不进 config_overwritten；写下的原值只上横幅与告警。
// 对照：取值非法的特效策略字符串、被钳制的整数类型没变，照旧在 config 里回显原值。经真实的 config.Load。
//
// 变异自证（隔离的 worktree，实测）：buildConfigEcho 的 offset 或每播放器 offset 的写下值改读被拒原值 → 红；
// mergeYAML 的 audio-frame-rate 分支拒收时改记进 WrittenValues → 红；特效策略的非字符串取值改记进
// WrittenValues、非法字符串改记进 RejectedValues → 红。prior-player 丢项时把原列表记进 WrittenValues
// 不影响回显（该键的写下值不读 WrittenValues），本用例不红，由 config 包的
// TestMergeYAMLPriorPlayerDroppedItems 拦。
func TestConfigEchoRejectedShowsAdopted(t *testing.T) {
	cfg := loadConfig(t, "offset: \"300\"\npoll: 5\nprior-player: [kugou, 1]\naudio-normalize: \"false\"\naudio-frame-rate: \"30\"\n"+
		"cloudmusicv3-effect-strategy: foo\nwesing-offset: abc\nqqmusic-idle-hide: 1.5\nkugou-offset: 430\n")
	written, effective, overwritten := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	wantEchoTypes(t, "多个键被拒", written, effective)
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)

	for key, want := range map[string][2]any{
		"offset":                       {float64(-200), float64(-200)},
		"audio-normalize":              {true, true},
		"audio-frame-rate":             {float64(25), float64(25)},
		"prior-player":                 {[]any{"kugou"}, []any{"kugou"}},
		"wesing-offset":                {float64(-200), float64(-200)},
		"qqmusic-offset":               {float64(-200), float64(-200)},
		"qqmusic-idle-hide":            {float64(0), float64(0)},
		"kugou-offset":                 {float64(430), float64(430)},
		"poll":                         {float64(5), float64(10)},
		"cloudmusicv3-effect-strategy": {"foo", "fadeout"},
	} {
		if !reflect.DeepEqual(w[key], want[0]) || !reflect.DeepEqual(e[key], want[1]) {
			t.Errorf("%s 写下/生效=%#v/%#v，期望 %#v/%#v", key, w[key], e[key], want[0], want[1])
		}
	}
	if want := []string{"poll", "prior-player", "kugou-offset"}; !reflect.DeepEqual(overwritten, want) {
		t.Errorf("config_overwritten=%v，期望 %v：被拒的键没有被采纳，不计入", overwritten, want)
	}

	// 特效策略写成非字符串：与没写一样，两表都是默认的 fadeout。
	cfg = loadConfig(t, "cloudmusicv3-effect-strategy: 1\n")
	written, effective, overwritten = buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	_, w = echoEntries(t, written)
	_, e = echoEntries(t, effective)
	if w["cloudmusicv3-effect-strategy"] != "fadeout" || e["cloudmusicv3-effect-strategy"] != "fadeout" || len(overwritten) != 0 {
		t.Errorf("cloudmusicv3-effect-strategy: 1 的写下/生效=%#v/%#v、config_overwritten=%v，期望 fadeout/fadeout、空",
			w["cloudmusicv3-effect-strategy"], e["cloudmusicv3-effect-strategy"], overwritten)
	}
}

// TestConfigEchoAudioFrameRate 帧率回显：默认 25/25，不进 config_overwritten；写 100（钳为 60）时
// config 回显写下的 100、config_effective 回显 60，显式标记进 config_overwritten。
//
// 变异自证（隔离副本，实测）：buildConfigEcho 删掉 audio-frame-rate 一行 → 本用例与
// TestConfigEchoKeysMatch 红；写下值也取运行值（不经 cfg.Written）→ 100/60 一例得 60/60，红。
func TestConfigEchoAudioFrameRate(t *testing.T) {
	cfg := defaultEchoConfig()
	written, effective, overwritten := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)
	if w["audio-frame-rate"] != float64(25) || e["audio-frame-rate"] != float64(25) {
		t.Errorf("默认写下/生效=%v/%v，期望 25/25", w["audio-frame-rate"], e["audio-frame-rate"])
	}
	if len(overwritten) != 0 {
		t.Errorf("默认配置下 config_overwritten=%v，期望为空", overwritten)
	}

	cfg = defaultEchoConfig()
	cfg.AudioFrameRate = 60 // 写 100，钳为 60
	cfg.WrittenValues = map[string]any{"audio-frame-rate": 100}
	cfg.ExplicitKeys["audio-frame-rate"] = true
	written, effective, overwritten = buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	_, w = echoEntries(t, written)
	_, e = echoEntries(t, effective)
	if w["audio-frame-rate"] != float64(100) || e["audio-frame-rate"] != float64(60) {
		t.Errorf("写下/生效=%v/%v，期望 100/60", w["audio-frame-rate"], e["audio-frame-rate"])
	}
	if !reflect.DeepEqual(overwritten, []string{"audio-frame-rate"}) {
		t.Errorf("config_overwritten=%v，期望 [audio-frame-rate]", overwritten)
	}
}
