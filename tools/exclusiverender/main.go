//go:build ignore

// exclusiverender 在指定的渲染端点上开一条 WASAPI 独占模式的静音流，保持数秒后关闭。用于 issue #115 的真机取证：
// 独占流是否出现在共享模式的音频会话枚举里（配合 tools/sessionprobe 观察）。
//
// 不发出任何声音：缓冲区全程填 AUDCLNT_BUFFERFLAGS_SILENT 并清零。
// 安全：硬拒绝用户正在使用的端点（RME ADAT 01+02、默认的 Realtek 扬声器、RME 耳机口）；只接受命令行给出的端点 GUID。
//
// 用法：go run ./tools/exclusiverender/main.go <端点 GUID，不含花括号> [保持秒数，默认 6] [音调 dBFS，如 -50；不给则全程静音]
//
// 给了音调幅度时，在 16 位或 32 位容器的格式上输出 440 Hz 正弦（幅度很低，且目标是没接设备的 ADAT 对），
// 用于观察独占流的会话峰值与进程 loopback 能否捕到。
package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:uintptrescapes
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	ret, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return ret
}

//go:uintptrescapes
func coCreate(clsid, iid, out uintptr) uintptr {
	r, _, _ := syscall.SyscallN(procCoCreateInstance.Addr(), clsid, 0, clsctxAll, iid, out)
	return r
}

const clsctxAll = 0x17

var procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")

func release(p unsafe.Pointer) {
	if p != nil {
		comCall(p, 2)
	}
}

func failed(hr uintptr) bool { return uint32(hr)&0x80000000 != 0 }

func mustGUID(s string) *windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return &g
}

var (
	clsidMMDeviceEnumerator = mustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidMMDeviceEnumerator   = mustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidAudioClient          = mustGUID("{1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}")
	iidAudioRenderClient    = mustGUID("{F294ACFC-3146-4483-A7BF-ADDCA7C260E2}")
	iidMeterInformation     = mustGUID("{C02216F6-8C67-4B5B-9D00-D008E73E0064}")
	subtypePCM              = mustGUID("{00000001-0000-0010-8000-00AA00389B71}")
)

// 只允许名称含 allowedPair 的端点；名称含下列任一子串的端点一律拒绝（用户正在用）。
// 不按 GUID 判：RME 驱动切换模式时整批端点换新 GUID（2026-10-13 实测），GUID 黑名单会失效。
const allowedPair = "29+30"

var forbiddenNames = []string{"1+2", "01+02", "Phones", "Realtek", "扬声器", "Speakers"}

type waveFormatExtensible struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
	ValidBits      uint16
	ChannelMask    uint32
	SubFormat      windows.GUID
}

func newFormat(rate uint32, container, valid uint16) waveFormatExtensible {
	const channels = 2
	return waveFormatExtensible{
		FormatTag:      0xFFFE, // WAVE_FORMAT_EXTENSIBLE
		Channels:       channels,
		SamplesPerSec:  rate,
		AvgBytesPerSec: rate * channels * uint32(container/8),
		BlockAlign:     channels * container / 8,
		BitsPerSample:  container,
		CbSize:         22,
		ValidBits:      valid,
		ChannelMask:    3, // FL | FR
		SubFormat:      *subtypePCM,
	}
}

// friendly 读端点的显示名与设备状态（DeviceState：1 = 活跃）。注册表只读。
func friendly(guid string) (name string, state uint64) {
	const root = `SOFTWARE\Microsoft\Windows\CurrentVersion\MMDevices\Audio\Render\{`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, root+guid+`}`, registry.QUERY_VALUE)
	if err != nil {
		return "(注册表无此端点)", 0
	}
	state, _, _ = k.GetIntegerValue("DeviceState")
	k.Close()
	p, err := registry.OpenKey(registry.LOCAL_MACHINE, root+guid+`}\Properties`, registry.QUERY_VALUE)
	if err != nil {
		return "(无属性)", state
	}
	defer p.Close()
	a, _, _ := p.GetStringValue(`{b3f8fa53-0004-438e-9003-51a46e139bfc},6`)
	b, _, _ := p.GetStringValue(`{a45c254e-df1c-4efd-8020-67d146a850e0},2`)
	return a + " / " + b, state
}

func logf(start time.Time, format string, a ...any) {
	fmt.Printf("[%5.2fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, a...))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: exclusiverender <端点 GUID> [保持秒数]")
		os.Exit(2)
	}
	target := strings.ToLower(strings.Trim(os.Args[1], "{}"))
	hold := 6
	if len(os.Args) > 2 {
		hold, _ = strconv.Atoi(os.Args[2])
	}
	if hold < 1 || hold > 15 {
		fmt.Println("保持秒数须在 1..15（RME 蓝屏先例：短跑）")
		os.Exit(2)
	}
	toneAmp := 0.0
	if len(os.Args) > 3 {
		db, err := strconv.ParseFloat(os.Args[3], 64)
		if err != nil || db > -40 || db < -100 {
			fmt.Println("音调幅度须在 -100..-40 dBFS（避免任何可闻或过大的输出）")
			os.Exit(2)
		}
		toneAmp = math.Pow(10, db/20)
	}
	name, state := friendly(target)
	fmt.Printf("目标端点 {%s} = %s（DeviceState=%d）\n", target, name, state)
	for _, bad := range forbiddenNames {
		if strings.Contains(name, bad) {
			fmt.Printf("拒绝：名称含 %q，是用户正在用的端点\n", bad)
			os.Exit(2)
		}
	}
	if !strings.Contains(name, "ADAT") || !strings.Contains(name, allowedPair) {
		fmt.Printf("拒绝：只允许名称含 ADAT 与 %q 的端点\n", allowedPair)
		os.Exit(2)
	}
	if state != 1 {
		fmt.Println("拒绝：该端点当前不是活跃状态（DeviceState != 1），可能已被驱动换新 GUID，请重新取 GUID")
		os.Exit(2)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	start := time.Now()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		fmt.Println("CoInitializeEx:", err)
		return
	}
	defer windows.CoUninitialize()

	var enum unsafe.Pointer
	if hr := coCreate(uintptr(unsafe.Pointer(clsidMMDeviceEnumerator)), uintptr(unsafe.Pointer(iidMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); failed(hr) {
		fmt.Printf("CoCreateInstance hr=%#x\n", uint32(hr))
		return
	}
	defer release(enum)

	// 按端点 ID 取设备：渲染端点 ID 形如 {0.0.0.00000000}.{guid}
	id, err := windows.UTF16PtrFromString(`{0.0.0.00000000}.{` + target + `}`)
	if err != nil {
		fmt.Println(err)
		return
	}
	var dev unsafe.Pointer
	// IMMDeviceEnumerator::GetDevice(pwstrId, &dev)
	if hr := comCall(enum, 5, uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(&dev))); failed(hr) {
		logf(start, "GetDevice 失败 hr=%#x", uint32(hr))
		return
	}
	defer release(dev)

	var client unsafe.Pointer
	// IMMDevice::Activate(IID_IAudioClient, CLSCTX_ALL, NULL, &client)
	if hr := comCall(dev, 3, uintptr(unsafe.Pointer(iidAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&client))); failed(hr) {
		logf(start, "Activate(IAudioClient) 失败 hr=%#x", uint32(hr))
		return
	}
	// 闭包读 client 的当前值：下面对齐重试会换掉它，按值 defer 会重复释放旧接口、漏释放新接口。
	defer func() { release(client) }()

	// 挑一个设备支持的独占格式。
	var chosen *waveFormatExtensible
	candidates := []struct {
		rate        uint32
		container   uint16
		valid       uint16
		description string
	}{
		{48000, 32, 24, "48k/24bit(32位容器)"}, {48000, 24, 24, "48k/24bit"}, {48000, 16, 16, "48k/16bit"},
		{48000, 32, 32, "48k/32bit"}, {44100, 16, 16, "44.1k/16bit"}, {96000, 32, 24, "96k/24bit(32位容器)"},
	}
	for _, c := range candidates {
		f := newFormat(c.rate, c.container, c.valid)
		hr := comCall(client, 7, 1, uintptr(unsafe.Pointer(&f)), 0) // IsFormatSupported(EXCLUSIVE, &f, NULL)
		logf(start, "IsFormatSupported(独占, %s) hr=%#x", c.description, uint32(hr))
		if hr == 0 {
			chosen = &f
			break
		}
	}
	if chosen == nil {
		logf(start, "设备没有可用的独占格式（可能该设备在 Windows 声音设置里禁用了独占），放弃")
		return
	}

	var defPeriod, minPeriod int64
	if hr := comCall(client, 9, uintptr(unsafe.Pointer(&defPeriod)), uintptr(unsafe.Pointer(&minPeriod))); failed(hr) { // GetDevicePeriod
		logf(start, "GetDevicePeriod 失败 hr=%#x", uint32(hr))
		return
	}
	logf(start, "设备周期 default=%d min=%d（100ns 单位）", defPeriod, minPeriod)

	const streamFlagsEvent = 0x00040000
	period := defPeriod
	hr := comCall(client, 3, 1, streamFlagsEvent, uintptr(period), uintptr(period), uintptr(unsafe.Pointer(chosen)), 0) // Initialize
	if uint32(hr) == 0x88890019 {                                                                                       // AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED
		var frames uint32
		comCall(client, 4, uintptr(unsafe.Pointer(&frames))) // GetBufferSize
		period = int64(float64(10000000)/float64(chosen.SamplesPerSec)*float64(frames) + 0.5)
		logf(start, "缓冲未对齐，按 %d 帧重算周期 %d 后重试", frames, period)
		release(client)
		client = nil
		if hr2 := comCall(dev, 3, uintptr(unsafe.Pointer(iidAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&client))); failed(hr2) {
			logf(start, "重新 Activate 失败 hr=%#x", uint32(hr2))
			return
		}
		hr = comCall(client, 3, 1, streamFlagsEvent, uintptr(period), uintptr(period), uintptr(unsafe.Pointer(chosen)), 0)
	}
	if failed(hr) {
		logf(start, "Initialize(独占) 失败 hr=%#x（0x8889000e=该设备不允许独占）", uint32(hr))
		return
	}
	logf(start, "Initialize(独占) 成功")

	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		logf(start, "CreateEvent: %v", err)
		return
	}
	defer windows.CloseHandle(ev)
	if hr := comCall(client, 13, uintptr(ev)); failed(hr) { // SetEventHandle
		logf(start, "SetEventHandle 失败 hr=%#x", uint32(hr))
		return
	}
	var bufFrames uint32
	comCall(client, 4, uintptr(unsafe.Pointer(&bufFrames))) // GetBufferSize
	var render unsafe.Pointer
	if hr := comCall(client, 14, uintptr(unsafe.Pointer(iidAudioRenderClient)), uintptr(unsafe.Pointer(&render))); failed(hr) { // GetService
		logf(start, "GetService(IAudioRenderClient) 失败 hr=%#x", uint32(hr))
		return
	}
	defer release(render)

	// 端点自身的峰值表（IMMDevice → IAudioMeterInformation），失败不影响实验。
	var meter unsafe.Pointer
	if hr := comCall(dev, 3, uintptr(unsafe.Pointer(iidMeterInformation)), clsctxAll, 0, uintptr(unsafe.Pointer(&meter))); failed(hr) {
		logf(start, "端点峰值表不可用 hr=%#x", uint32(hr))
		meter = nil
	}
	defer release(meter)
	endpointPeak := func() float32 {
		var v float32 = -1
		if meter != nil {
			comCall(meter, 3, uintptr(unsafe.Pointer(&v))) // GetPeakValue
		}
		return v
	}
	firstMax := -1
	phase := 0.0
	fill := func() bool {
		var data uintptr
		if hr := comCall(render, 3, uintptr(bufFrames), uintptr(unsafe.Pointer(&data))); failed(hr) { // GetBuffer
			logf(start, "GetBuffer 失败 hr=%#x", uint32(hr))
			return false
		}
		n := int(bufFrames) * int(chosen.BlockAlign)
		b := unsafe.Slice((*byte)(unsafe.Pointer(data)), n)
		for i := range b {
			b[i] = 0
		}
		flags := uintptr(2) // AUDCLNT_BUFFERFLAGS_SILENT
		if toneAmp > 0 && (chosen.BitsPerSample == 16 || chosen.BitsPerSample == 32) {
			step := 2 * math.Pi * 440 / float64(chosen.SamplesPerSec)
			bytesPer := int(chosen.BitsPerSample / 8)
			for f := 0; f < int(bufFrames); f++ {
				s := math.Sin(phase) * toneAmp
				phase += step
				for ch := 0; ch < int(chosen.Channels); ch++ {
					off := f*int(chosen.BlockAlign) + ch*bytesPer
					if bytesPer == 2 {
						v := int16(s * 32767)
						b[off], b[off+1] = byte(v), byte(v>>8)
					} else {
						v := int32(s * 2147483647)
						b[off], b[off+1], b[off+2], b[off+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
					}
				}
			}
			flags = 0
		}
		if firstMax < 0 {
			firstMax = 0
			for i := 0; i+1 < n; i += 2 {
				if v := int(int16(uint16(b[i]) | uint16(b[i+1])<<8)); v > firstMax {
					firstMax = v
				}
			}
			logf(start, "首个缓冲的 16 位样本最大值（仅对 16 位格式有意义）=%d，flags=%d", firstMax, flags)
		}
		if hr := comCall(render, 4, uintptr(bufFrames), flags); failed(hr) { // ReleaseBuffer(frames, flags)
			logf(start, "ReleaseBuffer 失败 hr=%#x", uint32(hr))
			return false
		}
		return true
	}
	if !fill() {
		return
	}
	if hr := comCall(client, 10); failed(hr) { // Start
		logf(start, "Start 失败 hr=%#x", uint32(hr))
		return
	}
	logf(start, "独占静音流已启动（缓冲 %d 帧），保持 %d 秒", bufFrames, hold)
	lastMeter := time.Time{}
	deadline := time.Now().Add(time.Duration(hold) * time.Second)
	for time.Now().Before(deadline) {
		if time.Since(lastMeter) >= time.Second {
			lastMeter = time.Now()
			logf(start, "端点峰值表 = %.5f", endpointPeak())
		}
		r, _ := windows.WaitForSingleObject(ev, 2000)
		if r != windows.WAIT_OBJECT_0 {
			logf(start, "等缓冲事件超时，提前结束")
			break
		}
		if !fill() {
			break
		}
	}
	comCall(client, 11) // Stop
	logf(start, "已 Stop，释放")
}
