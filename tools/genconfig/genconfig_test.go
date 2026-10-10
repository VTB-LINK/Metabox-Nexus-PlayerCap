package main

// 本文件只测 genconfig 生成的随包 config.yml：内容是内置模板，版本键取 -version 的盖章值。
// 两个打包流水线把各自的 VERSION 传进来：发版是 4.0.0 这样的版本号，开发构建是非 semver 的版本号。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
)

// 变异自证（隔离的 worktree，实测）：run 不把 -version 传给 DefaultConfigContent（恒传空串）→「发版」
// 「预发布」两例红。
func TestRunWritesTemplateWithStampedVersion(t *testing.T) {
	for _, c := range []struct {
		name    string
		version string
		want    string
	}{
		{"发版", "4.0.0", `config-version: "v4.0.0"`},
		{"预发布", "4.0.0-beta.3", `config-version: "v4.0.0-beta.3"`},
		{"CI 开发构建", "Metabox-Nexus-PlayerCap-20261009-153045-abc1234", `config-version: "v4.0.0-0"`},
		{"不传版本", "", `config-version: "v4.0.0-0"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			args := []string{path}
			if c.version != "" {
				args = []string{"-version", c.version, path}
			}
			out, err := run(args)
			if err != nil || out != path {
				t.Fatalf("run 返回 %q, %v，期望写到 %q", out, err, path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != config.DefaultConfigContent(c.version) {
				t.Errorf("写出的内容不是版本 %q 的模板", c.version)
			}
			if !strings.Contains(string(data), "\n"+c.want+"\n") {
				t.Errorf("写出的内容里没有 %s", c.want)
			}
		})
	}
}
