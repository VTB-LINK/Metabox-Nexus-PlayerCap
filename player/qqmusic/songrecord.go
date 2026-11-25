package qqmusic

// 本文件读 QQ 内存里当前歌曲的记录（#218，设计 §6.4 第 4 步）。QQ 为每首歌在私有可写内存里留一条 UTF-8 记录，专辑、歌手、
// 歌名、网址、参数直接相接（设计 §2.2）；读取者以 Struct1 的专辑、歌手、歌名拼出前缀，扫出记录、截取网址，据此认出 QQ 查关联
// 所用的那一条（assocreader.go 的 readRecord）。
//
// 读 QQ 的内存只经会话开始时复制的只读句柄（PROCESS_QUERY_INFORMATION | PROCESS_VM_READ），本文件只调用 VirtualQueryEx、
// ReadProcessMemory、DuplicateHandle、CloseHandle（songrecordreadonly_test.go 守）。不用 QQMusicMem 的句柄与方法：那个句柄
// 是 PROCESS_ALL_ACCESS，且轮询线程的 CheckValid 会把 hProcess 清零，跨 goroutine 读它是数据竞争。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

// memPrivate 即 MEM_PRIVATE（x/sys/windows 没有这个常量）。
const memPrivate = 0x20000

// memRegion 是 VirtualQueryEx 给出的一个区域。
type memRegion struct {
	base, size          uintptr
	state, protect, typ uint32
}

// memSource 是读 QQ 内存的两个操作，测试以桩替换。query 返回 addr 所在的区域；err 为 windows.ERROR_INVALID_PARAMETER 即
// 地址空间到头（addr 超出进程可访问的最高地址，VirtualQueryEx 文档约定的结束方式），其余错误是查询失败。read 自 addr 读入
// buf，可读到少于 len(buf)，此时 err 一般不为 nil。
type memSource struct {
	query func(addr uintptr) (memRegion, error)
	read  func(addr uintptr, buf []byte) (int, error)
}

// procMemSource 经进程句柄 h 读内存，VirtualQueryEx 的错误原样带回。
func procMemSource(h windows.Handle) memSource {
	return memSource{
		query: func(addr uintptr) (memRegion, error) {
			var mbi windows.MemoryBasicInformation
			if err := windows.VirtualQueryEx(h, addr, &mbi, unsafe.Sizeof(mbi)); err != nil {
				return memRegion{}, err
			}
			return memRegion{base: mbi.BaseAddress, size: mbi.RegionSize, state: mbi.State, protect: mbi.Protect, typ: mbi.Type}, nil
		},
		read: func(addr uintptr, buf []byte) (int, error) {
			if len(buf) == 0 {
				return 0, nil
			}
			var n uintptr
			err := windows.ReadProcessMemory(h, addr, &buf[0], uintptr(len(buf)), &n)
			return int(n), err
		},
	}
}

// recordSource 是读取者读当前歌曲记录所需的一切：经只读句柄的内存读取、Struct1 的绝对地址与本版本的偏移表。close 关闭
// 句柄，恰调一次：读取者退出时，或读取者没有起来时由 startAssocReader 调用（release）。
type recordSource struct {
	mem     memSource
	struct1 uintptr
	offs    dllOffsets
	close   func()
}

// release 关闭只读句柄；s 为 nil（本会话不读记录）时什么也不做。
func (s *recordSource) release() {
	if s != nil {
		s.close()
	}
}

// newRecordSource 由会话的进程句柄复制一个只读句柄，建立读记录的来源（设计 §6.4 第 4 步「谁来读」）。须在会话自己的
// goroutine 里、启动读取者之前调用：此时 hProcess 只由本 goroutine 读写。本版本不读记录（songRecordEnabled 为假）时不复制；
// 不读或复制失败时返回 nil，打一条 Detail（每会话一次）。复制而不按进程号另开：复制出的句柄指向同一个进程对象，QQ 退出后
// 进程号被复用也读不到别的进程。
func newRecordSource(m *QQMusicMem) *recordSource {
	if !m.songRecordEnabled() {
		log.Detail("QQ Music %s: the current song record format is not verified; lyric associations of online songs are not followed this session", m.Version())
		return nil
	}
	self := windows.CurrentProcess()
	var h windows.Handle
	err := windows.DuplicateHandle(self, windows.Handle(m.hProcess), self, &h, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, 0)
	if err != nil {
		log.Detail("Cannot duplicate a read-only handle to QQ Music; lyric associations of online songs are not followed this session: %v", err)
		return nil
	}
	return &recordSource{
		mem: procMemSource(h), struct1: m.qqmusicDllBase + m.offsets.Struct1, offs: m.offsets,
		close: func() { windows.CloseHandle(h) },
	}
}

// readSongPrefix 读 Struct1 里专辑、歌手、歌名三个字段（AlbumOff、SingerOff、NameOff 处），按 offs.UseWideStrings 分两支，
// 原样返回、不经 sanitizeString；任一段读不出 ok 即为假。
//   - 宽字符：32 位指针所指的 UTF-16 串，各至多 assoc.RecordStrMax 个码元、其内须有 NUL，原样解码，不成对的代理项解码为
//     U+FFFD（由 assoc.RecordNeedle 拒绝）。读指针失败、指针为 0、读串失败或读到的部分里没有 NUL 时读不出；串紧挨区域末尾而
//     只读到一部分时，NUL 在读到的部分里即可。
//   - 窄字符：24 字节的 MSVC 32 位 std::string，见 readNarrowField。
func readSongPrefix(src memSource, struct1 uintptr, offs dllOffsets) (album, singer, name string, ok bool) {
	var out [3]string
	var p [4]byte
	buf := make([]byte, 2*assoc.RecordStrMax)
	for i, off := range [...]uintptr{offs.AlbumOff, offs.SingerOff, offs.NameOff} {
		if !offs.UseWideStrings {
			s, ok := readNarrowField(src, struct1+off)
			if !ok {
				return "", "", "", false
			}
			out[i] = s
			continue
		}
		if n, err := src.read(struct1+off, p[:]); err != nil || n != len(p) {
			return "", "", "", false
		}
		ptr := binary.LittleEndian.Uint32(p[:])
		if ptr == 0 {
			return "", "", "", false
		}
		n, _ := src.read(uintptr(ptr), buf)
		s, ok := utf16UntilNUL(buf[:min(max(n, 0), len(buf))])
		if !ok {
			return "", "", "", false
		}
		out[i] = s
	}
	return out[0], out[1], out[2], true
}

// narrowStrMax 是窄字符串一段的字节上限：与宽字符的 assoc.RecordStrMax 个码元对应，一个 BMP 码元转成 UTF-8 至多 3 字节。
const narrowStrMax = 3 * assoc.RecordStrMax

// readNarrowField 读 addr 处 24 字节的 MSVC 32 位 std::string（22.16、22.22 的 Struct1，设计 §6.4 第 4 步「前缀」）：内联还是
// 堆上、堆指针与长度的判定交给 ssoFromBuf，它不转码、原样返回字节，堆上至多读 999 字节（它的长度上界）。读不满 24 字节、
// 堆上的串读不满长度字段所报的字节数（不交出半截，ssoFromBuf 因而给出空串）时读不出；解出的串还须字节数等于长度字段、在
// 1～narrowStrMax 字节之间、其内没有 NUL、是合法的 UTF-8，不合即读不出。不转码：这两个版本的 std::string 存的是 UTF-8
// （推断，依据见设计同节「窄字符串的编码」），与记录相同；某段若是 GBK 等别的编码，UTF-8 校验不过、按读不出，不会认错。
func readNarrowField(src memSource, addr uintptr) (string, bool) {
	var buf [24]byte
	if n, err := src.read(addr, buf[:]); err != nil || n != len(buf) {
		return "", false
	}
	s := ssoFromBuf(buf[:], func(ptr, n uint32) []byte {
		b := make([]byte, n)
		if got, err := src.read(uintptr(ptr), b); err != nil || got != len(b) {
			return nil
		}
		return b
	})
	if uint32(len(s)) != binary.LittleEndian.Uint32(buf[0x10:0x14]) || len(s) < 1 || len(s) > narrowStrMax ||
		strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s) {
		return "", false
	}
	return s, true
}

// utf16UntilNUL 把 b（UTF-16LE）解码到第一个 NUL 为止；没有 NUL 为假。
func utf16UntilNUL(b []byte) (string, bool) {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			return string(utf16.Decode(u)), true
		}
		u = append(u, c)
	}
	return "", false
}

// scanLimits 是一次扫描的上限：每块读入的字节数与读取总量（含相邻块的重叠）。
type scanLimits struct{ chunk, total int64 }

// recordScanLimits 是出货代码的上限（设计 §6.4 第 4 步「扫描」）：每块 4 MiB，复用一个缓冲区；读取总量 1.5 GiB。
var recordScanLimits = scanLimits{chunk: 4 << 20, total: 1536 << 20}

// recordScanStats 是一次扫描的统计，供日志：读取量（含块间重叠）、读失败或只读到一部分的块数、是否到了读取上限、是否因区域
// 查询失败或区域不前进而停下、耗时。
type recordScanStats struct {
	read         int64
	failedChunks int
	capped       bool
	queryFailed  bool
	elapsed      time.Duration
}

// scanRecord 在 src 里找以 needle（assoc.RecordNeedle 拼出的前缀，以 http 结尾）开头的记录（设计 §6.4 第 4 步「扫描」
// 「网址截取」）。自地址 0 起逐区查询，只读已提交（MEM_COMMIT）、私有（MEM_PRIVATE）、保护恰为 PAGE_READWRITE 的区域
// （带 PAGE_GUARD 等修饰的不等）。query 报 ERROR_INVALID_PARAMETER 即地址空间到头，正常结束；报别的错，或区域的末尾不在
// 查询地址之后（不前进，或 base+size 溢出），即停、记 queryFailed：查询点之后的内存没读到，当前记录可能就在其中。
//
// 区域按 lim.chunk 分块读入同一个缓冲区，相邻块重叠「len(needle) + assoc.RecordTailMax − 1」字节：命中起点落在重叠部分的
// 留给下一块（下一块自重叠处起），整条命中（前缀加尾部窗口）因此都在报它的那一块里，每处只报一次；区域的末块、到读取上限
// 的那一块没有下一块，读到的全归它。读取总量到 lim.total 即停、记 capped，最后一块按余量截短。块读失败或只读到一部分时只处理
// 读到的部分、跳过余下，记入 failedChunks；尾部被区域末尾或读到的部分截断的命中照常交 ParseRecordTail，截取不合即计 Malformed。
//
// 每处命中自 needle 末尾的 http 起取至多 assoc.RecordTailMax 字节交 assoc.ParseRecordTail：合格的追加 sc.Tails，不合的计
// sc.Malformed；第 assoc.RecordHitsMax+1 处命中即停、记 OverCap，不再读。capped、queryFailed 或 failedChunks > 0 时
// sc.Complete 为假。
//
// 每块之前调用 stop，为真即中止：aborted 为真，sc 与 st 作废。
func scanRecord(src memSource, needle []byte, lim scanLimits, stop func() bool) (sc assoc.RecordScan, st recordScanStats, aborted bool) {
	start := time.Now()
	defer func() {
		st.elapsed = time.Since(start)
		sc.Complete = !st.capped && !st.queryFailed && st.failedChunks == 0
	}()
	overlap := int64(len(needle) + assoc.RecordTailMax - 1)
	tail := len(needle) - len("http") // 命中处到 http 的距离
	buf := make([]byte, lim.chunk)
	hits := 0
	for addr := uintptr(0); ; {
		r, err := src.query(addr)
		if err != nil {
			st.queryFailed = !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
			return
		}
		end := r.base + r.size
		if end <= addr {
			st.queryFailed = true
			return
		}
		if r.state == windows.MEM_COMMIT && r.typ == memPrivate && r.protect == windows.PAGE_READWRITE {
			for off := max(r.base, addr); off < end; {
				if stop() {
					aborted = true
					return
				}
				if st.read >= lim.total {
					st.capped = true
					return
				}
				n := int64(min(uintptr(lim.chunk), end-off, uintptr(lim.total-st.read)))
				st.read += n
				own := n // 本块报的命中起点在 [0, own) 内；下一块自 off+own 起
				if off+uintptr(n) < end && st.read < lim.total && n > overlap {
					own = n - overlap
				}
				got, err := src.read(off, buf[:n])
				got = min(max(got, 0), int(n))
				if err != nil || got < int(n) {
					st.failedChunks++
				}
				b := buf[:got]
				for i := 0; i < got; {
					j := bytes.Index(b[i:], needle)
					if j < 0 || i+j >= int(own) {
						break
					}
					p := i + j
					if hits++; hits > assoc.RecordHitsMax {
						sc.OverCap = true
						return
					}
					if t, ok := assoc.ParseRecordTail(b[p+tail : min(got, p+tail+assoc.RecordTailMax)]); ok {
						sc.Tails = append(sc.Tails, t)
					} else {
						sc.Malformed++
					}
					i = p + 1
				}
				off += uintptr(own)
			}
		}
		addr = end
	}
}
