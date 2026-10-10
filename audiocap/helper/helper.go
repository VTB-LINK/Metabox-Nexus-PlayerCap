// Package helper 实现音频捕获 helper 子进程的进程内逻辑：从每个目标播放器进程的 WASAPI
// loopback 捕获 PCM，就地做 DSP（RMS、左右声道峰值与频谱），按样本数分帧，经 stdout 把自包含
// 帧流交给主服务（见 doc/design/audio-waveform-capture-design.md §2）。
//
// 本文件只放**无平台依赖的纯函数**（控制行解析、降混、按声道峰值累加、非静音样本窗口 window、
// 按样本数分帧 framer 与出帧循环 pumpFrames、丢最旧入队 enqueue、限时等待 waitTimeout、音量归一化），
// CI 可跑测试；捕获/写/控制的编排在 helper_windows.go（//go:build windows），端到端须在真机 Windows 上验。
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
// maxGain）与帧率 fps 仅 capture 命令携带，stop 命令取零值。
type control struct {
	op      string
	pid     uint32
	player  string
	norm    bool // 是否启用音量归一化
	target  int  // 目标 dBFS（可为负）
	maxGain int  // 增益封顶 dB
	fps     int  // 出帧帧率（帧每秒）
}

// parseControl 解析一行控制命令：
//
//	"capture <pid> <player> <norm:0|1> <target_dbfs> <maxgain_db> <fps>" → op="capture" + 7 字段
//	"stop <player>"                                                      → op="stop", player（pid 恒 0）
//
// norm 按 fields[3]=="1" 判定（非 "1" 一律 false）；target/maxGain/fps 走 strconv.Atoi。fps 小于 1
// 时算不出每帧样本数，按非法处理；取值范围由主服务的配置层钳制，此处不重复判断。
// 非法（空行 / 字段数不对 / pid 非无符号十进制 / target|maxGain|fps 非整数 / fps 小于 1 / 未知命令）
// 返回 error。
func parseControl(line string) (control, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return control{}, fmt.Errorf("空控制行")
	}
	switch fields[0] {
	case "capture":
		if len(fields) != 7 {
			return control{}, fmt.Errorf("capture 需 <pid> <player> <norm> <target_dbfs> <maxgain_db> <fps>，得到 %q", line)
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
		fps, e := strconv.Atoi(fields[6])
		if e != nil || fps < 1 {
			return control{}, fmt.Errorf("capture fps 非法 %q", fields[6])
		}
		return control{op: "capture", pid: uint32(pid), player: fields[2], norm: fields[3] == "1", target: target, maxGain: maxGain, fps: fps}, nil
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

// window 累积非静音样本供出帧：batch 为降混后的单声道样本，末尾 fresh 个是自上次出帧以来新增的，
// 其前是 framer 留下、补足分析窗的此前的样本；peaks 为新增样本在降混前按声道的 sample peak。零值可用。
type window struct {
	batch []float32
	peaks [2]float32
	fresh int
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
	mono := downmix(interleaved, channels)
	w.batch = append(w.batch, mono...)
	w.fresh += len(mono)
	w.peaks = accumulatePeaks(w.peaks, interleaved, channels)
}

// emitFunc 接收一帧。fresh 为本帧新增的非静音样本数，0 即保活零帧；batch 为交给分析的样本（末尾
// fresh 个是新增的，其前可有 framer 留下的此前的样本，见 framer.keep），保活零帧时为 nil；peaks 为
// 新增样本按声道的峰值。batch 与 framer 的内部缓冲共用底层数组，只在回调返回前有效。
type emitFunc func(batch []float32, peaks [2]float32, fresh int)

// framer 按样本数分帧：读包（含静音包）按采样帧数计入，每凑满 hop 出一帧，包跨越帧界时在界上切开。
// 帧率因此由数据自身的时钟决定，与读包的返回粒度无关。用 newFramer 构造。
type framer struct {
	hop    int // 每帧的采样帧数
	keep   int // 分析窗长；hop 不小于它时为 0。非 0 时窗口留着最近至多这么多个非静音样本，出帧交出的也至多这么多
	filled int // 本帧已计入的采样帧数（含静音包）
	win    window
}

// newFramer 构造每帧 hop 个采样帧的 framer，analysisLen 为分析窗长。hop 不小于 analysisLen 时出帧即
// 清空窗口。hop 小于 analysisLen 时每帧新增的样本不足一个分析窗，窗口留着最近至多 analysisLen 个
// 非静音样本，每帧交出其中最近的 analysisLen 个。留的不是 analysisLen-hop 个：静音包推进帧的计数而
// 不新增样本，一帧的新增可以远少于 hop，按「每帧恰新增 hop 个」留就补不满分析窗。
func newFramer(hop, analysisLen int) *framer {
	hop = max(hop, 1)
	keep := 0
	if hop < analysisLen {
		keep = analysisLen
	}
	return &framer{hop: hop, keep: keep, win: window{batch: make([]float32, 0, keep+hop)}}
}

// hopFor 返回帧率 fps 下每帧的采样帧数：向下取整，至少为 1。
func hopFor(sampleRate, fps int) int { return max(sampleRate/max(fps, 1), 1) }

// push 并入一个读包（交织样本；silent 为静音包），每凑满 hop 调一次 emit，一个包可出多帧。静音包只
// 推进计数、不进窗口。尾部不足一个采样帧的样本被忽略。
func (f *framer) push(interleaved []float32, channels int, silent bool, emit emitFunc) {
	channels = max(channels, 1)
	for n := len(interleaved) / channels; n > 0; n = len(interleaved) / channels {
		take := min(n, f.hop-f.filled)
		f.win.add(interleaved[:take*channels], channels, silent)
		interleaved = interleaved[take*channels:]
		if f.filled += take; f.filled == f.hop {
			f.flush(emit)
		}
	}
}

// flush 立即出一帧并把计数归零：窗口内有新增样本为真帧，否则为保活零帧。凑满 hop 时由 push 调用；
// 未凑满时由墙钟兜底调用（pumpFrames），出的是不足一帧的残帧。
func (f *framer) flush(emit emitFunc) {
	w := &f.win
	if w.fresh == 0 {
		emit(nil, [2]float32{}, 0)
		w.batch = w.batch[:0] // 隔了一帧静音：之前的样本不再进入之后的分析窗
	} else {
		out, kept := w.batch, w.batch[len(w.batch):] // keep 为 0：整帧交出，出帧后清空
		if f.keep > 0 {
			out = w.batch[max(len(w.batch)-f.keep, 0):] // 最近至多 keep 个；新增的不超过 hop、少于 keep，都在其中
			kept = out
		}
		emit(out, w.peaks, w.fresh)
		// emit 返回后才能挪动缓冲：out 与它共用底层数组。
		w.batch = w.batch[:copy(w.batch, kept)]
	}
	w.peaks, w.fresh, f.filled = [2]float32{}, 0, 0
}

// maxFrameGap 是有数据到来而凑不满一帧时两次出帧之间的墙钟上限，取读包的超时。管理器的 hangThreshold
// 与 noCaptureAfter 以「捕获正常时最长约 100ms 出一帧」为前提（audiocap/manager/gating.go）。
const maxFrameGap = 100 * time.Millisecond

// clockDue 报告是否该按墙钟出帧：since 为距上次出帧的时长，gotData 为本次读包是否读到了数据，
// interval 为帧间隔。
//
// 读到数据时上限为 maxFrameGap：连续的数据流按样本数先出帧，到不了这个上限；数据断续到来、始终凑不满
// 一帧时由它兜住。没读到数据时满一个帧间隔即出帧（不超过 maxFrameGap）：读包超时可能略早于
// maxFrameGap 返回（推断，未实测），此时仍用 maxFrameGap 会隔一次超时才出帧。
func clockDue(since time.Duration, gotData bool, interval time.Duration) bool {
	if gotData {
		return since >= maxFrameGap
	}
	return since >= min(interval, maxFrameGap)
}

// readFunc 读一个包到 dst，返回采样帧数、是否静音包、错误；与 wasapi.Capturer.Read 同签名。
type readFunc func(dst []float32) (frames int, silent bool, err error)

// pumpFrames 是一路捕获的出帧循环：读包，按样本数分帧（fr），每次读包返回后按墙钟兜底（clockDue），
// 直到 done 关闭（返回 nil）或读包出错（返回该错误）。buf 为读包缓冲，channels 为声道数，interval 为
// 帧间隔；now 可注入，测试用假时钟。
func pumpFrames(read readFunc, now func() time.Time, done <-chan struct{}, fr *framer, buf []float32, channels int, interval time.Duration, emit emitFunc) error {
	last := now()
	out := func(batch []float32, peaks [2]float32, fresh int) {
		emit(batch, peaks, fresh)
		last = now()
	}
	for {
		select {
		case <-done:
			return nil
		default:
		}
		n, silent, err := read(buf)
		if err != nil {
			return err
		}
		fr.push(buf[:n*channels], channels, silent, out)
		if clockDue(now().Sub(last), n > 0, interval) {
			fr.flush(out)
		}
	}
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
// （RMS²）的指数滑动均值估计并封顶 maxGainDB，避免静音被抬成噪声。捕获刚启动时估计尚无依据，
// 起步期内另有保护，使波形一开始就是正常电平、不出现增益过冲。纯逻辑、无平台依赖，逐帧编排在
// helper_windows.go。见 doc/design/audio-waveform-normalization-design.md §3.2。

const (
	normTau     = 1.3   // 响度估计的时间常数（秒），升降同值；25 帧每秒下每帧系数为 0.0303，此前按帧定为 0.03
	dbfsEpsilon = 1e-10 // dbfs 下限，防 log10(0)=-Inf

	// normGateMS 是静音门限 −70 dBFS 的均方值：RMS² 低于它的帧不参与响度估计，增益照常施加。取值同
	// ITU-R BS.1770 的绝对门限；max-gain 为默认的 30 dB 时，这样的帧放大后仍低于 −40 dBFS。
	normGateMS = 1e-7
	// normRestart：捕获开始后的头 normTau 秒音频内，本帧功率超过估计的这么多倍（10 dB）即把此前计入
	// 的帧视为前导而丢弃，以本帧重新起步。合成节拍包络上最响的帧比平均功率高不到 8 dB，音乐自身的
	// 拍点不触发。
	normRestart = 10
	// normFloor：起步期内估计不低于本帧功率的这个比例（−3.01 dB），输出因此不高于目标 3.01 dB。
	// 取一半是两帧累计平均的最坏情形（一帧极低、一帧正常），不会让原本不过冲的序列变差。
	normFloor = 0.5
)

// smoothing 返回时长为 n 个样本的一帧并入指数滑动均值的系数 1 − e^(−n/tauN)，tauN 为时间常数折合的
// 样本数。系数按帧的时长取，响应速度因此与帧率无关，不足一帧的残帧权重相应变小。
func smoothing(n int, tauN float64) float64 {
	return -math.Expm1(-float64(n) / tauN)
}

// dbfs 把线性幅度换成 dBFS；对 <=ε 的输入（含 0、负数）钳到 ε 下限，结果恒有限。
func dbfs(x float64) float64 {
	return 20 * math.Log10(max(x, dbfsEpsilon))
}

// gainFactor 由响度估计 rmsEst 算线性增益：抬向 targetDBFS，但增幅封顶 maxGainDB。
func gainFactor(rmsEst float64, targetDBFS, maxGainDB int) float64 {
	return math.Pow(10, min(float64(targetDBFS)-dbfs(rmsEst), float64(maxGainDB))/20)
}

// Normalizer 持响度估计（RMS² 的均值），对逐帧 dsp.Result 施平滑增益。非并发安全，每路捕获各持一个。
//
// 时长一律以样本数计：tauN 是 normTau 折合的样本数，warmN、seenN 与它同单位、凡期限都与它比。
// warmN 不足 tauN 时为起步期：估计是已计入各帧功率按样本数加权的累计平均，并受 normFloor 与
// normRestart 保护；满了以后是时间常数为 normTau 的对称指数平均，两项保护不再生效。
type Normalizer struct {
	enabled    bool
	targetDBFS int
	maxGainDB  int
	tauN       float64 // normTau 折合的样本数
	emaMS      float64 // 响度估计：RMS² 的均值（均方）
	warmN      float64 // 本次起步以来计入的样本数
	seenN      float64 // 捕获开始以来计入的样本数，累计到 tauN 为止；为 0 即尚无估计
}

// newNormalizer 构造一路的归一化器，sampleRate 为采样率（把 normTau 折合成样本数）。
func newNormalizer(enabled bool, targetDBFS, maxGainDB, sampleRate int) *Normalizer {
	return &Normalizer{enabled: enabled, targetDBFS: targetDBFS, maxGainDB: maxGainDB, tauN: normTau * float64(sampleRate)}
}

// Apply 就地把增益施到 r（RMS 与每个 Bin），并返回该线性增益，供调用方乘到 peak 上。n 为本帧新增的
// 样本数，即该帧并入估计的时长权重（见 smoothing）。disabled 时不碰 r、不更新估计，返回 1。
//
// 以下几种帧不计入估计，也不计入 warmN 与 seenN；尚无估计时不碰 r、返回 1，已有估计时按当前估计照常
// 施加增益：
//   - RMS² 低于 normGateMS 的静音帧。捕获刚启动时的零数据与暂停期间未带静音标志的近零数据都属此类，
//     计入会把估计拉低，其后的正常电平即被放大成过冲。
//   - RMS² 为 NaN 或 ±Inf：并入 NaN 会使估计永久为 NaN，此后每帧的 RMS、bins、peak 都是 NaN；并入
//     +Inf 会使估计永久为 +Inf、增益为 0，波形永久静音。
//   - n 不为正：没有时长。
func (nz *Normalizer) Apply(r *dsp.Result, n int) float32 {
	if !nz.enabled {
		return 1
	}
	// ms >= normGateMS 为接受式：NaN 的比较为假，不计入。
	if ms, d := float64(r.RMS)*float64(r.RMS), float64(n); n > 0 && ms >= normGateMS && !math.IsInf(ms, 1) {
		switch {
		case nz.seenN < nz.tauN && (nz.seenN == 0 || ms > normRestart*nz.emaMS):
			nz.emaMS, nz.warmN = ms, d // 首个计入的帧，或头 normTau 秒内的重新起步：以本帧播种
		case nz.warmN < nz.tauN:
			nz.warmN += d
			nz.emaMS += max(smoothing(n, nz.tauN), d/nz.warmN) * (ms - nz.emaMS) // 累计平均，期满处与指数平均衔接
			nz.emaMS = max(nz.emaMS, normFloor*ms)
		default:
			nz.emaMS += smoothing(n, nz.tauN) * (ms - nz.emaMS)
		}
		nz.seenN = min(nz.seenN+d, nz.tauN)
	}
	if nz.seenN == 0 {
		return 1
	}
	g := float32(gainFactor(math.Sqrt(nz.emaMS), nz.targetDBFS, nz.maxGainDB))
	r.RMS *= g
	for i := range r.Bins {
		r.Bins[i] *= g
	}
	return g
}
