package lyric

// 本文件测选中之后的伴奏解码器（设计 §4.2）：每拍读数与逐拍核对（PCMClock.Read），速率核对与停滞检测
// （PCMClock.Observe）。时刻由用例给出，不睡眠。

import (
	"math"
	"testing"
	"time"
)

// 逐拍核对：虚表不等、解码字节超过总字节、读不出都记为读失败；总字节不等于宣告时的值即判换歌。
//
// 变异自证（实测）：去掉虚表判断，「虚表变了」「虚表与总字节都变了」红；总字节变化也记为读失败，「总字节
// 变了」「关窗：总字节为 0」红；先判总字节再判虚表，「虚表与总字节都变了」红；去掉 pos <= total，「解码字节
// 超过总字节」红；改成 pos < total，「解码字节等于总字节」红。
func TestPCMClockRead(t *testing.T) {
	const inst = 0x0B0E0000
	total := secBytes(183.693)
	cases := []struct {
		name  string
		frame func(f *fakeMem)
		want  float32
		st    PCMRead
	}{
		{"对照组", func(f *fakeMem) { f.put32(inst+pcmPosOff, secBytes(12.3)) }, float32(float64(secBytes(12.3)) / pcmBytesPerSec), PCMOK},
		{"解码字节为 0", func(f *fakeMem) { f.put32(inst+pcmPosOff, 0) }, 0, PCMOK},
		{"解码字节等于总字节", func(f *fakeMem) { f.put32(inst+pcmPosOff, total) }, float32(float64(total) / pcmBytesPerSec), PCMOK},
		{"解码字节超过总字节", func(f *fakeMem) { f.put32(inst+pcmPosOff, total+1) }, 0, PCMBad},
		{"虚表变了", func(f *fakeMem) { f.put32(inst, testPCMVtable+4) }, 0, PCMBad},
		{"总字节变了", func(f *fakeMem) { f.put32(inst+pcmTotalOff, secBytes(201.5)) }, 0, PCMSongChanged},
		{"关窗：总字节为 0", func(f *fakeMem) { f.put32(inst+pcmTotalOff, 0); f.put32(inst+pcmPosOff, 0) }, 0, PCMSongChanged},
		{"虚表与总字节都变了", func(f *fakeMem) { f.put32(inst, 0); f.put32(inst+pcmTotalOff, 0) }, 0, PCMBad},
		{"读不出", func(f *fakeMem) { delete(f.b, inst) }, 0, PCMBad},
		{"只读得出前 16 字节", func(f *fakeMem) { delete(f.b, inst+pcmPosOff+3) }, 0, PCMBad},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.pcmInst(inst, testPCMVtable, accPath, total, secBytes(1))
		clock, err := confirmPCM(f, testPCMVtable, inst, testSongMID, func() {})
		if err != nil {
			t.Fatalf("%s：用例前提不成立：%v", c.name, err)
		}
		c.frame(f)
		if got, st := clock.Read(); got != c.want || st != c.st {
			t.Errorf("%s：得到 (%v, %v)，want (%v, %v)", c.name, got, st, c.want, c.st)
		}
	}
}

// 读失败的那一拍不改动上一次读到的位置：Observe 只看读数成功的拍（pollLyrics 读失败即 continue）。
//
// 变异自证（实测）：Read 在判定之前就记下读到的解码字节，读失败那一拍之后 Position 变成越界值而红。
func TestPCMClockReadKeepsLastGoodPosition(t *testing.T) {
	const inst = 0x0B0E0000
	f := newFakeMem()
	total := secBytes(183.693)
	f.pcmInst(inst, testPCMVtable, accPath, total, secBytes(1))
	clock, _ := confirmPCM(f, testPCMVtable, inst, "", func() {})
	f.put32(inst+pcmPosOff, secBytes(2))
	clock.Read()
	f.put32(inst+pcmPosOff, total+1)
	if _, st := clock.Read(); st != PCMBad {
		t.Fatalf("用例前提不成立：%v", st)
	}
	if got, want := clock.Position(), float32(float64(secBytes(2))/pcmBytesPerSec); got != want {
		t.Errorf("读失败之后位置为 %v，want 上一次成功读到的 %v", got, want)
	}
}

// paceTick 是一拍：距进入轮询的秒数 at、解码位置 pos（秒）、本拍是否已判为暂停。
type paceTick struct {
	at, pos float64
	paused  bool
}

// beats 从 at0 起每 50 ms 一拍、到 at1 为止（含），位置为 pos(at)，都没判为暂停。
func beats(at0, at1 float64, pos func(at float64) float64) []paceTick {
	return beatsEvery(0.05, at0, at1, pos)
}

// beatsEvery 同 beats，一拍 step 秒。
func beatsEvery(step, at0, at1 float64, pos func(at float64) float64) []paceTick {
	var out []paceTick
	for i := 0; ; i++ {
		at := at0 + float64(i)*step
		if at > at1+1e-9 {
			return out
		}
		out = append(out, paceTick{at: at, pos: pos(at)})
	}
}

// paused 把 ticks 都标为已判为暂停。
func pausedTicks(ticks []paceTick) []paceTick {
	for i := range ticks {
		ticks[i].paused = true
	}
	return ticks
}

func cat(parts ...[]paceTick) []paceTick {
	var out []paceTick
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// runPace 依次喂入各拍，返回第一个不是 PCMKeep 的结论与那一拍的 at（一直是 PCMKeep 时 at 为 −1）。
func runPace(ticks []paceTick) (PCMVerdict, float64, *PCMClock) {
	t0 := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
	c := &PCMClock{}
	for _, tk := range ticks {
		c.pos = secBytes(tk.pos)
		if v := c.Observe(t0.Add(time.Duration(tk.at*float64(time.Second))), tk.paused); v != PCMKeep {
			return v, tk.at, c
		}
	}
	return PCMKeep, -1, c
}

func linear(start, rate float64) func(float64) float64 {
	return func(at float64) float64 { return start + rate*at }
}

func flat(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// stalls 是从 30 s 起按 1 倍速推进、在 [from, to] 之间停住（判不成暂停）的位置，可有多段停顿。
func stalls(pairs ...[2]float64) func(float64) float64 {
	return func(at float64) float64 {
		lost := 0.0
		for _, s := range pairs {
			switch {
			case at > s[1]:
				lost += s[1] - s[0]
			case at > s[0]:
				lost += at - s[0]
			}
		}
		return 30 + at - lost
	}
}

// stepped300 是 30 ms 一拍、计数每 300 ms 跳 26460 bytes（0.15 s，速率 0.5）的读数，到 22 s 为止。
func stepped300() []paceTick {
	var out []paceTick
	for i := 0; i <= 733; i++ {
		out = append(out, paceTick{at: 0.03 * float64(i), pos: 30 + 0.15*float64(i/10)})
	}
	return out
}

// 速率核对（设计 §4.2）：按段测，一段是推进中、没有判为暂停的区间，推进满 5 s 时算一次 Δpos / Δ墙钟；
// 测满两段取较大的一个判定，须在 [0.95, 1.05]，每首只判一次。停顿只会拉低一段的速率，一段干净即可判对
// （代码审 F1、F4、F5）。一段从读数开始推进的那一拍起算：停着的拍（开播前、重唱归零之后）不算进去；暂停只
// 作废正在测的那一段。回退与快于合格速率的推进由 #190 的每拍上限作废，见 pcmratevoid_test.go；本文件的用例
// 里只有重唱的那一拍回退（作废一拍，结论不变）。
//
// 变异自证（实测）：
//   - 不看暂停（暂停不打断区间）：「暂停打断推进区间」按 5/7 判为不合格而红；
//   - 回退与停着的拍都不重新起算、停着的拍不顺延起点（只在回退时重新起算）：「两段起点都停着」按约 0.855 判为
//     不合格而红（#190 起回退那一拍由每拍上限作废，「重唱之后重新起算」不再靠这里重新起算；只有一段起点停着的
//     「开播前停 0.9 s」「重唱：归零后停 1 s 再走」靠另一段通过，拦不住）；
//   - 下限改成 0.96、上限改成 1.04：「速率 0.951」「速率 1.049」红；去掉下限：「速率 0.5」「速率 0.949」红；
//     去掉上限：「速率 1.088」「速率 1.051」红；
//   - 推进量改成 4 s：「速率 1.088」在 7.40 s 而不是 9.20 s 判出而红（另几例同样提前）；
//   - 判定之后不停、重新测两段：「每首只判一次」在速率 2 那两段之后判为不合格而红；
//   - 只测一段（pcmRateSegs 改成 1）：两例「只测完一段不判」与多例在第一段末判出而红，「F5」通过而红；
//   - 第二段不从第一段结束那一拍起算（沿用第一段的起点）：多例在第一段末的下一拍判出而红；
//   - 取较小的一个：「F1：停顿在第一段」「停顿在第二段」「1000 ms 一拍」判为不合格、「F5」「第一段与第二段
//     之间暂停」通过而红；
//   - 取第二段的：「停顿在第二段」判为不合格、「第一段与第二段之间暂停」通过而红；
//   - 暂停时连已测完的段一起作废：「第一段与第二段之间暂停」在第二段末不判而红。
func TestPCMClockRate(t *testing.T) {
	cases := []struct {
		name   string
		ticks  []paceTick
		want   PCMVerdict
		at     float64 // 判出的拍（不判出时 −1）
		rateOK bool    // 不判出时，判定是否已经做过
	}{
		{"速率 1.00", beats(0, 12, linear(30, 1)), PCMKeep, -1, true},
		{"速率 1.00：只测完一段不判", beats(0, 9.9, linear(30, 1)), PCMKeep, -1, false},
		{"速率 1.088（48 kHz 按 44.1 kHz 换算）", beats(0, 12, linear(30, rate48k)), PCMRateOff, 9.2, false},
		{"速率 1.088：只测完一段不判", beats(0, 9.15, linear(30, rate48k)), PCMKeep, -1, false},
		{"速率 0.5", beats(0, 22, linear(30, 0.5)), PCMRateOff, 20, false},
		{"速率 0.951", beats(0, 12, linear(30, 0.951)), PCMKeep, -1, true},
		{"速率 1.049", beats(0, 12, linear(30, 1.049)), PCMKeep, -1, true},
		{"速率 0.949", beats(0, 12, linear(30, 0.949)), PCMRateOff, 10.6, false},
		{"速率 1.051", beats(0, 12, linear(30, 1.051)), PCMRateOff, 9.6, false},
		{"不足 5 s 不核对", beats(0, 4.9, linear(30, 0.5)), PCMKeep, -1, false},
		// 推进 3 s，停 2 s（后 1 s 判为暂停），恢复后再推进。不打断的话 5 s 推进花了 7 s 墙钟。
		{"暂停打断推进区间", cat(
			beats(0, 3, linear(30, 1)),
			beats(3.05, 4, flat(33)),
			pausedTicks(beats(4.05, 5, flat(33))),
			beats(5.05, 16, linear(33-5, 1)),
		), PCMKeep, -1, true},
		{"一直暂停：不核对", cat(beats(0, 1, flat(30)), pausedTicks(beats(1.05, 4.9, flat(30)))), PCMKeep, -1, false},
		{"重唱之后重新起算", cat(beats(0, 3, linear(30, 1)), beats(3.05, 14, linear(-3.05, 1))), PCMKeep, -1, true},
		{"开播前停 0.9 s", cat(beats(0, 0.9, flat(30)), beats(0.95, 12, linear(30-0.9, 1))), PCMKeep, -1, true},
		{"重唱：归零后停 1 s 再走", cat(beats(0, 1, linear(30, 1)), beats(1.05, 2, flat(0)), beats(2.05, 13, linear(-2, 1))), PCMKeep, -1, true},
		// 两段的起点都有停着的拍：开播前停 0.9 s；第二段里重唱，归零后停 0.9 s。不顺延的话两段都约 0.847。
		{"两段起点都停着", cat(
			beats(0, 0.9, flat(30)),
			beats(0.95, 7, linear(30-0.9, 1)),
			beats(7.05, 7.95, flat(0)),
			beats(8, 15, linear(-7.95, 1)),
		), PCMKeep, -1, true},
		{"每首只判一次", cat(beats(0, 11, linear(30, 1)), beats(11.05, 25, linear(41-2*11.05, 2))), PCMKeep, -1, true},
		// F1：第一段推进 2 s、停 0.5 s、再推进，测得 5/5.5≈0.909；第二段正常。
		{"F1：停顿在第一段", beats(0, 12, stalls([2]float64{2, 2.5})), PCMKeep, -1, true},
		{"停顿在第二段", beats(0, 12, stalls([2]float64{7, 7.5})), PCMKeep, -1, true},
		// F1 复审：1000 ms 一拍，每拍都在变，停顿看不见；第一段测得 5.5/6≈0.917。
		{"1000 ms 一拍", beatsEvery(1, 0, 12, func(at float64) float64 {
			if at < 3 {
				return 30 + at
			}
			return 32.5 + at - 3
		}), PCMKeep, -1, true},
		// F4：计数台阶比轮询间隔粗，两段照样测得满。
		{"F4：30 ms 一拍、计数每 300 ms 跳一次（速率 0.5）", stepped300(), PCMRateOff, 20.4, false},
		// F5：1.088 的第一段被 0.4 s 的停顿拉到约 1.001，第二段没有停顿。
		{"F5：停顿把 1.088 拉进范围", cat(
			beats(0, 2, linear(30, rate48k)),
			beats(2.05, 2.4, flat(30+2*rate48k)),
			beats(2.45, 12, linear(30-0.4*rate48k, rate48k)),
		), PCMRateOff, 9.6, false},
		// 第一段 1.088，第二段测到一半判为暂停，恢复后按 1.00 推进：已测完的第一段仍算数。
		{"第一段与第二段之间暂停", cat(
			beats(0, 5, linear(30, rate48k)),
			beats(5.05, 6, flat(30+5*rate48k)),
			pausedTicks(beats(6.05, 7, flat(30+5*rate48k))),
			beats(7.05, 14, linear(30+5*rate48k-7, 1)),
		), PCMRateOff, 12.05, false},
		{"两段各有一次 0.5 s 的停顿（已知代价）", beats(0, 13, stalls([2]float64{2, 2.5}, [2]float64{7.5, 8})), PCMRateOff, 11, false},
	}
	for _, c := range cases {
		v, at, clock := runPace(c.ticks)
		if v != c.want || math.Abs(at-c.at) > 0.026 {
			t.Errorf("%s：第 %.2f s 判为 %v，want 第 %.2f s 判为 %v（速率 %.3f）", c.name, at, v, c.at, c.want, clock.Rate())
			continue
		}
		if v == PCMKeep && clock.rateDone != c.rateOK {
			t.Errorf("%s：核对做过 = %v，want %v", c.name, clock.rateDone, c.rateOK)
		}
	}
}

// 停滞检测（设计 §4.2）：进入轮询后 5 s 内一次都没推进（与暂停与否无关），判为停滞。唯一的实例也可能是
// 停住的残留对象，速率核对要等推进满 5 s，拦不住它。
//
// 变异自证（实测）：超时判断改成 >=，「恰好 5 s」红；去掉停滞检测，「5 s 内从未推进」红；「推进」改成
// 「与上一拍不同」，「只回退、不推进」红；推进的判据改成与进入轮询时的读数比，「重唱回 0 之后推进」红；
// 推进过之后仍按「最近 5 s 没推进」判，「第 4.9 s 推进」在之后的暂停里判为停滞而红。
func TestPCMClockStall(t *testing.T) {
	cases := []struct {
		name  string
		ticks []paceTick
		want  PCMVerdict
		at    float64
	}{
		{"5 s 内从未推进", beats(0, 8, flat(30)), PCMStalled, 5.05},
		{"恰好 5 s", beats(0, 5, flat(30)), PCMKeep, -1},
		{"一直判为暂停", cat(beats(0, 1, flat(30)), pausedTicks(beats(1.05, 8, flat(30)))), PCMStalled, 5.05},
		{"从 0 开始就不走", beats(0, 8, flat(0)), PCMStalled, 5.05},
		{"第 4.9 s 推进", cat(beats(0, 4.85, flat(30)), beats(4.9, 4.9, flat(30.05)), pausedTicks(beats(4.95, 30, flat(30.05)))), PCMKeep, -1},
		{"只回退、不推进", cat(beats(0, 1, flat(30)), beats(1.05, 8, flat(0))), PCMStalled, 5.05},
		{"重唱回 0 之后推进", cat(beats(0, 1, flat(30)), beats(1.05, 2, flat(0)), beats(2.05, 8, linear(-2, 1))), PCMKeep, -1},
		{"正常推进", beats(0, 8, linear(30, 1)), PCMKeep, -1},
	}
	for _, c := range cases {
		v, at, _ := runPace(c.ticks)
		if v != c.want || math.Abs(at-c.at) > 0.026 {
			t.Errorf("%s：第 %.2f s 判为 %v，want 第 %.2f s 判为 %v", c.name, at, v, c.at, c.want)
		}
	}
}
