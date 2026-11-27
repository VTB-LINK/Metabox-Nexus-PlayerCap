package qqmusic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/telemetry"
)

var (
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32   = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First     = modkernel32.NewProc("Process32FirstW")
	procProcess32Next      = modkernel32.NewProc("Process32NextW")
	procModule32First      = modkernel32.NewProc("Module32FirstW")
	procModule32Next       = modkernel32.NewProc("Module32NextW")
	procCloseHandle        = modkernel32.NewProc("CloseHandle")
	procReadProcessMemory  = modkernel32.NewProc("ReadProcessMemory")
	procWriteProcessMemory = modkernel32.NewProc("WriteProcessMemory")
	procVirtualAllocEx     = modkernel32.NewProc("VirtualAllocEx")
	procVirtualProtectEx   = modkernel32.NewProc("VirtualProtectEx")

	modpsapi                 = syscall.NewLazyDLL("psapi.dll")
	procEnumProcessModulesEx = modpsapi.NewProc("EnumProcessModulesEx")
	procGetModuleBaseNameW   = modpsapi.NewProc("GetModuleBaseNameW")
	procGetModuleInformation = modpsapi.NewProc("GetModuleInformation")

	procVirtualQueryEx   = modkernel32.NewProc("VirtualQueryEx")
	procGetTickCount     = modkernel32.NewProc("GetTickCount")
	procGetProcAddress   = modkernel32.NewProc("GetProcAddress")
	procGetModuleHandleW = modkernel32.NewProc("GetModuleHandleW")

	modversion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modversion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modversion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modversion.NewProc("VerQueryValueW")
)

// qqProcessAccess 是打开 QQ 音乐进程句柄申请的最小只读权限：
// EnumProcessModulesEx(LIST_MODULES_ALL)（64 位枚举 32 位模块）、GetModuleBaseNameW、
// GetModuleInformation 需要 QUERY_INFORMATION | VM_READ；VirtualQueryEx、GetExitCodeProcess、
// QueryFullProcessImageNameW 需要 QUERY_INFORMATION；ReadProcessMemory 需要 VM_READ；
// DuplicateHandle 以当前进程伪句柄复制相同权限子集，不需要 PROCESS_DUP_HANDLE。
const qqProcessAccess = windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ

const (
	TH32CS_SNAPPROCESS     = 0x00000002
	TH32CS_SNAPMODULE      = 0x00000008
	TH32CS_SNAPMODULE32    = 0x00000010
	MEM_COMMIT             = 0x1000
	MEM_RESERVE            = 0x2000
	PAGE_EXECUTE_READWRITE = 0x40
	PAGE_READWRITE         = 0x04
	LIST_MODULES_ALL       = 0x03
	TargetProcessName      = "qqmusic.exe"
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

// dllOffsets 存储版本相关的 DLL 内部偏移量
type dllOffsets struct {
	// DLL 级偏移（版本敏感，每次更新可能变化）
	Struct1      uintptr // 元数据结构体 1 基址（QQMusic.dll 内偏移）
	Struct2Ptr   uintptr // 元数据结构体 2 指针（QQMusic.dll 内偏移，0=不可用）
	FastTimerPtr uintptr // 快速进度计时器指针（QQMusic.dll 内偏移，0=不可用）
	FastTimerOff uintptr // 快速计时器进度字段偏移（相对于指针解引用后的对象）

	// Struct1 内部偏移（结构体成员位置，版本间可能变化）
	NameOff     uintptr // 歌名字段偏移（SSO String 或 WCHAR* 取决于 UseWideStrings）
	SingerOff   uintptr // 歌手字段偏移
	AlbumOff    uintptr // 专辑名字段偏移
	SongIDOff   uintptr // SongID DWORD 偏移
	DurationOff uintptr // 总时长 DWORD 偏移
	ProgressOff uintptr // 慢速进度 DWORD 偏移

	// 版本特性标志
	UseWideStrings    bool    // true=歌名/歌手字段为 WCHAR* 指针（v20.05+）
	DurationInSeconds bool    // true=时长字段单位为秒需 *1000（v20.05+）
	SongMidParamsOff  uintptr // v20.05: struct1 内 URL 参数字符串指针偏移（0=不可用）
	// 格式: "0=<songMid>&2=<id>|<id>"，备选来源
	StreamURLOff uintptr // v20.05: struct1 内流媒体 URL 字符串指针偏移（0=不可用）
	// 格式: "http://stream*.qqmusic.qq.com/<songMid>.<ext>"
	// 文件名去扩展名即 songMid，比 URL params 更可靠
	ProgressDllOff uintptr // v20.05: DLL 基址直接偏移，DWORD 即播放毫秒数（0=不可用）
	// CE 手动确认: 0xB61E78 = 准确ms进度（备选 0xAF3600）

	SongMidFromHeap bool // true=songMid 不在结构体里，由 FindSongMid 从堆 JSON 按时长扫出（v22.41 兜底）

	// SongIDDurCheckOff：稳定 songID 所在「播放会话」结构自带的时长字段偏移（相对 Struct1，
	// 0=不校验）。与 DurationOff 的显示时长交叉核对，确认该 songID 已同步到当前歌（v22.41）。
	SongIDDurCheckOff uintptr

	// PlayState：QQ 自己的播放状态字段（#172）。零值即本版本未收录，按进度停走判暂停。只在版本号精确匹配时读
	// （playStateAddr）。填之前须过 doc/design/qqmusic-start-stall-design.md §6.8 的核实清单：运行时的交叉核对
	// 发现不了「卡在在播值而每次暂停都短」，那份清单是唯一的保证。
	PlayState playStateSpec

	// SongRecord：本版本核实过 QQ 内存里当前歌曲记录的格式（#218，设计 §2.2、§6.4 第 4 步）：专辑、歌手、歌名、网址、参数直接
	// 相接的 UTF-8 记录，以 Struct1 的专辑、歌手、歌名拼出的前缀找得到。读取者据此认出 QQ 查关联所用的条目；为假时有候选的
	// 在线歌曲一律按无条目。只在版本号精确匹配时读（songRecordEnabled）。打开的依据是真值补测（QQ 显示所用的键与记录网址的
	// 键一致），见设计 §6.4 第 4 步「版本」。
	SongRecord bool
}

// playStateSpec 描述一个版本的播放状态字段（#172）。零值即本版本未收录、不读。
type playStateSpec struct {
	Off     uintptr  // QQMusic.dll 内偏移，指向一个 DWORD；0 = 未收录
	Paused  []uint32 // 暂停取值
	Playing []uint32 // 在播取值
	Loading []uint32 // 载入取值：按停走判定，停走自进入载入值的那一拍起量（judgeTick）
	// Opening 是 Loading 里「已要求开播」的取值（#243）：读到它的拍不按停走判暂停，停走自其后第一拍非开播的载入值起量。
	// A 类的 3：录像里只在换歌与按播放时出现，前一取值为在播或 2、后一取值为在播或 2，未见接暂停值；22.26 有一段出声后
	// 停在 3 达 48 s。
	Opening  []uint32
	MinPause time.Duration // 去抖：读到暂停值须持续不少于它才判暂停
	// LoadingMinGap 是载入值的采样门槛 G（#218，doc/design/qqmusic-lyric-association-design.md §6.5 第 5 步）：换歌认领时，
	// 轮询间隔与窗口里相邻两次播放状态读数的间隔都小于它，「没见到载入值」才算「同一次播放被 QQ 改名」的证据。常量；未经
	// 设计 §10 第 26 项实测核定的版本为 0，即不作改名证据。只为实测核定过的版本填值，不从别的版本借用；改它是单独的小提交，送审。
	// 22.82 已核定（rc.14 验收，32 次换歌，作废 3 次）：最短载入 Lᵢ 20 ms，算得 10 ms，不大于轮询下限 10 ms，任何配置下证据都
	// 不成立，与 0 等效，维持 0。
	LoadingMinGap time.Duration
}

// classify 按本版本的取值集合给一次读数分类：在 Paused 里为暂停，在 Playing 里为在播，在 Loading 里为载入，其余为
// 未知。读失败（PlayStateOK 为假）不经这里，由 judgeTick 直接按未知处理。
func (s playStateSpec) classify(raw uint32) playState {
	switch {
	case slices.Contains(s.Paused, raw):
		return statePaused
	case slices.Contains(s.Playing, raw):
		return statePlaying
	case slices.Contains(s.Loading, raw):
		return stateLoading
	}
	return stateUnknown
}

// knownVersions 版本偏移表
var knownVersions = map[string]dllOffsets{
	"20.05": {
		// CE 逆向 v20.05（便携版，32-bit x86）
		// QQMusic.dll base 0x5EC50000，BSS 0x5F73EE00-0x5F7B5302
		// 字符串类型：WCHAR* 指针（UTF-16 堆字符串），时长单位：秒
		// SongMidParamsOff: struct1+0xAC → ptr → UTF-16 "0=<songMid>&2=<v20id>|<id>"
		//   扫描验证：切歌后该堆地址内容就地更新（67→212 命中，7个交集地址之一）
		Struct1:           0xB63ED0,
		Struct2Ptr:        0,
		FastTimerPtr:      0,
		FastTimerOff:      0,
		SongIDOff:         0x40,
		DurationOff:       0x44,
		ProgressOff:       0x68,     // 静态值(文件大小KB)，仅作回退
		ProgressDllOff:    0xB61E78, // CE 手动确认，准确ms进度（备选 0xAF3600）
		NameOff:           0x70,
		SingerOff:         0x74,
		AlbumOff:          0x78,
		StreamURLOff:      0x80, // struct1+0x80 → WCHAR* → "http://.../00281PXu4DHKNp.wma"
		SongMidParamsOff:  0xAC, // 备选: "0=<songMid>&2=..."
		UseWideStrings:    true,
		DurationInSeconds: true,
		// 播放状态（#172）：tools/qqstateprobe 实测（2026-11-22，录像 qq172-2005.bin，420 s，2101 拍；构建目录未记录，
		// 探针只记 major.minor）。QQMusic.dll+0xAF35B0（.data+0x245B0；镜像 0xB63514 逐拍相同，不用），0 在播、1 暂停；
		// 静态可写节里没有 22.82 那种 2/3/4 的枚举。核实清单（设计 §6.8）：第 1 项 21.7、33.7、44.5 s 三次暂停与恢复；
		// 第 3 项拖动松手前读数停 1.4 s（109.1、117.3 s）期间为 0；第 4 项换歌后立刻暂停（74.7、90.9 s）为 1；第 5 项
		// 全程只取 0、1；第 2 项 4 次换歌中 51.7 s 那次在换歌那一拍闪 1，前后两拍为 0，闪烁短于 400 ms（取样上界），
		// MinPause 取 300 ms（盖住一拍取样下看到的长度，300～400 ms 的闪烁盖不住，设计 §6.1）；第 6 项（重启 QQ 后
		// 同一 RVA）未做。在播值是 0，偏移失效时最常读到的就是它，读失败必须报告（readPlayState）。
		PlayState: playStateSpec{Off: 0xAF35B0, Paused: []uint32{1}, Playing: []uint32{0}, MinPause: 300 * time.Millisecond},
	},
	"21.81": {
		// CE Lua 逆向 v21.81（32-bit x86）
		// SSO 字符串布局与 v22.16 相同，DLL base 0x72780000
		// 验证: 歌名"无地自容" SSO@DLL+0xB75840，歌手"黑豹乐队"，专辑"黑豹 同名专辑"
		// SongID=101832223 Duration=338333ms Progress=实时ms
		Struct1:      0xB75840,
		Struct2Ptr:   0,
		FastTimerPtr: 0,
		FastTimerOff: 0,
		NameOff:      0x00,
		SingerOff:    0x18,
		AlbumOff:     0x30,
		SongIDOff:    0x60,
		DurationOff:  0x68,
		ProgressOff:  0x6C,
	},
	"22.16": {
		Struct1:      0xC87C80,
		Struct2Ptr:   0xC86B00,
		FastTimerPtr: 0xC157D8,
		FastTimerOff: 0x618,
		NameOff:      0x00,
		SingerOff:    0x18,
		AlbumOff:     0x30,
		SongIDOff:    0x60,
		DurationOff:  0x68,
		ProgressOff:  0x6C,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-22，录像 qq172-2216.bin，420 s，2101 拍；重启 QQ 后
		// qq172-2216-restart.bin，150 s，751 拍；2026-11-23 真值补测 qq172-2216-gt.bin，383 s，1914 拍；构建目录未记录，
		// 探针只记 major.minor）。QQMusic.dll+0xC87CF0（.data+0x9CCF0，即 Struct1+0x70、SongIDOff+0x10），A 类字段，DWORD：
		// 0 暂停、1 在播、2 未开播或换歌开始、3 换歌载入中；静态可写节里没有 22.52、22.82 那种 2、3、4 的枚举。核实清单
		// （设计 §6.8）：第 1 项 81.7→87.5 s、97.7→104.1 s 等暂停与恢复，暂停中的补写（变 0 之后 0.2～0.6 s，至多一次）期间
		// 为 0；第 2 项换歌（116.1、127.1、138.3、156.7 s）不出现 0，过渡为 3（156.7 s 那次先 2 一拍、再 3 三拍，上界 1.0 s，
		// 落在 #96 的等待里），随后为 1；第 3 项换歌、拖动（179.9、191.5 s）、恢复之后读数停走期间为 1，按拍量最长 1.2 s；
		// 第 4 项换歌后立刻暂停（138.9、158.3 s）为 0，含补写；第 5 项三份录像只取 0～3。第 6 项部分：三份录像跨三个进程、
		// 两次开机（QQMusic.dll 基址 0x618E0000、0x5E4A0000），同一 RVA 的未开播、暂停与恢复、换歌语义不变；缺重启后换歌
		// 立即暂停，待真机验收。换歌不出现暂停值，MinPause 为 0。QQ 载入上次的歌而未开播时一直为 2，开播即变 1；2、3 归
		// 载入、不算在播（设计 §6.1）：按在播的话认领后 3 s 触发交叉核对甲，整个连接退回停走判定。暂停值是 0：偏移失效读到
		// 0 时判暂停，由交叉核对乙发现。补写早于 1 s，暂停值又常与最后一次前进同拍出现，交叉核对乙的余量见设计 §6.3。
		// 同步翻转的 0/1 字段 0xC85F14（1 暂停）未开播时为 0，与在播分不开，不用。
		PlayState: playStateSpec{Off: 0xC87CF0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1 处，.wma 之后为 0=。Struct1 为窄字符串，前缀按 readSongPrefix
		// 的窄字符一支读；专辑、歌手的偏移未逐段核对，待真机（设计 §10 第 12 项）。
		SongRecord: true,
	},
	"22.22": {
		Struct1:      0xC95EA0,
		Struct2Ptr:   0, // v22.22 中 Struct2 不再可用
		FastTimerPtr: 0xC23994,
		FastTimerOff: 0x798,
		NameOff:      0x80,
		SingerOff:    0x98,
		AlbumOff:     0xB0,
		SongIDOff:    0xE0,
		DurationOff:  0xE8,
		ProgressOff:  0xEC,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-22，录像 qq172-2222.bin，420 s，2101 拍；重启 QQ 后
		// qq172-2222-restart.bin，146 s，729 拍；2026-11-23 真值补测 qq172-2222-gt.bin，355 s，1775 拍；构建目录未记录）。
		// QQMusic.dll+0xC95F90（.data+0x9CF90，即 Struct1+0xF0、SongIDOff+0x10；与 22.16 的 Struct1+0x70 是同一字段，显示
		// 结构体整体后移 0x80），A 类字段，DWORD：0 暂停、1 在播、2 未开播或换歌开始、3 换歌载入中；静态可写节里没有 2、3、4
		// 的枚举。核实清单（设计 §6.8）：第 1 项 39.3→45.5 s、51.1→55.5 s 等暂停与恢复，暂停中的补写（变 0 之后 0.2～0.8 s，
		// 至多一次）期间为 0；第 2 项换歌（60.3、71.7、84.1、100.5 s）不出现 0，过渡为 2、3 至多三拍（上界 0.8 s），随后为 1；
		// 第 3 项换歌、拖动（120.3、130.1、151.7 s）、恢复之后读数停走期间为 1，按拍量最长 1.2 s；第 4 项换歌后立刻暂停
		// （85.7、101.3 s）为 0，含补写；第 5 项三份录像只取 0～3。第 6 项部分：三份录像的 QQMusic.dll 基址各不相同
		// （0x610F0000、0x618D0000、0x5E490000），同一 RVA 的未开播、暂停与恢复、换歌语义不变；缺重启后换歌立即暂停，待
		// 真机验收。MinPause、载入的归类、暂停值为 0 的后果与交叉核对乙的余量同 22.16。同步翻转的 0/1 字段 0xC95B84
		// （1 暂停）在 60.3 s 换歌时闪 1、未开播时为 0，不用。
		PlayState: playStateSpec{Off: 0xC95F90, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1 处，.wma 之后为 0=。Struct1 为窄字符串，前缀按 readSongPrefix
		// 的窄字符一支读；专辑、歌手的偏移未逐段核对，待真机（设计 §10 第 12 项）。
		SongRecord: true,
	},
	"22.26": {
		// qqprobe 实测（32-bit x86，2026-11-23，构建目录 TOOLS\QQMusic-v22.26-Modified\QQMusic：QQMusic.exe 签名有效，
		// QQMusic.dll 被改过，本机没有官方构建）：「君の知らない物語 (Inst.) / supercell」命中宽字符显示对象（UTF-16 歌名
		// 94 处、GBK 0 处），读回专辑「君の知らない物語」，时长 340400ms；会话 songID=268359544 精确命中、自带时长与显示时长
		// 一致，2026-11-24 经单曲详情接口核对为该曲（songmid 000JptHs40Om4N）。字段布局同 22.31+。按修改版收录：官方构建的
		// 静态数据 RVA 推断与修改版相同、未核对（项目负责人 2026-11-24 定，#232；设计 §1.3「22.26 的构建」、§10）。
		Struct1:           0xC281E8,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x71DA8,
		SongIDDurCheckOff: 0x71DB0,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe（加了本版偏移的副本）实测（2026-11-23，录像 qq172-2226.bin，493 s，
		// 2467 拍；重启 QQ 后 qq172-2226-restart.bin，108 s，542 拍）。QQMusic.dll+0xC99FA0（.data+0x9CFA0，即 Struct1+0x71DB8、
		// SongIDOff+0x10），A 类字段，DWORD：0 暂停、1 在播、2 未开播或换歌开始、3 载入；静态可写节里没有 2、3、4 的枚举。
		// 核实清单（设计 §6.8）：第 1 项 14.9→25.5 s、41.7→51.1 s 等暂停与恢复，补写期间为 0；第 2 项「下一首」（69.3、83.1、
		// 101.5、127.1 s）、四个入口与单曲循环放完（230.3、468.3 s）不出现 0，过渡为 2、3 至多两拍；第 3 项起步停顿为 1（偶尔
		// 夹一拍 3，如 274.3、318.5、335.5 s）；第 4 项换歌后立刻暂停（102.1、127.5 s）为 0；第 5 项两份录像只取 0～3。第 6 项
		// 部分：重启录像（同一次开机，基址相同）里同一 RVA 的未开播、暂停与恢复、换歌语义不变；缺重启后换歌立即暂停，待真机
		// 验收。换歌不出现暂停值，MinPause 为 0。QQ 载入上次的歌而未开播时一直为 2，开播先 3 一拍再变 1；2、3 归载入、不算
		// 在播（设计 §6.1）。暂停值是 0：偏移失效读到 0 时判暂停，由交叉核对乙发现。暂停中拖动 QQ 随即起播，读数跳转比
		// 标志变 1 早 0.4 s。
		PlayState: playStateSpec{Off: 0xC99FA0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测，专辑页（id 键，QQ 显示 id 键条目）、最近播放两次与搜索结果（mid 键，QQ
		// 显示 mid 键条目）四个入口下记录网址的键与 QQ 所用的键 4/4 一致，前缀各命中 1 处，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.31": {
		// CE 实测（32-bit x86，2026-07-18）：与 22.41 同一套宽字符模型（UTF-16 WCHAR* 指针，
		// 非 22.22 的窄 SSO——宽字符切换发生在 22.22→22.26 之间）。now-playing 显示对象布局
		// 与 22.41 逐字段一致，仅基址不同：+0x00 Name*、+0x04 Singer*、+0x08 Album*、
		// +0x0C 进度ms（实测亚秒高分辨率 → 无需 FastTimer）、+0x10 时长ms。
		// 上报 songid 恒 0；真数字 songID 同样在「播放会话」结构里（87/95 签名后第 8 字节），
		// 距 Struct1 0x71FD8（绝对 QQMusic.dll+0xCA52B0）。实测：享福服务器 songID=621653818
		// (0x250DAF3A) 恰落此偏移。SongIDDurCheckOff（+0x71FE0=DLL+0xCA52B8）自带时长与显示
		// 时长交叉核对，防换歌瞬间读到上一首。SongMidFromHeap 兜底。
		Struct1:           0xC332D8,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x71FD8,
		SongIDDurCheckOff: 0x71FE0,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2231.bin，321 s，1604 拍；真值补测
		// qq172-2231-gt.bin，307 s，1534 拍；重启 QQ 后 qq172-2231-restart.bin，100 s，500 拍；构建目录未记录）。
		// QQMusic.dll+0xCA52C0（.data+0x9D2C0，即 Struct1+0x71FE8、SongIDOff+0x10），A 类字段，DWORD：0 暂停、1 在播、2 未开播
		// 或换歌开始、3 载入；静态可写节里没有 2、3、4 的枚举。核实清单（设计 §6.8）：第 1 项 58.5→67.5 s、82.5→91.1 s 等暂停
		// 与恢复，补写期间为 0；第 2 项换歌（105.1、119.7、133.5、158.5 s）不出现 0，过渡为 3 至多两拍（上界 0.6 s）；第 3 项
		// 起步停顿为 1（145.3、300.9 s 各夹一拍 3），自锚点按拍量最长 2.0 s（105.1→107.1 s）；第 4 项换歌后立刻暂停（133.9、
		// 158.9 s）为 0；第 5 项三份录像只取 0～3。第 6 项部分：重启录像（同一次开机，基址相同）里同一 RVA 的未开播、暂停与
		// 恢复、换歌语义不变；缺重启后换歌立即暂停，待真机验收。换歌不出现暂停值，MinPause 为 0。QQ 载入上次的歌而未开播时
		// 一直为 2，开播先 3 一两拍再变 1；2、3 归载入、不算在播（设计 §6.1）：按在播的话认领后 3 s 触发交叉核对甲。暂停值
		// 是 0：偏移失效读到 0 时判暂停，由交叉核对乙发现。
		PlayState: playStateSpec{Off: 0xCA52C0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1～2 处、同一网址，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.41": {
		// CE 实测（32-bit x86）：结构体由窄 SSO 改为 UTF-16 WCHAR* 指针。
		// now-playing 显示对象在 QQMusic.dll 静态 .data（Struct1），跨歌验证字段原地更新：
		//   +0x00 Name*、+0x04 Singer*、+0x08 Album*、+0x0C 进度ms（亚秒高分辨率 →
		//   无需 FastTimer）、+0x10 时长ms。
		// 客户端上报 JSON 的 songid 恒 0，但真数字 songID 存在另一处固定「播放会话」结构里
		// （volume/speed 浮点 + songID + 时长），距 Struct1 0x722E8（绝对 QQMusic.dll+0xCB2830）。
		// 跨歌验证：上一首 2480434、当前 351669598 均落此偏移且与服务器一致。用它走现成的
		// songID 取词路径（musicu.fcg），无需堆扫描、无时序竞态。
		// 该结构与显示对象在换歌瞬间可能短暂不同步 → 用它自带时长（+0x722F0=DLL+0xCB2838）
		// 与显示时长交叉核对（SongIDDurCheckOff），不一致则弃用该 songID。
		// SongMidFromHeap 保留作兜底：songID 未就绪（读到 0）时回退 FindSongMid 堆扫描。
		Struct1:           0xC40548,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x722E8, // → QQMusic.dll+0xCB2830，稳定 songID 字段
		SongIDDurCheckOff: 0x722F0, // → QQMusic.dll+0xCB2838，同结构自带时长，用于同步性核对
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2241.bin，304 s，1519 拍；真值补测
		// qq172-2241-gt.bin，306 s，1531 拍；重启 QQ 后 qq172-2241-restart.bin，96 s，482 拍；构建目录未记录）。
		// QQMusic.dll+0xCB2840（.data+0x9D840，即 Struct1+0x722F8、SongIDOff+0x10），A 类字段，取值、MinPause 与载入的归类
		// 同 22.31；静态可写节里没有 2、3、4 的枚举。核实清单（设计 §6.8）：第 1 项 58.7→66.7 s、80.9→89.7 s 等暂停与恢复，
		// 补写期间为 0；第 2 项换歌（103.9、118.3、132.9、159.5 s，132.9 s 为占位串拍、取 2）不出现 0；第 3 项起步停顿为 1；
		// 第 4 项换歌后立刻暂停（133.5、159.9 s）为 0；第 5 项三份录像只取 0～3。第 6 项部分，同 22.31。
		PlayState: playStateSpec{Off: 0xCB2840, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1～2 处、同一网址，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.52": {
		// CE 实测（32-bit x86，2026-08-13）：字段布局与 22.41 一致，仅静态地址迁移。
		Struct1:           0xC58BD0,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x726C0,
		SongIDDurCheckOff: 0x726C8,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172）：tools/qqstateprobe 实测（2026-11-22，录像 qq172-2252.bin，420 s，2101 拍；构建目录未记录）。
		// QQMusic.dll+0xCC91C0（.data+0x9C1C0），与 22.82 的 0xD3BBA8 同语义的枚举：3 播放、4 暂停、2 换歌载入中。
		// 核实清单（设计 §6.8）：第 1 项九段暂停与恢复（如 29.7→34.1 s、135.1→142.1 s），暂停中的补写期间为 4；
		// 第 2 项五次换歌读到新歌名那一拍为 2、0.2 s 后回 3，不出现 4（210.3 s 一次按拍没读到 2，全程为 3）；第 3 项
		// 换歌、拖动（117.7、126.3 s）、恢复之后读数停走期间为 3；第 4 项换歌后立刻暂停（83.7、99.7 s）为 4；第 5 项
		// 全程只取 2、3、4。换歌不闪暂停值，MinPause 为 0。第 6 项（重启 QQ 后同一 RVA）未做。2 同 22.82 归载入（设计
		// §6.1）：同语义的枚举，QQ 启动后未开播时的取值未测，推断同为 2。
		PlayState: playStateSpec{Off: 0xCC91C0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
	},
	"22.60": {
		// qqprobe 实测（32-bit x86，2026-09-01，构建 22.60.02.13.33）：字段布局与 22.41/22.52
		// 宽字符模型逐字段一致，仅静态地址迁移。struct1=QQMusic.dll+0xC6DBA0，歌名/歌手指针
		// 精确命中（"奶茶加糖"/"贰茉EMO"）。songID 会话结构 SongIDOff 较 22.52(0x726C0) 迁移
		// +0x30；songID=231738927 经 QQ 搜索接口核对为该曲(songmid 001263nZ3ZMV1x)的数字 id。
		Struct1:           0xC6DBA0,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x726F0,
		SongIDDurCheckOff: 0x726F8,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2260.bin，285 s，1423 拍；真值补测
		// qq172-2260-gt.bin，273 s，1363 拍；重启 QQ 后 qq172-2260-restart.bin，79 s，395 拍；构建目录未记录）。
		// QQMusic.dll+0xCDE1B0（.data+0x9C1B0，即 Struct1+0x70610），E 类字段，与 22.82 的 0xD3BBA8 同语义的枚举：3 在播、
		// 4 暂停、2 换歌载入中或未开播。核实清单（设计 §6.8）：第 1 项 50.7→58.1 s、71.3→78.9 s 等暂停与恢复，补写期间为 4；
		// 第 2 项换歌（92.3、104.9、118.1、142.7 s）为 2 至多两拍、随后为 3，不出现 4；第 3 项起步停顿为 3；第 4 项换歌后立刻
		// 暂停（118.5、143.1 s）为 4；第 5 项主录像与重启录像只取 2～4，补测录像（其间没有暂停）只取 2、3。第 6 项部分：重启
		// 录像（同一次开机，基址相同）里同一 RVA 的未开播、暂停与恢复、换歌语义不变；缺重启后换歌立即暂停，待真机验收。换歌
		// 不出现暂停值，MinPause 为 0。QQ 载入上次的歌而未开播时一直为 2，开播即变 3；2 归载入、不算在播（同 22.82，设计
		// §6.1）。另有 A 类字段 0xCE02A0（SongIDOff+0x10），暂停段与本字段逐拍相同，换歌与开播时多闪 3，不用。
		PlayState: playStateSpec{Off: 0xCDE1B0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1 处，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.71": {
		// qqprobe 实测（32-bit x86，2026-09-29，构建目录 QQMusic2271.18.13.18）：
		// 与 22.31+ 一致
		Struct1:           0xC7BCF0,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x73C10,
		SongIDDurCheckOff: 0x73C18,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2271.bin，284 s，1421 拍；真值补测
		// qq172-2271-gt.bin，201 s，1007 拍；重启 QQ 后 qq172-2271-restart.bin，71 s，357 拍；构建目录未记录）。
		// QQMusic.dll+0xCEC400（.data+0x9C400，即 Struct1+0x70710），E 类字段，取值、MinPause 与载入的归类同 22.60。核实清单
		// （设计 §6.8）：第 1 项 51.9→59.7 s、73.1→80.7 s 等暂停与恢复，补写期间为 4；第 2 项换歌（94.9、107.3、120.9、
		// 147.1 s）为 2、随后为 3，不出现 4；第 3 项起步停顿为 3；第 4 项换歌后立刻暂停（121.5、147.5 s）为 4；第 5 项只取
		// 2～4。第 6 项部分，同 22.60。另有 A 类字段 0xCEF910（SongIDOff+0x10），换歌与开播时多闪 3，不用。
		PlayState: playStateSpec{Off: 0xCEC400, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1 处，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.76": {
		// qqprobe 实测（32-bit x86，2026-10-14，构建目录 QQMusic2276.00.18.59）：
		// 「享福 / 小思孩子」命中宽字符显示对象，时长 137223ms；会话 songID=621653818
		// 与该曲已知 ID 一致。相较 22.71，Struct1 后移 0x2000，字段及会话相对偏移不变。
		Struct1:           0xC7DCF0,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x73C10,
		SongIDDurCheckOff: 0x73C18,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2276.bin，322 s，1609 拍；真值补测
		// qq172-2276-gt.bin，225 s，1124 拍；重启 QQ 后 qq172-2276-restart.bin，63 s，316 拍；构建目录未记录）。
		// QQMusic.dll+0xCEE400（.data+0x9C400，即 Struct1+0x70710），E 类字段，取值、MinPause 与载入的归类同 22.60。核实清单
		// （设计 §6.8）：第 1 项 50.7→79.7 s、101.1→111.1 s 等暂停与恢复，补写期间为 4；第 2 项换歌（125.9、138.1、151.5、
		// 178.1 s）为 2 至多两拍、随后为 3，不出现 4；第 3 项起步停顿为 3；第 4 项换歌后立刻暂停（151.9、178.5 s）为 4；第 5 项
		// 只取 2～4。第 6 项部分，同 22.60。另有 A 类字段 0xCF1910（SongIDOff+0x10），换歌与开播时多闪 3，不用。
		PlayState: playStateSpec{Off: 0xCEE400, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1 处，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.81": {
		// qqprobe 实测（32-bit x86，2026-11-10，构建目录 QQMusic2281.02.26.01）：
		// 「Change of Coast / Neon Indian」命中宽字符显示对象，时长 192733ms；会话
		// songID=5020330，经单曲详情接口核对为该曲（专辑 GTA V Volume 2: The Score）。
		// 字段布局同 22.31+，Struct1 与会话相对偏移均迁移。
		Struct1:           0xCCB488,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x73F20,
		SongIDDurCheckOff: 0x73F28,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172、#222）：tools/qqstateprobe 实测（2026-11-23，录像 qq172-2281.bin，301 s，1503 拍；真值补测
		// qq172-2281-gt.bin，220 s，1099 拍；重启 QQ 后 qq172-2281-restart.bin，64 s，320 拍；构建目录未记录）。
		// QQMusic.dll+0xD3BBC0（.data+0x9CBC0，即 Struct1+0x70738），E 类字段，取值、MinPause 与载入的归类同 22.60。核实清单
		// （设计 §6.8）：第 1 项 60.7→69.3 s、84.1→92.5 s 等暂停与恢复，补写期间为 4；第 2 项换歌（107.1、120.1、133.5、
		// 160.9 s）为 2、随后为 3，不出现 4；第 3 项起步停顿为 3；第 4 项换歌后立刻暂停（133.9、161.5 s）为 4；第 5 项只取
		// 2～4。第 6 项部分，同 22.60。另有 A 类字段 0xD3F3B8（SongIDOff+0x10），换歌与开播时多闪 3，不用。
		PlayState: playStateSpec{Off: 0xD3BBC0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		// 当前歌曲记录（#218）：2026-11-23 真值补测（设计 §6.4 第 4 步「版本」），专辑页、最近播放、搜索结果、最近播放四个入口下
		// QQ 显示所用的键与记录网址的键 4/4 一致，前缀每次命中 1～2 处、同一网址，.wma 之后为 0=。
		SongRecord: true,
	},
	"22.82": {
		// qqprobe 实测（32-bit x86，2026-11-16，构建目录 QQMusic2282.03.58.27）：
		// 「Lettersong (致十年后的我) / Akie秋绘」命中宽字符显示对象，时长 340768ms；会话
		// songID=203398178，经单曲详情接口核对为该曲（专辑 夏鸣。2016）。
		// 字段布局同 22.31+，相较 22.81 Struct1 前移 0x20、会话相对偏移后移 0x8。
		Struct1:           0xCCB468,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x73F28,
		SongIDDurCheckOff: 0x73F30,
		UseWideStrings:    true,
		SongMidFromHeap:   true,
		// 播放状态（#172）：tools/qqstateprobe 实测（2026-11-22，录像 qq172-r1.bin，420 s，2084 拍；该次的构建目录
		// 未记录，上面 11-16 的构建目录不一定是同一个）。QQMusic.dll+0xD3BBA8（.data+0x9CBA8），DWORD：3 播放、4 暂停、
		// 2 换歌载入中。核实清单（设计 §6.8）：第 1 项 2.5→7.7 s、12.9→18.5 s 两次暂停与恢复，暂停中的补写期间为 4；
		// 第 2 项换歌（29.3、40.7、189.9 s）读到新歌名那一拍为 2、随后为 3，不出现 4；第 3 项换歌、拖动（130.5、137.9、
		// 146.5 s）、恢复之后读数停走期间为 3；第 4 项换歌后立刻暂停（52.1、69.7、87.5 s）为 4，含 +766 ms 的补写；
		// 第 5 项全程只取 2、3、4。换歌不闪暂停值，MinPause 为 0。第 6 项见真机验收另录的 qq2282-restart.bin（2026-11-22，
		// 设计 §1.3「QQ 启动后未开播」）：重启 QQ 后同一 RVA 取值语义不变；QQ 载入上次的歌而未开播时一直为 2，读数不动，
		// 开播即变 3。2 因此归载入、不算在播（设计 §6.1）：按在播的话认领后 3 s 触发交叉核对甲，整个连接退回停走判定；
		// 载入按停走判定，停走自进入 2 的那一拍起量，换歌那一拍 0.2～0.4 s 的 2 不会判暂停。
		PlayState: playStateSpec{Off: 0xD3BBA8, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		// 当前歌曲记录（#218）：2026-11-22 真机只读扫描（设计 §2.2，#218 评论 7465），专辑页与最近播放两种入口下，Struct1 的
		// 专辑、歌手、歌名（+0x08、+0x04、+0x00 的 UTF-16 指针）拼成的前缀在全部可读内存里都只命中当前那条，命中都在
		// MEM_PRIVATE、PAGE_READWRITE 的区域。
		SongRecord: true,
	},
}

// parseVer 把 "22.21" 解析成可比较的整数（major*1000+minor）。detectVersion 用
// fmt.Sprintf("%d.%02d") 产出版本号，格式恒为 major.minor。
func parseVer(s string) (int, bool) {
	maj, min, ok := strings.Cut(s, ".")
	if !ok {
		return 0, false
	}
	a, e1 := strconv.Atoi(maj)
	b, e2 := strconv.Atoi(min)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	return a*1000 + b, true
}

// nearestKnownVersion 返回 knownVersions 里 semver 距离 detected 最近的 key。
//
// 未知版本没有正确的偏移表，只能挑一个已知版本的偏移去赌。写死某个版本（此前是 22.16）
// 对「比最新还新」的版本必错——22.16 是窄 SSO，而 22.31/22.41 已切宽 WCHAR*，用窄模型
// 读宽指针立刻乱码。按距离取最近至少让这种版本落到 22.41、字符串模型跟得上。
//
// 但这是启发式，不是保证：版本号距离 ≠ 结构距离。22.16→22.22 字段偏移整个搬家（Name
// 0x00→0x80）、22.22→22.26 窄转宽，正好落在两代之间的未知版本仍是赌，且跨字符串模型
// 切换点会赌错。所以走这条路径必须上报读到的歌名样本（见 ReadAllMetadata），让野外数据
// 判断赌对没有、哪些版本需要补表。
//
// 距离相等时取版本号更大的一侧：确定的 tie-break（不受 map 迭代序影响），且新版更可能
// 延续最近的结构。
func nearestKnownVersion(detected string) string {
	dv, ok := parseVer(detected)
	if !ok {
		return latestKnownVersion()
	}
	best := ""
	bestDist, bestVer := -1, -1
	for k := range knownVersions {
		kv, ok := parseVer(k)
		if !ok {
			continue
		}
		d := kv - dv
		if d < 0 {
			d = -d
		}
		if bestDist < 0 || d < bestDist || (d == bestDist && kv > bestVer) {
			bestDist, bestVer, best = d, kv, k
		}
	}
	return best
}

// latestKnownVersion 返回版本号最大的已知 key。用作「连版本号都读不到」时的默认——
// 比写死最老版本更贴近现实：读不到版本的机器若是新装的，多半是新版本。
func latestKnownVersion() string {
	best := ""
	bestVer := -1
	for k := range knownVersions {
		if kv, ok := parseVer(k); ok && kv > bestVer {
			bestVer, best = kv, k
		}
	}
	return best
}

type QQMusicMem struct {
	pid            uint32
	hProcess       uintptr
	qqmusicDllBase uintptr
	gfWrapperBase  uintptr
	gfWrapperSize  uint32
	kernel32Base   uintptr    // kernel32.dll base in the target 32-bit process
	sliderPointer  uintptr    // Dynamic memory cave address where EDI is stored
	progressPtr    uintptr    // Address where hooked progress (ms) is stored
	progressTsPtr  uintptr    // Address where GetTickCount timestamp is stored
	version        string     // 检测到的 QQ 音乐版本号，如 "22.16"
	offsets        dllOffsets // 当前版本的偏移量
	fallbackTo     string     // 非空=version 无偏移表、按 nearestKnownVersion 回退到的版本；用于上报歌名样本判断赌对没有

	// 仅离线用例注入，生产恒为 nil。
	checkValidFn func() bool
	readMetaFn   func() (*SongMetadata, error)
}

// Close 关闭进程句柄并置 0；可重复调用，对 nil 或未持有句柄的实例无操作。
func (m *QQMusicMem) Close() {
	if m == nil || m.hProcess == 0 {
		return
	}
	h := m.hProcess
	m.hProcess = 0
	windows.CloseHandle(windows.Handle(h))
}

func (m *QQMusicMem) CheckValid() bool {
	if m == nil {
		return false
	}
	if m.checkValidFn != nil {
		return m.checkValidFn()
	}
	if m.hProcess == 0 {
		return false
	}
	var exitCode uint32
	err := windows.GetExitCodeProcess(windows.Handle(m.hProcess), &exitCode)
	if err != nil || exitCode != 259 { // STILL_ACTIVE is 259
		m.Close()
		return false
	}
	return true
}

type attachOps struct {
	openProcess   func(pid uint32) (windows.Handle, error)
	closeHandle   func(h windows.Handle) error
	probeModules  func(h windows.Handle) (dllBase, gfBase uintptr, gfSize uint32, k32Base uintptr, found bool)
	detectVersion func(m *QQMusicMem)
}

var defaultAttachOps = attachOps{
	openProcess: func(pid uint32) (windows.Handle, error) {
		return windows.OpenProcess(qqProcessAccess, false, pid)
	},
	closeHandle:   windows.CloseHandle,
	probeModules:  probeQQMusicModules,
	detectVersion: (*QQMusicMem).detectVersion,
}

func probeQQMusicModules(hProcess windows.Handle) (dllBase, gfBase uintptr, gfSize uint32, k32Base uintptr, found bool) {
	var modules [1024]uintptr
	var cbNeeded uint32

	retEnum, _, _ := procEnumProcessModulesEx.Call(
		uintptr(hProcess),
		uintptr(unsafe.Pointer(&modules[0])),
		uintptr(unsafe.Sizeof(modules)),
		uintptr(unsafe.Pointer(&cbNeeded)),
		uintptr(LIST_MODULES_ALL),
	)
	if retEnum == 0 {
		return 0, 0, 0, 0, false
	}

	// cbNeeded 是「**所需**字节数」不是「已写入字节数」：EnumProcessModulesEx 在缓冲区
	// 不足时仍返回 TRUE，只把实际需要的大小写进 cbNeeded（MSDN 明确如此）。不截断的话，
	// 进程模块数 > 1024 时 numModules > len(modules) → modules[i] 越界 panic。
	// 而全仓 recover() 零命中 → 任一 goroutine panic = **整个进程死亡**，不只是取词停摆。
	//
	// 截断而非按 MSDN 的两遍模式重新分配：漏找几个 DLL 的降级后果是 ConnectQQMusic 返错
	// → qqmusic 的重试循环继续转，已经够用；两遍模式要多一次动态分配 + 一次 syscall。
	numModules := cbNeeded / uint32(unsafe.Sizeof(modules[0]))
	if numModules > uint32(len(modules)) {
		log.Warn("QQMusic.exe module count %d exceeds buffer %d; scanning only the first %d (missing DLL will trigger retry)",
			numModules, len(modules), len(modules))
		numModules = uint32(len(modules))
	}
	for i := uint32(0); i < numModules; i++ {
		var name [256]uint16
		procGetModuleBaseNameW.Call(uintptr(hProcess), modules[i], uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		modName := syscall.UTF16ToString(name[:])
		lowerName := strings.ToLower(modName)

		if lowerName == "qqmusic.dll" {
			dllBase = modules[i]
			found = true
		}
		// v22.16: QQMusic_GFWrapper.dll, v22.22+: 仍存在但也新增 GF.dll
		if lowerName == "qqmusic_gfwrapper.dll" || lowerName == "gf.dll" {
			// 优先使用 GFWrapper（滑块 AOB 在其中），GF.dll 作为备选
			if gfBase == 0 || lowerName == "qqmusic_gfwrapper.dll" {
				gfBase = modules[i]
				var minfo MODULEINFO
				procGetModuleInformation.Call(uintptr(hProcess), modules[i], uintptr(unsafe.Pointer(&minfo)), uintptr(unsafe.Sizeof(minfo)))
				gfSize = minfo.SizeOfImage
			}
		}
		if lowerName == "kernel32.dll" {
			k32Base = modules[i]
		}
	}
	return dllBase, gfBase, gfSize, k32Base, found
}

var (
	// ConnectQQMusic / attachQQMusic 错误分类哨兵（#231）：供外层循环按进程层次合成非歌名状态。
	errQQMusicNotRunning     = i18n.Sentinel("QQMusic.exe not found")
	errQQMusicAccessDenied   = i18n.Sentinel("cannot open QQMusic.exe process (access denied)")
	errQQMusicModuleNotReady = i18n.Sentinel("QQMusic.dll not found in any QQMusic.exe process")

	// connectQQMusic 是 ConnectQQMusic 的底层入口，离线用例替换它以驱动 Start。
	connectQQMusic = connectQQMusicProcess
)

func attachQQMusicPID(pid uint32, ops attachOps) (*QQMusicMem, error) {
	h, err := ops.openProcess(pid)
	if err != nil {
		return nil, err
	}
	if h == 0 {
		return nil, nil
	}
	keep := false
	defer func() {
		if !keep {
			ops.closeHandle(h)
		}
	}()

	dllBase, gfBase, gfSize, k32Base, found := ops.probeModules(h)
	if !found {
		return nil, nil
	}

	mem := &QQMusicMem{
		pid:            pid,
		hProcess:       uintptr(h),
		qqmusicDllBase: dllBase,
		gfWrapperBase:  gfBase,
		gfWrapperSize:  gfSize,
		kernel32Base:   k32Base,
	}
	// 检测版本并加载偏移表
	ops.detectVersion(mem)
	keep = true
	return mem, nil
}

func attachQQMusic(pids []uint32, ops attachOps) (*QQMusicMem, error) {
	sawAccessDenied := false
	for _, pid := range pids {
		mem, err := attachQQMusicPID(pid, ops)
		if mem != nil {
			return mem, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			sawAccessDenied = true
		}
	}
	if sawAccessDenied {
		return nil, errQQMusicAccessDenied
	}
	return nil, errQQMusicModuleNotReady
}

// findQQMusicPIDs 枚举当前系统里所有 QQMusic.exe 的进程 ID。
func findQQMusicPIDs() ([]uint32, error) {
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPPROCESS), 0)
	if hSnap == uintptr(syscall.InvalidHandle) {
		return nil, i18n.Errorf("CreateToolhelp32Snapshot failed")
	}
	defer procCloseHandle.Call(hSnap)

	var pe32 PROCESSENTRY32W
	pe32.Size = uint32(unsafe.Sizeof(pe32))

	var pids []uint32
	ret, _, _ := procProcess32First.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	for ret != 0 {
		name := syscall.UTF16ToString(pe32.ExeFile[:])
		if strings.ToLower(name) == TargetProcessName {
			pids = append(pids, pe32.ProcessID)
		}
		ret, _, _ = procProcess32Next.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	}
	return pids, nil
}

// isQQMusicRunning 报告当前是否至少有一个 QQMusic.exe 进程在运行。
func isQQMusicRunning() bool {
	pids, err := findQQMusicPIDs()
	return err == nil && len(pids) > 0
}

func connectQQMusicProcess() (*QQMusicMem, error) {
	pids, err := findQQMusicPIDs()
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, errQQMusicNotRunning
	}
	return attachQQMusic(pids, defaultAttachOps)
}

func ConnectQQMusic() (*QQMusicMem, error) {
	return connectQQMusic()
}

// detectVersion 从 QQMusic.exe 的 PE 版本资源中提取版本号并匹配偏移表
func (m *QQMusicMem) detectVersion() {
	// 检测到的版本（已知 / 未知 / 读不到都算）设成全局 tag，让 Sentry 聚合「野外最多的是
	// 哪个 QQ 音乐版本」——异常上报只覆盖出问题的版本，这条覆盖全部。defer 兜住三条 return
	// 分支；m.version 在每条分支都已赋值。
	defer func() { telemetry.SetPlayerVersion("qqmusic", m.version) }()

	// 优先使用 QueryFullProcessImageNameW：直接从已打开的 hProcess 获取 exe 路径，
	// 避免 findQQMusicExePath 误返回其他版本的安装路径
	exePath := ""
	procQueryFullProcessImageNameW := modkernel32.NewProc("QueryFullProcessImageNameW")
	buf := make([]uint16, 512)
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImageNameW.Call(
		m.hProcess, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret != 0 && size > 0 {
		exePath = syscall.UTF16ToString(buf[:size])
	}

	// 回退：模块快照或已知安装路径
	if exePath == "" {
		exePath = findQQMusicExePath(m.pid)
	}

	// 连版本号都读不到（路径 / PE 资源失败）：没有可算距离的信息，退到最新已知版本。
	if exePath == "" {
		fb := latestKnownVersion()
		log.Warn("Cannot obtain QQMusic.exe path; falling back to latest known version v%s", fb)
		m.version = "unknown"
		m.fallbackTo = fb
		m.offsets = knownVersions[fb]
		return
	}

	major, minor := getFileVersion(exePath)
	if major == 0 && minor == 0 {
		fb := latestKnownVersion()
		log.Warn("Version detection failed; falling back to latest known version v%s", fb)
		m.version = "unknown"
		m.fallbackTo = fb
		m.offsets = knownVersions[fb]
		return
	}

	m.version = fmt.Sprintf("%d.%02d", major, minor)

	if offsets, ok := knownVersions[m.version]; ok {
		m.offsets = offsets
		log.Info("QQ Music version: %s (offsets matched)", m.version)
	} else {
		// 「预期之外」：这台机器上的 QQ 音乐是我们没见过的版本，没有正确的偏移表。
		// 按 semver 距离挑最近的已知版本去赌（而非写死某个版本）——读出来多半是垃圾
		// （见 AGENTS.md §5.1），这不是「降级运行」，是「正在读错内存」。上报移到
		// ReadAllMetadata：带上用这个 fallback 读到的歌名样本，才能判断赌对没有。
		fb := nearestKnownVersion(m.version)
		log.Warn("QQ Music version %s has no known offset table; falling back by distance to v%s", m.version, fb)
		m.fallbackTo = fb
		m.offsets = knownVersions[fb]
	}
}

// findQQMusicExePath 通过 PID 获取 QQMusic.exe 的完整路径
// 优先从进程模块快照读取实际路径，避免误读其他版本的安装路径
func findQQMusicExePath(pid uint32) string {
	// 方法 1: 通过模块快照获取目标进程的真实 exe 路径（优先）
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPMODULE|TH32CS_SNAPMODULE32), uintptr(pid))
	if hSnap != uintptr(syscall.InvalidHandle) {
		defer procCloseHandle.Call(hSnap)
		var me32 MODULEENTRY32W
		me32.Size = uint32(unsafe.Sizeof(me32))
		ret, _, _ := procModule32First.Call(hSnap, uintptr(unsafe.Pointer(&me32)))
		for ret != 0 {
			modName := syscall.UTF16ToString(me32.Module[:])
			if strings.EqualFold(modName, "QQMusic.exe") {
				return syscall.UTF16ToString(me32.ExePath[:])
			}
			ret, _, _ = procModule32Next.Call(hSnap, uintptr(unsafe.Pointer(&me32)))
		}
	}

	// 方法 2: 回退到已知安装路径（仅当模块快照失败时）
	knownPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Tencent", "QQMusic", "QQMusic.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Tencent", "QQMusic", "QQMusic.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Tencent", "QQMusic", "QQMusic.exe"),
	}
	for _, p := range knownPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// VS_FIXEDFILEINFO Windows PE 版本信息结构体
type vsFixedFileInfo struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// getFileVersion 从 PE 文件中提取 ProductVersion (major, minor)
func getFileVersion(path string) (major, minor uint16) {
	pathUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}

	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathUTF16)), 0)
	if size == 0 {
		return 0, 0
	}

	buf := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pathUTF16)),
		0,
		size,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return 0, 0
	}

	subBlock, _ := syscall.UTF16PtrFromString(`\`)
	var infoPtr uintptr
	var infoLen uint32
	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&infoPtr)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if ret == 0 || infoPtr == 0 {
		return 0, 0
	}

	// infoPtr 指向 buf 内部，但它是 uintptr——不持有对 buf 的引用。直接
	// (*vsFixedFileInfo)(unsafe.Pointer(infoPtr)) 会让 GC 有机会在下面解引用之前回收 buf。
	// 改为算偏移再回 buf 取地址：buf 因此保持存活，同时顺带做了越界校验。
	base := uintptr(unsafe.Pointer(&buf[0]))
	if infoPtr < base {
		return 0, 0
	}
	off := infoPtr - base
	if off+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(buf)) {
		return 0, 0
	}
	info := (*vsFixedFileInfo)(unsafe.Pointer(&buf[off]))
	if info.Signature != 0xFEEF04BD {
		return 0, 0
	}

	// ProductVersionMS: HIWORD=major, LOWORD=minor
	major = uint16(info.ProductVersionMS >> 16)
	minor = uint16(info.ProductVersionMS & 0xFFFF)
	return major, minor
}

// Version 返回检测到的 QQ 音乐版本号
func (m *QQMusicMem) Version() string {
	return m.version
}

func (m *QQMusicMem) Offsets() dllOffsets {
	return m.offsets
}

// playStateAddr 返回播放状态字段的绝对地址（#172）。本版本未收录（PlayState.Off 为 0），或按版本号距离借用了
// 别的版本的偏移表、读不到版本号而借用最新版本的（fallbackTo 非空）时为 0，即不读：借用偏移表时歌名赌对了，
// 状态偏移照样可能错，而错读状态比错读歌名难察觉。
func (m *QQMusicMem) playStateAddr() uintptr {
	if m.offsets.PlayState.Off == 0 || m.fallbackTo != "" {
		return 0
	}
	return m.qqmusicDllBase + m.offsets.PlayState.Off
}

// songRecordEnabled 报告本会话是否读 QQ 内存里的当前歌曲记录（#218，设计 §6.4 第 4 步「版本」）：只在该版本核实过记录格式
// （SongRecord）、且版本号精确匹配时读；按距离借用偏移表、读不到版本号而借用最新版本的（fallbackTo 非空）时不读，
// 同 playStateAddr。
func (m *QQMusicMem) songRecordEnabled() bool { return m.offsets.SongRecord && m.fallbackTo == "" }

// readPlayState 读播放状态字段（#172）。地址为 0 时返回 (0, false)；ReadProcessMemory 失败或读到的不足 4 字节
// 时同样返回 (0, false)。不能沿用 ReadUint32：它失败时返回 0，而 0 是 20.05 的在播取值。
func (m *QQMusicMem) readPlayState() (uint32, bool) {
	addr := m.playStateAddr()
	if addr == 0 {
		return 0, false
	}
	var val uint32
	var n uintptr
	r, _, _ := procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&val)), 4, uintptr(unsafe.Pointer(&n)))
	if r == 0 || n != 4 {
		return 0, false
	}
	return val, true
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
		return i18n.Errorf("QQMusic_GFWrapper.dll not found")
	}
	// Pattern: 39 86 F0000000 74 ?? 8B CE 89 86 F0000000
	// New version: cmp [esi+F0],eax / je +XX / mov ecx,esi / mov [esi+F0],eax
	// We target the 'mov [esi+F0],eax' at offset +10 from pattern start
	patterns := []struct {
		pat         []byte
		mask        []byte // 0xFF = exact match, 0x00 = wildcard
		writeOff    int
		stolenBytes []byte // original bytes at the write instruction
		captureReg  byte   // register index for 'mov [ptr], reg' in codecave
	}{
		{
			// Current version: cmp [esi+F0],eax / je ?? / mov ecx,esi / mov [esi+F0],eax
			pat:         []byte{0x39, 0x86, 0xF0, 0x00, 0x00, 0x00, 0x74, 0x00, 0x8B, 0xCE, 0x89, 0x86, 0xF0, 0x00, 0x00, 0x00},
			mask:        []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			writeOff:    10,                                         // offset to 'mov [esi+F0],eax'
			stolenBytes: []byte{0x89, 0x86, 0xF0, 0x00, 0x00, 0x00}, // mov [esi+F0],eax
			captureReg:  0x35,                                       // 'mov [addr], esi' -> 89 35 (capture esi = this pointer)
		},
		{
			// Legacy version: cmp esi,eax / cmovle esi,eax / mov [edi+F0],esi / pop edi / pop esi
			pat:         []byte{0x3B, 0xC6, 0x0F, 0x4E, 0xF0, 0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00, 0x5F, 0x5E},
			mask:        nil,                                        // all exact
			writeOff:    5,                                          // offset to 'mov [edi+F0],esi'
			stolenBytes: []byte{0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00}, // mov [edi+F0],esi
			captureReg:  0x3D,                                       // 'mov [addr], edi' -> 89 3D (capture edi = this pointer)
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
		return i18n.Errorf("aob pattern not found in memory (might be already injected)")
	}

	log.Info("AOB slider target address: 0x%X (pattern %d)", targetAddr, matchedPattern)

	// Check if already hooked
	firstByte := m.ReadBytes(targetAddr, 1)
	if len(firstByte) > 0 && firstByte[0] == 0xE9 {
		log.Info("Slider hook already active")
		return nil
	}

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return i18n.Errorf("VirtualAllocEx failed")
	}

	m.sliderPointer = caveAddr + 0x100
	log.Detail("Codecave @ 0x%X, pointer @ 0x%X", caveAddr, m.sliderPointer)

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

	log.Info("Slider AOB hook injected successfully")
	return nil
}

// InjectProgressAOB hooks the progress write instruction (QQMusic.dll+488B75:
// mov [eax+1AC], esi) to also save ESI (progress ms) and GetTickCount() to
// fixed addresses. This enables precise local-clock interpolation.
//
// Codecave assembly:
//
//	mov [pProgressMs], esi     ; save progress value
//	pushad                      ; save all registers
//	call GetTickCount           ; EAX = wall-clock ms
//	mov [pTimeStamp], eax       ; save timestamp
//	popad                       ; restore registers
//	mov [eax+1AC], esi          ; original stolen bytes
//	jmp returnAddr
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
			log.Info("Progress hook already active")
			return nil
		}
		return i18n.Errorf("unexpected bytes at dll+%X: got %X, want %X", targetOffset, actual, expect)
	}

	// Resolve tick count: use KUSER_SHARED_DATA (fixed at 0x7FFE0000 in all Windows processes).
	// This avoids needing to resolve GetTickCount across 32/64 bit process boundaries.
	// TickCount.LowPart = [0x7FFE0320], TickCountMultiplier = [0x7FFE0004]
	// Result = (LowPart * Multiplier) >> 24 = milliseconds
	log.Detail("Using KUSER_SHARED_DATA to obtain tick count")

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return i18n.Errorf("VirtualAllocEx failed for progress hook")
	}

	// Data area at cave+0x100 (progressMs) and cave+0x104 (timestamp)
	m.progressPtr = caveAddr + 0x100
	m.progressTsPtr = caveAddr + 0x104
	log.Detail("Progress hook cave @ 0x%X, progressPtr @ 0x%X, tsPtr @ 0x%X",
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

	log.Info("Progress hook injected successfully")
	return nil
}

type SongMetadata struct {
	Name        string
	Singer      string
	SongID      uint32
	SongMid     string // QQ Music 字母数字 MID（v20.05 从 URL 参数解析，其他版本空）
	ProgressMs  uint32
	DurationMs  uint32
	SliderVal   uint32
	PlayState   uint32 // 播放状态字段的原始取值（#172），PlayStateOK 为假时无意义
	PlayStateOK bool   // 读了且读成功；未收录、借用偏移表或读失败时为假
}

// ssoFromBuf 从 24 字节的 MSVC 32-bit std::string 快照里解出字符串。
// 布局：+0x00 是内联缓冲区**或**堆指针，+0x10 是 length，+0x14 是 capacity；
// capacity > 15 = 堆分配，<= 15 = 内联。readHeap 用于把堆上的 n 字节读回来。
//
// 抽成纯函数是为了可测：调用方 extractSSO 是 ReadAllMetadata 里的闭包，
// 而这段判定是「一错就往 overlay 推乱码标题」的路径，必须有门禁。
//
// **堆/内联的判定必须独立于 length 的合法性检查，别把它们挤回同一个 `&&`。**
// 挤在一起时（原写法 `capacity > 15 && length > 0 && length < 1000`），
// 「已确诊是堆字符串、但 length 不合法」会掉进内联分支 —— 而那里 buf[0:4] 装的是
// **堆指针不是文本**，指针的原始字节被当歌名返回；sanitizeString 只按 rune 过滤
// （0xB1 解成 RuneError、>= 32 被保留），于是 overlay 闪一个乱码标题。
//
// 触发主力是 clear()：capacity 保留（仍 > 15）而 length 归 0 —— 实测原写法这条 96.9%
// 出乱码。版本偏移错位（未知版本借用已知版本的偏移，现见 nearestKnownVersion；当时是固定回退
// 22.16）也能进来，实测原写法 8.5%
// 出乱码、其余靠内联分支「找不到 NUL 就返回空」侥幸兜住。两条现在都收敛为返回空。
//
// 上界保持 1000：同文件的 ReadSSOString 用的是 2048，别顺手抄过来，那是放宽两倍上界。
func ssoFromBuf(buf []byte, readHeap func(ptr, n uint32) []byte) string {
	if len(buf) < 24 {
		return ""
	}
	length := binary.LittleEndian.Uint32(buf[0x10:0x14])
	capacity := binary.LittleEndian.Uint32(buf[0x14:0x18])

	if capacity > 15 {
		if length > 0 && length < 1000 {
			if ptr := binary.LittleEndian.Uint32(buf[0x00:0x04]); ptr > 0x10000 {
				return string(bytes.Trim(readHeap(ptr, length), "\x00"))
			}
		}
		// 堆模式下读不出来就到此为止，绝不 fallthrough 到内联分支
		return ""
	}

	// 内联分支（capacity <= 15）：逻辑原样保留。
	//
	// 注意 idx 算完即弃、真正用的是未 clamp 的 inlineIdx —— 看着像 bug，但**别顺手改**：
	// 它对短名是已验证正确的路径（21.81 条目的注释记着验证用例「无地自容」= 12 字节 →
	// cap==15 → 走这里），动它没有收益、只有回归风险。上面的堆分支拦住之后，进到这里的
	// 就只剩真·内联字符串了。
	idx := bytes.IndexByte(buf[:16], 0)
	if idx == -1 {
		idx = 16
	}
	inlineIdx := bytes.IndexByte(buf, 0)
	if inlineIdx > 0 {
		return string(buf[:inlineIdx])
	}
	return ""
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
						log.Detail("Extracted cookie: %s...", cookieStr[:min(50, len(cookieStr))])

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
//
//	"remainingTime" : 182846,
//	"songPlayTime" : 183000,    ← this matches meta.DurationMs
//	"songmid" : "000mDR751jtpPf",
//
// We match songPlayTime against the known duration to identify the correct block.
func (m *QQMusicMem) FindSongMid(durationMs uint32) string {
	if durationMs == 0 {
		return ""
	}

	// 播放上报 JSON 有两种编码（见 extractSongMid），两种都要能匹配。用两个 playTime
	// needle 做区域快速排除：命中任一才对该区细扫，其余区直接跳过。
	needles := [][]byte{
		[]byte(fmt.Sprintf(`"songPlayTime":%d`, durationMs)),
		[]byte(fmt.Sprintf(`"songPlayTime" : %d`, durationMs)),
	}

	var mbi MEMORY_BASIC_INFORMATION
	addr := uintptr(0)
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
				hit := false
				for _, n := range needles {
					if bytes.Contains(data, n) {
						hit = true
						break
					}
				}
				if hit {
					if mid := extractSongMid(data, durationMs); mid != "" {
						log.Detail("Found songmid '%s' (songPlayTime=%d)", mid, durationMs)
						return mid
					}
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

// extractSongMid 从一块内存里按时长定位当前歌的 songMid。QQ 播放上报 JSON 两种编码：
//
//	紧凑（v22.41）："songPlayTime":189623,"songid":0,"songmid":"000S48Kb1DJdkJ"
//	带空格（旧）  ："songPlayTime" : 189623 ... "songmid" : "000S48Kb1DJdkJ"
//
// 以 songPlayTime==durationMs 锚定，取邻近（±500B）的 songmid——避免把别的歌的 mid
// 安到当前歌上。抽成纯函数是为了可测：这是「一错就整首放错歌词」的路径。
// playTime 命中后校验其后一字节非数字，挡掉「137223 命中 1372230 前缀」的误匹配。
func extractSongMid(data []byte, durationMs uint32) string {
	formats := []struct{ playTime, mid string }{
		{fmt.Sprintf(`"songPlayTime":%d`, durationMs), `"songmid":"`},
		{fmt.Sprintf(`"songPlayTime" : %d`, durationMs), `"songmid" : "`},
	}
	for _, f := range formats {
		needle := []byte(f.playTime)
		pt := bytes.Index(data, needle)
		if pt < 0 {
			continue
		}
		if after := pt + len(needle); after < len(data) && data[after] >= '0' && data[after] <= '9' {
			continue // playTime 是更长数字的前缀（如 137223 命中 1372230），非精确匹配
		}
		lo := pt - 500
		if lo < 0 {
			lo = 0
		}
		hi := pt + 500
		if hi > len(data) {
			hi = len(data)
		}
		window := data[lo:hi]
		mi := bytes.Index(window, []byte(f.mid))
		if mi < 0 {
			continue
		}
		start := mi + len(f.mid)
		end := bytes.IndexByte(window[start:], '"')
		if end <= 0 {
			continue
		}
		mid := string(window[start : start+end])
		if len(mid) >= 10 && len(mid) <= 20 {
			return mid
		}
	}
	return ""
}

func (m *QQMusicMem) ReadAllMetadata() (*SongMetadata, error) {
	if m.readMetaFn != nil {
		return m.readMetaFn()
	}
	if m.hProcess == 0 || m.qqmusicDllBase == 0 {
		return nil, i18n.Errorf("process not attached")
	}

	meta := &SongMetadata{}

	// Read Struct 1 (C87C80)
	extractSSO := func(addr uintptr) string {
		buf := m.ReadBytes(addr, 24)
		if len(buf) < 24 {
			return ""
		}
		return ssoFromBuf(buf, func(ptr, n uint32) []byte {
			return m.ReadBytes(uintptr(ptr), n)
		})
	}

	var name1, singer1 string
	var songId, duration, progress uint32

	// CE verified: struct is directly embedded at QQMusic.dll+offsets.Struct1, NOT a pointer
	struct1 := m.qqmusicDllBase + m.offsets.Struct1

	if m.offsets.UseWideStrings {
		// v20.05+: 歌名/歌手字段为 WCHAR* 指针，指向堆上的 UTF-16 字符串
		namePtr := m.ReadUint32(struct1 + m.offsets.NameOff)
		if namePtr != 0 {
			name1 = m.ReadWideString(uintptr(namePtr), 256)
		}
		singerPtr := m.ReadUint32(struct1 + m.offsets.SingerOff)
		if singerPtr != 0 {
			singer1 = m.ReadWideString(uintptr(singerPtr), 256)
		}
	} else {
		name1 = extractSSO(struct1 + m.offsets.NameOff)
		singer1 = extractSSO(struct1 + m.offsets.SingerOff)
	}

	if len(name1) > 1 {
		meta.Name = name1
	}
	if len(singer1) > 1 {
		meta.Singer = singer1
	}

	// 用了未知版本的距离回退时，把第一次读到的歌名样本上报——这是判断「赌对没有」的唯一
	// 野外依据：读出正常歌名 = 偏移大致对上，读出乱码 = 赌错、该版本需要补偏移表。
	// ReportOnce 按 key 去重，只报第一首；key 不含版本号（高基数会把 issue 裂开），版本号
	// 与样本放 extra。空名不报——那可能只是当前没歌在放，不代表偏移错。
	if m.fallbackTo != "" && meta.Name != "" {
		telemetry.ReportOnce("qqmusic.unknown_version",
			"QQ 音乐版本无已知偏移表，已按 semver 距离回退（读到的可能是垃圾，附歌名样本供核对）",
			map[string]string{
				"qqmusic.version":     m.version,
				"qqmusic.fallback_to": m.fallbackTo,
			},
			map[string]any{"name_sample": meta.Name})
	}

	duration = m.ReadUint32(struct1 + m.offsets.DurationOff)
	if m.offsets.DurationInSeconds {
		duration *= 1000 // 秒转毫秒（v20.05 时长单位为秒）
	}

	songId = m.ReadUint32(struct1 + m.offsets.SongIDOff)
	if m.offsets.SongIDDurCheckOff != 0 {
		// v22.41：稳定 songID 在独立的「播放会话」结构里（见 knownVersions 注释），换歌瞬间
		// 可能短暂滞后于显示结构体。用它自带的时长与显示时长交叉核对：不一致=该 songID 尚未
		// 落位到当前歌 → 作废（songId=0），让上层回退到 songMid 堆扫描。绝不拿上一首的 songID
		// 去请求——那会整首推错歌词，比空白更糟。稳态下两处时长精确相等（CE 实测）。
		if m.ReadUint32(struct1+m.offsets.SongIDDurCheckOff) != duration {
			songId = 0
		}
	}

	// Fast progress timer: QQMusic.dll+offsets.FastTimerPtr -> [ptr]+offsets.FastTimerOff
	// Updates every ~1 second (vs slow timer which updates every ~3-5 seconds)
	if m.progressPtr != 0 {
		// AOB Hook 注入的精确进度计数器（优先）
		progress = m.ReadUint32(m.progressPtr)
	} else if m.offsets.FastTimerPtr != 0 {
		fastTimerPtr := uintptr(m.ReadUint32(m.qqmusicDllBase + m.offsets.FastTimerPtr))
		if fastTimerPtr > 0x10000 {
			progress = m.ReadUint32(fastTimerPtr + m.offsets.FastTimerOff)
		} else {
			progress = m.ReadUint32(struct1 + m.offsets.ProgressOff)
		}
	} else if m.offsets.ProgressDllOff != 0 {
		// v20.05: DLL 基址直接偏移，DWORD 即准确播放毫秒数
		progress = m.ReadUint32(m.qqmusicDllBase + m.offsets.ProgressDllOff)
	} else {
		// 回退：使用结构体内慢速进度字段
		progress = m.ReadUint32(struct1 + m.offsets.ProgressOff)
	}

	if songId > 0 && songId < 0x3F000000 {
		meta.SongID = songId
	}
	meta.DurationMs = duration
	meta.ProgressMs = progress
	stateRaw, stateOK := m.readPlayState()

	// v20.05 songMid 提取（两路，优先用流 URL）
	// 路径1: struct1+StreamURLOff → WCHAR* → "http://.../00281PXu4DHKNp.wma"
	//        取最后 '/' 之后、最后 '.' 之前的部分即 songMid
	if m.offsets.StreamURLOff != 0 && meta.SongMid == "" {
		urlPtr := m.ReadUint32(struct1 + m.offsets.StreamURLOff)
		if urlPtr > 0x10000 {
			urlStr := m.ReadWideString(uintptr(urlPtr), 256)
			if slash := strings.LastIndex(urlStr, "/"); slash >= 0 {
				filename := urlStr[slash+1:]
				if dot := strings.Index(filename, "."); dot > 0 {
					mid := filename[:dot]
					if len(mid) >= 10 && len(mid) <= 20 {
						meta.SongMid = mid
					}
				}
			}
		}
	}
	// 路径2（备选）: struct1+SongMidParamsOff → WCHAR* → "0=<songMid>&2=..."
	if m.offsets.SongMidParamsOff != 0 && meta.SongMid == "" {
		paramsPtr := m.ReadUint32(struct1 + m.offsets.SongMidParamsOff)
		if paramsPtr > 0x10000 {
			paramsStr := m.ReadWideString(uintptr(paramsPtr), 128)
			if idx := strings.Index(paramsStr, "0="); idx != -1 {
				rest := paramsStr[idx+2:]
				end := strings.IndexByte(rest, '&')
				if end == -1 {
					end = len(rest)
				}
				mid := rest[:end]
				if len(mid) > 0 {
					meta.SongMid = mid
				}
			}
		}
	}

	// Read Struct 2 (backup source, not available in all versions)
	var name2, singer2 string
	if m.offsets.Struct2Ptr != 0 {
		struct2 := uintptr(m.ReadUint32(m.qqmusicDllBase + m.offsets.Struct2Ptr))
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
	}

	name := name2
	if len(name) < 2 {
		name = name1
	}
	singer := singer2
	if len(singer) < 2 {
		singer = singer1
	}

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
		Name:        name,
		Singer:      singer,
		SongID:      songId,
		SongMid:     meta.SongMid,
		ProgressMs:  progress,
		DurationMs:  duration,
		SliderVal:   sliderVal,
		PlayState:   stateRaw,
		PlayStateOK: stateOK,
	}, nil
}
