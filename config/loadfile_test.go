package config

// 本文件只测 loadFile 记下的配置文件路径与加载结果（Config.ConfigFile / ConfigFileState）：启动横幅的
// 「配置文件」一行照此说明本次用的是哪一份 config.yml、是否本次新生成、是否因读取或解析失败而没有生效。
// 选址规则本身由 configpath_test 守；这里断言的是记下的路径就是实际读（或生成）的那一份。

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("建测试文件失败: %v", err)
	}
}

// loadFileWith 注入 exe 路径与当前目录后跑一次 loadFile，返回得到的 Config。
func loadFileWith(t *testing.T, exePath, cwd string, exeErr error) Config {
	t.Helper()
	withPaths(t, exePath, cwd, exeErr)
	cfg := DefaultConfig()
	cfg.Sources = []string{"内置默认"}
	captureLog(t, func() { loadFile(&cfg, "") })
	return cfg
}

// 两份 config.yml 写不同的 poll（exe 目录 111、当前目录 222），以合并进来的值确认记下的路径就是读的那一份；
// 没有生效的情形（解析失败）值保持内置默认、poll 不标记为显式、来源仍是内置默认。
//
// 变异自证（隔离副本，实测）：loadFile 不记路径 → 六例全红；记下的路径改为恒取 exe 目录（不取 configPath
// 的结果）→「只有当前目录有」红；生成后不置 FileGenerated →「两处都没有」红；解析失败不置
// FileParseFailed、或解析失败仍把来源记为 config.yml → 两例解析失败红。
func TestLoadFileRecordsPathAndState(t *testing.T) {
	const inExe, inCwd, broken = "poll: 111\n", "poll: 222\n", "poll: [1, 2\n"
	cases := []struct {
		name             string
		exeYAML, cwdYAML string // 空串 = 该目录没有 config.yml
		wantInCwd        bool   // 记下的应是当前目录那份
		wantState        FileState
		wantPoll         int
		wantMerged       bool // config.yml 一层生效：poll 标记为显式、来源为 config.yml
	}{
		{"exe 目录有", inExe, "", false, FileLoaded, 111, true},
		{"只有当前目录有", "", inCwd, true, FileLoaded, 222, true},
		{"两处都有", inExe, inCwd, false, FileLoaded, 111, true},
		{"两处都没有", "", "", false, FileGenerated, 30, true},
		{"解析失败", broken, "", false, FileParseFailed, 30, false},
		{"解析失败时不改用当前目录那份", broken, inCwd, false, FileParseFailed, 30, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exeDir, cwd := t.TempDir(), t.TempDir()
			if c.exeYAML != "" {
				writeConfig(t, filepath.Join(exeDir, configName), c.exeYAML)
			}
			if c.cwdYAML != "" {
				writeConfig(t, filepath.Join(cwd, configName), c.cwdYAML)
			}
			cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), cwd, nil)

			want := filepath.Join(exeDir, configName)
			if c.wantInCwd {
				want = filepath.Join(cwd, configName)
			}
			if cfg.ConfigFile != want {
				t.Errorf("记下的路径\n  得 %s\n  期望 %s", cfg.ConfigFile, want)
			}
			if cfg.ConfigFileState != c.wantState {
				t.Errorf("加载结果=%d，期望 %d", cfg.ConfigFileState, c.wantState)
			}
			if cfg.Poll != c.wantPoll {
				t.Errorf("poll=%d，期望 %d", cfg.Poll, c.wantPoll)
			}
			wantSource := "内置默认"
			if c.wantMerged {
				wantSource = "config.yml"
			}
			if cfg.ExplicitKeys["poll"] != c.wantMerged || len(cfg.Sources) != 1 || cfg.Sources[0] != wantSource {
				t.Errorf("poll 显式=%v、来源=%v，期望 %v、[%s]", cfg.ExplicitKeys["poll"], cfg.Sources, c.wantMerged, wantSource)
			}
			if _, err := os.Stat(cfg.ConfigFile); err != nil {
				t.Errorf("记下的路径上应有文件（读到的或本次生成的）: %v", err)
			}
		})
	}
}

// loadFile 记下文件在改写之前的版本（Config.FileVersion）：没有版本键的是 v3.0.0，合法的版本键读出规范形式，
// 版本键非法、解析失败时为空串（版本未知）。强制重建的判定只用这个值。
//
// 变异自证（隔离的 worktree，实测）：loadFile 不记文件版本 → 前四例红；解析失败时也记 v3.0.0 →「解析失败」红。
func TestLoadFileRecordsFileVersion(t *testing.T) {
	for _, c := range []struct{ name, yml, want string }{
		{"没有版本键", "poll: 111\n", "v3.0.0"},
		{"带 v", "config-version: \"v4.0.0-0\"\npoll: 111\n", "v4.0.0-0"},
		{"不带 v、不带引号", "config-version: 4.2.1\npoll: 111\n", "v4.2.1"},
		{"高于盖章值", "config-version: \"v9.0.0\"\npoll: 111\n", "v9.0.0"},
		{"版本键非法", "config-version: 4.0\npoll: 111\n", ""},
		{"解析失败", "config-version: \"v4.0.0\"\npoll: [1, 2\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			exeDir := t.TempDir()
			writeConfig(t, filepath.Join(exeDir, configName), c.yml)
			cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), t.TempDir(), nil)
			if cfg.FileVersion != c.want {
				t.Errorf("记下的文件版本 = %q，期望 %q", cfg.FileVersion, c.want)
			}
		})
	}
}

// 存在但读不出（此处用同名目录顶替文件）：记下该路径与读取失败，config.yml 一层不生效。
//
// 变异自证（隔离副本，实测）：读取失败的分支不置 FileReadFailed → 红。
func TestLoadFileReadFailure(t *testing.T) {
	exeDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(exeDir, configName), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), t.TempDir(), nil)

	if want := filepath.Join(exeDir, configName); cfg.ConfigFile != want {
		t.Errorf("记下的路径\n  得 %s\n  期望 %s", cfg.ConfigFile, want)
	}
	if cfg.ConfigFileState != FileReadFailed {
		t.Errorf("加载结果=%d，期望 FileReadFailed(%d)", cfg.ConfigFileState, FileReadFailed)
	}
	if cfg.ExplicitKeys["poll"] || cfg.Sources[0] != "内置默认" {
		t.Errorf("读取失败时 config.yml 一层不该生效，实得 poll 显式=%v、来源=%v", cfg.ExplicitKeys["poll"], cfg.Sources)
	}
}

// 两处都没有、exe 目录又写不进去（此处用不存在的目录）：记下本应生成的路径与生成失败，不回落到别处。
//
// 变异自证（隔离副本，实测）：不存在的分支进入时不预置 FileGenerateFailed → 红。
func TestLoadFileGenerateFailure(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "no-such-dir")
	cfg := loadFileWith(t, filepath.Join(missingDir, "app.exe"), t.TempDir(), nil)

	if want := filepath.Join(missingDir, configName); cfg.ConfigFile != want {
		t.Errorf("记下的路径\n  得 %s\n  期望 %s", cfg.ConfigFile, want)
	}
	if cfg.ConfigFileState != FileGenerateFailed {
		t.Errorf("加载结果=%d，期望 FileGenerateFailed(%d)", cfg.ConfigFileState, FileGenerateFailed)
	}
	if cfg.ExplicitKeys["poll"] || cfg.Sources[0] != "内置默认" {
		t.Errorf("生成失败时 config.yml 一层不该生效，实得 poll 显式=%v、来源=%v", cfg.ExplicitKeys["poll"], cfg.Sources)
	}
}

// 定位不了 exe 时 configPath 返回相对路径（按进程当前目录解析），记下的仍须是绝对路径，且指向实际生成的文件。
//
// 变异自证（隔离副本，实测）：记下 configPath 的原样结果（不转绝对路径）→ 红。
func TestLoadFileRecordsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := loadFileWith(t, "", dir, os.ErrPermission)

	if !filepath.IsAbs(cfg.ConfigFile) || filepath.Base(cfg.ConfigFile) != configName {
		t.Fatalf("记下的路径应是 %s 的绝对路径，实得 %q", configName, cfg.ConfigFile)
	}
	if cfg.ConfigFileState != FileGenerated {
		t.Errorf("加载结果=%d，期望 FileGenerated(%d)", cfg.ConfigFileState, FileGenerated)
	}
	got, err1 := os.Stat(cfg.ConfigFile)
	want, err2 := os.Stat(filepath.Join(dir, configName))
	if err1 != nil || err2 != nil || !os.SameFile(got, want) {
		t.Errorf("记下的路径应指向当前目录里生成的那一份（%v / %v）", err1, err2)
	}
}
