package watchdog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
	"unsafe"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/restartmgr"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ErrConfirmNotReady 表示酷狗还没到可以确认的状态（主进程未起或不止一个、CEF 子进程未起、库未被打开、
// 查询失败等），稍后可重试。
var ErrConfirmNotReady = i18n.Sentinel("KuGou is not ready for reading its lyric database")

// ErrNotRunning 表示进程快照中没有任何酷狗进程（KuGou.exe 或 HD_KuGou.exe）在运行。
var ErrNotRunning = i18n.Sentinel("KuGou is not running")

// ErrConfirmUnsupported 表示这个酷狗不支持读库（版本读不出或在黑名单、实际加载的 libcef.dll 缺失、
// 不认识或未全部打上补丁），本会话不再尝试。
var ErrConfirmUnsupported = i18n.Sentinel("reading the KuGou lyric database is not supported for this KuGou")

// Confirmed 是确认通过时运行中的酷狗的情况。
type Confirmed struct {
	PID        uint32 // 主进程
	ExePath    string // 主进程的映像路径
	Version    string // 主进程映像的 FileVersion
	LibcefPath string // CEF 子进程目录下的 libcef.dll，9 处指纹全部是已打补丁的字节
	DataDir    string // KGMusicV3.db 正被主进程打开的数据目录
	LyricDir   string // 歌词目录（规范化的绝对路径），不保证存在
}

// maxINIBytes 是 KuGou.ini 的读取上限；超过即按读不出处理。
const maxINIBytes = 1 << 20

// Confirm 在读酷狗的数据库之前，以运行中的酷狗进程为准只读地确认（设计 §6.2）：主进程可定位、版本读得出
// 且不在黑名单、CEF 子进程目录下的 libcef.dll 已全部打上补丁、数据目录的 KGMusicV3.db 正被主进程打开，
// 并读出歌词目录。不退回注册表 KuGou8 或按版本推导 libcef.dll，数据目录也不退而取最新修改的那份。
//
// 错误包装 ErrConfirmNotReady（可重试）或 ErrConfirmUnsupported（本会话不再尝试）之一。同步执行，
// 耗时数百毫秒（Restart Manager），不得在轮询线程上调用；自身不打日志，只有指纹不认识时 CheckPatchStatus
// 打一条 Warn。
func Confirm() (Confirmed, error) { return confirmWith(realConfirmDeps) }

// confirmDeps 是确认用到的全部系统访问，测试以桩替换。
type confirmDeps struct {
	snapshot    func() ([]proc, error)
	imagePath   func(uint32) (string, error)
	readVersion func(string) string
	checkPatch  func(string) (bool, bool, error)
	holders     func(string) ([]uint32, error)
	regAppData  func() string // 注册表 HKCU\Software\KuGou 的 AppDataPath；读不出为空
	appData     func() string // %APPDATA%
	readFile    func(string) ([]byte, error)
	fileExists  func(string) bool // 存在且是普通文件
}

var realConfirmDeps = confirmDeps{
	snapshot:    snapshotProcs,
	imagePath:   imagePath,
	readVersion: readExeVersion,
	checkPatch:  CheckPatchStatus,
	holders:     restartmgr.HoldersOf,
	regAppData:  regAppDataPath,
	appData:     func() string { return os.Getenv("APPDATA") },
	readFile:    readShared,
	fileExists: func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular()
	},
}

func confirmWith(d confirmDeps) (Confirmed, error) {
	procs, err := d.snapshot()
	if err != nil {
		return Confirmed{}, i18n.Errorf("%w: take a process snapshot: %w", ErrConfirmNotReady, err)
	}
	k, err := findRunningKuGou(procs, d.imagePath)
	if err != nil {
		return Confirmed{}, err
	}
	c := Confirmed{PID: k.PID, ExePath: k.ExePath, Version: d.readVersion(k.ExePath)}
	if c.Version == "" {
		return Confirmed{}, i18n.Errorf("%w: cannot read the version of %s", ErrConfirmUnsupported, k.ExePath)
	}
	if isUnsupportedMajor(c.Version) {
		return Confirmed{}, i18n.Errorf("%w: KuGou version %s is not supported", ErrConfirmUnsupported, c.Version)
	}

	// 只认 CEF 子进程映像所在目录下的 libcef.dll：注册表 KuGou8 或按版本推导出的只是磁盘上的一份文件，
	// 不能证明是当前进程加载的。
	switch len(k.ChildDirs) {
	case 0:
		return Confirmed{}, i18n.Errorf("%w: KuGou.exe (PID %d) has no CEF child process yet", ErrConfirmNotReady, k.PID)
	case 1:
	default:
		return Confirmed{}, i18n.Errorf("%w: the CEF child processes of KuGou.exe run from different directories: %s", ErrConfirmUnsupported, strings.Join(k.ChildDirs, "; "))
	}
	c.LibcefPath = filepath.Join(k.ChildDirs[0], "libcef.dll")
	if !d.fileExists(c.LibcefPath) {
		return Confirmed{}, i18n.Errorf("%w: %s does not exist", ErrConfirmUnsupported, c.LibcefPath)
	}
	allPatched, canAutoFix, err := d.checkPatch(c.LibcefPath)
	switch {
	case err != nil:
		return Confirmed{}, i18n.Errorf("%w: check the patch status of %s: %w", ErrConfirmNotReady, c.LibcefPath, err)
	case !canAutoFix:
		return Confirmed{}, i18n.Errorf("%w: byte fingerprint of %s not recognized", ErrConfirmUnsupported, c.LibcefPath)
	case !allPatched:
		return Confirmed{}, i18n.Errorf("%w: %s is not fully patched", ErrConfirmUnsupported, c.LibcefPath)
	}

	if c.DataDir, err = heldDataDir(d, k.PID); err != nil {
		return Confirmed{}, err
	}
	c.LyricDir = lyricDir(d, c.DataDir, filepath.Dir(k.ExePath))
	return c, nil
}

// heldDataDir 在注册表 AppDataPath 与 %APPDATA%\KuGou8 两个候选（须是绝对路径、含 KGMusicV3.db，
// 大小写不敏感去重）里，选出 KGMusicV3.db 正被 pid 打开的那一个。没有候选、任一候选查询失败、
// 没有或不止一个被 pid 打开：未就绪。
func heldDataDir(d confirmDeps, pid uint32) (string, error) {
	var cands []string
	for _, dir := range []string{d.regAppData(), filepath.Join(d.appData(), "KuGou8")} {
		// 不是绝对路径的（空值拼出的 KuGou8、未展开的 %APPDATA%）会按本程序的当前目录解析，不用。
		if !filepath.IsAbs(dir) {
			continue
		}
		dir = filepath.Clean(dir)
		if slices.ContainsFunc(cands, func(c string) bool { return strings.EqualFold(c, dir) }) {
			continue
		}
		if d.fileExists(filepath.Join(dir, "KGMusicV3.db")) {
			cands = append(cands, dir)
		}
	}
	if len(cands) == 0 {
		return "", i18n.Errorf("%w: KGMusicV3.db not found in any candidate data directory", ErrConfirmNotReady)
	}
	var held []string
	for _, dir := range cands {
		db := filepath.Join(dir, "KGMusicV3.db")
		pids, err := d.holders(db)
		if err != nil {
			return "", i18n.Errorf("%w: query the processes holding %s: %w", ErrConfirmNotReady, db, err)
		}
		if slices.Contains(pids, pid) {
			held = append(held, dir)
		}
	}
	switch len(held) {
	case 0:
		return "", i18n.Errorf("%w: KGMusicV3.db is not held open by KuGou.exe (PID %d)", ErrConfirmNotReady, pid)
	case 1:
		return held[0], nil
	default:
		return "", i18n.Errorf("%w: KuGou.exe (PID %d) holds KGMusicV3.db in more than one directory: %s", ErrConfirmNotReady, pid, strings.Join(held, "; "))
	}
}

// lyricDir 取数据目录下 KuGou.ini 的 [LyricConfigSection] LyricPath；ini 读不出、缺项或不是绝对路径时退回
// 「安装目录\Lyric」。返回规范化后的路径，不判断目录是否存在：读取者整个会话都用确认时的这份结果，
// 目录可能之后才由酷狗建出，其下的文件不存在由读取者按暂缺重试（设计 §6.5 第 3 步）。
func lyricDir(d confirmDeps, dataDir, installDir string) string {
	dir := ""
	if b, err := d.readFile(filepath.Join(dataDir, "KuGou.ini")); err == nil {
		dir = lyricDirFromINI(b)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(installDir, "Lyric")
	}
	return filepath.Clean(dir)
}

// lyricDirFromINI 从 KuGou.ini 的内容取 [LyricConfigSection] 节内 LyricPath 的值（节名与键名大小写不敏感，
// 其他节的同名键不算）。FF FE 开头按 UTF-16LE，其余按 UTF-8（去掉 BOM）。没有该键或值为空返回空串。
func lyricDirFromINI(b []byte) string {
	var text string
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		u := make([]uint16, (len(b)-2)/2) // 奇数长度时丢弃末尾的残字节
		for i := range u {
			u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
		}
		text = string(utf16.Decode(u))
	} else {
		text = string(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}))
	}
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "LyricConfigSection")
			continue
		}
		if key, val, ok := strings.Cut(line, "="); inSection && ok && strings.EqualFold(strings.TrimSpace(key), "LyricPath") {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// openShared 只读打开 path，共享模式为读写删全共享、不加锁，句柄存续期间不妨碍酷狗读写、改名或删除该文件
// （os.Open 的共享模式不含 FILE_SHARE_DELETE，酷狗更新器替换文件时会共享冲突）。
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// readShared 经 openShared 读入 path 的全部内容（至多 maxINIBytes 字节）。
func readShared(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxINIBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxINIBytes {
		return nil, i18n.Errorf("%s is larger than %d bytes", path, maxINIBytes)
	}
	return b, nil
}

// regAppDataPath 读注册表 HKCU\Software\KuGou 的 AppDataPath（只读）；读不出返回空。
// 注册表可能是旧数据，只作数据目录的候选之一，须经 Restart Manager 核实。
func regAppDataPath() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\KuGou`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	v, _, err := key.GetStringValue("AppDataPath")
	if err != nil {
		return ""
	}
	return v
}

// runningKuGou 是运行中的酷狗：主进程，及其 CEF 子进程映像所在的各目录（大小写不敏感去重，保留先见到的写法）。
type runningKuGou struct {
	PID       uint32
	ExePath   string
	ChildDirs []string
}

// findRunningKuGou 在进程快照里定位酷狗主进程：映像名精确命中双名白名单（KuGou.exe 或 HD_KuGou.exe，
// 大小写不敏感）、父进程不在同进程族中者。没有任何酷狗进程时返回同时包装 ErrConfirmNotReady 与
// ErrNotRunning 的错误；有酷狗进程但主进程数为 0 或不止一个、或外层启动器正在交接给 HD_KuGou.exe（直接
// 子进程为 HD_KuGou.exe）时返回仅包装 ErrConfirmNotReady 的错误，不取 PID 最小者。父 PID 为主进程的
// 同进程族成员即 CEF 子进程；任一进程取不到映像路径或映像名不再属于白名单（PID 被复用）同样返回未就绪。
// 没有子进程不算错误，ChildDirs 为空。
func findRunningKuGou(procs []proc, imagePath func(pid uint32) (string, error)) (runningKuGou, error) {
	same := make(map[uint32]bool)
	for _, p := range procs {
		if isKuGouProcessName(p.Name) {
			same[p.PID] = true
		}
	}
	if len(same) == 0 {
		return runningKuGou{}, i18n.Errorf("%w: %w", ErrConfirmNotReady, ErrNotRunning)
	}
	var mains []uint32
	for _, p := range procs {
		if same[p.PID] && !same[p.PPID] {
			mains = append(mains, p.PID)
		}
	}
	switch len(mains) {
	case 0:
		return runningKuGou{}, i18n.Errorf("%w: KuGou.exe main process not found", ErrConfirmNotReady)
	case 1:
	default:
		return runningKuGou{}, i18n.Errorf("%w: %d KuGou.exe main processes are running", ErrConfirmNotReady, len(mains))
	}

	r := runningKuGou{PID: mains[0]}
	var err error
	if r.ExePath, err = kugouImage(imagePath, r.PID); err != nil {
		return runningKuGou{}, err
	}
	for _, p := range procs {
		if !same[p.PID] || p.PPID != r.PID {
			continue
		}
		exe, err := kugouImage(imagePath, p.PID)
		if err != nil {
			return runningKuGou{}, err
		}
		if strings.EqualFold(filepath.Base(exe), HDTargetProcessName) {
			return runningKuGou{}, i18n.Errorf("%w: KuGou launcher is handing off to HD_KuGou.exe", ErrConfirmNotReady)
		}
		dir := filepath.Dir(exe)
		if !slices.ContainsFunc(r.ChildDirs, func(c string) bool { return strings.EqualFold(c, dir) }) {
			r.ChildDirs = append(r.ChildDirs, dir)
		}
	}
	return r, nil
}

// kugouImage 取 pid 的映像路径，并核对映像名仍在酷狗双进程名白名单内。
func kugouImage(imagePath func(uint32) (string, error), pid uint32) (string, error) {
	exe, err := imagePath(pid)
	if err != nil {
		return "", i18n.Errorf("%w: query the image path of KuGou.exe (PID %d): %w", ErrConfirmNotReady, pid, err)
	}
	if !isKuGouProcessName(filepath.Base(exe)) {
		return "", i18n.Errorf("%w: PID %d is no longer KuGou.exe (image %s)", ErrConfirmNotReady, pid, exe)
	}
	return exe, nil
}

// imagePath 以 PROCESS_QUERY_LIMITED_INFORMATION 打开 pid，取其映像的完整路径。对目标进程只有这一种访问。
func imagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// proc 是进程快照里的一条记录。
type proc struct {
	PID, PPID uint32
	Name      string
}

// snapshotProcs 以 Toolhelp 快照枚举全部进程。写法同 audiocap/procscan 的 Snapshot；不导入该包：
// 它的 names_windows.go 导入了 player/kugou 与本包，会成环。枚举中途出错时返回错误而不是半份列表，
// 免得漏掉第二个主进程。
func snapshotProcs() ([]proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var procs []proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		procs = append(procs, proc{PID: e.ProcessID, PPID: e.ParentProcessID, Name: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return procs, nil
}
