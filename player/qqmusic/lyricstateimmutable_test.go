package qqmusic

// 本文件只测会话侧歌词状态机不改动交给它的行：setOnlineRaw 存深拷贝；apply 每次从原始行拷贝出新的一份再平移、套 offset，
// 不改动 setOnlineRaw 的实参、保存的在线原始行、结果所带的行，改动给出的行也不影响之后的输出。

import (
	"reflect"
	"testing"
)

// 连续两次不同偏移，结果都从原始行算起。变异自证：apply 不先 cloneLines（就地平移保存的原始行），或 setOnlineRaw
// 不深拷贝（存实参本身、或只拷外层切片），本例各自变红。
func TestLyricStateOnlineRawUntouched(t *testing.T) {
	raw := shiftFixture()
	var s lyricState
	s.reset(1)
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
		s.reset(1)
		applyWant(t, &s, desiredRes(1, src, -2000, lines, "A"), src == srcAssoc, true, assocADelayed2s...)
		applyWant(t, &s, desiredRes(1, src, 0, lines, "A"), src == srcAssoc, true, assocA0...)
		if !reflect.DeepEqual(lines, assocFixture("A")) {
			t.Fatalf("source %d: apply changed the result's lines: %+v", src, lines)
		}
	}
}
