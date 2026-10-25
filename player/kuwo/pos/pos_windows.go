//go:build windows

// 默认 Open 使用 KwService 的实时 PlayerCore；OpenSnapshot 保留旧四秒快照供诊断。
//
// 前置条件（插件侧必须满足）：酷我以**中完整性**运行。管理员启动时 VM_READ 会被拒（实测 err=5），
// 这时 Open 只会返回错误，播放器应降级为「没有位置」而不是刷屏。

package pos

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	memPrivate = 0x20000
	chunkSize  = 8 << 20
)

// Reader 绑定一个已定位的位置字段，轮询里直接用，避免每次重新扫描。
type Reader struct {
	h    windows.Handle
	addr uintptr
	song Song
	core *CoreReader
}

// Open reads the real-time core of the selected kwmusic PID. durationMs is kept
// for source compatibility; each read uses the duration from PlayerCore instead.
// Unknown builds fail closed, without silently falling back to a slow snapshot.
func Open(pid int, rid string, durationMs int32) (*Reader, error) {
	core, err := OpenCore(pid)
	if err != nil {
		return nil, err
	}
	s, err := core.Read(rid)
	if err != nil {
		core.Close()
		return nil, err
	}
	return &Reader{core: core, song: Song{RID: s.RID, DurationMs: s.DurationMs, PositionAddr: core.base + corePosition}}, nil
}

// OpenSnapshot is the legacy four-second clock, retained only for comparison.
// It does not provide real-time playback timing. Prefer Open or OpenCore.
func OpenSnapshot(pid int, rid string, durationMs int32) (*Reader, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("kuwo pos: open process %d: %w", pid, err)
	}
	rid = RIDWithPrefix(rid)
	var song Song
	var ok bool
	if len(rid) <= 16 {
		song, ok = scanForString(h, rid)
	}
	if !ok && durationMs > 0 {
		song, ok = scanForDuration(h, durationMs)
	}
	if !ok {
		windows.CloseHandle(h)
		return nil, ErrNotFound
	}
	return &Reader{h: h, addr: song.PositionAddr, song: song}, nil
}

// Song 返回定位到的歌曲信息（时长来自结构本身）。
func (r *Reader) Song() Song { return r.song }

// Position 读一次位置（毫秒）。
func (r *Reader) Position() (int32, error) {
	if r.core != nil {
		s, err := r.core.Read(r.song.RID)
		if err != nil {
			return 0, err
		}
		r.song.DurationMs = s.DurationMs
		return s.PositionMs, nil
	}
	b, err := readAt(r.h, r.addr, 4)
	if err != nil || len(b) < 4 {
		return 0, fmt.Errorf("kuwo pos: read at 0x%X: %w", r.addr, err)
	}
	return int32(getU32(b)), nil
}

// Close 释放进程句柄。
func (r *Reader) Close() {
	if r.core != nil {
		r.core.Close()
	}
	if r.h != 0 {
		windows.CloseHandle(r.h)
		r.h = 0
	}
}

func scanForString(h windows.Handle, rid string) (Song, bool) {
	pat := []byte(rid + "\x00")
	var found Song
	walkRegions(h, func(base uintptr, buf []byte) bool {
		from := 0
		for {
			i := strings.Index(string(buf[from:]), string(pat))
			if i < 0 {
				return true
			}
			addr := base + uintptr(from+i)
			head, err := readAt(h, addr, offPosition+4)
			if err == nil && len(head) >= offPosition+4 {
				if dur, ok := Validate(head, rid); ok {
					found = Song{RID: rid, DurationMs: dur, PositionAddr: addr + offPosition}
					return false
				}
			}
			from += i + len(pat)
			if from >= len(buf) {
				return true
			}
		}
	})
	return found, found.PositionAddr != 0
}

func scanForDuration(h windows.Handle, durationMs int32) (Song, bool) {
	want := uint32(durationMs)
	var found Song
	walkRegions(h, func(base uintptr, buf []byte) bool {
		for i := 0; i+4 <= len(buf); i += 4 {
			if getU32(buf[i:]) != want {
				continue
			}
			start := base + uintptr(i) - offDuration
			head, err := readAt(h, start, offPosition+4)
			if err != nil || len(head) < offPosition+4 {
				continue
			}
			rid, ok := readRIDAt(head)
			if !ok {
				continue
			}
			if dur, ok := Validate(head, rid); ok && dur == durationMs {
				found = Song{RID: rid, DurationMs: dur, PositionAddr: start + offPosition}
				return false
			}
		}
		return true
	})
	return found, found.PositionAddr != 0
}

// readRIDAt 从结构起点解析 "MUSIC_<digits>\0"。
func readRIDAt(head []byte) (string, bool) {
	if len(head) < 7 || string(head[:6]) != "MUSIC_" {
		return "", false
	}
	end := -1
	for i := 6; i < 16 && i < len(head); i++ {
		if head[i] == 0 {
			end = i
			break
		}
		if head[i] < '0' || head[i] > '9' {
			return "", false
		}
	}
	if end < 0 {
		return "", false
	}
	return string(head[:end]), true
}

// walkRegions 遍历私有可读写内存；cb 返回 false 表示提前结束。
func walkRegions(h windows.Handle, cb func(base uintptr, buf []byte) bool) {
	var mbi windows.MemoryBasicInformation
	addr := uintptr(0)
	for {
		if err := windows.VirtualQueryEx(h, addr, &mbi, unsafe.Sizeof(mbi)); err != nil || mbi.RegionSize == 0 {
			return
		}
		base := uintptr(mbi.BaseAddress)
		size := uintptr(mbi.RegionSize)
		okType := mbi.Type == memPrivate || mbi.Type == 0x40000
		okProt := mbi.Protect == windows.PAGE_READWRITE || mbi.Protect == windows.PAGE_EXECUTE_READWRITE ||
			mbi.Protect == windows.PAGE_WRITECOPY || mbi.Protect == windows.PAGE_READONLY
		if mbi.State == windows.MEM_COMMIT && okType && okProt && size >= 0x1000 {
			off := uintptr(0)
			for off < size {
				n := size - off
				if n > chunkSize {
					n = chunkSize
				}
				buf := make([]byte, n)
				var got uintptr
				if err := windows.ReadProcessMemory(h, base+off, &buf[0], n, &got); err == nil && got > 0 {
					if !cb(base+off, buf[:got]) {
						return
					}
				}
				if off+n >= size {
					break
				}
				off += n
			}
		}
		next := base + size
		if next <= addr {
			return
		}
		addr = next
	}
}

func readAt(h windows.Handle, addr uintptr, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	b := make([]byte, n)
	var got uintptr
	err := windows.ReadProcessMemory(h, addr, &b[0], uintptr(n), &got)
	if err != nil && got == 0 {
		return nil, err
	}
	return b[:got], nil
}
