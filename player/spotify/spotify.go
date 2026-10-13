// Package spotify 接入 Spotify 桌面版（Spotify.exe，CEF）作为第 7 个 player。
//
// 取数形态是「CDP + DOM」，与另外三家的关系：
//   - watchdog 照 cloudmusic（杀掉 + 带 flag 重启拿调试端口）；
//   - 收发与重连骨架照 sodamusic/kugou（一个会话一次 Connect，会话死了退避重连）；
//   - 位置时钟照 sodamusic 的边沿落锚 + applemusic 的翻转窗口（同类第四份，独立实现，见 clock.go）。
//
// 数据来源（全部实测于 Spotify 1.3.4.258 / libcef 151.3.18，2026-10）：
//   - 标题 / 歌手 / 封面：页面上的 data-testid 元素，直接读文本与 img.src；
//   - 时长 + 位置：[data-testid="progress-bar"] 的 `--progress-bar-transform` 浮点百分比 ×
//     <input max>（毫秒时长）。**不要用 <input value>**：它只有 5 秒粒度（实测 +5000/步）。
//   - 播放/暂停：播放按钮 svg path 的图标常量（语言无关，aria-label 会随系统语言变）；
//   - 歌词：Spotify 自家的 color-lyrics/v2（provider=MusixMatch，行级时间轴）。取词钥匙是
//     **曲目 URI**，而页面 DOM 里没有当前曲目的 URI（只有专辑/歌手链接与队列项）——
//     所以经 spotify_cli.exe（随 Spotify 一起安装的官方 CLI）拿一次 URI，见 watchdog.ReadNowPlaying。
//   - Bearer：不注入任何 JS，直接从 CDP 推送的 Network.requestWillBeSent 事件里旁观（见 cdp 包）。
//
// 稳定优先（AGENTS §0.1）：任何一步失败都降级（没有歌词、没有封面），绝不 panic、绝不盲干；
// 唯一会动到用户进程的是 watchdog 的「带 flag 重启」，那道闸在杀之前先验版本。
package spotify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/cdp"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
	"Metabox-Nexus-PlayerCap/player/spotify/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "spotify"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Spotify")

// SpotifyPlayer Spotify 播放器（CDP + DOM 实现）。
type SpotifyPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建 Spotify 播放器。
func New(offsetMs, pollMs int) *SpotifyPlayer {
	return &SpotifyPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// minPollInterval 是 Spotify 侧轮询下限（下限，不是目标值：配置写得更大就按配置走）。
//
// **依据是数据源的分辨率，不是单次调用的开销。** 页面上的位置与播放态都是 **~1Hz** 更新
// （实测 2026-10：`--progress-bar-transform` 的变化间隔 962~1026ms，位置本身是毫秒精度的浮点）。
// 两次更新之间反复去问只是重复读同一个值：轮询买到的只有「新采样落地后多久被发现」这段相位
// （≤ 一个轮询间隔），位置精度由本地时钟外推提供、与轮询频率无关。
//
// 200ms 与 sodamusic 取同一个值、同一理由：相位平均值 ~100ms，而每秒 5 次 Runtime.evaluate
// 往返（每次 1~3ms）打在主播的浏览器进程上可以忽略。要更紧的相位就调小它，收益上限只有 200ms。
const minPollInterval = 200 * time.Millisecond

// EffectivePollInterval 返回配置轮询间隔为 pollMs 毫秒时 Spotify 实际采用的间隔：低于 minPollInterval 抬到它。
// 轮询循环与 /service-status 的 config_effective 共用此函数，下限只在这里定义。
func EffectivePollInterval(pollMs int) time.Duration {
	d := time.Duration(pollMs) * time.Millisecond
	if d < minPollInterval {
		return minPollInterval
	}
	return d
}

// unsupportedRecheck 是判定「这一版 Spotify 不支持调试端口」之后的重探间隔。
// 与 cloudmusic 同值同理由：装了什么版本近乎永久，但用户升级后应当自动恢复（不必重启服务）。
const unsupportedRecheck = 30 * time.Second

// coverBudget 是下载封面 base64 的时间预算（每次下载各一份）。标题已先发，封面慢一点无妨。
const coverBudget = 5 * time.Second

// 封面图片 URL 形如 coverURLPrefix + 档位码 + 图片 hash，三个档位码对应三个尺寸。
//
// 出处：项目负责人给的 album 元数据口径——/metadata/4/album/<gid> 的 cover_group.image 恰好三条，
// size 为 SMALL / DEFAULT / LARGE，没有第四档；尺寸经真机实测（2026-10，10 张不同专辑三档全部 200，
// 尺寸依次为 64 / 300 / 640，同一 hash 换档位码 10/10 成立）。档位码不能由尺寸反推，故没有更大的档可猜。
// 页面上 [data-testid="cover-art-image"] 给的是 DEFAULT 档。
const (
	coverURLPrefix   = "https://i.scdn.co/image/"
	coverSizeSmall   = "ab67616d00004851" // SMALL，64×64
	coverSizeDefault = "ab67616d00001e02" // DEFAULT，300×300
	coverSizeLarge   = "ab67616d0000b273" // LARGE，640×640
)

// fetchCoverBase64 是 player.FetchCoverBase64 的可替换 seam：仅供 runCoverFetch 的离线测试注入桩
// （封面下载带 SSRF 拨号校验、拒回环地址，起不了本地服务）。生产恒为 player.FetchCoverBase64，不导出、外部无法改动。
var fetchCoverBase64 = player.FetchCoverBase64

// lyricsTimeout 是取一次歌词的 HTTP 预算。实测 100~300ms（含 TLS）。
const lyricsTimeout = 5 * time.Second

// lyricsRetryInterval / lyricsRetrySlow / lyricsFastTries / lyricsMaxTries 是「换歌时没拿到歌词」的补取节奏与上限，
// 都按已补取的次数计（见 lyricsRetry）。
const (
	lyricsRetryInterval = 5 * time.Second
	// lyricsRetrySlow 是补过 lyricsFastTries 次之后的补取节奏。
	//
	// 头 90 秒之后不立刻停：真机上「手里还没有 Bearer」或 token 过期（401）可以持续好几分钟 —— App 只在换歌、
	// 或者用户碰界面时才发 API 请求，而我们只能旁观它的请求。App 因为别的原因发了一次请求（于是我们抓到了新 token），
	// 那时补取仍然值钱。降频到 30 秒一次，代价可以忽略（一次 Runtime.evaluate）。
	lyricsRetrySlow = 30 * time.Second
	// lyricsFastTries 是按 lyricsRetryInterval 补取的次数（18 次，约 90 秒），之后降到 lyricsRetrySlow。
	lyricsFastTries = 18
	// lyricsMaxTries 是一首歌补取的次数上限（issue #111）：18 次快的加 17 次慢的，约 10 分钟的播放时间（暂停不补取、
	// 不计次），到顶即停并打一次日志，换歌复位。10 分钟长于绝大多数曲目，正常长度的歌在播完之前碰不到它；它给超长曲目
	// 与单曲循环（歌名歌手不变，不算换歌）一个终点。平台明确答复没有歌词（404、UNSYNCED）的不靠它，当场就停（见 settleLyrics）。
	lyricsMaxTries = 35
	// lyricsFailRepeat 是「歌词取不到」这条日志的复读间隔。补取每 5 秒试一次，
	// 逐次打日志会在一首没歌词的歌上刷出十几条（真机实测过），故只在超过这个间隔时复读。
	lyricsFailRepeat = 30 * time.Second
)

// progressLeadSec 是页面进度读数（`--progress-bar-transform`）在**播放中**比真实播放位置超前的量。
//
// **这是实测出来的，不是猜的**（Spotify 1.3.4.258，2026-10，两次独立观测）：
//
//  1. 播放中，界面上那个 mm:ss 文本恒等于 floor(transform 换算出的位置) - 1；**一暂停就对齐了**
//     （同一首歌：播放中 transform=154.041s / 文本 2:33；暂停后 transform=153.977s / 文本仍是 2:33）。
//     两条读数的口径只在"有没有动画"上不同 —— 播放中进度条在 1 秒内**动画**到下一个点位，
//     所以写进 style 的是**动画目标**（当前 + 1s），暂停时没有动画，写的就是当前位置。
//  2. 曲末（时长 279.000s 的《ゆらゆら》）：transform 打到 100%（=279.0s）之后 **1323ms** 才换歌，
//     而同一瞬间界面时间是 4:38（278s）。若 transform 等于真实位置，它应当在换歌那一刻才到 100%。
//
// 故：播放中真实位置 = transform 换算值 − progressLeadSec；暂停时两者相等（不校正）。
// 残差 ±0.3s（换歌那一瞬间的界面延迟无法从外部测准）由 spotify-offset 吸收；默认 -200 经项目负责人确认正合适，不单独对齐。
//
// **别把这条校正搬到时钟里**：它只作用于"读数 → 位置"这一步，且必须跟着读数那一刻的播放态走
// （暂停后再出现的读数是没有超前的，用播放中的规则去减会把位置拽回去 1 秒）。
//
// 播放倍速（`spotify_cli speed`）非 1x 时这条常数会失准（动画 1 秒覆盖的内容长度随倍速变），
// 未实测，故不猜。
const progressLeadSec = 1.0

// trackStartGraceSec 是换歌瞬间「位置读数还算不算开头」的容忍量（秒）。
//
// 换歌那一刻页面的位置与标题是同一次渲染出来的，属于当前曲目（对照 sodamusic 的 firstSong 判据，
// 那里换歌时进度源会带着上一首的收尾值，本项目的数据源没有那个问题）。但页面仍有极小概率先换标题、
// 后换进度条，故非本会话第一首时：读数大于这个值就夹到 0 —— 换歌时真实位置本就在 0 附近。
// 会话第一首不夹：服务在中途启动是最常见的开播姿势，那时位置本来就该是大的。
const trackStartGraceSec = 5.0

// stallWindow 是「图标判不出播放态」时的兜底判据：位置在这么久内没推进即视为暂停。
// 位置是 1Hz 采样的，窗口必须比采样周期大出余量（2.5s = 2 个周期 + 抖动）。
const stallWindow = 2500 * time.Millisecond

// extractErrRepeat 是取数错误的复读间隔（同一错误首次必打，之后每这么久复读一次）。
// 照 cloudmusic 的 extractErrLog：轮询路径上的瞬时错误（换歌间隙、页面重渲染）不设闸会刷屏。
const extractErrRepeat = 60 * time.Second

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *SpotifyPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// Start 启动 Spotify 轮询循环（阻塞，须在 goroutine 中调用）。
func (p *SpotifyPlayer) Start() {
	// 把调试 flag 补进用户的自启项：下次开机/自启就带端口，省掉一次「杀掉再拉起」。
	// 值不存在（用户没开自启）时什么都不做。
	watchdog.PatchRegistryAutoStart()

	unsupportedWarned := false

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		// 每轮先声明「未连上」，避免上一场的状态留在缓存里（与 sodamusic 同形）。
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("Spotify not running or not connected"), Reason: player.ReasonProcessNotRunning})
		p.Emit(player.EventClearSongData, nil)

		err := watchdog.EnsureDebugMode()
		switch {
		case errors.Is(err, watchdog.ErrNotRunning):
			// 未运行——用户可能稍后启动，静默等待重试（上面已报未启动态）。
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		case errors.Is(err, watchdog.ErrVersionUnsupported):
			// 近乎永久的状态：不空转、不每次刷屏，但给用户升级后自动恢复的机会。
			if !unsupportedWarned {
				unsupportedWarned = true
				log.Warn("Spotify does not support the remote debugging port; lyric capture is unavailable (reason: %v)", err)
			}
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{
				Status: "standby", Detail: i18n.T("Spotify version unsupported (remote debugging unavailable)"), Reason: player.ReasonVersionUnsupported,
			})
			if p.sleepOrStop(unsupportedRecheck) {
				return
			}
			continue
		case errors.Is(err, watchdog.ErrPortNotReady):
			// 刚带 flag 启动 / 端口被别的程序占着：短退避重试。
			if p.sleepOrStop(1 * time.Second) {
				return
			}
			continue
		case err != nil:
			log.Error("watchdog error: %v", err)
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}
		unsupportedWarned = false

		client, err := cdp.Connect()
		if err != nil {
			log.Warn("CDP connection failed: %v", err)
			if p.sleepOrStop(2 * time.Second) {
				return
			}
			continue
		}

		log.Info("CDP connected, monitoring playback state")
		p.runSession(client)
		client.Close()

		p.InvalidateSongGen() // 作废在飞的封面回写，别让它在 ClearSongData 之后复活已清空的 SongInfo
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("Spotify CDP disconnected"), Reason: player.ReasonCDPDisconnected})
		p.Emit(player.EventClearSongData, nil)
		if p.sleepOrStop(2 * time.Second) {
			return
		}
	}
}

// runSession 轮询页面播放态并 emit 事件，直到 CDP 断开。
func (p *SpotifyPlayer) runSession(client *cdp.Client) {
	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := EffectivePollInterval(p.pollMs)

	// cliPath 只在这里取一次：spotify_cli.exe 随 Spotify 安装、路径不会中途变；
	// 取不到就整场没有歌词（标题/歌手/进度照常），并在下面告警一次。
	cliPath, cliErr := watchdog.CLIPath()
	if cliErr != nil {
		log.Warn("Spotify CLI not found (%v); lyrics are unavailable this session, playback state still works", cliErr)
	}

	var clk clock
	var lastSongKey string
	var lastLineIdx = -1
	var currentLyrics []lyric.Line
	var currentLyricItems []player.LyricLine
	var currentTitle string
	var currentDurationSec float32
	// retry 是「已换到这首歌、但歌词还没拿到」的补取状态（见 lyricsRetry）。
	var retry lyricsRetry
	// lyricsFailAt 是上一次打印「歌词取不到」的时刻，用于限流复读（见 lyricsFailRepeat）。
	var lyricsFailAt time.Time
	// 取 URI 与取词在 runLyricsJob 里异步做，结果经 results 交回、在每一拍开头落地（见 lyricsJobs）。
	// 会话结束时作废在飞的那次：它在下一步边界退出，送结果也不会挂住。
	var jobs lyricsJobs
	defer jobs.stop()
	results := make(chan lyricsResult, 1)

	// 每首歌一个封面 goroutine，换歌/退出时由 ctx 取消——迟到的封面回写不会污染下一首/清屏后状态。
	// 保持 coverCancel 的显式调用（别包 defer/helper），让 go vet 的 lostcancel 看得见（对照 kugou.go）。
	var coverCancel context.CancelFunc

	// 图标判不出播放态时的兜底状态（见 stallWindow）。
	var lastPositionMs = -1.0
	var lastMoveAt time.Time
	var unknownIconWarned bool
	var cliMismatchWarned bool

	// 取数错误的降噪状态。
	var lastExtractErr string
	var lastExtractErrAt time.Time

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

		now := time.Now()

		// ── 取词结果落地（换歌后的首次与补取共用）──
		// 放在每一拍开头、Snapshot 之前：下面几处提前 continue 的分支不会把结果晾着，送结果的 goroutine 不会久等。
		// 被作废的那次（已换歌、或会话的上一次）代次对不上，丢弃。时钟每拍都在更新，位置按此刻外推即可。
		select {
		case r := <-results:
			if !jobs.accept(r.gen) {
				break
			}
			if r.cliMismatch && !cliMismatchWarned {
				cliMismatchWarned = true
				log.Warn("play state from the page icon disagrees with spotify_cli (page=%v cli=%v); check the icon constants", r.pagePlaying, !r.pagePlaying)
			}
			s := settleLyrics(r)
			if s.hint != "" {
				log.Info("platform returned only the hint \"%s\", treating as no lyrics", s.hint)
			}
			pos := float32(clk.liveAt(time.Now()))
			noLine := &player.LyricUpdate{
				Index: -1, Text: s.hint, Timestamp: 0, Position: pos, Progress: player.ClampProgress(pos, currentDurationSec),
			}
			switch {
			case len(s.lines) > 0:
				currentLyrics = s.lines
				currentLyricItems = toLyricLines(s.lines, offsetSec)
				lastLineIdx = -1 // all_lyrics 会清掉服务端缓存的 lyric_update，让当前行随后重发
				if r.retry {
					log.Info("Lyrics arrived late; re-sending all_lyrics (%d lines)", len(currentLyricItems))
				} else {
					log.Info("Lyrics loaded: %d lines (provider %s, %s)", len(currentLyrics), lyric.Provider(r.resp), lyric.SyncType(r.resp))
				}
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title:    currentTitle,
					Duration: currentDurationSec,
					Position: pos,
					Progress: player.ClampProgress(pos, currentDurationSec),
					Lyrics:   currentLyricItems,
					Count:    len(currentLyricItems),
				})
			case !r.retry:
				// 换歌时已以空歌词发过 all_lyrics；index:-1 的 lyric_update 必须显式带 Progress（契约：整首播到哪；
				// 漏写零值兜 0 会与 all_lyrics.progress 自相矛盾——lyricupdate_lint_test 全仓强制）。
				log.Info("Instrumental / no lyrics; word-by-word: %s", i18n.T("no"))
				p.Emit(player.EventLyricUpdate, noLine)
				if r.resp != nil && s.hint == "" {
					// 平台答复了、只是没有行级时间轴：不起补取（见 settleLyrics），但要留痕，与「取词链路坏了」区分开。
					log.Detail("Lyrics has no line-level timing (syncType %s); keeping this track without lyrics", lyric.SyncType(r.resp))
				} else if r.notFound {
					log.Detail("No lyrics for this track (HTTP 404); not retrying") // 平台答复 404：同上，不起补取、留痕
				}
			case s.hint != "":
				p.Emit(player.EventLyricUpdate, noLine) // 补取时才给出提示语：同换歌那次，随 index:-1 发出
			case r.notFound:
				log.Detail("No lyrics for this track (HTTP 404); not retrying")
			case s.stopRetry:
				log.Detail("Lyrics has no line-level timing (syncType %s); keeping this track without lyrics", lyric.SyncType(r.resp))
			case r.diag != "" && now.Sub(lyricsFailAt) >= lyricsFailRepeat:
				// 复读限流：首次已在换歌那次落地时报过，这里只在超过间隔时再报一次。
				lyricsFailAt = now
				log.Detail("Lyrics still unavailable: %s", r.diag)
			}
			started, gaveUp := retry.land(s, r.trackID, now)
			if started {
				log.Detail("No lyrics for this track yet; will retry while it plays")
				if r.diag != "" {
					lyricsFailAt = now
					log.Detail("Lyrics unavailable: %s", r.diag)
				}
			}
			if gaveUp {
				log.Detail("Giving up on lyrics for this track after %d attempts", lyricsMaxTries)
			}
		default:
		}

		snap, err := client.Snapshot()
		if err != nil {
			if client.IsClosed() {
				if coverCancel != nil {
					coverCancel()
				}
				return
			}
			// 瞬时错误（换歌间隙、页面重渲染）降噪后原地重试；连接真断了上面那条会退出。
			if err.Error() != lastExtractErr || now.Sub(lastExtractErrAt) >= extractErrRepeat {
				lastExtractErr = err.Error()
				lastExtractErrAt = now
				log.Warn("Extraction error: %v", err)
			}
			time.Sleep(pollInterval)
			continue
		}
		lastExtractErr = ""

		// 没有曲目（首页/未播放/刚启动）：静默等待，交给路由超时判 idle。
		if snap.Title == "" {
			time.Sleep(pollInterval)
			continue
		}

		name := snap.Title
		singer := strings.Join(snap.Artists, " / ")
		songKey := snapSongKey(snap)
		durationSec := float32(snap.DurationMs) / 1000.0
		posSec := float32(snap.PositionMs) / 1000.0

		playing, iconKnown := snap.Playing()
		if !iconKnown {
			// 图标常量不认识（Spotify 换了图标/结构）：退回「位置有没有在推进」。
			if lastPositionMs < 0 || snap.PositionMs != lastPositionMs {
				lastMoveAt = now
			}
			lastPositionMs = snap.PositionMs
			playing = !lastMoveAt.IsZero() && now.Sub(lastMoveAt) < stallWindow
			if !unknownIconWarned {
				unknownIconWarned = true
				log.Warn("play/pause icon not recognised; falling back to position-advance detection")
			}
		}

		// 页面读数 → 真实位置：播放中要减掉进度条的动画超前量（见 progressLeadSec）。
		// 判据必须用**这一刻的播放态**，不能用上一轮的：暂停后新出现的读数没有超前。
		posSec = truePositionSec(float32(snap.PositionMs)/1000.0, playing)

		// ── 换歌检测（歌名 + 歌手归一化后的稳定键）──
		if songKey != lastSongKey {
			firstSong := lastSongKey == ""
			lastSongKey = songKey
			lastLineIdx = -1
			currentLyrics = nil
			currentLyricItems = []player.LyricLine{} // 显式空 slice——nil 会序列化成 null
			currentTitle = buildTitle(name, singer)
			currentDurationSec = durationSec
			retry.stop()
			clk.reset()

			// 换歌时页面的位置读数（见 trackStartGraceSec）。
			if !firstSong && posSec > trackStartGraceSec {
				log.Detail("position %.2fs right after the song change looks stale; anchoring at 0", posSec)
				posSec = 0
			}
			// 换歌分支的落锚：时钟状态跟着新曲目重置，事件不在这里发（对外只有下面的 status）。
			clk.observe(clockRaw(posSec, durationSec), playing, float64(durationSec), now)

			if playing {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
			}

			// 封面：取消上一首、起本首。
			if coverCancel != nil {
				coverCancel()
			}
			var ctx context.Context
			ctx, coverCancel = context.WithCancel(context.Background())
			go p.runCoverFetch(ctx, snap.Cover, name, singer, currentTitle)

			// 歌词：取 URI（CLI）与取词都在 runLyricsJob 里异步做，这里不等 —— 网络差时三步取词最坏约 11s，
			// 同步等就这么久不读位置、不发事件（issue #107）。先以空歌词发 all_lyrics（与「还没拿到」同形），
			// 结果在之后某一拍的开头落地（见上面「取词结果落地」）；上一首在飞的那次随之作废。
			jctx, gen := jobs.start()
			go runLyricsJob(jctx, lyricsJob{gen: gen, name: name, singer: singer, pagePlaying: playing, iconKnown: iconKnown}, cliPath, client, results)

			// 位置按发送这一刻算（服务端以事件到达的时刻为采样时刻）。
			initPos := float32(clk.liveAt(time.Now()))
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: initPos,
				Progress: player.ClampProgress(initPos, currentDurationSec),
				Lyrics:   currentLyricItems,
				Count:    len(currentLyricItems),
			})
		}

		// ── 歌词补取：换歌后首次没拿到、且还在等（Bearer 通常在这一两秒内被抓到）；暂停时不补 ──
		if jobs.retryDue(&retry, playing, now) {
			jctx, gen := jobs.start()
			go runLyricsJob(jctx, lyricsJob{gen: gen, retry: true, trackID: retry.trackID}, cliPath, client, results)
		}

		// ── 时长补取：换歌那一拍页面刚渲染、时长读到 0 时的兜底 ──
		//
		// **补到就必须重发一次 all_lyrics，不能只补内部变量**：服务端缓存里 duration 只由 all_lyrics 写，
		// 而 progress 由每条 lyric_update 与 playback_* 写，只补一半会让缓存里出现 `duration:0` 与非 0 的
		// progress 并存（理由详见 sodamusic.go 的同一处）。
		if durationArrived(currentDurationSec, durationSec) {
			currentDurationSec = durationSec
			lastLineIdx = -1 // all_lyrics 会清掉服务端缓存的 lyric_update，让当前行随后重发
			pos := float32(clk.liveAt(time.Now()))
			log.Info("duration arrived late; re-sending all_lyrics (duration=%.2fs)", currentDurationSec)
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: pos,
				Progress: player.ClampProgress(pos, currentDurationSec),
				Lyrics:   currentLyricItems,
				Count:    len(currentLyricItems),
			})
		}

		// ── 播放态变化 + seek 检测 ──
		//
		// 时钟一拍只返回一个事件，seek 不会与恢复同拍，playback_resume 一拍至多一条（AGENTS §5.1）；
		// seek 与暂停同拍时由 clockEmits 补发暂停。
		beforeSec := float32(clk.liveAt(now))
		ev := clk.observe(clockRaw(posSec, durationSec), playing, float64(durationSec), now)
		if ev == evSeek {
			if posSec > beforeSec {
				log.Info("forward seek detected: %.2fs → %.2fs", beforeSec, posSec)
			} else {
				log.Info("Seek-back detected: %.2fs → %.2fs", beforeSec, posSec)
			}
			lastLineIdx = -1
		}
		for _, e := range clockEmits(ev, playing, posSec, float32(clk.liveAt(now))) {
			p.Emit(e.event, &player.PlaybackTimeInfo{Position: e.pos, Progress: player.ClampProgress(e.pos, currentDurationSec)})
			switch e.status {
			case "playing":
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("resumed @ %.2fs", e.pos)
			case "paused":
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
				log.Info("paused @ %.2fs", e.pos)
			}
		}

		// ── 本地时钟插值 + 歌词行匹配 ──
		interpSec := float32(clk.liveAt(time.Now()))
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
				// interpSec 进 Position/算 Progress；play_time 由 BuildLyricUpdate 按歌词时间轴算。
				p.Emit(player.EventLyricUpdate, lineUpdate(trueIdx, currentLyrics[trueIdx], offsetSec, interpSec, currentDurationSec))
			}
		}

		// 位置采样（见 BaseEmitter.EmitPositionSample）：播放中按固定间隔报一次，校准服务端缓存里的位置。
		// 传 interpSec，与上面 lyric_update 的 position 同一个量；offset 只进了 matchTime，这里不带。
		if playing {
			p.EmitPositionSample(time.Now(), interpSec, currentDurationSec)
		}

		time.Sleep(pollInterval)
	}
}

// lyricsRetry 是「换歌时没拿到歌词」的补取状态，节奏与上限见 lyricsRetryInterval / lyricsRetrySlow / lyricsFastTries / lyricsMaxTries。
//
// **节奏按已补取的次数定，不按距换歌过了多久**：暂停期间不补取、不计次（见 retryDue），按时间算的话，暂停很久之后
// 恢复，第一次补取就落到慢节奏、甚至越过上限。lastTry 只用来守间隔。
//
// **tries 由 start 复位**：节奏曾是会话里的局部变量、换歌不复位，上一首降到 30 秒之后，下一首从第一次补取起就是
// 30 秒一次（真机实测：换歌后 +29.8s 才第一次补取）。
type lyricsRetry struct {
	trackID string // 空串 = 不在等
	lastTry time.Time
	tries   int // 本首已补取的次数（不含换歌那次）
}

// start 开始等 trackID 的歌词（换歌时没拿到才调）；now 计为第一次尝试。
func (r *lyricsRetry) start(trackID string, now time.Time) {
	r.trackID = trackID
	r.lastTry = now
	r.tries = 0
}

// stop 不再补取（换歌、拿到了、平台答复没有歌词、或次数到顶）。
func (r *lyricsRetry) stop() { r.trackID = "" }

// due 报告此刻该不该补取一次；该则记下这次尝试。补过 lyricsFastTries 次之后降频，到 lyricsMaxTries 次即不再补。
func (r *lyricsRetry) due(now time.Time) bool {
	every := lyricsRetryInterval
	if r.tries >= lyricsFastTries {
		every = lyricsRetrySlow
	}
	if r.trackID == "" || r.tries >= lyricsMaxTries || now.Sub(r.lastTry) < every {
		return false
	}
	r.lastTry = now
	r.tries++
	return true
}

// land 按一次取词结果的落地判定（settleLyrics）更新补取状态：拿到或平台已答复即停，换歌那次没取到响应即开始；
// 之后次数已到上限而仍在等（最后一次补取也没拿到）即停。返回是否开始了补取、是否因到顶而停（调用方各打一次日志）。
func (r *lyricsRetry) land(s lyricsSettle, trackID string, now time.Time) (started, gaveUp bool) {
	if s.stopRetry {
		r.stop()
	}
	if s.startRetry {
		r.start(trackID, now)
		started = true
	}
	if r.trackID != "" && r.tries >= lyricsMaxTries {
		r.stop()
		gaveUp = true
	}
	return started, gaveUp
}

// lyricsJobs 是异步取词（runLyricsJob）的代次与在飞状态，只在轮询 goroutine 里读写，不加锁。
//
// 同一时刻至多一次当前代次的取词在飞。stop（换歌、会话结束）作废它：取消它的 ctx，它在下一步边界退出；
// 代次前进，它若已把结果送进通道，收到时也对不上、被丢弃。被作废的那次不再算在飞 —— 它手上那一步各有
// 预算（CLI 5s、直取 lyricsTimeout、页面两步 lyricsFetchBudget），做完即退，不必等它才起下一首的。
type lyricsJobs struct {
	gen    uint64
	cancel context.CancelFunc // 非 nil = 当前代次有一次在飞
}

// start 作废在飞的那次（若有），为新的一次取词开一个代次，返回交给 runLyricsJob 的 ctx 与代次。
func (j *lyricsJobs) start() (context.Context, uint64) {
	j.stop()
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	return ctx, j.gen
}

// stop 作废在飞的那次（若有）。重复调用无害。
func (j *lyricsJobs) stop() {
	if j.cancel != nil {
		j.cancel()
		j.cancel = nil
	}
	j.gen++
}

// accept 报告收到的结果该不该落地：只认当前代次、且它还在飞的那一次；认下即算它已结束。
func (j *lyricsJobs) accept(gen uint64) bool {
	if j.cancel == nil || gen != j.gen {
		return false
	}
	j.cancel()
	j.cancel = nil
	return true
}

// retryDue 报告此刻该不该起一次补取。未在播放（playing 为假）时不补取：暂停期间页面不发新请求、抓不到新 token，
// 补取只会重复同样的失败，路由也已在暂停超时后撤下这一家（2026-10-13 真机：暂停近一小时，每 30 秒补取一次）；
// 恢复播放后照常继续。
//
// **先看播放态与有没有在飞、再问 r.due**：due 一旦返回真就记下了这次尝试，暂停或在飞时先问它，会把这一拍的补取
// 白白记掉（暂停时还会耗掉次数），下一次要再等一整个间隔。
func (j *lyricsJobs) retryDue(r *lyricsRetry, playing bool, now time.Time) bool {
	return playing && j.cancel == nil && r.due(now)
}

// lyricsJob 是一次异步取词的请求。换歌那次 trackID 为空、由 CLI 取；补取那次带着已知的 trackID、不读 CLI。
type lyricsJob struct {
	gen     uint64
	retry   bool
	trackID string
	// 以下只用于换歌那次：日志里的歌名与歌手，换歌那一拍页面图标判出的播放态（与 CLI 对照，见 cliMismatch）。
	name, singer           string
	pagePlaying, iconKnown bool
}

// lyricsResult 是一次异步取词的结果，经容量为 1 的通道交回轮询循环。
type lyricsResult struct {
	lyricsJob
	// cliMismatch 报告 CLI 的播放态与页面图标不一致（只在换歌那次、图标认得出时判）：图标常量可能过期。
	cliMismatch bool
	lines       []lyric.Line
	resp        *lyric.Response
	diag        string
	// notFound 报告平台答复了 HTTP 404（没有这首歌的歌词，见 fetchLyrics）。
	notFound bool
}

// lyricsClient 是取词用到的 CDP 会话方法（*cdp.Client 实现它）；抽成接口只为离线测试注入桩。
type lyricsClient interface {
	Token() string
	AppPlatform() string
	AppVersion() string
	FetchLyrics(trackID, token string) (string, error)
	FetchViaPageCache(trackID string) (string, error)
}

// readNowPlaying / fetchLyricsDirect 是 watchdog.ReadNowPlaying / lyric.Fetch 的可替换 seam：仅供异步取词的
// 离线测试注入桩。生产恒为这两个函数，不导出、外部无法改动。
var (
	readNowPlaying    = watchdog.ReadNowPlaying
	fetchLyricsDirect = lyric.Fetch
)

// runLyricsJob 在独立 goroutine 里做一次取词：换歌那次先经 CLI 取曲目 URI，再按 fetchLyrics 的优先级取词，
// 结果送回轮询循环（见 lyricsJobs）。
//
// 每一步之前（含第一步之前）查 ctx，被作废即退出、不送结果。送结果时等到被收走或被作废：轮询循环每一拍开头都收一次，
// 会话结束时作废在飞的那次，故不会挂住；作废后才送进通道的结果代次对不上，落地时丢弃。
func runLyricsJob(ctx context.Context, job lyricsJob, cliPath string, client lyricsClient, out chan<- lyricsResult) {
	defer telemetry.Guard()
	if ctx.Err() != nil { // 还没被调度到就已作废（换歌、会话结束）：一步都不做
		return
	}
	r := lyricsResult{lyricsJob: job}
	if !job.retry {
		if cliPath != "" {
			np, err := readNowPlaying(cliPath)
			if err != nil {
				log.Warn("spotify_cli now-playing failed: %v", err)
			} else if np.HasTrack {
				r.trackID = lyric.TrackIDFromURI(np.URI)
				r.cliMismatch = job.iconKnown && np.IsPlaying != job.pagePlaying
			}
		}
		if ctx.Err() != nil {
			return
		}
		log.Info("♪ Song: %s - %s (id: %s)", job.name, job.singer, r.trackID)
	}
	r.lines, r.resp, r.diag, r.notFound = fetchLyrics(ctx, client, r.trackID)
	if ctx.Err() != nil {
		return
	}
	select {
	case out <- r:
	case <-ctx.Done():
	}
}

// lyricsSettle 是一次取词结果落地时怎么处理（settleLyrics 的判定；日志与事件由 runSession 发）。
type lyricsSettle struct {
	lines []lyric.Line // 非空 = 拿到了：换上、重发 all_lyrics
	// hint 是平台只给了「纯音乐」一类提示语时的那句（按无歌词处理，随 index:-1 的 lyric_update 发出）。
	hint                  string
	startRetry, stopRetry bool
}

// settleLyrics 判定一次取词结果怎么落地：
//   - 拿到行级歌词：换上，停止补取；
//   - 平台只给了提示语（player.IsPureMusicOnly）：按无歌词处理；
//   - 取到了响应却没有可用的行级歌词（UNSYNCED、零行、只有提示语），或平台答复 404（notFound）：平台已经答复，
//     再试也不会有——换歌那次不起补取，补取那次停止补取；
//   - 换歌那次没取到响应（401 / 403 / 网络错误等）：有 URI 才开始补取（没有 URI 就没有补取的钥匙，不必空转）；
//     补取仍没取到响应：照节奏继续，到次数上限为止（lyricsMaxTries，见 lyricsRetry.land）。
func settleLyrics(r lyricsResult) lyricsSettle {
	var s lyricsSettle
	if items := toLyricLines(r.lines, 0); player.IsPureMusicOnly(items) {
		s.hint = items[0].Text
	} else {
		s.lines = r.lines
	}
	answered := r.resp != nil || r.notFound
	switch {
	case len(s.lines) > 0:
		s.stopRetry = true
	case !r.retry:
		s.startRetry = r.trackID != "" && !answered
	default:
		s.stopRetry = answered
	}
	return s
}

// snapSongKey 是换歌检测的稳定键：歌名 + 歌手归一化后拼接。
func snapSongKey(s *cdp.Snapshot) string {
	return player.NormalizeSongName(s.Title) + "\x00" + player.NormalizeSongName(strings.Join(s.Artists, " / "))
}

// clockEmit 是一拍的时钟迁移要发出的一条对外事件。
type clockEmit struct {
	event string // player.EventPlaybackResume / player.EventPlaybackPause
	pos   float32
	// status 是随之发出的 status_update；空串 = 不发（seek 复用 playback_resume，不改状态）。
	status string
}

// clockEmits 把 observe 的结果翻译成本拍要发的事件序列。seekPos 是本拍读数（seek 之后的位置），
// livePos 是 observe 之后此刻的时钟位置（恢复/暂停用）。
//
// **seek 与暂停同拍要补发暂停**：observe 判了 seek 就不再返回 evPause，而时钟已记成暂停、下一拍
// 不会再报。只发 resume 的话服务端仍是 playing，位置继续外推（server.go 的 livePosition 把三种
// 事件都当采样点），歌词接着走。补发的 pause 带跳变后的位置。seek 不会与恢复同拍（见 observe），
// 故 playback_resume 一拍至多一条（AGENTS §5.1）。
func clockEmits(ev clockEvent, playing bool, seekPos, livePos float32) []clockEmit {
	switch ev {
	case evSeek:
		out := []clockEmit{{event: player.EventPlaybackResume, pos: seekPos}}
		if !playing {
			out = append(out, clockEmit{event: player.EventPlaybackPause, pos: seekPos, status: "paused"})
		}
		return out
	case evResume:
		return []clockEmit{{event: player.EventPlaybackResume, pos: livePos, status: "playing"}}
	case evPause:
		return []clockEmit{{event: player.EventPlaybackPause, pos: livePos, status: "paused"}}
	}
	return nil
}

// durationArrived 报告这一拍是否该补上换歌时没读到的时长：只在从未取到时补，不追后续变化
// （照 sodamusic 的「时长补取」：换歌之外的时长变化没有已知来源，跟着变只会把 all_lyrics 变成高频事件）。
func durationArrived(current, read float32) bool {
	return current <= 0 && read > 0
}

// clockRaw 把页面读数转成时钟的入参：位置非法（NaN / 负 / 明显超出时长）时返回 -1 表示
// 「本轮没有有效采样」。
//
// **这道闸是承重的**：进度条的百分比在换歌/页面重渲染的瞬间可能读到 0 或旧值，而时钟对
// 「没有采样」与「有效采样」的处理完全不同（前者不落锚、只按翻转窗口外推，后者落锚）。
func clockRaw(posSec, durationSec float32) float64 {
	invalid := !(posSec >= 0)
	if !invalid && durationSec > 0 && posSec > durationSec*1.05 {
		invalid = true
	}
	if invalid {
		return -1
	}
	return float64(posSec)
}

// truePositionSec 把页面读数（`--progress-bar-transform` 换算出的秒）转成真实播放位置：
// 播放中减掉动画超前量，暂停时原样（见 progressLeadSec 的实测依据）。
//
// 减成负数时取 0（刚开播的第一个读数可能小于超前量）。
func truePositionSec(rawSec float32, playing bool) float32 {
	if !playing {
		return rawSec
	}
	if v := rawSec - progressLeadSec; v > 0 {
		return v
	}
	return 0
}

// fetchLyrics 按优先级取一份歌词，返回解析好的行、响应、诊断串（为空表示拿到了行）与 notFound。
//
// 三层都实机验证过（2026-10）：
//   - Go 侧带 Bearer 直取（lyric.Fetch）最快，但真机上稳定 **403**（同一 token、同一 URL，
//     从页面里发就是 200，差别在请求来源），故它只是第一选择、不成即落下一层；
//   - 页面里带 Bearer 取（cdp.FetchLyrics）与 App 自己的请求同源同头（**缺 accept 或
//     app-platform 会被回 400**，真机实测），可用；
//   - 页面的 HTTP 缓存（cdp.FetchViaPageCache）不需要 token，但**只对 App 在本页面会话里真的
//     取过的曲目有效**：缓存里可能已有更早会话取过的响应，而我们无法凭空构造出那个 URL
//     （它还带封面 id 段）。所以它只是兜底，不是常路。
//
// 诊断串同时覆盖「请求失败」与「取到了但平台没有可用的行级歌词」（syncType 为 UNSYNCED 或
// lines 为空）两种情形 —— 后者此前是完全静默的，运维看不出「平台没歌词」与「取词链路坏了」的区别。
//
// notFound 报告平台答复了「没有这首歌的歌词」：页面内取词或页面缓存得到 HTTP 404（lyric.IsNotFound），且没有一层取到响应。
// Go 直取的状态码不参与：它在真机上稳定 403（请求来源不同），只说明这一层不行。
//
// 每一层之前查 ctx：被作废（换歌、会话结束）就不再发下一层的请求，返回空（调用方不会用它）。
func fetchLyrics(ctx context.Context, client lyricsClient, trackID string) ([]lyric.Line, *lyric.Response, string, bool) {
	if trackID == "" || ctx.Err() != nil {
		return nil, nil, "", false
	}
	tokenErr, notFound := "", false
	if token := client.Token(); token != "" {
		if resp, err := fetchLyricsDirect(trackID, token, client.AppPlatform(), client.AppVersion(), lyricsTimeout); err == nil {
			if lines := lyric.Parse(resp); len(lines) > 0 {
				return lines, resp, "", false
			} else {
				return nil, resp, noLineLyricsDiag(resp), false
			}
		} else {
			tokenErr = err.Error()
		}
		if ctx.Err() != nil {
			return nil, nil, "", false
		}
		if raw, err := client.FetchLyrics(trackID, token); err == nil && raw != "" {
			if resp, perr := lyric.ParseBody([]byte(raw)); perr == nil {
				if lines := lyric.Parse(resp); len(lines) > 0 {
					return lines, resp, "", false
				} else {
					return nil, resp, noLineLyricsDiag(resp), false
				}
			}
		} else if err != nil {
			tokenErr = err.Error()
			notFound = lyric.IsNotFound(err)
		}
	}
	if ctx.Err() != nil {
		return nil, nil, "", false
	}
	raw, err := client.FetchViaPageCache(trackID)
	if err == nil && raw != "" {
		if resp, perr := lyric.ParseBody([]byte(raw)); perr == nil {
			if lines := lyric.Parse(resp); len(lines) > 0 {
				return lines, resp, "", false
			} else {
				return nil, resp, noLineLyricsDiag(resp), false
			}
		}
	} else if lyric.IsNotFound(err) {
		notFound = true
	}
	if tokenErr != "" {
		return nil, nil, tokenErr, notFound
	}
	return nil, nil, i18n.T("lyrics unavailable"), notFound
}

// noLineLyricsDiag 是「取到了但平台没有行级歌词」的诊断串。它作为 %s 注入日志，不会被 logger 翻译，
// 须在源头本地化（agent-doc/06 §6.4 ⑤）；经 i18n.Errorf 取中文译文，译表在 i18n/messages_p4_err.go。
func noLineLyricsDiag(resp *lyric.Response) string {
	return i18n.Errorf("response has no line-level lyrics (syncType %s)", lyric.SyncType(resp)).Error()
}

// runCoverFetch 发歌曲信息与封面；每首歌一个，换歌时由 ctx 取消。
//
// 先发一条只带 URL、不带 base64 的信息让标题与封面尽快上屏，再下载 base64 补发完整信息。
// **不变量：即使 b64 拿不到（超时/过大/失败）也照发带 URL 的信息**——绝不为封面吞掉歌曲信息，
// 否则 OBS 整首停在上一首（轮询只在换歌时发、不自愈）。理由详见 sodamusic.go 的同名函数。
func (p *SpotifyPlayer) runCoverFetch(ctx context.Context, coverURL, name, singer, title string) {
	defer telemetry.Guard()
	select {
	case <-ctx.Done():
		return
	default:
	}
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL,
	})
	if coverURL == "" {
		return
	}
	// base64 先取 LARGE 档；取不到再用页面原 URL。对外的 Cover 两条都是页面原 URL：前端按它判断是不是同一首歌。
	b64 := ""
	if large := largeCoverURL(coverURL); large != coverURL {
		if b64 = fetchCoverBase64("Spotify", large, coverBudget); b64 == "" && ctx.Err() == nil {
			log.Detail("Large cover unavailable; falling back to the page's cover URL")
		}
	}
	if b64 == "" && ctx.Err() == nil {
		b64 = fetchCoverBase64("Spotify", coverURL, coverBudget)
	}

	select {
	case <-ctx.Done():
		return
	default:
	}
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL, CoverBase64: b64,
	})
}

// largeCoverURL 把页面给的 SMALL / DEFAULT 档封面 URL 换成 LARGE 档：只换档位码，其后的 hash 与查询串原样保留。
// 不是 i.scdn.co 的 https 图片地址、已是 LARGE、档位码不认识（例如艺人头像的前缀）时原样返回。
func largeCoverURL(u string) string {
	rest, ok := strings.CutPrefix(u, coverURLPrefix)
	if !ok {
		return u
	}
	for _, code := range []string{coverSizeSmall, coverSizeDefault} {
		if hash, ok := strings.CutPrefix(rest, code); ok {
			return coverURLPrefix + coverSizeLarge + hash
		}
	}
	return u
}

// buildTitle 组标题。**Spotify 自己的顺序是「歌名 - 歌手」**（实测 spotify_cli 的
// description 字段就是 "Oh Girl — 宇宙人"），故与 wesing / cloudmusicv3 / qqmusic 同侧，
// 与 kugou / sodamusic / applemusic 的「歌手 - 歌名」相反。歌手为空则只留歌名。
func buildTitle(name, singer string) string {
	if singer == "" {
		return name
	}
	return fmt.Sprintf("%s - %s", name, singer)
}

// toLyricLines 把解析结果映射成对外 LyricLine。Spotify 只提供行级歌词（见 lyric 包对
// syllables 的说明），故 sub_text / roma_text / 逐字三项都留空，由 BuildLyricLine 填入零值。
func toLyricLines(lines []lyric.Line, offsetSec float32) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, "", "", player.LyricTextDetailed{}, offsetSec)
	}
	return out
}

// lineUpdate 把一行歌词映射成 lyric_update 的载荷，字段口径同 toLyricLines。
func lineUpdate(index int, l lyric.Line, offsetSec, playPos, duration float32) *player.LyricUpdate {
	return player.BuildLyricUpdate(index, l.Time, l.Text, "", "", player.LyricTextDetailed{}, offsetSec, playPos, duration)
}
