package cloudmusic

// 本文件只测 domSync.observe 的判定：哪些拍算可信的跳秒，以及把时钟夹进哪个区间。
// 不测 runSession 怎样取 sent / got 与三个标志（轮询循环没有离线夹具）。时间一律由用例给定。

import (
	"math"
	"testing"
	"time"
)

// syncT0 是各对表用例的时间零点；secAt 返回零点之后 s 秒的时刻。
var syncT0 = time.Unix(1_700_000_000, 0)

func secAt(s float64) time.Time {
	return syncT0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

// usableAt 构造一拍可用的观测：在播、不在换歌前后、没有判 seek。
func usableAt(sent, got float64, sec int) domSample {
	return domSample{sent: secAt(sent), got: secAt(got), sec: sec, playing: true, sameSong: true}
}

func nearly(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-4 }

// observeStep 是用例里的一拍：喂给 observe 的观测与时钟读数，以及期望的返回值。
type observeStep struct {
	s    domSample
	pos  float32
	want float32
}

func runObserveSteps(t *testing.T, steps []observeStep) {
	t.Helper()
	var s domSync
	for i, st := range steps {
		if got := s.observe(st.s, st.pos); !nearly(got, st.want) {
			t.Errorf("第 %d 拍：observe = %.4f，期望 %.4f（时钟读数 %.4f）", i+1, got, st.want, st.pos)
		}
	}
}

// TestObserveClampsOnCredibleFlip 正例：相邻两拍都可用且秒数恰好加 1 时，把时钟夹进 [D−L, D−L+u]。
//
// 默认时间线：上一拍 sent=0、got=0.005、秒数 10；本拍 sent=0.100、got=0.105、秒数 11。
// w = 0.105，u = min(0.105, 1.005) = 0.105，区间 [10.70, 10.805]。
func TestObserveClampsOnCredibleFlip(t *testing.T) {
	prev := usableAt(0, 0.005, 10)
	cur := usableAt(0.100, 0.105, 11)
	first := func(p domSample) observeStep { return observeStep{p, 5, 5} } // 会话的第一拍：原样

	cases := []struct {
		name  string
		steps []observeStep
	}{
		// 变异自证：上下界对调、L 取反（lo = D + L）、先记本拍再比较（恒不校准），本例均红（实测）。
		{"时钟落后过多：置为下界", []observeStep{first(prev), {cur, 10.20, 10.70}}},
		// 变异自证：上界去掉 w 一项（u = 1 + r），本例红（得 11.00，实测）。
		{"时钟超前：置为上界", []observeStep{first(prev), {cur, 11.00, 10.805}}},
		// 变异自证：夹到区间中点，以下三例均红（得 10.7525，实测）。
		{"在区间内：不动", []observeStep{first(prev), {cur, 10.75, 10.75}}},
		{"恰在下界：不动", []observeStep{first(prev), {cur, 10.70, 10.70}}},
		{"恰在上界：不动", []observeStep{first(prev), {cur, 10.805, 10.805}}},
		{"跨分钟（59 → 60）", []observeStep{
			first(usableAt(0, 0.005, 59)), {usableAt(0.100, 0.105, 60), 59.00, 59.70},
		}},
		// 上一拍的 Extract 耗时 80ms：跳秒可能发生在它读 DOM 之后、返回之前，w 自上一拍的 sent 起算
		// 才盖得住。变异自证：w 改自上一拍的 got 起算（u = 0.025），本例红（得 10.725，实测）。
		{"上一拍往返耗时长：w 自上一拍的 sent 起算", []observeStep{
			first(usableAt(0, 0.080, 10)), {cur, 10.78, 10.78},
		}},
		// 相邻两拍的 sent 相隔 1.5s、本拍往返 5ms：起作用的是第二项，u = 1.005。
		// 变异自证：上界封顶在 D + 1（u = min(w, 1)），本例红（得 11.70，实测）。
		{"轮询间隔超过 1s：上界取 D−L+1+r", []observeStep{
			first(prev), {usableAt(1.500, 1.505, 11), 12.50, 11.705},
		}},
		// Extract 返回得晚（设计审查给出的时间线）：p(t) = 10.9 + t，时钟恒为 p − 0.3。本拍 sent=0.1 时
		// 页面读到 11，got=1.3，此刻 p = 12.2、时钟 11.9。u = min(1.3, 2.2) = 1.3，区间 [10.7, 12.0]，
		// 时钟是对的，不该动。变异自证：上界封顶在 D + 1，区间成了 [10.7, 11.7]，本例红（错误回拨到
		// 11.70，实测）。
		{"Extract 返回得晚：正确的时钟不被回拨", []observeStep{
			first(prev), {usableAt(0.100, 1.300, 11), 11.90, 11.90},
		}},
		{"连续两个跳秒各校一次", []observeStep{
			first(prev), {cur, 10.20, 10.70},
			{usableAt(1.000, 1.005, 11), 5, 5}, {usableAt(1.100, 1.105, 12), 12.50, 11.805},
		}},
		// 秒数不变的拍也是「上一拍」：w 从它的 sent 起算（0.105），不从更早的拍起算（0.205）。
		// 变异自证：只在跳秒的拍记上一拍，本例红（实测）。
		{"秒数不变的拍也记作上一拍", []observeStep{
			first(prev), {usableAt(0.100, 0.105, 10), 5, 5}, {usableAt(0.200, 0.205, 11), 11.00, 10.805},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runObserveSteps(t, c.steps) })
	}
}

// TestObserveIgnoresNonFlips 反例：不是可信的跳秒，一律原样返回。各例的时钟读数取 5，远在任何
// 可能的区间之外，判定一旦放行就会被夹走。
func TestObserveIgnoresNonFlips(t *testing.T) {
	const pos = 5
	keep := func(s domSample) observeStep { return observeStep{s, pos, pos} }
	prev := usableAt(0, 0.005, 10)
	next := usableAt(0.100, 0.105, 11)
	with := func(s domSample, mod func(*domSample)) domSample { mod(&s); return s }
	notPlaying := func(s *domSample) { s.playing = false }
	songChange := func(s *domSample) { s.sameSong = false }
	seeked := func(s *domSample) { s.seeked = true }

	cases := []struct {
		name  string
		steps []observeStep
	}{
		{"会话的第一拍", []observeStep{keep(next)}},
		// 变异自证：去掉「恰为加 1」的条件，以下三例均红；改为也接受加 2，「一次加 2」一例红（实测）。
		{"秒数不变", []observeStep{keep(prev), keep(usableAt(0.100, 0.105, 10))}},
		{"一次加 2", []observeStep{keep(prev), keep(usableAt(0.100, 0.105, 12))}},
		{"减 1", []observeStep{keep(prev), keep(usableAt(0.100, 0.105, 9))}},
		// −1 加 1 恰为 0：读不到进度的拍之后读到 0，不是跳秒。
		// 变异自证：usable 去掉 `sec >= 0`，本例红（实测）。
		{"上一拍读不到进度（−1 → 0）", []observeStep{keep(usableAt(0, 0.005, -1)), keep(usableAt(0.100, 0.105, 0))}},
		{"本拍读不到进度", []observeStep{keep(prev), keep(usableAt(0.100, 0.105, -1))}},
		// 恢复后的第一拍：上一拍还在暂停中。变异自证：usable 去掉 `playing`，以下三例均红（实测）。
		{"上一拍不在播（恢复后的第一拍）", []observeStep{keep(with(prev, notPlaying)), keep(next)}},
		// 暂停后 DOM 进度还会走约 0.5s，实测一例在暂停期间由 0 跳到 1；那时时钟已冻结，不能跟。
		{"本拍不在播（刚暂停，DOM 由 0 跳到 1）", []observeStep{
			keep(usableAt(0, 0.005, 0)), keep(with(usableAt(0.100, 0.105, 1), notPlaying)),
		}},
		{"两拍都不在播", []observeStep{keep(with(prev, notPlaying)), keep(with(next, notPlaying))}},
		// 变异自证：usable 去掉 `sameSong`，以下两例均红（实测）。
		{"上一拍是换歌拍或距换歌不足 500ms", []observeStep{keep(with(prev, songChange)), keep(next)}},
		{"本拍是换歌拍或距换歌不足 500ms", []observeStep{keep(prev), keep(with(next, songChange))}},
		// 变异自证：usable 去掉 `!seeked`，以下两例均红（实测）。
		{"上一拍判了 seek", []observeStep{keep(with(prev, seeked)), keep(next)}},
		{"本拍判了 seek", []observeStep{keep(prev), keep(with(next, seeked))}},
		// 隔着一个不可用的拍：可用（10）、不可用、可用（11）。不可用的拍把上一拍顶掉，第三拍没有可比的
		// 上一拍。变异自证：不可用的拍不顶掉上一拍（四个条件各注入一次：该条件不成立时不记本拍），
		// 对应的一例红（实测）。
		{"隔着读不到进度的拍", []observeStep{keep(prev), keep(usableAt(0.100, 0.105, -1)), keep(usableAt(0.200, 0.205, 11))}},
		{"隔着不在播的拍", []observeStep{
			keep(prev), keep(with(usableAt(0.100, 0.105, 10), notPlaying)), keep(usableAt(0.200, 0.205, 11)),
		}},
		{"隔着换歌前后的拍", []observeStep{
			keep(prev), keep(with(usableAt(0.100, 0.105, 10), songChange)), keep(usableAt(0.200, 0.205, 11)),
		}},
		{"隔着判了 seek 的拍", []observeStep{
			keep(prev), keep(with(usableAt(0.100, 0.105, 10), seeked)), keep(usableAt(0.200, 0.205, 11)),
		}},
		// 变异自证：去掉 `w <= 0` 的判断，以下两例均红（区间上界低于下界，时钟被夹到下界，实测）。
		{"本拍的 got 等于上一拍的 sent", []observeStep{keep(usableAt(1.000, 1.005, 10)), keep(usableAt(0.900, 1.000, 11))}},
		{"本拍的 got 早于上一拍的 sent", []observeStep{keep(usableAt(1.000, 1.005, 10)), keep(usableAt(0.800, 0.900, 11))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runObserveSteps(t, c.steps) })
	}
}
