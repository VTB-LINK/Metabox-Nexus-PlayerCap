//go:build windows

// Package wasapi 提供可复用的进程级 WASAPI loopback 捕获器（从 tools/audioprobe 的可行性 spike 抽取）。
//
// 机制照 OBS(plugins/win-wasapi/win-wasapi.cpp) 与 VTB-TOOLS_IO(audio_capture.cpp) 两份 C++ 参考，
// 难点是把异步激活的手写 COM 回调（IActivateAudioInterfaceCompletionHandler 的 vtable）搬进纯 Go：
//   - 回调对象 = 一个「首字段是 vtable 指针」的 Go 结构体；C 侧把 this 当接口指针，本文件把 this
//     还原成 *completionHandler 取 per-instance 状态（event / 激活结果 / IAudioClient）。
//   - §0.4 铁律：NewCallback 只能在包级 var、闭包不得捕获局部；4 个回调全部包级 var，per-instance
//     状态一律经 this 指针传递，绝不闭包捕获。回调返回值必须 uintptr 大小（runtime.compileCallback 强制）。
//   - 生命周期：completion event 是 ref-counted 对象的成员，不是栈字段；超时放弃时只丢我们自己的
//     owner 引用而不清理，system 仍持引用，晚到的 SetEvent 落在存活对象上 —— 不会 UAF（照 VTB 参考）。
//   - GC 固定：Go GC 看不到 C 侧持有的引用，故把 handler 存进包级 map 钉住，避免被回收；Go 堆对象不
//     移动，地址稳定，可安全交给 C。引用计数归零时解钉 + 关 event。
//
// 使用契约（承重）：
//   - COM/线程：Activate/Read/Close 必须在同一个 已 runtime.LockOSThread 且已 CoInitializeEx(MTA)
//     的 goroutine 上调用；本包不管理线程与 COM 初始化（策略留给调用方，见 audioprobe / helper）。
//   - 并发：单个 *Capturer 非并发安全。Read 与 Close 不可并发调用——调用方应先停止调用 Read、
//     再 Close。Read 的等待有 100ms 上限，停止只需让循环自然返回。
//   - 反复 Activate/Close 对称释放：每次成功 Activate 都在同一次调用内归还 completion handler 的
//     owner 引用并解 pin map（见 activateProcessLoopbackClient 成功路径）；Close 归还捕获侧资源
//     （Stop + Release IAudioClient/IAudioCaptureClient + CloseHandle 采集事件）。二者合起来保证
//     长命 helper 里反复激活/关闭不泄漏句柄/COM 引用/pin map 条目。
package wasapi

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// 公开常量：进程 loopback 虚拟设备格式固定 48000Hz/2ch/float32
// （GetMixFormat 返回 E_NOTIMPL，引擎把播放器侧任意采样率/位深/声道转成这个格式）。
// ---------------------------------------------------------------------------

const (
	SampleRate = 48000 // 采样率（Hz）
	Channels   = 2     // 声道数（交织）
)

// ---------------------------------------------------------------------------
// 内部常量
// ---------------------------------------------------------------------------

const (
	captureBits = 32 // 每样本位深（float32）

	// 200ms 共享模式缓冲（100ns 单位）。事件驱动下事件按引擎周期(~10ms)触发，缓冲只是环容量；
	// OBS 用 5s、VTB 用 30ms，取中间值即可，每 ≤100ms 排空一次不会溢出。
	captureBufDur100ns = 2_000_000

	// readTimeoutMs：单次 Read 里 WaitForSingleObject 的上限（毫秒）。事件驱动下引擎约每 10ms 触发；
	// 设 100ms 上限使 Read 在长时间静音（进程不出声、引擎不推缓冲）时也能定期返回 0 帧，
	// 让调用方循环得以检查停止信号、按墙钟发 keepalive，不至于永久阻塞在事件上。
	readTimeoutMs = 100
)

const (
	audioclientActivationTypeProcessLoopback = 1
	processLoopbackModeIncludeTree           = 0 // PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE
	processLoopbackModeExcludeTree           = 1 // PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE

	vtBlob = 0x0041 // VT_BLOB (=65)

	audclntSharemodeShared          = 0
	audclntStreamflagsLoopback      = 0x00020000
	audclntStreamflagsEventCallback = 0x00040000
	audclntBufferflagsSilent        = 0x2

	wfmtExtensible = 0xFFFE // WAVE_FORMAT_EXTENSIBLE

	sOK          = 0
	eNoInterface = 0x80004002
	ePointer     = 0x80004003
	eFail        = 0x80004005

	// vtable 偏移（前 3 为 IUnknown 的 QueryInterface/AddRef/Release）
	idxRelease           = 2
	idxAudioClientInit   = 3  // IAudioClient::Initialize
	idxAudioClientStart  = 10 // IAudioClient::Start
	idxAudioClientStop   = 11 // IAudioClient::Stop
	idxAudioClientSetEvt = 13 // IAudioClient::SetEventHandle
	idxAudioClientGetSvc = 14 // IAudioClient::GetService
	idxAsyncOpGetResult  = 3  // IActivateAudioInterfaceAsyncOperation::GetActivateResult
	idxCaptureGetBuffer  = 3  // IAudioCaptureClient::GetBuffer
	idxCaptureRelBuffer  = 4  // IAudioCaptureClient::ReleaseBuffer
	idxCaptureNextPacket = 5  // IAudioCaptureClient::GetNextPacketSize
)

// 设备路径不是 GUID，是宽字符串 VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK。
const virtualAudioDeviceProcessLoopback = `VAD\Process_Loopback`

// GUID / IID（小端字节序：windows.GUID 的 Data1..Data4 布局与 C 的 GUID 一致，直接按字段拆分）。
var (
	iidIAudioClient        = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidIActivateHandler    = windows.GUID{Data1: 0x41D949AB, Data2: 0x9862, Data3: 0x444A, Data4: [8]byte{0x80, 0xF6, 0xC2, 0x61, 0x33, 0x4D, 0xA5, 0xEB}}
	iidIUnknown            = windows.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	// IAgileObject：标记接口（无自有方法），我们的对象天然线程无关（无 apartment 亲和），如实应答 S_OK
	// 以满足 ActivateAudioInterfaceAsync 可能的 agility 检查，避免走 marshaling 路径。
	iidIAgileObject    = windows.GUID{Data1: 0x94EA2B94, Data2: 0xE9CC, Data3: 0x49E0, Data4: [8]byte{0xC0, 0xFF, 0xEE, 0x64, 0xCA, 0x8F, 0x5B, 0x90}}
	ksSubtypeIEEEFloat = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
)

// ---------------------------------------------------------------------------
// C 结构体的精确复刻（按 C 布局手排偏移）
// ---------------------------------------------------------------------------

// AUDIOCLIENT_ACTIVATION_PARAMS：ActivationType(enum,4) + union{ ProcessLoopbackParams{ TargetProcessId(4),
// ProcessLoopbackMode(enum,4) } }。union 只有一个成员，扁平化为三个 uint32；sizeof=12。
type audioclientActivationParams struct {
	activationType      uint32 // 0
	targetProcessID     uint32 // 4  (union: ProcessLoopbackParams.TargetProcessId)
	processLoopbackMode uint32 // 8  (union: ProcessLoopbackParams.ProcessLoopbackMode)
}

// PROPVARIANT（x64）：vt(2) + 3×WORD 保留(6) = 8 字节头，union 从偏移 8 起（8 字节对齐）。
// 这里只用 BLOB{ cbSize(ULONG,@8) + 4 字节 pad + pBlobData(BYTE*,@16) }；sizeof=24。
type propVariant struct {
	vt        uint16  // 0
	_         uint16  // 2  wReserved1
	_         uint16  // 4  wReserved2
	_         uint16  // 6  wReserved3
	cbSize    uint32  // 8  blob.cbSize
	_         uint32  // 12 pad，令 pBlobData 8 字节对齐
	pBlobData uintptr // 16 blob.pBlobData
}

// WAVEFORMATEXTENSIBLE 扁平手排：C 里 WAVEFORMATEX 恰 18 字节，但 Go 单独定义会因 uint32 对齐被 pad 到
// 20 字节，嵌入就会错位。故整体扁平化，逐字段钉死偏移；sizeof=40，cbSize 恒 22（=40-18）。
type waveFormatExtensible struct {
	wFormatTag      uint16       // 0
	nChannels       uint16       // 2
	nSamplesPerSec  uint32       // 4
	nAvgBytesPerSec uint32       // 8
	nBlockAlign     uint16       // 12
	wBitsPerSample  uint16       // 14
	cbSize          uint16       // 16
	wValidBits      uint16       // 18 Samples.wValidBitsPerSample
	dwChannelMask   uint32       // 20
	subFormat       windows.GUID // 24 (16B) → sizeof 40
}

// ---------------------------------------------------------------------------
// COM 回调对象：IActivateAudioInterfaceCompletionHandler
// ---------------------------------------------------------------------------

type completionHandlerVtbl struct {
	queryInterface    uintptr
	addRef            uintptr
	release           uintptr
	activateCompleted uintptr
}

// completionHandler：首字段必须是 vtable 指针 —— C 侧把本结构体地址当接口指针，读首字得到 vtable。
type completionHandler struct {
	vtbl     *completionHandlerVtbl
	refCount int32
	event    windows.Handle // 完成事件：本对象成员，非栈字段（超时放弃后晚到的 SetEvent 才有合法落点）
	result   uintptr        // 激活 HRESULT
	client   unsafe.Pointer // IAudioClient*（激活成功后由回调填入）
}

// 4 个回调：包级 var + 无捕获（只引用包级类型/函数）+ per-instance 状态经 this 传递。参数用「有类型指针」
// 而非 uintptr，从而 unsafe.Pointer(this) 是「指针→指针」转换、不触发 go vet 的 uintptr→Pointer 告警。
var completionHandlerQueryInterface = syscall.NewCallback(func(this *completionHandler, riid *windows.GUID, ppv *uintptr) uintptr {
	if ppv == nil {
		return ePointer
	}
	if riid != nil && (*riid == iidIUnknown || *riid == iidIActivateHandler || *riid == iidIAgileObject) {
		*ppv = uintptr(unsafe.Pointer(this)) // 存 uintptr（非指针）到调用方内存：无写屏障；对象由 pin map 保活
		handlerAddRef(this)
		return sOK
	}
	*ppv = 0
	return eNoInterface
})

var completionHandlerAddRef = syscall.NewCallback(func(this *completionHandler) uintptr {
	return handlerAddRef(this)
})

var completionHandlerRelease = syscall.NewCallback(func(this *completionHandler) uintptr {
	return handlerRelease(this)
})

var completionHandlerActivateCompleted = syscall.NewCallback(func(this *completionHandler, op unsafe.Pointer) uintptr {
	var hrActivate uint32  // GetActivateResult 第 1 出参是 HRESULT*（4 字节）
	var unk unsafe.Pointer // 第 2 出参是 IUnknown**；C 写入，无 Go 写屏障
	hr := comCall(op, idxAsyncOpGetResult, uintptr(unsafe.Pointer(&hrActivate)), uintptr(unsafe.Pointer(&unk)))
	if failed(hr) {
		this.result = hr
	} else {
		this.result = uintptr(hrActivate)
		if !failed(uintptr(hrActivate)) {
			this.client = unk // 请求的就是 IID_IAudioClient，直接接管这个引用
		} else if unk != nil {
			releaseUnknown(unk)
		}
	}
	if this.event != 0 {
		windows.SetEvent(this.event)
	}
	return sOK
})

// vtable 实例：包级 var，地址恒定、永不移动。
var completionHandlerVtblInstance = completionHandlerVtbl{
	queryInterface:    completionHandlerQueryInterface,
	addRef:            completionHandlerAddRef,
	release:           completionHandlerRelease,
	activateCompleted: completionHandlerActivateCompleted,
}

func handlerAddRef(h *completionHandler) uintptr {
	return uintptr(uint32(atomic.AddInt32(&h.refCount, 1)))
}

// handlerRelease：引用计数归零时（恰一次）解除 GC 固定并关闭 event。
func handlerRelease(h *completionHandler) uintptr {
	n := atomic.AddInt32(&h.refCount, -1)
	if n == 0 {
		unpinHandler(h)
		if h.event != 0 {
			windows.CloseHandle(h.event)
			h.event = 0
		}
	}
	return uintptr(uint32(n))
}

// pin map：持有 *completionHandler 的 Go 指针，阻止 GC 回收（C 侧的引用 GC 不可见）。
var (
	pinMu  sync.Mutex
	pinned = map[*completionHandler]struct{}{}
)

func pinHandler(h *completionHandler) {
	pinMu.Lock()
	pinned[h] = struct{}{}
	pinMu.Unlock()
}

func unpinHandler(h *completionHandler) {
	pinMu.Lock()
	delete(pinned, h)
	pinMu.Unlock()
}

func newCompletionHandler() (*completionHandler, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil) // manual-reset, 初始非信号
	if err != nil && ev == 0 {
		return nil, fmt.Errorf("CreateEvent(完成事件) 失败: %w", err)
	}
	h := &completionHandler{
		vtbl:     &completionHandlerVtblInstance,
		refCount: 1, // owner 引用
		event:    ev,
		result:   eFail,
	}
	pinHandler(h) // 取地址存入包级 map → 逃逸到堆、地址稳定、免被 GC 回收
	return h, nil
}

// ---------------------------------------------------------------------------
// 通用 COM 调用助手
// ---------------------------------------------------------------------------

func failed(hr uintptr) bool { return uint32(hr)&0x80000000 != 0 }

// comCall 调 obj 接口 vtable 第 idx 个方法，this 作为隐式首参。obj 是 unsafe.Pointer（指向 C 内存），
// *(**[64]uintptr)(obj) 先解一层得到 vtable 基址再索引 —— 全程无 uintptr→Pointer 转换。
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	ret, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return ret
}

func releaseUnknown(obj unsafe.Pointer) uintptr {
	if obj == nil {
		return 0
	}
	return comCall(obj, idxRelease)
}

// ---------------------------------------------------------------------------
// 异步激活 → 同步拿到 IAudioClient
// ---------------------------------------------------------------------------

// activateProcessLoopbackClient 对目标 PID（连/不连子进程树）异步激活进程 loopback 虚拟设备，等 2s 转同步，
// 返回 IAudioClient*（unsafe.Pointer，调用方负责 Release）。低于 Win10 build 19041 拿不到 API → 优雅报错。
func activateProcessLoopbackClient(pid uint32, includeTree bool) (unsafe.Pointer, error) {
	mmdev := windows.NewLazySystemDLL("Mmdevapi.dll")
	proc := mmdev.NewProc("ActivateAudioInterfaceAsync")
	if err := proc.Find(); err != nil {
		return nil, fmt.Errorf("ActivateAudioInterfaceAsync 不可用（需 Windows 10 build 19041+）: %w", err)
	}

	mode := uint32(processLoopbackModeIncludeTree)
	if !includeTree {
		mode = processLoopbackModeExcludeTree
	}
	params := audioclientActivationParams{
		activationType:      audioclientActivationTypeProcessLoopback,
		targetProcessID:     pid,
		processLoopbackMode: mode,
	}
	pv := propVariant{
		vt:        vtBlob,
		cbSize:    uint32(unsafe.Sizeof(params)),
		pBlobData: uintptr(unsafe.Pointer(&params)),
	}
	devPath, err := windows.UTF16PtrFromString(virtualAudioDeviceProcessLoopback)
	if err != nil {
		return nil, err
	}

	h, err := newCompletionHandler()
	if err != nil {
		return nil, err
	}

	var asyncOp unsafe.Pointer
	ret, _, _ := proc.Call(
		uintptr(unsafe.Pointer(devPath)),
		uintptr(unsafe.Pointer(&iidIAudioClient)),
		uintptr(unsafe.Pointer(&pv)),
		uintptr(unsafe.Pointer(h)),
		uintptr(unsafe.Pointer(&asyncOp)),
	)
	// proc.Call 是普通 Go 函数（非 syscall 的编译器特例），不会自动延长这些 Go 指针实参指向对象的存活期；
	// pv.pBlobData 又以 uintptr 持有 &params（GC 不可见）。故对同步段被消费的 blob/设备路径显式保活至此。
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&pv)
	runtime.KeepAlive(devPath)

	if asyncOp != nil {
		releaseUnknown(asyncOp) // 完成经 handler.event 跟踪，不用操作对象
	}
	if failed(ret) {
		handlerRelease(h) // 同步失败：不会再有回调，安全释放 owner 引用
		return nil, fmt.Errorf("ActivateAudioInterfaceAsync 失败: 0x%08X", uint32(ret))
	}

	// 异步转同步：等 2s。超时则 ABANDON —— 不释放 owner 引用（对象连同 event 泄漏），system 仍持引用，
	// 晚到的 ActivateCompleted 落在存活对象上，绝不 UAF。
	wr, _ := windows.WaitForSingleObject(h.event, 2000)
	if wr != windows.WAIT_OBJECT_0 {
		return nil, fmt.Errorf("激活超时（2s 未完成，已放弃并泄漏 handler 以避免 UAF；wait=0x%X）", wr)
	}
	if failed(h.result) {
		handlerRelease(h)
		return nil, fmt.Errorf("激活结果失败: 0x%08X", uint32(h.result))
	}
	client := h.client
	h.client = nil    // 所有权转移给调用方
	handlerRelease(h) // 释放 owner 引用（成功路径：completion handler 与其 event 在此归零回收）
	if client == nil {
		return nil, fmt.Errorf("激活成功但 IAudioClient 为空")
	}
	return client, nil
}

// ---------------------------------------------------------------------------
// Capturer：可复用的进程 loopback 捕获器
// ---------------------------------------------------------------------------

// Capturer 持有一路已激活并启动的进程 loopback 捕获。字段只在 Activate 内填充、Close 内清空。
// completion handler 不在此持有：它在 Activate 成功路径内即经引用计数归零而回收（见
// activateProcessLoopbackClient），激活期一过便与本对象无关。
type Capturer struct {
	client        unsafe.Pointer // IAudioClient*
	captureClient unsafe.Pointer // IAudioCaptureClient*
	event         windows.Handle // 采集事件（auto-reset，SetEventHandle 绑定）
}

// Activate 对 pid（含/不含子进程树）激活进程 loopback，按固定 48k/2ch/f32 初始化并启动采集，返回就绪的
// Capturer。任一步失败都对已获资源做对称释放后返回错误（不留半成品）。
//
// 调用前提见包注释：须在已 LockOSThread 且 CoInitializeEx(MTA) 的 goroutine 上调用。
func Activate(pid uint32, includeTree bool) (*Capturer, error) {
	client, err := activateProcessLoopbackClient(pid, includeTree)
	if err != nil {
		return nil, err
	}
	c := &Capturer{client: client}
	// 自此任何失败都走 c.Close() 归还已获资源（Close 幂等、按字段守卫，半成品也安全）。

	// 进程 loopback 虚拟设备 GetMixFormat 返回 E_NOTIMPL，格式硬编码 48k/2ch/32-bit float。
	blockAlign := uint16(Channels * captureBits / 8)
	format := waveFormatExtensible{
		wFormatTag:      wfmtExtensible,
		nChannels:       Channels,
		nSamplesPerSec:  SampleRate,
		nAvgBytesPerSec: SampleRate * uint32(blockAlign),
		nBlockAlign:     blockAlign,
		wBitsPerSample:  captureBits,
		cbSize:          22, // sizeof(WAVEFORMATEXTENSIBLE) - sizeof(WAVEFORMATEX) = 40 - 18
		wValidBits:      captureBits,
		dwChannelMask:   0x3, // KSAUDIO_SPEAKER_STEREO = FRONT_LEFT|FRONT_RIGHT
		subFormat:       ksSubtypeIEEEFloat,
	}

	// Initialize(SHARED, EVENTCALLBACK|LOOPBACK, bufDur, 0, &format, NULL)
	flags := uintptr(audclntStreamflagsEventCallback | audclntStreamflagsLoopback)
	hr := comCall(client, idxAudioClientInit,
		uintptr(audclntSharemodeShared),
		flags,
		uintptr(int64(captureBufDur100ns)),
		0,
		uintptr(unsafe.Pointer(&format)),
		0,
	)
	runtime.KeepAlive(&format) // format 以 uintptr 传入，保活至调用返回
	if failed(hr) {
		c.Close()
		return nil, fmt.Errorf("IAudioClient.Initialize 失败: 0x%08X", uint32(hr))
	}

	// 采集事件（auto-reset），SetEventHandle 绑定。
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil && ev == 0 {
		c.Close()
		return nil, fmt.Errorf("CreateEvent(采集) 失败: %w", err)
	}
	c.event = ev
	if hr := comCall(client, idxAudioClientSetEvt, uintptr(ev)); failed(hr) {
		c.Close()
		return nil, fmt.Errorf("SetEventHandle 失败: 0x%08X", uint32(hr))
	}

	// GetService(IID_IAudioCaptureClient)
	var capClient unsafe.Pointer
	if hr := comCall(client, idxAudioClientGetSvc,
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capClient)),
	); failed(hr) || capClient == nil {
		c.Close()
		return nil, fmt.Errorf("GetService(IAudioCaptureClient) 失败: 0x%08X", uint32(hr))
	}
	c.captureClient = capClient

	if hr := comCall(client, idxAudioClientStart); failed(hr) {
		c.Close()
		return nil, fmt.Errorf("IAudioClient.Start 失败: 0x%08X", uint32(hr))
	}
	return c, nil
}

// Read 等一次采集事件（上限 readTimeoutMs）后取「一个」packet，把交织 f32 写入 dst，返回写入帧数、
// 该 packet 是否静音、错误。
//
// dst 容量语义（承重，务必看）：
//   - Read 每次至多消费一个 WASAPI packet（事件驱动共享模式下即一个引擎周期，约 10ms，48k/2ch 下
//     约 480 帧 = 960 个 float32），且一旦 GetBuffer 取出就必须整包 ReleaseBuffer。
//   - 若 len(dst) 装不下本 packet，超出部分被丢弃（tail 截断），返回的 frames 只反映实际写入 dst 的
//     帧数——但整个 packet 已被消费，不会在下次 Read 再出现。返回帧数按整帧对齐（丢弃半帧）。
//   - 故调用方必须给足够大的 dst：建议 len(dst) >= SampleRate*Channels/10（100ms），远大于单个引擎
//     周期，实践中截断永不发生。
//
// 返回 (0, false, nil) 表示本次无数据（事件超时或引擎暂无新 packet）——静音/前奏/暂停时的正常情形；
// keepalive 补零策略在调用方，不在此。
func (c *Capturer) Read(dst []float32) (frames int, silent bool, err error) {
	if c == nil || c.captureClient == nil {
		return 0, false, fmt.Errorf("Read 在已关闭的 Capturer 上调用")
	}
	// 超时也继续尝试取包（drain-regardless，不完全信任事件；返回值无需处理）。
	windows.WaitForSingleObject(c.event, readTimeoutMs)

	var packet uint32
	if hr := comCall(c.captureClient, idxCaptureNextPacket, uintptr(unsafe.Pointer(&packet))); failed(hr) {
		return 0, false, fmt.Errorf("GetNextPacketSize 失败: 0x%08X", uint32(hr))
	}
	if packet == 0 {
		return 0, false, nil
	}

	var pData unsafe.Pointer
	var numFrames, bufFlags uint32
	if hr := comCall(c.captureClient, idxCaptureGetBuffer,
		uintptr(unsafe.Pointer(&pData)),
		uintptr(unsafe.Pointer(&numFrames)),
		uintptr(unsafe.Pointer(&bufFlags)),
		0, 0,
	); failed(hr) {
		return 0, false, fmt.Errorf("GetBuffer 失败: 0x%08X", uint32(hr))
	}

	silent = bufFlags&audclntBufferflagsSilent != 0
	n := int(numFrames) * Channels
	if n > len(dst) {
		n = len(dst)
	}
	n -= n % Channels // 对齐到整帧，避免 dst 容量不整时残留半帧
	if silent {
		clear(dst[:n]) // 静音包 WASAPI 可不填 pData；按零写出以保持时间线
	} else if n > 0 {
		src := unsafe.Slice((*float32)(pData), int(numFrames)*Channels)
		copy(dst[:n], src[:n])
	}
	// 整包归还（必须等于 GetBuffer 给的 numFrames）——即使 dst 只装下一部分。
	comCall(c.captureClient, idxCaptureRelBuffer, uintptr(numFrames))
	return n / Channels, silent, nil
}

// Close 对称释放捕获侧资源：Stop 流 → Release IAudioCaptureClient → Release IAudioClient →
// CloseHandle 采集事件，逐项置 nil/0。按字段守卫、可安全多次调用（幂等），半成品状态亦安全。
//
// completion handler 不在此释放：它已在 Activate 成功路径内经引用计数归零而回收。
func (c *Capturer) Close() error {
	if c == nil {
		return nil
	}
	if c.client != nil {
		comCall(c.client, idxAudioClientStop) // 停流（未 Start / 未 Initialize 也安全，返回码忽略）
	}
	if c.captureClient != nil {
		releaseUnknown(c.captureClient)
		c.captureClient = nil
	}
	if c.client != nil {
		releaseUnknown(c.client)
		c.client = nil
	}
	if c.event != 0 {
		windows.CloseHandle(c.event)
		c.event = 0
	}
	return nil
}
