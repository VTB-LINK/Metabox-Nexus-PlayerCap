package player

// 本文件只测 ClampProgress —— 四个播放器共用的播放进度计算。
//
// 它是从 cloudmusic / kugou 两份逐字相同的私有副本 + wesing / qqmusic 的手写展开合并而来，
// 合并的前提是「四处语义完全一致」，故这里把那个语义钉死：非正时长返回 0、越界钳到 [0,1]、
// 且**任何输入都不得漏出 NaN**。

import (
	"math"
	"testing"
)

func TestClampProgressNormal(t *testing.T) {
	if got := ClampProgress(30.0, 120.0); got != 0.25 {
		t.Fatalf("ClampProgress(30,120) = %v, want 0.25", got)
	}
}

// duration 未知是常态入口（内存/Redux 尚未读到、纯音乐、切歌瞬间），必须返回 0 而非炸开。
//
// 变异自证：删掉 `if duration <= 0 { return 0 }`，本例的 0/0 子用例变红（NaN != 0）。
func TestClampProgressNonPositiveDurationReturnsZero(t *testing.T) {
	cases := []struct {
		name               string
		playTime, duration float32
	}{
		{"0/0 —— 切歌瞬间最易撞上，也是唯一会漏 NaN 的组合", 0, 0},
		{"正数/0", 30, 0},
		{"负时长", 30, -1},
		{"两者皆 0 且时长为负", 0, -5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClampProgress(c.playTime, c.duration)
			if got != 0 {
				t.Fatalf("ClampProgress(%v,%v) = %v, want 0", c.playTime, c.duration, got)
			}
		})
	}
}

// NaN 绝不能进载荷：json.Marshal 编不了 NaN → WS 写 goroutine 退出但连接不关
// → 订阅者僵死却仍在册（AGENTS.md 3.7 记过的同一个坑）。
//
// 这条是本文件存在的核心理由：ClampFloat32 **拦不住 NaN**（NaN 的两个比较都为 false，
// 原样返回），所以护栏只能在除法之前。
func TestClampProgressNeverReturnsNaN(t *testing.T) {
	cases := [][2]float32{
		{0, 0},   // → NaN，若无护栏
		{30, 0},  // → +Inf，会被钳成 1
		{0, 120}, // 正常
	}
	for _, c := range cases {
		got := ClampProgress(c[0], c[1])
		if math.IsNaN(float64(got)) {
			t.Fatalf("ClampProgress(%v,%v) 返回了 NaN；它会让 WS 的 json.Marshal 报错并僵死订阅者", c[0], c[1])
		}
		if math.IsInf(float64(got), 0) {
			t.Fatalf("ClampProgress(%v,%v) 返回了 Inf", c[0], c[1])
		}
	}
}

// 证明护栏不可移到除法之后：ClampFloat32 自己是放行 NaN 的。
// 这条钉的是「为什么写成先判 duration」，防止有人把它重构成 ClampFloat32(x/d, 0, 1) 一行。
func TestClampFloat32LetsNaNThrough(t *testing.T) {
	nan := float32(math.NaN())
	if !math.IsNaN(float64(ClampFloat32(nan, 0, 1))) {
		t.Fatal("ClampFloat32 拦住了 NaN —— 若真如此，ClampProgress 的前置护栏才可以简化；" +
			"当前实现依赖「它拦不住」这一事实")
	}
}

// 越界钳位：seek 到超出时长、或插值跑过头时，progress 不得 > 1。
func TestClampProgressClampsRange(t *testing.T) {
	if got := ClampProgress(200, 120); got != 1 {
		t.Fatalf("ClampProgress(200,120) = %v, want 1", got)
	}
	if got := ClampProgress(-5, 120); got != 0 {
		t.Fatalf("ClampProgress(-5,120) = %v, want 0", got)
	}
}
