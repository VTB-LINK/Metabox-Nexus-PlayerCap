package qqmusic

// 本文件只测按本 gen 已接受的期望状态重新折算（lyricState.refresh，设计 §6.8「补取块」：已有本 gen 的期望状态则按它，
// 否则按「在线、偏移 0」）：在线原始行更新之后不丢掉已采纳的关联与偏移。

import "testing"

// 尚未接受任何结果：在线歌曲按在线、偏移 0，编号缺位按无；再来一次不要求重发，随后到达的同样输出也不要求。
func TestLyricStateRefreshDefault(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	refreshWant(t, &s, true, false)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, onlineRes(1, 0), true, false)

	s.reset(2)
	refreshWant(t, &s, false, true)
	refreshWant(t, &s, false, false)
	wantSource(t, &s, srcNone, "")
	applyWant(t, &s, noneRes(2), false, false)
}

// 已采纳在线关联，补取取到在线原始行：refresh 保持关联（不要求重发）。已采纳在线偏移，在线原始行变了：refresh 保持偏移、
// 按新行重发。反例：未知与过时 gen 的结果不成为 refresh 所按的期望状态。
// 变异自证：refresh 一律按「在线、偏移 0」（不按已接受的期望状态），本例即红。
func TestLyricStateRefreshKeepsAdopted(t *testing.T) {
	var s lyricState
	s.reset(1)
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, false)
	wantSource(t, &s, srcAssoc, "")

	applyWant(t, &s, onlineRes(1, -1000), true, true, onlineDelayed1s...)
	changed := shiftFixture()
	changed[1].Text = "b2"
	s.setOnlineRaw(changed)
	refreshWant(t, &s, true, true, onlineDelayed1s[0], "1:b2||@3.000")

	applyWant(t, &s, assocResult{gen: 1, kind: resUnknown}, true, false)
	applyWant(t, &s, desiredRes(0, srcAssoc, 0, assocFixture("A"), "A"), true, false)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineDelayed1s...)
}
