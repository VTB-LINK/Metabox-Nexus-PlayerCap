package player

// 本文件只测 LivePosition —— 由锚点位置、锚点时刻、当前时刻、是否在播、时长算出当前时刻的整曲位置。
//
// kugou、qqmusic 的轮询循环用它算歌词行匹配用的实时位置，也用它算换歌取词之后随 all_lyrics 发出的位置。
// 两家的循环没有离线夹具：这里只守算式，调用点传的是哪个时刻、哪个播放态，靠读代码与真机。
//
// 变异均在隔离副本（临时目录里只含 liveposition.go 与本文件的最小模块）里注入，每条一个副本。

import (
	"math"
	"testing"
	"time"
)

func TestLivePosition(t *testing.T) {
	// 不带单调时钟读数的固定时刻：Sub 按墙钟算，结果与运行环境无关。
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	after := func(d time.Duration) time.Time { return t0.Add(d) }

	cases := []struct {
		name       string
		anchorSec  float32
		anchorTime time.Time
		now        time.Time
		playing    bool
		duration   float32
		want       float32
	}{
		// 变异自证：恒不推进（删掉 `pos += ...`）、经过时间按毫秒数加（Seconds 换成 Milliseconds）、
		// 减数与被减数对调（anchorTime.Sub(now)），本例均红（实测）。
		{"播放中：锚点落下 1.5 秒后，位置前进 1.5 秒", 12, t0, after(1500 * time.Millisecond), true, 240, 13.5},
		{"播放中：当前时刻即锚点时刻，位置就是锚点", 12, t0, t0, true, 240, 12},
		// 变异自证：去掉 `playing` 判断，本例红（得 22，实测）。
		{"暂停中不前进：过了 10 秒仍是锚点位置", 12, t0, after(10 * time.Second), false, 240, 12},
		// 变异自证：删掉时长约束，本例红（得 243，实测）。
		{"播放中：推过了时长，钳到时长", 238, t0, after(5 * time.Second), true, 240, 240},
		{"播放中：恰好推到时长", 235, t0, after(5 * time.Second), true, 240, 240},
		// 变异自证：时长约束只在播放中生效（条件加上 `playing`），本例红（得 250，实测）。
		{"暂停中：锚点位置本身超过时长，同样钳到时长", 250, t0, after(time.Second), false, 240, 240},
		// 变异自证：时长约束的条件改成 `durationSec >= 0`，本例红（得 0，实测）。
		{"时长为 0（未知）：不钳，照常前进", 238, t0, after(5 * time.Second), true, 0, 243},
		// 变异自证：时长约束的条件改成 `durationSec != 0`，本例红（得 -1，实测）。
		{"时长为负：按未知处理，不钳", 238, t0, after(5 * time.Second), true, -1, 243},
		// 变异自证：去掉 `!anchorTime.IsZero()`，这两例红（Sub 饱和成约 292 年：有时长的得 240，
		// 时长为 0 的得约 9.2e9，实测）。
		{"锚点时刻为零值（尚未落锚）：不前进", 12, time.Time{}, t0, true, 240, 12},
		{"锚点时刻为零值且时长未知：不前进", 12, time.Time{}, t0, true, 0, 12},
		// 变异自证：去掉 `elapsed > 0`，本例红（得 9，实测）。
		{"时间倒流（当前时刻早于锚点时刻）：不后退", 12, t0, after(-3 * time.Second), true, 240, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LivePosition(c.anchorSec, c.anchorTime, c.now, c.playing, c.duration)
			if math.Abs(float64(got-c.want)) > 1e-4 {
				t.Fatalf("LivePosition(%v, …, playing=%v, duration=%v) = %v，期望 %v",
					c.anchorSec, c.playing, c.duration, got, c.want)
			}
		})
	}
}
