package kugou

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "kugou"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("KuGou")

// KuGouPlayer 酷狗音乐播放器（CDP 实现）
type KuGouPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
	seen     bool // 本次服务期间见过酷狗在运行（player.ProcessStatus 的 seen），只由 Start 所在的 goroutine 读写
}

// New 创建酷狗音乐播放器
func New(offsetMs, pollMs int) *KuGouPlayer {
	return &KuGouPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// 封面获取的时间预算。
const (
	coverEarlyWait = 200 * time.Millisecond

	// coverTotalBudget 是「换歌 → 发出 song_info_update」的上界：等封面 URL 与下载 b64
	// 都算在这里面。song_info 的发出被 b64 下载挡着，所以预算越长、标题在 OBS 上出现越晚
	// ——d448816 把它从 3s 实测调优到 800ms 正是为此（perf，非疏忽；周围注释里残留的「3s」
	// 是更早的 33e6ce1 写的，已随本次重写清掉）。
	//
	// 缩短它的前提是 runCoverFetch 末尾那条不变量：拿不到 b64 也照发。否则预算越短，
	// 「b64 超时 → 整首歌不发 song_info」的触发概率越高。
	coverTotalBudget = 800 * time.Millisecond

	// coverAPIFallbackTimeout 走 API 兜底（歌手艺术照）时另给的下载预算。该路径下不含
	// 封面的信息已先发过，故可以慢一点。
	coverAPIFallbackTimeout = 5 * time.Second

	// coverRetryTimeout 是下载失败、带 URL 的一条发出之后补取一次的下载预算（#194）。
	// 此时歌曲信息已发出，补取不挡任何东西，故与 API 兜底同样给得宽。
	coverRetryTimeout = 5 * time.Second
)

// fetchCoverBase64 是 player.FetchCoverBase64 的可替换 seam：仅供 runCoverFetch 的取消时序测试
// 注入可阻塞的桩。封面下载现带 SSRF 拨号校验（拒回环/内网），httptest 的 127.0.0.1 mock 已不可用，
// 故测试改注入桩而非起真实回环服务。生产恒为 player.FetchCoverBase64，不导出、外部无法改动。
var fetchCoverBase64 = player.FetchCoverBase64

// runCoverFetch 取封面并发出 song_info_update；每首歌一个，换歌时由 ctx 取消。
//
//	阶段1: 等 coverEarlyWait 取封面 URL（**常见情况下 URL 已预置在缓冲 channel 里，立即命中**）
//	  - 取到   → 等 b64（coverTotalBudget 内）→ 一次性发完整信息
//	  - 超时   → 先发不含封面的信息 → 继续等到 coverTotalBudget
//	    → 取到 → 等 b64 → 补发完整信息
//	    → 超时 → 调 API 兜底（歌手艺术照，coverAPIFallbackTimeout）
//	      → 仍无 URL → 不再发（不含封面的信息上面已发过）
//	阶段2: 拿到 URL 而 b64 为空 → 照发带 URL 的信息 → 再下载一次（coverRetryTimeout）→ 取到则补发带 b64 的信息
//
// **不变量：只要拿到了封面 URL，就必须发出 song_info_update，无论 b64 成功与否。**
// 常见路径上封面 URL 立即命中，走的是「跳过上面整个分支」——此前一次 song_info_update
// 都没发过。此时为 b64 失败而 return，OBS 上整首歌都会停在上一首的标题和封面，而轮询
// 循环只在换歌时发、不会自愈。b64 只是加速手段（省掉前端自己取图），拿不到就带 URL 发，
// 前端自行加载——FetchCoverBase64 在图片超限时本来就是这个约定。
func (p *KuGouPlayer) runCoverFetch(ctx context.Context, ch <-chan string, name, singer, title, hash string) {
	defer telemetry.Guard()
	start := time.Now()

	var coverURL string
	select {
	case coverURL = <-ch:
	case <-time.After(coverEarlyWait):
	case <-ctx.Done():
		return
	}

	var fromAPI bool
	if coverURL == "" {
		// 未能及时取到封面 URL，先把歌曲信息发出去，不让它被封面拖住
		select {
		case <-ctx.Done():
			return
		default:
		}
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: name, Singer: singer, Title: title,
		})
		remaining := coverTotalBudget - time.Since(start)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case coverURL = <-ch:
		case <-time.After(remaining):
			// 预算内 CDP 仍无封面，调 API 兜底（伴奏/歌手头像类曲目）
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Info("Cover unavailable; falling back to artist photo")
			coverURL = fetchCoverFromAPI(hash)
			fromAPI = true
		case <-ctx.Done():
			return
		}
		if coverURL == "" {
			return // 不含封面的歌曲信息已在上面发过，这里没有新东西可补
		}
	}

	// 已有封面 URL，下载 base64
	b64Timeout := coverTotalBudget - time.Since(start)
	if fromAPI {
		b64Timeout = coverAPIFallbackTimeout
	}
	var b64 string
	if b64Timeout > 0 {
		b64 = fetchCoverBase64("Kugou", coverURL, b64Timeout)
	}

	select {
	case <-ctx.Done():
		return
	default:
	}
	// 不变量落点：b64 为空（超时/过大/失败）也照发，带着 URL。绝不为封面吞掉歌曲信息。
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL, CoverBase64: b64,
	})
	if b64 != "" || ctx.Err() != nil {
		return
	}

	// 补取（#194）：带 URL 的一条已经发出，这里再下载一次不推迟任何东西。上面那次可能只是没赶上：
	// 封面地址在 coverTotalBudget 末尾才到时只剩几十毫秒、甚至没有下载时间，网络抖动也会失败。
	// 只补一次；仍取不到就停，前端按 URL 自行加载。补发的 cover 与上一条相同。
	b64 = fetchCoverBase64("Kugou", coverURL, coverRetryTimeout)
	if b64 == "" {
		log.Warn("Cover download failed: %s", coverURL)
		return
	}
	if ctx.Err() != nil {
		return
	}
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL, CoverBase64: b64,
	})
}

// elevationBackoffs 第 n 次提权失败后等多久再试；用尽即放弃提权：本次运行不再弹 UAC，只做不提权的检查。
// 30s → 2min：长到不至于打断直播，又给误点「否」的用户留了重试机会。
var elevationBackoffs = []time.Duration{30 * time.Second, 2 * time.Minute}

// elevationGate 管理「UAC 被拒之后怎么重试」。
//
// 不设退避与上限的后果：EnsurePatched 在 launchElevatedHelper 处弹 UAC，被拒即返回；
// 主循环随后 waitForCDP —— 酷狗没在跑时它立即失败（不走 90s 兜底）—— 再 sleep 2s 就又
// 进 EnsurePatched 弹下一次。即 UAC 安全桌面每约 2 秒全屏夺焦一次，直播时是事故。
type elevationGate struct {
	fails int
}

// record 记一次提权失败，返回本次该等多久、以及是否该放弃提权。
func (g *elevationGate) record() (time.Duration, bool) {
	g.fails++
	if g.fails > len(elevationBackoffs) {
		return 0, true
	}
	return elevationBackoffs[g.fails-1], false
}

// reset 在提权成功后清零，使下一次失败重新从最短退避算起。
func (g *elevationGate) reset() { g.fails = 0 }

// ensureAction 是 EnsurePatched 出错之后 Start 的处置。
type ensureAction uint8

const (
	ensureStop       ensureAction = iota // 停止修补：进入受阻状态，按 storeRecheck 重查（#231 之前是终态停用）
	ensureRetryLater                     // 不等 CDP，稍后重新走 EnsurePatched
	ensureContinue                       // 照常等 CDP（用户可能手动启动酷狗）
)

// maxRunningInstallMisses 是酷狗在运行却找不到安装时，连续多少轮（含本轮）之后按受阻处理。不在第一轮就受阻：
// FindKuGouInstall 取进程快照与 Start 查酷狗是否在运行之间，酷狗可能恰好启动；酷狗重复启动或主进程退出时
// 会短暂出现两个主进程（或成了孤儿的子进程），这期间按进程定位失败。两者都是短暂状态，后续轮次可能定位成功。
const maxRunningInstallMisses = 5

// classifyEnsureErr 决定 EnsurePatched 返回的错误怎么处置。err 不得为 nil（EnsurePatched 成功时不调用）；
// ErrElevationFailed、ErrElevationNeeded 与商店版门禁的两种错误（launch.PackagedError、launch.ErrExeUnknown，issue #212）
// 不经本函数，由 Start 先行处理。runningMisses 是酷狗在运行却找不到安装的连续轮数，含本轮。
//
// 找不到安装：不在运行时等待重试——它可能装在注册表与常见路径之外，启动后 FindKuGouInstall 按运行中的进程
// 定位（设计 §6.3）；在运行时同样等待重试，满 maxRunningInstallMisses 轮才停止修补。启动的是另一份安装
// （ErrInstallChanged）或运行态尚未就绪（ErrRunningNotReady）：等待重试，下一轮按运行中的进程定位。版本指纹
// 不认识：停止修补，立即重试只会反复弹 UAC。停止修补不是终态：Start 按 storeRecheck 重做修补检查，条件满足即
// 恢复（#231，设计 §5）。其余照常等 CDP。
func classifyEnsureErr(err error, kugouRunning bool, runningMisses int) ensureAction {
	switch {
	case errors.Is(err, watchdog.ErrInstallNotFound):
		if kugouRunning && runningMisses >= maxRunningInstallMisses {
			return ensureStop
		}
		return ensureRetryLater
	case errors.Is(err, watchdog.ErrInstallChanged), errors.Is(err, watchdog.ErrRunningNotReady):
		return ensureRetryLater
	case errors.Is(err, watchdog.ErrVersionUnsupported):
		return ensureStop
	}
	return ensureContinue
}

// installWait 记「找不到安装，等待酷狗启动」的提示是否已打过：连续多轮找不到只打一次，找到安装后重置。
type installWait struct{ logged bool }

// notify 在本轮找不到安装时调用，返回是否该打提示。
func (w *installWait) notify() bool {
	if w.logged {
		return false
	}
	w.logged = true
	return true
}

// reset 在找到安装（EnsurePatched 返回的不是 ErrInstallNotFound）后调用。
func (w *installWait) reset() { w.logged = false }

// ensurePatched、ensurePatchedNoElevate 与 isKuGouRunning 是 watchdog 同名函数的可替换 seam：仅供 Start 的离线测试注入桩
// （真实的修补检查会弹 UAC、结束酷狗，判进程会枚举本机进程）。生产恒为 watchdog 的实现，不导出、外部无法改动。
var (
	ensurePatched          = watchdog.EnsurePatched
	ensurePatchedNoElevate = watchdog.EnsurePatchedNoElevate
	isKuGouRunning         = watchdog.IsKuGouRunning
)

// storeRecheck 是修补受阻（微软商店版、版本不支持、在运行而找不到安装、提权已放弃）之后多久重做一次修补检查
// （issue #212、#231）。与网易云、Spotify 的「版本不支持」同值：这些情形多半持久，但用户改装、升级或手动修补之后应当
// 自动恢复、不必重启 PlayerCap。重查只针对修补检查；酷狗是否在运行仍按 loopStep 每轮判。
const storeRecheck = 30 * time.Second

// loopStep 是 Start 外层循环的常规节奏：每轮先判酷狗是否在运行、声明状态，修补检查到期才做（设计 §3.2：进程的出现与
// 退出按常规节奏判定，不随重查间隔推迟）。
const loopStep = 2 * time.Second

// installerURL 是酷狗官网 PC 安装包的直链（302 到当前版本的安装包，2026-11-17 实测）。
const installerURL = "https://download.kugou.com/download/kugou_pc"

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *KuGouPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// repairState 是修补检查的结论，与酷狗是否在运行分开保存，状态由二者合成（设计 §5.3）。零值表示没有受阻、每轮都做修补检查。
type repairState struct {
	block     player.Blocked
	recheckAt time.Time // 下一次重做修补检查的时刻；零值表示每轮都做
	noElevate bool      // 提权已放弃：本次运行不再请求提权
}

// set 记下受阻的原因（零值即没有受阻）与多久之后重做修补检查；提权已放弃的标记不变。
func (rs *repairState) set(b player.Blocked, after time.Duration) {
	rs.block, rs.recheckAt = b, time.Now().Add(after)
}

// kugouDetails 是酷狗没在读歌时各情形的 detail。取用时才经 i18n.T：界面语言在启动早期才设定。
func kugouDetails() player.ProcessDetails {
	return player.ProcessDetails{
		NotRunning:   i18n.T("KuGou not running"),
		Exited:       i18n.T("KuGou has exited"),
		Disconnected: i18n.T("KuGou CDP disconnected"),
	}
}

// running 判酷狗是否在运行，见到在运行即记入 p.seen（本次服务期间见过它运行，不复位，见 player.ProcessStatus）。
func (p *KuGouPlayer) running() bool {
	r := isKuGouRunning()
	p.seen = p.seen || r
	return r
}

// declareDuring 在修补检查进行期间每 loopStep 判一次酷狗是否在运行，按 block 声明状态；返回的函数停止它并等它退出。
// 修补检查可能长时间不返回：DLL 已修补时一直等到酷狗启动，要修补时等用户回应 UAC、等酷狗启动；其间酷狗退出或启动，
// 状态也须按常规节奏改报（设计 §3.2）。它与 Start 不会同时读写 p.seen：Start 在修补检查返回之后才停止它，停止时等它退出。
func (p *KuGouPlayer) declareDuring(block player.Blocked) (stop func()) {
	quit, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer telemetry.Guard()
		defer close(exited)
		tick := time.NewTicker(loopStep)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				running := p.running()
				p.EmitStatusIfChanged(player.ProcessStatus(running, p.seen, kugouDetails(), block))
			}
		}
	}()
	return func() { close(quit); <-exited }
}

// Start 启动酷狗音乐轮询循环（阻塞），只在 Stop 之后返回。
//
// 每轮先判酷狗是否在运行，状态由它与修补检查的结论（repairState）合成（#231，设计 §3.2、§5.3）：修补受阻不设终态，
// 酷狗不在运行报 waiting_process、受阻的原因写进 detail，在运行报 standby 加原因；修补检查按 storeRecheck 重做，进程的
// 出现与退出仍每轮判。提权被拒 3 次之后本次运行不再请求提权，只做不提权的检查（ensurePatchedNoElevate）。
func (p *KuGouPlayer) Start() {
	// 修补受阻的几种情形（设计 §5.3）：Detail 在酷狗运行时用，NotRunningDetail 在不运行时用，空则按普通的未运行。
	var (
		storeBlocked = player.Blocked{
			Reason:           player.ReasonVersionUnsupported,
			Detail:           i18n.T("KuGou Microsoft Store version unsupported (requires the official installer version)"),
			NotRunningDetail: i18n.T("KuGou not running; the Microsoft Store version is unsupported (requires the official installer version)"),
		}
		versionBlocked = player.Blocked{
			Reason:           player.ReasonVersionUnsupported,
			Detail:           i18n.T("KuGou libcef.dll version unsupported"),
			NotRunningDetail: i18n.T("KuGou not running; this libcef.dll version is unsupported and cannot be read after it starts"),
		}
		installBlocked = player.Blocked{
			Reason: player.ReasonInstallNotFound,
			Detail: i18n.T("KuGou installation not found"),
		}
		elevationBlocked = player.Blocked{
			Reason:           player.ReasonNoAdmin,
			Detail:           i18n.T("Administrator rights not obtained; restart PlayerCap to request again"),
			NotRunningDetail: i18n.T("KuGou not running; administrator rights not obtained, restart PlayerCap to request again"),
		}
		pathBlocked = player.Blocked{
			Reason: player.ReasonNoAdmin,
			Detail: i18n.T("Cannot read the KuGou installation path (KuGou may be running as administrator)"),
		}
	)
	var elevation elevationGate
	var install installWait
	var rs repairState
	var lastRetryWarn string // ensureRetryLater 非 ErrInstallNotFound 的上一条告警文本，用于每 2s 重试去重
	runningMisses := 0       // 酷狗在运行却找不到安装的连续轮数，见 maxRunningInstallMisses
	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		// 酷狗是否在运行先于一切：受阻期间关掉或打开酷狗，本轮即改报，不等重查。
		running := p.running()
		if !running {
			lastRetryWarn = ""
		}
		p.EmitStatusIfChanged(player.ProcessStatus(running, p.seen, kugouDetails(), rs.block))
		p.Emit(player.EventClearSongData, nil)

		if time.Now().Before(rs.recheckAt) {
			if p.sleepOrStop(loopStep) {
				return
			}
			continue
		}

		// 自动检测并修复：若 libcef.dll 未 patch 则 kill → patch → 重启酷狗。提权已放弃之后只做不提权的检查，
		// 本次运行内任何路径都不再弹 UAC。
		check := ensurePatched
		if rs.noElevate {
			check = ensurePatchedNoElevate
		}
		stopDeclaring := p.declareDuring(rs.block)
		err := check(p.StopCh)
		stopDeclaring()
		select {
		case <-p.StopCh:
			return
		default:
		}
		if !errors.Is(err, watchdog.ErrInstallNotFound) {
			install.reset() // 找到了安装（或 CDP 已可用）：下一次找不到时重新提示、重新计数
			runningMisses = 0
		}
		// 受阻的原因与上一次相同（重查得出同一结论）时不再打日志，状态也不变。
		prev := rs.block
		// 仅 ensureRetryLater 的非 ErrInstallNotFound 错误保留去重状态；其余全部分支（含 nil 与 ErrInstallNotFound）均清零。
		prevRetryWarn := lastRetryWarn
		lastRetryWarn = ""
		var packaged *launch.PackagedError
		// startLocal 只在本轮修补检查返回 nil 时启用：返回其他错误而照常等 CDP 的会话不读本地歌词，
		// 只在线取词（设计 §6.2「时机」）。读库之前读取者还要自行确认版本与补丁（watchdog.Confirm）。
		var startLocal localLyricsStarter
		switch {
		case err == nil:
			elevation.reset() // 提权成功（或本就无需提权）：下次失败重新从最短退避算起
			rs.set(player.Blocked{}, 0)
			// 修补检查返回 nil 即酷狗已在运行过：CDP 已可用、等到了它启动，或刚为修补结束并重新拉起它。
			p.seen = true
			startLocal = startLocalLyrics
		case errors.As(err, &packaged):
			// 微软商店版：门禁已在提权、结束、修补、拉起之前挡住，不去等 CDP（门禁不过即不连接）。按 storeRecheck 重查，
			// 用户改装官网版后不必重启 PlayerCap 即可恢复。
			rs.set(storeBlocked, storeRecheck)
			if rs.block != prev {
				log.Warn("KuGou is the Microsoft Store version (%s) and cannot be patched; skipped KuGou lyric capture. To use it, uninstall it and install the official installer version: %s", packaged.ExePath, installerURL)
				telemetry.ReportOnce("kugou.store_install", "酷狗音乐为微软商店版，无法修补", nil, map[string]any{"exe_path": packaged.ExePath})
			}
			continue
		case errors.Is(err, launch.ErrExeUnknown):
			// 有 KuGou.exe 在运行而读不到它的映像路径：无从确知不是商店版，门禁已拒绝提权、结束、修补。
			// 不去 waitForCDP，下一轮重新走修补检查。
			rs.set(pathBlocked, loopStep)
			if rs.block != prev {
				log.Warn("Cannot read the installation path of KuGou; auto-repair skipped. Run KuGou with the same privileges as this program")
			}
			continue
		case errors.Is(err, watchdog.ErrElevationFailed):
			// 退避后重试；用尽即放弃提权。不去 waitForCDP——没提权就没 patch，CDP 不可能起来，而 waitForCDP 在酷狗没跑时
			// 会立即返回，等于 2s 后又弹 UAC。退避期间照常每轮判进程。
			backoff, giveUp := elevation.record()
			if giveUp {
				log.Error("Failed to obtain administrator rights %d times in a row; stopping auto-repair (retrying only makes the UAC secure desktop repeatedly steal focus). To enable KuGou, restart this program and click \"Yes\" on the UAC prompt", len(elevationBackoffs)+1)
				rs.noElevate = true
				rs.set(elevationBlocked, storeRecheck)
				continue
			}
			log.Warn("Administrator rights not obtained (attempt %d/%d): %v — retrying in %v",
				elevation.fails, len(elevationBackoffs)+1, err, backoff)
			rs.set(player.Blocked{}, backoff)
			continue
		case errors.Is(err, watchdog.ErrElevationNeeded):
			// 提权已放弃、DLL 仍未修补：不弹 UAC，按 storeRecheck 重查（用户以其他方式修补之后即可连上）。
			rs.set(elevationBlocked, storeRecheck)
			continue
		default:
			if running && errors.Is(err, watchdog.ErrInstallNotFound) {
				runningMisses++
			} else {
				runningMisses = 0
			}
			switch classifyEnsureErr(err, running, runningMisses) {
			case ensureStop:
				// 版本指纹不认识、或在运行而连续多轮找不到安装：不立即重试（版本不认识时重试只会反复弹 UAC），按 storeRecheck 重查。
				if errors.Is(err, watchdog.ErrVersionUnsupported) {
					rs.set(versionBlocked, storeRecheck)
					if rs.block != prev {
						log.Error("Auto-repair failed: %v (downgrade to a supported KuGou version, or retry after adaptation)", err)
					}
				} else {
					rs.set(installBlocked, storeRecheck)
					if rs.block != prev {
						log.Error("Auto-repair failed: %v (run the KuGou launch tool manually or check the install path)", err)
					}
				}
				continue
			case ensureRetryLater:
				// 找到（或重新确认）安装或运行态就绪之前不去 waitForCDP，下一轮重新走修补检查。
				if !errors.Is(err, watchdog.ErrInstallNotFound) {
					msg := err.Error()
					lastRetryWarn = msg
					if msg != prevRetryWarn {
						log.Warn("%v; retrying", err)
					}
				} else if install.notify() {
					log.Info("KuGou installation not found; waiting for KuGou to start")
				}
				rs.set(player.Blocked{}, loopStep)
				continue
			}
			log.Warn("Auto-repair failed: %v (run the KuGou launch tool manually or check the install path)", err)
			// 其他错误继续等待——用户可能手动启动酷狗
			rs.set(player.Blocked{}, 0)
		}

		client, err := p.waitForCDP()
		if err != nil {
			select {
			case <-p.StopCh:
				return
			default:
			}
			if errors.As(err, new(*launch.PackagedError)) || errors.Is(err, launch.ErrExeUnknown) {
				// 等 CDP 期间运行中的酷狗没过商店版门禁：不打「CDP 未就绪」、不等，回到循环顶部，由修补检查开头的门禁
				// 报出同一错误，走上面商店版与路径未知的分支（警告、状态与重查间隔都在那里）。
				continue
			}
			// CDP 等待超时（酷狗可能已退出）：下一轮按进程是否在运行报状态，再重新走修补检查。
			log.Warn("CDP not ready; rechecking KuGou status...")
			rs.set(player.Blocked{}, loopStep)
			continue
		}

		p.seen = true
		log.Info("CDP connected, monitoring playback state")
		p.runSession(client, startLocal)
		client.Close()

		// 会话结束：回到循环顶部即按酷狗是否在运行报 waiting_process（process_exited）或 standby（cdp_disconnected），
		// 并清空歌曲数据；修补检查等 loopStep 之后再做。
		log.Info("Session ended; waiting for KuGou to restart...")
		rs.set(player.Blocked{}, loopStep)
	}
}

// connectCDP 与 gateRunning 是 waitForCDP 的可替换 seam：仅供离线测试注入桩（不连本机的 12233、不枚举本机进程）。
// 生产恒为 cdp.Connect 与 watchdog.GateRunning，不导出。
var (
	connectCDP  = cdp.Connect
	gateRunning = watchdog.GateRunning
)

// waitForCDP 轮询 CDP 端口直到酷狗响应，返回已连接的 client。
// 若酷狗进程退出则立即返回错误；90s 超时作为兜底。连接失败而酷狗在运行时报 standby / cdp_disconnected：修补检查可能
// 一直等到酷狗启动才返回，循环顶部报的还是未运行（#231，设计 §3.2）。
//
// 每次连接都随停止与 90 s 的期限中止（cdp.Connect 自身另有单次上限）：端口接受连接而不完成响应时，轮询 goroutine 不被
// 挡在连接里。
//
// 每次连接之前都对全部运行中的 KuGou.exe 过商店版门禁（issue #212）：等 CDP 最长 90 秒，其间用户启动的可能是商店版或
// 读不到路径的实例。门禁不过即原样返回它的错误、不连接。
func (p *KuGouPlayer) waitForCDP() (*cdp.Client, error) {
	deadline := time.Now().Add(90 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	go func() {
		defer telemetry.Guard()
		select {
		case <-p.StopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	nextLog := time.Now()
	for {
		select {
		case <-p.StopCh:
			return nil, i18n.Errorf("stopped")
		default:
		}

		if err := gateRunning(); err != nil {
			return nil, err
		}
		client, err := connectCDP(ctx)
		if err == nil {
			return client, nil
		}

		// 进程已退出则立即返回，不再等待 90s
		if !p.running() {
			return nil, i18n.Errorf("KuGou process has exited")
		}
		p.EmitStatusIfChanged(player.ProcessStatus(true, p.seen, kugouDetails(), player.Blocked{}))

		now := time.Now()
		if now.After(nextLog) {
			log.Info("Waiting for KuGou CDP port 12233... (%v)", err)
			nextLog = now.Add(10 * time.Second)
		}
		if now.After(deadline) {
			return nil, i18n.Errorf("CDP not ready within 90s: %w", err)
		}
		if p.sleepOrStop(loopStep) {
			return nil, i18n.Errorf("stopped")
		}
	}
}

// playInfoSource 是 runSession 用到的 CDP 客户端方法。生产恒为 *cdp.Client，会话测试注入按脚本返回读数的桩。
type playInfoSource interface {
	GetPlayInfo() (*cdp.PlayInfo, error)
	IsClosed() bool
}

// fetchLyrics 是旧版无 ctx 在线取词测试 seam：默认 nil，仅供既有会话测试桩覆盖（非 nil 时优先于 fetchLyricsCtx）。
var fetchLyrics func(hash string, durationMs int, name, singer string) ([]klyric.Line, error)

// fetchLyricsCtx 是支持取消的在线取词入口，生产恒为 klyric.FetchContext，不导出。
var fetchLyricsCtx = klyric.FetchContext

func doFetchLyrics(ctx context.Context, hash string, durationMs int, name, singer string) ([]klyric.Line, error) {
	if fetchLyrics != nil {
		return fetchLyrics(hash, durationMs, name, singer)
	}
	return fetchLyricsCtx(ctx, hash, durationMs, name, singer)
}

// localFoundRereadBudget 是换歌拍命中本地/缓存歌词（aw.found）后等待 CDP 重读的时间上限：
// 正常本机 CDP 响应约 1~5ms（快路径照常刷新切歌过渡态与锚点），超时则跳出重读并按现有锚点
// 立即发布首份本地歌词，在飞探针交由下一拍 nextPlayInfo 串行接管。
const localFoundRereadBudget = 50 * time.Millisecond

type cdpSample struct {
	info *cdp.PlayInfo
	at   time.Time
	err  error
}

// cdpProbe 管理会话期间的单飞 GetPlayInfo 查询，确保同一会话内绝不并发调用 client.GetPlayInfo。
type cdpProbe struct {
	inFlight bool
	resCh    chan cdpSample
}

func newCDPProbe() cdpProbe {
	return cdpProbe{resCh: make(chan cdpSample, 1)}
}

func (pr *cdpProbe) start(client playInfoSource) {
	pr.inFlight = true
	go func() {
		defer telemetry.Guard()
		info, err := client.GetPlayInfo()
		pr.resCh <- cdpSample{info: info, at: time.Now(), err: err}
	}()
}

type onlineFetchResult struct {
	lines []klyric.Line
	err   error
}

type onlineWaitOutcome struct {
	lines          []klyric.Line
	err            error
	freshSample    *cdpSample
	localFound     bool
	localSrc       assoc.Source
	localOffsetMs  int
	unassociated   bool
	localNone      bool
	preempted      bool
	switched       bool
	nextInfo       *cdp.PlayInfo
	nextSampleTime time.Time
	connErr        error
	stopped        bool
}

// awaitOnlineOrLocal 在换歌拍本地等待未命中时发起在线取词：无本地读取者时同步调用（保持基线事件序列逐字节一致）；
// 有本地读取者时在单飞 cdpProbe 与 local.results() 并发监听下执行，支持换歌即时取消、取消关联或本地歌词零等待抢占，
// 以及在飞 cdpProbe 尚未落地时暂存旧歌在线结果、发现已换歌后直接丢弃（设计 §6.4.4 第 1 节）。
func (p *KuGouPlayer) awaitOnlineOrLocal(
	localCtx context.Context,
	client playInfoSource,
	probe *cdpProbe,
	local *localSession,
	pollInterval time.Duration,
	hash string,
	durationMs int,
	name, singer string,
	initLocalNone bool,
	initOffsetMs int,
) onlineWaitOutcome {
	if local.r == nil {
		lines, err := doFetchLyrics(localCtx, hash, durationMs, name, singer)
		return onlineWaitOutcome{
			lines:         lines,
			err:           err,
			localNone:     initLocalNone,
			localOffsetMs: initOffsetMs,
		}
	}

	fetchCtx, fetchCancel := context.WithCancel(localCtx)
	defer fetchCancel()

	doneCh := make(chan onlineFetchResult, 1)
	go func() {
		defer telemetry.Guard()
		lines, err := doFetchLyrics(fetchCtx, hash, durationMs, name, singer)
		doneCh <- onlineFetchResult{lines: lines, err: err}
	}()

	waitPoll := max(pollInterval, 50*time.Millisecond)
	pollTicker := time.NewTicker(waitPoll)
	defer pollTicker.Stop()

	localNone := initLocalNone
	latestOffsetMs := initOffsetMs
	var pendingOnline *onlineFetchResult
	var latestSameSong *cdpSample

	for {
		select {
		case <-p.StopCh:
			return onlineWaitOutcome{stopped: true}
		case out := <-doneCh:
			doneCh = nil
			if !probe.inFlight {
				return onlineWaitOutcome{
					lines:         out.lines,
					err:           out.err,
					localNone:     localNone,
					localOffsetMs: latestOffsetMs,
				}
			}
			pendingOnline = &out
		case <-pollTicker.C:
			if !probe.inFlight && pendingOnline == nil {
				probe.start(client)
			}
		case sample := <-probe.resCh:
			probe.inFlight = false
			if sample.err != nil {
				return onlineWaitOutcome{connErr: sample.err}
			}
			if sample.info != nil && sample.info.Hash != "" && sample.info.Hash != hash {
				return onlineWaitOutcome{
					switched:       true,
					nextInfo:       sample.info,
					nextSampleTime: sample.at,
				}
			}
			if sample.info != nil && sample.info.Hash == hash {
				s := sample
				latestSameSong = &s
			}
			if pendingOnline != nil {
				fresh := latestSameSong
				if fresh == nil {
					s := sample
					fresh = &s
				}
				return onlineWaitOutcome{
					lines:         pendingOnline.lines,
					err:           pendingOnline.err,
					freshSample:   fresh,
					localNone:     localNone,
					localOffsetMs: latestOffsetMs,
				}
			}
		case res := <-local.results():
			local.note(res)
			if local.r == nil {
				// 收到 KindDisabled：下轮循环起 local.results() 返回 nil 自动屏蔽，继续等待在线取词完成。
				continue
			}
			if res.Hash != hash {
				continue
			}
			local.seen(res)
			switch res.Kind {
			case assoc.KindUnassociated:
				local.cur, local.curPlayedAt, local.carried = 0, res.PlayedAt, false
				return onlineWaitOutcome{
					freshSample:  latestSameSong,
					unassociated: true,
					preempted:    true,
				}
			case assoc.KindFound:
				if len(res.Lines) > 0 {
					local.cur, local.curPlayedAt, local.carried = res.Source, res.PlayedAt, false
					return onlineWaitOutcome{
						lines:         res.Lines,
						freshSample:   latestSameSong,
						localFound:    true,
						localSrc:      res.Source,
						localOffsetMs: res.LyricOffsetMs,
						preempted:     true,
					}
				}
			case assoc.KindNone:
				localNone = localNone || local.localNone(res)
				if res.HasMemoryLyric {
					latestOffsetMs = res.LyricOffsetMs
				}
			}
		}
	}
}

type onlineJobResult struct {
	gen   uint64
	hash  string
	lines []klyric.Line
	err   error
}

// onlineJobs 管理播放中无可读文件重选词时的单飞代次异步在线恢复请求（仅由 runSession goroutine 读写）。
type onlineJobs struct {
	gen       uint64
	cancel    context.CancelFunc
	attempted bool
	ch        chan onlineJobResult
}

func newOnlineJobs() onlineJobs {
	return onlineJobs{ch: make(chan onlineJobResult, 1)}
}

func (j *onlineJobs) stop() {
	j.gen++
	if j.cancel != nil {
		j.cancel()
		j.cancel = nil
	}
	select {
	case <-j.ch:
	default:
	}
}

func (j *onlineJobs) start(parent context.Context, hash string, durationMs int, name, singer string) {
	j.stop()
	j.attempted = true
	gen := j.gen
	ctx, cancel := context.WithCancel(parent)
	j.cancel = cancel
	go func() {
		defer telemetry.Guard()
		lines, err := doFetchLyrics(ctx, hash, durationMs, name, singer)
		select {
		case j.ch <- onlineJobResult{gen: gen, hash: hash, lines: lines, err: err}:
		case <-ctx.Done():
		}
	}()
}

// runSession polls PlayInfo and emits events until the CDP connection drops.
//
// startLocal 启动本会话读本地歌词的读取者（设计 §6.7）；它为 nil、或返回 nil，本会话只在线取词，与接入本地歌词之前相同。
func (p *KuGouPlayer) runSession(client playInfoSource, startLocal localLyricsStarter) {
	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := time.Duration(p.pollMs) * time.Millisecond

	// localCancel 让读取者退出。下面每条返回路径都必须显式调用，写法与理由同下方的 coverCancel：别改成 defer 闭包、
	// 也别包成 helper，vet 的 lostcancel 只认直接调用。不取消的话读取者 goroutine 在会话结束后仍在，占着同时至多
	// 一个的名额，酷狗重连后的会话起不了读取者、只能在线取词。
	localCtx, localCancel := context.WithCancel(context.Background())
	var local localSession
	if startLocal != nil {
		local.r = startLocal(localCtx)
	}
	probe := newCDPProbe()
	onlineJobs := newOnlineJobs()
	var pendingInfo *cdp.PlayInfo
	var pendingSampleTime time.Time

	var lastHash string   // detect song change
	var lastStatus string // detect play/pause/stop change
	var rawLines []klyric.Line
	var currentLyrics []klyric.Line
	var currentFP [32]byte // rawLines 套偏移之前的指纹（lyricFingerprint），播放中据此判酷狗的歌词是否换了
	var currentLyricOffsetMs int
	var unassociated bool
	var lastLineIdx int = -1
	var currentDurationSec float32
	var currentTitle string
	var currentName string
	var currentSinger string
	var currentCover string

	// coverCh carries the cover URL to the per-song cover goroutine.
	// coverCancel aborts the previous goroutine on song change.
	var coverCh chan string
	var coverCancel context.CancelFunc
	// 下面每条早退路径都必须显式 coverCancel()，否则最后一首歌的封面 goroutine 不会被取消
	// ——它会在 Start() 已 Emit EventClearSongData **之后**（最长约 16s：等地址 0.8s、API 兜底请求 5s、
	// 下载 5s、补取 5s）再 Emit EventSongInfoUpdate，把已清空的歌曲信息重新贴回 OBS。（注：父 context 是
	// Background，不 cancel 几乎不泄漏资源，goroutine 退出即可 GC；真正的收益是上面
	// 这个「迟到的 Emit 覆盖已清空状态」，不是「防泄漏」。）
	//
	// 别改成 defer 闭包、也别包成 helper 函数：go vet 的 lostcancel 只认对该变量的直接调用，
	// 任何间接层都会让它从此对这里失明（实测 defer func(){ if c!=nil {c()} }() 仍被报为泄漏）。
	// 保持显式调用，vet 才能在将来有人新增 return 却忘了取消时当场抓住。
	//
	// **别把 vet 绿当成这类问题已经清了。** cloudmusic/qqmusic/wesing 三家的封面
	// goroutine 都不带 context，因此
	// lostcancel **结构性看不见它们**——没有 CancelFunc 可丢，分析器就无从下手。
	// 那三处的「迟到的封面回写覆盖当前歌曲」改由代次守卫拦截（BaseEmitter 的 NewSongGen /
	// EmitForGen），vet 同样看不见某处是否漏接。

	// Local clock interpolation for smooth progress between polls
	var anchorProgressSec float32
	var anchorTime time.Time
	isPlaying := false

	// confirmed：当前歌已读到过它的 hash 处于播放或暂停（会话第一首在换歌时即算）。prev 据此保存比较基准，
	// 认出换歌过渡态读数里上一首的进度（#195）。
	var confirmed bool
	var prev prevProgressGuard
	// noSongClear：本会话尚未读到歌且尚未发出过无歌 clear_song_data。断线清缓存后重连无歌时，
	// 服务端依赖 waiting_song 之后的 clear_song_data 派生清除专属画面（#231）。
	noSongClear := true

	liveNow := func() float32 {
		return player.LivePosition(anchorProgressSec, anchorTime, time.Now(), isPlaying, currentDurationSec)
	}

	matchCurrentLine := func(interpSec float32) {
		matchTime := player.LyricMatchTime(interpSec, offsetSec)
		if len(currentLyrics) > 0 {
			trueIdx := -1
			for i := len(currentLyrics) - 1; i >= 0; i-- {
				if matchTime >= currentLyrics[i].Time {
					trueIdx = i
					break
				}
			}
			if trueIdx >= 0 && trueIdx != lastLineIdx {
				lastLineIdx = trueIdx
				line := currentLyrics[trueIdx]
				// interpSec 是插值出的实时位置，进 Position、并据它算 Progress；play_time 由 BuildLyricUpdate
				// 按歌词时间轴算。KRC 源有逐字（line.Detailed）、翻译（line.SubText）与音译
				// （line.RomaText 及其逐字 line.RomaDetailed，type=0 轨），LRC 回落四者皆空。音译与翻译
				// 各自透传、互不覆盖。words 的 play_time 已在 applyDetailedOffset 里套过 offset，这里透传。
				p.Emit(player.EventLyricUpdate, lineUpdate(trueIdx, line, offsetSec, interpSec, currentDurationSec))
			}
		}
	}

	applyLocalResult := func(res assoc.Result, interpSec float32) {
		pl := local.classify(lastHash, res)
		switch pl.kind {
		case pollUnassociated:
			onlineJobs.stop()
			if !unassociated || len(currentLyrics) > 0 {
				unassociated = true
				rawLines = nil
				currentLyrics = nil
				currentFP = lyricFingerprint(nil)
				currentLyricOffsetMs = 0
				lastLineIdx = -1
				log.Info("KuGou lyrics unassociated by user; lyrics cleared")
				currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, nil, offsetSec)
			}
		case pollFound:
			onlineJobs.stop()
			nfp := lyricFingerprint(pl.lines)
			if unassociated || nfp != currentFP || pl.lyricOffsetMs != currentLyricOffsetMs {
				contentChanged := unassociated || nfp != currentFP
				unassociated = false
				rawLines = cloneLines(pl.lines)
				currentFP = nfp
				currentLyricOffsetMs = pl.lyricOffsetMs
				lastLineIdx = -1
				display := buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
				if contentChanged {
					log.Info("Lyrics changed in KuGou; reloaded (%s): %d lines; per-character: %s", i18n.T(pl.src.String()), len(display), detailedFlag(display))
				} else {
					log.Info("KuGou lyric offset changed: %+dms", currentLyricOffsetMs)
				}
				currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, display, offsetSec)
				if len(currentLyrics) == 0 {
					rawLines = nil
					currentFP = lyricFingerprint(nil)
				} else {
					matchCurrentLine(interpSec)
				}
			}
		case pollClearLocalNone:
			onlineJobs.stop()
			rawLines = nil
			currentLyrics = nil
			currentFP = lyricFingerprint(nil)
			currentLyricOffsetMs = 0
			lastLineIdx = -1
			log.Info("KuGou has no lyrics for this local file; lyrics cleared")
			currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, nil, offsetSec)
		case pollRevokeMemNone:
			onlineJobs.stop()
			rawLines = nil
			currentLyrics = nil
			currentFP = lyricFingerprint(nil)
			currentLyricOffsetMs = pl.lyricOffsetMs
			lastLineIdx = -1
			onlineJobs.attempted = false
			if !pl.recoverOnline {
				log.Info("KuGou has no lyrics for this local file; lyrics cleared")
			}
			currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, nil, offsetSec)
			if pl.recoverOnline {
				onlineJobs.start(localCtx, lastHash, int(currentDurationSec*1000), currentName, currentSinger)
			}
		case pollNone:
			offsetChanged := pl.hasMemoryLyric && pl.lyricOffsetMs != currentLyricOffsetMs
			if pl.hasMemoryLyric && unassociated {
				unassociated = false
				onlineJobs.attempted = false
			}
			if !unassociated && len(rawLines) > 0 {
				if offsetChanged {
					currentLyricOffsetMs = pl.lyricOffsetMs
					lastLineIdx = -1
					display := buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
					log.Info("KuGou lyric offset changed: %+dms", currentLyricOffsetMs)
					currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, display, offsetSec)
					if len(currentLyrics) == 0 {
						rawLines = nil
						currentFP = lyricFingerprint(nil)
					} else {
						matchCurrentLine(interpSec)
					}
				}
			} else if !unassociated && len(rawLines) == 0 {
				if pl.hasMemoryLyric {
					currentLyricOffsetMs = pl.lyricOffsetMs
					if !onlineJobs.attempted {
						onlineJobs.start(localCtx, lastHash, int(currentDurationSec*1000), currentName, currentSinger)
					}
				}
			}
		}
	}

	applyOnlineJobResult := func(r onlineJobResult, interpSec float32) {
		if r.gen == onlineJobs.gen && r.hash == lastHash && !unassociated && len(rawLines) == 0 {
			if onlineJobs.cancel != nil {
				onlineJobs.cancel()
				onlineJobs.cancel = nil
			}
			if r.err != nil {
				log.Warn("lyric fetch failed: %v", r.err)
			} else if len(r.lines) > 0 {
				rawLines = cloneLines(r.lines)
				currentFP = lyricFingerprint(rawLines)
				lastLineIdx = -1
				display := buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
				log.Info("Lyrics recovered online in KuGou: %d lines; per-character: %s", len(display), detailedFlag(display))
				currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, interpSec, display, offsetSec)
				if len(currentLyrics) == 0 {
					rawLines = nil
					currentFP = lyricFingerprint(nil)
				} else {
					matchCurrentLine(interpSec)
				}
			}
		}
	}

	nextPlayInfo := func() (*cdp.PlayInfo, time.Time, error, bool) {
		if pendingInfo != nil {
			info, at := pendingInfo, pendingSampleTime
			pendingInfo, pendingSampleTime = nil, time.Time{}
			return info, at, nil, false
		}
		if local.r == nil || lastHash == "" {
			if probe.inFlight {
				select {
				case <-p.StopCh:
					return nil, time.Time{}, nil, true
				case s := <-probe.resCh:
					probe.inFlight = false
					return s.info, s.at, s.err, false
				}
			}
			info, err := client.GetPlayInfo()
			return info, time.Now(), err, false
		}
		if !probe.inFlight {
			probe.start(client)
		}
		for {
			select {
			case <-p.StopCh:
				return nil, time.Time{}, nil, true
			case s := <-probe.resCh:
				probe.inFlight = false
				return s.info, s.at, s.err, false
			case res := <-local.results():
				applyLocalResult(res, liveNow())
			case r := <-onlineJobs.ch:
				applyOnlineJobResult(r, liveNow())
			}
		}
	}

	for {
		select {
		case <-p.StopCh:
			if coverCancel != nil {
				coverCancel()
			}
			localCancel()
			onlineJobs.stop()
			return
		default:
		}

		// *cdp.Client.IsClosed() 与在飞 GetPlayInfo() 共用 c.mu；在飞期间跳过 IsClosed()，
		// 交由 nextPlayInfo() 接管 probe.resCh，连接断开时 probe.resCh 返回非 nil err 照常退出会话。
		if !probe.inFlight && client.IsClosed() {
			if coverCancel != nil {
				coverCancel()
			}
			localCancel()
			onlineJobs.stop()
			return
		}

		// sampleTime 是本轮进度读数（info.Progress）的时刻。换歌分支与进度更新处落锚都配它，不取用到读数
		// 时的时刻：换歌分支里的取词是阻塞的网络请求，取词返回后再取当前时刻，就把取词前的读数配到了
		// 取词后的时刻上，位置落后取词耗时；进度更新处拿推算位置与读数比较，耗时超过 3 秒还会判成回跳。
		// 换歌分支在取词之后会重读一次（或复用等待期拿到的最新样本），读到同一首歌时 info、progressSec 与它一起替换。
		info, sampleTime, err, stopNow := nextPlayInfo()
		if stopNow {
			if coverCancel != nil {
				coverCancel()
			}
			localCancel()
			onlineJobs.stop()
			return
		}
		if err != nil {
			log.Warn("GetPlayInfo failed: %v", err)
			// Connection likely broken
			if coverCancel != nil {
				coverCancel()
			}
			localCancel()
			onlineJobs.stop()
			return
		}

		if info == nil || info.Hash == "" {
			// No song loaded yet
			if lastHash == "" {
				// 连上之后还没有读到过歌：报 waiting_song 并紧跟恰好一条 clear_song_data，否则状态停在 Start
				// 发出的非歌名状态（#181），且断线清缓存后重连无歌时服务端不会向专属出口派发清除（#231）。
				// 经 EmitStatusIfChanged + LastStatus/TryEmit，通道满或仅余 1 槽时在后续无歌拍按序补齐。
				// 会话中途（读到过歌之后）读到没有歌照旧不发：可能只是切歌的间隙，发出去会让路由把 kugou 判为 idle。
				p.EmitStatusIfChanged(player.NoSongStatus(player.ReasonNoSong, i18n.T("KuGou has no current song")))
				if last, ok := p.LastStatus(); noSongClear && ok && last.Reason == player.ReasonNoSong {
					noSongClear = !p.TryEmit(player.EventClearSongData, nil)
				}
			}
			time.Sleep(pollInterval)
			continue
		}

		progressRaw, _ := strconv.ParseFloat(info.Progress, 64)
		durationMs, _ := strconv.ParseFloat(info.Duration, 64)
		// KuGou 的 progress 字段使用 100 纳秒单位（≠ 毫秒），除以 1e7 得到秒。
		// duration 字段为毫秒，除以 1000 得到秒。
		progressSec := float32(progressRaw / 1e7)
		durationSec := float32(durationMs) / 1000.0

		// prevReading：本轮采用的读数（换歌那一轮重读成功时为重读的）是过渡态、且进度判为上一首的（#195）。
		// 这样的读数不参与下方的进度更新与跳转检测：换歌拍读到播放、取词期间转为过渡态时 isPlaying 仍为真，
		// 不拦的话上一首的进度会被当成一次前跳，发 playback_resume 并重新落锚，刚按 0 宣告的位置又跳回去；
		// 之后各拍读到同样的过渡态读数也一样。歌词行匹配与位置采样只用锚点推算的位置，随之不受它影响。
		prevReading := false

		// ── 切歌检测 ──
		songChanged := info.Hash != lastHash
		if songChanged {
			onlineJobs.stop()
			onlineJobs.attempted = false
			noSongClear = false
			firstSong := lastHash == "" // 本会话（本次 CDP 连接）读到的第一首
			// 必须在下面重置锚点、时长与 isPlaying 之前：基准取的是上一首此刻的时钟。
			prev.rebase(confirmed, anchorProgressSec, anchorTime, isPlaying, currentDurationSec)
			lastHash = info.Hash
			lastLineIdx = -1
			currentDurationSec = durationSec
			// 与 statusStr、下方播放状态变化检测同一个判据（都认 "play"）：只认 "playing" 的话，状态为
			// "play" 时这里记成未在播，而 lastStatus 也是 "play"、状态变化检测不触发，本地时钟到下一次
			// 状态变化之前都不走。
			isPlaying = statusStr(info.PlayStatus) == "playing"
			// 换歌拍读到播放或暂停即确认：此时 lastStatus 会记成同一个值，下方状态变化检测不触发，只能在这里记。
			transient := statusStr(info.PlayStatus) == "standby"
			confirmed = !transient || firstSong
			// progress 仅在合法范围内时作为初始锚点，否则从 0 开始。过渡态读数的进度可能还是上一首的（#195），
			// 认出来的同样从 0 开始；读到播放或暂停的不比对。
			prevReading = transient && isProgressValid(progressSec, durationSec) && prev.isPrev(progressSec, sampleTime)
			if prevReading {
				log.Info("Progress %.2fs read while switching songs matches the previous song; announcing from 0", progressSec)
			}
			if isProgressValid(progressSec, durationSec) && !prevReading {
				anchorProgressSec = progressSec
			} else {
				anchorProgressSec = 0
			}
			anchorTime = sampleTime

			name, singer := splitFilename(info.Filename)
			// 捕获上一首歌的标识，用于判断是否是"同名同歌手但 hash 变了"的场景
			// （如开关伴唱模式，hash 变化但歌词应复用）
			prevName, prevSinger := currentName, currentSinger
			currentName = name
			currentSinger = singer
			currentTitle = buildTitle(name, singer)
			currentCover = normalizeKugouCoverURL(info.Cover)

			log.Info("♪ Song: %s - %s (hash: %s)", currentName, currentSinger, info.Hash)

			// 切歌瞬间 CDP 常回一个过渡态 play_status（新歌尚未起播，读到空/stopped），statusStr
			// 会把它塌成 standby。但 hash 已变，本身就证明「新歌入场」而非待机——这条 standby 发出去
			// 会让路由器把 kugou 判为 idle：正在播的其他播放器立刻顶上又被抢回（overlay 硬切/闪烁；
			// 单播放器时则 player_clear 瞬间全黑）。故过渡态不发 status_update，留给下方 play_status
			// 变化检测读到真实 playing/paused 再发（本轮取词后重读到的，或下一帧读到的）；真正的
			// playing/paused 一律照发。
			//
			// 会话的第一首例外：酷狗启动后恢复了上次的歌而未播放时，读到的同样既非播放也非暂停。此时状态还是
			// Start 发出的非歌名状态，压下防不了闪烁，只会让状态停在那一条直到开始播放（#181）。
			// 按 paused 发，不另发 playback_pause；开始播放后由下方变化检测照常发 playback_resume 与 playing。
			// 这条 paused 不会让路由切到 kugou：它没进过 playing，不算 holding（server/router.go 的 evaluateGroup）。
			s := statusStr(info.PlayStatus)
			if s == "standby" && firstSong {
				s = "paused"
			}
			if s != "standby" {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: s, Detail: currentTitle})
			}

			// 取消上一首歌的封面 goroutine
			if coverCancel != nil {
				coverCancel()
			}
			// 每首歌一个带缓冲的 channel（容量 1），由主循环写入封面 URL
			coverCh = make(chan string, 1)
			if currentCover != "" {
				coverCh <- currentCover
			}
			var ctx context.Context
			ctx, coverCancel = context.WithCancel(context.Background())
			go p.runCoverFetch(ctx, coverCh, currentName, currentSinger, currentTitle, info.Hash)

			// 获取歌词：先取酷狗为这首歌显示的本地歌词或取消关联状态（至多等 localWait），未命中再走可抢占在线取词（设计 §6.4.4）。
			aw := local.await(p.StopCh, info.Hash)
			if aw.stopped {
				// 等本地歌词时收到停止：同上方各条退出路径直接返回，不再在线取词、不发本首的歌词事件。
				// 取消照样直接调用，理由见 coverCancel 与 localCancel 的注释。
				coverCancel()
				localCancel()
				onlineJobs.stop()
				return
			}

			var waitRes onlineWaitOutcome
			switch {
			case aw.unassociated:
				onlineJobs.stop()
				unassociated = true
				rawLines = nil
				currentLyrics = nil
				currentFP = lyricFingerprint(nil)
				currentLyricOffsetMs = 0
				local.carried = false
				log.Info("KuGou lyrics unassociated by user; lyrics cleared")
			case aw.found:
				onlineJobs.stop()
				unassociated = false
				rawLines = cloneLines(aw.lines)
				currentFP = lyricFingerprint(rawLines)
				currentLyricOffsetMs = aw.lyricOffsetMs
				currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
				log.Info("Lyrics loaded (%s): %d lines; per-character: %s", i18n.T(aw.src.String()), len(currentLyrics), detailedFlag(currentLyrics))
			default:
				waitRes = p.awaitOnlineOrLocal(
					localCtx, client, &probe, &local, pollInterval,
					info.Hash, int(durationMs), name, singer,
					aw.none, aw.lyricOffsetMs,
				)
				if waitRes.stopped {
					coverCancel()
					localCancel()
					onlineJobs.stop()
					return
				}
				if waitRes.connErr != nil {
					log.Warn("GetPlayInfo failed: %v", waitRes.connErr)
					coverCancel()
					localCancel()
					onlineJobs.stop()
					return
				}
				if waitRes.switched {
					// 在线取词等待期间酷狗已切到新歌：不发本首歌词，将新读数挂起并立即进入新歌换歌分支。
					if !(name == prevName && singer == prevSinger) {
						rawLines = nil
						currentLyrics = nil
						currentFP = lyricFingerprint(nil)
						currentLyricOffsetMs = 0
						unassociated = false
					}
					lastStatus = info.PlayStatus
					pendingInfo, pendingSampleTime = waitRes.nextInfo, waitRes.nextSampleTime
					continue
				}
				switch {
				case waitRes.unassociated:
					onlineJobs.stop()
					unassociated = true
					rawLines = nil
					currentLyrics = nil
					currentFP = lyricFingerprint(nil)
					currentLyricOffsetMs = 0
					local.carried = false
					log.Info("KuGou lyrics unassociated by user; lyrics cleared")
				case waitRes.localFound:
					onlineJobs.stop()
					unassociated = false
					rawLines = cloneLines(waitRes.lines)
					currentFP = lyricFingerprint(rawLines)
					currentLyricOffsetMs = waitRes.localOffsetMs
					currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
					log.Info("Lyrics loaded (%s): %d lines; per-character: %s", i18n.T(waitRes.localSrc.String()), len(currentLyrics), detailedFlag(currentLyrics))
				default:
					unassociated = false
					// 酷狗里这一首是磁盘上的文件、确实没有歌词（localNone）：同名也不沿用上一首的歌词，取不到或出错都与不同名时
					// 相同（设计 §6.7「换歌拍」例外）。在线歌曲（含伴唱）不带本地文件标记，照旧沿用。
					sameSong := name == prevName && singer == prevSinger && !waitRes.localNone
					if waitRes.err != nil {
						log.Warn("lyric fetch failed: %v", waitRes.err)
						if sameSong && len(rawLines) > 0 {
							currentLyricOffsetMs = waitRes.localOffsetMs
							currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
							local.carried = true
						} else {
							rawLines = nil
							currentLyrics = nil
							currentFP = lyricFingerprint(nil)
							currentLyricOffsetMs = 0
							local.carried = false
						}
					} else if len(waitRes.lines) > 0 {
						rawLines = cloneLines(waitRes.lines)
						currentFP = lyricFingerprint(rawLines)
						currentLyricOffsetMs = waitRes.localOffsetMs
						currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
						log.Info("Lyrics loaded: %d lines; per-character: %s", len(currentLyrics), detailedFlag(currentLyrics))
					} else if sameSong && len(rawLines) > 0 {
						// 同名同歌手但本次取不到（伴唱模式 hash 变化）→ 从未偏移 rawLines 按本首偏移重新生成显示歌词。
						currentLyricOffsetMs = waitRes.localOffsetMs
						currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
						local.carried = true
						log.Info("Same song, hash changed; reusing previous lyrics (%d lines); word-by-word: %s", len(currentLyrics), detailedFlag(currentLyrics))
					} else {
						rawLines = nil
						currentLyrics = nil
						currentFP = lyricFingerprint(nil)
						currentLyricOffsetMs = 0
						local.carried = false
						log.Info("Instrumental / no lyrics; word-by-word: %s", detailedFlag(currentLyrics))
					}
				}
			}

			// 取词是阻塞的网络请求，返回时本轮读到的进度与播放状态都已过时：其间可能暂停、恢复或拖动过。
			// 仅在既未于 local.await/等待期被取消关联或本地歌词抢占、又无在飞 cdpProbe 时才发起重读（等待期
			// 若已有 cdpProbe 采回的同歌样本则直接复用；有本地读取者时经单飞 cdpProbe 发起并并发监听
			// local.results() 与 StopCh，防止阻塞重读拖延取消关联、内存撤销或换词/偏移更新；若本拍已由
			// local.await 命中本地/缓存歌词 aw.found，另设 localFoundRereadBudget 上限，快路径照常采纳
			// 重读，超时即跳出并保留在飞探针交由下一拍 nextPlayInfo 串行接管，以现有锚点 LivePosition
			// 立即发布首份本地歌词）；否则零等待跳过重读，以 LivePosition 当拍立即发出清词或本地歌词（设计 §6.4.4）。
			//
			// statusAtChange 是取词前的状态，即上面已发出（过渡态时压下未发）的那个。分支末尾记进
			// lastStatus 的必须是它，不是重读到的：取词期间状态变了，紧接着的播放状态变化检测才会发出
			// playback_pause / playback_resume 与状态；过渡态压下的那次状态也由该处补发。
			statusAtChange := info.PlayStatus
			emitPlaying := isPlaying
			var fresh *cdp.PlayInfo
			var rerr error
			var freshTime time.Time
			haveFresh := false
			if waitRes.freshSample != nil {
				fresh, rerr, freshTime = waitRes.freshSample.info, waitRes.freshSample.err, waitRes.freshSample.at
				haveFresh = true
			} else if !aw.unassociated && !waitRes.preempted && !probe.inFlight {
				if local.r == nil {
					fresh, rerr = client.GetPlayInfo()
					freshTime = time.Now()
					haveFresh = true
				} else {
					probe.start(client)
					var budgetTimer *time.Timer
					var budgetCh <-chan time.Time
					if aw.found {
						budgetTimer = time.NewTimer(localFoundRereadBudget)
						budgetCh = budgetTimer.C
					}
				rereadLoop:
					for {
						select {
						case <-p.StopCh:
							if budgetTimer != nil {
								budgetTimer.Stop()
							}
							coverCancel()
							localCancel()
							onlineJobs.stop()
							return
						case <-budgetCh:
							break rereadLoop
						case s := <-probe.resCh:
							probe.inFlight = false
							fresh, rerr, freshTime = s.info, s.err, s.at
							haveFresh = true
							break rereadLoop
						case res := <-local.results():
							pl := local.classify(lastHash, res)
							switch pl.kind {
							case pollEmpty:
								continue
							case pollUnassociated:
								onlineJobs.stop()
								unassociated = true
								rawLines = nil
								currentLyrics = nil
								currentFP = lyricFingerprint(nil)
								currentLyricOffsetMs = 0
								log.Info("KuGou lyrics unassociated by user; lyrics cleared")
								break rereadLoop
							case pollRevokeMemNone:
								onlineJobs.stop()
								rawLines = nil
								currentLyrics = nil
								currentFP = lyricFingerprint(nil)
								currentLyricOffsetMs = pl.lyricOffsetMs
								onlineJobs.attempted = false
								if !pl.recoverOnline {
									log.Info("KuGou has no lyrics for this local file; lyrics cleared")
								}
								if pl.recoverOnline {
									onlineJobs.start(localCtx, lastHash, int(currentDurationSec*1000), currentName, currentSinger)
								}
								break rereadLoop
							case pollClearLocalNone:
								onlineJobs.stop()
								rawLines = nil
								currentLyrics = nil
								currentFP = lyricFingerprint(nil)
								currentLyricOffsetMs = 0
								log.Info("KuGou has no lyrics for this local file; lyrics cleared")
								break rereadLoop
							case pollFound:
								nfp := lyricFingerprint(pl.lines)
								if unassociated || nfp != currentFP || pl.lyricOffsetMs != currentLyricOffsetMs {
									onlineJobs.stop()
									contentChanged := unassociated || nfp != currentFP
									unassociated = false
									rawLines = cloneLines(pl.lines)
									currentFP = nfp
									currentLyricOffsetMs = pl.lyricOffsetMs
									currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
									if contentChanged {
										log.Info("Lyrics changed in KuGou; reloaded (%s): %d lines; per-character: %s", i18n.T(pl.src.String()), len(currentLyrics), detailedFlag(currentLyrics))
									} else {
										log.Info("KuGou lyric offset changed: %+dms", currentLyricOffsetMs)
									}
									break rereadLoop
								}
							case pollNone:
								if pl.hasMemoryLyric && pl.lyricOffsetMs != currentLyricOffsetMs && len(rawLines) > 0 {
									currentLyricOffsetMs = pl.lyricOffsetMs
									currentLyrics = buildDisplayLyrics(rawLines, currentLyricOffsetMs, offsetSec)
									log.Info("KuGou lyric offset changed: %+dms", currentLyricOffsetMs)
									break rereadLoop
								} else if !unassociated && len(rawLines) == 0 && pl.hasMemoryLyric {
									currentLyricOffsetMs = pl.lyricOffsetMs
									if !onlineJobs.attempted {
										onlineJobs.start(localCtx, lastHash, int(currentDurationSec*1000), currentName, currentSinger)
										break rereadLoop
									}
								}
							}
						}
					}
					if budgetTimer != nil {
						budgetTimer.Stop()
					}
				}
			}
			if haveFresh && rereadSameSong(fresh, rerr, lastHash) {
				sampleTime = freshTime
				freshRaw, _ := strconv.ParseFloat(fresh.Progress, 64)
				info = fresh
				progressSec = float32(freshRaw / 1e7)
				switch statusStr(fresh.PlayStatus) {
				case "playing":
					emitPlaying = true
				case "paused":
					emitPlaying = false
				}
				// 本轮的读数换成了重读的，prevReading 也按它重判。判过渡态看的是重读到的状态，不是取词前的：
				// 重读读到播放或暂停，进度就是新歌的；读到播放或暂停时的确认由下方同一轮的状态变化检测记。
				prevReading = statusStr(fresh.PlayStatus) == "standby" && isProgressValid(progressSec, currentDurationSec) && prev.isPrev(progressSec, sampleTime)
				if prevReading {
					log.Info("Progress %.2fs read while switching songs matches the previous song; announcing from 0", progressSec)
					// 换歌拍读到播放、重读却是过渡态且进度是上一首的：切歌还没完成，撤销换歌拍记下的确认，
					// 过渡期延续到下方状态变化检测读到播放或暂停为止，其间各拍照样过滤。
					confirmed = false
				}
				if isProgressValid(progressSec, currentDurationSec) {
					if prevReading {
						anchorProgressSec = 0
					} else {
						anchorProgressSec = progressSec
					}
					anchorTime = sampleTime
				}
			}

			// 随歌词发出的位置取此刻的（服务端以事件到达的时刻为采样时刻）：重读成功时锚点就是刚读到的
			// 进度，并按 fresh 的播放态（emitPlaying）决定是否外推，避免等待期已暂停的样本继续按旧 playing
			// 往前外推；没有重读到则由取词前的锚点按经过的时间推算。
			initPlay := player.LivePosition(anchorProgressSec, anchorTime, time.Now(), emitPlaying, currentDurationSec)
			currentLyrics = p.emitLyrics(currentTitle, currentDurationSec, initPlay, currentLyrics, offsetSec)
			if len(currentLyrics) == 0 {
				rawLines = nil
				currentFP = lyricFingerprint(nil)
			}

			lastStatus = statusAtChange // 取词前的状态，理由见 statusAtChange
		} else if !confirmed && statusStr(info.PlayStatus) == "standby" && isProgressValid(progressSec, currentDurationSec) {
			// 换歌之后的各拍只在过渡期内判，即当前歌尚未确认：确认之后再读到与上一首相同的进度，可能是用户
			// 拖动到了那里，要按真实的跳转处理。用只读的 looksPrev：这里改写 (b) 的话，过渡期里一个只是接近 (a)
			// 的读数会把 (b) 挪走，下一次未确认的换歌就会拿挪过的 (b) 去比。不打日志（每拍一次会刷屏）。
			prevReading = prev.looksPrev(progressSec, sampleTime)
		}

		// ── 封面延迟到达（VIP 歌曲首次播放时封面为空，稍后才填入）──
		if cover, changed := offerLateCover(coverCh, currentCover, info.Cover); changed {
			currentCover = cover
			log.Info("Cover URL updated: %s", currentCover)
		}

		// ── 播放状态变化检测 ──
		if info.PlayStatus != lastStatus {
			lastStatus = info.PlayStatus

			switch info.PlayStatus {
			case "playing", "play":
				isPlaying = true
				confirmed = true
				// 恢复播放时用合法的 progress 重置锚点，否则沿用上次冻结位置
				if isProgressValid(progressSec, currentDurationSec) {
					anchorProgressSec = progressSec
				}
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("resumed @ %.2fs", anchorProgressSec)
			case "paused", "pause", "pasued":
				isPlaying = false
				confirmed = true
				// 暂停时冻结到合法进度；否则用本地时钟插值值冻结
				if isProgressValid(progressSec, currentDurationSec) {
					anchorProgressSec = progressSec
				} else {
					frozen := anchorProgressSec + float32(time.Since(anchorTime).Seconds())
					if currentDurationSec > 0 && frozen > currentDurationSec {
						frozen = currentDurationSec
					}
					anchorProgressSec = frozen
				}
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
				log.Info("paused @ %.2fs", anchorProgressSec)
			default:
				log.Warn("Unknown play_status value: %q", info.PlayStatus)
			}
		}

		// ── 进度更新 + seek 检测 ──
		// 仅当 progressSec 在合法范围内（[0, duration*1.05]）时才信任该值。
		// KuGou 在播放状态下可能返回超出歌曲时长的值（单位非 ms），此时让
		// 本地时钟自由运行，不更新锚点，不触发 seek 事件。判为上一首的读数同样不信任（见 prevReading）。
		if isPlaying && isProgressValid(progressSec, currentDurationSec) && !prevReading {
			// 与读数比较的是读数时刻的推算位置，落锚也配读数时刻（见 sampleTime）。
			interpSec := anchorProgressSec + float32(sampleTime.Sub(anchorTime).Seconds())
			drift := progressSec - interpSec
			if drift < -3.0 {
				log.Info("Seek-back detected: %.2fs → %.2fs", anchorProgressSec, progressSec)
				lastLineIdx = -1
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
			} else if drift > 3.0 {
				log.Info("forward seek detected: %.2fs → %.2fs", anchorProgressSec, progressSec)
				lastLineIdx = -1
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
			}
			anchorProgressSec = progressSec
			anchorTime = sampleTime
		}

		// ── 本地时钟插值 ──
		interpSec := liveNow()

		// ── 播放中跟随酷狗换词、取消关联、快慢偏移与无文件重选词恢复（设计 §6.4.4）──
		// 换歌拍不取：那一拍刚处理过歌词与偏移，超时后迟到的结果留到下一拍。不发 playback_resume，
		// 位置时钟、跳转检测与位置采样不受影响。此处保留一次非阻塞排空，覆盖 pollMs == 0 与
		// probe.resCh 抢先就绪的拍次。
		if !songChanged {
			select {
			case res := <-local.results():
				applyLocalResult(res, interpSec)
			default:
			}
			select {
			case r := <-onlineJobs.ch:
				applyOnlineJobResult(r, interpSec)
			default:
			}
		}

		// ── 歌词行匹配 ──
		matchCurrentLine(interpSec)

		// 位置采样（见 BaseEmitter.EmitPositionSample）：播放中按固定间隔报一次，校准服务端缓存里的位置
		// ——上面每轮都以平台进度重新落锚，3s 以内的偏差直接吸收、不发事件，无歌词的歌又没有歌词行可推。
		// 传 interpSec，与上面 lyric_update 的 position 同一个量；offset 只进了 matchTime，这里不带。
		if isPlaying {
			p.EmitPositionSample(time.Now(), interpSec, currentDurationSec)
		}

		if pollInterval > 0 {
			if local.r == nil || lastHash == "" {
				time.Sleep(pollInterval)
			} else {
				waitTimer := time.NewTimer(pollInterval)
				waitDone := false
				for !waitDone {
					select {
					case <-p.StopCh:
						waitTimer.Stop()
						if coverCancel != nil {
							coverCancel()
						}
						localCancel()
						onlineJobs.stop()
						return
					case <-waitTimer.C:
						waitDone = true
					case res := <-local.results():
						applyLocalResult(res, liveNow())
					case r := <-onlineJobs.ch:
						applyOnlineJobResult(r, liveNow())
					}
				}
			}
		}
	}
}

// splitFilename splits "Artist - Title" into (artist, title).
// If no " - " separator is found, returns ("", filename).
func splitFilename(filename string) (name, singer string) {
	idx := strings.Index(filename, " - ")
	if idx < 0 {
		return filename, ""
	}
	return strings.TrimSpace(filename[idx+3:]), strings.TrimSpace(filename[:idx])
}

func buildTitle(name, singer string) string {
	if singer == "" {
		return name
	}
	return fmt.Sprintf("%s - %s", singer, name)
}

func statusStr(playStatus string) string {
	switch playStatus {
	case "playing", "play":
		return "playing"
	case "paused", "pause", "pasued":
		return "paused"
	default:
		return "standby"
	}
}

// rereadSameSong 判断换歌取词之后重读到的播放信息能否当作本轮的读数：读成功、有歌，且仍是 hash
// 这一首。比较与轮询循环的换歌判据一致（hash 逐字相等）；放宽了，这里采用的读数会在下一轮被判成换歌。
// GetPlayInfo 在没有歌时返回 nil, nil，故须先判 fresh 非空。
func rereadSameSong(fresh *cdp.PlayInfo, err error, hash string) bool {
	return err == nil && fresh != nil && hash != "" && fresh.Hash == hash
}

// 认出上一首进度的两个容差（秒），理由见 prevProgressGuard。
const (
	prevProgressTolerance   = 1.0  // 与基准 (a) 推算位置的差
	frozenProgressTolerance = 0.01 // 与基准 (b) 的差：停住的读数原值逐字相同，只给浮点换算留余量
)

// prevProgressGuard 认出换歌过渡态读数里上一首的进度（#195，doc/design/kugou-double-announce-design.md §4 方案 A-2）。
//
// 酷狗切歌时可能先给出新歌的 hash、文件名与时长，播放态既非播放也非暂停，进度却仍是上一首的
// （推断自 15-kugou-40 中浮游天地第一次宣告的 1.512 s，见设计 §1.3；未经真机取证）。不认出来的话，它随 all_lyrics 原样发出，前端先跳到上一首的
// 位置、再由第二次宣告或 playback_resume 拉回。
//
// 基准两项：(a) 最近一首已确认（读到过播放或暂停；会话第一首在换歌时即算）的歌的时钟；(b) 自 (a) 设定以来
// 最近一次直接与 (a) 吻合的读数。未确认的宣告（包括被置 0 的锚点）一律不当基准：连续两次未起播的换歌里，
// 拿上一次的锚点比，第二次比的是 0，上一首的进度就漏了出去。
//
// (b) 只管酷狗把上一首的进度停在切歌那一刻的情形：(a) 随墙钟推算，过渡态一长就与停住的读数拉开，(b) 不动。
// (b) 只由直接与 (a) 吻合的读数设定，只与 (b) 吻合的读数不改写它，容差又只有浮点余量，所以被置 0 的只限于
// 离已确认的歌不到 1 s 的读数与和这类读数原值相同的读数；放宽任何一条，连续未确认的换歌会把误判一跳一跳地
// 推远，把从非 0 位置合法起播的新歌也置 0。
type prevProgressGuard struct {
	hasBase     bool
	baseSec     float32 // (a) 锚点进度
	baseAt      time.Time
	basePlaying bool
	baseDur     float32
	hasFrozen   bool
	frozenSec   float32 // (b)
}

// rebase 在换歌、重置锚点之前调用：当前歌已确认则以它此刻的时钟为 (a)，清空 (b)；未确认则 (a)、(b) 原样保留。
// 会话第一首之前 confirmed 为假，没有基准，isPrev 恒假。
func (g *prevProgressGuard) rebase(confirmed bool, anchorSec float32, anchorAt time.Time, playing bool, durSec float32) {
	if confirmed {
		*g = prevProgressGuard{hasBase: true, baseSec: anchorSec, baseAt: anchorAt, basePlaying: playing, baseDur: durSec}
	}
}

// isPrev 判 at 时刻读到的过渡态进度 p 是不是上一首的。与 (a) 推算位置相差不到 prevProgressTolerance 时为真并把
// (b) 设为 p；否则与 (b) 相差不到 frozenProgressTolerance 时为真，(b) 不变。只对过渡态读数调用。
func (g *prevProgressGuard) isPrev(p float32, at time.Time) bool {
	if !g.hasBase {
		return false
	}
	if math.Abs(float64(p-player.LivePosition(g.baseSec, g.baseAt, at, g.basePlaying, g.baseDur))) < prevProgressTolerance {
		g.frozenSec, g.hasFrozen = p, true
		return true
	}
	return g.hasFrozen && math.Abs(float64(p-g.frozenSec)) < frozenProgressTolerance
}

// looksPrev 是 isPrev 的只读版：判定与 isPrev 逐条相同（直接调用它），事后把 (b) 恢复原样。
// 换歌之后各拍用它，换歌拍与重读用 isPrev。
func (g *prevProgressGuard) looksPrev(p float32, at time.Time) bool {
	saved := *g
	defer func() { *g = saved }()
	return g.isPrev(p, at)
}

// normalizeKugouCoverURL 把 getPlayInfo 给的封面 URL 换成 800 档：路径里的 /stdmusic/120/ 换成 /stdmusic/800/。
// 只换查询串与片段之前的部分；其余尺寸段与路径没见过样本，原样返回。
func normalizeKugouCoverURL(u string) string {
	end := strings.IndexAny(u, "?#")
	if end < 0 {
		end = len(u)
	}
	return strings.Replace(u[:end], "/stdmusic/120/", "/stdmusic/800/", 1) + u[end:]
}

// offerLateCover 处理封面延迟到达：本轮读到的 raw 换成 800 档后与 current 不同，就送进 coverCh（满则丢），
// 返回它与 true；否则返回 current 与 false。只送 URL，由封面 goroutine 统一下载并发送，不额外发事件。
//
// 比较必须用换档之后的值：换歌时记下的 current 已是 800 档，拿原样的读数比，换歌那一轮紧接着就会
// 把它改回 120 档并打一条「Cover URL updated」；VIP 歌曲首播封面晚到时送进 coverCh 的也会是 120 档。
func offerLateCover(coverCh chan<- string, current, raw string) (string, bool) {
	cover := normalizeKugouCoverURL(raw)
	if cover == "" || cover == current {
		return current, false
	}
	select {
	case coverCh <- cover: // coverCh 为 nil 时走 default
	default:
	}
	return cover, true
}

// isProgressValid 判断 progressSec 是否在合理的播放范围内（[0, duration*1.05]）。
// 用于过滤 getPlayInfo 偶发返回的异常值。
func isProgressValid(progressSec, durationSec float32) bool {
	if progressSec < 0 {
		return false
	}
	if durationSec > 0 && progressSec > durationSec*1.05 {
		return false
	}
	return true
}

// emitLyrics 发出一首歌的全部歌词：all_lyrics，没有歌词时紧随一条 index:-1 的 lyric_update。position 是此刻的整曲
// 位置，lines 须已套 offset。返回处理后的 currentLyrics（只有纯音乐提示语时为 nil）。换歌拍与播放中换词共用。
func (p *KuGouPlayer) emitLyrics(title string, duration, position float32, lines []klyric.Line, offsetSec float32) []klyric.Line {
	lyricItems := toLyricLines(lines, offsetSec)
	progress := player.ClampProgress(position, duration)

	// 酷狗对没有歌词的歌返回一行「纯音乐，请欣赏」，而非空歌词。归一成
	// index:-1，让下游只认一个判据；提示语原样留在 text 里，用不用由它决定。
	//
	// **返回的 currentLyrics 必须一起清**：它是轮询里歌词行匹配的输入，不清的话
	// all_lyrics 说 count:0、轮询却仍按那一行发 index:0 的 lyric_update。
	pureHint := ""
	if hint, ok := player.PureMusicHint(lyricItems, title); ok {
		log.Info("platform returned only the hint \"%s\", treating as no lyrics", hint)
		pureHint = hint
		lines = nil
		lyricItems = []player.LyricLine{} // 显式空 slice——nil 会序列化成 null，破坏「空数据一律 {} / []」
	}

	p.Emit(player.EventAllLyrics, &player.AllLyricsData{
		Title:    title,
		Duration: duration,
		Position: position,
		Progress: progress,
		Lyrics:   lyricItems,
		Count:    len(lyricItems),
	})
	if len(lines) == 0 {
		// Progress 必须带上：它的契约是「整首播到哪」，纯音乐时歌照样在走。
		// 漏掉它会让 Go 零值兜出一个 0，与同一事件里的 all_lyrics.progress 自相矛盾。
		p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
			Index: -1, Text: pureHint, Timestamp: 0, Position: position, Progress: progress,
		})
	}
	return lines
}

func toLyricLines(lines []klyric.Line, offsetSec float32) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		// SubText（翻译）与 RomaText（音译，KRC type=0 轨）各自透传给 BuildLyricLine，互不覆盖；
		// LRC 回落时二者皆空。逐字音译（RomaDetailed）进 roma_text_detailed。
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec)
		out[i].RomaTextDetailed = l.RomaDetailed
	}
	return out
}

// lineUpdate 把一行歌词映射成 lyric_update 的载荷，字段口径同 toLyricLines：两者取自同一份
// currentLyrics，同一行的逐字音译在 all_lyrics 与 lyric_update 里相同。
func lineUpdate(index int, l klyric.Line, offsetSec, playPos, duration float32) *player.LyricUpdate {
	u := player.BuildLyricUpdate(index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec, playPos, duration)
	u.RomaTextDetailed = l.RomaDetailed
	return u
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time。
//
// **必须在 Fetch 之后、存进 currentLyrics 之前套一次**：BuildLyricLine 只调行级 play_time，
// TextDetailed 是原样透传的（对照 cloudmusic 的 applyTextDetailedOffset、qqmusic 的同名函数）。
// 漏了这步，逐字高亮会整体偏一个 offset 而行级却是对的，症状极像"前端没对齐"；且
// lyric_update 直接用 currentLyrics 里的 TextDetailed，不在这儿套就没有第二次机会。
//
// 只对新取回的歌词调用一次：伴唱模式复用 currentLyrics 那条路上的歌词已经套过了，重复套会翻倍。
func applyDetailedOffset(lines []klyric.Line, offsetSec float32) {
	for i := range lines {
		// 主歌词的逐字与音译的逐字各是一条时间轴（krc 解析时都没填 PlayTime），都要套。
		for _, d := range []*player.LyricTextDetailed{&lines[i].Detailed, &lines[i].RomaDetailed} {
			if len(d.Words) == 0 {
				continue
			}
			d.PlayTime = player.AdjustLyricPlayTime(d.Timestamp, offsetSec)
			for j := range d.Words {
				d.Words[j].PlayTime = player.AdjustLyricPlayTime(d.Words[j].Timestamp, offsetSec)
			}
		}
	}
}

// detailedFlag 报告这批歌词**实际**有没有逐字。
//
// 绝不写死——AGENTS.md §6.1：写死的「逐字：否」是断言不是报告，接通逐字的人会据此以为
// 自己没做成、回去改一个本来就对的实现。酷狗的逐字只在 KRC 源上有；download 回落到
// fmt=lrc 时只有行级，那时这里如实报「否」。
func detailedFlag(lines []klyric.Line) string {
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}

// ── 酷狗 API 封面兜底 ────────────────────────────────────────────────────────

var kugouAPIClient = &http.Client{Timeout: 5 * time.Second}

type kugouSongInfoResp struct {
	ImgURL     string `json:"imgUrl"`
	TransParam struct {
		UnionCover string `json:"union_cover"`
	} `json:"trans_param"`
}

// fetchCoverFromAPI 通过酷狗公开 API 获取歌曲封面 URL。
// 对于 CDP 未提供封面的曲目（伴奏、无专辑封面等），
// API 会返回歌手头像（imgUrl / trans_param.union_cover），
// 其中 {size} 占位符替换为 800。
//
// http 地址改用 https（#194）。API 给的歌手图在 singerimg.kugou.com 上，该图床拒绝 Go 默认的 User-Agent：
// 有的图 http 回 403、https 回 200，有的图 http 与 https 都回 403（2026-11-14 本机实测）；带浏览器标识则都回 200，
// 封面下载已统一带上（player/cover.go）。改 https 保留。
func fetchCoverFromAPI(hash string) string {
	url := "https://m.kugou.com/app/i/getSongInfo.php?cmd=playInfo&hash=" + hash
	resp, err := kugouAPIClient.Get(url)
	if err != nil {
		log.Warn("Cover API request failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r kugouSongInfoResp
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	cover := r.TransParam.UnionCover
	if cover == "" {
		cover = r.ImgURL
	}
	if cover == "" {
		return ""
	}
	cover = strings.ReplaceAll(cover, "{size}", "800")
	if rest, ok := strings.CutPrefix(cover, "http://"); ok {
		cover = "https://" + rest
	}
	return cover
}
