// Package watchdog 负责让 Spotify 桌面版带着 CEF 的远程调试端口（9223）跑起来，
// 并复用「播放器名 → 进程名」的单一真源（TargetProcessName）。
//
// 机制（2026-10 实测于 Spotify 1.3.4.258 / libcef 151.3.18）：
// Spotify 桌面版是 CEF 应用，它**自己**的隐藏 flag 表里有 `remote-debugging-port`
// （从 Spotify.dll 的 flag 描述串表里读到），且只认**启动时**的命令行——
// 实测在已运行的实例上再执行 `Spotify.exe --remote-debugging-port=9222`，
// 单实例逻辑只把窗口激活，9222 不开（进程数不变、无监听端口）。
// 因此拿端口只有一条路：**带 flag 重启**，与 cloudmusic/watchdog 的 EnsureDebugMode 同形。
//
// 与 cloudmusic/watchdog 的差异（改动前先读）：
//   - **杀之前先验 flag 存在**：读 Spotify.dll 的字节里有没有 ASCII 的 "remote-debugging-port"。
//     读不到（文件不存在/不可读）就**不动用户的进程**并返回 ErrVersionUnsupported。
//     这是 issue #40 的教训：宁可降级成「没有歌词」，也不换一个永远连不上的端口 + 一次白杀。
//   - **保活参数与 cloudmusic 同款，标记同样只能用 --disable-backgrounding-occluded-windows**：
//     实测（Spotify 1.3.4.258）只带调试口时，窗口最小化满 6 分钟、或被最大化窗口盖住几秒后，
//     页面被当作后台页，进度读数降到约每 2 秒、最终约每分钟一次，歌词行因此错过；带上
//     LaunchFlags 的四个防节流参数后两种场景 6 分钟内都保持每秒更新。四个参数各自的作用没有拆分验证，
//     故整组都带。判「已注入」须同时看调试口与保活标记：只看调试口会放过只带调试口的实例。
//   - **一次服务运行内只为补参数杀一次**：重启后主进程命令行若仍缺参数（被启动器改写等），
//     不再反复杀用户的播放器，只 Warn 一次（cloudmusic 没有这道保护，见 EnsureDebugMode）。
//   - **不做注册表之外的任何写入**：不打补丁、不写别人的 DLL。Spotify 的 libcef.dll 旁边有
//     .sig 签名文件，动它有被完整性校验拒绝的风险，这条路本包不碰（AGENTS §0.1）。
//
// 重启是**用户可见**的动作（当前播放中断数秒），但非破坏性：Spotify 会自己恢复曲目与位置
// （本次实测：taskkill /F 后带 flag 拉起，回到同一首歌、同一位置，只是处于暂停态）。
// 因此它与 cloudmusic 的重启同级，仍要打 Warn 日志留痕。
package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows/registry"
)

var log = logger.New("Spotify] [Watchdog") // 渲染为 [Spotify] [Watchdog]

// TargetProcessName 是 Spotify 主进程与全部子进程共用的映像名。
const TargetProcessName = "Spotify.exe"

// DebugPort 是给 Spotify 指定的 CEF 远程调试端口。
//
// **刻意避开 9222**：那是 cloudmusic/watchdog 的端口。两个调试端口撞在一起时，
// 先起来的那个占住端口、后一个的 watchdog 会以为「端口没起来」而反复重启自己的播放器。
// 12233 是酷狗、9229 是汽水，这里取 9223。
const DebugPort = 9223

// DebugFlag 是启动 Spotify 时注入的调试端口参数。
const DebugFlag = "--remote-debugging-port=9223"

// keepaliveMarker 判断主进程是否已带防节流参数。
// 不能用 DebugFlag 代替：只带调试口的实例（例如自启项只补过调试口）会逃过重启；
// 也不用 --disable-features=…，它的取值可能是用户或别的工具写的列表。
const keepaliveMarker = "--disable-backgrounding-occluded-windows"

// disableFeaturesPrefix 与 occlusionFeature 组成 LaunchFlags 的最后一项。
// Chromium 的同名开关以最后一个为准，补自启项时须并入已有列表而不是再追加一个。
const (
	disableFeaturesPrefix = "--disable-features="
	occlusionFeature      = "CalculateNativeWinOcclusion"
)

// LaunchFlags 是带 flag 重启 Spotify 时注入的全部参数（与 cloudmusic/watchdog 同款）：
//   - 调试端口：供 CDP 取数；
//   - 防遮挡 / 防后台节流：窗口最小化或被盖住时进度仍每秒更新（见包注释的实测）。
var LaunchFlags = []string{
	DebugFlag,
	keepaliveMarker,
	"--disable-renderer-backgrounding",
	"--disable-background-timer-throttling",
	disableFeaturesPrefix + occlusionFeature,
}

// debugFlagToken 是调试端口 flag 的名字（不含取值），用于在 Spotify.dll 里找这个 flag 是否被这一版认识。
const debugFlagToken = "remote-debugging-port"

// cliExeName 是随 Spotify 一起安装的命令行客户端。它给的是**权威的当前曲目 URI**
// （spotify:track:<base62 id>），而 URI 是取 Spotify 自家歌词的唯一钥匙 —— 页面的 DOM
// 里没有当前曲目的 URI（只有专辑/歌手链接与队列项）。见 spotify.go 的取词路径。
const cliExeName = "spotify_cli.exe"

// CLI 调用的时间预算。实测 `now-playing` 冷启动约 250ms（进程启动 + 与 App 的本地连接），
// 5s 是「App 忙到不回应」与「别把轮询循环卡住」之间的折中；超时即放弃本次取 URI。
const cliTimeout = 5 * time.Second

// portWait 是带 flag 拉起 Spotify 之后等调试端口的上限。实测冷启动约 3~6s（CEF 初始化）。
const portWait = 25 * time.Second

// exitWait 是 taskkill 之后等进程退干净的上限。进程没退就带 flag 拉起会撞单实例逻辑，
// 端口依旧不开，所以必须等（实测 /F 之后 1s 内全部退出）。
const exitWait = 8 * time.Second

// ErrNotRunning 表示 Spotify 没在跑。上层据此报「未启动」并静默等待（用户可能稍后启动）。
var ErrNotRunning = errors.New("Spotify 未在运行")

// ErrVersionUnsupported 表示这一版 Spotify 的 flag 表里没有 remote-debugging-port，
// 或安装文件读不到、无法确认。属于近乎永久的状态，上层应长退避重探而不是空转。
var ErrVersionUnsupported = errors.New("这一版 Spotify 不支持 remote-debugging-port（或安装文件不可读）")

// ErrPortNotReady 表示命令行里已经带着调试 flag，但端口还没起来（正在启动，或被别的程序占用）。
// 与 ErrVersionUnsupported 不同，它是瞬时的：上层短退避重试即可。
var ErrPortNotReady = errors.New("Spotify 带着调试 flag 在跑，但调试端口尚未就绪")

// scan 是一次进程扫描的结果。
type scan struct {
	PIDs    []int
	MainPID int
	ExePath string
	// HasDebugFlag / HasKeepalive 只看主进程的命令行。
	HasDebugFlag bool
	HasKeepalive bool
}

// hasToken 报告 cmdline 按空白切开后是否有与 flag 完全相同的一项。
// 不用子串匹配：`--remote-debugging-port=92230` 里也含 DebugFlag。
func hasToken(cmdline, flag string) bool {
	return slices.Contains(strings.Fields(cmdline), flag)
}

// scanProcesses 枚举全部 Spotify.exe：主进程 = 命令行里**不含** `--type=` 的那个
// （CEF 的 GPU/渲染/网络/存储/崩溃回收子进程都带 `--type=`）。
func scanProcesses() (scan, error) {
	var sc scan
	procs, err := process.Processes()
	if err != nil {
		return sc, i18n.Errorf("failed to list processes: %w", err)
	}
	for _, p := range procs {
		name, err := p.Name()
		if err != nil || !strings.EqualFold(name, TargetProcessName) {
			continue
		}
		sc.PIDs = append(sc.PIDs, int(p.Pid))
		if sc.ExePath == "" {
			if ep, err := p.Exe(); err == nil && ep != "" {
				sc.ExePath = ep
			}
		}
		cmd, err := p.Cmdline()
		if err != nil {
			continue
		}
		if strings.Contains(cmd, "--type=") {
			continue
		}
		sc.MainPID = int(p.Pid)
		// **标记必须是我们那一个端口，不能只匹配 flag 名**：Spotify 也可能被别的工具
		// （或我们上一次探测）以 `--remote-debugging-port=9222` 启动 —— 只匹配名字会把它
		// 判成「已带 flag」而转去等一个永远不会开的 9223，于是永远不修复。
		sc.HasDebugFlag = hasToken(cmd, DebugFlag)
		sc.HasKeepalive = hasToken(cmd, keepaliveMarker)
	}
	return sc, nil
}

// FindMainPID 返回 Spotify 主进程的 pid；未运行返回 ErrNotRunning。
func FindMainPID() (int, error) {
	sc, err := scanProcesses()
	if err != nil {
		return 0, err
	}
	if len(sc.PIDs) == 0 {
		return 0, ErrNotRunning
	}
	if sc.MainPID == 0 {
		// 进程在跑、但取不到主进程的命令行（权限）：不猜，交给上层退避重试。
		return 0, i18n.Errorf("Spotify main process not found among %d processes", len(sc.PIDs))
	}
	return sc.MainPID, nil
}

// DebugPortUp 报告 DebugPort 上是否已经能取到 DevTools 目标列表。
func DebugPortUp() bool {
	return devtoolsUp(fmt.Sprintf("http://127.0.0.1:%d/json/version", DebugPort))
}

// 进程与端口操作经包级变量调用，测试替换它们以离线驱动 EnsureDebugMode 的决策，不碰真进程。
var (
	scanFn         = scanProcesses
	portUpFn       = DebugPortUp
	supportsFlagFn = SupportsDebugFlag
	killFn         = func() error { return exec.Command("taskkill", "/F", "/IM", TargetProcessName).Run() }
	startFn        = func(exePath string, args []string) error { return exec.Command(exePath, args...).Start() }
)

// restartedThisRun 记录本次服务运行内是否已为补参数杀过 Spotify；
// keptMissingWarned 保证「重启后仍缺参数、不再重启」只 Warn 一次。
var (
	restartedThisRun  atomic.Bool
	keptMissingWarned atomic.Bool
)

// verdict 是 decideRestart 对一次扫描结果的判定。
type verdict int

const (
	verdictReady        verdict = iota // 参数齐全且端口通 → nil
	verdictPortNotReady                // 参数齐全但端口未起 → ErrPortNotReady
	verdictNoMainPID                   // 取不到主进程命令行且端口未起 → 报错，不动进程
	verdictRestart                     // 缺参数 → 验版本后带 LaunchFlags 重启
	verdictKeep                        // 缺参数，但本次运行已重启过且实例可用 → 不再重启
)

// decideRestart 按主进程命令行、端口状态与「本次运行是否已重启过」判定下一步。
//
// 已重启过之后仍缺参数（启动器改写命令行等）时，只要实例还带调试口或端口已通，就不再杀：
// 每轮都杀一次等于让用户的 Spotify 无法使用。缺调试口且端口不通的实例对我们没有用处
// （例如用户自己从开始菜单重开），仍照常重启。
func decideRestart(sc scan, portUp, restarted bool) verdict {
	if sc.MainPID == 0 {
		// 命令行读不到，无从判断参数：不动进程。端口通就照常用。
		if portUp {
			return verdictReady
		}
		return verdictNoMainPID
	}
	if sc.HasDebugFlag && sc.HasKeepalive {
		if portUp {
			return verdictReady
		}
		return verdictPortNotReady
	}
	if restarted && (portUp || sc.HasDebugFlag) {
		return verdictKeep
	}
	return verdictRestart
}

// EnsureDebugMode 确保 Spotify 带着调试端口与防节流参数在跑：
//   - Spotify 没在跑 → ErrNotRunning（**不代用户启动**：Spotify 有自己的开机自启，
//     我们在 PatchRegistryAutoStart 里把参数补进那条自启项，不需要也不应该替用户开机启动它）。
//   - 主进程两者俱备：端口通 → nil；端口未起 → ErrPortNotReady，短退避重试。
//   - 主进程缺调试口或缺保活标记 → **验版本 → 杀掉 → 带 LaunchFlags 拉起 → 等端口**。
//     端口已通也要重启：否则只带调试口的实例会一直在后台被节流。
//   - 本次运行已重启过一次仍缺参数 → 不再重启，Warn 一次，按端口通与否返回 nil 或 ErrPortNotReady。
//
// 与 cloudmusic/watchdog 的差异：cloudmusic 只看保活标记、缺了每轮都杀，没有「只杀一次」的保护。
//
// 返回 nil 表示端口已就绪、可以 Connect。
func EnsureDebugMode() error {
	sc, err := scanFn()
	if err != nil {
		return err
	}
	if len(sc.PIDs) == 0 {
		return ErrNotRunning
	}
	portUp := portUpFn()
	switch decideRestart(sc, portUp, restartedThisRun.Load()) {
	case verdictReady:
		return nil
	case verdictPortNotReady:
		return ErrPortNotReady
	case verdictNoMainPID:
		return i18n.Errorf("Spotify main process not found among %d processes", len(sc.PIDs))
	case verdictKeep:
		if !keptMissingWarned.Swap(true) {
			log.Warn("%s still lacks the debug or anti-throttling flags after our restart; not restarting it again during this run (progress may slow down while the window is minimized or covered)", TargetProcessName)
		}
		if portUp {
			return nil
		}
		return ErrPortNotReady
	}

	// —— 以下这条路径会杀掉用户正在用的 Spotify，故版本闸必须在 taskkill 之前（issue #40）——
	// 放晚了就是「杀了主播的播放器，换来一个永远连不上的端口」。
	if sc.ExePath == "" {
		return i18n.Errorf("cannot locate Spotify.exe path; leaving the running processes untouched")
	}
	supported, err := supportsFlagFn(sc.ExePath)
	if err != nil {
		// 验不了就不动它。宁可这一轮没有歌词，也不白杀一次。
		return i18n.Errorf("%w: %v", ErrVersionUnsupported, err)
	}
	if !supported {
		return ErrVersionUnsupported
	}

	if sc.HasDebugFlag {
		log.Warn("%s is running without the anti-throttling flags; restarting it once with them so progress keeps updating while the window is minimized or covered (playback resumes automatically)", TargetProcessName)
	} else {
		log.Warn("%s is running without %s; restarting it with the debug and anti-throttling flags (playback resumes automatically)", TargetProcessName, DebugFlag)
	}
	restartedThisRun.Store(true)
	if err := killFn(); err != nil {
		// 没有可杀的实例时 taskkill 返回非零；此处不致命，靠下面的等待判据决定成败。
		log.Warn("taskkill returned: %v", err)
	}
	if err := waitForExit(); err != nil {
		return err
	}

	log.Info("Starting %s %s", sc.ExePath, strings.Join(LaunchFlags, " "))
	if err := startFn(sc.ExePath, LaunchFlags); err != nil {
		return i18n.Errorf("failed to start Spotify: %w", err)
	}
	deadline := time.Now().Add(portWait)
	for time.Now().Before(deadline) {
		if portUpFn() {
			log.Success("Spotify restarted with the debug port on %d", DebugPort)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return i18n.Errorf("Spotify did not open the debug port within %s", portWait)
}

// waitForExit 等 Spotify 全部进程退干净。
func waitForExit() error {
	deadline := time.Now().Add(exitWait)
	for time.Now().Before(deadline) {
		sc, err := scanFn()
		if err != nil {
			return err
		}
		if len(sc.PIDs) == 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return i18n.Errorf("Spotify processes did not exit within %s", exitWait)
}

// SupportsDebugFlag 报告 exePath 所在安装目录里的 Spotify.dll 是否认识 remote-debugging-port。
//
// 判据是**字节**而不是版本号：这个 flag 是 Spotify 私有的隐藏开关，名字出现在它自己的
// flag 描述串表里（实测串形如 `...remote-app-config\0user-agent-product\0remote-debugging-port\0show-console...`）。
// 分块流式扫描（不把 43MB 的 DLL 整个读进内存），块间保留 len(needle)-1 字节重叠，
// 跨块边界的串才不漏。
func SupportsDebugFlag(exePath string) (bool, error) {
	dllPath := filepath.Join(filepath.Dir(exePath), "Spotify.dll")
	f, err := os.Open(dllPath)
	if err != nil {
		return false, i18n.Errorf("cannot read %s: %w", dllPath, err)
	}
	defer f.Close()

	needle := []byte(debugFlagToken)
	keep := len(needle) - 1
	buf := make([]byte, 1<<16)
	overlap := make([]byte, 0, keep)
	window := make([]byte, 0, keep+len(buf))
	for {
		n, err := f.Read(buf)
		if n > 0 {
			window = append(window[:0], overlap...)
			window = append(window, buf[:n]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			tail := window[len(window)-min(keep, len(window)):]
			overlap = append(overlap[:0], tail...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, i18n.Errorf("reading %s: %w", dllPath, err)
		}
	}
}

// devtoolsUp 报告 url 上是否有 DevTools 的 HTTP 端点在应答。
func devtoolsUp(url string) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// CLIPath 返回 spotify_cli.exe 的路径：优先取运行中实例的同目录，其次 %APPDATA%\Spotify。
func CLIPath() (string, error) {
	if sc, err := scanProcesses(); err == nil && sc.ExePath != "" {
		p := filepath.Join(filepath.Dir(sc.ExePath), cliExeName)
		if fileExists(p) {
			return p, nil
		}
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		p := filepath.Join(appData, "Spotify", cliExeName)
		if fileExists(p) {
			return p, nil
		}
	}
	return "", i18n.Errorf("%s not found", cliExeName)
}

// NowPlaying 是 spotify_cli now-playing --format json 里我们真正要的东西。
//
// 只要 URI 与 is_playing：歌名/歌手/封面/进度都从 CEF 页面按结构化字段取（DOM 里有的是分开的
// 字段，而 CLI 的 description 是「歌名 — 歌手」拼好的一个串，切不开）。
type NowPlaying struct {
	URI       string
	IsPlaying bool
	HasTrack  bool
}

// ReadNowPlaying 调 spotify_cli 取一次当前曲目。返回 HasTrack=false 表示没有曲目在播/已加载。
//
// 任何失败（CLI 缺失、超时、非零退出、JSON 变了）都返回 error，由调用方降级为「这一首没有歌词」。
func ReadNowPlaying(cliPath string) (NowPlaying, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cliPath, "now-playing", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return NowPlaying{}, i18n.Errorf("spotify_cli now-playing failed: %w", err)
	}
	// 只解我们认的字段：CLI 的输出会随版本增删字段，多出来的不该让我们失败。
	var parsed struct {
		CurrentlyPlaying *struct {
			URI       string `json:"uri"`
			IsPlaying bool   `json:"is_playing"`
		} `json:"currently_playing"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return NowPlaying{}, i18n.Errorf("cannot parse spotify_cli output: %w", err)
	}
	if parsed.CurrentlyPlaying == nil || parsed.CurrentlyPlaying.URI == "" {
		return NowPlaying{}, nil
	}
	return NowPlaying{
		URI:       parsed.CurrentlyPlaying.URI,
		IsPlaying: parsed.CurrentlyPlaying.IsPlaying,
		HasTrack:  true,
	}, nil
}

// PatchRegistryAutoStart 把 LaunchFlags 里缺的参数补进 HKCU 的自启项，使**开机自启**的
// Spotify 也带调试端口与防节流参数，省掉下次启动时的那一次「杀掉再拉起」。
//
// 与 cloudmusic/watchdog 的 PatchRegistryAutoStart 同形：值不存在（用户没开自启）就什么都不做 ——
// 绝不替用户创建一条自启项。
func PatchRegistryAutoStart() {
	patchAutostartAt(`Software\Microsoft\Windows\CurrentVersion\Run`, "Spotify")
}

// patchAutostartAt 修补 HKCU\keyPath 下名为 name 的自启值；键路径与值名作参数，供测试指向临时键。
func patchAutostartAt(keyPath, name string) {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		log.Error("Failed to open registry Run key: %v", err)
		return
	}
	defer key.Close()

	val, valType, err := key.GetStringValue(name)
	if err != nil {
		// 用户没把 Spotify 设为自启：不创建、不修改。
		return
	}
	newVal, changed := patchAutostartValue(val)
	if !changed {
		log.Info("Spotify autostart already carries the launch flags")
		return
	}
	// 写回要保持原值类型：REG_EXPAND_SZ 的值里可能带 %APPDATA% 之类，写成 REG_SZ 会改变它的展开行为。
	set := key.SetStringValue
	if valType == registry.EXPAND_SZ {
		set = key.SetExpandStringValue
	}
	if err := set(name, newVal); err != nil {
		log.Error("Failed to set registry Run value: %v", err)
		return
	}
	log.Success("Patched Spotify autostart flags: %s", newVal)
}

// patchAutostartValue 返回补齐 LaunchFlags 之后的自启值，以及是否有改动。
//
// 判据是按空白切开的完整参数项，原值只在末尾追加或就地并入，其余字节不动
// （exe 路径带引号且可能含空格，不能切开重拼）。--disable-features 特殊处理：
// Chromium 的同名开关以最后一个为准，已有该开关时把 CalculateNativeWinOcclusion 并入最后一个的列表，
// 而不是再追加一个把用户原有的列表覆盖掉。
func patchAutostartValue(val string) (string, bool) {
	if strings.TrimSpace(val) == "" {
		return val, false // 空值没有可执行文件路径，补参数只会写出一条残缺的自启项
	}
	// 可执行文件路径不是参数：路径里恰有一段与参数同形的文字时，不得被当作参数匹配或改写。
	cut := argsStart(val)
	if cut < 0 {
		return val, false // 引号没有配对：值已残缺，不在上面继续补参数
	}
	head, out := val[:cut], val[cut:]
	for _, f := range LaunchFlags {
		if !strings.HasPrefix(f, disableFeaturesPrefix) {
			if !hasToken(out, f) {
				out += " " + f
			}
			continue
		}
		start, end := lastTokenWithPrefix(out, disableFeaturesPrefix)
		if start < 0 {
			out += " " + f
			continue
		}
		list := out[start+len(disableFeaturesPrefix) : end]
		if slices.Contains(strings.Split(list, ","), occlusionFeature) {
			continue
		}
		sep := ","
		if list == "" {
			sep = ""
		}
		out = out[:end] + sep + occlusionFeature + out[end:]
	}
	return head + out, head+out != val
}

// argsStart 返回自启值里可执行文件路径之后（参数区）的起点。值以引号开头时，路径到配对的右引号为止；
// 否则取第一个空白分隔项（路径不含空格的常见形态；不带引号又含空格的路径由系统逐段试探，这里按第一项处理，
// 后面的路径片段只会被当作无关参数，不会被匹配成我们的参数）。以引号开头却找不到右引号时返回 -1（值已残缺）。
func argsStart(val string) int {
	lead := len(val) - len(strings.TrimLeftFunc(val, unicode.IsSpace))
	if lead == len(val) {
		return len(val)
	}
	if val[lead] == '"' {
		if j := strings.IndexByte(val[lead+1:], '"'); j >= 0 {
			return lead + 1 + j + 1
		}
		return -1
	}
	if j := strings.IndexFunc(val[lead:], unicode.IsSpace); j >= 0 {
		return lead + j
	}
	return len(val)
}

// lastTokenWithPrefix 返回 s 中最后一个以 prefix 开头的空白分隔项的 [start, end)；没有则返回 -1, -1。
// 空白的定义与 strings.Fields 一致（按 rune 判 unicode.IsSpace），与 hasToken 的切分口径相同。
func lastTokenWithPrefix(s, prefix string) (int, int) {
	start, end, tok := -1, -1, -1
	for i, r := range s {
		if !unicode.IsSpace(r) {
			if tok < 0 {
				tok = i
			}
			continue
		}
		if tok >= 0 && strings.HasPrefix(s[tok:i], prefix) {
			start, end = tok, i
		}
		tok = -1
	}
	if tok >= 0 && strings.HasPrefix(s[tok:], prefix) {
		start, end = tok, len(s)
	}
	return start, end
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
