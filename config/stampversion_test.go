package config

// 本文件只测盖章值（StampVersion）：程序写进 config.yml 版本键的值，以及模板里的版本键取它。

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// 发布版本得到版本号的规范形式（带 v、保留预发布后缀）；开发构建（本地的 0.0.0、空串、CI 的非 semver
// 版本号）得到最新一步迁移的边界，而不是 0.0.0。
//
// 变异自证（隔离的 worktree，实测）：开发构建的盖章值改成 v0.0.0 → 四例开发构建红；写入的版本键不带 v
// （发布版本原样返回）→「不带 v」各例红；去掉 0.0.0 的特判 →「本地构建」两例红；边界由 v4.0.0-0 改成
// v4.0.0 → 开发构建各例与 TestStampBoundaryIsLowestPrerelease 红。
func TestStampVersion(t *testing.T) {
	for _, c := range []struct{ name, version, want string }{
		{"正式版，不带 v", "4.0.0", "v4.0.0"},
		{"正式版，带 v", "v4.0.0", "v4.0.0"},
		{"预发布", "4.0.0-beta.3", "v4.0.0-beta.3"},
		{"补丁版本", "4.0.1", "v4.0.1"},
		{"首尾空白", " 4.0.0 ", "v4.0.0"},
		{"3.x 的发布版本", "3.3.4", "v3.3.4"},
		{"本地构建", "0.0.0", "v4.0.0-0"},
		{"本地构建，带 v", "v0.0.0", "v4.0.0-0"},
		{"空串", "", "v4.0.0-0"},
		{"CI 开发构建的非 semver 版本号", "Metabox-Nexus-PlayerCap-20261009-153045-abc1234", "v4.0.0-0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := StampVersion(c.version); got != c.want {
				t.Errorf("StampVersion(%q) = %q，期望 %q", c.version, got, c.want)
			}
		})
	}
}

// 边界是 4.0.0 的最低预发布：4.0.0 的任何预发布与正式版都不低于它，3.x 的任何版本都低于它。
func TestStampBoundaryIsLowestPrerelease(t *testing.T) {
	if !semver.IsValid(latestMigrationBoundary) {
		t.Fatalf("边界 %q 不是合法的 semver", latestMigrationBoundary)
	}
	for _, v := range []string{"v4.0.0-0", "v4.0.0-alpha.1", "v4.0.0-beta.3", "v4.0.0-rc.1", "v4.0.0", "v4.0.1"} {
		if semver.Compare(v, latestMigrationBoundary) < 0 {
			t.Errorf("%s 不应低于边界 %s", v, latestMigrationBoundary)
		}
	}
	for _, v := range []string{"v3.0.0", "v3.3.4", "v3.99.99", "v0.0.0"} {
		if semver.Compare(v, latestMigrationBoundary) >= 0 {
			t.Errorf("%s 应低于边界 %s", v, latestMigrationBoundary)
		}
	}
}

// 模板里有版本键、恰好一行，在文件头注释之后、addr 之前，取值是带双引号的盖章值；占位符不外泄。
// 用模板生成的文件读出的版本即盖章值。
//
// 变异自证（隔离的 worktree，实测）：模板里漏掉版本键 → 红（keyplaces_test.go 的 TestConfigKeyPlaces 同红）；
// 模板的版本键不加引号、不换占位符 → 红；DefaultConfigContent 不经 StampVersion（原样代入程序版本）→
// 开发构建与不带 v 的各例红。
func TestTemplateCarriesStampedVersionKey(t *testing.T) {
	for _, c := range []struct{ version, want string }{
		{"4.0.0", "v4.0.0"},
		{"v4.0.0", "v4.0.0"},
		{"4.0.0-beta.3", "v4.0.0-beta.3"},
		{"0.0.0", "v4.0.0-0"},
		{"", "v4.0.0-0"},
		{"dev-20261009-abc1234", "v4.0.0-0"},
	} {
		content := DefaultConfigContent(c.version)
		line := versionKey + `: "` + c.want + `"` + "\n"
		if n := strings.Count(content, versionKey+":"); n != 1 || !strings.Contains(content, "\n"+line) {
			t.Errorf("程序版本 %q：模板应恰有一行 %q，实际 %d 行版本键", c.version, strings.TrimSpace(line), n)
		}
		if strings.Contains(content, versionPlaceholder) {
			t.Errorf("程序版本 %q：模板里留着占位符 %s", c.version, versionPlaceholder)
		}
		at, addr := strings.Index(content, versionKey+":"), strings.Index(content, "\naddr:")
		if at < 0 || addr < 0 || at > addr {
			t.Errorf("程序版本 %q：版本键应在 addr 之前", c.version)
		}
		if got, state := readFileVersion(parseYAML(t, content)); got != c.want || state != versionKeyValid {
			t.Errorf("程序版本 %q：模板读出的版本 = %q, %d，期望 %q", c.version, got, state, c.want)
		}
	}
}

// 没有 config.yml 时按模板生成，生成出的文件带着盖章值；loadFile 记下的文件版本即盖章值。
func TestGeneratedFileCarriesStamp(t *testing.T) {
	for _, version := range []string{"4.0.0", "4.0.0-beta.3", "0.0.0"} {
		stamp := StampVersion(version)
		exeDir := t.TempDir()
		withPaths(t, filepath.Join(exeDir, "app.exe"), t.TempDir(), nil)
		cfg := DefaultConfig()
		captureLog(t, func() { loadFile(&cfg, version) })
		if cfg.ConfigFileState != FileGenerated || cfg.FileVersion != stamp {
			t.Errorf("盖章值 %s：加载结果=%d、文件版本=%q，期望 FileGenerated 与 %q", stamp, cfg.ConfigFileState, cfg.FileVersion, stamp)
		}
		if got := readTestFile(t, filepath.Join(exeDir, configName)); got != templateContent(stamp) {
			t.Errorf("盖章值 %s：生成的文件不是版本键为它的模板", stamp)
		}
	}
}
