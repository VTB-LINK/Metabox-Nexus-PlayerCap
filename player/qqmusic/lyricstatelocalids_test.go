package qqmusic

// 本文件只测会话侧歌词状态机对「有编号时的本地文件结果」照常应用（设计 §7「会话」第 5 版两条）：第 5 版读取者在有编号时也会
// 给出本地文件的结果，状态机不改，这里钉住它的行为。

import "testing"

// 有编号（songHasIDs 为真）时：
//   - 直接收到本地文件的结果 → 给出本地歌词（条目偏移 -1000），采纳的路径为该文件；
//   - 先按在线给出、再收到本代次的本地结果 → 要求重发一次，再收到同一份不重发；本地的条目随后消失（读取者改给在线）→ 退回在线；
//   - 新代次（编号由缺到有，gen 加一）收到同一份本地结果 → reset 之后照常给出一次；
//   - 停用 → 有编号退回在线、偏移 0。
func TestLyricStateLocalWithIDs(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, localRes(1), true, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)

	s.reset(2)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
	applyWant(t, &s, localRes(2), true, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)
	applyWant(t, &s, localRes(2), true, false)
	applyWant(t, &s, onlineRes(2, 0), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")

	s.reset(3) // 编号缺位的代次
	applyWant(t, &s, localRes(3), false, true, localLDelayed1s...)
	s.reset(4) // 编号由缺到有
	s.setOnlineRaw(shiftFixture())
	applyWant(t, &s, localRes(4), true, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)
	applyWant(t, &s, localRes(4), true, false)

	applyWant(t, &s, stoppedRes(4), true, true, onlineAt0...)
	wantSource(t, &s, srcOnline, "")
}
