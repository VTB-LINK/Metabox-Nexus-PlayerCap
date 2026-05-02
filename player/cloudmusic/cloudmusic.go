package cloudmusic

import (
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/lyric"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
)

const PlayerName = "cloudmusicv3"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("CloudMusic")

// CloudMusicPlayer 网易云音乐播放器
type CloudMusicPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建网易云播放器
func New(offsetMs, pollMs int) *CloudMusicPlayer {
	return &CloudMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

type baseRealClock struct {
	BaseRealTime        float32
	AnchorTime          time.Time
	LastKnownLyricIndex int
	Playing             bool
}

func (c *baseRealClock) GetCurrent() float32 {
	if !c.Playing {
		return c.BaseRealTime
	}
	elapsed := float32(time.Since(c.AnchorTime).Seconds())
	return c.BaseRealTime + elapsed
}

func normalizeSongIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), " ")
}

func snapshotMatchesCurrentSong(data *cdp.ExtractionData, songName, songArtist string) bool {
	targetName := normalizeSongIdentity(songName)
	if targetName == "" || data == nil || data.CurPlaying == nil {
		return false
	}
	if normalizeSongIdentity(data.CurPlaying.Track.Name) != targetName {
		return false
	}

	targetArtist := normalizeSongIdentity(songArtist)
	if targetArtist == "" {
		return true
	}
	currentArtist := normalizeSongIdentity(getArtists(data.CurPlaying.Track.Artists))
	if currentArtist == "" {
		return false
	}
	return currentArtist == targetArtist
}

// Start 启动轮询循环（阻塞）
func (p *CloudMusicPlayer) Start() {
	// 0. Patch Windows registry auto-start
	watchdog.PatchRegistryAutoStart()

	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: "网易云音乐未启动"})

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		restarted, err := watchdog.EnsureDebugMode()
		if err != nil {
			log.Error("Watchdog 错误: %v", err)
		}
		if restarted {
			log.Info("已重启网易云音乐，等待 5s...")
			time.Sleep(5 * time.Second)
		}

		client, err := cdp.Connect()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("CDP 已连接，开始轮询...")
		p.runSession(client)
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "网易云音乐已退出"})
		p.Emit(player.EventClearSongData, nil)
	}
}

func (p *CloudMusicPlayer) runSession(client *cdp.Client) {
	var lastDomSongName string
	var lastPlayingState int = -1
	var lastLineIdx int = -1
	var clock baseRealClock
	var currentSongName string
	var currentSongArtist string
	var currentSongTitle string // 当前歌曲标题（用于 status detail）
	offsetSec := float32(p.offsetMs) / 1000.0
	var activeLyrics []cdp.ExtractedLyric
	var activeSongID string
	var lastSongChangeTime time.Time
	var isPureMusic bool
	var cdpLyricsOK bool // CDP 已成功获取歌词，跳过 Redux fallback
	var songDuration float32

	pollInterval := time.Duration(p.pollMs) * time.Millisecond
	if pollInterval < 50*time.Millisecond {
		pollInterval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.StopCh:
			return
		case <-ticker.C:
		}

		data, err := client.Extract()
		if err != nil {
			if !strings.Contains(err.Error(), "no root") {
				log.Warn("提取错误: %v", err)
			}
			if client.IsClosed() {
				return
			}
			continue
		}

		if data == nil || data.CurPlaying == nil {
			continue
		}

		// Detect song change by DOM song name
		songName := data.DomSongName
		songArtist := data.DomArtist
		songCover := data.DomCoverUrl

		if songName == "" {
			songName = data.CurPlaying.Track.Name
		}
		if songArtist == "" {
			songArtist = getArtists(data.CurPlaying.Track.Artists)
		}
		if songCover == "" {
			songCover = data.CurPlaying.Track.Album.PicUrl
		}

		songChanged := false
		if songName != "" && songName != lastDomSongName {
			songChanged = true
			lastDomSongName = songName
			lastPlayingState = data.PlayingState // 保留当前状态，避免切歌时误触 state change → 多余的 resume(0)
			lastLineIdx = -1
			activeLyrics = nil
			activeSongID = ""
			isPureMusic = false
			cdpLyricsOK = false
			songDuration = 0
			clock = baseRealClock{LastKnownLyricIndex: -1}
			lastSongChangeTime = time.Now()
			currentSongName = songName
			currentSongArtist = songArtist

			log.Info("♪ 歌曲: %s - %s", songName, songArtist)

			// 先强制 Redux 获取当前歌词数据，确保 Redux Store 更新到最新歌曲
			client.ForceFetchLyricsInRedux()
			time.Sleep(300 * time.Millisecond)
			matchedRedux := snapshotMatchesCurrentSong(data, songName, songArtist)
			if matchedRedux {
				activeSongID = data.CurPlaying.ID
			}

			// 重新提取 Redux 状态，获取最新的 CurPlaying.ID
			freshData, freshErr := client.Extract()
			if freshErr == nil && snapshotMatchesCurrentSong(freshData, songName, songArtist) {
				activeSongID = freshData.CurPlaying.ID
				// 同步更新 data 以获取最新歌词/时长
				data = freshData
			}

			// Redux ID 为空时再等 500ms 重试一次
			if activeSongID == "" {
				time.Sleep(500 * time.Millisecond)
				retryData, retryErr := client.Extract()
				if retryErr == nil && snapshotMatchesCurrentSong(retryData, songName, songArtist) && retryData.CurPlaying.ID != "" {
					activeSongID = retryData.CurPlaying.ID
					data = retryData
					log.Info("重试获取 Redux ID 成功: %s", activeSongID)
				} else {
					log.Info("当前歌曲的 Redux 快照暂未对齐，等待后续补齐歌词和封面")
				}
			}
			matchedRedux = snapshotMatchesCurrentSong(data, songName, songArtist)
			if activeSongID != "" {
				log.Info("使用 Redux ID: %s", activeSongID)
			}

			// 封面优先使用 Redux 中的专辑封面 URL（高清），否则使用 CDP 接口或 DOM 封面
			if matchedRedux && data.CurPlaying != nil && data.CurPlaying.Track.Album.PicUrl != "" {
				songCover = data.CurPlaying.Track.Album.PicUrl
			}
			if activeSongID != "" {
				if detailCover, err := client.FetchCoverViaCDP(activeSongID); err == nil && detailCover != "" {
					songCover = detailCover
				}
			}

			songTitle := songName + " - " + songArtist
			currentSongTitle = songTitle

			// 先发不带 base64 的 songinfo，异步下载封面后补发
			p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
				Name: songName, Singer: songArtist, Title: songTitle,
				Cover: songCover,
			})

			// 异步获取封面 base64，完成后补发 song_info_update
			go func(name, artist, title, cover string) {
				if b64 := player.FetchCoverBase64(cover, 5*time.Second); b64 != "" {
					p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
						Name: name, Singer: artist, Title: title,
						Cover: cover, CoverBase64: b64,
					})
				}
			}(songName, songArtist, songTitle, songCover)

			// Fetch lyrics via CDP using Redux ID
			if activeSongID != "" {
				lrcText, err := client.FetchLyricsViaCDP(activeSongID)
				if err != nil {
					log.Warn("歌词获取失败: %v", err)
				} else if lrcText == "[PURE_MUSIC]" || lrcText == "[NO_LYRIC]" {
					// CDP 返回纯音乐，用 Go HTTP API 二次确认
					log.Info("CDP 返回纯音乐(ID=%s)", activeSongID)
					if apiLyrics, err2 := lyric.FetchLyrics(activeSongID); err2 == nil && len(apiLyrics) > 0 {
						log.Info("API 二次确认(ID=%s)有 %d 行歌词，使用 API 歌词", activeSongID, len(apiLyrics))
						for _, l := range apiLyrics {
							activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
								Index: l.Index, Time: l.Time, Text: l.Text,
							})
						}
						cdpLyricsOK = true
					} else {
						log.Info("API 二次确认(ID=%s)也无歌词，标记纯音乐", activeSongID)
						isPureMusic = true
						if matchedRedux && data.CurPlaying.Track.Duration > 0 {
							songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
						}
						p.Emit(player.EventAllLyrics, &player.AllLyricsData{
							SongTitle: songTitle, Duration: songDuration, PlayTime: clock.GetCurrent(), Lyrics: []player.LyricLine{}, Count: 0,
						})
						p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
							LineIndex: -1, Text: "", Timestamp: 0, PlayTime: clock.GetCurrent(),
						})
					}
				} else {
					cdpLyricsOK = true
					parsed := lyric.ParseLRC(lrcText)
					for _, l := range parsed {
						activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
							Index: l.Index, Time: l.Time, Text: l.Text,
						})
					}
				}
			}

			// 若 Redux ID 为空且 ForceFetch 后 Redux 歌词已有数据，直接采纳
			if activeSongID == "" && matchedRedux && len(data.Lyrics) > 0 {
				activeLyrics = data.Lyrics
				cdpLyricsOK = true
				log.Info("Redux ID 为空，直接使用 Redux 歌词: %d 行", len(activeLyrics))
			}

			// Broadcast all lyrics（仅在有普通歌词时发送，避免空歌词 + Redux 补发导致双发）
			if len(activeLyrics) > 0 {
				lyricItems := make([]player.LyricLine, len(activeLyrics))
				for i, l := range activeLyrics {
					lyricItems[i] = player.LyricLine{Index: l.Index, Time: l.Time, Text: l.Text}
				}

				if songDuration == 0 && matchedRedux && data.CurPlaying.Track.Duration > 0 {
					songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
				}

				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					SongTitle: songTitle, Duration: songDuration, PlayTime: clock.GetCurrent(), Lyrics: lyricItems, Count: len(lyricItems),
				})
			}

			// 切歌时主动发 status_update（lastPlayingState 不再重置，state change 段不会重复触发）
			if data.PlayingState == 2 {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: songTitle})
			}
		}

		// Redux lyrics appeared later（切歌 tick 跳过，避免旧歌词回写覆盖新歌词）
		// 等待 ForceFetchLyricsInRedux 完成后再接受 Redux 歌词（冷却 1.5 秒防止旧歌词被误采纳）
		if !songChanged && !isPureMusic && !cdpLyricsOK && len(data.Lyrics) > 0 && len(data.Lyrics) != len(activeLyrics) && time.Since(lastSongChangeTime) > 1*time.Second {
			if snapshotMatchesCurrentSong(data, currentSongName, currentSongArtist) && (activeSongID == "" || data.CurPlaying.ID == activeSongID || data.CurPlaying.ID == "") {
				activeLyrics = data.Lyrics
				log.Info("歌词加载完成(Redux): %d 行", len(activeLyrics))

				// 补发全量歌词给前端
				lyricItems := make([]player.LyricLine, len(activeLyrics))
				for i, l := range activeLyrics {
					lyricItems[i] = player.LyricLine{Index: l.Index, Time: l.Time, Text: l.Text}
				}
				if songDuration == 0 && data.CurPlaying.Track.Duration > 0 {
					songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
				}
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					SongTitle: currentSongTitle, Duration: songDuration, PlayTime: clock.GetCurrent(), Lyrics: lyricItems, Count: len(lyricItems),
				})
			}
		}

		// 超时兜底：CDP 失败且 Redux fallback 超时后仍无歌词，清空前端
		if !songChanged && !isPureMusic && !cdpLyricsOK && len(activeLyrics) == 0 && time.Since(lastSongChangeTime) > 3*time.Second {
			log.Info("歌词获取超时，标记纯音乐并清空前端")
			isPureMusic = true
			if songDuration == 0 && snapshotMatchesCurrentSong(data, currentSongName, currentSongArtist) && data.CurPlaying != nil && data.CurPlaying.Track.Duration > 0 {
				songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
			}
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				SongTitle: currentSongTitle, Duration: songDuration, PlayTime: clock.GetCurrent(), Lyrics: []player.LyricLine{}, Count: 0,
			})
			p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
				LineIndex: -1, Text: "", Timestamp: 0, PlayTime: clock.GetCurrent(),
			})
		}

		// Update clock
		isPlaying := (data.PlayingState == 2)
		if !clock.Playing && isPlaying {
			clock.Playing = true
			clock.AnchorTime = time.Now()
		} else if clock.Playing && !isPlaying {
			clock.BaseRealTime += float32(time.Since(clock.AnchorTime).Seconds())
			clock.Playing = false
		}

		// Find current lyric（应用 offset）
		trueLineIdx := -1
		currentTime := clock.GetCurrent() + offsetSec
		for i := len(activeLyrics) - 1; i >= 0; i-- {
			if currentTime >= activeLyrics[i].Time {
				trueLineIdx = i
				break
			}
		}

		// Seek detection — 切歌后短暂抑制，等待 DOM 进度条刷新到新歌曲
		seeked := false
		if data.DomTimeSec >= 0 && clock.Playing && time.Since(lastSongChangeTime) > 500*time.Millisecond {
			prevPlayTime := clock.GetCurrent()
			diff := float32(data.DomTimeSec) - prevPlayTime
			if diff < 0 {
				diff = -diff
			}
			if diff > 1.5 {
				targetPlayTime := float32(data.DomTimeSec)
				if targetPlayTime+0.01 < prevPlayTime {
					log.Info("检测到回跳: %.2fs → %.2fs", prevPlayTime, targetPlayTime)
				} else if targetPlayTime > prevPlayTime+0.01 {
					log.Info("检测到前跳: %.2fs → %.2fs", prevPlayTime, targetPlayTime)
				}
				clock.BaseRealTime = float32(data.DomTimeSec)
				clock.AnchorTime = time.Now()
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				seeked = true
			}
		}

		// State change
		if data.PlayingState != lastPlayingState {
			lastPlayingState = data.PlayingState
			if data.PlayingState == 2 {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentSongTitle})
				if !seeked {
					log.Info("恢复 @ %.2fs", clock.GetCurrent())
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				}
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentSongTitle})
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				log.Info("暂停 @ %.2fs", clock.GetCurrent())
			}
		}

		// Lyric boundary crossing
		if isPlaying && trueLineIdx >= 0 && trueLineIdx < len(activeLyrics) {
			if trueLineIdx != lastLineIdx {
				lastLineIdx = trueLineIdx
				currentLine := activeLyrics[trueLineIdx]
				playTime := clock.GetCurrent()
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					LineIndex: trueLineIdx, Text: currentLine.Text,
					Timestamp: currentLine.Time, PlayTime: playTime,
					Progress: player.ClampFloat32(playTime/songDuration, 0, 1),
				})
			}
		}
	}
}

func getArtists(artists []struct {
	Name string `json:"name"`
}) string {
	names := []string{}
	for _, a := range artists {
		names = append(names, a.Name)
	}
	return strings.Join(names, " / ")
}
