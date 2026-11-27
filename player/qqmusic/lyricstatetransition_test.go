package qqmusic

// 本文件只测设计 §6.8「转移」逐条（§7「会话」的转移各一例）：会话侧歌词状态机（lyricState.apply）在每条结果之后
// 给出的行（平移、套 offset 之后）、是否要求重发与来源。

import (
	"testing"
	"time"
)

// 在线关联撤销（设计 §6.8 转移第 1 条）：QQ 把条目改回指向该曲自己的歌词缓存（byUser=0、偏移 0），读取者读该缓存，
// 结果仍为在线关联、偏移 0；换成该缓存的行（这里以 B 代表），原关联 A 的 -2000 偏移不再套用。
func TestLyricStateAssocRevokedToOwnCache(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("B"), "该曲自己的缓存"), true, true, assocB0...)
	wantSource(t, &s, srcAssoc, "")
}

// 在线 → 认出之后换成 QQ 的缓存而行与在线的相同（设计 §8，#244）：折算后的行相同，不发，来源记为关联；再收到同一份
// 不发；随后换成别的行照发。反例：两边都是空行时换来源照发（补取未定之后转为无，会话据此发 index:-1）。
//
// 变异自证（均实测）：去掉「折算后的行相同不发」，第二条红；去掉「行非空」一条，反例红。
func TestLyricStateOnlineToAssocSameLinesNoResend(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
	same := desiredRes(1, srcAssoc, 0, shiftFixture(), "QQ 的缓存")
	applyWant(t, &s, same, true, false)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, same, true, false)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), true, true, assocB0...)
	wantSource(t, &s, srcAssoc, "")

	s.reset(2, false, time.Time{}) // 没有在线原始行
	pending := onlineRes(2, 0)
	pending.pending = true
	applyWant(t, &s, pending, true, true)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, noneRes(2), false, true)
	wantSource(t, &s, srcNone, "")
}

// 在线关联且偏移非 0，条目删除（设计 §6.8 转移第 2 条）：转为在线、偏移 0，用在线原始行，全部时间复原。
func TestLyricStateOnlineEntryRemoved(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, onlineRes(1, 0), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
}

// 关联由 A 改指 B 而 B 暂缺：转为在线、偏移 0（带待重试）；B 读到后转为关联 B。
func TestLyricStateAssocSwitchToMissingThenLoaded(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, true, assocA0...)
	missing := onlineRes(1, 0)
	missing.pending = true
	applyWant(t, &s, missing, true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), true, true, assocB0...)
	wantSource(t, &s, srcAssoc, "")
}

// 本地文件的关联撤销或文件暂缺：转为无，即要求重发、行为空（会话据此发 count:0 与 index:-1）。
func TestLyricStateLocalRevokedClears(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{}) // 编号缺位：没有在线原始行
	applyWant(t, &s, localRes(1), false, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)
	applyWant(t, &s, noneRes(1), false, true)
	wantSource(t, &s, srcNone, "")

	applyWant(t, &s, localRes(1), false, true, localLDelayed1s...)
	missing := noneRes(1)
	missing.pending = true
	applyWant(t, &s, missing, false, true)
	wantSource(t, &s, srcNone, "")
}

// 功能停用：在线关联退回在线、偏移 0。变异自证：停用不看 songHasIDs、一律按无，本例即红。
func TestLyricStateStoppedOnlineFallsBack(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	applyWant(t, &s, stoppedRes(1), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
}

// 功能停用：本地文件清词。
func TestLyricStateStoppedLocalClears(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	applyWant(t, &s, localRes(1), false, true, localLDelayed1s...)
	applyWant(t, &s, stoppedRes(1), false, true)
	wantSource(t, &s, srcNone, "")
}

// 未知不动作：不给出行、不改已采纳的状态，同一期望状态再来仍不要求重发；reset 之后第一条就是未知也不动作。
// 反例：未知带着别的字段、种类不在定义之内，同样不接受。变异自证：未知按停用处理，本例即红。
func TestLyricStateUnknownNoAction(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	unknown := noneRes(1)
	unknown.kind = resUnknown
	applyWant(t, &s, unknown, true, false)
	invalid := onlineRes(1, 0)
	invalid.kind = resDesired + 1
	applyWant(t, &s, invalid, true, false)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, false)
	refreshWant(t, &s, true, false)

	s.reset(2, false, time.Time{})
	applyWant(t, &s, assocResult{gen: 2, kind: resUnknown}, true, false)
	wantSource(t, &s, srcNone, "")
}
