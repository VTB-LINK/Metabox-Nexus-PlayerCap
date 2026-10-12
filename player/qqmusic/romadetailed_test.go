package qqmusic

// 本文件只测逐字音译（QQ 的 roma 轨自带的字级时间 → roma_text_detailed）这条链：mergeRoma 把配上的
// 那一行音译的字级单元带过来、对不上约定的行只留行级，applyDetailedOffset 套 offset，toLyricLines 与
// lineUpdate 的接线。行级音译（roma_text）的用例在 roma_test.go，沿用那里的 jpQRC / jpRoma / jpTrans。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func mergedLines(t *testing.T, roma string) []lyricLine {
	t.Helper()
	lines, _, _ := parseLRC(jpQRC, 0)
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3；jpQRC 样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeTrans(lines, jpTrans)
	mergeRoma(lines, roma)
	return lines
}

// mainUntouched 断言主歌词、翻译与主歌词的逐字都和没合并音译时一样：音译是纯增量。
func mainUntouched(t *testing.T, lines []lyricLine) {
	t.Helper()
	want, _, _ := parseLRC(jpQRC, 0)
	mergeTrans(want, jpTrans)
	for i := range want {
		got := lines[i]
		if got.Index != want[i].Index || got.Time != want[i].Time || got.Text != want[i].Text ||
			got.SubText != want[i].SubText || !reflect.DeepEqual(got.Detailed, want[i].Detailed) {
			t.Fatalf("行%d：主歌词、翻译或主歌词的逐字被动了\n got %+v\nwant %+v", i, got, want[i])
		}
	}
}

// 正常行：逐字音译的单元、起始与时长都是音译轨自己的（jpRoma 的行首较主歌词偏 2~7ms，字的切分也与
// 主歌词不同），行级的起始与时长取音译轨那一行的行头；各单元拼起来去掉首尾空白等于 roma_text。
//
// 变异自证（隔离的 worktree，实测）：删掉 mergeRoma 里给 RomaDetailed 赋值的那一行，本例没有逐字
// 音译而红；把 BuildRomaDetailed 的第二个实参换成主歌词的行文本 lines[i].Text，拼接与它不等、
// 各行都是零值而红。
func TestMergeRomaCarriesDetailed(t *testing.T) {
	lines := mergedLines(t, jpRoma)
	want := []player.LyricTextDetailed{
		{Timestamp: 0.002, Duration: 3, Words: []player.LyricTextDetailedWord{
			{Timestamp: 0.002, Duration: 1, Text: "kimi"}, {Timestamp: 1.002, Duration: 1, Text: "no"}, {Timestamp: 2.002, Duration: 1, Text: "koe"}}},
		{Timestamp: 3.007, Duration: 3, Words: []player.LyricTextDetailedWord{
			{Timestamp: 3.007, Duration: 1, Text: "tooku"}, {Timestamp: 5.007, Duration: 1, Text: "de"}}},
		{Timestamp: 5.995, Duration: 3, Words: []player.LyricTextDetailedWord{
			{Timestamp: 5.995, Duration: 1.5, Text: "hibiku"}, {Timestamp: 7.495, Duration: 1.5, Text: "yo"}}},
	}
	for i := range want {
		if !reflect.DeepEqual(lines[i].RomaDetailed, want[i]) {
			t.Fatalf("行%d 逐字音译：\n got %+v\nwant %+v", i, lines[i].RomaDetailed, want[i])
		}
		var joined strings.Builder
		for _, w := range lines[i].RomaDetailed.Words {
			joined.WriteString(w.Text)
		}
		if strings.TrimSpace(joined.String()) != lines[i].RomaText {
			t.Fatalf("行%d：各单元拼起来 %q 不等于 roma_text %q", i, joined.String(), lines[i].RomaText)
		}
	}
	// 单元数与主歌词的字数不必相等：行 1 主歌词 3 个字、音译 2 个单元。
	if len(lines[1].Detailed.Words) != 3 || len(lines[1].RomaDetailed.Words) != 2 {
		t.Fatalf("行1：主歌词 %d 字、音译 %d 单元，want 3 / 2", len(lines[1].Detailed.Words), len(lines[1].RomaDetailed.Words))
	}
	mainUntouched(t, lines)
}

// 单元的文本原样保留：词间的空格留在单元里，只含空格的单元单独成一个；文本为空的不成单元。
func TestMergeRomaDetailedKeepsSpaces(t *testing.T) {
	lines := mergedLines(t, "[0,3000]ki (0,1000)mi(1000,500) (1500,500)(1800,100)no (2000,1000)\n"+
		"[3000,3000]too (3000,1000)ku(4000,1000)")
	if lines[0].RomaText != "ki mi no" {
		t.Fatalf("行0 roma_text = %q, want %q", lines[0].RomaText, "ki mi no")
	}
	var texts []string
	for _, w := range lines[0].RomaDetailed.Words {
		texts = append(texts, w.Text)
	}
	if want := []string{"ki ", "mi", " ", "no "}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("行0 单元 = %q, want %q", texts, want)
	}
	if w := lines[0].RomaDetailed.Words[2]; w.Timestamp != 1.5 || w.Duration != 0.5 {
		t.Fatalf("行0 空格单元的时间 = %+v, want 1.5 / 0.5", w)
	}
	if len(lines[1].RomaDetailed.Words) != 2 || lines[1].RomaDetailed.Words[0].Text != "too " {
		t.Fatalf("行1 单元 = %+v", lines[1].RomaDetailed.Words)
	}
	mainUntouched(t, lines)
}

// 拿不到逐字音译、只留行级的各种行：roma_text 照常，RomaDetailed 为零值，主歌词与翻译不变。
//
// 变异自证（隔离的 worktree，实测）：mergeRoma 里不经 BuildRomaDetailed、直接
// `lines[i].RomaDetailed = romaLines[best].Detailed`，「末尾有不带时间戳的残留」与「字时间回退」
// 两例带着对不上、回退的单元放行而红；player.BuildRomaDetailed 里的回退判定改成恒假，「字时间回退」红。
func TestMergeRomaDetailedLineLevelOnly(t *testing.T) {
	cases := []struct {
		name, roma, wantRoma string
	}{
		// 音译轨是普通 LRC：没有字级时间。
		{"普通 LRC", "[00:00.00]kiminokoe", "kiminokoe"},
		// 行文本含最后一个时间戳之后的残留，字级单元不含：拼起来比 roma_text 少一截。
		{"末尾有不带时间戳的残留", "[0,3000]kimi(0,1000)no(1000,1000)koe", "kiminokoe"},
		// 字的起始在行内回退。
		{"字时间回退", "[0,3000]kimi(1000,1000)no(0,1000)koe(2000,1000)", "kiminokoe"},
		// 整行只有行头与文本、没有字级时间戳。
		{"没有字级时间戳", "[0,3000]kiminokoe", "kiminokoe"},
	}
	for _, c := range cases {
		lines := mergedLines(t, c.roma)
		if lines[0].RomaText != c.wantRoma {
			t.Fatalf("%s：roma_text = %q, want %q（行级音译不应受逐字的判定影响）", c.name, lines[0].RomaText, c.wantRoma)
		}
		if d := lines[0].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.Duration != 0 {
			t.Errorf("%s：逐字音译应为零值，got %+v", c.name, d)
		}
		mainUntouched(t, lines)
	}
}

// 没配上音译的行没有逐字音译：超出容差、占位符 "//"（即便它是带字级时间的 QRC 行）、QQ 自家文案、
// 没有音译轨。
//
// 变异自证（隔离的 worktree，实测）：把给 RomaDetailed 赋值的那一行挪到 transIsDroppable 的判断之前，
// 「QRC 形态的占位符」带着两个 "/" 单元放行而红。
func TestMergeRomaDetailedAbsentWhenNoRomaText(t *testing.T) {
	for name, roma := range map[string]string{
		"超出容差":        "[500,3000]zura(500,1000)shita(1500,1000)",
		"QRC 形态的占位符":  "[0,3000]/(0,500)/(500,500)",
		"LRC 形态的占位符":  "[00:00.00]//",
		"QQ 自家文案":     "[0,3000]QQ音乐享有(0,500)本翻译作品的著作权(500,500)",
		"没有音译轨":       "",
		"解析不出行的音译轨":   "[ti:テスト]",
		"行首时间差一位的音译轨": "[21,3000]kimi(21,1000)",
	} {
		lines := mergedLines(t, roma)
		if lines[0].RomaText != "" {
			t.Fatalf("%s：用例前提不成立，行0 配上了 roma_text %q", name, lines[0].RomaText)
		}
		if d := lines[0].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.Duration != 0 {
			t.Fatalf("%s：没有 roma_text 的行不应有逐字音译，got %+v", name, d)
		}
		mainUntouched(t, lines)
	}
}

// 同一行音译配给两行主歌词时（mergeRoma 是就近匹配，不是一对一），两行各有一份逐字音译，互不共享
// 底层数组：给其中一行套 offset、改单元，另一行不变。
//
// 变异自证（隔离的 worktree，实测）：mergeRoma 里不经 BuildRomaDetailed、直接赋
// romaLines[best].Detailed，两行共用一个 Words，本例红。
func TestMergeRomaDetailedNotSharedBetweenLines(t *testing.T) {
	lines, _, _ := parseLRC("[1000,500]あ(1000,500)\n[1010,500]い(1010,500)", 0)
	mergeRoma(lines, "[1005,500]a(1005,500)")
	if len(lines) != 2 || len(lines[0].RomaDetailed.Words) != 1 || len(lines[1].RomaDetailed.Words) != 1 {
		t.Fatalf("用例前提不成立：两行都应配上同一行音译，got %+v", lines)
	}
	lines[0].RomaDetailed.Words[0].PlayTime = 99
	lines[0].RomaDetailed.Words[0].Text = "改了"
	if w := lines[1].RomaDetailed.Words[0]; w.PlayTime != 0 || w.Text != "a" {
		t.Fatalf("改一行的逐字音译影响到了另一行：%+v", w)
	}
}

// offset 套到逐字音译的 play_time（行级与各单元），正负各一例，负到 0 以下时钳到 0；主歌词的逐字
// 同样套，两条时间轴互不影响。
//
// 变异自证（隔离的 worktree，实测）：applyDetailedOffset 里去掉 `&lines[i].RomaDetailed`，逐字音译的
// play_time 仍是 0 而红。
func TestApplyDetailedOffsetCoversRomaDetailed(t *testing.T) {
	for _, off := range []float32{0.25, -0.25, -10} {
		lines := mergedLines(t, jpRoma)
		applyDetailedOffset(lines, off)
		for i := range lines {
			for _, d := range []player.LyricTextDetailed{lines[i].Detailed, lines[i].RomaDetailed} {
				if len(d.Words) == 0 {
					t.Fatalf("行%d：用例前提不成立，两条时间轴都应有单元", i)
				}
				if got, want := d.PlayTime, player.AdjustLyricPlayTime(d.Timestamp, off); got != want {
					t.Fatalf("offset %v 行%d：行级 play_time = %v, want %v", off, i, got, want)
				}
				for j, w := range d.Words {
					if got, want := w.PlayTime, player.AdjustLyricPlayTime(w.Timestamp, off); got != want {
						t.Fatalf("offset %v 行%d 单元%d：play_time = %v, want %v", off, i, j, got, want)
					}
				}
			}
		}
		// 具体取值：行 2 第二个单元的时间戳 7.495。
		got := lines[2].RomaDetailed.Words[1].PlayTime
		switch {
		case off > 0 && !(got > 7.495):
			t.Fatalf("正 offset 应把 play_time 延后，got %v", got)
		case off == -0.25 && !(got > 0 && got < 7.495):
			t.Fatalf("负 offset 应把 play_time 提前，got %v", got)
		case off == -10 && got != 0:
			t.Fatalf("play_time 的下限是 0，got %v", got)
		}
	}
	// 改逐字音译不影响主歌词的逐字。
	lines := mergedLines(t, jpRoma)
	applyDetailedOffset(lines, 0.5)
	lines[0].RomaDetailed.Words[0].PlayTime = 99
	if got := lines[0].Detailed.Words[0].PlayTime; got != 0.5 {
		t.Fatalf("改逐字音译的 play_time 影响到了主歌词的逐字：%v", got)
	}
}

// 接线：all_lyrics 的行与 lyric_update 都带逐字音译，取自同一份歌词、内容相同；没有逐字音译的行
// 两处都是零值、序列化为 {}。
//
// 变异自证（隔离的 worktree，实测）：删掉 toLyricLines 里的 `out[i].RomaTextDetailed = l.RomaDetailed`，
// all_lyrics 的行没有逐字音译而红；删掉 lineUpdate 里的 `u.RomaTextDetailed = l.RomaDetailed`，
// lyric_update 没有而红。
func TestLyricWireRomaTextDetailed(t *testing.T) {
	// 行 0、2 有逐字音译；行 1 的音译末尾有残留，只有行级。
	lines := mergedLines(t, "[2,3000]kimi(2,1000)no(1002,1000)koe(2002,1000)\n"+
		"[3007,3000]tooku(3007,1000)de\n"+
		"[5995,3000]hibiku(5995,1500)yo(7495,1500)")
	applyDetailedOffset(lines, -0.25)
	items := toLyricLines(lines, -0.25)

	if d := items[0].RomaTextDetailed; len(d.Words) != 3 || d.Words[2].Text != "koe" || d.Words[2].Timestamp != 2.002 ||
		d.PlayTime != player.AdjustLyricPlayTime(0.002, -0.25) || d.Words[2].PlayTime != player.AdjustLyricPlayTime(2.002, -0.25) {
		t.Fatalf("all_lyrics 行0 的 roma_text_detailed 不对：%+v", d)
	}
	if d := items[1].RomaTextDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.PlayTime != 0 {
		t.Fatalf("all_lyrics 行1：只有行级音译，roma_text_detailed 应为零值，got %+v", d)
	}
	if items[1].RomaText != "tookude" || items[1].SubText != "在远方" || len(items[1].TextDetailed.Words) != 3 {
		t.Fatalf("all_lyrics 行1：行级音译、翻译与主歌词的逐字应照常，got %+v", items[1])
	}
	for i := range lines {
		u := lineUpdate(i, lines[i], -0.25, 4.2, 9)
		if !reflect.DeepEqual(u.RomaTextDetailed, items[i].RomaTextDetailed) {
			t.Fatalf("行%d：lyric_update 与 all_lyrics 的 roma_text_detailed 不同源\n lyric_update %+v\n all_lyrics   %+v", i, u.RomaTextDetailed, items[i].RomaTextDetailed)
		}
		if u.Index != i || u.Text != items[i].Text || u.SubText != items[i].SubText || u.RomaText != items[i].RomaText ||
			u.PlayTime != items[i].PlayTime || !reflect.DeepEqual(u.TextDetailed, items[i].TextDetailed) || u.Position != 4.2 {
			t.Fatalf("行%d：lyric_update 其余字段应与 all_lyrics 的行一致，got %+v", i, u)
		}
	}
	b, err := json.Marshal(&player.AllLyricsData{Lyrics: items, Count: len(items)})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Count(s, `"roma_text_detailed":{"timestamp":`) != 2 || strings.Count(s, `"roma_text_detailed":{}`) != 1 {
		t.Fatalf("wire 上应有 2 行带逐字音译、1 行为 {}：%s", s)
	}
}

// 没有音译的歌词（老接口只回主歌词与翻译、不带音译）：逐字音译全为零值。
func TestRomaDetailedZeroWithoutRoma(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeTrans(lines, jpTrans)
	applyDetailedOffset(lines, 0.3)
	for i, it := range toLyricLines(lines, 0.3) {
		if len(it.RomaTextDetailed.Words) != 0 || it.RomaTextDetailed.PlayTime != 0 || it.RomaText != "" {
			t.Fatalf("行%d：没有音译时 roma_text 与 roma_text_detailed 都应为空，got %+v", i, it)
		}
	}
}

// 音译解不开（是密文但坏了）时整条跳过：逐字音译同样没有，主歌词与翻译不变。
func TestAttachRomaBadCiphertextLeavesNoRomaDetailed(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeTrans(lines, jpTrans)
	attachRoma(lines, strings.Repeat("ab", 64), 1)
	for i := range lines {
		if lines[i].RomaText != "" || len(lines[i].RomaDetailed.Words) != 0 {
			t.Fatalf("行%d：解不开的音译不应留下任何东西，got roma=%q detailed=%+v", i, lines[i].RomaText, lines[i].RomaDetailed)
		}
	}
	mainUntouched(t, lines)
}
