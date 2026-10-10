package applemusic

// 本文件只测 clock：整秒位置读数 → 连续时钟 + 播放 / 暂停 / 跳转判定。

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

// atMs 按整毫秒给时刻：量「持续了多少毫秒」的用例用它，at 的浮点秒换算会差出一纳秒。
func atMs(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// 看到开播：位置从 0 起按墙钟走。真机上读数的第一跳在开播后 1.28~1.54s 才来、之后约每秒一跳；只要
// 每一跳都落在 [N+flipLagMin, N+flipLagMax] 里就不校正，位置等于开播以来的墙钟秒数。
//
// 变异自证：把 observe 播放分支改成每一跳都落锚到 raw（旧实现），1.45s 那一跳把位置从 1.45 拉回 1.0，
// 2.0s 处读到 1.55 而红。
func TestClockRunsFromStart(t *testing.T) {
	var c clock
	c.start(at(0), 300)
	for _, s := range []float64{0.05, 0.5, 1.0, 1.4} {
		if ev := c.observe(0, at(s)); ev != evNone {
			t.Fatalf("%.2fs：开播头一秒多读数还是 0，不应有事件，got %v", s, ev)
		}
	}
	if got := c.live(at(1.0)); !near(got, 1.0) {
		t.Fatalf("1.0s 处 live=%.3f, want 1.0", got)
	}
	c.observe(1, at(1.45))
	c.observe(1, at(2.0))
	if got := c.live(at(2.0)); !near(got, 2.0) {
		t.Fatalf("2.0s 处 live=%.3f, want 2.0（第一跳落在范围内，不校正）", got)
	}
	c.observe(2, at(2.47))
	c.observe(3, at(3.5))
	if got := c.live(at(3.6)); !near(got, 3.6) {
		t.Fatalf("3.6s 处 live=%.3f, want 3.6", got)
	}
}

// 位置偏离了读数允许的范围，就夹回最近的边：慢了（低于 N+flipLagMin）拉到 N+flipLagMin，快了（高于
// N+flipLagMax+距上次读数）压到那里；在范围内不动。
//
// 变异自证：把 observe 里 `case est < lo` 的落锚去掉，「慢了」一例仍是 11.0 而红；把 hi 里加的「距上次
// 读数」去掉，「读得晚」一例被错误地压回 11.63 而红。
func TestClockClampedIntoFlipWindow(t *testing.T) {
	cases := []struct {
		name    string
		est     float64 // 读数为 10 时的位置估计
		obsAt   float64 // 上一次读数（仍是 10）的时刻
		flipAt  float64 // 读到 11 的时刻
		wantPos float64 // 读到 11 那一刻的位置
	}{
		{"范围内不动", 10.5, 0.95, 1.0, 11.5},
		{"慢了拉上来", 10.0, 0.95, 1.0, 11 + flipLagMin},
		{"快了压下去", 10.9, 0.95, 1.0, 11 + flipLagMax + 0.05},
		{"读得晚（全扫挡了 0.4s）不算快", 10.5, 1.0, 1.4, 11.9},
	}
	for _, tc := range cases {
		var c clock
		c.reset(10, tc.est, at(0), true, 300)
		c.observe(10, at(tc.obsAt))
		if ev := c.observe(11, at(tc.flipAt)); ev != evNone {
			t.Fatalf("%s：小偏差只校正、不报事件，got %v", tc.name, ev)
		}
		if got := c.live(at(tc.flipAt)); !near(got, tc.wantPos) {
			t.Errorf("%s：live=%.3f, want %.3f", tc.name, got, tc.wantPos)
		}
	}
}

// 读数不跳，位置就不越过 lastRaw+1+flipLagMax：真实位置若过了它，读数早该跳了。缓冲、暂停尚未判出时
// 不往前冲。
//
// 变异自证：去掉 live 里的这道上限，2.5s 处读到 13.0 而红。
func TestClockDoesNotRunPastNextFlip(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), true, 300)
	c.relaxAt = at(-100)
	c.observe(10, at(1.5))
	if got := c.live(at(2.5)); !near(got, 10+1+flipLagMax) {
		t.Fatalf("读数停在 10 时 live=%.3f, want %.3f", got, 10+1+flipLagMax)
	}
}

// 停更 1.6s 判暂停，冻结在 lastRaw+1（真实位置在 lastRaw+0.38 ~ lastRaw+1.63 之间的某处停下，取中间）；
// 之后连着前进两跳判恢复，只报一次，位置按 读数+flipLagMid 接上。
//
// 变异自证：把 freeze 的落点 `c.lastRaw+1` 改成 `c.lastRaw`，冻结在 10 而红；把暂停分支的落锚
// `raw+flipLagMid` 改成 `raw`，恢复时 live 是 12 而红。
func TestClockPauseResume(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), true, 300)
	c.relaxAt = at(-100) // 开播已久：测的是常规门槛 pauseThreshold，不是开播放宽期
	if ev := c.observe(10, at(1.5)); ev != evNone {
		t.Fatalf("1.5s 未到暂停阈值，got %v", ev)
	}
	if ev := c.observe(10, at(1.7)); ev != evPause {
		t.Fatalf("1.7s 应判暂停，got %v", ev)
	}
	if got := c.live(at(5)); !near(got, 11) {
		t.Fatalf("暂停后冻结在 %.3f，want 11", got)
	}
	if ev := c.observe(11, at(9)); ev != evNone {
		t.Fatalf("暂停中第一跳只重锚、等下一跳确认，got %v", ev)
	}
	if ev := c.observe(12, at(10)); ev != evResume {
		t.Fatalf("连着第二跳应判恢复，got %v", ev)
	}
	if got := c.live(at(10)); !near(got, 12+flipLagMid) {
		t.Fatalf("恢复时 live=%.3f, want %.3f", got, 12+flipLagMid)
	}
	if ev := c.observe(13, at(11)); ev != evNone {
		t.Fatalf("恢复后的正常推进不应再报事件，got %v", ev)
	}
}

// 审查 P1-7：暂停中拖动只重锚、不报事件；之后恢复只报一次 evResume。目标读数持续满 jumpHold 才重锚，
// 与播放中的跳转同一套确认。
//
// 变异自证：把 observe 暂停分支里「一跳」判据的上限去掉（任何前进都算一跳），40s 那一跳
// 被当成恢复的第一跳、当场重锚而红。
func TestClockSeekWhilePaused(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), false, 300)
	if ev := c.observe(50, at(1)); ev != evNone {
		t.Fatalf("暂停中第一次读到新位置先不认，got %v", ev)
	}
	if got := c.live(at(1)); !near(got, 10.5) {
		t.Fatalf("未确认前位置不动，live=%.3f, want 10.5", got)
	}
	if ev := c.observe(50, at(1.3)); ev != evPausedSeek {
		t.Fatalf("暂停中往后拖 40s 应只重锚，got %v", ev)
	}
	c.observe(20, at(2))
	if ev := c.observe(20, at(2.3)); ev != evPausedSeek {
		t.Fatalf("暂停中往回拖应只重锚，got %v", ev)
	}
	if got := c.live(at(3)); !near(got, 20+flipLagMid) {
		t.Fatalf("暂停中拖动后位置=%.3f, want %.3f（冻结在新位置）", got, 20+flipLagMid)
	}
	c.observe(21, at(4))
	if ev := c.observe(22, at(5)); ev != evResume {
		t.Fatalf("拖完恢复播放应报一次恢复，got %v", ev)
	}
}

// 暂停中拖动进度条，读数同样先闪一下目标值又弹回原值、约半秒后才真正过去（2026-10-08 真机：暂停在
// 212，拖到约 63，歌词行依次推出目标行、原位置的行、目标行）。闪的那一下不认：整次拖动只重锚一次，
// 其间位置留在原处。
//
// 变异自证：把 observe 暂停分支改回「变化即重锚」（不经 holdJump），五例各返回三次 evPausedSeek 而红；
// 把 jumpHold 改回 40ms，「闪现 220ms」一例在闪现期间就重锚而红。
func TestClockPausedSeekFlickerIgnored(t *testing.T) {
	const realAt = 500 // 读数真正变成目标值的时刻（毫秒）
	for _, tc := range []struct {
		name               string
		target             float64
		gapMs              int
		flashFrom, flashTo int // 闪现的起止（毫秒，含起不含止）
		wantAt             int
	}{
		{"50ms 轮询、往回拖、闪一轮", 63, 50, 0, 50, 800},
		{"50ms 轮询、往后拖、闪一轮", 280, 50, 0, 50, 800},
		{"50ms 轮询、闪现 220ms", 63, 50, 0, 220, 800},
		{"20ms 间隔、闪现 15~42ms", 63, 20, 15, 42, 800},
		{"10ms 间隔、闪现持续 39ms", 63, 10, 0, 39, 800},
	} {
		var c clock
		c.reset(212, 212.5, atMs(-5000), false, 300)
		var seekAt []int
		for ms := 0; ms <= 1000; ms += tc.gapMs {
			raw := 212.0
			if (ms >= tc.flashFrom && ms < tc.flashTo) || ms >= realAt {
				raw = tc.target
			}
			switch ev := c.observe(raw, atMs(ms)); ev {
			case evPausedSeek:
				seekAt = append(seekAt, ms)
			case evNone:
			default:
				t.Errorf("%s：%dms 处不应有 %v", tc.name, ms, ev)
			}
			if ms < realAt {
				if got := c.live(atMs(ms)); !near(got, 212.5) {
					t.Errorf("%s：%dms 处位置应留在原处，live=%.3f", tc.name, ms, got)
				}
			}
		}
		if len(seekAt) != 1 || seekAt[0] != tc.wantAt {
			t.Errorf("%s：应只在 %dms 那次读数重锚一次，got %v", tc.name, tc.wantAt, seekAt)
			continue
		}
		if got := c.live(atMs(1000)); !near(got, tc.target+flipLagMid) || c.playing {
			t.Errorf("%s：重锚后 live=%.3f playing=%v，want %.3f 且仍是暂停", tc.name, got, c.playing, tc.target+flipLagMid)
		}
	}
}

// 暂停中真拖动（读数变成目标值后保持）：目标读数持续满 jumpHold 的那一次读数重锚——50ms 轮询下是第一次
// 读到之后 300ms 的那一次，读数间隔不短于 jumpHold 时就是第二次读数；连着拖过几个位置，每个各重锚一次。
//
// 变异自证：把 holdJump 末尾的 `now.Sub(pendingAt) >= jumpHold` 改成 true，10、20、50、70ms 四例提前到
// 第二次读数重锚而红；把 observe 暂停分支改回「变化即重锚」（不经 holdJump），六例都在第一次读数就重锚
// 而红。
func TestClockPausedSeekConfirmed(t *testing.T) {
	for _, tc := range []struct{ gapMs, holdMs int }{{10, 300}, {20, 300}, {50, 300}, {70, 350}, {300, 300}, {2000, 2000}} {
		var c clock
		c.reset(212, 212.5, atMs(-5000), false, 300)
		ms := 0
		for _, target := range []float64{63, 100, 30} { // 每个位置停 4 次读数
			var seekAt []int
			start := ms
			for end := ms + 4*tc.gapMs + tc.holdMs; ms < end; ms += tc.gapMs {
				switch ev := c.observe(target, atMs(ms)); ev {
				case evPausedSeek:
					seekAt = append(seekAt, ms-start)
				case evNone:
				default:
					t.Errorf("间隔 %dms：拖到 %.0f 时不应有 %v", tc.gapMs, target, ev)
				}
			}
			if len(seekAt) != 1 || seekAt[0] != tc.holdMs {
				t.Errorf("间隔 %dms：拖到 %.0f 应在持续 %dms 时重锚一次，got %v", tc.gapMs, target, tc.holdMs, seekAt)
			}
			if got := c.live(atMs(ms)); !near(got, target+flipLagMid) {
				t.Errorf("间隔 %dms：拖到 %.0f 后 live=%.3f, want %.3f", tc.gapMs, target, got, target+flipLagMid)
			}
		}
		if c.playing {
			t.Errorf("间隔 %dms：暂停中拖动不改变播放态", tc.gapMs)
		}
	}
}

// 暂停中的待确认与恢复交错：不丢恢复，也不多报。
//
// 变异自证：把 observe 暂停分支改回「变化即重锚」（不经 holdJump），四例都在第一次读到目标值或其后
// 一次读数当场重锚而红；去掉 freeze 里的 `c.jumpAt = time.Time{}`，「播放中待确认、随即冻结」一例在
// 300ms 那次读数（从冻结前第一次读到算起已满 jumpHold）就重锚而红。
func TestClockPausedSeekPendingThenResume(t *testing.T) {
	t.Run("待确认期间目标自己走了一格", func(t *testing.T) { // 轮询 2s：拖到 63 后随即按了播放
		var c clock
		c.reset(212, 212.5, at(0), false, 300)
		c.observe(212, at(2))
		if ev := c.observe(63, at(4)); ev != evNone {
			t.Fatalf("第一次读到目标值先不认，got %v", ev)
		}
		if ev := c.observe(64, at(6)); ev != evPausedSeek {
			t.Fatalf("第二次读到 目标+1 应确认拖动，got %v", ev)
		}
		if got := c.live(at(6)); !near(got, 64+flipLagMid) {
			t.Fatalf("重锚后 live=%.3f, want %.3f", got, 64+flipLagMid)
		}
		if ev := c.observe(66, at(8)); ev != evNone {
			t.Fatalf("恢复的第一跳等下一跳确认，got %v", ev)
		}
		if ev := c.observe(68, at(10)); ev != evResume {
			t.Fatalf("连着第二跳应判恢复，got %v", ev)
		}
		if ev := c.observe(70, at(12)); ev != evNone {
			t.Fatalf("恢复后的正常推进不应再报事件，got %v", ev)
		}
	})
	t.Run("闪了一下、随即在原位恢复", func(t *testing.T) {
		var c clock
		c.reset(212, 212.5, at(0), false, 300)
		c.observe(63, at(1.00)) // 闪现
		if ev := c.observe(213, at(1.05)); ev != evNone {
			t.Fatalf("原位置前进一格是恢复的第一跳，got %v", ev)
		}
		if ev := c.observe(214, at(2.05)); ev != evResume {
			t.Fatalf("连着第二跳应判恢复，got %v", ev)
		}
		if got := c.live(at(2.05)); !near(got, 214+flipLagMid) {
			t.Fatalf("恢复时 live=%.3f, want %.3f", got, 214+flipLagMid)
		}
	})
	t.Run("恢复的第一跳之后闪了一下", func(t *testing.T) {
		var c clock
		c.reset(212, 212.5, at(0), false, 300)
		c.observe(213, at(1.00)) // 第一跳
		if ev := c.observe(63, at(1.50)); ev != evNone {
			t.Fatalf("闪现不认，got %v", ev)
		}
		c.observe(213, at(1.55))
		if ev := c.observe(214, at(2.05)); ev != evResume {
			t.Fatalf("闪现不应打断恢复的确认，got %v", ev)
		}
	})
	t.Run("播放中待确认、随即冻结", func(t *testing.T) { // 位置结构作废时由 dropTwin 冻结
		var c clock
		c.reset(23, 23.5, atMs(-500), true, 300)
		c.observe(285, atMs(0))
		c.freeze(atMs(20))
		for _, ms := range []int{40, 300} {
			if ev := c.observe(285, atMs(ms)); ev != evNone {
				t.Fatalf("%dms：冻结前读到的那一次不算数，持续多久从暂停后第一次读到（40ms）起算，got %v", ms, ev)
			}
		}
		if ev := c.observe(285, atMs(340)); ev != evPausedSeek {
			t.Fatalf("重新持续满 jumpHold 后才重锚，got %v", ev)
		}
	})
}

// 2026-10-09 真机采样的三次「暂停 → 拖动 → 接着播放」与随后一次播放中的拖动（seekSamplePaused），整段
// 按 50 / 20 / 10ms 读数间隔的各个相位回放。只记录现状，不是承诺：
//
//   - 暂停中拖动，目标 T 与落点各重锚一次（evPausedSeek）：T 的读数停了约 1.1s，长于 jumpHold，分不出它
//     是过渡；之后的 V 只停约 245ms、不认，V−1 停约 512ms、认，再进到 V 是「一跳」。接着播放，V+1 那一跳
//     判恢复，只报一次。
//   - 第一次拖动（534545）时距上一次跳变只有 0.77s，还没到 pauseThreshold，clock 仍按播放中处理：T 与
//     V−1 各报一次 evSeek。
//
// 变异自证：把 jumpHold 改回 40ms，后两次拖动里只停约 245ms 的 V 也被认，事件多出两个而红；把 holdJump
// 末尾的持续判定改成恒真，同样而红。
func TestClockPausedSeekSampleReplay(t *testing.T) {
	tl := seekSamplePaused
	seekOff := seekLag + jumpHold.Seconds() // 播放中确认时的位置：第一次读到的值 + seekLag + 持续的时间
	want := []struct {
		ev  clockEvent
		pos float64
	}{
		{evSeek, 130 + seekOff}, {evSeek, 119 + seekOff}, // 534545：T=130，V=120
		{evPause, 124},
		{evPausedSeek, 205 + flipLagMid}, {evPausedSeek, 199 + flipLagMid}, {evResume, 201 + flipLagMid}, // 544623：T=205，V=200
		{evPause, 208},
		{evPausedSeek, 20 + flipLagMid}, {evPausedSeek, 16 + flipLagMid}, {evResume, 18 + flipLagMid}, // 559480：T=20，V=17
		{evSeek, 28 + seekOff}, // 567972：播放中拖到 28
	}
	for _, gap := range []int{50, 20, 10} {
		for phase := 0; phase < gap && !t.Failed(); phase++ {
			var c clock
			c.reset(tl[0].pos, tl[0].pos+flipLagMid, atMs(tl[0].ms), true, 256)
			c.relaxAt = at(-100)
			n := 0
			for ms := tl[0].ms + phase; ms <= tl[len(tl)-1].ms; ms += gap {
				ev := c.observe(sampleAt(tl, ms), atMs(ms))
				if ev == evNone {
					continue
				}
				if pos := c.live(atMs(ms)); n >= len(want) || ev != want[n].ev || math.Abs(pos-want[n].pos) > 0.05 {
					t.Errorf("间隔 %dms 相位 %d：第 %d 个事件是 %dms 处的 %v（位置 %.2f），与预期不符", gap, phase, n+1, ms, ev, pos)
				}
				n++
			}
			if n != len(want) {
				t.Errorf("间隔 %dms 相位 %d：共 %d 个事件，want %d", gap, phase, n, len(want))
			}
		}
	}
}

// 暂停中往前拖一两秒只是一跳，之后不再动：不能判恢复（复查实测旧实现会报 1.6s 假
// playing、让路由切过来）。
//
// 变异自证：把 observe 里「等下一跳确认」去掉（第一跳就判恢复），本例变红。
func TestClockSmallSeekWhilePausedIsNotResume(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), false, 300)
	for _, sec := range []float64{1, 2, 4, 8} {
		raw := 12.0
		if ev := c.observe(raw, at(sec)); ev == evResume {
			t.Fatalf("%.0fs：往前拖 2s 后静止，不应判恢复", sec)
		}
	}
	if c.playing {
		t.Fatal("仍应是暂停")
	}
	// 隔了很久（超过 resumeConfirm）再一跳：仍只是第一跳。
	if ev := c.observe(13, at(20)); ev == evResume {
		t.Fatal("与上一跳相隔超过 resumeConfirm，不应判恢复")
	}
}

// 播放中跳转：读数偏出范围 3s 以上、且目标读数持续满 jumpHold，才报一次 evSeek；位置按「第一次读到
// 目标值时真实位置是 目标+seekLag」接上。持续期间读数进了一格（真机上落点有时比目标少一、约 250ms 后
// 进一格）仍算同一次跳转，位置不低于进这一格所允许的下限（读数+flipLagMin）。正常推进不报。
//
// 变异自证：把落锚里的 seekLag 换成 flipLagMid（旧实现的落点），跳转后的 live 多出 0.2 而红；去掉落锚
// 处对 lo 的夹取（只按第一次读到的值推算），进了一格的那一例读到 100.6、比 101.38 慢 0.78 而红。
func TestClockSeekWhilePlaying(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), true, 300)
	if ev := c.observe(11, at(1)); ev != evNone {
		t.Fatalf("正常推进不应报跳转，got %v", ev)
	}
	if ev := c.observe(60, at(2)); ev != evNone {
		t.Fatalf("第一次读到新位置先不认，got %v", ev)
	}
	if got := c.live(at(2)); !near(got, 12.5) {
		t.Fatalf("未确认前位置不动，live=%.3f, want 12.5", got)
	}
	if ev := c.observe(60, at(2.25)); ev != evNone {
		t.Fatalf("持续 0.25s 还不认，got %v", ev)
	}
	if ev := c.observe(60, at(2.3)); ev != evSeek {
		t.Fatalf("持续满 jumpHold 应报 evSeek，got %v", ev)
	}
	if got := c.live(at(2.3)); !near(got, 60+seekLag+0.3) {
		t.Fatalf("跳转后 live=%.3f, want %.3f", got, 60+seekLag+0.3)
	}
	c.observe(5, at(3))
	if ev := c.observe(5, at(3.3)); ev != evSeek {
		t.Fatalf("回跳应报 evSeek，got %v", ev)
	}
	// 持续期间读数进了一格（落在 100，0.25s 后进到 101）：仍算同一次跳转。
	c.observe(100, at(4))
	if ev := c.observe(101, at(4.25)); ev != evNone {
		t.Fatalf("读数进了一格不是另一个目标，也还没持续够，got %v", ev)
	}
	if ev := c.observe(101, at(4.3)); ev != evSeek {
		t.Fatalf("从第一次读到 100 起持续满 jumpHold 应确认，got %v", ev)
	}
	if got := c.live(at(4.3)); !near(got, 101+flipLagMin) {
		t.Fatalf("live=%.3f, want %.3f（进到 101 的那一刻真实位置不低于 101+flipLagMin）", got, 101+flipLagMin)
	}
	if !c.playing {
		t.Fatal("播放中跳转不改变播放态")
	}
}

// 拖动进度条时读数先闪一下目标值又弹回原值，约半秒后才真正变过去（2026-10-08 真机，每 5ms 读一次：
// 23 → 285 → 24 → 285）。闪的那一下不认：整次拖动只报一次 evSeek，其间位置留在原来的时间线上。
//
// 变异自证：把 observe 里待确认那一段删掉（第一次读到就落锚、报 evSeek），两例各报出三次而红；删掉
// observe 开头的 `c.jumpAt = time.Time{}`（记过一次就一直有效），「弹回原值」一例在 1.05s 那次读数就报了
// evSeek 而红。
func TestClockSeekFlickerIgnored(t *testing.T) {
	for _, back := range []float64{23, 24} { // 弹回原值；弹回时原位置正好走了一格
		var c clock
		c.reset(23, 23.5, at(0), true, 300)
		c.relaxAt = at(-100)
		seeks := 0
		feed := func(raw, sec float64) {
			if c.observe(raw, at(sec)) == evSeek {
				seeks++
			}
		}
		feed(285, 0.50)
		feed(back, 0.55)
		for _, sec := range []float64{0.60, 0.80, 1.00} {
			feed(back, sec)
		}
		if got := c.live(at(1.0)); seeks != 0 || got < 24 || got > 25 {
			t.Fatalf("弹回 %.0f：闪的那一下不应认，seeks=%d live=%.3f（应仍在 24~25）", back, seeks, got)
		}
		feed(285, 1.05)
		feed(285, 1.30)
		if seeks != 0 {
			t.Fatalf("弹回 %.0f：真正变过去之后还要持续满 jumpHold 才认，seeks=%d", back, seeks)
		}
		feed(285, 1.35)
		if got := c.live(at(1.35)); seeks != 1 || !near(got, 285+seekLag+0.3) {
			t.Fatalf("弹回 %.0f：应只报一次 evSeek，seeks=%d live=%.3f", back, seeks, got)
		}
	}
}

// 闪现的目标值会被连着读到好几次：闪现约 220ms 时按 50ms 轮询能读到五次，读得比轮询密时（全扫途中
// 约每 20ms 读一次，见 scanTick）十几毫秒的闪现也能读到两三次。认跳转看的是目标读数持续了多久
// （jumpHold），不是读到了几次。闪现期间不认，真正变过去后持续满 jumpHold 的那一次读数才认，整次拖动
// 只报一次。
//
// 变异自证：把 holdJump 末尾的 `now.Sub(pendingAt) >= jumpHold` 改成 true（连着两次即认，旧实现），
// 五例都在闪现期间报出 evSeek 而红；把 jumpHold 改回 40ms，闪现 219、225ms 的两例在闪现期间报出 evSeek
// 而红；删掉 observe 开头的 `c.jumpAt = time.Time{}`（记过一次就一直有效），五例都在真正变过去的第一次
// 读数（500ms）就报了 evSeek 而红。
func TestClockSeekFlickerDenseSampling(t *testing.T) {
	const realAt = 500 // 读数真正变成目标值的时刻（毫秒）
	for _, tc := range []struct {
		name               string
		gapMs              int
		flashFrom, flashTo int // 闪现的起止（毫秒，含起不含止）
		wantAt             int // 该在哪一次读数确认
	}{
		{"50ms 间隔、闪现 225ms", 50, 0, 225, 800},
		{"20ms 间隔、闪现 219ms", 20, 0, 219, 800},
		{"20ms 间隔、闪现 15~42ms", 20, 15, 42, 800},
		{"10ms 间隔、闪现持续 39ms", 10, 0, 39, 800},
		{"5ms 间隔、闪现 25ms", 5, 2, 27, 800},
	} {
		var c clock
		c.reset(23, 23.5, atMs(-500), true, 300)
		c.relaxAt = at(-100)
		var seekAt []int
		for ms := 0; ms <= 1000; ms += tc.gapMs {
			raw := 23.0
			if (ms >= tc.flashFrom && ms < tc.flashTo) || ms >= realAt {
				raw = 285
			}
			if c.observe(raw, atMs(ms)) == evSeek {
				seekAt = append(seekAt, ms)
			}
			if ms == realAt-tc.gapMs && len(seekAt) == 0 {
				if got := c.live(atMs(ms)); got < 23.5 || got > 24.7 {
					t.Errorf("%s：闪现前后位置应留在原来的时间线上，live=%.3f", tc.name, got)
				}
			}
		}
		if len(seekAt) != 1 || seekAt[0] != tc.wantAt {
			t.Errorf("%s：应只在 %dms 那次读数报一次 evSeek，got %v", tc.name, tc.wantAt, seekAt)
			continue
		}
		if got, want := c.live(atMs(tc.wantAt)), 285+seekLag+float64(tc.wantAt-realAt)/1000; !near(got, want) {
			t.Errorf("%s：跳转后 live=%.3f, want %.3f", tc.name, got, want)
		}
	}
}

// 真跳转（读数变成目标值后不再弹回）在各种读数间隔下：目标读数持续满 jumpHold 的那一次读数确认，
// 只报一次。按轮询下限 50ms 读，是第一次读到之后 300ms 的那一次；间隔不短于 jumpHold（300ms，以及
// 配置上限 2000ms）时就是第二次读数。
//
// 变异自证：把 holdJump 末尾的 `now.Sub(pendingAt) >= jumpHold` 改成 true，5、10、20、50、70、299ms
// 六例提前确认而红；删掉 holdJump 里续上待确认的 `c.jumpAt = pendingAt`，间隔短于 jumpHold 的六例到
// 第三次读数时待确认已作废、一直确认不了，其余两例落锚算错，全部变红。
func TestClockSeekConfirmedAfterHold(t *testing.T) {
	for _, tc := range []struct{ gapMs, wantMs int }{
		{5, 300}, {10, 300}, {20, 300}, {50, 300}, {70, 350}, {299, 598}, {300, 300}, {2000, 2000},
	} {
		var c clock
		c.reset(23, 23.5, atMs(-500), true, 300)
		c.relaxAt = at(-100)
		var seekAt []int
		end := tc.wantMs + 3*tc.gapMs
		for ms := 0; ms <= end; ms += tc.gapMs {
			// 目标位置自己也在走：变成目标值时真实位置是 目标+seekLag，之后每一跳落在读数允许的范围里。
			raw := 285 + math.Floor(math.Max(0, float64(ms)-200)/1000)
			if c.observe(raw, atMs(ms)) == evSeek {
				seekAt = append(seekAt, ms)
			}
		}
		if len(seekAt) != 1 || seekAt[0] != tc.wantMs {
			t.Errorf("间隔 %dms：应只在 %dms 那次读数报一次 evSeek，got %v", tc.gapMs, tc.wantMs, seekAt)
			continue
		}
		if got, want := c.live(atMs(end)), 285+seekLag+float64(end)/1000; !near(got, want) {
			t.Errorf("间隔 %dms：跳转后 live=%.3f, want %.3f（从第一次读到目标值起算）", tc.gapMs, got, want)
		}
	}
}

// 2026-10-09 真机采样的 12 次播放中拖动（seekSamplePlaying），按轮询下限 50ms、全扫途中的 20ms、以及
// 10ms 三种读数间隔的各个相位回放：每次拖动恰好报一次 evSeek，在读数真正落下去、持续满 jumpHold 的那
// 一次读数，位置在目标附近；闪现与弹回都不认，只闪了一下就被下一次拖动取代的那次（519910）不报，
// 全程没有暂停类事件。整段连着回放一遍（相隔约 1s 的连续拖动在这里），每次拖动再单独回放一遍（此前
// 没有跳转，暂停按常规门槛判）。
//
// 位置的判据：落定后第一次正常跳变（读数变成 flipTo）的那一刻，真实位置在 flipTo+flipLagMin ~
// flipTo+flipLagMax，由它倒推确认时的真实位置，估计与它相差不得超过 0.7s。落点是「目标减一、约 250ms
// 后进一格」的两次（494701、502380），确认时读数已经进了那一格。
//
// 变异自证：把 jumpHold 改回 40ms，50ms 间隔下闪现约 220ms 的两次（494701、502380）在闪现、弹回、落定时
// 各报一次 evSeek 而红；把 holdJump 末尾的持续判定改成恒真，同样是这两次而红；去掉 observe 跳转确认处
// 对 lo 的夹取（只按第一次读到的值推算），这两次的位置比倒推出的真实位置慢约 1s 而红；去掉确认处的
// `c.relaxAt = now`，519910 那次之后读数停住的 1.3s 里判了暂停、521259 那次不再报 evSeek 而红。
func TestClockSeekSampleOneSeekPerDrag(t *testing.T) {
	tl := seekSamplePlaying
	var waitMin, waitMax int // 读数落定到确认相隔多久
	replay := func(name string, from, to int, drags []sampleDrag, gap, phase int) {
		// 落定后第一次读到的那一刻起、满 jumpHold 的那一次读数确认：不晚于落定后 hold 再加一个读数间隔。
		// 弹回短于读数间隔、没被读到时，闪现与落定读成连着的一段，从闪现算起：不早于闪现后 hold。
		hold := (int(jumpHold/time.Millisecond) + gap - 1) / gap * gap
		var c clock
		start := sampleAt(tl, from)
		c.reset(start, start+flipLagMid, atMs(from), true, 256)
		c.relaxAt = at(-100) // 开播已久，不在放宽期
		seen := make([]int, len(drags))
		for ms := from + phase; ms < to; ms += gap {
			ev := c.observe(sampleAt(tl, ms), atMs(ms))
			if ev == evNone {
				continue
			}
			i := len(drags) - 1
			for i >= 0 && drags[i].flash > ms {
				i--
			}
			if ev != evSeek || i < 0 || drags[i].land == 0 {
				t.Errorf("%s，间隔 %dms 相位 %d：%dms 处不应有事件 %v", name, gap, phase, ms, ev)
				continue
			}
			d, pos := drags[i], c.live(atMs(ms))
			seen[i]++
			waitMin, waitMax = min(waitMin, ms-d.land), max(waitMax, ms-d.land)
			if ms-d.flash < hold || ms-d.land >= hold+gap {
				t.Errorf("%s，间隔 %dms 相位 %d：%d 那次拖动在闪现后 %dms、落定后 %dms 确认，应不早于闪现后 %dms、早于落定后 %dms",
					name, gap, phase, d.flash, ms-d.flash, ms-d.land, hold, hold+gap)
			}
			if pos < d.to-1 || pos > d.to+1 {
				t.Errorf("%s，间隔 %dms 相位 %d：%d 那次拖动确认后位置=%.2f，应在目标 %.0f 附近", name, gap, phase, d.flash, pos, d.to)
			}
			if truth := d.flipTo + (flipLagMin+flipLagMax)/2 + float64(ms-d.flipAt)/1000; d.flipAt != 0 && math.Abs(pos-truth) > 0.7 {
				t.Errorf("%s，间隔 %dms 相位 %d：%d 那次拖动确认后位置=%.2f，由之后的正常跳变倒推应约为 %.2f", name, gap, phase, d.flash, pos, truth)
			}
		}
		for i, d := range drags {
			want := 1
			if d.land == 0 {
				want = 0
			}
			if seen[i] != want {
				t.Errorf("%s，间隔 %dms 相位 %d：%d 那次拖动报了 %d 次 evSeek，want %d", name, gap, phase, d.flash, seen[i], want)
			}
		}
	}
	for _, gap := range []int{50, 20, 10} {
		waitMin, waitMax = math.MaxInt, math.MinInt
		for phase := 0; phase < gap && !t.Failed(); phase++ {
			replay("整段", tl[0].ms, tl[len(tl)-1].ms, sampleDrags, gap, phase)
			for i, d := range sampleDrags {
				if d.land == 0 {
					// 没落下去的那次不单独回放：读数在原值上共停了 2.1s，单独回放时此前没有跳转、暂停按常规
					// 门槛判，1.6s 处就判成暂停；采样里它前面 2s 刚确认过一次跳转，门槛是放宽的。
					continue
				}
				// 从拖动前最后一次读数变化起，到下一次拖动的闪现为止。
				from := tl[sort.Search(len(tl), func(k int) bool { return tl[k].ms >= d.flash })-1].ms
				to := tl[len(tl)-1].ms
				if i+1 < len(sampleDrags) {
					to = sampleDrags[i+1].flash
				}
				replay(fmt.Sprintf("单独回放 %d", d.flash), from, to, sampleDrags[i:i+1], gap, phase)
			}
		}
		if !t.Failed() {
			t.Logf("间隔 %dms：读数落定后 %d~%dms 确认", gap, waitMin, waitMax)
		}
	}
}

// 认不认只看目标读数持续了多久（第一次与最近一次读到它相隔多久），差 1ms 也不认：持续 299ms 后弹回的
// 不认，位置留在原处；弹回之后再读到目标值要从头算，不能接着上一段。满 300ms 的那一次读数认。播放中
// 报 evSeek，暂停中报 evPausedSeek。
//
// 变异自证：把 holdJump 末尾的 `>= jumpHold` 改成 `> jumpHold`，满 300ms 的那次读数不认而红；把 jumpHold
// 改成 299ms，持续 299ms 的那次读数被认而红；删掉 observe 开头的 `c.jumpAt = time.Time{}`（弹回不作废），
// 弹回后第一次读到目标值（301ms）就认了而红。
func TestClockSeekHoldBoundary(t *testing.T) {
	for _, playing := range []bool{true, false} {
		want := evPausedSeek
		if playing {
			want = evSeek
		}
		newClock := func() *clock {
			c := &clock{}
			c.reset(23, 23.5, atMs(-500), playing, 300)
			c.relaxAt = at(-100)
			if ev := c.observe(285, atMs(0)); ev != evNone {
				t.Fatalf("playing=%v：第一次读到目标值先不认，got %v", playing, ev)
			}
			return c
		}

		c := newClock()
		for _, r := range []struct {
			ms  int
			raw float64
		}{{299, 285}, {300, 23}, {301, 285}, {600, 285}} {
			if ev := c.observe(r.raw, atMs(r.ms)); ev != evNone {
				t.Fatalf("playing=%v：目标值持续 299ms 后弹回、301ms 起重新出现，%dms 处不应认，got %v", playing, r.ms, ev)
			}
			if got := c.live(atMs(r.ms)); got < 23.5 || got > 24.7 {
				t.Fatalf("playing=%v：%dms 处位置应留在原处，live=%.3f", playing, r.ms, got)
			}
		}
		if ev := c.observe(285, atMs(601)); ev != want {
			t.Fatalf("playing=%v：重新出现后持续满 300ms 应认，got %v", playing, ev)
		}

		c = newClock()
		if ev := c.observe(285, atMs(300)); ev != want {
			t.Fatalf("playing=%v：目标值持续满 300ms 应认，got %v", playing, ev)
		}
	}
}

// seekTo55 给出一个开播已久、刚在 0s 确认了一次跳转（到 55）的 clock。
func seekTo55(t *testing.T) *clock {
	t.Helper()
	c := &clock{}
	c.reset(10, 10.5, at(-0.5), true, 300)
	c.relaxAt = at(-100)
	c.observe(55, at(-0.3))
	if ev := c.observe(55, at(0)); ev != evSeek {
		t.Fatalf("前置：应在 0s 确认跳转，got %v", ev)
	}
	return c
}

// 播放中跳转确认之后，读数的下一跳可以晚于 pauseThreshold 才来（2026-10-08 真机：拖动一次，约 2s 后报
// 暂停、再过 1s 报恢复，音频与波形都没断）。与开播一样，此后 startWindow 内按 startPauseThreshold 判
// 暂停；过了 startWindow 回到 pauseThreshold。
//
// 变异自证：去掉 observe 跳转确认处的 `c.relaxAt = now`，「停 2.5s 不算暂停」「连续跳转」两例在停住
// 1.7s、1.65s 处判了暂停、「停超过 3s 判暂停」「跳转后 1s 真暂停」两例提前判暂停而红；把 observe 里的
// `now.Sub(c.relaxAt) < startWindow` 改成 true（放宽期永不过期），「8s 之后回到常规门槛」一例停 1.7s
// 不判暂停而红。
func TestClockSeekRelaxesPauseThreshold(t *testing.T) {
	t.Run("停 2.5s 不算暂停", func(t *testing.T) {
		c := seekTo55(t)
		for _, sec := range []float64{0.5, 1.0, 1.5, 1.7, 2.0, 2.5} {
			if ev := c.observe(55, at(sec)); ev != evNone {
				t.Fatalf("跳转后停住 %.1fs 不应有事件，got %v", sec, ev)
			}
		}
		for i, sec := range []float64{2.55, 3.55, 4.55, 5.55} {
			if ev := c.observe(56+float64(i), at(sec)); ev != evNone {
				t.Fatalf("%.2fs：之后每秒一跳不应有事件，got %v", sec, ev)
			}
		}
		if !c.playing {
			t.Fatal("全程应是播放中")
		}
	})
	t.Run("停超过 3s 判暂停", func(t *testing.T) {
		c := seekTo55(t)
		if ev := c.observe(55, at(2.9)); ev != evNone {
			t.Fatalf("停 2.9s 还不到放宽后的门槛，got %v", ev)
		}
		if ev := c.observe(55, at(3.0)); ev != evPause {
			t.Fatalf("停满 3s 应判暂停，got %v", ev)
		}
	})
	t.Run("跳转后 1s 真暂停", func(t *testing.T) {
		c := seekTo55(t)
		c.observe(56, at(1.0)) // 走了一格，随后暂停
		for _, sec := range []float64{2.0, 2.6, 3.0, 3.9} {
			if ev := c.observe(56, at(sec)); ev != evNone {
				t.Fatalf("%.1fs：放宽期内停不满 3s 不判暂停，got %v", sec, ev)
			}
		}
		if ev := c.observe(56, at(4.0)); ev != evPause {
			t.Fatalf("停满 3s 应判暂停，got %v", ev)
		}
		if ev := c.observe(57, at(6.0)); ev != evNone {
			t.Fatalf("恢复的第一跳等下一跳确认，got %v", ev)
		}
		if ev := c.observe(58, at(7.0)); ev != evResume {
			t.Fatalf("连着第二跳应判恢复，got %v", ev)
		}
	})
	t.Run("8s 之后回到常规门槛", func(t *testing.T) {
		c := seekTo55(t)
		for i := 1; i <= 8; i++ {
			c.observe(55+float64(i), at(float64(i)))
		}
		if ev := c.observe(63, at(9.5)); ev != evNone {
			t.Fatalf("停 1.5s 未到 pauseThreshold，got %v", ev)
		}
		if ev := c.observe(63, at(9.7)); ev != evPause {
			t.Fatalf("跳转 8s 之后停 1.7s 应判暂停，got %v", ev)
		}
	})
	t.Run("连续跳转", func(t *testing.T) {
		c := seekTo55(t)
		seeks, cur := 1, 55.0
		for i, target := range []float64{120, 30} { // 每隔 2s 再拖一次，其间读数停着
			base := 2 * float64(i+1)
			for _, ev := range []clockEvent{
				c.observe(cur, at(base-0.35)), // 上一次跳转确认后已停 1.65s：过了常规门槛
				c.observe(target, at(base-0.3)),
				c.observe(target, at(base)),
			} {
				switch ev {
				case evSeek:
					seeks++
				case evPause, evResume, evPausedSeek:
					t.Fatalf("第 %d 次跳转前后不应有暂停类事件，got %v", i+2, ev)
				}
			}
			cur = target
		}
		if ev := c.observe(30, at(5.7)); ev != evNone || seeks != 3 || !c.playing {
			t.Fatalf("三次跳转应各报一次 evSeek、始终播放中，got seeks=%d playing=%v ev=%v", seeks, c.playing, ev)
		}
	})
}

// start / reset / freeze 之后没有待确认的跳转：此前记下的目标不能留到下一段去。
//
// 变异自证：删掉 freeze 里的 `c.jumpAt = time.Time{}`，freeze 一例变红。start / reset 整个重建 clock，
// 字段自然归零，这两例只防将来改成逐字段赋值时漏掉。
func TestClockPendingJumpCleared(t *testing.T) {
	for name, clear := range map[string]func(c *clock){
		"start":  func(c *clock) { c.start(atMs(20), 200) },
		"reset":  func(c *clock) { c.reset(24, 24.5, atMs(20), true, 300) },
		"freeze": func(c *clock) { c.freeze(atMs(20)) },
	} {
		var c clock
		c.reset(23, 23.5, atMs(-500), true, 300)
		if ev := c.observe(285, atMs(0)); ev != evNone || c.jumpAt.IsZero() {
			t.Fatalf("%s 前置：第一次读到目标值应记为待确认，got ev=%v jumpAt=%v", name, ev, c.jumpAt)
		}
		clear(&c)
		if !c.jumpAt.IsZero() {
			t.Errorf("%s 之后不应残留待确认的跳转", name)
		}
	}
}

// live 钳到时长。
//
// 变异自证：去掉 live 里钳到时长的那一段，读到 301.63 而红。
func TestClockClampsToDuration(t *testing.T) {
	var c clock
	c.reset(299, 299.5, at(0), true, 300)
	c.observe(300, at(0.6))
	if got := c.live(at(10)); got != 300 {
		t.Fatalf("live=%.3f, want 300（钳到时长）", got)
	}
}

// 轮询调慢（配置上限 2s）时，恢复后每次读数位置跳 2~3s：仍应判为播放推进、两跳内恢复。固定 2.5s 门槛
// 会把跳 3s 的那次当成暂停中拖动，抖动下可能一直恢复不了（复查实测轮询 3s 时永不恢复）。
//
// 变异自证：把「一跳」的判据改回固定 `d <= 2.5`，3s 那一跳被当成拖动、本例变红。
func TestClockResumeWithSlowPoll(t *testing.T) {
	var c clock
	c.reset(10, 10.5, at(0), false, 300)
	c.observe(10, at(2))
	if ev := c.observe(13, at(4)); ev != evNone {
		t.Fatalf("轮询 2s、跳 3s 应算一跳（待确认），got %v", ev)
	}
	if ev := c.observe(15, at(6)); ev != evResume {
		t.Fatalf("第二跳应判恢复，got %v", ev)
	}
}

// 开播头几秒：第一跳 1.45s 才来，之后若在缓冲、停住 2s 也不算暂停（startWindow 内门槛是
// startPauseThreshold）；开播 startWindow 之后，同样停 1.7s 即判暂停。
//
// 变异自证：去掉 observe 里 startWindow 的放宽（一律用 pauseThreshold），开头那段判成暂停而红。
func TestClockStartBufferingNotPause(t *testing.T) {
	var c clock
	c.start(at(0), 300)
	c.observe(1, at(1.45))
	for _, sec := range []float64{2.0, 2.5, 3.2, 3.45} {
		if ev := c.observe(1, at(sec)); ev == evPause {
			t.Fatalf("开播 %.2fs、停住 %.2fs 不应判暂停（缓冲）", sec, sec-1.45)
		}
	}
	c.observe(2, at(3.6))
	c.observe(10, at(11))
	if ev := c.observe(10, at(12.8)); ev != evPause {
		t.Fatalf("开播 8s 之后停住 1.8s 应判暂停，got %v", ev)
	}
}
