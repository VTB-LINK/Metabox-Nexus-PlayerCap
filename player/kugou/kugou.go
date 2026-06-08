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

// Start 启动酷狗音乐轮询循环（阻塞）
func (p *KuGouPlayer) Start() {
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
			log.Warn("自动修复失败: %v（请手动运行酷狗启动工具或检查安装路径）", err)
			// 其他错误继续等待——用户可能手动启动酷狗
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

	// Local clock interpolation for smooth progress between polls
	var anchorProgressSec float32
	var anchorTime time.Time
	isPlaying := false

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		if client.IsClosed() {
			return
		}

		info, err := client.GetPlayInfo()
		if err != nil {
			log.Warn("GetPlayInfo 失败: %v", err)
			// Connection likely broken
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
			// 封面 goroutine：
			//   阶段1: 等待 200ms 取封面 URL
			//     - 200ms 内取到   → 等 b64（3s 预算内）→ 一次性发完整信息
			//     - 200ms 超时     → 先发不含封面的信息 → 继续等（3s 总预算）
			//       → 3s 内取到   → 等 b64 → 补发完整信息
			//       → 3s 超时     → 调 API 兜底（歌手头像，5s）
			//         → 取到 b64  → 补发完整信息
			//         → 取不到    → 不再发
			go func(ctx context.Context, ch <-chan string, name, singer, title, hash string) {
				const earlyWait = 200 * time.Millisecond
				const totalBudget = 800 * time.Millisecond
				const apiFallbackTimeout = 5 * time.Second
				start := time.Now()

				var coverURL string
				select {
				case coverURL = <-ch:
				case <-time.After(earlyWait):
				case <-ctx.Done():
					return
				}

				var fromAPI bool
				if coverURL == "" {
					// 200ms 内未取到封面，先发不含封面的歌曲信息
					select {
					case <-ctx.Done():
						return
					default:
					}
					p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
						Name: name, Singer: singer, Title: title,
					})
					// 继续等待封面 URL（3s 总预算）
					remaining := totalBudget - time.Since(start)
					if remaining <= 0 {
						remaining = 0
					}
					select {
					case coverURL = <-ch:
					case <-time.After(remaining):
						// CDP 3s 内无封面，调 API 兜底（伴奏/歌手头像类曲目）
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
						return
					}
				}

				// 已有封面 URL，下载 base64
				var b64Timeout time.Duration
				if fromAPI {
					b64Timeout = apiFallbackTimeout
				} else {
					b64Timeout = totalBudget - time.Since(start)
					if b64Timeout <= 0 {
						return
					}
				}
				b64 := player.FetchCoverBase64("Kugou", coverURL, b64Timeout)
				if b64 == "" {
					return
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
			}(ctx, coverCh, currentName, currentSinger, currentTitle, info.Hash)

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
				log.Info("歌词加载完成: %d 行", len(lines))
			} else if sameSong && len(currentLyrics) > 0 {
				// 同名同歌手但本次取不到（伴唱模式 hash 变化）→ 复用上一次的歌词
				log.Info("同曲目 hash 变化，复用上一次歌词（%d 行）", len(currentLyrics))
			} else {
				currentLyrics = nil
				log.Info("纯音乐/无歌词")
			}

			lyricItems := toLyricLines(currentLyrics)
			initPlay := anchorProgressSec // 已经过合法性验证的初始进度
			progress := clampProgress(initPlay, currentDurationSec)
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
				progress := clampProgress(interpSec, currentDurationSec)
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index:     trueIdx,
					Text:      line.Text,
					SubText:   "",
					Timestamp: line.Time,
					PlayTime:  interpSec,
					Progress:  progress,
				})
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

func clampProgress(playTime, duration float32) float32 {
	if duration <= 0 {
		return 0
	}
	return player.ClampFloat32(playTime/duration, 0, 1)
}

func toLyricLines(lines []klyric.Line) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.LyricLine{Index: l.Index, Timestamp: l.Time, Text: l.Text}
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
