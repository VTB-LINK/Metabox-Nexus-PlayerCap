package qqmusic

// 本文件只测会话侧歌词状态机按完整代次（gen，reopen）取舍结果与停用这一会话级终态（设计 §6.8「输出」「会话的状态」
// 「重开」）：reopened 只换代次、不清在线原始行与已接受的结果；apply 只收代次完全相同的期望状态；停用不比代次，此后任何
// 结果都不收，reset 也不清它，refresh 按无条目折算。

import (
	"fmt"
	"testing"
	"time"
)

// atReopen 把结果的 reopen 换成 reopen。
func atReopen(r assocResult, reopen uint64) assocResult {
	r.reopen = reopen
	return r
}

// 重开只换代次：已接受的关联与指纹保留，refresh 不要求重发；在线原始行仍在，新代次给出在线时按它发出。
// 变异自证（实测）：reopened 像 reset 那样清空（refresh 即要求重发、在线一例为空行），本例红。
func TestLyricStateReopenedKeepsAdopted(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	s.reopened(1)
	refreshWant(t, &s, true, false)
	wantSource(t, &s, srcAssoc, "")
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), 1), true, false) // 同一份：指纹不变
	applyWant(t, &s, atReopen(onlineRes(1, 0), 1), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
}

// 期望状态须代次完全相同：（1，0）的在（1，1）下被拒，将来的（1，2）同样被拒；（1，1）的被收。被拒的不成为 refresh 所按的
// 结果，未知也不动作。
// 变异自证（实测）：apply 只比 gen，本例与下面两例都红。
func TestLyricStateRejectsOtherReopen(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...) // 换歌拍按在线、偏移 0 发出
	s.reopened(1)
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, false)
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), 2), true, false)
	applyWant(t, &s, atReopen(onlineRes(1, -1000), 0), true, false)
	applyWant(t, &s, assocResult{gen: 1, reopen: 1, kind: resUnknown}, true, false)
	wantSource(t, &s, srcOnline, "")
	refreshWant(t, &s, true, false) // 被拒的没有被记下：若记下了关联 A，这里会给出 A
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), 1), true, true, assocB0...)
	wantSource(t, &s, srcAssoc, "")
}

// reset 换到新的 gen 时 reopen 归零：重开过三次之后换歌，新 gen 的（2，0）被收，带着旧 reopen 的（2，3）与旧 gen 的
// （1，3）都被拒。变异自证（实测）：reset 不把 reopen 归零，本例红。
func TestLyricStateResetZeroesReopen(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.reopened(3)
	s.reset(2, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, atReopen(desiredRes(2, srcAssoc, 0, assocFixture("A"), "A"), 3), true, false)
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), 3), true, false)
	wantSource(t, &s, srcNone, "")
	applyWant(t, &s, desiredRes(2, srcAssoc, 0, assocFixture("A"), "A"), true, true, assocA0...)
}

// 连续两次重开：（1，2）下只收（1，2）的，（1，1）的迟到结果与（1，0）的残留都被拒。
func TestLyricStateTwoReopens(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	s.reopened(1)
	s.reopened(2)
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), 1), true, false)
	applyWant(t, &s, desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, false)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), 2), true, true, assocB0...)
}

// 停用不比代次（会话级终态）：（1，0）的停用在（1，1）下收；（1，0）与（1，3）的停用在（2，0）下收，将来的（3，0）也收。
// 有编号时转为在线、偏移 0（用在线原始行），编号缺位时为无。
// 变异自证（实测）：停用也比代次，各例都不收而红；refresh 不查停用（停用之后仍按此前接受的关联折算），各例都红。
func TestLyricStateStoppedAnyGeneration(t *testing.T) {
	assocA := desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A")
	t.Run("（1，0）的停用在（1，1）下", func(t *testing.T) {
		var s lyricState
		s.reset(1, false, time.Time{})
		s.setOnlineRaw(shiftFixture())
		applyWant(t, &s, assocA, true, true, assocADelayed2s...)
		s.reopened(1)
		applyWant(t, &s, stoppedRes(1), true, true, onlineAt0...)
		wantSource(t, &s, srcOnline, "")
	})
	for _, stop := range []assocResult{stoppedRes(1), atReopen(stoppedRes(1), 3), stoppedRes(3)} {
		t.Run(fmt.Sprintf("（%d，%d）的停用在（2，0）下", stop.gen, stop.reopen), func(t *testing.T) {
			var s lyricState
			s.reset(1, false, time.Time{})
			applyWant(t, &s, assocA, true, true, assocADelayed2s...)
			s.reset(2, false, time.Time{})
			s.setOnlineRaw(shiftFixture())
			applyWant(t, &s, desiredRes(2, srcAssoc, 0, assocFixture("B"), "B"), true, true, assocB0...)
			applyWant(t, &s, stop, true, true, onlineAt0...)
			wantSource(t, &s, srcOnline, "")
		})
	}
	t.Run("编号缺位", func(t *testing.T) {
		var s lyricState
		s.reset(1, false, time.Time{})
		s.reset(2, false, time.Time{})
		applyWant(t, &s, localRes(2), false, true, localLDelayed1s...)
		applyWant(t, &s, stoppedRes(1), false, true)
		wantSource(t, &s, srcNone, "")
	})
}

// 停用之后任何代次的结果都不收：同代次、重开之后、新 gen 的期望状态，在线带偏移的，本地文件的，再一次停用。reset（换歌）
// 也不清停用：新歌的 refresh 按无条目（有编号为在线、偏移 0，编号缺位为无），此后的期望状态照样被拒。
// 变异自证（实测）：reset 清掉停用，换歌之后的期望状态被收而红；apply 不先查停用（只靠 refresh 按无条目），reset 之后尚未
// 给出时到的那一条被折算、要求重发而红。
func TestLyricStateNothingAfterStopped(t *testing.T) {
	var s lyricState
	s.reset(1, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), true, true, assocADelayed2s...)
	applyWant(t, &s, stoppedRes(1), true, true, onlineAt0...)
	for _, res := range []assocResult{
		desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"),
		onlineRes(1, -1000),
		atReopen(desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), 1),
		desiredRes(2, srcAssoc, 0, assocFixture("B"), "B"),
		stoppedRes(1),
	} {
		applyWant(t, &s, res, true, false)
	}
	s.reopened(1)
	applyWant(t, &s, atReopen(desiredRes(1, srcAssoc, 0, assocFixture("B"), "B"), 1), true, false)
	refreshWant(t, &s, true, false)
	wantSource(t, &s, srcOnline, "")

	s.reset(2, false, time.Time{})
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...) // 新歌：在线、偏移 0
	applyWant(t, &s, desiredRes(2, srcAssoc, 0, assocFixture("B"), "B"), true, false)
	applyWant(t, &s, onlineRes(2, -1000), true, false)
	refreshWant(t, &s, true, false)
	wantSource(t, &s, srcOnline, "")

	s.reset(3, false, time.Time{}) // 编号缺位：无。reset 之后尚未给出时到的结果同样不收：不折算、不要求重发
	applyWant(t, &s, localRes(3), false, false)
	refreshWant(t, &s, false, true)
	applyWant(t, &s, localRes(3), false, false)
	wantSource(t, &s, srcNone, "")
}
