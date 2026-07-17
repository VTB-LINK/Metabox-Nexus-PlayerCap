package kugou

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
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
)

// runCoverFetch 取封面并发出 song_info_update；每首歌一个，换歌时由 ctx 取消。
//
//	阶段1: 等 coverEarlyWait 取封面 URL（**常见情况下 URL 已预置在缓冲 channel 里，立即命中**）
//	  - 取到   → 等 b64（coverTotalBudget 内）→ 一次性发完整信息
//	  - 超时   → 先发不含封面的信息 → 继续等到 coverTotalBudget
//	    → 取到 → 等 b64 → 补发完整信息
//	    → 超时 → 调 API 兜底（歌手艺术照，coverAPIFallbackTimeout）
//	      → 仍无 URL → 不再发（不含封面的信息上面已发过）
//
// **不变量：只要拿到了封面 URL，就必须发出 song_info_update，无论 b64 成功与否。**
// 常见路径上封面 URL 立即命中，走的是「跳过上面整个分支」——此前一次 song_info_update
// 都没发过。此时为 b64 失败而 return，OBS 上整首歌都会停在上一首的标题和封面，而轮询
// 循环只在换歌时发、不会自愈。b64 只是加速手段（省掉前端自己取图），拿不到就带 URL 发，
// 前端自行加载——FetchCoverBase64 在图片超限时本来就是这个约定。
func (p *KuGouPlayer) runCoverFetch(ctx context.Context, ch <-chan string, name, singer, title, hash string) {
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
			log.Info("无法获取封面，回退到歌手艺术照")
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
		b64 = player.FetchCoverBase64("Kugou", coverURL, b64Timeout)
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

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: "酷狗音乐未启动或 CDP 未就绪"})
		p.Emit(player.EventClearSongData, nil)

		// 自动检测并修复：若 libcef.dll 未 patch 则 kill → patch → 重启酷狗
		if err := watchdog.EnsurePatched(p.StopCh); err != nil {
			select {
			case <-p.StopCh:
				return
			default:
			}
			if errors.Is(err, watchdog.ErrInstallNotFound) {
				log.Error("自动修复失败: %v（请手动运行酷狗启动工具或检查安装路径）", err)
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: "未找到酷狗安装，已停止"})
				return
			}
			if errors.Is(err, watchdog.ErrVersionUnsupported) {
				// 版本指纹不认识：重试也只会反复弹 UAC 打错版本，终态停止。
				log.Error("自动修复失败: %v（请降级到受支持的酷狗版本，或等待适配后重试）", err)
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: "酷狗 libcef.dll 版本不受支持，已停止"})
				return
			}
			if errors.Is(err, watchdog.ErrElevationFailed) {
				// 退避后重试；用尽即终态。直接 continue，不去 waitForCDP——没提权就没 patch，
				// CDP 不可能起来，而 waitForCDP 在酷狗没跑时会立即返回，等于 2s 后又弹 UAC。
				backoff, giveUp := elevation.record()
				if giveUp {
					log.Error("连续 %d 次未取得管理员权限，停止自动修复（再试只会让 UAC 安全桌面反复夺焦）。"+
						"如需启用酷狗，请重启本程序并在 UAC 弹窗点「是」", len(elevationBackoffs)+1)
					p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "error", Detail: "未取得管理员权限，已停止自动修复"})
					return
				}
				log.Warn("未取得管理员权限（第 %d/%d 次）: %v —— %v 后重试",
					elevation.fails, len(elevationBackoffs)+1, err, backoff)
				if p.sleepOrStop(backoff) {
					return
				}
				continue
			}
			log.Warn("自动修复失败: %v（请手动运行酷狗启动工具或检查安装路径）", err)
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
			log.Warn("CDP 未就绪，重新检测酷狗状态...")
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("CDP 连接成功，开始监听播放状态")
		p.runSession(client)
		client.Close()

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "酷狗音乐 CDP 已断开"})
		p.Emit(player.EventClearSongData, nil)
		log.Info("会话结束，等待酷狗重新启动...")
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
			return nil, fmt.Errorf("stopped")
		default:
		}

		client, err := cdp.Connect()
		if err == nil {
			return client, nil
		}

		// 进程已退出则立即返回，不再等待 90s
		if !watchdog.IsKuGouRunning() {
			return nil, fmt.Errorf("酷狗进程已退出")
		}

		now := time.Now()
		if now.After(nextLog) {
			log.Info("等待酷狗 CDP 端口 12233 就绪... (%v)", err)
			nextLog = now.Add(10 * time.Second)
		}
		if now.After(deadline) {
			return nil, fmt.Errorf("CDP 90s 内未就绪: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// runSession polls PlayInfo and emits events until the CDP connection drops.
func (p *KuGouPlayer) runSession(client *cdp.Client) {
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
	// ——它会在 Start() 已 Emit EventClearSongData **之后**（最长约 5.8s）再 Emit
	// EventSongInfoUpdate，把已清空的歌曲信息重新贴回 OBS。（注：父 context 是
	// Background，不 cancel 几乎不泄漏资源，goroutine 退出即可 GC；真正的收益是上面
	// 这个「迟到的 Emit 覆盖已清空状态」，不是「防泄漏」。）
	//
	// 别改成 defer 闭包、也别包成 helper 函数：go vet 的 lostcancel 只认对该变量的直接调用，
	// 任何间接层都会让它从此对这里失明（实测 defer func(){ if c!=nil {c()} }() 仍被报为泄漏）。
	// 保持显式调用，vet 才能在将来有人新增 return 却忘了取消时当场抓住。
	//
	// **别把 vet 绿当成这类问题已经清了。** cloudmusic/qqmusic/wesing 三家的封面
	// goroutine 是裸 go func，连 context 都没有（全仓 WithCancel 只此一处），因此
	// lostcancel **结构性看不见它们**——没有 CancelFunc 可丢，分析器就无从下手。
	// 那三处同样会出现「迟到的封面回写覆盖当前歌曲」，而且比这里更糟：连换歌取消都没有。

	// Local clock interpolation for smooth progress between polls
	var anchorProgressSec float32
	var anchorTime time.Time
	isPlaying := false

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
		if err != nil {
			log.Warn("GetPlayInfo 失败: %v", err)
			// Connection likely broken
			if coverCancel != nil {
				coverCancel()
			}
			return
		}

		if info == nil || info.Hash == "" {
			// No song loaded yet
			time.Sleep(pollInterval)
			continue
		}

		progressRaw, _ := strconv.ParseFloat(info.Progress, 64)
		durationMs, _ := strconv.ParseFloat(info.Duration, 64)
		// KuGou 的 progress 字段使用 100 纳秒单位（≠ 毫秒），除以 1e7 得到秒。
		// duration 字段为毫秒，除以 1000 得到秒。
		progressSec := float32(progressRaw / 1e7)
		durationSec := float32(durationMs) / 1000.0

		// ── 切歌检测 ──
		if info.Hash != lastHash {
			lastHash = info.Hash
			lastLineIdx = -1
			currentDurationSec = durationSec
			isPlaying = (info.PlayStatus == "playing")
			// progress 仅在合法范围内时作为初始锚点，否则从 0 开始
			if isProgressValid(progressSec, durationSec) {
				anchorProgressSec = progressSec
			} else {
				anchorProgressSec = 0
			}
			anchorTime = time.Now()

			name, singer := splitFilename(info.Filename)
			// 捕获上一首歌的标识，用于判断是否是"同名同歌手但 hash 变了"的场景
			// （如开关伴唱模式，hash 变化但歌词应复用）
			prevName, prevSinger := currentName, currentSinger
			currentName = name
			currentSinger = singer
			currentTitle = buildTitle(name, singer)
			currentCover = strings.Replace(info.Cover, "/stdmusic/120/", "/stdmusic/800/", 1)

			log.Info("♪ 歌曲: %s - %s (hash: %s)", currentName, currentSinger, info.Hash)

			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: statusStr(info.PlayStatus), Detail: currentTitle})

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
			lines, lyrErr := klyric.Fetch(info.Hash, int(durationMs), name, singer)
			sameSong := name == prevName && singer == prevSinger
			if lyrErr != nil {
				log.Warn("歌词获取失败: %v", lyrErr)
				if !sameSong {
					currentLyrics = nil
				}
			} else if len(lines) > 0 {
				currentLyrics = lines
				log.Info("歌词加载完成: %d 行；逐字：否", len(lines))
			} else if sameSong && len(currentLyrics) > 0 {
				// 同名同歌手但本次取不到（伴唱模式 hash 变化）→ 复用上一次的歌词
				log.Info("同曲目 hash 变化，复用上一次歌词（%d 行）；逐字：否", len(currentLyrics))
			} else {
				currentLyrics = nil
				log.Info("纯音乐/无歌词；逐字：否")
			}

			lyricItems := toLyricLines(currentLyrics, offsetSec)
			initPlay := anchorProgressSec // 已经过合法性验证的初始进度
			progress := player.ClampProgress(initPlay, currentDurationSec)
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				PlayTime: initPlay,
				Progress: progress,
				Lyrics:   lyricItems,
				Count:    len(lyricItems),
			})
			if len(currentLyrics) == 0 {
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: "", Timestamp: 0, PlayTime: initPlay,
				})
			}

			lastStatus = info.PlayStatus
		}

		// ── 封面延迟到达（VIP 歌曲首次播放时封面为空，稍后才填入）──
		// 仅将 URL 送入 channel，由封面 goroutine 统一下载并发送（不额外发事件）
		if info.Cover != "" && info.Cover != currentCover {
			currentCover = info.Cover
			log.Info("封面 URL 已更新: %s", currentCover)
			if coverCh != nil {
				select {
				case coverCh <- currentCover:
				default:
				}
			}
		}

		// ── 播放状态变化检测 ──
		if info.PlayStatus != lastStatus {
			lastStatus = info.PlayStatus

			switch info.PlayStatus {
			case "playing", "play":
				isPlaying = true
				// 恢复播放时用合法的 progress 重置锚点，否则沿用上次冻结位置
				if isProgressValid(progressSec, currentDurationSec) {
					anchorProgressSec = progressSec
				}
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: anchorProgressSec})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("恢复 @ %.2fs", anchorProgressSec)
			case "paused", "pause", "pasued":
				isPlaying = false
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
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{PlayTime: anchorProgressSec})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
				log.Info("暂停 @ %.2fs", anchorProgressSec)
			default:
				log.Warn("未知的 play_status 值: %q", info.PlayStatus)
			}
		}

		// ── 进度更新 + seek 检测 ──
		// 仅当 progressSec 在合法范围内（[0, duration*1.05]）时才信任该值。
		// KuGou 在播放状态下可能返回超出歌曲时长的值（单位非 ms），此时让
		// 本地时钟自由运行，不更新锚点，不触发 seek 事件。
		if isPlaying && isProgressValid(progressSec, currentDurationSec) {
			interpSec := anchorProgressSec + float32(time.Since(anchorTime).Seconds())
			drift := progressSec - interpSec
			if drift < -3.0 {
				log.Info("检测到回跳: %.2fs → %.2fs", anchorProgressSec, progressSec)
				lastLineIdx = -1
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: progressSec})
			} else if drift > 3.0 {
				log.Info("检测到前跳: %.2fs → %.2fs", anchorProgressSec, progressSec)
				lastLineIdx = -1
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: progressSec})
			}
			anchorProgressSec = progressSec
			anchorTime = time.Now()
		}

		// ── 本地时钟插值 ──
		var interpSec float32
		if isPlaying {
			interpSec = anchorProgressSec + float32(time.Since(anchorTime).Seconds())
		} else {
			interpSec = anchorProgressSec
		}
		if currentDurationSec > 0 && interpSec > currentDurationSec {
			interpSec = currentDurationSec
		}

		matchTime := interpSec + offsetSec

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
				// interpSec 是插值出的实时位置，只喂 Progress；play_time 由 BuildLyricUpdate
				// 按歌词时间轴算。kugou 无逐字，故 detailed 传零值。
				p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
					trueIdx, line.Time, line.Text, "", player.LyricTextDetailed{},
					offsetSec, interpSec, currentDurationSec,
				))
			}
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
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, "", player.LyricTextDetailed{}, offsetSec)
	}
	return out
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
// 其中 {size} 占位符替换为 400。
func fetchCoverFromAPI(hash string) string {
	url := "https://m.kugou.com/app/i/getSongInfo.php?cmd=playInfo&hash=" + hash
	resp, err := kugouAPIClient.Get(url)
	if err != nil {
		log.Warn("封面 API 请求失败: %v", err)
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
	return strings.ReplaceAll(cover, "{size}", "800")
}
