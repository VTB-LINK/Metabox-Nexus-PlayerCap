package config

// 本文件只测强制重建的动作（ForceRebuild）：旧文件改名为备份、模板放置为 config.yml，各步失败与崩溃
// 之后磁盘上的文件，以及提示所需的差异（被替换的旧文件与模板取值不同的键，原值换算成当前语义）。
// 判定见 rebuilddecision_test.go，两个实例交错见 concurrentinstances_test.go。

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// diffMap 把差异收成「键 → [原值, 模板的值]」，模板里没有的键第二项为 "<无>"。
func diffMap(diffs []KeyDiff) map[string][2]any {
	out := map[string][2]any{}
	for _, d := range diffs {
		v := d.New
		if !d.InTemplate {
			v = "<无>"
		}
		out[d.Key] = [2]any{d.Old, v}
	}
	return out
}

// 重建不看文件是否变过：Load 迁移（原文件改名为备份）之后把 addr 改掉再重建，重建留下的备份里是改过的
// 那一份；此时目录里有两份备份，名字里的版本分别是 v3 与盖章值。config.yml 是当前版本的模板。
//
// 变异自证（隔离的 worktree，实测）：重建时也做比对（与别的内容不同即放弃）→ 红；备份名恒取改写前的
// 版本 → 红（两份备份都叫 v3）。
func TestForceRebuildBacksUpWhatIsInPlace(t *testing.T) {
	s := newSandbox(t, "offset: 200\naddr: \"127.0.0.1:8765\"\n")
	cfg, _ := s.load("4.0.0", testOps())
	if cfg.FileRewrite != RewriteMigrated {
		t.Fatalf("前置：应已迁移，改写=%d", cfg.FileRewrite)
	}
	edited := strings.Replace(s.read(), "127.0.0.1:8765", "127.0.0.1:9000", 1)
	s.write(edited) // Load 之后、重建之前被保存过

	var rep RebuildReport
	logs := captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, "4.0.0") })
	if !rep.Done || s.read() != templateContent("v4.0.0") {
		t.Fatalf("重建应成功且 config.yml 是 v4.0.0 的模板：%+v", rep)
	}
	s.wantFiles(configName, "config.backup-v3-*.yml", "config.backup-v4.0.0-*.yml")
	if got := readTestFile(t, rep.Backup); got != edited || !strings.Contains(got, "9000") {
		t.Errorf("重建留下的备份应是此刻在位的那一份（含 9000），实际\n%s", got)
	}
	if readTestFile(t, cfg.FileBackup) != "offset: 200\naddr: \"127.0.0.1:8765\"\n" {
		t.Errorf("迁移留下的备份应仍是最初的原文件")
	}
	if rep.BackupLegacy || !rep.DiffKnown {
		t.Errorf("被替换的是迁移后的文件，应报告为新写法且差异可知：%+v", rep)
	}
	want := map[string][2]any{"addr": {"127.0.0.1:9000", "0.0.0.0:8765"}}
	if got := diffMap(rep.Diffs); !reflect.DeepEqual(got, want) {
		t.Errorf("差异 = %v，期望 %v（offset 迁移后是 -200，与模板相同，不列）", got, want)
	}
	if !containsAll(logs, "重建", rep.Backup) {
		t.Errorf("日志应说明已重建并给出备份\n%s", logs)
	}

	// 重建之后再启动：正常加载模板，不再改写。
	again, _ := s.load("4.0.0", noWrites(t))
	if again.FileRewrite != RewriteNone || again.FileVersion != "v4.0.0" || again.Offset != -200 {
		t.Errorf("重建后再启动：改写=%d、文件版本=%q、offset=%d", again.FileRewrite, again.FileVersion, again.Offset)
	}
}

// 备份名里的版本取文件此刻在磁盘上的版本：Load 只改了版本键的是盖章值，Load 没写成的仍是改写前的版本
// （没有版本键写作 v3，版本键非法写作 unknown）。各留一份备份。
func TestForceRebuildBackupLabel(t *testing.T) {
	for _, c := range []struct{ name, content, version, glob string }{
		{"Load 只改了版本键", "config-version: \"v4.0.0\"\noffset: -5\n", "4.0.1", "config.backup-v4.0.1-*.yml"},
		{"Load 没写成：十六进制的 offset", "offset: 0x1f4\n", "4.0.0", "config.backup-v3-*.yml"},
		{"Load 没写成：版本键带引号", "\"config-version\": \"v4.0.0-rc.1\"\noffset: -5\n", "4.0.0", "config.backup-v4.0.0-rc.1-*.yml"},
		{"不需要改写", "config-version: \"v4.0.0\"\noffset: -5\n", "4.0.0", "config.backup-v4.0.0-*.yml"},
		{"版本键非法、Load 已重置", "config-version: 4.0\noffset: -5\n", "4.0.0", "config.backup-v4.0.0-*.yml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, c.content)
			cfg, _ := s.load(c.version, testOps())
			onDisk := s.read()
			var rep RebuildReport
			captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, c.version) })
			if !rep.Done || readTestFile(t, rep.Backup) != onDisk {
				t.Fatalf("重建应成功且备份是重建前在位的那一份：%+v", rep)
			}
			s.wantFiles(configName, c.glob)
		})
	}
}

// 提示里的原值是当前语义下的值（设计 §5.6），备份是哪种写法按它自己的版本键判断。
//
// 变异自证（隔离的 worktree，实测）：原值直接取磁盘上的字面（不做值层迁移）→「未落盘的旧写法」「v3 的
// 文件原样」红（列出 500 而不是 -500）；对已是新写法的备份再迁一遍 →「迁移已落盘」「已是新写法」等
// 带新版本键的各例红；模板里没有的键不列 →「模板里没有的键」红；与模板相同的键也列 →「与模板完全
// 相同」等三例红；版本键也列 → 版本键与模板不同的三例红。备份的语义按名字判断（名字写着 v3 即旧写法）
// 在本函数里无从写起，注入在 forceRebuild 里，由 TestForceRebuildReportsDiffInCurrentSemantics 的「备份名
// 写着 v3 而内容已带新版本键」拦下。
func TestDiffAgainstTemplate(t *testing.T) {
	template := []byte(templateContent("v4.0.0"))
	cases := []struct {
		name   string
		old    string
		want   map[string][2]any
		legacy bool
	}{
		{"未落盘的旧写法：原值换算成新语义", "offset: 0x1f4\nkugou-offset: 430\n",
			map[string][2]any{"offset": {-500, -200}}, true}, // kugou 的 430 换算后是 -430，与模板相同
		{"v3 的文件原样", "offset: 500\ncloudmusicv3-offset: 300\npoll: 50\n",
			map[string][2]any{"offset": {-500, -200}, "cloudmusicv3-offset": {-300, -500}, "poll": {50, 30}}, true},
		{"迁移已落盘：与字面相同", "config-version: \"v4.0.0\"\noffset: -500\nkugou-offset: -430\n",
			map[string][2]any{"offset": {-500, -200}}, false},
		{"已是新写法：正值即延后，不再换算", "config-version: \"v4.0.0-beta.3\"\noffset: 300\n",
			map[string][2]any{"offset": {300, -200}}, false},
		{"名字是 v3、内容已带新版本键", "config-version: \"v4.0.0-0\"\noffset: 200\n",
			map[string][2]any{"offset": {200, -200}}, false},
		{"版本键非法：按已处理过对待，不换算", "config-version: 4.0\noffset: 200\n",
			map[string][2]any{"offset": {200, -200}}, false},
		{"模板里没有的键", "config-version: \"v4.0.0\"\nwesing-offset: 0\ncloudmusicv3-poll: 80\nmy-note: hi\n",
			map[string][2]any{"wesing-offset": {0, "<无>"}, "cloudmusicv3-poll": {80, "<无>"}, "my-note": {"hi", "<无>"}}, false},
		{"与模板完全相同", string(template), map[string][2]any{}, false},
		{"列表与布尔", "config-version: \"v4.0.0\"\nprior-player: [wesing, kugou]\naudio-normalize: false\n",
			map[string][2]any{"prior-player": {[]interface{}{"wesing", "kugou"}, []interface{}{"wesing"}}, "audio-normalize": {false, true}}, false},
		{"类型不符的值照列", "config-version: \"v4.0.0\"\noffset: \"300\"\n",
			map[string][2]any{"offset": {"300", -200}}, false},
		{"空文件", "", map[string][2]any{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diffs, legacy, ok := diffAgainstTemplate([]byte(c.old), template, realPlayers)
			if !ok {
				t.Fatal("应解析得了")
			}
			if got := diffMap(diffs); !reflect.DeepEqual(got, c.want) {
				t.Errorf("差异 = %v，期望 %v", got, c.want)
			}
			if legacy != c.legacy {
				t.Errorf("旧写法 = %v，期望 %v", legacy, c.legacy)
			}
		})
	}

	t.Run("解析不了时不列差异", func(t *testing.T) {
		if diffs, _, ok := diffAgainstTemplate([]byte("poll: [1, 2\n"), template, realPlayers); ok || diffs != nil {
			t.Errorf("解析不了应返回 ok=false、不列差异，实际 %v, %v", diffs, ok)
		}
	})
	t.Run("模板里有的键按模板的键序，其余按名字排在后", func(t *testing.T) {
		diffs, _, _ := diffAgainstTemplate([]byte("config-version: \"v4.0.0\"\nzz: 1\nkugou-offset: 1\naa: 2\npoll: 99\naddr: x\n"), template, realPlayers)
		var keys []string
		for _, d := range diffs {
			keys = append(keys, d.Key)
		}
		if want := []string{"addr", "poll", "kugou-offset", "aa", "zz"}; !reflect.DeepEqual(keys, want) {
			t.Errorf("键序 = %v，期望 %v", keys, want)
		}
	})
}

// 文本层认不了的 offset: 0x1f4（Load 没有落盘）之后强制重建：列出的原值是 -500 而不是 500，并报告
// 重建留下的备份是旧写法。备份解析不了时不列差异。
func TestForceRebuildReportsDiffInCurrentSemantics(t *testing.T) {
	s := newSandbox(t, "offset: 0x1f4\naddr: \"127.0.0.1:9000\"\n")
	cfg, _ := s.load("4.0.0", testOps())
	if cfg.FileRewrite != RewriteMigratePending || cfg.Offset != -500 {
		t.Fatalf("前置：Load 应没有落盘而在内存里换算，改写=%d offset=%d", cfg.FileRewrite, cfg.Offset)
	}
	var rep RebuildReport
	captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, "4.0.0") })
	want := map[string][2]any{"offset": {-500, -200}, "addr": {"127.0.0.1:9000", "0.0.0.0:8765"}}
	if got := diffMap(rep.Diffs); !rep.Done || !rep.DiffKnown || !rep.BackupLegacy || !reflect.DeepEqual(got, want) {
		t.Errorf("重建结果 %+v，差异 %v，期望 %v 且备份是旧写法", rep, got, want)
	}

	t.Run("备份名写着 v3 而内容已带新版本键", func(t *testing.T) {
		s := newSandbox(t, "offset: 0x1f4\n")
		cfg, _ := s.load("4.0.0", testOps())                   // 没有落盘：磁盘上的版本仍按 v3
		s.write("config-version: \"v4.0.0-0\"\noffset: 200\n") // Load 之后被存成了新写法的内容
		var rep RebuildReport
		captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, "4.0.0") })
		want := map[string][2]any{"offset": {200, -200}}
		if got := diffMap(rep.Diffs); !rep.Done || rep.BackupLegacy || !reflect.DeepEqual(got, want) || !strings.Contains(rep.Backup, "config.backup-v3-") {
			t.Errorf("备份 %s 的内容是新写法：应报告为新写法、原值照字面 200，实际 %+v", rep.Backup, rep)
		}
	})

	t.Run("备份解析不了", func(t *testing.T) {
		s := newSandbox(t, "config-version: \"v4.0.0-0\"\noffset: -5\n")
		cfg, _ := s.load("4.0.0", testOps())
		s.write("poll: [1, 2\n") // Load 之后被存成了解析不了的内容
		var rep RebuildReport
		captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, "4.0.0") })
		if !rep.Done || rep.DiffKnown || len(rep.Diffs) != 0 || readTestFile(t, rep.Backup) != "poll: [1, 2\n" {
			t.Errorf("应照常重建、不列差异，备份是那份解析不了的内容：%+v", rep)
		}
	})
}

// 各步失败（设计 §3.6 强制重建的表）：不重建，告警里给出人工补救的办法与备份的路径。
//
// 变异自证（隔离的 worktree，实测）：失败时也报告 Done → 各例红；放置时 config.yml 已存在而覆盖它 →
// 「放置时 config.yml 已存在」红。
func TestForceRebuildFailures(t *testing.T) {
	const content = "config-version: \"v4.0.0-0\"\noffset: -5\n"
	for _, c := range []struct {
		name       string
		prepare    func(s *sandbox, ops *fileOps)
		config     string // 重建之后 config.yml 的内容；"-" 表示不存在
		backup     bool   // 是否留下备份
		wantInLogs []string
	}{
		{"写临时文件失败", func(s *sandbox, ops *fileOps) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errInjected }
		}, content, false, []string{"没有留下备份", "删除", "重启"}},
		{"改名失败", func(s *sandbox, ops *fileOps) {
			ops.rename = func(string, string) error { return errInjected }
		}, content, false, []string{"没有留下备份", "删除", "重启"}},
		{"放置时 config.yml 已存在", func(s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepRenamed {
					s.write("offset: 9\n")
				}
			}
		}, "offset: 9\n", true, []string{"不予覆盖", "删除", "重启", "config.backup-v4.0.0-"}},
		// 模板放不上去（硬链接与不覆盖的改名都失败）：重建不把备份放回，没有 config.yml，下次启动按模板生成。
		{"放置失败：不放回", func(s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}, "-", true, []string{"未能写入新文件", "下次启动时按模板生成", "config.backup-v4.0.0-"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, content)
			cfg, _ := s.load("4.0.0", testOps()) // 只改版本键：磁盘上是 v4.0.0
			onDisk := s.read()
			if c.config == content {
				c.config = onDisk
			}
			ops := testOps()
			c.prepare(s, ops)
			var rep RebuildReport
			logs := captureLog(t, func() { rep = forceRebuild(ops, &cfg, "4.0.0") })
			if rep.Done {
				t.Error("不应报告已重建")
			}
			files := []string{}
			if c.config == "-" {
				if s.exists() {
					t.Errorf("不应有 config.yml，实际 %q", s.read())
				}
			} else {
				files = append(files, configName)
				if s.read() != c.config {
					t.Errorf("config.yml = %q，期望 %q", s.read(), c.config)
				}
			}
			if c.backup {
				files = append(files, "config.backup-v4.0.0-*.yml")
				if rep.Backup == "" || readTestFile(t, rep.Backup) != onDisk {
					t.Errorf("备份 %q 应是被移走的那一份", rep.Backup)
				}
			} else if rep.Backup != "" {
				t.Errorf("文件没动时不应报告备份：%q", rep.Backup)
			}
			s.wantFiles(files...)
			if !containsAll(logs, append(c.wantInLogs, "[!]")...) {
				t.Errorf("告警应含 %v\n%s", c.wantInLogs, logs)
			}
		})
	}
}

// 各步之后崩溃（设计 §3.6 强制重建的表）：磁盘上的文件，与下一次启动的结果。
func TestForceRebuildCrashThenRestart(t *testing.T) {
	const content = "config-version: \"v4.0.0\"\noffset: -5\naddr: \"127.0.0.1:9000\"\n"
	template := templateContent("v4.0.0")
	tmp, bak := configName+".*.tmp", "config.backup-v4.0.0-*.yml"
	for _, c := range []struct {
		step   string
		config string // 崩溃后 config.yml 的内容；"-" 表示不存在
		files  []string
		next   string // 下次启动：unchanged 原文件照常加载（不再重建）、generate 按模板生成、template 加载模板
	}{
		{stepTempWritten, content, []string{configName, tmp}, "unchanged"},
		{stepBackupReserved, content, []string{configName, tmp, bak}, "unchanged"},
		{stepRenamed, "-", []string{tmp, bak}, "generate"},
		{stepPlaced, template, []string{configName, tmp, bak}, "template"},
		{stepTempRemoved, template, []string{configName, bak}, "template"},
	} {
		t.Run(c.step, func(t *testing.T) {
			s := newSandbox(t, content)
			cfg, _ := s.load("4.0.0", testOps())
			if !crashed(func() { captureLog(t, func() { forceRebuild(crashAfter(c.step), &cfg, "4.0.0") }) }) {
				t.Fatalf("没有走到 %s 这一步", c.step)
			}
			if c.config == "-" {
				if s.exists() {
					t.Errorf("崩溃后不应有 config.yml")
				}
			} else if s.read() != c.config {
				t.Errorf("崩溃后 config.yml = %q", s.read())
			}
			s.wantFiles(c.files...)

			next, _ := s.load("4.0.0", testOps())
			switch c.next {
			case "unchanged":
				// 文件版本已是 V：门槛不再成立（设计 §5.7，已接受）。
				if next.Offset != -5 || next.ConfigFileState != FileLoaded || next.ShouldRebuild("v4.0.0", "4.0.0", "") {
					t.Errorf("下次启动应照常加载原文件且不再重建：offset=%d", next.Offset)
				}
			case "generate":
				if next.ConfigFileState != FileGenerated || s.read() != template || next.OrphanBackup == "" {
					t.Errorf("下次启动应按模板生成并提示备份：加载结果=%d 提示=%q", next.ConfigFileState, next.OrphanBackup)
				}
			case "template":
				if next.ConfigFileState != FileLoaded || next.Offset != -200 || next.FileRewrite != RewriteNone {
					t.Errorf("下次启动应正常加载模板：加载结果=%d offset=%d", next.ConfigFileState, next.Offset)
				}
			}
		})
	}
}
