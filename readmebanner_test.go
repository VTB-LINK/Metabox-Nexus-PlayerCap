package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/applemusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	"Metabox-Nexus-PlayerCap/player/kugou"
	"Metabox-Nexus-PlayerCap/player/kuwo"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/sodamusic"
	"Metabox-Nexus-PlayerCap/player/spotify"
	"Metabox-Nexus-PlayerCap/player/wesing"
)

// TestReadmeExpectedBannerMatchesTemplate 钉住 README.md「### 预期输出」里的配置行与按模板生成的
// config.yml 经真实 config.Load 加载后的横幅输出逐行一致（issue #242）。
//
// 变异自证（隔离 worktree 实测）：README.md 的 kuwo offset 改回 -300ms → 红；删掉任一播放器行 → 红；
// 不先向临时目录写入模板而传空目录给 loadConfigIn → -count=2 第二次运行红。
func TestReadmeExpectedBannerMatchesTemplate(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("读取 README.md 失败: %v", err)
	}
	_, sec, ok := strings.Cut(string(raw), "\n### 预期输出\n")
	if !ok {
		t.Fatal("README.md 里找不到「### 预期输出」一节")
	}
	_, block, ok := strings.Cut(sec, "\n```\n")
	if !ok {
		t.Fatal("README.md「### 预期输出」里找不到代码块起始围栏")
	}
	block, _, ok = strings.Cut(block, "\n```\n")
	if !ok {
		t.Fatal("README.md「### 预期输出」代码块缺少闭合围栏")
	}
	// 跳过固定头（横幅标题、版本行、示例路径行）与底部分隔线，取由 bannerConfigLines 产出的配置行。
	_, gotConfigBlock, ok := strings.Cut(block, "   配置文件: C:\\PlayerCap\\config.yml\n")
	if !ok {
		t.Fatal("README.md「### 预期输出」缺少预期的配置文件行")
	}
	gotConfigBlock, _, ok = strings.Cut(gotConfigBlock, "===========================================================")
	if !ok {
		t.Fatal("README.md「### 预期输出」缺少底部分隔线")
	}

	allPlayers := []string{
		wesing.PlayerName,
		cloudmusic.PlayerName,
		qqmusic.PlayerName,
		kugou.PlayerName,
		sodamusic.PlayerName,
		applemusic.PlayerName,
		spotify.PlayerName,
		kuwo.PlayerName,
	}
	reg := config.RegisteredPlayers()
	slices.Sort(reg)
	sortedAll := slices.Clone(allPlayers)
	slices.Sort(sortedAll)
	if !slices.Equal(sortedAll, reg) {
		t.Fatalf("allPlayers = %v 与已注册播放器 %v 不一致，须同步 main() 的 playerNames", allPlayers, reg)
	}

	i18n.SetLanguage(i18n.Chinese)
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })

	dir := t.TempDir()
	cfgPath := writeConfigIn(t, dir, config.DefaultConfigContent(Version))
	cfg := loadConfigIn(t, dir, Version)
	if cfg.ConfigFile != cfgPath {
		t.Fatalf("ConfigFile = %q，期望 %q", cfg.ConfigFile, cfgPath)
	}
	w, e, o := buildConfigEcho(&cfg, allPlayers, effectiveEffectStrategy(cfg.EffectStrategy, false))
	want := strings.Join(bannerConfigLines(w, e, o, cfg.RejectedValues, allPlayers), "")

	if gotConfigBlock != want {
		t.Errorf("README.md「### 预期输出」配置行与模板加载结果不一致:\n--- README.md ---\n%s\n--- 实际渲染 ---\n%s", gotConfigBlock, want)
	}
}
