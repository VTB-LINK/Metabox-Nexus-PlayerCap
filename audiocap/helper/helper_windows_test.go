//go:build windows

package helper

import (
	"math"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/wasapi"
)

// audioBatch 造 n 个非零（恒 0.5）样本，保证 RMS>0 → 施归一化后 emaMS>0；谱确定可比。
func audioBatch(n int) []float32 {
	b := make([]float32, n)
	for i := range b {
		b[i] = 0.5
	}
	return b
}

// 真音频帧（非空 batch）经 encodeFrame → Normalizer 被喂到：播种且 emaMS>0。
func TestEncodeFrameRealAudioUpdatesEMA(t *testing.T) {
	a := dsp.New(fftSize)
	nz := newNormalizer(true, -14, 12, testSR)
	encodeFrame(a, nz, "kugou", 1, audioBatch(fftSize), [2]float32{}, fftSize)
	if nz.seenN == 0 {
		t.Fatalf("真音频帧后应已播种，got seenN=0")
	}
	if !(nz.emaMS > 0) {
		t.Fatalf("真音频帧后 emaMS 应 >0, got %v", nz.emaMS)
	}
}

// keepalive（空 batch）经 encodeFrame → EMA 绝不被触碰（承重：静音不更新，避免恢复过冲）。
func TestEncodeFrameKeepaliveLeavesEMA(t *testing.T) {
	a := dsp.New(fftSize)

	// (a) 从未播种：keepalive 后仍尚无估计（seenN==0、emaMS==0）
	nz := newNormalizer(true, -14, 12, testSR)
	encodeFrame(a, nz, "kugou", 1, nil, [2]float32{}, 0)
	if nz.seenN != 0 || nz.warmN != 0 || nz.emaMS != 0 {
		t.Fatalf("keepalive(未播种) 不应动估计: seenN=%v warmN=%v emaMS=%v", nz.seenN, nz.warmN, nz.emaMS)
	}

	// (b) 已播种：先喂真音频播种，记下估计与两个累计样本数，再 keepalive，三者都不变
	encodeFrame(a, nz, "kugou", 1, audioBatch(fftSize), [2]float32{}, fftSize)
	if nz.seenN == 0 {
		t.Fatalf("前置真音频帧应已播种")
	}
	before := *nz
	encodeFrame(a, nz, "kugou", 1, nil, [2]float32{}, 0) // keepalive：不经 Apply
	if *nz != before {
		t.Fatalf("keepalive(已播种) 不应改动归一化状态: before=%+v after=%+v", before, *nz)
	}
}

// disabled Normalizer 下真音频帧：产出的帧值 == 未归一化（间接验 passthrough）。
func TestEncodeFrameDisabledPassthrough(t *testing.T) {
	batch := audioBatch(fftSize)

	raw := encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12, testSR), "kugou", 7, batch, [2]float32{}, len(batch))

	// 参照：同窗直接 Analyze（未施任何增益）
	want := dsp.New(fftSize).Analyze(batch)

	var dec frame.Decoder
	dec.Push(raw)
	f, ok, err := dec.Next()
	if err != nil || !ok {
		t.Fatalf("解帧失败: ok=%v err=%v", ok, err)
	}
	if f.RMS != want.RMS {
		t.Fatalf("disabled RMS 应原样: got %v want %v", f.RMS, want.RMS)
	}
	assertClose(t, "disabled bins 原样", f.Bins, want.Bins)
}

// decodeOne 解出 encodeFrame 产出的单帧。
func decodeOne(t *testing.T, raw []byte) frame.Frame {
	t.Helper()
	var dec frame.Decoder
	dec.Push(raw)
	f, ok, err := dec.Next()
	if err != nil || !ok {
		t.Fatalf("解帧失败: ok=%v err=%v", ok, err)
	}
	return f
}

// 真帧与保活帧的 TSMillis 均为 QPC 毫秒：夹在调用 encodeFrame 前后两次 frame.QPCMillis 读数之间。
// 先以独立读取的系统 QPC 频率核对包初始化；再注入 2 倍加 1 的奇数频率（不会与写死的常见频率相等），
// 使不经 qpcFrequency 换算的实现变红。
func TestEncodeFrameTimestampQPC(t *testing.T) {
	var want int64 // 不经被测的 queryQPFreq
	procQPFreq.Call(uintptr(unsafe.Pointer(&want)))
	runtime.KeepAlive(&want)
	if qpcFrequency <= 0 || qpcFrequency != want {
		t.Fatalf("qpcFrequency = %d, 期望系统 QPC 频率 %d", qpcFrequency, want)
	}
	old := qpcFrequency
	freq := old*2 + 1
	qpcFrequency = freq
	t.Cleanup(func() { qpcFrequency = old })

	for _, c := range []struct {
		name  string
		batch []float32
	}{{"音频帧", audioBatch(fftSize)}, {"保活帧", nil}} {
		before := frame.QPCMillis(readQPC(), freq)
		raw := encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12, testSR), "kugou", 1, c.batch, [2]float32{}, len(c.batch))
		after := frame.QPCMillis(readQPC(), freq)
		if ts := decodeOne(t, raw).TSMillis; !(before <= ts && ts <= after) {
			t.Fatalf("%s TSMillis=%v 不在 QPC 毫秒区间 [%v, %v]", c.name, ts, before, after)
		}
	}
}

// peak 与 RMS、bins 乘同一归一化增益；flags 标记归一化，保活帧同样置位且 peak 恒为 0。
func TestEncodeFramePeaksAndFlags(t *testing.T) {
	batch := audioBatch(fftSize)
	raw := dsp.New(fftSize).Analyze(batch) // 未归一化参照
	peaks := [2]float32{0.5, 0.25}

	// 归一化启用、有音频
	f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12, testSR), "kugou", 1, batch, peaks, len(batch)))
	if f.Flags != frame.FlagNormalized {
		t.Fatalf("启用归一化 flags: got %#x want %#x", f.Flags, frame.FlagNormalized)
	}
	if f.RMS == raw.RMS {
		t.Fatalf("测试前提失效：期望增益 ≠ 1, RMS=%v", f.RMS)
	}
	if got, want := float64(f.PeakL)/float64(f.RMS), float64(peaks[0])/float64(raw.RMS); !approx(got, want) {
		t.Fatalf("PeakL/RMS=%v want %v（peak 应与 RMS 乘同一增益）", got, want)
	}
	if got, want := float64(f.PeakR)/float64(f.RMS), float64(peaks[1])/float64(raw.RMS); !approx(got, want) {
		t.Fatalf("PeakR/RMS=%v want %v（peak 应与 RMS 乘同一增益）", got, want)
	}

	// 归一化启用、保活帧：flags 仍置位，peak 不论传入值恒为 0
	f = decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12, testSR), "kugou", 1, nil, [2]float32{0.7, 0.3}, 0))
	if f.Flags != frame.FlagNormalized {
		t.Fatalf("保活帧 flags: got %#x want %#x", f.Flags, frame.FlagNormalized)
	}
	if f.PeakL != 0 || f.PeakR != 0 {
		t.Fatalf("保活帧 peak 应为 0: got %v %v", f.PeakL, f.PeakR)
	}

	// 归一化关闭、有音频：flags 为 0，peak、RMS、bins 逐位不变
	f = decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12, testSR), "kugou", 1, batch, peaks, len(batch)))
	if f.Flags != 0 {
		t.Fatalf("关闭归一化 flags: got %#x want 0", f.Flags)
	}
	if f.PeakL != 0.5 || f.PeakR != 0.25 {
		t.Fatalf("关闭归一化 peak 应原样: got %v %v", f.PeakL, f.PeakR)
	}
	if math.Float32bits(f.RMS) != math.Float32bits(raw.RMS) {
		t.Fatalf("关闭归一化 RMS 应逐位不变: got %v want %v", f.RMS, raw.RMS)
	}
	if len(f.Bins) != len(raw.Bins) {
		t.Fatalf("bins 长度: got %d want %d", len(f.Bins), len(raw.Bins))
	}
	for i := range raw.Bins {
		if math.Float32bits(f.Bins[i]) != math.Float32bits(raw.Bins[i]) {
			t.Fatalf("关闭归一化 bins[%d] 应逐位不变: got %v want %v", i, f.Bins[i], raw.Bins[i])
		}
	}
}

// 含 NaN 与 ±Inf 的非静音包经 window、启用归一化的 encodeFrame 编码后，帧内 rms、peak 与 bins
// 均为有限值。包长 fftSize 个立体声采样帧，降混后恰为 RMS 与 FFT 所用的 fftSize 个采样；非有限值
// 分布在左右两个声道。
func TestEncodeFrameNonFiniteSamples(t *testing.T) {
	pkt := audioBatch(2 * fftSize)
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	pkt[10], pkt[fftSize+1], pkt[2*fftSize-2] = nan, inf, -inf

	var w window
	w.add(pkt, 2, false)
	f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12, testSR), "kugou", 1, w.batch, w.peaks, w.fresh))
	assertFinite(t, "rms、peakL、peakR", []float32{f.RMS, f.PeakL, f.PeakR})
	assertFinite(t, "bins", f.Bins)
}

// 分帧接到 encodeFrame 的端到端：1 秒的 3000 Hz 正弦（幅度 0.5，恰落在第 64 个频点）按 480 帧的包
// 喂入，各帧率下解出的帧数等于帧率；除首帧外，每帧的分析窗都是完整的 1024 个样本，第 64 个频点的读数
// 为幅度的一半 0.25（周期 Hann 窗、整数频点，误差在 1% 以内）。60 帧每秒下每帧只新增 800 个样本，靠
// framer 留着的此前的样本补足。
//
// 变异自证（隔离副本，实测）：framer 不保留此前的样本 → 60 帧每秒下该频点读数为 0.234，红。encodeFrame
// 分析时改取 batch 开头的 fftSize 个不影响本用例（实测仍绿），由 TestEncodeFrameUsesLatestSamples 另守。
func TestFramesAtEachRateKeepFullAnalysisWindow(t *testing.T) {
	const bin, amp = 64, 0.5
	for _, fps := range []int{15, 25, 30, 50, 60} {
		a := dsp.New(fftSize)
		nz := newNormalizer(false, -14, 30, wasapi.SampleRate)
		var frames []frame.Frame
		emit := func(batch []float32, peaks [2]float32, fresh int) {
			frames = append(frames, decodeOne(t, encodeFrame(a, nz, "kugou", 1, batch, peaks, fresh)))
		}
		fr := newFramer(hopFor(wasapi.SampleRate, fps), fftSize)
		for i := 0; i < 100; i++ {
			pkt := make([]float32, 960)
			for j := 0; j < 480; j++ {
				n := i*480 + j
				v := float32(amp * math.Sin(2*math.Pi*bin*float64(n)/fftSize))
				pkt[2*j], pkt[2*j+1] = v, v
			}
			fr.push(pkt, wasapi.Channels, false, emit)
		}
		if len(frames) != fps {
			t.Fatalf("%d 帧每秒: 1 秒的样本解出 %d 帧", fps, len(frames))
		}
		for i, f := range frames[1:] {
			if len(f.Bins) != fftSize/2 {
				t.Fatalf("%d 帧每秒第 %d 帧: bins 长度 %d", fps, i+1, len(f.Bins))
			}
			if got, want := float64(f.Bins[bin]), amp/2; math.Abs(got-want) > 0.01*want {
				t.Fatalf("%d 帧每秒第 %d 帧: 第 %d 个频点读数 %.4f, 期望 %.3f（分析窗不足 1024 个样本时偏低）", fps, i+1, bin, got, want)
			}
		}
	}
}

// 高帧率下帧内混有静音包时，频谱与 RMS 仍按完整的分析窗算。60 帧每秒（每帧 800 个采样帧），依次喂
// 四个 480 帧的包：非静音、非静音、静音、非静音，非静音样本恒为 0.5，归一化关闭。第二帧只新增 320 个
// 样本，但此前已有 800 个非静音样本：其 RMS 为 0.5，直流频点 bins[0] 也应为 0.5（周期 Hann 窗下恒定
// 输入的读数即其幅度）；分析窗若只有 544 个样本，bins[0] 掉到约 0.28 而 RMS 不变，两者不一致。
//
// 变异自证（隔离副本，实测）：framer 出帧后只留 analysisLen-hop 个 → 第二帧 bins[0] 为 0.28，红。
func TestHighRateFrameWithSilentPacketKeepsFullWindow(t *testing.T) {
	a := dsp.New(fftSize)
	nz := newNormalizer(false, -14, 30, wasapi.SampleRate)
	var frames []frame.Frame
	emit := func(batch []float32, peaks [2]float32, fresh int) {
		frames = append(frames, decodeOne(t, encodeFrame(a, nz, "kugou", 1, batch, peaks, fresh)))
	}
	fr := newFramer(hopFor(wasapi.SampleRate, 60), fftSize)
	for _, silent := range []bool{false, false, true, false} {
		pkt := make([]float32, 960)
		for j := range pkt {
			pkt[j] = 0.5
		}
		fr.push(pkt, wasapi.Channels, silent, emit)
	}
	if len(frames) != 2 {
		t.Fatalf("出帧数 = %d, 期望 2", len(frames))
	}
	f := frames[1]
	if math.Abs(float64(f.RMS)-0.5) > 1e-6 {
		t.Fatalf("第二帧 RMS = %v, 期望 0.5", f.RMS)
	}
	if math.Abs(float64(f.Bins[0])-0.5) > 1e-3 {
		t.Fatalf("第二帧 bins[0] = %v, 期望 0.5（分析窗应为完整的 %d 个样本）", f.Bins[0], fftSize)
	}
}

// encodeFrame 的 RMS 与 bins 取 batch 的最近 fftSize 个样本：batch 前半为 0、后 fftSize 个为 0.5 时，
// RMS 为 0.5 而不是被前半稀释。
//
// 变异自证（隔离副本，实测）：改取 batch 开头的 fftSize 个 → RMS 为 0，红。
func TestEncodeFrameUsesLatestSamples(t *testing.T) {
	batch := append(make([]float32, fftSize), audioBatch(fftSize)...)
	f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 30, wasapi.SampleRate), "kugou", 1, batch, [2]float32{}, len(batch)))
	if math.Abs(float64(f.RMS)-0.5) > 1e-6 {
		t.Fatalf("RMS = %v, 期望 0.5（只看最近 %d 个样本）", f.RMS, fftSize)
	}
}

// encodeFrame 把本帧新增的样本数原样交给归一化作权重：先喂一个整窗的帧，再喂一个只新增 480 个样本的
// 帧，估计应与直接以 480 调 Apply 的参照相等。
//
// 变异自证（隔离副本，实测）：encodeFrame 传给 Apply 的样本数写死为 fftSize → 第二帧的权重偏大，红。
func TestEncodeFramePassesFreshToNormalizer(t *testing.T) {
	loud, quiet := audioBatch(fftSize), make([]float32, fftSize)
	for i := range quiet {
		quiet[i] = 0.05
	}
	a := dsp.New(fftSize)
	nz := newNormalizer(true, -14, 30, wasapi.SampleRate)
	encodeFrame(a, nz, "kugou", 1, loud, [2]float32{}, fftSize)
	encodeFrame(a, nz, "kugou", 1, quiet, [2]float32{}, 480)

	ref := newNormalizer(true, -14, 30, wasapi.SampleRate)
	for _, c := range []struct {
		batch []float32
		n     int
	}{{loud, fftSize}, {quiet, 480}} {
		res := dsp.New(fftSize).Analyze(c.batch)
		ref.Apply(&res, c.n)
	}
	if nz.emaMS != ref.emaMS {
		t.Fatalf("估计 = %v, 期望与按 480 个样本计权的参照 %v 相等", nz.emaMS, ref.emaMS)
	}
}

// pumpCapture 按控制命令的参数出帧：帧率 30 时，1 秒连续的数据（每 10ms 一个 480 帧的包）出 30 帧，
// 其后 6 次间隔 20ms 的无数据读包按帧间隔（33.3ms）出 3 帧保活零帧；每帧带控制命令里的播放器名与
// 传入的 epoch；归一化按 ctl.norm 开关，开启时恒定输入的 rms 即目标电平。
//
// 变异自证（隔离副本，实测）：pumpCapture 里帧率写死 25 → 出 28 帧，红；传给 pumpFrames 的帧间隔写成
// 0 → 无数据的 6 次读包各出一帧（共 36 帧），红；newNormalizer 的开关写死 false → flags 为 0，红；
// target 与 maxGain 对调 → rms 为 0.005，红；播放器名写死 → 红。captureLoop 在激活成功后调用本函数
// 这一处依赖真捕获，删掉该调用全包测试仍绿（实测），靠人守。
func TestPumpCaptureHonorsControl(t *testing.T) {
	for _, norm := range []bool{true, false} {
		clk := time.Unix(1000, 0)
		i := 0
		read := func(dst []float32) (int, bool, error) {
			defer func() { i++ }()
			switch {
			case i < 100:
				clk = clk.Add(10 * time.Millisecond)
				for j := 0; j < 960; j++ {
					dst[j] = 0.05
				}
				return 480, false, nil
			case i < 106:
				clk = clk.Add(20 * time.Millisecond)
				return 0, false, nil
			}
			return 0, false, errStopPump
		}
		frames := make(chan []byte, 256)
		ctl := control{op: "capture", pid: 1, player: "qqmusic", norm: norm, target: -20, maxGain: 12, fps: 30}
		if err := pumpCapture(read, func() time.Time { return clk }, ctl, 7, make(chan struct{}), frames); err != errStopPump {
			t.Fatalf("pumpCapture 应返回读包的错误, 得 %v", err)
		}
		close(frames)
		var got []frame.Frame
		for raw := range frames {
			got = append(got, decodeOne(t, raw))
		}
		if len(got) != 33 {
			t.Fatalf("norm=%v: 出帧数 = %d, 期望 33（30 帧音频 + 3 帧保活）", norm, len(got))
		}
		wantFlags, wantRMS := uint32(0), 0.05
		if norm {
			wantFlags, wantRMS = frame.FlagNormalized, math.Pow(10, -20.0/20) // 0.05 即 -26 dBFS，需 +6 dB，未触及上限 12
		}
		for k, f := range got {
			if f.Player != "qqmusic" || f.Epoch != 7 || f.Flags != wantFlags {
				t.Fatalf("norm=%v 第 %d 帧: player=%q epoch=%d flags=%#x, 期望 qqmusic、7、%#x", norm, k, f.Player, f.Epoch, f.Flags, wantFlags)
			}
			if k < 30 {
				if math.Abs(float64(f.RMS)-wantRMS) > 1e-4 {
					t.Fatalf("norm=%v 第 %d 帧: rms=%v, 期望 %v", norm, k, f.RMS, wantRMS)
				}
			} else if f.RMS != 0 || len(f.Bins) != fftSize/2 {
				t.Fatalf("norm=%v 第 %d 帧应为保活零帧: rms=%v bins=%d", norm, k, f.RMS, len(f.Bins))
			}
		}
	}
}

// 起步的端到端：归一化开启，捕获开头 100ms 是 -66 dBFS 的低电平（高于静音门限），其后为 -20 dBFS。
// 经分帧、分析、归一化、编码后解出的各帧都不满幅（rms < 0.4）；头两帧全是低电平，第三帧起的分析窗已
// 落在正常电平上，输出在目标 ±0.5 dB 以内（第三帧的分析窗还带着 64 个低电平样本，其后几帧因此略高于
// 目标）。
//
// 变异自证（隔离副本，实测）：去掉重新起步 → 第三帧高出目标 3 dB，红；起步期的期限与 τ 的秒数直接比
// （起步期一帧结束）→ 第三帧满幅，红。
func TestPumpCaptureStartupNoOvershoot(t *testing.T) {
	clk := time.Unix(1000, 0)
	i := 0
	read := func(dst []float32) (int, bool, error) {
		defer func() { i++ }()
		if i == 200 {
			return 0, false, errStopPump
		}
		clk = clk.Add(10 * time.Millisecond)
		level := float32(0.1)
		if i < 10 {
			level = 0.0005
		}
		for j := 0; j < 960; j++ {
			dst[j] = level
		}
		return 480, false, nil
	}
	frames := make(chan []byte, 256)
	ctl := control{op: "capture", pid: 1, player: "kugou", norm: true, target: -14, maxGain: 30, fps: 25}
	if err := pumpCapture(read, func() time.Time { return clk }, ctl, 1, make(chan struct{}), frames); err != errStopPump {
		t.Fatalf("pumpCapture: %v", err)
	}
	close(frames)
	target := math.Pow(10, -14.0/20)
	k := 0
	for raw := range frames {
		f := decodeOne(t, raw)
		if f.RMS >= 0.4 {
			t.Fatalf("第 %d 帧 rms=%v，满幅", k+1, f.RMS)
		}
		if k >= 2 && math.Abs(20*math.Log10(float64(f.RMS)/target)) > 0.5 {
			t.Fatalf("第 %d 帧 rms=%v，期望在目标 %.4f 的 ±0.5 dB 以内", k+1, f.RMS, target)
		}
		k++
	}
	if k != 50 {
		t.Fatalf("2 秒的样本解出 %d 帧，期望 50", k)
	}
}
