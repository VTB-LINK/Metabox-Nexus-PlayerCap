package player

// 本文件只测 BuildFurigana：对候选振假名片段的规范化（剔空、合并相邻纯文本）、文本守恒校验与切片隔离。

import (
	"reflect"
	"strings"
	"testing"
)

// 变异自证（隔离副本，实测）：将分配改为复用入参底层数组 LyricFurigana(segments[:0])，「返回切片与入参底层数组隔离」红。
func TestBuildFuriganaNormalizesAndValidates(t *testing.T) {
	t.Run("剔除空段、合并相邻纯文本并保留词首词尾空格", func(t *testing.T) {
		in := []LyricFuriganaSegment{
			{Text: " "},
			{Text: ""},
			{Text: " "},
			{Text: "向", Ruby: "む"},
			{Text: ""},
			{Text: "", Ruby: "無視"},
			{Text: "い"},
			{Text: "て"},
			{Text: "  "},
		}
		got := BuildFurigana(in, "向いて", "  向いて  ")
		want := LyricFurigana{
			{Text: "  "},
			{Text: "向", Ruby: "む"},
			{Text: "いて  "},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildFurigana() = %#v，want %#v", got, want)
		}
	})

	t.Run("无逐字歌词时保留相邻注音段", func(t *testing.T) {
		in := []LyricFuriganaSegment{
			{Text: "こっ"},
			{Text: "ち"},
			{Text: "向", Ruby: "む"},
			{Text: "い"},
			{Text: "て"},
			{Text: " "},
			{Text: "一", Ruby: "いっ"},
			{Text: "瞬", Ruby: "しゅん"},
			{Text: "だけ"},
		}
		got := BuildFurigana(in, "こっち向いて 一瞬だけ", "")
		want := LyricFurigana{
			{Text: "こっち"},
			{Text: "向", Ruby: "む"},
			{Text: "いて "},
			{Text: "一", Ruby: "いっ"},
			{Text: "瞬", Ruby: "しゅん"},
			{Text: "だけ"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildFurigana() = %#v，want %#v", got, want)
		}
	})

	t.Run("返回切片与入参底层数组隔离", func(t *testing.T) {
		in := []LyricFuriganaSegment{
			{Text: "向", Ruby: "む"},
			{Text: "いて"},
		}
		got := BuildFurigana(in, "向いて", "向いて")
		want := LyricFurigana{
			{Text: "向", Ruby: "む"},
			{Text: "いて"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildFurigana() = %#v，want %#v", got, want)
		}
		in[0].Text = "改"
		in[0].Ruby = "かい"
		in[1].Text = "変"
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("修改入参影响了返回值：got = %#v，want %#v", got, want)
		}
		got[0].Text = "別"
		if in[0].Text != "改" {
			t.Fatalf("修改返回值影响了入参：in = %#v", in)
		}
	})
}

// 变异自证（隔离副本，实测）：去掉 !hasRuby 判定，「全无 Ruby 片段」「仅空 Text 片段带 Ruby」红；
// 去掉 wordsText 逐字节相等校验，三条 wordsText 非空子例全红。
func TestBuildFuriganaRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name      string
		segments  []LyricFuriganaSegment
		lineText  string
		wordsText string
	}{
		{
			name:     "nil 片段",
			segments: nil,
			lineText: "向いて",
		},
		{
			name:     "空切片",
			segments: []LyricFuriganaSegment{},
			lineText: "向いて",
		},
		{
			name: "全无 Ruby 片段",
			segments: []LyricFuriganaSegment{
				{Text: "だけど"},
				{Text: "ホントは"},
			},
			lineText:  "だけどホントは",
			wordsText: "だけどホントは",
		},
		{
			name: "仅空 Text 片段带 Ruby",
			segments: []LyricFuriganaSegment{
				{Text: "", Ruby: "む"},
				{Text: "いて"},
			},
			lineText: "いて",
		},
		{
			name: "lineText 为空",
			segments: []LyricFuriganaSegment{
				{Text: "向", Ruby: "む"},
				{Text: "いて"},
			},
			lineText:  "",
			wordsText: "向いて",
		},
		{
			name: "TrimSpace(joined) 与 lineText 不等",
			segments: []LyricFuriganaSegment{
				{Text: "向", Ruby: "む"},
				{Text: "い"},
			},
			lineText: "向いて",
		},
		{
			name: "lineText 自带前导空格导致 TrimSpace 不等",
			segments: []LyricFuriganaSegment{
				{Text: " "},
				{Text: "向", Ruby: "む"},
				{Text: "いて"},
			},
			lineText:  " 向いて",
			wordsText: " 向いて",
		},
		{
			name: "wordsText 非空且缺前导空格",
			segments: []LyricFuriganaSegment{
				{Text: "向", Ruby: "む"},
				{Text: "いて"},
			},
			lineText:  "向いて",
			wordsText: " 向いて",
		},
		{
			name: "wordsText 非空且缺尾随空格",
			segments: []LyricFuriganaSegment{
				{Text: "向", Ruby: "む"},
				{Text: "いて"},
			},
			lineText:  "向いて",
			wordsText: "向いて ",
		},
		{
			name: "wordsText 非空且空白个数不同",
			segments: []LyricFuriganaSegment{
				{Text: " "},
				{Text: "向", Ruby: "む"},
				{Text: "いて "},
			},
			lineText:  "向いて",
			wordsText: "  向いて  ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildFurigana(tc.segments, tc.lineText, tc.wordsText); got != nil {
				t.Errorf("BuildFurigana() = %#v，want nil", got)
			}
		})
	}
}

func TestBuildFuriganaMergesLongPlainRunsLinearly(t *testing.T) {
	const plainCount = 32768
	segs := make([]LyricFuriganaSegment, plainCount+1)
	for i := 0; i < plainCount; i++ {
		segs[i] = LyricFuriganaSegment{Text: "a"}
	}
	segs[plainCount] = LyricFuriganaSegment{Text: "心", Ruby: "こころ"}

	plainText := strings.Repeat("a", plainCount)
	fullText := plainText + "心"
	want := LyricFurigana{
		{Text: plainText},
		{Text: "心", Ruby: "こころ"},
	}

	got := BuildFurigana(segs, fullText, fullText)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildFurigana(长连续纯文本) 段数=%d，want %d", len(got), len(want))
	}

	// 逐次 += 会产生约 plainCount 次字符串分配；线性合并应控制在常量/对数级。
	allocs := testing.AllocsPerRun(1, func() {
		if res := BuildFurigana(segs, fullText, fullText); len(res) != 2 {
			t.Fatalf("BuildFurigana len = %d，want 2", len(res))
		}
	})
	if allocs > 200 {
		t.Fatalf("BuildFurigana 合并 %d 个单字符纯文本段产生 %.0f 次分配，疑似平方级退化（上限 200）", plainCount, allocs)
	}
}
