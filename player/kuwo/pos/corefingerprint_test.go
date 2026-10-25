package pos

// Structure match of PlayerCore, exercised with a synthetic image so it runs on
// any platform. The builder follows the independently observed UG1 layout; each
// field can be corrupted on its own to prove the matching guard, not a read
// failure, is what rejects. Out-of-image addresses are backed by readable
// memory for the same reason.

import (
	"encoding/binary"
	"errors"
	"testing"
)

const (
	tModule    = 0x10000000
	tImage     = 0x2f000
	tPE        = 0x80
	tSizeOpt   = 0xe0
	tSecStart  = tPE + 24 + tSizeOpt
	tTextRVA   = 0x1000
	tTextSize  = 0x1b5d8
	tRdataRVA  = 0x1d000
	tRdataSize = 0x7aac
	tDataRVA   = 0x25000
	tDataSize  = 0x3000
	tRelocRVA  = 0x2d000
	tRelocSize = 0x1000
	tExportRVA = 0x1d000
	tExportSz  = 0x100
	tFuncRVA   = 0x12890
	tSingleRVA = 0x25750
	tGetterRVA = 0x14e70
	tVtableRVA = 0x1e3c8
	tSlots     = 65
	tHitSlot   = 38
	tRTTIRVA   = 0x1d200
	tNameRVA   = tExportRVA + 50
)

var errFakeRead = errors.New("fake read failure")

type img struct {
	buf      []byte
	module   uintptr
	declared uint64
	failAt   uintptr
}

func (m *img) u32(rva uint64, val uint32) { binary.LittleEndian.PutUint32(m.buf[rva:], val) }
func (m *img) u16(rva uint64, val uint16) { binary.LittleEndian.PutUint16(m.buf[rva:], val) }
func (m *img) put(rva uint64, b []byte)   { copy(m.buf[rva:], b) }

func (m *img) read(addr uintptr, n int) ([]byte, error) {
	if m.failAt != 0 && addr == m.failAt {
		return nil, errFakeRead
	}
	if addr < m.module {
		return nil, errFakeRead
	}
	off := uint64(addr - m.module)
	if off+uint64(n) > uint64(len(m.buf)) {
		return nil, errFakeRead
	}
	out := make([]byte, n)
	copy(out, m.buf[off:off+uint64(n)])
	return out, nil
}

func (m *img) validate() (coreImage, error) {
	return validateCoreImage(m.read, m.module, m.declared)
}

// baseImage builds a structurally valid PlayerCore image at the default base.
func baseImage() *img { return baseImageAt(tModule, tImage) }

// baseImageAt builds a valid image at an arbitrary load base and image size, so
// the match can be shown to depend on structure rather than fixed addresses.
func baseImageAt(module uintptr, declared uint64) *img {
	m := &img{buf: make([]byte, declared+0x2000), module: module, declared: declared}
	m.put(0, []byte("MZ"))
	m.u32(0x3c, tPE)
	m.put(tPE, []byte("PE\x00\x00"))
	m.u16(tPE+4, 0x14c)             // Machine
	m.u16(tPE+6, 4)                 // NumberOfSections
	m.u32(tPE+8, 0x6a2641e4)        // TimeDateStamp (UG1; logging only)
	m.u16(tPE+20, tSizeOpt)         // SizeOfOptionalHeader
	m.u16(tPE+24, 0x10b)            // optional magic PE32
	m.u32(tPE+80, uint32(declared)) // SizeOfImage
	m.u32(tPE+116, 16)              // NumberOfRvaAndSizes
	m.u32(tPE+120, tExportRVA)
	m.u32(tPE+124, tExportSz)

	sections := []struct {
		rva, size uint64
		characts  uint32
	}{
		{tTextRVA, tTextSize, 0x60000020},   // CNT_CODE | MEM_EXECUTE | MEM_READ
		{tRdataRVA, tRdataSize, 0x40000040}, // read-only data
		{tDataRVA, tDataSize, 0xc0000040},
		{tRelocRVA, tRelocSize, 0x42000040},
	}
	for i, s := range sections {
		h := uint64(tSecStart + i*40)
		m.u32(h+8, uint32(s.size)) // VirtualSize
		m.u32(h+12, uint32(s.rva)) // VirtualAddress
		m.u32(h+16, uint32(s.size))
		m.u32(h+36, s.characts)
	}

	// Export directory: one named export, GetPlayerCore.
	m.u32(tExportRVA+20, 1)             // NumberOfFunctions
	m.u32(tExportRVA+24, 1)             // NumberOfNames
	m.u32(tExportRVA+28, tExportRVA+40) // AddressOfFunctions
	m.u32(tExportRVA+32, tExportRVA+44) // AddressOfNames
	m.u32(tExportRVA+36, tExportRVA+48) // AddressOfNameOrdinals
	m.u32(tExportRVA+40, tFuncRVA)      // function RVA
	m.u32(tExportRVA+44, tNameRVA)      // name RVA
	m.u16(tExportRVA+48, 0)             // ordinal
	m.put(tNameRVA, []byte("GetPlayerCore\x00"))

	base := uint32(module)
	// GetPlayerCore: mov eax,<singleton>; ret.
	m.put(tFuncRVA, []byte{0xb8})
	m.u32(tFuncRVA+1, base+tSingleRVA)
	m.put(tFuncRVA+5, []byte{0xc3})
	// The read-position function body.
	m.put(tGetterRVA, coreGetterCode)
	// Singleton: first dword is the vtable pointer.
	m.u32(tSingleRVA, base+tVtableRVA)
	// RTTI complete-object-locator pointer precedes the vtable, in data.
	m.u32(tVtableRVA-4, base+tRTTIRVA)
	for i := 0; i < tSlots; i++ {
		switch i {
		case tHitSlot:
			m.u32(uint64(tVtableRVA+i*4), base+tGetterRVA)
		case tSlots - 1:
			m.u32(uint64(tVtableRVA+i*4), base+tDataRVA) // non-code: ends the walk
		default:
			m.u32(uint64(tVtableRVA+i*4), base+tTextRVA) // code, but not the getter
		}
	}
	return m
}

// twoExports rewrites the export directory with two named exports, both bound to
// the GetPlayerCore function, in non-overlapping tables and strings. Only the
// names differ, so a rejection can come only from the name-uniqueness rule.
func twoExports(m *img, first, second string) {
	const (
		funcs = tExportRVA + 40
		names = tExportRVA + 48
		ords  = tExportRVA + 56
		str0  = tExportRVA + 64
		str1  = tExportRVA + 80
	)
	m.u32(tExportRVA+20, 2) // NumberOfFunctions
	m.u32(tExportRVA+24, 2) // NumberOfNames
	m.u32(tExportRVA+28, funcs)
	m.u32(tExportRVA+32, names)
	m.u32(tExportRVA+36, ords)
	m.u32(funcs, tFuncRVA)
	m.u32(funcs+4, tFuncRVA)
	m.u32(names, str0)
	m.u32(names+4, str1)
	m.u16(ords, 0)
	m.u16(ords+2, 1)
	m.put(str0, []byte(first+"\x00"))
	m.put(str1, []byte(second+"\x00"))
}

func TestCoreImageAcceptsVerifiedStructure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() *img
		slot  int
	}{
		{"baseline", baseImage, tHitSlot},
		{"different timestamp", func() *img { m := baseImage(); m.u32(tPE+8, 0xdeadbeef); return m }, tHitSlot},
		{"different image size", func() *img { return baseImageAt(tModule, 0x31000) }, tHitSlot},
		{"different load base", func() *img { return baseImageAt(0x02000000, tImage) }, tHitSlot},
		{"two exports, one is GetPlayerCore", func() *img { m := baseImage(); twoExports(m, "GetOtherCore", "GetPlayerCore"); return m }, tHitSlot},
		{"reader in other slot", func() *img {
			m := baseImage()
			m.u32(uint64(tVtableRVA+tHitSlot*4), tModule+tTextRVA)
			m.u32(uint64(tVtableRVA+10*4), tModule+tGetterRVA)
			return m
		}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.build()
			got, err := m.validate()
			if err != nil {
				t.Fatalf("rejected valid image: %v", err)
			}
			if got.slot != tc.slot {
				t.Fatalf("slot=%d want %d", got.slot, tc.slot)
			}
			if got.singleton != m.module+tSingleRVA || got.vtable != m.module+tVtableRVA {
				t.Fatalf("binding=%+v", got)
			}
		})
	}
}

// Every case corrupts one field of an otherwise valid image.
// 变异自证（隔离副本，实测）：在副本里放松受检的守卫，对应用例即变红——或改报
// 另一个错误、或被 go test 捕获的 panic；逐条试过的守卫都变红。唯一的例外是 V 的
// 4 字节对齐那一行：它是早退冗余，去掉后不对齐的 V 仍被后续虚表遍历拒绝。
func TestCoreImageRejectsStructuralMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*img)
		want   error
	}{
		{"not MZ", func(m *img) { m.put(0, []byte("XY")) }, ErrUnsupportedCore},
		{"bad PE signature", func(m *img) { m.put(tPE, []byte("PX\x00\x00")) }, ErrUnsupportedCore},
		{"machine not i386", func(m *img) { m.u16(tPE+4, 0x8664) }, ErrUnsupportedCore},
		{"optional magic not PE32", func(m *img) { m.u16(tPE+24, 0x20b) }, ErrUnsupportedCore},
		{"e_lfanew too small", func(m *img) { m.u32(0x3c, 0x20) }, ErrUnsupportedCore},
		{"e_lfanew huge", func(m *img) { m.u32(0x3c, 0x7fffffff) }, ErrUnsupportedCore},
		{"e_lfanew past bound", func(m *img) { m.u32(0x3c, 0x1000-0x108+1) }, ErrUnsupportedCore},
		{"optional header too small", func(m *img) { m.u16(tPE+20, 0x6f) }, ErrUnsupportedCore},
		{"no data directories", func(m *img) { m.u32(tPE+116, 0) }, ErrUnsupportedCore},
		{"size of image mismatch", func(m *img) { m.u32(tPE+80, tImage+1) }, ErrUnsupportedCore},
		{"image size too small", func(m *img) { m.declared = 0x3fff; m.u32(tPE+80, 0x3fff) }, ErrUnsupportedCore},
		{"image size too large", func(m *img) { m.declared = (4 << 20) + 1 }, ErrUnsupportedCore},
		{"no sections", func(m *img) { m.u16(tPE+6, 0) }, ErrUnsupportedCore},
		{"too many sections", func(m *img) { m.u16(tPE+6, 17) }, ErrUnsupportedCore},
		{"section table past header", func(m *img) { m.u16(tPE+20, 0xf00) }, ErrUnsupportedCore},
		{"section past image", func(m *img) { m.u32(tSecStart+3*40+8, 0x4000) }, ErrUnsupportedCore},
		{"no executable section", func(m *img) { m.u32(tSecStart+36, 0x40000040) }, ErrUnsupportedCore},
		{"export missing", func(m *img) { m.u32(tPE+120, 0); m.u32(tPE+124, 0) }, ErrUnsupportedCore},
		{"export dir out of image", func(m *img) { m.u32(tPE+120, tImage+0x100) }, ErrUnsupportedCore},
		{"export dir too small", func(m *img) { m.u32(tPE+124, 39) }, ErrUnsupportedCore},
		{"number of names over limit", func(m *img) { m.u32(tExportRVA+24, 0x40000001) }, ErrUnsupportedCore},
		{"number of functions over limit", func(m *img) { m.u32(tExportRVA+20, 0x40000001) }, ErrUnsupportedCore},
		{"name table out of image", func(m *img) { m.u32(tExportRVA+32, tImage+0x100) }, ErrUnsupportedCore},
		{"func table outside dir", func(m *img) { m.u32(tExportRVA+28, tExportRVA-8) }, ErrUnsupportedCore},
		{"name missing", func(m *img) { m.put(tNameRVA, []byte("GetOtherCore\x00")) }, ErrUnsupportedCore},
		// Two valid, distinct GetPlayerCore entries (the layout is proven valid by the
		// accept case above): only the uniqueness rule may reject it. 变异自证：把
		// core_image.go 的 matches != 1 放宽为 matches == 0，本用例变红（实测）。
		{"name duplicate", func(m *img) { twoExports(m, "GetPlayerCore", "GetPlayerCore") }, ErrUnsupportedCore},
		{"name no terminator", func(m *img) {
			for i := uint64(0); i < 65; i++ {
				m.buf[tNameRVA+i] = 'A'
			}
		}, ErrUnsupportedCore},
		{"name runs off directory", func(m *img) {
			nr := uint64(tExportRVA + tExportSz - 5)
			m.u32(tExportRVA+44, uint32(nr))
			for i := uint64(0); i < 5; i++ {
				m.buf[nr+i] = 'A'
			}
		}, ErrUnsupportedCore},
		{"ordinal out of range", func(m *img) { m.u16(tExportRVA+48, 1) }, ErrUnsupportedCore},
		{"forwarded export", func(m *img) {
			// A real mov-ret inside the export directory: only the forwarded-export
			// guard (RVA within the directory) may reject it.
			m.u32(tExportRVA+40, tExportRVA+0x80)
			m.put(tExportRVA+0x80, []byte{0xb8})
			m.u32(tExportRVA+0x81, tModule+tSingleRVA)
			m.put(tExportRVA+0x85, []byte{0xc3})
		}, ErrUnsupportedCore},
		{"entry past image", func(m *img) { m.u32(tExportRVA+40, tImage-1) }, ErrUnsupportedCore},
		{"entry not mov-ret", func(m *img) { m.put(tFuncRVA, []byte{0xe9}) }, ErrUnsupportedCore},
		{"singleton misaligned", func(m *img) { m.u32(tFuncRVA+1, tModule+tSingleRVA+1) }, ErrUnsupportedCore},
		{"singleton below header", func(m *img) { m.u32(tFuncRVA+1, tModule+0x500) }, ErrUnsupportedCore},
		{"singleton past image", func(m *img) { m.u32(tFuncRVA+1, tModule+tImage-4) }, ErrUnsupportedCore},
		{"getter present but not in vtable", func(m *img) { m.u32(uint64(tVtableRVA+tHitSlot*4), tModule+tTextRVA) }, ErrUnsupportedCore},
		{"adjacent old table after rtti", func(m *img) {
			m.u32(uint64(tVtableRVA+0*4), tModule+tTextRVA)
			m.u32(uint64(tVtableRVA+1*4), tModule+tTextRVA)
			m.u32(uint64(tVtableRVA+2*4), tModule+tRTTIRVA) // data pointer: ends walk
			m.u32(uint64(tVtableRVA+3*4), tModule+tGetterRVA)
			m.u32(uint64(tVtableRVA+tHitSlot*4), tModule+tTextRVA)
		}, ErrUnsupportedCore},
		{"vtable minus four out of image", func(m *img) { m.u32(tSingleRVA, tModule) }, ErrUnsupportedCore},
		{"rtti pointer in exec", func(m *img) { m.u32(tVtableRVA-4, tModule+tGetterRVA) }, ErrUnsupportedCore},
		{"rtti pointer out of image", func(m *img) { m.u32(tVtableRVA-4, tModule+tImage+0x10) }, ErrUnsupportedCore},
		{"empty table: first slot not code", func(m *img) {
			for i := 0; i < tSlots; i++ {
				m.u32(uint64(tVtableRVA+i*4), tModule+tDataRVA)
			}
		}, ErrUnsupportedCore},
		{"slot code off by one", func(m *img) { m.buf[tGetterRVA+10]++ }, ErrUnsupportedCore},
		{"vtable zero", func(m *img) { m.u32(tSingleRVA, 0) }, ErrInvalidSample},
		{"vtable out of image", func(m *img) { m.u32(tSingleRVA, tModule+tImage+4) }, ErrUnsupportedCore},
		{"vtable misaligned", func(m *img) { m.u32(tSingleRVA, tModule+tVtableRVA+1) }, ErrUnsupportedCore},
		{"vtable near image end truncates", func(m *img) {
			vt := uint64(tImage - 0x10)
			m.u32(tSingleRVA, uint32(tModule+vt))
			m.u32(vt-4, tModule+tRTTIRVA)
			for i := uint64(0); i < 4; i++ {
				m.u32(vt+i*4, tModule+tTextRVA)
			}
		}, ErrUnsupportedCore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := baseImage()
			tc.mutate(m)
			if _, err := m.validate(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

// An out-of-image export name array is rejected even though that address is
// readable; and a read failure propagates rather than being swallowed.
func TestCoreImageReadFailuresPropagateWithoutPanic(t *testing.T) {
	m := baseImage()
	m.failAt = tModule + tVtableRVA - 4 // RTTI read fails
	if _, err := m.validate(); !errors.Is(err, errFakeRead) {
		t.Fatalf("read failure not propagated: %v", err)
	}
	m = baseImage()
	m.failAt = tModule // header read fails
	if _, err := m.validate(); !errors.Is(err, errFakeRead) {
		t.Fatalf("header read failure not propagated: %v", err)
	}
}

func TestVtableStability(t *testing.T) {
	const remembered = 0x1234
	if err := vtableStable(remembered, remembered); err != nil {
		t.Fatalf("stable vtable rejected: %v", err)
	}
	if err := vtableStable(0x5678, remembered); !errors.Is(err, ErrUnsupportedCore) {
		t.Fatalf("changed vtable: %v", err)
	}
	if err := vtableStable(0, remembered); !errors.Is(err, ErrInvalidSample) {
		t.Fatalf("zeroed vtable: %v", err)
	}
}
