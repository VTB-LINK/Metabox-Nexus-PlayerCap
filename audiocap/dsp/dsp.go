// Package dsp 对单声道音频窗口做基础频域分析：RMS + 归一化幅度谱。
// 纯 Go、无平台依赖，可在 CI 运行；供 helper 就地算帧（见 audio-waveform-capture-design.md §2.3）。
package dsp

import (
	"fmt"
	"math"
)

// Result 是一个分析窗的输出。
// Bins 为幅度谱，长度 = fftSize/2，对 [-1,1] 有界输入归一到 0..1；RMS 为该窗均方根。
type Result struct {
	RMS  float32
	Bins []float32
}

// Analyzer 持有固定 fftSize 的窗函数与复用缓冲，避免每帧分配。
// 非并发安全：每路捕获用独立 Analyzer；Analyze 返回的 Bins 复用内部缓冲，
// 在下一次 Analyze 调用前有效，调用方需先消费（helper 侧即时编码成帧）。
type Analyzer struct {
	fftSize int
	window  []float32 // 周期 Hann 窗，sum(window)==fftSize/2
	re, im  []float64 // FFT 工作缓冲
	bins    []float32 // 输出缓冲（长度 fftSize/2）
}

// New 构造 fftSize 点分析器。fftSize 必须是 ≥2 的 2 的幂，否则 panic（构造期校验）。
func New(fftSize int) *Analyzer {
	if fftSize < 2 || fftSize&(fftSize-1) != 0 {
		panic(fmt.Sprintf("dsp: fftSize 必须是 ≥2 的 2 的幂, got %d", fftSize))
	}
	w := make([]float32, fftSize)
	for i := range w {
		// 周期 Hann（除以 N，非 N-1）：sum(w)=N/2 恰好，给出 bins≤1 的归一上界。
		w[i] = float32(0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(fftSize))))
	}
	return &Analyzer{
		fftSize: fftSize,
		window:  w,
		re:      make([]float64, fftSize),
		im:      make([]float64, fftSize),
		bins:    make([]float32, fftSize/2),
	}
}

// Analyze 分析一窗单声道样本。len(mono) 应 == fftSize：不足在 FFT 侧补零，超出则截断。
// RMS 在加窗前的真实样本上算（sqrt(mean(x^2))），不含补的零，避免窗衰减与补零稀释响度。
func (a *Analyzer) Analyze(mono []float32) Result {
	n := a.fftSize
	k := len(mono)
	if k > n {
		k = n
	}

	// RMS：加窗前、仅真实样本。
	var sumSq float64
	for i := 0; i < k; i++ {
		v := float64(mono[i])
		sumSq += v * v
	}
	var rms float32
	if k > 0 {
		rms = float32(math.Sqrt(sumSq / float64(k)))
	}

	// 加 Hann 窗写入实部；k 之后补零；虚部清零。
	re, im := a.re, a.im
	for i := 0; i < n; i++ {
		if i < k {
			re[i] = float64(mono[i]) * float64(a.window[i])
		} else {
			re[i] = 0
		}
		im[i] = 0
	}

	fft(re, im)

	// 前 n/2 bin 取模，按 n/2 归一。
	half := n / 2
	norm := float64(half)
	bins := a.bins
	for i := 0; i < half; i++ {
		bins[i] = float32(math.Sqrt(re[i]*re[i]+im[i]*im[i]) / norm)
	}
	return Result{RMS: rms, Bins: bins}
}

// fft 就地基-2 Cooley-Tukey（迭代，Danielson-Lanczos），len 必须是 2 的幂。
// twiddle 增量递推（每级一次 cos/sin），fftSize≤64k 时 float64 累积误差可忽略。
func fft(re, im []float64) {
	n := len(re)

	// 位反转置换。
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}

	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wr, wi := math.Cos(ang), math.Sin(ang)
		half := length >> 1
		for i := 0; i < n; i += length {
			cr, ci := 1.0, 0.0
			for k := 0; k < half; k++ {
				lo := i + k
				hi := lo + half
				tr := cr*re[hi] - ci*im[hi]
				ti := cr*im[hi] + ci*re[hi]
				re[hi] = re[lo] - tr
				im[hi] = im[lo] - ti
				re[lo] += tr
				im[lo] += ti
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}
