package config

// 本文件只测自动迁移的步骤表与值层（stepsAfter、migrateValues、negateSaturating）：哪些版本的文件要迁、
// 哪些键的哪些取值被换算。文本层见 migratetext_test.go，写回文件见 upgradefile_test.go。

import (
	"math"
	"reflect"
	"testing"

	"golang.org/x/mod/semver"
)

// 步骤表按边界升序，边界都是合法的 semver，最后一步的边界即开发构建的盖章值。
//
// 变异自证（隔离的 worktree，实测）：步骤表的边界由 v4.0.0-0 改成 v4.0.0（latestMigrationBoundary 不动）→ 红。
func TestMigrationStepsAreOrdered(t *testing.T) {
	if len(migrationSteps) == 0 {
		t.Fatal("步骤表为空")
	}
	for i, s := range migrationSteps {
		if !semver.IsValid(s.boundary) {
			t.Errorf("第 %d 步的边界 %q 不是合法的 semver", i, s.boundary)
		}
		if i > 0 && semver.Compare(migrationSteps[i-1].boundary, s.boundary) >= 0 {
			t.Errorf("第 %d 步的边界 %s 不高于上一步的 %s", i, s.boundary, migrationSteps[i-1].boundary)
		}
	}
	if last := migrationSteps[len(migrationSteps)-1].boundary; last != latestMigrationBoundary {
		t.Errorf("最后一步的边界 %s 应等于 latestMigrationBoundary %s", last, latestMigrationBoundary)
	}
}

// 文件版本低于边界才迁：3.x 的任何版本都迁；4.0.0 的预发布、正式版与开发构建盖过的 4.0.0-0 都不再迁。
//
// 变异自证（隔离的 worktree，实测）：stepsAfter 的 < 改成 <= →「恰为边界」红；步骤表的边界改成 v4.0.0 →
// 「4.0.0 的预发布」各例红（由 4.0.0-beta.3 盖过的文件会再迁一遍）。
func TestStepsAfter(t *testing.T) {
	for _, c := range []struct {
		version string
		want    int
	}{
		{"v3.0.0", 1}, {"v3.3.4", 1}, {"v3.99.0", 1}, {"v0.0.0", 1}, {"v4.0.0-0", 0},
		{"v4.0.0-alpha.1", 0}, {"v4.0.0-beta.3", 0}, {"v4.0.0-rc.2", 0}, {"v4.0.0", 0}, {"v4.0.1", 0}, {"v5.0.0", 0},
	} {
		if got := len(stepsAfter(c.version)); got != c.want {
			t.Errorf("stepsAfter(%s) 给出 %d 步，期望 %d", c.version, got, c.want)
		}
	}
}

// 值层只动解析为 int 的 offset 与已注册播放器的 <player>-offset；其余类型、其余键、未注册播放器的键不动。
//
// 变异自证（隔离的 worktree，实测）：对字符串或小数取反（不判类型，按字面换算）→「字符串」「小数」红；
// int 最小值按普通取反处理 →「最小值」红；int 最小值按类型不符拒收（不换算）→「最小值」红；换算未注册
// 播放器的键 →「未注册播放器」红；只换算全局 offset →「播放器的键」红；在原 map 上改 →「不改动入参」红。
func TestMigrateValuesNegatesIntegerOffsets(t *testing.T) {
	players := []string{"kugou", "qqmusic", "kugou"} // 重复登记的名字只换算一次
	steps := stepsAfter(legacyFileVersion)
	cases := []struct {
		name string
		src  string
		want map[string]int // 被改动的键及其新值
	}{
		{"正数", "offset: 200", map[string]int{"offset": -200}},
		{"带正号", "offset: +200", map[string]int{"offset": -200}},
		{"负数", "offset: -300", map[string]int{"offset": 300}},
		{"零", "offset: 0", nil},
		{"负零", "offset: -0", nil},
		{"十六进制", "offset: 0x10", map[string]int{"offset": -16}},
		{"下划线分隔", "offset: 1_000", map[string]int{"offset": -1000}},
		{"字符串", `offset: "200"`, nil},
		{"单引号字符串", `offset: '200'`, nil},
		{"小数", "offset: 200.0", nil},
		{"空值", "offset:", nil},
		{"布尔", "offset: true", nil},
		{"列表", "offset: [200]", nil},
		{"最小值", "offset: -9223372036854775808", map[string]int{"offset": math.MaxInt}},
		{"最大值", "offset: 9223372036854775807", map[string]int{"offset": -math.MaxInt}},
		{"超过 int64 上限", "offset: 9223372036854775808", nil},
		{"超过上限的不妨碍别的键", "offset: 9223372036854775808\nkugou-offset: 430", map[string]int{"kugou-offset": -430}},
		{"播放器的键", "kugou-offset: 430\nqqmusic-offset: -50", map[string]int{"kugou-offset": -430, "qqmusic-offset": 50}},
		{"重复登记的播放器只换算一次", "kugou-offset: 430", map[string]int{"kugou-offset": -430}},
		{"未注册播放器", "foo-offset: 200\nwesing-offset: 5", nil},
		{"别的键", "poll: 30\naudio-normalize-target: -14\nprior-player-expire: 15", nil},
		{"嵌套映射里的 offset", "extra:\n  offset: 200", nil},
		{"没有 offset", "addr: x", nil},
		{"空文件", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := parseYAML(t, c.src)
			before := parseYAML(t, c.src)
			next, changed := migrateValues(m, steps, players)
			if !reflect.DeepEqual(m, before) {
				t.Errorf("不改动入参：入参被改成 %v", m)
			}
			got := map[string]int{}
			for key, ch := range changed {
				got[key] = ch.new
				if old, _ := before[key].(int); ch.old != old {
					t.Errorf("%s 记下的原值 = %d，期望 %d", key, ch.old, old)
				}
			}
			if len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
				t.Errorf("被改动的键 = %v，期望 %v", got, c.want)
			}
			// 副本里：被改动的键是新值，其余与入参相同。
			for key, v := range before {
				want := v
				if n, ok := c.want[key]; ok {
					want = n
				}
				if !reflect.DeepEqual(next[key], want) {
					t.Errorf("副本的 %s = %v，期望 %v", key, next[key], want)
				}
			}
			if len(next) != len(before) {
				t.Errorf("副本的键数 %d 与入参的 %d 不同", len(next), len(before))
			}
		})
	}
}

// 没有适用的步骤时一个键都不动：4.0 及以后的文件里的 offset 已是新语义。
func TestMigrateValuesWithoutSteps(t *testing.T) {
	m := parseYAML(t, "offset: 200\nkugou-offset: 430\n")
	next, changed := migrateValues(m, stepsAfter("v4.0.0"), []string{"kugou"})
	if len(changed) != 0 || !reflect.DeepEqual(next, m) {
		t.Errorf("没有步骤时不应改动：%v，改动 %v", next, changed)
	}
}

// 饱和取反：最小值得最大值，其余即相反数；对结果再取反不回到最小值（差 1），但每次启动的结果一致。
func TestNegateSaturating(t *testing.T) {
	for _, c := range [][2]int{{0, 0}, {1, -1}, {-1, 1}, {200, -200}, {math.MaxInt, -math.MaxInt}, {math.MinInt, math.MaxInt}, {math.MinInt + 1, math.MaxInt}} {
		if got := negateSaturating(c[0]); got != c[1] {
			t.Errorf("negateSaturating(%d) = %d，期望 %d", c[0], got, c[1])
		}
	}
}
