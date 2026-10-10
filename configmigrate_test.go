package main

// 本文件只测 config.yml 的版本处理经真实的 config.Load 之后对外的样子：v3 的文件迁移后回显的 offset、
// 被改写的文件与备份、启动横幅配置文件行上的附注。迁移各层的细节由 config 包的用例守。

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
)

const v3ConfigText = "addr: \"127.0.0.1:9000\"\n# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后\noffset: 200\n" +
	"cloudmusicv3-offset: 500\n# kugou-offset: 430\nwesing-offset: 0\n"

func writeConfigIn(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// echoOffsets 返回回显（config 与 config_effective 相同）里各 offset 键的取值。
func echoOffsets(t *testing.T, cfg *config.Config) map[string]any {
	t.Helper()
	written, effective, _ := buildConfigEcho(cfg, echoPlayers, cfg.EffectStrategy)
	_, w := echoEntries(t, written)
	_, e := echoEntries(t, effective)
	out := map[string]any{}
	for key, v := range w {
		if key == "offset" || strings.HasSuffix(key, "-offset") {
			if !reflect.DeepEqual(v, e[key]) {
				t.Errorf("%s 写下的值 %v 与生效值 %v 不同", key, v, e[key])
			}
			out[key] = v
		}
	}
	return out
}

// wantPathAndBackupApart 断言配置文件行（可能多行）的任何一行里都不同时出现 config.yml 的完整路径与备份
// 的文件名：两者接在同一行会超出终端宽度而折行。
func wantPathAndBackupApart(t *testing.T, desc, text, path, backupName string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, path) && strings.Contains(line, backupName) {
			t.Errorf("%s：config.yml 的路径与备份文件名不应在同一行：\n%s", desc, line)
		}
	}
}

// 没有版本键的 config.yml（v3）经真实的 Load：回显的 offset 是取反后的值（没写的播放器继承取反后的
// 全局值），文件已改写并带上版本号，原文件留作备份，配置文件行之下另起一行加注；再启动一次不再改写、
// 不再加注。命令行传入的 offset 不经文件，按新语义原样采用，不换算。
//
// 变异自证（隔离的 worktree，实测）：Load 不经 upgradeFile（直接合并解析结果）→ 红（回显是 200 / 500）；
// 命令行的 offset 也取反 → 红；横幅不加改写结果的附注 → 红；附注接回路径行 → 红。
func TestLoadMigratesV3Config(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigIn(t, dir, v3ConfigText)
	cfg := loadConfigIn(t, dir, "4.0.0")

	want := map[string]any{
		"offset": float64(-200), "wesing-offset": float64(0), "cloudmusicv3-offset": float64(-500),
		"qqmusic-offset": float64(-200), "kugou-offset": float64(-200), "sodamusic-offset": float64(-200), "applemusic-offset": float64(-200),
	}
	if got := echoOffsets(t, &cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("回显的 offset = %v，期望 %v", got, want)
	}
	data, _ := os.ReadFile(path)
	for _, line := range []string{"config-version: \"v4.0.0\"\n", "\noffset: -200\n", "\ncloudmusicv3-offset: -500\n", "\n# kugou-offset: -430\n", "\nwesing-offset: 0\n", "正值=延后，负值=提前\n"} {
		if !strings.Contains(string(data), line) {
			t.Errorf("改写后的 config.yml 应含 %q：\n%s", line, data)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "config.backup-v3-*.yml"))
	if len(backups) != 1 || backups[0] != cfg.FileBackup {
		t.Fatalf("应留下一份 v3 的备份并记进 Config：%v / %q", backups, cfg.FileBackup)
	}
	if original, _ := os.ReadFile(backups[0]); string(original) != v3ConfigText {
		t.Errorf("备份应是原文件的字节")
	}
	wantLine := "   配置文件: " + path + "\n      v3 → v4.0.0，已迁移，原文件备份为 " + filepath.Base(backups[0]) + "\n"
	line := bannerConfigFileLine(&cfg)
	if line != wantLine {
		t.Errorf("配置文件行\n得   %q\n期望 %q", line, wantLine)
	}
	wantPathAndBackupApart(t, "已迁移", line, path, filepath.Base(backups[0]))

	again := loadConfigIn(t, dir, "4.0.0")
	if got := echoOffsets(t, &again); !reflect.DeepEqual(got, want) {
		t.Errorf("再启动一次回显的 offset = %v，期望不变", got)
	}
	if got, wantLine := bannerConfigFileLine(&again), "   配置文件: "+path+"\n"; got != wantLine {
		t.Errorf("再启动一次配置文件行不应加注\n得   %q", got)
	}
	if after, _ := os.ReadFile(path); string(after) != string(data) {
		t.Errorf("再启动一次不应改写文件")
	}

	// 命令行的值按新语义原样采用：-offset 300 即延后 300ms；文件里的值照常迁移。
	dir2 := t.TempDir()
	writeConfigIn(t, dir2, v3ConfigText)
	cli := loadConfigIn(t, dir2, "4.0.0", "-offset", "300", "-kugou-offset=-50")
	got := echoOffsets(t, &cli)
	if got["offset"] != float64(300) || got["kugou-offset"] != float64(-50) || got["cloudmusicv3-offset"] != float64(-500) || got["qqmusic-offset"] != float64(300) {
		t.Errorf("命令行覆盖后回显的 offset = %v，期望 offset=300、kugou=-50、cloudmusicv3=-500、其余继承 300", got)
	}
}

// 文本层认不了的写法：文件不动，回显的仍是取反后的值，配置文件行之下另起一行注明本次在内存里换算。
func TestLoadConvertsInMemoryWhenFileCannotBeRewritten(t *testing.T) {
	const content = "offset: 0x1f4\n"
	dir := t.TempDir()
	path := writeConfigIn(t, dir, content)
	cfg := loadConfigIn(t, dir, "4.0.0")
	if got := echoOffsets(t, &cfg)["offset"]; got != float64(-500) {
		t.Errorf("回显的 offset = %v，期望 -500", got)
	}
	if data, _ := os.ReadFile(path); string(data) != content {
		t.Errorf("文件应不变，实际 %q", data)
	}
	want := "   配置文件: " + path + "\n      v3 → v4.0.0：应迁移而未能写入，本次在内存中换算\n"
	if got := bannerConfigFileLine(&cfg); got != want {
		t.Errorf("配置文件行\n得   %q\n期望 %q", got, want)
	}
}

// 配置文件行之下另起一行（缩进 6 个空格）的两类附注，中英文各一份：版本键与自动迁移的结果、按模板生成
// 而目录里另有备份的提示。路径行上只留加载结果的短注；不需要附注时仍只有路径那一行；应改而未写成、
// 又留有备份时，备份的文件名再起一行。任何一行里都不同时出现 config.yml 的路径与备份的文件名。
// Config 照 Load 返回时的样子直接构造。
//
// 变异自证（隔离的 worktree，实测）：附注对调迁移与只改版本键 → 红；未写成时不列留下的备份 → 两例红；
// 按模板生成而目录里有备份时不加注 → 红；中文译表缺一条附注 → 该例中文红；版本键与迁移的附注接回
// 路径行 → 有这类附注的八例红；目录里另有备份的提示接回路径行 → 该例红（后两条里带备份文件名的各例，
// 逐字比较与「不在同一行」都红）。
func TestBannerConfigFileRewriteNotes(t *testing.T) {
	const path = `C:\PlayerCap\config.yml`
	const backup = `C:\PlayerCap\config.backup-v3-20261009-153045-1234567890.yml`
	const name = "config.backup-v3-20261009-153045-1234567890.yml"
	cases := []struct {
		desc   string
		set    func(c *config.Config)
		zh, en string
	}{
		{"不需要改写", func(c *config.Config) {
			c.FileVersionLabel, c.FileStamp = "v4.0.0", "v4.0.0"
		}, "", ""},
		{"只改版本键", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp = config.RewriteStamped, "v4.0.0", "v4.0.1"
		}, "\n      v4.0.0 → v4.0.1", "\n      v4.0.0 → v4.0.1"},
		{"版本键非法、已重置", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp = config.RewriteStamped, "unknown", "v4.0.1"
		}, "\n      unknown → v4.0.1", "\n      unknown → v4.0.1"},
		{"已迁移", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp, c.FileBackup = config.RewriteMigrated, "v3", "v4.0.0", backup
		}, "\n      v3 → v4.0.0，已迁移，原文件备份为 " + name, "\n      v3 → v4.0.0, migrated; original kept as " + name},
		{"应迁移而未写成", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp = config.RewriteMigratePending, "v3", "v4.0.0-0"
		}, "\n      v3 → v4.0.0-0：应迁移而未能写入，本次在内存中换算", "\n      v3 → v4.0.0-0: migration could not be written; converted in memory for this run"},
		{"应迁移而未写成、留有备份", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp, c.FileBackup = config.RewriteMigratePending, "v3", "v4.0.0", backup
		}, "\n      v3 → v4.0.0：应迁移而未能写入，本次在内存中换算\n      原先的文件留在 " + name,
			"\n      v3 → v4.0.0: migration could not be written; converted in memory for this run\n      the previous file is kept as " + name},
		{"应改版本键而未写成", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp = config.RewriteStampPending, "v4.0.0", "v4.0.1"
		}, "\n      v4.0.0 → v4.0.1 未能写入", "\n      v4.0.0 → v4.0.1 could not be written"},
		{"应改版本键而未写成、留有备份", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp, c.FileBackup = config.RewriteStampPending, "v4.0.0", "v4.0.1", backup
		}, "\n      v4.0.0 → v4.0.1 未能写入\n      原先的文件留在 " + name, "\n      v4.0.0 → v4.0.1 could not be written\n      the previous file is kept as " + name},
		{"文件版本高于程序", func(c *config.Config) {
			c.FileRewrite, c.FileVersionLabel, c.FileStamp = config.RewriteNewerFile, "v4.2.0", "v4.0.0"
		}, "\n      文件版本 v4.2.0 高于本程序，按当前程序的语义读取", "\n      written by a newer version v4.2.0; read as is"},
		{"按模板生成", func(c *config.Config) {
			c.ConfigFileState, c.FileVersionLabel, c.FileStamp = config.FileGenerated, "v4.0.0", "v4.0.0"
		}, "（本次新生成）", " (newly generated)"},
		{"按模板生成而目录里有备份", func(c *config.Config) {
			c.ConfigFileState, c.OrphanBackup = config.FileGenerated, name
		}, "（本次新生成）\n      目录里另有备份 " + name + "，原有设置可能在其中",
			" (newly generated)\n      the backup " + name + " in this folder may hold your previous settings"},
		{"正常加载时不提旧的备份", func(c *config.Config) { c.OrphanBackup = name }, "", ""},
	}
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
	for _, c := range cases {
		cfg := defaultEchoConfig()
		cfg.ConfigFile = path
		c.set(&cfg)
		i18n.SetLanguage(i18n.Chinese)
		got := bannerConfigFileLine(&cfg)
		if want := "   配置文件: " + path + c.zh + "\n"; got != want {
			t.Errorf("%s\n得   %q\n期望 %q", c.desc, got, want)
		}
		wantPathAndBackupApart(t, c.desc, got, path, name)
		i18n.SetLanguage(i18n.English)
		got = bannerConfigFileLine(&cfg)
		if want := "   Config file: " + path + c.en + "\n"; got != want {
			t.Errorf("%s（英文）\n得   %q\n期望 %q", c.desc, got, want)
		}
		wantPathAndBackupApart(t, c.desc+"（英文）", got, path, name)
	}
}
