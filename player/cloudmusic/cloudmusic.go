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

			log.Info("♪ 歌曲: %s - %s", songName, songArtist)

			// Search correct ID
			searchID, err := client.SearchSongViaCDP(songName, songArtist)
			if err != nil {
				log.Warn("搜索失败: %v，使用 Redux ID: %s", err, data.CurPlaying.ID)
				searchID = data.CurPlaying.ID
			}
			activeSongID = searchID

			// Fetch HD cover
			if detailCover, err := client.FetchCoverViaCDP(activeSongID); err == nil && detailCover != "" {
				songCover = detailCover
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

			// Fetch lyrics via CDP
			lrcText, err := client.FetchLyricsViaCDP(activeSongID)
			if err != nil {
				log.Warn("歌词获取失败: %v", err)
				// 不使用 data.Lyrics 兜底（可能是旧歌词），等 ForceFetchLyricsInRedux 更新后由 Redux fallback 统一推送
			} else if lrcText == "[PURE_MUSIC]" || lrcText == "[NO_LYRIC]" {
				// 搜索 ID 返回纯音乐，但 Redux ID 可能是正确的（如组曲/混音歌曲搜索匹配到原曲）
				reduxID := data.CurPlaying.ID
				if reduxID != "" && reduxID != activeSongID {
					log.Info("搜索 ID %s 返回纯音乐，尝试 Redux ID %s", activeSongID, reduxID)
					lrcText2, err2 := client.FetchLyricsViaCDP(reduxID)
					if err2 == nil && lrcText2 != "[PURE_MUSIC]" && lrcText2 != "[NO_LYRIC]" && lrcText2 != "" {
						log.Info("Redux ID %s 歌词获取成功，使用 Redux 歌词", reduxID)
						activeSongID = reduxID
						lrcText = lrcText2
					}
				}
			}

			if lrcText == "[PURE_MUSIC]" || lrcText == "[NO_LYRIC]" {
				log.Info("检测到纯音乐/无歌词，清空歌词")
				isPureMusic = true
				// 立即发送空歌词
				if data.CurPlaying.Track.Duration > 0 {
					songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
				}
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					SongTitle: songTitle, Duration: songDuration, PlayTime: clock.GetCurrent(), Lyrics: []player.LyricLine{}, Count: 0,
				})
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					LineIndex: -1, Text: "", Timestamp: 0, PlayTime: clock.GetCurrent(),
				})
			} else {
				cdpLyricsOK = true
				parsed := lyric.ParseLRC(lrcText)
				for _, l := range parsed {
					activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
						Index: l.Index, Time: l.Time, Text: l.Text,
					})
				}
			}

			// Broadcast all lyrics（仅在有普通歌词时发送，避免空歌词 + Redux 补发导致双发）
			if len(activeLyrics) > 0 {
				lyricItems := make([]player.LyricLine, len(activeLyrics))
				for i, l := range activeLyrics {
					lyricItems[i] = player.LyricLine{Index: l.Index, Time: l.Time, Text: l.Text}
				}

				if songDuration == 0 && data.CurPlaying.Track.Duration > 0 {
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

			// 异步强制 NetEase PC 发起内部底层 API 获取歌词
			go client.ForceFetchLyricsInRedux()
		}

		// Redux lyrics appeared later（切歌 tick 跳过，避免旧歌词回写覆盖新歌词）
		// 等待 ForceFetchLyricsInRedux 完成后再接受 Redux 歌词（冷却 1.5 秒防止旧歌词被误采纳）
		if !songChanged && !isPureMusic && !cdpLyricsOK && len(data.Lyrics) > 0 && len(data.Lyrics) != len(activeLyrics) && time.Since(lastSongChangeTime) > 1*time.Second {
			if activeSongID == "" || data.CurPlaying.ID == activeSongID || data.CurPlaying.ID == "" || data.CurPlaying.Track.Name == lastDomSongName {
				activeLyrics = data.Lyrics
				log.Info("Redux 备用歌词加载成功: %d 行", len(activeLyrics))

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
			diff := float32(data.DomTimeSec) - clock.GetCurrent()
			if diff < 0 {
				diff = -diff
			}
			if diff > 1.5 {
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
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				}
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentSongTitle})
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
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
