package wesing

import (
	"fmt"
	"strings"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
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

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("K歌客户端未启动"), Reason: player.ReasonProcessNotRunning})
		p.Emit(player.EventClearSongData, nil)

		handle, pid := p.waitForProcess()
		p.runSession(handle, pid, offsetSec, pollInterval)
		proc.CloseProc(handle)

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("K歌客户端已退出"), Reason: player.ReasonProcessExited})
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
	// exitDeadAddr 播放时间地址读得通、值也「合理」，但从未推进过——地址是死的。
	// 唯一的处置是让 runSession 打掉 cachedTimeAddr 重新选址，见 issue #44。
	exitDeadAddr
)

// 死地址自愈的两个常数，见 issue #44。
const (
	// deadAddrTimeout 从进入轮询算起，多久没见过 playTime > 0 就判定地址是死的。
	//
	// 取值依据：健康态下 all_lyrics 的 play_time 实测就是 1.6~1.75s（进入轮询时歌已在放），
	// 即首个 tick 即为真，这条路径永不触发。5s 是给「主播开歌就暂停在 0.0」这类合法场景
	// 留的余量——它会白扫一次（几百 ms），但每首歌至多一次，代价可控。
	deadAddrTimeout = 5 * time.Second
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

	// deadAddrRetriedFor 记录已为哪首歌重新选过址（issue #44），保证每首歌至多重扫一次。
	// 以歌名为键而不是布尔量：换歌自然重置，而「同一首歌反复触发」被挡住——AOB 是全内存
	// 扫描（几百 ms），无节制重扫会把 CPU 烧掉，比它要修的问题更糟。
	deadAddrRetriedFor := ""

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
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song", Detail: i18n.T("K歌窗口未打开"), Reason: player.ReasonWindowNotOpen})
				p.Emit(player.EventClearSongData, nil)
				// lyric_idle 是**对外**事件，载荷必须是 {} 而非 null（「空数据一律 {}」，
				// openapi.yaml:10）。传 nil 会序列化成 null——实测就是这么破的。
				// 紧邻的 clear_song_data 传 nil 无妨：它是内部事件，被 server 的
				// internalEvents 拦住，根本不会序列化。
				p.Emit(player.EventLyricIdle, struct{}{})
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
		// 逐字 words 的 play_time 按 offset 统一套一次（在存进 lyrics、供本处与 pollLyrics 共用之前）。
		// BuildLyricLine 只调行级 play_time、Detailed 原样透传（对照 cloudmusic 的 applyTextDetailedOffset），
		// 不套这一步逐字高亮会整体偏一个 offset，而行级是对的。
		applyDetailedOffset(lyrics, offsetSec)
		lyricItems := make([]player.LyricLine, len(lyrics))
		for i, l := range lyrics {
			lyricItems[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, "", "", l.Detailed, offsetSec)
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
			Position: initialPlayTime, Progress: initialProgress,
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
		exitR := p.pollLyrics(handle, pid, lyrics, timeAddr, offsetSec, pollInterval, lastTitle, songTitle, songDuration,
			deadAddrRetriedFor != lastTitle)
		p.Emit(player.EventLyricIdle, struct{}{}) // 对外事件：{} 而非 null，理由同 :146

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
		case exitDeadAddr:
			// 打掉缓存，逼 initSong 走 FindPlayTimeAddr 重扫（issue #44）。
			//
			// **不清 lastTitle**：这不是切歌，是同一首歌重来。清了会让上面的
			// `state.SongTitle != lastTitle` 重新成立，deadAddrRetriedFor 的记忆随之作废，
			// 于是每 5 秒重扫一次、永不停止。
			deadAddrRetriedFor = lastTitle
			cachedTimeAddr = 0
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
	log.Info("歌词加载完成: %d 行；逐字：%s", len(lyrics), detailedFlag(lyrics))

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
	offsetSec float32, pollInterval time.Duration, currentTitle string, fullSongTitle string, songDuration float32,
	allowDeadAddrExit bool) exitReason {

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

	// 死地址检测（issue #44）：sawProgress 只由**内存直读的原始值**置位，
	// 绝不能挪到最小化插值之后——那里 playTime 被改写成 playTimeAtMinimize + elapsed，
	// 死地址（恒 0）一旦赶上最小化就会算出 0 + elapsed > 0，把自己伪装成活的。
	sawProgress := false
	pollStart := time.Now()

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

		// === 死地址检测（issue #44）===
		//
		// 上面那道闸拦不住「读得通、值也合理、但永远是同一个 0」。0 是完全合法的播放时间
		// （选址就发生在转 playing 的第一个 tick，此刻歌在前奏，真地址内容必然是 0），
		// 所以不能在校验里排除它——排除 0 会在最常见的时机拒掉真地址。
		//
		// 能区分「开头的 0」与「死地址的 0」的信号只有一个：**时间有没有推进过**。
		// 真地址至多在开头逗留一瞬；死地址永远停在 0。
		//
		// 必须放在最小化插值（下方）之前：那里会把 playTime 改写成 playTimeAtMinimize + elapsed。
		if playTime > 0 {
			sawProgress = true
		}
		if allowDeadAddrExit && !sawProgress && time.Since(pollStart) > deadAddrTimeout {
			log.Warn("播放时间 %v 内从未推进（恒为 %.2fs），疑似地址失效，重新定位", deadAddrTimeout, playTime)
			return exitDeadAddr
		}

		// Replay / seek-back 检测（在暂停检测之前，避免 unpause + replay 同 tick 双重 resume）
		if lastPlayTime > 0 && playTime < lastPlayTime-2.0 {
			log.Info("检测到回跳: %.2fs → %.2fs", lastPlayTime, playTime)
			lastLineIdx = -1
			if paused {
				paused = false
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: fullSongTitle})
			}
			p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: playTime, Progress: player.ClampProgress(playTime, songDuration)})
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
					p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: playTime, Progress: player.ClampProgress(playTime, songDuration)})
				}
			} else {
				frozen = false
				if paused {
					paused = false
					p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: fullSongTitle})
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: playTime, Progress: player.ClampProgress(playTime, songDuration)})
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
			// playTime 是内存直读的实时位置，进 Position、并据它算 Progress；play_time 由 BuildLyricUpdate
			// 按歌词时间轴算。KRC 源无关——wesing 逐字直接来自 CharElement 内存（l.Detailed），
			// words 的 play_time 已在 applyDetailedOffset 里套过 offset，这里透传；无逐字的行为零值 {}。
			p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
				l.Index, l.Time, l.Text, "", "", l.Detailed,
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

// applyDetailedOffset 把 offset 套到每行逐字的 play_time（行级 play_time 由 BuildLyric* 自己算）。
// **必须在存进 lyrics、供 all_lyrics 与 pollLyrics 共用之前只跑一次**：LoadLyrics 只填了 words 的
// Timestamp（内存原值）、留空 PlayTime，这里补上。对照 cloudmusic 的 applyTextDetailedOffset、
// qqmusic/kugou 的同名函数。无逐字的行 Words 为空、天然跳过。
func applyDetailedOffset(lyrics []lyric.LyricLine, offsetSec float32) {
	for i := range lyrics {
		d := &lyrics[i].Detailed
		if len(d.Words) == 0 {
			continue
		}
		d.PlayTime = player.AdjustLyricPlayTime(d.Timestamp, offsetSec)
		for j := range d.Words {
			d.Words[j].PlayTime = player.AdjustLyricPlayTime(d.Words[j].Timestamp, offsetSec)
		}
	}
}

// detailedFlag 报告这批歌词**实际**有没有逐字，绝不写死（AGENTS.md §6.1）。wesing 的逐字来自
// CharElement 的字级时间；时间轴不合法的行退回行级、Detailed 为空，此时如实报「否」。
func detailedFlag(lyrics []lyric.LyricLine) string {
	for _, l := range lyrics {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("是")
		}
	}
	return i18n.T("否")
}
