package helper

// 本文件只测归一化的时间行为：平滑系数按帧的时长换算、响应与帧率无关、残帧按自己的时长计权，
// 以及默认帧率下与「每帧固定系数 0.03」的旧实现等价。

import (
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
)

const (
	normTestTarget  = -14
	normTestMaxGain = 30
)

// applyRMS 把线性 rms、时长 n 个样本的一帧交给 nz，返回输出的 rms。
func applyRMS(nz *Normalizer, rms float64, n int) float64 {
	r := dsp.Result{RMS: float32(rms)}
	nz.Apply(&r, n)
	return float64(r.RMS)
}

// 系数由时间常数与帧的时长换算：48000 Hz、每帧 1920 个样本（25 帧每秒）、τ = 1.3 s 时为 0.030301；
// 时长为 0 时系数为 0；时长越长系数越大，恒小于 1。
//
// 变异自证（隔离副本，实测）：normTau 改为 1.0 → 0.039211，红；smoothing 写成 n/tauN（一阶近似）→
// 0.030769，红。
func TestSmoothingFromTimeConstant(t *testing.T) {
	tauN := normTau * testSR
	if got := smoothing(testHop, tauN); math.Abs(got-0.030301) > 1e-6 {
		t.Fatalf("smoothing(1920) = %.6f, 期望 0.030301", got)
	}
	if got := smoothing(0, tauN); got != 0 {
		t.Fatalf("smoothing(0) = %v, 期望 0", got)
	}
	prev := 0.0
	for _, n := range []int{1, 480, 800, 1920, 3200, 48000, 480000} {
		got := smoothing(n, tauN)
		if !(got > prev && got < 1) {
			t.Fatalf("smoothing(%d) = %v, 期望大于 %v 且小于 1", n, got, prev)
		}
		prev = got
	}
}

// 响应与帧率无关：各帧率下先以 −20 dBFS 稳定 30 秒，再阶跃到 −10 dBFS，其后第 1、2、3 秒整那一帧的
// 输出高出目标的 dB 数应等于按 τ 算出的解析值 −10·log10(1 − 0.9·e^(−t/τ))（估计自 0.1 倍功率向
// 新电平指数逼近），各帧率之间自然一致。
//
// 变异自证（隔离副本，实测）：smoothing 恒返回 0.03（按帧定系数）→ 25 帧每秒以外各帧率偏离解析值
// （15 帧每秒第 1 秒为 +3.67 dB，解析值 +2.34 dB），红。
func TestNormalizerResponseIndependentOfFrameRate(t *testing.T) {
	target := lin(normTestTarget)
	for _, fps := range []int{15, 25, 30, 50, 60} {
		hop := hopFor(testSR, fps)
		nz := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
		for i := 0; i < 30*fps; i++ {
			applyRMS(nz, lin(-20), hop)
		}
		for i := 1; i <= 3*fps; i++ {
			out := applyRMS(nz, lin(-10), hop)
			if i%fps != 0 {
				continue
			}
			sec := float64(i / fps)
			want := -10 * math.Log10(1-0.9*math.Exp(-sec/normTau))
			if got := toDB(out / target); math.Abs(got-want) > 0.01 {
				t.Errorf("%d 帧每秒，阶跃后第 %.0f 秒: 高出目标 %+.3f dB, 期望 %+.3f dB", fps, sec, got, want)
			}
		}
	}
}

// 残帧按自己的时长计权：同一段输入，按 1920 个样本的整帧喂与按 480 个样本的残帧喂（帧数为 4 倍），
// 相同累计时长处的估计一致。
//
// 变异自证（隔离副本，实测）：Apply 不看 n、恒按整帧 1920 取系数 → 残帧一路逼近得快 4 倍，红。
func TestNormalizerPartialFramesWeighByDuration(t *testing.T) {
	full := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
	part := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
	feed := func(rms float64, blocks int) {
		for i := 0; i < blocks; i++ {
			applyRMS(full, rms, 1920)
			for j := 0; j < 4; j++ {
				applyRMS(part, rms, 480)
			}
			if d := math.Abs(full.emaMS-part.emaMS) / full.emaMS; d > 1e-9 {
				t.Fatalf("rms=%v 第 %d 个整帧处: 整帧估计 %v, 残帧估计 %v（相对差 %.2e）", rms, i, full.emaMS, part.emaMS, d)
			}
		}
	}
	feed(lin(-20), 250) // 10 秒，稳定
	feed(lin(-10), 50)  // 上阶跃 2 秒
	feed(lin(-30), 50)  // 下阶跃 2 秒
}

// 数据断续到来时，各帧率下发出的残帧相同，估计也应相同：15 与 25 帧每秒各喂同一组「一个 480 帧的
// 包，然后一次 100ms 的读包超时」，逐帧比较估计。每个包的电平不同，否则估计恒定、比较无意义。
//
// 变异自证（隔离副本，实测）：framer.flush 的 fresh 恒报 hop → 两路的权重分别按 3200 与 1920 个样本
// 取，估计不同，红。
func TestTrickleEstimateIndependentOfFrameRate(t *testing.T) {
	run := func(fps int) (ema []float64) {
		clk := &fakeClock{t: time.Unix(1000, 0)}
		nz := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
		i := 0
		read := func(dst []float32) (int, bool, error) {
			defer func() { i++ }()
			switch {
			case i == 80:
				return 0, false, errStopPump
			case i%2 == 1:
				clk.t = clk.t.Add(100 * time.Millisecond)
				return 0, false, nil
			}
			clk.t = clk.t.Add(10 * time.Millisecond)
			level := float32(0.03 + 0.02*float64(i/2%7))
			for j := 0; j < 960; j++ {
				dst[j] = level
			}
			return 480, false, nil
		}
		emit := func(batch []float32, _ [2]float32, fresh int) {
			applyRMS(nz, float64(batch[len(batch)-1]), fresh) // 包内电平恒定，rms 即样本值
			ema = append(ema, nz.emaMS)
		}
		fr := newFramer(hopFor(testSR, fps), 1024)
		if err := pumpFrames(read, clk.now, make(chan struct{}), fr, make([]float32, testSR*2/10), 2, time.Second/time.Duration(fps), emit); err != errStopPump {
			t.Fatalf("pumpFrames: %v", err)
		}
		return ema
	}
	a, b := run(15), run(25)
	if len(a) != 40 || len(b) != 40 {
		t.Fatalf("帧数 = %d、%d, 期望各 40", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 帧: 15 帧每秒估计 %v, 25 帧每秒估计 %v", i, a[i], b[i])
		}
	}
}

// 没有时长的帧（n 不为正）不更新估计：尚无估计时返回增益 1、不碰 r；已有估计时估计不变，增益照常施加。
//
// 变异自证（隔离副本，实测）：去掉 Apply 里对 n 的判定 → n 为 0 的首帧也被拿去播种，返回的增益不是 1，红。
func TestNormalizerIgnoresFramesWithoutDuration(t *testing.T) {
	for _, n := range []int{0, -1920} {
		nz := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
		r := dsp.Result{RMS: 0.1, Bins: []float32{0.5}}
		if g := nz.Apply(&r, n); g != 1 || r.RMS != 0.1 || r.Bins[0] != 0.5 || nz.emaMS != 0 {
			t.Fatalf("n=%d、尚无估计: 增益=%v r=%+v 估计=%v, 期望增益 1、r 与估计不变", n, g, r, nz.emaMS)
		}

		applyRMS(nz, 0.1, testHop)
		before := nz.emaMS
		r = dsp.Result{RMS: 0.3}
		g := nz.Apply(&r, n)
		if nz.emaMS != before {
			t.Fatalf("n=%d、已有估计: 估计由 %v 变为 %v", n, before, nz.emaMS)
		}
		if want := float32(gainFactor(math.Sqrt(before), normTestTarget, normTestMaxGain)); g != want || r.RMS != 0.3*want {
			t.Fatalf("n=%d、已有估计: 增益=%v rms=%v, 期望按当前估计施加 %v", n, g, r.RMS, want)
		}
	}
}

// legacyEMA 是改为按时长换算之前的归一化（首帧播种，其后每帧固定系数 0.03），只作对照。
type legacyEMA struct {
	ema    float64
	seeded bool
}

func (l *legacyEMA) apply(rms float64) float64 {
	if ms := rms * rms; l.seeded {
		l.ema = 0.03*ms + 0.97*l.ema
	} else {
		l.ema, l.seeded = ms, true
	}
	return rms * gainFactor(math.Sqrt(l.ema), normTestTarget, normTestMaxGain)
}

// 默认帧率下的等价性：25 帧每秒、连续整帧时，稳态输出与旧实现逐帧相差不超过 0.05 dB。两者的时间
// 常数相差 1%（1.3 s 对 1.313 s），差别在大幅阶跃的尾段最明显：实测 −10 dB 下阶跃 0.048 dB、+10 dB
// 上阶跃 0.027 dB、节拍包络 0.006 dB。输入为有节拍起伏的合成包络，其后接 +10 dB 与 −10 dB 的阶跃；
// 头 12 秒（起步及其余波）不比。
//
// 变异自证（隔离副本，实测）：normTau 改为 1.0 → 最大相差 1.29 dB，红。
func TestNormalizerMatchesPerFrameCoefficientAtDefaultRate(t *testing.T) {
	in := beatEnvelope(25, 40, -18, 10)
	for _, seg := range []struct {
		dbfs float64
		sec  int
	}{{-20, 10}, {-10, 10}, {-20, 10}} {
		for i := 0; i < seg.sec*25; i++ {
			in = append(in, lin(seg.dbfs))
		}
	}
	nz := newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
	var old legacyEMA
	worst := 0.0
	for i, rms := range in {
		got, want := applyRMS(nz, rms, testHop), old.apply(rms)
		if i < 12*25 {
			continue
		}
		worst = math.Max(worst, math.Abs(toDB(got/want)))
	}
	if worst > 0.05 {
		t.Fatalf("与旧实现逐帧输出最大相差 %.4f dB, 上限 0.05 dB", worst)
	}
}
