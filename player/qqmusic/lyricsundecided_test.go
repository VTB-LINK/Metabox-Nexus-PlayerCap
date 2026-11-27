package qqmusic

// 本文件只测 lyricsUndecided：换歌拍取词失败（编号已知、补取未完）后读取者给出「在线」而在线原始行还没有，要显示的为空却
// 没有结论，showLyrics 据此不发 index:-1（下游读作没有歌词）。showLyrics 里的用法由 assocwiring_test.go 守。

import (
	"testing"
	"time"
)

func TestLyricsUndecided(t *testing.T) {
	line := []lyricLine{{Index: 0, Time: 1, Text: "a"}}
	cases := []struct {
		name           string
		lines          []lyricLine
		pending        bool
		src            lyricSource
		localUndecided bool
		want           bool
	}{
		{"补取未完、在线、行为空", nil, true, srcOnline, false, true},
		{"补取已完、在线、行为空、本地首轮未决", nil, false, srcOnline, true, true},
		{"两者均未决、在线、行为空", nil, true, srcOnline, true, true},
		{"反例：补取已了结且本地已决（纯音乐或超时）", nil, false, srcOnline, false, false},
		{"反例：有行（补取未完）", line, true, srcOnline, false, false},
		{"反例：有行（本地未决）", line, false, srcOnline, true, false},
		{"反例：本地文件的关联撤销，转为无", nil, true, srcNone, true, false},
		{"反例：关联歌词", nil, true, srcAssoc, true, false},
		{"反例：本地文件", nil, true, srcLocal, true, false},
	}
	for _, c := range cases {
		if got := lyricsUndecided(c.lines, c.pending, c.src, c.localUndecided); got != c.want {
			t.Errorf("%s：lyricsUndecided = %v，应为 %v", c.name, got, c.want)
		}
	}
}

// 触发的情形在状态机上的样子：换歌拍取词失败（不存在线原始行），读取者给出「在线、偏移 0」，apply 要求重发、给出空行、
// 来源为在线，补取未完时即没有结论。补取成功存入在线原始行后折算出歌词，已有结论。
func TestLyricsUndecidedAfterClaimFetchFailed(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	lines, changed := s.apply(onlineRes(1, 0), true, stateOffsetSec)
	if !changed || len(lines) != 0 || s.source() != srcOnline || !lyricsUndecided(lines, true, s.source(), s.undecided()) {
		t.Fatalf("取词失败后的「在线」应为要求重发、空行、来源在线且没有结论：changed %v、%d 行、来源 %d", changed, len(lines), s.source())
	}
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
}
