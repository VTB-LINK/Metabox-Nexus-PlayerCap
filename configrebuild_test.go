package main

// 本文件只测 config.yml 强制重建在 main 一侧的接线：启动时的版本检查拿到网关的 config_force_update 之后，
// 用 Load 记下的改写前版本判定、重建、提示、留下标记并重启；以及提示框的内容。
//
// 网关用本地的 httptest 服务顶替，配置在临时目录里，提示与重启换成记录调用的桩：不联网、不等待、不重启。
// 判定与重建本身由 config 包的用例守。没有覆盖的：连不上网关的那条路径（它会等 4 秒并动任务栏）、真的
// 要下载更新时的先后（updateIfNeeded 经 restartSelf 退出，测不了），前者不重建由 checkAndUpdate 的提前
// 返回保证，后者的顺序由 TestCheckAndUpdateRebuildsBeforeUpdating 按源码查。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
)

// gateway 起一个返回 body 的假网关，把版本检查请求指向它，并把提示与重启换成桩。返回记录：提示各次的
// 行、重启的次数。程序版本设为 version，用例结束时全部还原。
type rebuildProbe struct {
	notices  [][]string
	restarts int
}

func fakeGateway(t *testing.T, version string, status int, body string) *rebuildProbe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	probe := &rebuildProbe{}
	oldEndpoint, oldVersion, oldRestart, oldNotice, oldOut := versionCheckEndpoint, Version, restartForRebuild, showRebuildNotice, log.Writer()
	versionCheckEndpoint, Version = srv.URL, version
	restartForRebuild = func() { probe.restarts++ }
	showRebuildNotice = func(lines []string) { probe.notices = append(probe.notices, lines) }
	log.SetOutput(io.Discard)
	t.Setenv(envConfigRebuilt, "") // 用例里 Setenv 的两个标记随用例结束还原
	t.Setenv(envConfigBackup, "")
	os.Unsetenv(envConfigRebuilt)
	os.Unsetenv(envConfigBackup)
	t.Cleanup(func() {
		srv.Close()
		versionCheckEndpoint, Version, restartForRebuild, showRebuildNotice = oldEndpoint, oldVersion, oldRestart, oldNotice
		log.SetOutput(oldOut)
	})
	return probe
}

func gatewayBody(tag, forceUpdate string) string {
	return `{"tag_name":"` + tag + `","name":"` + tag + `","config_force_update":"` + forceUpdate + `","assets":[]}`
}

// v3 的 config.yml、程序 4.0.0、网关要求 v4.0.0：Load 先迁移并盖上 v4.0.0（磁盘上已不低于门槛），
// checkAndUpdate 仍以 Load 记下的改写前版本 3.0.0 判定成立，重建、提示一次、重启一次，留下两个环境变量。
// 目录里是模板与两份备份（原文件、迁移后的文件）。重启出来的进程带着标记、读到的是模板，不再重建。
//
// 变异自证（隔离的 worktree，实测）：
//   - releaseInfo 新字段的 JSON 标签写错（configForceUpdate）→ 红。
//   - 判定时重读文件（用磁盘上的版本而不是 cfg.FileVersion）→ 红。
//   - checkAndUpdate 不调 rebuildConfigIfForced → 红；重建之后不重启 → 红；没重建也重启 →
//     TestCheckAndUpdateDoesNotRebuild 里拿到响应而不重建的七例红。
//   - 接线处忽略已重建标记（传空串）→「文件版本被人为改低」一段红。
//   - 重建之后不设已重建标记 →「重启出来的进程」一段红（标记为空）；不设备份的环境变量 → 红。
//   - 重建不成功也提示并重启 → TestCheckAndUpdateRebuildFailureKeepsRunning 红。
//   - 提示框里的备份写成完整路径 → 红（提示不含目录的那条断言；另见 TestRebuildNoticeLines）。
func TestCheckAndUpdateRebuildsConfig(t *testing.T) {
	probe := fakeGateway(t, "4.0.0", http.StatusOK, gatewayBody("v4.0.0", "v4.0.0"))
	dir := t.TempDir()
	path := writeConfigIn(t, dir, v3ConfigText)
	cfg := loadConfigIn(t, dir, "4.0.0")
	migrated, _ := os.ReadFile(path)
	if cfg.FileVersion != "v3.0.0" || !strings.Contains(string(migrated), `config-version: "v4.0.0"`) {
		t.Fatalf("前置：Load 应已迁移并盖上 v4.0.0，记下的改写前版本应是 v3.0.0（实际 %q）", cfg.FileVersion)
	}

	checkAndUpdate(&cfg)

	if probe.restarts != 1 || len(probe.notices) != 1 {
		t.Fatalf("应提示一次、重启一次，实际提示 %d 次、重启 %d 次", len(probe.notices), probe.restarts)
	}
	if data, _ := os.ReadFile(path); string(data) != config.DefaultConfigContent("4.0.0") {
		t.Errorf("config.yml 应是 v4.0.0 的模板，实际\n%s", data)
	}
	v3Backups, _ := filepath.Glob(filepath.Join(dir, "config.backup-v3-*.yml"))
	newBackups, _ := filepath.Glob(filepath.Join(dir, "config.backup-v4.0.0-*.yml"))
	if len(v3Backups) != 1 || len(newBackups) != 1 {
		t.Fatalf("应有原文件与迁移后的文件两份备份，实际 %v / %v", v3Backups, newBackups)
	}
	if data, _ := os.ReadFile(newBackups[0]); string(data) != string(migrated) {
		t.Errorf("重建留下的备份应是迁移后的那一份")
	}
	if got := os.Getenv(envConfigRebuilt); got != "v4.0.0" {
		t.Errorf("%s = %q，期望 v4.0.0", envConfigRebuilt, got)
	}
	if got := os.Getenv(envConfigBackup); got != newBackups[0] {
		t.Errorf("%s = %q，期望重建留下的备份 %q", envConfigBackup, got, newBackups[0])
	}
	// 提示框里是备份的文件名，不带目录；完整路径只在上面的环境变量（与日志）里。
	notice := strings.Join(probe.notices[0], "\n")
	for _, want := range []string{filepath.Base(newBackups[0]) + "（与 config.yml 同目录）", `addr: "127.0.0.1:9000" → "0.0.0.0:8765"`, "wesing-offset: 0 → （模板中未设置）", "已是新写法"} {
		if !strings.Contains(notice, want) {
			t.Errorf("提示应含 %q：\n%s", want, notice)
		}
	}
	if strings.Contains(notice, dir) {
		t.Errorf("提示框里的备份应只写文件名，不带目录 %q：\n%s", dir, notice)
	}
	if strings.Contains(notice, "\n║    offset:") { // 迁移后是 -200，与模板相同
		t.Errorf("与模板相同的 offset 不应列出：\n%s", notice)
	}

	// 重启出来的进程：带着标记，读到的是模板。
	next := loadConfigIn(t, dir, "4.0.0")
	checkAndUpdate(&next)
	if probe.restarts != 1 || len(probe.notices) != 1 {
		t.Errorf("重启出来的进程不应再重建：提示 %d 次、重启 %d 次", len(probe.notices), probe.restarts)
	}
	// 即便文件版本被人为改低，同一个门槛在这棵进程树里也只处理一次。
	writeConfigIn(t, dir, v3ConfigText)
	lowered := loadConfigIn(t, dir, "4.0.0")
	checkAndUpdate(&lowered)
	if probe.restarts != 1 {
		t.Errorf("带着已重建标记时同一门槛不应再处理，重启 %d 次", probe.restarts)
	}
}

// 不重建的各种响应：字段缺失或不合法、门槛高于程序版本、文件版本不低于门槛、响应非 200、响应解析不了。
// 都不提示、不重启、不动文件、不留标记。「还有更新可用」一例反过来：判定在是否更新的分支之前，
// 即便响应里有更高的版本（这里没有可下载的文件，更新分支随即返回），也照常重建并重启。
func TestCheckAndUpdateDoesNotRebuild(t *testing.T) {
	for _, c := range []struct {
		name, content, version string
		status                 int
		body                   string
		rebuilt                bool
	}{
		{"字段缺失", v3ConfigText, "4.0.0", 200, `{"tag_name":"v4.0.0","name":"v4.0.0","assets":[]}`, false},
		{"字段为空串", v3ConfigText, "4.0.0", 200, gatewayBody("v4.0.0", ""), false},
		{"字段不是版本号", v3ConfigText, "4.0.0", 200, gatewayBody("v4.0.0", "yes"), false},
		{"字段名写成驼峰", v3ConfigText, "4.0.0", 200, `{"tag_name":"v4.0.0","name":"v4.0.0","configForceUpdate":"v4.0.0","assets":[]}`, false},
		{"门槛高于程序版本", v3ConfigText, "4.0.0", 200, gatewayBody("v4.0.0", "v4.0.1"), false},
		{"预发布版本够不着 v4.0.0 的门槛", v3ConfigText, "4.0.0-rc.2", 200, gatewayBody("v4.0.0-rc.2", "v4.0.0"), false},
		{"文件版本不低于门槛", "config-version: \"v4.0.0\"\noffset: -5\n", "4.0.1", 200, gatewayBody("v4.0.1", "v4.0.0"), false},
		{"响应非 200", v3ConfigText, "4.0.0", 500, gatewayBody("v4.0.0", "v4.0.0"), false},
		{"响应解析不了", v3ConfigText, "4.0.0", 200, `{"tag_name": `, false},
		{"还有更新可用：先判定", v3ConfigText, "4.0.0", 200, gatewayBody("v4.0.1", "v4.0.0"), true},
		{"线上示例值留到 4.0", v3ConfigText, "4.0.0", 200, gatewayBody("v4.0.0", "v3.7.0"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := fakeGateway(t, c.version, c.status, c.body)
			dir := t.TempDir()
			path := writeConfigIn(t, dir, c.content)
			cfg := loadConfigIn(t, dir, c.version)
			before, _ := os.ReadFile(path)

			checkAndUpdate(&cfg)

			after, _ := os.ReadFile(path)
			want := 0
			if c.rebuilt {
				want = 1
			}
			if probe.restarts != want || len(probe.notices) != want {
				t.Errorf("提示 %d 次、重启 %d 次，期望各 %d 次", len(probe.notices), probe.restarts, want)
			}
			if changed := string(before) != string(after); changed != c.rebuilt {
				t.Errorf("文件有变 = %v，期望 %v", changed, c.rebuilt)
			}
			if marked := os.Getenv(envConfigRebuilt) != ""; marked != c.rebuilt {
				t.Errorf("留下已重建标记 = %v，期望 %v", marked, c.rebuilt)
			}
		})
	}
}

// 判定成立而重建没有成功（这里在 Load 之后把 config.yml 删掉，重建时改名失败）：不提示、不重启、
// 不留标记，以已加载的配置继续运行。
func TestCheckAndUpdateRebuildFailureKeepsRunning(t *testing.T) {
	probe := fakeGateway(t, "4.0.0", http.StatusOK, gatewayBody("v4.0.0", "v4.0.0"))
	dir := t.TempDir()
	path := writeConfigIn(t, dir, v3ConfigText)
	cfg := loadConfigIn(t, dir, "4.0.0")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checkAndUpdate(&cfg)
	if probe.restarts != 0 || len(probe.notices) != 0 || os.Getenv(envConfigRebuilt) != "" {
		t.Errorf("重建失败时不应提示、重启或留标记：提示 %d 次、重启 %d 次、标记 %q", len(probe.notices), probe.restarts, os.Getenv(envConfigRebuilt))
	}
	if cfg.Offset != -200 {
		t.Errorf("应以已加载的配置继续运行，offset = %d", cfg.Offset)
	}
}

// checkAndUpdate 里判定在是否更新的分支之前：按源码里首次调用的先后查（真的要下载更新时进程经
// restartSelf 退出，行为上测不了）。
//
// 变异自证（隔离的 worktree，实测）：把 rebuildConfigIfForced 的调用挪到 updateIfNeeded 之后 → 红；
// 把 decideUpdate 的调用搬回 checkAndUpdate、放在判定之前 → 红。
func TestCheckAndUpdateRebuildsBeforeUpdating(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]token.Pos{} // checkAndUpdate 里各函数首次被调用的位置
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "checkAndUpdate" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					if _, seen := first[id.Name]; !seen {
						first[id.Name] = call.Pos()
					}
				}
			}
			return true
		})
	}
	rebuild, hasRebuild := first["rebuildConfigIfForced"]
	if !hasRebuild {
		t.Fatal("checkAndUpdate 没有调用 rebuildConfigIfForced")
	}
	if _, ok := first["restartForRebuild"]; !ok {
		t.Error("checkAndUpdate 没有调用 restartForRebuild：重建之后不更新时须重启")
	}
	for _, later := range []string{"updateIfNeeded", "decideUpdate", "performUpdateAll", "restartSelf"} {
		if pos, ok := first[later]; ok && pos < rebuild {
			t.Errorf("checkAndUpdate 里 %s 的调用在 rebuildConfigIfForced 之前：判定须在是否更新的分支之前", later)
		}
	}
	if _, ok := first["updateIfNeeded"]; !ok {
		t.Error("checkAndUpdate 没有调用 updateIfNeeded：本用例据它判断先后，函数改名后须同步")
	}
}

// 提示框的内容，中英文各一份：已重建、备份的文件名（不带目录，另说明与 config.yml 同目录）、与模板取值
// 不同的键（原值 → 重建后的值）、备份里的 offset 是哪种写法、接下来会重启（不带秒数，倒计时另在框外）。
//
// 变异自证（隔离的 worktree，实测）：不列备份 → 红；备份写成完整路径 → 红（逐字比较与「各行不含目录」
// 都红）；差异的箭头两侧对调 → 红；模板里没有的键写成 null → 红；新旧写法的说明对调 → 红；备份解析
// 不了时仍说明写法 → 红；中文译表缺一条 → 红。
func TestRebuildNoticeLines(t *testing.T) {
	const dir = `C:\Users\streamer\Desktop\Metabox-Nexus-PlayerCap`
	const name = "config.backup-v3-20261009-153045-1234567890.yml"
	const backup = dir + `\` + name
	const top, bottom = "╔═════════════════════════════════════════════════════════╗", "╚═════════════════════════════════════════════════════════╝"
	diffs := []config.KeyDiff{
		{Key: "addr", Old: "127.0.0.1:9000", New: "0.0.0.0:8765", InTemplate: true},
		{Key: "offset", Old: -500, New: -200, InTemplate: true},
		{Key: "prior-player", Old: []interface{}{"wesing", "kugou"}, New: []interface{}{"wesing"}, InTemplate: true},
		{Key: "audio-normalize", Old: false, New: true, InTemplate: true},
		{Key: "wesing-offset", Old: 0, InTemplate: false},
	}
	cases := []struct {
		name   string
		report config.RebuildReport
		zh, en []string
	}{
		{"旧写法的备份，列出差异", config.RebuildReport{Done: true, Backup: backup, DiffKnown: true, Diffs: diffs, BackupLegacy: true},
			[]string{top,
				"║  ⚠ config.yml 已按当前版本的模板重建",
				"║  旧配置备份: " + name + "（与 config.yml 同目录）",
				"║  与模板取值不同的设置（原值 → 重建后的值）:",
				`║    addr: "127.0.0.1:9000" → "0.0.0.0:8765"`,
				"║    offset: -500 → -200",
				`║    prior-player: ["wesing","kugou"] → ["wesing"]`,
				"║    audio-normalize: false → true",
				"║    wesing-offset: 0 → （模板中未设置）",
				"║  备份里的 offset 仍是旧写法（正值=提前），请以上面列出的值为准",
				"║  程序将重启以加载新的 config.yml",
				bottom},
			[]string{top,
				"║  ⚠ config.yml has been rebuilt from the template of this version",
				"║  Previous file kept as " + name + " (same folder as config.yml)",
				"║  Settings that differed from the template (previous → now):",
				`║    addr: "127.0.0.1:9000" → "0.0.0.0:8765"`,
				"║    offset: -500 → -200",
				`║    prior-player: ["wesing","kugou"] → ["wesing"]`,
				"║    audio-normalize: false → true",
				"║    wesing-offset: 0 → (not set in the template)",
				"║  Offsets in the backup still use the old form (positive = earlier); use the values listed above",
				"║  The program will restart to load the new config.yml",
				bottom}},
		{"新写法的备份，没有差异", config.RebuildReport{Done: true, Backup: backup, DiffKnown: true},
			[]string{top,
				"║  ⚠ config.yml 已按当前版本的模板重建",
				"║  旧配置备份: " + name + "（与 config.yml 同目录）",
				"║  旧配置里的设置与模板没有不同",
				"║  备份里的 offset 已是新写法（正值=延后）",
				"║  程序将重启以加载新的 config.yml",
				bottom},
			[]string{top,
				"║  ⚠ config.yml has been rebuilt from the template of this version",
				"║  Previous file kept as " + name + " (same folder as config.yml)",
				"║  None of its settings differed from the template",
				"║  Offsets in the backup already use the new form (positive = later)",
				"║  The program will restart to load the new config.yml",
				bottom}},
		{"备份解析不了", config.RebuildReport{Done: true, Backup: backup},
			[]string{top,
				"║  ⚠ config.yml 已按当前版本的模板重建",
				"║  旧配置备份: " + name + "（与 config.yml 同目录）",
				"║  旧配置解析不了，不列出其中的设置",
				"║  程序将重启以加载新的 config.yml",
				bottom},
			[]string{top,
				"║  ⚠ config.yml has been rebuilt from the template of this version",
				"║  Previous file kept as " + name + " (same folder as config.yml)",
				"║  The previous file could not be parsed, so its settings are not listed",
				"║  The program will restart to load the new config.yml",
				bottom}},
	}
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
	for _, c := range cases {
		for _, l := range []struct {
			lang  i18n.Lang
			label string
			want  []string
		}{{i18n.Chinese, "", c.zh}, {i18n.English, "（英文）", c.en}} {
			i18n.SetLanguage(l.lang)
			lines := rebuildNoticeLines(c.report)
			if got := strings.Join(lines, "\n"); got != strings.Join(l.want, "\n") {
				t.Errorf("%s%s\n得：\n%s\n期望：\n%s", c.name, l.label, got, strings.Join(l.want, "\n"))
			}
			// 备份只写文件名：带上目录的那一行在终端里会折行，续行没有左边框。
			for _, line := range lines {
				if strings.Contains(line, dir) {
					t.Errorf("%s%s：提示框里不应出现备份所在的目录 %q：\n%s", c.name, l.label, dir, line)
				}
			}
		}
	}
}
