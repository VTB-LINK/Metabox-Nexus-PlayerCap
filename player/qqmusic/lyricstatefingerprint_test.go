package qqmusic

// 本文件只测会话侧歌词状态机何时要求重发（lyricState 的指纹：来源、内容摘要、偏移，设计 §6.8「应用期望状态」第 2 步）。

import (
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// 同一结果再来不要求重发；输出相同的等价写法（待重试与否、停用与在线偏移 0、停用与无）也不要求。
func TestLyricStateSameResultNoResend(t *testing.T) {
	pending := func(r assocResult) assocResult { r.pending = true; return r }
	cases := []struct {
		name         string
		songHasIDs   bool
		first, again assocResult
		want         []string
	}{
		{"在线、偏移非 0", true, onlineRes(1, -1000), onlineRes(1, -1000), onlineDelayed1s},
		{"在线关联（行是新分配的同一内容）", true, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"),
			desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), assocADelayed2s},
		{"本地文件", false, localRes(1), localRes(1), localLDelayed1s},
		{"无", false, noneRes(1), noneRes(1), nil},
		{"在线偏移 0 之后待重试", true, onlineRes(1, 0), pending(onlineRes(1, 0)), onlineAt0},
		{"在线偏移 0 之后停用", true, onlineRes(1, 0), stoppedRes(1), onlineAt0},
		{"停用之后在线偏移 0", true, stoppedRes(1), onlineRes(1, 0), onlineAt0},
		{"无之后停用（编号缺位）", false, noneRes(1), stoppedRes(1), nil},
		{"停用两次（编号缺位）", false, stoppedRes(1), stoppedRes(1), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s lyricState
			s.reset(1, false, time.Time{})
			s.setOnlineRaw(shiftFixture())
			applyWant(t, &s, c.first, c.songHasIDs, true, c.want...)
			applyWant(t, &s, c.again, c.songHasIDs, false)
		})
	}
}

// 只改偏移也要求重发。在线一例最要紧：在线的内容摘要取自在线原始行、不含偏移，只靠指纹里的偏移区分。
// 变异自证：指纹不含偏移，本例即红。
func TestLyricStateOffsetOnlyResends(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
	applyWant(t, &s, onlineRes(1, -500), true, true, onlineDelayed500...)
	applyWant(t, &s, onlineRes(1, -1000), true, true, onlineDelayed1s...)
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, true, assocA0...)
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
}

// 在线原始行变了（补取取到的与先前的不同，含只差一个逐字时间）：来源为在线时要求重发、按新行且保持偏移；重新取到相同内容
// 不要求。来源为关联时在线原始行变了不影响输出。变异自证：在线的指纹不含原始行的摘要，本例即红。
func TestLyricStateOnlineRawChangeResends(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(1, -1000), true, true, onlineDelayed1s...)

	text := shiftFixture()
	text[1].Text = "b2"
	s.setOnlineRaw(text)
	applyWant(t, &s, onlineRes(1, -1000), true, true, onlineDelayed1s[0], "1:b2||@3.000")
	again := shiftFixture()
	again[1].Text = "b2"
	s.setOnlineRaw(again)
	applyWant(t, &s, onlineRes(1, -1000), true, false)

	word := shiftFixture()
	word[1].Text = "b2"
	word[0].RomaDetailed.Words[1].Timestamp = 0.8
	s.setOnlineRaw(word)
	applyWant(t, &s, onlineRes(1, -1000), true, true,
		"0:a||@1.200 d1.200/1.700 x:1.200/1.700 x:1.600/2.100 r1.250/1.750 x:1.250/1.750 x:1.800/2.300", "1:b2||@3.000")

	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, true, assocA0...)
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, false)
}

// 在线原始行为空（换歌拍在线取词失败）而来源为在线：要求重发、行为空，结果自带的行不用。之后存入空切片仍不要求重发，
// 存入非空的行再应用即按它发出。
func TestLyricStateOnlineEmptyRaw(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	applyWant(t, &s, desiredRes(1, srcOnline, 0, assocFixture("X"), ""), true, true)
	wantSource(t, &s, srcOnline, "")
	s.setOnlineRaw([]lyricLine{})
	applyWant(t, &s, onlineRes(1, 0), true, false)
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
}

// 采纳的本地路径跟随最近接受的本地结果：同一份歌词换了音频文件不要求重发，路径更新；来源不是本地文件时为空。
// 反例：在线关联的结果带着本地路径（读取者不会这样给）也不当作采纳的本地文件。
func TestLyricStateLocalPathFollowsAdopted(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	wantSource(t, &s, srcNone, "")
	applyWant(t, &s, localRes(1), false, true, localLDelayed1s...)
	other := localRes(1)
	other.localPath = `D:\music\y.flac`
	applyWant(t, &s, other, false, false)
	wantSource(t, &s, srcLocal, `D:\music\y.flac`)
	applyWant(t, &s, stoppedRes(1), false, true)
	wantSource(t, &s, srcNone, "")

	s = lyricState{} // 停用是会话级终态、reset 不清，下面换一个会话
	s.reset(2, false, time.Time{})
	odd := desiredRes(2, srcAssoc, 0, assocFixture("A"), "A")
	odd.localPath = localFlac
	applyWant(t, &s, odd, true, true, assocA0...)
	wantSource(t, &s, srcAssoc, "")
}

// 仅 Furigana 变化时（文本、时间戳、逐字均相同），lyricState 指纹变化并触发重发；
// Furigana 相同时不无谓重发；跨来源切换仅 Furigana 不同时不被 shownSum 误判为相同而吞掉。
func TestLyricStateFuriganaChangeResends(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	if out, ch := s.apply(onlineRes(1, -1000), true, stateOffsetSec); !ch || len(out) != 2 || out[0].Furigana != nil {
		t.Fatalf("首次无振假名 apply = (%+v, %v)，want changed=true 且 Furigana=nil", out, ch)
	}

	// 仅新增 Furigana，必须触发重发。
	kanaA := player.LyricFurigana{{Text: "a", Ruby: "あ"}}
	withKanaA := shiftFixture()
	withKanaA[0].Furigana = kanaA
	s.setOnlineRaw(withKanaA)
	out, ch := s.apply(onlineRes(1, -1000), true, stateOffsetSec)
	if !ch || !reflect.DeepEqual(out[0].Furigana, kanaA) {
		t.Fatalf("仅新增 Furigana 时 apply = (changed=%v, %#v)，want (true, %#v)", ch, out, kanaA)
	}

	// 再次存入完全相同的 Furigana，不重发。
	sameKanaA := shiftFixture()
	sameKanaA[0].Furigana = player.LyricFurigana{{Text: "a", Ruby: "あ"}}
	s.setOnlineRaw(sameKanaA)
	if out, ch := s.apply(onlineRes(1, -1000), true, stateOffsetSec); ch || out != nil {
		t.Fatalf("相同 Furigana 不应重发：got (%+v, %v)", out, ch)
	}

	// 仅改 Ruby 文本，必须触发重发。
	kanaB := player.LyricFurigana{{Text: "a", Ruby: "い"}}
	withKanaB := shiftFixture()
	withKanaB[0].Furigana = kanaB
	s.setOnlineRaw(withKanaB)
	out, ch = s.apply(onlineRes(1, -1000), true, stateOffsetSec)
	if !ch || !reflect.DeepEqual(out[0].Furigana, kanaB) {
		t.Fatalf("仅修改 Furigana Ruby 时 apply = (changed=%v, %#v)，want (true, %#v)", ch, out, kanaB)
	}

	// 跨来源切换到关联歌词：其余字段全同、仅 Furigana 不同，shownSum 不同故必须重发。
	assocSameExceptKana := shiftFixture()
	assocSameExceptKana[0].Furigana = player.LyricFurigana{{Text: "a", Ruby: "う"}}
	out, ch = s.apply(desiredRes(1, srcAssoc, -1000, assocSameExceptKana, "K"), true, stateOffsetSec)
	if !ch || !reflect.DeepEqual(out[0].Furigana, assocSameExceptKana[0].Furigana) {
		t.Fatalf("跨来源仅 Furigana 不同时应重发：got (changed=%v, %#v)", ch, out)
	}

	// 切回在线且在线行与当前已显示行含 Furigana 完全一致：应由 shownSum 判定相同而不重发。
	onlineMatchesAssoc := shiftFixture()
	onlineMatchesAssoc[0].Furigana = player.LyricFurigana{{Text: "a", Ruby: "う"}}
	s.setOnlineRaw(onlineMatchesAssoc)
	if out, ch := s.apply(onlineRes(1, -1000), true, stateOffsetSec); ch || out != nil {
		t.Fatalf("跨来源全字段含 Furigana 一致时不应重发：got (%+v, %v)", out, ch)
	}
}
