package lyric

// 本文件只测 Word 型逐字：<span> 的起止时间、词文本，span 之间的空白归入前一个词，行文本的空白折叠，
// 以及背景人声（外层没有时间的 <span ttm:role="x-bg"> 套着逐字 span）。

import (
	"strings"
	"testing"
)

const wordDocHead = `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Word">
<head><metadata><iTunesMetadata><translations><translation xml:lang="zh-Hans">
<text for="L1">你好世界</text></translation></translations></iTunesMetadata></metadata></head>
<body dur="10.000"><div begin="1.000" end="3.500">
`

const wordDocTail = `
</div></body></tt>`

// 逐字时间 + 译文对齐（合成）。词间空格必须落在 words 里：overlay 的逐字效果与 lyrics_detailed[].text
// 都直接拼接 words，空格只在行文本里时英文会显示成「HelloWorld」（审查 P2）。
//
// 变异自证：把 span 的 begin 解析去掉，Words 为空即红；去掉 parseLineContent 里把 span 间 chardata
// 并入前一个词的分支，w[0] 变回 "Hello" 而红。
func TestParseTTMLWordTiming(t *testing.T) {
	doc := wordDocHead + `<p begin="1.000" end="3.500" itunes:key="L1"><span begin="1.000" end="1.800">Hello</span> <span begin="2.000" end="3.500">World</span></p>` + wordDocTail
	lines := ParseTTML(doc)
	if len(lines) != 1 {
		t.Fatalf("行数=%d want 1", len(lines))
	}
	l := lines[0]
	if l.Text != "Hello World" {
		t.Fatalf("Text=%q want \"Hello World\"（span 拼接）", l.Text)
	}
	if l.SubText != "你好世界" {
		t.Fatalf("SubText=%q want 你好世界", l.SubText)
	}
	if len(l.Detailed.Words) != 2 {
		t.Fatalf("逐字数=%d want 2", len(l.Detailed.Words))
	}
	w := l.Detailed.Words
	if w[0].Text != "Hello " || !approx(w[0].Timestamp, 1.0) || !approx(w[0].Duration, 0.8) {
		t.Fatalf("word0=%+v want {\"Hello \",1.0,0.8}（词间空格归入前一个词）", w[0])
	}
	if w[1].Text != "World" || !approx(w[1].Timestamp, 2.0) || !approx(w[1].Duration, 1.5) {
		t.Fatalf("word1=%+v want {World,2.0,1.5}（行尾不留空白）", w[1])
	}
}

// 空白的几种形态：词文本里多个空白/换行折成一个空格、行尾空白去掉、中日文 span 紧挨着时不凭空加空格；
// 行文本里连续的 XML 空白（空格、制表、回车、换行）同样折成一个空格——文档带换行缩进排版时，
// span 之间的换行与缩进不得原样进入行文本。全角空格是歌词自己的排版，不折。
//
// 变异自证（隔离副本，均实测）：把「空白折成一个空格」改成原样追加，第一例 w[0] 变成 "Hello\n  " 而红；
// 去掉 parseLineContent 结尾对末词的 TrimRight，第一例 w[1] 变成 "World " 而红。
// 行文本：把 parseLineContent 返回处的 foldSpace 改回 strings.TrimSpace，「多空白与行尾」「词内自带空格」
// 「span 间为换行缩进」「Line 型行内换行与制表」四例的行文本变红；把 foldSpace 改成
// strings.Join(strings.Fields(s), " ")，「全角空格原样保留」变红。
func TestParseTTMLWordSpacing(t *testing.T) {
	cases := []struct {
		name string
		p    string
		text string
		want []string
	}{
		{"多空白与行尾", `<p begin="1" end="3" itunes:key="L1"><span begin="1" end="2">Hello</span>` + "\n  " + `<span begin="2" end="3">World</span>  </p>`, "Hello World", []string{"Hello ", "World"}},
		{"中文紧挨", `<p begin="1" end="3" itunes:key="L1"><span begin="1" end="2">你</span><span begin="2" end="3">好</span></p>`, "你好", []string{"你", "好"}},
		{"词内自带空格", `<p begin="1" end="3" itunes:key="L1"><span begin="1" end="2">Hey </span> <span begin="2" end="3">you</span></p>`, "Hey you", []string{"Hey ", "you"}},
		{"span 间为换行缩进", `<p begin="1" end="3" itunes:key="L1">` + "\n      " + `<span begin="1" end="2">Hello</span>` + "\n      " + `<span begin="2" end="3">World</span>` + "\n    " + `</p>`, "Hello World", []string{"Hello ", "World"}},
		{"Line 型行内换行与制表", `<p begin="1" end="3" itunes:key="L1">Hello` + "\r\n\t  " + `World</p>`, "Hello World", nil},
		{"全角空格原样保留", `<p begin="1" end="3" itunes:key="L1">你好　世界</p>`, "你好　世界", nil},
	}
	for _, c := range cases {
		lines := ParseTTML(wordDocHead + c.p + wordDocTail)
		if len(lines) != 1 {
			t.Fatalf("%s: 行数=%d want 1", c.name, len(lines))
		}
		if lines[0].Text != c.text {
			t.Errorf("%s: 行文本=%q want %q", c.name, lines[0].Text, c.text)
		}
		w := lines[0].Detailed.Words
		if len(w) != len(c.want) {
			t.Fatalf("%s: 逐字数=%d want %d", c.name, len(w), len(c.want))
		}
		for i := range w {
			if w[i].Text != c.want[i] {
				t.Fatalf("%s: word%d=%q want %q", c.name, i, w[i].Text, c.want[i])
			}
		}
	}
}

// 背景人声：<span ttm:role="x-bg"> 自己没有 begin / end，里面套着逐字 span（Apple 交付规范与下发样本的
// 写法，Word 型常见，也可能在行首）。内层 span 须各自成词，words 拼接与行文本一致。
//
// 变异自证（隔离副本，实测）：把 parseLineContent 里「解不出 begin 的 span 不整段吞」那个 continue 去掉
// （回到整段读完、不产生词），前六例全红：逐字数不对，或背景词不在 words 里。
func TestParseTTMLBackgroundVocals(t *testing.T) {
	type word struct {
		text    string
		ts, dur float32
	}
	cases := []struct {
		name   string
		p      string
		text   string
		words  []word
		joined bool // words 拼接须等于行文本
	}{
		{
			"x-bg 在行尾",
			`<span begin="1.0" end="1.5">Alpha</span> <span begin="1.5" end="2.0">beta</span> <span ttm:role="x-bg"><span begin="3.0" end="3.5">(gamma</span> <span begin="3.5" end="4.25">delta)</span></span>`,
			"Alpha beta (gamma delta)",
			[]word{{"Alpha ", 1.0, 0.5}, {"beta ", 1.5, 0.5}, {"(gamma ", 3.0, 0.5}, {"delta)", 3.5, 0.75}},
			true,
		},
		{
			"x-bg 在行首",
			`<span ttm:role="x-bg"><span begin="1.0" end="1.4">(gamma</span> <span begin="1.4" end="1.9">delta)</span></span> <span begin="2.0" end="2.5">Alpha</span> <span begin="2.6" end="3.0">beta</span>`,
			"(gamma delta) Alpha beta",
			[]word{{"(gamma ", 1.0, 0.4}, {"delta) ", 1.4, 0.5}, {"Alpha ", 2.0, 0.5}, {"beta", 2.6, 0.4}},
			true,
		},
		{
			"x-bg 在行中",
			`<span begin="1.0" end="1.5">Alpha</span> <span ttm:role="x-bg"><span begin="1.5" end="2.0">(gamma)</span></span> <span begin="2.0" end="2.5">beta</span>`,
			"Alpha (gamma) beta",
			[]word{{"Alpha ", 1.0, 0.5}, {"(gamma) ", 1.5, 0.5}, {"beta", 2.0, 0.5}},
			true,
		},
		{
			"中文紧挨，内外都没有空白",
			`<span begin="1.0" end="1.5">甲</span><span begin="1.5" end="2.0">乙</span><span ttm:role="x-bg"><span begin="2.0" end="2.5">（丙</span><span begin="2.5" end="3.0">丁）</span></span>`,
			"甲乙（丙丁）",
			[]word{{"甲", 1.0, 0.5}, {"乙", 1.5, 0.5}, {"（丙", 2.0, 0.5}, {"丁）", 2.5, 0.5}},
			true,
		},
		{
			"两层没有时间的 span",
			`<span begin="1.0" end="1.5">Alpha</span> <span><span ttm:role="x-bg"><span begin="2.0" end="2.5">(gamma)</span></span></span>`,
			"Alpha (gamma)",
			[]word{{"Alpha ", 1.0, 0.5}, {"(gamma)", 2.0, 0.5}},
			true,
		},
		{
			"没有时间的 span 里只有文字：并入前一个词",
			`<span begin="1.0" end="1.5">Alpha</span> <span begin="1.5" end="2.0">beta</span> <span ttm:role="x-bg">(ooh)</span>`,
			"Alpha beta (ooh)",
			[]word{{"Alpha ", 1.0, 0.5}, {"beta (ooh)", 1.5, 0.5}},
			true,
		},
		{
			// 记录现状：外层解得出 begin 时仍整段读成一个词，用外层的时间。
			"外层 x-bg 自带时间：整段作一个词",
			`<span begin="1.0" end="1.5">Alpha</span> <span ttm:role="x-bg" begin="2.0" end="3.0"><span begin="2.0" end="2.5">(gamma</span> <span begin="2.5" end="3.0">delta)</span></span>`,
			"Alpha (gamma delta)",
			[]word{{"Alpha ", 1.0, 0.5}, {"(gamma delta)", 2.0, 1.0}},
			true,
		},
		{
			// 记录现状：第一个词之前、没有时间的文字只进行文本，不属于任何词。
			"行首没有时间的文字",
			`<span ttm:role="x-bg">(ooh)</span> <span begin="1.0" end="1.5">Alpha</span>`,
			"(ooh) Alpha",
			[]word{{"Alpha", 1.0, 0.5}},
			false,
		},
	}
	for _, c := range cases {
		lines := ParseTTML(wordDocHead + `<p begin="1.0" end="6.0" itunes:key="L1">` + c.p + `</p>` + wordDocTail)
		if len(lines) != 1 {
			t.Errorf("%s: 行数=%d want 1", c.name, len(lines))
			continue
		}
		l := lines[0]
		if l.Text != c.text {
			t.Errorf("%s: 行文本=%q want %q", c.name, l.Text, c.text)
		}
		w := l.Detailed.Words
		if len(w) != len(c.words) {
			t.Errorf("%s: 逐字数=%d want %d（%+v）", c.name, len(w), len(c.words), w)
			continue
		}
		var joined strings.Builder
		for i, cw := range c.words {
			if w[i].Text != cw.text || !approx(w[i].Timestamp, cw.ts) || !approx(w[i].Duration, cw.dur) {
				t.Errorf("%s: word%d=%+v want {%q %v %v}", c.name, i, w[i], cw.text, cw.ts, cw.dur)
			}
			joined.WriteString(w[i].Text)
		}
		if c.joined && joined.String() != l.Text {
			t.Errorf("%s: words 拼接=%q 与行文本 %q 不一致", c.name, joined.String(), l.Text)
		}
	}
}
