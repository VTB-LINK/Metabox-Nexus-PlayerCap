package helper

// 本文件只测数据不连续日志的限频 discLog：输入累计值与时刻，判定此刻报出多少次，含 restore 退回的次数
// 如何再报（用假时刻，不触碰声卡）。

import (
	"math"
	"testing"
	"time"
)

// discStep 是一次 observe：在起点之后 at 时刻观察到累计值 total，期望报出 want 次。
type discStep struct {
	total uint64
	at    time.Duration
	want  uint64
}

var discStart = time.Unix(1000, 0)

func runDiscSteps(t *testing.T, d *discLog, steps []discStep) {
	t.Helper()
	for i, s := range steps {
		if got := d.observe(s.total, discStart.Add(s.at)); got != s.want {
			t.Fatalf("第 %d 步 observe(%d, +%v) = %d, 期望 %d", i, s.total, s.at, got, s.want)
		}
	}
}

func TestDiscLogObserve(t *testing.T) {
	const big = uint64(1) << 40
	for _, c := range []struct {
		name      string
		steps     []discStep
		wantFlush uint64
	}{
		{"从未出现不连续：一分钟内永不报", []discStep{
			{0, 0, 0}, {0, time.Second, 0}, {0, discLogInterval, 0}, {0, time.Minute, 0},
		}, 0},
		{"首次出现即报，次数为累计值", []discStep{
			{0, 0, 0}, {3, time.Millisecond, 3},
		}, 0},
		{"第一次观察就带累计值也立即报", []discStep{
			{2, 0, 2},
		}, 0},
		{"窗口内的增量只累计不报，留给 flush", []discStep{
			{1, 0, 1}, {2, time.Second, 0}, {4, 5 * time.Second, 0}, {4, discLogInterval - time.Nanosecond, 0},
		}, 3},
		{"窗口到期一次性报出累计次数", []discStep{
			{1, 0, 1}, {2, time.Second, 0}, {5, 5 * time.Second, 0}, {6, discLogInterval, 5},
		}, 0},
		{"到期后的观察仍按待报判定：报出即清零，此后无增量不报", []discStep{
			{1, 0, 1}, {3, time.Second, 0}, {3, discLogInterval, 2}, {3, 3 * discLogInterval, 0},
		}, 0},
		{"报出后重新计窗：到期前的新增不报", []discStep{
			{1, 0, 1}, {2, discLogInterval, 1}, {3, discLogInterval + time.Second, 0}, {3, 2*discLogInterval - time.Nanosecond, 0}, {3, 2 * discLogInterval, 1},
		}, 0},
		{"长时间无不连续后再出现：距上次已满窗口，立即报", []discStep{
			{1, 0, 1}, {1, time.Hour, 0}, {2, time.Hour + time.Second, 1},
		}, 0},
		{"累计值回退视为重置，从 0 起计入、不回绕成巨大值", []discStep{
			{100, 0, 100}, {5, discLogInterval, 5}, {7, 2 * discLogInterval, 2},
		}, 0},
		{"窗口内的重置只累计不报", []discStep{
			{100, 0, 100}, {5, time.Second, 0}, {0, 2 * time.Second, 0},
		}, 5},
		{"回退到 0 不新增待报", []discStep{
			{4, 0, 4}, {0, discLogInterval, 0},
		}, 0},
		{"时钟回拨按未到期处理，追上后再报", []discStep{
			{1, 0, 1}, {3, -time.Hour, 0}, {3, discLogInterval - time.Nanosecond, 0}, {3, discLogInterval, 2},
		}, 0},
		{"一次性大增量", []discStep{
			{big, 0, big}, {math.MaxUint64, discLogInterval, math.MaxUint64 - big},
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var d discLog
			runDiscSteps(t, &d, c.steps)
			if got := d.flush(); got != c.wantFlush {
				t.Fatalf("flush = %d, 期望 %d", got, c.wantFlush)
			}
			if got := d.flush(); got != 0 {
				t.Fatalf("第二次 flush = %d, 期望 0", got)
			}
		})
	}
}

// TestDiscLogFlushThenObserve 断言 flush 清掉的次数不会在之后的 observe 里再报一次。
func TestDiscLogFlushThenObserve(t *testing.T) {
	var d discLog
	runDiscSteps(t, &d, []discStep{{1, 0, 1}, {4, time.Second, 0}})
	if got := d.flush(); got != 3 {
		t.Fatalf("flush = %d, 期望 3", got)
	}
	runDiscSteps(t, &d, []discStep{{4, 2 * discLogInterval, 0}, {5, 3 * discLogInterval, 1}})
}

// TestDiscLogPerCaptureIndependent 断言每路一个实例、互不干扰：一路刚报过不压住另一路的首次报出，
// 一路的待报不出现在另一路。
func TestDiscLogPerCaptureIndependent(t *testing.T) {
	var a, b discLog
	runDiscSteps(t, &a, []discStep{{2, 0, 2}})
	runDiscSteps(t, &b, []discStep{{0, 0, 0}, {1, time.Second, 1}})
	runDiscSteps(t, &a, []discStep{{3, 2 * time.Second, 0}})
	runDiscSteps(t, &b, []discStep{{1, 3 * time.Second, 0}})
	if got := b.flush(); got != 0 {
		t.Fatalf("b.flush = %d, 期望 0", got)
	}
	if got := a.flush(); got != 1 {
		t.Fatalf("a.flush = %d, 期望 1", got)
	}
}

// TestDiscLogRestore 断言 restore 把次数加回待报（饱和不回绕），不改上次报出的时刻与「已报过」：加回的
// 次数在窗口内不报，到期与新增的一并报出，flush 也取得回。
func TestDiscLogRestore(t *testing.T) {
	for _, c := range []struct {
		name      string
		run       func(t *testing.T, d *discLog)
		wantFlush uint64
	}{
		{"加回后窗口内不报，到期一次性报出", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}})
			d.restore(1)
			runDiscSteps(t, d, []discStep{{1, time.Second, 0}, {1, discLogInterval - time.Nanosecond, 0}, {1, discLogInterval, 1}})
		}, 0},
		{"加回的与到期时的新增一并报出", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{2, 0, 2}})
			d.restore(2)
			runDiscSteps(t, d, []discStep{{3, discLogInterval, 3}})
		}, 0},
		{"加回的与窗口内已累计的待报相加", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}, {2, time.Second, 0}})
			d.restore(4)
			runDiscSteps(t, d, []discStep{{2, discLogInterval, 5}})
		}, 0},
		{"连续加回两次累加", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}})
			d.restore(2)
			d.restore(3)
		}, 5},
		{"flush 取回加回的次数", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}})
			d.restore(3)
		}, 3},
		{"restore(0) 无副作用", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}})
			d.restore(0)
			runDiscSteps(t, d, []discStep{{1, discLogInterval, 0}})
		}, 0},
		{"restore 不改「已报过」：从未报过的路首次出现仍立即报", func(t *testing.T, d *discLog) {
			d.restore(0)
			runDiscSteps(t, d, []discStep{{0, 0, 0}, {1, time.Millisecond, 1}})
		}, 0},
		{"加回接近上限时饱和不回绕", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{math.MaxUint64 - 1, 0, math.MaxUint64 - 1}})
			d.restore(math.MaxUint64 - 1)
			d.restore(5)
		}, math.MaxUint64},
		{"加回到上限后 observe 的新增也饱和", func(t *testing.T, d *discLog) {
			runDiscSteps(t, d, []discStep{{1, 0, 1}})
			d.restore(math.MaxUint64)
			runDiscSteps(t, d, []discStep{{6, time.Second, 0}, {6, discLogInterval, math.MaxUint64}})
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var d discLog
			c.run(t, &d)
			if got := d.flush(); got != c.wantFlush {
				t.Fatalf("flush = %d, 期望 %d", got, c.wantFlush)
			}
		})
	}
}
