package watchdog

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/rum"
	"Metabox-Nexus-PlayerCap/telemetry"

	"Metabox-Nexus-PlayerCap/i18n"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var log = logger.New("KuGou] [Watchdog") // 渲染为 [KuGou] [Watchdog]

// ErrInstallNotFound 表示未找到酷狗安装。酷狗不在运行时上层等它启动后重试，届时按运行中的进程定位（酷狗可能
// 装在注册表与常见路径之外）；在运行时上层同样重试，连续多轮仍找不到才按受阻处理、改为定时重查。见 kugou.go 的 classifyEnsureErr。
var ErrInstallNotFound = i18n.Sentinel("KuGou installation not found (KuGou.exe or libcef.dll is missing)")

// ErrVersionUnsupported 表示 libcef.dll 在已知偏移处的字节指纹既非原版也非已 patch，
// 版本不认识，拒绝盲打补丁。上层不立即重试（重试也只会反复弹 UAC），按受阻处理、定时重走只读的判定，
// 换成受支持的版本后照常修补。见 kugou.go 的分流。
var ErrVersionUnsupported = i18n.Sentinel("libcef.dll version unsupported (byte fingerprint at the known offsets does not match)")

// ErrInstallChanged 表示修补前预选了安装、等到酷狗启动后重新定位，启动的却是另一份安装（或重新定位出错）。
// 此时不结束酷狗、不修补，上层稍后重试，届时按运行中的进程定位。见 killAndPatch。
var ErrInstallChanged = i18n.Sentinel("KuGou that started is a different installation; repair postponed")

// ErrElevationFailed 表示未取得管理员权限（用户拒绝了 UAC，或 helper 未按时就绪）。
//
// **上层必须退避并设上限**：本函数在 launchElevatedHelper 处弹 UAC，被拒即返回；调用方
// 若立刻重试，UAC 安全桌面就会全屏夺焦——直播时是事故。见 kugou.go 的 elevationGate。
var ErrElevationFailed = i18n.Sentinel("Administrator rights not obtained")

// ErrElevationNeeded 表示 libcef.dll 需要修补，而本次运行已放弃提权（UAC 连续被拒，见 kugou.go 的 elevationGate）：
// EnsurePatchedNoElevate 走到要弹 UAC 的分支时返回它，不调用 launchElevatedHelper。
var ErrElevationNeeded = i18n.Sentinel("libcef.dll needs repair, but elevation is disabled for this run")

// ErrRunningNotReady 表示酷狗进程正在运行但尚未就绪（外层启动器交接中、主进程未唯一确认、
// 尚无 CEF 子进程或路径瞬时不可读）。运行态绝不回退注册表/磁盘预选，由上层稍后重试。
var ErrRunningNotReady = i18n.Sentinel("KuGou is running but not ready yet")

const (
	TargetProcessName   = "KuGou.exe"
	HDTargetProcessName = "HD_KuGou.exe"
	cdpPort             = 12233

	// knownVersion is the KuGou.exe FileVersion the patches were reverse-engineered against.
	// Patches may work on other versions but are not guaranteed.
	knownVersion = "20.1.22.27795"
)

// TargetProcessNames 是酷狗主/子进程允许使用的精确映像文件名白名单（顺序：先主壳 HD_KuGou.exe、后 KuGou.exe，供结束进程时先停父进程）。
var TargetProcessNames = []string{HDTargetProcessName, TargetProcessName}

// isKuGouProcessName 报告 name 是否精确匹配酷狗允许的映像文件名白名单（大小写不敏感）。
// 严禁使用前缀、后缀、子串或正则匹配，防止误触酷狗直播、酷狗概念版、酷狗唱唱等其他程序。
func isKuGouProcessName(name string) bool {
	return strings.EqualFold(name, TargetProcessName) || strings.EqualFold(name, HDTargetProcessName)
}

// InstallMeta 保存最近识别到的酷狗脱敏元信息（仅白名单枚举文件名与清洗后版本号，绝不含路径）。
type InstallMeta struct {
	Version    string
	ExeVariant string
}

// ExeVariant 将酷狗可执行文件路径映射为固定白名单枚举（"KuGou.exe" / "HD_KuGou.exe"），不匹配返回空串。
func ExeVariant(exePath string) string {
	base := filepath.Base(strings.TrimSpace(exePath))
	switch {
	case strings.EqualFold(base, TargetProcessName):
		return TargetProcessName
	case strings.EqualFold(base, HDTargetProcessName):
		return HDTargetProcessName
	default:
		return ""
	}
}

var (
	lastMetaMu            sync.Mutex
	lastMeta              InstallMeta
	lastLibcefMismatchIdx atomic.Int32
	uacPatchAttempt       atomic.Int32
)

func recordInstallMeta(exePath, ver string) {
	lastMetaMu.Lock()
	lastMeta = InstallMeta{
		Version:    player.SanitizeVersion(ver),
		ExeVariant: ExeVariant(exePath),
	}
	lastMetaMu.Unlock()
}

// LastInstallMeta 返回最近一次检查或探测记录的酷狗脱敏安装元信息；若为空且酷狗正在运行，尝试只读探测一次。
func LastInstallMeta() InstallMeta {
	lastMetaMu.Lock()
	m := lastMeta
	lastMetaMu.Unlock()
	if m.Version != "" || m.ExeVariant != "" {
		return m
	}
	if run, err := runningKuGouFn(); err == nil && run.ExePath != "" {
		ver := readExeVersion(run.ExePath)
		recordInstallMeta(run.ExePath, ver)
		lastMetaMu.Lock()
		m = lastMeta
		lastMetaMu.Unlock()
	}
	return m
}

// VersionBlockError 为 ErrVersionUnsupported 附带脱敏版本号与 libcef 1..9 指纹失配位，供上层 RUM 分类上报。
type VersionBlockError struct {
	Version          string
	MismatchIdx      int // 1..9 表示 libcef 指纹失配；0 表示 10.x 等版本不支持
	IsLibcefMismatch bool
	Err              error
}

func (e *VersionBlockError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *VersionBlockError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type helperTimeoutError struct {
	err error
}

func (e *helperTimeoutError) Error() string { return e.err.Error() }
func (e *helperTimeoutError) Unwrap() error { return e.err }

func emitKugouIntervention(outcome string, attempt int, elapsedMs int64) {
	if elapsedMs < 0 {
		elapsedMs = 0
	}
	props := rum.Props{
		"player":  "kugou",
		"kind":    "kugou_uac_patch",
		"outcome": outcome,
	}
	if attempt > 0 {
		props["attempt"] = attempt
	}
	if elapsedMs > 0 || outcome != "detached_cmd_fallback" {
		props["elapsed_ms"] = elapsedMs
	}
	rum.Track(rum.CatPlayer, "player_intervention", props)
}

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
//
// 经 openShared 以读写删全共享打开：Confirm 每个会话都会调用本函数，句柄存续期间不能挡住酷狗更新器改名或删除该 DLL。
func CheckPatchStatus(libcefPath string) (allPatched bool, canAutoFix bool, err error) {
	return checkPatchStatus(libcefPath, true)
}

// checkPatchStatus 同 CheckPatchStatus；warn 为假时指纹不认识也不打警告（ensurePatched 重查同一份 DLL 时）。
func checkPatchStatus(libcefPath string, warn bool) (allPatched bool, canAutoFix bool, err error) {
	lastLibcefMismatchIdx.Store(0)
	f, err := openShared(libcefPath)
	if err != nil {
		return false, false, i18n.Errorf("open libcef.dll: %w", err)
	}
	defer f.Close()
	all, fix, mismatchIdx, err := classifyLibcefDetail(f, warn)
	if !fix && err == nil && mismatchIdx > 0 {
		lastLibcefMismatchIdx.Store(int32(mismatchIdx))
	}
	return all, fix, err
}

// classifyLibcef 是 CheckPatchStatus 的纯判定核心，只依赖 io.ReaderAt，便于用合成
// 字节流覆盖三态（真实 DLL 有 ~139MB，不适合进单元测试）。*os.File 满足 io.ReaderAt，
// 其 ReadAt 在读不满时必返回 error（io.ReaderAt 契约），故无需 io.ReadFull；ReadAt
// 也不移动文件偏移，与并发/复用同一路径的读写互不干扰。warn 为真时，指纹不认识打一条 Warn，写明哪一处、什么字节。
func classifyLibcef(r io.ReaderAt, warn bool) (allPatched bool, canAutoFix bool, err error) {
	all, fix, _, err := classifyLibcefDetail(r, warn)
	return all, fix, err
}

func classifyLibcefDetail(r io.ReaderAt, warn bool) (allPatched bool, canAutoFix bool, mismatchIdx int, err error) {
	allPatched = true // 直到发现某处不等于 data
	for i, p := range libcefPatches {
		buf := make([]byte, len(p.data)) // len(orig)==len(data)，见 TestPatchTableConsistency
		if _, err := r.ReadAt(buf, p.offset); err != nil {
			return false, false, 0, i18n.Errorf("read Patch%d (offset=0x%X): %w", i+1, p.offset, err)
		}
		isData := bytes.Equal(buf, p.data)
		isOrig := bytes.Equal(buf, p.orig)
		if !isData {
			allPatched = false
		}
		if !isData && !isOrig {
			// 该偏移既非已打补丁的字节，也非我们认识的原始指令：版本指纹不匹配
			if warn {
				log.Warn("Patch%d (offset=0x%X) bytes %X are neither original nor patched; libcef.dll version unsupported", i+1, p.offset, buf)
			}
			return false, false, i + 1, nil
		}
	}
	return allPatched, true, 0, nil
}

// FindKuGouInstall locates the KuGou executable and the libcef.dll it actually loads.
//
// 运行态与冷态严格分离：
//   - 酷狗正在运行时（runningKuGouFn 返回 nil）：经 resolveRunningInstall 仅认运行中 CEF 子进程所在的唯一目录，
//     绝不回退注册表 KuGou8 或按版本目录猜测；
//   - 确知没有任何酷狗进程运行时（errors.Is(rerr, ErrNotRunning)）：经 findColdInstall 按磁盘与注册表预选；
//   - 其余运行态瞬态/异常（多主进程、启动器交接中、路径查询失败）：返回包装 ErrRunningNotReady 的错误，绝不退回冷态。
func FindKuGouInstall() (exePath, libcefPath string, err error) {
	run, rerr := runningKuGouFn()
	if rerr == nil {
		return resolveRunningInstall(run)
	}
	if errors.Is(rerr, ErrNotRunning) {
		return findColdInstall()
	}
	return "", "", i18n.Errorf("%w: %v", ErrRunningNotReady, rerr)
}

// locateRunningInstall 只按运行中的进程定位安装，供修补前重新确认（killAndPatch）：
// runningKuGouFn 出错时原样返回错误，绝不退回注册表或版本目录推导。
func locateRunningInstall() (string, string, error) {
	run, err := runningKuGouFn()
	if err != nil {
		return "", "", err
	}
	return resolveRunningInstall(run)
}

// resolveRunningInstall 根据运行中的酷狗进程快照解析安装路径与 libcef.dll：
// 零注册表/版本目录回退，只认运行中 CEF 子进程实际所在的唯一目录（与 Confirm 同源）。
func resolveRunningInstall(run runningKuGou) (exePath, libcefPath string, err error) {
	switch len(run.ChildDirs) {
	case 0:
		return "", "", i18n.Errorf("%w: KuGou (PID %d) has no CEF child process yet", ErrRunningNotReady, run.PID)
	case 1:
	default:
		return "", "", i18n.Errorf("%w: the CEF child processes of KuGou run from different directories: %s", ErrVersionUnsupported, strings.Join(run.ChildDirs, "; "))
	}
	lc := kugou8LibcefIfDir(run.ChildDirs[0])
	if lc == "" {
		return "", "", i18n.Errorf("%w: libcef.dll not found in %s", ErrInstallNotFound, run.ChildDirs[0])
	}
	return run.ExePath, lc, nil
}

// findColdInstall 在确知没有任何酷狗进程运行时按磁盘与注册表预选安装：
// KuGou.exe 取注册表 AppPath 或常见安装根；libcef.dll 首选注册表 KuGou8（须经 underDir 守卫同属该安装目录），
// 不可用时再按 KuGou.exe 版本推导。
func findColdInstall() (exePath, libcefPath string, err error) {
	exePath = findKuGouExe("")
	if exePath == "" {
		return "", "", ErrInstallNotFound
	}
	libcefPath = libcefFromKuGou8()
	if !underDir(libcefPath, filepath.Dir(exePath)) {
		libcefPath = ""
	}
	if libcefPath == "" {
		libcefPath = findLibcefForExe(filepath.Dir(exePath), readExeVersion(exePath))
	}
	if libcefPath == "" {
		return "", "", ErrInstallNotFound
	}
	return exePath, libcefPath, nil
}

// underDir 报告 path 是否在 dir 之下（不含 dir 本身）。filepath.Rel 先 Clean，再按路径分隔符逐段比较，Windows 上
// 不分大小写，故 `…\B2\x` 不算在 `…\B` 之下；path 为空、不同盘符时返回 false。
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runningKuGouFn 取运行中的酷狗（主进程映像与 CEF 子进程目录），供找安装用；测试替换。
var runningKuGouFn = func() (runningKuGou, error) {
	procs, err := snapshotProcs()
	if err != nil {
		return runningKuGou{}, err
	}
	return findRunningKuGou(procs, imagePath)
}

// regKuGouFn 只读地取注册表 HKCU\Software\KuGou 下名为 name 的字符串值，读不出返回空；测试替换。
var regKuGouFn = func(name string) string {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\KuGou`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	v, _, err := key.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

// kugouInstallRoots 是注册表 AppPath 不可用时依次查找 KuGou.exe 的常见安装根；测试替换。
var kugouInstallRoots = []string{
	`C:\Program Files\KuGou\KGMusic`,
	`C:\Program Files (x86)\KuGou\KGMusic`,
}

// libcefFromKuGou8 读注册表 HKCU\Software\KuGou\KuGou8 并经 kugou8LibcefIfDir 守卫，
// 返回其下的 libcef.dll 路径；不可用（键缺失/指文件/无 libcef.dll）时返回空。
func libcefFromKuGou8() string {
	return kugou8LibcefIfDir(regKuGouFn("KuGou8"))
}

// kugou8LibcefIfDir 是 KuGou8 的守卫（抽出以便不碰注册表做单测），运行中子进程的目录也经它：dir 必须是
// 一个存在的目录、且其下存在普通文件 libcef.dll，才返回该 libcef.dll 路径。KuGou8 已知会被改写成 KuGou.exe
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
	lfi, err := os.Stat(lc)
	if err != nil || !lfi.Mode().IsRegular() {
		return ""
	}
	return lc
}

// findKuGouExe 定位 KuGou.exe：running（运行中主进程的映像路径，酷狗不在运行时为空）优先，其次注册表
// AppPath（其下须有 KuGou.exe），否则扫描常见安装根。
func findKuGouExe(running string) string {
	if running != "" {
		return running
	}
	if appPath := regKuGouFn("AppPath"); appPath != "" {
		exe := filepath.Join(appPath, "KuGou.exe")
		if _, se := os.Stat(exe); se == nil {
			return exe
		}
	}
	for _, base := range kugouInstallRoots {
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
// 版本号按数值逐段比较（compareVersion）：按字符串比较时 20.1.9.1 排在 20.1.22.27795 之后。
// 目录名不是点分的纯数字（parseVersion 不认，如 backup、20.1.x）就不是版本目录，跳过、不参与比较。
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
	var bestVer []uint64
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
		v, ok := parseVersion(name)
		if !ok {
			continue // 不是版本目录
		}
		if best == "" || compareVersion(v, bestVer) > 0 {
			best, bestName, bestVer = lc, name, v
		}
	}
	if best != "" && ver != "" && !isStubToV20(ver, bestVer) {
		log.Warn("No libcef.dll directory matching KuGou.exe version %q; falling back to the highest version %q", ver, bestName)
	}
	return best
}

// isStubToV20 报告 exeVer 是否为外层 8.x/低版本启动器壳（首段 < 10）且对应子目录版本为 20.x 及以上（首段 >= 20）。
// 酷狗 10.x 首段为 10，绝不命中此条件。
func isStubToV20(exeVer string, targetVer []uint64) bool {
	ev, ok := parseVersion(exeVer)
	return ok && len(ev) > 0 && ev[0] < 10 && len(targetVer) > 0 && targetVer[0] >= 20
}

// effectiveKuGouVersion 核准用于日志、遥测与大版本黑名单判定的酷狗版本号：
// 当 exeVer 为空时回落 libcef.dll 所在目录名；当冷态预选的外层启动器壳为低版本（首段 < 10，如 8.9.8.9）
// 且同安装目录下的 libcef.dll 目录名为合法 20.x+ 版本号时，采用该子目录真实版本号（10.x 绝不被覆盖）。
func effectiveKuGouVersion(exeVer, exePath, libcefPath string) string {
	dirVer := filepath.Base(filepath.Dir(libcefPath))
	if exeVer == "" {
		return dirVer
	}
	if underDir(libcefPath, filepath.Dir(exePath)) {
		if dv, ok := parseVersion(dirVer); ok && isStubToV20(exeVer, dv) {
			return dirVer
		}
	}
	return exeVer
}

// parseVersion 把点分的版本目录名拆成各段数值。有空段、或某段不是十进制纯数字（含正负号）时 ok 为 false。
func parseVersion(name string) (v []uint64, ok bool) {
	for _, s := range strings.Split(name, ".") {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, false
		}
		v = append(v, n)
	}
	return v, true
}

// compareVersion 逐段比较数值，a 小于、等于、大于 b 分别返回 -1、0、1；段数不同时缺的段按 0 计。
func compareVersion(a, b []uint64) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y uint64
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return cmp.Compare(x, y)
		}
	}
	return 0
}

// gateRunningFn 核验全部运行中的酷狗进程（KuGou.exe 与 HD_KuGou.exe，商店版门禁，issue #212）；测试替换。
var gateRunningFn = func() error { return gateKuGou(snapshotProcs, imagePath) }

// GateRunning 对全部运行中的酷狗进程过商店版门禁（issue #212），供上层连接 CDP 之前核验：EnsurePatched 返回之后
// 等 CDP 的那段时间里，用户启动的可能是商店版或读不到路径的实例。没有在运行的返回 nil。
func GateRunning() error { return gateRunningFn() }

// gateKuGou 在进程快照里找出全部精确命中白名单的酷狗进程（KuGou.exe 与 HD_KuGou.exe，不分大小写），逐个读映像
// 路径并过 launch.Gate：任一读不到或在 WindowsApps 下即返回该错误，没有在运行的返回 nil。killKuGou 以 taskkill /IM
// 结束全部同名进程，只核验其中一个不能授权结束其余的（例如另一用户会话里的商店版）。快照取不到时按路径未知拒绝。
// shortcut: 快照之后才退出的进程取不到路径，同样按路径未知拒绝；这一轮作罢、下一轮重来，不区分「已退出」与「无权」。
func gateKuGou(snapshot func() ([]proc, error), imagePath func(uint32) (string, error)) error {
	procs, err := snapshot()
	if err != nil {
		return i18n.Errorf("%w: take a process snapshot: %w", launch.ErrExeUnknown, err)
	}
	for _, p := range procs {
		if !isKuGouProcessName(p.Name) {
			continue
		}
		exe, _ := imagePath(p.PID) // 读不到时为空串，门禁按路径未知拒绝
		if err := launch.Gate(exe); err != nil {
			return err
		}
	}
	return nil
}

// snapshotProcsFn、killImageFn 与 killPollStep 为进程存活查询与结束进程的可替换 seam；测试替换。
var (
	snapshotProcsFn = snapshotProcs
	killImageFn     = func(name string) { _ = exec.Command("taskkill", "/F", "/IM", name).Run() }
	killPollStep    = 300 * time.Millisecond
)

// killKuGou 按 TargetProcessNames 顺序依次结束主壳 HD_KuGou.exe 与子进程 KuGou.exe，
// 并轮询 IsKuGouRunning 确认全部退出（至多 10 次 × 300ms）后再写补丁。
func killKuGou() {
	log.Info("Terminating KuGou process...")
	for _, name := range TargetProcessNames {
		killImageFn(name)
	}
	for i := 0; i < 10; i++ {
		time.Sleep(killPollStep)
		if !IsKuGouRunning() {
			return
		}
	}
	log.Warn("KuGou process did not fully exit within 3s; continuing...")
}

// IsKuGouRunning returns true if any KuGou.exe or HD_KuGou.exe process is alive.
func IsKuGouRunning() bool {
	procs, err := snapshotProcsFn()
	if err != nil {
		return false
	}
	for _, p := range procs {
		if isKuGouProcessName(p.Name) {
			return true
		}
	}
	return false
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
		emitKugouIntervention("detached_cmd_fallback", 0, 0)
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
	return &helperTimeoutError{err: i18n.Errorf("patch timed out (no result within 30s)")}
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
	return &helperTimeoutError{err: i18n.Errorf("elevated helper not ready within 30s (started but unresponsive)")}
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

// errHelperExited 返回提权 helper 在结束酷狗前已退出的错误（与 readyToKill 同构）。
func errHelperExited() error {
	return i18n.Errorf("elevated patch helper exited before KuGou was stopped; KuGou was left running and the repair will be retried")
}

// readyToKill 根据等酷狗的结论决定能否结束酷狗去修补，返回 nil 才可以。酷狗出现之后再查一次 helper：
// awaitKuGou 看过 helper 之后还要查进程快照才看到酷狗，helper 可能在其间退出；不在就不结束酷狗，
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
	return errHelperExited()
}

// checkReadyToKill 在执行不可逆终止酷狗之前非阻塞复查停止信号与提权 helper 存活：
// stopCh 已关闭返回停止错误，helper 已退出返回 errHelperExited()，否则返回 nil（issue #141）。
func checkReadyToKill(stopCh <-chan struct{}, helperAlive func() bool) error {
	select {
	case <-stopCh:
		return i18n.Errorf("stopped")
	default:
	}
	if helperAlive != nil && !helperAlive() {
		return errHelperExited()
	}
	return nil
}

const (
	// runningReadyStep 与 runningReadyMaxWait 是冷态提权 helper 等待运行态交接及 CEF 子进程就绪的轮询节奏与上限。
	runningReadyStep    = 250 * time.Millisecond
	runningReadyMaxWait = 15 * time.Second
)

// waitUntilRunningReady 在已持有提权 helper 且看到酷狗出现后，有界等待启动器交接与 CEF 子进程就绪：
// 瞬态错误（ErrRunningNotReady、ErrConfirmNotReady）期间保活 helper、不发 CANCEL、不杀进程；
// 每次等待前以及 locate 返回成功前均复查 stopCh 与 helperAlive，严守「结束酷狗之前再查一次 helper」（#141）。
// 遇到非瞬态错误或超过 maxWait，原样返回最后一次错误交 killAndPatch 包装 ErrInstallChanged。
func waitUntilRunningReady(stopCh <-chan struct{}, helperAlive func() bool, step, maxWait time.Duration, locate func() (string, string, error)) (string, string, error) {
	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-stopCh:
			return "", "", i18n.Errorf("stopped")
		default:
		}
		exe, lc, err := locate()
		if err == nil {
			if err := checkReadyToKill(stopCh, helperAlive); err != nil {
				return "", "", err
			}
			return exe, lc, nil
		}
		if !errors.Is(err, ErrRunningNotReady) && !errors.Is(err, ErrConfirmNotReady) {
			return "", "", err
		}
		if helperAlive != nil && !helperAlive() {
			return "", "", errHelperExited()
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return "", "", err
		}
		wait := step
		if wait > rem {
			wait = rem
		}
		timer := time.NewTimer(wait)
		select {
		case <-stopCh:
			timer.Stop()
			return "", "", i18n.Errorf("stopped")
		case <-timer.C:
		}
	}
}

// isRegularFileFn 报告 path 是否存在且为普通文件；测试替换。
var isRegularFileFn = func(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// resolveLaunchExe 决定修补完成后重启酷狗的入口并做前置门禁与存在性校验：
// 非 HD_KuGou.exe 过 launch.Gate 后原样返回；HD_KuGou.exe 要求同目录存在普通文件 KuGou.exe 且过 launch.Gate，
// 缺失时绝不回退直启 HD_KuGou.exe，返回包装 ErrInstallNotFound 的错误。
func resolveLaunchExe(exePath string, isRegular func(string) bool) (string, error) {
	if !strings.EqualFold(filepath.Base(exePath), HDTargetProcessName) {
		if err := launch.Gate(exePath); err != nil {
			return "", err
		}
		return exePath, nil
	}
	launcher := filepath.Join(filepath.Dir(exePath), TargetProcessName)
	if err := launch.Gate(launcher); err != nil {
		return "", err
	}
	if !isRegular(launcher) {
		return "", i18n.Errorf("%w: KuGou launcher %s not found for restarting %s", ErrInstallNotFound, launcher, exePath)
	}
	return launcher, nil
}

// killAndPatch 在等到酷狗启动、readyToKill 通过之后调用：先经 locate（生产为经 waitUntilRunningReady 包装的
// locateRunningInstall，只按运行中的进程定位）重新定位安装，与修补前预选的 exePath、libcefPath 比较（exe 允许
// 同安装目录在 KuGou.exe 与 HD_KuGou.exe 之间切换），都匹配、过商店版门禁、确认重启入口可用且所有 preKill
// 复查通过后才 kill 结束酷狗、trigger 触发 helper 写入。不同或重新定位出错时两者都不调用，返回包装
// ErrInstallChanged 的错误，由调用方关闭 helper（写 CANCEL，helper 不写 DLL）、稍后重试。
//
// kill 之前再对全部运行中的酷狗进程过一次商店版门禁（issue #212，gateRunningFn），对运行态 exe 做启动器预检
// （resolveLaunchExe），最后执行可选 preKill 复查（生产经 checkReadyToKill 复查 stopCh 与 helper.running，
// 封堵最终预检期间的退出窗口）：任一不过时原样返回其错误，不结束、不触发。
func killAndPatch(exePath, libcefPath string, locate func() (string, string, error), kill func(), trigger func() error, preKill ...func() error) error {
	exe, lc, err := locate()
	if err != nil {
		// 内层错误按 %v 带出、不按 %w：不得被认作 ErrInstallNotFound，否则上层按找不到安装分流
		return i18n.Errorf("%w: locating KuGou again after it started failed: %v", ErrInstallChanged, err)
	}
	if !sameKuGouExe(exe, exePath) || !samePath(lc, libcefPath) {
		return i18n.Errorf("%w: started %s with %s; prepared %s with %s", ErrInstallChanged, exe, lc, exePath, libcefPath)
	}
	if err := gateRunningFn(); err != nil {
		return err
	}
	if _, err := resolveLaunchExe(exe, isRegularFileFn); err != nil {
		return err
	}
	for _, check := range preKill {
		if check == nil {
			continue
		}
		if err := check(); err != nil {
			return err
		}
	}
	log.Info("KuGou detected; stopping it and repairing the DLL...")
	kill()
	if err := trigger(); err != nil {
		return i18n.Errorf("DLL patch failed: %w", err)
	}
	return nil
}

// awaitPatchedKuGou 是 DLL 已修补时的收尾：wait 等到酷狗启动之后（生产为 waitForKuGou(stopCh, nil)），对全部运行中的
// 酷狗进程再过一次商店版门禁（issue #212），门禁的错误原样返回。EnsurePatched 开头核验时酷狗可能还没运行（门禁放行），
// 等来的可能是商店版或读不到路径的实例。
func awaitPatchedKuGou(wait func() kugouWait) error {
	if wait() == kugouWaitStopped {
		return i18n.Errorf("stopped")
	}
	return gateRunningFn()
}

// samePath 报告两个路径 Clean 后是否相同（不分大小写）。
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// sameKuGouExe 报告 a 与 b 是否指向同一酷狗安装目录的可执行文件：路径相同，或位于同一目录且基名均在酷狗精确白名单内
// （允许冷态预选根目录 KuGou.exe 后，运行态交接落定为同目录 HD_KuGou.exe）。
func sameKuGouExe(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	return samePath(filepath.Dir(a), filepath.Dir(b)) &&
		isKuGouProcessName(filepath.Base(a)) &&
		isKuGouProcessName(filepath.Base(b))
}

// EnsurePatched is the main entry point called by the player before connecting to CDP.
//
// New workflow (like NetEase Cloud Music):
//  1. CDP already available → return immediately (fast path)
//  2. Locate installation, log version
//  3. DLL already patched → wait for KuGou process to appear, then return
//  4. DLL not patched →
//     a. Precheck restart launcher, then UAC UPFRONT: launch elevated helper that waits for a trigger
//     b. Wait for KuGou to appear in process list (user starts KuGou)
//     c. Wait for launcher handoff/CEF ready (keeping helper alive), kill KuGou, trigger helper to patch (no second UAC)；
//     helper 已退出时不结束酷狗，返回错误交上层重试；启动的不是预选的那份安装时同样不结束酷狗，返回 ErrInstallChanged（killAndPatch）
//     d. Verify, re-launch KuGou via resolved launcher
//
// 商店版门禁（issue #212）在开头，先于「CDP 已可用」的早退与一切提权、结束、修补、拉起：全部运行中的酷狗进程
// 逐个核验，选定安装之后再核验预选的 exe（酷狗不在运行时它取自注册表或常见安装根，修补、拉起的都是它）；DLL 已修补、
// 只等酷狗启动时，等到之后再核验一次（awaitPatchedKuGou），要修补时 killAndPatch 在结束之前再核验一次。门禁不过时原样
// 返回它的错误。
func EnsurePatched(stopCh <-chan struct{}) error { return ensurePatched(stopCh, true) }

// EnsurePatchedNoElevate 与 EnsurePatched 相同，但走到需要提权的分支时返回 ErrElevationNeeded，不调用 launchElevatedHelper。
// 上层放弃提权之后一律以此调用（设计 doc/design/converge-player-behavior-231-design.md §5.3）：DLL 已修补或 CDP 已可用时
// 照常返回 nil，这两种情形都不需要提权。
func EnsurePatchedNoElevate(stopCh <-chan struct{}) error { return ensurePatched(stopCh, false) }

// isCDPAvailableFn 与 checkPatchStatusFn 为 ensurePatched 的离线测试 seam；生产恒为 isCDPAvailable 与 checkPatchStatus。
var (
	isCDPAvailableFn   = isCDPAvailable
	checkPatchStatusFn = checkPatchStatus
)

// installLogged 与 fingerprintWarned 是 ensurePatched 的日志去重：上层在修补受阻期间每 30 秒重走一次判定（kugou.go 的
// storeRecheck），安装目录与版本（键为 KuGou.exe 路径与版本）、指纹不认识的警告（键为 libcef.dll 路径与版本）各只在键与上一次
// 打过的不同时才打。只由 ensurePatched 读写，ensurePatched 只在 kugou 的 Start 所在的 goroutine 上调用。
var installLogged, fingerprintWarned string

// ensurePatched 是 EnsurePatched 与 EnsurePatchedNoElevate 的实体，allowElevate 为假时不弹 UAC。
func ensurePatched(stopCh <-chan struct{}, allowElevate bool) error {
	if err := gateRunningFn(); err != nil {
		return err
	}
	if isCDPAvailableFn() {
		log.Detail("CDP already available; skipping patch check")
		return nil
	}

	exePath, libcefPath, err := FindKuGouInstall()
	if err != nil {
		return err
	}
	if err := launch.Gate(exePath); err != nil {
		return err
	}

	// 版本号不作**正向**判据——「能不能打」由 libcef.dll 的字节指纹决定（见
	// CheckPatchStatus）。KuGou.exe 的版本号与它 vendored 的 CEF 版本相互独立，拿前者当
	// 正向闸门既会误伤（同 CEF 的点版本被拒）又会漏放（CEF 热修但 exe 版本不变）。
	ver := effectiveKuGouVersion(readExeVersion(exePath), exePath, libcefPath)
	recordInstallMeta(exePath, ver)
	if key := exePath + "|" + ver; key != installLogged {
		installLogged = key
		log.Info("KuGou install directory: %s", filepath.Dir(exePath))
		if ver == knownVersion {
			log.Info("KuGou version: %s (recommended)", ver)
		} else {
			log.Info("KuGou version: %s (recommended version is %s; the libcef.dll byte fingerprint is authoritative)", ver, knownVersion)
		}
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
		return &VersionBlockError{
			Version: player.SanitizeVersion(ver),
			Err:     i18n.Errorf("%w: KuGou %s is in the 10.x series; patches target only %s (different CEF baseline)", ErrVersionUnsupported, ver, knownVersion),
		}
	}

	fingerprint := libcefPath + "|" + ver
	allPatched, canAutoFix, err := checkPatchStatusFn(libcefPath, fingerprint != fingerprintWarned)
	if err != nil {
		return i18n.Errorf("check patch status: %w", err)
	}
	if !canAutoFix {
		// 9 处偏移里有字节既非原版也非已 patch：这个 DLL 不是我们逆向针对的那个 CEF。
		// 拒绝盲打——拿错版本的偏移写 93MB 签名 DLL 会让用户被迫重装酷狗。上层按受阻处理、定时重查。
		fingerprintWarned = fingerprint
		mismatchIdx := int(lastLibcefMismatchIdx.Swap(0))

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
		return &VersionBlockError{
			Version:          player.SanitizeVersion(ver),
			MismatchIdx:      mismatchIdx,
			IsLibcefMismatch: true,
			Err:              i18n.Errorf("%w: libcef.dll of %s does not match known offsets; refusing blind patch (reverse-engineered against %s)", ErrVersionUnsupported, ver, knownVersion),
		}
	}

	if allPatched {
		// DLL 已 patch，等待酷狗启动（仿网易云逻辑，不自动启动）
		return awaitPatchedKuGou(func() kugouWait { return waitForKuGou(stopCh, nil) })
	}

	// DLL 未 patch：先确认重启启动器可用，再弹 UAC 获取提权（缺失时不弹 UAC、不结束酷狗）。
	launchTarget, err := resolveLaunchExe(exePath, isRegularFileFn)
	if err != nil {
		return err
	}
	if !allowElevate {
		return ErrElevationNeeded
	}
	log.Info("libcef.dll needs repair; requesting administrator rights (UAC)...")
	patchStart := time.Now()
	attempt := int(uacPatchAttempt.Add(1))
	helper, err := launchElevatedHelper(libcefPath)
	if err != nil {
		outcome := "uac_denied"
		var hte *helperTimeoutError
		if errors.As(err, &hte) {
			outcome = "helper_timeout"
		}
		emitKugouIntervention(outcome, attempt, time.Since(patchStart).Milliseconds())
		return i18n.Errorf("%w (user may have denied UAC): %v", ErrElevationFailed, err)
	}
	defer helper.Close()

	log.Info("Administrator rights obtained; waiting for KuGou to start to complete the repair...")
	if err := readyToKill(waitForKuGou(stopCh, helper.running), helper.running); err != nil {
		return err
	}

	// 检测到酷狗、helper 仍在：等待启动器交接与 CEF 子进程就绪（保活 helper），确认启动的仍是预选的那份安装，再停止酷狗并触发 patch（无需再次 UAC）
	var startedExe string
	locate := func() (string, string, error) {
		exe, lc, err := waitUntilRunningReady(stopCh, helper.running, runningReadyStep, runningReadyMaxWait, locateRunningInstall)
		if err == nil {
			startedExe = exe
		}
		return exe, lc, err
	}
	if err := killAndPatch(exePath, libcefPath, locate, killKuGou, helper.TriggerAndWait, func() error {
		return checkReadyToKill(stopCh, helper.running)
	}); err != nil {
		var hte *helperTimeoutError
		if errors.As(err, &hte) {
			emitKugouIntervention("helper_timeout", attempt, time.Since(patchStart).Milliseconds())
		}
		return err
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
		emitKugouIntervention("patch_reverted_by_av", attempt, time.Since(patchStart).Milliseconds())
		telemetry.ReportOnce("kugou.patch_reverted",
			"酷狗补丁写入后字节未变更（疑似被杀软静默回滚）",
			map[string]string{"kugou.version": ver},
			map[string]any{"libcef_path": libcefPath})
		return i18n.Errorf("post-patch write verification failed: bytes unchanged (check your antivirus software)")
	}

	uacPatchAttempt.Store(0)
	emitKugouIntervention("ok", attempt, time.Since(patchStart).Milliseconds())
	log.Success("libcef.dll patch complete. Restarting KuGou...")
	if startedExe != "" {
		recordInstallMeta(startedExe, ver)
		resolved, rerr := resolveLaunchExe(startedExe, isRegularFileFn)
		if rerr != nil {
			log.Warn("Failed to start KuGou: %v (please start it manually)", rerr)
			return nil
		}
		launchTarget = resolved
	}
	if err := launchKuGou(launchTarget); err != nil {
		log.Warn("Failed to start KuGou: %v (please start it manually)", err)
	}
	return nil
}
