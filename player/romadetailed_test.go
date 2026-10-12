package player

// 本文件只测 BuildRomaDetailed：由候选的逐字音译与行级音译得出对外的 roma_text_detailed 时的约定
// （空单元去掉、各单元拼起来去掉首尾空白等于行级音译、时间轴合法、不与候选共享切片）。qqmusic、
// kugou、sodamusic、cloudmusicv3 四家各自拼候选的用例在各自的包里。

import (
	"math"
	"strings"
	"testing"
)

func romaUnit(start, dur float32, text string) LyricTextDetailedWord {
	return LyricTextDetailedWord{Timestamp: start, Duration: dur, Text: text}
}

func romaCandidate(units ...LyricTextDetailedWord) LyricTextDetailed {
	return LyricTextDetailed{Timestamp: 10, PlayTime: 9.8, Duration: 3, Words: units}
}

func joinUnits(d LyricTextDetailed) string {
	var b strings.Builder
	for _, w := range d.Words {
		b.WriteString(w.Text)
	}
	return b.String()
}

// 正常行：各单元原样带过（文本含尾空格、起始、时长、PlayTime），行级三项原样带过。
//
// 变异自证（隔离的 worktree，实测）：把比较写成 `joined.String() != romaText`（不去首尾空白），
// 末单元带尾空格的本例得零值而红。
func TestBuildRomaDetailedKeepsUnits(t *testing.T) {
	in := romaCandidate(
		LyricTextDetailedWord{Timestamp: 10, PlayTime: 9.8, Duration: 1, Text: "ki "},
		LyricTextDetailedWord{Timestamp: 11, PlayTime: 10.8, Duration: 0.5, Text: "mi "},
		LyricTextDetailedWord{Timestamp: 11.5, PlayTime: 11.3, Duration: 1.5, Text: "da "},
	)
	got := BuildRomaDetailed(in, "ki mi da")
	if got.Timestamp != 10 || got.PlayTime != 9.8 || got.Duration != 3 {
		t.Fatalf("行级三项应原样带过，got %+v", got)
	}
	if len(got.Words) != 3 {
		t.Fatalf("应有 3 个单元，got %+v", got.Words)
	}
	for i, w := range in.Words {
		if got.Words[i] != w {
			t.Fatalf("单元 %d = %+v，want 原样 %+v", i, got.Words[i], w)
		}
	}
	if s := strings.TrimSpace(joinUnits(got)); s != "ki mi da" {
		t.Fatalf("各单元拼起来去掉首尾空白 = %q，want 行级音译", s)
	}
}

// text 为空串的单元去掉；只含空格的单元保留（它单独占一个单元，是排版的一部分）。被去掉的空单元
// 不参与时间轴的判定。
//
// 变异自证（隔离的 worktree，实测）：不跳过空单元（`if w.Text == ""` 改成恒假），空单元参与时间轴的
// 判定，本例里它的起始早于上一个单元、整行成了零值而红；改成 `strings.TrimSpace(w.Text) == ""`（连
// 纯空格单元一起去掉），拼起来成了 kimi、与行级音译不等，整行成了零值而红；去掉末尾的
// `d.Words = words`（返回的还是候选自己的 Words），空单元还在而红。
func TestBuildRomaDetailedDropsEmptyKeepsSpace(t *testing.T) {
	got := BuildRomaDetailed(romaCandidate(
		romaUnit(10, 1, "ki"),
		romaUnit(3, 1, ""), // 空单元：去掉；它的起始早于上一个单元，不算回退
		romaUnit(11, 0.2, " "),
		romaUnit(11.2, 1, "mi"),
		romaUnit(12.2, 0.1, ""),
	), "ki mi")
	want := []string{"ki", " ", "mi"}
	if len(got.Words) != len(want) {
		t.Fatalf("单元应为 %q，got %+v", want, got.Words)
	}
	for i, w := range want {
		if got.Words[i].Text != w {
			t.Fatalf("单元 %d 的文本 = %q，want %q", i, got.Words[i].Text, w)
		}
	}
	if got.Words[1].Timestamp != 11 || got.Words[2].Timestamp != 11.2 {
		t.Fatalf("留下的单元应带着各自的时间，got %+v", got.Words)
	}
}

// 行级音译为空的行没有逐字音译：全是空格的单元拼起来去掉首尾空白也是空串，与空的行级音译「相等」，
// 不能据此放行。没有任何非空单元同理。
//
// 变异自证（隔离的 worktree，实测）：删掉开头的 `romaText == ""`，第一段四例都不再是零值而红
// （前两例带着纯空格的单元，后两例带着候选的行级三项）。
func TestBuildRomaDetailedEmptyRomaText(t *testing.T) {
	cases := map[string]LyricTextDetailed{
		"全是空格的单元": romaCandidate(romaUnit(10, 1, " "), romaUnit(11, 1, "  ")),
		"一个空格":    romaCandidate(romaUnit(10, 1, " ")),
		"没有单元":    romaCandidate(),
		"全是空单元":   romaCandidate(romaUnit(10, 1, ""), romaUnit(11, 1, "")),
	}
	for name, in := range cases {
		if got := BuildRomaDetailed(in, ""); len(got.Words) != 0 || got.Timestamp != 0 || got.Duration != 0 || got.PlayTime != 0 {
			t.Errorf("%s：行级音译为空时应为零值，got %+v", name, got)
		}
	}
	// 行级音译非空而没有可用的单元：同样是零值。
	for name, in := range map[string]LyricTextDetailed{
		"没有单元":  romaCandidate(),
		"全是空单元": romaCandidate(romaUnit(10, 1, ""), romaUnit(11, 1, "")),
		"零值":    {},
	} {
		if got := BuildRomaDetailed(in, "ki mi"); len(got.Words) != 0 || got.Timestamp != 0 {
			t.Errorf("%s：没有非空单元时应为零值，got %+v", name, got)
		}
	}
}

// 各单元拼起来去掉首尾空白不等于行级音译：整行不给，不推与 roma_text 对不上的逐字。
//
// 变异自证（隔离的 worktree，实测）：`strings.TrimSpace(joined.String()) != romaText` 这道比较改成
// 恒假，七例都返回带单元的结果而红。
func TestBuildRomaDetailedJoinMustEqualRomaText(t *testing.T) {
	units := romaCandidate(romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi"))
	for _, romaText := range []string{
		"ki mi da", // 行级多一段（单元缺了尾巴）
		"ki",       // 行级少一段
		"kimi",     // 词间空格不同
		"Ki mi",    // 大小写不同
		"ki  mi",   // 空格个数不同
		" ki mi",   // 行级自带首空格（行级音译本应已去首尾空白）
		"mi ki",    // 次序不同
	} {
		if got := BuildRomaDetailed(units, romaText); len(got.Words) != 0 {
			t.Errorf("行级音译 %q：与各单元拼接不等，应为零值，got %+v", romaText, got.Words)
		}
	}
	if got := BuildRomaDetailed(units, "ki mi"); len(got.Words) != 2 {
		t.Fatalf("对照：相等时应有 2 个单元，got %+v", got.Words)
	}
}

// 时间轴不合法的行整行不给：单元的起始在行内回退、时长为负、起始或时长不是有限数，行级的起始或
// 时长不合法。起始相同不算回退（两个单元可以同时开始）。
//
// 变异自证（隔离的 worktree，实测）：`w.Timestamp < lastStart` 改成恒假、或不更新 lastStart，
// 「起始回退」红；把它改成 `<=`，「起始相同」「从 0 秒起」红；循环里去掉对 w.Duration 的
// validDetailedTime，「时长为负」「时长 NaN」「时长 +Inf」红；去掉对 w.Timestamp 的，「起始 NaN」
// 「起始 +Inf」红（「起始为负」仍由回退判定拦下：它小于 lastStart 的初值 0）；开头去掉对
// d.Timestamp / d.Duration 的检查，四条行级的例红；validDetailedTime 改成拒绝式
// `!(v < 0 || v > math.MaxFloat32)`，四条 NaN 红；去掉上界 `v <= math.MaxFloat32`，三条 +Inf 红。
func TestBuildRomaDetailedTimeline(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	bad := map[string]LyricTextDetailed{
		"起始回退":      romaCandidate(romaUnit(11, 1, "ki "), romaUnit(10.999, 1, "mi")),
		"时长为负":      romaCandidate(romaUnit(10, -0.001, "ki "), romaUnit(11, 1, "mi")),
		"起始为负":      romaCandidate(romaUnit(-1, 1, "ki "), romaUnit(11, 1, "mi")),
		"起始 NaN":    romaCandidate(romaUnit(10, 1, "ki "), romaUnit(nan, 1, "mi")),
		"时长 NaN":    romaCandidate(romaUnit(10, nan, "ki "), romaUnit(11, 1, "mi")),
		"起始 +Inf":   romaCandidate(romaUnit(10, 1, "ki "), romaUnit(inf, 1, "mi")),
		"时长 +Inf":   romaCandidate(romaUnit(10, 1, "ki "), romaUnit(11, inf, "mi")),
		"行级起始 NaN":  {Timestamp: nan, Duration: 3, Words: []LyricTextDetailedWord{romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi")}},
		"行级时长 NaN":  {Timestamp: 10, Duration: nan, Words: []LyricTextDetailedWord{romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi")}},
		"行级时长为负":    {Timestamp: 10, Duration: -1, Words: []LyricTextDetailedWord{romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi")}},
		"行级起始 +Inf": {Timestamp: inf, Duration: 3, Words: []LyricTextDetailedWord{romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi")}},
	}
	for name, in := range bad {
		if got := BuildRomaDetailed(in, "ki mi"); len(got.Words) != 0 || got.Timestamp != 0 || got.Duration != 0 {
			t.Errorf("%s：时间轴不合法应为零值，got %+v", name, got)
		}
	}
	good := map[string]LyricTextDetailed{
		"起始相同":    romaCandidate(romaUnit(10, 1, "ki "), romaUnit(10, 1, "mi")),
		"时长为零":    romaCandidate(romaUnit(10, 0, "ki "), romaUnit(11, 0, "mi")),
		"从 0 秒起":  {Timestamp: 0, Duration: 0, Words: []LyricTextDetailedWord{romaUnit(0, 1, "ki "), romaUnit(1, 1, "mi")}},
		"单元早于行起始": romaCandidate(romaUnit(9, 1, "ki "), romaUnit(11, 1, "mi")),
	}
	for name, in := range good {
		if got := BuildRomaDetailed(in, "ki mi"); len(got.Words) != 2 {
			t.Errorf("%s：时间轴合法应有 2 个单元，got %+v", name, got)
		}
	}
}

// 返回的 Words 是新分配的切片：改它不影响候选，改候选也不影响它。QQ 的同一行音译可能配给不止一行
// 主歌词，酷狗与汽水的候选取自主歌词的字，共享底层数组会让一处的改动（套 offset）落到另一处。
//
// 变异自证（隔离的 worktree，实测）：把 `words := make(...)` 换成 `words := d.Words[:0]`（就地
// 过滤、与候选共享底层数组），本例红；去掉末尾的 `d.Words = words` 同样红。
func TestBuildRomaDetailedDoesNotShareWords(t *testing.T) {
	in := romaCandidate(romaUnit(10, 1, "ki "), romaUnit(11, 1, "mi"))
	got := BuildRomaDetailed(in, "ki mi")
	if len(got.Words) != 2 {
		t.Fatalf("应有 2 个单元，got %+v", got.Words)
	}
	got.Words[0].PlayTime = 99
	got.Words[1].Text = "改了"
	if in.Words[0].PlayTime != 0 || in.Words[1].Text != "mi" {
		t.Fatalf("改返回值影响到了候选：%+v", in.Words)
	}
	in.Words[0].Timestamp = 77
	if got.Words[0].Timestamp != 10 {
		t.Fatalf("改候选影响到了返回值：%+v", got.Words)
	}
}
