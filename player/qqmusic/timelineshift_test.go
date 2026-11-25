package qqmusic

// 本文件只测按 QQ 关联条目的 LyricOffset 平移歌词时间轴（shiftTimeline，设计 §6.7）：负数延后、正数提前，
// 行与逐字的起始一并平移，下限 0；时长与 PlayTime 不动。

import (
	"reflect"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// shiftFixture 返回两行：第 0 行 0.2 秒、带主歌词与音译的逐字；第 1 行 2 秒、只有行级。
func shiftFixture() []lyricLine {
	words := func(ts ...float32) []player.LyricTextDetailedWord {
		var w []player.LyricTextDetailedWord
		for _, t := range ts {
			w = append(w, player.LyricTextDetailedWord{Timestamp: t, Duration: 0.1, PlayTime: 9, Text: "x"})
		}
		return w
	}
	return []lyricLine{
		{
			Index: 0, Time: 0.2, Text: "a",
			Detailed:     player.LyricTextDetailed{Timestamp: 0.2, Duration: 1, PlayTime: 9, Words: words(0.2, 0.6)},
			RomaDetailed: player.LyricTextDetailed{Timestamp: 0.25, Duration: 1, PlayTime: 9, Words: words(0.25, 0.7)},
		},
		{Index: 1, Time: 2, Text: "b"},
	}
}

// 变异自证（实测）：跳过 RomaDetailed、不平移逐字起始，各自即红。
func TestShiftTimelineDelay(t *testing.T) {
	lines := shiftFixture()
	shiftTimeline(lines, -1000)
	want := shiftFixture()
	want[0].Time, want[1].Time = 1.2, 3
	want[0].Detailed.Timestamp, want[0].Detailed.Words[0].Timestamp, want[0].Detailed.Words[1].Timestamp = 1.2, 1.2, 1.6
	want[0].RomaDetailed.Timestamp, want[0].RomaDetailed.Words[0].Timestamp, want[0].RomaDetailed.Words[1].Timestamp = 1.25, 1.25, 1.7
	assertTimeline(t, lines, want)
}

// 变异自证（实测）：去掉下限 0 即红。
func TestShiftTimelineAdvanceFloorsAtZero(t *testing.T) {
	lines := shiftFixture()
	shiftTimeline(lines, 500)
	want := shiftFixture()
	want[0].Time, want[1].Time = 0, 1.5
	want[0].Detailed.Timestamp, want[0].Detailed.Words[0].Timestamp, want[0].Detailed.Words[1].Timestamp = 0, 0, 0.1
	want[0].RomaDetailed.Timestamp, want[0].RomaDetailed.Words[0].Timestamp, want[0].RomaDetailed.Words[1].Timestamp = 0, 0, 0.2
	assertTimeline(t, lines, want)
}

// 反例：偏移 0 逐项不变；没有逐字的行，其逐字结构保持零值；空输入不出错。
func TestShiftTimelineNoopCases(t *testing.T) {
	lines := shiftFixture()
	shiftTimeline(lines, 0)
	if !reflect.DeepEqual(lines, shiftFixture()) {
		t.Errorf("shiftTimeline(0) changed the lines: %+v", lines)
	}
	shiftTimeline(lines, -1000)
	if !reflect.DeepEqual(lines[1].Detailed, player.LyricTextDetailed{}) || !reflect.DeepEqual(lines[1].RomaDetailed, player.LyricTextDetailed{}) {
		t.Errorf("a line without words got %+v / %+v; want zero values", lines[1].Detailed, lines[1].RomaDetailed)
	}
	shiftTimeline(nil, 1000)
}

// assertTimeline 逐项比较时间（容差 0.5 ms），其余字段须完全相同。
func assertTimeline(t *testing.T, got, want []lyricLine) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		ok := approx(g.Time, w.Time) && g.Index == w.Index && g.Text == w.Text
		for _, d := range [][2]player.LyricTextDetailed{{g.Detailed, w.Detailed}, {g.RomaDetailed, w.RomaDetailed}} {
			ok = ok && approx(d[0].Timestamp, d[1].Timestamp) && d[0].Duration == d[1].Duration && d[0].PlayTime == d[1].PlayTime &&
				len(d[0].Words) == len(d[1].Words)
			for j := 0; ok && j < len(d[0].Words); j++ {
				gw, ww := d[0].Words[j], d[1].Words[j]
				ok = approx(gw.Timestamp, ww.Timestamp) && gw.Duration == ww.Duration && gw.PlayTime == ww.PlayTime && gw.Text == ww.Text
			}
		}
		if !ok {
			t.Errorf("line %d = %+v\nwant %+v", i, g, w)
		}
	}
}
