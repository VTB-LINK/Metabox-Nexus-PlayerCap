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
	events   chan player.Event
	stopCh   chan struct{}
	offsetMs int
	pollMs   int
}

// New 创建网易云播放器
func New(offsetMs, pollMs int) *CloudMusicPlayer {
	return &CloudMusicPlayer{
		events:   make(chan player.Event, 128),
		stopCh:   make(chan struct{}),
		offsetMs: offsetMs,
		pollMs:   pollMs,
	}
}

func (p *CloudMusicPlayer) Name() string                { return PlayerName }
func (p *CloudMusicPlayer) Events() <-chan player.Event { return p.events }
func (p *CloudMusicPlayer) Stop()                       { close(p.stopCh) }

func (p *CloudMusicPlayer) emit(evtType string, data interface{}) {
	select {
	case p.events <- player.Event{PlayerName: PlayerName, Type: evtType, Data: data}:
	default:
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

	p.emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: "网易云音乐未启动"})

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		restarted, err := watchdog.EnsureDebugMode()
		if err != nil {
			log.Error("Watchdog error: %v", err)
		}
		if restarted {
			log.Info("App restarted. Waiting 5s...")
			time.Sleep(5 * time.Second)
		}

		client, err := cdp.Connect()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("CDP connected. Polling started...")
		p.runSession(client)
		p.emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "网易云音乐已退出"})
		p.emit(player.EventClearSongData, nil)
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

	pollInterval := time.Duration(p.pollMs) * time.Millisecond
	if pollInterval < 50*time.Millisecond {
		pollInterval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
		}

		data, err := client.Extract()
		if err != nil {
			if !strings.Contains(err.Error(), "no root") {
				log.Warn("Extract err: %v", err)
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
			clock = baseRealClock{LastKnownLyricIndex: -1}

			log.Info("Song: %s - %s", songName, songArtist)

			// Search correct ID
			searchID, err := client.SearchSongViaCDP(songName, songArtist)
			if err != nil {
				log.Warn("Search failed: %v, using Redux ID: %s", err, data.CurPlaying.ID)
				searchID = data.CurPlaying.ID
			}
			activeSongID = searchID

			// Fetch HD cover
			if detailCover, err := client.FetchCoverViaCDP(activeSongID); err == nil && detailCover != "" {
				songCover = detailCover
			}

			songTitle := songName + " - " + songArtist
			currentSongTitle = songTitle

			// Download cover → base64 (2s timeout, non-blocking on failure)
			coverBase64 := player.FetchCoverBase64(songCover, 2*time.Second)
			if coverBase64 != "" {
				log.Detail("封面已获取 → base64")
			}

			p.emit(player.EventSongInfoUpdate, &player.SongInfo{
				Name: songName, Singer: songArtist, Title: songTitle,
				Cover: songCover, CoverBase64: coverBase64,
			})

			// Fetch lyrics via CDP
			lrcText, err := client.FetchLyricsViaCDP(activeSongID)
			if err != nil {
				log.Warn("Lyrics fetch failed: %v", err)
				if len(data.Lyrics) > 0 {
					activeLyrics = data.Lyrics
				}
			} else {
				parsed := lyric.ParseLRC(lrcText)
				for _, l := range parsed {
					activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
						Index: l.Index, Time: l.Time, Text: l.Text,
					})
				}
			}

			// Broadcast all lyrics
			lyricItems := make([]player.LyricLine, len(activeLyrics))
			for i, l := range activeLyrics {
				lyricItems[i] = player.LyricLine{Index: l.Index, Time: l.Time, Text: l.Text}
			}

			var durSec float32
			if data.CurPlaying.Track.Duration > 0 {
				durSec = float32(data.CurPlaying.Track.Duration) / 1000.0
			}

			p.emit(player.EventAllLyrics, &player.AllLyricsData{
				SongTitle: songTitle, Duration: durSec, Lyrics: lyricItems, Count: len(lyricItems),
			})

			// 切歌时主动发 status_update（lastPlayingState 不再重置，state change 段不会重复触发）
			if data.PlayingState == 2 {
				p.emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: songTitle})
			}
		}

		// Redux lyrics appeared later（切歌 tick 跳过，避免旧歌词回写覆盖新歌词）
		if !songChanged && len(data.Lyrics) > 0 && len(data.Lyrics) != len(activeLyrics) {
			activeLyrics = data.Lyrics
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

		// Seek detection
		seeked := false
		if data.DomTimeSec >= 0 && clock.Playing {
			diff := float32(data.DomTimeSec) - clock.GetCurrent()
			if diff < 0 {
				diff = -diff
			}
			if diff > 1.5 {
				clock.BaseRealTime = float32(data.DomTimeSec)
				clock.AnchorTime = time.Now()
				p.emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				seeked = true
			}
		}

		// State change
		if data.PlayingState != lastPlayingState {
			lastPlayingState = data.PlayingState
			if data.PlayingState == 2 {
				p.emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentSongTitle})
				if !seeked {
					p.emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
				}
			} else {
				p.emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentSongTitle})
				p.emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{PlayTime: clock.GetCurrent()})
			}
		}

		// Lyric boundary crossing
		if isPlaying && trueLineIdx >= 0 && trueLineIdx < len(activeLyrics) {
			if trueLineIdx != lastLineIdx {
				lastLineIdx = trueLineIdx
				currentLine := activeLyrics[trueLineIdx]
				p.emit(player.EventLyricUpdate, &player.LyricUpdate{
					LineIndex: trueLineIdx, Text: currentLine.Text,
					Timestamp: currentLine.Time, PlayTime: clock.GetCurrent(),
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
