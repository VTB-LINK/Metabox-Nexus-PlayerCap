// Package sodamusic 接入汽水音乐（字节跳动 SodaMusic.exe，Electron + Vue3）作为第 5 个 player。
//
// 形态最接近 kugou（CDP + 明文 KRC 逐字歌词），故实现照 kugou 骨架，换歌/时钟外推照 cloudmusic：
//   - watchdog：找主进程 pid → 复刻 process._debugProcess 开 9229（绕过原生反调试，见 watchdog 包）。
//   - cdp：连 9229 → 主进程 executeJavaScript 桥进 rendererMain → transport 请求 sharedState.get('player')。
//   - 歌词：字节直接给**已解密的明文 KRC**（酷狗格式），用 player/krc 公共包解析，逐字白捡。
//   - 进度：sharedState 给每秒粗 progressSeconds，本地墙钟外推平滑（同 kugou/cloudmusic）。
//
// 稳定优先（AGENTS §0.1）：任何一步失败都降级重试，绝不 panic、绝不动汽水音乐进程状态。
package sodamusic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
	"Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "sodamusic"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Soda")

// SodaMusicPlayer 汽水音乐播放器（CDP 实现）。
type SodaMusicPlayer struct {
	player.BaseEmitter
	offsetMs         int
	pollMs           int
	rumGate          player.SessionRUMGate[*sodaSessionRUM]
	testBeforeAttach func() // 仅供单测注入 Register 锁外发 attached 前的并发交错屏障
	attachMetaMu     sync.Mutex
	attachMeta       sodaAttachMeta
}

// New 创建汽水音乐播放器。
func New(offsetMs, pollMs int) *SodaMusicPlayer {
	return &SodaMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// coverBudget 是下载封面 base64 的时间预算。标题已先发，封面慢一点无妨。
const coverBudget = 3 * time.Second

// durationGrace 是换歌后等 durationSeconds 就绪的上限，超时就按 0 发。
// 取 2s：真机上时长比歌名晚不到一个轮询到几百毫秒，2s 有充分余量；同时短到即使某个源
// 永远给不出时长，每次换歌也只多等这一下，不会让播放器显得「卡住」。
const durationGrace = 2 * time.Second

// minPollInterval 汽水侧轮询下限（下限，不是目标值：配置写得更大就按配置走）。
//
// **依据是数据源的分辨率，不是单次调用的开销。** 真机实测（2026-08-08）：一次 Extract
// 的往返只要 1.6~8ms，并不重；但它取回的 `progressSeconds` 是 **1Hz 采样**，两次刷新之间
// 反复去问只是重复读同一个值。轮询频率在这里唯一买到的是「新采样落地后多久被发现」这一段
// 相位（≤ 一个轮询间隔），位置精度本身由本地时钟外推提供、与轮询频率无关；逐字高亮更是
// 由前端按 play_time 插值，与此无关。
//
// 200ms 是在这段相位（实测均值 96ms，见 §7.7）与「每秒 33 次往返打在主播的音乐进程上」
// 之间取的折中。要更紧的相位就调小它，但收益上限只有 200ms，别指望它能提高分辨率。
const minPollInterval = 200 * time.Millisecond

// EffectivePollInterval 返回配置轮询间隔为 pollMs 毫秒时汽水实际采用的间隔：低于 minPollInterval 抬到它。
// 轮询循环与 /service-status 的 config_effective 共用此函数，下限只在这里定义。
func EffectivePollInterval(pollMs int) time.Duration {
	d := time.Duration(pollMs) * time.Millisecond
	if d < minPollInterval {
		return minPollInterval
	}
	return d
}

// exitFailGrace：Extract 连续失败满这么久，即按汽水正在退出处理，释放 inspector 会话（issue #188）。
//
// 这是 waitingForDisconnect 事件之外的兜底（AGENTS §7.7.4）：事件没来（旧版 Node 不认订阅、或别的
// 原因）时，会话最多把退出中的主进程多拖这么久。取 10s：取数平时偶有失败（换歌间隙、渲染器繁忙，
// 探针自身的超时是 2.5s），正常运行中连续失败能持续多久没有实测数据，10s 是四个探针超时的量级；
// 误判的代价只是断开后隔一段时间重连（见 reattachCooldown）。托盘播放时取数照常成功（真机上托盘
// 播放进度正常），不会走到这里。
const exitFailGrace = 10 * time.Second

// reattachCooldown：因察觉退出而释放会话后，同一主进程 pid 在这段时间内不激活 inspector、也不连接。
//
// 释放后主进程约 2s 内退出（issue #188 真机）。其间再去激活，是往正在拆除 Node 环境的进程里建远程
// 线程：按 Node 源码，激活入口在环境清理后会断言失败（AGENTS §7.7.4）。15s 给足余量；若其实是误判、
// 汽水还在，冷却期满照常重连。
const reattachCooldown = 15 * time.Second

// sodaDetails 是汽水在「没在读歌」时各种情形的 detail。
func sodaDetails() player.ProcessDetails {
	return player.ProcessDetails{
		NotRunning:   i18n.T("Soda Music not running"),
		Exited:       i18n.T("Soda Music has exited"),
		Disconnected: i18n.T("Soda Music CDP disconnected"),
	}
}

// extractSource 是 Start / runSession 用到的 CDP 客户端方法。生产恒为 *cdp.Client，测试注入按脚本返回的桩。
type extractSource interface {
	Extract() (*cdp.ExtractionData, error)
	IsClosed() bool
	Close()
}

// findMainPID、ensureInspector、connectCDP 是 Start 用到的 watchdog / cdp 入口的可替换 seam：仅供 Start 的
// 离线测试注入桩。生产恒为原函数，不导出。
var (
	findMainPID     = watchdog.FindMainPID
	ensureInspector = watchdog.EnsureInspector
	connectCDP      = func(ctx context.Context) (extractSource, error) {
		c, err := cdp.Connect(ctx)
		if err != nil {
			return nil, err // 不把 nil 的 *cdp.Client 装进接口
		}
		return c, nil
	}
)

// sodaRunning 报告汽水是否在运行：FindMainPID 不返回 ErrNotRunning 即算（主进程读不到、枚举进程出错都按在运行）。
func sodaRunning() bool {
	_, err := findMainPID()
	return !errors.Is(err, watchdog.ErrNotRunning)
}

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *SodaMusicPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// storeRecheck 是检测到微软商店版之后多久重查一次（issue #212）。与网易云、Spotify 的「版本不支持」同值：装的是
// 商店版近乎永久，但用户改装官网版后应当自动恢复、不必重启 PlayerCap。
const storeRecheck = 30 * time.Second

// recheckStep 是商店版重查等待期间判一次汽水是否还在运行的间隔，与外层循环未运行时的等待同为 3 秒：进程退出按外层循环
// 的常规节奏察觉，不等满 storeRecheck（#231，设计 doc/design/converge-player-behavior-231-design.md §3.2）。
const recheckStep = 3 * time.Second

// Start 启动汽水音乐轮询循环（阻塞，须在 goroutine 中调用）。
//
// 每轮先找主进程，非歌名状态一律由 player.ProcessStatus 合成（#231，设计 §3.2）：没有 SodaMusic.exe 报 waiting_process，
// 从未见它运行为 process_not_running、运行过为 process_exited；其余都按在运行报 standby 加原因（主进程的命令行读不到、
// 读不到安装路径为 no_admin，商店版为 version_unsupported；冷却期内、枚举进程出错、inspector 打不开、连 CDP 之前与
// 连不上为 cdp_disconnected，连上之后由 runSession 改报）。商店版按 storeRecheck 重查，其间每 recheckStep 判一次进程，
// 退出不等满重查间隔。
func (p *SodaMusicPlayer) Start() {
	// releasedPID / releasedAt：最近一次因察觉退出而释放会话的主进程与时刻（见 reattachCooldown）。
	var releasedPID int
	var releasedAt time.Time
	d := sodaDetails()
	seen := false           // 本次服务期间见过汽水在运行：区分 process_exited 与 process_not_running
	storeWarned := false    // 微软商店版只警告一次（issue #212）
	exeUnknown := false     // 上一轮读不到汽水的安装路径（issue #212）：只警告一次
	mainUnreadable := false // 上一轮读不到主进程的命令行：只警告一次
	var bt sodaBlockTracker

	// 连接 CDP 随停止中止（cdp.Connect 自身另有单次上限）：9229 端口接受连接而不完成响应时，轮询 goroutine 不被挡在连接里。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer telemetry.Guard()
		select {
		case <-p.StopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		pid, err := findMainPID()
		running := !errors.Is(err, watchdog.ErrNotRunning)
		if running {
			seen = true
		}
		unreadable := errors.Is(err, watchdog.ErrMainUnreadable)
		if unreadable && !mainUnreadable {
			log.Warn("Cannot read the command line of the Soda Music main process; its inspector was not opened. Run Soda Music with the same privileges as this program")
		}
		mainUnreadable = unreadable
		if err != nil || (pid == releasedPID && time.Since(releasedAt) < reattachCooldown) {
			// 未运行——用户可能稍后启动，静默等待重试。在运行而认不出主进程（命令行读不到）报 standby / no_admin；
			// 枚举进程出错、刚释放的那个主进程正在退出（冷却期内）按在运行报 standby，同样等待。
			storeWarned, exeUnknown = false, false
			switch {
			case !running:
				bt.onNotRunning()
			case unreadable:
				bt.onBlocked("no_admin")
			case err != nil:
				bt.onBlocked("cdp_port_not_ready")
			}
			var block player.Blocked
			if unreadable {
				block = player.Blocked{Reason: player.ReasonNoAdmin, Detail: i18n.T("Cannot read the Soda Music main process (Soda Music may be running as administrator)")}
			}
			p.EmitStatusIfChanged(player.ProcessStatus(running, seen, d, block))
			p.Emit(player.EventClearSongData, nil)
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}

		err = ensureInspector(pid)
		var block player.Blocked // 汽水在运行而读不了它的原因；零值即调试通道未就绪
		var packaged *launch.PackagedError
		if errors.As(err, &packaged) {
			block = player.Blocked{Reason: player.ReasonVersionUnsupported, Detail: i18n.T("Soda Music Microsoft Store version unsupported (requires the official installer version)")}
		} else if errors.Is(err, launch.ErrExeUnknown) {
			block = player.Blocked{Reason: player.ReasonNoAdmin, Detail: i18n.T("Cannot read the Soda Music installation path (Soda Music may be running as administrator)")}
		}
		// 状态在前、清空在后（设计 4.3 第 1 条）；内容不变不重发。
		p.EmitStatusIfChanged(player.ProcessStatus(true, true, d, block))
		p.Emit(player.EventClearSongData, nil)
		if packaged != nil {
			// 微软商店版：门禁已在开 inspector（注入）之前挡住。只警告一次、按 storeRecheck 重查，用户改装官网版后
			// 不必重启 PlayerCap。不连接。
			bt.onBlocked("store_install")
			if !storeWarned {
				storeWarned = true
				log.Warn("Soda Music is the Microsoft Store version (%s) and its inspector cannot be opened; skipped Soda Music lyric capture. To use it, uninstall it and install the official installer version", packaged.ExePath)
				telemetry.ReportOnce("sodamusic.store_install", "汽水音乐为微软商店版，无法开启调试接口", nil, map[string]any{"exe_path": packaged.ExePath})
			}
			if stopped, _ := player.RecheckWait(p.StopCh, storeRecheck, recheckStep, sodaRunning); stopped {
				return
			}
			continue
		}
		storeWarned = false
		if errors.Is(err, launch.ErrExeUnknown) {
			// 汽水在运行而读不到它的映像路径：无从确知不是商店版，门禁已拒绝注入。不连接。
			bt.onBlocked("no_admin")
			if !exeUnknown {
				exeUnknown = true
				log.Warn("Cannot read the installation path of Soda Music; its inspector was not opened. Run Soda Music with the same privileges as this program")
			}
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}
		exeUnknown = false
		if err != nil {
			bt.onBlocked("cdp_port_not_ready")
			log.Warn("failed to open SodaMusic inspector: %v", err)
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}

		connectStart := time.Now()
		client, err := connectCDP(ctx)
		if err != nil {
			bt.onBlocked("cdp_port_not_ready")
			log.Warn("CDP connection failed: %v", err)
			if p.sleepOrStop(2 * time.Second) {
				return
			}
			continue
		}
		bt.onRecovered()
		p.setSessionAttachMeta(time.Since(connectStart).Milliseconds(), "")

		log.Info("CDP connected, monitoring playback state")
		exiting := p.runSessionInternal(client, true)
		if exiting {
			releasedPID, releasedAt = pid, time.Now()
		}
		client.Close()

		// 状态在前、清空在后：察觉退出即报 process_exited（会话释放后主进程随即结束），否则按汽水是否还在运行报
		// process_exited 或 cdp_disconnected。
		p.EmitStatusIfChanged(player.ProcessStatus(!exiting && sodaRunning(), true, d, player.Blocked{}))
		p.Emit(player.EventClearSongData, nil)
		if p.sleepOrStop(2 * time.Second) {
			return
		}
	}
}

// runSession 轮询 sharedState 播放态并 emit 事件，直到 CDP 断开、收到停止信号，或察觉汽水正在退出。
// 返回 true 表示察觉退出：调用方须立即关闭 client，汽水主进程才能结束（见 exitFailGrace）。
func (p *SodaMusicPlayer) runSession(client extractSource) bool {
	return p.runSessionInternal(client, false)
}

func (p *SodaMusicPlayer) runSessionInternal(client extractSource, checkProcOnDisconnect bool) bool {
	meta := p.takeSessionAttachMeta()
	rumSess := p.startSessionRUM(meta.version, meta.attachMs)
	disconnectCause := "cdp_closed"
	defer func() {
		p.endSessionRUM(rumSess, disconnectCause)
	}()

	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := EffectivePollInterval(p.pollMs)
	var trackSeq uint64

	// failSince 是这一段连续取数失败的起点，取数成功即清零。
	var failSince time.Time

	// lastMediaID 是已宣告的那首歌，空 = 没有（会话刚建立，或因没有歌曲已清空）。announced 是本会话宣告过歌曲，
	// 清空之后仍为真：换歌分支「是不是本会话第一首」的判据用它，不用 lastMediaID。
	var lastMediaID string
	var announced bool
	// clearData：读到没有歌曲时待入队的 clear_song_data。通道满时 Emit 直接丢弃，而清空之后 lastMediaID 已复位、不会再判
	// 一次，丢了就再没有机会补，服务端缓存里一直留着上一首。故它在没有歌曲的拍上逐拍重试，且只在 waiting_song 已入队之后
	// 才发；之间读到歌曲即不再补（新歌的 all_lyrics 与 song_info_update 覆盖清空要清的全部缓存字段）。会话开始即待发：
	// 上一次连接宣告过的歌可能仍留在画面上（断开时缓存已清而画面按决定保留，设计
	// doc/design/converge-player-behavior-231-design.md 4.5），连上而没有歌曲同样要清。
	clearData := true
	var lastPlaying = -1 // -1 未知 / 0 暂停 / 1 播放
	// pendingID / pendingSince / pendingRawFresh：正在等就绪的那首歌、从哪一刻开始等、
	// 以及**这首歌出现之后有没有见过新采样**（见换歌分支的两道就绪闸）。
	var pendingID string
	var pendingSince time.Time
	var pendingRawFresh bool
	// throttleWarned 保证「节流没关掉」只告警一次——它每轮都会重新上报，不设闸会刷屏。
	var throttleWarned bool
	// loading 是本轮的 isLoading，供 livePos 判定「该不该继续外推」。
	var loading bool
	var currentLyrics []krc.Line
	// currentLyricItems 是本首歌**已发出的那份** all_lyrics.lyrics。留着它只为一件事：
	// 时长晚到时按同一份歌词重发一次 all_lyrics（见下面的时长补取）。
	var currentLyricItems []player.LyricLine
	var lastLineIdx = -1
	var currentDurationSec float32
	var currentTitle string
	var currentPureHint string

	// 每首歌一个封面 goroutine，换歌/退出时由 ctx 取消——迟到的封面回写不会污染下一首/清屏后状态。
	// 保持 coverCancel 的显式调用（别包 defer/helper），让 go vet 的 lostcancel 看得见（对照 kugou.go）。
	var coverCancel context.CancelFunc

	// romaCh 是「从酷狗借音译」的 goroutine → 主循环回传通道，每首歌一个、容量 1（见 roma.go）。
	// 换歌时重建，旧 channel 连同其上迟到的结果一起弃置——故无需 ctx/代次淘汰。nil（换歌前）时
	// 下面的非阻塞 select 走 default，安全。
	var romaCh chan []krc.Line

	// 本地时钟外推：锚点进度 + 自锚点起的墙钟流逝，平滑两次轮询间的进度。
	var anchorProgressSec float32
	var anchorTime time.Time
	isPlaying := false

	// lastRawSec 是上一次见到的 sharedState 原始采样值，用来判定「这一轮是不是新采样」。
	//
	// **承重，别退回「每轮都落锚」**：汽水的 progressSeconds 是 ~1Hz 采样——真机实测
	// （2026-08-08，连采 60 次变化）Δt 970~1060ms、Δv +1.0000±0.01，而值本身是采样瞬间的
	// 真实位置（形如 45.519，毫秒精度，不是整秒量化）。也就是说两次采样之间它一直是旧值，
	// 陈旧程度 0~1s。只有在**它变化时**重新落锚，锚点之后的墙钟流逝才是真的「已经又播了多久」；
	// 每轮都落锚会把 time.Since(anchorTime) 清零 → 外推形同虚设 → position / progress / 歌词
	// 行匹配一起退化成 1s 阶梯、系统性滞后 0~1s（均值 0.5s）。
	//
	// 这是照 kugou 骨架时**不能照抄**的一处：酷狗的进度是 100ns 计时器直读
	// （kugou.go:368 `progressRaw/1e7`），每轮都是新鲜值，落不落锚没有差别。
	//
	// 真正的分量在极端情形：汽水窗口最小化久了会被 Chromium 后台节流，采样间隔从 1s 掉到
	// **60s**（实测 dv=+60.0004 / dt=59990ms）。那时每轮落锚 = 进度整整冻结一分钟再跳 60s
	// （实测「检测到前跳: 0.00s → 28.01s」，歌词在第 0 行卡了 28 秒）；边沿落锚则照常平滑推进，
	// 因为 60s 内音频时钟与墙钟只差 0.4ms。详见 AGENTS §7.7.1 / §7.7.2。
	var lastRawSec float32 = -1 // -1 = 本会话尚未见过任何采样

	// livePos 返回「此刻」的整曲实时位置（秒）：播放中 = 锚点 + 自锚点起的墙钟流逝，
	// 暂停 = 锚点原值；有时长则钳到时长。position / progress / 歌词匹配三处共用它，
	// 避免同一个量在三处各写一遍钳制而分叉。
	livePos := func() float32 {
		v := anchorProgressSec
		// 缓冲中不外推：那时 isPlaying 仍为 true 而 progressSeconds 停更，再按墙钟往前推
		// 就是纯粹的凭空前冲（歌词翻过几行而音频没动），缓冲结束还会因 drift 被判成一次假回跳。
		// **判据用平台自己给的 isLoading，不新造超时阈值**——它本来就在载荷里，此前被解析后丢弃。
		// 真机多轮采样中播放态下它恒为 false，所以这道闸不会误伤正常播放。
		if isPlaying && !loading {
			v += float32(time.Since(anchorTime).Seconds())
		}
		if currentDurationSec > 0 && v > currentDurationSec {
			v = currentDurationSec
		}
		return v
	}

	for {
		select {
		case <-p.StopCh:
			disconnectCause = "stopped"
			if coverCancel != nil {
				coverCancel()
			}
			return false
		default:
		}

		if client.IsClosed() {
			if checkProcOnDisconnect && !sodaRunning() {
				disconnectCause = "process_exited"
			} else {
				disconnectCause = "cdp_closed"
			}
			if coverCancel != nil {
				coverCancel()
			}
			return false
		}

		data, err := client.Extract()
		if err != nil {
			// 提取失败多是瞬时的（换歌间隙、渲染器繁忙），原地重试；连接真断了才退出重连。
			//
			// 汽水退出时不是瞬时的：主进程停在 Node 退出前的等待里，直到这条会话断开（issue #188，
			// AGENTS §7.7.4）。inspector 宣告进入这一等待（ErrHostExiting），或取数连续失败满
			// exitFailGrace，都按退出处理，交 Start 释放会话。
			if failSince.IsZero() {
				failSince = time.Now()
			}
			hostExiting := errors.Is(err, cdp.ErrHostExiting)
			exiting := hostExiting || time.Since(failSince) >= exitFailGrace
			if !exiting && !client.IsClosed() {
				time.Sleep(pollInterval)
				continue
			}
			if hostExiting {
				disconnectCause = "host_exiting"
				log.Info("SodaMusic main process is exiting; releasing the inspector session")
			} else if exiting {
				disconnectCause = "fail_grace_10s"
				log.Warn("SodaMusic data unavailable for %s (%v); treating it as exiting and releasing the inspector session", exitFailGrace, err)
			} else if checkProcOnDisconnect && !sodaRunning() {
				disconnectCause = "process_exited"
			} else {
				disconnectCause = "cdp_closed"
			}
			if coverCancel != nil {
				coverCancel()
			}
			return exiting
		}
		failSince = time.Time{}

		// 没有歌曲（首页、清空播放列表）：当拍报 waiting_song 并清歌曲数据（issue #193，设计 4.3 第 2、3 条），宣告过的歌
		// 当拍复位，不按时长保留：汽水换歌时是否有没有歌曲的一刻未实测，已知的换歌过渡是 mediaId 先变、歌名晚到（见换歌
		// 分支），不走这里。不能静默等待：路由不让 playing 超时（只有 loading 与暂停中的占位会超时），已宣告的歌会一直
		// 停在那首。清空后各变量回到会话刚建立时的值，announced 除外；同一首歌再出现按换歌重新宣告。汽水退出不经过这里：
		// 那时取数只会失败，见上。
		//
		// 已在下载的封面清空时由 coverCancel 取消。残余：封面协程通过 ctx 检查之后、清空之前的极窄窗口里，它的
		// song_info_update 可能落在 clear_song_data 之后；换歌时取消上一首的封面同样如此（既有）。
		if data.MediaID == "" && data.Name == "" {
			if lastMediaID != "" {
				if coverCancel != nil {
					coverCancel()
				}
				romaCh = nil
				lastMediaID, pendingID, lastPlaying, isPlaying, lastLineIdx = "", "", -1, false, -1
				currentLyrics, currentLyricItems, currentDurationSec, currentTitle, currentPureHint, anchorProgressSec = nil, nil, 0, "", "", 0
				clearData = true
			}
			// 先报状态、后清空，同 Start。状态经 EmitStatusIfChanged：持续没有歌曲时不随轮询重发，没入队的下一拍补发。
			// 清空看最近一次入队的状态是不是 waiting_song：本拍刚入队，或此前已入队、本拍被去重（中间的歌名状态因通道满
			// 被丢弃时即如此），都算就位；看 EmitStatusIfChanged 的返回值会把后者当成没入队，清空就永远等不到。
			p.EmitStatusIfChanged(player.NoSongStatus(player.ReasonNoSong, i18n.T("Soda Music has no current song")))
			if last, _ := p.LastStatus(); clearData && last.Reason == player.ReasonNoSong {
				clearData = !p.TryEmit(player.EventClearSongData, nil)
			}
			time.Sleep(pollInterval)
			continue
		}

		progressSec := data.ProgressSeconds
		durationSec := data.DurationSeconds

		// 本轮是不是新采样（决定能否落锚，见 lastRawSec）。无条件记录：非法值也算「见过」，
		// 它再变成合法值时必然与此不同，不会漏掉一次落锚。
		rawChanged := progressSec != lastRawSec
		lastRawSec = progressSec
		if rawChanged {
			pendingRawFresh = true
		}
		loading = data.IsLoading

		if data.Throttled && !throttleWarned {
			throttleWarned = true
			rumSess.onCompat("background_throttling_stuck", "")
			log.Warn("SodaMusic renderer background throttling could not be disabled: after the window stays minimized, the progress source drops to 1/60Hz, so seek/pause may take up to 60s to be detected")
		}

		// ── 换歌检测（稳定 MediaID）──
		if data.MediaID != "" && data.MediaID != lastMediaID {
			// mediaDetail（名/歌手/歌词/封面）是异步加载的：换歌瞬间 mediaId 先更新，有个窗口
			// name/歌词仍为空。此刻**别提交这次换歌**——否则空名标题与空歌词会「定格」到下次换歌
			// （现象：日志打「♪ 歌曲:  -  (id: …)」，切走再切回才好）。不更新 lastMediaID，静默跳过
			// 本轮，下轮 mediaDetail 就绪（name 非空）再正常处理。符合「未就绪则重试、不向外发半成品」。
			if data.Name == "" {
				time.Sleep(pollInterval)
				continue
			}
			if data.MediaID != pendingID {
				pendingID = data.MediaID
				pendingSince = time.Now()
				pendingRawFresh = false
			}
			// 时长与歌名**不同源**：名/歌手/歌词来自 mediaDetail，时长来自播放器自身的
			// durationSeconds，实测会晚到——真机录音里出现过「name/歌词/封面都齐了、
			// durationSeconds 仍是 0」的一轮。此时若照发，all_lyrics 就带着 duration=0 出去，
			// 而 ClampProgress 的 `duration <= 0` 闸会把 progress 钉死在 0，**且 all_lyrics
			// 每首只发一次、不自愈**：中途连入的前端整首拿到进度条归零。故再等它一会儿。
			//
			// **宽限必须有上限**，这也是它与上面「等 name」的区别：name 为空 = 没有任何可显示
			// 的东西，等多久都值；时长为 0 只是降级，而拿不到时长的源（电台/直播流）如果因此
			// 永不上报，就是把一个显示缺陷换成了整个播放器静默消失。超时就按 0 发，
			// 后续由下面的时长补取修正 progress。
			if durationSec <= 0 && time.Since(pendingSince) < durationGrace {
				time.Sleep(pollInterval)
				continue
			}
			// 先记「这是不是本会话的第一首」，再改 announced —— 顺序反了这个判据永远为假。因没有歌曲而清空之后
			// 再来的歌不算第一首，照常过下面的新鲜度闸：它出现那一拍的读数可能仍是清空之前的旧值。
			firstSong := !announced
			announced = true
			lastMediaID = data.MediaID
			lastLineIdx = -1
			currentDurationSec = durationSec
			isPlaying = data.IsPlaying
			// 落锚同样要过「采样是否新鲜」这道闸（与暂停分支、进度分支同一判据）。
			//
			// **isProgressValid 在这里拦不住陈旧值**：它只挡 `progressSec > duration*1.05`，
			// 而上一首停在 200s、新歌时长 ≥191s 就整个放行——流行歌普遍 180~300s，最常见的情形
			// 恰恰漏过。progressSeconds 是 1Hz 采样，换歌瞬间 mediaId 先更新、它仍是**上一首**的
			// 收尾值；照单全收就会让 all_lyrics 带着 Position≈200 / Progress≈0.9 随新歌发出，
			// 而 all_lyrics 每首只发一次不重发 —— overlay 据此起自由时钟，从新歌的 200s 处开始
			// 高亮，约 1s 后下一次采样才触发「检测到回跳」拉回 0。换歌瞬间歌词从末段闪回开头。
			//
			// 没见过新采样就锚 0：换歌后真实位置本就是 0 附近，这是唯一安全的默认。
			//
			// **只对「从一首歌换到另一首」设这道闸**（firstSong 为假时）。会话刚建立那一次
			// 没有「上一首」——此时的 progressSeconds 就是当前这首的值，只是最多陈旧 1s，
			// 照单收下才对。一律锚 0 会让中途启动的服务先报 position=0、再被下一次采样判成
			// 一次「前跳」（真机实测：`检测到前跳: 0.00s → 61.14s`），而服务在歌曲中途启动
			// 恰恰是最常见的开播姿势。
			freshEnough := pendingRawFresh || firstSong
			if freshEnough && isProgressValid(progressSec, durationSec) {
				anchorProgressSec = progressSec
			} else {
				anchorProgressSec = 0
			}
			anchorTime = time.Now()

			name := data.Name
			singer := strings.Join(data.Artists, " / ")
			currentTitle = buildTitle(name, singer)

			log.Info("♪ Song: %s - %s (id: %s)", name, singer, data.MediaID)

			// 换歌瞬间发一次真实播放态。isPlaying 是明确布尔（非酷狗那种会塌成 standby 的过渡态），
			// playing/paused 都是正常态、不会被路由判 idle，直接发。
			if isPlaying {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
			}
			lastPlaying = boolToInt(isPlaying)

			// 封面：取消上一首、起本首。URL 已随 Extract 就绪，直接传。
			trackSeq++
			waitCover := data.CoverURL != ""
			rumSess.beginTrack(trackSeq, waitCover)
			if coverCancel != nil {
				coverCancel()
			}
			var ctx context.Context
			ctx, coverCancel = context.WithCancel(context.Background())
			go p.runCoverFetchWithRUM(ctx, data.CoverURL, name, singer, currentTitle, rumSess, trackSeq)

			// 歌词：明文 KRC 解析 + 翻译轨合并 + 套 offset，逐字有无由 detailedFlag 如实反映。
			lyricStart := time.Now()
			currentLyrics = parseSodaLyrics(data, offsetSec)
			if len(currentLyrics) > 0 {
				log.Info("Lyrics loaded: %d lines; per-character: %s", len(currentLyrics), detailedFlag(currentLyrics))
			} else {
				log.Info("Instrumental / no lyrics; word-by-word: %s", detailedFlag(currentLyrics))
			}

			lyricItems := toLyricLines(currentLyrics, offsetSec)
			initPlay := anchorProgressSec
			progress := player.ClampProgress(initPlay, currentDurationSec)

			// 若平台只返回一句提示语（或歌名行加提示语），归一成 index:-1。
			currentPureHint = ""
			if hint, ok := player.PureMusicHint(lyricItems, currentTitle); ok {
				log.Info("platform returned only the hint \"%s\", treating as no lyrics", hint)
				currentPureHint = hint
				currentLyrics = nil
				lyricItems = []player.LyricLine{} // 显式空 slice——nil 会序列化成 null
			}

			lyricSource := "none"
			if len(currentLyrics) > 0 || currentPureHint != "" {
				lyricSource = "online"
			} else if data.Err != "" {
				lyricSource = "err"
			}
			romaInitOutcome := "skipped"
			if len(currentLyrics) > 0 {
				romaInitOutcome = "pending"
			}
			rumSess.onTrackLyricReady(trackSeq, lyricSource, currentLyrics, currentPureHint, time.Since(lyricStart).Milliseconds(), waitCover, romaInitOutcome)

			currentLyricItems = lyricItems
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: initPlay,
				Progress: progress,
				Lyrics:   lyricItems,
				Count:    len(lyricItems),
			})
			if len(currentLyrics) == 0 {
				// index:-1 的 lyric_update 必须显式带 Progress（契约：整首播到哪；漏写零值兜 0 会与
				// all_lyrics.progress 自相矛盾——lyricupdate_lint_test 全仓强制）。
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: currentPureHint, Timestamp: 0, Position: initPlay, Progress: progress,
				})
			}

			// 音译异步补发：汽水平台无音译源，事后从酷狗按歌名/时长借一份、按主歌词文本对齐补进
			// RomaText（详见 roma.go）。只在**有主歌词可对齐**时起（纯音乐/无词无从对齐）。每首歌一个
			// 通道，传 currentLyrics 的快照给 goroutine 独占改写，避免与本循环抢 currentLyrics。
			romaCh = make(chan []krc.Line, 1)
			if len(currentLyrics) > 0 {
				snapshot := append([]krc.Line(nil), currentLyrics...)
				go p.fetchKugouRomaWithRUM(romaCh, snapshot, name, singer, int(currentDurationSec*1000), rumSess, trackSeq)
			}
		}

		// 音译补发落地：goroutine 借酷狗对齐后把整份歌词经 romaCh 交回，本循环（currentLyrics 的
		// 单写者）换掉它并重发一次 all_lyrics，随后 lastLineIdx 归 -1 逼下一轮按含音译的当前行重发
		// lyric_update——如此 all_lyrics 与其后每条 lyric_update 都带音译，与其余各家同源一致。
		// 非阻塞收：没到就跳过；对齐 0 行时 goroutine 根本不送，这里恒空转（不重发）。
		// 重发一次 all_lyrics 是既有受支持的动作（对照下面「时长晚到」补发）。
		select {
		case merged := <-romaCh:
			currentLyrics = merged
			currentLyricItems = toLyricLines(merged, offsetSec)
			lastLineIdx = -1
			pos := livePos()
			log.Info("Romanization filled in; re-emitting all_lyrics (%d lines)", len(currentLyricItems))
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: pos,
				Progress: player.ClampProgress(pos, currentDurationSec),
				Lyrics:   currentLyricItems,
				Count:    len(currentLyricItems),
			})
		default:
		}

		// 时长补取：换歌那一轮已经等过 durationGrace，绝大多数情况这里是空转。它是宽限超时那条
		// 路径的兜底 —— 那时 currentDurationSec 会是 0，而 ClampProgress 的 `duration <= 0` 闸
		// 会让 progress 恒 0（真机录音里实见过整首 progress=0）、插值也失去上限。
		//
		// **补到就必须重发一次 all_lyrics，不能只补内部时钟。** 服务端缓存里 `ps.Duration` 只由
		// all_lyrics 写，而 `ps.Progress` 由 all_lyrics、lyric_update 与 playback_pause/resume 都写
		// （server.go 的 UpdatePlayerState）。只补内部时钟的话，此后每条 lyric_update 会把 progress 写成非 0，
		// 而 duration 永远停在 0 —— 缓存里同时存在 `duration:0` 与 `progress:0.57`，
		// buildInitEvents / FullState / HTTP `/all_lyrics` 会把这个**自相矛盾**的载荷发给中途连入
		// 的每一个前端。补之前两个字段同为 0（错但自洽），只补一半反而把降级态变成了不自洽态，
		// 而 server.go 的注释正把这类组合列为已修过的 bug 形状。重发只在这条罕见路径上发生一次。
		//
		// **只在从未取到时补**，不去追后续变化：换歌之外的时长变化没有已知来源，跟着变只会平白
		// 引入抖动，还会把 all_lyrics 变成高频事件。
		if currentDurationSec <= 0 && durationSec > 0 {
			currentDurationSec = durationSec
			pos := livePos()
			prog := player.ClampProgress(pos, currentDurationSec)
			log.Info("duration arrived late; re-sending all_lyrics (duration=%.2fs)", currentDurationSec)
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: pos,
				Progress: prog,
				Lyrics:   currentLyricItems,
				Count:    len(currentLyricItems),
			})
			// 重发 all_lyrics 会清掉服务端缓存的 lyric_update（server.go）；无歌词曲目须紧跟补发
			// index:-1 并保留当前曲目的提示原文，否则无歌词结论与提示语从服务端缓存永久丢失。
			if len(currentLyrics) == 0 {
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: currentPureHint, Timestamp: 0, Position: pos, Progress: prog,
				})
			}
		}

		// ── 播放状态变化检测 ──
		curPlaying := boolToInt(data.IsPlaying)
		if curPlaying != lastPlaying {
			lastPlaying = curPlaying
			if data.IsPlaying {
				isPlaying = true
				// **与暂停分支同一道闸**：只有本轮刚刷新的采样才比现有锚点可信。
				// 暂停时锚点已被冻结成外推出来的准确位置；恢复时若无条件采纳 progressSec，
				// 而平台的精确补发要约 0.7s 才到，拿到的就是暂停前那个最多滞后 1s 的旧值 ——
				// 位置当场倒退最多 1s，playback_resume 报小，跨行时还会重发上一行歌词
				// （OBS 上歌词退一行、约 1s 后再跳回来）。采样没刷新就什么都不做，锚点原样保留。
				if rawChanged && isProgressValid(progressSec, currentDurationSec) {
					anchorProgressSec = progressSec
				}
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("resumed @ %.2fs", anchorProgressSec)
			} else {
				// 冻结位置：**只有本轮刚刷新的采样才比外推值可信**。progressSeconds 是 1Hz 的，
				// 上一轮那个旧采样最多滞后 1s，直接拿它当冻结点会把「停在哪」写早近一秒。
				// 暂停瞬间平台会补发一次精确采样（实测约 0.7s 后到达），那一次由下面的
				// rawChanged 分支采纳，把锚点修正到真值。
				frozen := livePos()
				if rawChanged && isProgressValid(progressSec, currentDurationSec) {
					frozen = progressSec
				}
				isPlaying = false
				anchorProgressSec = frozen
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
				log.Info("paused @ %.2fs", anchorProgressSec)
			}
		}

		// ── 进度更新 + seek 检测 ──
		//
		// **落锚的门是 rawChanged，不是「每轮」**（理由见 lastRawSec）。暂停时也落锚：那正是
		// 暂停瞬间补发的精确采样、以及暂停期间 seek 的唯一入口。seek 判定则只在播放中做——
		// 暂停时外推时钟不走，drift 恒等于「新采样 - 冻结值」，拿 3s 阈值去判会把一次正常的
		// 暂停补发当成跳转。
		if rawChanged && isProgressValid(progressSec, currentDurationSec) {
			if isPlaying {
				before := livePos()
				drift := progressSec - before
				if drift < -3.0 {
					log.Info("Seek-back detected: %.2fs → %.2fs", before, progressSec)
					lastLineIdx = -1
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
				} else if drift > 3.0 {
					log.Info("forward seek detected: %.2fs → %.2fs", before, progressSec)
					lastLineIdx = -1
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
				}
			}
			anchorProgressSec = progressSec
			anchorTime = time.Now()
		}

		// ── 本地时钟插值 ──
		interpSec := livePos()

		matchTime := player.LyricMatchTime(interpSec, offsetSec)

		// ── 歌词行匹配 → lyric_update ──
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
				// interpSec 进 Position/算 Progress；play_time 由 BuildLyricUpdate 按歌词时间轴算。
				// words 的 play_time 已在 applyDetailedOffset 里套过 offset，这里透传。
				p.Emit(player.EventLyricUpdate, lineUpdate(trueIdx, line, offsetSec, interpSec, currentDurationSec))
			}
		}

		// 位置采样（见 BaseEmitter.EmitPositionSample）：播放中按固定间隔报一次，校准服务端缓存里的位置
		// ——采样变化时重新落锚，3s 以内的偏差直接吸收、不发事件，无歌词的歌又没有歌词行可推。
		// 缓冲中（loading）照发：那时状态仍是 playing、livePos 停住，而服务端仍按墙钟外推，采样把它的
		// 超前量限制在一个间隔以内。传 interpSec，与上面 lyric_update 的 position 同一个量；offset 只进了
		// matchTime，这里不带。
		if isPlaying {
			p.EmitPositionSample(time.Now(), interpSec, currentDurationSec)
		}

		time.Sleep(pollInterval)
	}
}

// runCoverFetch 发歌曲信息与封面；每首歌一个，换歌时由 ctx 取消。
//
// 先发一条只带 URL、不带 base64 的信息让标题与封面尽快上屏，再下载 base64 补发完整信息。
// **不变量：即使 b64 拿不到（超时/过大/失败）也照发带 URL 的信息**——绝不为封面吞掉歌曲信息，
// 否则 OBS 整首停在上一首（轮询只在换歌时发、不自愈）。FetchCoverBase64 图片超限时本就返回空 URL 照发。
//
// **第一条必须带上 coverURL，别再退回「不含封面」。** 前端（旧版歌词页 lyric_page.html，4.0 起是线上 v3 的冻结副本，源码在前端仓库的 public/v3/；其
// updateSongInfoRow）用 `info.cover_base64 || info.cover` 取封面源，为空就走
// `coverArt.classList.remove('visible')` —— 那是**无过渡的硬隐藏**。于是换歌时前端先被这条
// 空封面把旧封面瞬间抹掉，等第二条 base64 到了又因为此刻 `visible` 已被摘掉而落进「首次显示」
// 分支静默贴上：现象就是封面闪一下、既没有淡出也没有淡入（切播放器与网易云自己切歌都正常，
// 唯独汽水自己切歌闪，差别就在这一条）。
//
// 带上 URL 后：第一条的 cover 非空 → 前端走淡出/淡入过渡；第二条的 `info.cover` 与第一条相同 →
// 前端的 isSameSong 判定为真 → 只静默把图源升级成 base64，不会再放一次动画。这正是前端那套
// isSameSong 逻辑预设的用法，cloudmusic 一直就是这么发的（cloudmusic.go 的首条带 Cover）。
//
// 对照 kugou：它首条不带封面是**因为那时确实还没拿到 URL**（`if coverURL == ""` 才发那一条）；
// 汽水的 URL 随 Extract 一起到、就在入参里，没有理由丢掉。
func (p *SodaMusicPlayer) runCoverFetch(ctx context.Context, coverURL, name, singer, title string) {
	p.runCoverFetchWithRUM(ctx, coverURL, name, singer, title, nil, 0)
}

func (p *SodaMusicPlayer) runCoverFetchWithRUM(ctx context.Context, coverURL, name, singer, title string, rumSess *sodaSessionRUM, trackSeq uint64) {
	defer telemetry.Guard()
	select {
	case <-ctx.Done():
		return
	default:
	}
	// 先发标题 + 封面 URL（不含 base64）
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL,
	})

	if coverURL == "" {
		return // 汽水一般都给封面；确实没有则标题已发，无新东西可补
	}
	b64, coverOut := player.FetchCoverBase64ContextWithOutcome(ctx, "Soda", coverURL, coverBudget)

	select {
	case <-ctx.Done():
		return
	default:
	}
	rumSess.onCoverOutcome(trackSeq, coverOut)
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL, CoverBase64: b64,
	})
}

// parseSodaLyrics 把 Extract 里的明文 KRC 解析成带逐字的 Line，合并翻译轨，并套 offset。
// 非 KRC / 空内容 → 返回 nil（按无歌词处理）。汽水实测歌词恒为 krc 型明文。
func parseSodaLyrics(data *cdp.ExtractionData, offsetSec float32) []krc.Line {
	if data.LyricType != "krc" || data.LyricContent == "" {
		return nil
	}
	lines := krc.ParsePlainKRC(data.LyricContent)
	if len(lines) == 0 {
		return nil
	}
	// 音译轨（KRC 内嵌 type=0）本应由 ParsePlainKRC 写进 RomaText，但汽水平台的 KRC 实测不含
	// 内嵌轨，故此处 RomaText 恒空——音译改由 fetchKugouRoma 事后从酷狗借来补入（见 roma.go），
	// 不在本同步路径里。此处只把独立翻译轨（tlyric）按时间戳合并进 SubText。applySodaTranslations
	// 只碰 SubText、绝不动 RomaText，两轨互不干扰。
	applySodaTranslations(lines, data.TranslationLRC)
	applyDetailedOffset(lines, offsetSec)
	return lines
}

// buildTitle 组「歌手 - 歌名」（与 kugou 一致）。歌手为空则只留歌名。
func buildTitle(name, singer string) string {
	if singer == "" {
		return name
	}
	return fmt.Sprintf("%s - %s", singer, name)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isProgressValid 判断 progressSec 是否在合理播放范围 [0, duration*1.05]，过滤异常值。
func isProgressValid(progressSec, durationSec float32) bool {
	if progressSec < 0 {
		return false
	}
	if durationSec > 0 && progressSec > durationSec*1.05 {
		return false
	}
	return true
}

// toLyricLines 把解析结果映射成对外 LyricLine（subText/romaText/detailed 透传给 BuildLyricLine）。
// RomaText 是内嵌 KRC type=0 音译轨（平台下发则由 ParsePlainKRC 填、无则空）或事后从酷狗借来的
// （见 roma.go），与 SubText（独立 tlyric 翻译）是两条独立轨、互不覆盖。逐字音译（RomaDetailed）
// 进 roma_text_detailed：BuildLyricLine 不填它，这里赋值。
func toLyricLines(lines []krc.Line, offsetSec float32) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec)
		out[i].RomaTextDetailed = l.RomaDetailed
	}
	return out
}

// lineUpdate 把一行歌词映射成 lyric_update 的载荷，字段口径同 toLyricLines：两者取自同一份
// currentLyrics，同一行的逐字音译在 all_lyrics 与 lyric_update 里相同。借来的音译补发时主循环先换掉
// currentLyrics、再重发 all_lyrics，其后的 lyric_update 取的就是带逐字音译的那一份。
func lineUpdate(index int, l krc.Line, offsetSec, playPos, duration float32) *player.LyricUpdate {
	u := player.BuildLyricUpdate(index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec, playPos, duration)
	u.RomaTextDetailed = l.RomaDetailed
	return u
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time。
//
// **必须在解析之后、存进 currentLyrics 之前套一次**：BuildLyricLine/BuildLyricUpdate 只算行级
// play_time，TextDetailed 原样透传（对照 kugou.go 的同名函数）。漏了这步逐字高亮会整体偏一个
// offset 而行级却对，症状极像"前端没对齐"。只对新解析的歌词套一次，别重复套。
//
// 逐字音译（RomaDetailed）一并套：平台的 KRC 若内嵌 type=0 轨，krc 解析出的逐字音译同样没填 PlayTime。
// 从酷狗借来的那一份不经这里——它构造时直接取汽水主歌词各个字已套过 offset 的 PlayTime（roma.go 的
// charRomaDetailed），两条路得到的值相同。
func applyDetailedOffset(lines []krc.Line, offsetSec float32) {
	for i := range lines {
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

// detailedFlag 报告这批歌词**实际**有没有逐字（AGENTS §6.1：如实报告，绝不写死）。
func detailedFlag(lines []krc.Line) string {
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}
