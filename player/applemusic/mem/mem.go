// Package mem 只读扫描 Apple Music for Windows（AppleMusic.exe）进程内存，取「正在播放」元数据、
// 歌词 TTML 与播放位置结构。
//
// 为什么内存扫描而非 SMTC：用户要求纯内存、零 WinRT 依赖（与 qqmusic/wesing 一路）。三样东西各自的
// 形态与判据见同包的 propstore.go、ttmlscan.go、twin.go；哪份属于当前歌、何时扫，由 player 层决定。
//
// 纯只读：ReadProcessMemory + VirtualQueryEx，不写目标一个字节（AGENTS §0.1）。普通权限即可：
// AppleMusic.exe 与本服务同用户、同完整性级别（2026-10-08 在未提权会话中实测可读）。
package mem

import (
	"errors"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TargetProcessName Apple Music for Windows 进程名。
const TargetProcessName = "AppleMusic.exe"

var (
	// ErrNotRunning AppleMusic.exe 没有在运行。
	ErrNotRunning = errors.New("AppleMusic.exe 未运行")
	// ErrAccessDenied 进程在，但打不开（例如 Apple Music 以管理员身份运行、本服务没有）。
	ErrAccessDenied = errors.New("无权读取 AppleMusic.exe 进程")
)

var (
	k32                   = windows.NewLazySystemDLL("kernel32.dll")
	procReadProcessMemory = k32.NewProc("ReadProcessMemory")
	procVirtualQueryEx    = k32.NewProc("VirtualQueryEx")
)

const (
	memCommit     = 0x1000
	memPrivate    = 0x20000
	pageReadWrite = 0x04
	maxRegion     = 256 << 20 // 跳过超大区，防卡

	// maxKeptBuf 全扫结束后读缓冲留用的容量上限。缓冲按遇到的最大区增长（可接近 maxRegion），留用是为了
	// 下一次全扫不再分配；但稳态播放时零全扫，留着的缓冲在整个附加期间一直占着内存。取 64MB：2026-10-08
	// 本机最大可读区 30.3MB，两倍有余，这种情形留得住。目标进程里有更大的区时，缓冲在全扫结束时丢掉，
	// 后果是回到每次全扫重新分配（从头逐级长到最大区的大小），换来服务不常驻这块内存。
	maxKeptBuf = 64 << 20

	// maxTwinCandidates 发现扫描收集的位置候选上限。本机实测约 530 个；超出即截断并在 Snapshot 里
	// 标出，由调用方记日志——候选每轮都要复读，不能无界。
	maxTwinCandidates = 4096

	// scanYield 全扫途中每隔这么久（在两块内存之间）回调一次调用方，见 Scan。
	scanYield = 20 * time.Millisecond
)

// memoryBasicInformation 对应 MEMORY_BASIC_INFORMATION（x64 含对齐填充）。
type memoryBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	_                 uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
	_                 uint32
}

// Reader 是对 AppleMusic.exe 的一次只读附加。
//
// 不可并发调用：全扫的读缓冲由 Reader 持有、可跨全扫复用，两个 Scan 同时跑会互相改写对方正在解析的字节。
// 现在的用法是一个 Reader 只在一个 goroutine 里用（applemusic 包的 Start → runSession）。Scan 的 during
// 回调在同一个 goroutine 里调 ReadTwin / ReadBytes 不在此限：它们各自另分配小缓冲，不碰读缓冲。
type Reader struct {
	h   windows.Handle
	pid uint32
	// buf 是 eachRegion 的读缓冲：按遇到的最大区增长（不超过 maxRegion）；全扫结束后容量不超过
	// maxKeptBuf 的留着给下一次用，超过的丢掉（dropOversizedBuf）；Close 时释放。Scan 的返回值不得
	// 引用它（见 eachRegion）。
	buf []byte
}

// dropOversizedBuf 在全扫结束时调用：读缓冲的容量超过 maxKeptBuf 就丢掉，下一次全扫重新分配；
// 不超过的原样留用。
func (r *Reader) dropOversizedBuf() {
	if cap(r.buf) > maxKeptBuf {
		r.buf = nil
	}
}

// Attach 找到 AppleMusic.exe 并以只读方式打开。失败返回 ErrNotRunning / ErrAccessDenied / 其它错误，
// 由上层降级重试。
func Attach() (*Reader, error) {
	pid, err := findPID()
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, ErrAccessDenied
		}
		return nil, err
	}
	return &Reader{h: h, pid: pid}, nil
}

// findPID 用 Toolhelp 快照找进程（不逐个打开别的进程取名，每 3s 一次也便宜）。
func findPID() (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(syscall.UTF16ToString(pe.ExeFile[:]), TargetProcessName) {
			return pe.ProcessID, nil
		}
	}
	return 0, ErrNotRunning
}

// PID 返回附加的进程号。
func (r *Reader) PID() uint32 { return r.pid }

// Close 释放进程句柄与读缓冲。
func (r *Reader) Close() {
	if r.h != 0 {
		windows.CloseHandle(r.h)
		r.h = 0
	}
	r.buf = nil
}

// Valid 报告目标进程是否仍存活（句柄可查退出码且仍在运行）。
func (r *Reader) Valid() bool {
	if r.h == 0 {
		return false
	}
	var code uint32
	if err := windows.GetExitCodeProcess(r.h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// eachRegion 遍历 MEM_PRIVATE 且保护属性恰为 PAGE_READWRITE 的已提交区，回调拿到区基址与内容。
//
// 只读这一类区，是 2026-10-08 真机量出来的：三样目标（属性存储、TTML、位置结构）全部落在其中；
// 而全扫的耗时几乎都花在 ReadProcessMemory 上：MEM_MAPPED / MEM_IMAGE 区与 PRIVATE 里的写合并区（0x404，
// 多半是显存共享，本机约 240MB）占了大半。只读 PRIVATE+RW 后一次全扫 0.32~0.42s；只排除 MAPPED/IMAGE、
// 仍读写合并区时 2.3s，读全部可读区 3.2~4.2s。这三个数是同一天、逐窗找候选还没有预筛时量的，只用来比较
// 区的取舍；现行耗时见 Scan。
//
// 读缓冲（r.buf）跨区复用，按遇到的最大区增长；遍历结束时容量不超过 maxKeptBuf 的留给下一次全扫，
// 长到最大区的大小之后不再分配，超过的丢掉、下一次全扫重新分配（dropOversizedBuf，取舍见 maxKeptBuf）。
// 每个区新分配时，每次全扫的分配量等于目标进程全部可读内存（审查方真机 150 秒累计约 210 GB）；只在
// 一次遍历内复用时，每次全扫仍要从头逐级长到最大区的大小（2026-10-08 本机实测每次约 66MB，最大可读区
// 30.3MB）；跨全扫复用后（2026-10-09 真机复测，最大可读区 31.3MB）每次收候选的全扫分配约 16MB，都来自
// 结果本身（TTML 文本、封面与签名的拷贝），5 次全扫触发的 GC 由 11 次降到 4 次。
//
// dropOversizedBuf 本身有离线测试；它在这里的调用只测到「查询失败即返回」那个出口（句柄为 0 的 Reader），
// 遍历真进程走完的出口共用同一个 defer，依赖真进程、没有离线测试。
//
// data 只在这次回调期间有效，回调返回后即被下一个区覆盖：fn 要留用的内容必须拷贝。Scan 收的三样都满足——
// findNowPlaying 的 Cover / Sig 是拷贝，标题等字符串与 findTTML 的文档是解码时新建的，位置候选只记地址。
func (r *Reader) eachRegion(fn func(base uintptr, data []byte)) {
	defer r.dropOversizedBuf() // 遍历有三处出口，统一在返回时处理
	var addr uintptr
	for addr < 0x7FFFFFFF0000 {
		var m memoryBasicInformation
		ret, _, _ := procVirtualQueryEx.Call(uintptr(r.h), addr,
			uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
		if ret == 0 {
			return
		}
		next := m.BaseAddress + m.RegionSize
		if next <= addr {
			return
		}
		if m.State == memCommit && m.Type == memPrivate && m.Protect == pageReadWrite &&
			m.RegionSize > 0 && m.RegionSize < maxRegion {
			if uintptr(cap(r.buf)) < m.RegionSize {
				r.buf = make([]byte, m.RegionSize)
			}
			b := r.buf[:m.RegionSize]
			var read uintptr
			procReadProcessMemory.Call(uintptr(r.h), m.BaseAddress,
				uintptr(unsafe.Pointer(&b[0])), m.RegionSize, uintptr(unsafe.Pointer(&read)))
			if read > 0 {
				fn(m.BaseAddress, b[:read])
			}
		}
		addr = next
	}
}

// TTMLDoc 是一份 TTML 文档全文、它在内存中的副本数，以及每份副本在进程内的绝对地址（升序，
// len(Addrs) == Count）。
type TTMLDoc struct {
	Text  string
	Count int
	Addrs []uintptr
}

// Snapshot 是一次全扫的结果。
type Snapshot struct {
	// Songs 每份 Apple Music「正在播放」存储一项（同值多份即重复，供计票）。
	Songs []NowPlaying
	// Docs 按全文去重，顺序为首次出现的顺序（内存地址升序）。
	Docs []TTMLDoc
	// Twins 位置结构候选的地址（仅 Scan(true) 收集），地址升序。
	Twins []uintptr
	// TwinsTruncated 候选超过 maxTwinCandidates 被截断。
	TwinsTruncated bool
}

// Scan 全扫一遍（只读 PRIVATE+RW 区），收集元数据与 TTML；wantTwins 时顺带收集位置候选。
// 调用方负责节流：它是本包唯一的重操作。
//
// 耗时（2026-10-09 真机复测，本机可读区 436MB、5511 个区，Apple Music 暂停中）：收候选 0.19~0.24s，不收
// 0.12~0.16s。逐窗找候选加预筛、读缓冲跨全扫复用之前（2026-10-08，可读区 476MB、2316 个区）是 0.40s
// （只读 88ms、属性存储搜索 40ms、TTML 搜索 40ms、逐窗找候选 240ms）与 0.15s。
//
// during 非 nil 时，扫描途中约每 scanYield 在两块内存之间调一次：全扫这零点几秒里调用方还得照常读位置
// （ReadTwin，24 字节）——开播、位置跳变、拖动恰恰常发生在全扫的时候（换歌会触发全扫），等扫完才看见，
// 位置锚点与歌词行都要晚这么久。during 里只可调 ReadTwin / ReadBytes，不可再调 Scan。
func (r *Reader) Scan(wantTwins bool, during func()) Snapshot {
	var s Snapshot
	docIdx := map[string]int{}
	yielded := time.Now()
	r.eachRegion(func(base uintptr, data []byte) {
		for _, np := range findNowPlaying(data) {
			np.Addr += base // 块内偏移 → 进程内绝对地址
			s.Songs = append(s.Songs, np)
		}
		s.Docs = addDocs(s.Docs, docIdx, base, data)
		if wantTwins && !s.TwinsTruncated {
			s.Twins, s.TwinsTruncated = appendTwinCandidates(s.Twins, base, data)
		}
		if during != nil && time.Since(yielded) >= scanYield {
			during()
			yielded = time.Now()
		}
	})
	return s
}

// ReadTwin 读 addr 处的位置结构（24 字节，亚毫秒）。ok=false：读失败或形态不对。
func (r *Reader) ReadTwin(addr uintptr) (Twin, bool) {
	b, ok := r.readAt(addr, twinSize)
	if !ok {
		return Twin{}, false
	}
	return parseTwin(b)
}

// ReadBytes 读 addr 处的 n 字节（盯住一份「正在播放」存储用：与 NowPlaying.Sig 比对，亚毫秒）。
// 读不全（已被释放、跨到未映射页）→ ok=false。
func (r *Reader) ReadBytes(addr uintptr, n int) ([]byte, bool) {
	return r.readAt(addr, n)
}

// readAt 从目标进程绝对地址读 n 字节。跨越未映射页/部分失败 → ok=false。
func (r *Reader) readAt(addr uintptr, n int) ([]byte, bool) {
	if r.h == 0 || n <= 0 {
		return nil, false
	}
	buf := make([]byte, n)
	var read uintptr
	ret, _, _ := procReadProcessMemory.Call(uintptr(r.h), addr,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(unsafe.Pointer(&read)))
	if ret == 0 || int(read) < n {
		return nil, false
	}
	return buf, true
}
