package qqmusic

// 本文件只测同一首重开的信号（设计 §6.8「重开」）：本会话载入信号是否可用（loadingUsable）、载入值由无到有（loadEdge）与
// 按载入值写入重开的条件（reopenByLoading）。runSession 的接线由 assocwiring_test.go 守。

import (
	"fmt"
	"testing"
	"time"
)

// 载入信号可用：版本有载入取值、本会话未判标志不可信、实际轮询不超过 1 s。轮询恰 1 s 为可用（边界）。
// 第 5 轮收录的 A 类（22.31）、E 类（22.60）同样可用。反例：已判不可信；轮询 1 s 加 1 ns；20.05（没有载入取值）；21.81
// 与零值规格（未收录）。
// 变异自证（实测）：不看是否判不可信、轮询恰 1 s 判为不可用，各自红；21.81 也填上 22.16 的字段，「21.81 未收录」一例红。
func TestLoadingUsable(t *testing.T) {
	s2282, s2252, s2005 := knownVersions["22.82"].PlayState, knownVersions["22.52"].PlayState, knownVersions["20.05"].PlayState
	for _, c := range []struct {
		name string
		spec playStateSpec
		bad  bool
		poll time.Duration
		want bool
	}{
		{"22.82、未判不可信、轮询 50 ms", s2282, false, 50 * time.Millisecond, true},
		{"22.52 同样有载入取值", s2252, false, 50 * time.Millisecond, true},
		{"22.31（A 类）有载入取值", knownVersions["22.31"].PlayState, false, 50 * time.Millisecond, true},
		{"22.60（E 类）有载入取值", knownVersions["22.60"].PlayState, false, 50 * time.Millisecond, true},
		{"21.81 未收录", knownVersions["21.81"].PlayState, false, 50 * time.Millisecond, false},
		{"轮询恰 1 s", s2282, false, time.Second, true},
		{"已判不可信", s2282, true, 50 * time.Millisecond, false},
		{"轮询超过 1 s", s2282, false, time.Second + 1, false},
		{"20.05 没有载入取值", s2005, false, 50 * time.Millisecond, false},
		{"零值规格", playStateSpec{}, false, 50 * time.Millisecond, false},
	} {
		if got := c.spec.loadingUsable(c.bad, c.poll); got != c.want {
			t.Errorf("%s：loadingUsable = %v，want %v", c.name, got, c.want)
		}
	}
}

// loadEdge 按 classify 判每拍是否为载入值、返回由无到有。22.82：读数 3、2、2、3、2 在第 2、5 拍为真；读失败（rawOK 为假，
// 原始取值即使是 2）夹在两拍 2 之间，前后各算一次由无到有；取值集合以外的 9 不算载入，也隔开前后两段；会话第一拍就是载入
// 值也算由无到有。20.05 下 2 不在任何集合里，从不为真。22.31（A 类）的 3 与 2 同属载入，连在一起只算一次由无到有；22.60
// （E 类）的 3 是在播值。
// 变异自证（实测）：读失败不当作非载入（只看 raw），「读失败夹在两拍 2 之间」一例第 3 拍不为真而红；不经 classify、写死
// raw == 2，20.05 与零值规格两例为真而红；返回本拍是否为载入值而不是由无到有，「3、2、2、3、2」第 3 拍为真而红；22.31 的
// 载入漏掉 3、22.60 照 A 类填（载入 2 与 3），各自那一例红。
func TestLoadEdge(t *testing.T) {
	type rd struct {
		raw uint32
		ok  bool
	}
	r := func(raws ...uint32) []rd {
		var out []rd
		for _, x := range raws {
			out = append(out, rd{x, true})
		}
		return out
	}
	for _, c := range []struct {
		name  string
		spec  playStateSpec
		reads []rd
		want  []bool
	}{
		{"3、2、2、3、2", knownVersions["22.82"].PlayState, r(3, 2, 2, 3, 2), []bool{false, true, false, false, true}},
		{"读失败夹在两拍 2 之间", knownVersions["22.82"].PlayState, []rd{{2, true}, {2, false}, {2, true}}, []bool{true, false, true}},
		{"取值集合以外的 9", knownVersions["22.82"].PlayState, r(3, 9, 9, 2, 9, 2), []bool{false, false, false, true, false, true}},
		{"暂停值 4 不是载入", knownVersions["22.82"].PlayState, r(4, 4, 3), []bool{false, false, false}},
		{"20.05", knownVersions["20.05"].PlayState, r(0, 2, 1, 2, 3, 2), []bool{false, false, false, false, false, false}},
		// A 类：2 接在 3 后面同属载入，不算由无到有；0 是暂停值，不是载入。
		{"22.31：1、3、2、1、3、0", knownVersions["22.31"].PlayState, r(1, 3, 2, 1, 3, 0), []bool{false, true, false, false, true, false}},
		// E 类：3 是在播值、4 是暂停值，都不是载入。
		{"22.60：3、2、2、3、4、2", knownVersions["22.60"].PlayState, r(3, 2, 2, 3, 4, 2), []bool{false, true, false, false, false, true}},
		{"零值规格", playStateSpec{}, r(2, 3, 2), []bool{false, false, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var e loadEdge
			var got []bool
			t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local)
			for i, x := range c.reads {
				got = append(got, e.step(c.spec, x.raw, x.ok, t0.Add(time.Duration(i)*50*time.Millisecond)))
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("各拍 %v，want %v", got, c.want)
			}
		})
	}
}

// 按载入值写入重开：可用、由无到有、已认领、本拍歌名等于已认领的、编号已知，全真为真。反例逐项：不可用；不是由无到有；
// 歌名不同（换歌那一拍；等进度或等 songMid 的拍，lastName 仍是上一首）；还没认领过（lastName 为空，含本拍歌名也为空）；
// 歌名为占位串 "QQ音乐"；编号缺位（本地文件不读记录）。
// 变异自证（实测）：不比歌名、不看 lastName 非空，各自红（assocwiring_test.go 的门禁同时报出）。
func TestReopenByLoading(t *testing.T) {
	for _, c := range []struct {
		name         string
		usable, edge bool
		song, last   string // 本拍读到的歌名、已认领的歌名
		hasIDs, want bool
	}{
		{"全真", true, true, "百", "百", true, true},
		{"载入信号不可用", false, true, "百", "百", true, false},
		{"不是由无到有", true, false, "百", "百", true, false},
		{"换歌那一拍", true, true, "千", "百", true, false},
		{"等进度或等 songMid 的拍", true, true, "千", "百", true, false},
		{"还没认领过", true, true, "百", "", true, false},
		{"还没认领过、歌名为空", true, true, "", "", true, false},
		{"歌名为占位串", true, true, "QQ音乐", "百", true, false},
		{"编号缺位", true, true, "百", "百", false, false},
	} {
		if got := reopenByLoading(c.usable, c.edge, c.song, c.last, c.hasIDs); got != c.want {
			t.Errorf("%s：reopenByLoading = %v，want %v", c.name, got, c.want)
		}
	}
}
