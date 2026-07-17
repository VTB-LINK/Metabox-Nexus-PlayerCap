package watchdog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/telemetry"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var log = logger.New("KuGou] [Watchdog") // 渲染为 [KuGou] [Watchdog]

// ErrInstallNotFound 表示未找到酷狗安装，属于不可恢复错误，不应重试。
var ErrInstallNotFound = errors.New("未找到酷狗安装（KuGou.exe 或 libcef.dll 不存在）")

// ErrVersionUnsupported 表示 libcef.dll 在已知偏移处的字节指纹既非原版也非已 patch，
// 版本不认识，拒绝盲打补丁。与 ErrInstallNotFound 同属终态错误，不应重试（重试也只会
// 反复弹 UAC）。上层据此停止而非空转，见 kugou.go 的分流。
var ErrVersionUnsupported = errors.New("libcef.dll 版本不受支持（已知偏移处字节指纹不匹配）")

// ErrElevationFailed 表示未取得管理员权限（用户拒绝了 UAC，或 helper 未按时就绪）。
//
// **上层必须退避并设上限**：本函数在 launchElevatedHelper 处弹 UAC，被拒即返回；调用方
// 若立刻重试，UAC 安全桌面就会全屏夺焦——直播时是事故。见 kugou.go 的 elevationGate。
var ErrElevationFailed = errors.New("未取得管理员权限")

const (
	targetProcess = "KuGou.exe"
	cdpPort       = 12233

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
		return false, false, fmt.Errorf("open libcef.dll: %w", err)
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
			return false, false, fmt.Errorf("read Patch%d (offset=0x%X): %w", i+1, p.offset, err)
		}
		isData := bytes.Equal(buf, p.data)
		isOrig := bytes.Equal(buf, p.orig)
		if !isData {
			allPatched = false
		}
		if !isData && !isOrig {
			// 该偏移既非已打补丁的字节，也非我们认识的原始指令：版本指纹不匹配
			log.Warn("Patch%d (offset=0x%X) 字节 %X 既非原始也非已 patch，libcef.dll 版本不受支持", i+1, p.offset, buf)
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
		log.Warn("未找到与 KuGou.exe 版本 %q 同名的 libcef.dll 目录，回退到版本号最大的 %q", ver, bestName)
	}
	return best
}

// killKuGou terminates all running KuGou.exe processes and waits for them to exit.
func killKuGou() {
	log.Info("正在终止酷狗进程...")
	exec.Command("taskkill", "/F", "/IM", targetProcess).Run() //nolint
	// Poll until all instances exit (max 5s)
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if !IsKuGouRunning() {
			return
		}
	}
	log.Warn("酷狗进程未在 5s 内完全退出，继续...")
}

// IsKuGouRunning returns true if any KuGou.exe process is alive.
func IsKuGouRunning() bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+targetProcess, "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), targetProcess)
}

// launchKuGou starts KuGou.exe in detached mode from its own directory.
func launchKuGou(exePath string) error {
	cmd := exec.Command(exePath)
	cmd.Dir = filepath.Dir(exePath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 KuGou.exe: %w", err)
	}
	log.Info("酷狗已启动（PID %d），等待初始化...", cmd.Process.Pid)
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
		return fmt.Errorf("落笔前复检失败: %w", err)
	}
	if !canAutoFix {
		return fmt.Errorf("落笔前复检：libcef.dll 版本指纹不符，拒绝 patch")
	}
	if allPatched {
		return nil // 已是 patched 态，无需再写
	}

	bak := libcefPath + ".playercap.bak"
	if err := copyFile(libcefPath, bak); err != nil {
		return fmt.Errorf("备份 libcef.dll 失败: %w", err)
	}

	if err := patchDLLBytes(libcefPath); err != nil {
		rollback(bak, libcefPath)
		return fmt.Errorf("patch 写入失败（已尝试回滚）: %w", err)
	}

	// 写后自校验：9 处必须全部变为 data。校验的是刚写的字节，只能抓「写没落盘/被回滚」，
	// 版本正确性由上面的复检与 orig 指纹保证。
	verified, _, verr := CheckPatchStatus(libcefPath)
	if verr != nil {
		rollback(bak, libcefPath)
		return fmt.Errorf("patch 后校验读取失败（已尝试回滚）: %w", verr)
	}
	if !verified {
		rollback(bak, libcefPath)
		return fmt.Errorf("patch 后校验失败：字节未按预期变更（已回滚，可能被杀软拦截）")
	}

	os.Remove(bak) // 成功，删除备份
	return nil
}

// rollback 尽力从备份恢复原文件。恢复成功后删备份；恢复失败则保留备份并在日志留痕，
// 供人工恢复——绝不静默丢弃。
func rollback(bak, dst string) {
	if err := copyFile(bak, dst); err != nil {
		log.Error("从备份回滚失败：%v —— 原文件备份保留在 %s，请手动恢复", err, bak)
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
		return fmt.Errorf("打开 libcef.dll: %w", err)
	}
	for i, p := range libcefPatches {
		if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("Patch%d seek: %w", i+1, err)
		}
		if _, err := f.Write(p.data); err != nil {
			f.Close()
			return fmt.Errorf("Patch%d write (offset=0x%X): %w", i+1, p.offset, err)
		}
	}
	// 显式 Sync + Close 并返回其错误：写文件不检查 Close 会漏掉延迟刷盘的失败，
	// 让「patch 成功」名不副实。
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync libcef.dll: %w", err)
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

// TriggerAndWait signals the helper to patch the DLL and waits for the result.
// Must not be called more than once per helper.
func (h *ElevatedHelper) TriggerAndWait() error {
	if err := os.WriteFile(h.trigFile, []byte("GO"), 0600); err != nil {
		return fmt.Errorf("写入触发文件失败: %w", err)
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
	return fmt.Errorf("patch 超时（30s 内未收到结果）")
}

// Close releases the process handle and cleans up temp signal files.
func (h *ElevatedHelper) Close() {
	windows.CloseHandle(h.hProcess)
	os.Remove(h.trigFile)  //nolint
	os.Remove(h.readyFile) //nolint
	os.Remove(h.resFile)   //nolint
}

// RunPatchHelper is called when the process is re-launched elevated with
// --kugou-patch-helper <libcefPath>. libcefPath 从命令行参数读取，不再经 %TEMP%
// 文件（见 launchElevatedHelper 的说明）。它 signal READY，等 trigger，然后 patch
// （patchWithBackup 内含落笔前复检、备份、自校验、失败回滚），最后写结果文件退出。
func RunPatchHelper(libcefPath string) {
	tmpDir := os.TempDir()
	readyFile := filepath.Join(tmpDir, "kugou_cdp_patch_ready.txt")
	trigFile := filepath.Join(tmpDir, "kugou_cdp_patch_trigger.txt")
	resFile := filepath.Join(tmpDir, "kugou_cdp_patch_result.txt")

	if strings.TrimSpace(libcefPath) == "" {
		writePatchResult(resFile, "ERROR: 未提供 libcef.dll 路径")
		os.Exit(1)
	}

	// Signal ready to main process
	_ = os.WriteFile(readyFile, []byte("READY"), 0600)

	// Wait for trigger (max 10 min)
	deadline := time.Now().Add(10 * time.Minute)
	triggered := false
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(trigFile); statErr == nil {
			os.Remove(trigFile) //nolint
			triggered = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !triggered {
		writePatchResult(resFile, "ERROR: 等待超时（10分钟内未收到 patch 指令）")
		os.Exit(1)
	}

	if err := patchWithBackup(libcefPath); err != nil {
		writePatchResult(resFile, "ERROR: "+err.Error())
		os.Exit(1)
	}
	writePatchResult(resFile, "OK")
	os.Exit(0)
}

// launchElevatedHelper re-launches self elevated with --kugou-patch-helper via
// ShellExecuteExW. Waits up to 30s for the helper to signal READY.
func launchElevatedHelper(libcefPath string) (*ElevatedHelper, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("获取自身路径失败: %w", err)
	}
	tmpDir := os.TempDir()
	readyFile := filepath.Join(tmpDir, "kugou_cdp_patch_ready.txt")
	trigFile := filepath.Join(tmpDir, "kugou_cdp_patch_trigger.txt")
	resFile := filepath.Join(tmpDir, "kugou_cdp_patch_result.txt")

	os.Remove(readyFile) //nolint
	os.Remove(trigFile)  //nolint
	os.Remove(resFile)   //nolint

	// libcefPath 通过命令行参数传给提权进程，不经 %TEMP% 文件——后者在 UAC 窗口内可被
	// 同用户的中完整性进程改写，把提权后的高完整性写入重定向到任意文件（本地提权）。
	// 命令行在 ShellExecuteExW 调用时即固定，提权进程从 os.Args 读，中途无法篡改。
	verbW, _ := windows.UTF16PtrFromString("runas")
	fileW, _ := windows.UTF16PtrFromString(exe)
	paramsW, _ := windows.UTF16PtrFromString(`--kugou-patch-helper "` + libcefPath + `"`)
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
		return nil, fmt.Errorf("UAC 提权失败（用户拒绝了 UAC？）: %w", sysErr)
	}
	if info.hProcess == 0 {
		return nil, fmt.Errorf("UAC 提权失败：无法获取进程句柄（用户拒绝了 UAC？）")
	}

	h := &ElevatedHelper{
		hProcess:  info.hProcess,
		trigFile:  trigFile,
		resFile:   resFile,
		readyFile: readyFile,
	}

	// Wait for READY signal (max 30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(readyFile); err == nil {
			if strings.TrimSpace(string(data)) == "READY" {
				return h, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.Close()
	return nil, fmt.Errorf("elevated helper 未在 30s 内就绪（已启动但无响应）")
}

// waitForKuGou polls every 500ms until KuGou.exe appears or stopCh is closed.
// Returns true when KuGou is running, false when stopped.
func waitForKuGou(stopCh <-chan struct{}) bool {
	if IsKuGouRunning() {
		return true
	}
	log.Info("等待酷狗音乐启动...")
	for {
		select {
		case <-stopCh:
			return false
		default:
		}
		if IsKuGouRunning() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
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
//     c. Kill KuGou, trigger helper to patch (no second UAC)
//     d. Verify, re-launch KuGou
func EnsurePatched(stopCh <-chan struct{}) error {
	if isCDPAvailable() {
		log.Detail("CDP 已就绪，跳过 patch 检查")
		return nil
	}

	exePath, libcefPath, err := FindKuGouInstall()
	if err != nil {
		return err
	}
	log.Info("酷狗安装目录: %s", filepath.Dir(exePath))

	// 版本号不作**正向**判据——「能不能打」由 libcef.dll 的字节指纹决定（见
	// CheckPatchStatus）。KuGou.exe 的版本号与它 vendored 的 CEF 版本相互独立，拿前者当
	// 正向闸门既会误伤（同 CEF 的点版本被拒）又会漏放（CEF 热修但 exe 版本不变）。
	ver := readExeVersion(exePath)
	if ver == "" {
		ver = filepath.Base(filepath.Dir(libcefPath))
	}
	if ver == knownVersion {
		log.Info("酷狗版本: %s（推荐版本）", ver)
	} else {
		log.Info("酷狗版本: %s（推荐版本为 %s，实际以 libcef.dll 字节指纹为准）", ver, knownVersion)
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
		return fmt.Errorf("%w：酷狗 %s 属 10.x 系列，补丁只针对 %s（CEF 基线不同）", ErrVersionUnsupported, ver, knownVersion)
	}

	allPatched, canAutoFix, err := CheckPatchStatus(libcefPath)
	if err != nil {
		return fmt.Errorf("检查 patch 状态: %w", err)
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
		return fmt.Errorf("%w：%s 的 libcef.dll 与已知偏移不符，拒绝盲打（逆向针对 %s）", ErrVersionUnsupported, ver, knownVersion)
	}

	if allPatched {
		// DLL 已 patch，等待酷狗启动（仿网易云逻辑，不自动启动）
		if !waitForKuGou(stopCh) {
			return fmt.Errorf("stopped")
		}
		return nil
	}

	// DLL 未 patch：先弹 UAC 获取提权（helper 在后台等待触发指令）
	log.Info("libcef.dll 需要修复，正在请求管理员权限（UAC）...")
	helper, err := launchElevatedHelper(libcefPath)
	if err != nil {
		return fmt.Errorf("%w（用户可能拒绝了 UAC）: %v", ErrElevationFailed, err)
	}
	defer helper.Close()

	log.Info("管理员权限已获取，等待酷狗音乐启动以完成修复...")
	if !waitForKuGou(stopCh) {
		return fmt.Errorf("stopped")
	}

	// 检测到酷狗，立即停止并触发 patch（无需再次 UAC）
	log.Info("检测到酷狗音乐，正在停止并修复 DLL...")
	killKuGou()

	if err := helper.TriggerAndWait(); err != nil {
		return fmt.Errorf("DLL patch 失败: %w", err)
	}

	verified, _, verifyErr := CheckPatchStatus(libcefPath)
	if verifyErr != nil {
		return fmt.Errorf("patch 后读取验证失败: %w", verifyErr)
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
		return fmt.Errorf("patch 写入后验证失败：字节未变更（请检查防病毒软件）")
	}

	log.Success("libcef.dll patch 完成！正在重新启动酷狗...")
	if err := launchKuGou(exePath); err != nil {
		log.Warn("启动酷狗失败: %v（请手动启动）", err)
	}
	return nil
}
