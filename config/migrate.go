package config

import (
	"math"
	"slices"

	"golang.org/x/mod/semver"
)

// config.yml 的自动迁移（设计 doc/design/config-version-and-offset-delay-design.md §4）。
//
// 迁移步骤是一张按边界升序排列的表。文件版本低于某一步的边界即执行该步，按序执行；迁移与改版本键在
// 同一次写入里完成，所以不会迁两遍。以后新增语义变更就在表尾加一项并把 latestMigrationBoundary 改成
// 它的边界，不改已有项。
//
// 一步迁移只做一件事：把若干整数键从旧语义换算成新语义。值层（migrateValues）只动解析为 int 的值，
// 与 mergeYAML 采纳的范围相同；文本层（rewriteMigrated）不自行判断哪些键该改，只把值层改过的键写回
// 对应的行。命令行传入的值不经文件，不迁移。

// migrationStep 是一次配置语义变更的迁移。
type migrationStep struct {
	// boundary 是首个含该变更的版本；拿不准时取该版本号的最低预发布 X.Y.Z-0。
	boundary string
	// intKeys 返回本步要换算的整数键，players 是已注册的播放器名。
	intKeys func(players []string) []string
	// convert 把旧语义下的取值换算成新语义下的取值。
	convert func(v int) int
	// phrases 是模板历史上说明旧语义的注释短语及其替换，依次替换，长的在前。
	phrases [][2]string
	// note 是迁移时随版本键一起写进文件的一行注释（文件里原先没有版本键时）。
	note string
}

var migrationSteps = []migrationStep{
	{
		// issue #54：offset 由提前量改为延迟量，取值取反。4.0.0 的任何预发布都已是新语义。
		boundary: "v4.0.0-0",
		intKeys:  offsetKeys,
		convert:  negateSaturating,
		// 对 config/config.go、main.go、config.yml 的全部历史检索，模板里说明方向的句子只有这两种。
		phrases: [][2]string{
			{"正值=歌词提前，负值=延后", "正值=延后，负值=提前"},
			{"正值=歌词提前", "正值=延后"},
		},
		note: "自 4.0 起 offset 表示延迟（正值=延后，负值=提前）；本文件的 offset 已在迁移时取反",
	},
}

// offsetKeys 返回全部与时间有关的 offset 键：全局的 offset 与各已注册播放器的 <player>-offset。
//
// 同一个名字只列一次：一个键在一步里只能换算一次，重复登记的播放器名不得让它被取反两遍。
func offsetKeys(players []string) []string {
	keys := []string{"offset"}
	for _, p := range players {
		if key := p + "-offset"; !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// negateSaturating 返回 v 的相反数；int 的最小值取反会溢出回它自己，改为返回最大值。
//
// 旧语义下的最小值是「延后到极限」，新语义下的最大值含义相同（只差 1 毫秒）。普通取反对它是错的：
// 值不变而版本已盖，等于把「延后到极限」静默变成「提前到极限」。
func negateSaturating(v int) int {
	if v == math.MinInt {
		return math.MaxInt
	}
	return -v
}

// stepsAfter 返回边界高于 version 的各步迁移，保持表里的顺序。
func stepsAfter(version string) []migrationStep {
	var steps []migrationStep
	for _, s := range migrationSteps {
		if semver.Compare(version, s.boundary) < 0 {
			steps = append(steps, s)
		}
	}
	return steps
}

// intChange 是值层对一个整数键的改动。
type intChange struct{ old, new int }

// migrateValues 在 m 的副本上依次执行 steps，返回副本与被改动的键（取值没变的不在其中）。m 本身不动。
// 只换算解析为 int 的值：写成字符串、小数、空值的键本就被 mergeYAML 拒收，超过 int64 上限的十进制数
// 解析为 uint64，同样不是 int。
func migrateValues(m map[string]interface{}, steps []migrationStep, players []string) (map[string]interface{}, map[string]intChange) {
	out := make(map[string]interface{}, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	changed := map[string]intChange{}
	for _, s := range steps {
		for _, key := range s.intKeys(players) {
			v, ok := out[key].(int)
			if !ok {
				continue
			}
			c, seen := changed[key]
			if !seen {
				c.old = v
			}
			c.new = s.convert(v)
			out[key], changed[key] = c.new, c
		}
	}
	for key, c := range changed {
		if c.old == c.new {
			delete(changed, key)
		}
	}
	return out, changed
}
