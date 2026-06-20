package watchdog

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/logger"

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

// EnsureDebugMode checks if cloudmusic is running. If so, checks if it has the debug flag.
// If not, it kills it and restarts it with the debug flag.
// Returns true if the process was restarted, so we can wait a bit before connecting.
func EnsureDebugMode() (bool, error) {
	procs, err := process.Processes()
	if err != nil {
		return false, fmt.Errorf("failed to list processes: %v", err)
	}

	var hasNetease bool
	var hasFlags bool
	var exePath string
	var pidsToKill []int

	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}
		if strings.EqualFold(name, TargetProcessName) {
			hasNetease = true
			pidsToKill = append(pidsToKill, int(p.Pid))

			// Try to get exe path if we haven't already
			if exePath == "" {
				ep, err := p.Exe()
				if err == nil && ep != "" {
					exePath = ep
				}
			}

			// Check command line：是否已带我们注入的保活参数
			cmdline, err := p.Cmdline()
			if err == nil {
				if strings.Contains(cmdline, keepaliveMarker) {
					hasFlags = true
				}
			}
		}
	}

	if !hasNetease {
		// Not running at all, nothing to do
		return false, nil
	}

	if hasFlags {
		// Already running with debug + keepalive flags
		return false, nil
	}

	log.Warn("发现 %s 未启用调试/保活参数，正在重启...", TargetProcessName)

	if exePath == "" {
		// Fallback to standard path
		exePath = `C:\Program Files\NetEase\CloudMusic\cloudmusic.exe`
	}

	// Kill all instances
	log.Info("正在终止 %d 个进程...", len(pidsToKill))
	exec.Command("taskkill", "/F", "/IM", TargetProcessName).Run()
	time.Sleep(2 * time.Second)

	// Restart with debug + keepalive flags
	log.Info("正在重启 %s （%s）", exePath, strings.Join(LaunchFlags, " "))
	cmd := exec.Command(exePath, LaunchFlags...)
	err = cmd.Start()
	if err != nil {
		return false, fmt.Errorf("failed to restart process: %v", err)
	}

	// Wait a few seconds for CEF to initialize
	time.Sleep(3 * time.Second)
	return true, nil
}
