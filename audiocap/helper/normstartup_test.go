package helper

// 本文件只测归一化在捕获起步阶段的行为：静音门限、起步期（重新起步、按样本数加权的累计平均、下限）
// 及其期限，以及起步期结束后回到对称的指数平均。时间行为（系数换算、帧率无关）见 normtiming_test。

import (
	"math"
	"testing"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
)

// fullScale 是检查页 RMS 走带的满幅阈值：走带画的是 min(1, rms×2.5)。目标 -14 dBFS 时高出 6.04 dB 即满幅。
const fullScale = 0.4

// newTestNormalizer 返回测试默认参数（target -14、max-gain 30、48000 Hz）的归一化器。
func newTestNormalizer() *Normalizer {
	return newNormalizer(true, normTestTarget, normTestMaxGain, testSR)
}

// tauSamples 是时间常数折合的样本数（48000 Hz 下 62400）。
func tauSamples() float64 { return normTau * testSR }

func repeatRMS(rms float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = rms
	}
	return s
}

// rampDB 造 n 帧按 dB 线性上升的前导：末帧比 -20 dBFS 低 stepDB，往前逐帧再低 stepDB。
func rampDB(stepDB float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = lin(-20 - stepDB*float64(n-i))
	}
	return s
}

// 验收序列：各种前导之后恒为 -20 dBFS。含前导在内，任何一帧的输出都不满幅、高出目标不超过 3.02 dB
// （下限为本帧功率的一半，即 3.01 dB）；正常帧自第 settle 帧起稳定在目标 ±1 dB 以内。settle 取实测值，
// 钉住收敛不变慢。前导比正常低 10 dB 时恰在重新起步的门槛上，走哪条路取决于浮点舍入，故以 9、11 dB
// 两行分别钉住下限与重新起步，10 dB 一行只给两条路中较松的上界。
//
// 变异自证（隔离副本，实测）：去掉起步期的下限 →「20 帧低 9 dB」、各渐入行等过冲超限（最高 +7.8 dB），
// 红；下限只用于算增益、不写回估计 →「8 帧低 9 dB」等行收敛变慢、期满后超过 3.02 dB，红；去掉重新
// 起步 →「8 帧 -60 dBFS」「30 帧 -40 dBFS」等行收敛变慢，红；起步期系数用常数（不取 d/t）→
// 「1 帧 -26 dBFS」等行收敛变慢（第 30 帧才稳定），红。
func TestStartupLeadInsNeverOvershoot(t *testing.T) {
	linRamp := make([]float64, 10) // 10 帧按幅度线性渐入
	for i := range linRamp {
		linRamp[i] = lin(-20) * float64(i+1) / 11
	}
	for _, c := range []struct {
		name   string
		lead   []float64
		settle int
	}{
		{"无前导", nil, 1},
		{"1 帧为 0", []float64{0}, 1},
		{"25 帧为 0", repeatRMS(0, 25), 1},
		{"1 帧 -80 dBFS", []float64{lin(-80)}, 1},
		{"1 帧 -50 dBFS", []float64{lin(-50)}, 1},
		{"1 帧 -34 dBFS", []float64{lin(-34)}, 1},
		{"1 帧 -26 dBFS", []float64{lin(-26)}, 3},
		{"3 帧渐入 -50/-40/-30 dBFS", []float64{lin(-50), lin(-40), lin(-30)}, 1},
		{"8 帧 -60 dBFS", repeatRMS(lin(-60), 8), 1},
		{"30 帧 -40 dBFS", repeatRMS(lin(-40), 30), 1},
		{"8 帧低 14 dB", repeatRMS(lin(-34), 8), 1},
		{"20 帧低 14 dB", repeatRMS(lin(-34), 20), 1},
		{"8 帧低 11 dB", repeatRMS(lin(-31), 8), 1},
		{"20 帧低 11 dB", repeatRMS(lin(-31), 20), 1},
		{"8 帧低 10 dB", repeatRMS(lin(-30), 8), 14},
		{"20 帧低 10 dB", repeatRMS(lin(-30), 20), 28},
		{"8 帧低 9 dB", repeatRMS(lin(-29), 8), 14},
		{"20 帧低 9 dB", repeatRMS(lin(-29), 20), 28},
		{"8 帧低 6 dB", repeatRMS(lin(-26), 8), 14},
		{"20 帧低 6 dB", repeatRMS(lin(-26), 20), 28},
		{"10 帧按幅度线性渐入", linRamp, 17},
		{"每帧 +1 dB 渐入 10 帧", rampDB(1, 10), 17},
		{"每帧 +1 dB 渐入 20 帧", rampDB(1, 20), 28},
		{"每帧 +1 dB 渐入 30 帧", rampDB(1, 30), 30},
		{"每帧 +2 dB 渐入 10 帧", rampDB(2, 10), 17},
		{"每帧 +2 dB 渐入 20 帧", rampDB(2, 20), 28},
		{"每帧 +2 dB 渐入 30 帧", rampDB(2, 30), 29},
		{"每帧 +3 dB 渐入 10 帧", rampDB(3, 10), 17},
		{"每帧 +3 dB 渐入 20 帧", rampDB(3, 20), 25},
		{"每帧 +6 dB 渐入 5 帧", rampDB(6, 5), 10},
		{"每帧 +6 dB 渐入 10 帧", rampDB(6, 10), 14},
	} {
		t.Run(c.name, func(t *testing.T) {
			nz := newTestNormalizer()
			target := lin(normTestTarget)
			check := func(what string, i int, rms float64) float64 {
				out := applyRMS(nz, rms, testHop)
				if out >= fullScale || toDB(out/target) > 3.02 {
					t.Fatalf("%s第 %d 帧（输入 %.1f dBFS）: 输出 %.4f，高出目标 %+.2f dB", what, i+1, toDB(rms), out, toDB(out/target))
				}
				return toDB(out / target)
			}
			for i, rms := range c.lead {
				check("前导", i, rms)
			}
			settle := 0 // 最后一个偏离目标超过 1 dB 的正常帧的序号（自 1 起）
			for i := 0; i < 400; i++ {
				if d := check("正常帧", i, lin(-20)); math.Abs(d) > 1 {
					settle = i + 1
				}
			}
			if settle+1 > c.settle {
				t.Fatalf("自第 %d 个正常帧起才稳定在 ±1 dB 以内，期望不晚于第 %d 帧", settle+1, c.settle)
			}
		})
	}
}

// steadyReference 返回 env 逐帧经「跑到稳态的对称指数平均」后的输出，作为稳态下应有的显示。
func steadyReference(env []float64) []float64 {
	var ref legacyEMA
	out := make([]float64, len(env))
	for i, rms := range env {
		out[i] = ref.apply(rms)
	}
	return out
}

// musicLeads 是接在节拍包络之前的前导，电平相对包络的功率平均 mean（线性 rms）给出。
func musicLeads(mean float64) map[string][]float64 {
	ramp := func(stepDB float64, n int) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = mean * lin(-stepDB*float64(n-i))
		}
		return s
	}
	return map[string][]float64{
		"无前导":          nil,
		"1 帧为 0":       {0},
		"8 帧低 40 dB":   repeatRMS(mean*lin(-40), 8),
		"30 帧低 20 dB":  repeatRMS(mean*lin(-20), 30),
		"20 帧低 14 dB":  repeatRMS(mean*lin(-14), 20),
		"20 帧低 10 dB":  repeatRMS(mean*lin(-10), 20),
		"20 帧低 6 dB":   repeatRMS(mean*lin(-6), 20),
		"20 帧每帧 +3 dB": ramp(3, 20),
		"20 帧每帧 +1 dB": ramp(1, 20),
		"10 帧每帧 +6 dB": ramp(6, 10),
	}
}

// 验收序列：前导之后接有节拍起伏的合成包络（拍间落差 6、10、14 dB），从包络的多个相位起步，与同一包络
// 跑到稳态的参照逐帧比较。稳态下拍点本就高于目标（落差 10、14 dB 时也会满幅），所以只数「参照不满幅
// 而被测满幅」的帧，应为 0。
//
// 变异自证（隔离副本，实测）：去掉重新起步 → 红；重新起步时不重置起步期的累计样本数 →「30 帧低 20 dB」
// 新增满幅帧（落差 10 dB 下 3 个），红；去掉下限 → 红；期满那一帧以本帧重新播种 →「无前导」新增
// 满幅帧，红。
func TestStartupLeadInThenMusicAddsNoFullScaleFrames(t *testing.T) {
	for _, contrast := range []float64{6, 10, 14} {
		env := beatEnvelope(25, 80, -18, contrast)
		ref := steadyReference(env)
		var ms float64
		for _, v := range env {
			ms += v * v
		}
		mean := math.Sqrt(ms / float64(len(env)))
		for name, lead := range musicLeads(mean) {
			for _, start := range []int{750, 751, 752, 753, 755, 757, 758, 760, 761, 763, 766, 770} {
				nz := newTestNormalizer()
				for _, rms := range lead {
					applyRMS(nz, rms, testHop)
				}
				added := 0
				for i := start; i < start+500; i++ {
					if out := applyRMS(nz, env[i], testHop); out >= fullScale && ref[i] < fullScale {
						added++
					}
				}
				if added != 0 {
					t.Errorf("落差 %.0f dB、前导「%s」、起步相位 %d: 新增满幅帧 %d 个", contrast, name, start, added)
				}
			}
		}
	}
}

// 起步期保护的代价有上界：无前导地从节拍包络中间起步时，拍点被下限压到目标 +3 dB，估计略偏高、波形
// 略小。头 1.3 秒相对稳态参照的平均偏差不低于 -1.0、-2.0、-2.7 dB（落差 6、10、14 dB；多个起步相位
// 取最坏），5 秒后逐帧都在 ±1 dB 以内。
//
// 变异自证（隔离副本，实测）：下限取本帧功率本身（估计跟随最响的帧）→ 落差 6 dB 下平均偏差 -2.04 dB，
// 超出下界，红；下限在起步期之后仍生效 → 5 秒处仍偏离稳态 1.8 dB，红。
func TestStartupCostOnCleanStartIsBounded(t *testing.T) {
	for _, c := range []struct{ contrast, floorDB float64 }{{6, -1.0}, {10, -2.0}, {14, -2.7}} {
		env := beatEnvelope(25, 80, -18, c.contrast)
		ref := steadyReference(env)
		for _, start := range []int{750, 751, 752, 753, 755, 757, 758, 760, 761, 763, 766, 770} {
			nz := newTestNormalizer()
			var sum float64
			for i := start; i < start+500; i++ {
				d := toDB(applyRMS(nz, env[i], testHop) / ref[i])
				if i-start < 33 {
					sum += d
				}
				if i-start >= 125 && math.Abs(d) > 1 {
					t.Errorf("落差 %.0f dB、起步相位 %d: 起步 %.2f 秒处仍偏离稳态 %+.2f dB", c.contrast, start, float64(i-start)/25, d)
					break
				}
			}
			if avg := sum / 33; avg < c.floorDB {
				t.Errorf("落差 %.0f dB、起步相位 %d: 头 1.3 秒平均偏差 %+.2f dB，低于下界 %+.1f dB", c.contrast, start, avg, c.floorDB)
			}
		}
	}
}

// 低于静音门限（-70 dBFS）的帧不参与估计：尚无估计时它们原样输出（增益 1），其后第一个正常帧的输出
// 即目标，前导多长都一样（100 帧已超过起步期保护的时长）。
//
// 变异自证（隔离副本，实测）：去掉门限 → 红；门限帧不并入估计但计入累计样本数 → 前导的首帧即被施加
// max-gain，红；尚无估计时也施加增益 → 同样红。
func TestStartupSilentLeadInIsIgnored(t *testing.T) {
	target := lin(normTestTarget)
	for _, lead := range []float64{0, lin(-90), lin(-71)} {
		for _, n := range []int{1, 25, 100} {
			nz := newTestNormalizer()
			for i := 0; i < n; i++ {
				r := dsp.Result{RMS: float32(lead), Bins: []float32{0.5}}
				if g := nz.Apply(&r, testHop); g != 1 || r.RMS != float32(lead) || r.Bins[0] != 0.5 {
					t.Fatalf("前导 %.0f dBFS 第 %d 帧: 增益 %v、r=%+v，期望原样输出", toDB(lead), i+1, g, r)
				}
			}
			if d := toDB(applyRMS(nz, lin(-20), testHop) / target); math.Abs(d) > 0.01 {
				t.Fatalf("%d 帧 %.0f dBFS 之后首个正常帧偏离目标 %+.2f dB", n, toDB(lead), d)
			}
		}
	}
}

// 首个计入的帧即以自身播种：不论播放器的电平离目标多远，首帧的输出就是目标。
//
// 变异自证（隔离副本，实测）：改为以目标电平播种 → -34 dBFS 的首帧低于目标近 20 dB，红。
func TestStartupFirstFrameAtTarget(t *testing.T) {
	target := lin(normTestTarget)
	for _, dbfs := range []float64{-34, -20, -6} {
		if d := toDB(applyRMS(newTestNormalizer(), lin(dbfs), testHop) / target); math.Abs(d) > 0.01 {
			t.Errorf("电平 %.0f dBFS 的首帧偏离目标 %+.2f dB", dbfs, d)
		}
	}
}

// specModel 按设计逐帧算估计，只含累计平均与其后的指数平均（不含下限与重新起步），供电平起伏平缓、
// 不触发这两项保护的序列核对。
type specModel struct{ ema, warm float64 }

func (m *specModel) step(ms float64, n int) {
	d := float64(n)
	switch {
	case m.warm == 0:
		m.ema, m.warm = ms, d
	case m.warm < tauSamples():
		m.warm += d
		m.ema += math.Max(smoothing(n, tauSamples()), d/m.warm) * (ms - m.ema)
	default:
		m.ema += smoothing(n, tauSamples()) * (ms - m.ema)
	}
}

// 起步期内估计是已计入各帧功率按样本数加权的平均，期满后是指数平均，交接处不重算、不跳变。序列为
// 相邻帧相差不超过 2 dB 的起伏、整帧与残帧混合，不触发下限与重新起步，逐帧与 specModel 核对，跨过
// 起步期的结束。
//
// 变异自证（隔离副本，实测）：累计权重在并入之后才加上本帧的样本数（权重成了 d/(t-d)）→ 红；起步期
// 提前一半结束 → 红；期满那一帧以本帧重新播种 → 红。
func TestEstimateFollowsWarmupThenEMA(t *testing.T) {
	nz := newTestNormalizer()
	var model specModel
	sizes := []int{1920, 480, 1920, 960, 1920}
	total := 0.0
	for i := 0; total < 3*tauSamples(); i++ {
		n := sizes[i%len(sizes)]
		rms := lin(-20 + 1.0*math.Sin(float64(i)*0.9))
		r := dsp.Result{RMS: float32(rms)}
		nz.Apply(&r, n)
		model.step(float64(float32(rms))*float64(float32(rms)), n)
		total += float64(n)
		if d := math.Abs(nz.emaMS-model.ema) / model.ema; d > 1e-12 {
			t.Fatalf("第 %d 帧（累计 %.0f 个样本）: 估计 %v，按设计应为 %v", i+1, total, nz.emaMS, model.ema)
		}
	}
}

// 起步期结束后是对称的指数平均：下限与重新起步都不再生效。先喂满起步期，再依次阶跃 +20 dB、-10 dB、
// +10 dB，每帧的估计等于参考递推 ema += α·(ms − ema)。
//
// 变异自证（隔离副本，实测）：下限在起步期之后仍生效 → 红；重新起步既不限头 τ 秒也不限起步期 → +20 dB
// 阶跃的首帧被重新播种，红；起步期之后上升时改用更大的系数 → 红。
func TestAfterWarmupIsSymmetricEMA(t *testing.T) {
	nz := newTestNormalizer()
	for i := 0; i < 50; i++ { // 2 秒，起步期已过
		applyRMS(nz, lin(-30), testHop)
	}
	alpha := smoothing(testHop, tauSamples())
	ref := nz.emaMS
	for _, dbfs := range []float64{-10, -20, -10} {
		for i := 0; i < 20; i++ {
			rms := float64(float32(lin(dbfs)))
			applyRMS(nz, lin(dbfs), testHop)
			ref += alpha * (rms*rms - ref)
			if d := math.Abs(nz.emaMS-ref) / ref; d > 1e-12 {
				t.Fatalf("阶跃到 %.0f dBFS 后第 %d 帧: 估计 %v，指数平均应为 %v", dbfs, i+1, nz.emaMS, ref)
			}
		}
	}
}

// 重新起步的条件：头 τ 秒内，本帧功率超过估计的 10 倍即以本帧重新播种（输出即目标）；不到 10 倍时
// 走累计平均与下限。τ 秒之后即便仍在重新起步带来的起步期内，同样的跳变也只受下限保护。
//
// 变异自证（隔离副本，实测）：门槛改为 4 倍 →「9.5 倍」一例被重新播种，红；门槛改为 100 倍 → 红；
// 重新起步不限头 τ 秒（仍限起步期内）→ 末段那一帧被重新播种（估计为该帧功率而非其一半），红；重新起步
// 时不重置起步期的累计样本数 → 红。
func TestRestartThreshold(t *testing.T) {
	base := lin(-40)
	ms0 := float64(float32(base)) * float64(float32(base))

	nz := newTestNormalizer()
	applyRMS(nz, base, testHop)
	applyRMS(nz, base*math.Sqrt(10.5), testHop)
	if got, want := nz.emaMS, sq32(base*math.Sqrt(10.5)); got != want {
		t.Fatalf("功率为估计的 10.5 倍: 估计 %v，期望以本帧重新播种 %v", got, want)
	}

	nz = newTestNormalizer()
	applyRMS(nz, base, testHop)
	applyRMS(nz, base*math.Sqrt(9.5), testHop)
	if got, want := nz.emaMS, (ms0+sq32(base*math.Sqrt(9.5)))/2; math.Abs(got-want)/want > 1e-12 {
		t.Fatalf("功率为估计的 9.5 倍: 估计 %v，期望两帧的平均 %v（不重新起步）", got, want)
	}

	// 头 τ 秒内重新起步一次，把起步期延到 τ 秒之后；此后再来高 20 dB 的帧，只受下限保护。
	nz = newTestNormalizer()
	for i := 0; i < 20; i++ {
		applyRMS(nz, base, testHop)
	}
	high := base * 10
	for i := 0; i < 13; i++ { // 首帧触发重新起步；累计计入 33 帧，已过 τ 秒，而本次起步只有 13 帧
		applyRMS(nz, high, testHop)
	}
	peak := high * 10
	out := applyRMS(nz, peak, testHop)
	if got, want := nz.emaMS, sq32(peak)/2; got != want {
		t.Fatalf("τ 秒之后的跳变: 估计 %v，期望被下限抬到本帧功率的一半 %v（不重新起步）", got, want)
	}
	if d := toDB(out / lin(normTestTarget)); math.Abs(d-3.01) > 0.01 {
		t.Fatalf("τ 秒之后的跳变: 输出高出目标 %+.2f dB，期望 +3.01 dB", d)
	}
}

// sq32 返回 rms 经 float32 舍入后的平方，即归一化器实际看到的功率。
func sq32(rms float64) float64 { return float64(float32(rms)) * float64(float32(rms)) }

// 重新起步至多把起步期保护延长到 2τ：电平每 12 帧上一个台阶（功率为此前的 12 倍，超过重新起步的
// 门槛），一直持续。头 τ 秒内的台阶次次重新起步，之后的不再重新起步；累计 2τ（65 帧）之后起步期必已
// 结束，第 72 帧的台阶按指数平均处理。
//
// 变异自证（隔离副本，实测）：重新起步时把捕获以来的累计样本数也清零 → 每个台阶都重新起步、起步期永不
// 结束，第 72 帧被重新播种，红；重新起步不限头 τ 秒 → 同样红。
func TestRestartCannotExtendWarmupBeyondTwoTau(t *testing.T) {
	nz := newTestNormalizer()
	level := lin(-60)
	for i := 0; i < 71; i++ {
		if i%12 == 11 {
			level *= math.Sqrt(12)
		}
		applyRMS(nz, level, testHop)
	}
	before := nz.emaMS
	level *= math.Sqrt(12)
	applyRMS(nz, level, testHop) // 第 72 帧
	want := before + smoothing(testHop, tauSamples())*(sq32(level)-before)
	if d := math.Abs(nz.emaMS-want) / want; d > 1e-12 {
		t.Fatalf("2τ 之后的台阶: 估计 %v，指数平均应为 %v（起步期应已结束）", nz.emaMS, want)
	}
}

// 下限的取值是本帧功率的一半。一帧 -26 dBFS 之后三帧 -20 dBFS：估计逐帧是两、三、四帧的平均，下限
// 不介入；两帧 -27 dBFS 之后一帧 -20 dBFS：三帧的平均低于该帧功率的一半，估计被抬到一半，输出高出
// 目标 3.01 dB。
//
// 变异自证（隔离副本，实测）：下限取 1/4 → 后一例的估计为三帧的平均，红；下限取本帧功率本身 → 前一例
// 的第二帧估计等于该帧功率，红。
func TestWarmupFloor(t *testing.T) {
	nz := newTestNormalizer()
	low, normal := sq32(lin(-26)), sq32(lin(-20))
	applyRMS(nz, lin(-26), testHop)
	for k := 1; k <= 3; k++ {
		applyRMS(nz, lin(-20), testHop)
		if want := (low + float64(k)*normal) / float64(k+1); math.Abs(nz.emaMS-want)/want > 1e-12 {
			t.Fatalf("-26 dBFS 之后第 %d 个 -20 dBFS 帧: 估计 %v，期望 %d 帧的平均 %v", k, nz.emaMS, k+1, want)
		}
	}

	nz = newTestNormalizer()
	applyRMS(nz, lin(-27), testHop)
	applyRMS(nz, lin(-27), testHop)
	out := applyRMS(nz, lin(-20), testHop)
	if want := normal / 2; nz.emaMS != want {
		t.Fatalf("两帧 -27 dBFS 之后的 -20 dBFS 帧: 估计 %v，期望被抬到该帧功率的一半 %v", nz.emaMS, want)
	}
	if d := toDB(out / lin(normTestTarget)); math.Abs(d-3.01) > 0.01 {
		t.Fatalf("输出高出目标 %+.2f dB，期望 +3.01 dB", d)
	}
}

// 静音门限在 -70 dBFS：-69.5 dBFS 的帧计入（以它播种），-70.5 dBFS 的不计入。
//
// 变异自证（隔离副本，实测）：门限改为 -60 dBFS → 前一例红；改为 -80 dBFS → 后一例红；比较方向写反 →
// 两例都红。
func TestGateThreshold(t *testing.T) {
	nz := newTestNormalizer()
	applyRMS(nz, lin(-69.5), testHop)
	if want := sq32(lin(-69.5)); nz.emaMS != want {
		t.Fatalf("-69.5 dBFS 应计入: 估计 %v，期望 %v", nz.emaMS, want)
	}

	nz = newTestNormalizer()
	r := dsp.Result{RMS: float32(lin(-70.5))}
	if g := nz.Apply(&r, testHop); g != 1 || nz.emaMS != 0 {
		t.Fatalf("-70.5 dBFS 不应计入: 增益 %v、估计 %v", g, nz.emaMS)
	}
}

// 已有估计之后的门限帧：估计与两个累计样本数都不变，增益仍按当前估计施加到该帧的 rms 与 bins 上。
//
// 变异自证（隔离副本，实测）：门限帧一律返回增益 1 → 红；门限帧把估计清零 → 红。
func TestGatedFrameKeepsEstimateAndGetsGain(t *testing.T) {
	nz := newTestNormalizer()
	applyRMS(nz, lin(-20), testHop)
	ema, warm, seen := nz.emaMS, nz.warmN, nz.seenN
	want := float32(gainFactor(math.Sqrt(ema), normTestTarget, normTestMaxGain))

	r := dsp.Result{RMS: float32(lin(-80)), Bins: []float32{0.25}}
	g := nz.Apply(&r, testHop)
	if nz.emaMS != ema || nz.warmN != warm || nz.seenN != seen {
		t.Fatalf("门限帧改动了状态: 估计 %v→%v、起步累计 %v→%v、总累计 %v→%v", ema, nz.emaMS, warm, nz.warmN, seen, nz.seenN)
	}
	if g != want || r.RMS != float32(lin(-80))*want || r.Bins[0] != 0.25*want {
		t.Fatalf("门限帧: 增益 %v、r=%+v，期望按当前估计施加 %v", g, r, want)
	}
}

// 起步期内增益封顶照常生效：首帧 -65 dBFS（高于门限）需要 51 dB 才到目标，实得 max-gain 的 30 dB。
//
// 变异自证（隔离副本，实测）：gainFactor 去掉封顶 → 首帧输出即目标，红。
func TestWarmupRespectsMaxGain(t *testing.T) {
	r := dsp.Result{RMS: float32(lin(-65))}
	g := newTestNormalizer().Apply(&r, testHop)
	if want := float32(math.Pow(10, normTestMaxGain/20.0)); g != want {
		t.Fatalf("增益 %v，期望封顶 %v", g, want)
	}
}

// 起步期按累计样本数结束，期限是 τ × 采样率（48000 Hz 下 62400 个样本）。连续整帧时第 32 帧之后
// （61440 个）仍在期内，高 8 dB 的帧受下限保护；第 33 帧之后（63360 个）已结束，同样的帧按指数平均
// 处理。全部按 480 个样本的残帧喂时，129 帧（61920 个）仍在期内、130 帧（恰 62400 个）结束。采样率
// 换成 44100 时期限随之为 57330 个样本。
//
// 变异自证（隔离副本，实测）：期限与 τ 的秒数直接比（首帧的样本数即超过 1.3）→ 本用例与
// TestStartupCountsSamplesNotSeconds 均红；累计按帧数计（每帧都算 1920 个样本）→「残帧 129 帧，仍在
// 期内」红；期限写死 62400 →「44100 Hz 整帧 30 帧，已结束」红；起步期提前一半结束 → 红。
func TestWarmupEndsByAccumulatedSamples(t *testing.T) {
	for _, c := range []struct {
		name       string
		sampleRate int
		frame      int // 每帧样本数
		frames     int // 先喂这么多帧恒定电平
		inWarmup   bool
	}{
		{"整帧 32 帧，仍在期内", testSR, 1920, 32, true},
		{"整帧 33 帧，已结束", testSR, 1920, 33, false},
		{"残帧 129 帧，仍在期内", testSR, 480, 129, true},
		{"残帧 130 帧，已结束", testSR, 480, 130, false},
		{"44100 Hz 整帧 29 帧，仍在期内", 44100, 1920, 29, true},
		{"44100 Hz 整帧 30 帧，已结束", 44100, 1920, 30, false},
	} {
		nz := newNormalizer(true, normTestTarget, normTestMaxGain, c.sampleRate)
		for i := 0; i < c.frames; i++ {
			applyRMS(nz, lin(-30), c.frame)
		}
		before := nz.emaMS
		ms := sq32(lin(-22)) // 高 8 dB：不到重新起步的门槛
		applyRMS(nz, lin(-22), c.frame)
		want := before + smoothing(c.frame, normTau*float64(c.sampleRate))*(ms-before)
		if c.inWarmup {
			want = ms / 2
		}
		if d := math.Abs(nz.emaMS-want) / want; d > 1e-12 {
			t.Errorf("%s: 高 8 dB 的帧之后估计 %v，期望 %v", c.name, nz.emaMS, want)
		}
	}
}

// 首帧 rms 0.001、次帧 0.1：次帧不得满幅。起步期的期限若拿样本数去和以秒计的 τ 比，首帧即告结束，
// 次帧走普通的指数平均，输出约 1.14。
//
// 变异自证（隔离副本，实测）：同 TestWarmupEndsByAccumulatedSamples 的第一种变异，红。
func TestStartupCountsSamplesNotSeconds(t *testing.T) {
	nz := newTestNormalizer()
	applyRMS(nz, 0.001, testHop)
	if out := applyRMS(nz, 0.1, testHop); out >= fullScale {
		t.Fatalf("次帧输出 %.3f，满幅", out)
	}
}
