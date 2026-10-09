//go:build windows

package helper

import (
	"math"
	"runtime"
	"testing"
	"unsafe"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
	"Metabox-Nexus-PlayerCap/audiocap/frame"
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
	nz := newNormalizer(true, -14, 12)
	encodeFrame(a, nz, "kugou", 1, audioBatch(fftSize), [2]float32{})
	if !nz.seeded {
		t.Fatalf("真音频帧后应已播种，got seeded=false")
	}
	if !(nz.emaMS > 0) {
		t.Fatalf("真音频帧后 emaMS 应 >0, got %v", nz.emaMS)
	}
}

// keepalive（空 batch）经 encodeFrame → EMA 绝不被触碰（承重：静音不更新，避免恢复过冲）。
func TestEncodeFrameKeepaliveLeavesEMA(t *testing.T) {
	a := dsp.New(fftSize)

	// (a) 从未播种：keepalive 后仍 seeded==false、emaMS==0
	nz := newNormalizer(true, -14, 12)
	encodeFrame(a, nz, "kugou", 1, nil, [2]float32{})
	if nz.seeded || nz.emaMS != 0 {
		t.Fatalf("keepalive(未播种) 不应动 EMA: seeded=%v emaMS=%v", nz.seeded, nz.emaMS)
	}

	// (b) 已播种：先喂真音频播种，记下 emaMS，再 keepalive，emaMS 不变
	encodeFrame(a, nz, "kugou", 1, audioBatch(fftSize), [2]float32{})
	if !nz.seeded {
		t.Fatalf("前置真音频帧应已播种")
	}
	before := nz.emaMS
	encodeFrame(a, nz, "kugou", 1, nil, [2]float32{}) // keepalive：不经 Apply
	if nz.emaMS != before {
		t.Fatalf("keepalive(已播种) 不应改 emaMS: before=%v after=%v", before, nz.emaMS)
	}
}

// disabled Normalizer 下真音频帧：产出的帧值 == 未归一化（间接验 passthrough）。
func TestEncodeFrameDisabledPassthrough(t *testing.T) {
	batch := audioBatch(fftSize)

	raw := encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12), "kugou", 7, batch, [2]float32{})

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
		raw := encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12), "kugou", 1, c.batch, [2]float32{})
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
	f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12), "kugou", 1, batch, peaks))
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
	f = decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12), "kugou", 1, nil, [2]float32{0.7, 0.3}))
	if f.Flags != frame.FlagNormalized {
		t.Fatalf("保活帧 flags: got %#x want %#x", f.Flags, frame.FlagNormalized)
	}
	if f.PeakL != 0 || f.PeakR != 0 {
		t.Fatalf("保活帧 peak 应为 0: got %v %v", f.PeakL, f.PeakR)
	}

	// 归一化关闭、有音频：flags 为 0，peak、RMS、bins 逐位不变
	f = decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(false, -14, 12), "kugou", 1, batch, peaks))
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
	b, p := w.take()
	f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(true, -14, 12), "kugou", 1, b, p))
	assertFinite(t, "rms、peakL、peakR", []float32{f.RMS, f.PeakL, f.PeakR})
	assertFinite(t, "bins", f.Bins)
}
