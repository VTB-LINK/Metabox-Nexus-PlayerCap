package lyric

// 本文件只测译文轨：译文按 itunes:key 对齐到正文行（非按行号、非时间戳）；<text> 里套 <span> 时取全部
// 文字；多个 <translation> 同一 key 时先到先得。
//
// 夹具 testdata/qingtian_line.xml 是从 Apple Music 进程内存实抽的连续 UTF-16 TTML（周杰伦《晴天》，
// 42 行 Line 型，正文繁体，带 zh-Hans 译轨）。

import (
	"os"
	"testing"
)

// transDoc 拼一份 Line 型文档：translations 是 <translations> 的内层，三行正文的 key 为 L1~L3。
func transDoc(translations string) string {
	return `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line">` +
		`<head><metadata><iTunesMetadata xmlns="http://music.apple.com/lyric-ttml-internal"><translations>` + translations + `</translations></iTunesMetadata></metadata></head>` +
		`<body dur="20.000"><div begin="1.000" end="9.000">` +
		`<p begin="1.000" end="3.000" itunes:key="L1">first line</p>` +
		`<p begin="3.000" end="6.000" itunes:key="L2">second line</p>` +
		`<p begin="6.000" end="9.000" itunes:key="L3">third line</p>` +
		`</div></body></tt>`
}

// 译文 <text> 里套 span（背景人声的译文、或整句包在 span 里）：取元素内全部文字，按文档顺序，
// 连续的 XML 空白折成一个空格。
//
// 变异自证（隔离副本，实测）：把 transText 改回只收直属文字（`xml:",chardata"`），「尾部套 span」只剩
// "主句"、「整句包在 span 里」与「两层 span」变成空串，连同「span 在中间」「换行缩进排版」「实体」共六例变红；
// 把 flattenText 里的 foldSpace 换成 strings.TrimSpace，「换行缩进排版」变红。
func TestParseTTMLTranslationNestedSpan(t *testing.T) {
	cases := []struct {
		name string
		text string // <text for="L1"> 的内层
		want string
	}{
		{"尾部套 span", `主句 <span ttm:role="x-bg">(和声)</span>`, "主句 (和声)"},
		{"整句包在 span 里", `<span>整句都在里面</span>`, "整句都在里面"},
		{"两层 span", `<span><span ttm:role="x-bg">(里层)</span></span>`, "(里层)"},
		{"span 在中间", `前半 <span>中间</span> 后半`, "前半 中间 后半"},
		{"换行缩进排版", "\n      前半\n      <span>后半</span>\n    ", "前半 后半"},
		{"实体", `甲 &amp; <span>乙 &lt;丙&gt;</span>`, "甲 & 乙 <丙>"},
		{"没有 span 的原样", `普通译文`, "普通译文"},
		{"span 里只有空白：等于没有译文", `<span> </span>`, ""},
	}
	for _, c := range cases {
		lines := ParseTTML(transDoc(`<translation xml:lang="zh-Hans"><text for="L1">` + c.text + `</text><text for="L2">第二句</text></translation>`))
		if len(lines) != 3 {
			t.Fatalf("%s: 行数=%d want 3", c.name, len(lines))
		}
		if lines[0].SubText != c.want {
			t.Errorf("%s: SubText=%q want %q", c.name, lines[0].SubText, c.want)
		}
		if lines[1].SubText != "第二句" || lines[2].SubText != "" {
			t.Errorf("%s: 其余行的译文被带偏: L2=%q L3=%q", c.name, lines[1].SubText, lines[2].SubText)
		}
	}
}

// 多个 <translation>（多种语言）：同一 key 先到先得，后面语言的不覆盖前面的。第一种语言缺的 key
// （没有该 <text>，或只有空白）由后面的补上。
//
// 变异自证（隔离副本，实测）：把 ParseTTML 里「同一 for 已有译文则不覆盖」的判断去掉（后写覆盖先写），
// L1 变成 "second language" 而红；把译文改成按行序（第 i 行取第 i 个 <text>）而不是按 itunes:key 取，
// L2 为空、L3 变成 "second language" 而红。
func TestParseTTMLMultipleTranslations(t *testing.T) {
	lines := ParseTTML(transDoc(
		`<translation xml:lang="zh-Hans"><text for="L1">第一种语言</text><text for="L3">  </text></translation>` +
			`<translation xml:lang="en"><text for="L1">second language</text><text for="L2">only in second</text><text for="L3">filled by second</text></translation>`))
	if len(lines) != 3 {
		t.Fatalf("行数=%d want 3", len(lines))
	}
	for i, want := range []string{"第一种语言", "only in second", "filled by second"} {
		if lines[i].SubText != want {
			t.Errorf("L%d: SubText=%q want %q", i+1, lines[i].SubText, want)
		}
	}
}

// 42 行，首行文本（繁体）+ 译文（简体，按 itunes:key L1 对齐）。
// 变异自证（隔离副本，实测）：把 ParseTTML 里的 `SubText: trans[p.Key]` 改成不填译文，首行译文为空而红。
// 把译文改成按行序而不是按 itunes:key 取，本例仍绿——夹具里 key 与行序一致，本例只守「有译文」；那一变异
// 由 TestParseTTMLMultipleTranslations 守。
func TestParseTTMLLineFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/qingtian_line.xml")
	if err != nil {
		t.Fatalf("读夹具: %v", err)
	}
	lines := ParseTTML(string(data))
	if len(lines) != 42 {
		t.Fatalf("行数=%d, want 42", len(lines))
	}
	// 首行：繁体正文 + 简体译文
	if !approx(lines[0].Time, 29.188) {
		t.Fatalf("lines[0].Time=%v want 29.188", lines[0].Time)
	}
	if lines[0].Text != "故事的小黃花" {
		t.Fatalf("lines[0].Text=%q want 故事的小黃花（繁体正文）", lines[0].Text)
	}
	if lines[0].SubText != "故事的小黄花" {
		t.Fatalf("lines[0].SubText=%q want 故事的小黄花（zh-Hans 译文，按 itunes:key L1 对齐）", lines[0].SubText)
	}
	// Line 型无逐字
	if len(lines[0].Detailed.Words) != 0 {
		t.Fatalf("Line 型不应有逐字 Words，got %d", len(lines[0].Detailed.Words))
	}
	// 每行都应有译文（本歌译轨覆盖全 42 行）
	missing := 0
	for _, l := range lines {
		if l.SubText == "" {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("本歌译轨应覆盖全部 42 行，缺 %d 行 SubText（itunes:key 对齐是否失效？）", missing)
	}
}
