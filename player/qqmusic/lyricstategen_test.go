package qqmusic

// 本文件只测会话侧歌词状态机按 gen 取舍结果与 reset（设计 §6.8「播放中」：gen 不是当前的丢弃；「会话的状态」换歌时清空）。

import "testing"

// 过时 gen 丢弃：A（gen 1）→ B（gen 2）→ 又回到 A（gen 3）。迟到的 gen 1、gen 2 期望状态都不接受，gen 1 虽是同一首歌也不
// 接受；将来的 gen 同样不接受。丢弃的结果不成为 refresh 所按的期望状态，也不打乱已采纳的状态。停用不比代次，见
// lyricstatereopen_test.go。
// 变异自证：去掉 gen 核对，或先记下结果再核对 gen，本例各自变红。
func TestLyricStateDropsStaleGen(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.reset(2)
	s.reset(3)
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(2, srcAssoc, 0, assocFixture("B"), "B"), true, false)
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, false)
	wantSource(t, &s, srcNone, "")
	refreshWant(t, &s, true, true, onlineAt0...) // 最后丢弃的是关联 A：若被记下，这里会给出 A

	applyWant(t, &s, onlineRes(3, -1000), true, true, onlineDelayed1s...)
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, false)
	applyWant(t, &s, localRes(4), false, false)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, onlineRes(3, -1000), true, false)
	refreshWant(t, &s, true, false)
}

// reset 清空在线原始行、已采纳的状态与本地路径：之后第一次应用必定要求重发，即使输出与上一首相同（新歌要重新发词）。
// 变异自证：reset 不清已采纳的指纹，或不清在线原始行，本例各自变红。
func TestLyricStateResetClears(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
	s.reset(2)
	wantSource(t, &s, srcNone, "")
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(2, 0), true, true, onlineAt0...)

	s.reset(3) // 没有再存在线原始行：来源为在线即空行
	applyWant(t, &s, onlineRes(3, 0), true, true)

	s.reset(4)
	applyWant(t, &s, localRes(4), false, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)
	s.reset(5)
	wantSource(t, &s, srcNone, "")
	applyWant(t, &s, localRes(5), false, true, localLDelayed1s...)
	s.reset(6)
	applyWant(t, &s, noneRes(6), false, true)
	applyWant(t, &s, noneRes(6), false, false)
}
