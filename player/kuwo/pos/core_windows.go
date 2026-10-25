//go:build windows

package pos

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CoreReader binds the direct KwService child of the selected kwmusic process.
// It is owned by one polling goroutine; Close must not race Read.
type CoreReader struct {
	h         windows.Handle
	base      uintptr // the singleton address resolved from GetPlayerCore
	module    uintptr
	vtable    uintptr // singleton vtable value at open time; a change rejects
	timestamp uint32  // PE TimeDateStamp, logging only
	slot      int     // matched vtable slot, logging only
}

// Build returns the PE timestamp and matched vtable slot, for the connect log.
func (r *CoreReader) Build() (timestamp uint32, slot int) { return r.timestamp, r.slot }

// OpenCore accepts the main kwmusic PID, not an arbitrary service PID. Selecting
// by parent avoids attaching to a stale service from a different player instance.
func OpenCore(mainPID int) (*CoreReader, error) {
	pid, err := servicePID(uint32(mainPID))
	if err != nil {
		return nil, err
	}
	module, size, err := coreModule(pid)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, err
	}
	r := &CoreReader{h: h, module: module}
	img, err := validateCoreImage(r.read, module, uint64(size))
	if err != nil {
		r.Close()
		return nil, err
	}
	r.base, r.vtable, r.timestamp, r.slot = img.singleton, img.vtable, img.timestamp, img.slot
	return r, nil
}

func (r *CoreReader) read(addr uintptr, n int) ([]byte, error) {
	if r.h == 0 {
		return nil, ErrInvalidSample
	}
	b, err := readAt(r.h, addr, n)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, ErrInvalidSample
	}
	return b, nil
}

// Read returns current RID, refreshed duration and client progress. An empty
// expectedRID discovers identity; a nonempty RID rejects stale caller state.
// On unknown versions or torn/song-changing samples it returns an error. There
// is no implicit fallback to the former four-second clock. A vtable value that
// differs from open time rejects; a zero value is a not-yet-constructed object.
func (r *CoreReader) Read(expectedRID string) (Sample, error) {
	v, err := r.read(r.base, 4)
	if err != nil {
		return Sample{}, err
	}
	if err := vtableStable(uintptr(getU32(v)), r.vtable); err != nil {
		return Sample{}, err
	}
	return readCoreSample(r.read, r.base, expectedRID)
}

func (r *CoreReader) Close() {
	if r.h != 0 {
		windows.CloseHandle(r.h)
		r.h = 0
	}
}

func servicePID(parent uint32) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var found uint32
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ParentProcessID == parent && strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "KwService.exe") {
			if found != 0 {
				return 0, ErrUnsupportedCore
			}
			found = entry.ProcessID
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return 0, err
	}
	if found == 0 {
		return 0, ErrNotFound
	}
	return found, nil
}

// coreModule returns the module base and its in-memory size (ModBaseSize). The
// size is no longer pinned; validateCoreImage checks it against SizeOfImage.
func coreModule(pid uint32) (uintptr, uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, pid)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(snap)
	entry := windows.ModuleEntry32{Size: uint32(windows.SizeofModuleEntry32)}
	for err = windows.Module32First(snap, &entry); err == nil; err = windows.Module32Next(snap, &entry) {
		if strings.EqualFold(windows.UTF16ToString(entry.Module[:]), "PlayerCore.dll") {
			return entry.ModBaseAddr, entry.ModBaseSize, nil
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return 0, 0, err
	}
	return 0, 0, ErrNotFound
}
