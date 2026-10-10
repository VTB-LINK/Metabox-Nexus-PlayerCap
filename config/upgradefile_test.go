package config

// 本文件只测 loadFile 对 config.yml 版本的整套处理（upgradeFile）：迁移或改版本键并写回文件、本次运行
// 采用的取值、记进 Config 的各项，以及写不成、写到一半崩溃之后磁盘上的文件与下一次启动的结果
// （设计 §3.3 与 §3.6 的各张表）。值层、文本层、替换文件的步骤各自的细节见 migratevalues_test.go、
// migratetext_test.go、replacefile_test.go。

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// v3 时期的一份配置：没有版本键，offset 是提前量。
const v3Config = "# 我的配置\naddr: \"127.0.0.1:9000\"\n\n# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后\noffset: 200\npoll: 40\n" +
	"kugou-offset: 430\n# qqmusic-offset: 350\nwesing-offset: 0\n"

// v3Migrated 是 v3Config 迁移到 stamp 之后的全文。
func v3Migrated(stamp string) string {
	return strings.Replace(migratedHeader, "\"v4.0.0\"", `"`+stamp+`"`, 1) +
		"# 我的配置\naddr: \"127.0.0.1:9000\"\n\n# 歌词时间偏移（毫秒），正值=延后，负值=提前\noffset: -200\npoll: 40\n" +
		"kugou-offset: -430\n# qqmusic-offset: -350\nwesing-offset: 0\n"
}

// noWrites 返回任何写操作都算失败的文件操作：用于断言某次加载不碰文件。
func noWrites(t *testing.T) *fileOps {
	ops := testOps()
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		t.Errorf("本次加载不应写文件（试图建 %s）", pattern)
		return nil, errInjected
	}
	ops.rename = func(a, b string) error { t.Errorf("本次加载不应改名 %s", a); return errInjected }
	return ops
}

// wantOffsets 断言本次运行采用的全局 offset 与 kugou 的 offset。
func wantOffsets(t *testing.T, cfg Config, global, kugou int) {
	t.Helper()
	if cfg.Offset != global || cfg.GetPlayerOffset("kugou") != kugou {
		t.Errorf("本次运行的 offset = %d、kugou = %d，期望 %d、%d", cfg.Offset, cfg.GetPlayerOffset("kugou"), global, kugou)
	}
}

// v3 的文件：offset 取反后生效，文件整体换成迁移结果并带上新版本号，原文件改名为备份，各项记进 Config。
// 发布版本盖版本号，开发构建盖迁移边界。
//
// 变异自证（隔离的 worktree，实测）：
//   - 改写之后才记文件版本（FileVersion 取改写后文件的版本键）→ 红。
//   - 迁移与改版本键分两次写（先写迁移结果、不带版本键，再写版本键）→ TestUpgradeCrashThenRestart 的
//     「迁移/placed」「迁移/temp-removed」红（第一次写完即崩溃，文件已取反而没有版本键，下次启动
//     再迁一遍）。
//   - 备份改回先复制再覆盖（复制出备份、os.WriteFile 覆盖原文件）→ TestUpgradeFileChangedWhileLoading 红。
//   - 交给 mergeYAML 的仍是迁移前的解析结果 → 红（本次运行的 offset 是 200）。
//   - 迁移的日志行写回 config.yml 与备份的完整路径 → 三例红。
func TestLoadFileMigratesV3(t *testing.T) {
	for _, c := range []struct{ name, version, stamp string }{
		{"发布版本", "4.0.0", "v4.0.0"},
		{"预发布版本", "4.0.0-beta.3", "v4.0.0-beta.3"},
		{"开发构建", "0.0.0", "v4.0.0-0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, v3Config)
			cfg, logs := s.load(c.version, testOps())

			wantOffsets(t, cfg, -200, -430)
			if cfg.Poll != 40 || cfg.Addr != "127.0.0.1:9000" || !cfg.ExplicitKeys["offset"] || !cfg.ExplicitKeys["kugou-offset"] {
				t.Errorf("其余键应照常合并且 offset 记为显式：poll=%d addr=%q 显式=%v", cfg.Poll, cfg.Addr, cfg.ExplicitKeys)
			}
			if got := s.read(); got != v3Migrated(c.stamp) {
				t.Errorf("config.yml\n得：\n%s\n期望：\n%s", got, v3Migrated(c.stamp))
			}
			s.wantFiles(configName, "config.backup-v3-20261009-153045-*.yml")
			if cfg.ConfigFileState != FileLoaded || cfg.FileRewrite != RewriteMigrated ||
				cfg.FileVersion != "v3.0.0" || cfg.FileVersionLabel != "v3" || cfg.FileStamp != c.stamp {
				t.Errorf("记下的各项：加载结果=%d 改写=%d 文件版本=%q 写法=%q 盖章值=%q",
					cfg.ConfigFileState, cfg.FileRewrite, cfg.FileVersion, cfg.FileVersionLabel, cfg.FileStamp)
			}
			if cfg.FileBackup == "" || readTestFile(t, cfg.FileBackup) != v3Config {
				t.Errorf("备份 %q 应是原文件的字节", cfg.FileBackup)
			}
			if !containsAll(logs, "迁移", filepath.Base(cfg.FileBackup)) {
				t.Errorf("日志应说明已迁移并给出备份：\n%s", logs)
			}
			// 迁移的那一行只写文件名；完整路径在其后的「已加载」一行。
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "迁移到") && strings.Contains(line, s.dir) {
					t.Errorf("迁移的日志行不应带目录：\n%s", line)
				}
			}
			if !strings.Contains(logs, s.path) {
				t.Errorf("日志里应有 config.yml 的完整路径（已加载一行）：\n%s", logs)
			}
		})
	}
}

// 幂等与版本更替：按顺序以各版本加载同一个目录，逐次断言改写结果、文件版本、采用的 offset，以及文件
// 有没有变。任何一步都不会把已取反的 offset 再取反。
//
// 变异自证（隔离的 worktree，实测）：
//   - 边界由 4.0.0-0 改成 4.0.0（步骤表与 latestMigrationBoundary 同改）→「4.0.0-beta.3 盖过的文件由
//     4.0.0 读取」「开发构建之后换预发布版本」红（再迁一遍，offset 变回 200），另有「开发构建连续两次」
//     等三例因盖章值随之变了而红。
//   - 开发构建的盖章值改成 0.0.0 →「开发构建连续两次」红（第二次再迁一遍）。
//   - 步骤选择的 < 改成 <=（stepsAfter）→「开发构建之后换发布版本」红。
//   - 降级时把版本键改低 →「发布版本之后换开发构建」「降级」红。
//   - 版本相同也写文件 →「连续两次」各例红。
func TestLoadFileIsIdempotentAcrossVersions(t *testing.T) {
	type step struct {
		version     string
		rewrite     RewriteState
		fileVersion string // 本次记下的改写前版本
		diskVersion string // 加载之后文件里的版本键
		changed     bool   // 本次加载改了文件
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"发布版本连续两次", []step{
			{"4.0.0", RewriteMigrated, "v3.0.0", "v4.0.0", true},
			{"4.0.0", RewriteNone, "v4.0.0", "v4.0.0", false},
			{"4.0.0", RewriteNone, "v4.0.0", "v4.0.0", false},
		}},
		{"开发构建连续两次", []step{
			{"0.0.0", RewriteMigrated, "v3.0.0", "v4.0.0-0", true},
			{"", RewriteNone, "v4.0.0-0", "v4.0.0-0", false},
			{"dev-20261009-abc1234", RewriteNone, "v4.0.0-0", "v4.0.0-0", false},
		}},
		{"开发构建之后换发布版本", []step{
			{"0.0.0", RewriteMigrated, "v3.0.0", "v4.0.0-0", true},
			{"4.0.0", RewriteStamped, "v4.0.0-0", "v4.0.0", true},
			{"4.0.0", RewriteNone, "v4.0.0", "v4.0.0", false},
		}},
		{"开发构建之后换预发布版本", []step{
			{"0.0.0", RewriteMigrated, "v3.0.0", "v4.0.0-0", true},
			{"4.0.0-alpha.1", RewriteStamped, "v4.0.0-0", "v4.0.0-alpha.1", true},
		}},
		{"发布版本之后换开发构建", []step{
			{"4.0.0", RewriteMigrated, "v3.0.0", "v4.0.0", true},
			{"0.0.0", RewriteNewerFile, "v4.0.0", "v4.0.0", false},
			{"4.0.0", RewriteNone, "v4.0.0", "v4.0.0", false},
		}},
		{"4.0.0-beta.3 盖过的文件由 4.0.0 读取", []step{
			{"4.0.0-beta.3", RewriteMigrated, "v3.0.0", "v4.0.0-beta.3", true},
			{"4.0.0", RewriteStamped, "v4.0.0-beta.3", "v4.0.0", true},
		}},
		{"逐版本升级", []step{
			{"4.0.0", RewriteMigrated, "v3.0.0", "v4.0.0", true},
			{"4.0.1", RewriteStamped, "v4.0.0", "v4.0.1", true},
			{"4.1.0", RewriteStamped, "v4.0.1", "v4.1.0", true},
		}},
		{"降级", []step{
			{"4.0.1", RewriteMigrated, "v3.0.0", "v4.0.1", true},
			{"4.0.0", RewriteNewerFile, "v4.0.1", "v4.0.1", false},
			{"4.0.1", RewriteNone, "v4.0.1", "v4.0.1", false},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, v3Config)
			for i, st := range c.steps {
				before := s.read()
				ops := testOps()
				if !st.changed {
					ops = noWrites(t)
				}
				cfg, _ := s.load(st.version, ops)
				if cfg.FileRewrite != st.rewrite || cfg.FileVersion != st.fileVersion {
					t.Errorf("第 %d 次（%q）：改写=%d、文件版本=%q，期望 %d、%q", i+1, st.version, cfg.FileRewrite, cfg.FileVersion, st.rewrite, st.fileVersion)
				}
				wantOffsets(t, cfg, -200, -430)
				after := s.read()
				if got, _ := readFileVersion(parseYAML(t, after)); got != st.diskVersion {
					t.Errorf("第 %d 次（%q）之后文件里的版本 = %q，期望 %q", i+1, st.version, got, st.diskVersion)
				}
				if (before != after) != st.changed {
					t.Errorf("第 %d 次（%q）：文件有变 = %v，期望 %v", i+1, st.version, before != after, st.changed)
				}
				if st.changed && i > 0 { // 只改版本键：除版本键一行外逐字节相同
					if want := strings.Replace(before, `"`+st.fileVersion+`"`, `"`+st.diskVersion+`"`, 1); after != want {
						t.Errorf("第 %d 次（%q）只应改版本键一行\n得：\n%s", i+1, st.version, after)
					}
				}
			}
			s.wantFiles(configName, "config.backup-v3-*.yml") // 自始至终只有迁移留下的那一份备份
		})
	}
}

// 降级：发布版本读到更高版本的文件时告警；开发构建读到发布版本盖过的文件是常态，不告警。两者都不动文件。
func TestLoadFileNewerFileWarnsOnlyOnRelease(t *testing.T) {
	const content = "config-version: \"v4.2.0\"\noffset: -200\nkugou-offset: -430\n"
	for _, c := range []struct {
		version string
		warn    bool
	}{{"4.0.0", true}, {"4.1.9", true}, {"0.0.0", false}, {"dev-build", false}} {
		s := newSandbox(t, content)
		cfg, logs := s.load(c.version, noWrites(t))
		if cfg.FileRewrite != RewriteNewerFile || cfg.FileVersion != "v4.2.0" || s.read() != content {
			t.Errorf("程序版本 %q：改写=%d、文件版本=%q，文件应不变", c.version, cfg.FileRewrite, cfg.FileVersion)
		}
		wantOffsets(t, cfg, -200, -430)
		if got := strings.Contains(logs, "[!]"); got != c.warn {
			t.Errorf("程序版本 %q：有告警 = %v，期望 %v\n%s", c.version, got, c.warn, logs)
		}
	}
}

// 只改版本键（没有适用的迁移步骤）：只有版本键一行的值变成带引号的盖章值，其余字节逐一相同，不留备份。
// 版本键非法时同样只换该行并告警，不迁移。
//
// 变异自证（隔离的 worktree，实测）：只改版本键路径成功后不删备份 → 各例红；非法版本键按 v3 处理 →
// 两例非法红（offset 被取反）；非法版本键不告警 → 红。
func TestLoadFileStampsVersionKeyOnly(t *testing.T) {
	const rest = "addr: x\noffset: -200  # 延迟\nkugou-offset: 0x10\n# qqmusic-offset: 350\n# 正值=歌词提前\n"
	for _, c := range []struct {
		name, line, want, fileVersion string
		invalid                       bool
	}{
		{"带 v、双引号", `config-version: "v4.0.0"`, `config-version: "v4.0.1"`, "v4.0.0", false},
		{"不带 v", `config-version: "4.0.0"`, `config-version: "v4.0.1"`, "v4.0.0", false},
		{"单引号", `config-version: 'v4.0.0'`, `config-version: "v4.0.1"`, "v4.0.0", false},
		{"不带引号", `config-version: v4.0.0`, `config-version: "v4.0.1"`, "v4.0.0", false},
		{"行尾注释", `config-version: "v4.0.0"   # 勿改`, `config-version: "v4.0.1"   # 勿改`, "v4.0.0", false},
		{"开发构建盖过的", `config-version: "v4.0.0-0"`, `config-version: "v4.0.1"`, "v4.0.0-0", false},
		{"非法：小数", `config-version: 4.0`, `config-version: "v4.0.1"`, "", true},
		{"非法：空值", `config-version:`, `config-version: "v4.0.1"`, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, "# 头\n"+c.line+"\n"+rest)
			cfg, logs := s.load("4.0.1", testOps())
			if got, want := s.read(), "# 头\n"+c.want+"\n"+rest; got != want {
				t.Errorf("config.yml\n得   %q\n期望 %q", got, want)
			}
			s.wantFiles(configName)
			if cfg.FileRewrite != RewriteStamped || cfg.FileVersion != c.fileVersion || cfg.FileBackup != "" {
				t.Errorf("改写=%d、文件版本=%q、备份=%q，期望只改版本键、%q、无备份", cfg.FileRewrite, cfg.FileVersion, cfg.FileBackup, c.fileVersion)
			}
			wantOffsets(t, cfg, -200, 16) // 不迁移：取值原样
			if got := strings.Contains(logs, "[!]"); got != c.invalid {
				t.Errorf("有告警 = %v，期望 %v\n%s", got, c.invalid, logs)
			}
		})
	}
}

// 文本层认不了的写法、不是 UTF-8、多文档、只读文件：不写文件，原文件字节不变，本次按迁移后的取值运行并
// 告警；再加载一次结果相同（每次启动都在内存里换算，不会迁两遍）。
//
// 变异自证（隔离的 worktree，实测）：不写文件时交给 mergeYAML 的是迁移前的解析结果 → 各例红；去掉只读
// 检查 → 两例只读红（文件被换成不带只读属性的新文件）；去掉文档数量的检查 →「多文档、第二个文档里有
// 注释掉的键」红（第二个文档被改写）；去掉 UTF-8 检查 → 不红（「UTF-16」一例另被「恰好对应一行」拦下）。
func TestLoadFileLeavesUnrewritableFilesAlone(t *testing.T) {
	utf16le := func(s string) string {
		var b []byte
		for _, u := range utf16.Encode(append([]rune{0xFEFF}, []rune(s)...)) {
			b = append(b, byte(u), byte(u>>8))
		}
		return string(b)
	}
	for _, c := range []struct {
		name, content string
		readOnly      bool
		global, kugou int
		pending       RewriteState
	}{
		{"十六进制", "offset: 0x1f4\nkugou-offset: 430\n", false, -500, -430, RewriteMigratePending},
		{"下划线分隔", "offset: 1_000\n", false, -1000, -1000, RewriteMigratePending},
		// 生效的键写成前导零：yaml 解析为 8，行里的数字按十进制是 10，与原值不符，不写文件。被注释掉的键
		// 没有这道对照，由文本层自己拒认前导零（migratetext_test.go）。
		{"前导零（解析为八进制）", "offset: 010\nkugou-offset: 430\n# qqmusic-offset: 010\n", false, -8, -430, RewriteMigratePending},
		{"嵌套映射里另有同名的行", "offset: 200\nextra:\n  offset: 5\n", false, -200, -200, RewriteMigratePending},
		{"流式映射", "{offset: 200, kugou-offset: 430}\n", false, -200, -430, RewriteMigratePending},
		{"键带引号", "\"offset\": 200\n", false, -200, -200, RewriteMigratePending},
		{"锚点与别名", "offset: &o 200\nkugou-offset: *o\n", false, -200, -200, RewriteMigratePending},
		{"块标量里形如注释掉的键的行", "offset: 200\nnote: |\n  # kugou-offset: 5\n", false, -200, -200, RewriteMigratePending},
		{"多文档", "offset: 200\n---\nconfig-version: \"v4.0.0\"\noffset: -500\n", false, -200, -200, RewriteMigratePending},
		{"多文档、第二个文档里有注释掉的键", "offset: 200\n---\n# kugou-offset: 5\n", false, -200, -200, RewriteMigratePending},
		{"UTF-16", utf16le("offset: 200\r\nkugou-offset: 430\r\n"), false, -200, -430, RewriteMigratePending},
		{"只读文件", v3Config, true, -200, -430, RewriteMigratePending},
		{"只改版本键而键带引号", "\"config-version\": \"v4.0.0-0\"\noffset: -200\nkugou-offset: -430\n", false, -200, -430, RewriteStampPending},
		{"只改版本键而文件只读", "config-version: \"v4.0.0-0\"\noffset: -200\nkugou-offset: -430\n", true, -200, -430, RewriteStampPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, c.content)
			if c.readOnly {
				if err := os.Chmod(s.path, 0444); err != nil {
					t.Fatal(err)
				}
			}
			for round := 1; round <= 2; round++ {
				cfg, logs := s.load("4.0.0", noWrites(t))
				wantOffsets(t, cfg, c.global, c.kugou)
				if cfg.FileRewrite != c.pending || cfg.ConfigFileState != FileLoaded {
					t.Errorf("第 %d 次：改写=%d、加载结果=%d，期望 %d 与 FileLoaded", round, cfg.FileRewrite, cfg.ConfigFileState, c.pending)
				}
				if got := s.read(); got != c.content {
					t.Errorf("第 %d 次：原文件字节应不变，实际 %q", round, got)
				}
				if !strings.Contains(logs, "[!]") {
					t.Errorf("第 %d 次：应有告警\n%s", round, logs)
				}
			}
			s.wantFiles(configName)
			if info, _ := os.Stat(s.path); c.readOnly && info.Mode().Perm()&0200 != 0 {
				t.Error("只读属性应保留")
			}
		})
	}
}

// 写入失败（目录不可写、文件被占用，这里注入失败）：原文件字节不变，本次按迁移后的取值运行；下次启动
// 写得成时照常迁移。
func TestLoadFileWriteFailureThenRetry(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(ops *fileOps)
	}{
		{"写临时文件失败", func(ops *fileOps) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errInjected }
		}},
		{"改名失败", func(ops *fileOps) { ops.rename = func(string, string) error { return errInjected } }},
		// 新文件放不上去（硬链接与不覆盖的改名都失败）：原文件已改名为备份，由硬链接放回原位。
		{"放置失败：放回", func(ops *fileOps) {
			link := ops.link
			ops.link = func(src, dst string) error {
				if strings.HasSuffix(src, ".tmp") {
					return errInjected
				}
				return link(src, dst)
			}
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, v3Config)
			ops := testOps()
			c.fail(ops)
			cfg, logs := s.load("4.0.0", ops)
			wantOffsets(t, cfg, -200, -430)
			if cfg.FileRewrite != RewriteMigratePending || cfg.FileBackup != "" || s.read() != v3Config || !strings.Contains(logs, "[!]") {
				t.Errorf("改写=%d、备份=%q，文件应不变并告警\n%s", cfg.FileRewrite, cfg.FileBackup, logs)
			}
			s.wantFiles(configName)

			cfg, _ = s.load("4.0.0", testOps())
			wantOffsets(t, cfg, -200, -430)
			if cfg.FileRewrite != RewriteMigrated || s.read() != v3Migrated("v4.0.0") {
				t.Errorf("下次启动应照常迁移，改写=%d", cfg.FileRewrite)
			}
		})
	}
}

// 新文件放不上去、原文件也放不回（硬链接与不覆盖的改名对两者都失败）：没有 config.yml，原文件完整留在
// 备份里并在告警里给出名字，本次按内存换算运行；下次启动按模板生成并提示这份备份。迁移与只改版本键
// 两条路径相同（只改版本键路径此时不删备份）。config.yml 这个名字上没有做过任何创建或删除。
func TestUpgradeFilePlaceFailsAndCannotPutBack(t *testing.T) {
	const stamped = "config-version: \"v4.0.0\"\noffset: -200\nkugou-offset: -430\n"
	for _, c := range []struct {
		name, content, version, backupGlob string
		pending                            RewriteState
	}{
		{"迁移", v3Config, "4.0.0", "config.backup-v3-*.yml", RewriteMigratePending},
		{"只改版本键", stamped, "4.0.1", "config.backup-v4.0.0-*.yml", RewriteStampPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, c.content)
			ops := testOps()
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { return errInjected }
			cfg, logs := s.load(c.version, ops)
			wantOffsets(t, cfg, -200, -430)
			if cfg.FileRewrite != c.pending || s.exists() {
				t.Errorf("改写=%d、config.yml 存在=%v，期望未写成且没有 config.yml", cfg.FileRewrite, s.exists())
			}
			s.wantFiles(c.backupGlob)
			if cfg.FileBackup == "" || readTestFile(t, cfg.FileBackup) != c.content {
				t.Fatalf("备份 %q 应是原文件的字节", cfg.FileBackup)
			}
			if !containsAll(logs, "[!]", filepath.Base(cfg.FileBackup)) {
				t.Errorf("告警应给出备份的名字\n%s", logs)
			}

			next, logs := s.load(c.version, testOps())
			if next.ConfigFileState != FileGenerated || next.OrphanBackup != filepath.Base(cfg.FileBackup) || !strings.Contains(logs, "[!]") {
				t.Errorf("下次启动应按模板生成并提示备份：加载结果=%d 提示=%q\n%s", next.ConfigFileState, next.OrphanBackup, logs)
			}
			if readTestFile(t, cfg.FileBackup) != c.content {
				t.Error("备份不应被动过")
			}
		})
	}
}

// 单点失败的遍历：迁移、只改版本键、强制重建、按模板生成四条路径，逐个让第 n 次文件操作失败（硬链接
// 可用与不可用各一轮），直到 n 超出这一路的操作次数。无论失败落在哪一步：
//
//   - 用户的原文件都留有一份完整的副本（只改版本键写成之后是只差版本键一行的那一份），下一次正常启动
//     之后仍然如此；
//   - config.yml 没有被按路径删除过（newSandbox 收尾时断言）。
//
// 硬链接不可用的一轮在 Windows 上走真实的不覆盖改名，在别的平台上放置一律失败，两条性质同样成立。
//
// 变异自证（隔离的 worktree，实测）：放置失败后删除 config.yml（placeIfAbsent 在硬链接与改名都失败时删
// dst）→ 四条路径「硬链接可用=false」一轮里失败落在改名上的各一例红；TestLoadFileWriteFailureThenRetry
// 的「放置失败：放回」、TestUpgradeFilePlaceFailsAndCannotPutBack 两例、TestGeneratePlaceFailure 与
// rebuildfile_test.go 的「放置失败：不放回」同红，都红在 newSandbox 收尾的断言上。
func TestSingleFailureNeverLosesUserFile(t *testing.T) {
	const stamped = "config-version: \"v4.0.0\"\noffset: -200\nkugou-offset: -430\n"
	for _, c := range []struct {
		name, initial, version string
		run                    func(s *sandbox, ops *fileOps)
		copies                 []string // 结束时目录里须有其中之一；空即这一路没有用户的文件
	}{
		{"迁移", v3Config, "4.0.0", func(s *sandbox, ops *fileOps) { s.load("4.0.0", ops) }, []string{v3Config}},
		{"只改版本键", stamped, "4.0.1", func(s *sandbox, ops *fileOps) { s.load("4.0.1", ops) },
			[]string{stamped, strings.Replace(stamped, "v4.0.0", "v4.0.1", 1)}},
		{"强制重建", stamped, "4.0.0", func(s *sandbox, ops *fileOps) {
			cfg, _ := s.load("4.0.0", testOps())
			captureLog(s.t, func() { forceRebuild(ops, &cfg, "4.0.0") })
		}, []string{stamped}},
		{"按模板生成", "", "4.0.0", func(s *sandbox, ops *fileOps) { s.load("4.0.0", ops) }, nil},
	} {
		for _, links := range []bool{true, false} {
			for n, reached := 1, true; reached; n++ {
				t.Run(fmt.Sprintf("%s/硬链接可用=%v/第 %d 次操作失败", c.name, links, n), func(t *testing.T) {
					s := newSandbox(t, c.initial)
					ops, count := failNth(n)
					if !links {
						ops.link = func(string, string) error { return errInjected }
					}
					c.run(s, ops)
					reached = *count >= n
					if len(c.copies) > 0 && !slices.ContainsFunc(c.copies, s.hasCopyOf) {
						t.Fatalf("目录里 %v 都不是用户的原文件", s.names("*"))
					}
					s.load(c.version, testOps())
					if len(c.copies) > 0 && !slices.ContainsFunc(c.copies, s.hasCopyOf) {
						t.Errorf("下次启动之后目录里 %v 都不是用户的原文件", s.names("*"))
					}
				})
			}
		}
	}
}

// 加载之后、改名之前文件被保存过（改名得到的那一份与加载时读到的不同）：把它放回 config.yml，内容是被
// 保存过的那一份，不留备份，本次按加载时那一份的换算运行；下次启动对新内容重走迁移。
// 放回时 config.yml 又出现了，两份都留。
func TestUpgradeFileChangedWhileLoading(t *testing.T) {
	const saved = "offset: 700\nkugou-offset: 1\n"
	saveDuring := func(s *sandbox) *fileOps {
		ops := testOps()
		ops.step = func(step string) {
			if step == stepTempWritten {
				s.write(saved)
			}
		}
		return ops
	}
	t.Run("放回", func(t *testing.T) {
		s := newSandbox(t, v3Config)
		cfg, logs := s.load("4.0.0", saveDuring(s))
		wantOffsets(t, cfg, -200, -430)
		if cfg.FileRewrite != RewriteMigratePending || cfg.FileBackup != "" || s.read() != saved {
			t.Errorf("改写=%d、备份=%q、config.yml=%q，期望未写成、无备份、被保存过的内容", cfg.FileRewrite, cfg.FileBackup, s.read())
		}
		s.wantFiles(configName)
		if !strings.Contains(logs, "加载期间") {
			t.Errorf("应告警文件在加载期间被改动过\n%s", logs)
		}

		cfg, _ = s.load("4.0.0", testOps())
		wantOffsets(t, cfg, -700, -1)
		if cfg.FileRewrite != RewriteMigrated || readTestFile(t, cfg.FileBackup) != saved {
			t.Errorf("下次启动应对被保存过的内容重走迁移，改写=%d", cfg.FileRewrite)
		}
	})
	t.Run("只改版本键路径同样放回", func(t *testing.T) {
		s := newSandbox(t, "config-version: \"v4.0.0\"\noffset: -200\n")
		cfg, _ := s.load("4.0.1", saveDuring(s))
		if cfg.FileRewrite != RewriteStampPending || s.read() != saved {
			t.Errorf("改写=%d、config.yml=%q，期望未写成、被保存过的内容", cfg.FileRewrite, s.read())
		}
		s.wantFiles(configName)
	})
	t.Run("放回时 config.yml 又出现了", func(t *testing.T) {
		s := newSandbox(t, v3Config)
		ops := saveDuring(s)
		inner := ops.step
		ops.step = func(step string) {
			inner(step)
			if step == stepRenamed {
				s.write("offset: 1\n")
			}
		}
		cfg, _ := s.load("4.0.0", ops)
		if cfg.FileRewrite != RewriteMigratePending || s.read() != "offset: 1\n" || cfg.FileBackup == "" || readTestFile(t, cfg.FileBackup) != saved {
			t.Errorf("改写=%d、config.yml=%q、备份=%q，期望两份都留", cfg.FileRewrite, s.read(), cfg.FileBackup)
		}
		s.wantFiles(configName, "config.backup-v3-*.yml")
	})
}

// 改名之后别的程序又新建了 config.yml：放置不覆盖，新建的那一份字节不变，备份是被移走的那一份，临时文件
// 已删，本次按内存换算运行。只改版本键路径此时不删备份。
func TestUpgradeFileAppearedWhileReplacing(t *testing.T) {
	const other = "offset: 9\n"
	for _, c := range []struct {
		name, content, version string
		pending                RewriteState
	}{
		{"迁移", v3Config, "4.0.0", RewriteMigratePending},
		{"只改版本键", "config-version: \"v4.0.0\"\noffset: -200\nkugou-offset: -430\n", "4.0.1", RewriteStampPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, c.content)
			ops := testOps()
			ops.step = func(step string) {
				if step == stepVerified {
					s.write(other)
				}
			}
			cfg, logs := s.load(c.version, ops)
			wantOffsets(t, cfg, -200, -430)
			if cfg.FileRewrite != c.pending || s.read() != other {
				t.Errorf("改写=%d、config.yml=%q，期望未写成、别的程序新建的那一份", cfg.FileRewrite, s.read())
			}
			if cfg.FileBackup == "" || readTestFile(t, cfg.FileBackup) != c.content {
				t.Errorf("备份 %q 应是被移走的那一份", cfg.FileBackup)
			}
			s.wantFiles(configName, "config.backup-*.yml")
			if !strings.Contains(logs, filepath.Base(cfg.FileBackup)) {
				t.Errorf("告警应给出备份的名字\n%s", logs)
			}
		})
	}
}

// 各步之后崩溃（设计 §3.6 的表逐格）：断言磁盘上的文件，再以同一目录重新加载一次，断言「下次启动」。
// 迁移与只改版本键两条路径。崩溃留下的临时文件与空的预留备份名不自动清理。
//
// 变异自证（隔离的 worktree，实测）：按模板生成时不提示目录里的备份 → 四例「没有 config.yml、有备份」红；
// 只改版本键路径未经核对就删备份（改名之后立即删）→「只改版本键/renamed」「只改版本键/verified」红
// （崩溃后既没有 config.yml 也没有备份）；替换的各步用 defer 清理临时文件 → 除两例 temp-removed 外各例红
// （磁盘上少了临时文件）。
func TestUpgradeCrashThenRestart(t *testing.T) {
	const stamped = "config-version: \"v4.0.0\"\noffset: -200\nkugou-offset: -430\n"
	stampedNew := strings.Replace(stamped, "v4.0.0", "v4.0.1", 1)
	type disk struct {
		config string   // config.yml 的内容；"-" 表示不存在
		files  []string // 目录里的全部文件（glob）
	}
	paths := []struct {
		name, original, version, result, backupGlob string
		done                                        RewriteState // 重走时的改写结果
	}{
		{"迁移", v3Config, "4.0.0", v3Migrated("v4.0.0"), "config.backup-v3-*.yml", RewriteMigrated},
		{"只改版本键", stamped, "4.0.1", stampedNew, "config.backup-v4.0.0-*.yml", RewriteStamped},
	}
	for _, p := range paths {
		tmp := configName + ".*.tmp"
		steps := []struct {
			step  string
			after disk
			next  string // 下次启动：redo 重走、generate 按模板生成并提示备份、none 正常加载不再改写
		}{
			{stepTempWritten, disk{p.original, []string{configName, tmp}}, "redo"},
			{stepBackupReserved, disk{p.original, []string{configName, tmp, p.backupGlob}}, "redo"},
			{stepRenamed, disk{"-", []string{tmp, p.backupGlob}}, "generate"},
			{stepVerified, disk{"-", []string{tmp, p.backupGlob}}, "generate"},
			{stepPlaced, disk{p.result, []string{configName, tmp, p.backupGlob}}, "none"},
			{stepTempRemoved, disk{p.result, []string{configName, p.backupGlob}}, "none"},
		}
		for _, st := range steps {
			t.Run(p.name+"/"+st.step, func(t *testing.T) {
				s := newSandbox(t, p.original)
				if !crashed(func() { s.load(p.version, crashAfter(st.step)) }) {
					t.Fatalf("没有走到 %s 这一步", st.step)
				}
				if st.after.config == "-" {
					if s.exists() {
						t.Errorf("崩溃后不应有 config.yml，实际 %q", s.read())
					}
				} else if got := s.read(); got != st.after.config {
					t.Errorf("崩溃后 config.yml\n得   %q\n期望 %q", got, st.after.config)
				}
				s.wantFiles(st.after.files...)
				backups := s.backups()
				if st.step != stepTempWritten && st.step != stepBackupReserved && s.file(backups[0]) != p.original {
					t.Errorf("备份 %s 应是原文件", backups[0])
				}

				cfg, logs := s.load(p.version, testOps())
				switch st.next {
				case "redo":
					if cfg.FileRewrite != p.done || s.read() != p.result {
						t.Errorf("下次启动应重走，改写=%d", cfg.FileRewrite)
					}
				case "generate":
					if cfg.ConfigFileState != FileGenerated || s.read() != templateContent("v"+p.version) {
						t.Errorf("下次启动应按模板生成，加载结果=%d", cfg.ConfigFileState)
					}
					if cfg.OrphanBackup != backups[0] || !containsAll(logs, "[!]", backups[0]) {
						t.Errorf("应提示目录里的备份 %s：记下 %q\n%s", backups[0], cfg.OrphanBackup, logs)
					}
					if s.file(backups[0]) != p.original {
						t.Errorf("备份不应被动过")
					}
				case "none":
					if cfg.FileRewrite != RewriteNone || cfg.ConfigFileState != FileLoaded || s.read() != p.result {
						t.Errorf("下次启动应正常加载、不再改写，改写=%d", cfg.FileRewrite)
					}
				}
				if cfg.OrphanBackup != "" && st.next != "generate" {
					t.Errorf("没有按模板生成时不应提示备份，实际 %q", cfg.OrphanBackup)
				}
			})
		}
	}
}

// 按模板生成时的备份提示：只提最新一份非空的；目录里没有备份、或只有空的预留名时不提。
func TestGenerateHintsAtLatestBackup(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"没有备份", nil, ""},
		{"只有空的预留名", map[string]string{"config.backup-v3-20261009-153045-1.yml": ""}, ""},
		{"跳过空的，取最新的非空", map[string]string{
			"config.backup-v3-20261001-000000-1.yml":     "older",
			"config.backup-v4.0.0-20261005-000000-2.yml": "newer",
			"config.backup-v4.0.0-20261009-000000-3.yml": "",
		}, "config.backup-v4.0.0-20261005-000000-2.yml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t, "")
			for name, content := range c.files {
				writeTestFile(t, filepath.Join(s.dir, name), content)
			}
			cfg, logs := s.load("4.0.0", testOps())
			if cfg.ConfigFileState != FileGenerated || cfg.OrphanBackup != c.want {
				t.Errorf("加载结果=%d、提示的备份=%q，期望 FileGenerated 与 %q", cfg.ConfigFileState, cfg.OrphanBackup, c.want)
			}
			if got := strings.Contains(logs, "[!]"); got != (c.want != "") {
				t.Errorf("有告警 = %v，期望 %v\n%s", got, c.want != "", logs)
			}
			for name, content := range c.files {
				if s.file(name) != content {
					t.Errorf("备份 %s 不应被动过", name)
				}
			}
		})
	}
}

// 按模板生成同样是不存在才放置：模板写好之后 config.yml 已由别的实例建出，就不覆盖、改为加载那一份
// （这里是一份 v3 的文件，随即照常迁移）。
//
// 变异自证（隔离的 worktree，实测）：生成时直接写入（os.WriteFile 覆盖）→ 红（别的实例建出的文件被模板盖掉）。
func TestGenerateDoesNotOverwriteFileThatAppeared(t *testing.T) {
	s := newSandbox(t, "")
	ops := testOps()
	real := ops.createTemp
	appeared := false
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := real(dir, pattern)
		if !appeared && pattern == tempPattern { // 模板的临时文件建出之后，别的实例放上了它的 config.yml
			appeared = true
			s.write(v3Config)
		}
		return f, err
	}
	cfg, _ := s.load("4.0.0", ops)
	if cfg.ConfigFileState != FileLoaded || cfg.FileRewrite != RewriteMigrated || cfg.OrphanBackup != "" {
		t.Errorf("加载结果=%d、改写=%d，期望加载别的实例建出的那一份并迁移", cfg.ConfigFileState, cfg.FileRewrite)
	}
	wantOffsets(t, cfg, -200, -430)
	if s.read() != v3Migrated("v4.0.0") {
		t.Errorf("config.yml 应是那一份的迁移结果，实际\n%s", s.read())
	}
	s.wantFiles(configName, "config.backup-v3-*.yml")
}

// 按模板生成时模板放不上去（硬链接与不覆盖的改名都失败）：告警，不留 config.yml 也不留临时文件，本次以
// 内置默认继续。
func TestGeneratePlaceFailure(t *testing.T) {
	s := newSandbox(t, "")
	ops := testOps()
	ops.link = func(string, string) error { return errInjected }
	ops.moveNoReplace = func(string, string) error { return errInjected }
	cfg, logs := s.load("4.0.0", ops)
	if cfg.ConfigFileState != FileGenerateFailed || cfg.Offset != DefaultConfig().Offset || !strings.Contains(logs, "[!]") {
		t.Errorf("加载结果=%d、offset=%d，期望未能生成、内置默认并告警\n%s", cfg.ConfigFileState, cfg.Offset, logs)
	}
	s.wantFiles()
}
