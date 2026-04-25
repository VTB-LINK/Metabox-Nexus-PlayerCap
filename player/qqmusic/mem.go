package qqmusic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	modkernel32          = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32 = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First   = modkernel32.NewProc("Process32FirstW")
	procProcess32Next    = modkernel32.NewProc("Process32NextW")
	procModule32First    = modkernel32.NewProc("Module32FirstW")
	procModule32Next     = modkernel32.NewProc("Module32NextW")
	procOpenProcess      = modkernel32.NewProc("OpenProcess")
	procCloseHandle      = modkernel32.NewProc("CloseHandle")
	procReadProcessMemory= modkernel32.NewProc("ReadProcessMemory")
	procWriteProcessMemory = modkernel32.NewProc("WriteProcessMemory")
	procVirtualAllocEx   = modkernel32.NewProc("VirtualAllocEx")
	procVirtualProtectEx = modkernel32.NewProc("VirtualProtectEx")

	modpsapi                 = syscall.NewLazyDLL("psapi.dll")
	procEnumProcessModulesEx = modpsapi.NewProc("EnumProcessModulesEx")
	procGetModuleBaseNameW   = modpsapi.NewProc("GetModuleBaseNameW")
	procGetModuleInformation = modpsapi.NewProc("GetModuleInformation")

	procVirtualQueryEx       = modkernel32.NewProc("VirtualQueryEx")
	procGetTickCount         = modkernel32.NewProc("GetTickCount")
	procGetProcAddress       = modkernel32.NewProc("GetProcAddress")
	procGetModuleHandleW     = modkernel32.NewProc("GetModuleHandleW")
)

const (
	PROCESS_ALL_ACCESS = 0x1F0FFF
	TH32CS_SNAPPROCESS = 0x00000002
	TH32CS_SNAPMODULE  = 0x00000008
	TH32CS_SNAPMODULE32= 0x00000010
	MEM_COMMIT         = 0x1000
	MEM_RESERVE        = 0x2000
	PAGE_EXECUTE_READWRITE = 0x40
	PAGE_READWRITE     = 0x04
	LIST_MODULES_ALL   = 0x03
)

type MEMORY_BASIC_INFORMATION struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

type MODULEINFO struct {
	LpBaseOfDll unsafe.Pointer
	SizeOfImage uint32
	EntryPoint  unsafe.Pointer
}

type PROCESSENTRY32W struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

type MODULEENTRY32W struct {
	Size         uint32
	ModuleID     uint32
	ProcessID    uint32
	GlblcntUsage uint32
	ProccntUsage uint32
	ModBaseAddr  *byte
	ModBaseSize  uint32
	Module       [256]uint16
	ExePath      [260]uint16
}

type QQMusicMem struct {
	pid            uint32
	hProcess       uintptr
	qqmusicDllBase uintptr
	gfWrapperBase  uintptr
	gfWrapperSize  uint32
	kernel32Base   uintptr // kernel32.dll base in the target 32-bit process
	sliderPointer  uintptr // Dynamic memory cave address where EDI is stored
	progressPtr    uintptr // Address where hooked progress (ms) is stored
	progressTsPtr  uintptr // Address where GetTickCount timestamp is stored
}

func (m *QQMusicMem) CheckValid() bool {
	if m.hProcess == 0 {
		return false
	}
	var exitCode uint32
	syscall.GetExitCodeProcess(syscall.Handle(m.hProcess), &exitCode)
	if exitCode != 259 { // STILL_ACTIVE is 259
		m.hProcess = 0
		return false
	}
	return true
}

func ConnectQQMusic() (*QQMusicMem, error) {
	mem := &QQMusicMem{}
	
	// 1. Find process ID
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPPROCESS), 0)
	if hSnap == uintptr(syscall.InvalidHandle) {
		return nil, errors.New("CreateToolhelp32Snapshot failed")
	}
	defer procCloseHandle.Call(hSnap)

	var pe32 PROCESSENTRY32W
	pe32.Size = uint32(unsafe.Sizeof(pe32))

	var pids []uint32
	ret, _, _ := procProcess32First.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	for ret != 0 {
		name := syscall.UTF16ToString(pe32.ExeFile[:])
		if strings.ToLower(name) == "qqmusic.exe" {
			pids = append(pids, pe32.ProcessID)
		}
		ret, _, _ = procProcess32Next.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	}

	if len(pids) == 0 {
		return nil, errors.New("QQMusic.exe not found")
	}

	for _, pid := range pids {
		hProcess, _, _ := procOpenProcess.Call(uintptr(PROCESS_ALL_ACCESS), 0, uintptr(pid))
		if hProcess == 0 {
			continue
		}

		var modules [1024]uintptr
		var cbNeeded uint32

		retEnum, _, _ := procEnumProcessModulesEx.Call(
			hProcess,
			uintptr(unsafe.Pointer(&modules[0])),
			uintptr(unsafe.Sizeof(modules)),
			uintptr(unsafe.Pointer(&cbNeeded)),
			uintptr(LIST_MODULES_ALL),
		)

		foundDll := false
		if retEnum != 0 {
			numModules := cbNeeded / uint32(unsafe.Sizeof(modules[0]))
			for i := uint32(0); i < numModules; i++ {
				var name [256]uint16
				procGetModuleBaseNameW.Call(hProcess, modules[i], uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
				modName := syscall.UTF16ToString(name[:])
				lowerName := strings.ToLower(modName)

				if lowerName == "qqmusic.dll" {
					mem.qqmusicDllBase = modules[i]
					foundDll = true
				}
				if lowerName == "qqmusic_gfwrapper.dll" {
					mem.gfWrapperBase = modules[i]
					var minfo MODULEINFO
					procGetModuleInformation.Call(hProcess, modules[i], uintptr(unsafe.Pointer(&minfo)), uintptr(unsafe.Sizeof(minfo)))
					mem.gfWrapperSize = minfo.SizeOfImage
				}
				if lowerName == "kernel32.dll" {
					mem.kernel32Base = modules[i]
				}
			}
		}

		if foundDll {
			mem.pid = pid
			mem.hProcess = hProcess
			return mem, nil
		}
		procCloseHandle.Call(hProcess)
	}

	return nil, errors.New("QQMusic.dll not found in any QQMusic.exe process")
}

func (m *QQMusicMem) ReadUint32(addr uintptr) uint32 {
	var val uint32
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&val)), 4, uintptr(unsafe.Pointer(&bytesRead)))
	return val
}

func (m *QQMusicMem) ReadFloat32(addr uintptr) float32 {
	var val float32
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&val)), 4, uintptr(unsafe.Pointer(&bytesRead)))
	return val
}

func (m *QQMusicMem) ReadBytes(addr uintptr, size uint32) []byte {
	buf := make([]byte, size)
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&bytesRead)))
	return buf
}

func (m *QQMusicMem) WriteBytes(addr uintptr, data []byte) bool {
	var bytesWritten uintptr
	ret, _, _ := procWriteProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&bytesWritten)))
	return ret != 0
}

func (m *QQMusicMem) ReadString(addr uintptr, maxLen uint32) string {
	buf := m.ReadBytes(addr, maxLen)
	idx := bytes.IndexByte(buf, 0)
	if idx != -1 {
		buf = buf[:idx]
	}
	return string(buf)
}

func (m *QQMusicMem) ReadWideString(addr uintptr, maxLen uint32) string {
	buf := m.ReadBytes(addr, maxLen*2)
	u16 := make([]uint16, len(buf)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(buf[i*2:])
		if u16[i] == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

// aobMatch scans data for pattern with optional mask (nil mask = exact match)
func aobMatch(data, pattern, mask []byte) int {
	pLen := len(pattern)
	for i := 0; i <= len(data)-pLen; i++ {
		matched := true
		for j := 0; j < pLen; j++ {
			if mask != nil && mask[j] == 0x00 {
				continue // wildcard
			}
			if data[i+j] != pattern[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

// AOB Injection for Accompaniment Slider
func (m *QQMusicMem) InjectSliderAOB() error {
	if m.gfWrapperBase == 0 {
		return errors.New("QQMusic_GFWrapper.dll not found")
	}
	// Pattern: 39 86 F0000000 74 ?? 8B CE 89 86 F0000000
	// New version: cmp [esi+F0],eax / je +XX / mov ecx,esi / mov [esi+F0],eax
	// We target the 'mov [esi+F0],eax' at offset +10 from pattern start
	patterns := []struct{
		pat []byte
		mask []byte // 0xFF = exact match, 0x00 = wildcard
		writeOff int
		stolenBytes []byte // original bytes at the write instruction
		captureReg byte // register index for 'mov [ptr], reg' in codecave
	}{
		{
			// Current version: cmp [esi+F0],eax / je ?? / mov ecx,esi / mov [esi+F0],eax
			pat:  []byte{0x39, 0x86, 0xF0, 0x00, 0x00, 0x00, 0x74, 0x00, 0x8B, 0xCE, 0x89, 0x86, 0xF0, 0x00, 0x00, 0x00},
			mask: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			writeOff: 10, // offset to 'mov [esi+F0],eax'
			stolenBytes: []byte{0x89, 0x86, 0xF0, 0x00, 0x00, 0x00}, // mov [esi+F0],eax
			captureReg: 0x35, // 'mov [addr], esi' -> 89 35 (capture esi = this pointer)
		},
		{
			// Legacy version: cmp esi,eax / cmovle esi,eax / mov [edi+F0],esi / pop edi / pop esi
			pat:  []byte{0x3B, 0xC6, 0x0F, 0x4E, 0xF0, 0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00, 0x5F, 0x5E},
			mask: nil, // all exact
			writeOff: 5, // offset to 'mov [edi+F0],esi'
			stolenBytes: []byte{0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00}, // mov [edi+F0],esi
			captureReg: 0x3D, // 'mov [addr], edi' -> 89 3D (capture edi = this pointer)
		},
	}

	chunkSize := uint32(m.gfWrapperSize)
	moduleData := m.ReadBytes(m.gfWrapperBase, chunkSize)

	var targetAddr uintptr
	var matchedPattern int = -1
	for pi, p := range patterns {
		offset := aobMatch(moduleData, p.pat, p.mask)
		if offset != -1 {
			targetAddr = m.gfWrapperBase + uintptr(offset) + uintptr(p.writeOff)
			matchedPattern = pi
			break
		}
	}
	if matchedPattern == -1 {
		return errors.New("aob pattern not found in memory (might be already injected)")
	}

	log.Info("AOB 滑块目标地址: 0x%X (模式 %d)", targetAddr, matchedPattern)

	// Check if already hooked
	firstByte := m.ReadBytes(targetAddr, 1)
	if len(firstByte) > 0 && firstByte[0] == 0xE9 {
		log.Info("滑块 Hook 已激活")
		return nil
	}

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return errors.New("VirtualAllocEx failed")
	}

	m.sliderPointer = caveAddr + 0x100
	log.Detail("Codecave @ 0x%X, 指针 @ 0x%X", caveAddr, m.sliderPointer)

	// Build assembly
	buf := new(bytes.Buffer)
	
	pat := patterns[matchedPattern]

	// Create "mov [m.sliderPointer], reg" to capture the object pointer
	buf.Write([]byte{0x89, pat.captureReg})
	binary.Write(buf, binary.LittleEndian, uint32(m.sliderPointer))
	
	// Write the original stolen bytes
	buf.Write(pat.stolenBytes)
	
	// calculate JMP rel32 back to targetAddr + 6
	returnAddr := targetAddr + 6
	caveJmpAddr := caveAddr + uintptr(buf.Len())
	rel32Back := uint32(returnAddr - caveJmpAddr - 5)
	
	buf.WriteByte(0xE9)
	binary.Write(buf, binary.LittleEndian, rel32Back)

	// Write codecave payload
	m.WriteBytes(caveAddr, buf.Bytes())

	// Unprotect target memory to write JMP
	var oldProtect uint32
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, PAGE_EXECUTE_READWRITE, uintptr(unsafe.Pointer(&oldProtect)))

	// Write JMP to target
	jmpBuf := new(bytes.Buffer)
	jmpBuf.WriteByte(0xE9) // JMP
	rel32Target := uint32(caveAddr - targetAddr - 5)
	binary.Write(jmpBuf, binary.LittleEndian, rel32Target)
	jmpBuf.WriteByte(0x90) // NOP, because original was 6 bytes, JMP is 5 bytes

	m.WriteBytes(targetAddr, jmpBuf.Bytes())
	
	// Restore protect
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, uintptr(oldProtect), uintptr(unsafe.Pointer(&oldProtect)))
	
	log.Info("滑块 AOB Hook 注入成功")
	return nil
}

// InjectProgressAOB hooks the progress write instruction (QQMusic.dll+488B75:
// mov [eax+1AC], esi) to also save ESI (progress ms) and GetTickCount() to
// fixed addresses. This enables precise local-clock interpolation.
//
// Codecave assembly:
//   mov [pProgressMs], esi     ; save progress value
//   pushad                      ; save all registers
//   call GetTickCount           ; EAX = wall-clock ms
//   mov [pTimeStamp], eax       ; save timestamp
//   popad                       ; restore registers
//   mov [eax+1AC], esi          ; original stolen bytes
//   jmp returnAddr
func (m *QQMusicMem) InjectProgressAOB() error {
	// AOB context: E8 ?? ?? ?? ?? 89 45 E8 | 89 B0 AC 01 00 00 | 8B F0
	// We hook the 6-byte instruction: 89 B0 AC 01 00 00 = mov [eax+000001AC], esi
	targetOffset := uintptr(0x488B75)
	targetAddr := m.qqmusicDllBase + targetOffset

	// Verify the target bytes
	expect := []byte{0x89, 0xB0, 0xAC, 0x01, 0x00, 0x00}
	actual := m.ReadBytes(targetAddr, 6)
	if !bytes.Equal(actual, expect) {
		// Check if already hooked (first byte = JMP = 0xE9)
		if len(actual) > 0 && actual[0] == 0xE9 {
			log.Info("进度 Hook 已激活")
			return nil
		}
		return fmt.Errorf("unexpected bytes at dll+%X: got %X, want %X", targetOffset, actual, expect)
	}

	// Resolve tick count: use KUSER_SHARED_DATA (fixed at 0x7FFE0000 in all Windows processes).
	// This avoids needing to resolve GetTickCount across 32/64 bit process boundaries.
	// TickCount.LowPart = [0x7FFE0320], TickCountMultiplier = [0x7FFE0004]
	// Result = (LowPart * Multiplier) >> 24 = milliseconds
	log.Detail("使用 KUSER_SHARED_DATA 获取 tick count")

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return errors.New("VirtualAllocEx failed for progress hook")
	}

	// Data area at cave+0x100 (progressMs) and cave+0x104 (timestamp)
	m.progressPtr = caveAddr + 0x100
	m.progressTsPtr = caveAddr + 0x104
	log.Detail("进度 Hook Cave @ 0x%X, progressPtr @ 0x%X, tsPtr @ 0x%X",
		caveAddr, m.progressPtr, m.progressTsPtr)

	// Build codecave assembly
	buf := new(bytes.Buffer)

	// mov [pProgressMs], esi  →  89 35 <addr32>
	buf.Write([]byte{0x89, 0x35})
	binary.Write(buf, binary.LittleEndian, uint32(m.progressPtr))

	// pushad → 60 (save all registers)
	buf.WriteByte(0x60)

	// Inline GetTickCount via KUSER_SHARED_DATA:
	// mov eax, [0x7FFE0320]    ; TickCount.LowPart    → A1 20 03 FE 7F
	buf.Write([]byte{0xA1, 0x20, 0x03, 0xFE, 0x7F})
	// mov edx, [0x7FFE0004]    ; TickCountMultiplier   → 8B 15 04 00 FE 7F
	buf.Write([]byte{0x8B, 0x15, 0x04, 0x00, 0xFE, 0x7F})
	// mul edx                  ; EDX:EAX = LowPart * Multiplier → F7 E2
	buf.Write([]byte{0xF7, 0xE2})
	// shrd eax, edx, 24        ; EAX = (result >> 24)  → 0F AC D0 18
	buf.Write([]byte{0x0F, 0xAC, 0xD0, 0x18})

	// mov [pTimeStamp], eax  →  A3 <addr32>
	buf.WriteByte(0xA3)
	binary.Write(buf, binary.LittleEndian, uint32(m.progressTsPtr))

	// popad → 61 (restore all registers)
	buf.WriteByte(0x61)

	// stolen bytes: mov [eax+000001AC], esi  →  89 B0 AC 01 00 00
	buf.Write(expect)

	// jmp returnAddr  →  E9 <rel32>
	returnAddr := targetAddr + 6
	jmpSite := caveAddr + uintptr(buf.Len())
	rel32Back := uint32(returnAddr - jmpSite - 5)
	buf.WriteByte(0xE9)
	binary.Write(buf, binary.LittleEndian, rel32Back)

	// Write codecave
	m.WriteBytes(caveAddr, buf.Bytes())

	// Unprotect target and write JMP
	var oldProtect uint32
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, PAGE_EXECUTE_READWRITE, uintptr(unsafe.Pointer(&oldProtect)))

	jmpBuf := new(bytes.Buffer)
	jmpBuf.WriteByte(0xE9) // JMP rel32
	rel32Target := uint32(caveAddr - targetAddr - 5)
	binary.Write(jmpBuf, binary.LittleEndian, rel32Target)
	jmpBuf.WriteByte(0x90) // NOP pad (6 bytes original, 5 byte JMP)

	m.WriteBytes(targetAddr, jmpBuf.Bytes())

	// Restore protection
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, uintptr(oldProtect), uintptr(unsafe.Pointer(&oldProtect)))

	log.Info("进度 Hook 注入成功")
	return nil
}

type SongMetadata struct {
	Name       string
	Singer     string
	SongID     uint32
	ProgressMs uint32
	DurationMs uint32
	SliderVal  uint32
}

func (m *QQMusicMem) ReadSSOString(addr uintptr) string {
	// SSO String is 24 bytes (0x18)
	// +0x10 : uint32 len
	// +0x14 : uint32 cap
	buf := m.ReadBytes(addr, 24)
	if len(buf) < 24 {
		return ""
	}
	
	strLen := binary.LittleEndian.Uint32(buf[0x10:0x14])
	strCap := binary.LittleEndian.Uint32(buf[0x14:0x18])

	if strCap > 15 {
		// It's a pointer at +0x00
		ptr := binary.LittleEndian.Uint32(buf[0x00:0x04])
		if ptr == 0 || strLen == 0 || strLen > 2048 {
			return ""
		}
		strBytes := m.ReadBytes(uintptr(ptr), strLen)
		return string(strBytes)
	}

	// Inline string
	if strLen > 15 {
		return ""
	}
	return string(buf[:strLen])
}

func extractString(m *QQMusicMem, buf []byte) string {
	if len(buf) < 24 {
		return ""
	}
	strLen := binary.LittleEndian.Uint32(buf[0x10:0x14])
	strCap := binary.LittleEndian.Uint32(buf[0x14:0x18])

	// Check if pointer
	if strCap > 15 && strCap < 0xFFFFFF && strLen > 0 && strLen < 4096 {
		ptr := binary.LittleEndian.Uint32(buf[0x00:0x04])
		if ptr > 0x10000 {
			s := m.ReadBytes(uintptr(ptr), strLen)
			return string(s)
		}
	}
	
	// Check if completely inline
	idx := bytes.IndexByte(buf, 0)
	if idx > 0 {
		return string(buf[:idx])
	}
	return ""
}

func sanitizeString(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 { // only printable characters
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

var lastCachedCookie string
var lastCookieTime time.Time

func (m *QQMusicMem) FindCookie() string {
	if lastCachedCookie != "" && time.Since(lastCookieTime) < 5*time.Minute {
		return lastCachedCookie
	}

	var mbi MEMORY_BASIC_INFORMATION
	addr := uintptr(0)
	pattern := []byte("qm_keyst=")

	for {
		ret, _, _ := procVirtualQueryEx.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Sizeof(mbi)))
		if ret == 0 {
			break
		}

		if mbi.State == MEM_COMMIT && (mbi.Protect == PAGE_READWRITE || mbi.Protect == PAGE_EXECUTE_READWRITE) {
			buf := make([]byte, mbi.RegionSize)
			var bytesRead uintptr
			procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), mbi.RegionSize, uintptr(unsafe.Pointer(&bytesRead)))
			if bytesRead > 0 {
				idx := bytes.Index(buf[:bytesRead], pattern)
				if idx != -1 {
					// We found "qm_keyst=" ! Parse until ';' or '\0'
					start := idx
					end := start
					for end < int(bytesRead) && buf[end] != 0 && buf[end] != ';' && buf[end] != ' ' {
						end++
					}
					// Actually, let's grab the whole block like CheatEngine showed:
					// "qqmusic_key=...; qqmusic_uin=...; qm_keyst=..."
					// Let's just find the first \0 after idx and stringify the whole thing if it contains qqmusic_key.
					
					// Safest approach: Extract the full string starting from whatever looks like a cookie block.
					// We can walk left to see where it started.
					left := start
					for left > 0 && buf[left-1] != 0 && (buf[left-1] >= 32 && buf[left-1] <= 126) {
						left--
					}
					right := start
					for right < int(bytesRead) && buf[right] != 0 && (buf[right] >= 32 && buf[right] <= 126) {
						right++
					}
					
					// Full string
					cookieStr := string(buf[left:right])
					if strings.Contains(cookieStr, "qqmusic_key=") || strings.Contains(cookieStr, "qm_keyst=") {
						lastCachedCookie = cookieStr
						lastCookieTime = time.Now()
						log.Detail("提取到 Cookie: %s...", cookieStr[:min(50, len(cookieStr))])

						return cookieStr
					}
				}
			}
		}
		
		addr = mbi.BaseAddress + mbi.RegionSize
		// Keep searching reasonably
		if addr > 0x7FFFFFFF {
			break
		}
	}
	return ""
}

// FindSongMid extracts the current song's MID directly from QQ Music's internal
// JSON cache in process memory. CE analysis confirmed the JSON block format:
//   "remainingTime" : 182846,
//   "songPlayTime" : 183000,    ← this matches meta.DurationMs
//   "songmid" : "000mDR751jtpPf",
// We match songPlayTime against the known duration to identify the correct block.
func (m *QQMusicMem) FindSongMid(durationMs uint32) string {
	if durationMs == 0 {
		return ""
	}

	var mbi MEMORY_BASIC_INFORMATION
	addr := uintptr(0)
	pattern := []byte(`"songmid" : "`)
	// We also search for songPlayTime near the songmid to validate
	playTimePattern := []byte(fmt.Sprintf(`"songPlayTime" : %d`, durationMs))

	for {
		ret, _, _ := procVirtualQueryEx.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Sizeof(mbi)))
		if ret == 0 {
			break
		}

		if mbi.State == MEM_COMMIT && (mbi.Protect == PAGE_READWRITE || mbi.Protect == PAGE_EXECUTE_READWRITE) && mbi.RegionSize < 100*1024*1024 {
			buf := make([]byte, mbi.RegionSize)
			var bytesRead uintptr
			procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), mbi.RegionSize, uintptr(unsafe.Pointer(&bytesRead)))
			if bytesRead > 0 {
				data := buf[:bytesRead]
				// First check if this region even has our songPlayTime
				if !bytes.Contains(data, playTimePattern) {
					addr = mbi.BaseAddress + mbi.RegionSize
					if addr > 0x7FFFFFFF {
						break
					}
					continue
				}
				// Now search for songmid patterns near the playTime match
				searchBuf := data
				for {
					idx := bytes.Index(searchBuf, pattern)
					if idx == -1 {
						break
					}
					midStart := idx + len(pattern)
					endQuote := bytes.IndexByte(searchBuf[midStart:], '"')
					if endQuote > 0 && endQuote < 30 {
						mid := string(searchBuf[midStart : midStart+endQuote])
						if len(mid) >= 10 && len(mid) <= 20 {
							// Check that songPlayTime is nearby (within 500 bytes)
							contextStart := idx - 500
							if contextStart < 0 {
								contextStart = 0
							}
							contextEnd := midStart + endQuote + 500
							if contextEnd > len(searchBuf) {
								contextEnd = len(searchBuf)
							}
							context := searchBuf[contextStart:contextEnd]
							if bytes.Contains(context, playTimePattern) {
								log.Detail("找到 songmid '%s' (songPlayTime=%d)", mid, durationMs)
								return mid
							}
						}
					}
					searchBuf = searchBuf[idx+len(pattern):]
				}
			}
		}

		addr = mbi.BaseAddress + mbi.RegionSize
		if addr > 0x7FFFFFFF {
			break
		}
	}
	return ""

}

func (m *QQMusicMem) ReadAllMetadata() (*SongMetadata, error) {
	if m.qqmusicDllBase == 0 {
		return nil, errors.New("process not attached")
	}

	meta := &SongMetadata{}

	// Read Struct 1 (C87C80)
	extractSSO := func(addr uintptr) string {
		buf := m.ReadBytes(addr, 24)
		if len(buf) < 24 {
			return ""
		}
		// Based on libc++ / MSVC string representation heuristics:
		// If capacity is large, the first 4 bytes are a pointer.
		// However, it's safer to just check if it's a valid pointer vs inline string.
		// In MSVC 32-bit: +0 is buffer or pointer, +16 is length, +20 is capacity.
		length := m.ReadUint32(addr + 16)
		capacity := m.ReadUint32(addr + 20)
		
		if capacity > 15 && length > 0 && length < 1000 {
			ptr := m.ReadUint32(addr)
			if ptr > 0x10000 {
				strBuf := m.ReadBytes(uintptr(ptr), length)
				return string(bytes.Trim(strBuf, "\x00"))
			}
		} else {
			// Inline reading up to the length usually
			// or just find the first null byte in the union buffer
			idx := bytes.IndexByte(buf[:16], 0)
			if idx == -1 {
				idx = 16
			}
			// wait, if capacity logic isn't MSVC, maybe libc++ 23-byte inline?
			// Just universally read inline string till null
			inlineIdx := bytes.IndexByte(buf, 0)
			if inlineIdx > 0 {
				return string(buf[:inlineIdx])
			}
		}
		return ""
	}

	var name1, singer1 string
	var songId, duration, progress uint32

	// CE verified: struct is directly embedded at QQMusic.dll+C87C80, NOT a pointer
	struct1 := m.qqmusicDllBase + 0xC87C80

	name1 = extractSSO(struct1 + 0x00)
	singer1 = extractSSO(struct1 + 0x18)

	if len(name1) > 1 {
		meta.Name = name1
	}
	if len(singer1) > 1 {
		meta.Singer = singer1
	}

	// CE verified offsets: +0x60=SongID, +0x68=DurationMs, +0x6C=ProgressMs
	songId = m.ReadUint32(struct1 + 0x60)
	duration = m.ReadUint32(struct1 + 0x68)

	// Fast progress timer: QQMusic.dll+0xC157D8 -> [ptr]+0x618
	// Updates every ~1 second (vs struct1+0x6C which updates every ~3-5 seconds)
	fastTimerPtr := uintptr(m.ReadUint32(m.qqmusicDllBase + 0xC157D8))
	if fastTimerPtr > 0x10000 {
		progress = m.ReadUint32(fastTimerPtr + 0x618)
	} else {
		// Fallback to struct1+0x6C (slow timer)
		progress = m.ReadUint32(struct1 + 0x6C)
	}

	if songId > 0 && songId < 0x3F000000 {
		meta.SongID = songId
	}
	meta.DurationMs = duration
	meta.ProgressMs = progress

	// Read Struct 2 (C86B00)
	struct2 := uintptr(m.ReadUint32(m.qqmusicDllBase + 0xC86B00))
	var name2, singer2 string
	if struct2 != 0 {
		namePtr := m.ReadUint32(struct2 + 0x64)
		if namePtr != 0 {
			name2 = m.ReadWideString(uintptr(namePtr), 256)
		}
		singerPtr := m.ReadUint32(struct2 + 0x68)
		if singerPtr != 0 {
			singer2 = m.ReadWideString(uintptr(singerPtr), 256)
		}
	}

	name := name2
	if len(name) < 2 { name = name1 }
	singer := singer2
	if len(singer) < 2 { singer = singer1 }
	
	// sanitize
	name = sanitizeString(name)
	singer = sanitizeString(singer)

	// Read dynamic slider value
	var sliderVal uint32
	if m.sliderPointer != 0 {
		edi := m.ReadUint32(m.sliderPointer)
		if edi != 0 {
			sliderVal = m.ReadUint32(uintptr(edi) + 0xF0)
		}
	}

	return &SongMetadata{
		Name:       name,
		Singer:     singer,
		SongID:     songId,
		ProgressMs: progress,
		DurationMs: duration,
		SliderVal:  sliderVal,
	}, nil
}
