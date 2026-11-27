package qqmusic

// 本文件只测会话侧歌词状态机不改动交给它的行：setOnlineRaw 存深拷贝；apply 每次从原始行拷贝出新的一份再平移、套 offset，
// 不改动 setOnlineRaw 的实参、保存的在线原始行、结果所带的行，改动给出的行也不影响之后的输出。

import (
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// 连续两次不同偏移，结果都从原始行算起。变异自证：apply 不先 cloneLines（就地平移保存的原始行），或 setOnlineRaw
// 不深拷贝（存实参本身、或只拷外层切片），本例各自变红。
func TestLyricStateOnlineRawUntouched(t *testing.T) {
	raw := shiftFixture()
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(raw)
	applyWant(t, &s, onlineRes(1, -1000), true, true, onlineDelayed1s...)
	applyWant(t, &s, onlineRes(1, 500), true, true, onlineAdvanced500...)
	if !reflect.DeepEqual(raw, shiftFixture()) {
		t.Fatalf("apply changed the lines passed to setOnlineRaw: %+v", raw)
	}

	raw[0].Text = "changed"
	raw[0].Detailed.Words[0].Timestamp = 99
	raw[0].RomaDetailed.Words[1].Timestamp = 99
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)

	out, _ := s.apply(onlineRes(1, -1000), true, stateOffsetSec)
	out[0].Text = "changed"
	out[0].Detailed.Words[0].Timestamp = 99
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
}

// 关联与本地文件结果所带的行：连续两次不同偏移都从结果的原样算起，结果的行不被改动。变异自证：apply 不先 cloneLines，
// 本例即红。
func TestLyricStateResultLinesUntouched(t *testing.T) {
	for _, src := range []lyricSource{srcAssoc, srcLocal} {
		lines := assocFixture("A")
		var s lyricState
		s.reset(1, false, time.Time{})
		applyWant(t, &s, desiredRes(1, src, -2000, lines, "A"), src == srcAssoc, true, assocADelayed2s...)
		applyWant(t, &s, desiredRes(1, src, 0, lines, "A"), src == srcAssoc, true, assocA0...)
		if !reflect.DeepEqual(lines, assocFixture("A")) {
			t.Fatalf("source %d: apply changed the result's lines: %+v", src, lines)
		}
	}
}

// 振假名切片的不可变性：篡改 setOnlineRaw 入参的 Furigana、或篡改 apply 输出的 Furigana，
// 均不影响状态机内部保存的在线原始行、关联/本地结果行与后续输出。
// 变异自证（隔离克隆副本，实测）：删掉 cloneLines 中 Furigana 的 slices.Clone，本例即红。
func TestLyricStateFuriganaImmutable(t *testing.T) {
	want := player.LyricFurigana{
		{Text: "心", Ruby: "こころ"},
		{Text: "の"},
	}

	raw := shiftFixture()
	raw[0].Furigana = player.LyricFurigana{{Text: "心", Ruby: "こころ"}, {Text: "の"}}

	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(raw)

	// 篡改传给 setOnlineRaw 的入参切片。
	raw[0].Furigana[0].Ruby = "tampered-input"
	out1, ch1 := s.apply(onlineRes(1, -1000), true, stateOffsetSec)
	if !ch1 || !reflect.DeepEqual(out1[0].Furigana, want) {
		t.Fatalf("篡改 setOnlineRaw 入参影响了在线输出：changed=%v Furigana=%#v，want %#v", ch1, out1[0].Furigana, want)
	}

	// 篡改 apply 返回的切片，验证后续输出不受污染。
	out1[0].Furigana[0].Ruby = "tampered-output"
	out2, ch2 := s.apply(onlineRes(1, 0), true, stateOffsetSec)
	if !ch2 || !reflect.DeepEqual(out2[0].Furigana, want) {
		t.Fatalf("篡改 apply 输出影响了后续在线输出：changed=%v Furigana=%#v，want %#v", ch2, out2[0].Furigana, want)
	}

	for _, src := range []lyricSource{srcAssoc, srcLocal} {
		lines := assocFixture("F")
		lines[0].Furigana = player.LyricFurigana{{Text: "心", Ruby: "こころ"}, {Text: "の"}}
		var st lyricState
		st.reset(1, false, time.Time{})
		got1, ch := st.apply(desiredRes(1, src, -2000, lines, "F"), src == srcAssoc, stateOffsetSec)
		if !ch || !reflect.DeepEqual(got1[0].Furigana, want) {
			t.Fatalf("source %d 首次 apply：changed=%v Furigana=%#v，want %#v", src, ch, got1[0].Furigana, want)
		}
		got1[0].Furigana[0].Ruby = "tampered-result-output"
		if !reflect.DeepEqual(lines[0].Furigana, want) {
			t.Fatalf("source %d 篡改 apply 输出影响了结果原件：%#v", src, lines[0].Furigana)
		}
		got2, ch := st.apply(desiredRes(1, src, 0, lines, "F"), src == srcAssoc, stateOffsetSec)
		if !ch || !reflect.DeepEqual(got2[0].Furigana, want) {
			t.Fatalf("source %d 二次 apply：changed=%v Furigana=%#v，want %#v", src, ch, got2[0].Furigana, want)
		}
	}
}
