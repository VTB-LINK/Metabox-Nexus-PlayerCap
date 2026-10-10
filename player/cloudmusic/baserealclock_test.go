package cloudmusic

// 本文件只测 baseRealClock 的读写：按给定时刻取读数、对表回拨时的行匹配下限、seek 置位、起走与冻结。
// 不测 runSession 的各个位置使用点取的是 at 还是 lineAt（轮询循环没有离线夹具）。时间一律由用例给定。

import (
	"testing"
	"time"
)

func checkClock(t *testing.T, c *baseRealClock, at time.Time, wantAt, wantLine float32, what string) {
	t.Helper()
	if got := c.at(at); !nearly(got, wantAt) {
		t.Errorf("%s：at = %.4f，期望 %.4f", what, got, wantAt)
	}
	if got := c.lineAt(at); !nearly(got, wantLine) {
		t.Errorf("%s：lineAt = %.4f，期望 %.4f", what, got, wantLine)
	}
}

// TestClockReadsAtGivenTime 读数按传入的时刻取，不取当前时刻；不在走时不前进。
//
// 变异自证：at 改回 `c.BaseRealTime + time.Since(c.AnchorTime)`，本例红（锚点在 2023 年，
// 读数成了几千万秒，实测）。
func TestClockReadsAtGivenTime(t *testing.T) {
	var c baseRealClock
	checkClock(t, &c, secAt(3), 0, 0, "零值、不在走")

	c.setPlaying(true, secAt(10))
	checkClock(t, &c, secAt(10), 0, 0, "起走的那一刻")
	checkClock(t, &c, secAt(11.5), 1.5, 1.5, "起走 1.5 秒后")
	checkClock(t, &c, secAt(9), 0, 0, "早于锚点的时刻：不后退")

	// 重复置为在播，读数不变。去掉 setPlaying 里同状态的提前返回是等价变异（在同一读数上重新落锚），
	// 这里不红（实测）。
	c.setPlaying(true, secAt(12))
	checkClock(t, &c, secAt(13), 3, 3, "重复置为在播之后")
}

// TestClockForwardAdjustSetsNoFloor 向前对表之后行匹配与对外位置同一个读数。
//
// 把 adjust 改成向前时也记下限是等价变异：记下的是对表前的读数，低于对表后的时钟，不起作用，
// 本例不红（实测）。
func TestClockForwardAdjustSetsNoFloor(t *testing.T) {
	var c baseRealClock
	c.setPlaying(true, secAt(0))
	c.adjust(10.70, secAt(10.34)) // 时钟 10.34 → 10.70
	checkClock(t, &c, secAt(10.34), 10.70, 10.70, "向前对表的那一刻")
	checkClock(t, &c, secAt(10.84), 11.20, 11.20, "0.5 秒后")
}

// TestClockBackwardAdjustKeepsLineFloor 回拨之后对外位置取回拨后的值；行匹配取回拨前的值，直到
// 时钟走过它，行号因此不回退。
//
// 变异自证：回拨不记下限、lineAt 不取下限（直接返回 at），本例均红（实测）。
func TestClockBackwardAdjustKeepsLineFloor(t *testing.T) {
	var c baseRealClock
	c.setPlaying(true, secAt(0))
	c.adjust(0.805, secAt(1.055)) // 时钟 1.055 → 0.805，下限 1.055
	checkClock(t, &c, secAt(1.055), 0.805, 1.055, "回拨的那一刻")
	checkClock(t, &c, secAt(1.155), 0.905, 1.055, "回拨 0.1 秒后")
	checkClock(t, &c, secAt(1.305), 1.055, 1.055, "时钟恰好追上下限")
	checkClock(t, &c, secAt(1.405), 1.155, 1.155, "时钟走过下限之后")
}

// TestClockSecondBackwardAdjustKeepsHigherFloor 下限未走完又回拨一次：下限仍是较早那次的行匹配位置。
//
// 变异自证：回拨时下限记 at 而不是 lineAt，本例红（下限掉到 0.905，实测）。
func TestClockSecondBackwardAdjustKeepsHigherFloor(t *testing.T) {
	var c baseRealClock
	c.setPlaying(true, secAt(0))
	c.adjust(0.805, secAt(1.055))
	c.adjust(0.850, secAt(1.155)) // 时钟 0.905 → 0.850
	checkClock(t, &c, secAt(1.155), 0.850, 1.055, "第二次回拨的那一刻")
}

// TestClockJumpClearsFloor seek 检测置位之后行号跟着位置走，下限清掉。
//
// 变异自证：jump 不清下限，「回跳到下限以下」红（lineAt 仍是 1.055，实测）。
func TestClockJumpClearsFloor(t *testing.T) {
	var c baseRealClock
	c.setPlaying(true, secAt(0))
	c.adjust(0.805, secAt(1.055))
	c.jump(0, secAt(1.155))
	checkClock(t, &c, secAt(1.155), 0, 0, "回跳到下限以下")
	checkClock(t, &c, secAt(1.655), 0.5, 0.5, "回跳 0.5 秒后")

	c.adjust(0.20, secAt(1.655)) // 再回拨一次：0.5 → 0.2
	c.jump(30, secAt(1.755))
	checkClock(t, &c, secAt(1.755), 30, 30, "前跳")
}

// TestClockFreezeAfterBackwardAdjust 回拨之后、时钟走过下限之前暂停：冻结在时钟读数（playback_pause
// 的位置与其它对外事件同一条时间线），下限保留；恢复后行匹配仍不低于下限。
//
// 变异自证：冻结取 lineAt（冻结值成了 1.055）、冻结时清掉下限（暂停中 lineAt 掉到 0.905），本例
// 均红（实测）。
func TestClockFreezeAfterBackwardAdjust(t *testing.T) {
	var c baseRealClock
	c.setPlaying(true, secAt(0))
	c.adjust(0.805, secAt(1.055))

	c.setPlaying(false, secAt(1.155))
	checkClock(t, &c, secAt(1.155), 0.905, 1.055, "冻结的那一刻")
	checkClock(t, &c, secAt(9), 0.905, 1.055, "暂停中不前进")

	c.setPlaying(true, secAt(9))
	checkClock(t, &c, secAt(9.05), 0.955, 1.055, "恢复后、时钟走过下限之前")
	checkClock(t, &c, secAt(9.30), 1.205, 1.205, "恢复后、时钟走过下限之后")
}
