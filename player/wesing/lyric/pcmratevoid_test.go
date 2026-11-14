package lyric

// 本文件测速率核对的每拍上限与完成期限（#190，设计 doc/design/wesing-small-seek-design.md §3.3、§4.2、§4.3、
// §4.6）：本拍推进为负、或超过「1.05 × 拍间时长 + 0.2 s」即超出范围，作废正在测的段，这一拍的拍间时长计入
// 30 s 的完成期限；期限只累计超出范围的拍，暂停既不累计也不清零。直接喂 Observe；与前跳判定串起来的用例见
// pcmratelink_test.go。用例编号 S1 起，对应设计 §4。

import (
	"bytes"
	"fmt"
	stdlog "log"
	"math"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// jump 是自 at 起叠加在读数上的跳变 by 秒（负为回退）。
type jump struct{ at, by float64 }

// counter 是 PCM 式的读数：30 s 起按 rho 倍速推进，按 50 ms 台阶向下取整（台阶相位 phi 秒），再叠加 at 不晚于
// 本拍的各次跳变。
func counter(rho, phi float64, jumps ...jump) func(float64) float64 {
	return func(at float64) float64 {
		p := 30 + 0.05*math.Floor((rho*at+phi)/0.05+1e-9)
		for _, j := range jumps {
			if at >= j.at-1e-9 {
				p += j.by
			}
		}
		return p
	}
}

// every30 是从 0 起每 30 ms 一拍、到 end 为止的读数。
func every30(end float64, pos func(float64) float64) []paceTick { return beatsEvery(0.03, 0, end, pos) }

// paceWant 是一个序列的结论：判出的拍（一直 PCMKeep 时 at 为 −1、done 为核对是否做过）、期限的累计量（为负不查）
// 与是否到期。
type paceWant struct {
	v        PCMVerdict
	at       float64
	done     bool
	voided   time.Duration
	timedOut bool
}

func keep(voided time.Duration) paceWant { return paceWant{PCMKeep, -1, true, voided, false} }

// checkPace 喂入 ticks，按 want 断言。期限累计量的容差 2 ms：拍的时刻由秒数换算，有截断。
func checkPace(t *testing.T, name string, ticks []paceTick, w paceWant) {
	t.Helper()
	v, at, c := runPace(ticks)
	if v != w.v || math.Abs(at-w.at) > 0.026 {
		t.Errorf("%s：第 %.2f s 判为 %v，want 第 %.2f s 判为 %v（速率 %.4f，期限累计 %v）", name, at, v, w.at, w.v, c.Rate(), c.voided)
		return
	}
	if v == PCMKeep && c.rateDone != w.done {
		t.Errorf("%s：核对做过 = %v，want %v（速率 %.4f）", name, c.rateDone, w.done, c.Rate())
	}
	if c.RateTimedOut() != w.timedOut {
		t.Errorf("%s：到期 = %v，want %v", name, c.RateTimedOut(), w.timedOut)
	}
	if w.voided >= 0 && (c.voided-w.voided).Abs() > 2*time.Millisecond {
		t.Errorf("%s：期限累计 %v，want %v", name, c.voided, w.voided)
	}
}

const ms = time.Millisecond

// 正例：超出范围的拍作废、计入期限，结论同没有跳变（设计 §4.2 的 S1～S9、S39～S41）。
//
// 变异自证（实测）：
//   - 去掉上限（只剩回退）：S1～S5、S8 的 53 与 55、S9、S39～S41 红（S39 测得 2.124、S40 测得 1.199，与录音 24
//     的 2.102、1.197 相符）；
//   - 回退照旧（不作废、不计入期限，只在不高于段起点时重新起算）：S6、S7、S8 的 56 与 59 红；
//   - 宽余改为 0.3 s：S1 红；
//   - 超出范围作废但不计入期限、期限累计被作废的整段时长：各例的期限断言红；
//   - 期限把暂停的拍也累计：S9 红。
func TestPCMRateCapVoids(t *testing.T) {
	// S3 的均匀插值：录音 14 的 120.13 → 139.66（19.53 s）分在 10 拍。
	var interp []jump
	for k := 0; k < 10; k++ {
		interp = append(interp, jump{2.01 + 0.03*float64(k), 1.953})
	}
	stopJump := func(at float64) float64 { // S4：2.0 s 起停住 0.8 s（判不成暂停），恢复时 +2.5 s
		base := counter(1, 0.049)
		switch {
		case at < 2:
			return base(at)
		case at < 2.8:
			return base(2)
		}
		return base(at-0.8) + 2.5
	}
	cases := []struct {
		name  string
		ticks []paceTick
		want  paceWant
	}{
		{"S1 30 ms 一拍，1.95 s 时 +0.3 s（那一拍没有落台阶，推进恰为 0.30 s）",
			every30(16, counter(1, 0.049, jump{1.95, 0.3})), keep(30 * ms)},
		{"S2 同上 +1.9 s", every30(16, counter(1, 0.049, jump{1.95, 1.9})), keep(30 * ms)},
		{"S3 录音 14 的区间按均匀插值：10 拍各 +1.953 s", every30(16, counter(1, 0.049, interp...)), keep(300 * ms)},
		{"S3 同样端点集中在一拍", every30(16, counter(1, 0.049, jump{2.01, 19.53})), keep(30 * ms)},
		{"S4 停住 0.8 s（未判暂停）后 +2.5 s", every30(16, stopJump), keep(30 * ms)},
		{"S5 2000 ms 一拍，第三拍 +0.5 s（推进 2.5 s，上限 2.3 s）",
			beatsEvery(2, 0, 30, counter(1, 0.049, jump{4, 0.5})), keep(2000 * ms)},
		{"S6 两段各一次 1.5 s 回跳、新位置仍高于段起点（#177 的 58）", cat(
			beats(0, 4, linear(30, 1)),
			beats(4.05, 9.5, linear(28.5, 1)),
			beats(9.55, 16, linear(27, 1)),
		), keep(100 * ms)},
		{"S7 0.1 s 的回退", cat(beats(0, 2.95, linear(30, 1)), beats(3, 16, linear(29.85, 1))), keep(50 * ms)},
		{"S8 #177 的 53：第一段推进 2 s 时前跳 30 s", cat(
			beats(0, 2, linear(30, 1)),
			beats(2.05, 16, linear(60, 1)),
		), keep(50 * ms)},
		{"S8 #177 的 55：第二段里前跳", cat(beats(0, 7, linear(30, 1)), beats(7.05, 20, linear(60, 1))), keep(50 * ms)},
		{"S8 #177 的 56：两段各一次 3 s、2.5 s 回跳", cat(
			beats(0, 4, linear(30, 1)),
			beats(4.05, 11, linear(27, 1)),
			beats(11.05, 20, linear(24.5, 1)),
		), keep(100 * ms)},
		{"S8 #177 的 59：重唱到段起点以下", cat(beats(0, 3, linear(30, 1)), beats(3.05, 14, linear(-3.05, 1))), keep(50 * ms)},
		{"S9 判为暂停后，恢复那一拍含暂停中的 +5 s", cat(
			beats(0, 3, linear(30, 1)),
			beats(3.05, 4, flat(33)),
			pausedTicks(beats(4.05, 6, flat(33))),
			beats(6.05, 18, linear(32, 1)),
		), keep(50 * ms)},
		{"S39 录音 24《美人鱼》：段起算后三次前跳 +0.9、+1.0、+1.5 s",
			every30(16, counter(1, 0.049, jump{1, 0.9}, jump{2, 1}, jump{3.04, 1.5})), keep(90 * ms)},
		{"S40 录音 24《Shivers》：2 s 时 +0.8 s", every30(16, counter(1, 0.049, jump{2, 0.8})), keep(30 * ms)},
		{"S41 录音 24《Sugar》：+151.44 s", every30(16, counter(1, 0.049, jump{2, 151.44})), keep(30 * ms)},
	}
	for _, c := range cases {
		checkPace(t, c.name, c.ticks, c.want)
	}
}

// 反例：没有超出范围的拍，期限累计为 0，结论同 #145（设计 §4.2 的 S10～S22）。S21（#145 的停顿用例）在
// pcmclock_test.go 的 TestPCMClockRate，结论与判出时刻不变。
//
// 变异自证（实测）：
//   - 上限不乘 1.05：S16 红；
//   - 宽余改为 0.1 s：S13、S14、S15、S18 红（pcmclock_test.go 的 F4 同样红）；
//   - 期限把暂停的拍也累计：S20 红；
//   - 回退照旧、期限累计被作废的整段时长：S22 三例红；
//   - 停着的拍不顺延起点、回退与停着的拍都不重新起算：S22「两段起点都停着」红。
func TestPCMRateCapKeeps(t *testing.T) {
	var jitter []paceTick // S10：拍间 30～45 ms 轮流变化
	for i, at := 0, 0.0; at <= 60; i++ {
		jitter = append(jitter, paceTick{at: at, pos: counter(1, 0.049)(at)})
		at += 0.030 + 0.001*float64(i%16)
	}
	s13 := cat(beatsEvery(0.03, 0, 1.98, linear(30, 1)),
		[]paceTick{{at: 2.1, pos: 32}, {at: 2.13, pos: 32.15}}, // 2.0 s 读数、被抢占到 2.1 s 才取时刻；下一拍推进 0.15 s
		beatsEvery(0.03, 2.16, 16, linear(30.02, 1)))
	cases := []struct {
		name  string
		ticks []paceTick
		want  paceWant
	}{
		{"S10 50 ms 台阶，拍间 30～45 ms，播 60 s", jitter, keep(0)},
		{"S11 一拍被拖到 500 ms，其间推进 0.5 s", cat(beats(0, 3, linear(30, 1)), beats(3.5, 16, linear(30, 1))), keep(0)},
		{"S12 读失败 2.5 s 后恢复：拍间 2.53 s、推进 2.55 s",
			cat(beatsEvery(0.03, 0, 3, linear(30, 1)), beatsEvery(0.03, 5.53, 18, linear(30.02, 1))), keep(0)},
		{"S13 上一拍读数之后被抢占 0.1 s，本拍拍间 30 ms、推进 0.15 s", s13, keep(0)},
		{"S14 #145 F4：计数每 300 ms 跳 0.15 s、速率 0.5", stepped300(), paceWant{PCMRateOff, 20.4, false, 0, false}},
		{"S15 计数每 150 ms 跳 0.15 s（速率 1）", every30(16, func(at float64) float64 {
			return 30 + 0.15*math.Floor(at/0.15+1e-6)
		}), keep(0)},
		{"S16 拍间 4 s（2000 ms 一拍、读失败一拍）、推进 4.25 s：上限 4.4 s",
			cat(beatsEvery(2, 0, 4, linear(30, 1)), beatsEvery(2, 8, 30, linear(30.25, 1))), keep(0)},
		{"S17 48 kHz、50 ms 一拍", beats(0, 12, linear(30, rate48k)), paceWant{PCMRateOff, 9.2, false, 0, false}},
		{"S17 48 kHz、2000 ms 一拍：每拍 2.18 s 不超出 2.3 s，测出不符",
			beatsEvery(2, 0, 20, linear(30, rate48k)), paceWant{PCMRateOff, 12, false, 0, false}},
		{"S18 192 kHz、30 ms 一拍：每拍 0.10～0.15 s，不超出 0.2315 s",
			every30(10, counter(rate192k, 0.049)), paceWant{PCMRateOff, 2.31, false, 0, false}},
		{"S20 停住 1 s 判暂停、再停 2 s、恢复", cat(
			beats(0, 2, linear(30, 1)),
			beats(2.05, 3, flat(32)),
			pausedTicks(beats(3.05, 5, flat(32))),
			beats(5.05, 18, linear(27, 1)),
		), keep(0)},
		// S22：#145 的重唱用例（pcmclock_test.go）结论不变，回退那一拍作废、期限累计一拍。
		{"S22 重唱之后重新起算", cat(beats(0, 3, linear(30, 1)), beats(3.05, 14, linear(-3.05, 1))), keep(50 * ms)},
		{"S22 重唱：归零后停 1 s 再走", cat(beats(0, 1, linear(30, 1)), beats(1.05, 2, flat(0)), beats(2.05, 13, linear(-2, 1))), keep(50 * ms)},
		{"S22 两段起点都停着", cat(
			beats(0, 0.9, flat(30)),
			beats(0.95, 7, linear(30-0.9, 1)),
			beats(7.05, 7.95, flat(0)),
			beats(8, 15, linear(-7.95, 1)),
		), keep(50 * ms)},
	}
	for _, c := range cases {
		checkPace(t, c.name, c.ticks, c.want)
	}
}

// S19 边界（设计 §4.2）：拍间 0.2 s 时上限恰为 0.41 s（72324 bytes），推进恰为上限不作废，多一个采样（4 bytes）作废。
// 按字节比较，避开浮点误差。
//
// 变异自证（实测）：`>` 改为 `>=`、上限不乘 1.05、宽余改为 0.1 s 或 0.19 s，「推进恰为上限」红；去掉上限、
// 宽余改为 0.3 s、超出范围不计入期限，「多一个采样」红。
func TestPCMRateCapBoundary(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		adv    uint32
		voided time.Duration
	}{
		{"推进恰为上限", 72324, 0},
		{"多一个采样", 72328, 200 * ms},
	} {
		clock := &PCMClock{pos: secBytes(30)}
		clock.Observe(t0, false)
		clock.pos += c.adv
		clock.Observe(t0.Add(200*ms), false)
		if clock.voided != c.voided {
			t.Errorf("%s：期限累计 %v，want %v", c.name, clock.voided, c.voided)
		}
	}
}

// 已接受的残余（设计 §3.5 的 1～3、§4.2 的 S42～S44）：断言现有结论。规则改动使它们变化时，同步改设计 §3.5 与
// agent-doc/14 的条目。
//
// 变异自证（实测）：宽余改为 0.19 s 或 0.1 s，S42 那一拍被作废而红；宽余改为 0.1 s，S44「10 拍各多推进 0.15 s」
// 每拍作废而红；去掉上限、超出范围不计入期限、期限累计被作废的整段时长，S44「夹一拍 +0.6 s」红。
func TestPCMRateAcceptedResiduals(t *testing.T) {
	// S42 残余 1（设计审第 1 轮 F1）：1.95 s 那拍推进 0.23 s，不超过上限 0.2315 s，不作废。第一段 4.77 s 测得约
	// 1.0545，只测完一段，仍为 PCMKeep；正常推进到 9.78 s 测满第二段，取较大者判为不符，不是期限到期。
	f1 := counter(1, 0.049, jump{1.95, 0.23})
	v, _, c := runPace(every30(4.77, f1))
	if v != PCMKeep || c.segs != 1 || math.Abs(c.Rate()-1.0545) > 0.0005 {
		t.Errorf("S42 到 4.77 s：%v、已测 %d 段、速率 %.4f，want PCMKeep、1 段、约 1.0545", v, c.segs, c.Rate())
	}
	checkPace(t, "S42 第二段测满", every30(12, f1), paceWant{PCMRateOff, 9.78, false, 0, false})

	// S44 残余 3（缓慢拖动的小步）：每拍都不超出上限，计入所在的段，测满第二段时判为不符。
	drag := func(at float64) float64 { // 2 s 起 3 s 内 2.2 倍速
		switch {
		case at < 2:
			return 30 + at
		case at < 5:
			return 32 + 2.2*(at-2)
		}
		return 38.6 + (at - 5)
	}
	var steps []jump
	for k := 0; k < 10; k++ {
		steps = append(steps, jump{2.01 + 0.03*float64(k), 0.15})
	}
	for _, c := range []struct {
		name   string
		ticks  []paceTick
		voided time.Duration
	}{
		{"S44 2.2 倍速慢拖 3 s（每拍多出约 0.036 s）", every30(16, drag), 0},
		{"S44 10 拍各多推进 0.15 s（每拍至多 0.20 s）", every30(16, counter(1, 0.049, steps...)), 0},
		{"S44 慢拖中夹一拍 +0.6 s：那一拍作废，其余小步照常计入",
			every30(16, func(at float64) float64 {
				if at >= 3.5-1e-9 {
					return drag(at) + 0.6
				}
				return drag(at)
			}), 30 * ms},
	} {
		v, at, clock := runPace(c.ticks)
		if v != PCMRateOff || clock.RateTimedOut() || clock.Rate() <= pcmRateMax || (clock.voided-c.voided).Abs() > 2*ms {
			t.Errorf("%s：第 %.2f s 判为 %v、到期 %v、速率 %.3f、期限累计 %v，want 测满两段判为不符、不到期、速率超出 1.05、期限累计 %v",
				c.name, at, v, clock.RateTimedOut(), clock.Rate(), clock.voided, c.voided)
		}
	}
}

// S43 残余 2（升级重审）：192 kHz、拍长恒为 58.5703125 ms（取整到纳秒）、位置 30 + 0.05·⌊5.1n⌋。九拍各推进
// 0.25 s、一拍推进 0.30 s，上限约 0.2615 s：每十拍作废一次，两次作废之间段只推进 2.0 s，永远测不满；期限每十拍
// 累计一拍，第 513 次作废（第 5130 拍，约 300.5 s）时满 30 s，判为不符、记为到期。#177 的规则下约 2.4 s 测出。
//
// 变异自证（实测）：去掉上限、宽余改为 0.3 s，第 40 拍测出不符而红；宽余改为 0.1 s，第 513 拍到期而红；超出
// 范围不计入期限，第 5130 拍不判出而红；期限累计被作废的整段时长，第 570 拍到期而红。
func TestPCMRateSlowExitResidual(t *testing.T) {
	const tick = 58570312 * time.Nanosecond
	t0 := time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)
	c := &PCMClock{}
	for n := 0; n <= 5200; n++ {
		c.pos = secBytes(30) + 8820*uint32(51*n/10)
		v := c.Observe(t0.Add(time.Duration(n)*tick), false)
		if n < 5130 && v != PCMKeep {
			t.Fatalf("第 %d 拍判为 %v，want 第 5130 拍才判出（期限累计 %v）", n, v, c.voided)
		}
		if n == 5130 {
			if v != PCMRateOff || !c.RateTimedOut() || c.voided != 513*tick || c.segs != 0 {
				t.Errorf("第 5130 拍：%v、到期 %v、期限累计 %v、已测 %d 段，want PCMRateOff、到期、%v、0 段", v, c.RateTimedOut(), c.voided, c.segs, 513*tick)
			}
			return
		}
	}
}

// jumpsEvery 是从 0 起每 step 秒一拍、到 end 为止的读数：1 倍速推进，从 first 起每隔 gap 秒跳变 by 秒，直到
// last 为止（含）。
func jumpsEvery(step, end, first, gap, last, by float64) []paceTick {
	var out []paceTick
	jumps := 0
	for i := 0; ; i++ {
		at := float64(i) * step
		if at > end+1e-9 {
			return out
		}
		k := math.Round((at - first) / gap)
		if k >= 0 && at <= last+1e-9 && math.Abs(at-first-k*gap) < step/2 {
			jumps++
		}
		out = append(out, paceTick{at: at, pos: 30 + at + by*float64(jumps)})
	}
}

// 前跳之后停滞检测不受影响，前跳算作推进（#177 设计 §6.4 的 60）。
//
// 变异自证（实测）：超出范围的拍不算推进（`pos > c.last` 之外再要求不超出），本例在第 5.05 s 判为停滞而红。
func TestPCMStallAfterJump(t *testing.T) {
	ticks := cat(
		beats(0, 4.85, flat(30)),
		beats(4.9, 4.9, flat(60)),
		pausedTicks(beats(4.95, 30, flat(60))),
	)
	if v, at, _ := runPace(ticks); v != PCMKeep {
		t.Errorf("第 %.2f s 判为 %v，want 一直 PCMKeep", at, v)
	}
}

// 完成期限（设计 §4.3 的 S23～S28，#177 的 61～66 改用真实的推进）：只累计超出范围的拍各自的拍间时长，暂停既不
// 累计也不清零，满 30 s 仍未测满两段即判为不符，且记为到期。
//
// 变异自证（实测）：
//   - 去掉上限：S23、S24、S26、S27 红（提前测出不符）；回退照旧：S28 红；
//   - 超出范围作废但不计入期限：S23、S24、S27、S28 不判出、S26 的期限断言红；
//   - 期限累计被作废的整段时长：S26 两例、S27、S28 红；
//   - 期限把暂停的拍也累计：S24 提前到 38 s 判出而红；判为暂停时清零期限：S24 推迟到 66 s 判出而红；
//   - 核对完成之后仍判（判定挪到 rateDone 返回之前）：S25 红。
func TestPCMRateDeadline(t *testing.T) {
	cases := []struct {
		name  string
		ticks []paceTick
		want  paceWant
	}{
		{"S23 2000 ms 一拍、每拍推进 5 s：28 s 时仍 PCMKeep，满 30 s 判出",
			beatsEvery(2, 0, 40, linear(30, 2.5)), paceWant{PCMRateOff, 30, false, 30 * time.Second, true}},
		{"S24 暂停不累计也不清零：超出 16 s、暂停 20 s，恢复后再累计 14 s", cat(
			beatsEvery(2, 0, 16, linear(30, 2.5)),
			pausedTicks(beatsEvery(2, 18, 36, flat(70))),
			beatsEvery(2, 38, 80, linear(70-2.5*36, 2.5)),
		), paceWant{PCMRateOff, 50, false, 30 * time.Second, true}},
		{"S25 核对完成之后不再计期限", cat(
			beats(0, 12, linear(30, 1)),
			beatsEvery(2, 14, 74, linear(42-2.5*12, 2.5)),
		), keep(0)},
		{"S26 开唱后前 12 s 每 4 s 前跳 5 s", jumpsEvery(0.05, 23, 4, 4, 12, 5), keep(150 * ms)},
		{"S26 开唱后 40 s 内每 4 s 前跳 5 s（10 次）", jumpsEvery(0.05, 52, 4, 4, 40, 5), keep(500 * ms)},
		{"S27 2000 ms 一拍、每 4 s 前跳 5 s：第 15 次判出（已知代价）",
			jumpsEvery(2, 90, 4, 4, 90, 5), paceWant{PCMRateOff, 60, false, 30 * time.Second, true}},
		{"S28 2000 ms 一拍、每 4 s 回跳 2.5 s：回退同样计入期限",
			jumpsEvery(2, 90, 4, 4, 90, -2.5), paceWant{PCMRateOff, 60, false, 30 * time.Second, true}},
	}
	for _, c := range cases {
		checkPace(t, c.name, c.ticks, c.want)
	}
}

// S38 作废的 Detail 每首只记一条（设计 §3.3、§4.6）：比例不对时每拍都可能超出，逐条记会刷屏。
//
// 变异自证（实测）：每次超出都记，第一首记了三条而红；「已记过」改为包级、跨首共用，两处都红；宽余改为 0.3 s，
// 第一条记的是 2.05 s 而红；去掉上限，没有作废日志而红。
func TestPCMRateVoidNoteOncePerSong(t *testing.T) {
	var buf bytes.Buffer
	w, flags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	stdlog.SetFlags(0)
	defer func() { stdlog.SetOutput(w); stdlog.SetFlags(flags) }()

	const key = "PCM rate segment voided: advanced %.2fs over %.2fs"
	prefix := strings.SplitN(i18n.T(key), "%", 2)[0]
	notes := func() []string {
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, prefix) {
				out = append(out, l)
			}
		}
		buf.Reset()
		return out
	}

	runPace(every30(30, counter(1, 0.049, jump{1.95, 0.3}, jump{5, 2}, jump{8, -1})))
	if got := notes(); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf(i18n.T(key), 0.30, 0.03)) {
		t.Errorf("一首里三次超出范围：作废日志 %q，want 恰一条、带第一次的推进 0.30s 与历时 0.03s", got)
	}
	runPace(every30(60, counter(1, 0.049)))
	if got := notes(); len(got) != 0 {
		t.Errorf("正常推进 60 s：作废日志 %q，want 没有", got)
	}
	runPace(every30(10, counter(1, 0.049, jump{2, 1})))
	if got := notes(); len(got) != 1 {
		t.Errorf("下一首再超出：作废日志 %q，want 一条", got)
	}
}
