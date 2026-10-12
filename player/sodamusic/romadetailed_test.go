package sodamusic

// 本文件只测从酷狗借音译时的逐字音译（RomaDetailed）：字级对齐接受的行按公共边界分组
// （groupRomaUnits）、文本取酷狗的音译片段、时间取汽水自己主歌词的字（charRomaDetailed），拿不到的
// 行只留行级；以及它到 all_lyrics 的行与 lyric_update 的接线。行级音译（RomaText）怎么对齐见
// roma_test.go，那里的断言一条未改。
//
// 样例为自拟的短句。汽水一侧每个字 500ms、首尾相接；酷狗一侧的时间故意放在 500 秒之后、每字 70ms：
// 逐字音译里出现酷狗那一侧的数值，就是取错了时间轴。

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// testOffset 是各用例给汽水主歌词套的 offset（借音译之前由 parseSodaLyrics 套）。
const testOffset = 0.25

// sodaTimed 造一行带时间的汽水主歌词：自 startSec 起每个字 500ms。Text 与 krc 解析的产物同口径
// （各字相接后去掉首尾空白）。PlayTime 未填，由用例经 applyDetailedOffset 套。
func sodaTimed(idx int, startSec float32, chars ...string) krc.Line {
	words := make([]player.LyricTextDetailedWord, len(chars))
	for i, c := range chars {
		words[i] = player.LyricTextDetailedWord{Timestamp: startSec + 0.5*float32(i), Duration: 0.5, Text: c}
	}
	return krc.Line{
		Index: idx, Time: startSec, Text: strings.TrimSpace(strings.Join(chars, "")),
		Detailed: player.LyricTextDetailed{Timestamp: startSec, Duration: 0.5 * float32(len(chars)), Words: words},
	}
}

// kugouTimed 造一行字级可靠的酷狗行，时间放在 500 秒之后。
func kugouTimed(chars, roma []string) klyric.Line {
	l := kugouCharLine(chars, roma)
	for i := range l.Detailed.Words {
		l.Detailed.Words[i].Timestamp = 500 + 0.07*float32(i)
		l.Detailed.Words[i].Duration = 0.07
	}
	l.Detailed.Timestamp = 500
	l.Detailed.Duration = 0.07 * float32(len(chars))
	return l
}

// unit 是期望的一个单元：起始 ts、时长 dur，PlayTime 为套过 testOffset 的起始。
func unit(ts, dur float32, text string) player.LyricTextDetailedWord {
	return player.LyricTextDetailedWord{Timestamp: ts, PlayTime: ts + testOffset, Duration: dur, Text: text}
}

// borrow 走一遍借音译的合并：汽水主歌词先套 offset（同 parseSodaLyrics），再合并酷狗的结果。
// 返回前核对两件对任何输入都该成立的事：主歌词、翻译与主歌词的逐字没被动过；有逐字音译的行，各单元
// 拼起来去掉首尾空白等于 RomaText。
func borrow(t *testing.T, soda []krc.Line, kugou []klyric.Line) romaMergeStats {
	t.Helper()
	applyDetailedOffset(soda, testOffset)
	before := make([]krc.Line, len(soda))
	for i, l := range soda {
		before[i] = l
		before[i].Detailed.Words = slices.Clone(l.Detailed.Words)
	}
	st := mergeKugouRomaStats(soda, kugou)
	for i, l := range soda {
		if l.Index != before[i].Index || l.Time != before[i].Time || l.Text != before[i].Text || l.SubText != before[i].SubText ||
			!reflect.DeepEqual(l.Detailed, before[i].Detailed) {
			t.Fatalf("行%d：主歌词、翻译或主歌词的逐字被动了\n got %+v\nwant %+v", i, l, before[i])
		}
		if len(l.RomaDetailed.Words) == 0 {
			continue
		}
		var joined strings.Builder
		for _, w := range l.RomaDetailed.Words {
			joined.WriteString(w.Text)
		}
		if strings.TrimSpace(joined.String()) != l.RomaText || l.RomaText == "" {
			t.Fatalf("行%d：各单元拼起来 %q 去掉首尾空白应等于 RomaText %q", i, joined.String(), l.RomaText)
		}
	}
	return st
}

func wantUnits(t *testing.T, where string, got player.LyricTextDetailed, want ...player.LyricTextDetailedWord) {
	t.Helper()
	if !reflect.DeepEqual(got.Words, want) {
		t.Fatalf("%s 的逐字音译：\n got %+v\nwant %+v", where, got.Words, want)
	}
}

func wantNoDetailed(t *testing.T, where string, got player.LyricTextDetailed) {
	t.Helper()
	if len(got.Words) != 0 || got.Timestamp != 0 || got.PlayTime != 0 || got.Duration != 0 {
		t.Fatalf("%s 不应有逐字音译（应为零值），got %+v", where, got)
	}
}

// —— 分组（纯函数）——————————————————————————————————————————————————————

// 相邻两个 rune 之间，汽水字与酷狗 token 同时换了才切开；只换其一的地方不切。
//
// 变异自证（隔离的 worktree，实测）：把边界条件的 `&&` 改成 `||`（任一边换了就切），「token 跨两个字」
// 「一个字里两个 token」与两例「交错」都多切出组而红；只看汽水字（去掉 `tok != runeTok[k-1]`），
// 「token 跨两个字」与两例「交错」红；只看 token（去掉 `w != runeWord[k-1]`），「一个字里两个 token」
// 与两例「交错」红；组内的 token 不去重（每个 rune 都追加一次），「token 跨两个字」、两例「交错」、
// 「多 rune 且边界重合」「整行一个字一个 token」红；不更新 lastWord，「token 跨两个字」与两例「交错」红。
// 以上各条同时使下面对应的端到端用例（TokenSpansWords、TokensInOneWord、Staggered）变红。
func TestGroupRomaUnits(t *testing.T) {
	g := func(first, last int, toks ...int) romaGroup {
		return romaGroup{firstWord: first, lastWord: last, toks: toks}
	}
	cases := []struct {
		name              string
		runeWord, runeTok []int
		want              []romaGroup
	}{
		{"一一对应", []int{0, 1, 2}, []int{5, 6, 7}, []romaGroup{g(0, 0, 5), g(1, 1, 6), g(2, 2, 7)}},
		// 汽水 [君][だ][け][を]、token [君][だけ][を]
		{"token 跨两个字", []int{0, 1, 2, 3}, []int{0, 1, 1, 2}, []romaGroup{g(0, 0, 0), g(1, 2, 1), g(3, 3, 2)}},
		// 汽水 [君][だけ][を]、token [君][だ][け][を]
		{"一个字里两个 token", []int{0, 1, 1, 2}, []int{0, 1, 2, 3}, []romaGroup{g(0, 0, 0), g(1, 1, 1, 2), g(2, 2, 3)}},
		// 汽水 [あい][うえ]、token [あ][いう][え]：没有一处是两边同时换的
		{"交错", []int{0, 0, 1, 1}, []int{0, 1, 1, 2}, []romaGroup{g(0, 1, 0, 1, 2)}},
		// 汽水 [a][bc]、token [ab][c]
		{"交错（另一向）", []int{0, 1, 1}, []int{0, 0, 1}, []romaGroup{g(0, 1, 0, 1)}},
		// 多个 rune 的字对多个 rune 的 token，边界重合
		{"多 rune 且边界重合", []int{0, 0, 1, 1}, []int{3, 3, 4, 4}, []romaGroup{g(0, 0, 3), g(1, 1, 4)}},
		// 中间隔着不属于任何 rune 的字（标点）与没对上的 token：下标跳号，照样按「同时换」切
		{"下标跳号", []int{0, 2, 4}, []int{1, 3, 9}, []romaGroup{g(0, 0, 1), g(2, 2, 3), g(4, 4, 9)}},
		{"单个 rune", []int{3}, []int{8}, []romaGroup{g(3, 3, 8)}},
		{"整行一个字一个 token", []int{0, 0, 0}, []int{2, 2, 2}, []romaGroup{g(0, 0, 2)}},
		{"空", nil, nil, nil},
	}
	for _, c := range cases {
		if got := groupRomaUnits(c.runeWord, c.runeTok); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// —— 字级对齐的行产出逐字音译 ——————————————————————————————————————————————

// 断行与字切分都相同：每个字一个单元，文本是酷狗的片段原样，时间是汽水这个字的（不是酷狗的），
// PlayTime 是汽水这个字套过 offset 的播出时间；行级三项取汽水这一行的。
//
// 变异自证（隔离的 worktree，实测）：删掉 mergeKugouRomaStats 里的 `sodaLines[i].RomaDetailed = c.detailed`，
// 本例没有逐字音译而红；charRomaDetailed 的单元不带 PlayTime（去掉 `PlayTime: first.PlayTime`），
// 单元的 play_time 是 0 而红；行级不带 PlayTime 同理红；传给 BuildRomaDetailed 的第二个实参换成
// line.Text，拼接与它不等、整行为零值而红。
func TestBorrowRomaDetailedOneToOne(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "君", "だ", "け", "を")}
	kugou := []klyric.Line{kugouTimed([]string{"君", "だ", "け", "を"}, []string{"kimi ", "da ", "ke ", "wo "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 || st.matched != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	if soda[0].RomaText != "kimi da ke wo" {
		t.Fatalf("RomaText=%q", soda[0].RomaText)
	}
	d := soda[0].RomaDetailed
	wantUnits(t, "行0", d, unit(10, 0.5, "kimi "), unit(10.5, 0.5, "da "), unit(11, 0.5, "ke "), unit(11.5, 0.5, "wo "))
	if d.Timestamp != 10 || d.Duration != 2 || d.PlayTime != 10+testOffset {
		t.Fatalf("行级三项应取汽水这一行的（10 / 2 / %v），got %v / %v / %v", 10+testOffset, d.Timestamp, d.Duration, d.PlayTime)
	}
}

// 断行不同（酷狗 2 行对汽水 3 行）：每个汽水行各自产出逐字音译，跨酷狗断行的那一行也一样。
func TestBorrowRomaDetailedDifferentLineBreaks(t *testing.T) {
	kugou := []klyric.Line{
		kugouTimed([]string{"君", "だ", "け", "を", "見"}, []string{"kimi ", "da ", "ke ", "wo ", "mi "}),
		kugouTimed([]string{"つ", "め", "て", "る"}, []string{"tsu ", "me ", "te ", "ru "}),
	}
	soda := []krc.Line{
		sodaTimed(0, 10, "君", "だ", "け", "を"),
		sodaTimed(1, 20, "見", "つ", "め"), // 見 在酷狗行 0 的末尾，つめ 在酷狗行 1
		sodaTimed(2, 30, "て", "る"),
	}
	if st := borrow(t, soda, kugou); st.charLines != 3 {
		t.Fatalf("stats=%+v, want 字级 3 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "kimi "), unit(10.5, 0.5, "da "), unit(11, 0.5, "ke "), unit(11.5, 0.5, "wo "))
	wantUnits(t, "行1", soda[1].RomaDetailed, unit(20, 0.5, "mi "), unit(20.5, 0.5, "tsu "), unit(21, 0.5, "me "))
	wantUnits(t, "行2", soda[2].RomaDetailed, unit(30, 0.5, "te "), unit(30.5, 0.5, "ru "))
	if d := soda[1].RomaDetailed; d.Timestamp != 20 || d.Duration != 1.5 {
		t.Fatalf("行1 行级应取汽水这一行的，got %v / %v", d.Timestamp, d.Duration)
	}
}

// 一个酷狗 token 跨几个汽水字：合成一个单元，起始是第一个字的、时长算到最后一个字结束。
//
// 变异自证（隔离的 worktree，实测）：单元的时长只取最后一个字的（`Duration: last.Duration`），
// 本例的 1.0 成了 0.5 而红；起始取最后一个字的（`first, last := words[g.lastWord], ...`），起始成了
// 11 而红。
func TestBorrowRomaDetailedTokenSpansWords(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "君", "だ", "け", "を")}
	kugou := []klyric.Line{kugouTimed([]string{"君", "だけ", "を"}, []string{"kimi ", "dake ", "wo "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "kimi "), unit(10.5, 1, "dake "), unit(11.5, 0.5, "wo "))
}

// 几个酷狗 token 落在一个汽水字里：合成一个单元，文本是各片段依次相接，时间就是那个字的。
func TestBorrowRomaDetailedTokensInOneWord(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "君", "だけ", "を")}
	kugou := []klyric.Line{kugouTimed([]string{"君", "だ", "け", "を"}, []string{"kimi ", "da ", "ke ", "wo "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "kimi "), unit(10.5, 0.5, "da ke "), unit(11, 0.5, "wo "))
}

// 两边的切分交错（汽水 [あい][うえ]、酷狗 [あ][いう][え]）：没有公共边界，整行一个单元。
func TestBorrowRomaDetailedStaggered(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "あい", "うえ")}
	kugou := []klyric.Line{kugouTimed([]string{"あ", "いう", "え"}, []string{"a ", "iu ", "e "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 1, "a iu e "))
}

// 标点与空格：汽水里归一化后不剩 rune 的字（标点、空格）不属于任何单元，其余单元仍带各自那个字的
// 时间；酷狗里的标点 token 不参与对齐，它的片段不进行级音译、也不进逐字。字里自带的尾空格不影响。
func TestBorrowRomaDetailedPunctuationAndSpaces(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "君 ", "、", "だ", " ", "け！")}
	kugou := []klyric.Line{kugouTimed(
		[]string{"君", "，", "だ", "け", "。"},
		[]string{"kimi ", "x", "da ", "ke", " "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	if soda[0].RomaText != "kimi da ke" {
		t.Fatalf("RomaText=%q, want %q（标点 token 的片段不进行级音译）", soda[0].RomaText, "kimi da ke")
	}
	// 汽水的字：君(10) 、(10.5) だ(11) 空格(11.5) け！(12)
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "kimi "), unit(11, 0.5, "da "), unit(12, 0.5, "ke"))
}

// 片段为空的 token 自成一组时不成单元（拼起来仍等于行级音译）。
func TestBorrowRomaDetailedEmptyFragmentSkipped(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "君", "っ", "だ")}
	kugou := []klyric.Line{kugouTimed([]string{"君", "っ", "だ"}, []string{"kimi ", "", "da "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "kimi "), unit(11, 0.5, "da "))
}

// 小书写体假名折叠（汽水 ィ 对酷狗 イ）对逐字音译同样成立：字与行文本走的是同一个归一化。
func TestBorrowRomaDetailedSmallKana(t *testing.T) {
	soda := []krc.Line{sodaTimed(0, 10, "サ", "ィ", "レ")}
	kugou := []klyric.Line{kugouTimed([]string{"サ", "イ", "レ"}, []string{"sa ", "i ", "re "})}
	if st := borrow(t, soda, kugou); st.charLines != 1 {
		t.Fatalf("stats=%+v, want 字级 1 行", st)
	}
	wantUnits(t, "行0", soda[0].RomaDetailed, unit(10, 0.5, "sa "), unit(10.5, 0.5, "i "), unit(11, 0.5, "re "))
}

// offset 为负、把播出时间压到 0 以下时，逐字音译的 PlayTime 与主歌词的字一样钳在 0：它取的就是
// 那个字的 PlayTime。
func TestBorrowRomaDetailedNegativeOffset(t *testing.T) {
	for _, off := range []float32{-0.25, -100} {
		soda := []krc.Line{sodaTimed(0, 10, "君", "だ")}
		applyDetailedOffset(soda, off)
		mergeKugouRoma(soda, []klyric.Line{kugouTimed([]string{"君", "だ"}, []string{"kimi ", "da "})})
		d := soda[0].RomaDetailed
		if len(d.Words) != 2 {
			t.Fatalf("offset %v：应有 2 个单元，got %+v", off, d.Words)
		}
		if got, want := d.PlayTime, player.AdjustLyricPlayTime(10, off); got != want {
			t.Fatalf("offset %v：行级 play_time = %v, want %v", off, got, want)
		}
		for j, w := range d.Words {
			if got, want := w.PlayTime, player.AdjustLyricPlayTime(w.Timestamp, off); got != want || got != soda[0].Detailed.Words[j].PlayTime {
				t.Fatalf("offset %v 单元%d：play_time = %v, want %v（同主歌词那个字的）", off, j, got, want)
			}
		}
	}
}

// —— 拿不到逐字音译、只留行级的行 ——————————————————————————————————————————————

// 走行级回落配上的行不产逐字：不知道音译的哪一段对着哪个字。这一行原先若带着逐字音译（平台的 KRC
// 内嵌音译轨时由 krc 解析填上），行级音译被换掉之后要一并清掉。
//
// 变异自证（隔离的 worktree，实测）：删掉行级回落分支里的 `sodaLines[i].RomaDetailed = player.LyricTextDetailed{}`，
// 第二段留着与新 RomaText 对不上的旧逐字音译而红。
func TestBorrowRomaDetailedLineFallbackHasNone(t *testing.T) {
	kugou := []klyric.Line{kugouLineOnly("君だけを", "kimi dake wo")}
	soda := []krc.Line{sodaTimed(0, 10, "君", "だ", "け", "を")}
	if st := borrow(t, soda, kugou); st.lineLines != 1 || st.charLines != 0 {
		t.Fatalf("stats=%+v, want 行级回落 1 行", st)
	}
	if soda[0].RomaText != "kimi dake wo" {
		t.Fatalf("RomaText=%q", soda[0].RomaText)
	}
	wantNoDetailed(t, "行级回落的行", soda[0].RomaDetailed)

	// 原先带着逐字音译的行，行级回落换掉 RomaText 之后不留旧的。
	soda = []krc.Line{sodaTimed(0, 10, "君", "だ", "け", "を")}
	soda[0].RomaText = "旧的"
	soda[0].RomaDetailed = player.LyricTextDetailed{Timestamp: 10, Duration: 2, Words: []player.LyricTextDetailedWord{{Timestamp: 10, Duration: 2, Text: "旧的"}}}
	mergeKugouRoma(soda, kugou)
	if soda[0].RomaText != "kimi dake wo" {
		t.Fatalf("RomaText=%q", soda[0].RomaText)
	}
	wantNoDetailed(t, "行级回落换掉 RomaText 的行", soda[0].RomaDetailed)

	// 两层都没配上的行，两个字段都不动。
	soda = []krc.Line{sodaTimed(0, 10, "別", "の", "歌")}
	soda[0].RomaText = "原有"
	old := player.LyricTextDetailed{Timestamp: 10, Duration: 2, Words: []player.LyricTextDetailedWord{{Timestamp: 10, Duration: 2, Text: "原有"}}}
	soda[0].RomaDetailed = old
	if n := mergeKugouRoma(soda, kugou); n != 0 {
		t.Fatalf("用例前提不成立：不该配上，got %d", n)
	}
	if soda[0].RomaText != "原有" || !reflect.DeepEqual(soda[0].RomaDetailed, old) {
		t.Fatalf("没配上的行不应被动，got %q / %+v", soda[0].RomaText, soda[0].RomaDetailed)
	}
}

// 酷狗的一个 token 被汽水拆到两行：两行都不接受字级，没有行级音译、也没有逐字音译。
func TestBorrowRomaDetailedStraddleRejected(t *testing.T) {
	kugou := []klyric.Line{kugouTimed([]string{"君", "だけ", "を"}, []string{"kimi ", "dake ", "wo "})}
	soda := []krc.Line{sodaTimed(0, 10, "君", "だ"), sodaTimed(1, 20, "け", "を")}
	if st := borrow(t, soda, kugou); st.matched != 0 {
		t.Fatalf("stats=%+v, want 一行都不配", st)
	}
	for i := range soda {
		if soda[i].RomaText != "" {
			t.Fatalf("行%d RomaText=%q, want 空", i, soda[i].RomaText)
		}
		wantNoDetailed(t, "token 跨行的行", soda[i].RomaDetailed)
	}
}

// 汽水这一行没有逐字、或字的文本与行文本对不上：字级对齐照常给行级音译（它只看行文本），逐字音译
// 没有——无从把 rune 归到字上。
//
// 变异自证（隔离的 worktree，实测）：去掉 charRomaDetailed 里逐个比对 rune 的那道判断
// （`textRunes[len(runeWord)] != nr`），「字的文本换了一个字」「字的文本次序不同」带着错位的单元放行
// 而红；去掉同一行的 `len(runeWord) >= len(textRunes)`，「字的文本多一个字」下标越界 panic；去掉其后的
// `len(runeWord) != len(textRunes)`，「少一个字且那个字的片段为空」放行而红。
func TestBorrowRomaDetailedWordsDoNotMatchText(t *testing.T) {
	kugou := []klyric.Line{kugouTimed([]string{"君", "だ", "け", "を"}, []string{"kimi ", "da ", "ke ", "wo "})}
	cases := []struct {
		name  string
		chars []string
	}{
		{"没有逐字", nil},
		{"字的文本少一个字", []string{"君", "だ", "け"}},
		{"字的文本多一个字", []string{"君", "だ", "け", "を", "よ"}},
		{"字的文本换了一个字", []string{"君", "だ", "げ", "を"}},
		{"字的文本次序不同", []string{"だ", "君", "け", "を"}},
		{"字的文本全是标点", []string{"、", "。"}},
		{"字的文本开头多一个字", []string{"あ", "君", "だ", "け", "を"}},
	}
	for _, c := range cases {
		line := sodaTimed(0, 10, c.chars...)
		line.Text = "君だけを" // 行文本是对的，字的文本对不上
		soda := []krc.Line{line}
		if st := borrow(t, soda, kugou); st.charLines != 1 {
			t.Fatalf("%s：stats=%+v, want 字级 1 行（行级音译只看行文本）", c.name, st)
		}
		if soda[0].RomaText != "kimi da ke wo" {
			t.Fatalf("%s：RomaText=%q", c.name, soda[0].RomaText)
		}
		if d := soda[0].RomaDetailed; len(d.Words) != 0 || d.Timestamp != 0 || d.PlayTime != 0 || d.Duration != 0 {
			t.Errorf("%s：不应有逐字音译（应为零值），got %+v", c.name, d)
		}
	}

	// 少的那个字恰好对着片段为空的 token：其余单元拼起来仍等于行级音译，拼接那道核对拦不住它。
	line := sodaTimed(0, 10, "君", "だ", "け")
	line.Text = "君だけを"
	soda := []krc.Line{line}
	borrow(t, soda, []klyric.Line{kugouTimed([]string{"君", "だ", "け", "を"}, []string{"kimi ", "da ", "ke", ""})})
	if soda[0].RomaText != "kimi da ke" {
		t.Fatalf("少一个字且那个字的片段为空：RomaText=%q", soda[0].RomaText)
	}
	wantNoDetailed(t, "少一个字且那个字的片段为空", soda[0].RomaDetailed)
	// 同一情形直接调函数、给的 token 个数与字的 rune 个数相同：只剩「字的 rune 个数须等于行文本的」
	// 这一道能拦下。
	wantNoDetailed(t, "少一个字（直接调用）", charRomaDetailed(soda[0], []int{0, 1, 2}, []string{"kimi ", "da ", "ke"}, "kimi da ke"))
}

// 汽水主歌词的字时间不合法：单元的起始回退、或单元的时长为负（跨几个字的单元里，最后一个字结束得
// 比第一个字开始还早）。逐字音译不给，行级音译照常。
//
// 变异自证（隔离的 worktree，实测）：player.BuildRomaDetailed 里的回退判定改成恒假，「起始回退」红；
// 去掉它对单元时长的检查，「时长为负」红。
func TestBorrowRomaDetailedInvalidTimeline(t *testing.T) {
	// 起始回退：第三个字的起始早于第二个字。
	regress := sodaTimed(0, 10, "君", "だ", "け", "を")
	regress.Detailed.Words[2].Timestamp = 9
	// 时长为负：だ、け 合成一个单元（酷狗的 token 是「だけ」），け 的结束（8 + 0.5）早于 だ 的开始（10.5）。
	// 三个单元的起始是 10 / 10.5 / 11.5，不回退——只有时长为负这一处不合法。
	negative := sodaTimed(0, 10, "君", "だ", "け", "を")
	negative.Detailed.Words[2].Timestamp = 8
	for name, c := range map[string]struct {
		line  krc.Line
		kugou klyric.Line
	}{
		"起始回退": {regress, kugouTimed([]string{"君", "だ", "け", "を"}, []string{"kimi ", "da ", "ke ", "wo "})},
		"时长为负": {negative, kugouTimed([]string{"君", "だけ", "を"}, []string{"kimi ", "dake ", "wo "})},
	} {
		soda := []krc.Line{c.line}
		if st := borrow(t, soda, []klyric.Line{c.kugou}); st.charLines != 1 {
			t.Fatalf("%s：stats=%+v, want 字级 1 行", name, st)
		}
		if soda[0].RomaText == "" {
			t.Fatalf("%s：行级音译应照常", name)
		}
		wantNoDetailed(t, name, soda[0].RomaDetailed)
	}
}

// charRomaDetailed 的入参彼此对不上时为零值、不越界：rune 对到的 token 个数与这一行的 rune 个数不同
// （经 alignCharLevel 走不到：它只对整行每个 rune 都对齐上的行调用，直接调函数钉住）；各单元拼起来
// 不等于给定的行级音译。
//
// 变异自证（隔离的 worktree，实测）：去掉 `len(runeWord) != len(runeToks)`，token 个数少的一例在
// groupRomaUnits 里下标越界 panic。
func TestCharRomaDetailedMismatchedInputs(t *testing.T) {
	lines := []krc.Line{sodaTimed(0, 10, "君", "だ")}
	applyDetailedOffset(lines, testOffset)
	tokenRoma := []string{"kimi ", "da "}
	if d := charRomaDetailed(lines[0], []int{0, 1}, tokenRoma, "kimi da"); len(d.Words) != 2 {
		t.Fatalf("对照：入参一致时应有 2 个单元，got %+v", d)
	}
	wantNoDetailed(t, "token 个数少于 rune 个数", charRomaDetailed(lines[0], []int{0}, tokenRoma, "kimi da"))
	wantNoDetailed(t, "token 个数多于 rune 个数", charRomaDetailed(lines[0], []int{0, 1, 1}, tokenRoma, "kimi da"))
	wantNoDetailed(t, "没有 token", charRomaDetailed(lines[0], nil, tokenRoma, "kimi da"))
	wantNoDetailed(t, "拼接不等于行级音译", charRomaDetailed(lines[0], []int{0, 1}, tokenRoma, "kimi da yo"))
	wantNoDetailed(t, "行级音译为空", charRomaDetailed(lines[0], []int{0, 1}, tokenRoma, ""))
}

// —— 不共享、接线 ——————————————————————————————————————————————————————

// 逐字音译的单元与汽水主歌词的字不共享底层数组；传给借音译 goroutine 的是歌词的浅拷贝，它的逐字
// 与主循环手里的那一份共用，借音译只能另行分配、不能就地改。
func TestBorrowRomaDetailedDoesNotShareWithMain(t *testing.T) {
	current := []krc.Line{sodaTimed(0, 10, "君", "だ")}
	applyDetailedOffset(current, testOffset)
	snapshot := append([]krc.Line(nil), current...) // 同 runSession 交给 fetchKugouRoma 的那一份
	mergeKugouRoma(snapshot, []klyric.Line{kugouTimed([]string{"君", "だ"}, []string{"kimi ", "da "})})
	if len(snapshot[0].RomaDetailed.Words) != 2 {
		t.Fatalf("应有 2 个单元，got %+v", snapshot[0].RomaDetailed)
	}
	if current[0].RomaText != "" || len(current[0].RomaDetailed.Words) != 0 {
		t.Fatalf("合并改到了主循环手里的那一份：%+v", current[0])
	}
	snapshot[0].RomaDetailed.Words[0].PlayTime = 99
	snapshot[0].RomaDetailed.Words[1].Text = "改了"
	if w := current[0].Detailed.Words; w[0].PlayTime != 10+testOffset || w[1].Text != "だ" {
		t.Fatalf("改逐字音译影响到了主歌词的逐字：%+v", w)
	}
}

// 接线：补发时 all_lyrics 的行与其后的 lyric_update 都带逐字音译，取自同一份歌词、内容相同；
// 只有行级音译的行与没有音译的行两处都是零值、序列化为 {}。
//
// 变异自证（隔离的 worktree，实测）：删掉 toLyricLines 里的 `out[i].RomaTextDetailed = l.RomaDetailed`，
// all_lyrics 的行没有逐字音译而红；删掉 lineUpdate 里的 `u.RomaTextDetailed = l.RomaDetailed`，
// lyric_update 没有而红。
func TestLyricWireRomaTextDetailed(t *testing.T) {
	soda := []krc.Line{
		sodaTimed(0, 10, "君", "だ"),   // 字级对齐：有逐字音译
		sodaTimed(1, 20, "特", "殊"),   // 行级回落：只有行级音译
		sodaTimed(2, 30, "la", "la"), // 没配上
	}
	soda[0].SubText = "只有你"
	borrow(t, soda, []klyric.Line{
		kugouTimed([]string{"君", "だ"}, []string{"kimi ", "da "}),
		kugouLineOnly("特殊", "toku shu"),
	})
	items := toLyricLines(soda, testOffset)

	wantUnits(t, "all_lyrics 行0", items[0].RomaTextDetailed, unit(10, 0.5, "kimi "), unit(10.5, 0.5, "da "))
	if items[0].RomaText != "kimi da" || items[0].SubText != "只有你" || items[0].Text != "君だ" || len(items[0].TextDetailed.Words) != 2 {
		t.Fatalf("all_lyrics 行0：主歌词、翻译、行级音译应照常，got %+v", items[0])
	}
	if items[1].RomaText != "toku shu" || items[2].RomaText != "" {
		t.Fatalf("行级音译：行1=%q 行2=%q", items[1].RomaText, items[2].RomaText)
	}
	wantNoDetailed(t, "all_lyrics 行1（行级回落）", items[1].RomaTextDetailed)
	wantNoDetailed(t, "all_lyrics 行2（没配上）", items[2].RomaTextDetailed)

	for i := range soda {
		u := lineUpdate(i, soda[i], testOffset, 12.5, 200)
		if !reflect.DeepEqual(u.RomaTextDetailed, items[i].RomaTextDetailed) {
			t.Fatalf("行%d：lyric_update 与 all_lyrics 的 roma_text_detailed 不同源\n lyric_update %+v\n all_lyrics   %+v", i, u.RomaTextDetailed, items[i].RomaTextDetailed)
		}
		if u.Index != i || u.Text != items[i].Text || u.SubText != items[i].SubText || u.RomaText != items[i].RomaText ||
			u.PlayTime != items[i].PlayTime || !reflect.DeepEqual(u.TextDetailed, items[i].TextDetailed) || u.Position != 12.5 {
			t.Fatalf("行%d：lyric_update 其余字段应与 all_lyrics 的行一致，got %+v", i, u)
		}
	}

	b, err := json.Marshal(&player.AllLyricsData{Lyrics: items, Count: len(items)})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"roma_text_detailed":{"timestamp":10,"play_time":10.25,"duration":1,"words":[{"timestamp":10,"play_time":10.25,"duration":0.5,"text":"kimi "},{"timestamp":10.5,"play_time":10.75,"duration":0.5,"text":"da "}]}`) {
		t.Fatalf("wire 上行0 的 roma_text_detailed 不对：%s", s)
	}
	if n := strings.Count(s, `"roma_text_detailed":{}`); n != 2 {
		t.Fatalf("wire 上应有 2 行的 roma_text_detailed 为 {}，got %d：%s", n, s)
	}
}

// 平台的 KRC 内嵌音译轨时（实测没有，解析照样支持），krc 解析出的逐字音译经 applyDetailedOffset
// 套上 offset、经 toLyricLines 进 wire，与借来的那一份走同一个出口。
//
// 变异自证（隔离的 worktree，实测）：applyDetailedOffset 里去掉 `&lines[i].RomaDetailed`，本例的
// play_time 仍是 0 而红。
func TestApplyDetailedOffsetCoversEmbeddedRomaDetailed(t *testing.T) {
	for _, off := range []float32{0.25, -0.25} {
		line := sodaTimed(0, 10, "君", "だ")
		line.RomaText = "kimi da"
		line.RomaDetailed = player.LyricTextDetailed{Timestamp: 10, Duration: 1, Words: []player.LyricTextDetailedWord{
			{Timestamp: 10, Duration: 0.5, Text: "kimi "}, {Timestamp: 10.5, Duration: 0.5, Text: "da "},
		}}
		lines := []krc.Line{line}
		applyDetailedOffset(lines, off)
		d := toLyricLines(lines, off)[0].RomaTextDetailed
		if len(d.Words) != 2 || d.PlayTime != 10+off || d.Words[0].PlayTime != 10+off || d.Words[1].PlayTime != 10.5+off {
			t.Fatalf("offset %v：逐字音译的 play_time 应套过 offset，got %+v", off, d)
		}
		if w := lines[0].Detailed.Words[1]; w.PlayTime != 10.5+off {
			t.Fatalf("offset %v：主歌词的逐字同样应套过，got %+v", off, w)
		}
	}
}
