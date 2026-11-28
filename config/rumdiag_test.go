package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestLoadFileRecordsCWDConflict(t *testing.T) {
	t.Run("exe目录与CWD同时存在config.yml", func(t *testing.T) {
		exeDir, cwd := t.TempDir(), t.TempDir()
		touch(t, filepath.Join(exeDir, configName))
		touch(t, filepath.Join(cwd, configName))
		cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), cwd, nil)
		if !cfg.HasCWDConflict {
			t.Errorf("exe 目录与 CWD 均存在 config.yml 时 HasCWDConflict 应为 true")
		}
	})

	t.Run("仅exe目录存在", func(t *testing.T) {
		exeDir, cwd := t.TempDir(), t.TempDir()
		touch(t, filepath.Join(exeDir, configName))
		cfg := loadFileWith(t, filepath.Join(exeDir, "app.exe"), cwd, nil)
		if cfg.HasCWDConflict {
			t.Errorf("仅 exe 目录存在 config.yml 时 HasCWDConflict 应为 false")
		}
	})

	t.Run("同目录不冲突", func(t *testing.T) {
		dir := t.TempDir()
		touch(t, filepath.Join(dir, configName))
		cfg := loadFileWith(t, filepath.Join(dir, "app.exe"), dir, nil)
		if cfg.HasCWDConflict {
			t.Errorf("exe 目录与 CWD 相同时 HasCWDConflict 应为 false")
		}
	})
}

func TestUpgradeFileRecordsMigratedKeysAndRewriteFailReason(t *testing.T) {
	t.Run("迁移成功记录有序去重的已知键", func(t *testing.T) {
		s := newSandbox(t, v3Config)
		cfg, _ := s.load("4.0.0", testOps())
		if cfg.FileRewrite != RewriteMigrated {
			t.Fatalf("期望 RewriteMigrated，实际 %d", cfg.FileRewrite)
		}
		if cfg.RewriteFailReason != "" {
			t.Errorf("成功迁移时 RewriteFailReason 应为空串，实际 %q", cfg.RewriteFailReason)
		}
		want := []string{"kugou-offset", "offset"}
		if !slices.Equal(cfg.MigratedKeys, want) {
			t.Errorf("MigratedKeys = %v，期望 %v", cfg.MigratedKeys, want)
		}
	})

	utf16le := func(str string) string {
		var b []byte
		for _, u := range utf16.Encode(append([]rune{0xFEFF}, []rune(str)...)) {
			b = append(b, byte(u), byte(u>>8))
		}
		return string(b)
	}

	cases := []struct {
		name     string
		content  string
		readOnly bool
		prepare  func(s *sandbox, ops *fileOps)
		want     string
	}{
		{"只读文件", v3Config, true, nil, "read_only"},
		{"非UTF8", utf16le("offset: 200\r\n"), false, nil, "non_utf8"},
		{"多文档", "offset: 200\n---\nconfig-version: \"v4.0.0\"\n", false, nil, "multi_doc"},
		{"不支持语法_十六进制", "offset: 0x1f4\n", false, nil, "unsupported_syntax"},
		{"不支持语法_版本键带引号", "\"config-version\": \"v4.0.0-0\"\noffset: -200\n", false, nil, "unsupported_syntax"},
		{"回读校验不符_块标量版本键", "config-version: |\n  v4.0.0-0\noffset: -200\n", false, nil, "verify_mismatch"},
		{"写临时文件失败", v3Config, false, func(s *sandbox, ops *fileOps) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errInjected }
		}, "temp_failed"},
		{"改名失败", v3Config, false, func(s *sandbox, ops *fileOps) {
			ops.rename = func(string, string) error { return errInjected }
		}, "rename_failed"},
		{"加载期间被改动", v3Config, false, func(s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepTempWritten {
					s.write("offset: 700\n")
				}
			}
		}, "file_changed"},
		{"放置时已有新文件", v3Config, false, func(s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepVerified {
					s.write("offset: 9\n")
				}
			}
		}, "file_appeared"},
		{"放置失败", v3Config, false, func(s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}, "place_failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSandbox(t, tc.content)
			if tc.readOnly {
				if err := os.Chmod(s.path, 0444); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(s.path, 0644) })
			}
			ops := testOps()
			if tc.prepare != nil {
				tc.prepare(s, ops)
			}
			cfg, _ := s.load("4.0.0", ops)
			if cfg.RewriteFailReason != tc.want {
				t.Errorf("RewriteFailReason = %q，期望 %q", cfg.RewriteFailReason, tc.want)
			}
			if strings.Contains(cfg.RewriteFailReason, string(os.PathSeparator)) || strings.Contains(cfg.RewriteFailReason, ":") {
				t.Errorf("RewriteFailReason 不得含路径或错误原文：%q", cfg.RewriteFailReason)
			}
		})
	}
}

func TestForceRebuildRecordsFailKind(t *testing.T) {
	const content = "config-version: \"v4.0.0-0\"\noffset: -5\n"

	t.Run("成功时FailKind为空", func(t *testing.T) {
		s := newSandbox(t, content)
		cfg, _ := s.load("4.0.0", testOps())
		var rep RebuildReport
		captureLog(t, func() { rep = forceRebuild(testOps(), &cfg, "4.0.0") })
		if !rep.Done || rep.FailKind != "" {
			t.Errorf("成功重建时 Done=%v FailKind=%q，期望 true 与空串", rep.Done, rep.FailKind)
		}
	})

	for _, tc := range []struct {
		name    string
		prepare func(s *sandbox, ops *fileOps)
		want    string
	}{
		{"写临时文件失败", func(s *sandbox, ops *fileOps) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errInjected }
		}, "temp_failed"},
		{"改名失败", func(s *sandbox, ops *fileOps) {
			ops.rename = func(string, string) error { return errInjected }
		}, "rename_failed"},
		{"放置时已有新文件", func(s *sandbox, ops *fileOps) {
			ops.step = func(step string) {
				if step == stepRenamed {
					s.write("offset: 9\n")
				}
			}
		}, "file_appeared"},
		{"放置失败", func(s *sandbox, ops *fileOps) {
			ops.link = func(string, string) error { return errInjected }
			ops.moveNoReplace = func(string, string) error { return errInjected }
		}, "place_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSandbox(t, content)
			cfg, _ := s.load("4.0.0", testOps())
			ops := testOps()
			tc.prepare(s, ops)
			var rep RebuildReport
			captureLog(t, func() { rep = forceRebuild(ops, &cfg, "4.0.0") })
			if rep.Done || rep.FailKind != tc.want {
				t.Errorf("重建失败时 Done=%v FailKind=%q，期望 false 与 %q", rep.Done, rep.FailKind, tc.want)
			}
		})
	}
}

func TestIsKnownKeyMatchesMergeYAMLAndRejectsUnknown(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读取 config.go 失败: %v", err)
	}
	keys, _, _ := scanKeys(t, string(src), t.Errorf)
	if len(keys) == 0 {
		t.Fatal("未从 mergeYAML 扫出配置键")
	}

	const testPlayer = "zzknownkey"
	registerForTest(testPlayer)

	for _, k := range keys {
		actual := strings.Replace(k, playerKey, testPlayer, 1)
		if !IsKnownKey(actual) {
			t.Errorf("IsKnownKey(%q) = false，但 mergeYAML 认识该键", actual)
		}
	}

	for _, bad := range []string{
		"",
		"unknown-key",
		"D:\\Secret\\song.mp3",
		"unregisteredplayer-offset",
		"zzknownkey-unknown",
		"config-version",
		"song_name",
	} {
		if IsKnownKey(bad) {
			t.Errorf("IsKnownKey(%q) = true，但该键不在已知配置键白名单内", bad)
		}
	}
}
