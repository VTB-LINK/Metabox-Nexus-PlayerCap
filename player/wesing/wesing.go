package wesing

import (
	"fmt"
	"math"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

const (
	PlayerName        = "wesing"
	TargetProcessName = "WeSing.exe"
)

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

		p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Detail: i18n.T("WeSing client not running"), Reason: player.ReasonProcessNotRunning})
		p.Emit(player.EventClearSongData, nil)

		handle, pid := p.waitForProcess()
		p.runSession(handle, pid, offsetSec, pollInterval)
		proc.CloseProc(handle)

		p.InvalidateSongGen() // 作废在飞的封面回写，别让它在 ClearSongData 之后复活已清空的 SongInfo（issue #118）
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("WeSing client has exited"), Reason: player.ReasonProcessExited})
		p.Emit(player.EventClearSongData, nil)
		log.Info("Session ended, waiting for a new WeSing process...")
		time.Sleep(2 * time.Second)
	}
}

func (p *WesingPlayer) waitForProcess() (syscall.Handle, uint32) {
	log.Info("Waiting for WeSing.exe to start...")
	printed := false
	for {
		select {
		case <-p.StopCh:
			return 0, 0
		default:
		}
		pid, err := proc.FindProcess(TargetProcessName)
		if err == nil {
			handle, err := proc.OpenProc(pid)
			if err == nil {
				log.Info("Found WeSing.exe (PID: %d)", pid)
				return handle, pid
			}
		}
		if !printed {
			log.Info("WeSing.exe not running, waiting...")
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
	// exitAltAddr 当前的播放时间地址停着，而选址时落选的另一个候选在走：改用它（见 altWatch）。
	exitAltAddr
	// exitPCMRate 伴奏解码器的推进速率不在 [0.95, 1.05]（或速率核对到期未能测出）：采样格式不是 44.1 kHz、
	// 16 bit、双声道，换算比例不成立。只这一首改走兜底，下一首仍先试 PCM（#145、#196）。
	exitPCMRate
	// exitPCMStalled 伴奏解码器进入轮询后 5 s 内一次都没推进：可能是停住的残留对象。只这一首改走兜底，
	// 下一首仍先试 PCM（#145）。
	exitPCMStalled
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

// 这首歌的音译轨登记了却还没有行时最多等多久（见 waitRoma）：romaWaitTries × romaWaitStep。
const (
	romaWaitTries = 5
	romaWaitStep  = 200 * time.Millisecond
)

func (p *WesingPlayer) runSession(handle syscall.Handle, pid uint32, offsetSec float32, pollInterval time.Duration) {
	modules, err := proc.EnumModules(pid)
	if err != nil {
		log.Error("Failed to enumerate modules: %v", err)
		return
	}

	lastTitle := ""
	var cachedTimeAddr uint32
	// cachedAltAddrs 是与 cachedTimeAddr 同一次定位时落选的候选，随它一起沿用到之后的歌。
	//
	// 沿用地址而不带候选的话，选错一次就整场换不回来：定位那一刻歌暂停着，凭读数猜中了一个不走的
	// 结构；恢复播放之前就切了歌，换歌不清缓存地址，下一首 initSong 走缓存路径——候选若是空的，
	// altWatch 无物可盯；那个读数又大于 0，#44 的「从未推进」也不触发。此后每一首的位置与歌词行都
	// 停在那个读数上，约 1 秒后报暂停。
	var cachedAltAddrs []uint32
	// altSwitchedFor 记录已为哪首歌换过播放时间地址：这一首不再盯候选，一首歌至多换一次、不来回换
	// （每换一次都要把这首歌重新宣告一遍）。换歌时清掉。
	altSwitchedFor := ""
	var lastPhase proc.PlayPhase
	lastLoadingTitle := ""

	// deadAddrRetriedFor 记录已为哪首歌重新选过址（issue #44），保证每首歌至多重扫一次。
	// 以歌名为键而不是布尔量：换歌自然重置，而「同一首歌反复触发」被挡住——AOB 是全内存
	// 扫描（几百 ms），无节制重扫会把 CPU 烧掉，比它要修的问题更糟。
	deadAddrRetriedFor := ""

	// CKSongsGraph 的主虚表（#139）：虚表来自模块映像，会话内不变，只在第一首宣告之后定位一次；
	// 失败也只失败一次（graphVtable 留 0），本会话总时长照旧只用宣告时的来源。
	var graphVtable uint32
	graphTried := false

	// 伴奏解码器 CAudioDecodeForPCM 的主虚表（#145）：定位成功后会话内不再重复，失败留 0、下一首再试
	// （见 selectPCM）。skip 记录这一次播放的 PCM 判为不符或停滞过（exitPCMRate、exitPCMStalled，见 pcmSkip）：
	// 只这一首改走兜底，下一首仍先试 PCM（#196）。
	var pcmVtable uint32
	var skip pcmSkip
	notes := pcmNotes{}
	// lyric_idle 一律经它发出：上一次宣告之后至多一条（#180）。
	var idle idleNotice

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
				// 先作废本首的代次再清空（#146 代码审）：封面 goroutine 可能仍在补读或下载，它迟到的回写经
				// EmitForGen 仍有效，会把已清空的歌曲信息贴回服务端缓存。只关 K 歌窗口、进程未退时，Start 里
				// 的 InvalidateSongGen 不会执行（同 issue #118）。✅ songgenorder_test.go（AST）
				p.InvalidateSongGen()
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song", Detail: i18n.T("WeSing window not open"), Reason: player.ReasonWindowNotOpen})
				// clear_song_data 传 nil 无妨：它是内部事件，被 server 的 internalEvents 拦住，不会序列化。
				p.Emit(player.EventClearSongData, nil)
				// 轮询以 exitWindowClosed 等原因返回时已发过一条的，这里不再发（#180）。
				idle.emit(p.TryEmit)
				lastPhase = proc.PhaseStandby
			}
			lastTitle = ""
			skip.reset()
			time.Sleep(1 * time.Second)
			continue

		case proc.PhaseLoading:
			if state.SongTitle != lastLoadingTitle {
				log.Info("Loading song: %s", state.SongTitle)
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
				log.Info("♪ Song: %s", state.SongTitle)
				lastTitle = state.SongTitle
			}
			skip.seen(state.SongTitle)
		}

		// === 播放中：初始化歌词并开始轮询 ===
		lyrics, ok := p.initSong(handle, modules)
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}

		// Broadcast lyrics
		offsetSec := float32(p.offsetMs) / 1000.0
		// 逐字 words 的 play_time 按 offset 统一套一次（在存进 lyrics、供本处与 pollLyrics 共用之前）。
		// BuildLyricLine 只调行级 play_time、Detailed 原样透传（对照 cloudmusic 的 applyTextDetailedOffset），
		// 不套这一步逐字高亮会整体偏一个 offset，而行级是对的。
		applyDetailedOffset(lyrics, offsetSec)
		lyricItems := toLyricItems(lyrics, offsetSec)

		// 时钟首选伴奏解码器（#145，设计 §4.4）：选中之后读歌曲信息、以 mid 交叉核对，总时长取自核对那次的
		// 读数，初始位置取自宣告前重读的（#191，见 resolvePCM）。选不中、或 mid 对不上，本首走兜底。核对通过之后，
		// 歌曲信息与封面按路径 mid 取（#146，见 resolvePCM）；songConfirmed 为假即歌曲信息未确认，交给补读（startCoverFetch）。
		var songTitle, songName, singer, coverURL, songMID string
		songConfirmed := true
		var songDuration, initialPlayTime float32
		var clock *lyric.PCMClock
		if !skip.on {
			inst, err := selectPCM(handle, pid, &pcmVtable)
			if err == nil {
				var s pcmSong
				if clock, s, err = resolvePCM(lastTitle, realPCMQueries(handle, pcmVtable, inst)); err == nil {
					songTitle, songName, singer, coverURL, songMID, songConfirmed = s.title, s.name, s.singer, s.cover, s.mid, s.confirmed
					initialPlayTime = s.pos
				}
			}
			if err != nil {
				notes.unavailable(err.Error())
			}
		}

		var timeAddr uint32
		var altAddrs []uint32
		durSrc := lyric.DurationEstimated
		if clock != nil {
			// 总时长直接用解码器的，不经界面字符串与 CKSongsGraph：路径里的 mid 已确认它属于这首歌（设计 §4.5）。
			// 总时长与初始位置都由整数换算，不会是 NaN。
			songDuration = clock.Total()
			log.Detail("Song clock: PCM decoder 0x%08X (duration %.2fs)", clock.Addr(), songDuration)
		} else {
			// 兜底，次序与 #145 之前相同：定位播放时间 → 界面时长字符串 → 歌曲信息（getSongMeta，封面
			// 按专辑 mid，#146）。mid 对不上转来的也在这里重读：PCM 模式宣告前只按窗口歌名查了记录，没有封面。
			timeAddr, altAddrs, ok = p.locatePlayTime(handle, cachedTimeAddr, cachedAltAddrs)
			if !ok {
				time.Sleep(1 * time.Second)
				continue
			}
			cachedTimeAddr, cachedAltAddrs = timeAddr, altAddrs
			if altSwitchedFor == lastTitle {
				altAddrs = nil
			}

			// 歌曲总时长。来源记下来：播放中 CKSongsGraph 同步之后按来源判断它是否不对（#139）。
			if d, err := lyric.FindSongDuration(handle); err == nil {
				songDuration = d
				durSrc = lyric.DurationFromUI
			} else if len(lyrics) > 0 {
				songDuration = lyrics[len(lyrics)-1].Time + 10
			}

			// 歌曲信息（封面 base64 异步获取，避免阻塞管线）
			songTitle, songName, singer, coverURL, songMID = p.getSongMeta(handle, lastTitle)
			// 读失败或值不合法（NaN/越界）时退回 0，绝不让未校验的值进 Emit——
			// 同 pollLyrics 热路径的理由：NaN 会让 WS 的 json.Marshal 报错并僵死订阅者。
			if t, err := lyric.ReadPlayTime(handle, timeAddr); err == nil && lyric.IsPlausiblePlayTime(t) {
				initialPlayTime = t
			}
			log.Detail("Song clock: play-time address 0x%08X", timeAddr)
		}

		// 本首的代次在第一条宣告事件之前建立（#146，设计 §3.6）：此前先宣告、后调 NewSongGen，两者之间上一首
		// 迟到的封面回写仍被 EmitForGen 视为有效，服务端收到后整体替换歌曲信息，这首又可能不再回写，于是一直
		// 是上一首的。封面 goroutine 用同一个代次。✅ songgenorder_test.go（AST）
		coverGen := p.NewSongGen()
		idle.announced()
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: songTitle})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: songName, Singer: singer, Title: songTitle,
			Cover: coverURL,
		})
		// all_lyrics 确认入队：没入队就在 pollLyrics 里每拍再试（见 allLyricsAnnounce）。
		ann := &allLyricsAnnounce{title: songTitle, items: lyricItems}
		ann.send(p.TryEmit, initialPlayTime, songDuration)

		// 异步获取封面 base64（带重试），完成后补发 song_info_update；宣告时没有 mid 的，同一个重试循环里
		// 先补读歌曲信息（#145）；PCM 模式下未确认的歌曲信息与缺的封面都按路径 mid 补读（#146），见 startCoverFetch。
		// 带代次守卫：这里的窗口最长约 10s（5×1s 内存重试 + 5s 下载），是 cloudmusic / qqmusic / wesing 三家里最宽的；
		// 无守卫时迟到的回写会把这首歌的标题+封面盖在下一首上，且整首歌不自愈。
		// 内存重试用的句柄由封面 goroutine 自己另开、自己关，见 startCoverFetch（issue #117）。
		p.startCoverFetch(realCoverFetchOps, coverGen, pid, lastTitle, songMID, songName, singer, songTitle, coverURL, clock != nil, !songConfirmed)

		lastPhase = proc.PhasePlaying

		// CKSongsGraph 的实例每首重扫（0.1~0.3 s），放在宣告之后，宣告不推迟。不跨歌沿用：已释放对象的
		// 首 dword 可能仍是虚表，沿用会让它一直占着位置。exitAltAddr / exitDeadAddr 回到这里重新宣告时
		// 同样重扫。只在兜底模式：PCM 模式的总时长已取自解码器（设计 §4.3）。
		var graph *lyric.SongsGraphWatch
		if clock == nil {
			if !graphTried {
				graphTried = true
				graphVtable = locateSongsGraph(handle, pid)
			}
			if graphVtable != 0 {
				graph = lyric.NewSongsGraphWatch(handle, graphVtable, lyric.FindSongsGraphInstances(handle, graphVtable),
					lyrics[len(lyrics)-1].Time, durSrc)
			}
		}

		// 歌词轮询循环
		exitR, altAddr := p.pollLyrics(handle, pid, lyrics, lyricItems, timeAddr, altAddrs, offsetSec, pollInterval, lastTitle, songTitle, songDuration,
			graph, deadAddrRetriedFor != lastTitle, clock, ann)
		idle.emit(p.TryEmit)

		switch exitR {
		case exitProcessDied:
			return
		case exitSongChanged:
			log.Info("Song change detected, reloading...")
			lastTitle = ""
			altSwitchedFor = ""
			continue
		case exitWindowClosed:
			log.Info("WeSing window closed")
			lastTitle = ""
			altSwitchedFor = ""
			skip.reset()
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
		case exitAltAddr:
			// 同一首歌换一个播放时间地址重来：不清 lastTitle（理由同上），把它当缓存地址交给 initSong，
			// 不必重扫。被换下的那个进候选名单、带到之后的歌里；这一首不再盯（altSwitchedFor）。
			cachedAltAddrs = swapAlt(cachedAltAddrs, altAddr, cachedTimeAddr)
			cachedTimeAddr = altAddr
			altSwitchedFor = lastTitle
			continue
		case exitPCMRate:
			// 只这一首改走兜底重来，下一首仍先试 PCM（#196，设计 doc/design/wesing-small-seek-design.md §3.6）。
			// 不清 lastTitle，理由同 exitDeadAddr。到期判出的没有测出速率，不报 Rate（#177 设计 §4.9）。每次判出
			// 一条 Warn，不经 pcmNotes：这是这一首的结论，测出的那条还带这一首的速率。
			if clock.RateTimedOut() {
				log.Warn("PCM decoder rate was not measured: out-of-range advances (seeks or a wrong sample format) kept voiding the measurement for 30s; using the existing play-time source for this song")
			} else {
				log.Warn("PCM decoder rate %.3f is outside [0.95, 1.05]; using the existing play-time source for this song", clock.Rate())
			}
			skip.mark(lastTitle)
			continue
		case exitPCMStalled:
			// 只这一首改走兜底重来，下一首仍先试 PCM。不清 lastTitle，理由同 exitDeadAddr。
			notes.unavailable(i18n.T("the accompaniment decoder did not advance within 5s"))
			skip.mark(lastTitle)
			continue
		}
	}
}

// initSong 加载歌词与音译。
func (p *WesingPlayer) initSong(handle syscall.Handle, modules []proc.Module) ([]lyric.LyricLine, bool) {
	_, subStructAddr, err := lyric.FindLyricHost(handle, modules)
	if err != nil {
		return nil, false
	}

	lyrics, err := lyric.LoadLyrics(handle, subStructAddr)
	if err != nil || len(lyrics) == 0 {
		return nil, false
	}
	log.Info("Lyrics loaded: %d lines; per-character: %s", len(lyrics), detailedFlag(lyrics))
	if n := loadRomaWithWait(handle, subStructAddr, lyrics); n > 0 {
		log.Info("Romanization attached: %d/%d lines", n, len(lyrics))
	}
	return lyrics, true
}

// locatePlayTime 定位播放时间地址（兜底模式）。altAddrs 是定位时落选的其它候选；沿用缓存地址时原样带回
// 传入的 cachedAltAddrs（与那个地址同一次定位时落选的）。
func (p *WesingPlayer) locatePlayTime(handle syscall.Handle, cachedTimeAddr uint32, cachedAltAddrs []uint32) (timeAddr uint32, altAddrs []uint32, ok bool) {
	if cachedTimeAddr != 0 {
		// 与 validateTimeAddr 共用同一个判定，别再各写一份——这两处曾经是一对拷贝，
		// 这里写对了（接受式），timer.go 里写成了德摩根拒绝式，导致 NaN 通过校验。
		if t, err := lyric.ReadPlayTime(handle, cachedTimeAddr); err == nil && lyric.IsPlausiblePlayTime(t) {
			return cachedTimeAddr, cachedAltAddrs, true
		}
	}

	var err error
	for retry := 0; retry < 10; retry++ {
		timeAddr, altAddrs, err = lyric.FindPlayTimeAddr(handle)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
		if !p.isProcessAlive() {
			return 0, nil, false
		}
	}
	if err != nil {
		return 0, nil, false
	}

	return timeAddr, altAddrs, true
}

// selectPCM 挑出这首歌的伴奏解码器（#145），本会话还没有 CAudioDecodeForPCM 的主虚表时先定位它。虚表定位
// 成功后会话内不再重复；失败时 *vtable 留 0，下一首再试：会话可能在全民K歌刚启动时就开始，那时
// KSongsAVCapture.dll 未必已加载，模块表因此当场重新枚举（同 locateSongsGraph）。
func selectPCM(handle syscall.Handle, pid uint32, vtable *uint32) (uint32, error) {
	if *vtable == 0 {
		modules, err := proc.EnumModules(pid)
		if err == nil {
			*vtable, err = lyric.FindPCMVtable(handle, modules)
		}
		if err != nil {
			return 0, err
		}
	}
	return lyric.SelectPCM(handle, *vtable)
}

// pcmNotes 记下本会话已 Warn 过的「PCM 不可用」原因（设计 §4.4「日志」）：每个原因 Warn 一次，之后降为
// Detail。这些原因多半每首都会重现（模块没加载、没有伴奏实例），每首一条 Warn 会刷屏。
type pcmNotes map[string]bool

func (n pcmNotes) unavailable(reason string) {
	if n[reason] {
		log.Detail("PCM clock unavailable (%s); using the existing play-time source for this song", reason)
		return
	}
	n[reason] = true
	log.Warn("PCM clock unavailable (%s); using the existing play-time source for this song", reason)
}

// pcmSkip 记录这一次播放不再试 PCM：速率核对判为不符（exitPCMRate，#196）或停滞过（exitPCMStalled，#145 设计
// §4.2）。on 为真时同一首回退重来期间不再试 PCM，换成别的歌即恢复先试（设计 doc/design/wesing-small-seek-design.md
// §3.6）。
//
// 按窗口标题清除，不按退出原因（#145 代码审 F6）：exitSongChanged 也用于同一首连续读失败的重来与 PCM 总字节变化，
// 在那里清除会让判为不符或停住的实例再被选中。是否生效只看 on，不与 lastTitle 比较：重来时 lastTitle 被清空，
// 窗口标题又可能短暂为空。所以很快重新点开同一首（未经待机）、同名的另一首接着播放，都按同一首处理。
// ✅ pcmskip_test.go（含 AST：两个退出原因都调 mark，选 PCM 只看 on）
type pcmSkip struct {
	on    bool
	title string // 标记时的窗口标题
}

func (s *pcmSkip) mark(title string) { s.on, s.title = true, title }

// seen 在播放阶段每次看到窗口标题时调用：换成别的歌（非空且不同）即清除。空标题不算换歌，同 pollLyrics。
func (s *pcmSkip) seen(title string) {
	if title != "" && title != s.title {
		s.on = false
	}
}

// reset 在待机、关窗时调用。
func (s *pcmSkip) reset() { s.on = false }

// idleNotice 记录上一次宣告之后是否已发过 lyric_idle（#180）：每首在轮询结束时发一条，随后进入待机时不再发。
// 此前两处各发一条，播放中关窗口时连发两条，与契约（openapi.yaml 的 lyric_idle：窗口关闭时发送一条）不符。
// 会话开始后未宣告即进入待机（加载中关窗口）仍发一条。✅ idlenotice_test.go（含 AST：runSession 只经它发出）
type idleNotice struct{ sent bool }

// announced 在本首宣告时调用。
func (n *idleNotice) announced() { n.sent = false }

// emit 在上一次宣告之后还没有一条入队时发一条 lyric_idle，经 tryEmit（BaseEmitter.TryEmit）发出，入队才记为已发
// （同 #133 / #134）：轮询结束时那条因事件通道满被丢弃的，进入待机时照常再发。它是对外事件，载荷必须是 {}
// 而非 null（「空数据一律 {}」，openapi.yaml:10）：传 nil 会序列化成 null，实测出过这个问题。
func (n *idleNotice) emit(tryEmit func(evtType string, data interface{}) bool) {
	if !n.sent {
		n.sent = tryEmit(player.EventLyricIdle, struct{}{})
	}
}

// confirmMID 是交给 ConfirmPCM 核对的 mid（设计 §4.1）：只用歌名等于窗口歌名的记录的 mid，否则返回空串、
// 跳过核对。#145 时 FindSongInfo 匹配不到窗口歌名会退回地址最高的记录，可能是上一首，用它的 mid 会把正确的伴奏
// 判为不符（真机 11-04）；#146 起它匹配不到即返回未找到（设计 §3.1），这里的核对照旧保留。songName、mid 是按
// 窗口歌名查到的记录（FindSongInfo）的歌名与 mid，windowTitle 是窗口歌名。#146 起它另用于判情形 b（见 resolvePCM）。
func confirmMID(songName, windowTitle, mid string) string {
	if windowTitle == "" || songName != windowTitle {
		return ""
	}
	return mid
}

// pcmQueries 是 PCM 模式宣告前流程用到的查询（#146，设计 §3.3、§3.4），测试里换成假的。
type pcmQueries struct {
	findInfo func(windowTitle string) (lyric.SongInfo, error)  // 按窗口歌名查记录（FindSongInfo：同名取地址最高的，兜底歌手设计审 R3）
	confirm  func(mid string) (*lyric.PCMClock, string, error) // ConfirmPCM，返回时钟与路径 mid；mid 是交叉核对用的
	byMID    func(songMID string) (lyric.SongInfo, bool)       // FindSongInfoByMID
	album    func(songMID string) (albumMID string)            // FindAlbumMID
	position func(c *lyric.PCMClock) float32                   // 宣告用的位置：重读一次，读不出退回核对那次的（#191）
}

func realPCMQueries(handle syscall.Handle, vtable, inst uint32) pcmQueries {
	return pcmQueries{
		findInfo: func(w string) (lyric.SongInfo, error) { return lyric.FindSongInfo(handle, w) },
		confirm: func(mid string) (*lyric.PCMClock, string, error) {
			c, err := lyric.ConfirmPCM(handle, vtable, inst, mid)
			if err != nil {
				return nil, "", err
			}
			return c, c.MID(), nil
		},
		byMID: func(mid string) (lyric.SongInfo, bool) { return lyric.FindSongInfoByMID(handle, mid) },
		album: func(mid string) string { return lyric.FindAlbumMID(handle, mid) },
		position: func(c *lyric.PCMClock) float32 {
			if t, st := c.Read(); st == lyric.PCMOK {
				return t
			}
			// 读不出，或总字节变了（换歌，由轮询的第一拍发现）：Position 仍是核对那次的读数。
			return c.Position()
		},
	}
}

// pcmSong 是 PCM 模式宣告用的歌曲信息（设计 §3.3）。
type pcmSong struct {
	title, name, singer, cover string
	mid                        string  // 路径 mid：补读与封面都按它查
	confirmed                  bool    // 歌名、歌手属于路径这首（情形 a、b）；为假即情形 c，交给补读
	pos                        float32 // 宣告用的初始位置（秒），查找全部做完之后读的（#191）
}

// resolvePCM 是 PCM 模式的宣告前流程（设计 §3.3）。#145 的交叉核对原样保留：按窗口歌名查记录，confirmMID 只在
// 记录歌名等于窗口歌名时给出它的 mid，ConfirmPCM 以它核对路径，不过就返回错误、本首走兜底。路径 mid 不参与证明
// 路径自己（设计审 F2）：同名的两首歌，路径可能是上一首残留的，只要窗口歌名匹配上的记录给出了不同的 mid，就照
// #145 等待或转兜底。
//
// 核对通过之后，宣告用的歌曲信息只取属于路径这首的：
//   - a. 按路径 mid 查到记录：用它；
//   - b. 查不到，但窗口歌名匹配上的记录的 mid 等于路径 mid：用那条记录；
//   - c. 都没有（记录尚未写入、窗口歌名匹配不到，或匹配上的记录没有 mid）：未确认，歌名用窗口歌名、歌手留空，
//     交给补读。#145 在这里宣告 FindSongInfo 按窗口歌名退回的记录，可能是上一首的歌名、歌手连同封面（设计审 F1）；
//     #146 起 FindSongInfo 不再退回（设计 §3.1）。
//
// 封面按路径 mid 查专辑 mid 构造（设计 §3.4），查不到就不发。路径 mid 为空（路径不合形状）
// 时两者都不查，按情形 c 宣告且没有可补读的。
//
// 宣告用的初始位置在最后重读（#191）：核对那次的读数之后还有按 mid 查歌曲信息与专辑，共 2～3 遍可写区全扫；
// 沿用那次的读数宣告，按真机录音推算落后约 0.55～0.65 s。
func resolvePCM(window string, q pcmQueries) (*lyric.PCMClock, pcmSong, error) {
	rec, _ := q.findInfo(window)
	clock, mid, err := q.confirm(confirmMID(rec.Name, window, rec.Mid))
	if err != nil {
		return nil, pcmSong{}, err
	}
	s := pcmSong{name: window, mid: mid}
	if mid == "" {
		log.Detail("The accompaniment path carries no song mid; announcing the window title without singer or cover")
	} else {
		if info, ok := q.byMID(mid); ok {
			s.name, s.singer, s.confirmed = info.Name, info.Singer, true
			log.Detail("Song info found by mid %s", mid)
		} else if confirmMID(rec.Name, window, rec.Mid) == mid {
			s.name, s.singer, s.confirmed = rec.Name, rec.Singer, true
			log.Detail("Song info for mid %s taken from the record matching the window title", mid)
		} else {
			log.Detail("Song info for mid %s not found yet; announcing the window title", mid)
		}
		if a := q.album(mid); a != "" {
			s.cover = lyric.AlbumCoverURL(a)
			log.Detail("Album mid for %s: %s", mid, a)
		} else {
			log.Detail("Album mid for %s not found yet", mid)
		}
	}
	s.title = s.name
	if s.singer != "" {
		s.title = fmt.Sprintf("%s - %s", s.name, s.singer)
	}
	s.pos = q.position(clock)
	return clock, s, nil
}

// seekBack 判回跳（重唱）：读数比上一拍的位置小 2 s 以上。判为最小化的拍不判（设计 §4.3）：兜底模式下最小化
// 时现有播放时间停住，上一拍的位置却是按墙钟外推的值，两者一比每拍都成立，每拍发 playback_resume、重发当前行
// （真机 11-04）。还原后的第一拍照旧比较，外推跑过了头时以一次回跳纠正。PCM 模式下 minimized 恒为假。
// 门槛与前跳共用（lyric.SeekJumpSec，#177）。
func seekBack(lastPlayTime, playTime float32, minimized bool) bool {
	return !minimized && lastPlayTime > 0 && playTime < lastPlayTime-lyric.SeekJumpSec
}

// altWatch 盯着定位播放时间时落选的候选：当前地址的读数停着、而某个候选在走，说明选错了。
//
// 定位时会挑正在走的那个（lyric.pickAdvancing），可那一刻歌若是暂停的，哪个都不走，只能先取第一个；
// 全民K歌开着音译（双行排版）时进程里留着一个时间恒为 0 的单行结构，排在前面的正是它（2026-10-09
// 真机：锁在它上面，歌词行一行不推、状态一直报暂停）。之后开播，在走的那个才露出来。
type altWatch struct {
	addrs  []uint32
	last   []float32 // 上一次检查时各候选的读数；NaN = 那次读不出
	mine   float32   // 上一次检查时当前地址的读数
	primed bool
}

// 两次检查（约隔 1 秒）之间候选的读数涨了这么多秒才算在走：下限滤掉抖动，上限滤掉乱跳的垃圾值。
const (
	altAdvanceMin = 0.3
	altAdvanceMax = 3.0
)

// check 约每秒调一次，mine 是当前地址此刻的读数。当前地址自上次检查没动、而某个候选在走时，返回那个候选。
//
// valid 对一个在走的候选做最后的复核（此刻是不是还能改用），不过的跳过、接着看下一个。复核必须在这里
// 逐个做，不能等 check 返回之后再做：名单里排前的那个若已被复用成别的内容、读数仍合理且在涨，每一秒
// 被选中的都是它、复核又不过，排在后面那个真在走的就永远轮不到——当前地址若还停在非 0 值上，#44 的
// 重扫也不触发，整首不走。
func (w *altWatch) check(read func(addr uint32) (float32, bool), mine float32, valid func(addr uint32) bool) (uint32, bool) {
	if len(w.addrs) == 0 {
		return 0, false
	}
	cur := make([]float32, len(w.addrs))
	for i, a := range w.addrs {
		if v, ok := read(a); ok {
			cur[i] = v
		} else {
			cur[i] = float32(math.NaN())
		}
	}
	prev, prevMine, primed := w.last, w.mine, w.primed
	w.last, w.mine, w.primed = cur, mine, true
	if !primed || mine != prevMine {
		return 0, false
	}
	for i, a := range w.addrs {
		// NaN 的比较恒为假：上次或这次读不出的候选不会被选中。
		if d := cur[i] - prev[i]; d > altAdvanceMin && d < altAdvanceMax && valid(a) {
			return a, true
		}
	}
	return 0, false
}

// swapAlt 是改用候选 next 之后的候选名单：原名单里的 next 换成刚被换下的 prev，其余照旧、次序不变。
func swapAlt(alts []uint32, next, prev uint32) []uint32 {
	out := make([]uint32, 0, len(alts))
	for _, a := range alts {
		if a == next {
			a = prev
		}
		out = append(out, a)
	}
	return out
}

// locateSongsGraph 定位 CKSongsGraph 的主虚表，不可用时记一条 Warn、返回 0。模块表在这里重新枚举，
// 不用会话开始时的那份：会话可能在全民K歌刚启动时就开始，那时 KSongsAVCapture.dll 未必已加载，
// 而定位失败在本会话内不再重试。
func locateSongsGraph(handle syscall.Handle, pid uint32) uint32 {
	modules, err := proc.EnumModules(pid)
	var vtable uint32
	if err == nil {
		vtable, err = lyric.FindSongsGraphVtable(handle, modules)
	}
	if err != nil {
		log.Warn("CKSongsGraph unavailable (%v); song duration keeps its existing source for this session", err)
		return 0
	}
	return vtable
}

// loadRomaWithWait 给歌词配上音译，返回配上的行数。
func loadRomaWithWait(handle syscall.Handle, subStructAddr uint32, lyrics []lyric.LyricLine) int {
	return waitRoma(
		func() (int, bool) { return lyric.LoadRoma(handle, subStructAddr, lyrics) },
		func() { time.Sleep(romaWaitStep) },
	)
}

// waitRoma 反复调 load，直到配上了、或者不必再等。只在 pending（这首歌的音译轨登记了、行向量却还是
// 空的）时等：歌词每首只加载这一次，那时错过整首都没有音译。这种时序没有在真机上见过，等待是防它的。
//
// 其余情形立刻返回，因为这段等待卡在宣告之前——initSong 返回之后才发 status_update、song_info_update、
// all_lyrics，wesing 又在优先组，等多久切主输出就晚多久。2026-10-09 真机，从带音译的歌切到不带音译的，
// 音译轨还留着上一首的 113 行：当时只要音译轨在、配上 0 行就等，于是白等满了 1 秒才宣告。
func waitRoma(load func() (attached int, pending bool), wait func()) int {
	for try := 0; ; try++ {
		n, pending := load()
		if n > 0 || !pending || try >= romaWaitTries {
			return n
		}
		wait()
	}
}

func (p *WesingPlayer) getSongMeta(handle syscall.Handle, windowTitle string) (songTitle, songName, singer, coverURL, songMID string) {
	return songMeta(windowTitle,
		func(w string) (lyric.SongInfo, error) { return lyric.FindSongInfoPreferMID(handle, w) },
		func(mid string) (lyric.SongInfo, bool) { return lyric.FindSongInfoByMID(handle, mid) },
		func(mid string) string { return lyric.FindAlbumMID(handle, mid) })
}

// songMeta 是兜底模式的宣告前歌曲信息（getSongMeta），查询换成函数供测试（#146，设计 §3.1、§3.2、§3.4）：
//   - 只用窗口歌名匹配上的记录（FindSongInfoPreferMID：同名的取地址最高的、有 mid 的一条），歌名、歌手、mid 取它的。
//     匹配不到（含查找出错）时歌名用窗口歌名，歌手、mid、封面留空，不查专辑，交给补读按窗口歌名重新匹配：#146
//     之前宣告 FindSongInfo 退回的那条，可能是上一首的歌名、歌手（兜底歌手设计审 R1）。歌名这里再核对一次；
//   - 记录有 mid 而没有歌手时按 mid 查，查到的歌名等于记录歌名才取它的歌手（§9 二测：这一版的 StartKSong 记录
//     可能不带歌手）；
//   - 有 mid 就按它查专辑 mid、构造封面地址，查不到不发。
func songMeta(windowTitle string, findInfo func(string) (lyric.SongInfo, error), byMID func(string) (lyric.SongInfo, bool), album func(string) string) (songTitle, songName, singer, coverURL, songMID string) {
	rec, err := findInfo(windowTitle)
	if err != nil || rec.Name != windowTitle {
		log.Detail("No song record matches the window title; announcing it without singer or cover")
		return windowTitle, windowTitle, "", "", ""
	}
	songName, singer, songMID = rec.Name, rec.Singer, rec.Mid
	if songMID == "" {
		log.Detail("The song record matching the window title has no mid; the cover is left to retries")
	} else {
		if singer == "" {
			if info, ok := byMID(songMID); ok && info.Name == songName {
				singer = info.Singer
			}
		}
		if a := album(songMID); a != "" {
			coverURL = lyric.AlbumCoverURL(a)
			log.Detail("Album mid for %s: %s", songMID, a)
		} else {
			log.Detail("Album mid for %s not found yet", songMID)
		}
	}
	songTitle = songName
	if singer != "" {
		songTitle = fmt.Sprintf("%s - %s", songName, singer)
	}
	return songTitle, songName, singer, coverURL, songMID
}

// pollLyrics 轮询播放时间、推歌词行，直到本首结束。第二个返回值只在 exitAltAddr 时有意义：该改用的地址。
// lyricItems 是宣告时发出的 all_lyrics 歌词，总时长被 graph 纠正时原样重发；graph 为 nil 时不纠正。
// ann 是宣告的 all_lyrics，没入队时每拍再试（两种模式共用）。
//
// clock 非 nil 是 PCM 模式（#145，设计 §4.3）：播放时间取自伴奏解码器并逐拍核对；不做最小化外推，拖动
// 窗口时不豁免暂停判定；不做死地址检测、不盯候选（timeAddr、altAddrs 不用，graph 为 nil）；另做速率核对
// 与停滞检测。clock 为 nil 是兜底模式，行为同 #145 之前。
func (p *WesingPlayer) pollLyrics(handle syscall.Handle, pid uint32, lyrics []lyric.LyricLine, lyricItems []player.LyricLine, timeAddr uint32, altAddrs []uint32,
	offsetSec float32, pollInterval time.Duration, currentTitle string, fullSongTitle string, songDuration float32,
	graph *lyric.SongsGraphWatch, allowDeadAddrExit bool, clock *lyric.PCMClock, ann *allLyricsAnnounce) (exitReason, uint32) {

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

	// 回跳与前跳、最小化外推、暂停与恢复的判定（#177，见 tickJudge）。
	judge := newTickJudge()
	isMinimized := false
	isMoving := false

	// 死地址检测（issue #44）：sawProgress 只由**内存直读的原始值**置位，
	// 绝不能挪到最小化插值之后——那里 playTime 被改写成 playTimeAtMinimize + elapsed，
	// 死地址（恒 0）一旦赶上最小化就会算出 0 + elapsed > 0，把自己伪装成活的。
	sawProgress := false
	pollStart := time.Now()

	alt := altWatch{addrs: altAddrs}
	readAlt := func(addr uint32) (float32, bool) {
		v, err := lyric.ReadPlayTime(handle, addr)
		return v, err == nil && lyric.IsPlausiblePlayTime(v)
	}
	stillTimeStruct := func(addr uint32) bool { return lyric.IsPlayTimeStruct(handle, addr) }

	for {
		select {
		case <-p.StopCh:
			return exitProcessDied, 0
		default:
		}

		pollCount++

		if pollCount%windowCheckInterval == 0 {
			if !p.isProcessAlive() {
				return exitProcessDied, 0
			}
			state := proc.GetPlayState(pid)
			// PCM 模式下窗口最小化、拖动时读数照常走：两者恒为假，于是不外推，读数停住满 1 s 即判暂停。
			if clock == nil {
				isMinimized = state.IsMinimized
				isMoving = state.IsMoving
			}

			switch state.Phase {
			case proc.PhaseStandby:
				return exitWindowClosed, 0
			case proc.PhaseLoading:
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					return exitSongChanged, 0
				}
			case proc.PhasePlaying:
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					return exitSongChanged, 0
				}
			}
		}

		var playTime float32
		readOK := false
		if clock != nil {
			// 逐拍核对（设计 §4.2）：总字节变了即换歌；虚表不等、解码字节超过总字节与读不出同记为读失败。
			// 位置由整数换算，不会是 NaN 或负数。
			t, st := clock.Read()
			if st == lyric.PCMSongChanged {
				return exitSongChanged, 0
			}
			playTime, readOK = t, st == lyric.PCMOK
		} else {
			t, err := lyric.ReadPlayTime(handle, timeAddr)
			// 值校验与读失败同等对待，别只判 err：地址是靠 AOB 选出来的，选中之后内存
			// 内容仍可能变成 NaN/越界（结构体重初始化、切歌、堆复用）。未校验的 NaN 会
			// 直接进 PlayTime 与 Progress，而 encoding/json 编码不了 NaN → WS 的
			// WriteJSON 报错 → 写 goroutine 退出但不关连接 → 订阅者僵死却仍在册。
			// validateTimeAddr 只在「发现地址」那一刻校验过一次，管不到这条 30ms 的热路径。
			playTime, readOK = t, err == nil && lyric.IsPlausiblePlayTime(t)
		}
		if !readOK {
			failCount++
			if failCount > int(3000/pollMs) {
				return exitSongChanged, 0
			}
			time.Sleep(pollInterval)
			continue
		}
		failCount = 0
		// 本拍时刻在读数成功之后立即取（设计 §4.1）：读数之前的卡顿全部计入前跳判定的经过时间。
		now := time.Now()

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
		if clock == nil && allowDeadAddrExit && !sawProgress && time.Since(pollStart) > deadAddrTimeout {
			log.Warn("Play time never advanced within %v (stuck at %.2fs), address likely stale, relocating", deadAddrTimeout, playTime)
			return exitDeadAddr, 0
		}

		// 落选的候选在走而这个地址停着：改用它（见 altWatch）。与上面的窗口检查同一节奏，约每秒一次；
		// 同样必须用内存直读的原始值，放在最小化插值之前。只在兜底模式。
		if clock == nil && pollCount%windowCheckInterval == 0 {
			// 候选可能是好几首歌之前扫到的：改用之前复核它此刻仍是一个播放时间结构，那块内存另作
			// 他用了就跳过它、看下一个候选。
			if addr, ok := alt.check(readAlt, playTime, stillTimeStruct); ok {
				log.Warn("Play-time address 0x%08X is stalled while another one advances; switching to 0x%08X", timeAddr, addr)
				return exitAltAddr, addr
			}

			// CKSongsGraph 与播放时间逐秒同步推进之后，以它的总时长纠正宣告时的值，每首至多一次（#139）。
			// 同样要用内存直读的原始值（设计 §4.2）：最小化外推的值是按墙钟算出来的，不是播放器自己的读数。
			//
			// **纠正了就必须重发 all_lyrics**：服务端缓存里的 duration 只由 all_lyrics 写入，只改本地变量，
			// 之后各事件的 progress 按新时长算、缓存里的 duration 却还是旧值（同 spotify 的「时长补取」）。
			// all_lyrics 会清掉服务端缓存的 lyric_update，所以把 lastLineIdx 复位，让当前行随后重发。
			// 用 TryEmit：事件通道满被丢弃时不改本地时长、不记为已纠正，下一拍再试，免得本地与服务端
			// 缓存整首不一致（同 #133 / #134）。
			if d, ok := graph.Check(playTime, songDuration); ok && p.TryEmit(player.EventAllLyrics, &player.AllLyricsData{
				Title: fullSongTitle, Duration: d,
				Position: playTime, Progress: player.ClampProgress(playTime, d),
				Lyrics: lyricItems, Count: len(lyricItems),
			}) {
				graph.Adopted(songDuration, d)
				songDuration = d
				lastLineIdx = -1
			}
		}

		// 回跳与前跳、最小化外推、暂停与恢复（#177，见 tickJudge），用内存直读的原始值；发出的次序为跳转、
		// 暂停、恢复，都排在歌词行之前，越过行首时先 playback_resume、后 lyric_update。
		out := judge.step(playTime, now, isMinimized, isMoving, songDuration)
		if out.resetLine {
			lastLineIdx = -1
		}
		p.emitTick(out, playTime, fullSongTitle, songDuration)
		playTime = out.pos

		// 速率核对与停滞检测（设计 §4.2），用本拍的暂停判定。跳转与回退由速率核对按每拍上限自行作废，不经
		// 跳转判定（#190，doc/design/wesing-small-seek-design.md §3.3）。
		if clock != nil {
			switch clock.Observe(now, judge.paused) {
			case lyric.PCMRateOff:
				return exitPCMRate, 0
			case lyric.PCMStalled:
				return exitPCMStalled, 0
			}
		}

		// 宣告的 all_lyrics 补发与歌词行匹配。排在本拍的暂停与恢复判定之后，与本拍其他事件用同一个 playTime。
		lastLineIdx = p.lyricTail(ann, lyrics, lastLineIdx, playTime, offsetSec, songDuration)

		// 位置采样（见 BaseEmitter.EmitPositionSample）：未判暂停时按固定间隔报一次，校准服务端缓存里的
		// 位置——不足 pauseDuration 的停顿与前跳都不发事件，长间奏里又没有歌词行可推。传 playTime，
		// 与上面 lyric_update、playback_pause / playback_resume 的 position 同一个量；offset 只进了
		// adjustedTime，这里不带。
		if !judge.paused {
			p.EmitPositionSample(time.Now(), playTime, songDuration)
		}

		time.Sleep(pollInterval)
	}
}

// allLyricsAnnounce 是一首歌宣告的 all_lyrics（设计 §4.4「all_lyrics 确认入队」）。宣告时经 TryEmit 发出，
// 没入队（事件通道满）就留着，pollLyrics 每拍再试，直到入队。#145 之前用 Emit，通道满时整首丢失宣告；#139
// 的纠正只在时长不对时顺带补发，PCM 模式没有这条路径。
type allLyricsAnnounce struct {
	title string
	items []player.LyricLine
	sent  bool
}

// send 在还没入队时发一次，位置与进度按 position、duration 重填，返回这一次是否入队；入队过的不再发。
//
// 每次都重填，不沿用宣告时的位置：服务端收到 all_lyrics 会无条件覆盖缓存的位置，旧位置会把其间已入队的
// 暂停位置覆盖回去（设计审 F4）。
func (a *allLyricsAnnounce) send(tryEmit func(evtType string, data interface{}) bool, position, duration float32) bool {
	if a.sent {
		return false
	}
	a.sent = tryEmit(player.EventAllLyrics, &player.AllLyricsData{
		Title: a.title, Duration: duration,
		Position: position, Progress: player.ClampProgress(position, duration),
		Lyrics: a.items, Count: len(a.items),
	})
	return a.sent
}

// lyricTail 是 pollLyrics 每拍在暂停与恢复判定之后的一段：宣告的 all_lyrics 还没入队就按本拍的位置与时长
// 再发一次，入队了就复位 lastLineIdx，让当前行随后重发（all_lyrics 会清掉服务端缓存的 lyric_update，
// 同 #139）；然后推当前歌词行。尚未到首行时没有当前行，不发。返回新的 lastLineIdx。
//
// 当前行经 TryEmit 发出，入队之后才记为已发，没入队的下一拍按那一拍的位置再发（设计 §4.4，代码审 F3）：
// 补发的 all_lyrics 正是在通道刚腾出位置时入队，很可能占掉最后一个槽位，而服务端缓存里的当前行已被它
// 清掉，先记行号的话停在这一行时一直没有。
func (p *WesingPlayer) lyricTail(ann *allLyricsAnnounce, lyrics []lyric.LyricLine, lastLineIdx int, playTime, offsetSec, songDuration float32) int {
	if ann.send(p.TryEmit, playTime, songDuration) {
		lastLineIdx = -1
	}

	// Match lyric line
	adjustedTime := player.LyricMatchTime(playTime, offsetSec)
	currentIdx := lyric.FindCurrentLine(lyrics, adjustedTime)
	// playTime 是内存直读的实时位置，进 Position、并据它算 Progress；play_time 由 BuildLyricUpdate
	// 按歌词时间轴算。KRC 源无关——wesing 逐字直接来自 CharElement 内存（LyricLine.Detailed），
	// words 的 play_time 已在 applyDetailedOffset 里套过 offset，这里透传；无逐字的行为零值 {}。
	if currentIdx != lastLineIdx && currentIdx >= 0 &&
		p.TryEmit(player.EventLyricUpdate, lineUpdate(lyrics[currentIdx], offsetSec, playTime, songDuration)) {
		lastLineIdx = currentIdx
	}
	return lastLineIdx
}

func (p *WesingPlayer) isProcessAlive() bool {
	_, err := proc.FindProcess(TargetProcessName)
	return err == nil
}

// toLyricItems 把内存读出的歌词行映射成对外的 LyricLine（all_lyrics 用）。翻译恒空——wesing 没有
// 翻译源；音译取自内存里的音译轨（RomaText），这首歌没有音译轨、或这一行没有音译时为空串。
// 两者是各自独立的字段，音译绝不能落进 sub_text。逐字音译（RomaDetailed）进 roma_text_detailed。
func toLyricItems(lyrics []lyric.LyricLine, offsetSec float32) []player.LyricLine {
	items := make([]player.LyricLine, len(lyrics))
	for i, l := range lyrics {
		items[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, "", l.RomaText, l.Detailed, offsetSec)
		items[i].RomaTextDetailed = l.RomaDetailed
	}
	return items
}

// lineUpdate 把一行歌词映射成 lyric_update 的载荷，字段口径同 toLyricItems。
func lineUpdate(l lyric.LyricLine, offsetSec, playTime, songDuration float32) *player.LyricUpdate {
	u := player.BuildLyricUpdate(l.Index, l.Time, l.Text, "", l.RomaText, l.Detailed, offsetSec, playTime, songDuration)
	u.RomaTextDetailed = l.RomaDetailed
	return u
}

// applyDetailedOffset 把 offset 套到每行逐字的 play_time（行级 play_time 由 BuildLyric* 自己算）。
// **必须在存进 lyrics、供 all_lyrics 与 pollLyrics 共用之前只跑一次**：LoadLyrics 只填了 words 的
// Timestamp（内存原值）、留空 PlayTime，这里补上。对照 cloudmusic 的 applyTextDetailedOffset、
// qqmusic/kugou 的同名函数。无逐字的行 Words 为空、天然跳过。
func applyDetailedOffset(lyrics []lyric.LyricLine, offsetSec float32) {
	for i := range lyrics {
		// 主歌词的逐字与音译的逐字各是一条时间轴，都要套。
		for _, d := range []*player.LyricTextDetailed{&lyrics[i].Detailed, &lyrics[i].RomaDetailed} {
			if len(d.Words) == 0 {
				continue
			}
			d.PlayTime = player.AdjustLyricPlayTime(d.Timestamp, offsetSec)
			for j := range d.Words {
				d.Words[j].PlayTime = player.AdjustLyricPlayTime(d.Words[j].Timestamp, offsetSec)
			}
		}
	}
}

// detailedFlag 报告这批歌词**实际**有没有逐字，绝不写死（AGENTS.md §6.1）。wesing 的逐字来自
// CharElement 的字级时间；时间轴不合法的行退回行级、Detailed 为空，此时如实报「否」。
func detailedFlag(lyrics []lyric.LyricLine) string {
	for _, l := range lyrics {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}
