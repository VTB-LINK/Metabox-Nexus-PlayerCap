package qqmusic

// 本文件只测 [kana:] 标签收集（kanaCollector）与词条解析（parseKanaTag）。

import (
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func TestParseKanaTagValidForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []kanaEntry
	}{
		{
			name: "空串",
			body: "",
			want: nil,
		},
		{
			name: "纯无时间词条含连续跳过与长音",
			body: "1こころ1111111し1あき1もと1やすし1ペ1ザー",
			want: []kanaEntry{
				{count: 1, ruby: "こころ"},
				{count: 1},
				{count: 1},
				{count: 1},
				{count: 1},
				{count: 1},
				{count: 1},
				{count: 1, ruby: "し"},
				{count: 1, ruby: "あき"},
				{count: 1, ruby: "もと"},
				{count: 1, ruby: "やすし"},
				{count: 1, ruby: "ペ"},
				{count: 1, ruby: "ザー"},
			},
		},
		{
			name: "纯带音节时间词条（blue hour 与 風景画 样本）",
			body: "2あ(128976,624)し(129600,161)た(129761,159)3ペ(353471,221)ザ(353692,381)ー(354073,381)ジ(354455,386)ュ(354841,386)",
			want: []kanaEntry{
				{count: 2, ruby: "あした"},
				{count: 3, ruby: "ペザージュ"},
			},
		},
		{
			name: "带时间、无时间与跳过词条混排（含 0 与 9 位时间戳边界）",
			body: "1ぼく2あ(128976,624)し(129600,161)た(129761,159)11む1あ(0,0)9い(999999999,123456789)",
			want: []kanaEntry{
				{count: 1, ruby: "ぼく"},
				{count: 2, ruby: "あした"},
				{count: 1},
				{count: 1, ruby: "む"},
				{count: 1, ruby: "あ"},
				{count: 9, ruby: "い"},
			},
		},
		{
			name: "假名范围边界含中点与平片假名叠字符",
			body: "1ぁゖゝゞァヺ・ーヽヾ1・(10,20)ゝ(30,40)ヾ(50,60)",
			want: []kanaEntry{
				{count: 1, ruby: "ぁゖゝゞァヺ・ーヽヾ"},
				{count: 1, ruby: "・ゝヾ"},
			},
		},
		{
			name: "末尾空跳过词条",
			body: "1こころ2",
			want: []kanaEntry{
				{count: 1, ruby: "こころ"},
				{count: 2},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseKanaTag(tc.body)
			if !ok {
				t.Fatalf("parseKanaTag(%q) ok = false，want true", tc.body)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseKanaTag(%q) = %#v，want %#v", tc.body, got, tc.want)
			}
		})
	}
}

func TestParseKanaTagRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "0 开头带假名", body: "0あ"},
		{name: "单独 0", body: "0"},
		{name: "后续词条以 0 开头", body: "1あ0い"},
		{name: "非数字开头（假名）", body: "あ"},
		{name: "非数字开头（拉丁字母）", body: "a"},
		{name: "前导空格", body: " 1あ"},
		{name: "含汉字", body: "1心"},
		{name: "假名后含汉字", body: "1あ心"},
		{name: "含拉丁字母", body: "1abc"},
		{name: "假名后含拉丁字母", body: "1あa"},
		{name: "词条间含空格", body: "1あ 1い"},
		{name: "含 Emoji（增补平面代理对）", body: "1🌸"},
		{name: "假名后含 Emoji", body: "1あ🌸"},
		{name: "带时间后含 Emoji", body: "1あ(10,20)🌸"},
		{name: "非法 UTF-8 字节跟在计数后", body: "1\xff"},
		{name: "非法 UTF-8 字节跟在假名后", body: "1あ\x80"},
		{name: "非法 UTF-8 字节开头", body: "\xfe1あ"},
		{name: "假名码点下界外 U+3040", body: "1\u3040"},
		{name: "假名码点上界外 U+30FF", body: "1\u30ff"},
		{name: "叠字符々不在假名读音范围", body: "1々"},
		{name: "括号缺逗号", body: "1あ(100 50)"},
		{name: "括号仅一个数字", body: "1あ(100)"},
		{name: "括号缺右括号", body: "1あ(100,50"},
		{name: "第二音节缺右括号", body: "1あ(100,50)し(200,30"},
		{name: "括号起始为空", body: "1あ(,50)"},
		{name: "括号时长为空", body: "1あ(100,)"},
		{name: "括号起始为负数", body: "1あ(-1,50)"},
		{name: "括号时长为负数", body: "1あ(100,-50)"},
		{name: "括号含非数字", body: "1あ(1a,50)"},
		{name: "括号含全角数字", body: "1あ(１０,２０)"},
		{name: "括号起始超 9 位整数", body: "1あ(1000000000,50)"},
		{name: "括号时长超 9 位整数", body: "1あ(100,1000000000)"},
		{name: "括号多余逗号", body: "1あ(100,50,10)"},
		{name: "连续双括号无假名", body: "1あ(100,50)(200,30)"},
		{name: "计数后直接跟括号", body: "1(100,50)"},
		{name: "同词条前无括号后带括号", body: "1あし(100,50)"},
		{name: "同词条前带括号后无括号（末尾）", body: "1あ(100,50)し"},
		{name: "同词条前带括号后无括号（后接新词条）", body: "1あ(100,50)し2い"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseKanaTag(tc.body)
			if ok || got != nil {
				t.Errorf("parseKanaTag(%q) = (%#v, %v)，want (nil, false)", tc.body, got, ok)
			}
		})
	}
}

func TestKanaCollectorHeaderAndCap(t *testing.T) {
	t.Run("常规行与大小写区分", func(t *testing.T) {
		var c kanaCollector
		for _, nonKana := range []string{
			"[ti:心のプラカード]",
			"[ar:AKB48]",
			"[0,100]心(0,50)",
			"[KANA:1こころ]",
			"plain text",
		} {
			if c.feed(nonKana) {
				t.Fatalf("feed(%q) = true，want false", nonKana)
			}
		}
		if c.hasTag() {
			t.Fatalf("未喂入 [kana:] 前 hasTag() = true，want false")
		}
		if !c.feed("[kana:1こころ]") {
			t.Fatalf("feed([kana:1こころ]) = false，want true")
		}
		if !c.hasTag() {
			t.Fatalf("喂入 [kana:] 后 hasTag() = false，want true")
		}
		got, ok, reason := c.entries()
		want := []kanaEntry{{count: 1, ruby: "こころ"}}
		if !ok || reason != "" || !reflect.DeepEqual(got, want) {
			t.Fatalf("entries() = (%#v, %v, %q)，want (%#v, true, \"\")", got, ok, reason, want)
		}
	})

	t.Run("剥除紧贴的 QRC XML LyricContent 头壳", func(t *testing.T) {
		var c kanaCollector
		if c.feed(`<Lyric_1 LyricType="1" LyricContent="[ti:title]`) {
			t.Fatalf("非 [kana:] 的 XML 头不应被消费")
		}
		if !c.feed(`<Lyric_1 LyricType="1" LyricContent="[kana:1こころ]`) {
			t.Fatalf("紧跟 LyricContent=\" 的 [kana:] 应被消费")
		}
		if !c.feed(`<?xml version="1.0"?><QrcInfos><Lyric_1 LyricType="1" LyricContent="[kana:1む]"/>`) {
			t.Fatalf("同行闭合的 XML [kana:] 应被消费")
		}
		got, ok, reason := c.entries()
		want := []kanaEntry{{count: 1, ruby: "こころ"}, {count: 1, ruby: "む"}}
		if !ok || reason != "" || !reflect.DeepEqual(got, want) {
			t.Fatalf("entries() = (%#v, %v, %q)，want (%#v, true, \"\")", got, ok, reason, want)
		}
	})

	t.Run("多行 [kana:] 顺序追加含空标签体", func(t *testing.T) {
		var c kanaCollector
		for _, line := range []string{
			"[kana:1こころ]",
			"[kana:]",
			"[kana:2あ(10,20)し(30,40)た(50,60)]",
		} {
			if !c.feed(line) {
				t.Fatalf("feed(%q) = false，want true", line)
			}
		}
		got, ok, reason := c.entries()
		want := []kanaEntry{
			{count: 1, ruby: "こころ"},
			{count: 2, ruby: "あした"},
		}
		if !ok || reason != "" || !reflect.DeepEqual(got, want) {
			t.Fatalf("entries() = (%#v, %v, %q)，want (%#v, true, \"\")", got, ok, reason, want)
		}
	})

	t.Run("缺右括号判无效并释放已收集切片", func(t *testing.T) {
		var c kanaCollector
		if !c.feed("[kana:1こころ]") {
			t.Fatal("首行合法 [kana:] 应返回 true")
		}
		if !c.feed("[kana:1む") {
			t.Fatal("缺右括号的 [kana:] 仍应消费该行并返回 true")
		}
		if !c.feed("[kana:1き]") {
			t.Fatal("已无效后的 [kana:] 仍应消费该行并返回 true")
		}
		got, ok, reason := c.entries()
		if ok || got != nil || reason == "" {
			t.Fatalf("entries() = (%#v, %v, %q)，want (nil, false, non-empty)", got, ok, reason)
		}
		if c.items != nil {
			t.Fatalf("置无效后应释放内部切片，实际 len=%d", len(c.items))
		}
	})

	t.Run("语法非法判无效并释放已收集切片", func(t *testing.T) {
		var c kanaCollector
		c.feed("[kana:1こころ]")
		c.feed("[kana:0む]")
		got, ok, reason := c.entries()
		if ok || got != nil || reason == "" {
			t.Fatalf("entries() = (%#v, %v, %q)，want (nil, false, non-empty)", got, ok, reason)
		}
		if c.items != nil {
			t.Fatalf("语法非法后应释放内部切片，实际 len=%d", len(c.items))
		}
	})

	t.Run("累计恰为 256 KiB 允许通过", func(t *testing.T) {
		var c kanaCollector
		half := strings.Repeat("1", maxKanaTagBytes/2)
		if !c.feed("[kana:" + half + "]") {
			t.Fatal("第一半应被消费")
		}
		if !c.feed("[kana:" + half + "]") {
			t.Fatal("第二半应被消费")
		}
		got, ok, reason := c.entries()
		if !ok || reason != "" || len(got) != maxKanaTagBytes {
			t.Fatalf("恰等于上限时 entries() = (len=%d, %v, %q)，want (len=%d, true, \"\")", len(got), ok, reason, maxKanaTagBytes)
		}
	})

	t.Run("累计 256 KiB + 1 判超限且不再增长内存", func(t *testing.T) {
		var c kanaCollector
		full := strings.Repeat("1", maxKanaTagBytes)
		c.feed("[kana:" + full + "]")
		if !c.feed("[kana:1]") {
			t.Fatal("超限行仍应被消费并返回 true")
		}
		got, ok, reason := c.entries()
		if ok || got != nil || reason == "" {
			t.Fatalf("超限后 entries() = (len=%d, %v, %q)，want (nil, false, non-empty)", len(got), ok, reason)
		}
		if c.items != nil {
			t.Fatalf("超限后应释放已收集切片，实际 len=%d", len(c.items))
		}
		bytesBefore := c.totalBytes
		// 继续喂入合法标签不得再分配或累积。
		c.feed("[kana:1こころ]")
		if c.items != nil || c.totalBytes != bytesBefore {
			t.Fatalf("已超限后不应再累积：items=%v totalBytes=%d (before=%d)", c.items, c.totalBytes, bytesBefore)
		}
	})
}

func FuzzParseKanaTag(f *testing.F) {
	f.Add("", "こっち向いて", "こっち", "向いて", "心")
	f.Add("1む1こころ", "こっち向いて", "こっち", "向いて", "心")
	f.Add("2あ(128976,624)し(129600,161)た(129761,159)1こころ", "明日", "明", "日", "心")
	f.Add("111111", "心意告示牌４８", "心意告示牌", "４８", "")
	f.Add("1む", `向"/>`, "向", "", "")
	f.Add("1こう1おつ", "甲乙", "甲", "", "丙")
	f.Add("1あし(100,50)", "向", "向", "", "")
	f.Add("1\xff", "向\x80", "向", "\x80", "🌸心")

	f.Fuzz(func(t *testing.T, body, line1Text, word1A, word1B, line2Text string) {
		var c kanaCollector
		c.feed(body)
		c.feed("[kana:" + body + "]")
		_, _, _ = c.entries()

		entries, ok := parseKanaTag(body)
		if !ok {
			if entries != nil {
				t.Fatalf("parseKanaTag(%q) ok=false 但返回非空切片 %#v", body, entries)
			}
			return
		}

		// 模式 1：LRC 逐行模式（Detailed.Words 为空）
		lrcLines := []lyricLine{
			{Index: 0, Text: line1Text},
			{Index: 1, Text: line2Text},
		}
		assertAttachInvariants(t, lrcLines, entries)

		// 模式 2：QRC 逐字模式
		qrcLines := []lyricLine{
			{
				Index: 0,
				Text:  line1Text,
				Detailed: player.LyricTextDetailed{
					Words: []player.LyricTextDetailedWord{
						{Text: word1A},
						{Text: word1B},
					},
				},
			},
			{
				Index: 1,
				Text:  line2Text,
				Detailed: player.LyricTextDetailed{
					Words: []player.LyricTextDetailedWord{
						{Text: line2Text},
					},
				},
			},
		}
		assertAttachInvariants(t, qrcLines, entries)
	})
}

func assertAttachInvariants(t *testing.T, lines []lyricLine, entries []kanaEntry) {
	t.Helper()
	ok, reason := attachFurigana(lines, entries)
	if !ok {
		if reason == "" {
			t.Fatalf("attachFurigana 返回 false 但 reason 为空")
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Fatalf("attachFurigana 失败时第 %d 行残留 Furigana = %#v", i, l.Furigana)
			}
		}
		return
	}
	for i, l := range lines {
		if len(l.Furigana) == 0 {
			if l.Furigana != nil {
				t.Fatalf("第 %d 行空 Furigana 应为 nil，实际 %#v", i, l.Furigana)
			}
			continue
		}
		var joined strings.Builder
		hasRuby := false
		for j, seg := range l.Furigana {
			if seg.Text == "" {
				t.Fatalf("第 %d 行第 %d 段出现空 Text：%#v", i, j, l.Furigana)
			}
			if seg.Ruby != "" {
				hasRuby = true
			} else if j > 0 && l.Furigana[j-1].Ruby == "" {
				t.Fatalf("第 %d 行第 %d 与 %d 段出现相邻无 Ruby 段：%#v", i, j-1, j, l.Furigana)
			}
			joined.WriteString(seg.Text)
		}
		if !hasRuby {
			t.Fatalf("第 %d 行非空 Furigana 不含任何 Ruby：%#v", i, l.Furigana)
		}
		full := joined.String()
		trimmed := strings.TrimSpace(full)
		if len(l.Detailed.Words) > 0 {
			var wb strings.Builder
			for _, w := range l.Detailed.Words {
				wb.WriteString(w.Text)
			}
			if full != wb.String() {
				t.Fatalf("第 %d 行 Furigana 拼接 %q != wordsText %q", i, full, wb.String())
			}
			if trimmed != l.Text {
				if !(strings.HasPrefix(l.Text, trimmed) && isQRCXMLClosingTail(l.Text[len(trimmed):])) {
					t.Fatalf("第 %d 行 TrimSpace(joined)=%q 与 l.Text=%q 不守恒且非合法 XML 尾壳", i, trimmed, l.Text)
				}
			}
		} else if trimmed != l.Text {
			t.Fatalf("第 %d 行（LRC）TrimSpace(joined)=%q != l.Text=%q", i, trimmed, l.Text)
		}
	}
}
