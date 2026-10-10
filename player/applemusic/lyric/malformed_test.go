package lyric

// 本文件只测降级：无译轨的歌 SubText 全空；空或畸形输入返回 nil，不 panic；没有时间的行、
// 不在 <body><div> 直属位置的行不产生歌词行。

import "testing"

func TestParseTTMLNoTransAndGarbage(t *testing.T) {
	doc := `<tt itunes:timing="Line"><body><div><p begin="1.0" end="2.0" itunes:key="L1">only text</p></div></body></tt>`
	lines := ParseTTML(doc)
	if len(lines) != 1 || lines[0].SubText != "" || lines[0].Text != "only text" {
		t.Fatalf("无译轨解析异常: %+v", lines)
	}
	if got := ParseTTML("not xml at all <<<"); got != nil {
		t.Fatalf("畸形输入应返回 nil，got %v", got)
	}
	if got := ParseTTML(""); got != nil {
		t.Fatalf("空输入应返回 nil")
	}
}

// 记录现状：itunes:timing="None"（静态歌词，<p> 没有 begin / end）解析为空——没有时间就无从逐行推送，
// 上层按（暂时）没有歌词处理。同一文档里个别 <p> 缺 begin 时只跳过那一行。
//
// 变异自证（隔离副本，实测）：去掉 ParseTTML 里 begin 解不出即跳过的判断，没有时间的行以 0 秒进入结果，
// 两处断言都红。
func TestParseTTMLTimingNone(t *testing.T) {
	doc := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="None">` +
		`<body><div><p itunes:key="L1">static line one</p><p itunes:key="L2">static line two</p></div></body></tt>`
	if lines := ParseTTML(doc); len(lines) != 0 {
		t.Errorf("timing=None 应解析为空，got %+v", lines)
	}
	mixed := `<tt itunes:timing="Line"><body><div><p itunes:key="L1">no time</p><p begin="2.0" end="3.0" itunes:key="L2">timed</p></div></body></tt>`
	lines := ParseTTML(mixed)
	if len(lines) != 1 || lines[0].Text != "timed" || lines[0].Index != 0 || !approx(lines[0].Time, 2.0) {
		t.Fatalf("缺 begin 的行应只跳过它自己: %+v", lines)
	}
}

// 记录现状：只收 <body> 下直属 <div> 里的 <p>。<p> 直接挂在 <body> 下、或在嵌套的 <div> 里时不收
// （Apple 下发的文档未见这两种结构）。这里只钉住现有结果，改变它须先有真机样本。
//
// 变异自证（隔离副本，实测）：把 ttmlDoc.Ps 的路径由 `body>div>p` 改成 `body>p`，「直属 body」收到 1 行、
// 后两例收不到 <div> 里的行而红；改成 `body>div>div>p`，「嵌套 div」收到 1 行而红。
func TestParseTTMLParagraphPlacement(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"直属 body", `<p begin="1.0" end="2.0">under body</p>`, nil},
		{"嵌套 div", `<div><div><p begin="1.0" end="2.0">nested</p></div></div>`, nil},
		{"外层 div 的直属行与嵌套行并存", `<div><p begin="1.0" end="2.0">direct</p><div><p begin="2.0" end="3.0">nested</p></div><p begin="3.0" end="4.0">direct again</p></div>`, []string{"direct", "direct again"}},
		{"多个 div", `<div><p begin="1.0" end="2.0">one</p></div><div><p begin="2.0" end="3.0">two</p></div>`, []string{"one", "two"}},
	}
	for _, c := range cases {
		lines := ParseTTML(`<tt itunes:timing="Line"><body dur="10.000">` + c.body + `</body></tt>`)
		if len(lines) != len(c.want) {
			t.Errorf("%s: 行数=%d want %d（%+v）", c.name, len(lines), len(c.want), lines)
			continue
		}
		for i, w := range c.want {
			if lines[i].Text != w || lines[i].Index != i {
				t.Errorf("%s: lines[%d]={Index:%d Text:%q} want {%d %q}", c.name, i, lines[i].Index, lines[i].Text, i, w)
			}
		}
	}
}
