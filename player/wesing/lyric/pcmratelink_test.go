package lyric

// 本文件只测前跳判定与速率核对的联动（#177 设计 §6.4 的 67～74，#190 设计 §4.4 的 S29～S37）：按 pollLyrics
// 的次序，每拍先判前跳（用本拍判定之前的暂停状态），再判暂停，再更新基准，最后把本拍的暂停判定交给 Observe。
// #190 起速率核对不再接收跳转判定的结果，自己按每拍上限作废（pcmratevoid_test.go）；这里守两者串起来时事件与
// 核对的结论。暂停判定照判定部分的写法：读数停满 1 s 起为暂停，变了即恢复（判定部分本身由 wesing 包的
// tickjudge_test.go 守）。读数一直推进的用例里不会判暂停。

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

type linkResult struct {
	v   PCMVerdict
	at  float64   // 判出的拍（一直 PCMKeep 时为 −1）
	fwd []float64 // 判为前跳的拍
	c   *PCMClock
}

// runLinked 按拍串起 SeekBase 与 Observe，直到第一个不是 PCMKeep 的结论。
func runLinked(rds []rd) linkResult {
	var s SeekBase
	c := &PCMClock{}
	var r linkResult
	paused, frozen := false, false
	var frozenSince time.Time
	last := -1.0
	for _, x := range rds {
		now, pos := atSec(x.at), float32(x.pos)
		fwd := s.Forward(pos, now, paused, false)
		if fwd {
			r.fwd = append(r.fwd, x.at)
		}
		if last >= 0 && x.pos == last {
			if !frozen {
				frozenSince, frozen = now, true
			}
			if now.Sub(frozenSince) >= time.Second {
				paused = true
			}
		} else {
			frozen, paused = false, false
		}
		last = x.pos
		s.Settle(pos, now)
		c.pos = secBytes(x.pos)
		if v := c.Observe(now, paused); v != PCMKeep {
			r.v, r.at, r.c = v, x.at, c
			return r
		}
	}
	r.v, r.at, r.c = PCMKeep, -1, c
	return r
}

// phase 是一段播放：dur 秒内读数按 rate 倍速推进（rate 为 0 即停住）。
type phase struct{ dur, rate float64 }

// phased 从 0 起每 step 秒一拍、起始读数 start，按 phases 依次推进：每拍的增量按上一拍到这一拍所在的那段算。
func phased(step, start float64, phases ...phase) []rd {
	var end float64
	for _, p := range phases {
		end += p.dur
	}
	rateAt := func(t float64) float64 {
		var acc float64
		for _, p := range phases {
			if t < acc+p.dur-1e-9 {
				return p.rate
			}
			acc += p.dur
		}
		return 0
	}
	out := []rd{{0, start}}
	pos := start
	for i := 1; ; i++ {
		at := float64(i) * step
		if at > end+1e-9 {
			return out
		}
		pos += rateAt(at-step) * step
		out = append(out, rd{at, pos})
	}
}

func repeat(n int, ps ...phase) []phase {
	var out []phase
	for i := 0; i < n; i++ {
		out = append(out, ps...)
	}
	return out
}

// 采样格式按 176400 bytes/s 换算出的速率：96 kHz、192 kHz（16 bit、双声道）。
const (
	rate96k  = 384000.0 / 176400
	rate192k = 768000.0 / 176400
)

// 变异自证（实测）：
//   - 去掉每拍上限：S29、S31～S37 红（S35、S36、S37 测出不符）；
//   - 超出范围的拍不计入期限：S31～S34 不判出而红；超出范围的拍不算推进：S29、S31～S34 判为停滞而红；
//   - 期限把暂停的拍也累计：S30、S34 红；判为暂停时清零期限：S34 红；
//   - 起始读数以首读时刻作基准：S29、S31～S34 多判出一拍前跳而红；
//   - 前跳判据恒为假、第一次变化不置 known：S32～S35 的前跳拍数红；
//   - 基准取 V 首次读到的时刻：S31、S33 多判出前跳而红。
func TestRateCheckLinkedWithSeek(t *testing.T) {
	// 交替的拍间隔：2.1 s、1.9 s……，速率 3。超前交替为 3×1.9−(1.9+2.1)=1.7 s 与 3×2.1−(2.1+1.9)=2.3 s。
	var alt []rd
	for i, at := 0, 0.0; at <= 120; i++ {
		alt = append(alt, rd{at, 30 + 3*at})
		if i%2 == 0 {
			at += 2.1
		} else {
			at += 1.9
		}
	}
	plus := func(by float64) func(float64) float64 { // 30 ms 一拍、44.1 kHz，开唱 2 s 时跳变 by 秒
		return func(at float64) float64 {
			if at >= 2-1e-9 {
				return 30 + by + at
			}
			return 30 + at
		}
	}
	interp := func(at float64) float64 { // S37：S3 的均匀插值，2.01 s 起 10 拍各 +1.953 s
		n := math.Floor((at-2.01)/0.03+1e-6) + 1
		return 30 + at + 1.953*math.Max(0, math.Min(10, n))
	}
	cases := []struct {
		name     string
		rds      []rd
		want     PCMVerdict
		at       float64 // 判出的拍；为 −1 时要求一直 PCMKeep 且核对已做过
		timedOut bool
		fwd      int // 判为前跳的拍数
	}{
		{"S29（67）2000 ms 一拍，第一次变化含 +5 s 前跳：这一拍推进 7 s 超出上限，作废",
			[]rd{{0, 30}, {2, 37}, {4, 39}, {6, 41}, {8, 43}, {10, 45}, {12, 47}, {14, 49}, {16, 51}},
			PCMKeep, -1, false, 0},
		{"S30（68）30 ms 一拍，「播 4 s、停 2 s」7 次，再连续播 12 s",
			phased(0.03, 30, append(repeat(7, phase{4, 1}, phase{2, 0}), phase{12, 1})...),
			PCMKeep, -1, false, 0},
		{"S31（69）2000 ms 一拍，96 kHz：不判前跳；每拍推进 4.35 s 超出上限 2.3 s，到期判出",
			phased(2, 30, phase{40, rate96k}), PCMRateOff, 30, true, 0},
		{"S32（70）2000 ms 一拍，192 kHz：每拍判前跳，到期判出",
			phased(2, 30, phase{80, rate192k}), PCMRateOff, 30, true, 14},
		{"S33（71）速率 3、拍间隔交替：每拍超出，到期判出",
			alt, PCMRateOff, 30.1, true, 7},
		{"S34（72）2000 ms 一拍，192 kHz，每播 10 s 停 4 s：暂停不清零累计",
			phased(2, 30, repeat(6, phase{10, rate192k}, phase{4, 0})...), PCMRateOff, 38, true, 12},
		{"S35（73）30 ms 一拍、44.1 kHz，开唱 2 s 时 +5 s", every(0.03, 0, 20, plus(5)), PCMKeep, -1, false, 1},
		{"S35（74）30 ms 一拍、44.1 kHz，正常推进 60 s", every(0.03, 0, 60, runPos(30)), PCMKeep, -1, false, 0},
		{"S36 30 ms 一拍、开唱 2 s 时 +1.5 s：不判前跳，速率核对通过", every(0.03, 0, 20, plus(1.5)), PCMKeep, -1, false, 0},
		{"S37 S3 的均匀插值：每拍超前约 1.92 s，不判前跳，速率核对通过", every(0.03, 0, 20, interp), PCMKeep, -1, false, 0},
	}
	for _, c := range cases {
		r := runLinked(c.rds)
		got := fmt.Sprintf("%v@%.2f timedOut=%v fwd=%d", r.v, r.at, r.c.RateTimedOut(), len(r.fwd))
		want := fmt.Sprintf("%v@%.2f timedOut=%v fwd=%d", c.want, c.at, c.timedOut, c.fwd)
		if math.Abs(r.at-c.at) < 0.005 {
			got = fmt.Sprintf("%v@%.2f timedOut=%v fwd=%d", r.v, c.at, r.c.RateTimedOut(), len(r.fwd))
		}
		if got != want {
			t.Errorf("%s：%s，want %s（速率 %.3f，前跳 %v）", c.name, got, want, r.c.Rate(), r.fwd)
			continue
		}
		if c.want == PCMKeep {
			if !r.c.rateDone || math.Abs(r.c.Rate()-1) > 0.02 {
				t.Errorf("%s：核对做过 = %v、速率 %.3f，want 已做过且约 1.00", c.name, r.c.rateDone, r.c.Rate())
			}
		}
		if strings.HasPrefix(c.name, "S30") && r.c.voided != 0 {
			t.Errorf("%s：期限累计 %v，want 0（暂停不累计）", c.name, r.c.voided)
		}
	}
}
