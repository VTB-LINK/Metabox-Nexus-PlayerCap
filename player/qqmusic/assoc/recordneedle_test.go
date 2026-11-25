package assoc

// 本文件只测 RecordNeedle：由专辑、歌手、歌名拼出当前歌曲记录的前缀（设计 §6.4 第 4 步「前缀」）。

import (
	"testing"
	"unicode/utf16"
)

func TestRecordNeedle(t *testing.T) {
	// 不成对的代理项按 UTF-16 解码得 U+FFFD。
	bad := string(utf16.Decode([]uint16{'a', 0xD800, 'b'}))
	cases := []struct {
		name                string
		album, singer, song string
		want                string // 空串为假
	}{
		{"中日文与拉丁文", "君の知らない物語", "supercell", "君の知らない物語 (Inst.)", "君の知らない物語supercell君の知らない物語 (Inst.)http"},
		{"首尾空格与大小写原样保留", " Album ", "SuperCell", "name\t", " Album SuperCellname\thttp"},
		{"单字符各段", "a", "B", "c", "aBchttp"},

		{"专辑为空", "", "supercell", "君の知らない物語", ""},
		{"歌手为空", "君の知らない物語", "", "君の知らない物語", ""},
		{"歌名为空", "君の知らない物語", "supercell", "", ""},
		{"专辑含不成对代理项", bad, "supercell", "x", ""},
		{"歌手含不成对代理项", "x", bad, "x", ""},
		{"歌名含不成对代理项", "x", "supercell", bad, ""},
		{"歌名含 U+FFFD 字面", "x", "supercell", "a�b", ""},
		{"歌名含非法 UTF-8", "x", "supercell", "a\xffb", ""},
	}
	for _, c := range cases {
		got, ok := RecordNeedle(c.album, c.singer, c.song)
		if ok != (c.want != "") || string(got) != c.want {
			t.Errorf("%s：RecordNeedle = (%q, %v)，应为 %q", c.name, got, ok, c.want)
		}
	}
}
