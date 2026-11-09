package lyric

// 本文件测按窗口歌名查歌曲信息记录（#146，设计 §3.1、§3.2、§4「兜底模式的歌曲信息」）：FindSongInfo（PCM 模式的
// 交叉核对）与 FindSongInfoPreferMID（兜底模式）。假内存与扫描沿用 roma_test.go 的 fakeMem、bymid_test.go 的
// fakeScan；记录的形状取自真机只读探针（2026-11-04、11-08）。

import (
	"strings"
	"testing"
)

const siShiversMID = "004DfBLx0IvNDq"

// noSingerRecord 是 11-08 二测所见、不带歌手的 StartKSong 记录（Shivers）。
func noSingerRecord(mid, name string) string {
	return `{"cmd":"KSong","subcmd":{"value":"StartKSong","data":{"mid":"` + mid + `","duration":"","songname":"` + name +
		`","lSongMask":"undefined","iHasCp":"undefined"}}}}`
}

// truncatedRecord 是前半段被覆盖、没有 mid 的残缺副本（11-08 二测所见）；singer 非空时带上 singername。
func truncatedRecord(name, singer string) string {
	s := "fromtag=0\x00" + `tion":"","songname":"` + name + `"`
	if singer != "" {
		s += `,"singername":"` + singer + `"`
	}
	return s + `,"lSongMask":"undefined","iHasCp":"undefined"}}}}`
}

// 两种选取的各种情形。recs 按地址从低到高摆放；pcm、prefer 是 FindSongInfo、FindSongInfoPreferMID 的结果，零值即
// 返回错误。
//
// 变异自证（实测）：
//   - 歌手照旧在读到的 2 KB 里找（不看 }）：「记录不带歌手、后面别的对象有」「singername 在 } 之后」取到了别的对象
//     的歌手而红；
//   - 找不到 singername 仍整条作废（#146 之前）：「记录不带歌手，2 KB 内没有别的 singername」找不到、R3 的 PCM 一侧
//     跳过残缺副本取到较低的那条而红（「后面别的对象有」那例 2 KB 内有键，不受这条影响，由上一条守）；
//   - 歌手为空整条作废（singerEndIdx <= 0）：「singername 为空」找不到而红；
//   - 歌手上限改成 99 字节：「歌手恰 100 字节」红；去掉上限：「歌手 101 字节」被认下而红；
//   - 找不到结尾引号时歌手为空、记录仍有效：「singername 没有结尾引号」被认下而红；
//   - } 从歌名开头找（不跳过歌名）：「歌名里有 }」歌手为空而红；
//   - 匹配不到时退回地址最高的有效记录（#146 之前）：「R1」「窗口歌名为空」返回了上一首而红；
//   - 不论 preferMID 都取地址最高的：R3 与「兜底取有 mid 的里地址最高的」的兜底一侧红；
//   - 不论 preferMID 都优先有 mid 的：R3 与「兜底取有 mid 的里地址最高的」的 PCM 一侧红（兜底歌手设计审 R3）；
//   - 从地址最低的往上看：「同名多份副本取地址最高」「都没有 mid」「兜底取有 mid 的里地址最高的」两侧、R3 的 PCM
//     一侧取到旧的而红；
//   - 每处向后读 1024 字节：有命中的各例读取量的断言都红；
//   - 歌名不按 JSON 解码（以第一个引号为结尾）：「歌名带转义引号」「歌名转义不合法」红；歌手同样不解码：「歌手带
//     转义引号」「歌手转义不合法」红（#146 代码审）。
func TestFindSongInfo(t *testing.T) {
	cur := SongInfo{Mid: bmMID, Name: "漠河舞厅", Singer: "柳爽"}
	none := SongInfo{}
	cases := []struct {
		name, window string
		recs         []string
		pcm, prefer  SongInfo
	}{
		{"StartKSong 记录", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", "柳爽")}, cur, cur},
		{"记录不带歌手、后面别的对象有（Shivers 读成了林俊杰）", "Shivers",
			[]string{noSingerRecord(siShiversMID, "Shivers") + `{"cmd":"KSong","data":{"singername":"林俊杰"}}`},
			SongInfo{Mid: siShiversMID, Name: "Shivers"}, SongInfo{Mid: siShiversMID, Name: "Shivers"}},
		{"记录不带歌手，2 KB 内没有别的 singername", "Shivers", []string{noSingerRecord(siShiversMID, "Shivers")},
			SongInfo{Mid: siShiversMID, Name: "Shivers"}, SongInfo{Mid: siShiversMID, Name: "Shivers"}},
		{"singername 在 } 之后", "漠河舞厅",
			[]string{`{"mid":"` + bmMID + `","songname":"漠河舞厅"},{"singername":"柳爽"}`},
			SongInfo{Mid: bmMID, Name: "漠河舞厅"}, SongInfo{Mid: bmMID, Name: "漠河舞厅"}},
		{"singername 为空", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", "")},
			SongInfo{Mid: bmMID, Name: "漠河舞厅"}, SongInfo{Mid: bmMID, Name: "漠河舞厅"}},
		{"歌手恰 100 字节", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", strings.Repeat("a", 100))},
			SongInfo{Mid: bmMID, Name: "漠河舞厅", Singer: strings.Repeat("a", 100)},
			SongInfo{Mid: bmMID, Name: "漠河舞厅", Singer: strings.Repeat("a", 100)}},
		{"歌手 101 字节", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", strings.Repeat("a", 101))}, none, none},
		{"singername 没有结尾引号", "漠河舞厅", []string{`{"mid":"` + bmMID + `","songname":"漠河舞厅","singername":"柳爽`}, none, none},
		{"歌名里有 }", "A}B", []string{startKSong(bmMID, "A}B", "柳爽")},
			SongInfo{Mid: bmMID, Name: "A}B", Singer: "柳爽"}, SongInfo{Mid: bmMID, Name: "A}B", Singer: "柳爽"}},
		{"R1：唯一的记录是上一首，匹配不到窗口歌名", "漠河舞厅",
			[]string{startKSong(bmPrevMID, "这世界那么多人", "莫文蔚")}, none, none},
		{"窗口歌名为空", "", []string{startKSong(bmMID, "漠河舞厅", "柳爽")}, none, none},
		{"上一首地址更高，取匹配上的这首", "漠河舞厅",
			[]string{startKSong(bmMID, "漠河舞厅", "柳爽"), startKSong(bmPrevMID, "这世界那么多人", "莫文蔚")}, cur, cur},
		{"同名多份副本取地址最高", "漠河舞厅",
			[]string{startKSong(bmMID, "漠河舞厅", "旧副本"), startKSong(bmMID, "漠河舞厅", "柳爽")}, cur, cur},
		{"R3：同名的较低地址有 mid，较高地址的残缺副本没有", "漠河舞厅",
			[]string{startKSong(bmMID, "漠河舞厅", "柳爽"), truncatedRecord("漠河舞厅", "")},
			SongInfo{Name: "漠河舞厅"}, cur},
		{"都没有 mid：两者都取地址最高", "漠河舞厅",
			[]string{truncatedRecord("漠河舞厅", "甲"), truncatedRecord("漠河舞厅", "乙")},
			SongInfo{Name: "漠河舞厅", Singer: "乙"}, SongInfo{Name: "漠河舞厅", Singer: "乙"}},
		{"兜底取有 mid 的里地址最高的", "漠河舞厅",
			[]string{startKSong(bmPrevMID, "漠河舞厅", "更旧"), startKSong(bmMID, "漠河舞厅", "柳爽"), truncatedRecord("漠河舞厅", "残缺")},
			SongInfo{Name: "漠河舞厅", Singer: "残缺"}, cur},
		{"没有 songname", "漠河舞厅", []string{`{"mid":"` + bmMID + `","singername":"柳爽"}`}, none, none},
		{"歌名带转义引号", `Song "Live"`, []string{startKSong(bmMID, `Song \"Live\"`, "柳爽")},
			SongInfo{Mid: bmMID, Name: `Song "Live"`, Singer: "柳爽"}, SongInfo{Mid: bmMID, Name: `Song "Live"`, Singer: "柳爽"}},
		{"歌手带转义引号", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", `柳\"爽`)},
			SongInfo{Mid: bmMID, Name: "漠河舞厅", Singer: `柳"爽`}, SongInfo{Mid: bmMID, Name: "漠河舞厅", Singer: `柳"爽`}},
		{"歌名转义不合法", `A\qB`, []string{startKSong(bmMID, `A\qB`, "柳爽")}, none, none},
		{"歌手转义不合法", "漠河舞厅", []string{startKSong(bmMID, "漠河舞厅", `柳\q爽`)}, none, none},
	}
	for _, c := range cases {
		for _, preferMID := range []bool{false, true} {
			f := newFakeMem()
			for i, s := range c.recs {
				base := 0x02000000 + uint32(i)*0x1000
				f.zeros(base, 0x100) // 向前读的 200 字节读得出
				f.utf16At(base+0x100, s, 0x900)
			}
			r := &readLog{fakeMem: f}
			scans := 0
			got, err := findSongInfo(r, fakeScan(f, &scans), c.window, preferMID)
			want := c.pcm
			if preferMID {
				want = c.prefer
			}
			if got != want || (err == nil) != (want != none) {
				t.Errorf("%s（preferMID=%v）：得到 (%+v, %v)，want %+v", c.name, preferMID, got, err, want)
			}
			if scans != 1 {
				t.Errorf("%s（preferMID=%v）：扫了 %d 遍，want 1", c.name, preferMID, scans)
			}
			for _, n := range r.reads {
				if n != 2048 && n != 200 {
					t.Errorf("%s（preferMID=%v）：每处应向后读 2048、向前读 200 字节，实得 %v", c.name, preferMID, r.reads)
					break
				}
			}
		}
	}
}
