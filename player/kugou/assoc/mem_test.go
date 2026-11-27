package assoc

// 本文件以纯内存假地址空间桩测酷狗进程内存只读快照器（#250/#251，设计 §6.2、§6.3）：
//   - kugou.dll PE32+ 头指纹校验与未知版本降级、六处虚表失配、Overlay 未激活与三处句柄对齐；
//   - 音频（+0x58）与 MV（+0xE0）16 字节二进制 Hash 读取及全零得空串、std::wstring SSO/堆读取与两遍一致性撕裂防御。
//
// 变异自证（均实测变红后还原）：
//   - mem.go 将 Overlay+0x58 改读 0x59 → TestProcessMemOverlayStatesAndMV/音频与MV二进制Hash读取及全零得空串 红（CurrentSongHash 错位读出 da13c9529cbbb7203d813abe616b6100）；
//   - mem.go 去掉 ovBuf[0x08] == 0 检查 → TestProcessMemOverlayStatesAndMV/Overlay未激活或低位指针返memStatusNotReady/Overlay_active为0 红（返回 status=0 而非 memStatusNotReady）。

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	testKuGouBase uintptr = 0x7FF610000000
	testHeapBase  uintptr = 0x0000020000000000
)

type fakeMemRegion struct {
	base uintptr
	data []byte
}

type fakeModule struct {
	base uintptr
	name string
}

type fakeMemSpace struct {
	handle     windows.Handle
	openErr    error
	enumErr    error
	modules    []fakeModule
	regions    []fakeMemRegion
	onRead     func(addr uintptr, size int)
	enumCalls  int
	readCalls  int
	closeCalls int
}

func newFakeMemSpace() *fakeMemSpace {
	return &fakeMemSpace{handle: 0x1234}
}

func (f *fakeMemSpace) addRegion(base uintptr, size int) []byte {
	buf := make([]byte, size)
	f.regions = append(f.regions, fakeMemRegion{base: base, data: buf})
	return buf
}

func (f *fakeMemSpace) regionAt(addr uintptr, size int) []byte {
	for _, r := range f.regions {
		if addr >= r.base && addr+uintptr(size) <= r.base+uintptr(len(r.data)) {
			off := int(addr - r.base)
			return r.data[off : off+size]
		}
	}
	return nil
}

func (f *fakeMemSpace) writePtr(addr uintptr, val uintptr) {
	b := f.regionAt(addr, 8)
	if b == nil {
		panic(fmt.Sprintf("writePtr out of mapped region: 0x%X", addr))
	}
	binary.LittleEndian.PutUint64(b, uint64(val))
}

func (f *fakeMemSpace) writePE(base uintptr, lfanew uint32, machine uint16, optMagic uint16, ts, sizeOfImage uint32) {
	buf := f.addRegion(base, int(lfanew)+0x108)
	binary.LittleEndian.PutUint16(buf[0x00:0x02], 0x5A4D)
	binary.LittleEndian.PutUint32(buf[0x3C:0x40], lfanew)
	nt := buf[lfanew : lfanew+0x108]
	binary.LittleEndian.PutUint32(nt[0x00:0x04], 0x00004550)
	binary.LittleEndian.PutUint16(nt[0x04:0x06], machine)
	binary.LittleEndian.PutUint32(nt[0x08:0x0C], ts)
	binary.LittleEndian.PutUint16(nt[0x18:0x1A], optMagic)
	binary.LittleEndian.PutUint32(nt[0x50:0x54], sizeOfImage)
}

func (f *fakeMemSpace) sys() memSys {
	return memSys{
		openProcess: func(pid uint32) (windows.Handle, error) {
			if f.openErr != nil {
				return 0, f.openErr
			}
			return f.handle, nil
		},
		enumModules: func(h windows.Handle, mods []windows.Handle) (int, error) {
			f.enumCalls++
			if h != f.handle {
				return 0, errors.New("invalid handle")
			}
			if f.enumErr != nil {
				return 0, f.enumErr
			}
			n := len(f.modules)
			for i := 0; i < len(mods) && i < n; i++ {
				mods[i] = windows.Handle(f.modules[i].base)
			}
			return n, nil
		},
		moduleBaseName: func(h windows.Handle, mod windows.Handle) (string, error) {
			if h != f.handle {
				return "", errors.New("invalid handle")
			}
			for _, m := range f.modules {
				if windows.Handle(m.base) == mod {
					return m.name, nil
				}
			}
			return "", errors.New("module not found")
		},
		readMemory: func(h windows.Handle, addr uintptr, buf []byte) error {
			f.readCalls++
			if h != f.handle {
				return errors.New("invalid handle")
			}
			if f.onRead != nil {
				f.onRead(addr, len(buf))
			}
			src := f.regionAt(addr, len(buf))
			if src == nil {
				return windows.ERROR_PARTIAL_COPY
			}
			copy(buf, src)
			return nil
		},
		closeHandle: func(h windows.Handle) error {
			f.closeCalls++
			return nil
		},
	}
}

func putSongHandle(dst []byte, h songHandle) {
	binary.LittleEndian.PutUint32(dst[0x00:0x04], h.SongID)
	binary.LittleEndian.PutUint32(dst[0x04:0x08], h.ListIdx)
	binary.LittleEndian.PutUint64(dst[0x08:0x10], uint64(h.ListPtr))
}

// validFixture 在 fakeMemSpace 中搭建酷狗 20.1.22.27795 的 kugou.dll 模块头、
// PlayMgr -> PlayCore -> PlayQueueCore -> Overlay 播放链，以及 LyricManagerImpl -> CLyric 对象。
type validFixture struct {
	space        *fakeMemSpace
	playMgrPtr   uintptr
	playMgrBuf   []byte
	playCorePtr  uintptr
	playCoreBuf  []byte
	qCorePtr     uintptr
	qCoreBuf     []byte
	overlayPtr   uintptr
	overlayBuf   []byte
	handle       songHandle
	singletonPtr uintptr
	implPtr      uintptr
	implBuf      []byte
	cLyricPtr    uintptr
	clBuf        []byte
	nextHeap     uintptr
}

func newValidFixture(t *testing.T) *validFixture {
	t.Helper()
	spec := knownKuGouSpecs[0]
	f := newFakeMemSpace()
	f.modules = []fakeModule{
		{base: testKuGouBase, name: "kugou.dll"},
	}
	f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)

	// 在 kugou.dll 全局静态区映射两个入口指针。
	f.addRegion(testKuGouBase+spec.playMgrRVA, 8)
	f.addRegion(testKuGouBase+spec.lyricMgrRVA, 8)

	fix := &validFixture{
		space:    f,
		nextHeap: testHeapBase,
		handle: songHandle{
			SongID:  1001,
			ListIdx: 3,
			ListPtr: 0x000001AA00001000,
		},
	}
	alloc := func(size int) (uintptr, []byte) {
		addr := fix.nextHeap
		fix.nextHeap += uintptr((size + 0xFFF) &^ 0xFFF)
		return addr, f.addRegion(addr, size)
	}

	// PlayMgr -> PlayCore -> PlayQueueCore -> Overlay 播放覆盖层链。
	playMgrPtr, playMgrBuf := alloc(0x28)
	playCorePtr, playCoreBuf := alloc(0x78)
	qCorePtr, qCoreBuf := alloc(0xE0)
	overlayPtr, overlayBuf := alloc(0xF0)

	f.writePtr(testKuGouBase+spec.playMgrRVA, playMgrPtr)
	binary.LittleEndian.PutUint64(playMgrBuf[0x00:0x08], uint64(testKuGouBase+spec.vtPlayMgrRVA))
	binary.LittleEndian.PutUint64(playMgrBuf[0x20:0x28], uint64(playCorePtr))

	binary.LittleEndian.PutUint64(playCoreBuf[0x00:0x08], uint64(testKuGouBase+spec.vtPlayCoreRVA))
	binary.LittleEndian.PutUint64(playCoreBuf[0x70:0x78], uint64(qCorePtr))

	binary.LittleEndian.PutUint64(qCoreBuf[0x00:0x08], uint64(testKuGouBase+spec.vtQCoreRVA))
	putSongHandle(qCoreBuf[0x68:0x78], fix.handle)
	binary.LittleEndian.PutUint64(qCoreBuf[0xC0:0xC8], uint64(overlayPtr))
	putSongHandle(qCoreBuf[0xC8:0xD8], fix.handle)

	binary.LittleEndian.PutUint64(overlayBuf[0x00:0x08], uint64(testKuGouBase+spec.vtOverlayRVA))
	overlayBuf[0x08] = 1 // active
	putSongHandle(overlayBuf[0x10:0x20], fix.handle)
	overlayBuf[0x2C] = 0 // audio

	// LyricManagerImpl 3 跳链：
	// [kgBase+lyricMgrRVA] -> singletonPtr -> wrapPtr -> implPtr
	singletonPtr, _ := alloc(8)
	wrapPtr, _ := alloc(8)
	implPtr, implBuf := alloc(0x2D8)
	f.writePtr(testKuGouBase+spec.lyricMgrRVA, singletonPtr)
	f.writePtr(singletonPtr, wrapPtr)
	f.writePtr(wrapPtr, implPtr)
	binary.LittleEndian.PutUint64(implBuf[0x00:0x08], uint64(testKuGouBase+spec.vtPrimaryRVA))
	binary.LittleEndian.PutUint64(implBuf[0x08:0x10], uint64(testKuGouBase+spec.vtSecondaryRVA))

	// CLyric 对象。
	cLyricPtr, clBuf := alloc(0x1A8)
	binary.LittleEndian.PutUint64(implBuf[0x38:0x40], uint64(cLyricPtr))

	fix.playMgrPtr = playMgrPtr
	fix.playMgrBuf = playMgrBuf
	fix.playCorePtr = playCorePtr
	fix.playCoreBuf = playCoreBuf
	fix.qCorePtr = qCorePtr
	fix.qCoreBuf = qCoreBuf
	fix.overlayPtr = overlayPtr
	fix.overlayBuf = overlayBuf
	fix.singletonPtr = singletonPtr
	fix.implPtr = implPtr
	fix.implBuf = implBuf
	fix.cLyricPtr = cLyricPtr
	fix.clBuf = clBuf
	return fix
}

func (fix *validFixture) allocBytes(b []byte) uintptr {
	addr := fix.nextHeap
	size := len(b)
	if size == 0 {
		size = 16
	}
	fix.nextHeap += uintptr((size + 0xFFF) &^ 0xFFF)
	dst := fix.space.addRegion(addr, size)
	copy(dst, b)
	return addr
}

func (fix *validFixture) writeOverlayHash(t *testing.T, off int, hexStr string) {
	t.Helper()
	if hexStr == "" {
		clear(fix.overlayBuf[off : off+16])
		return
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil || len(raw) != 16 {
		t.Fatalf("writeOverlayHash(%q) err=%v len=%d", hexStr, err, len(raw))
	}
	copy(fix.overlayBuf[off:off+16], raw)
}

func (fix *validFixture) writeStdWStringInline(dst []byte, u []uint16, capVal uint64) {
	clear(dst[:32])
	for i, ch := range u {
		binary.LittleEndian.PutUint16(dst[i*2:i*2+2], ch)
	}
	binary.LittleEndian.PutUint64(dst[0x10:0x18], uint64(len(u)))
	binary.LittleEndian.PutUint64(dst[0x18:0x20], capVal)
}

func (fix *validFixture) writeStdWStringHeap(dst []byte, u []uint16, capVal uint64) {
	clear(dst[:32])
	raw := make([]byte, len(u)*2)
	for i, ch := range u {
		binary.LittleEndian.PutUint16(raw[i*2:i*2+2], ch)
	}
	ptr := fix.allocBytes(raw)
	binary.LittleEndian.PutUint64(dst[0x00:0x08], uint64(ptr))
	binary.LittleEndian.PutUint64(dst[0x10:0x18], uint64(len(u)))
	binary.LittleEndian.PutUint64(dst[0x18:0x20], capVal)
}

func TestOpenMemMatchesKnown20_1_22_27795(t *testing.T) {
	spec := knownKuGouSpecs[0]
	f := newFakeMemSpace()
	// 超过初始模块缓冲（写入 300 个模块，仅放 kugou.dll 在末尾，无 deferred.dll），验证扩容重调路径与不分大小写匹配。
	for i := 0; i < 299; i++ {
		f.modules = append(f.modules, fakeModule{
			base: 0x7FF700000000 + uintptr(i)*0x10000,
			name: fmt.Sprintf("other%d.dll", i),
		})
	}
	f.modules = append(f.modules,
		fakeModule{base: testKuGouBase, name: "KuGou.DLL"},
	)
	f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)

	src, kind, fp, err := openProcessMemWith(4242, f.sys())
	if err != nil {
		t.Fatalf("期望打开成功，实际 err=%v", err)
	}
	if kind != memOpenOK {
		t.Fatalf("kind=%v，期望 memOpenOK", kind)
	}
	if src == nil || src.PID() != 4242 {
		t.Fatalf("src=%v PID=%d，期望 PID=4242", src, func() uint32 {
			if src == nil {
				return 0
			}
			return src.PID()
		}())
	}
	if f.enumCalls < 2 {
		t.Fatalf("enumCalls=%d，期望 >=2 以验证模块缓冲扩容重调", f.enumCalls)
	}
	for _, want := range []string{"kugou.dll", "6A2A1FAD", "0248D000"} {
		if !strings.Contains(strings.ToUpper(fp), strings.ToUpper(want)) {
			t.Errorf("指纹摘要 %q 缺少 %q", fp, want)
		}
	}
	if strings.Contains(strings.ToLower(fp), "deferred.dll") {
		t.Errorf("指纹摘要 %q 不应再引用 deferred.dll", fp)
	}
	src.Close()
	if f.closeCalls != 1 {
		t.Fatalf("Close 后 closeCalls=%d，期望 1", f.closeCalls)
	}
}

func TestOpenMemRejectsUnknownPEOrMissingModule(t *testing.T) {
	spec := knownKuGouSpecs[0]

	t.Run("篡改kugou.dll时间戳", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp+1, spec.kgSizeOfImage)

		src, kind, fp, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported || fp == "" {
			t.Fatalf("got src=%v kind=%v fp=%q，期望 nil / memOpenUnsupported / 非空指纹", src, kind, fp)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("篡改kugou.dll镜像大小", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage+0x1000)

		src, kind, fp, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported || fp == "" {
			t.Fatalf("got src=%v kind=%v fp=%q，期望 nil / memOpenUnsupported / 非空指纹", src, kind, fp)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("非PE32+头被拒", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x010B, spec.kgTimeDateStamp, spec.kgSizeOfImage) // PE32 而非 PE32+

		src, kind, _, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported {
			t.Fatalf("got src=%v kind=%v，期望 nil / memOpenUnsupported", src, kind)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("缺少kugou.dll返memOpenNotReady", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: 0x7FF630000000, name: "other.dll"},
		}

		src, kind, fp, err := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenNotReady || fp != "" || err == nil {
			t.Fatalf("got src=%v kind=%v fp=%q err=%v，期望 nil / memOpenNotReady / 空串 / 非 nil err", src, kind, fp, err)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("OpenProcess失败", func(t *testing.T) {
		f := newFakeMemSpace()
		f.openErr = errors.New("access denied")

		src, kind, fp, err := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenFailed || fp != "" || err == nil {
			t.Fatalf("got src=%v kind=%v fp=%q err=%v，期望 nil / memOpenFailed / 空串 / 非 nil err", src, kind, fp, err)
		}
		if f.closeCalls != 0 {
			t.Fatalf("OpenProcess 失败不应 CloseHandle，实际 closeCalls=%d", f.closeCalls)
		}
	})
}

func TestProcessMemOverlayStatesAndMV(t *testing.T) {
	const (
		audioHash = "17da13c9529cbbb7203d813abe616b61"
		mvHash    = "abcdef0123456789fedcba9876543210"
	)

	t.Run("音频与MV二进制Hash读取及全零得空串", func(t *testing.T) {
		// 1. isMV == 0 读取 Overlay+0x58..0x68（同时在 +0xE0 填入干扰值确保只读音频偏移）。
		fix := newValidFixture(t)
		fix.overlayBuf[0x2C] = 0
		fix.writeOverlayHash(t, 0x58, audioHash)
		fix.writeOverlayHash(t, 0xE0, mvHash)

		src, kind, _, err := openProcessMemWith(101, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("仅 kugou.dll 打开失败: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		got, st := src.ReadSnapshot()
		if st != memStatusOK {
			t.Fatalf("音频快照 status=%v，期望 memStatusOK", st)
		}
		if got.IsMV || got.CurrentSongHash != audioHash || got.Handle != fix.handle ||
			got.PlayMgrPtr != fix.playMgrPtr || got.PlayCorePtr != fix.playCorePtr ||
			got.QCorePtr != fix.qCorePtr || got.OverlayPtr != fix.overlayPtr {
			t.Fatalf("音频快照字段不符：%+v", got)
		}

		// 2. isMV != 0 读取 Overlay+0xE0..0xF0。
		fix.overlayBuf[0x2C] = 1
		got, st = src.ReadSnapshot()
		if st != memStatusOK {
			t.Fatalf("MV 快照 status=%v，期望 memStatusOK", st)
		}
		if !got.IsMV || got.CurrentSongHash != mvHash {
			t.Fatalf("MV 快照 got IsMV=%v Hash=%q，want true / %q", got.IsMV, got.CurrentSongHash, mvHash)
		}

		// 3. MV 16 字节全零得 CurrentSongHash == ""。
		fix.writeOverlayHash(t, 0xE0, "")
		got, st = src.ReadSnapshot()
		if st != memStatusOK || !got.IsMV || got.CurrentSongHash != "" {
			t.Fatalf("MV 全零摘要 got st=%v IsMV=%v Hash=%q，want OK / true / 空串", st, got.IsMV, got.CurrentSongHash)
		}

		// 4. 音频 16 字节全零得 CurrentSongHash == ""。
		fix.overlayBuf[0x2C] = 0
		fix.writeOverlayHash(t, 0x58, "")
		got, st = src.ReadSnapshot()
		if st != memStatusOK || got.IsMV || got.CurrentSongHash != "" {
			t.Fatalf("音频全零摘要 got st=%v IsMV=%v Hash=%q，want OK / false / 空串", st, got.IsMV, got.CurrentSongHash)
		}
	})

	t.Run("Overlay未激活或低位指针返memStatusNotReady", func(t *testing.T) {
		spec := knownKuGouSpecs[0]
		cases := []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{"Overlay_active为0", func(fix *validFixture) {
				fix.overlayBuf[0x08] = 0
			}},
			{"playMgr全局指针为0", func(fix *validFixture) {
				fix.space.writePtr(testKuGouBase+spec.playMgrRVA, 0)
			}},
			{"playCore指针小于0x10000", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.playMgrBuf[0x20:0x28], 0x200)
			}},
			{"qCore指针小于0x10000", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.playCoreBuf[0x70:0x78], 0x200)
			}},
			{"overlayPtr小于0x10000", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.qCoreBuf[0xC0:0xC8], 0x200)
			}},
			{"curHandle.ListPtr小于0x10000", func(fix *validFixture) {
				h := fix.handle
				h.ListPtr = 0x200
				putSongHandle(fix.qCoreBuf[0xC8:0xD8], h)
			}},
			{"playHandle.ListPtr小于0x10000", func(fix *validFixture) {
				h := fix.handle
				h.ListPtr = 0x200
				putSongHandle(fix.qCoreBuf[0x68:0x78], h)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				tc.setup(fix)
				src, kind, _, err := openProcessMemWith(102, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()
				if _, st := src.ReadSnapshot(); st != memStatusNotReady {
					t.Fatalf("status=%v，期望 memStatusNotReady", st)
				}
			})
		}
	})

	t.Run("三处句柄任一不对齐返memStatusTorn", func(t *testing.T) {
		cases := []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{"curHandle与playHandle的SongID不等", func(fix *validFixture) {
				h := fix.handle
				h.SongID++
				putSongHandle(fix.qCoreBuf[0xC8:0xD8], h)
			}},
			{"curHandle与playHandle的ListIdx不等", func(fix *validFixture) {
				h := fix.handle
				h.ListIdx++
				putSongHandle(fix.qCoreBuf[0xC8:0xD8], h)
			}},
			{"curHandle与playHandle的ListPtr不等", func(fix *validFixture) {
				h := fix.handle
				h.ListPtr += 0x100
				putSongHandle(fix.qCoreBuf[0xC8:0xD8], h)
			}},
			{"ovHandle与curHandle的SongID不等", func(fix *validFixture) {
				h := fix.handle
				h.SongID++
				putSongHandle(fix.overlayBuf[0x10:0x20], h)
			}},
			{"ovHandle与curHandle的ListIdx不等", func(fix *validFixture) {
				h := fix.handle
				h.ListIdx++
				putSongHandle(fix.overlayBuf[0x10:0x20], h)
			}},
			{"ovHandle与curHandle的ListPtr不等", func(fix *validFixture) {
				h := fix.handle
				h.ListPtr += 0x100
				putSongHandle(fix.overlayBuf[0x10:0x20], h)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				tc.setup(fix)
				src, kind, _, err := openProcessMemWith(103, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()
				if _, st := src.ReadSnapshot(); st != memStatusTorn {
					t.Fatalf("status=%v，期望 memStatusTorn", st)
				}
			})
		}
	})

	t.Run("六处虚表任一失配返memStatusBadVtable", func(t *testing.T) {
		cases := []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{"PlayMgr虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.playMgrBuf[0x00:0x08], 0xDEAD0001)
			}},
			{"PlayCore虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.playCoreBuf[0x00:0x08], 0xDEAD0002)
			}},
			{"PlayQueueCore虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.qCoreBuf[0x00:0x08], 0xDEAD0003)
			}},
			{"Overlay虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.overlayBuf[0x00:0x08], 0xDEAD0004)
			}},
			{"LyricManagerImpl主虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.implBuf[0x00:0x08], 0xDEAD0005)
			}},
			{"LyricManagerImpl次虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.implBuf[0x08:0x10], 0xDEAD0006)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				tc.setup(fix)
				src, kind, _, err := openProcessMemWith(104, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()
				if _, st := src.ReadSnapshot(); st != memStatusBadVtable {
					t.Fatalf("status=%v，期望 memStatusBadVtable", st)
				}
			})
		}
	})
}

func TestReadSnapshotConsistentAndStrings(t *testing.T) {
	const sampleHash = "af479254a6cd798cca9bcf3a3599aae2"

	t.Run("内联wstring与负数偏移与取消关联全21字段一致", func(t *testing.T) {
		fix := newValidFixture(t)
		fix.writeOverlayHash(t, 0x58, sampleHash)

		binary.LittleEndian.PutUint32(fix.implBuf[0x20:0x24], 77)
		binary.LittleEndian.PutUint32(fix.implBuf[0x60:0x64], 9988)
		fix.implBuf[0x68] = 0
		fix.implBuf[0x90] = 1
		fix.implBuf[0x91] = 0
		fix.implBuf[0x92] = 1
		// std::wstring cap=7 内联（"用户取消关联" 共 6 个 UTF-16 单元 <= 7）
		fix.writeStdWStringInline(fix.implBuf[0x70:0x90], utf16.Encode([]rune("用户取消关联")), 7)

		negOff := int32(-1500)
		negManual := int32(-250)
		binary.LittleEndian.PutUint32(fix.clBuf[0x104:0x108], uint32(negOff))
		binary.LittleEndian.PutUint32(fix.clBuf[0x108:0x10C], uint32(negManual))
		binary.LittleEndian.PutUint64(fix.clBuf[0x110:0x118], 0x0000021100000010)
		binary.LittleEndian.PutUint64(fix.clBuf[0x118:0x120], 0x0000021100000210)
		binary.LittleEndian.PutUint32(fix.clBuf[0x148:0x14C], 2)
		fix.writeStdWStringInline(fix.clBuf[0x128:0x148], utf16.Encode([]rune("a.krc")), 7)

		src, kind, _, err := openProcessMemWith(9001, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("openProcessMemWith failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		got, st := src.ReadSnapshot()
		if st != memStatusOK {
			t.Fatalf("ReadSnapshot status=%v，期望 memStatusOK", st)
		}
		want := memSnapshot{
			PlayMgrPtr:         fix.playMgrPtr,
			PlayCorePtr:        fix.playCorePtr,
			QCorePtr:           fix.qCorePtr,
			OverlayPtr:         fix.overlayPtr,
			Handle:             fix.handle,
			IsMV:               false,
			CurrentSongHash:    sampleHash,
			ImplPtr:            fix.implPtr,
			TaskID:             77,
			CLyricPtr:          fix.cLyricPtr,
			KrcID:              9988,
			SearchDone:         true,
			NoLyricFound:       false,
			IsUserUnassociated: true,
			AssocPath:          "用户取消关联",
			LyricOffsetMs:      -1500,
			ManualOffsetMs:     -250,
			VecBegin:           0x0000021100000010,
			VecEnd:             0x0000021100000210,
			FormatType:         2,
			LyricFilePath:      "a.krc",
		}
		if got != want {
			t.Fatalf("快照字段不符：\ngot  = %+v\nwant = %+v", got, want)
		}
	})

	t.Run("堆分配std::wstring(cap=8/16/64)", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			assoc string
			aCap  uint64
			lrc   string
			lCap  uint64
		}{
			{"cap8与cap16", "12345678", 8, `C:\L\1.krc`, 16},
			{"cap16与cap32", `C:\L\用户取消关联.lrc`, 16, `D:\KuGou\Lyric\测试歌曲.krc`, 32},
			{"cap64长路径", `C:\Users\主播\Lyrics\用户取消关联.lrc`, 64, `D:\KuGou\Lyric\测试歌曲.krc`, 32},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				fix.writeOverlayHash(t, 0x58, sampleHash)
				fix.writeStdWStringHeap(fix.implBuf[0x70:0x90], utf16.Encode([]rune(tc.assoc)), tc.aCap)
				fix.writeStdWStringHeap(fix.clBuf[0x128:0x148], utf16.Encode([]rune(tc.lrc)), tc.lCap)

				src, kind, _, err := openProcessMemWith(9002, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()

				got, st := src.ReadSnapshot()
				if st != memStatusOK {
					t.Fatalf("status=%v，期望 memStatusOK", st)
				}
				if got.CurrentSongHash != sampleHash || got.AssocPath != tc.assoc || got.LyricFilePath != tc.lrc {
					t.Fatalf("解码不符：hash=%q assoc=%q lrc=%q", got.CurrentSongHash, got.AssocPath, got.LyricFilePath)
				}
			})
		}
	})

	t.Run("内嵌NUL截断与不成对UTF-16代理项", func(t *testing.T) {
		fix := newValidFixture(t)
		fix.writeOverlayHash(t, 0x58, sampleHash)
		// 内嵌 NUL 截断
		assocU := append(utf16.Encode([]rune("前面")), 0x0000)
		assocU = append(assocU, utf16.Encode([]rune("后面"))...)
		fix.writeStdWStringInline(fix.implBuf[0x70:0x90], assocU, 7)
		// 不成对高代理 0xD800 与低代理 0xDC00
		surrU := []uint16{'X', 0xD800, 'Y', 0xDC00, 'Z'}
		fix.writeStdWStringHeap(fix.clBuf[0x128:0x148], surrU, 16)

		src, kind, _, err := openProcessMemWith(9003, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		got, st := src.ReadSnapshot()
		if st != memStatusOK {
			t.Fatalf("status=%v，期望 memStatusOK", st)
		}
		if got.AssocPath != "前面" {
			t.Errorf("AssocPath=%q，期望 前面", got.AssocPath)
		}
		wantLrc := "X\uFFFDY\uFFFDZ"
		if got.LyricFilePath != wantLrc {
			t.Errorf("LyricFilePath=%q，期望 %q", got.LyricFilePath, wantLrc)
		}
	})

	t.Run("CLyricPtr为0时跳过CLyric读取", func(t *testing.T) {
		fix := newValidFixture(t)
		binary.LittleEndian.PutUint64(fix.implBuf[0x38:0x40], 0)
		fix.writeOverlayHash(t, 0x58, sampleHash)

		src, kind, _, err := openProcessMemWith(9004, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		got, st := src.ReadSnapshot()
		if st != memStatusOK {
			t.Fatalf("status=%v，期望 memStatusOK", st)
		}
		if got.CLyricPtr != 0 || got.LyricOffsetMs != 0 || got.ManualOffsetMs != 0 || got.VecBegin != 0 || got.VecEnd != 0 || got.FormatType != 0 || got.LyricFilePath != "" {
			t.Fatalf("CLyricPtr=0 时 CLyric 字段应全为零值，实得 %+v", got)
		}
	})
}

func TestReadSnapshotDetectsTornBadVtableAndShutdown(t *testing.T) {
	t.Run("两遍之间改动LyricOffsetMs返memStatusTorn", func(t *testing.T) {
		fix := newValidFixture(t)
		binary.LittleEndian.PutUint32(fix.clBuf[0x104:0x108], 100)
		src, kind, _, err := openProcessMemWith(1, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		clReads := 0
		fix.space.onRead = func(addr uintptr, size int) {
			if addr == fix.cLyricPtr && size == 0x1A8 {
				clReads++
				if clReads == 2 {
					binary.LittleEndian.PutUint32(fix.clBuf[0x104:0x108], 200)
				}
			}
		}

		if _, st := src.ReadSnapshot(); st != memStatusTorn {
			t.Fatalf("status=%v，期望 memStatusTorn", st)
		}
	})

	t.Run("两遍之间改动Overlay音频Hash返memStatusTorn", func(t *testing.T) {
		fix := newValidFixture(t)
		fix.writeOverlayHash(t, 0x58, "17da13c9529cbbb7203d813abe616b61")
		src, kind, _, err := openProcessMemWith(1, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		ovReads := 0
		fix.space.onRead = func(addr uintptr, size int) {
			if addr == fix.overlayPtr && size == 0xF0 {
				ovReads++
				if ovReads == 2 {
					fix.writeOverlayHash(t, 0x58, "af479254a6cd798cca9bcf3a3599aae2")
				}
			}
		}

		if _, st := src.ReadSnapshot(); st != memStatusTorn {
			t.Fatalf("status=%v，期望 memStatusTorn", st)
		}
	})

	t.Run("字符串长度与容量越界返memStatusTorn", func(t *testing.T) {
		subcases := []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{
				name: "wstring_size大于cap",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.implBuf[0x80:0x88], 10)
					binary.LittleEndian.PutUint64(fix.implBuf[0x88:0x90], 7)
				},
			},
			{
				name: "wstring_size超上限32767",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.implBuf[0x80:0x88], 32768)
					binary.LittleEndian.PutUint64(fix.implBuf[0x88:0x90], 40000)
				},
			},
			{
				name: "wstring_cap超上限65536",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.implBuf[0x80:0x88], 10)
					binary.LittleEndian.PutUint64(fix.implBuf[0x88:0x90], 65537)
				},
			},
			{
				name: "wstring堆指针小于0x10000",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.implBuf[0x70:0x78], 0x200)
					binary.LittleEndian.PutUint64(fix.implBuf[0x80:0x88], 8)
					binary.LittleEndian.PutUint64(fix.implBuf[0x88:0x90], 16)
				},
			},
			{
				name: "CLyricPtr非零但小于0x10000",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.implBuf[0x38:0x40], 0x200)
				},
			},
		}
		for _, tc := range subcases {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				tc.setup(fix)
				src, kind, _, err := openProcessMemWith(1, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()
				if _, st := src.ReadSnapshot(); st != memStatusTorn {
					t.Fatalf("status=%v，期望 memStatusTorn", st)
				}
			})
		}
	})

	t.Run("isShuttingDown或lyricMgr低位指针返memStatusNotReady", func(t *testing.T) {
		spec := knownKuGouSpecs[0]
		for _, tc := range []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{"isShuttingDown非零", func(fix *validFixture) {
				fix.implBuf[0x68] = 1
			}},
			{"lyricMgr单例指针为0", func(fix *validFixture) {
				fix.space.writePtr(testKuGouBase+spec.lyricMgrRVA, 0)
			}},
			{"lyricMgr单例指针为1", func(fix *validFixture) {
				fix.space.writePtr(testKuGouBase+spec.lyricMgrRVA, 1)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				tc.setup(fix)
				src, kind, _, err := openProcessMemWith(1, fix.space.sys())
				if err != nil || kind != memOpenOK {
					t.Fatalf("open failed: kind=%v err=%v", kind, err)
				}
				defer src.Close()
				if _, st := src.ReadSnapshot(); st != memStatusNotReady {
					t.Fatalf("status=%v，期望 memStatusNotReady", st)
				}
			})
		}
	})

	t.Run("读失败且模块枚举失败返memStatusProcessGone", func(t *testing.T) {
		fix := newValidFixture(t)
		src, kind, _, err := openProcessMemWith(1, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		// 将 CLyricPtr 指向未映射地址以触发 readMemory 失败；先确认进程仍在时返 memStatusTorn。
		binary.LittleEndian.PutUint64(fix.implBuf[0x38:0x40], 0x0000039900000000)
		if _, st := src.ReadSnapshot(); st != memStatusTorn {
			t.Fatalf("进程仍在且读失败时 status=%v，期望 memStatusTorn", st)
		}

		// 进程已退出导致 enumModules 也失败时返 memStatusProcessGone。
		fix.space.enumErr = errors.New("process exited")
		if _, st := src.ReadSnapshot(); st != memStatusProcessGone {
			t.Fatalf("进程退出时 status=%v，期望 memStatusProcessGone", st)
		}
	})

	t.Run("Close幂等且关闭后不再调readMemory", func(t *testing.T) {
		fix := newValidFixture(t)
		src, kind, _, err := openProcessMemWith(1, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		src.Close()
		src.Close()
		if fix.space.closeCalls != 1 {
			t.Fatalf("连续调用两次 Close 后 closeCalls=%d，期望 1", fix.space.closeCalls)
		}
		readsBefore := fix.space.readCalls
		if _, st := src.ReadSnapshot(); st != memStatusProcessGone {
			t.Fatalf("Close 后 ReadSnapshot status=%v，期望 memStatusProcessGone", st)
		}
		if fix.space.readCalls != readsBefore {
			t.Fatalf("Close 后不应再调 readMemory，实际多调了 %d 次", fix.space.readCalls-readsBefore)
		}
	})
}
