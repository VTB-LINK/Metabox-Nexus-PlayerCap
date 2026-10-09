package manager

import (
	"reflect"
	"testing"
	"time"
)

// subsOf 把一个「被订阅命中」的名字集合包成 hasSub 闭包，模拟 sink.HasSubscribers 的语义
// （per-player 命中，或 root 下的 active —— 两者对调用方都表现为「该家 hasSub 为真」）。
func subsOf(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(n string) bool { return set[n] }
}

func setOf(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestDesiredCaptures(t *testing.T) {
	all := []string{"kugou", "wesing", "qqmusic", "cloudmusicv3", "sodamusic"}

	cases := []struct {
		name    string
		hasRoot bool
		playing map[string]bool
		hasSub  func(string) bool
		want    map[string]bool
	}{
		{
			// per-player 订阅 qqmusic：入选，且无 root 时在播集合不预热。
			name:    "per-player only",
			hasRoot: false,
			playing: setOf("kugou"),
			hasSub:  subsOf("qqmusic"),
			want:    setOf("qqmusic"),
		},
		{
			// root 订阅 + active=kugou（经 hasSub 命中）+ 在播集合 {kugou,wesing} + per-player qqmusic。
			// 期望：active(kugou) ∪ 预热在播(kugou,wesing) ∪ per-player(qqmusic)。
			name:    "root warms playing set",
			hasRoot: true,
			playing: setOf("kugou", "wesing"),
			hasSub:  subsOf("kugou", "qqmusic"),
			want:    setOf("kugou", "wesing", "qqmusic"),
		},
		{
			// 无 root：只认 per-player，在播集合（kugou）不预热。
			name:    "no root, no warmup",
			hasRoot: false,
			playing: setOf("kugou", "wesing"),
			hasSub:  subsOf("qqmusic"),
			want:    setOf("qqmusic"),
		},
		{
			// 无任何订阅：空集（上游据此拆 helper、不扫不抓）。
			name:    "no subscribers",
			hasRoot: false,
			playing: setOf("kugou"),
			hasSub:  subsOf(),
			want:    map[string]bool{},
		},
		{
			// active 经 hasSub 恒入选，即便它不在 playing 集合里（root-only 且 active 未在播的边角）。
			name:    "active via hasSub always in",
			hasRoot: true,
			playing: setOf(),
			hasSub:  subsOf("cloudmusicv3"),
			want:    setOf("cloudmusicv3"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := desiredCaptures(all, c.hasRoot, c.playing, c.hasSub)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("desiredCaptures = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIdlePlayers 覆盖空闲帧判定：有订阅且（从未转发，或距最近转发严格大于 noCaptureAfter）才推，
// 恰好等于阈值不推，无订阅不推；结果按 names 顺序。
func TestIdlePlayers(t *testing.T) {
	// 阈值写进了 doc/openapi.yaml 与 AGENTS.md（约 300 毫秒），改值须同步文档。
	if noCaptureAfter != 300*time.Millisecond {
		t.Fatalf("noCaptureAfter = %v, 文档写的是 300ms", noCaptureAfter)
	}
	names := []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic"}
	now := time.Now()
	recent := now.Add(-100 * time.Millisecond)
	over := now.Add(-noCaptureAfter - time.Millisecond)

	cases := []struct {
		name   string
		hasSub func(string) bool
		last   map[string]time.Time
		want   []string
	}{
		{"有订阅且从未转发", subsOf("kugou"), nil, []string{"kugou"}},
		{"有订阅且阈值内转发过", subsOf("kugou"), map[string]time.Time{"kugou": recent}, nil},
		{"有订阅且超过阈值", subsOf("kugou"), map[string]time.Time{"kugou": over}, []string{"kugou"}},
		{"有订阅且恰好等于阈值", subsOf("kugou"), map[string]time.Time{"kugou": now.Add(-noCaptureAfter)}, nil},
		{"无订阅且从未转发", subsOf(), nil, nil},
		{"无订阅且超过阈值", subsOf(), map[string]time.Time{"kugou": over}, nil},
		{"多播放器混合", subsOf("wesing", "qqmusic", "kugou", "sodamusic"),
			map[string]time.Time{"qqmusic": recent, "kugou": over, "cloudmusicv3": over},
			[]string{"wesing", "kugou", "sodamusic"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := idlePlayers(names, c.hasSub, c.last, now, noCaptureAfter); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("idlePlayers = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestJudgeWatchdog 覆盖看门狗判定：stale 与播放状态无关；只有「本次下发后收到过真帧且未超阈值」
// 的行算 helper 存活证据，基线、has=false、dispatchedAt 零值都不算。
func TestJudgeWatchdog(t *testing.T) {
	now := time.Now()
	dispatched := now.Add(-5 * time.Second)
	over := now.Add(-hangThreshold - time.Second)
	recent := now.Add(-100 * time.Millisecond)

	stuck := func(p string) frameSnap { return frameSnap{player: p, last: over, has: true, dispatchedAt: over} }
	cases := []struct {
		name      string
		rows      []frameSnap
		wantStale []string
		wantKill  bool
	}{
		{"单个 stale 无其他行", []frameSnap{stuck("kugou")}, []string{"kugou"}, true},
		{"stale + 另一行本次下发后有真帧", []frameSnap{
			stuck("kugou"),
			{player: "qqmusic", last: recent, has: true, dispatchedAt: dispatched},
		}, []string{"kugou"}, false},
		{"stale + 另一行只有基线未超阈值", []frameSnap{
			stuck("kugou"),
			{player: "qqmusic", last: recent, has: true, dispatchedAt: recent},
		}, []string{"kugou"}, true},
		{"stale + 另一行 has=false", []frameSnap{
			stuck("kugou"),
			{player: "qqmusic", has: false, dispatchedAt: dispatched},
		}, []string{"kugou"}, true},
		{"stale + 另一行有近帧但 dispatchedAt 为零值", []frameSnap{
			stuck("kugou"),
			{player: "qqmusic", last: recent, has: true},
		}, []string{"kugou"}, true},
		// 本行对应的播放器处于暂停：helper 存活时暂停也出保活零帧，无帧即故障，照样列入 stale。
		{"暂停中超阈值", []frameSnap{{player: "kugou", last: over, has: true, dispatchedAt: dispatched}}, []string{"kugou"}, true},
		{"has=false 超阈值不列入", []frameSnap{{player: "kugou", last: over, has: false, dispatchedAt: dispatched}}, nil, false},
		{"多个 stale + 一行真帧", []frameSnap{
			stuck("kugou"),
			stuck("wesing"),
			{player: "qqmusic", last: recent, has: true, dispatchedAt: dispatched},
		}, []string{"kugou", "wesing"}, false},
		{"全部有真帧", []frameSnap{
			{player: "kugou", last: recent, has: true, dispatchedAt: dispatched},
			{player: "qqmusic", last: recent, has: true, dispatchedAt: dispatched},
		}, nil, false},
		{"只有基线未超阈值", []frameSnap{{player: "kugou", last: recent, has: true, dispatchedAt: recent}}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stale, kill := judgeWatchdog(c.rows, now)
			if !reflect.DeepEqual(stale, c.wantStale) || kill != c.wantKill {
				t.Fatalf("judgeWatchdog = (%v, %v), 期望 (%v, %v)", stale, kill, c.wantStale, c.wantKill)
			}
		})
	}
}

// TestRecordCaptureFailureQuick 模拟「每次下发 3s 后失败」：前 4 次未放弃、退避依次 1/2/4/8s，
// 第 5 次放弃。次数按字面写死，放弃阈值被改动即红。
func TestRecordCaptureFailureQuick(t *testing.T) {
	var f captureFailure
	at := time.Now()
	wantBackoff := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i := 1; i <= 5; i++ {
		dispatchedAt := at
		at = dispatchedAt.Add(3 * time.Second)
		f = recordCaptureFailure(f, at, dispatchedAt, 100)
		if f.count != i || f.pid != 100 {
			t.Fatalf("第 %d 次失败后 count=%d pid=%d, 期望 count=%d pid=100", i, f.count, f.pid, i)
		}
		if i < 5 {
			if f.givenUp {
				t.Fatalf("第 %d 次失败不应放弃", i)
			}
			if got := f.retryAt.Sub(at); got != wantBackoff[i-1] {
				t.Fatalf("第 %d 次失败退避 = %v, 期望 %v", i, got, wantBackoff[i-1])
			}
			at = f.retryAt
		} else if !f.givenUp {
			t.Fatalf("第 %d 次失败应放弃", i)
		}
	}
}

// TestRecordCaptureFailureWindow 覆盖次数重置与下发时刻缺失两条规则。
func TestRecordCaptureFailureWindow(t *testing.T) {
	at := time.Now()
	prev := captureFailure{count: 3}

	// 本次捕获跑满窗口后才失败：次数重置为 1，退避回到 backoffBase。
	f := recordCaptureFailure(prev, at, at.Add(-quickFailWindow), 100)
	if f.count != 1 || f.givenUp || f.retryAt.Sub(at) != backoffBase {
		t.Fatalf("窗口外失败 = %+v, 期望 count=1、未放弃、退避 %v", f, backoffBase)
	}

	// 下发时刻缺失按不足窗口处理：加 1。
	f = recordCaptureFailure(prev, at, time.Time{}, 100)
	if f.count != 4 {
		t.Fatalf("下发时刻缺失时 count = %d, 期望 4", f.count)
	}
}

// TestCaptureBackoffCap 断言退避逐次翻倍、不超过 backoffCap（n 很大时也不溢出）。
func TestCaptureBackoffCap(t *testing.T) {
	for n, want := range map[int]time.Duration{1: backoffBase, 2: 2 * backoffBase, 4: 8 * backoffBase} {
		if got := captureBackoff(n); got != want {
			t.Fatalf("captureBackoff(%d) = %v, 期望 %v", n, got, want)
		}
	}
	for _, n := range []int{6, 7, 40, 100} {
		if got := captureBackoff(n); got != backoffCap {
			t.Fatalf("captureBackoff(%d) = %v, 期望封顶 %v", n, got, backoffCap)
		}
	}
}

// TestCaptureStable 断言下发后跑满 quickFailWindow 才算跑稳。
func TestCaptureStable(t *testing.T) {
	d := time.Now()
	if !captureStable(d, d.Add(quickFailWindow)) {
		t.Fatalf("跑满窗口应为真")
	}
	if captureStable(d, d.Add(quickFailWindow-time.Millisecond)) {
		t.Fatalf("不足窗口应为假")
	}
	if captureStable(time.Time{}, d) {
		t.Fatalf("下发时刻缺失应为假")
	}
}

// TestFilterWant 覆盖过滤：退避期内剔除、到期放行、已放弃剔除、PID 变化清账并放行、无记账放行。
func TestFilterWant(t *testing.T) {
	now := time.Now()
	want := map[string]uint32{"kugou": 1, "wesing": 2, "qqmusic": 3, "sodamusic": 40, "cloudmusicv3": 5}
	fails := map[string]captureFailure{
		"kugou":     {count: 1, retryAt: now.Add(time.Second), pid: 1},
		"wesing":    {count: 2, retryAt: now, pid: 2},
		"qqmusic":   {count: 5, givenUp: true, pid: 3},
		"sodamusic": {count: 5, givenUp: true, pid: 4},
	}
	filterWant(want, fails, now)

	if got, exp := want, map[string]uint32{"wesing": 2, "sodamusic": 40, "cloudmusicv3": 5}; !reflect.DeepEqual(got, exp) {
		t.Fatalf("过滤后 want = %v, 期望 %v", got, exp)
	}
	if _, ok := fails["sodamusic"]; ok {
		t.Fatalf("PID 变化后应清除 sodamusic 的记账")
	}
	if _, ok := fails["qqmusic"]; !ok {
		t.Fatalf("PID 未变时已放弃的记账应保留")
	}
}
