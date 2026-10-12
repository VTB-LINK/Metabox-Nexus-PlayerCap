package krc

// 本文件只测逐字音译 Line.RomaDetailed 的构造（romaDetailed）：type=0 轨的逐字片段配上主歌词
// 同一位置那个字的时间。片段数与字数不符、没有音译轨、行级音译为空、时间轴不合法的行为零值，
// 且任何情形下主歌词、翻译、行级音译与 RomaWords 都不变。
//
// 样例为自拟的短句，写法同 romawords_test.go（mkLangTagWords 造 [language:] 头部）。

import (
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// 正常行：第 j 个单元 = 片段 j 的原文 + 主歌词第 j 个字的起始与时长；行级取主歌词这一行的。
// PlayTime 此处不填（offset 由播放器套）。
//
// 变异自证（隔离的 worktree，实测）：把 ParsePlainKRC 里的 `RomaDetailed: romaDetailed(...)` 删掉，
// 本例没有逐字音译而红；romaDetailed 里单元的时间一律取 detailed.Words[0] 的，第二个单元的起始不对
// 而红；单元的文本写成 strings.TrimSpace(frags[j])，拼起来成了 kimidakewo、与行级音译不等，整行为
// 零值而红；行级的 Timestamp / Duration 不取 detailed 的（留零），行级的断言红。
func TestRomaDetailedPairsFragmentsWithWords(t *testing.T) {
	roma := [][]string{
		{"kimi ", "da ", "ke ", "wo "},
		{"mi ", "tsu ", "me "},
	}
	lines := ParsePlainKRC(mkLangTagWords([]string{"只有你", "凝视"}, roma) + "\n" +
		"[1000,1200]<0,250,0>君<250,250,0>だ<500,300,0>け<800,400,0>を\n" +
		"[5000,0]<0,333,0>見<333,333,0>つ<666,334,0>め\n")
	if len(lines) != 2 {
		t.Fatalf("行数=%d, want 2", len(lines))
	}
	for i, l := range lines {
		d := l.RomaDetailed
		if len(d.Words) != len(roma[i]) {
			t.Fatalf("行%d：逐字音译应有 %d 个单元，got %+v", i, len(roma[i]), d.Words)
		}
		if d.Timestamp != l.Detailed.Timestamp || d.Duration != l.Detailed.Duration {
			t.Fatalf("行%d：行级起始/时长 = %v/%v，want 主歌词这一行的 %v/%v", i, d.Timestamp, d.Duration, l.Detailed.Timestamp, l.Detailed.Duration)
		}
		if d.PlayTime != 0 {
			t.Fatalf("行%d：PlayTime 此处不应填，got %v", i, d.PlayTime)
		}
		var joined strings.Builder
		for j, w := range d.Words {
			main := l.Detailed.Words[j]
			if w.Text != roma[i][j] || w.Timestamp != main.Timestamp || w.Duration != main.Duration || w.PlayTime != 0 {
				t.Fatalf("行%d 单元%d = %+v，want 文本 %q、时间取主歌词的字 %+v", i, j, w, roma[i][j], main)
			}
			joined.WriteString(w.Text)
		}
		if strings.TrimSpace(joined.String()) != l.RomaText {
			t.Fatalf("行%d：各单元拼起来去掉首尾空白 = %q，应等于行级音译 %q", i, strings.TrimSpace(joined.String()), l.RomaText)
		}
	}
	// 具体取值钉一处：第 0 行第 2 个单元 = "ke "，起始 1000+500ms、时长 300ms；行级 1.0s / 1.2s。
	// 第 1 行的行时长为 0，主歌词以字级时长求和兜底，逐字音译沿用同一个值。
	if w := lines[0].RomaDetailed.Words[2]; w.Text != "ke " || w.Timestamp != 1.5 || w.Duration != 0.3 {
		t.Fatalf("行0 单元2 = %+v，want ke / 1.5 / 0.3", w)
	}
	if d := lines[0].RomaDetailed; d.Timestamp != 1.0 || d.Duration != 1.2 {
		t.Fatalf("行0 行级 = %v/%v，want 1.0/1.2", d.Timestamp, d.Duration)
	}
	if d := lines[1].RomaDetailed; d.Duration != lines[1].Detailed.Duration || d.Duration == 0 {
		t.Fatalf("行1 行级时长 = %v，want 与主歌词相同的字级时长之和 %v", d.Duration, lines[1].Detailed.Duration)
	}
}

// 逐字音译的 Words 与主歌词的 Words 不共享底层数组：播放器给两条时间轴各套一次 offset，对外各是
// 一个字段，改一个不能影响另一个。
func TestRomaDetailedDoesNotShareWordsWithMain(t *testing.T) {
	lines := ParsePlainKRC(mkLangTagWords(nil, [][]string{{"ki ", "mi "}}) + "\n" +
		"[0,1000]<0,500,0>き<500,500,0>み\n")
	l := &lines[0]
	if len(l.RomaDetailed.Words) != 2 {
		t.Fatalf("前提不成立：应有 2 个单元，got %+v", l.RomaDetailed.Words)
	}
	l.RomaDetailed.Words[0].PlayTime = 99
	l.RomaDetailed.Words[1].Text = "改了"
	if l.Detailed.Words[0].PlayTime != 0 || l.Detailed.Words[1].Text != "み" {
		t.Fatalf("改逐字音译影响到了主歌词的逐字：%+v", l.Detailed.Words)
	}
	l.Detailed.Words[0].Timestamp = 77
	if l.RomaDetailed.Words[0].Timestamp != 0 {
		t.Fatalf("改主歌词的逐字影响到了逐字音译：%+v", l.RomaDetailed.Words)
	}
	if l.RomaWords[1] != "mi " {
		t.Fatalf("改逐字音译影响到了 RomaWords：%#v", l.RomaWords)
	}
}

// 片段数与字数不符的行只留行级：每行一个片段的轨（老 KRC）、片段多于字、片段少于字，都不臆测配对。
// 同一份歌词里片段数相符的行照常有逐字音译。
//
// 变异自证（隔离的 worktree，实测）：把 romaDetailed 的 `len(frags) != len(detailed.Words)` 改成
// 「按较短的一边配对」（不等也配），行 0 的单片段 "kimidakewo" 被配到第一个字上、拼接恰与行级音译
// 相等而放行，本例红；只去掉这道判断，没有片段的行下标越界 panic。
func TestRomaDetailedFragmentCountMismatch(t *testing.T) {
	roma := [][]string{
		{"kimidakewo"},                       // 行0：1 片段对 4 字
		{"mi ", "tsu ", "me ", "te ", "ru "}, // 行1：5 片段对 3 字
		{"te "},                              // 行2：1 片段对 2 字
		{"so ", "ra "},                       // 行3：2 片段对 2 字，相符
		{},                                   // 行4：没有片段
	}
	lines := ParsePlainKRC(mkLangTagWords(nil, roma) + "\n" +
		"[0,1000]<0,250,0>君<250,250,0>だ<500,250,0>け<750,250,0>を\n" +
		"[1000,1000]<0,333,0>見<333,333,0>つ<666,334,0>め\n" +
		"[2000,1000]<0,500,0>て<500,500,0>る\n" +
		"[3000,1000]<0,500,0>そ<500,500,0>ら\n" +
		"[4000,1000]<0,500,0>う<500,500,0>み\n")
	if len(lines) != 5 {
		t.Fatalf("行数=%d, want 5", len(lines))
	}
	for _, i := range []int{0, 1, 2, 4} {
		if d := lines[i].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.Duration != 0 {
			t.Fatalf("行%d：片段数与字数不符，逐字音译应为零值，got %+v", i, d)
		}
	}
	// 行级音译照常：片段数不符只是没有逐字。
	wantRoma := []string{"kimidakewo", "mi tsu me te ru", "te", "so ra", ""}
	for i, w := range wantRoma {
		if lines[i].RomaText != w {
			t.Fatalf("行%d RomaText=%q, want %q（行级音译不应受逐字的判定影响）", i, lines[i].RomaText, w)
		}
	}
	if d := lines[3].RomaDetailed; len(d.Words) != 2 || d.Words[1].Text != "ra " || d.Words[1].Timestamp != 3.5 {
		t.Fatalf("行3：片段数相符应有逐字音译，got %+v", d)
	}
}

// 空片段不成单元，纯空格的片段单独占一个单元；全是空格、行级音译为空的行没有逐字音译。
//
// 变异自证（隔离的 worktree，实测）：去掉空片段、认定行级音译为空这两条判断在 player.BuildRomaDetailed
// 里，把它们改坏本例同样红（写法见 player 包 TestBuildRomaDetailedDropsEmptyKeepsSpace 与
// TestBuildRomaDetailedEmptyRomaText）。本例另守住「先按下标配字、后去空片段」的次序：romaDetailed 里
// 先滤掉空片段再比片段数与字数，行 0 剩 3 个片段对 4 个字、整行成了零值而红。
func TestRomaDetailedEmptyAndSpaceFragments(t *testing.T) {
	roma := [][]string{
		{"ki ", "", " ", "mi "}, // 行0：空片段去掉，空格片段保留
		{" ", "", "  "},         // 行1：行级音译为空
		{"", ""},                // 行2：全是空片段
	}
	lines := ParsePlainKRC(mkLangTagWords(nil, roma) + "\n" +
		"[0,2000]<0,400,0>き<400,400,0>っ<800,400,0>、<1200,400,0>み\n" +
		"[2000,1500]<0,500,0>ラ<500,500,0>ラ<1000,500,0>ラ\n" +
		"[4000,1000]<0,500,0>あ<500,500,0>あ\n")
	if len(lines) != 3 {
		t.Fatalf("行数=%d, want 3", len(lines))
	}
	d := lines[0].RomaDetailed
	want := []player.LyricTextDetailedWord{
		{Timestamp: 0, Duration: 0.4, Text: "ki "},
		{Timestamp: 0.8, Duration: 0.4, Text: " "},
		{Timestamp: 1.2, Duration: 0.4, Text: "mi "},
	}
	if !reflect.DeepEqual(d.Words, want) {
		t.Fatalf("行0 逐字音译 = %+v\nwant %+v（空片段去掉、空格片段保留、各带所配那个字的时间）", d.Words, want)
	}
	if lines[0].RomaText != "ki  mi" {
		t.Fatalf("行0 RomaText=%q, want %q", lines[0].RomaText, "ki  mi")
	}
	for _, i := range []int{1, 2} {
		if lines[i].RomaText != "" || len(lines[i].RomaDetailed.Words) != 0 {
			t.Fatalf("行%d：行级音译为空，逐字音译应为零值，got roma=%q detailed=%+v", i, lines[i].RomaText, lines[i].RomaDetailed)
		}
	}
}

// 没有音译轨（没有 [language:]、只有翻译轨）：逐字音译为零值。
func TestRomaDetailedAbsentTrack(t *testing.T) {
	for name, text := range map[string]string{
		"没有 [language:]":  "[0,1000]<0,500,0>甲<500,500,0>乙\n",
		"只有翻译轨":           mkLangTagWords([]string{"译文"}, nil) + "\n[0,1000]<0,500,0>甲<500,500,0>乙\n",
		"[language:] 解不开": "[language:%%%]\n[0,1000]<0,500,0>甲<500,500,0>乙\n",
	} {
		lines := ParsePlainKRC(text)
		if len(lines) != 1 {
			t.Fatalf("%s：行数=%d, want 1", name, len(lines))
		}
		if d := lines[0].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.Duration != 0 {
			t.Errorf("%s：逐字音译应为零值，got %+v", name, d)
		}
		if lines[0].Text != "甲乙" || len(lines[0].Detailed.Words) != 2 {
			t.Errorf("%s：主歌词应照常，got %+v", name, lines[0])
		}
	}
}

// 主歌词里夹一个 0 词行：它在 type=0 轨里照样占一格，其后各行的逐字音译不能错位一格
// （与 RomaText / RomaWords 同一个 rawIdx 口径）。
func TestRomaDetailedSkipsBlankLineWithoutMisalign(t *testing.T) {
	roma := [][]string{{"A "}, {"SKIP "}, {"D1 ", "D2 "}}
	lines := ParsePlainKRC(mkLangTagWords(nil, roma) + "\n" +
		"[0,1000]<0,500,0>甲\n" +
		"[1000,1000]\n" +
		"[2000,1000]<0,500,0>大<500,500,0>胆\n")
	if len(lines) != 2 {
		t.Fatalf("行数=%d, want 2（0 词行应被跳过）", len(lines))
	}
	d := lines[1].RomaDetailed
	if len(d.Words) != 2 || d.Words[0].Text != "D1 " || d.Words[1].Text != "D2 " || d.Words[1].Timestamp != 2.5 {
		t.Fatalf("行1 逐字音译 = %+v，want D1/D2（错位到上一格会拿到 SKIP）", d.Words)
	}
}

// 主歌词的字时间在行内回退：逐字音译不给（时间取自这些字，照搬就是一条回退的时间轴），行级音译、
// 主歌词与它自己的逐字照旧。
func TestRomaDetailedInvalidMainTimeline(t *testing.T) {
	lines := ParsePlainKRC(mkLangTagWords(nil, [][]string{{"ki ", "mi "}, {"so ", "ra "}}) + "\n" +
		"[0,1000]<500,500,0>き<0,500,0>み\n" + // 第二个字的起始早于第一个
		"[2000,1000]<0,500,0>そ<500,500,0>ら\n")
	if len(lines) != 2 {
		t.Fatalf("行数=%d, want 2", len(lines))
	}
	if d := lines[0].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 {
		t.Fatalf("行0：字时间回退，逐字音译应为零值，got %+v", d)
	}
	if lines[0].RomaText != "ki mi" || lines[0].Text != "きみ" || len(lines[0].Detailed.Words) != 2 {
		t.Fatalf("行0：行级音译与主歌词应照旧，got %+v", lines[0])
	}
	if len(lines[1].RomaDetailed.Words) != 2 {
		t.Fatalf("行1：时间轴合法应有逐字音译，got %+v", lines[1].RomaDetailed)
	}
}

// romaDetailed 的入参与行级音译对不上（各单元拼起来不等于 romaText）时为零值。经 ParsePlainKRC
// 走不到这一支（两者同出自一条 type=0 轨），直接调函数钉住。
func TestRomaDetailedJoinMismatch(t *testing.T) {
	detailed := player.LyricTextDetailed{Timestamp: 1, Duration: 1, Words: []player.LyricTextDetailedWord{
		{Timestamp: 1, Duration: 0.5, Text: "き"}, {Timestamp: 1.5, Duration: 0.5, Text: "み"},
	}}
	if d := romaDetailed(detailed, []string{"ki ", "mi "}, "ki mi yo"); len(d.Words) != 0 {
		t.Fatalf("拼接与行级音译不等应为零值，got %+v", d)
	}
	if d := romaDetailed(detailed, []string{"ki ", "mi "}, "ki mi"); len(d.Words) != 2 {
		t.Fatalf("对照：相等时应有 2 个单元，got %+v", d)
	}
	// 没有逐字的行（LRC 回落）：片段也没有，零值。
	if d := romaDetailed(player.LyricTextDetailed{}, nil, ""); len(d.Words) != 0 {
		t.Fatalf("没有逐字的行应为零值，got %+v", d)
	}
}

// 加上 RomaDetailed 之后，主歌词、翻译、行级音译、RomaWords 与主歌词的逐字都与原先相同：同一份 KRC
// 上逐字段对照期望值，含片段数不符与时间回退两种拿不到逐字音译的行。
func TestRomaDetailedLeavesOtherFieldsUntouched(t *testing.T) {
	roma := [][]string{{"ki ", "mi "}, {"sora"}}
	lines := ParsePlainKRC(mkLangTagWords([]string{"你", "天空"}, roma) + "\n" +
		"[0,1000]<0,500,0>き<500,500,0>み\n" +
		"[2000,1000]<0,500,0>そ<500,500,0>ら\n")
	want := []Line{
		{Index: 0, Time: 0, Text: "きみ", SubText: "你", RomaText: "ki mi", RomaWords: []string{"ki ", "mi "},
			Detailed: player.LyricTextDetailed{Timestamp: 0, Duration: 1, Words: []player.LyricTextDetailedWord{
				{Timestamp: 0, Duration: 0.5, Text: "き"}, {Timestamp: 0.5, Duration: 0.5, Text: "み"}}}},
		{Index: 1, Time: 2, Text: "そら", SubText: "天空", RomaText: "sora", RomaWords: []string{"sora"},
			Detailed: player.LyricTextDetailed{Timestamp: 2, Duration: 1, Words: []player.LyricTextDetailedWord{
				{Timestamp: 2, Duration: 0.5, Text: "そ"}, {Timestamp: 2.5, Duration: 0.5, Text: "ら"}}}},
	}
	if len(lines) != len(want) {
		t.Fatalf("行数=%d, want %d", len(lines), len(want))
	}
	for i := range want {
		got := lines[i]
		got.RomaDetailed = player.LyricTextDetailed{} // 只比其余字段
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("行%d 其余字段变了：\n got %+v\nwant %+v", i, got, want[i])
		}
	}
	if len(lines[0].RomaDetailed.Words) != 2 || len(lines[1].RomaDetailed.Words) != 0 {
		t.Fatalf("逐字音译：行0 应有、行1（片段数不符）应无，got %+v / %+v", lines[0].RomaDetailed, lines[1].RomaDetailed)
	}
}
