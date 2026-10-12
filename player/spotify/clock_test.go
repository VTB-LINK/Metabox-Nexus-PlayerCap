package spotify

// 本文件是位置时钟（clock.go）的离线用例：不碰 Spotify、不碰 CDP，只喂采样与假时刻，
// 钉住「边沿落锚 + 翻转窗口 + 单事件」这三条承重行为。参照 player/applemusic/clock_test.go
// 的写法（六家里唯一把时钟行为钉住的一份，本项目照它补第二份）。

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.02 {
		t.Errorf("%s = %.3f，期望 %.3f", label, got, want)
	}
}

// 播放中：锚点之后按墙钟推进，但两次采样之间最多只许推到「采样 + 1s + 抖动容忍」（翻转窗口）。
func TestClockRunsWithinFlipWindow(t *testing.T) {
	var c clock
	if ev := c.observe(0, true, 200, t0); ev != evNone {
		t.Fatalf("首次 observe 不应返回迁移事件，得到 %v", ev)
	}
	approx(t, "t0+0.5s", c.liveAt(t0.Add(500*time.Millisecond)), 0.5)
	approx(t, "t0+1.3s", c.liveAt(t0.Add(1300*time.Millisecond)), 1.3)
	// 读数不跳就不许再往前：上限 = 0 + 1 + 0.35
	approx(t, "t0+3s（翻转窗口）", c.liveAt(t0.Add(3*time.Second)), 1+flipLagMax)
}

// 暂停：状态变化那一拍冻结在当前外推值，之后不再推进。
func TestClockFreezesOnPause(t *testing.T) {
	var c clock
	c.observe(10, true, 200, t0)
	if ev := c.observe(10, false, 200, t0.Add(500*time.Millisecond)); ev != evPause {
		t.Fatalf("暂停迁移应为 evPause，得到 %v", ev)
	}
	approx(t, "冻结值", c.liveAt(t0.Add(500*time.Millisecond)), 10.5)
	approx(t, "暂停 10 秒后", c.liveAt(t0.Add(10*time.Second)), 10.5)
}

// 恢复：只有状态迁移事件，位置从那一点继续走。
func TestClockResumesFromFrozenPosition(t *testing.T) {
	var c clock
	c.observe(30, true, 200, t0)
	c.observe(30, false, 200, t0.Add(time.Second)) // 冻结在 31
	if ev := c.observe(30, true, 200, t0.Add(2*time.Second)); ev != evResume {
		t.Fatalf("恢复迁移应为 evResume，得到 %v", ev)
	}
	approx(t, "恢复即位置", c.liveAt(t0.Add(2*time.Second)), 31)
	approx(t, "恢复后 0.5s", c.liveAt(t0.Add(2500*time.Millisecond)), 31.5)
}

// seek：显著前跳/回跳都返回 evSeek，新采样立刻成为锚点。
func TestClockDetectsSeek(t *testing.T) {
	var c clock
	c.observe(10, true, 300, t0)
	if ev := c.observe(60, true, 300, t0.Add(time.Second)); ev != evSeek {
		t.Fatalf("前跳应为 evSeek，得到 %v", ev)
	}
	approx(t, "前跳后的位置", c.liveAt(t0.Add(time.Second)), 60)

	var b clock
	b.observe(100, true, 300, t0)
	if ev := b.observe(20, true, 300, t0.Add(time.Second)); ev != evSeek {
		t.Fatalf("回跳应为 evSeek，得到 %v", ev)
	}
	approx(t, "回跳后的位置", b.liveAt(t0.Add(time.Second)), 20)
}

// 阈值以内的小幅跳变被吸收：不报 seek，只重锚（每次采样本来就该重锚）。
func TestClockAbsorbsSmallDrift(t *testing.T) {
	var c clock
	c.observe(10, true, 300, t0)
	if ev := c.observe(10.8, true, 300, t0.Add(time.Second)); ev != evNone {
		t.Fatalf("1s 内的 0.2s 偏差不应报 seek，得到 %v", ev)
	}
	approx(t, "重锚后的位置", c.liveAt(t0.Add(time.Second)), 10.8)
}

// 同一拍里既换位置又恢复播放：只返回**一个**事件（AGENTS §5.1 的「同拍只发一次
// playback_resume」在这里由类型保证）。
//
// 这一拍返回的是 evResume（不是 evSeek），这是刻意的：**seek 只在播放中判定**（暂停的时钟
// 没有可比对的"应到位置"，拿阈值去判会把一次正常的恢复当成跳转 —— 与 sodamusic 同一条规则），
// 而恢复分支会把新采样直接当锚点，所以这一条 resume 事件带的就是跳转后的位置，信息不丢。
func TestClockReturnsOneEventPerTick(t *testing.T) {
	var c clock
	c.observe(10, false, 300, t0) // 暂停在 10
	ev := c.observe(80, true, 300, t0.Add(2*time.Second))
	if ev != evResume {
		t.Fatalf("暂停中跳位再恢复应为单个 evResume，得到 %v", ev)
	}
	approx(t, "恢复事件带的位置", c.liveAt(t0.Add(2*time.Second)), 80)

	// 播放中跳位才是 seek。
	var d clock
	d.observe(10, true, 300, t0)
	if ev := d.observe(80, true, 300, t0.Add(2*time.Second)); ev != evSeek {
		t.Fatalf("播放中前跳应为 evSeek，得到 %v", ev)
	}
}

// 时长是上限；进度条的百分比算出超过时长（服务端四舍五入）时不能越界。
func TestClockClampsToDuration(t *testing.T) {
	var c clock
	c.observe(29.9, true, 30, t0)
	approx(t, "钳到时长", c.liveAt(t0.Add(5*time.Second)), 30)

	var d clock
	d.observe(0, true, 0, t0) // 时长未知
	approx(t, "时长未知时只受翻转窗口约束", d.liveAt(t0.Add(9*time.Second)), 1+flipLagMax)
}

// 没有有效采样（页面卡住/换歌间隙）时不会凭空前进：这正是翻转窗口存在的理由。
func TestClockNoRunawayWithoutSamples(t *testing.T) {
	var c clock
	c.observe(42, true, 300, t0)
	approx(t, "10 秒无采样", c.liveAt(t0.Add(10*time.Second)), 43+flipLagMax)
	// 采样恢复后照常落在新锚点上。
	if ev := c.observe(52, true, 300, t0.Add(10*time.Second)); ev != evSeek {
		t.Fatalf("采样恢复带来的前跳应为 evSeek，得到 %v", ev)
	}
	approx(t, "恢复采样后", c.liveAt(t0.Add(10*time.Second)), 52)
}

// 读数停更超过翻转窗口期间发生的前跳也要报 seek：drift 必须对照已对外报出的（被钳住的）位置，
// 而不是不受钳制的墙钟外推。这一条钉住 observe 里「先算 drift、再更新 lastRaw」的顺序。
// 变异自证：把 `c.lastRaw = rawSec` 挪到算 drift 之前 → 本例红（drift 由 2.65 变成 1.0），
// 而 TestClockDetectsSeek 仍绿（停更不足一个窗口时两种顺序结果相同，实测）。
func TestClockSeekAfterStalledReadingsUsesClampedPosition(t *testing.T) {
	var c clock
	c.observe(42, true, 300, t0)
	// 页面卡住：读数 3 秒不变（每拍都读到同一个值），外推被钳在 42 + 1.35。
	for d := 200 * time.Millisecond; d < 3*time.Second; d += 200 * time.Millisecond {
		if ev := c.observe(42, true, 300, t0.Add(d)); ev != evNone {
			t.Fatalf("读数不变不应有迁移，%v 得到 %v", d, ev)
		}
	}
	approx(t, "停更 3 秒后的对外位置", c.liveAt(t0.Add(3*time.Second)), 42+samplePeriodSec+flipLagMax)
	if ev := c.observe(46, true, 300, t0.Add(3*time.Second)); ev != evSeek {
		t.Fatalf("停更后读数比对外位置超前 2.65s 应判 evSeek，得到 %v", ev)
	}
	approx(t, "seek 之后的位置", c.liveAt(t0.Add(3*time.Second)), 46)
}

// reset 之后是全新的一首歌：旧锚点与旧时长都不再影响它。
func TestClockResetForgetsPreviousSong(t *testing.T) {
	var c clock
	c.observe(200, true, 240, t0)
	c.reset()
	if ev := c.observe(0, true, 180, t0.Add(time.Second)); ev != evNone {
		t.Fatalf("reset 之后首次 observe 不应返回迁移事件，得到 %v", ev)
	}
	approx(t, "新歌位置", c.liveAt(t0.Add(time.Second)), 0)
	approx(t, "新歌 0.5s 后", c.liveAt(t0.Add(1500*time.Millisecond)), 0.5)
}

// clockRaw 是「页面读数 → 时钟入参」的唯一闸门：非法读数返回 -1（本轮不落锚）。
func TestClockRawRejectsInvalidReadings(t *testing.T) {
	cases := []struct {
		pos, dur float32
		want     float64
	}{
		{0, 200, 0},
		{12.5, 200, 12.5},
		{-1, 200, -1},                  // 负值
		{float32(math.NaN()), 200, -1}, // NaN
		{210, 200, -1},                 // 超出时长 5% 以上
		{205, 200, 205},                // 时长零点几秒的抖动仍在容忍内
		{500, 0, 500},                  // 时长未知时不做上限判定
	}
	for _, c := range cases {
		if got := clockRaw(c.pos, c.dur); got != c.want {
			t.Errorf("clockRaw(%v, %v) = %v，期望 %v", c.pos, c.dur, got, c.want)
		}
	}
}
