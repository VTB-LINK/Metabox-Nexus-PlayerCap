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
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
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

// elevationBackoffs 第 n 次提权失败后等多久再试；用尽即终态放弃。
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

// record 记一次提权失败，返回本次该等多久、以及是否该终态放弃。
func (g *elevationGate) record() (time.Duration, bool) {
	g.fails++
	if g.fails > len(elevationBackoffs) {
		return 0, true
	}
	return elevationBackoffs[g.fails-1], false
}

// reset 在提权成功后清零，使下一次失败重新从最短退避算起。
func (g *elevationGate) reset() { g.fails = 0 }

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
// 退避最长 2min，不可打断的话会让退出卡住这么久。
func (p *KuGouPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// Start 启动酷狗音乐轮询循环（阻塞）
func (p *KuGouPlayer) Start() {
	var elevation elevationGate
	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Detail: i18n.T("KuGou not running or CDP not ready"), Reason: player.ReasonProcessNotRunning})
		p.Emit(player.EventClearSongData, nil)

		// 自动检测并修复：若 libcef.dll 未 patch 则 kill → patch → 重启酷狗
		if err := watchdog.EnsurePatched(p.StopCh); err != nil {
			select {
			case <-p.StopCh:
				return
			default:
			}
			if errors.Is(err, watchdog.ErrInstallNotFound) {
				log.Error("Auto-repair failed: %v (run the KuGou launch tool manually or check the install path)", err)
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: i18n.T("KuGou installation not found, stopped"), Reason: player.ReasonInstallNotFound})
				return
			}
			if errors.Is(err, watchdog.ErrVersionUnsupported) {
				// 版本指纹不认识：重试也只会反复弹 UAC 打错版本，终态停止。
				log.Error("Auto-repair failed: %v (downgrade to a supported KuGou version, or retry after adaptation)", err)
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: i18n.T("KuGou libcef.dll version unsupported, stopped"), Reason: player.ReasonVersionUnsupported})
				return
			}
			if errors.Is(err, watchdog.ErrElevationFailed) {
				// 退避后重试；用尽即终态。直接 continue，不去 waitForCDP——没提权就没 patch，
				// CDP 不可能起来，而 waitForCDP 在酷狗没跑时会立即返回，等于 2s 后又弹 UAC。
				backoff, giveUp := elevation.record()
				if giveUp {
					log.Error("Failed to obtain administrator rights %d times in a row; stopping auto-repair (retrying only makes the UAC secure desktop repeatedly steal focus). To enable KuGou, restart this program and click \"Yes\" on the UAC prompt", len(elevationBackoffs)+1)
					p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: i18n.T("Administrator rights not obtained, auto-repair stopped"), Reason: player.ReasonNoAdmin})
					return
				}
				log.Warn("Administrator rights not obtained (attempt %d/%d): %v — retrying in %v",
					elevation.fails, len(elevationBackoffs)+1, err, backoff)
				if p.sleepOrStop(backoff) {
					return
				}
				continue
			}
			log.Warn("Auto-repair failed: %v (run the KuGou launch tool manually or check the install path)", err)
			// 其他错误继续等待——用户可能手动启动酷狗
		} else {
			elevation.reset() // 提权成功（或本就无需提权）：下次失败重新从最短退避算起
		}

		client, err := p.waitForCDP()
		if err != nil {
			select {
			case <-p.StopCh:
				return
			default:
			}
			// CDP 等待超时（酷狗可能已退出），重新走 EnsurePatched 检测
			log.Warn("CDP not ready; rechecking KuGou status...")
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("CDP connected, monitoring playback state")
		p.runSession(client)
		client.Close()

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("KuGou CDP disconnected"), Reason: player.ReasonCDPDisconnected})
		p.Emit(player.EventClearSongData, nil)
		log.Info("Session ended; waiting for KuGou to restart...")
		time.Sleep(2 * time.Second)
	}
}

// waitForCDP 轮询 CDP 端口直到酷狗响应，返回已连接的 client。
// 若酷狗进程退出则立即返回错误；90s 超时作为兜底。
func (p *KuGouPlayer) waitForCDP() (*cdp.Client, error) {
	deadline := time.Now().Add(90 * time.Second)
	nextLog := time.Now()
	for {
		select {
		case <-p.StopCh:
			return nil, i18n.Errorf("stopped")
		default:
		}

		client, err := cdp.Connect()
		if err == nil {
			return client, nil
		}

		// 进程已退出则立即返回，不再等待 90s
		if !watchdog.IsKuGouRunning() {
			return nil, i18n.Errorf("KuGou process has exited")
		}

		now := time.Now()
		if now.After(nextLog) {
			log.Info("Waiting for KuGou CDP port 12233... (%v)", err)
			nextLog = now.Add(10 * time.Second)
		}
		if now.After(deadline) {
			return nil, i18n.Errorf("CDP not ready within 90s: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// playInfoSource 是 runSession 用到的 CDP 客户端方法。生产恒为 *cdp.Client，会话测试注入按脚本返回读数的桩。
type playInfoSource interface {
	GetPlayInfo() (*cdp.PlayInfo, error)
	IsClosed() bool
}

// fetchLyrics 是 klyric.Fetch 的可替换 seam：仅供会话测试避开联网取词。生产恒为 klyric.Fetch，不导出。
var fetchLyrics = klyric.Fetch

// runSession polls PlayInfo and emits events until the CDP connection drops.
func (p *KuGouPlayer) runSession(client playInfoSource) {
	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := time.Duration(p.pollMs) * time.Millisecond

	var lastHash string   // detect song change
	var lastStatus string // detect play/pause/stop change
	var currentLyrics []klyric.Line
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
	// goroutine 都不带 context（非测试代码里 WithCancel 只有这里与 sodamusic 的 runSession），因此
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

	for {
		select {
		case <-p.StopCh:
			if coverCancel != nil {
				coverCancel()
			}
			return
		default:
		}

		if client.IsClosed() {
			if coverCancel != nil {
				coverCancel()
			}
			return
		}

		info, err := client.GetPlayInfo()
		// sampleTime 是本轮进度读数（info.Progress）的时刻。换歌分支与进度更新处落锚都配它，不取用到读数
		// 时的时刻：换歌分支里的取词是阻塞的网络请求，取词返回后再取当前时刻，就把取词前的读数配到了
		// 取词后的时刻上，位置落后取词耗时；进度更新处拿推算位置与读数比较，耗时超过 3 秒还会判成回跳。
		// 换歌分支在取词之后会重读一次，读到同一首歌时 info、progressSec 与它一起换成新读数。
		sampleTime := time.Now()
		if err != nil {
			log.Warn("GetPlayInfo failed: %v", err)
			// Connection likely broken
			if coverCancel != nil {
				coverCancel()
			}
			return
		}

		if info == nil || info.Hash == "" {
			// No song loaded yet
			if lastHash == "" {
				// 连上之后还没有读到过歌：报 waiting_song，否则状态停在 Start 发出的 waiting_process（#181）。
				// 经 EmitStatusIfChanged，持续无歌不随轮询重发。会话中途（读到过歌之后）读到没有歌照旧不发：
				// 可能只是切歌的间隙，发出去会让路由把 kugou 判为 idle（理由同下方换歌的过渡态）。
				p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_song", Detail: i18n.T("KuGou has no current song"), Reason: player.ReasonNoSong})
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
		if info.Hash != lastHash {
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
			// Start 发出的 waiting_process，压下防不了闪烁，只会让状态停在「未启动」直到开始播放（#181）。
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

			// 获取歌词
			lines, lyrErr := fetchLyrics(info.Hash, int(durationMs), name, singer)
			sameSong := name == prevName && singer == prevSinger
			if lyrErr != nil {
				log.Warn("lyric fetch failed: %v", lyrErr)
				if !sameSong {
					currentLyrics = nil
				}
			} else if len(lines) > 0 {
				applyDetailedOffset(lines, offsetSec)
				currentLyrics = lines
				log.Info("Lyrics loaded: %d lines; per-character: %s", len(lines), detailedFlag(lines))
			} else if sameSong && len(currentLyrics) > 0 {
				// 同名同歌手但本次取不到（伴唱模式 hash 变化）→ 复用上一次的歌词
				// 复用的是已套过 offset 的 currentLyrics，不要再套一次。
				log.Info("Same song, hash changed; reusing previous lyrics (%d lines); word-by-word: %s", len(currentLyrics), detailedFlag(currentLyrics))
			} else {
				currentLyrics = nil
				log.Info("Instrumental / no lyrics; word-by-word: %s", detailedFlag(currentLyrics))
			}

			lyricItems := toLyricLines(currentLyrics, offsetSec)

			// 取词是阻塞的网络请求，返回时本轮读到的进度与播放状态都已过时：其间可能暂停、恢复或拖动过。
			// 再读一次，读到的仍是这首歌就把它当作本轮的读数——info、progressSec 与 sampleTime 一起替换，
			// 进度合法则以它落锚；下面随歌词发出的位置、本轮其后的播放状态变化检测与进度更新都基于它。
			// 读不到、或读到的已不是这首歌，则沿用取词前的读数，留给下一轮。重读出错不在这里退出：
			// 连接若已断，下一轮的 GetPlayInfo 会处理。
			//
			// statusAtChange 是取词前的状态，即上面已发出（过渡态时压下未发）的那个。分支末尾记进
			// lastStatus 的必须是它，不是重读到的：取词期间状态变了，紧接着的播放状态变化检测才会发出
			// playback_pause / playback_resume 与状态；过渡态压下的那次状态也由该处补发。
			statusAtChange := info.PlayStatus
			if fresh, rerr := client.GetPlayInfo(); rereadSameSong(fresh, rerr, lastHash) {
				sampleTime = time.Now()
				freshRaw, _ := strconv.ParseFloat(fresh.Progress, 64)
				info = fresh
				progressSec = float32(freshRaw / 1e7)
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
			// 进度，没有重读到则由取词前的锚点按经过的时间推算。算式与下方歌词行匹配处的 interpSec 是同一个。
			initPlay := player.LivePosition(anchorProgressSec, anchorTime, time.Now(), isPlaying, currentDurationSec)
			progress := player.ClampProgress(initPlay, currentDurationSec)

			// 酷狗对没有歌词的歌返回一行「纯音乐，请欣赏」，而非空歌词。归一成
			// index:-1，让下游只认一个判据；提示语原样留在 text 里，用不用由它决定。
			//
			// **currentLyrics 必须一起清**：它是下面 pollLyrics 的输入，不清的话
			// all_lyrics 说 count:0、轮询却仍按那一行发 index:0 的 lyric_update。
			pureHint := ""
			if player.IsPureMusicOnly(lyricItems) {
				log.Info("platform returned only the hint \"%s\", treating as no lyrics", lyricItems[0].Text)
				pureHint = lyricItems[0].Text
				currentLyrics = nil
				lyricItems = []player.LyricLine{} // 显式空 slice——nil 会序列化成 null，破坏「空数据一律 {} / []」
			}

			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: initPlay,
				Progress: progress,
				Lyrics:   lyricItems,
				Count:    len(lyricItems),
			})
			if len(currentLyrics) == 0 {
				// Progress 必须带上：它的契约是「整首播到哪」，纯音乐时歌照样在走。
				// 漏掉它会让 Go 零值兜出一个 0，与同一事件里的 all_lyrics.progress 自相矛盾。
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: pureHint, Timestamp: 0, Position: initPlay, Progress: progress,
				})
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
		interpSec := player.LivePosition(anchorProgressSec, anchorTime, time.Now(), isPlaying, currentDurationSec)

		matchTime := player.LyricMatchTime(interpSec, offsetSec)

		// ── 歌词行匹配 ──
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

		// 位置采样（见 BaseEmitter.EmitPositionSample）：播放中按固定间隔报一次，校准服务端缓存里的位置
		// ——上面每轮都以平台进度重新落锚，3s 以内的偏差直接吸收、不发事件，无歌词的歌又没有歌词行可推。
		// 传 interpSec，与上面 lyric_update 的 position 同一个量；offset 只进了 matchTime，这里不带。
		if isPlaying {
			p.EmitPositionSample(time.Now(), interpSec, currentDurationSec)
		}

		time.Sleep(pollInterval)
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
