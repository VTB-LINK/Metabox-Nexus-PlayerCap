package cloudmusic

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/lyric"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "cloudmusicv3"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("CloudMusic")

// CloudMusicPlayer 网易云音乐播放器
type CloudMusicPlayer struct {
	player.BaseEmitter
	offsetMs  int
	pollMs    int
	connected int32 // atomic：CDP 会话是否在进行（网易云已连上）。供特效捕获器门控生命周期。
}

// IsConnected 报告取词 CDP 会话是否在进行（网易云已连上并在监听）。
// 特效捕获器据此门控：未连上时静默待命，不独立探测 9222、不刷屏。
func (p *CloudMusicPlayer) IsConnected() bool {
	return atomic.LoadInt32(&p.connected) == 1
}

// New 创建网易云播放器
func New(offsetMs, pollMs int) *CloudMusicPlayer {
	return &CloudMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// baseRealClock 是 runSession 的位置时钟：在走时位置 = BaseRealTime 加上自 AnchorTime 起经过的时间，
// 否则为 BaseRealTime；不钳时长。读写都按调用方给的时刻，不在内部取当前时刻。时钟只有一条时间线：
// 对外位置、seek 检测与冻结都取 at，只有行匹配取 lineAt（doc/design/cloudmusic-dom-clock-sync-design.md §3.4）。
type baseRealClock struct {
	BaseRealTime        float32
	AnchorTime          time.Time
	LastKnownLyricIndex int
	Playing             bool

	// lineFloor 是行匹配下限：对表回拨时记下回拨前行匹配所用的位置，时钟走过它之前行匹配不低于它，
	// 行号因此不随回拨回退。零值即没有下限（时钟读数不为负）。
	lineFloor float32
}

// at 返回 t 时刻的时钟读数。
func (c *baseRealClock) at(t time.Time) float32 {
	return player.LivePosition(c.BaseRealTime, c.AnchorTime, t, c.Playing, 0)
}

// lineAt 返回 t 时刻行匹配用的读数：at 与行匹配下限的较大者。只供行匹配。
func (c *baseRealClock) lineAt(t time.Time) float32 {
	return max(c.at(t), c.lineFloor)
}

// adjust 把 t 时刻的读数改到 want（对表）。回拨时把回拨前行匹配所用的位置记为下限。
func (c *baseRealClock) adjust(want float32, t time.Time) {
	if want < c.at(t) {
		c.lineFloor = c.lineAt(t)
	}
	c.BaseRealTime, c.AnchorTime = want, t
}

// jump 把 t 时刻的读数置为 pos（seek 检测），清掉下限：跳转后行号跟着位置走。
func (c *baseRealClock) jump(pos float32, t time.Time) {
	c.BaseRealTime, c.AnchorTime, c.lineFloor = pos, t, 0
}

// setPlaying 让时钟在 t 时刻起走或冻结。冻结在时钟读数（at）而不是下限：playback_pause 的位置要与
// 其它对外事件同一条时间线；下限保留，恢复后行匹配仍取较大者，行号不回退。
func (c *baseRealClock) setPlaying(playing bool, t time.Time) {
	if playing == c.Playing {
		return
	}
	c.BaseRealTime, c.AnchorTime, c.Playing = c.at(t), t, playing
}

func normalizeSongIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), " ")
}

func buildSongIdentityKey(songName, songArtist string) string {
	name := normalizeSongIdentity(songName)
	if name == "" {
		return ""
	}
	return name + "\x00" + normalizeSongIdentity(songArtist)
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

// unsupportedRecheck 检测到不支持的网易云版本后，多久重新探一次。
// 取大值是因为「装的是 v2」是个近乎永久的状态：既不该占 CPU 空转，也得给用户升级后
// 自动恢复的机会（不必重启 PlayerCap）。
const unsupportedRecheck = 30 * time.Second

// EffectivePollInterval 返回配置轮询间隔为 pollMs 毫秒时网易云实际采用的间隔：低于 50ms 抬到 100ms，
// 兼防 time.NewTicker(<=0) panic。轮询循环与 /service-status 的 config_effective 共用此函数，下限只在这里定义。
func EffectivePollInterval(pollMs int) time.Duration {
	d := time.Duration(pollMs) * time.Millisecond
	if d < 50*time.Millisecond {
		return 100 * time.Millisecond
	}
	return d
}

// Start 启动轮询循环（阻塞）
func (p *CloudMusicPlayer) Start() {
	// 0. Patch Windows registry auto-start
	watchdog.PatchRegistryAutoStart()

	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("NetEase Cloud Music not running"), Reason: player.ReasonProcessNotRunning})

	warnedUnsupported := false // v2 警告只打一次（它是永久状态，每轮打就是新的刷屏）

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		restarted, err := watchdog.EnsureDebugMode()
		var unsupported *watchdog.UnsupportedVersionError
		if errors.As(err, &unsupported) {
			// v2：什么都不做。watchdog 已在 taskkill 之前挡住，这里只负责「别再往下走」。
			//
			// **本条真正治的是什么，别搞错**（issue #40 的现象描述实测有误）：
			//   - 治的是 **taskkill**：v2 用户的网易云会被杀一次并强制重启，换来一个永远连不上
			//     的端口。实测确认只杀「一次」——`hasFlags` 看的是 `Cmdline()` 里有没有我们传的
			//     marker，而 Windows 记录的是**我们传了什么**，与 v2 认不认识那些参数无关，
			//     所以第二轮起就早退了。杀一次也是杀，直播中尤其。
			//   - 治的是 **status 说谎**：这里原先发 waiting_process「网易云音乐未启动」——
			//     **假的**，他的网易云明明开着。这才是最误导的一条，issue 反而没提。
			//   - **不治「刷屏」**：issue 说「连不上会打印一大堆垃圾」，但实测 9222 不通时
			//     整条路径 **10 秒 0 行日志**（Connect 失败只 sleep+continue，不打日志）。
			//     真正的 55.8 行/秒需要「9222 开着且会话立刻死」，那是与 v2/v3 无关的另一条缺陷。
			//
			// 只打一次（用户选的 A 方案）：v2 是**永久状态**，每轮打就是新的刷屏。
			if !warnedUnsupported {
				log.Warn("%s (%s) — skipped NetEase lyric capture and effect mirroring; upgrade to v%d or later to use them",
					unsupported.Error(), unsupported.ExePath, 3)

				// 这是个「预期之外」的事件：我们只支持 v3+，而这台机器上装的是 v2。
				// 上报是为了回答「有多少主播还在用 v2」——那个数字决定要不要为 v2 做点什么。
				// ReportOnce 自己去重，与上面 warnedUnsupported 的重置逻辑无关（那条是给主播的
				// 提醒，状态变了该再提醒；这条是给我们的情报，一台机器一次就够）。
				telemetry.ReportOnce("cloudmusic.unsupported_version",
					"网易云音乐版本不支持 CDP（需 v3+）",
					map[string]string{"cloudmusicv3.version": unsupported.Version},
					map[string]any{"exe_path": unsupported.ExePath})

				warnedUnsupported = true
			}
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{
				Status: "standby", Detail: fmt.Sprintf(i18n.T("NetEase Cloud Music v%s unsupported (requires v3+)"), unsupported.Version), Reason: player.ReasonVersionUnsupported,
			})
			time.Sleep(unsupportedRecheck)
			continue
		}
		warnedUnsupported = false // 版本变了（升级/换装）→ 下次再是 v2 可以再警告一次
		if err != nil {
			log.Error("watchdog error: %v", err)
		}
		if restarted {
			log.Info("restarted NetEase Cloud Music, waiting 5s...")
			time.Sleep(5 * time.Second)
		}

		client, err := cdp.Connect()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("CDP connected, monitoring playback state")
		atomic.StoreInt32(&p.connected, 1) // 供特效捕获器门控：网易云已连上才截帧
		p.runSession(client)
		atomic.StoreInt32(&p.connected, 0)
		log.Info("session ended, waiting for NetEase Cloud Music to restart...")
		p.InvalidateSongGen() // 作废在飞的封面回写，别让它在 ClearSongData 之后复活已清空的 SongInfo
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("NetEase Cloud Music has exited"), Reason: player.ReasonProcessExited})
		p.Emit(player.EventClearSongData, nil)
	}
}

func (p *CloudMusicPlayer) runSession(client *cdp.Client) {
	var lastSongIdentityKey string
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
	var dom domSync    // 按 DOM 跳秒对表：上一拍的观测
	var pending untold // 校准改动了时钟而前端尚不知道的量

	// emitPos 发出带位置的对外事件（all_lyrics、lyric_update、playback_pause、playback_resume）。前端据它
	// 落锚到此刻的时钟读数，此前的校准都已包含在内，未告知量随之清零。**本包这四种事件只许经它发出**
	// （✅ player/cloudmusicanchor_lint_test.go）：绕过它发一条，未告知量不清零，随后会多发一条由校准
	// 触发的 playback_resume。
	emitPos := func(evtType string, data interface{}) {
		p.Emit(evtType, data)
		pending.anchored()
	}

	// posNow 返回此刻的时钟读数，供带位置的对外事件与位置采样取 position。下游按事件到达的时刻落锚
	// （服务端 UpdatePlayerState 记 PositionAt，前端自收到起按墙钟插值），所以取发出那一刻的读数，不取
	// 本拍的 now：两者之间隔着阻塞调用（Redux 歌词晚到时的 ExtractTlyricLines）时，按 now 取的位置到达时
	// 已落后一个阻塞时长，直到下一条带位置的事件。没有阻塞的拍上两者只差微秒。
	posNow := func() float32 { return clock.at(time.Now()) }

	ticker := time.NewTicker(EffectivePollInterval(p.pollMs))
	defer ticker.Stop()

	var errLog extractErrLog // 会话级：Extract 错误的降噪状态

	for {
		select {
		case <-p.StopCh:
			return
		case <-ticker.C:
		}

		// sent / got 紧贴这次 Extract 取：DOM 进度是在两者之间读到的，对表按它们算跳秒发生的区间
		// （domSync.observe）。
		sent := time.Now()
		data, err := client.Extract()
		got := time.Now()
		if err != nil {
			// "no root" 是启动期 #root 未就绪的预期噪声，原样静默（别扩大这条过滤面：
			// 'null: no root' / 'null: no react container' / 'null: store not found' 三串同生于
			// fa59084，作者知道另两串存在却只过滤了这一个 —— 那是选择，不是遗漏）。
			// 其余错误交给降噪：既不刷屏，也不彻底静默。
			if !strings.Contains(err.Error(), "no root") {
				if should, repeat, dur := errLog.next(err.Error(), time.Now()); should {
					if repeat {
						log.Warn("Extraction error persisting for %s (still retrying): %v", dur.Round(time.Second), err)
					} else {
						log.Warn("Extraction error: %v", err)
					}
				}
			}
			if client.IsClosed() {
				return
			}
			continue
		}
		errLog.reset() // 取词成功 → 下次故障可再记一次

		if data == nil || data.CurPlaying == nil {
			continue
		}

		// 一拍里用到两个时刻，不能合成一个：
		//   - now 配与 DOM 读数成对的读写：起走与冻结、行匹配、seek 检测里与 DOM 比的量及置位、对表、通知的
		//     限频。取 got，即读到 DOM 进度的时刻；换歌那一拍在分支之后重取（见分支末尾）。改配「用到它时」
		//     的时刻，本拍里阻塞调用的耗时会被算成时钟与界面进度的偏差。
		//   - 对外事件的 position 与位置采样取发出那一刻的读数（posNow）：下游按到达的时刻落锚。
		now := got

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
		songIdentityKey := buildSongIdentityKey(songName, songArtist)
		if songIdentityKey != "" && songIdentityKey != lastSongIdentityKey {
			songChanged = true
			lastSongIdentityKey = songIdentityKey
			lastPlayingState = data.PlayingState // 保留当前状态，避免切歌时误触 state change → 多余的 resume(0)
			lastLineIdx = -1
			activeLyrics = nil
			activeSongID = ""
			isPureMusic = false
			cdpLyricsOK = false
			songDuration = 0
			clock = baseRealClock{LastKnownLyricIndex: -1} // 行匹配下限随之清掉
			pending.anchored()                             // 上一首的未告知量不带到新歌
			lastSongChangeTime = time.Now()
			currentSongName = songName
			currentSongArtist = songArtist

			log.Info("♪ song: %s - %s", songName, songArtist)

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
					log.Info("retried Redux ID successfully: %s", activeSongID)
				} else {
					log.Info("Redux snapshot for current song not yet aligned; lyrics and cover will be filled in later")
				}
			}
			matchedRedux = snapshotMatchesCurrentSong(data, songName, songArtist)
			if activeSongID != "" {
				log.Info("using Redux ID: %s", activeSongID)
			}
			if activeSongID == "" {
				targetDurationMs := 0
				if data.CurPlaying != nil && data.CurPlaying.Track.Duration > 0 {
					targetDurationMs = data.CurPlaying.Track.Duration
				}
				if foundID, searchErr := lyric.SearchSongID(songName, songArtist, targetDurationMs); searchErr == nil && foundID != "" {
					activeSongID = foundID
					log.Info("Redux ID empty, searched matching NetEase ID: %s (duration=%dms)", activeSongID, targetDurationMs)
				} else if searchErr != nil {
					log.Warn("Redux ID empty, NetEase ID search failed: %v", searchErr)
				}
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

			// 异步获取封面 base64，完成后补发 song_info_update。
			// 带代次守卫：下载最长 5s，其间可能已换歌；迟到的回写若无守卫会把这首歌的
			// 标题+封面盖在下一首上，且 song_info_update 只在换歌分支发一次，整首歌不自愈。
			coverGen := p.NewSongGen()
			go func(gen uint64, name, artist, title, cover string) {
				defer telemetry.Guard()
				if b64 := player.FetchCoverBase64("CloudMusic", cover, 5*time.Second); b64 != "" {
					p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
						Name: name, Singer: artist, Title: title,
						Cover: cover, CoverBase64: b64,
					})
				}
			}(coverGen, songName, songArtist, songTitle, songCover)

			// Fetch lyrics via CDP using Redux ID
			if activeSongID != "" {
				lrcResult, err := client.FetchLyricsViaCDP(activeSongID)
				if err != nil {
					log.Warn("lyric fetch failed: %v", err)
				} else if lrcResult.PureMusic || lrcResult.NoLyric {
					// CDP 返回纯音乐，用 Go HTTP API 二次确认
					log.Info("CDP reported pure music (ID=%s)", activeSongID)
					// 判据必须是 hasRealText 而不是 len()：ParseLRC 会保留 intentional blank
					// （正文为空、表示此刻清空歌词的行），一首只有 blank 的 lrc 行数 > 0 却
					// 一个字都没有 —— 按行数判就会误判成「有歌词」、置 cdpLyricsOK 关掉 Redux
					// fallback，OBS 整首空白且不走下面的「标记纯音乐」清屏路径。
					// 与 resolveCDPLyrics 同一判据，别让这两条分支再分叉。
					if apiLyrics, err2 := lyric.FetchLyrics(activeSongID, offsetSec); err2 == nil && hasRealText(apiLyrics) {
						log.Info("API recheck (ID=%s) found %d lyric lines, using API lyrics; per-character: %s", activeSongID, len(apiLyrics), lyricDetailedFlag(apiLyrics))
						for _, l := range apiLyrics {
							activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
								Index: l.Index, Time: l.Time, Text: l.Text, SubText: l.SubText, RomaText: l.RomaText, TextDetailed: l.TextDetailed,
							})
						}
						cdpLyricsOK = true
					} else {
						log.Info("API recheck (ID=%s) found no lyrics either, marking as pure music", activeSongID)
						isPureMusic = true
						if matchedRedux && data.CurPlaying.Track.Duration > 0 {
							songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
						}
						playTime := posNow()
						progress := player.ClampProgress(playTime, songDuration)
						emitPos(player.EventAllLyrics, &player.AllLyricsData{
							Title: songTitle, Duration: songDuration, Position: playTime, Progress: progress,
							Lyrics: []player.LyricLine{}, Count: 0,
						})
						emitPos(player.EventLyricUpdate, &player.LyricUpdate{
							Index: -1, Text: "", Timestamp: 0, Position: playTime, Progress: progress,
						})
					}
				} else {
					parsed, ok := resolveCDPLyrics(lrcResult.Lrc, lrcResult.Tlyric, lrcResult.Romalrc, lrcResult.Yrc, offsetSec)
					if ok {
						cdpLyricsOK = true
						log.Info("lyrics loaded (CDP): %d lines (ID=%s); per-character: %s", len(parsed), activeSongID, lyricDetailedFlag(parsed))
						for _, l := range parsed {
							activeLyrics = append(activeLyrics, cdp.ExtractedLyric{
								Index: l.Index, Time: l.Time, Text: l.Text, SubText: l.SubText, RomaText: l.RomaText, TextDetailed: l.TextDetailed,
							})
						}
					} else {
						// CDP 声称有歌词（非 PureMusic/NoLyric），但解析出 0 行——lrc 只含元数据
						// 标签（[ti:]/[ar:]）而无时间戳行。**不置 cdpLyricsOK**，交给下面的 Redux
						// fallback（:379 用 Redux 歌词，:407 兜底标记纯音乐清屏）；置了会挡住 fallback，
						// activeLyrics 保持空、前端整首歌停在上一首。
						log.Warn("CDP lyrics parsed to 0 lines (ID=%s), deferring to Redux fallback", activeSongID)
					}
				}
			}

			// 若 Redux ID 为空且 ForceFetch 后 Redux 歌词已有数据，直接采纳
			if activeSongID == "" && matchedRedux && len(data.Lyrics) > 0 {
				activeLyrics = data.Lyrics
				cdpLyricsOK = true
				log.Info("Redux ID empty, using Redux lyrics directly: %d lines; per-character: no", len(activeLyrics))
				// 通过独立 CDP 评估读取 Redux 中的翻译歌词
				if tmap := client.ExtractTlyricLines(); tmap != nil {
					applyTlyricMap(activeLyrics, tmap)
				}
			}

			// Broadcast all lyrics（仅在有普通歌词时发送，避免空歌词 + Redux 补发导致双发）
			if len(activeLyrics) > 0 {
				lyricItems := make([]player.LyricLine, len(activeLyrics))
				for i, l := range activeLyrics {
					lyricItems[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.RomaText, l.TextDetailed, offsetSec)
				}

				if songDuration == 0 && matchedRedux && data.CurPlaying.Track.Duration > 0 {
					songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
				}
				playTime := posNow()
				progress := player.ClampProgress(playTime, songDuration)

				// 网易云对没有歌词的歌返回一行「纯音乐，请欣赏」而非空歌词，归一成 index:-1。
				//
				// 这里是两条路径的汇合点，故只需拦一次：
				//   :356 CDP 说是纯音乐 → HTTP API 二次确认 → API 返回那句提示语，
				//        而 hasRealText 认为它是真文本（它没错，那七个字确实是文本），
				//        于是判定「API 说有歌词」并流到这里。**CDP 的判断本来是对的。**
				//   :389 CDP 说有歌词 → resolveCDPLyrics 解析出那一行 → 也流到这里。
				//
				// activeLyrics 必须一起清：它是后续轮询的输入，不清的话 all_lyrics 说
				// count:0、轮询却仍按那一行发 index:0。
				if player.IsPureMusicOnly(lyricItems) {
					log.Info("platform returned only the hint \"%s\", treating as no lyrics", lyricItems[0].Text)
					pureHint := lyricItems[0].Text
					activeLyrics = nil
					isPureMusic = true
					emitPos(player.EventAllLyrics, &player.AllLyricsData{
						Title: songTitle, Duration: songDuration, Position: playTime, Progress: progress,
						Lyrics: []player.LyricLine{}, Count: 0,
					})
					emitPos(player.EventLyricUpdate, &player.LyricUpdate{
						Index: -1, Text: pureHint, Timestamp: 0, Position: playTime, Progress: progress,
					})
				} else {
					emitPos(player.EventAllLyrics, &player.AllLyricsData{
						Title: songTitle, Duration: songDuration, Position: playTime, Progress: progress,
						Lyrics: lyricItems, Count: len(lyricItems),
					})
				}
			}

			// 切歌时主动发 status_update（lastPlayingState 不再重置，state change 段不会重复触发）
			if data.PlayingState == 2 {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: songTitle})
			}

			// 上面的取词是阻塞的（实测 459～696ms），时钟在它之后才由 0 起走，同改动前：换歌后的落后量
			// 「分支耗时 − 起播延迟」是对表设计的起点（设计 §1.2）。起点若配 got（分支之前），时钟会超前
			// 一个分支耗时，每次换歌都在约 1s 后误报一次回跳。这一拍本就不参与对表（domSample.sameSong）。
			now = time.Now()
		}

		// lateLyrics 发出本拍补发的歌词事件（非换歌拍的 all_lyrics 与紧随的 index:-1 的 lyric_update），由
		// 下面两个分支之一设置，在对表之后调用（见调用处）。两个分支至多一个成立：前一个成立后 isPureMusic
		// 为真或 activeLyrics 非空，后一个的条件不再成立。状态（activeLyrics、isPureMusic、songDuration）仍在
		// 分支里就地更新，本拍的行匹配要用；位置在发出时才取。
		var lateLyrics func()

		// Redux lyrics appeared later（切歌 tick 跳过，避免旧歌词回写覆盖新歌词）
		// 等待 ForceFetchLyricsInRedux 完成后再接受 Redux 歌词（冷却 1.5 秒防止旧歌词被误采纳）
		if !songChanged && !isPureMusic && !cdpLyricsOK && len(data.Lyrics) > 0 && len(data.Lyrics) != len(activeLyrics) && time.Since(lastSongChangeTime) > 1*time.Second {
			if snapshotMatchesCurrentSong(data, currentSongName, currentSongArtist) && (activeSongID == "" || data.CurPlaying.ID == activeSongID || data.CurPlaying.ID == "") {
				activeLyrics = data.Lyrics
				log.Info("lyrics loaded (Redux): %d lines; per-character: no", len(activeLyrics))

				// 通过独立 CDP 评估读取 Redux 中的翻译歌词
				if tmap := client.ExtractTlyricLines(); tmap != nil {
					applyTlyricMap(activeLyrics, tmap)
				}

				// 补发全量歌词给前端
				lyricItems := make([]player.LyricLine, len(activeLyrics))
				for i, l := range activeLyrics {
					// Redux fallback 路径不产音译：l.RomaText 恒空（Redux 提取的 ExtractedLyric
					// 不含 romaText），这里透传即空串，roma_text 契约上仍在、不污染 sub_text。
					lyricItems[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.RomaText, l.TextDetailed, offsetSec)
				}
				if songDuration == 0 && data.CurPlaying.Track.Duration > 0 {
					songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
				}

				// 同 :420 —— Redux 里同样可能只有那一行提示语。
				// 文案与那边逐字一致（AGENTS.md §6.1）：来源是 Redux 这件事，上面那条
				// 「歌词加载完成(Redux)」已经说了；这里再分叉一版措辞只会让 grep 漏掉一半。
				if player.IsPureMusicOnly(lyricItems) {
					log.Info("platform returned only the hint \"%s\", treating as no lyrics", lyricItems[0].Text)
					pureHint := lyricItems[0].Text
					activeLyrics = nil
					isPureMusic = true
					lateLyrics = func() {
						playTime := posNow()
						progress := player.ClampProgress(playTime, songDuration)
						emitPos(player.EventAllLyrics, &player.AllLyricsData{
							Title: currentSongTitle, Duration: songDuration, Position: playTime, Progress: progress,
							Lyrics: []player.LyricLine{}, Count: 0,
						})
						emitPos(player.EventLyricUpdate, &player.LyricUpdate{
							Index: -1, Text: pureHint, Timestamp: 0, Position: playTime, Progress: progress,
						})
					}
				} else {
					lateLyrics = func() {
						playTime := posNow()
						progress := player.ClampProgress(playTime, songDuration)
						emitPos(player.EventAllLyrics, &player.AllLyricsData{
							Title: currentSongTitle, Duration: songDuration, Position: playTime, Progress: progress,
							Lyrics: lyricItems, Count: len(lyricItems),
						})
					}
				}
			}
		}

		// 超时兜底：CDP 失败且 Redux fallback 超时后仍无歌词，清空前端
		if !songChanged && !isPureMusic && !cdpLyricsOK && len(activeLyrics) == 0 && time.Since(lastSongChangeTime) > 3*time.Second {
			log.Info("lyric fetch timed out, marking pure music and clearing frontend")
			isPureMusic = true
			if songDuration == 0 && snapshotMatchesCurrentSong(data, currentSongName, currentSongArtist) && data.CurPlaying != nil && data.CurPlaying.Track.Duration > 0 {
				songDuration = float32(data.CurPlaying.Track.Duration) / 1000.0
			}
			lateLyrics = func() {
				playTime := posNow()
				progress := player.ClampProgress(playTime, songDuration)
				emitPos(player.EventAllLyrics, &player.AllLyricsData{
					Title: currentSongTitle, Duration: songDuration, Position: playTime, Progress: progress,
					Lyrics: []player.LyricLine{}, Count: 0,
				})
				emitPos(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: "", Timestamp: 0, Position: playTime, Progress: progress,
				})
			}
		}

		// Update clock：非在播转在播时起走，在播转非在播时冻结在时钟读数。
		isPlaying := (data.PlayingState == 2)
		clock.setPlaying(isPlaying, now)

		// Find current lyric（应用 offset，使用 displayStart 边界）。**只有这里取 lineAt**：对表回拨之后、
		// 时钟走过回拨前的位置之前，行匹配不低于回拨前的位置，行号不因回拨回退；其余各处一律取 at。
		trueLineIdx := -1
		currentTime := player.LyricMatchTime(clock.lineAt(now), offsetSec)
		for i := len(activeLyrics) - 1; i >= 0; i-- {
			ds := player.LyricDisplayStart(activeLyrics[i].Time, activeLyrics[i].TextDetailed)
			if currentTime >= ds {
				trueLineIdx = i
				break
			}
		}

		// Seek detection — 切歌后短暂抑制，等待 DOM 进度条刷新到新歌曲。与 DOM 比的是 now 时刻的时钟读数
		// （at）：拿行匹配下限去比，会把对表的回拨量算成偏差。它发的 playback_resume 排在对表之前，但判了
		// seek 的这一拍不对表。
		settled := time.Since(lastSongChangeTime) > 500*time.Millisecond
		seeked := false
		if data.DomTimeSec >= 0 && clock.Playing && settled {
			prevPlayTime := clock.at(now)
			diff := float32(data.DomTimeSec) - prevPlayTime
			absDiff := diff
			if absDiff < 0 {
				absDiff = -absDiff
			}
			// 回跳阈值更低(1.0s)以支持逐字内 seek；前跳阈值保持 1.5s 避免抖动误报
			threshold := float32(1.5)
			if diff < 0 {
				threshold = 1.0
			}
			if absDiff > threshold {
				targetPlayTime := float32(data.DomTimeSec)
				if targetPlayTime+0.01 < prevPlayTime {
					log.Info("Seek-back detected: %.2fs → %.2fs", prevPlayTime, targetPlayTime)
				} else if targetPlayTime > prevPlayTime+0.01 {
					log.Info("forward seek detected: %.2fs → %.2fs", prevPlayTime, targetPlayTime)
				}
				clock.jump(targetPlayTime, now) // 置位并清掉行匹配下限：跳转后行号跟着位置走
				pos := posNow()
				emitPos(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: pos, Progress: player.ClampProgress(pos, songDuration)})
				seeked = true
			}
		}

		// 按 DOM 跳秒对表（doc/design/cloudmusic-dom-clock-sync-design.md §3，实测与取值的来历见 AGENTS.md §7.9）。
		// 必须在 seek 检测之后：判了 seek 的那一拍时钟刚被置成整秒值，不对表；也必须每拍都调 observe，
		// 不可用的拍要把上一拍顶掉。行匹配在此之前已算好，校准后的时钟从下一拍起进入行匹配。
		// 单次校准量达到门槛时打一行日志、不受通知限频约束：直播时 stderr 是唯一的现场记录，跳秒时刻
		// 不稳会表现为这一行反复出现。
		before := clock.at(now)
		synced := dom.observe(domSample{
			sent: sent, got: got, sec: data.DomTimeSec,
			playing: clock.Playing, sameSong: !songChanged && settled, seeked: seeked,
		}, before)
		if d := synced - before; d != 0 {
			clock.adjust(synced, now)
			pending.add(d)
			if d >= syncNotifySec || d <= -syncNotifySec {
				log.Info("synced to UI progress: %.2fs → %.2fs", before, synced)
			}
		}

		// 本拍补发的歌词事件在对表之后才发出：位置是校准后的读数，本拍的校准量随之清零，拍末的通知检查不会
		// 再为它另发一条 playback_resume。放回对表之前（分支里就地发出），同一拍会先后落锚两次（AGENTS.md
		// §3.2）。排在状态变化之前，与推迟之前的先后相同。
		if lateLyrics != nil {
			lateLyrics()
		}

		// State change
		if data.PlayingState != lastPlayingState {
			lastPlayingState = data.PlayingState
			if data.PlayingState == 2 {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentSongTitle})
				if !seeked {
					playTime := posNow()
					log.Info("resumed @ %.2fs", playTime)
					emitPos(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: playTime, Progress: player.ClampProgress(playTime, songDuration)})
				}
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentSongTitle})
				playTime := posNow()
				emitPos(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: playTime, Progress: player.ClampProgress(playTime, songDuration)})
				log.Info("paused @ %.2fs", playTime)
			}
		}

		// Lyric boundary crossing
		if isPlaying && trueLineIdx >= 0 && trueLineIdx < len(activeLyrics) {
			if trueLineIdx != lastLineIdx {
				lastLineIdx = trueLineIdx
				currentLine := activeLyrics[trueLineIdx]
				// posNow() 是发出这一刻的时钟读数（本拍对表之后的值），进 Position、并据它算 Progress；它不取
				// 行匹配下限，否则前端会被锚回回拨前的时间线。play_time 由 BuildLyricUpdate 按歌词时间轴算
				// （原先为取它而构造了整个 LyricLine）。
				emitPos(player.EventLyricUpdate, player.BuildLyricUpdate(
					trueLineIdx, currentLine.Time, currentLine.Text, currentLine.SubText, currentLine.RomaText,
					currentLine.TextDetailed, offsetSec, posNow(), songDuration,
				))
			}
		}

		// 由校准触发的通知：在播、未告知量达到门槛、且距上一次这样的通知不短于限频间隔时发一条
		// playback_resume，位置取发出这一刻的时钟读数。必须排在行号变化的 lyric_update 之后：本拍对表之后
		// 只要发过带位置的对外事件（补发的歌词、状态变化、行号变化），未告知量已被清零，这里不会再发；排在
		// 对表之前的只有换歌分支与 seek 检测，那两种拍不对表。所以它不与任何带位置的事件同拍
		// （AGENTS.md §3.2 的同拍去重）。
		if isPlaying && pending.due(now) {
			pos := posNow()
			emitPos(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: pos, Progress: player.ClampProgress(pos, songDuration)})
		}

		// 位置采样（见 BaseEmitter.EmitPositionSample）：播放中、本曲的 all_lyrics 已发出（有歌词，或已判
		// 纯音乐）之后按固定间隔报一次。本播放器的时钟会被对表改动而不发事件（未达通知门槛的校准），
		// 服务端缓存的位置由其后的 lyric_update 与这里的采样带到。时刻与位置成对取在发出这一刻（服务端
		// 以到达的时刻为采样时刻），与上面 lyric_update 的 position 同一个量：不取行匹配下限；offset 只进了
		// currentTime，这里不带。它是内部事件，不清未告知量。
		if isPlaying && (len(activeLyrics) > 0 || isPureMusic) {
			t := time.Now()
			p.EmitPositionSample(t, clock.at(t), songDuration)
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

// resolveCDPLyrics 解析 CDP 返回的歌词并判定是否算「拿到了可用歌词」。
//
// ok 为真会让调用方置 cdpLyricsOK，从而关掉 Redux fallback——所以它**必须在解析出真正
// 有字的歌词行之后**才为真。ParseLRC 对只含元数据标签（[ti:]/[ar:]，无 [mm:ss.xx] 时间戳
// 行）的非空 lrc 会返回 0 行；此时若仍标记成功，activeLyrics 保持空、fallback 被挡，前端
// 整首歌停在上一首。
//
// PureMusic 分支里 API 二次确认那条（cloudmusic.go:304）**必须用同一个判据**（hasRealText），
// 两条都是「置 cdpLyricsOK → 关掉 Redux fallback」的入口，判据分叉就会漏掉一条。
// 注：这里原先写着「与那条『len(apiLyrics)>0 才置真』保持一致」——那句话是反的，
// d63a67d 把 ParseLRC 的契约从「行数即歌词数」改成「行数含 blank」之后，len() 判据就失效了，
// 而当时只改了本函数、漏了 :304。别再把 len() 当参照系。
func resolveCDPLyrics(lrc, tlyric, romalrc, yrc string, offsetSec float32) (parsed []lyric.LyricLine, ok bool) {
	parsed = lyric.ParseLRC(lrc)
	// 判据是「有没有**实词**行」，不是「有没有行」：ParseLRC 会保留 intentional blank
	// （正文为空、表示此刻清空歌词的行），一首只有 blank 的 lrc 解析出的行数 > 0 却一个字
	// 都没有——按行数判就会误判成「有歌词」，关掉 Redux fallback，OBS 整首空白。
	if !hasRealText(parsed) {
		return nil, false
	}
	lyric.MergeTlyric(parsed, tlyric)
	lyric.MergeRomalrc(parsed, romalrc)
	lyric.MergeYRC(parsed, yrc, offsetSec)
	return parsed, true
}

// hasRealText 判断解析结果里有没有**实词**行。intentional blank（Text 为空、表示此刻清空
// 歌词的行）不算——只有 blank 的结果等于「这首歌没有歌词」。
// extractErrRepeat 同一条持续中的 Extract 错误多久复读一次。
const extractErrRepeat = 60 * time.Second

// extractErrLog 给 Extract 的错误做降噪：同一错误首次打一次，之后每 extractErrRepeat
// 复读一次「仍在重试」，Extract 成功后重置（下次故障可再记一次）。
//
// **为什么不能每 tick 打**：poll 默认 30，被上面的 `<50ms→100ms` 抬到 100ms → **10 行/秒**。
// 日志走 stderr 且不落盘（AGENTS §0），几分钟就把控制台回滚缓冲冲干净，**连带冲掉其余
// 播放器的全部日志** —— 而那是直播出问题时唯一的现场证据。这是本条真正的危害。
//
// **为什么也不能彻底静默**：同上，它是唯一的现场证据。「一次都不打」和「10 行/秒」一样坏。
//
// **为什么不在这里加「连续失败 N 次就退出会话」**（原始审计发现的隐含修法，已核实为错）：
//   - Extract 出错时 `runSession` 只有 socket 死亡（`client.IsClosed()`）一条退出路径，看着像漏，
//     但它事实上挡住了一个更大的雷：退出会话 → 外层 for 重跑 `watchdog.EnsureDebugMode()`，
//     而那个函数在「Cmdline() 读失败」与「进程无保活 marker」之间**不可区分**，会
//     `taskkill /F /IM cloudmusic.exe`。今天「永不退出」让这个雷最多踩一次；加失败计数
//     等于提高频率 —— **直播中反复杀主播的网易云**。
//   - 而退出会话的收益近乎零：headline 场景（网易云更新后 fiber 形态变）下 EnsureDebugMode
//     因 `hasFlags==true` 直接早退、**本来就是 no-op**；真的是网易云更新，那 = 进程重启 =
//     socket 断 = `IsClosed()` 为真 = 已经会正常退出，压根不走这条路。
//   - `store not found` 是**冷启动必经串**（要求 Redux 的 playing 分片已 hydrate），纯次数
//     阈值会误杀启动期；而 poll 可被 per-player 覆盖，同一个 N 对应的真实时长能差 20 倍。
type extractErrLog struct {
	msg     string    // 当前持续中的错误串（空 = 无故障）
	since   time.Time // 本轮故障首次出现的时刻
	lastLog time.Time // 上次打日志的时刻
}

// next 决定这条错误该不该打：shouldLog=false 即压制；repeat=true 表示这是「仍在重试」的
// 复读，dur 为已持续时长。
func (e *extractErrLog) next(msg string, now time.Time) (shouldLog, repeat bool, dur time.Duration) {
	if msg != e.msg {
		e.msg, e.since, e.lastLog = msg, now, now
		return true, false, 0
	}
	if now.Sub(e.lastLog) >= extractErrRepeat {
		e.lastLog = now
		return true, true, now.Sub(e.since)
	}
	return false, false, 0
}

// reset 在 Extract 成功后调用。
func (e *extractErrLog) reset() { e.msg = "" }

func hasRealText(lines []lyric.LyricLine) bool {
	for _, l := range lines {
		if l.Text != "" {
			return true
		}
	}
	return false
}

func lyricDetailedFlag(lines []lyric.LyricLine) string {
	for _, line := range lines {
		if len(line.TextDetailed.Words) > 0 {
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}

// applyTlyricMap applies a tlyric timestamp map (ms -> text) to extracted lyrics.
func applyTlyricMap(lyrics []cdp.ExtractedLyric, tmap map[int]string) {
	for i := range lyrics {
		if text, ok := tmap[int(lyrics[i].Time*1000+0.5)]; ok {
			lyrics[i].SubText = text
		}
	}
}
