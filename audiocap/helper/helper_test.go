package helper

import (
	"math"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
)

// 整帧的测试参数：48000 Hz 下 25 帧每秒，每帧 1920 个样本。
const (
	testSR  = 48000
	testHop = 1920
)

func TestParseControlCapture(t *testing.T) {
	// 恰 7 字段正常：norm=1→true、target 为负（-14 合法）、末字段为帧率。帧率取 30 而非默认的 25，
	// maxGain 取 12 与帧率不同：parseControl 把帧率写死为 25 即红（隔离副本，实测）。
	got, err := parseControl("capture 123 kugou 1 -14 12 30")
	if err != nil {
		t.Fatalf("capture 正常应无错: %v", err)
	}
	if want := (control{op: "capture", pid: 123, player: "kugou", norm: true, target: -14, maxGain: 12, fps: 30}); got != want {
		t.Fatalf("capture 正常: got %+v want %+v", got, want)
	}

	// 多余空白容忍（strings.Fields 归一）；norm=0 → false
	if got, err := parseControl("  capture   42   wesing 0 -20 12   60 "); err != nil ||
		got != (control{op: "capture", pid: 42, player: "wesing", norm: false, target: -20, maxGain: 12, fps: 60}) {
		t.Fatalf("空白容忍: got %+v err %v", got, err)
	}

	// 帧率下界：1 合法（范围钳制在配置层，此处只拒绝算不出每帧样本数的值）
	if got, err := parseControl("capture 1 p 0 0 0 1"); err != nil || got.fps != 1 {
		t.Fatalf("fps=1 应合法: got %+v err %v", got, err)
	}

	// norm 字段语义：仅 "1" → true，其余（"0"/"2"/"x"）一律 false（按 ==1 判定）
	for _, tc := range []struct {
		field string
		want  bool
	}{{"1", true}, {"0", false}, {"2", false}, {"x", false}} {
		got, err := parseControl("capture 1 p " + tc.field + " -14 12 25")
		if err != nil {
			t.Fatalf("norm=%q 应解析成功: %v", tc.field, err)
		}
		if got.norm != tc.want {
			t.Errorf("norm=%q: got %v want %v", tc.field, got.norm, tc.want)
		}
	}
}

func TestParseControlStop(t *testing.T) {
	// stop 2 字段正常：pid 恒 0、归一化字段取零值
	got, err := parseControl("stop qqmusic")
	if err != nil {
		t.Fatalf("stop 正常应无错: %v", err)
	}
	if want := (control{op: "stop", player: "qqmusic"}); got != want {
		t.Fatalf("stop 正常: got %+v want %+v", got, want)
	}
}

func TestParseControlInvalid(t *testing.T) {
	// 多举反例/负路径：均须报 error。去掉 parseControl 里的 fps < 1 判定后，fps 为 0 与为负的两条不再
	// 报错（隔离副本，实测）：0 会让 captureLoop 算帧间隔时除零。字段数仍按 6 判定时，7 字段的正常
	// 命令被拒（TestParseControlCapture 红，实测）。
	for _, bad := range []string{
		"",                                // 空行
		"   ",                             // 全空白
		"capture 123 kugou",               // 字段数 3（最初的格式）
		"capture 123 kugou 1 -14",         // 字段数 5
		"capture 123 kugou 1 -14 30",      // 字段数 6（无帧率的旧格式，现非法）
		"capture 123 kugou 1 -14 30 25 x", // 字段数 8
		"capture abc kugou 1 -14 30 25",   // pid 非数字
		"capture -1 kugou 1 -14 30 25",    // pid 负号（ParseUint 拒）
		"capture 123 kugou 1 xx 30 25",    // target 非数字
		"capture 123 kugou 1 -14 yy 25",   // maxGain 非数字
		"capture 123 kugou 1 -14 30 0",    // fps 为 0
		"capture 123 kugou 1 -14 30 -25",  // fps 为负
		"capture 123 kugou 1 -14 30 25.5", // fps 非整数
		"capture 123 kugou 1 -14 30 fast", // fps 非数字
		"stop",                            // 缺 player（字段数 1）
		"stop a b",                        // 字段过多（字段数 3）
		"resume kugou",                    // 未知命令
	} {
		if _, err := parseControl(bad); err == nil {
			t.Errorf("应报错但没报: %q", bad)
		}
	}
}

func TestDownmix(t *testing.T) {
	// 立体声 [L0,R0,L1,R1] → mono[i]=(L+R)/2
	assertClose(t, "立体声", downmix([]float32{1, -1, 0.5, 0.5}, 2), []float32{0, 0.5})
	// 单声道原样
	assertClose(t, "单声道", downmix([]float32{0.3, 0.7}, 1), []float32{0.3, 0.7})
	// 空输入 → 空
	if got := downmix(nil, 2); len(got) != 0 {
		t.Errorf("空输入应得空: %v", got)
	}
	// 尾部不足一整帧被忽略：3 样本 / 2 声道 = 1 帧
	assertClose(t, "尾部截断", downmix([]float32{2, 4, 99}, 2), []float32{3})
}

func TestAccumulatePeaks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		acc      [2]float32
		in       []float32
		channels int
		want     [2]float32
	}{
		// 按声道分别取绝对值的最大值；先降混再取峰会得 {0.3, 0.3}
		{"按声道", [2]float32{}, []float32{0.1, -0.7, -0.4, 0.2}, 2, [2]float32{0.4, 0.7}},
		{"保留初值", [2]float32{0.5, 0.5}, []float32{0.2, 0.3}, 2, [2]float32{0.5, 0.5}},
		{"NaN 不计入", [2]float32{}, []float32{float32(math.NaN()), 0.3}, 2, [2]float32{0, 0.3}},
		{"声道 1 的 NaN 不计入", [2]float32{}, []float32{0.3, float32(math.NaN())}, 2, [2]float32{0.3, 0}},
		// 步长须为声道数：按 2 步进会把第 3 声道错当成下一帧的声道 0 / 1。
		{"三声道只看声道 0 和 1", [2]float32{}, []float32{0.1, 0.2, 0.9, -0.5, 0.4, 0.8}, 3, [2]float32{0.5, 0.4}},
		{"超过 1 保留", [2]float32{}, []float32{1.5, -2}, 2, [2]float32{1.5, 2}},
		{"单声道并入两侧", [2]float32{}, []float32{0.3, -0.6}, 1, [2]float32{0.6, 0.6}},
	} {
		if got := accumulatePeaks(tc.acc, tc.in, tc.channels); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestWindow(t *testing.T) {
	// 非空的静音包不计入样本、峰值与新增样本数
	var silent window
	silent.add([]float32{0.9, 0.9}, 2, true)
	if len(silent.batch) != 0 || silent.peaks != ([2]float32{}) || silent.fresh != 0 {
		t.Fatalf("静音包不应计入: %+v", silent)
	}

	// 非静音包：batch 为降混样本，peaks 为降混前按声道的峰值，fresh 为降混样本数
	var w window
	w.add([]float32{0.2, -0.4, 0.6, 0}, 2, false)
	assertClose(t, "batch", w.batch, []float32{-0.1, 0.3})
	if want := ([2]float32{0.6, 0.4}); w.peaks != want {
		t.Fatalf("peaks: got %v want %v", w.peaks, want)
	}
	if w.fresh != 2 {
		t.Fatalf("fresh: got %d want 2（降混后的样本数，不是交织样本数）", w.fresh)
	}

	// 多个包：样本按序追加、峰值逐包并入、fresh 累加，中间的静音包不计入。
	// 声道 0 的峰值取自 B、声道 1 的取自 A，只保留最后一包的峰值即红。
	var m window
	m.add([]float32{0.2, -0.4, 0.6, 0}, 2, false)
	m.add([]float32{0.9, 0.9}, 2, true)
	m.add([]float32{-0.9, 0.1, 0.1, 0.3}, 2, false)
	assertClose(t, "多包 batch", m.batch, []float32{-0.1, 0.3, -0.4, 0.2})
	if want := ([2]float32{0.9, 0.4}); m.peaks != want {
		t.Fatalf("多包 peaks: got %v want %v", m.peaks, want)
	}
	if m.fresh != 4 {
		t.Fatalf("多包 fresh: got %d want 4", m.fresh)
	}

	// 非有限采样（NaN、±Inf）按 0 参与降混与取峰，batch 与 peaks 不出现 NaN 或无穷大；两声道各含非有限值与有限值。
	var nf window
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	nf.add([]float32{nan, 0.2, -0.8, inf, -inf, 0.6}, 2, false)
	assertFinite(t, "非有限采样 batch", nf.batch)
	assertFinite(t, "非有限采样 peaks", nf.peaks[:])
	assertClose(t, "非有限采样 batch", nf.batch, []float32{0.1, -0.4, 0.3})
	if want := ([2]float32{0.8, 0.6}); nf.peaks != want {
		t.Fatalf("非有限采样 peaks: got %v want %v", nf.peaks, want)
	}

	// 有限值不动，包括绝对值大于 1 的。
	var big window
	big.add([]float32{1.5, -2}, 2, false)
	assertClose(t, "超过 1 的 batch", big.batch, []float32{-0.25})
	if want := ([2]float32{1.5, 2}); big.peaks != want {
		t.Fatalf("超过 1 的 peaks: got %v want %v", big.peaks, want)
	}

	// 边界值与次正规数、负零也是有限值，逐位不动。add 就地改写入参，故比对入参；另比对 peaks，
	// 使改为拷贝后清洗时仍能拦住误伤边界值。
	edge := []float32{math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32, -math.SmallestNonzeroFloat32, float32(math.Copysign(0, -1)), 0.5}
	bits := make([]uint32, len(edge))
	for i, v := range edge {
		bits[i] = math.Float32bits(v)
	}
	var e window
	e.add(edge, 2, false)
	for i, v := range edge {
		if math.Float32bits(v) != bits[i] {
			t.Fatalf("有限值 edge[%d] 被改写: got %#08x want %#08x", i, math.Float32bits(v), bits[i])
		}
	}
	if e.peaks != ([2]float32{math.MaxFloat32, math.MaxFloat32}) {
		t.Fatalf("边界值 peaks: got %v want 两侧均为 MaxFloat32", e.peaks)
	}
	var sub window
	sub.add([]float32{math.SmallestNonzeroFloat32, -math.SmallestNonzeroFloat32}, 2, false)
	if sub.peaks != ([2]float32{math.SmallestNonzeroFloat32, math.SmallestNonzeroFloat32}) {
		t.Fatalf("次正规数 peaks: got %v want 两侧均为 SmallestNonzeroFloat32", sub.peaks)
	}
}

func TestEnqueueDropOldest(t *testing.T) {
	// cap=1：连发三帧，永不阻塞（三步 select 均带 default），通道最终只保留最新（"c"）。
	ch := make(chan []byte, 1)
	for _, f := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		enqueue(ch, f) // 若会阻塞，测试将挂死 → 失败信号
	}
	if len(ch) != 1 {
		t.Fatalf("cap=1 连发三帧后应只剩 1 帧, got len=%d", len(ch))
	}
	if got := string(<-ch); got != "c" {
		t.Fatalf("应保留最新帧 %q, got %q", "c", got)
	}

	// cap 足够时按序全入、顺序不变（不触发丢弃路径）。
	ch2 := make(chan []byte, 3)
	for _, f := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		enqueue(ch2, f)
	}
	if len(ch2) != 3 {
		t.Fatalf("cap=3 连发三帧应全入, got len=%d", len(ch2))
	}
	for _, want := range []string{"a", "b", "c"} {
		if got := string(<-ch2); got != want {
			t.Fatalf("按序入队: got %q want %q", got, want)
		}
	}
}

// TestWaitTimeout 断言收尾等待有上限：未 Done 的 WaitGroup 在时限后返回 false，Done 后返回 true。
// 调用放在 goroutine 里并另设 2s 判定上限，退化成无限等待时直接变红而不是挂死。
func TestWaitTimeout(t *testing.T) {
	call := func(wg *sync.WaitGroup, d time.Duration) bool {
		t.Helper()
		res := make(chan bool, 1)
		go func() { res <- waitTimeout(wg, d) }()
		select {
		case ok := <-res:
			return ok
		case <-time.After(2 * time.Second):
			t.Fatalf("waitTimeout(%v) 2s 内未返回", d)
			return false
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	if call(&wg, 20*time.Millisecond) {
		t.Fatalf("未 Done 的 WaitGroup 应超时返回 false")
	}
	wg.Done()
	if !call(&wg, time.Second) {
		t.Fatalf("Done 后应返回 true")
	}
}

// ===== 音量归一化（Normalizer + 纯增益数学）=====

// approx 比较 float64，容差够小以验证数学恒等、够宽以吸收 float32 往返舍入。
func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-6 }

func TestDbfsFloor(t *testing.T) {
	// 0 经 ε 下限等价于 dbfs(ε)，且必为有限值
	z, e := dbfs(0), dbfs(dbfsEpsilon)
	if z != e {
		t.Fatalf("dbfs(0)=%v 应等于 dbfs(ε)=%v", z, e)
	}
	if math.IsInf(z, 0) || math.IsNaN(z) {
		t.Fatalf("dbfs(0) 应有限, got %v", z)
	}
	// 数值：20*log10(1e-10) = -200
	if want := -200.0; !approx(z, want) {
		t.Fatalf("dbfs(0)=%v want %v", z, want)
	}
}

func TestDbfsNegativeInput(t *testing.T) {
	// 负输入、比 ε 还小的正输入都被钳到同一下限，结果有限（反例：不得 NaN/Inf）
	floor := dbfs(dbfsEpsilon)
	for _, x := range []float64{-1, -1e9, 1e-20, 0} {
		got := dbfs(x)
		if got != floor || math.IsInf(got, 0) || math.IsNaN(got) {
			t.Fatalf("dbfs(%v)=%v 应被 ε 钳到 %v 且有限", x, got, floor)
		}
	}
	// 正常区间仍成立：dbfs(1)=0、dbfs(0.1)=-20
	if !approx(dbfs(1), 0) || !approx(dbfs(0.1), -20) {
		t.Fatalf("dbfs(1)=%v dbfs(0.1)=%v", dbfs(1), dbfs(0.1))
	}
}

func TestGainFactorCap(t *testing.T) {
	// 极安静输入（含 0）增益被 maxGain 封顶
	const target, maxGain = -14, 12
	want := math.Pow(10, float64(maxGain)/20)
	for _, rms := range []float64{0, 1e-9, 1e-6} {
		if got := gainFactor(rms, target, maxGain); !approx(got, want) {
			t.Fatalf("gainFactor(%v) 封顶=%v want %v", rms, got, want)
		}
	}
}

func TestGainFactorAttenuates(t *testing.T) {
	// 输入 dBFS 高于 target → 增益 <1（衰减）
	const target, maxGain = -14, 12
	g := gainFactor(1.0, target, maxGain) // dbfs(1)=0 > -14
	if !(g < 1) {
		t.Fatalf("应衰减但 g=%v", g)
	}
	if want := math.Pow(10, -14.0/20); !approx(g, want) {
		t.Fatalf("衰减 g=%v want %v", g, want)
	}
}

func TestGainFactorUnity(t *testing.T) {
	// 输入 dBFS 恰等于 target 且未触顶 → g==1（rms=0.1→dbfs=-20，target=-20，diff=0）
	if g := gainFactor(0.1, -20, 12); !approx(g, 1) {
		t.Fatalf("恰等 target 应 g=1, got %v", g)
	}
	// 边界 target==maxGain：安静输入下 min 取 maxGain
	if g, want := gainFactor(1e-9, 6, 6), math.Pow(10, 6.0/20); !approx(g, want) {
		t.Fatalf("target==maxGain 封顶=%v want %v", g, want)
	}
}

func TestNormalizerSeedAndApply(t *testing.T) {
	const target, maxGain = -14, 12
	nz := newNormalizer(true, target, maxGain, testSR)
	if nz.seenN != 0 || nz.emaMS != 0 {
		t.Fatalf("构造后应尚无估计: seenN=%v emaMS=%v", nz.seenN, nz.emaMS)
	}
	// 首帧 RMS=0.1 → emaMS = 0.1² = 0.01（播种），RMS/Bins 按 g 缩放
	g := gainFactor(0.1, target, maxGain)
	r := &dsp.Result{RMS: 0.1, Bins: []float32{0.2, 0.4}}
	nz.Apply(r, testHop)
	if nz.seenN != testHop || !approx(nz.emaMS, 0.01) {
		t.Fatalf("首帧播种后 seenN=%v emaMS=%v want %d/0.01", nz.seenN, nz.emaMS, testHop)
	}
	if !approx(float64(r.RMS), 0.1*g) {
		t.Fatalf("RMS 缩放=%v want %v", r.RMS, 0.1*g)
	}
	assertClose(t, "bins 缩放", r.Bins, []float32{float32(0.2 * g), float32(0.4 * g)})

	// 次帧 RMS=0.2 → 起步期内取两帧功率的平均（不重新播种；功率为 4 倍，不到重新起步的门槛）
	nz.Apply(&dsp.Result{RMS: 0.2}, testHop)
	if want := (0.01 + 0.04) / 2; !approx(nz.emaMS, want) {
		t.Fatalf("次帧 emaMS=%v want %v（应为两帧的平均）", nz.emaMS, want)
	}

	// 恒定输入收敛稳定：持续 RMS=0.1 多帧后 emaMS 稳在 0.01
	nz2 := newNormalizer(true, target, maxGain, testSR)
	for i := 0; i < 500; i++ {
		nz2.Apply(&dsp.Result{RMS: 0.1}, testHop)
	}
	if !approx(nz2.emaMS, 0.01) {
		t.Fatalf("恒定输入收敛 emaMS=%v want≈0.01", nz2.emaMS)
	}
}

func TestNormalizerDisabledPassthrough(t *testing.T) {
	nz := newNormalizer(false, -14, 12, testSR)
	r := &dsp.Result{RMS: 0.3, Bins: []float32{0.1, 0.5, 0.9}}
	nz.Apply(r, testHop)
	if r.RMS != 0.3 {
		t.Fatalf("disabled 不应改 RMS: %v", r.RMS)
	}
	assertClose(t, "disabled bins 逐位不变", r.Bins, []float32{0.1, 0.5, 0.9})
	// disabled 下连续 Apply，EMA 字段完全不被触碰
	nz.Apply(&dsp.Result{RMS: 0.9}, testHop)
	if nz.seenN != 0 || nz.warmN != 0 || nz.emaMS != 0 {
		t.Fatalf("disabled 不应动估计: seenN=%v warmN=%v emaMS=%v", nz.seenN, nz.warmN, nz.emaMS)
	}
	// 历史未污染：此刻切 enable，仍从当前帧重新播种
	nz.enabled = true
	nz.Apply(&dsp.Result{RMS: 0.1}, testHop)
	if nz.seenN == 0 || !approx(nz.emaMS, 0.01) {
		t.Fatalf("enable 后应从当前帧播种: seenN=%v emaMS=%v", nz.seenN, nz.emaMS)
	}
}

func TestNormalizerApplyReturnsGain(t *testing.T) {
	// 未启用：返回 1，r 逐位不变
	r := &dsp.Result{RMS: 0.3, Bins: []float32{0.1, 0.5}}
	if g := newNormalizer(false, -14, 12, testSR).Apply(r, testHop); g != 1 {
		t.Fatalf("disabled 应返回 1, got %v", g)
	}
	if math.Float32bits(r.RMS) != math.Float32bits(0.3) ||
		math.Float32bits(r.Bins[0]) != math.Float32bits(0.1) || math.Float32bits(r.Bins[1]) != math.Float32bits(0.5) {
		t.Fatalf("disabled 不应改 r: %+v", *r)
	}

	// 启用：首帧返回值即施到 RMS 上的增益
	r = &dsp.Result{RMS: 0.01}
	g := newNormalizer(true, -14, 30, testSR).Apply(r, testHop)
	if want := float32(gainFactor(0.01, -14, 30)); g != want {
		t.Fatalf("首帧增益: got %v want %v", g, want)
	}
	if r.RMS != 0.01*g {
		t.Fatalf("RMS 应乘返回的增益: got %v want %v", r.RMS, 0.01*g)
	}
}

// TestNormalizerIgnoresNonFiniteRMS：NaN / +Inf 的 RMS 不进 EMA。否则并入 NaN 后每帧的增益都是 NaN，
// 并入 +Inf 后每帧的增益都是 0（波形永久静音）。
func TestNormalizerIgnoresNonFiniteRMS(t *testing.T) {
	const target, maxGain = -14, 12
	nan, inf := float32(math.NaN()), float32(math.Inf(1))

	// 已播种：非有限帧按当前估计施增益，且之后的有限帧与「跳过那两帧」时结果相等（非有限帧若计入
	// 累计样本数，起步期内下一帧的权重就不同）。
	nz := newNormalizer(true, target, maxGain, testSR)
	ref := newNormalizer(true, target, maxGain, testSR)
	g0 := nz.Apply(&dsp.Result{RMS: 0.1}, testHop)
	ref.Apply(&dsp.Result{RMS: 0.1}, testHop)
	for _, v := range []float32{nan, inf} {
		if g := nz.Apply(&dsp.Result{RMS: v}, testHop); g != g0 {
			t.Fatalf("RMS=%v 时增益 = %v, 期望按当前 EMA 得 %v", v, g, g0)
		}
	}
	g, want := nz.Apply(&dsp.Result{RMS: 0.2}, testHop), ref.Apply(&dsp.Result{RMS: 0.2}, testHop)
	if math.IsNaN(float64(g)) || math.IsInf(float64(g), 0) || g != want {
		t.Fatalf("非有限帧之后的增益 = %v, 期望 %v", g, want)
	}

	// 未播种：返回 1、不碰 r，之后首个有限帧照常播种。
	nz = newNormalizer(true, target, maxGain, testSR)
	r := &dsp.Result{RMS: nan, Bins: []float32{0.5}}
	if g := nz.Apply(r, testHop); g != 1 || r.Bins[0] != 0.5 {
		t.Fatalf("未播种时 RMS=NaN: 增益 = %v, bins = %v, 期望 1、[0.5]", g, r.Bins)
	}
	if nz.seenN != 0 || nz.warmN != 0 {
		t.Fatalf("RMS=NaN 不应播种，也不应计入累计样本数: seenN=%v warmN=%v", nz.seenN, nz.warmN)
	}
	nz.Apply(&dsp.Result{RMS: 0.1}, testHop)
	if nz.seenN == 0 || !approx(nz.emaMS, 0.01) {
		t.Fatalf("首个有限帧应播种: seenN=%v emaMS=%v", nz.seenN, nz.emaMS)
	}
}

func TestNormalizerEmptyBins(t *testing.T) {
	// nil / 空 bins 均不得 panic；RMS 仍正常缩放
	g := gainFactor(0.1, -14, 12)
	rNil := &dsp.Result{RMS: 0.1, Bins: nil}
	newNormalizer(true, -14, 12, testSR).Apply(rNil, testHop)
	if !approx(float64(rNil.RMS), 0.1*g) {
		t.Fatalf("nil bins 下 RMS=%v want %v", rNil.RMS, 0.1*g)
	}
	rEmpty := &dsp.Result{RMS: 0.1, Bins: []float32{}}
	newNormalizer(true, -14, 12, testSR).Apply(rEmpty, testHop)
	if len(rEmpty.Bins) != 0 || !approx(float64(rEmpty.RMS), 0.1*g) {
		t.Fatalf("空 bins 下 bins=%v RMS=%v", rEmpty.Bins, rEmpty.RMS)
	}
}

func TestNormalizerBinsMayExceedOne(t *testing.T) {
	// 低响度需高增益，bin 含 1.0，施增益后应 >1（确认不钳）
	g := gainFactor(0.1, -14, 12)
	if !(g > 1) {
		t.Fatalf("测试前提失效：期望 g>1, got %v", g)
	}
	r := &dsp.Result{RMS: 0.1, Bins: []float32{1.0, 0.8}}
	newNormalizer(true, -14, 12, testSR).Apply(r, testHop)
	if !(r.Bins[0] > 1) {
		t.Fatalf("bin=1.0 施增益后应 >1（不钳）, got %v", r.Bins[0])
	}
}

func assertClose(t *testing.T, name string, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 长度: got %d want %d (%v)", name, len(got), len(want), got)
	}
	for i := range want {
		if !(math.Abs(float64(got[i]-want[i])) <= 1e-6) { // 接受式：NaN 的比较为假，判失败
			t.Fatalf("%s[%d]: got %v want %v", name, i, got[i], want[i])
		}
	}
}

// assertFinite 断言 got 全为有限值，用于没有期望值、只需判有限的场合。
func assertFinite(t *testing.T, name string, got []float32) {
	t.Helper()
	for i, v := range got {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("%s[%d] 应为有限值, got %v", name, i, v)
		}
	}
}
