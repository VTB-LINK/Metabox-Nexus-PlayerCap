package watchdog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/telemetry"

	"Metabox-Nexus-PlayerCap/i18n"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var log = logger.New("KuGou] [Watchdog") // 渲染为 [KuGou] [Watchdog]

// ErrInstallNotFound 表示未找到酷狗安装，属于不可恢复错误，不应重试。
var ErrInstallNotFound = i18n.Sentinel("KuGou installation not found (KuGou.exe or libcef.dll is missing)")

// ErrVersionUnsupported 表示 libcef.dll 在已知偏移处的字节指纹既非原版也非已 patch，
// 版本不认识，拒绝盲打补丁。与 ErrInstallNotFound 同属终态错误，不应重试（重试也只会
// 反复弹 UAC）。上层据此停止而非空转，见 kugou.go 的分流。
var ErrVersionUnsupported = i18n.Sentinel("libcef.dll version unsupported (byte fingerprint at the known offsets does not match)")

// ErrElevationFailed 表示未取得管理员权限（用户拒绝了 UAC，或 helper 未按时就绪）。
//
// **上层必须退避并设上限**：本函数在 launchElevatedHelper 处弹 UAC，被拒即返回；调用方
// 若立刻重试，UAC 安全桌面就会全屏夺焦——直播时是事故。见 kugou.go 的 elevationGate。
var ErrElevationFailed = i18n.Sentinel("Administrator rights not obtained")

const (
	TargetProcessName = "KuGou.exe"
	cdpPort           = 12233

	// knownVersion is the KuGou.exe FileVersion the patches were reverse-engineered against.
	// Patches may work on other versions but are not guaranteed.
	knownVersion = "20.1.22.27795"
)

// patchEntry describes one patch at a disk offset in libcef.dll.
//
// orig 与 data 均为该偏移处的完整字节序列，长度必须相等（TestPatchTableConsistency
// 强制）。orig 是 20.1.22.27795 / CEF 89.20.0 原版实测出的原始指令字节，data 是打完
// 补丁后的字节。二者共同构成版本指纹：CheckPatchStatus 要求 9 处全部落在 {orig,data}
// 之内才认为「这个 DLL 我认识」。这是刻意用「字节」而非「版本号」当钥匙——libcef.dll
// 是酷狗 vendored 的 Chromium，其偏移随 CEF 基线漂移，与 KuGou.exe 的版本号相互独立。
type patchEntry struct {
	offset int64
	orig   []byte
	data   []byte
}

// libcefPatches holds all 9 patches for libcef.dll (CEF 89.20.0, shipped with KuGou
// 20.1.22.27795). Applies in-order; idempotent (re-applying already-patched bytes is
// harmless). orig 字节实测自覆盖安装的原版 DLL，opcode / 语义 / 补丁长度三重印证：
// P1/P5/P7 是写端口立即数（原始立即数 0xAAAAAAAA 是 CEF 的「端口未设」哨兵，出现两次），
// P2/P3/P6 NOP 掉条件跳转（0F 87 ja / 0F 84 jz），P4/P8 NOP 掉 CALL（E8 rel32），
// P9 NOP 掉 cmovz（0F 44）。
var libcefPatches = []patchEntry{
	{0x58C63EB, []byte{0xC7, 0x06, 0xAA, 0xAA, 0xAA, 0xAA}, []byte{0xC7, 0x06, 0xC9, 0x2F, 0x00, 0x00}},             // Patch1: force port=12233 init
	{0x58C6415, []byte{0xE8, 0x96, 0xA9, 0xE9, 0xFC}, []byte{0x90, 0x90, 0x90, 0x90, 0x90}},                         // Patch4: NOP CALL sub_1827619B0
	{0x58C6420, []byte{0x0F, 0x87, 0x96, 0x01, 0x00, 0x00}, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},             // Patch2: NOP ja (port range check)
	{0x58C6428, []byte{0x0F, 0x84, 0x8E, 0x01, 0x00, 0x00}, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},             // Patch3: NOP jz (parse success check)
	{0x4BC180E, []byte{0x8B, 0x95, 0x7C, 0x01, 0x00, 0x00}, []byte{0xBA, 0xC9, 0x2F, 0x00, 0x00, 0x90}},             // Patch5: child cmdline port=12233
	{0x4BEDE41, []byte{0x0F, 0x84, 0x70, 0x01, 0x00, 0x00}, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},             // Patch6: NOP jz (DevTools startup skip)
	{0x4BEDE4C, []byte{0x41, 0xC7, 0x07, 0xAA, 0xAA, 0xAA, 0xAA}, []byte{0x41, 0xC7, 0x07, 0xC9, 0x2F, 0x00, 0x00}}, // Patch7: force port=12233 in DevTools fn
	{0x4BEDEB5, []byte{0xE8, 0xF6, 0x2E, 0xB7, 0xFD}, []byte{0x90, 0x90, 0x90, 0x90, 0x90}},                         // Patch8: NOP parse CALL
	{0x4BEDED0, []byte{0x0F, 0x44, 0xDA}, []byte{0x90, 0x90, 0x90}},                                                 // Patch9: NOP cmovz
}

// isUnsupportedMajor 判断 KuGou.exe 版本是否属于已知不兼容的大版本黑名单。
// 这是 orig 字节指纹之外的一道确定性负向防线，见 EnsurePatched 处的说明。
func isUnsupportedMajor(ver string) bool {
	return strings.HasPrefix(ver, "10.")
}

// isCDPAvailable checks if KuGou's CDP port is responsive.
func isCDPAvailable() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json", cdpPort))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// CheckPatchStatus reads libcef.dll at all 9 patch offsets and classifies the file.
//
// 返回 (allPatched, canAutoFix, err) 的三态语义：
//   - 每个偏移处的字节要么等于 p.data（已打）要么等于 p.orig（待打）；
//   - allPatched：9 处全部等于 data —— 已完整 patch，无需再动。
//   - canAutoFix：9 处全部落在 {orig, data} 内 —— 这个 DLL 的指纹我们认识，
//     可以安全地（幂等地）打补丁；含「部分已打」的混合态也算可打。
//   - 任一偏移处两者都不是 → canAutoFix=false：版本不认识，拒绝盲打。
//   - 任一偏移读失败（含短于偏移的 DLL）→ 返回 err：宁可报错也不盲打。
//
// 与旧实现的关键区别：旧版只用 Patch1 单点 6 字节做版本指纹，而 Patch1 的 data 恰好
// 等于它自己的哨兵位点，于是「打错版本的损坏 DLL」在第一次盲写后就把哨兵改成「已确认」，
// 与健康态不可区分。现在 9 处 orig 联合校验，且 orig≠data，任何一处漂移都会被抓住——
// 尤其 P5–P9 所在的、距 Patch1 有 12.8MB 的另一个代码簇，旧版对它零证据。
func CheckPatchStatus(libcefPath string) (allPatched bool, canAutoFix bool, err error) {
	f, err := os.Open(libcefPath)
	if err != nil {
		return false, false, i18n.Errorf("open libcef.dll: %w", err)
	}
	defer f.Close()
	return classifyLibcef(f)
}

// classifyLibcef 是 CheckPatchStatus 的纯判定核心，只依赖 io.ReaderAt，便于用合成
// 字节流覆盖三态（真实 DLL 有 ~139MB，不适合进单元测试）。*os.File 满足 io.ReaderAt，
// 其 ReadAt 在读不满时必返回 error（io.ReaderAt 契约），故无需 io.ReadFull；ReadAt
// 也不移动文件偏移，与并发/复用同一路径的读写互不干扰。
func classifyLibcef(r io.ReaderAt) (allPatched bool, canAutoFix bool, err error) {
	allPatched = true // 直到发现某处不等于 data
	for i, p := range libcefPatches {
		buf := make([]byte, len(p.data)) // len(orig)==len(data)，见 TestPatchTableConsistency
		if _, err := r.ReadAt(buf, p.offset); err != nil {
			return false, false, i18n.Errorf("read Patch%d (offset=0x%X): %w", i+1, p.offset, err)
		}
		isData := bytes.Equal(buf, p.data)
		isOrig := bytes.Equal(buf, p.orig)
		if !isData {
			allPatched = false
		}
		if !isData && !isOrig {
			// 该偏移既非已打补丁的字节，也非我们认识的原始指令：版本指纹不匹配
			log.Warn("Patch%d (offset=0x%X) bytes %X are neither original nor patched; libcef.dll version unsupported", i+1, p.offset, buf)
			return false, false, nil
		}
	}
	return allPatched, true, nil
}

// FindKuGouInstall locates the KuGou executable and the libcef.dll it actually loads.
//
// libcef.dll 必须是 KuGou.exe 实际加载的那一份。酷狗把不同版本的 CEF 放在以版本号命名
// 的子目录里，升级后旧目录常残留，选错版本就会拿不匹配的偏移把 DLL 写坏。
//
// 选址两级：
//  1. 首选注册表 KuGou8 —— 酷狗自己维护、指向当前版本目录的权威指针，是「选中实际加载
//     的那份」最直接的信号。但它会变（实测有的机器/时刻指向 KuGou.exe 而非目录），故用
//     IsDir + 有 libcef.dll 守卫（kugou8LibcefIfDir），指文件即视为不可用。
//  2. 拿不到才按 KuGou.exe 的 FileVersion 推导版本目录（findLibcefForExe）。
//
// 早先的 951a361 直接删掉了 KuGou8、只留 exe 版本推导，是过度修复：当时确实读到 KuGou8
// 指向 exe（真实观测），但正确动作是守卫而非删除——KuGou8 大多数时候（含本机现在）就是
// 正确答案，删掉它把选址压在「exe 版本号 == CEF 目录名」这个未必成立的假设上。
func FindKuGouInstall() (exePath, libcefPath string, err error) {
	exePath = findKuGouExe()
	if exePath == "" {
		return "", "", ErrInstallNotFound
	}
	if lc := libcefFromKuGou8(); lc != "" {
		libcefPath = lc
	} else {
		libcefPath = findLibcefForExe(filepath.Dir(exePath), readExeVersion(exePath))
	}
	if libcefPath == "" {
		return "", "", ErrInstallNotFound
	}
	return exePath, libcefPath, nil
}

// libcefFromKuGou8 读注册表 HKCU\Software\KuGou\KuGou8 并经 kugou8LibcefIfDir 守卫，
// 返回其下的 libcef.dll 路径；不可用（键缺失/指文件/无 libcef.dll）时返回空。
func libcefFromKuGou8() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\KuGou`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	dir, _, err := key.GetStringValue("KuGou8")
	if err != nil {
		return ""
	}
	return kugou8LibcefIfDir(dir)
}

// kugou8LibcefIfDir 是 KuGou8 的守卫（抽出以便不碰注册表做单测）：dir 必须是一个存在的
// 目录、且其下有 libcef.dll，才返回该 libcef.dll 路径。KuGou8 已知会被改写成 KuGou.exe
// （文件），此时返回空，让调用方回落 exe 版本推导。
func kugou8LibcefIfDir(dir string) string {
	if dir == "" {
		return ""
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return ""
	}
	lc := filepath.Join(dir, "libcef.dll")
	if _, err := os.Stat(lc); err != nil {
		return ""
	}
	return lc
}

// findKuGouExe 定位 KuGou.exe：注册表 AppPath 优先，否则扫描常见安装根。
func findKuGouExe() string {
	if key, kerr := registry.OpenKey(registry.CURRENT_USER, `Software\KuGou`, registry.QUERY_VALUE); kerr == nil {
		defer key.Close()
		if appPath, _, e := key.GetStringValue("AppPath"); e == nil && appPath != "" {
			exe := filepath.Join(appPath, "KuGou.exe")
			if _, se := os.Stat(exe); se == nil {
				return exe
			}
		}
	}
	for _, base := range []string{
		`C:\Program Files\KuGou\KGMusic`,
		`C:\Program Files (x86)\KuGou\KGMusic`,
	} {
		exe := filepath.Join(base, "KuGou.exe")
		if _, e := os.Stat(exe); e == nil {
			return exe
		}
	}
	return ""
}

// findLibcefForExe 在 base 下选出 KuGou.exe（版本 ver）实际加载的 libcef.dll。
//
// 首选与 exe 版本同名的目录——这是最可靠的信号。找不到时才扫描所有含 libcef.dll 的
// 版本目录并取版本号最大者：这是异常兜底（ver 读取失败、或目录名与 exe 版本不一致），
// 取最大而非 os.ReadDir 的字典序第一，是为了避免选中升级残留的旧版本目录。
// 注：字典序对酷狗 a.b.c.d 版本号仅在同位数下与数值序一致，故仅作兜底，不作首选。
func findLibcefForExe(base, ver string) string {
	if ver != "" {
		lc := filepath.Join(base, ver, "libcef.dll")
		if _, err := os.Stat(lc); err == nil {
			return lc
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	var best, bestName string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		lc := filepath.Join(base, name, "libcef.dll")
		if _, err := os.Stat(lc); err != nil {
			continue
		}
		if name == ver {
			return lc // 精确匹配 exe 版本
		}
		if bestName == "" || name > bestName {
			best, bestName = lc, name
		}
	}
	if best != "" && ver != "" {
		log.Warn("No libcef.dll directory matching KuGou.exe version %q; falling back to the highest version %q", ver, bestName)
	}
	return best
}

// killKuGou terminates all running KuGou.exe processes and waits for them to exit.
func killKuGou() {
	log.Info("Terminating KuGou process...")
	exec.Command("taskkill", "/F", "/IM", TargetProcessName).Run() //nolint
	// Poll until all instances exit (max 5s)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if !IsKuGouRunning() {
			return
		}
	}
	log.Warn("KuGou process did not fully exit within 5s; continuing...")
}

// IsKuGouRunning returns true if any KuGou.exe process is alive.
func IsKuGouRunning() bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+TargetProcessName, "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), TargetProcessName)
}

// launchKuGou 以 KuGou.exe 所在目录为工作目录、经 launch.Detached 拉起酷狗，不直接 Start：直接启动的酷狗
// 是 PlayerCap 的子进程，按进程树结束 PlayerCap 的工具（OBS 的 Autostarter 插件）会把它一并结束（issue #127）。
// 经 cmd.exe 中转时拿不到酷狗的 PID，日志里不再打。
func launchKuGou(exePath string) error {
	detached, err := launch.Detached(exePath, nil, filepath.Dir(exePath))
	if err != nil {
		return i18n.Errorf("start KuGou.exe: %w", err)
	}
	if !detached {
		log.Warn("%s was started directly (its path, arguments or working directory cannot be passed through cmd.exe unchanged); it stays in PlayerCap's process tree, so tools that end PlayerCap together with its child processes (such as OBS autostart plugins) will close it as well", TargetProcessName)
	}
	log.Info("KuGou started; waiting for initialization...")
	return nil
}

// shellExecuteInfoW mirrors the SHELLEXECUTEINFOW struct for x64 Windows.
// Field offsets match the SDK layout: cbSize(0) fMask(4) hwnd(8) lpVerb(16)
// lpFile(24) lpParameters(32) lpDirectory(40) nShow(48) [pad4] hInstApp(56)
// lpIDList(64) lpClass(72) hkeyClass(80) dwHotKey(88) [pad4] hIconOrMonitor(96)
// hProcess(104)  total=112 bytes.
type shellExecuteInfoW struct {
	cbSize         uint32
	fMask          uint32
	hwnd           windows.HWND
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       windows.Handle
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      windows.Handle
	dwHotKey       uint32
	hIconOrMonitor windows.Handle
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess uint32 = 0x00000040
	seeMaskNoAsync        uint32 = 0x00000100
)

var (
	procShellExecuteExW         = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	procGetFileVersionInfoSizeW = windows.NewLazySystemDLL("version.dll").NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = windows.NewLazySystemDLL("version.dll").NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = windows.NewLazySystemDLL("version.dll").NewProc("VerQueryValueW")
)

// vsFixedFileInfo mirrors VS_FIXEDFILEINFO (only the fields we need).
type vsFixedFileInfo struct {
	Signature     uint32
	StrucVersion  uint32
	FileVersionMS uint32 // HIWORD=major, LOWORD=minor
	FileVersionLS uint32 // HIWORD=build, LOWORD=revision
	// remaining fields unused
}

// readExeVersion returns the FileVersion string (e.g. "20.1.22.27795") from a
// Windows PE executable using the VerQueryValue API.  Returns "" on any error.
func readExeVersion(exePath string) string {
	pathW, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return ""
	}
	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathW)), 0)
	if size == 0 {
		return ""
	}
	buf := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), 0,
		uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return ""
	}
	subBlock, _ := windows.UTF16PtrFromString(`\`)
	var pInfo uintptr
	var infoLen uint32
	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&pInfo)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if ret == 0 || pInfo == 0 {
		return ""
	}
	// pInfo 指向 buf 内部，但它是 uintptr——不持有对 buf 的引用。直接
	// (*vsFixedFileInfo)(unsafe.Pointer(pInfo)) 会让 GC 有机会在下面解引用之前回收 buf。
	// 改为算偏移再回 buf 取地址：buf 因此保持存活，同时顺带做了越界校验。
	base := uintptr(unsafe.Pointer(&buf[0]))
	if pInfo < base {
		return ""
	}
	off := pInfo - base
	if off+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(buf)) {
		return ""
	}
	fi := (*vsFixedFileInfo)(unsafe.Pointer(&buf[off]))
	// Signature 校验：VS_FIXEDFILEINFO 的第一个字段恒为 0xFEEF04BD。
	// 不校验的话，一旦偏移算错就会把任意 4 组字节 Sprintf 成一个**格式合法的垃圾
	// 版本号**——上层无从分辨，而这正是最危险的失败模式（空串至少是个明确信号）。
	// qqmusic/mem.go 的同一份复制粘贴代码本来就有这个校验，这里漏了。
	if fi.Signature != 0xFEEF04BD {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		fi.FileVersionMS>>16, fi.FileVersionMS&0xFFFF,
		fi.FileVersionLS>>16, fi.FileVersionLS&0xFFFF,
	)
}

func writePatchResult(path, content string) {
	_ = os.WriteFile(path, []byte(content), 0600)
}

// patchWithBackup 在提权进程内安全地打补丁：落笔前复检版本指纹、备份原文件、写补丁、
// 写后自校验，任何一步失败都从备份回滚。这是 patchDLLBytes 唯一的调用者——裸写原语
// 不再从任何入口直接暴露。
func patchWithBackup(libcefPath string) error {
	// 落笔前复检：主进程虽已 CheckPatchStatus，但 check 与 write 之间隔着 UAC 弹窗 +
	// 等用户启动酷狗（无上界）+ killKuGou，这段时间 DLL 可能被酷狗自动更新器换掉。
	// 用最新字节判定，不认识就拒绝——把跨进程 TOCTOU 窗口收敛到紧贴写盘处。
	allPatched, canAutoFix, err := CheckPatchStatus(libcefPath)
	if err != nil {
		return i18n.Errorf("pre-write recheck failed: %w", err)
	}
	if !canAutoFix {
		return i18n.Errorf("pre-write recheck: libcef.dll version fingerprint mismatch; refusing to patch")
	}
	if allPatched {
		return nil // 已是 patched 态，无需再写
	}

	bak := libcefPath + ".playercap.bak"
	if err := copyFile(libcefPath, bak); err != nil {
		return i18n.Errorf("back up libcef.dll failed: %w", err)
	}

	if err := patchDLLBytes(libcefPath); err != nil {
		rollback(bak, libcefPath)
		return i18n.Errorf("patch write failed (rollback attempted): %w", err)
	}

	// 写后自校验：9 处必须全部变为 data。校验的是刚写的字节，只能抓「写没落盘/被回滚」，
	// 版本正确性由上面的复检与 orig 指纹保证。
	verified, _, verr := CheckPatchStatus(libcefPath)
	if verr != nil {
		rollback(bak, libcefPath)
		return i18n.Errorf("post-patch verification read failed (rollback attempted): %w", verr)
	}
	if !verified {
		rollback(bak, libcefPath)
		return i18n.Errorf("post-patch verification failed: bytes did not change as expected (rolled back; possibly blocked by antivirus)")
	}

	os.Remove(bak) // 成功，删除备份
	return nil
}

// rollback 尽力从备份恢复原文件。恢复成功后删备份；恢复失败则保留备份并在日志留痕，
// 供人工恢复——绝不静默丢弃。
func rollback(bak, dst string) {
	if err := copyFile(bak, dst); err != nil {
		log.Error("Rollback from backup failed: %v — original backup kept at %s, please restore manually", err, bak)
		return
	}
	os.Remove(bak)
}

// copyFile 复制 src 到 dst（覆盖已存在的 dst），fsync 后关闭确保落盘。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// patchDLLBytes seeks to each patch offset and writes only the changed bytes,
// then fsyncs. Does NOT read/write the entire file. 只应由 patchWithBackup 调用
// （它负责备份与回滚）；单独调用没有任何安全网。
func patchDLLBytes(libcefPath string) error {
	f, err := os.OpenFile(libcefPath, os.O_RDWR, 0)
	if err != nil {
		return i18n.Errorf("open libcef.dll: %w", err)
	}
	for i, p := range libcefPatches {
		if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
			f.Close()
			return i18n.Errorf("Patch%d seek: %w", i+1, err)
		}
		if _, err := f.Write(p.data); err != nil {
			f.Close()
			return i18n.Errorf("Patch%d write (offset=0x%X): %w", i+1, p.offset, err)
		}
	}
	// 显式 Sync + Close 并返回其错误：写文件不检查 Close 会漏掉延迟刷盘的失败，
	// 让「patch 成功」名不副实。
	if err := f.Sync(); err != nil {
		f.Close()
		return i18n.Errorf("fsync libcef.dll: %w", err)
	}
	return f.Close()
}

// ElevatedHelper represents a persistent elevated patch-helper process.
// The helper starts elevated (via UAC), signals READY, then waits for a
// trigger before applying patches – so only ONE UAC prompt is needed per session.
type ElevatedHelper struct {
	hProcess  windows.Handle
	trigFile  string
	resFile   string
	readyFile string
}

// 触发文件的两条指令。两者互不为前缀：helper 读到写了一半的内容，不会认成另一条。
const (
	triggerGo     = "GO"     // 酷狗已结束，开始写 DLL
	triggerCancel = "CANCEL" // 主进程不再需要本 helper：退出，不写 DLL
)

const (
	// helperPollInterval 是 helper 等指令的轮询间隔，也是主进程退出后 helper 至多再等的时长。
	helperPollInterval = 200 * time.Millisecond
	// helperTriggerTimeout 是 helper 等指令的上限。
	helperTriggerTimeout = 10 * time.Minute
	// helperTokenLen 是 helper 令牌的长度：8 字节随机数的小写十六进制。
	helperTokenLen = 16
)

// newHelperToken 生成一次拉起所用的令牌。信号文件名带上它，每次拉起各用一组信号文件：上一次拉起的
// helper 卡住后醒来，读写的仍是它自己那一组，不会与新的 helper 轮询同一个触发文件、抢同一个 GO
// （issue #140）。crypto/rand.Read 不返回错误（失败即终止进程）。
func newHelperToken() string {
	b := make([]byte, helperTokenLen/2)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// isHelperToken 判断 s 是否为 newHelperToken 的格式。令牌要拼进文件名，只认恰为 helperTokenLen 个
// 小写十六进制字符，路径分隔符、".." 之类一律不合格。
func isHelperToken(s string) bool {
	if len(s) != helperTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// signalFiles 返回令牌 token 那一组信号文件（READY、触发、结果）的路径。文件里只放 READY、GO、CANCEL
// 与结果，不放路径（#41）。
func signalFiles(dir, token string) (ready, trig, res string) {
	p := filepath.Join(dir, "kugou_cdp_patch_"+token)
	return p + "_ready.txt", p + "_trigger.txt", p + "_result.txt"
}

// TriggerAndWait signals the helper to patch the DLL and waits for the result.
// Must not be called more than once per helper.
func (h *ElevatedHelper) TriggerAndWait() error {
	if err := os.WriteFile(h.trigFile, []byte(triggerGo), 0600); err != nil {
		return i18n.Errorf("write trigger file failed: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(h.resFile); err == nil {
			result := strings.TrimSpace(string(data))
			if result == "OK" {
				return nil
			}
			if strings.HasPrefix(result, "ERROR") {
				return fmt.Errorf("%s", result)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return i18n.Errorf("patch timed out (no result within 30s)")
}

// running 报告 helper 进程是否可能仍在运行：只有确知已退出时返回 false，调用方据此不结束酷狗（issue #141）。
// 等待失败（无从确认）按仍在运行处理，沿用 #141 之前的行为——结束酷狗、由 TriggerAndWait 的结果判断：
// ShellExecuteExW 返回的句柄若不可等待，按「不在」处理会让酷狗永远不被结束、修补流程整体失效。
func (h *ElevatedHelper) running() bool {
	return !h.exited()
}

// exited 报告 helper 进程是否确知已退出；仍在运行、等待失败（无从确认）都返回 false。
func (h *ElevatedHelper) exited() bool {
	ev, _ := windows.WaitForSingleObject(h.hProcess, 0)
	return ev == windows.WAIT_OBJECT_0
}

// Close 释放进程句柄、清理本次拉起的信号文件。除非确知 helper 已退出，否则向触发文件写 CANCEL 而不是
// 删掉它：launchElevatedHelper 等不到 READY、写触发文件失败、waitForKuGou 因 stopCh 返回时，helper 仍在
// 等指令，不通知它就要等到本进程退出或 10 分钟超时才走（issue #140）。helper 读到 CANCEL 即退出、不写
// DLL；已收到 GO、正在写 DLL 的不再读触发文件，写完或回滚后退出，退出前删掉触发文件与 READY。
func (h *ElevatedHelper) Close() {
	if h.exited() {
		os.Remove(h.trigFile) //nolint
	} else {
		_ = os.WriteFile(h.trigFile, []byte(triggerCancel), 0600)
	}
	windows.CloseHandle(h.hProcess)
	os.Remove(h.readyFile) //nolint
	os.Remove(h.resFile)   //nolint
}

// RunPatchHelper is called when the process is re-launched elevated with
// --kugou-patch-helper <libcefPath> <主进程 PID> <令牌>（args 是这三项）。libcefPath 从命令行参数读取，
// 不经 %TEMP% 文件（见 launchElevatedHelper 的说明）。流程见 runPatchHelper。
func RunPatchHelper(args []string) {
	// 取不到自身创建时间时为 0，openParent 随之拒绝任何主进程：宁可不写。
	self, _ := processCreated(windows.CurrentProcess())
	os.Exit(runPatchHelper(args, windows.GetCurrentProcessId(), self, os.TempDir()))
}

// runPatchHelper 是 RunPatchHelper 的本体，返回进程退出码。selfPID、selfCreated 是 helper 自己的
// PID 与创建时间（Unix 纳秒），tmpDir 是信号文件所在目录，三者为测试而外置。
//
// 参数个数或令牌不合格时直接退出，不写任何文件。路径或 PID 不合格、主进程打不开或核对不过时，把原因
// 写进本次的结果文件后退出，不写 READY、不等指令。否则 signal READY、等指令：等待期间主进程退出或收到
// CANCEL 即退出，不写 DLL；收到 GO 才 patch（patchWithBackup 内含落笔前复检、备份、自校验、失败回滚），
// 写入阶段不再看主进程，写完或回滚后写结果文件退出。
//
// 退出前删掉本组的 READY 与触发文件，只留结果文件给主进程读（主进程的 Close 删）：Close 可能先于 READY
// 写下、或在写入期间写下 CANCEL，按令牌命名的文件此后没有人再清理。
func runPatchHelper(args []string, selfPID uint32, selfCreated int64, tmpDir string) int {
	libcefPath, parentPID, token, err := parseHelperArgs(args, selfPID)
	if token == "" {
		return 1
	}
	readyFile, trigFile, resFile := signalFiles(tmpDir, token)
	defer os.Remove(trigFile)  //nolint
	defer os.Remove(readyFile) //nolint

	var parent windows.Handle
	if err == nil {
		parent, err = openParent(parentPID, selfCreated)
	}
	if err != nil {
		writePatchResult(resFile, "ERROR: "+err.Error())
		return 1
	}
	defer windows.CloseHandle(parent)

	// Signal ready to main process
	_ = os.WriteFile(readyFile, []byte("READY"), 0600)

	deadline := time.Now().Add(helperTriggerTimeout)
	switch awaitTrigger(
		func() bool { return !time.Now().Before(deadline) },
		func(wait time.Duration) bool { return parentExitedWithin(parent, wait) },
		func() string { return takeTrigger(trigFile) },
	) {
	case verdictParentExited, verdictCancelled:
		return 0
	case verdictTimedOut:
		writePatchResult(resFile, "ERROR: "+i18n.T("timed out waiting (no patch instruction within 10 minutes)"))
		return 1
	}

	if err := patchWithBackup(libcefPath); err != nil {
		writePatchResult(resFile, "ERROR: "+err.Error())
		return 1
	}
	writePatchResult(resFile, "OK")
	return 0
}

// helperParams 是提权 helper 的命令行参数：libcef.dll 路径、主进程 PID 与令牌。路径加引号（可能含
// 空格）；它以 libcef.dll 结尾，不会以反斜杠结尾而把右引号转义掉。
func helperParams(libcefPath string, parentPID uint32, token string) string {
	return `--kugou-patch-helper "` + libcefPath + `" ` + strconv.FormatUint(uint64(parentPID), 10) + " " + token
}

// parseHelperArgs 解析 --kugou-patch-helper 之后的参数，恰为 libcef.dll 路径、主进程 PID、令牌三项。
// 令牌须合 isHelperToken 的格式（它要拼进信号文件名），先于其余两项检查：个数与令牌都合格、路径或 PID
// 不合格时，报错的同时仍返回令牌，调用方据此把原因写进本次的结果文件；否则令牌返回空串。PID 为 0 或
// 等于 helper 自己的 PID 时拒绝：前者打不开，后者等的是自己、永远等不到退出。
func parseHelperArgs(args []string, selfPID uint32) (libcefPath string, parentPID uint32, token string, err error) {
	if len(args) != 3 {
		return "", 0, "", i18n.Errorf("expected 3 arguments after --kugou-patch-helper (libcef.dll path, main process PID, token), got %d", len(args))
	}
	if !isHelperToken(args[2]) {
		return "", 0, "", i18n.Errorf("invalid helper token: %q", args[2])
	}
	if strings.TrimSpace(args[0]) == "" {
		return "", 0, args[2], i18n.Errorf("libcef.dll path not provided")
	}
	pid, perr := strconv.ParseUint(args[1], 10, 32)
	if perr != nil || pid == 0 || uint32(pid) == selfPID {
		return "", 0, args[2], i18n.Errorf("invalid main process PID: %q", args[1])
	}
	return args[0], uint32(pid), args[2], nil
}

// processCreated 返回进程的创建时间（Unix 纳秒）。
func processCreated(h windows.Handle) (int64, error) {
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return 0, i18n.Errorf("query process creation time: %w", err)
	}
	return created.Nanoseconds(), nil
}

// openParent 打开主进程的同步句柄。打开之前主进程若已退出，PID 可能已被复用：主进程一定早于 helper
// 创建，创建时间不早于 selfCreated（helper 自己的创建时间）的就不是它。打不开、核对不过都返回错误，
// 调用方按主进程已不在处理。句柄打开之后进程对象不再释放，PID 不会再被复用。
func openParent(pid uint32, selfCreated int64) (windows.Handle, error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, i18n.Errorf("open main process (PID %d): %w", pid, err)
	}
	created, err := processCreated(h)
	if err == nil && created >= selfCreated {
		err = i18n.Errorf("process %d was not created before this helper; the main process has exited", pid)
	}
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// parentExitedWithin 等主进程至多 d，其间退出返回 true。等待本身失败时无法确认主进程还在，同样返回
// true：宁可不写。
func parentExitedWithin(h windows.Handle, d time.Duration) bool {
	ev, _ := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	return ev != uint32(windows.WAIT_TIMEOUT)
}

// takeTrigger 读触发文件。内容恰为 GO 或 CANCEL 时删掉文件并返回该指令；文件不存在、主进程刚建好文件
// 还没写完（空或半截）、内容不认识，都返回空串、不删，下一轮再读。
func takeTrigger(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	switch s := string(data); s {
	case triggerGo, triggerCancel:
		os.Remove(path) //nolint
		return s
	}
	return ""
}

// helperVerdict 是 helper 等指令的结论。
type helperVerdict int

const (
	verdictPatch        helperVerdict = iota // 收到 GO：写 DLL
	verdictParentExited                      // 主进程已退出：不写
	verdictCancelled                         // 收到 CANCEL：不写
	verdictTimedOut                          // 超时：不写
)

// awaitTrigger 轮询到得出结论为止。parentExited(wait) 等主进程至多 wait、其间退出返回 true。每轮先等
// 主进程一个轮询间隔（兼作轮询的等待），再读触发文件。同一轮里主进程已退出、触发文件里又有 GO 时按
// 主进程退出处理、不写 DLL，触发文件也不读：发出 GO 的主进程已不在，写完之后没有人校验、重启酷狗，
// 也没有人拦住用户这时自行启动酷狗。读到 GO 后再不等待地查一次主进程：它可能在本轮的等待之后才写下
// GO、随即退出，同样不写。
func awaitTrigger(expired func() bool, parentExited func(wait time.Duration) bool, takeTrigger func() string) helperVerdict {
	for !expired() {
		if parentExited(helperPollInterval) {
			return verdictParentExited
		}
		switch takeTrigger() {
		case triggerGo:
			if parentExited(0) {
				return verdictParentExited
			}
			return verdictPatch
		case triggerCancel:
			return verdictCancelled
		}
	}
	return verdictTimedOut
}

// launchElevatedHelper re-launches self elevated with --kugou-patch-helper via
// ShellExecuteExW. Waits up to 30s for the helper to signal READY（helper 启动即失败时提前返回，见 waitReady）.
func launchElevatedHelper(libcefPath string) (*ElevatedHelper, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, i18n.Errorf("get own executable path failed: %w", err)
	}
	// 每次拉起用新令牌、各用一组信号文件，见 newHelperToken。
	token := newHelperToken()
	readyFile, trigFile, resFile := signalFiles(os.TempDir(), token)

	// libcefPath 通过命令行参数传给提权进程，不经 %TEMP% 文件——后者在 UAC 窗口内可被
	// 同用户的中完整性进程改写，把提权后的高完整性写入重定向到任意文件（本地提权）。
	// 命令行在 ShellExecuteExW 调用时即固定，提权进程从 os.Args 读，中途无法篡改。
	// 其后是本进程 PID（helper 据此在本进程退出时随之退出）与令牌（issue #140）。
	verbW, _ := windows.UTF16PtrFromString("runas")
	fileW, _ := windows.UTF16PtrFromString(exe)
	paramsW, _ := windows.UTF16PtrFromString(helperParams(libcefPath, windows.GetCurrentProcessId(), token))
	dirW, _ := windows.UTF16PtrFromString(filepath.Dir(exe))

	info := shellExecuteInfoW{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verbW,
		lpFile:       fileW,
		lpParameters: paramsW,
		lpDirectory:  dirW,
		nShow:        0,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, sysErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return nil, i18n.Errorf("UAC elevation failed (user may have denied UAC?): %w", sysErr)
	}
	if info.hProcess == 0 {
		return nil, i18n.Errorf("UAC elevation failed: could not get process handle (user may have denied UAC?)")
	}

	h := &ElevatedHelper{
		hProcess:  info.hProcess,
		trigFile:  trigFile,
		resFile:   resFile,
		readyFile: readyFile,
	}
	if err := h.waitReady(); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// helperReadyTimeout 是等 helper 写下 READY 的上限。
const helperReadyTimeout = 30 * time.Second

// waitReady 等 helper 写下 READY，至多 helperReadyTimeout。helper 启动即失败（写下 ERROR 或进程已退出）
// 时立即返回错误，带上它写下的原因（issue #142）。判定见 awaitReady。
func (h *ElevatedHelper) waitReady() error {
	deadline := time.Now().Add(helperReadyTimeout)
	switch v, reason := awaitReady(
		func() bool { return !time.Now().Before(deadline) },
		h.exited,
		func() string { return readFileText(h.readyFile) },
		func() string { return readFileText(h.resFile) },
		helperPollInterval,
	); v {
	case readyOK:
		return nil
	case readyFailed:
		return i18n.Errorf("elevated helper failed at startup: %s", reason)
	case readyExited:
		return i18n.Errorf("elevated helper exited at startup without giving a reason")
	}
	return i18n.Errorf("elevated helper not ready within 30s (started but unresponsive)")
}

// readFileText 返回文件内容；读不到返回空串。
func readFileText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// readyWait 是等 helper READY 的结论。
type readyWait int

const (
	readyOK       readyWait = iota // 写下了 READY
	readyFailed                    // 结果文件里有 ERROR：启动即失败，原因随之返回
	readyExited                    // 进程已退出，没有留下原因
	readyTimedOut                  // 超时
)

// awaitReady 每 interval 轮询一次，直到得出结论。每轮依次看 READY、helper 是否已退出、结果文件：READY 在
// 就算就绪（与结果文件同一轮出现时也是）；先看进程再读结果文件，已退出时读到的就是写完的内容。helper 仍在
// 运行时读到 ERROR，要连续两轮内容相同才采信，以免报出写了一半的原因；它写完结果随即退出，通常下一轮就以
// 已退出的分支读到全文。helperExited 只在确知已退出时返回 true，无从确认时照旧等到超时。
func awaitReady(expired, helperExited func() bool, readReady, readResult func() string, interval time.Duration) (readyWait, string) {
	last := ""
	for !expired() {
		if strings.TrimSpace(readReady()) == "READY" {
			return readyOK, ""
		}
		exited := helperExited()
		res := strings.TrimSpace(readResult())
		if strings.HasPrefix(res, "ERROR: ") && (exited || res == last) {
			return readyFailed, strings.TrimPrefix(res, "ERROR: ")
		}
		if exited {
			return readyExited, ""
		}
		last = res
		time.Sleep(interval)
	}
	return readyTimedOut, ""
}

// kugouWait 是等酷狗启动的结论。
type kugouWait int

const (
	kugouStarted         kugouWait = iota // 酷狗已出现，helper 仍在（或本就没有 helper）
	kugouStartedNoHelper                  // 酷狗已出现，但 helper 已退出：不得结束酷狗
	kugouWaitStopped                      // stopCh 已关闭
)

// waitForKuGou polls every 500ms until KuGou.exe appears or stopCh is closed. helperRunning 为 nil
// 表示没有 helper（DLL 已修补，只等酷狗）。判定见 awaitKuGou。
func waitForKuGou(stopCh <-chan struct{}, helperRunning func() bool) kugouWait {
	logged := false
	return awaitKuGou(stopCh, 500*time.Millisecond, helperRunning, func() bool {
		if IsKuGouRunning() {
			return true
		}
		if !logged {
			log.Info("Waiting for KuGou to start...")
			logged = true
		}
		return false
	})
}

// awaitKuGou 每 interval 轮询一次，直到 stopCh 关闭或酷狗出现。每轮依次看 stopCh、helper、酷狗。
//
// helper 中途退出（它等指令至多 10 分钟）时不就此返回，接着等酷狗：此时返回，上层重试会立即再弹 UAC，
// 酷狗一直不启动就每次 helper 超时弹一次。酷狗出现时 helper 已退出（含同一轮里两者同时发生）返回
// kugouStartedNoHelper：没有 helper 就修补不了，结束酷狗只会让它白白关掉（issue #141）。
func awaitKuGou(stopCh <-chan struct{}, interval time.Duration, helperRunning, kugouRunning func() bool) kugouWait {
	lost := false
	for {
		select {
		case <-stopCh:
			return kugouWaitStopped
		default:
		}
		if helperRunning != nil && !lost && !helperRunning() {
			lost = true
			log.Warn("Elevated patch helper has exited (it waits at most 10 minutes); KuGou will not be stopped when it starts, and the repair will be retried after that")
		}
		if kugouRunning() {
			if lost {
				return kugouStartedNoHelper
			}
			return kugouStarted
		}
		select {
		case <-stopCh:
			return kugouWaitStopped
		case <-time.After(interval):
		}
	}
}

// readyToKill 根据等酷狗的结论决定能否结束酷狗去修补，返回 nil 才可以。酷狗出现之后再查一次 helper：
// awaitKuGou 看过 helper 之后还要跑一次 tasklist 才看到酷狗，helper 可能在其间退出；不在就不结束酷狗，
// 返回错误交上层重试（issue #141）。
func readyToKill(w kugouWait, helperRunning func() bool) error {
	switch w {
	case kugouWaitStopped:
		return i18n.Errorf("stopped")
	case kugouStarted:
		if helperRunning() {
			return nil
		}
	}
	return i18n.Errorf("elevated patch helper exited before KuGou was stopped; KuGou was left running and the repair will be retried")
}

// EnsurePatched is the main entry point called by the player before connecting to CDP.
//
// New workflow (like NetEase Cloud Music):
//  1. CDP already available → return immediately (fast path)
//  2. Locate installation, log version
//  3. DLL already patched → wait for KuGou process to appear, then return
//  4. DLL not patched →
//     a. UAC UPFRONT: launch elevated helper that waits for a trigger
//     b. Wait for KuGou to appear in process list (user starts KuGou)
//     c. Kill KuGou, trigger helper to patch (no second UAC)；helper 已退出时不结束酷狗，返回错误交上层重试
//     d. Verify, re-launch KuGou
func EnsurePatched(stopCh <-chan struct{}) error {
	if isCDPAvailable() {
		log.Detail("CDP already available; skipping patch check")
		return nil
	}

	exePath, libcefPath, err := FindKuGouInstall()
	if err != nil {
		return err
	}
	log.Info("KuGou install directory: %s", filepath.Dir(exePath))

	// 版本号不作**正向**判据——「能不能打」由 libcef.dll 的字节指纹决定（见
	// CheckPatchStatus）。KuGou.exe 的版本号与它 vendored 的 CEF 版本相互独立，拿前者当
	// 正向闸门既会误伤（同 CEF 的点版本被拒）又会漏放（CEF 热修但 exe 版本不变）。
	ver := readExeVersion(exePath)
	if ver == "" {
		ver = filepath.Base(filepath.Dir(libcefPath))
	}
	if ver == knownVersion {
		log.Info("KuGou version: %s (recommended)", ver)
	} else {
		log.Info("KuGou version: %s (recommended version is %s; the libcef.dll byte fingerprint is authoritative)", ver, knownVersion)
	}
	// 全量版本 tag：支持与否都设，让 Sentry 聚合野外最多的酷狗版本（异常上报只覆盖被拒的版本）。
	telemetry.SetPlayerVersion("kugou", ver)

	// 但保留一道**负向**黑名单：已知不兼容的大版本在字节校验之前就确定性拒绝。10.x 与
	// 补丁针对的 20.x 是不同的 CEF 基线，偏移不同；orig 指纹通常能识别，但黑名单零成本、
	// 且不受「某个 10.x 的字节恰好落进 20.x {orig,data}」这种巧合影响。由 80e9182 引入
	// （实际遭遇过 10.x）。黑名单读 exe 版本、字节指纹读 libcef 内容，两个独立证据源。
	if isUnsupportedMajor(ver) {
		// 「预期之外」：主播装的是 10.x。我们知道它不支持（所以这里是正确的降级），
		// 但不知道有多少人这样 —— 那个数字决定要不要为 10.x 做逆向。
		// 与网易云 v2 那条同构。
		telemetry.ReportOnce("kugou.version_unsupported",
			"酷狗 10.x 系列不受支持（补丁只针对 20.x 的 CEF 基线）",
			map[string]string{"kugou.version": ver},
			map[string]any{"known_version": knownVersion})
		return i18n.Errorf("%w: KuGou %s is in the 10.x series; patches target only %s (different CEF baseline)", ErrVersionUnsupported, ver, knownVersion)
	}

	allPatched, canAutoFix, err := CheckPatchStatus(libcefPath)
	if err != nil {
		return i18n.Errorf("check patch status: %w", err)
	}
	if !canAutoFix {
		// 9 处偏移里有字节既非原版也非已 patch：这个 DLL 不是我们逆向针对的那个 CEF。
		// 拒绝盲打——拿错版本的偏移写 93MB 签名 DLL 会让用户被迫重装酷狗。终态错误。
		//
		// 「预期之外」，且是这几条里最值钱的一条：酷狗换了 vendored 的 CEF，而我们的偏移表
		// 停在 20.1.22.27795 / CEF 89.20.0。上报带上酷狗版本 —— **那正是下一次该逆向哪个
		// 版本的输入**。注意酷狗版本与它 vendored 的 CEF 版本相互独立（见 :699 注释），
		// 所以这里报的是酷狗版本，只作线索，不能直接当 CEF 版本用。
		telemetry.ReportOnce("kugou.libcef_unsupported",
			"酷狗的 libcef.dll 与已知偏移不符，已拒绝打补丁",
			map[string]string{"kugou.version": ver},
			map[string]any{
				"known_version": knownVersion,
				"libcef_path":   libcefPath,
			})
		return i18n.Errorf("%w: libcef.dll of %s does not match known offsets; refusing blind patch (reverse-engineered against %s)", ErrVersionUnsupported, ver, knownVersion)
	}

	if allPatched {
		// DLL 已 patch，等待酷狗启动（仿网易云逻辑，不自动启动）
		if waitForKuGou(stopCh, nil) == kugouWaitStopped {
			return i18n.Errorf("stopped")
		}
		return nil
	}

	// DLL 未 patch：先弹 UAC 获取提权（helper 在后台等待触发指令）
	log.Info("libcef.dll needs repair; requesting administrator rights (UAC)...")
	helper, err := launchElevatedHelper(libcefPath)
	if err != nil {
		return i18n.Errorf("%w (user may have denied UAC): %v", ErrElevationFailed, err)
	}
	defer helper.Close()

	log.Info("Administrator rights obtained; waiting for KuGou to start to complete the repair...")
	if err := readyToKill(waitForKuGou(stopCh, helper.running), helper.running); err != nil {
		return err
	}

	// 检测到酷狗、helper 仍在，立即停止并触发 patch（无需再次 UAC）
	log.Info("KuGou detected; stopping it and repairing the DLL...")
	killKuGou()

	if err := helper.TriggerAndWait(); err != nil {
		return i18n.Errorf("DLL patch failed: %w", err)
	}

	verified, _, verifyErr := CheckPatchStatus(libcefPath)
	if verifyErr != nil {
		return i18n.Errorf("post-patch read verification failed: %w", verifyErr)
	}
	if !verified {
		// 「预期之外」：helper 报告写成功了，但读回来字节还是原样。
		//
		// 这不是假想 —— 硬化笔记记着「杀软静默回滚写入时，字节仍为 orig」，酷狗 libcef 补丁
		// 被回滚是已发生过的真实故障。主播那头只看到「补丁失败」，我们这头需要知道**有多少人
		// 被杀软挡住**，那决定要不要去做白名单引导之类的事。
		telemetry.ReportOnce("kugou.patch_reverted",
			"酷狗补丁写入后字节未变更（疑似被杀软静默回滚）",
			map[string]string{"kugou.version": ver},
			map[string]any{"libcef_path": libcefPath})
		return i18n.Errorf("post-patch write verification failed: bytes unchanged (check your antivirus software)")
	}

	log.Success("libcef.dll patch complete. Restarting KuGou...")
	if err := launchKuGou(exePath); err != nil {
		log.Warn("Failed to start KuGou: %v (please start it manually)", err)
	}
	return nil
}
