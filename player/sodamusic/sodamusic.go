// Package sodamusic 接入汽水音乐（字节跳动 SodaMusic.exe，Electron + Vue3）作为第 5 个 player。
//
// 形态最接近 kugou（CDP + 明文 KRC 逐字歌词），故实现照 kugou 骨架，换歌/时钟外推照 cloudmusic：
//   - watchdog：找主进程 pid → 复刻 process._debugProcess 开 9229（绕过原生反调试，见 watchdog 包）。
//   - cdp：连 9229 → 主进程 executeJavaScript 桥进 rendererMain → transport 请求 sharedState.get('player')。
//   - 歌词：字节直接给**已解密的明文 KRC**（酷狗格式），用 player/krc 公共包解析，逐字白捡。
//   - 进度：sharedState 给每秒粗 progressSeconds，本地墙钟外推平滑（同 kugou/cloudmusic）。
//
// 稳定优先（AGENTS §0.1）：任何一步失败都降级重试，绝不 panic、绝不动汽水音乐进程状态。
package sodamusic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
	"Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
)

const PlayerName = "sodamusic"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Soda")

// SodaMusicPlayer 汽水音乐播放器（CDP 实现）。
type SodaMusicPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建汽水音乐播放器。
func New(offsetMs, pollMs int) *SodaMusicPlayer {
	return &SodaMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// coverBudget 是下载封面 base64 的时间预算。标题已先发，封面慢一点无妨。
const coverBudget = 3 * time.Second

// durationGrace 是换歌后等 durationSeconds 就绪的上限，超时就按 0 发。
// 取 2s：真机上时长比歌名晚不到一个轮询到几百毫秒，2s 有充分余量；同时短到即使某个源
// 永远给不出时长，每次换歌也只多等这一下，不会让播放器显得「卡住」。
const durationGrace = 2 * time.Second

// minPollInterval 汽水侧轮询下限（下限，不是目标值：配置写得更大就按配置走）。
//
// **依据是数据源的分辨率，不是单次调用的开销。** 真机实测（2026-08-08）：一次 Extract
// 的往返只要 1.6~8ms，并不重；但它取回的 `progressSeconds` 是 **1Hz 采样**，两次刷新之间
// 反复去问只是重复读同一个值。轮询频率在这里唯一买到的是「新采样落地后多久被发现」这一段
// 相位（≤ 一个轮询间隔），位置精度本身由本地时钟外推提供、与轮询频率无关；逐字高亮更是
// 由前端按 play_time 插值，与此无关。
//
// 200ms 是在这段相位（实测均值 96ms，见 §7.7）与「每秒 33 次往返打在主播的音乐进程上」
// 之间取的折中。要更紧的相位就调小它，但收益上限只有 200ms，别指望它能提高分辨率。
const minPollInterval = 200 * time.Millisecond

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *SodaMusicPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// Start 启动汽水音乐轮询循环（阻塞，须在 goroutine 中调用）。
func (p *SodaMusicPlayer) Start() {
	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("汽水音乐未启动或未连上"), Reason: player.ReasonProcessNotRunning})
		p.Emit(player.EventClearSongData, nil)

		pid, err := watchdog.FindMainPID()
		if err != nil {
			// 未运行——用户可能稍后启动，静默等待重试。
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}

		if err := watchdog.EnsureInspector(pid); err != nil {
			log.Warn("开启汽水音乐 inspector 失败: %v", err)
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}

		client, err := cdp.Connect()
		if err != nil {
			log.Warn("CDP 连接失败: %v", err)
			if p.sleepOrStop(2 * time.Second) {
				return
			}
			continue
		}

		log.Info("CDP 连接成功，开始监听播放状态")
		p.runSession(client)
		client.Close()

		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("汽水音乐 CDP 已断开"), Reason: player.ReasonCDPDisconnected})
		p.Emit(player.EventClearSongData, nil)
		if p.sleepOrStop(2 * time.Second) {
			return
		}
	}
}

// runSession 轮询 sharedState 播放态并 emit 事件，直到 CDP 断开。
func (p *SodaMusicPlayer) runSession(client *cdp.Client) {
	offsetSec := float32(p.offsetMs) / 1000.0
	pollInterval := time.Duration(p.pollMs) * time.Millisecond
	if pollInterval < minPollInterval {
		pollInterval = minPollInterval
	}

	var lastMediaID string
	var lastPlaying = -1 // -1 未知 / 0 暂停 / 1 播放
	// pendingID / pendingSince / pendingRawFresh：正在等就绪的那首歌、从哪一刻开始等、
	// 以及**这首歌出现之后有没有见过新采样**（见换歌分支的两道就绪闸）。
	var pendingID string
	var pendingSince time.Time
	var pendingRawFresh bool
	// throttleWarned 保证「节流没关掉」只告警一次——它每轮都会重新上报，不设闸会刷屏。
	var throttleWarned bool
	// loading 是本轮的 isLoading，供 livePos 判定「该不该继续外推」。
	var loading bool
	var currentLyrics []krc.Line
	// currentLyricItems 是本首歌**已发出的那份** all_lyrics.lyrics。留着它只为一件事：
	// 时长晚到时按同一份歌词重发一次 all_lyrics（见下面的时长补取）。
	var currentLyricItems []player.LyricLine
	var lastLineIdx = -1
	var currentDurationSec float32
	var currentTitle string

	// 每首歌一个封面 goroutine，换歌/退出时由 ctx 取消——迟到的封面回写不会污染下一首/清屏后状态。
	// 保持 coverCancel 的显式调用（别包 defer/helper），让 go vet 的 lostcancel 看得见（对照 kugou.go）。
	var coverCancel context.CancelFunc

	// 本地时钟外推：锚点进度 + 自锚点起的墙钟流逝，平滑两次轮询间的进度。
	var anchorProgressSec float32
	var anchorTime time.Time
	isPlaying := false

	// lastRawSec 是上一次见到的 sharedState 原始采样值，用来判定「这一轮是不是新采样」。
	//
	// **承重，别退回「每轮都落锚」**：汽水的 progressSeconds 是 ~1Hz 采样——真机实测
	// （2026-08-08，连采 60 次变化）Δt 970~1060ms、Δv +1.0000±0.01，而值本身是采样瞬间的
	// 真实位置（形如 45.519，毫秒精度，不是整秒量化）。也就是说两次采样之间它一直是旧值，
	// 陈旧程度 0~1s。只有在**它变化时**重新落锚，锚点之后的墙钟流逝才是真的「已经又播了多久」；
	// 每轮都落锚会把 time.Since(anchorTime) 清零 → 外推形同虚设 → position / progress / 歌词
	// 行匹配一起退化成 1s 阶梯、系统性滞后 0~1s（均值 0.5s）。
	//
	// 这是照 kugou 骨架时**不能照抄**的一处：酷狗的进度是 100ns 计时器直读
	// （kugou.go:368 `progressRaw/1e7`），每轮都是新鲜值，落不落锚没有差别。
	//
	// 真正的分量在极端情形：汽水窗口最小化久了会被 Chromium 后台节流，采样间隔从 1s 掉到
	// **60s**（实测 dv=+60.0004 / dt=59990ms）。那时每轮落锚 = 进度整整冻结一分钟再跳 60s
	// （实测「检测到前跳: 0.00s → 28.01s」，歌词在第 0 行卡了 28 秒）；边沿落锚则照常平滑推进，
	// 因为 60s 内音频时钟与墙钟只差 0.4ms。详见 AGENTS §7.7.1 / §7.7.2。
	var lastRawSec float32 = -1 // -1 = 本会话尚未见过任何采样

	// livePos 返回「此刻」的整曲实时位置（秒）：播放中 = 锚点 + 自锚点起的墙钟流逝，
	// 暂停 = 锚点原值；有时长则钳到时长。position / progress / 歌词匹配三处共用它，
	// 避免同一个量在三处各写一遍钳制而分叉。
	livePos := func() float32 {
		v := anchorProgressSec
		// 缓冲中不外推：那时 isPlaying 仍为 true 而 progressSeconds 停更，再按墙钟往前推
		// 就是纯粹的凭空前冲（歌词翻过几行而音频没动），缓冲结束还会因 drift 被判成一次假回跳。
		// **判据用平台自己给的 isLoading，不新造超时阈值**——它本来就在载荷里，此前被解析后丢弃。
		// 真机多轮采样中播放态下它恒为 false，所以这道闸不会误伤正常播放。
		if isPlaying && !loading {
			v += float32(time.Since(anchorTime).Seconds())
		}
		if currentDurationSec > 0 && v > currentDurationSec {
			v = currentDurationSec
		}
		return v
	}

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

		data, err := client.Extract()
		if err != nil {
			// 提取失败可能是瞬时（换歌间隙/无歌曲）。连接真断了才退出重连。
			if client.IsClosed() {
				if coverCancel != nil {
					coverCancel()
				}
				return
			}
			time.Sleep(pollInterval)
			continue
		}

		// 无歌曲加载（首页/未播放）：静默等待，交给路由超时判 idle。
		if data.MediaID == "" && data.Name == "" {
			time.Sleep(pollInterval)
			continue
		}

		progressSec := data.ProgressSeconds
		durationSec := data.DurationSeconds

		// 本轮是不是新采样（决定能否落锚，见 lastRawSec）。无条件记录：非法值也算「见过」，
		// 它再变成合法值时必然与此不同，不会漏掉一次落锚。
		rawChanged := progressSec != lastRawSec
		lastRawSec = progressSec
		if rawChanged {
			pendingRawFresh = true
		}
		loading = data.IsLoading

		if data.Throttled && !throttleWarned {
			throttleWarned = true
			log.Warn("汽水渲染器的后台节流没能关掉：窗口最小化久了进度源会掉到 1/60Hz，seek/暂停最长 60s 后才被发现")
		}

		// ── 换歌检测（稳定 MediaID）──
		if data.MediaID != "" && data.MediaID != lastMediaID {
			// mediaDetail（名/歌手/歌词/封面）是异步加载的：换歌瞬间 mediaId 先更新，有个窗口
			// name/歌词仍为空。此刻**别提交这次换歌**——否则空名标题与空歌词会「定格」到下次换歌
			// （现象：日志打「♪ 歌曲:  -  (id: …)」，切走再切回才好）。不更新 lastMediaID，静默跳过
			// 本轮，下轮 mediaDetail 就绪（name 非空）再正常处理。符合「未就绪则重试、不向外发半成品」。
			if data.Name == "" {
				time.Sleep(pollInterval)
				continue
			}
			if data.MediaID != pendingID {
				pendingID = data.MediaID
				pendingSince = time.Now()
				pendingRawFresh = false
			}
			// 时长与歌名**不同源**：名/歌手/歌词来自 mediaDetail，时长来自播放器自身的
			// durationSeconds，实测会晚到——真机录音里出现过「name/歌词/封面都齐了、
			// durationSeconds 仍是 0」的一轮。此时若照发，all_lyrics 就带着 duration=0 出去，
			// 而 ClampProgress 的 `duration <= 0` 闸会把 progress 钉死在 0，**且 all_lyrics
			// 每首只发一次、不自愈**：中途连入的前端整首拿到进度条归零。故再等它一会儿。
			//
			// **宽限必须有上限**，这也是它与上面「等 name」的区别：name 为空 = 没有任何可显示
			// 的东西，等多久都值；时长为 0 只是降级，而拿不到时长的源（电台/直播流）如果因此
			// 永不上报，就是把一个显示缺陷换成了整个播放器静默消失。超时就按 0 发，
			// 后续由下面的时长补取修正 progress。
			if durationSec <= 0 && time.Since(pendingSince) < durationGrace {
				time.Sleep(pollInterval)
				continue
			}
			// 先记「这是不是本会话的第一首」，再改 lastMediaID —— 顺序反了这个判据永远为假。
			firstSong := lastMediaID == ""
			lastMediaID = data.MediaID
			lastLineIdx = -1
			currentDurationSec = durationSec
			isPlaying = data.IsPlaying
			// 落锚同样要过「采样是否新鲜」这道闸（与暂停分支、进度分支同一判据）。
			//
			// **isProgressValid 在这里拦不住陈旧值**：它只挡 `progressSec > duration*1.05`，
			// 而上一首停在 200s、新歌时长 ≥191s 就整个放行——流行歌普遍 180~300s，最常见的情形
			// 恰恰漏过。progressSeconds 是 1Hz 采样，换歌瞬间 mediaId 先更新、它仍是**上一首**的
			// 收尾值；照单全收就会让 all_lyrics 带着 Position≈200 / Progress≈0.9 随新歌发出，
			// 而 all_lyrics 每首只发一次不重发 —— overlay 据此起自由时钟，从新歌的 200s 处开始
			// 高亮，约 1s 后下一次采样才触发「检测到回跳」拉回 0。换歌瞬间歌词从末段闪回开头。
			//
			// 没见过新采样就锚 0：换歌后真实位置本就是 0 附近，这是唯一安全的默认。
			//
			// **只对「从一首歌换到另一首」设这道闸**（firstSong 为假时）。会话刚建立那一次
			// 没有「上一首」——此时的 progressSeconds 就是当前这首的值，只是最多陈旧 1s，
			// 照单收下才对。一律锚 0 会让中途启动的服务先报 position=0、再被下一次采样判成
			// 一次「前跳」（真机实测：`检测到前跳: 0.00s → 61.14s`），而服务在歌曲中途启动
			// 恰恰是最常见的开播姿势。
			freshEnough := pendingRawFresh || firstSong
			if freshEnough && isProgressValid(progressSec, durationSec) {
				anchorProgressSec = progressSec
			} else {
				anchorProgressSec = 0
			}
			anchorTime = time.Now()

			name := data.Name
			singer := strings.Join(data.Artists, " / ")
			currentTitle = buildTitle(name, singer)

			log.Info("♪ 歌曲: %s - %s (id: %s)", name, singer, data.MediaID)

			// 换歌瞬间发一次真实播放态。isPlaying 是明确布尔（非酷狗那种会塌成 standby 的过渡态），
			// playing/paused 都是正常态、不会被路由判 idle，直接发。
			if isPlaying {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
			}
			lastPlaying = boolToInt(isPlaying)

			// 封面：取消上一首、起本首。URL 已随 Extract 就绪，直接传。
			if coverCancel != nil {
				coverCancel()
			}
			var ctx context.Context
			ctx, coverCancel = context.WithCancel(context.Background())
			go p.runCoverFetch(ctx, data.CoverURL, name, singer, currentTitle)

			// 歌词：明文 KRC 解析 + 翻译轨合并 + 套 offset，逐字有无由 detailedFlag 如实反映。
			currentLyrics = parseSodaLyrics(data, offsetSec)
			if len(currentLyrics) > 0 {
				log.Info("歌词加载完成: %d 行；逐字：%s", len(currentLyrics), detailedFlag(currentLyrics))
			} else {
				log.Info("纯音乐/无歌词；逐字：%s", detailedFlag(currentLyrics))
			}

			lyricItems := toLyricLines(currentLyrics, offsetSec)
			initPlay := anchorProgressSec
			progress := player.ClampProgress(initPlay, currentDurationSec)

			// 若平台只返回一句提示语（如「纯音乐，请欣赏」），归一成 index:-1。
			pureHint := ""
			if player.IsPureMusicOnly(lyricItems) {
				log.Info("平台只返回提示语「%s」，按无歌词处理", lyricItems[0].Text)
				pureHint = lyricItems[0].Text
				currentLyrics = nil
				lyricItems = []player.LyricLine{} // 显式空 slice——nil 会序列化成 null
			}

			currentLyricItems = lyricItems
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: initPlay,
				Progress: progress,
				Lyrics:   lyricItems,
				Count:    len(lyricItems),
			})
			if len(currentLyrics) == 0 {
				// index:-1 的 lyric_update 必须显式带 Progress（契约：整首播到哪；漏写零值兜 0 会与
				// all_lyrics.progress 自相矛盾——lyricupdate_lint_test 全仓强制）。
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: pureHint, Timestamp: 0, Position: initPlay, Progress: progress,
				})
			}
		}

		// 时长补取：换歌那一轮已经等过 durationGrace，绝大多数情况这里是空转。它是宽限超时那条
		// 路径的兜底 —— 那时 currentDurationSec 会是 0，而 ClampProgress 的 `duration <= 0` 闸
		// 会让 progress 恒 0（真机录音里实见过整首 progress=0）、插值也失去上限。
		//
		// **补到就必须重发一次 all_lyrics，不能只补内部时钟。** 服务端缓存里 `ps.Duration` 只由
		// all_lyrics 写，而 `ps.Progress` 由 all_lyrics 与 lyric_update 两条都写（server.go 的
		// UpdatePlayerState）。只补内部时钟的话，此后每条 lyric_update 会把 progress 写成非 0，
		// 而 duration 永远停在 0 —— 缓存里同时存在 `duration:0` 与 `progress:0.57`，
		// buildInitEvents / FullState / HTTP `/all_lyrics` 会把这个**自相矛盾**的载荷发给中途连入
		// 的每一个前端。补之前两个字段同为 0（错但自洽），只补一半反而把降级态变成了不自洽态，
		// 而 server.go 的注释正把这类组合列为已修过的 bug 形状。重发只在这条罕见路径上发生一次。
		//
		// **只在从未取到时补**，不去追后续变化：换歌之外的时长变化没有已知来源，跟着变只会平白
		// 引入抖动，还会把 all_lyrics 变成高频事件。
		if currentDurationSec <= 0 && durationSec > 0 {
			currentDurationSec = durationSec
			pos := livePos()
			log.Info("时长晚到，补发 all_lyrics（duration=%.2fs）", currentDurationSec)
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title:    currentTitle,
				Duration: currentDurationSec,
				Position: pos,
				Progress: player.ClampProgress(pos, currentDurationSec),
				Lyrics:   currentLyricItems,
				Count:    len(currentLyricItems),
			})
		}

		// ── 播放状态变化检测 ──
		curPlaying := boolToInt(data.IsPlaying)
		if curPlaying != lastPlaying {
			lastPlaying = curPlaying
			if data.IsPlaying {
				isPlaying = true
				// **与暂停分支同一道闸**：只有本轮刚刷新的采样才比现有锚点可信。
				// 暂停时锚点已被冻结成外推出来的准确位置；恢复时若无条件采纳 progressSec，
				// 而平台的精确补发要约 0.7s 才到，拿到的就是暂停前那个最多滞后 1s 的旧值 ——
				// 位置当场倒退最多 1s，playback_resume 报小，跨行时还会重发上一行歌词
				// （OBS 上歌词退一行、约 1s 后再跳回来）。采样没刷新就什么都不做，锚点原样保留。
				if rawChanged && isProgressValid(progressSec, currentDurationSec) {
					anchorProgressSec = progressSec
				}
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
				log.Info("恢复 @ %.2fs", anchorProgressSec)
			} else {
				// 冻结位置：**只有本轮刚刷新的采样才比外推值可信**。progressSeconds 是 1Hz 的，
				// 上一轮那个旧采样最多滞后 1s，直接拿它当冻结点会把「停在哪」写早近一秒。
				// 暂停瞬间平台会补发一次精确采样（实测约 0.7s 后到达），那一次由下面的
				// rawChanged 分支采纳，把锚点修正到真值。
				frozen := livePos()
				if rawChanged && isProgressValid(progressSec, currentDurationSec) {
					frozen = progressSec
				}
				isPlaying = false
				anchorProgressSec = frozen
				anchorTime = time.Now()
				p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: anchorProgressSec, Progress: player.ClampProgress(anchorProgressSec, currentDurationSec)})
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
				log.Info("暂停 @ %.2fs", anchorProgressSec)
			}
		}

		// ── 进度更新 + seek 检测 ──
		//
		// **落锚的门是 rawChanged，不是「每轮」**（理由见 lastRawSec）。暂停时也落锚：那正是
		// 暂停瞬间补发的精确采样、以及暂停期间 seek 的唯一入口。seek 判定则只在播放中做——
		// 暂停时外推时钟不走，drift 恒等于「新采样 - 冻结值」，拿 3s 阈值去判会把一次正常的
		// 暂停补发当成跳转。
		if rawChanged && isProgressValid(progressSec, currentDurationSec) {
			if isPlaying {
				before := livePos()
				drift := progressSec - before
				if drift < -3.0 {
					log.Info("检测到回跳: %.2fs → %.2fs", before, progressSec)
					lastLineIdx = -1
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
				} else if drift > 3.0 {
					log.Info("检测到前跳: %.2fs → %.2fs", before, progressSec)
					lastLineIdx = -1
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: progressSec, Progress: player.ClampProgress(progressSec, currentDurationSec)})
				}
			}
			anchorProgressSec = progressSec
			anchorTime = time.Now()
		}

		// ── 本地时钟插值 ──
		interpSec := livePos()

		matchTime := interpSec + offsetSec

		// ── 歌词行匹配 → lyric_update ──
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
				// interpSec 进 Position/算 Progress；play_time 由 BuildLyricUpdate 按歌词时间轴算。
				// words 的 play_time 已在 applyDetailedOffset 里套过 offset，这里透传。
				p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
					trueIdx, line.Time, line.Text, line.SubText, line.Detailed,
					offsetSec, interpSec, currentDurationSec,
				))
			}
		}

		time.Sleep(pollInterval)
	}
}

// runCoverFetch 发歌曲信息与封面；每首歌一个，换歌时由 ctx 取消。
//
// 先发一条只带 URL、不带 base64 的信息让标题与封面尽快上屏，再下载 base64 补发完整信息。
// **不变量：即使 b64 拿不到（超时/过大/失败）也照发带 URL 的信息**——绝不为封面吞掉歌曲信息，
// 否则 OBS 整首停在上一首（轮询只在换歌时发、不自愈）。FetchCoverBase64 图片超限时本就返回空 URL 照发。
//
// **第一条必须带上 coverURL，别再退回「不含封面」。** 前端（lyric_page.html 的
// updateSongInfoRow）用 `info.cover_base64 || info.cover` 取封面源，为空就走
// `coverArt.classList.remove('visible')` —— 那是**无过渡的硬隐藏**。于是换歌时前端先被这条
// 空封面把旧封面瞬间抹掉，等第二条 base64 到了又因为此刻 `visible` 已被摘掉而落进「首次显示」
// 分支静默贴上：现象就是封面闪一下、既没有淡出也没有淡入（切播放器与网易云自己切歌都正常，
// 唯独汽水自己切歌闪，差别就在这一条）。
//
// 带上 URL 后：第一条的 cover 非空 → 前端走淡出/淡入过渡；第二条的 `info.cover` 与第一条相同 →
// 前端的 isSameSong 判定为真 → 只静默把图源升级成 base64，不会再放一次动画。这正是前端那套
// isSameSong 逻辑预设的用法，cloudmusic 一直就是这么发的（cloudmusic.go 的首条带 Cover）。
//
// 对照 kugou：它首条不带封面是**因为那时确实还没拿到 URL**（`if coverURL == ""` 才发那一条）；
// 汽水的 URL 随 Extract 一起到、就在入参里，没有理由丢掉。
func (p *SodaMusicPlayer) runCoverFetch(ctx context.Context, coverURL, name, singer, title string) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	// 先发标题 + 封面 URL（不含 base64）
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL,
	})

	if coverURL == "" {
		return // 汽水一般都给封面；确实没有则标题已发，无新东西可补
	}
	b64 := player.FetchCoverBase64("Soda", coverURL, coverBudget)

	select {
	case <-ctx.Done():
		return
	default:
	}
	p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: name, Singer: singer, Title: title,
		Cover: coverURL, CoverBase64: b64,
	})
}

// parseSodaLyrics 把 Extract 里的明文 KRC 解析成带逐字的 Line，合并翻译轨，并套 offset。
// 非 KRC / 空内容 → 返回 nil（按无歌词处理）。汽水实测歌词恒为 krc 型明文。
func parseSodaLyrics(data *cdp.ExtractionData, offsetSec float32) []krc.Line {
	if data.LyricType != "krc" || data.LyricContent == "" {
		return nil
	}
	lines := krc.ParsePlainKRC(data.LyricContent)
	if len(lines) == 0 {
		return nil
	}
	applySodaTranslations(lines, data.TranslationLRC) // 独立翻译轨（tlyric）按时间戳合并进 SubText
	applyDetailedOffset(lines, offsetSec)
	return lines
}

// buildTitle 组「歌手 - 歌名」（与 kugou 一致）。歌手为空则只留歌名。
func buildTitle(name, singer string) string {
	if singer == "" {
		return name
	}
	return fmt.Sprintf("%s - %s", singer, name)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isProgressValid 判断 progressSec 是否在合理播放范围 [0, duration*1.05]，过滤异常值。
func isProgressValid(progressSec, durationSec float32) bool {
	if progressSec < 0 {
		return false
	}
	if durationSec > 0 && progressSec > durationSec*1.05 {
		return false
	}
	return true
}

// toLyricLines 把解析结果映射成对外 LyricLine（subText/detailed 透传给 BuildLyricLine）。
func toLyricLines(lines []krc.Line, offsetSec float32) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.Detailed, offsetSec)
	}
	return out
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time。
//
// **必须在解析之后、存进 currentLyrics 之前套一次**：BuildLyricLine/BuildLyricUpdate 只算行级
// play_time，TextDetailed 原样透传（对照 kugou.go 的同名函数）。漏了这步逐字高亮会整体偏一个
// offset 而行级却对，症状极像"前端没对齐"。只对新解析的歌词套一次，别重复套。
func applyDetailedOffset(lines []krc.Line, offsetSec float32) {
	for i := range lines {
		d := &lines[i].Detailed
		if len(d.Words) == 0 {
			continue
		}
		d.PlayTime = player.AdjustLyricPlayTime(d.Timestamp, offsetSec)
		for j := range d.Words {
			d.Words[j].PlayTime = player.AdjustLyricPlayTime(d.Words[j].Timestamp, offsetSec)
		}
	}
}

// detailedFlag 报告这批歌词**实际**有没有逐字（AGENTS §6.1：如实报告，绝不写死）。
func detailedFlag(lines []krc.Line) string {
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("是")
		}
	}
	return i18n.T("否")
}
