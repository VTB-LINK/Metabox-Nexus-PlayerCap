package qqmusic

// 本文件只测 parseLRC 拆出的逐行解析 parseLRCLines（设计 §6.6 第 3 步）：它不把无时间戳的文本按时长均分，
// 至多解析 maxLocalLines+1 行；在线取词的 parseLRC 照旧均分。

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 变异自证（实测）：parseLRCLines 改为调用带兜底的 parseLRC，三例都红。
func TestParseLRCLinesNoEvenSpread(t *testing.T) {
	for _, in := range []string{"plain\ntext", "[ti:x]", "[00:00.00]\nnot lyric data", "", "\n \n"} {
		if lines, _, _ := parseLRCLines(in); len(lines) != 0 {
			t.Errorf("parseLRCLines(%q) = %d lines %+v, want 0", in, len(lines), lines)
		}
	}
}

// 在线取词不变：无时间戳的两行仍按 8 秒均分到 0 与 4 秒。
func TestParseLRCStillSpreadsPlainText(t *testing.T) {
	lines, _, _ := parseLRC("plain\ntext", 8000)
	if len(lines) != 2 || lines[0].Text != "plain" || lines[0].Time != 0 || lines[1].Text != "text" || lines[1].Time != 4 {
		t.Errorf("parseLRC(\"plain\\ntext\", 8000) = %+v, want plain@0 and text@4", lines)
	}
	if lines, _, _ := parseLRC("[00:00.00]\nnot lyric data", 8000); len(lines) != 1 || lines[0].Text != "not lyric data" {
		t.Errorf("parseLRC with an empty timed line = %+v, want the plain line spread", lines)
	}
}

// 有时间戳的输入：两者的行、歌名与歌手逐项相同（LRC 与 QRC 逐字两种）。
func TestParseLRCLinesMatchesParseLRCOnTimedInput(t *testing.T) {
	for _, in := range []string{
		"[ti:歌名]\n[ar:歌手]\n[00:01.00]第一行\n[00:03.50]第二行\n无时间戳的行\n",
		qrcXML("[ti:歌名]\n[ar:歌手]\n[1000,2000]第(1000,500)一(1500,500)行(2000,1000)\n"),
	} {
		l1, n1, s1 := parseLRC(in, 8000)
		l2, n2, s2 := parseLRCLines(in)
		if len(l2) == 0 || !reflect.DeepEqual(l1, l2) || n1 != n2 || s1 != s2 {
			t.Errorf("parseLRCLines(%q) = %+v, %q, %q\nparseLRC gives %+v, %q, %q", in, l2, n2, s2, l1, n1, s1)
		}
	}
}

// 超过上限的输入解析到 maxLocalLines+1 行即停，不把整份展开。
// 变异自证（实测）：去掉提前停止即红。
func TestParseLRCLinesStopsPastLimit(t *testing.T) {
	var b strings.Builder
	for i := range maxLocalLines + 100 {
		fmt.Fprintf(&b, "[%d:%02d.00]l%d\n", i/60, i%60, i)
	}
	if lines, _, _ := parseLRCLines(b.String()); len(lines) != maxLocalLines+1 {
		t.Errorf("parseLRCLines of %d timed lines = %d lines, want %d", maxLocalLines+100, len(lines), maxLocalLines+1)
	}
}
