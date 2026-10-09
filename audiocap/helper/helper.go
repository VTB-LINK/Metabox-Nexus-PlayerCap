// Package helper 实现音频捕获 helper 子进程的进程内逻辑：从每个目标播放器进程的 WASAPI
// loopback 捕获 PCM，就地做 DSP（RMS、左右声道峰值与频谱），按 cadence 分帧，经 stdout 把自包含
// 帧流交给主服务（见 docs/audio-waveform-capture-design.md §2）。
//
// 本文件只放**无平台依赖的纯函数**（控制行解析、降混、按声道峰值累加、两次出帧之间的样本与
// 峰值窗口 window、丢最旧入队 enqueue、限时等待 waitTimeout、音量归一化），CI 可跑测试；
// 捕获/写/控制的编排在 helper_windows.go（//go:build windows），端到端须在真机 Windows 上验。
package helper

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
)

// control 是一行控制命令解析后的结构。op ∈ {"capture","stop"}；归一化字段（norm/target/
// maxGain）仅 capture 命令携带，stop 命令取零值。
type control struct {
	op      string
	pid     uint32
	player  string
	norm    bool // 是否启用音量归一化
	target  int  // 目标 dBFS（可为负）
	maxGain int  // 增益封顶 dB
}

// parseControl 解析一行控制命令：
//
//	"capture <pid> <player> <norm:0|1> <target_dbfs> <maxgain_db>" → op="capture" + 6 字段
//	"stop <player>"                                                → op="stop", player（pid 恒 0）
//
// norm 按 fields[3]=="1" 判定（非 "1" 一律 false）；target/maxGain 走 strconv.Atoi。
// 非法（空行 / 字段数不对 / pid 非无符号十进制 / target|maxGain 非整数 / 未知命令）返回 error。
func parseControl(line string) (control, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return control{}, fmt.Errorf("空控制行")
	}
	switch fields[0] {
	case "capture":
		if len(fields) != 6 {
			return control{}, fmt.Errorf("capture 需 <pid> <player> <norm> <target_dbfs> <maxgain_db>，得到 %q", line)
		}
		pid, e := strconv.ParseUint(fields[1], 10, 32)
		if e != nil {
			return control{}, fmt.Errorf("capture pid 非法 %q: %w", fields[1], e)
		}
		target, e := strconv.Atoi(fields[4])
		if e != nil {
			return control{}, fmt.Errorf("capture target_dbfs 非法 %q: %w", fields[4], e)
		}
		maxGain, e := strconv.Atoi(fields[5])
		if e != nil {
			return control{}, fmt.Errorf("capture maxgain_db 非法 %q: %w", fields[5], e)
		}
		return control{op: "capture", pid: uint32(pid), player: fields[2], norm: fields[3] == "1", target: target, maxGain: maxGain}, nil
	case "stop":
		if len(fields) != 2 {
			return control{}, fmt.Errorf("stop 需 <player>，得到 %q", line)
		}
		return control{op: "stop", player: fields[1]}, nil
	default:
		return control{}, fmt.Errorf("未知控制命令 %q", fields[0])
	}
}

// downmix 把交织多声道 f32 降混成单声道（各声道算术平均）。interleaved 长度应为 channels 的
// 整数倍，尾部不足一整帧的样本被忽略。channels<=1 视作已是单声道，原样返回。
func downmix(interleaved []float32, channels int) []float32 {
	if channels <= 1 {
		return interleaved
	}
	n := len(interleaved) / channels
	mono := make([]float32, n)
	for i := 0; i < n; i++ {
		base := i * channels
		var sum float32
		for c := 0; c < channels; c++ {
			sum += interleaved[base+c]
		}
		mono[i] = sum / float32(channels)
	}
	return mono
}

// accumulatePeaks 把交织样本按声道取绝对值的最大值并入 acc：声道 0 入 acc[0]，声道 1 入 acc[1]；
// channels<=1 时同一值并入两侧，channels>2 时只看声道 0 和 1，尾部不足一整帧的样本被忽略。
// 用比较而非内建 max：NaN 比较为假、自然不计入，而 max 遇 NaN 返回 NaN。超过 1 照常保留，作为削波信息。
func accumulatePeaks(acc [2]float32, interleaved []float32, channels int) [2]float32 {
	step := max(channels, 1)
	for i := 0; i+step <= len(interleaved); i += step {
		l := float32(math.Abs(float64(interleaved[i])))
		r := l
		if step > 1 {
			r = float32(math.Abs(float64(interleaved[i+1])))
		}
		if l > acc[0] {
			acc[0] = l
		}
		if r > acc[1] {
			acc[1] = r
		}
	}
	return acc
}

// window 累积两次出帧之间的非静音样本：batch 为降混后的单声道样本，peaks 为降混前按声道的
// sample peak。零值可用。
type window struct {
	batch []float32
	peaks [2]float32
}

// add 并入一个读包。silent（静音包）或 interleaved 为空时不做任何事。
//
// 非有限采样（NaN、±Inf）在降混与累积峰值之前按 0 计入：一个这样的采样可能让本帧 rms、bins 成为
// NaN 或无穷大，±Inf 还会让 peak 成为无穷大，并写到线上。有限值（含大于 1 的）不动。会就地改写
// 传入的 interleaved。
func (w *window) add(interleaved []float32, channels int, silent bool) {
	if silent || len(interleaved) == 0 {
		return
	}
	for i, v := range interleaved {
		if !(v >= -math.MaxFloat32 && v <= math.MaxFloat32) { // 接受式：NaN 的比较为假，落入置 0
			interleaved[i] = 0
		}
	}
	w.batch = append(w.batch, downmix(interleaved, channels)...)
	w.peaks = accumulatePeaks(w.peaks, interleaved, channels)
}

// take 取出当前 batch 与 peaks 并清零。返回的 batch 与内部缓冲共用底层数组，在下次 add 之前有效。
func (w *window) take() ([]float32, [2]float32) {
	b, p := w.batch, w.peaks
	w.batch = w.batch[:0]
	w.peaks = [2]float32{}
	return b, p
}

// enqueue 把帧非阻塞入队：满则丢最旧再放；仍满（并发生产者刚补满）则丢本帧。永不阻塞采集
// goroutine——这是「采集排空与写解耦」的承重点（§2.5）。纯 channel 逻辑、无平台依赖，故放此。
func enqueue(ch chan []byte, f []byte) {
	select {
	case ch <- f:
		return
	default:
	}
	select { // 丢最旧
	case <-ch:
	default:
	}
	select { // 再试一次；仍满则丢本帧
	case ch <- f:
	default:
	}
}

// waitTimeout 至多等待 d 让 wg 归零：期间归零返回 true，超时返回 false。超时后内部等待 goroutine
// 留在 wg.Wait 上，随 wg 归零或进程退出结束（调用方超时即退出进程，不构成长期泄漏）。
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// ===== 音量归一化（EMA-AGC）=====
// 对 dsp.Result 的 RMS/Bins 施一个统一线性增益，使响度收敛到目标 dBFS。增益按 RMS 功率
// （RMS²）的指数滑动均值估计并封顶 maxGainDB，避免静音被抬成噪声。纯逻辑、无平台依赖，
// 逐帧编排在 helper_windows.go。见 docs/audio-waveform-normalization-design.md §3.2。

const (
	normAlpha   = 0.03  // EMA 平滑系数，τ≈1s @ ~30fps（真机可调）
	dbfsEpsilon = 1e-10 // dbfs 下限，防 log10(0)=-Inf
)

// dbfs 把线性幅度换成 dBFS；对 <=ε 的输入（含 0、负数）钳到 ε 下限，结果恒有限。
func dbfs(x float64) float64 {
	return 20 * math.Log10(max(x, dbfsEpsilon))
}

// gainFactor 由响度估计 rmsEst 算线性增益：抬向 targetDBFS，但增幅封顶 maxGainDB。
func gainFactor(rmsEst float64, targetDBFS, maxGainDB int) float64 {
	return math.Pow(10, min(float64(targetDBFS)-dbfs(rmsEst), float64(maxGainDB))/20)
}

// Normalizer 持 RMS² 的 EMA 状态，对逐帧 dsp.Result 施平滑增益。非并发安全，每路捕获各持一个。
type Normalizer struct {
	enabled    bool
	targetDBFS int
	maxGainDB  int
	emaMS      float64 // RMS² 的指数滑动均值（均方）
	seeded     bool
}

func newNormalizer(enabled bool, targetDBFS, maxGainDB int) *Normalizer {
	return &Normalizer{enabled: enabled, targetDBFS: targetDBFS, maxGainDB: maxGainDB}
}

// Apply 就地把增益施到 r（RMS 与每个 Bin），并返回该线性增益，供调用方乘到 peak 上。
// disabled 时不碰 r、不更新 EMA，返回 1。
//
// RMS² 为 NaN 或 ±Inf 时既不更新也不播种 EMA：并入 NaN 会使 EMA 永久为 NaN，此后每帧的 RMS、
// bins、peak 都是 NaN；并入 +Inf 会使 EMA 永久为 +Inf、增益为 0，此后每帧的 RMS、bins、peak 都是 0，
// 波形永久静音。此时若尚未播种，不碰 r、返回 1；已播种则按当前 EMA 照常施加增益。
func (nz *Normalizer) Apply(r *dsp.Result) float32 {
	if !nz.enabled {
		return 1
	}
	if ms := float64(r.RMS) * float64(r.RMS); math.IsNaN(ms) || math.IsInf(ms, 0) {
		if !nz.seeded {
			return 1
		}
	} else if nz.seeded {
		nz.emaMS = normAlpha*ms + (1-normAlpha)*nz.emaMS
	} else {
		nz.emaMS = ms
		nz.seeded = true
	}
	g := float32(gainFactor(math.Sqrt(nz.emaMS), nz.targetDBFS, nz.maxGainDB))
	r.RMS *= g
	for i := range r.Bins {
		r.Bins[i] *= g
	}
	return g
}
