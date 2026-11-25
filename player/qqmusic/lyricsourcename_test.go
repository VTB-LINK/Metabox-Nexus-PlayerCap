package qqmusic

// 本文件只测歌词来源名（lyricSource.String）：在源头经 i18n.T（AGENTS.md §6.4 ⑤）。测试在中文下跑（i18n 的零值语言）；
// 英文即调用点的字面量，由 i18n/catalogcoverage_test.go 管住它在译表里且不含汉字。

import "testing"

// 变异自证：String 不经 i18n.T 直接返回英文、或两个来源对调，本例各自变红。
func TestLyricSourceNames(t *testing.T) {
	for _, c := range []struct {
		src  lyricSource
		want string
	}{
		{srcOnline, "QQ 音乐在线歌词"},
		{srcAssoc, "QQ 音乐关联"},
		{srcLocal, "QQ 音乐本地文件关联"},
		{srcNone, "无"},
		{srcNone + 1, "无"}, // 反例：定义之外的值按无
	} {
		if got := c.src.String(); got != c.want {
			t.Errorf("lyricSource(%d).String() = %q, want %q", int(c.src), got, c.want)
		}
	}
}
