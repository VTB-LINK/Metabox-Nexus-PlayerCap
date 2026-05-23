package watchdog

import (
	"bytes"
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

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var log = logger.New("KuGouWatchdog")

const (
	targetProcess = "KuGou.exe"
	cdpPort       = 12233

	// knownVersion is the KuGou.exe FileVersion the patches were reverse-engineered against.
	// Patches may work on other versions but are not guaranteed.
	knownVersion = "20.1.22.27795"
)

// patchEntry describes one patch at a disk offset in libcef.dll.
type patchEntry struct {
	offset int64
	data   []byte
}

// libcefPatches holds all 9 patches for libcef.dll (version 20.1.22.27795).
// Applies in-order; idempotent (re-applying already-patched bytes is harmless).
var libcefPatches = []patchEntry{
	{0x58C63EB, []byte{0xC7, 0x06, 0xC9, 0x2F, 0x00, 0x00}},       // Patch1: force port=12233 init
	{0x58C6415, []byte{0x90, 0x90, 0x90, 0x90, 0x90}},             // Patch4: NOP CALL sub_1827619B0
	{0x58C6420, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},       // Patch2: NOP ja (port range check)
	{0x58C6428, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},       // Patch3: NOP jz (parse success check)
	{0x4BC180E, []byte{0xBA, 0xC9, 0x2F, 0x00, 0x00, 0x90}},       // Patch5: child cmdline port=12233
	{0x4BEDE41, []byte{0x90, 0x90, 0x90, 0x90, 0x90, 0x90}},       // Patch6: NOP jz (DevTools startup skip)
	{0x4BEDE4C, []byte{0x41, 0xC7, 0x07, 0xC9, 0x2F, 0x00, 0x00}}, // Patch7: force port=12233 in DevTools fn
	{0x4BEDEB5, []byte{0x90, 0x90, 0x90, 0x90, 0x90}},             // Patch8: NOP parse CALL
	{0x4BEDED0, []byte{0x90, 0x90, 0x90}},                         // Patch9: NOP cmovz
}

// versionSentinel contains the expected bytes at Patch1's offset in the UNPATCHED DLL.
// The value 0xAAAAAAAA is CEF's hard-coded sentinel for "port not set".
// Presence of this value at offset 0x58C63EB confirms version compatibility.
var (
	p1Offset  int64 = 0x58C63EB
	p1Orig          = []byte{0xC7, 0x06, 0xAA, 0xAA, 0xAA, 0xAA}
	p1Patched       = []byte{0xC7, 0x06, 0xC9, 0x2F, 0x00, 0x00}
)

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

// CheckPatchStatus reads libcef.dll and returns whether all patches are applied
// and whether this DLL version is supported for auto-patching.
//
// Returns (allPatched, canAutoFix, err).
// canAutoFix is false when the DLL version doesn't match known offsets.
func CheckPatchStatus(libcefPath string) (allPatched bool, canAutoFix bool, err error) {
	f, err := os.Open(libcefPath)
	if err != nil {
		return false, false, fmt.Errorf("open libcef.dll: %w", err)
	}
	defer f.Close()

	// --- Version check via Patch1 sentinel ---
	if _, err := f.Seek(p1Offset, io.SeekStart); err != nil {
		return false, false, fmt.Errorf("seek Patch1: %w", err)
	}
	buf := make([]byte, len(p1Orig))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false, false, fmt.Errorf("read Patch1: %w", err)
	}

	switch {
	case bytes.Equal(buf, p1Patched):
		// Patch1 already applied – version confirmed, fall through to check the rest
		canAutoFix = true
	case bytes.Equal(buf, p1Orig):
		// Original sentinel present – version confirmed, but needs patching
		return false, true, nil
	default:
		// Unknown bytes: different CEF version, cannot safely patch
		log.Warn("libcef.dll Patch1 字节不匹配（版本不支持自动 patch）：%X", buf)
		return false, false, nil
	}

	// --- Check each remaining patch ---
	for i, p := range libcefPatches {
		if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
			log.Warn("Patch%d seek 失败: %v", i+1, err)
			return false, true, nil
		}
		rb := make([]byte, len(p.data))
		if _, err := io.ReadFull(f, rb); err != nil {
			log.Warn("Patch%d read 失败: %v", i+1, err)
			return false, true, nil
		}
		if !bytes.Equal(rb, p.data) {
			log.Detail("Patch%d 未应用（offset=0x%X）", i+1, p.offset)
			return false, true, nil
		}
	}
	return true, true, nil
}

// FindKuGouInstall locates the KuGou executable and libcef.dll.
// Reads installation paths from the registry first, then falls back to
// scanning common Program Files locations.
func FindKuGouInstall() (exePath, libcefPath string, err error) {
	key, kerr := registry.OpenKey(registry.CURRENT_USER, `Software\KuGou`, registry.QUERY_VALUE)
	if kerr == nil {
		defer key.Close()

		// KuGou8 points to the versioned directory (e.g. …\20.1.22.27795)
		if vDir, _, e := key.GetStringValue("KuGou8"); e == nil && vDir != "" {
			lc := filepath.Join(vDir, "libcef.dll")
			if _, se := os.Stat(lc); se == nil {
				libcefPath = lc
			}
		}
		// AppPath points to the launcher directory
		if appPath, _, e := key.GetStringValue("AppPath"); e == nil && appPath != "" {
			exe := filepath.Join(appPath, "KuGou.exe")
			if _, se := os.Stat(exe); se == nil {
				exePath = exe
			}
		}
	}

	// Fallback: scan well-known install roots
	if exePath == "" || libcefPath == "" {
		for _, base := range []string{
			`C:\Program Files\KuGou\KGMusic`,
			`C:\Program Files (x86)\KuGou\KGMusic`,
		} {
			exe := filepath.Join(base, "KuGou.exe")
			if _, e := os.Stat(exe); e != nil {
				continue
			}
			// Look for a versioned subdirectory containing libcef.dll
			entries, e := os.ReadDir(base)
			if e != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				lc := filepath.Join(base, entry.Name(), "libcef.dll")
				if _, e2 := os.Stat(lc); e2 == nil {
					if exePath == "" {
						exePath = exe
					}
					if libcefPath == "" {
						libcefPath = lc
					}
					break
				}
			}
			if exePath != "" && libcefPath != "" {
				break
			}
		}
	}

	if exePath == "" || libcefPath == "" {
		return "", "", fmt.Errorf("未找到酷狗安装（KuGou.exe 或 libcef.dll 不存在）")
	}
	return exePath, libcefPath, nil
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
	fi := (*vsFixedFileInfo)(unsafe.Pointer(pInfo))
	return fmt.Sprintf("%d.%d.%d.%d",
		fi.FileVersionMS>>16, fi.FileVersionMS&0xFFFF,
		fi.FileVersionLS>>16, fi.FileVersionLS&0xFFFF,
	)
}

// RunPatch is called when the process is re-launched elevated with --kugou-patch.
// Reads the DLL path from a temp input file, applies all patches by seeking
// to exact offsets and writing only the changed bytes (no full DLL read/write),
// then writes "OK" or "ERROR: ..." to the result file.
func RunPatch() {
	inputFile := filepath.Join(os.TempDir(), "kugou_cdp_patch_input.txt")
	resultFile := filepath.Join(os.TempDir(), "kugou_cdp_patch_result.txt")

	data, err := os.ReadFile(inputFile)
	if err != nil {
		writePatchResult(resultFile, "ERROR: 无法读取参数文件: "+err.Error())
		os.Exit(1)
	}
	libcefPath := strings.TrimSpace(string(data))

	if err := patchDLLBytes(libcefPath); err != nil {
		writePatchResult(resultFile, "ERROR: "+err.Error())
		os.Exit(1)
	}
	writePatchResult(resultFile, "OK")
	os.Exit(0)
}

func writePatchResult(path, content string) {
	_ = os.WriteFile(path, []byte(content), 0600)
}

// patchDLLBytes opens the DLL for writing and seeks to each patch offset,
// writing only the changed bytes. Does NOT read/write the entire file.
func patchDLLBytes(libcefPath string) error {
	f, err := os.OpenFile(libcefPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开 libcef.dll: %w", err)
	}
	defer f.Close()

	for i, p := range libcefPatches {
		if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
			return fmt.Errorf("Patch%d seek: %w", i+1, err)
		}
		if _, err := f.Write(p.data); err != nil {
			return fmt.Errorf("Patch%d write (offset=0x%X): %w", i+1, p.offset, err)
		}
	}
	return nil
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
}

// RunPatchHelper is called when the process is re-launched elevated with
// --kugou-patch-helper. It signals READY, waits for a trigger, patches the
// DLL in-place, and writes the result file before exiting.
func RunPatchHelper() {
	tmpDir := os.TempDir()
	inputFile := filepath.Join(tmpDir, "kugou_cdp_patch_input.txt")
	readyFile := filepath.Join(tmpDir, "kugou_cdp_patch_ready.txt")
	trigFile := filepath.Join(tmpDir, "kugou_cdp_patch_trigger.txt")
	resFile := filepath.Join(tmpDir, "kugou_cdp_patch_result.txt")

	data, err := os.ReadFile(inputFile)
	if err != nil {
		writePatchResult(resFile, "ERROR: 无法读取参数文件: "+err.Error())
		os.Exit(1)
	}
	libcefPath := strings.TrimSpace(string(data))

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

	if err := patchDLLBytes(libcefPath); err != nil {
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
	inputFile := filepath.Join(tmpDir, "kugou_cdp_patch_input.txt")
	readyFile := filepath.Join(tmpDir, "kugou_cdp_patch_ready.txt")
	trigFile := filepath.Join(tmpDir, "kugou_cdp_patch_trigger.txt")
	resFile := filepath.Join(tmpDir, "kugou_cdp_patch_result.txt")

	os.Remove(readyFile) //nolint
	os.Remove(trigFile)  //nolint
	os.Remove(resFile)   //nolint

	if err := os.WriteFile(inputFile, []byte(libcefPath), 0600); err != nil {
		return nil, fmt.Errorf("写入参数文件失败: %w", err)
	}

	verbW, _ := windows.UTF16PtrFromString("runas")
	fileW, _ := windows.UTF16PtrFromString(exe)
	paramsW, _ := windows.UTF16PtrFromString("--kugou-patch-helper")
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

	ver := readExeVersion(exePath)
	if ver == "" {
		ver = filepath.Base(filepath.Dir(libcefPath))
	}
	log.Info("酷狗版本: %s", ver)
	if strings.HasPrefix(ver, "10.") {
		log.Warn("检测到酷狗版本 %s（10.x 系列），该版本不受支持，跳过 patch", ver)
		return fmt.Errorf("酷狗版本 %s 不受支持（仅支持 %s）", ver, knownVersion)
	}
	if ver == knownVersion {
		log.Detail("版本已验证（推荐版本 %s）", knownVersion)
	} else {
		log.Warn("当前版本 %s 未经验证，推荐版本为 %s", ver, knownVersion)
		log.Warn("patch 偏移仅针对推荐版本，DLL 可能被损坏；如遇问题请降级至推荐版本后重试")
	}

	allPatched, _, err := CheckPatchStatus(libcefPath)
	if err != nil {
		return fmt.Errorf("检查 patch 状态: %w", err)
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
		return fmt.Errorf("获取管理员权限失败（用户可能拒绝了 UAC）: %w", err)
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
		return fmt.Errorf("patch 写入后验证失败：字节未变更（请检查防病毒软件）")
	}

	log.Success("libcef.dll patch 完成！正在重新启动酷狗...")
	if err := launchKuGou(exePath); err != nil {
		log.Warn("启动酷狗失败: %v（请手动启动）", err)
	}
	return nil
}
