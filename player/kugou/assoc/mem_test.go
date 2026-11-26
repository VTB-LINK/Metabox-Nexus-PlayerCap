package assoc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	testKuGouBase    uintptr = 0x7FF610000000
	testDeferredBase uintptr = 0x7FF620000000
	testHeapBase     uintptr = 0x0000020000000000
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

// buildValidKuGouFixture 在 fakeMemSpace 中搭建酷狗 20.1.22.27795 的模块头、两条指针链、
// ILyricConfigManager、LyricManagerImpl 与 CLyric 对象。
type validFixture struct {
	space          *fakeMemSpace
	storageHostPtr uintptr
	cfgMgrPtr      uintptr
	cfgBuf         []byte
	singletonPtr   uintptr
	implPtr        uintptr
	implBuf        []byte
	cLyricPtr      uintptr
	clBuf          []byte
	nextHeap       uintptr
}

func newValidFixture(t *testing.T) *validFixture {
	t.Helper()
	spec := knownKuGouSpecs[0]
	f := newFakeMemSpace()
	f.modules = []fakeModule{
		{base: testKuGouBase, name: "kugou.dll"},
		{base: testDeferredBase, name: "deferred.dll"},
	}
	f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)
	f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage)

	// 在 kugou.dll 全局静态区映射两个入口指针。
	f.addRegion(testKuGouBase+spec.lyricMgrRVA, 8)
	f.addRegion(testKuGouBase+spec.lyricStorageRVA, 8)

	fix := &validFixture{
		space:    f,
		nextHeap: testHeapBase,
	}
	alloc := func(size int) (uintptr, []byte) {
		addr := fix.nextHeap
		fix.nextHeap += uintptr((size + 0xFFF) &^ 0xFFF)
		return addr, f.addRegion(addr, size)
	}

	// ILyricConfigManager 5 跳链：
	// [kgBase+lyricStorageRVA] -> storageHost -> storagePtr -> svcWrap -> svcPtr -> configMgrPtr
	storageHost, _ := alloc(8)
	storagePtr, _ := alloc(8)
	svcWrap, _ := alloc(8)
	svcPtr, _ := alloc(8)
	cfgMgrPtr, cfgBuf := alloc(0x1C0)
	f.writePtr(testKuGouBase+spec.lyricStorageRVA, storageHost)
	f.writePtr(storageHost, storagePtr)
	f.writePtr(storagePtr, svcWrap)
	f.writePtr(svcWrap, svcPtr)
	f.writePtr(svcPtr, cfgMgrPtr)
	binary.LittleEndian.PutUint64(cfgBuf[0x00:0x08], uint64(testDeferredBase+spec.vtConfigMgrRVA))
	binary.LittleEndian.PutUint64(cfgBuf[0x170:0x178], 0x000001AA00001000)

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

	fix.storageHostPtr = storageHost
	fix.cfgMgrPtr = cfgMgrPtr
	fix.cfgBuf = cfgBuf
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

func (fix *validFixture) writeStdStringInline(dst []byte, b []byte, capVal uint64) {
	clear(dst[:32])
	copy(dst[:16], b)
	binary.LittleEndian.PutUint64(dst[0x10:0x18], uint64(len(b)))
	binary.LittleEndian.PutUint64(dst[0x18:0x20], capVal)
}

func (fix *validFixture) writeStdStringHeap(dst []byte, b []byte, capVal uint64) {
	clear(dst[:32])
	ptr := fix.allocBytes(b)
	binary.LittleEndian.PutUint64(dst[0x00:0x08], uint64(ptr))
	binary.LittleEndian.PutUint64(dst[0x10:0x18], uint64(len(b)))
	binary.LittleEndian.PutUint64(dst[0x18:0x20], capVal)
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
	// 超过初始模块缓冲（写入 300 个模块，目标 DLL 放在末尾），验证扩容重调路径与不分大小写匹配。
	for i := 0; i < 298; i++ {
		f.modules = append(f.modules, fakeModule{
			base: 0x7FF700000000 + uintptr(i)*0x10000,
			name: fmt.Sprintf("other%d.dll", i),
		})
	}
	f.modules = append(f.modules,
		fakeModule{base: testKuGouBase, name: "KuGou.DLL"},
		fakeModule{base: testDeferredBase, name: "Deferred.dll"},
	)
	f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)
	f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage)

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
	for _, want := range []string{"kugou.dll", "deferred.dll", "6A2A1FAD", "0248D000", "6A0180FC", "0011F000"} {
		if !strings.Contains(strings.ToUpper(fp), strings.ToUpper(want)) {
			t.Errorf("指纹摘要 %q 缺少 %q", fp, want)
		}
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
			{base: testDeferredBase, name: "deferred.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp+1, spec.kgSizeOfImage)
		f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage)

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
			{base: testDeferredBase, name: "deferred.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage+0x1000)
		f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage)

		src, kind, fp, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported || fp == "" {
			t.Fatalf("got src=%v kind=%v fp=%q，期望 nil / memOpenUnsupported / 非空指纹", src, kind, fp)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("篡改deferred.dll时间戳", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
			{base: testDeferredBase, name: "deferred.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)
		f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp+1, spec.defSizeOfImage)

		src, kind, fp, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported || fp == "" {
			t.Fatalf("got src=%v kind=%v fp=%q，期望 nil / memOpenUnsupported / 非空指纹", src, kind, fp)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("篡改deferred.dll镜像大小", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
			{base: testDeferredBase, name: "deferred.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)
		f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage+0x1000)

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
			{base: testDeferredBase, name: "deferred.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x010B, spec.kgTimeDateStamp, spec.kgSizeOfImage) // PE32 而非 PE32+
		f.writePE(testDeferredBase, 0xE8, 0x8664, 0x020B, spec.defTimeDateStamp, spec.defSizeOfImage)

		src, kind, _, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenUnsupported {
			t.Fatalf("got src=%v kind=%v，期望 nil / memOpenUnsupported", src, kind)
		}
		if f.closeCalls != 1 {
			t.Fatalf("closeCalls=%d，期望 1", f.closeCalls)
		}
	})

	t.Run("缺少deferred.dll", func(t *testing.T) {
		f := newFakeMemSpace()
		f.modules = []fakeModule{
			{base: testKuGouBase, name: "kugou.dll"},
		}
		f.writePE(testKuGouBase, 0xF8, 0x8664, 0x020B, spec.kgTimeDateStamp, spec.kgSizeOfImage)

		src, kind, fp, _ := openProcessMemWith(100, f.sys())
		if src != nil || kind != memOpenNotReady || fp != "" {
			t.Fatalf("got src=%v kind=%v fp=%q，期望 nil / memOpenNotReady / 空串", src, kind, fp)
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

func TestReadSnapshotConsistentAndStrings(t *testing.T) {
	t.Run("内联字符串与负数偏移与取消关联", func(t *testing.T) {
		fix := newValidFixture(t)
		// std::string cap=15 内联
		fix.writeStdStringInline(fix.cfgBuf[0x198:0x1B8], []byte("1234567890ABCDE"), 15)

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
			ConfigMgrPtr:       fix.cfgMgrPtr,
			SongItemPtr:        0x000001AA00001000,
			CurrentSongHash:    "1234567890ABCDE",
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

	t.Run("堆分配std::string(cap=16/31)与std::wstring(cap=8/16)", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			hash  string
			sCap  uint64
			assoc string
			aCap  uint64
			lrc   string
			lCap  uint64
		}{
			{"cap16与cap8", "0123456789ABCDEF", 16, "12345678", 8, `C:\L\1.krc`, 16},
			{"cap31与cap16", "0123456789ABCDEF0123456789ABCDE", 31, `C:\L\用户取消关联.lrc`, 16, `D:\KuGou\Lyric\测试歌曲.krc`, 32},
			{"32位MD5哈希cap47与长路径", "0123456789ABCDEF0123456789ABCDEF", 47, `C:\Users\主播\Lyrics\用户取消关联.lrc`, 64, `D:\KuGou\Lyric\测试歌曲.krc`, 32},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fix := newValidFixture(t)
				fix.writeStdStringHeap(fix.cfgBuf[0x198:0x1B8], []byte(tc.hash), tc.sCap)
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
				if got.CurrentSongHash != tc.hash || got.AssocPath != tc.assoc || got.LyricFilePath != tc.lrc {
					t.Fatalf("字符串解码不符：hash=%q assoc=%q lrc=%q", got.CurrentSongHash, got.AssocPath, got.LyricFilePath)
				}
			})
		}
	})

	t.Run("内嵌NUL截断与不成对UTF-16代理项", func(t *testing.T) {
		fix := newValidFixture(t)
		fix.writeStdStringHeap(fix.cfgBuf[0x198:0x1B8], []byte("AABBCC\x00DDEEFF"), 31)
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
		if got.CurrentSongHash != "AABBCC" {
			t.Errorf("CurrentSongHash=%q，期望 AABBCC", got.CurrentSongHash)
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
		fix.writeStdStringInline(fix.cfgBuf[0x198:0x1B8], []byte("HASH1"), 15)

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

	t.Run("两遍之间改动CurrentSongHash返memStatusTorn", func(t *testing.T) {
		fix := newValidFixture(t)
		fix.writeStdStringInline(fix.cfgBuf[0x198:0x1B8], []byte("HASH_A"), 15)
		src, kind, _, err := openProcessMemWith(1, fix.space.sys())
		if err != nil || kind != memOpenOK {
			t.Fatalf("open failed: kind=%v err=%v", kind, err)
		}
		defer src.Close()

		cfgReads := 0
		fix.space.onRead = func(addr uintptr, size int) {
			if addr == fix.cfgMgrPtr && size == 0x1C0 {
				cfgReads++
				if cfgReads == 2 {
					fix.writeStdStringInline(fix.cfgBuf[0x198:0x1B8], []byte("HASH_B"), 15)
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
				name: "string_size大于cap",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1A8:0x1B0], 20)
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1B0:0x1B8], 15)
				},
			},
			{
				name: "string_size超上限64",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1A8:0x1B0], 65)
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1B0:0x1B8], 128)
				},
			},
			{
				name: "string_cap超上限4096",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1A8:0x1B0], 8)
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1B0:0x1B8], 4097)
				},
			},
			{
				name: "string堆指针小于0x10000",
				setup: func(fix *validFixture) {
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x198:0x1A0], 0x200)
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1A8:0x1B0], 16)
					binary.LittleEndian.PutUint64(fix.cfgBuf[0x1B0:0x1B8], 31)
				},
			},
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

	t.Run("三虚表校验不符返memStatusBadVtable", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(fix *validFixture)
		}{
			{"LyricManagerImpl主虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.implBuf[0x00:0x08], 0xDEAD0001)
			}},
			{"LyricManagerImpl次虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.implBuf[0x08:0x10], 0xDEAD0002)
			}},
			{"ILyricConfigManager虚表不符", func(fix *validFixture) {
				binary.LittleEndian.PutUint64(fix.cfgBuf[0x00:0x08], 0xDEAD0003)
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
				if _, st := src.ReadSnapshot(); st != memStatusBadVtable {
					t.Fatalf("status=%v，期望 memStatusBadVtable", st)
				}
			})
		}
	})

	t.Run("isShuttingDown或低位指针返memStatusNotReady", func(t *testing.T) {
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
			{"lyricStorage单例指针为0", func(fix *validFixture) {
				fix.space.writePtr(testKuGouBase+spec.lyricStorageRVA, 0)
			}},
			{"lyricStorage中间指针为1", func(fix *validFixture) {
				fix.space.writePtr(fix.storageHostPtr, 1)
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
