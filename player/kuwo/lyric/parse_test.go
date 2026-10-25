package lyric

// 本文件守酷我 LRC 的解析细节：
//   - 行时间 [mm:ss.xxx]（1~3 位小数都要吃得下）；
//   - 行内 <a,b> 不混入行级正文；逐字转换由 wordtiming_test.go 独立验证；
//   - [offset:±ms] 要累加到每一行；
//   - [kuwo:nnn] / [ver:v1.0] / [ti:] / [ar:] / [al:] / [by:] 这些标签行不是歌词，跳过。
//
// 变异自证：把 stripWordMarks 去掉（标记会混进正文）、或把 offset 忽略，用例即红。

import "testing"

func TestParseLRCStripsWordMarks(t *testing.T) {
	lrc := "[kuwo:126]\n[ver:v1.0]\n[ti:测试]\n[00:13.140]<360,-360>你<840,120>好<1320,600>世<1800,1080>界\n"
	lines := ParseLRC(lrc)
	if len(lines) != 1 {
		t.Fatalf("应只剩 1 行歌词，实得 %d：%+v", len(lines), lines)
	}
	if lines[0].TimeMs != 13140 {
		t.Errorf("时间应为 13140ms，实为 %d", lines[0].TimeMs)
	}
	if lines[0].Text != "你好世界" {
		t.Errorf("正文应为「你好世界」，实为 %q", lines[0].Text)
	}
}

func TestParseLRCAppliesOffset(t *testing.T) {
	lrc := "[offset:-500]\n[00:01.000]甲\n[00:02.500]乙\n"
	lines := ParseLRC(lrc)
	if len(lines) != 2 {
		t.Fatalf("应 2 行，实得 %d", len(lines))
	}
	if lines[0].TimeMs != 500 {
		t.Errorf("offset 未生效：%d，应 500", lines[0].TimeMs)
	}
	if lines[1].TimeMs != 2000 {
		t.Errorf("offset 未生效：%d，应 2000", lines[1].TimeMs)
	}
}

func TestParseLRCFractionDigits(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"[00:01.5]甲", 1500},
		{"[00:01.50]甲", 1500},
		{"[00:01.500]甲", 1500},
		{"[01:02.345]甲", 62345},
		{"[10:00.000]甲", 600000},
	}
	for _, c := range cases {
		lines := ParseLRC(c.in)
		if len(lines) != 1 {
			t.Fatalf("%q 解析失败", c.in)
		}
		if lines[0].TimeMs != c.want {
			t.Errorf("%q 时间=%d，应 %d", c.in, lines[0].TimeMs, c.want)
		}
	}
}

func TestParseLRCSkipsNonLyricLines(t *testing.T) {
	lrc := "[kuwo:065]\n[ver:v1.0]\n[ti:湮灭]\n[ar:萨吉]\n[al:湮灭]\n[by:kalakaer1]\n[offset:0]\n"
	if lines := ParseLRC(lrc); len(lines) != 0 {
		t.Errorf("标签行不应产出歌词行：%+v", lines)
	}
}

func TestParseLRCDropsEmptyTextAfterMarks(t *testing.T) {
	lrc := "[00:01.000]<153,-153><306,0>\n[00:02.000]有字\n"
	lines := ParseLRC(lrc)
	if len(lines) != 1 {
		t.Fatalf("只有标记的行应被丢弃，实得 %d 行：%+v", len(lines), lines)
	}
	if lines[0].Text != "有字" {
		t.Errorf("正文=%q", lines[0].Text)
	}
}

func TestStripWordMarks(t *testing.T) {
	cases := map[string]string{
		"<153,-153>你<408,102>好": "你好",
		"无标记":                   "无标记",
		"<1,2>":                 "",
		"混<3,4>排<5,6>文":         "混排文",
	}
	for in, want := range cases {
		if got := stripWordMarks(in); got != want {
			t.Errorf("stripWordMarks(%q)=%q，应 %q", in, got, want)
		}
	}
}
