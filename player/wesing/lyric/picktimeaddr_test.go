package lyric

// 本文件只测 pickAdvancing：播放时间有多个候选地址时，挑读数正在走的那个。

import "testing"

// fakeClockMem 按「第几轮采样」给出各候选的读数：values[addr] 是它每一轮的读数序列（不够长就沿用
// 最后一个），缺席的地址读不出。
type fakeClockMem struct {
	round  int
	values map[uint32][]float32
	bad    map[uint32]map[int]bool // 哪一轮读不出
}

func (f *fakeClockMem) read(addr uint32) (float32, bool) {
	seq, ok := f.values[addr]
	if !ok || f.bad[addr][f.round] {
		return 0, false
	}
	i := f.round
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], true
}

func (f *fakeClockMem) wait() { f.round++ }

// 真机（2026-10-09，全民K歌开着音译）：单行结构的时间恒为 0、排在前面，在走的是后面的双行结构。
// 旧实现取第一个，锁在不走的那个上。
//
// 变异自证：把 pickAdvancing 的采样循环整个删掉（不看谁在走），「两个都不为 0、在走的排在后面」
// 一例选中不走的那个而红；把比较里的 `+timeAdvanceEps` 去掉，「抖动不算在走」一例选中后一个而红；
// 把末尾挑读数不为 0 的那一段删掉（都不走时恒取第一个），「暂停着」一例选中读数为 0 的那个而红。
func TestPickAdvancingPrefersMovingCandidate(t *testing.T) {
	const stale, live = 0x103A12EC, 0x03E8FB08
	cases := []struct {
		name   string
		addrs  []uint32
		values map[uint32][]float32
		want   int
	}{
		{"在走的排在后面", []uint32{stale, live}, map[uint32][]float32{stale: {0}, live: {49.8, 49.84, 49.88}}, 1},
		{"在走的排在前面", []uint32{live, stale}, map[uint32][]float32{stale: {0}, live: {49.8, 49.84}}, 0},
		{"第三轮才看得出在走", []uint32{stale, live}, map[uint32][]float32{stale: {0}, live: {12, 12, 12, 12.03}}, 1},
		{"两个都不为 0、在走的排在后面", []uint32{stale, live}, map[uint32][]float32{stale: {30}, live: {49.8, 49.84}}, 1},
		{"抖动不算在走", []uint32{stale, live}, map[uint32][]float32{stale: {30}, live: {48, 48.005, 48.005}}, 0},
		{"暂停着：取读数不为 0 的那个", []uint32{stale, live}, map[uint32][]float32{stale: {0}, live: {239.79}}, 1},
		{"开头就暂停，都是 0：取第一个", []uint32{stale, live}, map[uint32][]float32{stale: {0}, live: {0}}, 0},
	}
	for _, c := range cases {
		f := &fakeClockMem{values: c.values}
		if got := pickAdvancing(f.read, c.addrs, timeSampleRounds, f.wait); got != c.want {
			t.Errorf("%s：选中第 %d 个，want 第 %d 个", c.name, got, c.want)
		}
	}
}

// 采样起点读不出的候选没有可比的基准，之后读出什么都不算「在走」；途中读不出的那一轮跳过。
//
// 变异自证：把 pickAdvancing 里的 `valid[i] &&` 去掉，起点读不出（基准记成 0）的那个候选被当成从 0
// 走到了 5 而红。
func TestPickAdvancingIgnoresUnreadableBaseline(t *testing.T) {
	const a, b = 0x1000, 0x2000
	f := &fakeClockMem{
		values: map[uint32][]float32{a: {5, 5, 5}, b: {30, 30.04}},
		bad:    map[uint32]map[int]bool{a: {0: true}},
	}
	if got := pickAdvancing(f.read, []uint32{a, b}, timeSampleRounds, f.wait); got != 1 {
		t.Fatalf("起点读不出的候选不该被选中，got 第 %d 个", got)
	}
	// 在走的那个中途有一轮读不出：跳过那一轮，下一轮照样认出。
	f = &fakeClockMem{
		values: map[uint32][]float32{a: {0}, b: {30, 30, 30.08}},
		bad:    map[uint32]map[int]bool{b: {1: true}},
	}
	if got := pickAdvancing(f.read, []uint32{a, b}, timeSampleRounds, f.wait); got != 1 {
		t.Fatalf("中途一轮读不出不影响认出在走的候选，got 第 %d 个", got)
	}
}
