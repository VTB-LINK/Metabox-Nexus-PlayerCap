// Package park 把网易云主窗口移到屏幕外（仍保持 shown、未遮挡）以维持渲染出帧（off-screen 保活），
// 并能原样还原。窗口被移到「虚拟桌面包围盒之外」，与具体显示器分辨率/DPI 缩放无关，多屏安全。
//
// 崩溃兜底：泊车时把原始窗口位置落盘；playercap 启动时若发现遗留泊车状态 → 立即还原（救回上次崩溃）。
package park

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"Metabox-Nexus-PlayerCap/logger"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows"
)

var log = logger.New("CloudMusic] [Park") // 渲染为 [CloudMusic] [Park]

const targetProcessName = "cloudmusic.exe"

var (
	user32                     = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows            = user32.NewProc("EnumWindows")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procGetWindowTextW         = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW   = user32.NewProc("GetWindowTextLengthW")
	procGetWindowRect          = user32.NewProc("GetWindowRect")
	procGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	procSetWindowPos           = user32.NewProc("SetWindowPos")
	procIsIconic               = user32.NewProc("IsIconic")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	procIsWindow               = user32.NewProc("IsWindow")
)

// cachedHwnd 缓存主窗句柄，避免每次轮询都做进程枚举（IsWindow 校验，失效才重找）。
// 用独立的 cacheMu 保护：mainWindow 会在持有 mu 的 Park/Unpark 内被调用，故不能复用 mu（非可重入）。
var (
	cachedHwnd uintptr
	cacheMu    sync.Mutex
)

func isWindowValid(h uintptr) bool {
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(h)
	return r != 0
}

// mainWindow 返回主窗句柄（缓存优先，失效则重新枚举查找）。
func mainWindow() uintptr {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cachedHwnd != 0 && isWindowValid(cachedHwnd) {
		return cachedHwnd
	}
	cachedHwnd = findMainAny()
	return cachedHwnd
}

const (
	swShowNormal      = 1
	swShowMinimized   = 2
	swShowNoActivate  = 4
	smXVirtualScreen  = 76
	smCXVirtualScreen = 78
	swpNoActivate     = 0x0010
	swpNoZOrder       = 0x0004
	swpNoSize         = 0x0001
)

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }

// windowPlacement 对应 Win32 WINDOWPLACEMENT
type windowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    point
	PtMaxPosition    point
	RcNormalPosition rect
}

// parkState 落盘的泊车状态（供崩溃后还原）
type parkState struct {
	Parked    bool            `json:"parked"`
	Placement windowPlacement `json:"placement"`
}

var (
	mu             sync.Mutex      // 保护 parkedMem / savedPlacement
	parkedMem      bool            // 内存态泊车标志（IsParked 快速返回，避免高频读盘）
	savedPlacement windowPlacement // 泊车前的原始窗口位置（供 Unpark 还原）
)

func stateFilePath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(os.TempDir(), "mbx-cloudmusic-park.json")
	}
	return filepath.Join(filepath.Dir(exe), "mbx-cloudmusic-park.json")
}

func writeState(s *parkState) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	os.WriteFile(stateFilePath(), b, 0644)
}

func readState() *parkState {
	b, err := os.ReadFile(stateFilePath())
	if err != nil {
		return nil
	}
	var s parkState
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func clearState() { os.Remove(stateFilePath()) }

// cloudmusicPIDs 返回所有 cloudmusic.exe 进程 PID 集合
func cloudmusicPIDs() map[uint32]bool {
	out := map[uint32]bool{}
	procs, err := process.Processes()
	if err != nil {
		return out
	}
	for _, p := range procs {
		if name, err := p.Name(); err == nil && name == targetProcessName {
			out[uint32(p.Pid)] = true
		}
	}
	return out
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

func windowTitle(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// findMainAny 按 WINDOWPLACEMENT.rcNormalPosition（还原态尺寸，最小化/隐藏时仍有效）找面积最大的
// cloudmusic 窗口 = 主窗。用还原态尺寸而非当前 rect：最小化时当前 rect 是 (-32000)，只有还原态尺寸
// 才能正确识别主窗（~1058×752 最大），并避免误选迷你播放器/桌面歌词/窗口边框等小窗口。
func findMainAny() uintptr {
	pids := cloudmusicPIDs()
	if len(pids) == 0 {
		return 0
	}
	var found uintptr
	var maxArea int64
	cb := windows.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		if !pids[windowPID(hwnd)] {
			return 1
		}
		wp, ok := getPlacement(hwnd)
		if !ok {
			return 1
		}
		r := wp.RcNormalPosition
		area := int64(r.Right-r.Left) * int64(r.Bottom-r.Top)
		if area > maxArea {
			maxArea = area
			found = hwnd
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return found
}

// IsMainMinimized 主窗是否最小化（iconic）。
func IsMainMinimized() bool {
	h := mainWindow()
	if h == 0 {
		return false
	}
	r, _, _ := procIsIconic.Call(h)
	return r != 0
}

// ForegroundIsOtherApp 当前前台窗口是否属于「另一个真实应用」（非网易云、非资源管理器/shell）。
// 用于区分最小化方式：按钮最小化焦点立刻切到下一个 app；任务栏最小化焦点先到 shell(explorer)。
func ForegroundIsOtherApp() bool {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return false
	}
	pid := windowPID(fg)
	if pid == 0 {
		return false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	name, err := p.Name()
	if err != nil {
		return false
	}
	ln := strings.ToLower(name)
	return ln != targetProcessName && ln != "explorer.exe"
}

// IsWindows11 报告当前系统是否为 Windows 11（major 10 且 build ≥ 22000，或 major > 10）。
// 用于禁用 park 策略：Win11(尤其 24H2) 的 DWM 会停止合成「完全不可见/被遮挡」的 Chromium 窗口，
// 屏外泊车窗口随即停止渲染 → 保活失效（实测一泊车就冻结、永不出新帧）。Win11 下应改走 fadeout。
func IsWindows11() bool {
	v := windows.RtlGetVersion()
	if v == nil {
		return false
	}
	return v.MajorVersion > 10 || (v.MajorVersion == 10 && v.BuildNumber >= 22000)
}

// IsMainForeground 主窗是否为前台窗口（用户点任务栏/Alt-Tab 切到网易云时为真，用于自动 unpark）。
func IsMainForeground() bool {
	h := mainWindow()
	if h == 0 {
		return false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	return fg == h
}

func getRect(hwnd uintptr) rect {
	var r rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// DebugList 返回所有属于 cloudmusic 进程的顶层窗口信息（调试用）。
func DebugList() []string {
	pids := cloudmusicPIDs()
	var out []string
	cb := windows.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		if pids[windowPID(hwnd)] {
			r := getRect(hwnd)
			vis := isVisible(hwnd)
			out = append(out, fmt.Sprintf("hwnd=%d vis=%v title=%q rect=(%d,%d,%d,%d) w=%d h=%d",
				hwnd, vis, windowTitle(hwnd), r.Left, r.Top, r.Right, r.Bottom, r.Right-r.Left, r.Bottom-r.Top))
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out
}

func getPlacement(hwnd uintptr) (windowPlacement, bool) {
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	r, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
	return wp, r != 0
}

func setPlacement(hwnd uintptr, wp *windowPlacement) {
	wp.Length = uint32(unsafe.Sizeof(*wp))
	procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(wp)))
}

func systemMetric(idx int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(idx))
	return int32(r)
}

// offscreenX 返回虚拟桌面包围盒之外的 X 坐标（所有显示器之外）
func offscreenX() int32 {
	left := systemMetric(smXVirtualScreen)
	width := systemMetric(smCXVirtualScreen)
	x := left + width + 200
	if x < 30000 {
		x = 30000 // 安全下限：确保远离任何显示器
	}
	return x
}

// IsParked 是否已泊车（按落盘状态）
// IsParked 是否已泊车（内存状态，不读磁盘——windowStateLoop 高频调用）。
func IsParked() bool {
	mu.Lock()
	defer mu.Unlock()
	return parkedMem
}

// Park 把网易云主窗口移到屏幕外。原始位置存内存(供还原)+落盘(供崩溃恢复)。已泊车则忽略。
func Park() bool {
	mu.Lock()
	defer mu.Unlock()
	if parkedMem {
		return true
	}
	hwnd := mainWindow()
	if hwnd == 0 {
		log.Warn("未找到网易云主窗口，无法泊车")
		return false
	}
	wp, ok := getPlacement(hwnd)
	if !ok {
		log.Warn("读取窗口位置失败，无法泊车")
		return false
	}
	parkedMem = true
	savedPlacement = wp
	writeState(&parkState{Parked: true, Placement: wp}) // 落盘供崩溃恢复
	// 用 SetWindowPlacement 一步完成：取消最小化 + 移到屏外 + 不激活。
	// rcNormalPosition 是还原态尺寸，据此构造同尺寸的屏外位置。
	w := wp.RcNormalPosition.Right - wp.RcNormalPosition.Left
	h := wp.RcNormalPosition.Bottom - wp.RcNormalPosition.Top
	off := wp
	off.Flags = 0
	off.ShowCmd = swShowNoActivate
	ox := offscreenX()
	off.RcNormalPosition = rect{Left: ox, Top: 100, Right: ox + w, Bottom: 100 + h}
	setPlacement(hwnd, &off) // 取消最小化 + 不激活（但 SetWindowPlacement 会把位置钳到可见工作区）
	// SetWindowPos 不钳制，真正移到屏外（SetWindowPlacement 对正常态窗口会夹到屏幕边缘）
	procSetWindowPos.Call(hwnd, 0, uintptr(ox), 100, 0, 0, swpNoActivate|swpNoZOrder|swpNoSize)
	log.Success("网易云已泊车至屏幕外（不抢焦点）")
	return true
}

// Unpark 把窗口还原到泊车前的位置并清除状态。
func Unpark() bool {
	mu.Lock()
	defer mu.Unlock()
	if !parkedMem {
		return false
	}
	hwnd := mainWindow()
	wp := savedPlacement
	parkedMem = false
	clearState()
	if hwnd == 0 {
		return false // 窗口没了，状态已清
	}
	if wp.ShowCmd == swShowMinimized {
		wp.ShowCmd = swShowNormal // 若泊车时是从最小化态存的，还原成正常显示而非再最小化
	}
	setPlacement(hwnd, &wp)
	log.Success("网易云窗口已还原")
	return true
}

// RestoreOrphaned 在 playercap 启动时调用：若发现遗留泊车状态（上次崩溃时仍在泊车），立即还原窗口。
func RestoreOrphaned() {
	s := readState()
	if s == nil || !s.Parked {
		return
	}
	log.Warn("检测到遗留泊车状态（上次异常退出），正在还原网易云窗口...")
	mu.Lock()
	parkedMem = true
	savedPlacement = s.Placement // 采纳崩溃前落盘的原始位置
	mu.Unlock()
	Unpark()
}
