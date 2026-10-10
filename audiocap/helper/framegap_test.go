package helper

// 本文件只测出帧的墙钟兜底：clockDue 的判定，以及 pumpFrames 在数据断续、完全没有数据、连续到来
// 三种读包时序下两帧间隔的上界（用假的读包函数与假时钟，不触碰声卡）。

import (
	"errors"
	"testing"
	"time"
)

var errStopPump = errors.New("测试结束")

// fakeClock 是只由测试推进的时钟。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// step 是假读包函数的一步：先让时钟走 wait，再返回 frames 个采样帧（0 即没有数据）。
type step struct {
	wait   time.Duration
	frames int
	silent bool
}

// runPump 以 steps 循环 rounds 轮驱动 pumpFrames，返回每帧的出帧时刻（相对起点）与新增样本数。
// 读到的非静音样本恒为 0.5。
func runPump(t *testing.T, fps int, steps []step, rounds int) (at []time.Duration, fresh []int) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	start := clk.t
	i := 0
	read := func(dst []float32) (int, bool, error) {
		if i == rounds*len(steps) {
			return 0, false, errStopPump
		}
		s := steps[i%len(steps)]
		i++
		clk.t = clk.t.Add(s.wait)
		for j := 0; j < s.frames*2; j++ {
			dst[j] = 0.5
		}
		return s.frames, s.silent, nil
	}
	emit := func(_ []float32, _ [2]float32, n int) {
		at = append(at, clk.t.Sub(start))
		fresh = append(fresh, n)
	}
	fr := newFramer(hopFor(testSR, fps), 1024)
	err := pumpFrames(read, clk.now, make(chan struct{}), fr, make([]float32, testSR*2/10), 2, time.Second/time.Duration(fps), emit)
	if err != errStopPump {
		t.Fatalf("pumpFrames 应返回读包的错误, 得 %v", err)
	}
	return at, fresh
}

// maxGap 返回相邻两帧（含起点到首帧）间隔的最大值。
func maxGap(at []time.Duration) time.Duration {
	var prev, worst time.Duration
	for _, a := range at {
		worst = max(worst, a-prev)
		prev = a
	}
	return worst
}

// 判定：读到数据时以 maxFrameGap 为上限，没读到数据时以帧间隔为上限（不超过 maxFrameGap）。
//
// 变异自证（隔离副本，实测）：读到数据时恒返回 false → 「读到数据、满 100ms」一行红；没读到数据时也用
// maxFrameGap → 「没读到数据、满一个帧间隔」一行红；去掉对帧间隔的封顶 → 末两行红。
func TestClockDue(t *testing.T) {
	const iv = 40 * time.Millisecond
	for _, c := range []struct {
		name     string
		since    time.Duration
		gotData  bool
		interval time.Duration
		want     bool
	}{
		{"读到数据、不足 100ms", 99 * time.Millisecond, true, iv, false},
		{"读到数据、满 100ms", 100 * time.Millisecond, true, iv, true},
		{"读到数据、已满帧间隔但不足 100ms", 60 * time.Millisecond, true, iv, false},
		{"没读到数据、不足帧间隔", 39 * time.Millisecond, false, iv, false},
		{"没读到数据、满一个帧间隔", 40 * time.Millisecond, false, iv, true},
		{"没读到数据、帧间隔长于 100ms 时以 100ms 为限", 100 * time.Millisecond, false, time.Second, true},
		{"没读到数据、帧间隔长于 100ms、不足 100ms", 99 * time.Millisecond, false, time.Second, false},
	} {
		if got := clockDue(c.since, c.gotData, c.interval); got != c.want {
			t.Errorf("%s: clockDue(%v, %v, %v) = %v, 期望 %v", c.name, c.since, c.gotData, c.interval, got, c.want)
		}
	}
}

// 数据断续到来、始终凑不满一帧：25 帧每秒（hop=1920）下每 90ms 读到一个 480 帧的非静音包，从不读到
// 0 帧。两帧的间隔不得超过 maxFrameGap 加一个包的到达间隔（190ms），出的是残帧。管理器的
// noCaptureAfter（300ms）与 hangThreshold 依赖这一上界。
//
// 变异自证（隔离副本，实测）：clockDue 只在没读到数据时才可能为真（读到数据恒 false）→ 20 个包只凑满
// 5 帧、间隔 360ms，红。
func TestPumpFramesTrickleBounded(t *testing.T) {
	at, fresh := runPump(t, 25, []step{{90 * time.Millisecond, 480, false}}, 20)
	if len(at) == 0 {
		t.Fatalf("始终不出帧")
	}
	if g := maxGap(at); g > maxFrameGap+90*time.Millisecond {
		t.Fatalf("两帧最大间隔 %v, 上限 %v", g, maxFrameGap+90*time.Millisecond)
	}
	for i, n := range fresh {
		if n != 960 {
			t.Fatalf("第 %d 帧 fresh=%d, 期望 960（两个包的残帧）", i, n)
		}
	}
}

// 完全没有数据：读包每 100ms 超时返回 0 帧，每次返回都出一帧保活零帧；读包略早于 100ms 返回
// （94ms）时同样每次都出。
//
// 变异自证（隔离副本，实测）：没读到数据时也用 maxFrameGap → 94ms 一组隔一次才出帧（间隔 188ms），红。
func TestPumpFramesNoDataKeepalive(t *testing.T) {
	for _, wait := range []time.Duration{100 * time.Millisecond, 94 * time.Millisecond} {
		at, fresh := runPump(t, 15, []step{{wait, 0, false}}, 10) // 15 帧每秒：帧间隔 66.7ms，取值范围内最长
		if len(at) != 10 {
			t.Fatalf("读包间隔 %v: 10 次超时出了 %d 帧, 期望每次一帧", wait, len(at))
		}
		if g := maxGap(at); g != wait {
			t.Fatalf("读包间隔 %v: 两帧最大间隔 %v", wait, g)
		}
		for i, n := range fresh {
			if n != 0 {
				t.Fatalf("第 %d 帧 fresh=%d, 期望保活零帧", i, n)
			}
		}
	}
}

// 兜底出帧之后从该时刻重新计时：读包每 30ms 返回 0 帧（帧间隔 40ms）时每隔一次才满一个帧间隔，
// 10 次读包出 5 帧。
//
// 变异自证（隔离副本，实测）：出帧后不更新上次出帧时刻 → 第 2 次读包起每次都出帧（9 帧），红。
func TestPumpFramesRestartsClockAfterFlush(t *testing.T) {
	at, _ := runPump(t, 25, []step{{30 * time.Millisecond, 0, false}}, 10)
	if len(at) != 5 {
		t.Fatalf("出帧数 = %d, 期望 5（60ms 一帧）: %v", len(at), at)
	}
}

// 连续的数据流不受墙钟影响：读包每 10ms 返回 480 帧，各帧率下 1 秒内的帧数等于帧率，且都是整帧。
//
// 变异自证（隔离副本，实测）：maxFrameGap 改为 50ms → 15 帧每秒下 1 秒出 20 帧（切出残帧），红；
// 按样本数出帧后不更新上次出帧时刻 → 同样出 20 帧，红。
func TestPumpFramesSteadyStreamUnaffected(t *testing.T) {
	for _, fps := range []int{15, 25, 30, 60} {
		at, fresh := runPump(t, fps, []step{{10 * time.Millisecond, 480, false}}, 100)
		if len(at) != fps {
			t.Fatalf("%d 帧每秒: 1 秒内出 %d 帧", fps, len(at))
		}
		for i, n := range fresh {
			if n != hopFor(testSR, fps) {
				t.Fatalf("%d 帧每秒第 %d 帧 fresh=%d, 期望整帧 %d", fps, i, n, hopFor(testSR, fps))
			}
		}
	}
}

// 静音包照样按样本数出保活零帧，节拍同帧率：25 帧每秒下 1 秒的静音包出 25 帧。
//
// 变异自证（隔离副本，实测）：framer 的静音包不推进计数 → 只剩墙钟兜底，1 秒内出 10 帧，红。
func TestPumpFramesSilentPacketsKeepRate(t *testing.T) {
	at, fresh := runPump(t, 25, []step{{10 * time.Millisecond, 480, true}}, 100)
	if len(at) != 25 {
		t.Fatalf("1 秒静音包出 %d 帧, 期望 25", len(at))
	}
	for i, n := range fresh {
		if n != 0 {
			t.Fatalf("第 %d 帧 fresh=%d, 期望保活零帧", i, n)
		}
	}
}

// 停止信号已关闭时不读包、不出帧，返回 nil；读包出错时原样返回该错误，不再出帧。
//
// 变异自证（隔离副本，实测）：去掉循环开头对 done 的检查 → 读包被调用，红；读包出错后仍走到兜底
// 判定 → 多出一帧，红。
func TestPumpFramesStops(t *testing.T) {
	done := make(chan struct{})
	close(done)
	reads, emits := 0, 0
	// 读包若被调用即返回错误：循环少了对 done 的检查时以返回值非 nil 变红，而不是挂死。
	read := func([]float32) (int, bool, error) { reads++; return 0, false, errStopPump }
	emit := func([]float32, [2]float32, int) { emits++ }
	clk := &fakeClock{t: time.Unix(1000, 0)}
	if err := pumpFrames(read, clk.now, done, newFramer(1920, 1024), make([]float32, 960), 2, 40*time.Millisecond, emit); err != nil {
		t.Fatalf("done 已关闭应返回 nil, 得 %v", err)
	}
	if reads != 0 || emits != 0 {
		t.Fatalf("done 已关闭仍读包 %d 次、出帧 %d 次", reads, emits)
	}

	// 读包出错：此前时钟已走过 1 秒，若出错后仍做兜底判定会多出一帧。
	read = func([]float32) (int, bool, error) {
		clk.t = clk.t.Add(time.Second)
		return 0, false, errStopPump
	}
	if err := pumpFrames(read, clk.now, make(chan struct{}), newFramer(1920, 1024), make([]float32, 960), 2, 40*time.Millisecond, emit); err != errStopPump {
		t.Fatalf("读包出错应返回该错误, 得 %v", err)
	}
	if emits != 0 {
		t.Fatalf("读包出错后仍出帧 %d 次", emits)
	}
}
