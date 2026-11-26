package qqmusic

// 本文件只测歌词行的深拷贝（cloneLines）：改副本（含逐字与振假名切片）不影响原件。

import (
	"reflect"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// 变异自证（实测）：不另行分配 Detailed.Words、RomaDetailed.Words 或 Furigana，各自即红。
func TestCloneLinesIsDeep(t *testing.T) {
	orig := shiftFixture()
	c := cloneLines(orig)
	if !reflect.DeepEqual(c, orig) {
		t.Fatalf("cloneLines = %+v, want equal to the original", c)
	}
	c[0].Text = "changed"
	c[0].Detailed.Words[0].Timestamp = 99
	c[0].RomaDetailed.Words[1].Text = "changed"
	shiftTimeline(c, -5000)
	if !reflect.DeepEqual(orig, shiftFixture()) {
		t.Errorf("changing the clone changed the original: %+v", orig)
	}

	origWithKana := shiftFixture()
	origWithKana[0].Furigana = player.LyricFurigana{
		{Text: "心", Ruby: "こころ"},
		{Text: "の"},
	}
	wantKana := player.LyricFurigana{
		{Text: "心", Ruby: "こころ"},
		{Text: "の"},
	}
	ck := cloneLines(origWithKana)
	if !reflect.DeepEqual(ck, origWithKana) {
		t.Fatalf("cloneLines(with Furigana) = %+v, want equal to the original", ck)
	}
	ck[0].Furigana[0].Text = "changed"
	ck[0].Furigana[0].Ruby = "かわった"
	if !reflect.DeepEqual(origWithKana[0].Furigana, wantKana) {
		t.Errorf("changing clone Furigana changed the original: %+v", origWithKana[0].Furigana)
	}
}

// 反例：nil 与空切片的拷贝为空，不出错。
func TestCloneLinesEmpty(t *testing.T) {
	if c := cloneLines(nil); len(c) != 0 {
		t.Errorf("cloneLines(nil) = %+v", c)
	}
	if c := cloneLines([]lyricLine{}); len(c) != 0 {
		t.Errorf("cloneLines(empty) = %+v", c)
	}
}
