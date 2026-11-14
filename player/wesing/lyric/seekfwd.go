package lyric

// 前跳判定与它的基准（#177，设计见 doc/design/wesing-forward-seek-design.md §4.1、§4.2）。两种取数模式共用，
// 不读进程。与 PCMClock 同包，是为了包内的测试能把它与速率核对（PCMClock.Observe）串起来驱动。

import "time"

// SeekJumpSec 是判为跳转的门槛（秒）：前跳的超前量、回跳的回退量都要超过它。
const SeekJumpSec float32 = 2.0

// SeekBase 记录前跳判定的基准：上一拍报出的位置 v，以及基准时刻 base——读到 v 之前、最后一次读到旧值的
// 那一拍的时刻。v 生效不早于那次读取，所以 v 首次被读到之前停住了多久（慢轮询、调度延迟、兜底读数冻结），
// 都算进经过时间；取 v 首次读到的时刻会丢掉这一段，松手追上时误判为前跳（设计审第 1 轮 F1）。
//
// 本次轮询的第一个读数之前没有读数，它生效于何时未知，第一次变化之前 known 为假、不判前跳。
type SeekBase struct {
	v      float32
	base   time.Time
	lastAt time.Time // 上一次读数成功那一拍的时刻
	have   bool      // 已有第一个读数
	known  bool      // base 已知：起始读数已变化过
}

// Forward 判本拍是否为前跳：r 是本拍外推之前的直读值，now 是本拍读数成功之后取的时刻，paused 是本拍判定
// 之前的暂停状态，minimized 是本拍是否判为最小化（兜底模式外推期间）。
func (s *SeekBase) Forward(r float32, now time.Time, paused, minimized bool) bool {
	return seekForward(s.v, float32(now.Sub(s.base).Seconds()), r, s.known, paused, minimized)
}

// Settle 在每拍末尾用本拍报出的位置（外推之后的值）更新基准。起始读数第一次变化的那一拍判不了是不是前跳，
// 之后 known 才为真。#190 起速率核对按自己的上一拍判每拍推进，不再需要知道哪一拍是第一次变化。
func (s *SeekBase) Settle(pos float32, now time.Time) {
	switch {
	case !s.have:
		s.v, s.have = pos, true
	case pos != s.v:
		s.v, s.base, s.known = pos, s.lastAt, true
	}
	s.lastAt = now
}

// seekForward 是前跳判据：基准已知、未判暂停、本拍未判最小化，且读数比「上一拍位置 + 自基准时刻起的经过
// 时间」超前 SeekJumpSec 以上（严格大于）。经过时间不乘速率：基准随读数变化逐拍更新，速率偏差不累积
// （设计 §4.4）。
func seekForward(v, elapsed, r float32, known, paused, minimized bool) bool {
	return known && !paused && !minimized && r-(v+elapsed) > SeekJumpSec
}
