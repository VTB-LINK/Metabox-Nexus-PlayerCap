package lyric

// 本文件只测前跳判定与它的基准（#177，设计 §4.1、§4.2、§6.1、§6.2）：判据 seekForward 本身，以及 SeekBase
// 按拍喂入读数时基准时刻怎么取、何时已知。不经 pollLyrics，时刻由用例给出。

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// 前跳判据（设计 §6.1 的 1～22）。数值取自 2026-11-13 的录音或可精确表示的边界值；经过时间是自基准时刻起
// 算的，基准比 V 首次读到早一拍，所以正常推进时是两拍。
//
// 变异自证（实测）：
//   - 判据恒为假（删去前跳判定）：1～9 红；
//   - 门控照抄 seekBack 的 v > 0：6「重唱归零后被前跳」红；
//   - 超前量不加经过时间（r − v > 2）：12「+2.1」、13「+2.5」、14、22 红；
//   - 门槛改为 1 s：14、15、22 红；改为 3 s：7 红；`>` 改为 `>=`：14 红；
//   - 去掉暂停门控：17 红；去掉最小化门控：18 红；去掉 known：16 红。
func TestSeekForward(t *testing.T) {
	cases := []struct {
		name               string
		v, elapsed, r      float32
		known, paused, min bool
		want               bool
	}{
		// 正例
		{"1 录音 +5 s 越过行首", 225.94, 0.06, 230.85, true, false, false, true},
		{"2 录音 +5 s 行内", 243.40, 0.06, 248.35, true, false, false, true},
		{"3 录音 +30 s", 249.15, 0.06, 279.00, true, false, false, true},
		{"4 拖动中的一拍", 52.40, 0.18, 78.99, true, false, false, true},
		{"5 跳到末尾附近", 100, 0.06, 285.80, true, false, false, true},
		{"6 重唱归零后 0.5 s 内被前跳 10 s", 0, 0.5, 10, true, false, false, true},
		{"7 超前 2 s 加 1 ms", 10, 0.5, 12.501, true, false, false, true},
		{"8 读失败 1 s 后恢复、其间被前跳 5 s", 50, 1.03, 56, true, false, false, true},
		{"9 还原后第一拍：外推值 100、读数 110", 100, 0.06, 110, true, false, false, true},
		// 反例
		{"10 正常推进", 100, 0.06, 100.05, true, false, false, false},
		{"11 晚一拍：没变", 100, 0.06, 100, true, false, false, false},
		{"11 晚一拍再追上", 100, 0.09, 100.10, true, false, false, false},
		{"12 轮询 2000 ms：+2.0", 100, 4.0, 102.0, true, false, false, false},
		{"12 轮询 2000 ms：+2.1（速率 1.05）", 100, 4.0, 102.1, true, false, false, false},
		{"13 调度卡顿 2.5 s", 100, 2.53, 102.5, true, false, false, false},
		{"13 调度卡顿 10 s", 100, 10.03, 110.01, true, false, false, false},
		{"13 读失败 1 s 后恢复、正常推进", 100, 1.03, 101.0, true, false, false, false},
		{"14 超前恰为 2 s", 10, 0.5, 12.5, true, false, false, false},
		{"15 小幅拖动：超前 1.9 s", 100, 0.06, 101.96, true, false, false, false},
		{"16 基准未知（本次第一个读数）", 100, 0.06, 110, false, false, false, false},
		{"17 已判暂停、停住 1.5 s 后 +30 s", 100, 1.5, 130, true, true, false, false},
		{"18 判为最小化、读数超前外推值 10 s", 100, 0.06, 110, true, false, true, false},
		{"19 读数变小 5 s", 100, 0.06, 95, true, false, false, false},
		{"20 暂停 30 s 后恢复的第一拍", 100, 30, 100.05, true, true, false, false},
		{"21 停住 0.8 s 后接着走", 100, 0.86, 100.05, true, false, false, false},
		{"22 停住 0.8 s 后被前跳 2.5 s", 100, 0.86, 102.5, true, false, false, false},
	}
	for _, c := range cases {
		if got := seekForward(c.v, c.elapsed, c.r, c.known, c.paused, c.min); got != c.want {
			t.Errorf("%s：seekForward(v=%v, elapsed=%v, r=%v, known=%v, paused=%v, min=%v) = %v，want %v",
				c.name, c.v, c.elapsed, c.r, c.known, c.paused, c.min, got, c.want)
		}
	}
}

// rd 是一拍成功的读数：距起点的秒数与读数（秒）。
type rd struct{ at, pos float64 }

var seekT0 = time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)

func atSec(s float64) time.Time { return seekT0.Add(time.Duration(math.Round(s * 1e9))) }

// feedBase 按拍喂入读数（不判暂停、不最小化，读数即报出的位置），返回判为前跳的那些拍的 at。
func feedBase(rds []rd) []float64 {
	var s SeekBase
	var fwd []float64
	for _, x := range rds {
		now, r := atSec(x.at), float32(x.pos)
		if s.Forward(r, now, false, false) {
			fwd = append(fwd, x.at)
		}
		s.Settle(r, now)
	}
	return fwd
}

// every 从 at0 起每 step 秒一拍、到 at1 为止（含），读数为 pos(at)。
func every(step, at0, at1 float64, pos func(at float64) float64) []rd {
	var out []rd
	for i := 0; ; i++ {
		at := at0 + float64(i)*step
		if at > at1+1e-9 {
			return out
		}
		out = append(out, rd{at, pos(at)})
	}
}

func joinRd(parts ...[]rd) []rd {
	var out []rd
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func constPos(v float64) func(float64) float64 { return func(float64) float64 { return v } }

func runPos(start float64) func(float64) float64 {
	return func(at float64) float64 { return start + at }
}

// 基准（设计 §6.2 的 23～31）：基准时刻取读到 V 之前最后一次读到旧值的时刻；起始读数第一次变化之前不判。
// 23～26 是慢轮询、调度延迟与拖窗冻结的组合：界面在 V 生效后冻结，首次读到 V 时它已停了一段，松手后读数
// 追上墙钟，伴奏一直正常播放，不是跳转。
//
// 变异自证（实测）：
//   - 基准取 V 首次读到的时刻（第 1 版）：23、24、25、26 红；
//   - 基准时刻每拍更新（不论值是否变化）：25、26、27 红（23、24 里 V 只读到一拍，两种取法相同）；
//   - 超前量不加经过时间：23～27 红；
//   - 起始读数以首读时刻作基准（第一个读数即 known）：28 红；
//   - Forward 恒为假：30、31 红。
func TestSeekBaseSequences(t *testing.T) {
	cases := []struct {
		name string
		rds  []rd
		want []float64 // 判为前跳的拍
	}{
		{"23 设计审 F1：2000 ms 一拍，50.05 在 0.05 s 生效后冻结",
			[]rd{{0, 50.00}, {2.10, 50.05}, {4.20, 54.20}}, nil},
		{"24 慢轮询加调度延迟",
			[]rd{{0, 50.00}, {2.60, 50.05}, {4.65, 54.65}}, nil},
		{"25 30 ms 一拍，拖窗冻结与我方卡顿叠加",
			joinRd([]rd{{0, 50.00}}, every(0.03, 2.53, 3.97, constPos(50.05)), []rd{{4.00, 54.00}}), nil},
		{"26 2000 ms 一拍，首次读到晚 0.5 s，按住 10 s",
			joinRd([]rd{{0, 50.00}}, every(2, 2.50, 12.50, constPos(50.05)), []rd{{14.50, 64.50}}), nil},
		{"27 30 ms 一拍，拖窗停 2.3 s 后追上",
			joinRd(every(0.03, 0, 2.97, runPos(47)), every(0.03, 3.00, 5.28, constPos(50.00)), every(0.03, 5.31, 7, runPos(47))), nil},
		{"28 本次第一个读数已陈旧（拖窗跨过宣告）",
			joinRd(every(0.03, 0, 1.00, constPos(50.00)), every(0.03, 1.03, 3, runPos(53))), nil},
		{"29 读数来回",
			joinRd([]rd{{0, 50.00}, {0.03, 49.95}, {0.06, 50.00}}, every(0.03, 0.09, 2, runPos(49.94))), nil},
		{"30 正例：24 的末拍被前跳 5 s",
			[]rd{{0, 50.00}, {2.60, 50.05}, {4.65, 59.65}}, []float64{4.65}},
		{"31 正例：30 ms 一拍，停 2.3 s 后被前跳 5 s",
			joinRd(every(0.03, 0, 2.97, runPos(47)), every(0.03, 3.00, 5.28, constPos(50.00)), []rd{{5.31, 57.31}}, every(0.03, 5.34, 7, runPos(52))),
			[]float64{5.31}},
	}
	for _, c := range cases {
		if got := feedBase(c.rds); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s：判为前跳的拍 %v，want %v", c.name, got, c.want)
		}
	}
}

// 基准已知（设计 §4.1）：起始读数第一次变化之后 known 才为真，此后一直为真；本次第一个读数本身不算。#190 起
// Settle 不再把第一次变化报给速率核对（速率核对按自己的上一拍判，见 pcmratevoid_test.go），这里只守 known。
//
// 变异自证（实测）：起始读数以首读时刻作基准（第一个读数即 known），三例都红；第一次变化不置 known，后两例红；
// 回退时清掉 known，「之后的变化」红。
func TestSeekBaseKnown(t *testing.T) {
	cases := []struct {
		name string
		pos  []float64 // 每 30 ms 一拍
		want int       // known 从第几拍（下标）起为真；−1 为始终不真
	}{
		{"第一个读数", []float64{30}, -1},
		{"停着、之后第一次变化", []float64{30, 30, 30, 30.05}, 3},
		{"之后的变化", []float64{30, 30.05, 30.10, 30.15, 0, 0.05}, 1},
	}
	for _, c := range cases {
		var s SeekBase
		got := -1
		for i, p := range c.pos {
			s.Settle(float32(p), atSec(0.03*float64(i)))
			if s.known && got < 0 {
				got = i
			}
			if got >= 0 && !s.known {
				t.Errorf("%s：第 %d 拍 known 又变回假", c.name, i)
			}
		}
		if got != c.want {
			t.Errorf("%s：known 从第 %d 拍起为真，want %d", c.name, got, c.want)
		}
	}
}
