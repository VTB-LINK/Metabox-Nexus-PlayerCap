package qqmusic

// 本文件只测 attachFurigana：全曲守恒对齐、目标字符判定、QRC 尾壳与未打轴残留防错位。

import (
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

func wordsOf(texts ...string) player.LyricTextDetailed {
	words := make([]player.LyricTextDetailedWord, len(texts))
	for i, s := range texts {
		words[i] = player.LyricTextDetailedWord{Text: s}
	}
	return player.LyricTextDetailed{Words: words}
}

func mustParseKana(t *testing.T, body string) []kanaEntry {
	t.Helper()
	entries, ok := parseKanaTag(body)
	if !ok {
		t.Fatalf("parseKanaTag(%q) 失败", body)
	}
	return entries
}

// 变异自证（隔离副本，实测）：
//   - isKanaTarget 误纳半角数字 '0'..'9'：「半角 ASCII 数字不消耗游标」红；
//   - 删掉终点 cursor == len(entries) 守恒检查：TestAttachFuriganaCursorMismatchClearsAll「词条多 1 条」红；
//   - 删掉 QRC XML 尾壳剥除：TestAttachFuriganaQRCXMLClosingTail 4 条含字末行子例全红；
//   - 删掉未打轴尾巴目标子词元同构校验：TestAttachFuriganaUntimedTailSafety 前三条反例全红。
func TestAttachFuriganaCoreCases(t *testing.T) {
	t.Run("送假名切分（单 word 与分字一致）", func(t *testing.T) {
		entries := mustParseKana(t, "1む1む")
		lines := []lyricLine{
			{Index: 0, Text: "向いて", Detailed: wordsOf("向いて")},
			{Index: 1, Text: "向いて", Detailed: wordsOf("向", "い", "て")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := player.LyricFurigana{
			{Text: "向", Ruby: "む"},
			{Text: "いて"},
		}
		for i := range lines {
			if !reflect.DeepEqual(lines[i].Furigana, want) {
				t.Errorf("line %d Furigana = %#v，want %#v", i, lines[i].Furigana, want)
			}
		}
	})

	t.Run("熟字训 count=2（单 word 与跨双 word）与 count=3", func(t *testing.T) {
		entries := mustParseKana(t, "2あ(10,20)し(30,40)た(50,60)2あした3ペ(1,2)ザ(3,4)ー(5,6)ジ(7,8)ュ(9,10)")
		lines := []lyricLine{
			{Index: 0, Text: "明日", Detailed: wordsOf("明日")},
			{Index: 1, Text: "明日", Detailed: wordsOf("明", "日")},
			{Index: 2, Text: "風景画", Detailed: wordsOf("風", "景", "画")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := []player.LyricFurigana{
			{{Text: "明日", Ruby: "あした"}},
			{{Text: "明日", Ruby: "あした"}},
			{{Text: "風景画", Ruby: "ペザージュ"}},
		}
		for i := range lines {
			if !reflect.DeepEqual(lines[i].Furigana, want[i]) {
				t.Errorf("line %d Furigana = %#v，want %#v", i, lines[i].Furigana, want[i])
			}
		}
	})

	t.Run("叠字符日々与 CJK 基本区及扩展 A 区边界汉字", func(t *testing.T) {
		entries := mustParseKana(t, "1ひ1び1て1すと1いち1きゅう")
		// U+3400(㐀)、U+4DB5(䶵)、U+4E00(一)、U+9FBB(龻) 在目标范围内；
		// U+33FF、U+4DB6、U+4DFF、U+9FBC、U+FF0F、U+FF1A 在范围外不消耗游标。
		lines := []lyricLine{
			{Index: 0, Text: "日々", Detailed: wordsOf("日", "々")},
			{Index: 1, Text: "\u33ff\u3400\u4db6\u4db5\u4dff\u4e00\u9fbb\u9fbc\uff0f\uff1a"},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want0 := player.LyricFurigana{
			{Text: "日", Ruby: "ひ"},
			{Text: "々", Ruby: "び"},
		}
		want1 := player.LyricFurigana{
			{Text: "\u33ff"},
			{Text: "\u3400", Ruby: "て"},
			{Text: "\u4db6"},
			{Text: "\u4db5", Ruby: "すと"},
			{Text: "\u4dff"},
			{Text: "\u4e00", Ruby: "いち"},
			{Text: "\u9fbb", Ruby: "きゅう"},
			{Text: "\u9fbc\uff0f\uff1a"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want0) {
			t.Errorf("line 0 Furigana = %#v，want %#v", lines[0].Furigana, want0)
		}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("line 1 Furigana = %#v，want %#v", lines[1].Furigana, want1)
		}
	})

	t.Run("全角数字注音、多位全角数字跳过与多位全角数字带注音", func(t *testing.T) {
		entries := mustParseKana(t, "1ご1びょう1こころ12よんじゅうはち1ねん1よんじゅうはち2だいじゅう")
		lines := []lyricLine{
			{Index: 0, Text: "５秒", Detailed: wordsOf("５", "秒")},
			{Index: 1, Text: "心 - AKB４８", Detailed: wordsOf("心", " - ", "AKB", "４８")},
			{Index: 2, Text: "４８年", Detailed: wordsOf("４８", "年")},
			{Index: 3, Text: "４８", Detailed: wordsOf("４８")},
			{Index: 4, Text: "第１０", Detailed: wordsOf("第", "１０")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := []player.LyricFurigana{
			{{Text: "５", Ruby: "ご"}, {Text: "秒", Ruby: "びょう"}},
			{{Text: "心", Ruby: "こころ"}, {Text: " - AKB４８"}},
			{{Text: "４８", Ruby: "よんじゅうはち"}, {Text: "年", Ruby: "ねん"}},
			{{Text: "４８", Ruby: "よんじゅうはち"}},
			{{Text: "第１０", Ruby: "だいじゅう"}},
		}
		for i := range lines {
			if !reflect.DeepEqual(lines[i].Furigana, want[i]) {
				t.Errorf("line %d Furigana = %#v，want %#v", i, lines[i].Furigana, want[i])
			}
		}
	})

	t.Run("半角 ASCII 数字不消耗游标", func(t *testing.T) {
		entries := mustParseKana(t, "1いち")
		lines := []lyricLine{
			{Index: 0, Text: "123 １", Detailed: wordsOf("123", " ", "１")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := player.LyricFurigana{
			{Text: "123 "},
			{Text: "１", Ruby: "いち"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want) {
			t.Errorf("line 0 Furigana = %#v，want %#v", lines[0].Furigana, want)
		}
	})

	t.Run("LRC 逐行模式与 QRC 逐字模式（含词首尾空格保留）", func(t *testing.T) {
		entries := mustParseKana(t, "1む1いっ1しゅん1ぼく")
		lines := []lyricLine{
			{
				Index:    0,
				Text:     "こっち向いて 一瞬だけ",
				Detailed: wordsOf("  ", "こっち", "向いて", " ", "一", "瞬", "だけ", "  "),
			},
			{
				Index: 1,
				Text:  "だけどホントは",
			},
			{
				Index: 2,
				Text:  "僕だけ",
			},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want0 := player.LyricFurigana{
			{Text: "  こっち"},
			{Text: "向", Ruby: "む"},
			{Text: "いて "},
			{Text: "一", Ruby: "いっ"},
			{Text: "瞬", Ruby: "しゅん"},
			{Text: "だけ  "},
		}
		want2 := player.LyricFurigana{
			{Text: "僕", Ruby: "ぼく"},
			{Text: "だけ"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want0) {
			t.Errorf("line 0 Furigana = %#v，want %#v", lines[0].Furigana, want0)
		}
		if lines[1].Furigana != nil {
			t.Errorf("纯假名行 Furigana = %#v，want nil", lines[1].Furigana)
		}
		if !reflect.DeepEqual(lines[2].Furigana, want2) {
			t.Errorf("line 2 Furigana = %#v，want %#v", lines[2].Furigana, want2)
		}
	})

	t.Run("空词条列表直接成功；有词条无歌词行判失配", func(t *testing.T) {
		lines := []lyricLine{{Index: 0, Text: "心"}}
		if ok, reason := attachFurigana(lines, nil); !ok || reason != "" || lines[0].Furigana != nil {
			t.Fatalf("空词条 attachFurigana = (%v, %q, %#v)，want (true, \"\", nil)", ok, reason, lines[0].Furigana)
		}
		if ok, reason := attachFurigana(nil, mustParseKana(t, "1こころ")); ok || reason == "" {
			t.Fatalf("无行有词条 attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
	})
}

func TestAttachFuriganaQRCXMLClosingTail(t *testing.T) {
	t.Run(`末行紧贴 "/>`, func(t *testing.T) {
		entries := mustParseKana(t, "1こころ1む")
		origWords := wordsOf("向")
		lines := []lyricLine{
			{Index: 0, Text: "心", Detailed: wordsOf("心")},
			{Index: 1, Text: `向"/>`, Detailed: origWords},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want1 := player.LyricFurigana{{Text: "向", Ruby: "む"}}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("末行 Furigana = %#v，want %#v", lines[1].Furigana, want1)
		}
		if lines[1].Text != `向"/>` || !reflect.DeepEqual(lines[1].Detailed, origWords) {
			t.Errorf("末行原值被修改：Text=%q Detailed=%#v", lines[1].Text, lines[1].Detailed)
		}
	})

	t.Run(`末行紧贴 "/></LyricInfo>`, func(t *testing.T) {
		entries := mustParseKana(t, "1む")
		origWords := wordsOf("向")
		lines := []lyricLine{
			{Index: 0, Text: `向"/></LyricInfo>`, Detailed: origWords},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		if !reflect.DeepEqual(lines[0].Furigana, player.LyricFurigana{{Text: "向", Ruby: "む"}}) {
			t.Errorf("Furigana = %#v", lines[0].Furigana)
		}
		if lines[0].Text != `向"/></LyricInfo>` || !reflect.DeepEqual(lines[0].Detailed, origWords) {
			t.Errorf("原值被修改：Text=%q Detailed=%#v", lines[0].Text, lines[0].Detailed)
		}
	})

	t.Run(`末行紧贴 "/></LyricInfo></QrcInfos>`, func(t *testing.T) {
		entries := mustParseKana(t, "1こころ1む")
		origWords := wordsOf("向")
		lines := []lyricLine{
			{Index: 0, Text: "心", Detailed: wordsOf("心")},
			{Index: 1, Text: `向"/></LyricInfo></QrcInfos>`, Detailed: origWords},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		if !reflect.DeepEqual(lines[0].Furigana, player.LyricFurigana{{Text: "心", Ruby: "こころ"}}) {
			t.Errorf("首行 Furigana = %#v", lines[0].Furigana)
		}
		if !reflect.DeepEqual(lines[1].Furigana, player.LyricFurigana{{Text: "向", Ruby: "む"}}) {
			t.Errorf("末行 Furigana = %#v", lines[1].Furigana)
		}
		if lines[1].Text != `向"/></LyricInfo></QrcInfos>` || !reflect.DeepEqual(lines[1].Detailed, origWords) {
			t.Errorf("末行原值被修改：Text=%q Detailed=%#v", lines[1].Text, lines[1].Detailed)
		}
	})

	t.Run(`词尾带空格紧贴 "/>（向 (2000,500)"/>）`, func(t *testing.T) {
		entries := mustParseKana(t, "1む")
		origWords := wordsOf("向 ")
		lines := []lyricLine{
			{Index: 0, Text: `向 "/>`, Detailed: origWords},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := player.LyricFurigana{{Text: "向", Ruby: "む"}, {Text: " "}}
		if !reflect.DeepEqual(lines[0].Furigana, want) {
			t.Errorf("Furigana = %#v，want %#v", lines[0].Furigana, want)
		}
		if lines[0].Text != `向 "/>` || !reflect.DeepEqual(lines[0].Detailed, origWords) {
			t.Errorf("原值被修改：Text=%q Detailed=%#v", lines[0].Text, lines[0].Detailed)
		}
	})

	t.Run(`空白尾奏行紧贴 "/>（  (2000,500)"/>）`, func(t *testing.T) {
		entries := mustParseKana(t, "1こころ")
		origWords := wordsOf("  ")
		lines := []lyricLine{
			{Index: 0, Text: "心", Detailed: wordsOf("心")},
			{Index: 1, Text: `"/>`, Detailed: origWords},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		if !reflect.DeepEqual(lines[0].Furigana, player.LyricFurigana{{Text: "心", Ruby: "こころ"}}) {
			t.Errorf("首行 Furigana = %#v，want こころ", lines[0].Furigana)
		}
		if lines[1].Furigana != nil {
			t.Errorf("空白尾奏行 Furigana = %#v，want nil", lines[1].Furigana)
		}
		if lines[1].Text != `"/>` || !reflect.DeepEqual(lines[1].Detailed, origWords) {
			t.Errorf("空白尾奏行原值被修改：Text=%q Detailed=%#v", lines[1].Text, lines[1].Detailed)
		}
	})
}

func TestAttachFuriganaUntimedTailSafety(t *testing.T) {
	t.Run("未打轴汉字尾巴不得由后续多位全角数字吸收差额", func(t *testing.T) {
		entries := mustParseKana(t, "1こう1おつ2へいすう1てい")
		lines := []lyricLine{
			{Index: 0, Text: "甲乙", Detailed: wordsOf("甲")},
			{Index: 1, Text: "丙４８丁", Detailed: wordsOf("丙", "４８", "丁")},
		}
		ok, reason := attachFurigana(lines, entries)
		if ok || reason == "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("失配后第 %d 行残留 Furigana = %#v，want nil", i, l.Furigana)
			}
		}
	})

	t.Run("纯假名词后藏未打轴汉字判整曲失配", func(t *testing.T) {
		entries := mustParseKana(t, "1こころ")
		lines := []lyricLine{
			{Index: 0, Text: "あ乙", Detailed: wordsOf("あ")},
			{Index: 1, Text: "心", Detailed: wordsOf("心")},
		}
		ok, reason := attachFurigana(lines, entries)
		if ok || reason == "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("失配后第 %d 行残留 Furigana = %#v，want nil", i, l.Furigana)
			}
		}
	})

	t.Run("数字子词元被劈裂（４(0,50)8）判整曲失配", func(t *testing.T) {
		entries := mustParseKana(t, "1よん")
		lines := []lyricLine{
			{Index: 0, Text: "４8", Detailed: wordsOf("４")},
		}
		ok, reason := attachFurigana(lines, entries)
		if ok || reason == "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
		if lines[0].Furigana != nil {
			t.Errorf("失配后残留 Furigana = %#v，want nil", lines[0].Furigana)
		}
	})

	t.Run("纯假名未打轴尾巴目标子词元同构仅单行局部降级", func(t *testing.T) {
		entries := mustParseKana(t, "1む1こころ")
		lines := []lyricLine{
			{Index: 0, Text: "向いて", Detailed: wordsOf("向")},
			{Index: 1, Text: "心", Detailed: wordsOf("心")},
		}
		ok, reason := attachFurigana(lines, entries)
		wantReason := i18n.T("line text mismatch on some lines")
		if !ok || reason != wantReason {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, %q)", ok, reason, wantReason)
		}
		if lines[0].Furigana != nil {
			t.Errorf("第 0 行应局部降级为 nil，实际 %#v", lines[0].Furigana)
		}
		want1 := player.LyricFurigana{{Text: "心", Ruby: "こころ"}}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("第 1 行应保留注音 %#v，实际 %#v", want1, lines[1].Furigana)
		}
	})
}

func TestAttachFuriganaCursorMismatchClearsAll(t *testing.T) {
	cases := []struct {
		name    string
		entries []kanaEntry
		lines   []lyricLine
	}{
		{
			name:    "词条多 1 条",
			entries: mustParseKana(t, "1こころ1む1き"),
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
				{Index: 1, Text: "向", Detailed: wordsOf("向")},
			},
		},
		{
			name:    "词条少 1 条",
			entries: mustParseKana(t, "1こころ"),
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
				{Index: 1, Text: "向", Detailed: wordsOf("向")},
			},
		},
		{
			name:    "count=2 越过行尾",
			entries: mustParseKana(t, "1こころ2あした"),
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
				{Index: 1, Text: "明", Detailed: wordsOf("明")},
				{Index: 2, Text: "日", Detailed: wordsOf("日")},
			},
		},
		{
			name:    "count=2 试图跨越中间夹杂假名",
			entries: mustParseKana(t, "1こころ2きみのかお"),
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
				{Index: 1, Text: "君の顔", Detailed: wordsOf("君", "の", "顔")},
			},
		},
		{
			name:    "count=2 试图跨越空格",
			entries: mustParseKana(t, "1こころ2あした"),
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
				{Index: 1, Text: "明 日", Detailed: wordsOf("明", " ", "日")},
			},
		},
		{
			name:    "非法 count<=0 防御",
			entries: []kanaEntry{{count: 0, ruby: "こころ"}},
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
			},
		},
		{
			name:    "空跳过非法 count<=0 防御",
			entries: []kanaEntry{{count: 0}},
			lines: []lyricLine{
				{Index: 0, Text: "心", Detailed: wordsOf("心")},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := attachFurigana(tc.lines, tc.entries)
			if ok || reason == "" {
				t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
			}
			for i, l := range tc.lines {
				if l.Furigana != nil {
					t.Errorf("失配后第 %d 行残留半截 Furigana = %#v，want nil", i, l.Furigana)
				}
			}
		})
	}
}

func TestAttachFuriganaClearsStaleFuriganaOnFailureOrEmpty(t *testing.T) {
	makeAttached := func(t *testing.T) []lyricLine {
		t.Helper()
		lines := []lyricLine{
			{Index: 0, Text: "心", Detailed: wordsOf("心")},
			{Index: 1, Text: "向いて", Detailed: wordsOf("向", "い", "て")},
		}
		ok, reason := attachFurigana(lines, mustParseKana(t, "1こころ1む"))
		if !ok || reason != "" {
			t.Fatalf("预置对齐失败：(%v, %q)", ok, reason)
		}
		if lines[0].Furigana == nil || lines[1].Furigana == nil {
			t.Fatalf("预置对齐后两行应均含 Furigana：%#v", lines)
		}
		return lines
	}

	cases := []struct {
		name    string
		entries []kanaEntry
		wantOK  bool
	}{
		{name: "空词条清空已有注音", entries: nil, wantOK: true},
		{name: "空切片词条清空已有注音", entries: []kanaEntry{}, wantOK: true},
		{name: "终点守恒失败（词条多 1 条）清空已有注音", entries: mustParseKana(t, "1こころ1む1き"), wantOK: false},
		{name: "游标不足（词条少 1 条）清空已有注音", entries: mustParseKana(t, "1こころ"), wantOK: false},
		{name: "行内越界/跨假名失配清空已有注音", entries: mustParseKana(t, "1こころ2む"), wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := makeAttached(t)
			ok, reason := attachFurigana(lines, tc.entries)
			if ok != tc.wantOK {
				t.Fatalf("attachFurigana = (%v, %q)，want ok=%v", ok, reason, tc.wantOK)
			}
			if !tc.wantOK && reason == "" {
				t.Fatalf("失败返回时 reason 不应为空")
			}
			for i, l := range lines {
				if l.Furigana != nil {
					t.Errorf("再次调用后第 %d 行仍残留旧 Furigana = %#v，want nil", i, l.Furigana)
				}
			}
		})
	}
}

func TestAttachFuriganaLongPlainTextLinear(t *testing.T) {
	const plainLen = 64 << 10
	plain := strings.Repeat("a", plainLen)
	lineText := plain + "心"
	entries := mustParseKana(t, "1こころ")
	want := player.LyricFurigana{
		{Text: plain},
		{Text: "心", Ruby: "こころ"},
	}

	lines := []lyricLine{{Index: 0, Text: lineText, Detailed: wordsOf(lineText)}}
	ok, reason := attachFurigana(lines, entries)
	if !ok || reason != "" {
		t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
	}
	if !reflect.DeepEqual(lines[0].Furigana, want) {
		t.Fatalf("长纯文本行 Furigana 段数=%d，want %d", len(lines[0].Furigana), len(want))
	}

	allocs := testing.AllocsPerRun(1, func() {
		ls := []lyricLine{{Index: 0, Text: lineText, Detailed: wordsOf(lineText)}}
		if ok, reason := attachFurigana(ls, entries); !ok || len(ls[0].Furigana) != 2 {
			t.Fatalf("attachFurigana = (%v, %q), len=%d", ok, reason, len(ls[0].Furigana))
		}
	})
	if allocs > 200 {
		t.Fatalf("64 KiB 连续纯文本行对齐产生 %.0f 次分配，疑似平方级退化（上限 200）", allocs)
	}
}

func TestAttachFuriganaEmptySkipSpan(t *testing.T) {
	t.Run("空跳过 count=2 试图跨假名判整曲失配", func(t *testing.T) {
		entries := mustParseKana(t, "21ひ")
		lines := []lyricLine{
			{Index: 0, Text: "心の向"},
		}
		ok, reason := attachFurigana(lines, entries)
		if ok || reason == "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("失配后第 %d 行残留 Furigana = %#v，want nil", i, l.Furigana)
			}
		}
	})

	t.Run("空跳过 count=2 越过行尾判整曲失配", func(t *testing.T) {
		entries := mustParseKana(t, "2")
		lines := []lyricLine{
			{Index: 0, Text: "心"},
		}
		ok, reason := attachFurigana(lines, entries)
		if ok || reason == "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (false, non-empty)", ok, reason)
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("失配后第 %d 行残留 Furigana = %#v，want nil", i, l.Furigana)
			}
		}
	})

	t.Run("空跳过 count=2 连续跳过两汉字并保留后续注音", func(t *testing.T) {
		entries := mustParseKana(t, "21む")
		lines := []lyricLine{
			{Index: 0, Text: "明日向", Detailed: wordsOf("明", "日", "向")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		want := player.LyricFurigana{
			{Text: "明日"},
			{Text: "向", Ruby: "む"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want) {
			t.Errorf("Furigana = %#v，want %#v", lines[0].Furigana, want)
		}
	})

	t.Run("多位全角数字配空跳过 count=1 整体跳过不出错", func(t *testing.T) {
		entries := mustParseKana(t, "1")
		lines := []lyricLine{
			{Index: 0, Text: "AKB４８", Detailed: wordsOf("AKB", "４８")},
		}
		ok, reason := attachFurigana(lines, entries)
		if !ok || reason != "" {
			t.Fatalf("attachFurigana = (%v, %q)，want (true, \"\")", ok, reason)
		}
		if lines[0].Furigana != nil {
			t.Errorf("无注音行 Furigana = %#v，want nil", lines[0].Furigana)
		}
	})
}
