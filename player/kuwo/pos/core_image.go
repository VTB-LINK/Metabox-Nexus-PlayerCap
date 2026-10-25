package pos

import (
	"bytes"
	"encoding/hex"
)

// This file is build-constraint free so the PlayerCore structure match can be
// compiled and tested off Windows with a synthetic image. Only the process
// handle and Toolhelp calls (core_windows.go) are Windows-specific.
//
// PlayerCore is matched by structure, not by a pinned timestamp or fixed RVAs:
// resolve the exported GetPlayerCore to a singleton, confirm the read-position
// function is a virtual method of the singleton's own vtable, and bind to that
// object. GetPlayerCore is mov eax,<singleton>;ret. We inspect machine code; we
// never LoadLibrary, call a remote function, allocate or write remote memory.
var coreGetterCode, _ = hex.DecodeString("558bec51568b7508ba01000000c745fc000000008b868801000048741648740b4875268b8e94010000eb0e8b8e90010000eb068b8e8c01000085c9740c8b018d55fc5251ff50148bd08b450c8b4dfc89088bc2898ea00200005e8be55dc2")

const (
	scnMemExecute  = 0x20000000
	exportDirBytes = 40       // IMAGE_EXPORT_DIRECTORY header size
	maxExportBytes = 64 << 10 // export directory upper bound
	maxExportNames = 256      // NumberOfNames upper bound
	maxExportFuncs = 4096     // NumberOfFunctions upper bound
	maxExportName  = 64       // exported symbol length upper bound
	maxVtableSlots = 256      // vtable walk upper bound
	exportSymbol   = "GetPlayerCore"
	imageSizeMin   = 0x4000  // 16 KiB
	imageSizeMax   = 4 << 20 // 4 MiB
)

// coreImage is the result of a successful structure match.
type coreImage struct {
	singleton uintptr // absolute address of the PlayerCore singleton object
	vtable    uintptr // vtable pointer read from the singleton at open time
	slot      int     // vtable slot holding the read-position function
	timestamp uint32  // PE TimeDateStamp, for logging only
}

type execRange struct{ start, end uint64 }

// validator carries the bounds and executable regions parsed from the header,
// so every remote read is range-checked against this specific image.
type validator struct {
	read      memoryRead
	module    uintptr
	imageSize uint64
	exec      []execRange
}

// inImage reports whether [rva, rva+n) lies strictly within the image. The
// subtraction form avoids overflow for attacker-controlled offsets and counts.
func (v *validator) inImage(rva, n uint64) bool {
	return n > 0 && rva < v.imageSize && n <= v.imageSize-rva
}

// abs converts an absolute remote address to an RVA within this image.
func (v *validator) abs(addr uintptr) (uint64, bool) {
	a := uint64(addr)
	m := uint64(v.module)
	if a < m {
		return 0, false
	}
	return a - m, true
}

func (v *validator) inImageAbs(addr uintptr, n uint64) bool {
	rva, ok := v.abs(addr)
	return ok && v.inImage(rva, n)
}

func (v *validator) execContains(rva uint64) bool {
	for _, r := range v.exec {
		if rva >= r.start && rva < r.end {
			return true
		}
	}
	return false
}

// readImage reads from an RVA after range-checking it; a short read is an error.
func (v *validator) readImage(rva, n uint64) ([]byte, error) {
	if !v.inImage(rva, n) {
		return nil, ErrUnsupportedCore
	}
	b, err := v.read(v.module+uintptr(rva), int(n))
	if err != nil {
		return nil, err
	}
	if uint64(len(b)) != n {
		return nil, ErrUnsupportedCore
	}
	return b, nil
}

// validateCoreImage confirms the module is a PlayerCore build by structure and
// returns the bound singleton, its vtable value, the matched slot and the PE
// timestamp. Any structural mismatch returns ErrUnsupportedCore; a read failure
// returns the underlying error; an object not yet constructed returns the
// retryable ErrInvalidSample.
func validateCoreImage(read memoryRead, module uintptr, imageSize uint64) (coreImage, error) {
	if imageSize < imageSizeMin || imageSize > imageSizeMax {
		return coreImage{}, ErrUnsupportedCore
	}
	if uint64(module)+imageSize > 1<<32 {
		return coreImage{}, ErrUnsupportedCore
	}
	v := &validator{read: read, module: module, imageSize: imageSize}

	timestamp, exportRVA, exportSize, err := v.parseHeader()
	if err != nil {
		return coreImage{}, err
	}
	singleton, err := v.resolveSingleton(exportRVA, exportSize)
	if err != nil {
		return coreImage{}, err
	}
	vtable, slot, err := v.bindReader(singleton)
	if err != nil {
		return coreImage{}, err
	}
	return coreImage{singleton: singleton, vtable: vtable, slot: slot, timestamp: timestamp}, nil
}

// parseHeader validates the PE header page and the section table, returns the
// timestamp (logging only) and the export directory's RVA and size.
func (v *validator) parseHeader() (timestamp uint32, exportRVA, exportSize uint64, err error) {
	h, err := v.read(v.module, 0x1000)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(h) != 0x1000 {
		return 0, 0, 0, ErrUnsupportedCore
	}
	if string(h[:2]) != "MZ" {
		return 0, 0, 0, ErrUnsupportedCore
	}
	pe := uint64(getU32(h[0x3c:]))
	if pe < 0x40 || pe > 0x1000-0x108 {
		return 0, 0, 0, ErrUnsupportedCore
	}
	p := int(pe)
	if string(h[p:p+4]) != "PE\x00\x00" || getU16(h[p+4:]) != 0x14c {
		return 0, 0, 0, ErrUnsupportedCore
	}
	sizeOfOptional := getU16(h[p+20:])
	if sizeOfOptional < 0x70 {
		return 0, 0, 0, ErrUnsupportedCore
	}
	if getU16(h[p+24:]) != 0x10b { // optional header magic, PE32
		return 0, 0, 0, ErrUnsupportedCore
	}
	if getU32(h[p+116:]) < 1 { // NumberOfRvaAndSizes
		return 0, 0, 0, ErrUnsupportedCore
	}
	if uint64(getU32(h[p+80:])) != v.imageSize { // SizeOfImage == ModBaseSize
		return 0, 0, 0, ErrUnsupportedCore
	}
	timestamp = getU32(h[p+8:])

	numSections := int(getU16(h[p+6:]))
	if numSections < 1 || numSections > 16 {
		return 0, 0, 0, ErrUnsupportedCore
	}
	secStart := p + 24 + int(sizeOfOptional)
	if secStart+numSections*40 > len(h) {
		return 0, 0, 0, ErrUnsupportedCore
	}
	haveExec := false
	for i := 0; i < numSections; i++ {
		s := secStart + i*40
		length := uint64(getU32(h[s+8:])) // VirtualSize
		if length == 0 {
			length = uint64(getU32(h[s+16:])) // SizeOfRawData
		}
		rva := uint64(getU32(h[s+12:])) // VirtualAddress
		if !v.inImage(rva, length) {
			return 0, 0, 0, ErrUnsupportedCore
		}
		if getU32(h[s+36:])&scnMemExecute != 0 {
			v.exec = append(v.exec, execRange{start: rva, end: rva + length})
			haveExec = true
		}
	}
	if !haveExec {
		return 0, 0, 0, ErrUnsupportedCore
	}

	exportRVA = uint64(getU32(h[p+120:])) // DataDirectory[0].VirtualAddress
	exportSize = uint64(getU32(h[p+124:]))
	if !v.inImage(exportRVA, exportSize) || exportSize < exportDirBytes || exportSize > maxExportBytes {
		return 0, 0, 0, ErrUnsupportedCore
	}
	return timestamp, exportRVA, exportSize, nil
}

// resolveSingleton parses the export directory, binds the single GetPlayerCore
// export to mov eax,<singleton>;ret, and returns the relocated singleton address.
func (v *validator) resolveSingleton(exportRVA, exportSize uint64) (uintptr, error) {
	exp, err := v.readImage(exportRVA, exportSize)
	if err != nil {
		return 0, err
	}
	numFuncs := getU32(exp[20:])
	numNames := getU32(exp[24:])
	if numFuncs == 0 || numFuncs > maxExportFuncs || numNames == 0 || numNames > maxExportNames {
		return 0, ErrUnsupportedCore
	}
	funcsRVA := uint64(getU32(exp[28:]))
	namesRVA := uint64(getU32(exp[32:]))
	ordinalsRVA := uint64(getU32(exp[36:]))
	funcs, ok := v.exportTable(exp, exportRVA, exportSize, funcsRVA, uint64(numFuncs)*4)
	if !ok {
		return 0, ErrUnsupportedCore
	}
	names, ok := v.exportTable(exp, exportRVA, exportSize, namesRVA, uint64(numNames)*4)
	if !ok {
		return 0, ErrUnsupportedCore
	}
	ordinals, ok := v.exportTable(exp, exportRVA, exportSize, ordinalsRVA, uint64(numNames)*2)
	if !ok {
		return 0, ErrUnsupportedCore
	}

	matches, nameIndex := 0, -1
	for i := 0; i < int(numNames); i++ {
		name, ok := exportName(exp, exportRVA, exportSize, uint64(getU32(names[i*4:])))
		if !ok {
			return 0, ErrUnsupportedCore
		}
		if name == exportSymbol {
			matches++
			nameIndex = i
		}
	}
	if matches != 1 {
		return 0, ErrUnsupportedCore
	}
	ordinal := uint64(getU16(ordinals[nameIndex*2:]))
	if ordinal >= uint64(numFuncs) {
		return 0, ErrUnsupportedCore
	}
	funcRVA := uint64(getU32(funcs[ordinal*4:]))
	// Reject forwarded exports, whose "RVA" points back into the export directory.
	if funcRVA >= exportRVA && funcRVA < exportRVA+exportSize {
		return 0, ErrUnsupportedCore
	}
	entry, err := v.readImage(funcRVA, 6)
	if err != nil {
		return 0, err
	}
	if entry[0] != 0xb8 || entry[5] != 0xc3 {
		return 0, ErrUnsupportedCore
	}
	imm := getU32(entry[1:])
	if imm%4 != 0 {
		return 0, ErrUnsupportedCore
	}
	// The singleton object must lie wholly inside the mapped image, past the
	// header page, so its fields stay within readable memory.
	lo := uint64(v.module) + 0x1000
	if uint64(imm) < lo || uint64(imm)+coreSize > uint64(v.module)+v.imageSize {
		return 0, ErrUnsupportedCore
	}
	return uintptr(imm), nil
}

// exportTable range-checks a sub-table against both the image and the export
// directory block it must live inside, returning the slice of the block.
func (v *validator) exportTable(exp []byte, exportRVA, exportSize, tableRVA, n uint64) ([]byte, bool) {
	if !v.inImage(tableRVA, n) {
		return nil, false
	}
	if tableRVA < exportRVA || n > exportSize || tableRVA-exportRVA > exportSize-n {
		return nil, false
	}
	off := tableRVA - exportRVA
	return exp[off : off+n], true
}

// exportName reads a NUL-terminated name from inside the export directory,
// bounded by both the remaining directory bytes and the symbol-length cap.
func exportName(exp []byte, exportRVA, exportSize, nameRVA uint64) (string, bool) {
	if nameRVA < exportRVA || nameRVA >= exportRVA+exportSize {
		return "", false
	}
	off := nameRVA - exportRVA
	limit := exportSize - off
	if limit > maxExportName+1 {
		limit = maxExportName + 1
	}
	end := bytes.IndexByte(exp[off:off+limit], 0)
	if end < 0 {
		return "", false
	}
	return string(exp[off : off+uint64(end)]), true
}

// bindReader confirms the singleton's vtable holds the read-position function,
// proving the object layout (RID/duration/backend/position offsets) belongs to
// this class rather than a stray or neighbouring one. It returns the vtable
// value and the matched slot index. A zero vtable means the object is not yet
// constructed and is retryable.
func (v *validator) bindReader(singleton uintptr) (uintptr, int, error) {
	head, err := v.read(singleton, 4)
	if err != nil {
		return 0, 0, err
	}
	if len(head) != 4 {
		return 0, 0, ErrUnsupportedCore
	}
	vt := uintptr(getU32(head))
	if vt == 0 {
		return 0, 0, ErrInvalidSample
	}
	if uint64(vt)%4 != 0 || !v.inImageAbs(vt, 4) {
		return 0, 0, ErrUnsupportedCore
	}
	// MSVC places the RTTI "complete object locator" pointer at vtable-4; it is
	// a data pointer. Its presence is evidence that vt starts a real vtable.
	if !v.inImageAbs(vt-4, 4) {
		return 0, 0, ErrUnsupportedCore
	}
	rtti, err := v.read(vt-4, 4)
	if err != nil {
		return 0, 0, err
	}
	if len(rtti) != 4 {
		return 0, 0, ErrUnsupportedCore
	}
	rttiPtr := uintptr(getU32(rtti))
	rttiRVA, ok := v.abs(rttiPtr)
	if !ok || !v.inImage(rttiRVA, 1) || v.execContains(rttiRVA) {
		return 0, 0, ErrUnsupportedCore
	}

	vtRVA, _ := v.abs(vt)
	slots := (v.imageSize - vtRVA) / 4
	if slots > maxVtableSlots {
		slots = maxVtableSlots
	}
	if slots == 0 {
		return 0, 0, ErrUnsupportedCore
	}
	table, err := v.read(vt, int(slots)*4)
	if err != nil {
		return 0, 0, err
	}
	if uint64(len(table)) != slots*4 {
		return 0, 0, ErrUnsupportedCore
	}
	for i := 0; i < int(slots); i++ {
		target := uintptr(getU32(table[i*4:]))
		targetRVA, ok := v.abs(target)
		// The vtable ends at the first slot that is not a code pointer; MSVC
		// puts an RTTI data pointer before the next class, so the walk never
		// slides into a neighbouring vtable.
		if !ok || !v.execContains(targetRVA) {
			break
		}
		if !v.inImage(targetRVA, uint64(len(coreGetterCode))) {
			continue
		}
		code, err := v.read(target, len(coreGetterCode))
		if err != nil {
			return 0, 0, err
		}
		if bytes.Equal(code, coreGetterCode) {
			return vt, i, nil
		}
	}
	return 0, 0, ErrUnsupportedCore
}

// vtableStable rejects a singleton whose vtable pointer differs from open time;
// a zero pointer is a not-yet-constructed object and is retryable.
func vtableStable(got, remembered uintptr) error {
	switch got {
	case remembered:
		return nil
	case 0:
		return ErrInvalidSample
	default:
		return ErrUnsupportedCore
	}
}

func getU16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
