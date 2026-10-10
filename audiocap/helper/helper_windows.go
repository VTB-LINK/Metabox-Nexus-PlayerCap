//go:build windows

package helper

import (
	"bufio"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/wasapi"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const (
	// fftSize 是每路 DSP 的窗长（2 的幂）。1024 点 @48kHz ≈ 21ms。帧率高于 46 时每帧新增的样本不足
	// 一窗，由 framer 留着最近的非静音样本补足。
	fftSize = 1024

	// outChanCap 是「采集排空」与「写 stdout」解耦的有界 channel 容量。满时 drop-oldest，
	// 保证采集 goroutine 永不被 stdout 写阻塞冻住（§2.5）。~2KB/帧、64 帧上限 <128KB。
	outChanCap = 64

	// shutdownGrace 是 stdin EOF 后等待全部采集 goroutine 退出的上限。超时说明有 goroutine 卡在
	// 不返回的 COM 调用里，Run 直接退出进程；否则未赋入 Job Object 的 helper 会成为孤儿。
	shutdownGrace = 3 * time.Second
)

// epochCounter 是 helper 进程内计数器：每次成功 Activate 自增一次，作为该路本次激活的 epoch，首个为 1。
// helper 重启后重新从 1 开始，故帧流中的 epoch 不保证单调；断点靠 QPC 时间戳辅助区分（设计 §4）。
var epochCounter uint32

// zeroBins 是 keepalive（静音）帧用的全零幅度谱，长度与实帧一致，只读共享。
var zeroBins = make([]float32, fftSize/2)

// QPC：x/sys/windows 未封装 QueryPerformance*，照本仓库 wasapi 的 NewLazySystemDLL 范式取 kernel32。
var (
	kernel32     = windows.NewLazySystemDLL("kernel32.dll")
	procQPC      = kernel32.NewProc("QueryPerformanceCounter")
	procQPFreq   = kernel32.NewProc("QueryPerformanceFrequency")
	qpcFrequency = queryQPFreq() // 包初始化时采一次；此后只读
)

// capture 是一路捕获的控制柄。done 关闭即通知该路采集 goroutine 收尾。
type capture struct {
	done chan struct{}
}

// Run 是 helper 子进程的主循环：读 stdin 控制命令、驱动每路捕获、把帧写 stdout。阻塞直到
// stdin EOF（父进程关管道，防孤儿兜底）或读错，随后关停所有捕获、收尾写端、返回；采集 goroutine
// 在 shutdownGrace 内未全部退出时不返回，直接 os.Exit(0)。
//
// 并发拓扑：本 goroutine=控制/读 stdin；每个被 capture 的 player 一个采集 goroutine（各自
// LockOSThread+MTA，COM 亲和）；单个写 goroutine 从有界 channel 取帧写 stdout。三者经
// done channel、有界帧 channel、WaitGroup 编排关停；卡死不返回的采集 goroutine 由限时退出进程回收。
//
// 遥测无需传 DSN：DSN 是编译期 ldflags 注入的包级变量，helper 与主服务同一 exe → DSN 天然一致，
// telemetry.Init(Options{}) 已用对（helper 短路早于主服务 init，故 helper 自己 Init，见 §2.1）。
func Run(stdin io.Reader, stdout io.Writer) {
	// stdout 只写帧：把标准 log 钉死到 stderr，杜绝任何库（含 telemetry/logger，皆走 std log）
	// 经默认 log 污染帧流——stdout 污染会冲垮主服务分帧，是致命故障。
	log.SetOutput(os.Stderr)

	// helper 进程自己的遥测：允许 defer Guard 在 helper 内 re-panic 杀 helper（隔离在此进程，
	// 主服务由 Job Object/重启兜底）。Guard 无论 Init 与否都能安全 re-panic（未启用即零开销直抛）。
	telemetry.Init(telemetry.Options{})
	defer telemetry.Guard()

	frames := make(chan []byte, outChanCap)

	// 写 goroutine（单个）。挂 Guard：写端 panic 亦 re-panic 杀 helper（隔离）。
	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer telemetry.Guard()
		defer writeWG.Done()
		writeLoop(stdout, frames)
	}()

	// 控制 goroutine = 本 goroutine：逐行读 stdin。active 只被本 goroutine 读写 → 无需锁。
	var capWG sync.WaitGroup
	active := map[string]*capture{}

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		ctl, err := parseControl(scanner.Text())
		if err != nil {
			log.Printf("[helper] 忽略非法控制行: %v", err)
			continue
		}
		switch ctl.op {
		case "capture":
			if _, ok := active[ctl.player]; ok {
				continue // 该 player 已在捕获，幂等忽略
			}
			c := &capture{done: make(chan struct{})}
			active[ctl.player] = c
			capWG.Add(1)
			go func(ctl control, done <-chan struct{}) {
				defer telemetry.Guard()
				defer capWG.Done()
				captureLoop(ctl, done, frames)
			}(ctl, c.done)
		case "stop":
			if c, ok := active[ctl.player]; ok {
				close(c.done)
				delete(active, ctl.player)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[helper] stdin 读取结束/出错: %v", err)
	}

	// stdin EOF/错误 → 关停所有捕获、排空、干净收尾。
	// 关停顺序（防 send-on-closed-channel）：先关所有 done → 等所有采集 goroutine 退出
	//（此后再无人向/从 frames 收发）→ 才 close(frames) → 等写 goroutine 排空剩余帧退出。
	// 等待至多 shutdownGrace：超时即直接退出进程，且不 close(frames)——卡住的 goroutine 若恢复
	// 会向 frames 发送，关闭后发送即 panic。
	for _, c := range active {
		close(c.done)
	}
	if !waitTimeout(&capWG, shutdownGrace) {
		log.Printf("[helper] 关停超时：%v 内仍有采集 goroutine 未退出（可能卡在 COM 调用），直接退出进程", shutdownGrace)
		os.Exit(0)
	}
	close(frames)
	writeWG.Wait()
}

// writeLoop 把帧字节写到 stdout，直到 frames 关闭且排空，或写失败（父读端已关）。
// **只写帧、绝不写任何诊断**（诊断全走 stderr）。
//
// 背压解耦（§2.5）：写阻塞（父读端停顿、Windows 管道 ~64KB 满）只会卡住本 goroutine——采集
// 侧靠 frames 的有界+drop-oldest 永不被冻住，故这里用平凡阻塞写即可（“非阻塞”落在系统层：采集
// 不被阻塞）。父进程真死则 Write 报错，本 goroutine 退出，最终由 stdin EOF 触发整体关停。
func writeLoop(w io.Writer, frames <-chan []byte) {
	for f := range frames {
		if _, err := w.Write(f); err != nil {
			log.Printf("[helper] 写帧失败（父进程读端可能已关闭）: %v", err)
			return
		}
	}
}

// captureLoop 跑一路捕获的完整生命周期，全程在同一 LockOSThread + MTA 的 goroutine 上（wasapi
// 契约：Activate/Read/Close 须同线程 + MTA）。出帧循环在 pumpFrames：Read → 按样本数分帧（每
// 采样率/ctl.fps 个采样帧一帧）→ 分析编码 → drop-oldest 入队；静音也照发零帧（keepalive，使「无帧」
// 明确等价于故障而非静音），没有数据时按墙钟约每 100ms 一帧。done 关闭或读错即返回 → defer Close
// 对称释放。
//
// peak 的窗口为本帧新增的全部非静音样本（降混前按声道取峰），与 RMS、bins 只用最近至多 fftSize 个
// 降混样本不同：peak 要抓住瞬态，RMS 与 bins 受 FFT 窗长约束。
//
// ctl.norm/target/maxGain 为音量归一化参数：构造该路独立的 Normalizer（持独立 EMA），逐帧经
// encodeFrame 施于真音频帧（keepalive 零帧不经此路，不更新 EMA，避免静音后恢复过冲）。
func captureLoop(ctl control, done <-chan struct{}, frames chan []byte) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := coInitMTA(); err != nil {
		log.Printf("[helper][%s] CoInitializeEx 失败: %v", ctl.player, err)
		return
	}
	defer windows.CoUninitialize()

	// 若在起线程期间已被 stop（done 早关），跳过一次无谓的 Activate/Close。
	select {
	case <-done:
		return
	default:
	}

	c, err := wasapi.Activate(ctl.pid, true) // 含子进程树
	if err != nil {
		log.Printf("[helper][%s] WASAPI 激活失败(pid=%d): %v", ctl.player, ctl.pid, err)
		return // 这一路失败不崩整个 helper（其余路照跑）
	}
	defer c.Close()

	if err := pumpCapture(c.Read, time.Now, ctl, atomic.AddUint32(&epochCounter, 1), done, frames); err != nil {
		log.Printf("[helper][%s] 读取失败: %v", ctl.player, err)
	}
}

// pumpCapture 把 read 读到的包按 ctl 的参数分帧、分析、归一化、编码后入队 frames，直到 done 关闭
// （返回 nil）或读包出错（返回该错误）。captureLoop 在激活成功后以 Capturer.Read 与 time.Now 调用；
// 两者可注入，本函数不触碰 WASAPI。
func pumpCapture(read readFunc, now func() time.Time, ctl control, epoch uint32, done <-chan struct{}, frames chan []byte) error {
	analyzer := dsp.New(fftSize)
	nz := newNormalizer(ctl.norm, ctl.target, ctl.maxGain, wasapi.SampleRate) // 每路一个，持独立 EMA
	readBuf := make([]float32, wasapi.SampleRate*wasapi.Channels/10)          // 100ms 容量，避免 Read 截断
	fr := newFramer(hopFor(wasapi.SampleRate, ctl.fps), fftSize)
	emit := func(batch []float32, peaks [2]float32, fresh int) {
		enqueue(frames, encodeFrame(analyzer, nz, ctl.player, epoch, batch, peaks, fresh))
	}
	return pumpFrames(read, now, done, fr, readBuf, wasapi.Channels, time.Second/time.Duration(ctl.fps), emit)
}

// encodeFrame 用 batch 的**最近 fftSize 样本**算 RMS 与 bins，peaks 为本帧新增的全部非静音样本
// 按声道的峰值，fresh 为新增的样本数（归一化的时长权重），编码为独立字节（可安全跨 goroutine 传递：
// frame.Encode 已把 bins 拷入返回切片，返回后复用 analyzer 内部缓冲安全）。归一化增益同乘到 RMS、
// bins 与 peak；nz 启用时每帧（含 keepalive）置 FlagNormalized。batch 空（本帧没有非静音样本）→ 发
// keepalive 零帧：RMS、peak 为 0（不论传入的 peaks）、bins 全零。
func encodeFrame(a *dsp.Analyzer, nz *Normalizer, player string, epoch uint32, batch []float32, peaks [2]float32, fresh int) []byte {
	f := frame.Frame{
		Player:   player,
		Epoch:    epoch,
		TSMillis: frame.QPCMillis(readQPC(), qpcFrequency),
	}
	if nz.enabled {
		f.Flags = frame.FlagNormalized
	}
	if len(batch) > 0 {
		w := batch
		if len(w) > fftSize {
			w = w[len(w)-fftSize:] // 最近 fftSize 样本
		}
		res := a.Analyze(w)
		g := nz.Apply(&res, fresh) // 仅真音频帧施归一化（disabled 返回 1）；keepalive 分支不经此路，不更新 EMA
		f.RMS = res.RMS
		f.PeakL = peaks[0] * g
		f.PeakR = peaks[1] * g
		f.Bins = res.Bins
	} else {
		f.Bins = zeroBins // keepalive：静音零帧（RMS 与 peak 取零值）
	}
	return frame.Encode(nil, f)
}

// coInitMTA 在当前（已 LockOSThread 的）goroutine 上初始化 MTA COM。S_FALSE(0x1) 表示本线程已
// 初始化过，视作成功（照 tools/audioprobe 范式）。
func coInitMTA() error {
	err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
	if err != nil {
		if errno, ok := err.(syscall.Errno); !ok || uintptr(errno) != 0x1 {
			return err
		}
	}
	return nil
}

// readQPC 读一次 QueryPerformanceCounter 原始计数。QPC 是系统级单调源（开机起，不随 helper
// 进程重启归零）——helper 重启后时间戳不倒退，走带器可处理（§4）。
func readQPC() int64 {
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(&c)
	return c
}

// queryQPFreq 读一次 QueryPerformanceFrequency。QPC 频率开机后恒定。
func queryQPFreq() int64 {
	var f int64
	procQPFreq.Call(uintptr(unsafe.Pointer(&f)))
	runtime.KeepAlive(&f)
	return f
}
