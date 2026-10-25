package pos

// 本文件守两件事：
//   1. 结构校验按实测布局认人（rid + NUL + 15/15 + 时长 + 位置），错一位就该拒；
//   2. Tracker 能把「4 秒一次的滞后快照」变成连续位置，区分正常刷新与跳变，并在停更时冻结。
//
// 布局与节奏来自实测（9.6.0.0）：
//
//	+MUSIC_204419549\0 | 15 | 15 | 29000 | 21220 | 1 | 0 | ptr | 3 | 1
//	                                 ^dur   ^pos
//	位置每 4.000s 刷新、每次 +4000ms（rate 严格 1:1）。
//
// 变异自证：把 offPosition 换成 offDuration（拿时长当位置）→ Validate 用例红；
// 把 Stalled 的余量去掉 → 冻结用例红；把 seek 判据去掉 → 跳变用例红。

import (
	"testing"
	"time"
)

// buildStruct 按实测布局造一个结构缓冲。
func buildStruct(rid string, qualityHi, qualityLo, durMs, posMs int32) []byte {
	b := make([]byte, 64)
	copy(b, rid)
	b[len(rid)] = 0
	put := func(off int, v uint32) {
		b[off] = byte(v)
		b[off+1] = byte(v >> 8)
		b[off+2] = byte(v >> 16)
		b[off+3] = byte(v >> 24)
	}
	put(offQualityHi, uint32(qualityHi))
	put(offQualityLo, uint32(qualityLo))
	put(offDuration, uint32(durMs))
	put(offPosition, uint32(posMs))
	return b
}

func TestValidateAcceptsRealLayout(t *testing.T) {
	rid := "MUSIC_204419549"
	b := buildStruct(rid, 15, 15, 29000, 21220)
	dur, ok := Validate(b, rid)
	if !ok {
		t.Fatal("实测布局应校验通过")
	}
	if dur != 29000 {
		t.Errorf("时长=%d，应 29000", dur)
	}
}

func TestValidateRejectsBadShapes(t *testing.T) {
	rid := "MUSIC_204419549"
	cases := []struct {
		name string
		buf  []byte
		rid  string
	}{
		{"rid 不符", buildStruct("MUSIC_111111111", 15, 15, 29000, 1000), rid},
		{"位置大于时长", buildStruct(rid, 15, 15, 29000, 40000), rid},
		{"时长为 0", buildStruct(rid, 15, 15, 0, 0), rid},
		{"时长离谱", buildStruct(rid, 15, 15, 99_000_000, 1000), rid},
		{"质量槽离谱", buildStruct(rid, 9999, 15, 29000, 1000), rid},
		{"缓冲太短", buildStruct(rid, 15, 15, 29000, 1000)[:20], rid},
		{"没有 NUL 结尾", []byte("MUSIC_204419549XXXXXXXXXXXXX"), rid},
	}
	for _, c := range cases {
		if _, ok := Validate(c.buf, c.rid); ok {
			t.Errorf("%s：不该通过", c.name)
		}
	}
}

func TestTrackerInterpolatesBetweenSnapshots(t *testing.T) {
	var tr Tracker
	t0 := time.Now()
	tr.Observe(10_000, t0)

	if got := tr.Estimate(t0, 200_000); got < 9.99 || got > 10.01 {
		t.Errorf("t0 位置=%.3f，应 10.000", got)
	}
	if got := tr.Estimate(t0.Add(2*time.Second), 200_000); got < 11.99 || got > 12.01 {
		t.Errorf("t0+2s 位置=%.3f，应 12.000（靠插值补上）", got)
	}

	changed, seek := tr.Observe(14_000, t0.Add(4*time.Second))
	if !changed {
		t.Error("新快照应报告 changed")
	}
	if seek {
		t.Error("正常的 4 秒刷新不该判成 seek")
	}
	if got := tr.Estimate(t0.Add(4*time.Second), 200_000); got < 13.99 || got > 14.01 {
		t.Errorf("t0+4s 位置=%.3f，应 14.000", got)
	}
	if tr.Jumps() != 0 {
		t.Errorf("正常刷新后 jumps=%d，应 0", tr.Jumps())
	}
}

func TestTrackerDetectsSeekAndLoop(t *testing.T) {
	var tr Tracker
	t0 := time.Now()
	tr.Observe(5_000, t0)

	// 4 秒后本应约 9000，读数却是 30000：用户向前拖拽
	changed, seek := tr.Observe(30_000, t0.Add(4*time.Second))
	if !changed || !seek {
		t.Fatalf("向前拖拽应判成 seek（changed=%v seek=%v）", changed, seek)
	}
	if tr.Jumps() != 1 {
		t.Errorf("jumps=%d，应 1", tr.Jumps())
	}
	if got := tr.Estimate(t0.Add(4*time.Second), 200_000); got < 29.99 || got > 30.01 {
		t.Errorf("seek 后位置=%.3f，应 30.000", got)
	}

	// 单曲循环回到 0：读数变小，一定是跳变
	changed, seek = tr.Observe(0, t0.Add(8*time.Second))
	if !changed || !seek {
		t.Fatalf("循环回 0 应判成 seek（changed=%v seek=%v）", changed, seek)
	}
	if tr.Jumps() != 2 {
		t.Errorf("jumps=%d，应 2", tr.Jumps())
	}
}

// 首次读到的快照相位未知：+4000 只隔 1.2 秒就到，不该被误报成 seek（真机实测踩到过）。
func TestTrackerFirstUpdateIsNotSeek(t *testing.T) {
	var tr Tracker
	t0 := time.Now()
	tr.Observe(14_450, t0)
	changed, seek := tr.Observe(18_450, t0.Add(1200*time.Millisecond))
	if !changed {
		t.Fatal("应报告 changed")
	}
	if seek {
		t.Error("相位未知时的 +4000 不该判成 seek")
	}
	if tr.Jumps() != 0 {
		t.Errorf("jumps=%d，应 0", tr.Jumps())
	}
}

func TestTrackerFreezesWhenSnapshotStalls(t *testing.T) {
	var tr Tracker
	t0 := time.Now()
	tr.Observe(10_000, t0)

	if tr.Stalled(t0.Add(2 * time.Second)) {
		t.Error("2 秒内不应判定停更")
	}
	if !tr.Stalled(t0.Add(SnapshotInterval + stallSlack + 100*time.Millisecond)) {
		t.Error("超过 快照周期+余量 应判定停更")
	}
	frozen := tr.Estimate(t0.Add(30*time.Second), 200_000)
	if frozen > 10.0+float32((SnapshotInterval+stallSlack).Seconds())+0.01 {
		t.Errorf("停更后应冻结在锚点附近，实得 %.3f", frozen)
	}
}

func TestTrackerClampsToDuration(t *testing.T) {
	var tr Tracker
	t0 := time.Now()
	tr.Observe(29_000, t0) // 实测：只缓存了一部分的歌，结构里的时长就是 29000
	got := tr.Estimate(t0.Add(10*time.Second), 29_000)
	if got > 29.0 {
		t.Errorf("位置=%.3f，不该超过时长 29", got)
	}
}

func TestTrackerReset(t *testing.T) {
	var tr Tracker
	tr.Observe(5_000, time.Now())
	tr.Reset()
	if !tr.Stalled(time.Now()) {
		t.Error("Reset 后应处于停更状态")
	}
	if got := tr.Estimate(time.Now(), 100_000); got != 0 {
		t.Errorf("Reset 后位置应为 0，实得 %.3f", got)
	}
	if tr.Jumps() != 0 {
		t.Error("Reset 后 jumps 应清零")
	}
}

func TestRIDWithPrefix(t *testing.T) {
	if got := RIDWithPrefix("204419549"); got != "MUSIC_204419549" {
		t.Errorf("got %q", got)
	}
	if got := RIDWithPrefix("MUSIC_204419549"); got != "MUSIC_204419549" {
		t.Errorf("got %q", got)
	}
}
