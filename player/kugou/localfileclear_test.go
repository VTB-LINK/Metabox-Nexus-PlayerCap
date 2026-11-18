package kugou

// 本文件只测本地文件在播放中清词（#205，设计 §6.7「播放中」）：收到这一首的「没有」、不待重试、带本地文件标记、这一首
// 有界限且 P 属于这一次播放的结果，而当前歌词是换歌拍沿用的上一首歌词（其后还没有采纳过本地歌词），或采纳自本地、
// 而结果的 P 与记下的不同时，清掉当前歌词：以此刻的位置发 count:0 的 all_lyrics 与 index:-1 的 lyric_update。之后来源
// 清零、记下新 P、沿用标记清除，同样的结果再来不重发。P 相同的按「2甲」保持；当前歌词是在线取到的不清。夹具与辅助
// 函数见 fakelocal_test.go、localfileswitch_test.go。
//
// 变异自证（逐条注入、跑完即还原，实测）记在报告与各用例的注释里。

import (
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
)

// A 是会话第一首（在线，与 B 同名），0.1 s 收到 P 为 aP（默认 0 s）的结果，给 B 定下界限 aP；B 于 2.1 s 换歌。B 的
// 换歌拍没有回复时等满 300 ms，其后各拍在 3.4 s、4.4 s、5.4 s……（读数 1、2、3 s），回复的 after 自 2.1 s 起算：1 s、2 s、
// 3 s 送到的分别在读数 1、2、3 s 那拍收下。换歌拍于 0.1 s 收到回复的两例，其后各拍在 3.2 s、4.2 s……，收下的拍次不变。
//
// 变异自证：
//   - 清词时不清沿用标记：「清掉、同样的结果不重发」红；清词后来源、P 与沿用标记都不变（重复结果重发）：再加「单曲循环」；
//   - 清词后不清零来源：「清掉之后来源清零」红；
//   - 采纳本地歌词时不清沿用标记，或去掉「2甲」边界（来源已记、P 相同也清）：两个「P 相同」例红；
//   - 当前歌词是在线取到的也清：「在线取到歌词」两例与「不同名、本就无歌词」红；
//   - 同名复用一支不置沿用标记：在线取不到的五个「清掉」例红；出错一支不置：「在线出错时保留的歌词同样清掉」红；
//   - 去掉「有界限」要求：TestSessionLocalFileFirstSong 红；去掉「大于界限」一条：「P 等于界限」「P 早于界限」「换歌拍
//     误选旧 P」红；去掉 5 秒一条：「早于开始时刻减 5 秒」红；5 秒改为严格晚于：「恰为开始时刻减 5 秒」红；
//   - 去掉待重试一条、会话不看本地文件标记：「带待重试」「不带本地文件标记」分别红。
func TestSessionLocalFileMidSongClear(t *testing.T) {
	const sec = time.Second
	dn := func(h string, d time.Duration) assoc.Result { return diskNone(h, lpt(d)) }
	retry := dn("XB", 2*sec)
	retry.Retry = true
	type step = reply
	// keep 是不清时 3.4 s 那拍的事件：照常发沿用歌词的第 0 行（换歌拍那一拍还轮不到它，见 lyricsAt）。
	keep := []string{"lyric_update#0:A一"}
	cases := []struct {
		name   string
		aP     int64 // 0 即 lpt(0)
		file   string
		hash   string
		b      []step
		bStart []string // B 换歌拍的歌词事件；nil 即沿用 A 的（shown("A")）
		want   []string // 其后的歌词事件
	}{
		{"清掉、同样的结果不重发", 0, sameName, "XB", []step{{sec, dn("XB", 2*sec)}, {2 * sec, dn("XB", 2*sec)}},
			nil, lyricsAt("1.00", "")},
		{"在线出错时保留的歌词同样清掉", 0, sameName, "EB", []step{{sec, dn("EB", 2*sec)}},
			nil, lyricsAt("1.00", "")},
		{"清掉之后再来的歌词照常换上", 0, sameName, "XB", []step{{sec, dn("XB", 2*sec)}, {2 * sec, playedAt(found("XB", lyricSet("新")), lpt(2*sec))}},
			nil, slices.Concat(lyricsAt("1.00", ""), lyricsAt("2.00", "新"))},
		{"先采纳旧 P 的关联、再收到新 P 的：清掉", 0, sameName, "XB", []step{
			{sec, playedAt(foundAssoc("XB", lyricSet("旧")), lpt(-sec))},
			{2 * sec, dn("XB", 2*sec)},
		}, nil, slices.Concat(lyricsAt("1.00", "旧"), lyricsAt("2.00", ""))},
		{"旧关联内容恰与沿用的相同、未重发：同样清掉", 0, sameName, "XB", []step{
			{sec, playedAt(foundAssoc("XB", lyricSet("A")), lpt(-sec))},
			{2 * sec, dn("XB", 2*sec)},
		}, nil, slices.Concat(keep, lyricsAt("2.00", ""))},
		// 清掉之前来源是关联；清掉之后同一次播放（P 相同）带「关联未取用」的缓存要照常换上，来源未清零会被「2甲」挡住。
		{"清掉之后来源清零", 0, sameName, "XB", []step{
			{sec, playedAt(foundAssoc("XB", lyricSet("关联")), lpt(-sec))},
			{2 * sec, dn("XB", 2*sec)},
			{3 * sec, playedAt(foundSkipped("XB", lyricSet("缓存")), lpt(2*sec))},
		}, nil, slices.Concat(lyricsAt("1.00", "关联"), lyricsAt("2.00", ""), lyricsAt("3.00", "缓存"))},
		{"单曲循环重播后清掉，同样的结果不重发", 0, sameName, "XB", []step{
			{sec, playedAt(found("XB", lyricSet("缓存")), lpt(2*sec))},
			{2 * sec, dn("XB", 3*sec)},
			{3 * sec, dn("XB", 3*sec)},
		}, nil, slices.Concat(lyricsAt("1.00", "缓存"), lyricsAt("2.00", ""))},
		{"首次播放换歌拍时还没有 P、落盘后清掉", 0, sameName, "XB", []step{
			{100 * time.Millisecond, assoc.Result{Hash: "XB", Kind: assoc.KindNone}},
			{sec, dn("XB", 2*sec)},
		}, nil, lyricsAt("1.00", "")},
		{"换歌拍误选旧 P、落盘后清掉", 0, sameName, "XB", []step{
			{100 * time.Millisecond, dn("XB", -sec)},
			{sec, dn("XB", 2*sec)},
		}, nil, lyricsAt("1.00", "")},
		{"P 恰为开始时刻减 5 秒，清掉", lpt(-5 * sec), sameName, "XB", []step{{sec, dn("XB", -2900*time.Millisecond)}},
			nil, lyricsAt("1.00", "")},
		// 反例：不清。
		{"P 相同按「2甲」保持（关联）", 0, sameName, "XB", []step{
			{sec, playedAt(foundAssoc("XB", lyricSet("关联")), lpt(2*sec))},
			{2 * sec, dn("XB", 2*sec)},
		}, nil, lyricsAt("1.00", "关联")},
		{"P 相同按「2甲」保持（缓存）", 0, sameName, "XB", []step{
			{sec, playedAt(found("XB", lyricSet("缓存")), lpt(2*sec))},
			{2 * sec, dn("XB", 2*sec)},
		}, nil, lyricsAt("1.00", "缓存")},
		{"P 等于界限", 0, sameName, "XB", []step{{sec, dn("XB", 0)}}, nil, keep},
		{"P 早于界限", 0, sameName, "XB", []step{{sec, dn("XB", -sec)}}, nil, keep},
		{"P 早于开始时刻减 5 秒", lpt(-5 * sec), sameName, "XB", []step{{sec, diskNone("XB", lpt(-2900*time.Millisecond)-1)}}, nil, keep},
		{"过时 hash", 0, sameName, "XB", []step{{sec, dn("A", 2*sec)}}, nil, keep},
		{"带待重试", 0, sameName, "XB", []step{{sec, retry}}, nil, keep},
		{"不带本地文件标记", 0, sameName, "XB", []step{{sec, onlineNone("XB", lpt(2*sec))}}, nil, keep},
		{"暂时失败", 0, sameName, "XB", []step{{sec, assoc.Result{Hash: "XB", Kind: assoc.KindTransient}}}, nil, keep},
		{"在线取到歌词（同名）", 0, sameName, "B", []step{{sec, dn("B", 2*sec)}}, shown("B"), []string{"lyric_update#0:B一"}},
		{"在线取到歌词（不同名）", 0, "歌手 - 另一首", "B", []step{{sec, dn("B", 2*sec)}}, shown("B"), []string{"lyric_update#0:B一"}},
		{"不同名、本就无歌词", 0, "歌手 - 另一首", "XB", []step{{sec, dn("XB", 2*sec)}}, shown(""), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aP := c.aP
			if aP == 0 {
				aP = lpt(0)
			}
			run := runFile(t, nil, map[string][]reply{
				"A":    {{100 * time.Millisecond, onlineNone("A", aP)}},
				c.hash: c.b,
			}, slices.Concat(beats(sameName, "A", 2), beats(c.file, c.hash, 5))...)
			if run.setAt[1] != 2100*time.Millisecond {
				t.Fatalf("B 于 %v 换歌，want 2.1s", run.setAt[1])
			}
			bStart := c.bStart
			if bStart == nil {
				bStart = shown("A")
			}
			checkLyricEvents(t, lyricEvents(run.events), slices.Concat(lyricsAt("0.00", "A"), bStart, c.want))
		})
	}
}

// 会话第一首没有界限，播放中的清词同样不用：H 换歌拍采纳本地歌词（P 为 -1 s），之后收到 P 为 0 s（不同、且属于这一次
// 播放）的「没有」、带本地文件标记，不清。
func TestSessionLocalFileFirstSong(t *testing.T) {
	run := runFile(t, nil, map[string][]reply{"XH": {
		{100 * time.Millisecond, playedAt(found("XH", lyricSet("H")), lpt(-time.Second))},
		{1500 * time.Millisecond, diskNone("XH", lpt(0))},
	}}, beats(sameName, "XH", 3)...)
	checkLyricEvents(t, lyricEvents(run.events), lyricsAt("0.00", "H"))
}
