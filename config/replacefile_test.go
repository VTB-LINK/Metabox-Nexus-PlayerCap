package config

// 本文件只测替换 config.yml 的步骤本身（replaceFile 及其两个原语、备份的命名与查找）：各步成功与失败时
// 的返回与磁盘上的文件。它用的内容是任意字节，不涉及版本与迁移；经 loadFile 的整套流程、各步之后崩溃
// 再启动的结果见 upgradefile_test.go，两个实例交错见 concurrentinstances_test.go。硬链接不可用时的退路
// （不覆盖的改名）在这里一律注入；它在真实文件系统上的行为见 replacefile_windows_test.go。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 两种模式：迁移（留备份）与只改版本键（成功后删备份）。
var replaceModes = []struct {
	name       string
	keepBackup bool
}{{"迁移", true}, {"只改版本键", false}}

// 正常路径：config.yml 换成新内容，没有临时文件；迁移留下一份备份（被换下的那一份），只改版本键不留。
//
// 变异自证（隔离的 worktree，实测）：只改版本键成功后不删备份 → 该例红；迁移成功后也删备份 → 该例红；
// 成功后不删临时文件 → 两例红。
func TestReplaceFileSucceeds(t *testing.T) {
	for _, mode := range replaceModes {
		t.Run(mode.name, func(t *testing.T) {
			s := newSandbox(t, "old")
			res := replaceFile(testOps(), s.path, []byte("new"), []byte("old"), "v3", mode.keepBackup)
			if res.failure != replaceDone || res.err != nil {
				t.Fatalf("结果 = %+v，期望成功", res)
			}
			if got := s.read(); got != "new" {
				t.Errorf("config.yml = %q，期望 new", got)
			}
			if !mode.keepBackup {
				s.wantFiles(configName)
				if res.backup != "" {
					t.Errorf("只改版本键不应报告备份，实际 %q", res.backup)
				}
				return
			}
			s.wantFiles(configName, "config.backup-v3-20261009-153045-*.yml")
			if got := readTestFile(t, res.backup); got != "old" || filepath.Dir(res.backup) != s.dir {
				t.Errorf("备份 %s = %q，期望同目录下的 old", res.backup, got)
			}
		})
	}
}

// 各步失败：返回对应的原因，磁盘上的文件与设计 §3.6 的表一致。两种模式下相同（放置没成功时备份都不删）。
//
// 变异自证（隔离的 worktree，实测）：
//   - 放置新文件时允许覆盖已存在的 config.yml（先删再放）→「放置时 config.yml 已存在」两例与放回的
//     「…又出现了」红。
//   - 只改版本键路径在自己的放置没成功时也删备份 →「放置时 config.yml 已存在」的只改版本键一例红。
//   - 预留备份名成功而改名失败后不删空的预留文件 →「改名失败」红。
//   - 不核对改名得到的那一份 →「加载之后被保存过」各例红（被保存的内容成了备份、config.yml 是旧内容
//     的改写）；核对不同也照写、不放回 → 同上各例红；读不出时照写 →「备份读不出」红。
//   - 写临时文件失败后仍去改名 →「写临时文件失败」红；放置失败后不删临时文件 → 各放置失败的例红。
//   - 硬链接失败时不走退路（直接返回硬链接的错误）→「硬链接不可用、改名报已存在」「…其间别的程序建出了
//     config.yml」各两例红。
//   - 放置失败后删除 config.yml（placeIfAbsent 在硬链接与改名都失败时删 dst）→「…其间别的程序建出了
//     config.yml」两例红（那一份被删掉，没有任何副本），「放置因别的原因失败…：放回」「放置失败且放不回」
//     各两例因 newSandbox 收尾的断言而红。原先的实现退回带 O_EXCL 的创建并写入，写入出错后正是这样
//     清理的：让写入失败、并在句柄关闭之后删除之前把 config.yml 换成另一份内容，那一份被删且没有副本
//     （在那份实现上实测）。
func TestReplaceFileFailures(t *testing.T) {
	type want struct {
		failure replaceFailure
		config  string // config.yml 的内容；"-" 表示不存在
		backup  string // 留下的备份的内容；"" 表示没有备份
	}
	isTemp := func(name string) bool { return strings.HasSuffix(name, ".tmp") }
	cases := []struct {
		name    string
		initial string                                       // config.yml 的初始内容（expect 恒为 "old"）
		prepare func(t *testing.T, s *sandbox, ops *fileOps) // 注入失败或别的程序的动作
		want    want
	}{
		{"写临时文件失败", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			real := ops.createTemp
			ops.createTemp = func(dir, pattern string) (*os.File, error) {
				if pattern == tempPattern {
					return nil, errInjected
				}
				return real(dir, pattern)
			}
			ops.rename = func(string, string) error { t.Error("写临时文件失败后不应再改名"); return errInjected }
		}, want{replaceTempFailed, "old", ""}},

		{"预留备份名失败", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			real := ops.createTemp
			ops.createTemp = func(dir, pattern string) (*os.File, error) {
				if pattern != tempPattern {
					return nil, errInjected
				}
				return real(dir, pattern)
			}
		}, want{replaceRenameFailed, "old", ""}},

		{"改名失败", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.rename = func(string, string) error { return errInjected }
		}, want{replaceRenameFailed, "old", ""}},

		{"改名时 config.yml 已不在", "", func(t *testing.T, s *sandbox, ops *fileOps) {}, want{replaceRenameFailed, "-", ""}},

		{"加载之后被保存过：放回", "saved", func(t *testing.T, s *sandbox, ops *fileOps) {}, want{replaceFileChanged, "saved", ""}},

		{"加载之后被保存过，放回时 config.yml 又出现了：两份都留", "saved", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepRenamed {
					s.write("other")
				}
			}
		}, want{replaceFileChanged, "other", "saved"}},

		{"备份读不出：放回", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.readFile = func(string) ([]byte, error) { return nil, errInjected }
		}, want{replaceFileChanged, "old", ""}},

		{"放置时 config.yml 已存在：不覆盖", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepVerified {
					s.write("other")
				}
			}
		}, want{replaceFileAppeared, "other", "old"}},

		{"硬链接不可用、改名报已存在：同样按 config.yml 已存在处理", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(src, dst string) error {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: fs.ErrExist}
			}
			ops.step = func(step string) {
				if step == stepVerified {
					s.write("other")
				}
			}
		}, want{replaceFileAppeared, "other", "old"}},

		{"放置因别的原因失败（硬链接与改名都失败）：放回", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			real := ops.link
			ops.link = func(src, dst string) error {
				if isTemp(src) {
					return errInjected
				}
				return real(src, dst)
			}
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}, want{replacePlaceFailed, "old", ""}},

		{"放置失败且放不回：备份留着", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}, want{replacePlaceFailed, "-", "old"}},

		// 放置与放回各失败一次，每次失败之前 config.yml 都由别的程序建出（内容 U2）。本次操作没有在这个
		// 名字上建过任何东西，不得删它。
		{"硬链接与改名都失败、其间别的程序建出了 config.yml：不删它", "old", func(t *testing.T, s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { s.write("U2"); return errInjected }
		}, want{replacePlaceFailed, "U2", "old"}},
	}
	for _, c := range cases {
		for _, mode := range replaceModes {
			t.Run(c.name+"/"+mode.name, func(t *testing.T) {
				s := newSandbox(t, c.initial)
				ops := testOps()
				c.prepare(t, s, ops)
				res := replaceFile(ops, s.path, []byte("new"), []byte("old"), "v3", mode.keepBackup)

				if res.failure != c.want.failure {
					t.Errorf("失败原因 = %d（%v），期望 %d", res.failure, res.err, c.want.failure)
				}
				files := []string{}
				if c.want.config == "-" {
					if s.exists() {
						t.Errorf("config.yml 不应存在，实际内容 %q", s.read())
					}
				} else {
					files = append(files, configName)
					if got := s.read(); got != c.want.config {
						t.Errorf("config.yml = %q，期望 %q", got, c.want.config)
					}
				}
				if c.want.backup == "" {
					if res.backup != "" {
						t.Errorf("不应留下备份，实际报告 %q", res.backup)
					}
				} else {
					files = append(files, "config.backup-v3-*.yml")
					if res.backup == "" || readTestFile(t, res.backup) != c.want.backup {
						t.Errorf("备份 %q 的内容应是 %q", res.backup, c.want.backup)
					}
				}
				s.wantFiles(files...) // 没有临时文件，没有空的预留备份名
			})
		}
	}
}

// 强制重建一路（expect 为 nil）不比对：config.yml 此刻是什么，备份里就是什么。放置因别的原因失败时不放回
// （没有 config.yml 而有备份，下次启动按模板生成，正是重建要的结果）。
//
// 变异自证（隔离的 worktree，实测）：expect 为 nil 时也比对（与空内容比）→「不比对」红；放置失败时放回 →
// 「放置失败不放回」红。
func TestReplaceFileWithoutExpectation(t *testing.T) {
	t.Run("不比对", func(t *testing.T) {
		s := newSandbox(t, "whatever is there now")
		res := replaceFile(testOps(), s.path, []byte("template"), nil, "v4.0.0", true)
		if res.failure != replaceDone || s.read() != "template" || readTestFile(t, res.backup) != "whatever is there now" {
			t.Fatalf("结果 = %+v、config.yml = %q", res, s.read())
		}
		s.wantFiles(configName, "config.backup-v4.0.0-*.yml")
	})
	t.Run("放置失败不放回", func(t *testing.T) {
		s := newSandbox(t, "old")
		ops := testOps()
		real := ops.link
		ops.link = func(src, dst string) error {
			if strings.HasSuffix(src, ".tmp") {
				return errInjected
			}
			return real(src, dst)
		}
		ops.moveNoReplace = func(string, string) error { return errInjected }
		res := replaceFile(ops, s.path, []byte("template"), nil, "v3", true)
		if res.failure != replacePlaceFailed || s.exists() || readTestFile(t, res.backup) != "old" {
			t.Fatalf("结果 = %+v、config.yml 存在 = %v，期望放置失败、没有 config.yml、备份为 old", res, s.exists())
		}
		s.wantFiles("config.backup-v3-*.yml")
	})
}

// 目录里已有别的临时文件与空的备份名（崩溃遗留，或别的实例正在用）：本次操作不删、不读、不链接它们。
//
// 变异自证（隔离的 worktree，实测）：临时文件改回固定的名字（config.yml.tmp）→ 红（别人的临时文件被
// 覆盖、随后被删）；写入前按名字模式清理别的临时文件 → 红。
func TestReplaceFileLeavesForeignFilesAlone(t *testing.T) {
	s := newSandbox(t, "old")
	foreign := map[string]string{
		"config.yml.tmp":                          "someone else's temp, fixed name",
		"config.yml.123.tmp":                      "someone else's temp",
		"config.backup-v3-20261009-153045-77.yml": "",
		"config.backup-v3-20260101-000000-1.yml":  "an older backup",
	}
	for name, content := range foreign {
		writeTestFile(t, filepath.Join(s.dir, name), content)
	}
	ops := testOps()
	ops.readFile = func(path string) ([]byte, error) {
		if _, isForeign := foreign[filepath.Base(path)]; isForeign {
			t.Errorf("不应读别人的文件 %s", path)
		}
		return os.ReadFile(path)
	}
	ops.link = func(src, dst string) error {
		if _, isForeign := foreign[filepath.Base(src)]; isForeign {
			t.Errorf("不应链接别人的文件 %s", src)
		}
		return os.Link(src, dst)
	}
	res := replaceFile(ops, s.path, []byte("new"), []byte("old"), "v3", true)
	if res.failure != replaceDone || s.read() != "new" {
		t.Fatalf("结果 = %+v、config.yml = %q", res, s.read())
	}
	for name, content := range foreign {
		if got := s.file(name); got != content {
			t.Errorf("别人的文件 %s 被改成 %q", name, got)
		}
	}
	if len(s.names("*")) != len(foreign)+2 {
		t.Errorf("目录里应是别人的 %d 个文件加 config.yml 与本次的备份，实际 %v", len(foreign), s.names("*"))
	}
}

// 备份名：config.backup-<版本>-<本地时间>-<随机段>.yml，版本取文件的版本（没有版本键写作 v3，非法写作
// unknown，预发布后缀保留）。
func TestBackupNaming(t *testing.T) {
	for _, c := range []struct {
		version string
		state   versionKeyState
		label   string
	}{
		{legacyFileVersion, versionKeyAbsent, "v3"},
		{"v3.0.0", versionKeyValid, "v3.0.0"},
		{"v4.0.0", versionKeyValid, "v4.0.0"},
		{"v4.0.0-beta.3", versionKeyValid, "v4.0.0-beta.3"},
		{"v4.0.0-0", versionKeyValid, "v4.0.0-0"},
		{"", versionKeyInvalid, "unknown"},
	} {
		label := backupLabel(c.version, c.state)
		if label != c.label {
			t.Errorf("backupLabel(%q, %d) = %q，期望 %q", c.version, c.state, label, c.label)
		}
		s := newSandbox(t, "old")
		res := replaceFile(testOps(), s.path, []byte("new"), nil, label, true)
		name := filepath.Base(res.backup)
		want := regexp.MustCompile(`^config\.backup-` + regexp.QuoteMeta(c.label) + `-20261009-153045-\d+\.yml$`)
		if !want.MatchString(name) {
			t.Errorf("备份名 %q 不符合 config.backup-%s-<时间>-<随机段>.yml", name, c.label)
		}
		if g := backupNameRE.FindStringSubmatch(name); g == nil || g[1] != c.label || g[2] != "20261009-153045" {
			t.Errorf("backupNameRE 从 %q 里认出 %v，期望版本 %s、时间 20261009-153045", name, g, c.label)
		}
	}
}

// 同一秒、同一版本连续两次：名字不同，先前那一份的字节不变。名字是预留出来的，不是先查后用；先查后用
// 的写法在这种先后两次的调用里看不出差别，要两个实例交错才露出来，见 concurrentinstances_test.go 的
// TestConcurrentMigrations。
func TestBackupNamesAreReservedNotProbed(t *testing.T) {
	s := newSandbox(t, "first")
	a := replaceFile(testOps(), s.path, []byte("second"), nil, "v3", true)
	b := replaceFile(testOps(), s.path, []byte("third"), nil, "v3", true)
	if a.failure != replaceDone || b.failure != replaceDone || a.backup == b.backup {
		t.Fatalf("两次都应成功且备份名不同：%+v / %+v", a, b)
	}
	if readTestFile(t, a.backup) != "first" || readTestFile(t, b.backup) != "second" || s.read() != "third" {
		t.Errorf("备份应依次是 first、second，config.yml 是 third")
	}
	s.wantFiles(configName, "config.backup-v3-20261009-153045-*.yml", "config.backup-v3-20261009-153045-*.yml")
}

// 不存在才放置（真实的硬链接）：目标不在时放上去、源仍在；目标已在时返回「已存在」且不动它。两种情形
// 下源都没有被移走。
func TestPlaceIfAbsentByLink(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeTestFile(t, src, "content")
	if moved, err := placeIfAbsent(testOps(), src, dst); err != nil || moved {
		t.Fatalf("目标不在时应经硬链接成功、源不移走：moved=%v err=%v", moved, err)
	}
	if readTestFile(t, dst) != "content" || readTestFile(t, src) != "content" {
		t.Errorf("放置后目标与源都应是 content")
	}
	writeTestFile(t, src, "other")
	writeTestFile(t, dst, "in place")
	if moved, err := placeIfAbsent(testOps(), src, dst); !errors.Is(err, fs.ErrExist) || moved {
		t.Errorf("目标已在时应返回「已存在」、源不移走，实际 moved=%v err=%v", moved, err)
	}
	if got := readTestFile(t, dst); got != "in place" {
		t.Errorf("目标已在时不应动它，实际 %q", got)
	}
}

// 不存在才放置只用两个操作：先硬链接，它成功或报「已存在」即照此返回；因别的原因失败时退回不覆盖的
// 改名，结果原样返回。此外不碰任何文件：目标这个名字上没有创建、写入或删除，也不读源。返回的 moved 只在
// 经改名放置成功时为真：只有那时源名已经空出，调用方不得再删它。
//
// 变异自证（隔离的 worktree，实测）：
//   - 硬链接失败时不走退路（直接返回硬链接的错误）→ 三例「硬链接不可用」红。
//   - 放置失败后删除目标 →「硬链接不可用、改名失败」红。
//   - 硬链接报「已存在」时也去改名 →「硬链接报已存在」红。
//   - moved 恒为假 →「硬链接不可用、改名成功」红；恒为真 → 其余四例红，本包另有二十余个用例因留下
//     临时文件而红；改名失败时也报为真 →「硬链接不可用、改名报已存在」「硬链接不可用、改名失败」红，
//     TestGeneratePlaceFailure 与「按模板生成时 config.yml 已出现」因留下临时文件同红。
func TestPlaceIfAbsentUsesOnlyLinkAndMove(t *testing.T) {
	exists := &os.LinkError{Op: "link", Old: "src", New: "dst", Err: fs.ErrExist}
	errMove := errors.New("move failed")
	for _, c := range []struct {
		name       string
		link, move error // 硬链接与改名各自的结果
		calls      []string
		moved      bool
		want       error
	}{
		{"硬链接成功", nil, errMove, []string{"link src dst"}, false, nil},
		{"硬链接报已存在", exists, nil, []string{"link src dst"}, false, fs.ErrExist},
		{"硬链接不可用、改名成功", errInjected, nil, []string{"link src dst", "move src dst"}, true, nil},
		{"硬链接不可用、改名报已存在", errInjected, exists, []string{"link src dst", "move src dst"}, false, fs.ErrExist},
		{"硬链接不可用、改名失败", errInjected, errMove, []string{"link src dst", "move src dst"}, false, errMove},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			ops := &fileOps{
				link:          func(src, dst string) error { calls = append(calls, "link "+src+" "+dst); return c.link },
				moveNoReplace: func(src, dst string) error { calls = append(calls, "move "+src+" "+dst); return c.move },
				createTemp:    func(string, string) (*os.File, error) { t.Error("不应建文件"); return nil, errInjected },
				rename:        func(string, string) error { t.Error("不应做会覆盖的改名"); return errInjected },
				readFile:      func(string) ([]byte, error) { t.Error("不应读文件"); return nil, errInjected },
				remove:        func(path string) error { t.Errorf("不应删除 %s", path); return errInjected },
			}
			moved, err := placeIfAbsent(ops, "src", "dst")
			if !slices.Equal(calls, c.calls) {
				t.Errorf("做过的操作 = %v，期望 %v", calls, c.calls)
			}
			if moved != c.moved {
				t.Errorf("moved = %v，期望 %v", moved, c.moved)
			}
			if (c.want == nil) != (err == nil) || !errors.Is(err, c.want) {
				t.Errorf("返回 %v，期望 %v", err, c.want)
			}
		})
	}
}

// 最新一份非空备份：按名字里的时间取最新，空的（预留之后没来得及改名的）与名字不符的不算。
//
// 变异自证（隔离的 worktree，实测）：不跳过空的备份 →「最新的是空的」红；按整个文件名排序（版本在前）→
// 「版本号大的更旧」红。
func TestLatestBackup(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"没有备份", map[string]string{"config.yml": "x"}, ""},
		{"一份", map[string]string{"config.backup-v3-20261009-153045-1.yml": "a"}, "config.backup-v3-20261009-153045-1.yml"},
		{"取时间最新的", map[string]string{
			"config.backup-v3-20261009-153045-9.yml":     "a",
			"config.backup-v3-20261009-153046-1.yml":     "b",
			"config.backup-v4.0.0-20260930-235959-5.yml": "c",
		}, "config.backup-v3-20261009-153046-1.yml"},
		{"版本号大的更旧", map[string]string{
			"config.backup-v4.0.0-beta.3-20260101-000000-5.yml": "old",
			"config.backup-v3-20261009-153045-1.yml":            "new",
		}, "config.backup-v3-20261009-153045-1.yml"},
		{"最新的是空的", map[string]string{
			"config.backup-v3-20261009-153045-1.yml": "a",
			"config.backup-v3-20261010-000000-2.yml": "",
		}, "config.backup-v3-20261009-153045-1.yml"},
		{"只有空的", map[string]string{"config.backup-v3-20261009-153045-1.yml": ""}, ""},
		{"只有名字相近的", map[string]string{
			"config.backup.yml": "a", "config.backup-mine.yml": "b", "config.backup-v3-2026-1.yml": "c",
			"config.yml.123.tmp": "d", "xconfig.backup-v3-20261009-153045-1.yml": "e",
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range c.files {
				writeTestFile(t, filepath.Join(dir, name), content)
			}
			if got := latestBackup(dir); got != c.want {
				t.Errorf("latestBackup = %q，期望 %q", got, c.want)
			}
		})
	}
	if got := latestBackup(filepath.Join(t.TempDir(), "no-such-dir")); got != "" {
		t.Errorf("目录不存在时应返回空串，实际 %q", got)
	}
}
