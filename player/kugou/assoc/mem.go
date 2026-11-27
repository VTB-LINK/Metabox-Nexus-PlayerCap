package assoc

// 本文件实现酷狗进程内存的只读一致性快照器（仅依赖 golang.org/x/sys/windows 的 10 个只读标识符，
// 由 player/kugoumemreadonly_lint_test.go 强制守卫）。通过 ReadProcessMemory 校验 kugou.dll 的
// PE32+ 头指纹，再以两遍一致性读取与六虚表校验提取当前播放覆盖层与歌词管理器状态。

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
)

type memOpenKind uint8

const (
	memOpenOK memOpenKind = iota
	memOpenNotReady
	memOpenUnsupported
	memOpenFailed
)

type memStatus uint8

const (
	memStatusOK memStatus = iota
	memStatusNotReady
	memStatusTorn
	memStatusBadVtable
	memStatusProcessGone
)

type songHandle struct {
	SongID  uint32
	ListIdx uint32
	ListPtr uintptr
}

type memSnapshot struct {
	PlayMgrPtr         uintptr
	PlayCorePtr        uintptr
	QCorePtr           uintptr
	OverlayPtr         uintptr
	Handle             songHandle
	IsMV               bool
	CurrentSongHash    string
	ImplPtr            uintptr
	TaskID             uint32
	CLyricPtr          uintptr
	KrcID              uint32
	SearchDone         bool
	NoLyricFound       bool
	IsUserUnassociated bool
	AssocPath          string
	LyricOffsetMs      int
	ManualOffsetMs     int
	VecBegin           uintptr
	VecEnd             uintptr
	FormatType         uint32
	LyricFilePath      string
}

type memSource interface {
	ReadSnapshot() (memSnapshot, memStatus)
	PID() uint32
	Close()
}

const (
	minUserPtr uintptr = 0x10000

	initialModuleCap = 256
	maxModules       = 4096
	maxModuleNameLen = 260

	dosHeaderSize = 0x40
	minLfanew     = 0x40
	maxLfanew     = 0x1000
	ntHeaderSize  = 0x108

	playMgrBufSize  = 0x28
	playCoreBufSize = 0x78
	qCoreBufSize    = 0xE0
	overlayBufSize  = 0xF0
	implBufSize     = 0x2D8
	cLyricBufSize   = 0x1A8

	maxWStringSize = 32767
	maxWStringCap  = 65536
)

type kugouMemSpec struct {
	kgTimeDateStamp uint32
	kgSizeOfImage   uint32
	playMgrRVA      uintptr
	vtPlayMgrRVA    uintptr
	vtPlayCoreRVA   uintptr
	vtQCoreRVA      uintptr
	vtOverlayRVA    uintptr
	lyricMgrRVA     uintptr
	vtPrimaryRVA    uintptr
	vtSecondaryRVA  uintptr
}

var knownKuGouSpecs = []kugouMemSpec{
	{
		// 酷狗 20.1.22.27795（64 位 PE32+）
		kgTimeDateStamp: 0x6A2A1FAD,
		kgSizeOfImage:   0x0248D000,
		playMgrRVA:      0x22CF6F8,
		vtPlayMgrRVA:    0x1DCA7F0,
		vtPlayCoreRVA:   0x1D12730,
		vtQCoreRVA:      0x1DC5418,
		vtOverlayRVA:    0x1D086A0,
		lyricMgrRVA:     0x22CDB50,
		vtPrimaryRVA:    0x1C66D60,
		vtSecondaryRVA:  0x1C66D98,
	},
}

// memSys 是进程内存读取涉及的全部系统调用，测试以假地址空间桩替换。
type memSys struct {
	openProcess    func(pid uint32) (windows.Handle, error)
	enumModules    func(h windows.Handle, mods []windows.Handle) (needed int, err error)
	moduleBaseName func(h windows.Handle, mod windows.Handle) (string, error)
	readMemory     func(h windows.Handle, addr uintptr, buf []byte) error
	closeHandle    func(h windows.Handle) error
}

var realMemSys = memSys{
	openProcess: func(pid uint32) (windows.Handle, error) {
		return windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	},
	enumModules: func(h windows.Handle, mods []windows.Handle) (int, error) {
		const handleSize = uint32(unsafe.Sizeof(windows.Handle(0)))
		var cbNeeded uint32
		var first *windows.Handle
		if len(mods) > 0 {
			first = &mods[0]
		}
		cb := uint32(len(mods)) * handleSize
		if err := windows.EnumProcessModulesEx(h, first, cb, &cbNeeded, windows.LIST_MODULES_64BIT); err != nil {
			return 0, err
		}
		return int(cbNeeded / handleSize), nil
	},
	moduleBaseName: func(h windows.Handle, mod windows.Handle) (string, error) {
		var buf [maxModuleNameLen]uint16
		if err := windows.GetModuleBaseName(h, mod, &buf[0], uint32(len(buf))); err != nil {
			return "", err
		}
		return decodeUTF16Z(buf[:]), nil
	},
	readMemory: func(h windows.Handle, addr uintptr, buf []byte) error {
		if len(buf) == 0 {
			return nil
		}
		var n uintptr
		if err := windows.ReadProcessMemory(h, addr, &buf[0], uintptr(len(buf)), &n); err != nil {
			return err
		}
		if n != uintptr(len(buf)) {
			return windows.ERROR_PARTIAL_COPY
		}
		return nil
	},
	closeHandle: func(h windows.Handle) error {
		return windows.CloseHandle(h)
	},
}

// openProcessMem 以只读权限打开 pid，校验 kugou.dll 的 PE32+ 头指纹。
// 第 3 返回值 fingerprint 在 memOpenOK 与 memOpenUnsupported 时填写 kugou.dll 指纹摘要，其余情况为空串。
func openProcessMem(pid uint32) (memSource, memOpenKind, string, error) {
	return openProcessMemWith(pid, realMemSys)
}

func openProcessMemWith(pid uint32, sys memSys) (memSource, memOpenKind, string, error) {
	h, err := sys.openProcess(pid)
	if err != nil {
		return nil, memOpenFailed, "", err
	}
	if h == 0 {
		return nil, memOpenFailed, "", i18n.Errorf("open process %d returned null handle", pid)
	}
	keep := false
	defer func() {
		if !keep {
			_ = sys.closeHandle(h)
		}
	}()

	mods, err := enumProcessModules(sys, h)
	if err != nil {
		return nil, memOpenNotReady, "", err
	}
	var kgBase uintptr
	for _, mod := range mods {
		if mod == 0 {
			continue
		}
		name, err := sys.moduleBaseName(h, mod)
		if err != nil {
			continue
		}
		if strings.EqualFold(name, "kugou.dll") {
			kgBase = uintptr(mod)
			break
		}
	}
	if kgBase == 0 {
		return nil, memOpenNotReady, "", i18n.Errorf("kugou.dll not loaded yet")
	}

	kgTS, kgSize, kgBad, err := readPEFingerprint(sys, h, kgBase)
	if err != nil {
		if kgBad {
			return nil, memOpenUnsupported, "", err
		}
		return nil, memOpenNotReady, "", err
	}

	fp := fmt.Sprintf("kugou.dll(ts=0x%08X,size=0x%08X)", kgTS, kgSize)
	for _, spec := range knownKuGouSpecs {
		if spec.kgTimeDateStamp == kgTS && spec.kgSizeOfImage == kgSize {
			keep = true
			return &processMem{
				pid:    pid,
				h:      h,
				kgBase: kgBase,
				spec:   spec,
				sys:    sys,
			}, memOpenOK, fp, nil
		}
	}
	return nil, memOpenUnsupported, fp, i18n.Errorf("unsupported KuGou PE fingerprint: %s", fp)
}

func enumProcessModules(sys memSys, h windows.Handle) ([]windows.Handle, error) {
	size := initialModuleCap
	for attempt := 0; attempt < 3; attempt++ {
		mods := make([]windows.Handle, size)
		needed, err := sys.enumModules(h, mods)
		if err != nil {
			return nil, err
		}
		if needed <= 0 {
			return nil, nil
		}
		if needed <= len(mods) {
			return mods[:needed], nil
		}
		if len(mods) >= maxModules {
			return mods[:maxModules], nil
		}
		size = min(needed, maxModules)
	}
	mods := make([]windows.Handle, size)
	needed, err := sys.enumModules(h, mods)
	if err != nil {
		return nil, err
	}
	if needed < 0 {
		return nil, nil
	}
	return mods[:min(needed, len(mods))], nil
}

// readPEFingerprint 经 ReadProcessMemory 校验 64 位 PE32+ 的 DOS/NT 头，返回 TimeDateStamp 与 SizeOfImage。
// badHeader 为真表示模块头格式本身不符合预期；为假表示内存读取失败。
func readPEFingerprint(sys memSys, h windows.Handle, base uintptr) (ts, sizeOfImage uint32, badHeader bool, err error) {
	if base < minUserPtr {
		return 0, 0, true, i18n.Errorf("invalid module base 0x%X", base)
	}
	var dos [dosHeaderSize]byte
	if err := sys.readMemory(h, base, dos[:]); err != nil {
		return 0, 0, false, err
	}
	if binary.LittleEndian.Uint16(dos[0x00:0x02]) != 0x5A4D {
		return 0, 0, true, i18n.Errorf("invalid DOS magic at 0x%X", base)
	}
	lfanew := binary.LittleEndian.Uint32(dos[0x3C:0x40])
	if lfanew < minLfanew || lfanew > maxLfanew {
		return 0, 0, true, i18n.Errorf("invalid e_lfanew 0x%X at 0x%X", lfanew, base)
	}
	var nt [ntHeaderSize]byte
	if err := sys.readMemory(h, base+uintptr(lfanew), nt[:]); err != nil {
		return 0, 0, false, err
	}
	if binary.LittleEndian.Uint32(nt[0x00:0x04]) != 0x00004550 ||
		binary.LittleEndian.Uint16(nt[0x04:0x06]) != 0x8664 ||
		binary.LittleEndian.Uint16(nt[0x18:0x1A]) != 0x020B {
		return 0, 0, true, i18n.Errorf("not a 64-bit PE32+ image at 0x%X", base)
	}
	return binary.LittleEndian.Uint32(nt[0x08:0x0C]), binary.LittleEndian.Uint32(nt[0x50:0x54]), false, nil
}

type processMem struct {
	pid    uint32
	h      windows.Handle
	kgBase uintptr
	spec   kugouMemSpec
	sys    memSys
}

func (m *processMem) PID() uint32 { return m.pid }

func (m *processMem) Close() {
	if m.h == 0 {
		return
	}
	h := m.h
	m.h = 0
	_ = m.sys.closeHandle(h)
}

func (m *processMem) ReadSnapshot() (memSnapshot, memStatus) {
	if m.h == 0 {
		return memSnapshot{}, memStatusProcessGone
	}
	s1, st := m.readOnce()
	if st != memStatusOK {
		return memSnapshot{}, st
	}
	s2, st := m.readOnce()
	if st != memStatusOK {
		return memSnapshot{}, st
	}
	if s1 != s2 {
		return memSnapshot{}, memStatusTorn
	}
	return s2, memStatusOK
}

func (m *processMem) readOnce() (memSnapshot, memStatus) {
	var snap memSnapshot

	// 1. PlayMgr -> PlayCore -> PlayQueueCore -> Overlay 播放覆盖层链。
	playMgrPtr, st := m.derefChain(m.kgBase+m.spec.playMgrRVA, 1)
	if st != memStatusOK {
		return memSnapshot{}, st
	}
	var pmBuf [playMgrBufSize]byte
	if err := m.sys.readMemory(m.h, playMgrPtr, pmBuf[:]); err != nil {
		return memSnapshot{}, m.classifyReadFailure()
	}
	if binary.LittleEndian.Uint64(pmBuf[0x00:0x08]) != uint64(m.kgBase+m.spec.vtPlayMgrRVA) {
		return memSnapshot{}, memStatusBadVtable
	}
	playCorePtr := uintptr(binary.LittleEndian.Uint64(pmBuf[0x20:0x28]))
	if playCorePtr < minUserPtr {
		return memSnapshot{}, memStatusNotReady
	}

	var pcBuf [playCoreBufSize]byte
	if err := m.sys.readMemory(m.h, playCorePtr, pcBuf[:]); err != nil {
		return memSnapshot{}, m.classifyReadFailure()
	}
	if binary.LittleEndian.Uint64(pcBuf[0x00:0x08]) != uint64(m.kgBase+m.spec.vtPlayCoreRVA) {
		return memSnapshot{}, memStatusBadVtable
	}
	qCorePtr := uintptr(binary.LittleEndian.Uint64(pcBuf[0x70:0x78]))
	if qCorePtr < minUserPtr {
		return memSnapshot{}, memStatusNotReady
	}

	var qcBuf [qCoreBufSize]byte
	if err := m.sys.readMemory(m.h, qCorePtr, qcBuf[:]); err != nil {
		return memSnapshot{}, m.classifyReadFailure()
	}
	if binary.LittleEndian.Uint64(qcBuf[0x00:0x08]) != uint64(m.kgBase+m.spec.vtQCoreRVA) {
		return memSnapshot{}, memStatusBadVtable
	}
	playHandle := decodeSongHandle(qcBuf[0x68:0x78])
	curHandle := decodeSongHandle(qcBuf[0xC8:0xD8])
	overlayPtr := uintptr(binary.LittleEndian.Uint64(qcBuf[0xC0:0xC8]))
	if curHandle.ListPtr < minUserPtr || playHandle.ListPtr < minUserPtr || overlayPtr < minUserPtr {
		return memSnapshot{}, memStatusNotReady
	}
	if curHandle != playHandle {
		return memSnapshot{}, memStatusTorn
	}

	var ovBuf [overlayBufSize]byte
	if err := m.sys.readMemory(m.h, overlayPtr, ovBuf[:]); err != nil {
		return memSnapshot{}, m.classifyReadFailure()
	}
	if binary.LittleEndian.Uint64(ovBuf[0x00:0x08]) != uint64(m.kgBase+m.spec.vtOverlayRVA) {
		return memSnapshot{}, memStatusBadVtable
	}
	if ovBuf[0x08] == 0 {
		return memSnapshot{}, memStatusNotReady
	}
	ovHandle := decodeSongHandle(ovBuf[0x10:0x20])
	if ovHandle != curHandle {
		return memSnapshot{}, memStatusTorn
	}

	isMV := ovBuf[0x2C] != 0
	rawHash := ovBuf[0x58:0x68]
	if isMV {
		rawHash = ovBuf[0xE0:0xF0]
	}
	snap.PlayMgrPtr = playMgrPtr
	snap.PlayCorePtr = playCorePtr
	snap.QCorePtr = qCorePtr
	snap.OverlayPtr = overlayPtr
	snap.Handle = curHandle
	snap.IsMV = isMV
	if !allZeroBytes(rawHash) {
		snap.CurrentSongHash = hex.EncodeToString(rawHash)
	}

	// 2. LyricManagerImpl 3 跳链：
	// [kgBase + lyricMgrRVA] -> singletonPtr -> wrapPtr -> implPtr
	implPtr, st := m.derefChain(m.kgBase+m.spec.lyricMgrRVA, 3)
	if st != memStatusOK {
		return memSnapshot{}, st
	}
	var implBuf [implBufSize]byte
	if err := m.sys.readMemory(m.h, implPtr, implBuf[:]); err != nil {
		return memSnapshot{}, m.classifyReadFailure()
	}
	if binary.LittleEndian.Uint64(implBuf[0x00:0x08]) != uint64(m.kgBase+m.spec.vtPrimaryRVA) ||
		binary.LittleEndian.Uint64(implBuf[0x08:0x10]) != uint64(m.kgBase+m.spec.vtSecondaryRVA) {
		return memSnapshot{}, memStatusBadVtable
	}
	if implBuf[0x68] != 0 {
		return memSnapshot{}, memStatusNotReady
	}
	snap.ImplPtr = implPtr
	snap.TaskID = binary.LittleEndian.Uint32(implBuf[0x20:0x24])
	snap.CLyricPtr = uintptr(binary.LittleEndian.Uint64(implBuf[0x38:0x40]))
	snap.KrcID = binary.LittleEndian.Uint32(implBuf[0x60:0x64])
	snap.SearchDone = implBuf[0x90] != 0
	snap.NoLyricFound = implBuf[0x91] != 0
	snap.IsUserUnassociated = implBuf[0x92] != 0
	snap.AssocPath, st = m.readStdWString(implBuf[0x70:0x90])
	if st != memStatusOK {
		return memSnapshot{}, st
	}

	// 3. CLyric 状态读取。
	if snap.CLyricPtr != 0 {
		if snap.CLyricPtr < minUserPtr {
			return memSnapshot{}, memStatusTorn
		}
		var clBuf [cLyricBufSize]byte
		if err := m.sys.readMemory(m.h, snap.CLyricPtr, clBuf[:]); err != nil {
			return memSnapshot{}, m.classifyReadFailure()
		}
		snap.LyricOffsetMs = int(int32(binary.LittleEndian.Uint32(clBuf[0x104:0x108])))
		snap.ManualOffsetMs = int(int32(binary.LittleEndian.Uint32(clBuf[0x108:0x10C])))
		snap.VecBegin = uintptr(binary.LittleEndian.Uint64(clBuf[0x110:0x118]))
		snap.VecEnd = uintptr(binary.LittleEndian.Uint64(clBuf[0x118:0x120]))
		snap.FormatType = binary.LittleEndian.Uint32(clBuf[0x148:0x14C])
		snap.LyricFilePath, st = m.readStdWString(clBuf[0x128:0x148])
		if st != memStatusOK {
			return memSnapshot{}, st
		}
	}

	return snap, memStatusOK
}

func decodeSongHandle(raw []byte) songHandle {
	return songHandle{
		SongID:  binary.LittleEndian.Uint32(raw[0x00:0x04]),
		ListIdx: binary.LittleEndian.Uint32(raw[0x04:0x08]),
		ListPtr: uintptr(binary.LittleEndian.Uint64(raw[0x08:0x10])),
	}
}

func allZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func (m *processMem) derefChain(addr uintptr, hops int) (uintptr, memStatus) {
	cur := addr
	for i := 0; i < hops; i++ {
		if cur < minUserPtr {
			return 0, memStatusNotReady
		}
		var raw [8]byte
		if err := m.sys.readMemory(m.h, cur, raw[:]); err != nil {
			return 0, m.classifyReadFailure()
		}
		cur = uintptr(binary.LittleEndian.Uint64(raw[:]))
		if cur < minUserPtr {
			return 0, memStatusNotReady
		}
	}
	return cur, memStatusOK
}

func (m *processMem) classifyReadFailure() memStatus {
	var probe [1]windows.Handle
	if _, err := m.sys.enumModules(m.h, probe[:]); err != nil {
		return memStatusProcessGone
	}
	return memStatusTorn
}

func (m *processMem) readStdWString(raw []byte) (string, memStatus) {
	size := binary.LittleEndian.Uint64(raw[0x10:0x18])
	capVal := binary.LittleEndian.Uint64(raw[0x18:0x20])
	if size > capVal || size > maxWStringSize || capVal > maxWStringCap {
		return "", memStatusTorn
	}
	if size == 0 {
		return "", memStatusOK
	}
	var src []byte
	if capVal <= 7 {
		src = raw[:size*2]
	} else {
		ptr := uintptr(binary.LittleEndian.Uint64(raw[0x00:0x08]))
		if ptr < minUserPtr {
			return "", memStatusTorn
		}
		src = make([]byte, size*2)
		if err := m.sys.readMemory(m.h, ptr, src); err != nil {
			return "", m.classifyReadFailure()
		}
	}
	u := make([]uint16, size)
	for i := uint64(0); i < size; i++ {
		u[i] = binary.LittleEndian.Uint16(src[i*2 : i*2+2])
	}
	return decodeUTF16Z(u), memStatusOK
}

func decodeUTF16Z(u []uint16) string {
	for i, ch := range u {
		if ch == 0 {
			u = u[:i]
			break
		}
	}
	if len(u) == 0 {
		return ""
	}
	return string(utf16.Decode(u))
}
