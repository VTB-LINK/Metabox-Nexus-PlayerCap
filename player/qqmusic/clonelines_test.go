package qqmusic

// 本文件只测歌词行的深拷贝（cloneLines）：改副本（含逐字切片）不影响原件。

import (
	"reflect"
	"testing"
)

// 变异自证（实测）：不另行分配 Detailed.Words 或 RomaDetailed.Words，各自即红。
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
