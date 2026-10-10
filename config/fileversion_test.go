package config

// 本文件只测 config.yml 版本键的读取（readFileVersion）与它同盖章值比较后的处置（planUpgrade）。
// 各例从 YAML 文本起步：带不带引号、写成数字还是文字，解析出的类型不同，判定跟着变。

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func parseYAML(t *testing.T, src string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("解析 %q 失败: %v", src, err)
	}
	return m
}

// 没有版本键按 v3.0.0；合法的版本号带不带 v、有无引号、大写 V、首尾空白都认，读出的是规范形式；
// 键在而不是合法版本号的一律判为非法、版本为空串。
//
// 变异自证（隔离的 worktree，实测）：没有版本键时返回空串或判为非法 →「没有版本键」各例红；不认大写 V、
// 不去首尾空白、不补 v → 对应各例红；非法版本键按 v3 处理（返回 legacyFileVersion 与 versionKeyAbsent）
// → 非法各例红；数字写法（4.0、4）先转成字符串再判 → 该两例红。
func TestReadFileVersion(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		want  string
		state versionKeyState
	}{
		{"没有版本键", "offset: 200\n", "v3.0.0", versionKeyAbsent},
		{"空文件", "", "v3.0.0", versionKeyAbsent},
		{"只有注释", "# config-version: \"v4.0.0\"\n", "v3.0.0", versionKeyAbsent},
		{"版本键在嵌套映射里不算", "extra:\n  config-version: \"v4.0.0\"\n", "v3.0.0", versionKeyAbsent},

		{"带 v、带引号", `config-version: "v4.0.0"`, "v4.0.0", versionKeyValid},
		{"不带 v、带引号", `config-version: "4.0.0"`, "v4.0.0", versionKeyValid},
		{"不带 v、不带引号", `config-version: 4.0.0`, "v4.0.0", versionKeyValid},
		{"带 v、不带引号", `config-version: v4.0.0`, "v4.0.0", versionKeyValid},
		{"单引号", `config-version: 'v4.0.0'`, "v4.0.0", versionKeyValid},
		{"大写 V", `config-version: "V4.0.0"`, "v4.0.0", versionKeyValid},
		{"首尾空白", `config-version: "  v4.0.0  "`, "v4.0.0", versionKeyValid},
		{"首尾空白加大写 V", `config-version: "  V4.0.0 "`, "v4.0.0", versionKeyValid},
		{"预发布", `config-version: "4.0.0-beta.3"`, "v4.0.0-beta.3", versionKeyValid},
		{"最低预发布", `config-version: "4.0.0-0"`, "v4.0.0-0", versionKeyValid},
		{"3.x 的版本号", `config-version: "3.3.4"`, "v3.3.4", versionKeyValid},
		{"0.0.0", `config-version: "0.0.0"`, "v0.0.0", versionKeyValid},
		// x/mod/semver 接受简写：带引号的 "4.0"、"4" 是字符串，按 v4.0.0 对待。
		{"两段简写、带引号", `config-version: "4.0"`, "v4.0.0", versionKeyValid},
		{"一段简写、带引号", `config-version: "4"`, "v4.0.0", versionKeyValid},
		{"带构建元数据", `config-version: "v4.0.0+build.5"`, "v4.0.0", versionKeyValid},

		// 不带引号的 4.0 解析为小数、4 解析为整数：不是字符串，非法。
		{"小数", `config-version: 4.0`, "", versionKeyInvalid},
		{"整数", `config-version: 4`, "", versionKeyInvalid},
		{"空串", `config-version: ""`, "", versionKeyInvalid},
		{"空值", `config-version:`, "", versionKeyInvalid},
		{"任意文字", `config-version: "abc"`, "", versionKeyInvalid},
		{"两个 v", `config-version: "vv4.0.0"`, "", versionKeyInvalid},
		{"四段", `config-version: "4.0.0.1"`, "", versionKeyInvalid},
		{"前导零", `config-version: "04.0.0"`, "", versionKeyInvalid},
		{"布尔", `config-version: true`, "", versionKeyInvalid},
		{"列表", `config-version: [4, 0, 0]`, "", versionKeyInvalid},
		{"映射", `config-version: {major: 4}`, "", versionKeyInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, state := readFileVersion(parseYAML(t, c.src))
			if got != c.want || state != c.state {
				t.Errorf("readFileVersion(%q) = %q, %d，期望 %q, %d", c.src, got, state, c.want, c.state)
			}
		})
	}
}

// 文件版本 S0 与盖章值 P 的关系定处置：低于则迁移并盖章，相等不动，高于不动（不把版本键改低），
// 版本键非法则只盖章、不迁移。比较按完整 semver：预发布低于同号正式版。
//
// 变异自证（隔离的 worktree，实测）：比较的 < 改成 <= →「相等」各例红；降级时也盖章（把版本键改低）→
// 「文件高于程序」各例红；非法版本键也迁移 →「版本键非法」各例红；非法版本键不盖章 → 同上各例红；
// 按字符串比较而不按 semver → 「预发布低于正式版」「10 大于 9」红。
func TestPlanUpgrade(t *testing.T) {
	cases := []struct {
		name                     string
		src                      string
		stamp                    string
		stampKey, migrate, newer bool
	}{
		{"没有版本键，发布版本", "offset: 200\n", "v4.0.0", true, true, false},
		{"没有版本键，开发构建", "offset: 200\n", latestMigrationBoundary, true, true, false},
		{"没有版本键，预发布版本", "offset: 200\n", "v4.0.0-beta.3", true, true, false},
		{"3.x 的版本号", `config-version: "3.3.4"`, "v4.0.0", true, true, false},
		{"低一个补丁版本", `config-version: "v4.0.0"`, "v4.0.1", true, true, false},
		{"预发布低于正式版", `config-version: "v4.0.0-rc.2"`, "v4.0.0", true, true, false},
		{"开发构建盖过的，换发布版本", `config-version: "v4.0.0-0"`, "v4.0.0", true, true, false},
		{"开发构建盖过的，换预发布版本", `config-version: "v4.0.0-0"`, "v4.0.0-alpha.1", true, true, false},
		{"10 大于 9", `config-version: "v4.9.0"`, "v4.10.0", true, true, false},

		{"相等", `config-version: "v4.0.0"`, "v4.0.0", false, false, false},
		{"相等，不带 v", `config-version: "4.0.0"`, "v4.0.0", false, false, false},
		{"相等，不带引号", `config-version: 4.0.0`, "v4.0.0", false, false, false},
		{"相等，简写", `config-version: "4.0"`, "v4.0.0", false, false, false},
		{"相等，开发构建", `config-version: "v4.0.0-0"`, latestMigrationBoundary, false, false, false},
		{"相等，只差构建元数据", `config-version: "v4.0.0+x"`, "v4.0.0", false, false, false},

		{"文件高于程序", `config-version: "v4.0.1"`, "v4.0.0", false, false, true},
		{"发布版本盖过的，换开发构建", `config-version: "v4.0.0"`, latestMigrationBoundary, false, false, true},
		{"正式版盖过的，换预发布版本", `config-version: "v4.0.0"`, "v4.0.0-rc.2", false, false, true},
		{"文件高一个大版本", `config-version: "v5.0.0"`, "v4.2.0", false, false, true},

		{"版本键非法：小数", `config-version: 4.0`, "v4.0.0", true, false, false},
		{"版本键非法：空值", `config-version:`, "v4.0.0", true, false, false},
		{"版本键非法：文字", `config-version: abc`, latestMigrationBoundary, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planUpgrade(parseYAML(t, c.src), c.stamp)
			if p.stampKey != c.stampKey || p.migrate != c.migrate || p.newer != c.newer {
				t.Errorf("盖章=%v 迁移=%v 文件较新=%v，期望 %v %v %v（文件版本 %q）",
					p.stampKey, p.migrate, p.newer, c.stampKey, c.migrate, c.newer, p.fileVersion)
			}
			if p.stamp != c.stamp {
				t.Errorf("stamp = %q，期望 %q", p.stamp, c.stamp)
			}
		})
	}
}
