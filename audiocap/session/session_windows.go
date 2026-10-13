//go:build windows

package session

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// endpointRefresh 是渲染端点列表与各端点会话管理器的缓存期限（设计 §4.4）：热插拔在这么久之内反映。
	endpointRefresh = 5 * time.Second

	clsctxAll          = 0x17
	eRender            = 0 // EDataFlow
	deviceStateActive  = 1 // DEVICE_STATE_ACTIVE
	sessionStateActive = 1 // AudioSessionStateActive

	// vtable 下标（前 3 为 IUnknown 的 QueryInterface/AddRef/Release）
	idxQueryInterface       = 0
	idxRelease              = 2
	idxEnumAudioEndpoints   = 3  // IMMDeviceEnumerator::EnumAudioEndpoints
	idxCollectionGetCount   = 3  // IMMDeviceCollection::GetCount
	idxCollectionItem       = 4  // IMMDeviceCollection::Item
	idxDeviceActivate       = 3  // IMMDevice::Activate
	idxGetSessionEnumerator = 5  // IAudioSessionManager2::GetSessionEnumerator
	idxSessionsGetCount     = 3  // IAudioSessionEnumerator::GetCount
	idxSessionsGetSession   = 4  // IAudioSessionEnumerator::GetSession
	idxGetState             = 3  // IAudioSessionControl::GetState
	idxGetProcessID         = 14 // IAudioSessionControl2::GetProcessId
	idxIsSystemSounds       = 15 // IAudioSessionControl2::IsSystemSoundsSession
)

// GUID / IID（windows.GUID 的字段布局与 C 的 GUID 一致）。取值由 guid_windows_test.go 对照规范字符串核对。
var (
	clsidMMDeviceEnumerator  = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator   = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioSessionManager2 = windows.GUID{Data1: 0x77AA99A0, Data2: 0x1BD6, Data3: 0x484F, Data4: [8]byte{0x8B, 0xC7, 0x2C, 0x65, 0x4C, 0x9A, 0x9B, 0x6F}}
	iidIAudioSessionControl2 = windows.GUID{Data1: 0xBFB7FF88, Data2: 0x7239, Data3: 0x4FC9, Data4: [8]byte{0x8F, 0xA2, 0x07, 0xC9, 0x50, 0xBE, 0x9C, 0x6D}}
)

var procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")

// COMEnumerator 经 IMMDeviceEnumerator 与各端点的 IAudioSessionManager2 枚举会话。只做同步 COM 调用，
// 不注册任何回调（设计 §3 方案 E）；只读，不开音频流。全部方法须在同一个已 LockOSThread 且已
// CoInitializeEx(MTA) 的 goroutine 上调用，非并发安全。用完调 Close。
//
// 缓存：端点列表与各端点的会话管理器每 endpointRefresh 重建一次，其间每拍复用；某端点枚举失败（如
// AUDCLNT_E_DEVICE_INVALIDATED）即丢弃它的会话管理器，到下次重建前该端点每拍报同一错误，本拍按不完整处理。
//
// shortcut: 重建时全部端点重新 Activate，不按端点 ID 复用会话管理器；实测每次重建的开销显著时，改为按
// IMMDevice::GetId 匹配复用。
type COMEnumerator struct {
	devEnum  unsafe.Pointer // IMMDeviceEnumerator；首次枚举时创建，失效时丢弃，下拍重建
	cached   []cachedEndpoint
	listedAt time.Time // 端点列表的建立时刻；零值即未建立
}

// cachedEndpoint 是一个渲染端点的会话管理器，或建立 / 使用它时的错误。
type cachedEndpoint struct {
	mgr unsafe.Pointer // IAudioSessionManager2；nil 时见 err
	err error
}

// NewCOMEnumerator 返回一个尚未做任何 COM 调用的枚举器。
func NewCOMEnumerator() *COMEnumerator { return &COMEnumerator{} }

func (e *COMEnumerator) enumerate() ([]endpoint, error) {
	if e.devEnum == nil {
		hr, _, _ := procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
			0,
			clsctxAll,
			uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
			uintptr(unsafe.Pointer(&e.devEnum)),
		)
		if failed(hr) {
			e.devEnum = nil
			return nil, hrError("CoCreateInstance(MMDeviceEnumerator)", hr)
		}
	}
	if e.listedAt.IsZero() || time.Since(e.listedAt) >= endpointRefresh {
		if err := e.refresh(); err != nil {
			return nil, err
		}
	}
	eps := make([]endpoint, len(e.cached))
	for i := range e.cached {
		c := &e.cached[i]
		if c.mgr == nil {
			eps[i].err = c.err
			continue
		}
		eps[i] = sessionsOf(c.mgr)
		if eps[i].err != nil {
			releaseUnknown(c.mgr)
			c.mgr, c.err = nil, eps[i].err
		}
	}
	return eps, nil
}

// refresh 丢弃缓存，重新列出活跃渲染端点并逐个 Activate 会话管理器。EnumAudioEndpoints 失败时连同设备
// 枚举器一起丢弃（如音频服务重启后旧对象失效），下拍重建；单个端点 Activate 失败记在该端点上。
func (e *COMEnumerator) refresh() error {
	e.releaseEndpoints()
	var coll unsafe.Pointer // IMMDeviceCollection
	if hr := comCall(e.devEnum, idxEnumAudioEndpoints, eRender, deviceStateActive, uintptr(unsafe.Pointer(&coll))); failed(hr) {
		releaseUnknown(e.devEnum)
		e.devEnum = nil
		return hrError("EnumAudioEndpoints", hr)
	}
	defer releaseUnknown(coll)
	var n uint32
	if hr := comCall(coll, idxCollectionGetCount, uintptr(unsafe.Pointer(&n))); failed(hr) {
		return hrError("IMMDeviceCollection.GetCount", hr)
	}
	cached := make([]cachedEndpoint, n)
	for i := range cached {
		cached[i].mgr, cached[i].err = activateSessionManager(coll, uint32(i))
	}
	e.cached, e.listedAt = cached, time.Now()
	return nil
}

// activateSessionManager 取集合里第 i 个端点的 IAudioSessionManager2。会话管理器自持引用，取到后即释放
// IMMDevice。
func activateSessionManager(coll unsafe.Pointer, i uint32) (unsafe.Pointer, error) {
	var dev unsafe.Pointer // IMMDevice
	if hr := comCall(coll, idxCollectionItem, uintptr(i), uintptr(unsafe.Pointer(&dev))); failed(hr) {
		return nil, hrError("IMMDeviceCollection.Item", hr)
	}
	defer releaseUnknown(dev)
	var mgr unsafe.Pointer
	if hr := comCall(dev, idxDeviceActivate, uintptr(unsafe.Pointer(&iidIAudioSessionManager2)), clsctxAll, 0, uintptr(unsafe.Pointer(&mgr))); failed(hr) {
		return nil, hrError("IMMDevice.Activate(IAudioSessionManager2)", hr)
	}
	return mgr, nil
}

// sessionsOf 枚举一个端点上的全部会话。
func sessionsOf(mgr unsafe.Pointer) endpoint {
	var senum unsafe.Pointer // IAudioSessionEnumerator
	if hr := comCall(mgr, idxGetSessionEnumerator, uintptr(unsafe.Pointer(&senum))); failed(hr) {
		return endpoint{err: hrError("GetSessionEnumerator", hr)}
	}
	defer releaseUnknown(senum)
	var n int32
	if hr := comCall(senum, idxSessionsGetCount, uintptr(unsafe.Pointer(&n))); failed(hr) {
		return endpoint{err: hrError("IAudioSessionEnumerator.GetCount", hr)}
	}
	sessions := make([]sessionInfo, max(n, 0))
	for i := range sessions {
		sessions[i] = sessionAt(senum, int32(i))
	}
	return endpoint{sessions: sessions}
}

// sessionAt 查询第 i 个会话的状态；只对 Active 的会话再取 PID 与是否系统声音，省去对大量 Inactive 会话的
// QueryInterface（设计 §4.4）。
func sessionAt(senum unsafe.Pointer, i int32) sessionInfo {
	var ctl unsafe.Pointer // IAudioSessionControl
	if hr := comCall(senum, idxSessionsGetSession, uintptr(i), uintptr(unsafe.Pointer(&ctl))); failed(hr) {
		return sessionInfo{err: hrError("GetSession", hr)}
	}
	defer releaseUnknown(ctl)
	var state int32 // AudioSessionState
	if hr := comCall(ctl, idxGetState, uintptr(unsafe.Pointer(&state))); failed(hr) {
		return sessionInfo{err: hrError("GetState", hr)}
	}
	if state != sessionStateActive {
		return sessionInfo{}
	}
	s := sessionInfo{active: true}
	var ctl2 unsafe.Pointer // IAudioSessionControl2
	if hr := comCall(ctl, idxQueryInterface, uintptr(unsafe.Pointer(&iidIAudioSessionControl2)), uintptr(unsafe.Pointer(&ctl2))); failed(hr) {
		s.pidErr = hrError("QueryInterface(IAudioSessionControl2)", hr)
		return s
	}
	defer releaseUnknown(ctl2)
	hrPid := comCall(ctl2, idxGetProcessID, uintptr(unsafe.Pointer(&s.pid)))
	s.system = comCall(ctl2, idxIsSystemSounds) == 0 // S_OK 为系统声音会话，S_FALSE 与失败按否
	s.pidErr = pidProblem(uint32(hrPid), s.system)   // 失败与跨进程会话（成功码 AUDCLNT_S_NO_SINGLE_PROCESS）都不能归属
	return s
}

// Close 释放全部缓存的 COM 对象。须在创建它们的线程上调用。
func (e *COMEnumerator) Close() {
	e.releaseEndpoints()
	releaseUnknown(e.devEnum)
	e.devEnum = nil
}

func (e *COMEnumerator) releaseEndpoints() {
	for _, c := range e.cached {
		releaseUnknown(c.mgr)
	}
	e.cached, e.listedAt = nil, time.Time{}
}

func failed(hr uintptr) bool { return uint32(hr)&0x80000000 != 0 }

func hrError(step string, hr uintptr) error { return fmt.Errorf("%s hr=0x%08X", step, uint32(hr)) }

// comCall 调 obj 接口 vtable 第 idx 个方法，this 作为隐式首参（同 audiocap/wasapi 的 comCall）。
//
// uintptrescapes：调用点就地写的 uintptr(unsafe.Pointer(&x)) 使 x 逃逸到堆并保活至本调用返回；先存进变量
// 再传则不受保护，栈增长后 COM 写出参会写到旧栈上。由 audiocap/wasapi/comcall_lint_test.go 一并守。
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	ret, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return ret
}

func releaseUnknown(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, idxRelease)
	}
}
