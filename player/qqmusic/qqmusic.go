package qqmusic

import (
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

const PlayerName = "qqmusic"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("QQMusic")

// QQMusicPlayer QQ 音乐播放器
type QQMusicPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建 QQ 音乐播放器
func New(offsetMs, pollMs int) *QQMusicPlayer {
	return &QQMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// Start 启动 QQ 音乐轮询循环（阻塞）
func (p *QQMusicPlayer) Start() {
	offsetSec := float32(p.offsetMs) / 1000.0

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: "QQ音乐未启动"})
		p.Emit(player.EventClearSongData, nil)

		mem, err := ConnectQQMusic()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		log.Info("已连接 QQMusic.exe (PID: %d, 版本: %s)", mem.pid, mem.Version())

		p.runSession(mem, offsetSec)

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "QQ音乐已退出"})
		p.Emit(player.EventClearSongData, nil)
		log.Info("会话结束，等待新的 QQMusic 进程...")
		time.Sleep(2 * time.Second)
	}
}

func (p *QQMusicPlayer) runSession(mem *QQMusicMem, offsetSec float32) {
	if err := mem.InjectSliderAOB(); err != nil {
		log.Warn("滑块 Hook 失败: %v", err)
	}

	var lastName string
	var currentLyrics []lyricLine
	var lastLineIdx int = -1
	var currentDurationSec float32 = 0
	var isPaused bool = false
	var currentTitle string

	// 快速计时器锚点 + 本地时钟插值
	var anchorProgressMs uint32 = 0
	var anchorTime time.Time
	var lastMemProgress uint32 = 0
	const pauseTimeoutSec = 1.0

	pollInterval := time.Duration(p.pollMs) * time.Millisecond
	if pollInterval < 30*time.Millisecond {
		pollInterval = 50 * time.Millisecond
	}

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		time.Sleep(pollInterval)

		if !mem.CheckValid() {
			return
		}

		meta, err := mem.ReadAllMetadata()
		if err != nil {
			continue
		}

		// --- 切歌检测 ---
		if meta.Name != lastName && meta.Name != "" && meta.Name != "QQ音乐" {
			lastName = meta.Name
			lastLineIdx = -1
			anchorProgressMs = meta.ProgressMs
			anchorTime = time.Now()
			lastMemProgress = meta.ProgressMs
			isPaused = false

			log.Info("♪ 歌曲: %s - %s", meta.Name, meta.Singer)

			cookie := mem.FindCookie()
			var lrcName, lrcSinger string

			currentLyrics, lrcName, lrcSinger, err = fetchLRC(meta.SongID, cookie, meta.DurationMs)
			coverURL := fetchCoverURL(meta.SongID)

			if lrcName != "" {
				meta.Name = lrcName
			}
			if lrcSinger != "" {
				meta.Singer = lrcSinger
			}

			title := meta.Name + " - " + meta.Singer
			currentTitle = title
			currentDurationSec = float32(meta.DurationMs) / 1000.0

			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: title})
			p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
				Name: meta.Name, Singer: meta.Singer, Title: title, Cover: coverURL,
			})

			if err != nil {
				log.Warn("歌词获取失败: %v", err)
				progress := float32(0)
				if currentDurationSec > 0 {
					progress = player.ClampFloat32((float32(meta.ProgressMs)/1000.0)/currentDurationSec, 0, 1)
				}
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					PlayTime: float32(meta.ProgressMs) / 1000.0,
					Progress: progress,
					Lyrics:   []player.LyricLine{}, Count: 0,
				})
			} else {
				log.Info("歌词加载完成: %d 行", len(currentLyrics))
				lyricItems := toLyricLines(currentLyrics)
				progress := float32(0)
				if currentDurationSec > 0 {
					progress = player.ClampFloat32((float32(meta.ProgressMs)/1000.0)/currentDurationSec, 0, 1)
				}
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					PlayTime: float32(meta.ProgressMs) / 1000.0,
					Progress: progress,
					Count:    len(lyricItems), Lyrics: lyricItems,
				})
			}

			// 异步封面下载
			go func(url, name, singer, t string) {
				if url == "" {
					return
				}
				if b64 := player.FetchCoverBase64(url, 5*time.Second); b64 != "" {
					p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
						Name: name, Singer: singer, Title: t, Cover: url, CoverBase64: b64,
					})
				}
			}(coverURL, meta.Name, meta.Singer, title)
		}

		// --- 快速计时器锚点更新 + seek 检测 ---
		if meta.ProgressMs != lastMemProgress {
			// Seek 检测：进度大幅跳变（> 2s 超出正常播放推进）视为跳转
			seekDetected := false
			resumeEmitted := false
			if lastMemProgress > 0 {
				// 回跳：进度回退超过 2s
				if meta.ProgressMs+2000 < lastMemProgress {
					log.Info("检测到回跳: %.2fs → %.2fs", float32(lastMemProgress)/1000.0, float32(meta.ProgressMs)/1000.0)
					seekDetected = true
				}
				// 前跳：进度前进量超出实际经过时间 2s 以上
				if !seekDetected && meta.ProgressMs > lastMemProgress {
					advance := meta.ProgressMs - lastMemProgress
					elapsed := uint32(time.Since(anchorTime).Milliseconds())
					if advance > elapsed+2000 {
						log.Info("检测到前跳: %.2fs → %.2fs", float32(lastMemProgress)/1000.0, float32(meta.ProgressMs)/1000.0)
						seekDetected = true
					}
				}
			}
			if seekDetected {
				lastLineIdx = -1 // 重置歌词行号
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
					PlayTime: float32(meta.ProgressMs) / 1000.0,
				})
				resumeEmitted = true
			}

			if isPaused {
				isPaused = false
				if !resumeEmitted {
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
						PlayTime: float32(meta.ProgressMs) / 1000.0,
					})
				}
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("恢复 @ %.2fs", float32(meta.ProgressMs)/1000.0)
			}
			anchorProgressMs = meta.ProgressMs
			anchorTime = time.Now()
			lastMemProgress = meta.ProgressMs
		}

		// --- 本地时钟插值 ---
		elapsed := time.Since(anchorTime)
		interpolatedMs := float32(anchorProgressMs) + float32(elapsed.Milliseconds())

		// 暂停检测：快速计时器停滞超过阈值
		if elapsed.Seconds() > pauseTimeoutSec && !isPaused && lastName != "" {
			isPaused = true
			interpolatedMs = float32(anchorProgressMs)
			p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{
				PlayTime: float32(anchorProgressMs) / 1000.0,
			})
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
			log.Info("暂停 @ %.2fs", float32(anchorProgressMs)/1000.0)
		}

		if isPaused {
			interpolatedMs = float32(anchorProgressMs)
		}

		// 钳位
		durationMs := float32(meta.DurationMs)
		if durationMs > 0 && interpolatedMs > durationMs {
			interpolatedMs = durationMs
		}

		progressSec := interpolatedMs/1000.0 + offsetSec

		// 歌词行匹配
		trueIdx := -1
		for i := len(currentLyrics) - 1; i >= 0; i-- {
			if progressSec >= currentLyrics[i].Time {
				trueIdx = i
				break
			}
		}

		if trueIdx >= 0 && trueIdx != lastLineIdx {
			lastLineIdx = trueIdx
			line := currentLyrics[trueIdx]

			var progress float32 = 0
			if currentDurationSec > 0 {
				progress = player.ClampFloat32(progressSec/currentDurationSec, 0, 1)
			}

			p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
				Index:     trueIdx,
				Text:      line.Text,
				SubText:   "",
				Timestamp: line.Time,
				PlayTime:  progressSec,
				Progress:  progress,
			})
		}
	}
}

// toLyricLines converts internal lyricLine to player.LyricLine
func toLyricLines(lines []lyricLine) []player.LyricLine {
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.LyricLine{Index: l.Index, Timestamp: l.Time, Text: l.Text}
	}
	return out
}
