package dsp

import (
	"math"
	"testing"
)

func TestRMSFullScaleSine(t *testing.T) {
	const n = 1024
	a := New(n)
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(math.Sin(2 * math.Pi * float64(i) / 64))
	} // 满幅正弦
	got := a.Analyze(s).RMS
	want := float32(math.Sqrt2 / 2) // ≈0.707
	if !(math.Abs(float64(got-want)) <= 0.02) {
		t.Fatalf("RMS=%v want≈%v", got, want)
	}
}

func TestRMSSilence(t *testing.T) {
	a := New(1024)
	if r := a.Analyze(make([]float32, 1024)).RMS; r != 0 {
		t.Fatalf("silence RMS=%v want 0", r)
	}
}

func TestSpectrumPeakBin(t *testing.T) {
	const n = 1024
	a := New(n)
	s := make([]float32, n)
	// 频率 = 采样率/周期；这里周期 16 样本 → bin = n/16 = 64
	for i := range s {
		s[i] = float32(math.Sin(2 * math.Pi * float64(i) / 16))
	}
	bins := a.Analyze(s).Bins
	peak := 0
	for i := range bins {
		if bins[i] > bins[peak] {
			peak = i
		}
	}
	if peak < 60 || peak > 68 {
		t.Fatalf("peak bin=%d want≈64", peak)
	}
	if len(bins) != n/2 {
		t.Fatalf("len(bins)=%d want %d", len(bins), n/2)
	}
}

// --- 边界补充测试 ---

func TestNewPanicsNonPowerOfTwo(t *testing.T) {
	for _, bad := range []int{0, 1, 3, 1000, -8} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("New(%d) 应 panic（非 2 的幂或过小）", bad)
				}
			}()
			New(bad)
		}()
	}
}

func TestNewAcceptsPowersOfTwo(t *testing.T) {
	for _, ok := range []int{2, 4, 256, 1024, 4096} {
		if a := New(ok); a == nil {
			t.Fatalf("New(%d) 返回 nil", ok)
		}
	}
}

// 不足长度补零：FFT 补零但 RMS 只按真实样本算，不被补的零稀释。
func TestAnalyzeShortInputZeroPad(t *testing.T) {
	a := New(8)
	r := a.Analyze([]float32{1, 1}) // 2 个真实样本，其余补零
	if !(math.Abs(float64(r.RMS-1)) <= 1e-6) {
		t.Fatalf("短输入 RMS=%v want 1（按真实样本算）", r.RMS)
	}
	if len(r.Bins) != 4 {
		t.Fatalf("len(bins)=%d want 4", len(r.Bins))
	}
}

// 空输入：不 panic、RMS=0、bins 全零且长度正确。
func TestAnalyzeEmptyInput(t *testing.T) {
	a := New(16)
	r := a.Analyze(nil)
	if r.RMS != 0 {
		t.Fatalf("空输入 RMS=%v want 0", r.RMS)
	}
	if len(r.Bins) != 8 {
		t.Fatalf("len(bins)=%d want 8", len(r.Bins))
	}
	for i, v := range r.Bins {
		if v != 0 {
			t.Fatalf("空输入 bins[%d]=%v want 0", i, v)
		}
	}
}

// 幅度谱归一化上界：对 [-1,1] 有界输入，加 Hann 窗后 bins 恒 ≤ 1。
func TestBinsBoundedForFullScale(t *testing.T) {
	const n = 512
	a := New(n)
	s := make([]float32, n)
	for i := range s { // 满幅方波，逼近归一上界
		if i%2 == 0 {
			s[i] = 1
		} else {
			s[i] = -1
		}
	}
	for i, v := range a.Analyze(s).Bins {
		if !(v >= 0 && v <= 1+1e-6) {
			t.Fatalf("bins[%d]=%v 越界 [0,1]", i, v)
		}
	}
}
