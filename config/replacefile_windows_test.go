//go:build windows

package config

// 本文件在真实的文件系统上测不覆盖的改名（moveNoReplace，Windows 上是不带任何标志的 MoveFileEx），以及
// 硬链接不可用时经它走完的各条路径。别的平台上没有这条退路（replacefile_other.go），所以只在 Windows 上
// 跑；注入失败的各例见 replacefile_test.go、upgradefile_test.go、rebuildfile_test.go。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// 不覆盖的改名：目标已存在时返回「已存在」，源与目标的字节都不变；目标不存在时源整份移过去，源名不再
// 存在；源不存在时报错，目标不出现。
//
// 变异自证（隔离的 worktree，实测）：
//   - 换成会覆盖的 os.Rename →「目标已存在」红（目标被换成源的内容），TestReplaceFileWithoutHardLinks 的
//     两例「config.yml 已存在」与「放回时 config.yml 又出现了」、TestLoadAndRebuildWithoutHardLinks 的
//     「按模板生成时 config.yml 已出现」同红。
//   - 返回的错误认不出「已存在」→「目标已存在」与上述两例「config.yml 已存在」红。
//   - 只给 MoveFileEx 带上 MOVEFILE_COPY_ALLOWED → 不红：同一卷内的改名用不到复制。这个标志靠人守。
func TestMoveNoReplace(t *testing.T) {
	t.Run("目标已存在", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		writeTestFile(t, src, "new")
		writeTestFile(t, dst, "in place")
		if err := moveNoReplace(src, dst); !errors.Is(err, fs.ErrExist) {
			t.Errorf("应返回「已存在」，实际 %v", err)
		}
		if readTestFile(t, src) != "new" || readTestFile(t, dst) != "in place" {
			t.Errorf("源与目标的字节都不应变：源 %q、目标 %q", readTestFile(t, src), readTestFile(t, dst))
		}
	})
	t.Run("目标不存在", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		writeTestFile(t, src, "new")
		if err := moveNoReplace(src, dst); err != nil {
			t.Fatalf("应成功，实际 %v", err)
		}
		if got := readTestFile(t, dst); got != "new" {
			t.Errorf("目标 = %q，期望源的内容", got)
		}
		if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("改名之后源名应不存在，Lstat 返回 %v", err)
		}
	})
	t.Run("源不存在", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "dst")
		if err := moveNoReplace(filepath.Join(dir, "missing"), dst); err == nil || errors.Is(err, fs.ErrExist) {
			t.Errorf("应报错且不是「已存在」，实际 %v", err)
		}
		if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("目标不应出现，Lstat 返回 %v", err)
		}
	})
	t.Run("目标已存在而源不存在", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "dst")
		writeTestFile(t, dst, "in place")
		if err := moveNoReplace(filepath.Join(dir, "missing"), dst); err == nil {
			t.Error("应报错")
		}
		if got := readTestFile(t, dst); got != "in place" {
			t.Errorf("目标 = %q，不应被动过", got)
		}
	})
	// 路径原样交给 MoveFileEx，没有 \\?\ 前缀。超过 MAX_PATH 的路径仍然可用，靠的是 Go 运行时在
	// Windows 10 1703（10.0.15063）及以上给整个进程置上 IsLongPathAwareProcess（runtime 的
	// initLongPathSupport）；更早的系统上没有这一条，本例跳过。
	t.Run("路径超过 MAX_PATH", func(t *testing.T) {
		if v := windows.RtlGetVersion(); v.MajorVersion < 10 || (v.MajorVersion == 10 && v.BuildNumber < 15063) {
			t.Skipf("Windows %d.%d.%d 上进程不带长路径支持", v.MajorVersion, v.MinorVersion, v.BuildNumber)
		}
		dir := filepath.Join(t.TempDir(), strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		if len(src) <= windows.MAX_PATH || len(dst) <= windows.MAX_PATH {
			t.Fatalf("夹具的路径应超过 MAX_PATH，实际 %d、%d", len(src), len(dst))
		}
		writeTestFile(t, src, "new")
		if err := moveNoReplace(src, dst); err != nil {
			t.Fatalf("应成功，实际 %v", err)
		}
		if got := readTestFile(t, dst); got != "new" {
			t.Errorf("目标 = %q，期望源的内容", got)
		}
		writeTestFile(t, src, "newer")
		if err := moveNoReplace(src, dst); !errors.Is(err, fs.ErrExist) {
			t.Errorf("目标已存在时应返回「已存在」，实际 %v", err)
		}
		if readTestFile(t, src) != "newer" || readTestFile(t, dst) != "new" {
			t.Errorf("源与目标的字节都不应变")
		}
	})
}

// noLinks 返回硬链接不可用（os.Link 失败且不是「已存在」）的文件操作，退路是真实的不覆盖改名。
func noLinks() *fileOps {
	ops := testOps()
	ops.link = func(string, string) error { return errInjected }
	return ops
}

// 硬链接不可用时，替换 config.yml 的步骤经不覆盖的改名走完，结果与硬链接一路相同：放置之后没有临时文件
// （它已被改名为 config.yml，其后不再按这个名字删除，见 TestMovedSourceNameIsNotRemoved），放回之后没有
// 备份名。此时 config.yml 已存在同样不覆盖，那一份的字节不变。
//
// 变异自证（隔离的 worktree，实测）：硬链接失败时不走退路 →「成功」「config.yml 已存在」「加载之后被
// 保存过」各两例红，TestLoadAndRebuildWithoutHardLinks 五例红。
func TestReplaceFileWithoutHardLinks(t *testing.T) {
	for _, mode := range replaceModes {
		t.Run("成功/"+mode.name, func(t *testing.T) {
			s := newSandbox(t, "old")
			res := replaceFile(noLinks(), s.path, []byte("new"), []byte("old"), "v3", mode.keepBackup)
			if res.failure != replaceDone || res.err != nil || s.read() != "new" {
				t.Fatalf("结果 = %+v、config.yml = %q，期望成功且为 new", res, s.read())
			}
			if !mode.keepBackup {
				s.wantFiles(configName)
				return
			}
			s.wantFiles(configName, "config.backup-v3-*.yml")
			if readTestFile(t, res.backup) != "old" {
				t.Errorf("备份 %s 应是 old", res.backup)
			}
		})
		t.Run("config.yml 已存在：不覆盖/"+mode.name, func(t *testing.T) {
			s := newSandbox(t, "old")
			ops := noLinks()
			ops.step = func(step string) {
				if step == stepVerified {
					s.write("other")
				}
			}
			res := replaceFile(ops, s.path, []byte("new"), []byte("old"), "v3", mode.keepBackup)
			if res.failure != replaceFileAppeared || s.read() != "other" || readTestFile(t, res.backup) != "old" {
				t.Errorf("结果 = %+v、config.yml = %q，期望不覆盖别的程序新建的那一份、备份为 old", res, s.read())
			}
			s.wantFiles(configName, "config.backup-v3-*.yml") // 临时文件已删
		})
		t.Run("加载之后被保存过：放回/"+mode.name, func(t *testing.T) {
			s := newSandbox(t, "saved")
			res := replaceFile(noLinks(), s.path, []byte("new"), []byte("old"), "v3", mode.keepBackup)
			if res.failure != replaceFileChanged || res.backup != "" || s.read() != "saved" {
				t.Errorf("结果 = %+v、config.yml = %q，期望放回被保存过的那一份、不留备份", res, s.read())
			}
			s.wantFiles(configName)
		})
	}
	t.Run("放回时 config.yml 又出现了：两份都留", func(t *testing.T) {
		s := newSandbox(t, "saved")
		ops := noLinks()
		ops.step = func(step string) {
			if step == stepRenamed {
				s.write("other")
			}
		}
		res := replaceFile(ops, s.path, []byte("new"), []byte("old"), "v3", true)
		if res.failure != replaceFileChanged || s.read() != "other" || res.backup == "" || readTestFile(t, res.backup) != "saved" {
			t.Errorf("结果 = %+v、config.yml = %q，期望两份都留", res, s.read())
		}
		s.wantFiles(configName, "config.backup-v3-*.yml")
	})
}

// plantedContent 是哨兵文件的内容。
const plantedContent = "another instance's file"

// plantAfterMove 返回硬链接不可用的文件操作。它的不覆盖改名在真实的改名成功之后、返回之前，在 src 的原名
// 处建一个哨兵文件（别的实例的 os.CreateTemp 恰好生成了这个刚空出的名字），并把路径记进返回的切片。
func plantAfterMove(t *testing.T) (*fileOps, *[]string) {
	ops, planted := noLinks(), &[]string{}
	ops.moveNoReplace = func(src, dst string) error {
		if err := moveNoReplace(src, dst); err != nil {
			return err
		}
		writeTestFile(t, src, plantedContent)
		*planted = append(*planted, src)
		return nil
	}
	return ops, planted
}

// 经改名放置成功之后，src 的名字已经空出，不再属于本次操作：调用方不得再按这个名字删除。别的实例若恰好
// 拿到了这个名字，被删的会是它的文件。各条路径上在空出的名字处放一个哨兵，结束时它须仍在：迁移、只改
// 版本键、强制重建、按模板生成（空出的是临时文件名），以及「加载之后被保存过：放回」（空出的是备份名）。
//
// 变异自证（隔离的 worktree，实测）：
//   - replaceFile 放置之后改回无条件删临时文件 →「迁移」「只改版本键」「强制重建」红。
//   - 放回之后改回无条件删备份名 → 两例「加载之后被保存过：放回」红。
//   - generateDefaultConfig 改回无条件删临时文件 →「按模板生成」红。
//   - placeIfAbsent 的 moved 恒为假 → 六例全红（三处调用方原先都是无条件删，那时同为六例全红）。
func TestMovedSourceNameIsNotRemoved(t *testing.T) {
	const stamped = "config-version: \"v4.0.0\"\noffset: -200\nkugou-offset: -430\n"
	tmp := configName + ".*.tmp"
	// wantPlanted 断言经改名放置了恰好一次，空出的名字符合 glob，其上的哨兵仍在。
	wantPlanted := func(t *testing.T, planted *[]string, glob string) {
		t.Helper()
		if len(*planted) != 1 {
			t.Fatalf("经改名放置了 %d 次（%v），期望 1 次", len(*planted), *planted)
		}
		name := filepath.Base((*planted)[0])
		if ok, _ := filepath.Match(glob, name); !ok {
			t.Errorf("空出的名字 %s 应符合 %s", name, glob)
		}
		if b, err := os.ReadFile((*planted)[0]); err != nil || string(b) != plantedContent {
			t.Errorf("空出的名字 %s 上别的实例的文件被删或被改了：%q，%v", name, b, err)
		}
	}

	t.Run("迁移", func(t *testing.T) {
		s := newSandbox(t, v3Config)
		ops, planted := plantAfterMove(t)
		cfg, _ := s.load("4.0.0", ops)
		if cfg.FileRewrite != RewriteMigrated || s.read() != v3Migrated("v4.0.0") {
			t.Fatalf("改写=%d，期望已迁移", cfg.FileRewrite)
		}
		wantPlanted(t, planted, tmp)
		s.wantFiles(configName, "config.backup-v3-*.yml", tmp)
	})
	t.Run("只改版本键", func(t *testing.T) {
		s := newSandbox(t, stamped)
		ops, planted := plantAfterMove(t)
		cfg, _ := s.load("4.0.1", ops)
		if cfg.FileRewrite != RewriteStamped {
			t.Fatalf("改写=%d，期望只改版本键", cfg.FileRewrite)
		}
		wantPlanted(t, planted, tmp)
		s.wantFiles(configName, tmp) // 自己的备份照常删掉
	})
	t.Run("强制重建", func(t *testing.T) {
		s := newSandbox(t, stamped)
		cfg, _ := s.load("4.0.0", testOps())
		ops, planted := plantAfterMove(t)
		var rep RebuildReport
		captureLog(t, func() { rep = forceRebuild(ops, &cfg, "4.0.0") })
		if !rep.Done || s.read() != templateContent("v4.0.0") {
			t.Fatalf("重建结果 = %+v，期望已重建", rep)
		}
		wantPlanted(t, planted, tmp)
		s.wantFiles(configName, "config.backup-v4.0.0-*.yml", tmp)
	})
	t.Run("按模板生成", func(t *testing.T) {
		s := newSandbox(t, "")
		ops, planted := plantAfterMove(t)
		cfg, _ := s.load("4.0.0", ops)
		if cfg.ConfigFileState != FileGenerated {
			t.Fatalf("加载结果=%d，期望按模板生成", cfg.ConfigFileState)
		}
		wantPlanted(t, planted, tmp)
		s.wantFiles(configName, tmp)
	})
	for _, c := range []struct {
		name, content, version, backupGlob string
		pending                            RewriteState
	}{
		{"迁移", v3Config, "4.0.0", "config.backup-v3-*.yml", RewriteMigratePending},
		{"只改版本键", stamped, "4.0.1", "config.backup-v4.0.0-*.yml", RewriteStampPending},
	} {
		t.Run("加载之后被保存过：放回/"+c.name, func(t *testing.T) {
			const saved = "offset: 700\n"
			s := newSandbox(t, c.content)
			ops, planted := plantAfterMove(t)
			ops.step = func(step string) {
				if step == stepTempWritten {
					s.write(saved)
				}
			}
			cfg, _ := s.load(c.version, ops)
			if cfg.FileRewrite != c.pending || cfg.FileBackup != "" || s.read() != saved {
				t.Fatalf("改写=%d、备份=%q、config.yml=%q，期望放回被保存过的那一份", cfg.FileRewrite, cfg.FileBackup, s.read())
			}
			wantPlanted(t, planted, c.backupGlob)
			s.wantFiles(configName, c.backupGlob) // 备份名下是别的实例的文件；自己的临时文件已删
		})
	}
}

// 硬链接不可用时，经 loadFile 与 forceRebuild 的四条路径照常完成，磁盘上的结果与硬链接一路相同。
func TestLoadAndRebuildWithoutHardLinks(t *testing.T) {
	t.Run("迁移", func(t *testing.T) {
		s := newSandbox(t, v3Config)
		cfg, _ := s.load("4.0.0", noLinks())
		wantOffsets(t, cfg, -200, -430)
		if cfg.FileRewrite != RewriteMigrated || s.read() != v3Migrated("v4.0.0") || readTestFile(t, cfg.FileBackup) != v3Config {
			t.Errorf("改写=%d，期望已迁移、备份为原文件\n%s", cfg.FileRewrite, s.read())
		}
		s.wantFiles(configName, "config.backup-v3-*.yml")
	})
	t.Run("只改版本键", func(t *testing.T) {
		s := newSandbox(t, "config-version: \"v4.0.0\"\noffset: -200\n")
		cfg, _ := s.load("4.0.1", noLinks())
		if cfg.FileRewrite != RewriteStamped || s.read() != "config-version: \"v4.0.1\"\noffset: -200\n" {
			t.Errorf("改写=%d、config.yml=%q，期望只改版本键", cfg.FileRewrite, s.read())
		}
		s.wantFiles(configName)
	})
	t.Run("强制重建", func(t *testing.T) {
		const content = "config-version: \"v4.0.0\"\noffset: -5\n"
		s := newSandbox(t, content)
		cfg, _ := s.load("4.0.0", testOps())
		var rep RebuildReport
		captureLog(t, func() { rep = forceRebuild(noLinks(), &cfg, "4.0.0") })
		if !rep.Done || s.read() != templateContent("v4.0.0") || readTestFile(t, rep.Backup) != content {
			t.Errorf("重建结果 = %+v，期望 config.yml 为模板、备份为被替换的那一份", rep)
		}
		s.wantFiles(configName, "config.backup-v4.0.0-*.yml")
	})
	t.Run("按模板生成", func(t *testing.T) {
		s := newSandbox(t, "")
		cfg, _ := s.load("4.0.0", noLinks())
		if cfg.ConfigFileState != FileGenerated || s.read() != templateContent("v4.0.0") {
			t.Errorf("加载结果=%d，期望按模板生成", cfg.ConfigFileState)
		}
		s.wantFiles(configName)
	})
	t.Run("按模板生成时 config.yml 已出现：不覆盖", func(t *testing.T) {
		s := newSandbox(t, "")
		ops := noLinks()
		appeared := false
		ops.step = func(step string) {
			if !appeared && step == stepTempWritten { // 模板的临时文件写好之后，别的实例放上了它的 config.yml
				appeared = true
				s.write(v3Config)
			}
		}
		cfg, _ := s.load("4.0.0", ops)
		if cfg.ConfigFileState != FileLoaded || cfg.FileRewrite != RewriteMigrated || s.read() != v3Migrated("v4.0.0") {
			t.Errorf("加载结果=%d、改写=%d，期望加载别的实例建出的那一份并迁移", cfg.ConfigFileState, cfg.FileRewrite)
		}
		s.wantFiles(configName, "config.backup-v3-*.yml")
	})
}
