package wesing

import (
	"fmt"
	"strings"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

const PlayerName = "wesing"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Wesing")

// WesingPlayer 全民K歌播放器
type WesingPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建全民K歌播放器
func New(offsetMs, pollMs int) *WesingPlayer {
	return &WesingPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// Start 启动全民K歌轮询循环（阻塞）
func (p *WesingPlayer) Start() {
	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := time.Duration(p.pollMs) * time.Millisecond

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: "K歌客户端未启动"})
		p.Emit(player.EventClearSongData, nil)

		handle, pid := p.waitForProcess()
		p.runSession(handle, pid, offsetSec, pollInterval)
		proc.CloseProc(handle)

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "K歌客户端已退出"})
		p.Emit(player.EventClearSongData, nil)
		log.Info("会话结束，等待新的 WeSing 进程...")
		time.Sleep(2 * time.Second)
	}
}

func (p *WesingPlayer) waitForProcess() (syscall.Handle, uint32) {
	log.Info("等待 WeSing.exe 启动...")
	printed := false
	for {
		select {
		case <-p.StopCh:
			return 0, 0
		default:
		}
		pid, err := proc.FindProcess("WeSing.exe")
		if err == nil {
			handle, err := proc.OpenProc(pid)
			if err == nil {
				log.Info("找到 WeSing.exe (PID: %d)", pid)
				return handle, pid
			}
		}
		if !printed {
			log.Info("WeSing.exe 未运行，等待中...")
			printed = true
		}
		time.Sleep(2 * time.Second)
	}
}

// exitReason 轮询退出原因
type exitReason int

const (
	exitProcessDied exitReason = iota
	exitSongChanged
	exitWindowClosed
)

func (p *WesingPlayer) runSession(handle syscall.Handle, pid uint32, offsetSec float32, pollInterval time.Duration) {
	modules, err := proc.EnumModules(pid)
	if err != nil {
		log.Error("枚举模块失败: %v", err)
		return
	}

	lastTitle := ""
	var cachedTimeAddr uint32
	var lastPhase proc.PlayPhase
	lastLoadingTitle := ""

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		if !p.isProcessAlive() {
			return
		}

		state := proc.GetPlayState(pid)

		switch state.Phase {
		case proc.PhaseStandby:
			if lastPhase != proc.PhaseStandby {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song", Detail: "K歌窗口未打开"})
				p.Emit(player.EventClearSongData, nil)
				p.Emit(player.EventLyricIdle, nil)
				lastPhase = proc.PhaseStandby
			}
			lastTitle = ""
			time.Sleep(1 * time.Second)
			continue

		case proc.PhaseLoading:
			if state.SongTitle != lastLoadingTitle {
				log.Info("歌曲加载中: %s", state.SongTitle)
				lastLoadingTitle = state.SongTitle
				// NOTE: 不向 router 发送 loading 事件。
				// wesing 的 loading 可能持续很久（超过 prior-player-expire），
				// 期间无音频/歌词输出，若触发优先组抢占会导致普通组被中断并出现空白窗口，
				// loading 超时回退后又会再次切换，观感不佳。
				// 改为仅在 PhasePlaying 时才通知 router，实现一次性精准切换。
				//
				// p.Emit(player.EventStatusUpdate, &player.StatusInfo{
				// 	Status: "loading",
				// 	Detail: fmt.Sprintf("加载中: %s", state.SongTitle),
				// })
				lastPhase = proc.PhaseLoading
			}
			time.Sleep(500 * time.Millisecond)
			continue

		case proc.PhasePlaying:
			if state.SongTitle != lastTitle {
				log.Info("♪ 歌曲: %s", state.SongTitle)
				lastTitle = state.SongTitle
			}
		}

		// === 播放中：初始化歌词并开始轮询 ===
		lyrics, timeAddr, ok := p.initSong(handle, pid, modules, cachedTimeAddr)
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}
		cachedTimeAddr = timeAddr

		// Broadcast lyrics
		offsetSec := float32(p.offsetMs) / 1000.0
		lyricItems := make([]player.LyricLine, len(lyrics))
		for i, l := range lyrics {
			lyricItems[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, "", player.LyricTextDetailed{}, offsetSec)
		}

		// 歌曲总时长
		var songDuration float32
		if d, err := lyric.FindSongDuration(handle); err == nil {
			songDuration = d
		} else if len(lyrics) > 0 {
			songDuration = lyrics[len(lyrics)-1].Time + 10
		}

		// 歌曲信息（封面 base64 异步获取，避免阻塞管线）
		songTitle, songName, singer, coverURL, songMID := p.getSongMeta(handle, pid, lastTitle)
		// 读失败或值不合法（NaN/越界）时退回 0，绝不让未校验的值进 Emit——
		// 同 pollLyrics 热路径的理由：NaN 会让 WS 的 json.Marshal 报错并僵死订阅者。
		initialPlayTime, err := lyric.ReadPlayTime(handle, timeAddr)
		if err != nil || !lyric.IsPlausiblePlayTime(initialPlayTime) {
			initialPlayTime = 0
		}

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: songTitle})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: songName, Singer: singer, Title: songTitle,
			Cover: coverURL,
		})
		initialProgress := player.ClampProgress(initialPlayTime, songDuration)
		p.Emit(player.EventAllLyrics, &player.AllLyricsData{
			Title: songTitle, Duration: songDuration,
			PlayTime: initialPlayTime, Progress: initialProgress,
			Lyrics: lyricItems, Count: len(lyricItems),
		})

		// 异步获取封面 base64（带重试），完成后补发 song_info_update。
		// 带代次守卫：这里的窗口最长约 10s（5×1s 内存重试 + 5s 下载），是三家里最宽的；
		// 无守卫时迟到的回写会把这首歌的标题+封面盖在下一首上，且整首歌不自愈。
		coverGen := p.NewSongGen()
		go func(gen uint64, handle syscall.Handle, mid, name, singer, title, url string) {
			coverURL := url
			// 如果初始未获取到封面 URL，重试从内存中搜索（K歌客户端可能延迟加载封面数据）
			if coverURL == "" && mid != "" {
				for i := 0; i < 5; i++ {
					time.Sleep(1 * time.Second)
					if found := lyric.FindCoverURL(handle, mid); found != "" {
						coverURL = strings.Replace(found, "/mid_album_500/", "/mid_album_800/", 1)
						log.Info("封面 URL 重试第 %d 次获取成功", i+1)
						break
					}
				}
			}
			if coverURL == "" {
				log.Warn("封面 URL 获取失败，跳过 base64 编码")
				return
			}
			if b64 := player.FetchCoverBase64("WeSing", coverURL, 5*time.Second); b64 != "" {
				p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
					Name: name, Singer: singer, Title: title,
					Cover: coverURL, CoverBase64: b64,
				})
			} else {
				log.Warn("封面下载失败: %s", coverURL)
			}
		}(coverGen, handle, songMID, songName, singer, songTitle, coverURL)

		lastPhase = proc.PhasePlaying

		// 歌词轮询循环
		exitR := p.pollLyrics(handle, pid, lyrics, timeAddr, offsetSec, pollInterval, lastTitle, songTitle, songDuration)
		p.Emit(player.EventLyricIdle, nil)

		switch exitR {
		case exitProcessDied:
			return
		case exitSongChanged:
			log.Info("检测到切歌，重新加载...")
			lastTitle = ""
			continue
		case exitWindowClosed:
			log.Info("K歌窗口已关闭")
			lastTitle = ""
			continue
		}
	}
}

func (p *WesingPlayer) initSong(handle syscall.Handle, pid uint32, modules []proc.Module, cachedTimeAddr uint32) ([]lyric.LyricLine, uint32, bool) {
	_, subStructAddr, err := lyric.FindLyricHost(handle, modules)
	if err != nil {
		return nil, 0, false
	}

	lyrics, err := lyric.LoadLyrics(handle, subStructAddr)
	if err != nil || len(lyrics) == 0 {
		return nil, 0, false
	}
	log.Info("歌词加载完成: %d 行；逐字：否", len(lyrics))

	if cachedTimeAddr != 0 {
		// 与 validateTimeAddr 共用同一个判定，别再各写一份——这两处曾经是一对拷贝，
		// 这里写对了（接受式），timer.go 里写成了德摩根拒绝式，导致 NaN 通过校验。
		if t, err := lyric.ReadPlayTime(handle, cachedTimeAddr); err == nil && lyric.IsPlausiblePlayTime(t) {
			return lyrics, cachedTimeAddr, true
		}
	}

	var timeAddr uint32
	for retry := 0; retry < 10; retry++ {
		timeAddr, err = lyric.FindPlayTimeAddr(handle)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
		if !p.isProcessAlive() {
			return nil, 0, false
		}
	}
	if err != nil {
		return nil, 0, false
	}

	return lyrics, timeAddr, true
}

func (p *WesingPlayer) getSongMeta(handle syscall.Handle, pid uint32, windowTitle string) (songTitle, songName, singer, coverURL, songMID string) {
	songInfo, err := lyric.FindSongInfo(handle, windowTitle)

	if err == nil {
		songMID = songInfo.Mid
	}
	if songMID != "" {
		coverURL = lyric.FindCoverURL(handle, songMID)
	}
	coverURL = strings.Replace(coverURL, "/mid_album_500/", "/mid_album_800/", 1)

	if err == nil {
		if songInfo.Singer != "" {
			title := fmt.Sprintf("%s - %s", songInfo.Name, songInfo.Singer)
			return title, songInfo.Name, songInfo.Singer, coverURL, songMID
		}
		return songInfo.Name, songInfo.Name, "", coverURL, songMID
	}
	title := proc.GetSongTitle(pid)
	return title, title, "", coverURL, songMID
}

func (p *WesingPlayer) pollLyrics(handle syscall.Handle, pid uint32, lyrics []lyric.LyricLine, timeAddr uint32,
	offsetSec float32, pollInterval time.Duration, currentTitle string, fullSongTitle string, songDuration float32) exitReason {

	lastLineIdx := -1
	failCount := 0
	pollMs := int(pollInterval.Milliseconds())
	if pollMs < 1 {
		pollMs = 30
	}

	windowCheckInterval := int(1000 / pollMs)
	if windowCheckInterval < 1 {
		windowCheckInterval = 1
	}
	pollCount := 0

	var lastPlayTime float32 = -1
	paused := false
	isMinimized := false
	isMoving := false
	var frozenSince time.Time
	frozen := false
	const pauseDuration = 1 * time.Second

	var minimizedAt time.Time
	var playTimeAtMinimize float32
	wasMinimized := false

	for {
		select {
		case <-p.StopCh:
			return exitProcessDied
		default:
		}

		pollCount++

		if pollCount%windowCheckInterval == 0 {
			if !p.isProcessAlive() {
				return exitProcessDied
			}
			state := proc.GetPlayState(pid)
			isMinimized = state.IsMinimized
			isMoving = state.IsMoving

			switch state.Phase {
			case proc.PhaseStandby:
				return exitWindowClosed
			case proc.PhaseLoading:
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					return exitSongChanged
				}
			case proc.PhasePlaying:
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					return exitSongChanged
				}
			}
		}

		playTime, err := lyric.ReadPlayTime(handle, timeAddr)
		// 值校验与读失败同等对待，别只判 err：地址是靠 AOB 选出来的，选中之后内存
		// 内容仍可能变成 NaN/越界（结构体重初始化、切歌、堆复用）。未校验的 NaN 会
		// 直接进 PlayTime 与 Progress，而 encoding/json 编码不了 NaN → WS 的
		// WriteJSON 报错 → 写 goroutine 退出但不关连接 → 订阅者僵死却仍在册。
		// validateTimeAddr 只在「发现地址」那一刻校验过一次，管不到这条 30ms 的热路径。
		if err != nil || !lyric.IsPlausiblePlayTime(playTime) {
			failCount++
			if failCount > int(3000/pollMs) {
				return exitSongChanged
			}
			time.Sleep(pollInterval)
			continue
		}
		failCount = 0

		// Replay / seek-back 检测（在暂停检测之前，避免 unpause + replay 同 tick 双重 resume）
		if lastPlayTime > 0 && playTime < lastPlayTime-2.0 {
			log.Info("检测到回跳: %.2fs → %.2fs", lastPlayTime, playTime)
			lastLineIdx = -1
			if paused {
				paused = false
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: fullSongTitle})
			}
			p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: playTime})
			frozen = false
		}

		// Minimized interpolation
		if isMinimized {
			if paused {
				wasMinimized = true
				frozen = false
			} else {
				if !wasMinimized {
					minimizedAt = time.Now()
					playTimeAtMinimize = playTime
					wasMinimized = true
				}
				elapsed := float32(time.Since(minimizedAt).Seconds())
				playTime = playTimeAtMinimize + elapsed
				if songDuration > 0 && playTime > songDuration {
					playTime = songDuration
				}
				frozen = false
			}
		} else {
			if wasMinimized {
				wasMinimized = false
			}
			if isMoving {
				frozen = false
			} else if lastPlayTime >= 0 && playTime == lastPlayTime {
				if !frozen {
					frozenSince = time.Now()
					frozen = true
				}
				if frozen && time.Since(frozenSince) >= pauseDuration && !paused {
					paused = true
					p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: fullSongTitle})
					p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{PlayTime: playTime})
				}
			} else {
				frozen = false
				if paused {
					paused = false
					p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: fullSongTitle})
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{PlayTime: playTime})
				}
			}
		}

		lastPlayTime = playTime

		// Match lyric line
		adjustedTime := playTime + offsetSec
		currentIdx := lyric.FindCurrentLine(lyrics, adjustedTime)
		if currentIdx != lastLineIdx && currentIdx >= 0 {
			lastLineIdx = currentIdx
			l := lyrics[currentIdx]
			// playTime 是内存直读的实时位置，只喂 Progress；play_time 由 BuildLyricUpdate
			// 按歌词时间轴算。wesing 无逐字，故 detailed 传零值。
			p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
				l.Index, l.Time, l.Text, "", player.LyricTextDetailed{},
				offsetSec, playTime, songDuration,
			))
		}

		time.Sleep(pollInterval)
	}
}

func (p *WesingPlayer) isProcessAlive() bool {
	_, err := proc.FindProcess("WeSing.exe")
	return err == nil
}
