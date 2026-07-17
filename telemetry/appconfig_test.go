package telemetry

// 本文件只测一件事：**自身信息（版本/路径/config）采得对不对**。
// 系统信息在 compatshim_test.go / timezonekey_test.go，Init 的接线在 initwiring_test.go。

import (
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
)

// TestOverwrittenIsDeterministic 钉死 Overwritten 是**确定序**的。
//
// Go 的 map 迭代序是随机的。不排序的话，同一台机器每次启动都会得到不同顺序的 overwritten
// 列表 —— 在 Sentry 里看着就像「主播反复在改配置」，而实际上他什么都没动。
// 本仓库在 config 的 clampPolls 与 router 的 evaluateGroup 里已经各踩过一次这个坑。
//
// 用 14 个 key（正是 main.go 那份清单的规模：6 个全局 + 4 个播放器 ×2）——
// key 越多，「随机顺序恰好等于字典序」的概率越低，这个门禁就越不容易假绿。
//
// 变异自证：删掉 collectAppInfo 里的 sort.Strings 即红。
func TestOverwrittenIsDeterministic(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, k := range []string{
		"addr", "offset", "poll", "prior-player", "prior-player-expire",
		"cloudmusicv3-effect-strategy",
		"wesing-offset", "wesing-poll", "kugou-offset", "kugou-poll",
		"qqmusic-offset", "qqmusic-poll", "cloudmusicv3-offset", "cloudmusicv3-poll",
	} {
		cfg.ExplicitKeys[k] = true
	}

	want := []string{
		"addr", "cloudmusicv3-effect-strategy", "cloudmusicv3-offset", "cloudmusicv3-poll",
		"kugou-offset", "kugou-poll", "offset", "poll",
		"prior-player", "prior-player-expire", "qqmusic-offset", "qqmusic-poll",
		"wesing-offset", "wesing-poll",
	}

	// 跑很多次：map 迭代序每次随机，没排序的话这里迟早（且很快）会抓到一个不同的顺序。
	for i := 0; i < 100; i++ {
		got := collectAppInfo("1.0.0", cfg, nil).Overwritten
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("第 %d 次采集的顺序不是字典序：\n got: %v\nwant: %v\n"+
				"map 迭代序随机 —— 不 sort 的话，Sentry 里会看着像主播每次启动都在改配置", i, got, want)
		}
	}
}

// TestOverwrittenOnlyIncludesExplicit ExplicitKeys 里值为 false 的不算改过。
//
// 那个 map 的语义是「这个键被显式设置过吗」，false 表示「有这个键但没被显式设」。
// 把 false 也算进去，等于报告主播改了他没改的东西。
func TestOverwrittenOnlyIncludesExplicit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ExplicitKeys["addr"] = true
	cfg.ExplicitKeys["poll"] = false // 有键，但没被显式设

	got := collectAppInfo("1.0.0", cfg, nil).Overwritten
	if !reflect.DeepEqual(got, []string{"addr"}) {
		t.Errorf("Overwritten = %v，want [addr] —— 值为 false 的键不算改过", got)
	}
}

// TestPlayersUseResolvedValuesForAllNames 钉死**每个播放器都有条目，且用的是 resolved 值**。
//
// 两个都容易写错：
//   - 遍历 cfg.Players 而不是 playerNames → **没被显式配过的播放器直接从上报里消失**，
//     而那恰恰是绝大多数情况（默认配置下 cfg.Players 是空的）。查故障时看到「只有 wesing」
//     会以为另外三个没启用。
//   - 读 cfg.Players[name].Offset 而不是 cfg.GetPlayerOffset(name) → 未配专属值时拿到 nil/0，
//     而实际生效的是全局值。
//
// 变异自证：把遍历改成 `for name := range cfg.Players` 即红（kugou 会消失）。
func TestPlayersUseResolvedValuesForAllNames(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Offset = 200
	cfg.Poll = 30
	wesingOffset := 500
	cfg.Players["wesing"] = &config.PlayerConfig{Offset: &wesingOffset} // 只配 offset，不配 poll

	a := collectAppInfo("1.0.0", cfg, []string{"wesing", "kugou"})

	players, ok := a.Config["players"].(map[string]any)
	if !ok {
		t.Fatalf("config.players 不是 map，实为 %T", a.Config["players"])
	}

	w, ok := players["wesing"].(map[string]any)
	if !ok {
		t.Fatal("wesing 没有条目")
	}
	if w["offset"] != 500 {
		t.Errorf("wesing.offset = %v，want 500（专属值）", w["offset"])
	}
	if w["poll"] != 30 {
		t.Errorf("wesing.poll = %v，want 30（没配专属值，回落到全局）", w["poll"])
	}

	k, ok := players["kugou"].(map[string]any)
	if !ok {
		t.Fatal("kugou 没有条目 —— 未显式配置的播放器也必须出现，" +
			"否则默认配置下的上报里一个播放器都没有，看着像全都没启用")
	}
	if k["offset"] != 200 || k["poll"] != 30 {
		t.Errorf("kugou = %v，want offset=200 poll=30（全局值）", k)
	}
}

// TestExePathNotRedacted 钉死 exe 路径**不做脱敏**。
//
// 明确的产品决定（2026-07-17）：路径含 Windows 用户名，但那正是有用的部分 ——
// 装在哪个盘、是不是 OneDrive 同步目录、路径里有没有中文，都是真实排查过的方向。
//
// 这条测试存在的意义是防止有人「好心」加脱敏：那会把上面那些线索一起抹掉。
// 要改这个决定，先改这条测试，别偷偷加。
func TestExePathNotRedacted(t *testing.T) {
	a := collectAppInfo("1.0.0", config.DefaultConfig(), nil)

	if a.ExePath == "" {
		t.Skip("这台机器拿不到 exe 路径")
	}
	if strings.Contains(a.ExePath, "<user>") || strings.Contains(a.ExePath, "***") {
		t.Errorf("exe 路径 = %q，含脱敏占位符 —— 明确决定过不脱敏，见本测试的注释", a.ExePath)
	}
	// go test 的二进制跑在临时目录，路径形状与真实安装不同，所以只断言「是个绝对路径」。
	if !strings.Contains(a.ExePath, `\`) && !strings.Contains(a.ExePath, "/") {
		t.Errorf("exe 路径 = %q，不像个路径", a.ExePath)
	}
}

// TestCustomizedTagReflectsOverwritten config.customized 这个 tag 要如实反映有没有改过配置。
func TestCustomizedTagReflectsOverwritten(t *testing.T) {
	if got := boolTag(false); got != "false" {
		t.Errorf("boolTag(false) = %q，want \"false\"", got)
	}
	if got := boolTag(true); got != "true" {
		t.Errorf("boolTag(true) = %q，want \"true\"", got)
	}

	clean := collectAppInfo("1.0.0", config.DefaultConfig(), nil)
	if len(clean.Overwritten) != 0 {
		t.Errorf("默认配置不该有 overwritten 项，实得 %v", clean.Overwritten)
	}
}
