package config

// 本文件只测强制重建的判定（rebuildRequired 与 Config.ShouldRebuild）：改写之前的文件版本 S0、网关字段
// F、程序版本 V 与已处理的门槛之间的关系。重建的动作见 rebuildfile_test.go。

import "testing"

// 门槛判定：S0 < F 且 F <= V 才重建。设计 §5.3 的各行与边界。
//
// 变异自证（隔离的 worktree，实测）：去掉 F <= V →「F 高于 V」各例与预发布阶段各例红；S0 < F 改成 <= →
// 「S0 等于 F」等七例红；F 不合法时按 V 判 →「F 缺失」等四例红；V 不是发布版本时也判 →「开发构建、
// S0 与 F 都不高于 0.0.0」红（其余开发构建各例不红：F 高于 0.0.0 时 F <= V 本就不成立）；S0 未知时
// 按 v3 处理 →「S0 未知」「S0 不是版本号」红；忽略已重建标记 →「同一门槛来两次」等四例红；带标记即
// 一律不判 →「标记低于门槛：先 4.0.0 后 4.0.1」「标记不是版本号」红；标记的比较 <= 改成 < →「同一门槛
// 来两次」「标记等于门槛，写法不同」红。
func TestRebuildRequired(t *testing.T) {
	cases := []struct {
		name              string
		s0, f, v, handled string
		want              bool
	}{
		// §5.3 的表：F = 4.0.0。
		{"3.x 升 4.0.0", "v3.0.0", "v4.0.0", "4.0.0", "", true},
		{"之后再升 4.0.1：S0 = 4.0.0", "v4.0.0", "v4.0.0", "4.0.1", "", false},
		{"3.x 跳过 4.0.0 直接到 4.0.1", "v3.0.0", "v4.0.0", "4.0.1", "", true},
		{"把 F 改成 4.0.1，首次以 4.0.1 启动", "v4.0.0", "v4.0.1", "4.0.1", "", true},
		{"改字段之前已跑过 4.0.1", "v4.0.1", "v4.0.1", "4.0.1", "", false},
		{"同一版本反复启动：重建后 S0 = V", "v4.0.0", "v4.0.0", "4.0.0", "", false},
		{"版本已发布、跑过之后才设 F = V", "v4.2.0", "v4.2.0", "4.2.0", "", false},
		{"3.x 的版本键", "v3.3.4", "v4.0.0", "4.0.0", "", true},

		// 三个边界。
		{"S0 等于 F", "v4.0.0", "v4.0.0", "4.1.0", "", false},
		{"S0 高于 F", "v4.0.5", "v4.0.0", "4.1.0", "", false},
		{"F 等于 V", "v3.0.0", "v4.1.0", "4.1.0", "", true},
		{"F 高于 V", "v3.0.0", "v4.1.0", "4.0.9", "", false},
		{"F 高于 V 一个预发布", "v3.0.0", "v4.0.0", "4.0.0-rc.9", "", false},

		// 线上现值：程序 3.3.4、字段 3.7.0。
		{"线上现值", "v3.0.0", "v3.7.0", "3.3.4", "", false},
		{"示例值留到 4.0 发布", "v3.0.0", "v3.7.0", "4.0.0", "", true},

		// F 的写法。
		{"F 缺失", "v3.0.0", "", "4.0.0", "", false},
		{"F 是空白", "v3.0.0", "   ", "4.0.0", "", false},
		{"F 不是版本号", "v3.0.0", "abc", "4.0.0", "", false},
		{"F 是 true", "v3.0.0", "true", "4.0.0", "", false},
		{"F 不带 v", "v3.0.0", "4.0.0", "4.0.0", "", true},
		{"F 带首尾空白", "v3.0.0", " v4.0.0 ", "4.0.0", "", true},
		{"F 带预发布", "v3.0.0", "v4.0.0-alpha.1", "4.0.0-alpha.1", "", true},

		// S0 未知、V 不是发布版本。
		{"S0 未知", "", "v4.0.0", "4.0.0", "", false},
		{"S0 不是版本号", "unknown", "v4.0.0", "4.0.0", "", false},
		{"开发构建 0.0.0", "v3.0.0", "v4.0.0", "0.0.0", "", false},
		{"开发构建的非 semver 版本号", "v3.0.0", "v4.0.0", "dev-20261009-abc1234", "", false},
		{"开发构建、F 是 0.0.0", "v3.0.0", "v0.0.0", "0.0.0", "", false},
		{"V 为空", "v3.0.0", "v4.0.0", "", "", false},
		{"开发构建、S0 与 F 都不高于 0.0.0", "v0.0.0-0", "v0.0.0", "0.0.0", "", false},

		// 预发布阶段：F = 4.0.0 时预发布版本的程序满足不了 F <= V。
		{"预发布 alpha", "v3.0.0", "v4.0.0", "4.0.0-alpha.1", "", false},
		{"预发布 beta", "v3.0.0", "v4.0.0", "4.0.0-beta.3", "", false},
		{"预发布 rc", "v3.0.0", "v4.0.0", "4.0.0-rc.2", "", false},
		{"正式版读到 v3", "v3.0.0", "v4.0.0", "4.0.0", "", true},
		{"正式版读到 rc 盖过的", "v4.0.0-rc.2", "v4.0.0", "4.0.0", "", true},
		{"正式版读到开发构建盖过的", "v4.0.0-0", "v4.0.0", "4.0.0", "", true},
		{"F 取首个预发布：当版成立", "v3.0.0", "v4.0.0-alpha.1", "4.0.0-alpha.1", "", true},
		{"F 取首个预发布：正式版时已重建过的不再重建", "v4.0.0-alpha.1", "v4.0.0-alpha.1", "4.0.0", "", false},
		{"F 取首个预发布：正式版时 v3 照样重建", "v3.0.0", "v4.0.0-alpha.1", "4.0.0", "", true},

		// 已重建标记：门槛不高于标记即跳过，更高照常判定。
		{"同一门槛来两次", "v3.0.0", "v4.0.0", "4.0.0", "v4.0.0", false},
		{"同一门槛来两次，文件版本被人为改低", "v1.0.0", "v4.0.0", "4.0.1", "v4.0.0", false},
		{"标记等于门槛，写法不同", "v3.0.0", "4.0.0", "4.0.0", " v4.0.0 ", false},
		{"标记高于门槛", "v3.0.0", "v4.0.0", "4.0.1", "v4.0.1", false},
		{"标记低于门槛：先 4.0.0 后 4.0.1", "v4.0.0", "v4.0.1", "4.0.1", "v4.0.0", true},
		{"标记低于门槛，但 S0 不低于门槛", "v4.0.1", "v4.0.1", "4.0.1", "v4.0.0", false},
		{"标记低于门槛，但 F 高于 V", "v4.0.0", "v4.0.2", "4.0.1", "v4.0.0", false},
		{"标记不是版本号：当作没有", "v3.0.0", "v4.0.0", "4.0.0", "yes", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rebuildRequired(c.s0, c.f, c.v, c.handled); got != c.want {
				t.Errorf("rebuildRequired(S0=%q, F=%q, V=%q, 已处理=%q) = %v，期望 %v", c.s0, c.f, c.v, c.handled, got, c.want)
			}
		})
	}
}

// ShouldRebuild 用的是 loadFile 记下的改写前版本，且只在文件本次正常加载时判。
//
// v3 的文件经 loadFile 之后，磁盘上的版本键已是盖章值，而 Config 记下的改写前版本仍是 3.0.0，以它判定
// 须成立；拿磁盘上的版本去判则不成立（钉住「判定不得重读文件」）。同一文件先后由 4.0.0、4.0.1 加载，
// 第二次记下的是 4.0.0：进程若盖了版本号而不判定，原先的版本信息即丢失。
//
// 变异自证（隔离的 worktree，实测）：改写之后才记文件版本（FileVersion 取盖章值）→ 红；ShouldRebuild
// 不看加载结果 →「本次新生成」「解析失败」红。
func TestShouldRebuildUsesVersionBeforeRewrite(t *testing.T) {
	s := newSandbox(t, v3Config)
	cfg, _ := s.load("4.0.0", testOps())
	onDisk, _ := readFileVersion(parseYAML(t, s.read()))
	if onDisk != "v4.0.0" || cfg.FileVersion != "v3.0.0" {
		t.Fatalf("前置：磁盘上的版本应已是 v4.0.0、记下的改写前版本应是 v3.0.0，实际 %q、%q", onDisk, cfg.FileVersion)
	}
	if !cfg.ShouldRebuild("v4.0.0", "4.0.0", "") {
		t.Error("以改写前的版本 3.0.0 判定应成立")
	}
	if rebuildRequired(onDisk, "v4.0.0", "4.0.0", "") {
		t.Error("以磁盘上的版本判定不成立：判定若重读文件就不会重建")
	}

	next, _ := s.load("4.0.1", testOps())
	if next.FileVersion != "v4.0.0" || next.ShouldRebuild("v4.0.0", "4.0.1", "") {
		t.Errorf("第二次由 4.0.1 加载：改写前版本 = %q，对门槛 4.0.0 不应再成立", next.FileVersion)
	}
	if !next.ShouldRebuild("v4.0.1", "4.0.1", "") {
		t.Error("门槛升到 4.0.1 时应成立")
	}

	t.Run("本次新生成", func(t *testing.T) {
		s := newSandbox(t, "")
		cfg, _ := s.load("4.0.0", testOps())
		cfg.FileVersion = "v3.0.0" // 即便版本看上去低于门槛
		if cfg.ConfigFileState != FileGenerated || cfg.ShouldRebuild("v4.0.0", "4.0.0", "") {
			t.Errorf("本次新生成的文件不判，加载结果=%d", cfg.ConfigFileState)
		}
	})
	t.Run("解析失败", func(t *testing.T) {
		s := newSandbox(t, "poll: [1, 2\n")
		cfg, _ := s.load("4.0.0", testOps())
		cfg.FileVersion = "v3.0.0"
		if cfg.ConfigFileState != FileParseFailed || cfg.ShouldRebuild("v4.0.0", "4.0.0", "") {
			t.Errorf("解析失败的文件不判，加载结果=%d", cfg.ConfigFileState)
		}
	})
	t.Run("版本键非法", func(t *testing.T) {
		s := newSandbox(t, "config-version: 4.0\noffset: -200\n")
		cfg, _ := s.load("4.0.1", testOps())
		if cfg.FileVersion != "" || cfg.ShouldRebuild("v4.0.1", "4.0.1", "") {
			t.Errorf("版本键非法时版本未知，不判；记下的版本 = %q", cfg.FileVersion)
		}
	})
}
