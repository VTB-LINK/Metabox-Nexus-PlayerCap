package watchdog

import (
	"os/exec"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/rum"

	"github.com/shirou/gopsutil/v3/process"
)

var log = logger.New("CloudMusic] [Watchdog") // 渲染为 [CloudMusic] [Watchdog]

const TargetProcessName = "cloudmusic.exe"
const DebugFlag = "--remote-debugging-port=9222"

// keepaliveMarker 仅我们注入时才带（网易云自身不加此开关），用于判断是否已注入全部参数。
// 注：网易云子进程自带 --disable-features=CalculateNativeWinOcclusion，不能用它当标记。
const keepaliveMarker = "--disable-backgrounding-occluded-windows"

// LaunchFlags 启动网易云时注入的全部参数：
//   - 调试端口：供 CDP 取词/截帧
//   - 防遮挡 / 防后台降帧：窗口被遮挡/最小化时仍持续渲染出帧（特效镜像保活）
var LaunchFlags = []string{
	DebugFlag,
	"--disable-backgrounding-occluded-windows",
	"--disable-renderer-backgrounding",
	"--disable-background-timer-throttling",
	"--disable-features=CalculateNativeWinOcclusion",
}

// proc 是一个运行中的 cloudmusic.exe：映像路径与命令行，读不到为空。
type proc struct {
	ExePath string
	Cmdline string
}

// 进程枚举、结束、拉起与等待经包级变量调用，测试替换它们以离线驱动 EnsureDebugMode，不碰真进程。
var (
	runningFn = launch.Running
	scanFn    = scanProcesses
	killFn    = func() { exec.Command("taskkill", "/F", "/IM", TargetProcessName).Run() }
	detachFn  = launch.Detached
	sleepFn   = time.Sleep
)

// scanProcesses 枚举全部 cloudmusic.exe：按名字在进程快照里找（launch.Running），读不到映像路径的也在结果里、
// 路径为空，由门禁拒绝；命令行按进程号读，读不到为空。
func scanProcesses() ([]proc, error) {
	running, err := runningFn(TargetProcessName)
	if err != nil {
		return nil, i18n.Errorf("failed to list processes: %w", err)
	}
	out := make([]proc, 0, len(running))
	for _, r := range running {
		c := proc{ExePath: r.ExePath}
		if p, err := process.NewProcess(int32(r.PID)); err == nil {
			if cmdline, err := p.Cmdline(); err == nil {
				c.Cmdline = cmdline
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// gateAll 对 procs 逐个过 launch.Gate，任一映像路径读不到或在 WindowsApps 下即返回该错误（issue #212）。
// taskkill /IM 按名字结束全部 cloudmusic.exe，只核验其中一个不能授权结束其余的（例如另一用户会话里的商店版）。
func gateAll(procs []proc) error {
	for _, p := range procs {
		if err := launch.Gate(p.ExePath); err != nil {
			return err
		}
	}
	return nil
}

// IsRunning 报告是否有 cloudmusic.exe 在运行；枚举失败按在运行处理（不因一次失败改报未运行）。只按名字查进程快照，
// 不读命令行、不过门禁，供外层循环每轮判一次。
func IsRunning() bool {
	procs, err := runningFn(TargetProcessName)
	return err != nil || len(procs) > 0
}

// GateRunning 枚举全部 cloudmusic.exe 并逐个过门禁（issue #212），供不经 EnsureDebugMode、独立连接 9222 或
// 操作网易云窗口的路径（特效的 shim 注入与泊车）在连接、操作之前调用。没有运行中的 cloudmusic.exe 时返回
// launch.ErrExeUnknown：没有可核验的映像路径，调试端口上的不是可确认的网易云。
func GateRunning() error {
	procs, err := scanFn()
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		return launch.ErrExeUnknown
	}
	return gateAll(procs)
}

var (
	lastVerMu   sync.Mutex
	lastVerExe  string
	lastVerText string
)

func cacheVersionFromExe(exePath string) {
	if exePath == "" {
		return
	}
	lastVerMu.Lock()
	same := lastVerExe == exePath && lastVerText != ""
	lastVerMu.Unlock()
	if same {
		return
	}
	v := player.SanitizeVersion(ReadExeVersion(exePath))
	if v == "" {
		return
	}
	lastVerMu.Lock()
	lastVerExe = exePath
	lastVerText = v
	lastVerMu.Unlock()
}

// LastVersion 返回最近一次检测到的网易云清洗后版本号（只读，不含路径）。
func LastVersion() string {
	lastVerMu.Lock()
	v := lastVerText
	lastVerMu.Unlock()
	return v
}

// EnsureDebugMode checks if cloudmusic is running. If so, checks if it has the debug flag.
// If not, it kills it and restarts it with the debug flag.
// Returns true if the process was restarted, so we can wait a bit before connecting.
//
// 门禁不过（任一 cloudmusic.exe 的映像路径读不到或在 WindowsApps 下）时原样返回 launch 的错误，不结束、不拉起。
func EnsureDebugMode() (bool, error) {
	procs, err := scanFn()
	if err != nil {
		return false, err
	}
	if len(procs) == 0 {
		// Not running at all, nothing to do
		return false, nil
	}

	// 商店版门禁（issue #212）必须在这里：先于下面的 hasFlags 早退与一切结束、拉起。放在 hasFlags 之后，带着
	// 保活参数的商店版走不到它，也就给不出改用官网安装版的提示。读不到路径也拒绝，不再退回默认安装路径：
	// 那个路径上的不一定是在运行的这一份，而 taskkill 照样会结束在运行的全部实例。
	if err := gateAll(procs); err != nil {
		return false, err
	}

	for _, p := range procs {
		if strings.Contains(p.Cmdline, keepaliveMarker) {
			// Already running with debug + keepalive flags
			cacheVersionFromExe(procs[0].ExePath)
			return false, nil
		}
	}

	exePath := procs[0].ExePath // 门禁已确认每个都读得到、都不在 WindowsApps 下
	v := ReadExeVersion(exePath)
	if v != "" {
		lastVerMu.Lock()
		lastVerExe = exePath
		lastVerText = player.SanitizeVersion(v)
		lastVerMu.Unlock()
	}

	// v2 检查必须在这里 —— 在 taskkill 之前、在上面 hasFlags 早退之后。
	//
	// 放晚了（比如放到 Connect 失败处）就来不及：下面会把用户正在用的网易云 taskkill 掉、
	// 带调试参数重启 —— 而 v2 的 CEF 根本不开 remote debugging，重启完照样连不上。
	// 净效果是「杀了用户的播放器，换来一个永远连不上的端口」。issue #40 记录的正是这个。
	//
	// 放在 hasFlags 之后是为了省一次文件版本读取：已经带着我们的参数在跑的，必然是被我们
	// 重启过的 v3（v2 不会走到那一步）。
	if IsUnsupportedVersion(v) {
		return false, &UnsupportedVersionError{Version: v, ExePath: exePath}
	}

	log.Warn("%s running without debug/keepalive flags; restarting...", TargetProcessName)

	// Kill all instances
	log.Info("Terminating %d process(es)...", len(procs))
	killFn()
	sleepFn(2 * time.Second)

	// Restart with debug + keepalive flags.
	// 经 launch.Detached 拉起，不直接 Start：直接启动的网易云是 PlayerCap 的子进程，按进程树结束 PlayerCap
	// 的工具（OBS 的 Autostarter 插件）会把它一并结束（issue #127）。
	log.Info("Restarting %s (%s)", exePath, strings.Join(LaunchFlags, " "))
	detached, err := detachFn(exePath, LaunchFlags, "")
	if err != nil {
		return false, i18n.Errorf("failed to restart process: %v", err)
	}
	if !detached {
		log.Warn("%s was started directly (its path, arguments or working directory cannot be passed through cmd.exe unchanged); it stays in PlayerCap's process tree, so tools that end PlayerCap together with its child processes (such as OBS autostart plugins) will close it as well", TargetProcessName)
		rum.Track(rum.CatPlayer, "player_intervention", rum.Props{
			"player":  "cloudmusicv3",
			"kind":    "controlled_restart",
			"outcome": "detached_cmd_fallback",
		})
	}

	// Wait a few seconds for CEF to initialize
	sleepFn(3 * time.Second)
	return true, nil
}
