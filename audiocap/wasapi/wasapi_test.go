//go:build windows

package wasapi

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 手排的 C 结构体一旦偏移错位，激活/初始化就会静默失败或崩溃 —— 这里把每个偏移与总大小钉死，
// 任何字段增删或对齐变化都会被立即抓住。数值取自 Windows SDK 的 x64 布局。

func TestActivationParamsLayout(t *testing.T) {
	var p audioclientActivationParams
	if got := unsafe.Sizeof(p); got != 12 {
		t.Fatalf("sizeof(AUDIOCLIENT_ACTIVATION_PARAMS)=%d, 期望 12", got)
	}
	check(t, "activationType", unsafe.Offsetof(p.activationType), 0)
	check(t, "targetProcessID", unsafe.Offsetof(p.targetProcessID), 4)
	check(t, "processLoopbackMode", unsafe.Offsetof(p.processLoopbackMode), 8)
}

func TestPropVariantLayout(t *testing.T) {
	var pv propVariant
	if got := unsafe.Sizeof(pv); got != 24 {
		t.Fatalf("sizeof(PROPVARIANT)=%d, 期望 24（x64）", got)
	}
	check(t, "vt", unsafe.Offsetof(pv.vt), 0)
	check(t, "cbSize", unsafe.Offsetof(pv.cbSize), 8)
	check(t, "pBlobData", unsafe.Offsetof(pv.pBlobData), 16)
}

func TestWaveFormatExtensibleLayout(t *testing.T) {
	var w waveFormatExtensible
	if got := unsafe.Sizeof(w); got != 40 {
		t.Fatalf("sizeof(WAVEFORMATEXTENSIBLE)=%d, 期望 40", got)
	}
	// cbSize 必须为 40-18=22，否则 WASAPI 会把它当纯 WAVEFORMATEX 解析。
	if got := unsafe.Sizeof(w) - 18; got != 22 {
		t.Fatalf("cbSize 应为 %d, 期望 22", got)
	}
	check(t, "wFormatTag", unsafe.Offsetof(w.wFormatTag), 0)
	check(t, "nChannels", unsafe.Offsetof(w.nChannels), 2)
	check(t, "nSamplesPerSec", unsafe.Offsetof(w.nSamplesPerSec), 4)
	check(t, "nAvgBytesPerSec", unsafe.Offsetof(w.nAvgBytesPerSec), 8)
	check(t, "nBlockAlign", unsafe.Offsetof(w.nBlockAlign), 12)
	check(t, "wBitsPerSample", unsafe.Offsetof(w.wBitsPerSample), 14)
	check(t, "cbSize", unsafe.Offsetof(w.cbSize), 16)
	check(t, "wValidBits", unsafe.Offsetof(w.wValidBits), 18)
	check(t, "dwChannelMask", unsafe.Offsetof(w.dwChannelMask), 20)
	check(t, "subFormat", unsafe.Offsetof(w.subFormat), 24)
}

func TestCompletionHandlerLayout(t *testing.T) {
	var h completionHandler
	// vtbl 必须是首字段：C 侧把结构体地址当接口指针，读首字得到 vtable。
	check(t, "completionHandler.vtbl", unsafe.Offsetof(h.vtbl), 0)

	var v completionHandlerVtbl
	if got := unsafe.Sizeof(v); got != 4*unsafe.Sizeof(uintptr(0)) {
		t.Fatalf("sizeof(vtbl)=%d, 期望 4 个方法槽", got)
	}
	check(t, "queryInterface", unsafe.Offsetof(v.queryInterface), 0)
	check(t, "addRef", unsafe.Offsetof(v.addRef), unsafe.Sizeof(uintptr(0)))
	check(t, "release", unsafe.Offsetof(v.release), 2*unsafe.Sizeof(uintptr(0)))
	check(t, "activateCompleted", unsafe.Offsetof(v.activateCompleted), 3*unsafe.Sizeof(uintptr(0)))
}

func TestGUIDSize(t *testing.T) {
	if got := unsafe.Sizeof(windows.GUID{}); got != 16 {
		t.Fatalf("sizeof(GUID)=%d, 期望 16", got)
	}
}

func check(t *testing.T, name string, got, want uintptr) {
	t.Helper()
	if got != want {
		t.Errorf("%s 偏移=%d, 期望 %d", name, got, want)
	}
}
