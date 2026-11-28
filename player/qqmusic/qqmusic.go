package qqmusic

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "qqmusic"

// errSongMidUnavailable 表示 v20.05 上 songMid 等窗已尽仍为空（多半是本地文件播放）。
// 走 err 分支即可——那条路径会发标题 + 空歌词，是这种情况下唯一诚实的降级。
var errSongMidUnavailable = i18n.Sentinel("songMid unavailable (possibly playing a local file)")

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("QQMusic")

// QQMusicPlayer QQ 音乐播放器
type QQMusicPlayer struct {
	player.BaseEmitter
	offsetMs         int
	pollMs           int
	rumGate          player.SessionRUMGate[*qqSessionRUM]
	testBeforeAttach func() // 仅供单测注入 Register 锁外发 attached 前的并发交错屏障
	lastAttachMs     atomic.Int64
}

// New 创建 QQ 音乐播放器
func New(offsetMs, pollMs int) *QQMusicPlayer {
	return &QQMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

// EffectivePollInterval 返回配置轮询间隔为 pollMs 毫秒时 QQ 音乐实际采用的间隔：低于 30ms 抬到 50ms。
// 轮询循环与 /service-status 的 config_effective 共用此函数，下限只在这里定义。
func EffectivePollInterval(pollMs int) time.Duration {
	d := time.Duration(pollMs) * time.Millisecond
	if d < 30*time.Millisecond {
		return 50 * time.Millisecond
	}
	return d
}

// qqProcessRunning 用于会话结束时判 QQMusic.exe 是否仍在运行，离线用例替换它以驱动外层状态。
var qqProcessRunning = isQQMusicRunning

// qqMusicDetails 是 QQ 音乐在「没在读歌」时各种情形的 detail。
func qqMusicDetails() player.ProcessDetails {
	return player.ProcessDetails{
		NotRunning:   i18n.T("QQ Music not running"),
		Exited:       i18n.T("QQ Music has exited"),
		Disconnected: i18n.T("QQ Music disconnected"),
	}
}

func qqMusicNoAdminBlocked() player.Blocked {
	return player.Blocked{
		Reason: player.ReasonNoAdmin,
		Detail: i18n.T("Cannot read the QQ Music process (QQ Music may be running as administrator)"),
	}
}

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *QQMusicPlayer) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	select {
	case <-p.StopCh:
		t.Stop()
		return true
	case <-t.C:
		return false
	}
}

// Start 启动 QQ 音乐轮询循环（阻塞）。
//
// 非歌名状态一律由 player.ProcessStatus / NoSongStatus 合成、经 EmitStatusIfChanged 发出（#231，设计
// doc/design/converge-player-behavior-231-design.md §3.2）：进程未运行报 waiting_process（按本次服务期间是否
// 见过它运行区分 process_not_running 与 process_exited）；进程在运行而拒绝访问报 standby / no_admin，
// 模块尚未就绪或会话结束而进程仍在运行报 standby / cdp_disconnected。
func (p *QQMusicPlayer) Start() {
	offsetSec := float32(p.offsetMs) / 1000.0
	d := qqMusicDetails()
	seen := false // 本次服务期间见过 QQMusic.exe 在运行：区分 process_exited 与 process_not_running
	var blockTrack qqBlockTracker

	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		attachStart := time.Now()
		mem, err := ConnectQQMusic()
		if err != nil {
			switch {
			case errors.Is(err, errQQMusicAccessDenied):
				seen = true
				running := qqProcessRunning()
				if running {
					blockTrack.onBlocked(player.ReasonNoAdmin)
				} else {
					blockTrack.onNotRunning()
				}
				p.EmitStatusIfChanged(player.ProcessStatus(running, true, d, qqMusicNoAdminBlocked()))
			case errors.Is(err, errQQMusicModuleNotReady):
				seen = true
				running := qqProcessRunning()
				if running {
					blockTrack.onBlocked("cdp_port_not_ready")
				} else {
					blockTrack.onNotRunning()
				}
				p.EmitStatusIfChanged(player.ProcessStatus(running, true, d, player.Blocked{}))
			default:
				blockTrack.onNotRunning()
				p.EmitStatusIfChanged(player.ProcessStatus(false, seen, d, player.Blocked{}))
			}
			p.Emit(player.EventClearSongData, nil)
			if p.sleepOrStop(2 * time.Second) {
				return
			}
			continue
		}

		seen = true
		p.lastAttachMs.Store(time.Since(attachStart).Milliseconds())
		blockTrack.onRecovered()
		log.Info("Connected to QQMusic.exe (PID: %d, version: %s)", mem.pid, mem.Version())

		p.runSession(mem, offsetSec)

		p.InvalidateSongGen() // 作废在飞的封面回写，别让它在 ClearSongData 之后复活已清空的 SongInfo
		running := qqProcessRunning()
		if !running {
			blockTrack.onNotRunning()
		}
		p.EmitStatusIfChanged(player.ProcessStatus(running, true, d, player.Blocked{}))
		p.Emit(player.EventClearSongData, nil)
		log.Info("Session ended; waiting for a new QQMusic process...")
		if p.sleepOrStop(2 * time.Second) {
			return
		}
	}
}

func (p *QQMusicPlayer) runSession(mem *QQMusicMem, offsetSec float32) {
	defer mem.Close()
	rumSess := p.startSessionRUM(mem.Version(), mem.fallbackTo, p.lastAttachMs.Load())
	disconnectCause := "process_exited"
	defer func() {
		p.endSessionRUM(rumSess, disconnectCause)
	}()

	// 滑块 AOB 探针已暂时关闭，详见 #39（含完整设计文档与恢复条件）。
	//
	// 为什么关：它的产出 SliderVal 全仓库零消费方（mem.go 里声明+赋值，无任何读取方），
	// 而代价是每次会话都往 QQ 音乐进程写内存 + 装 codecave + 打 E9 跳转——这是杀软
	// 重点盯的注入签名。为一个尚未实现的功能长期扛误报风险不划算。
	//
	// 注：此前这里写的是「本程序**唯一**会写外部进程内存的地方」——那是错的。
	// 同一个文件里还有 InjectProgressAOB（mem.go 内，VirtualAllocEx + WriteBytes +
	// E9 跳转 + VirtualProtectEx，注入签名与本函数完全一致），它只是运行时不写，
	// 因为调用点在 84f4dc1 被删了、函数本体留到今天。写那句时的排查粒度是「按播放器
	// 包」而非「按文件」，给一个函数写注释却没往下翻 20 行。
	//
	// 恢复：仅取消下面三行注释不够——ConnectQQMusic 已收窄为只读权限（PROCESS_QUERY_INFORMATION |
	// PROCESS_VM_READ，#233），不再允许写外部进程内存；恢复注入需另行评估写权限。InjectSliderAOB 及其
	// 逆向实现原样保留在 mem.go。关闭期间 m.sliderPointer 保持 0，mem.go 的 `if m.sliderPointer != 0`
	// 守卫会让 sliderVal 停在 0，无其他副作用。
	//
	// **恢复后如何验证**（别重蹈覆辙）：不能用「同一个 QQ 音乐进程跑前后两轮」来做
	// A/B —— E9 补丁与 codecave 写在 QQ 音乐地址空间里，本程序退出**不撤销**（全仓无
	// VirtualFreeEx/还原代码），所以第二轮跑的是一个仍带着第一轮 hook 的 QQ 音乐。
	// 反证就是 mem.go 里那句 `if firstByte[0] == 0xE9 { log.Info("滑块 Hook 已激活") }`
	// ——它存在的唯一理由就是 hook 跨我方重启存活。要验必须每轮重启 QQ 音乐本身。
	// if err := mem.InjectSliderAOB(); err != nil {
	// 	log.Warn("滑块 Hook 失败: %v", err)
	// }

	var lastName string
	var currentLyrics []lyricLine
	var midWait midWaitGate      // v20.05 等 songMid 就绪的有界等待
	var progressWait midWaitGate // 换歌后等进度从上一首换过来的有界等待（见 progressCarriedOver）
	var lastLineIdx int = -1
	var currentDurationSec float32 = 0
	var isPaused bool = false
	var judge judgeState // 暂停与恢复判定的跨拍状态（#172），见 judgeTick
	var hold switchHold  // 换歌前进度归零的暂扣，见 switchHold
	var currentTitle string
	var currentName, currentSinger string // 认领时（或补取按开关取舍后）的歌名与歌手（#228、#229）
	var currentCoverURL string            // 认领时取到的封面地址，补取作废旧任务且未取到新地址时回退用（#229）

	// 换歌后歌词补取：songID/songMid 落位可能滞后于显示结构体几秒（v22.41 实测），换歌那帧
	// 常取不到歌词。认领并发标题后，在窗口内持续重试，ID 一落位就补上，避免整首空白。
	var lyricsPending bool
	var backfill backfillWindow  // 补取的窗口与重试节流（#227）：截止与节流只在它里面比较
	var retryCount int           // 本首歌已做的补取尝试次数（用于「多次未果才打日志」）
	const lyricRetryLogAfter = 4 // 重试满这么多次仍未就绪才打一行 Detail（快速落位则完全静默）

	// 快速计时器锚点 + 本地时钟插值
	var anchorProgressMs uint32 = 0
	var anchorTime time.Time
	var lastMemProgress uint32 = 0

	// 歌词关联（#218，设计 §6.8）：读取者 goroutine 承担读文件、解密、Restart Manager 查询与读 QQ 内存里的当前歌曲记录，
	// 本循环只经 Submit 写入当前歌曲、经 takeResult 非阻塞地取结果。会话的每条退出路径都取消它。读记录用的
	// 只读句柄在这里（本 goroutine，hProcess 尚未被 CheckValid 改动）由 newRecordSource 复制，交读取者退出时关闭。上一个读取者
	// 尚未退出时 reader 为 nil（该句柄当即关闭）：Submit 什么也不做，Results 为 nil 通道、永远取不到，行为同未接入。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, _ := startAssocReader(ctx, defaultAssocDeps(mem.pid, mem.Version(), newRecordSource(mem)))
	var lyricSt lyricState             // 本首的在线原始行与采纳的期望状态，代次（gen，reopen）与 assocKey 的相同
	var assocKey songKey               // 最近写给读取者的当前歌曲
	var pendingCover backfillCoverGate // 本地认定未决时暂缓的补取封面请求（#248），换歌清零
	var reopenG reopenGuard            // 同一首重开的统一跨拍守卫（#245），协调 R1/R2/R3 触发与去重
	var repausePending bool            // 暂停中补发的 playback_pause（随歌词、暂停中跳转）入队失败，仍在暂停中时每拍再试；恢复与换歌时清掉
	var repausePos float32             // 那条 playback_pause 的位置：它所跟随的 all_lyrics 的位置，或跳转的落点；再试时不变
	var noSongClear = true             // 本会话首次连上无歌报 waiting_song/no_song 后尚待发出的 clear_song_data；入队成功后置假

	// showLyrics 换上歌词状态机给出的行并发出（设计 §6.8「应用期望状态」第 3 步），补取块与每拍的结果共用；行为空即无歌词。
	// 每次都重置 lastLineIdx：服务端收到 all_lyrics 即清空缓存的当前行，nextLine 只在行号变大时宣告，不重置则仍落在同一行时
	// 当前行不再宣告。位置由读数 baseSec 与其时刻 at 经 LivePosition 推算。
	// 在线歌词还在补取而要显示的为空（换歌拍取词失败后读取者给出「在线」），没有结论，不发 index:-1（下游读作没有歌词）：
	// 显示本就为空时什么也不发，补取成功或超时同现状；否则只发空的 all_lyrics 清掉显示，同换歌拍编号未落位。
	// 已判暂停时随后在同一位置 pos（暂停中即读数本身）补发 playback_pause：前端收到 all_lyrics 即开始插值，须再冻结。这是暂停中
	// 补发（#172，doc/design/qqmusic-start-stall-design.md §6.2 末）的唯一一处，补取与播放中换词都经这里；它紧跟在发出之后，
	// 早于调用方其后的同步请求（补取块的取封面地址）。不取锚点：暂停后 QQ 补写一次进度的那一拍，补取重读到的是新读数而锚点
	// 还是旧的，按锚点冻结会把前端拉回一步，与本拍稍后落下的锚点不一致。入队失败由 repausePending 记下、按同一位置每拍再试
	// （同 kuwo 的 resendPauseIfPending）：这条丢了，前端到恢复为止一直按 all_lyrics 往前插值（lyric_update 只校正锚点、不冻结）。
	showLyrics := func(lines []lyricLine, baseSec float32, at time.Time) {
		undecided := lyricsUndecided(lines, lyricsPending, lyricSt.source(), lyricSt.undecided())
		if undecided && len(currentLyrics) == 0 {
			return
		}
		currentLyrics = lines
		lastLineIdx = -1
		pos := player.LivePosition(baseSec, at, time.Now(), !isPaused, currentDurationSec)
		prog := player.ClampProgress(pos, currentDurationSec)
		if len(lines) == 0 && !undecided {
			log.Info("Detected instrumental/no lyrics; clearing lyrics; per-word: %s", detailedFlag(lines))
			p.emitNoLyrics(currentTitle, currentDurationSec, pos, prog, lyricSt.hint())
		} else {
			items := toLyricLines(lines, offsetSec) // 没有结论时为空切片：count:0 的 all_lyrics，不带 index:-1
			p.Emit(player.EventAllLyrics, &player.AllLyricsData{
				Title: currentTitle, Duration: currentDurationSec, Position: pos, Progress: prog,
				Count: len(items), Lyrics: items,
			})
		}
		if isPaused {
			repausePos = pos
			repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)
		}
	}

	// drainAssoc 非阻塞地取至多一条读取者结果并在无变化时检查首轮本地认定截止（#248），有变化即按锚点经 showLyrics 发出。
	// 每拍尾部与 ReadAllMetadata() 失败拍的未决态共用，确保读失败跳过尾部时未决态也能在 1s 上界内解除。
	drainAssoc := func(now time.Time) {
		prevSrc, prevShift, prevContent, prevUndecided := lyricSt.source(), lyricSt.shown.lyricOffsetMs, lyricSt.shown.content, lyricSt.undecided()
		lines, changed := takeResult(&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec)
		fromTake := changed
		if !changed {
			lines, changed = lyricSt.expireWait(now, assocKey.hasIDs(), offsetSec)
		}
		if changed {
			showLyrics(lines, float32(anchorProgressMs)/1000.0, anchorTime)
			rumSess.onDrainChanged(assocKey.gen, prevSrc, prevShift, prevContent, prevUndecided, !fromTake, &lyricSt, lines)
		}
	}

	// reopenSong 写入同一首重开（设计 §6.8「重开」、#245 增补设计 §3.3）：先经 reopenG.noteReopen(progressMs) 统一登记本次重开
	// 消费（清掉旧挂起边沿与兜底挂起并记录进度读数供跨拍去重），再 reopen 加一，歌词状态只换代次（不 reset，在线原始行与已接受的
	// 结果保留，新代次的结果按「播放中」应用），写给读取者重读当前歌曲记录；gen 不变。只做加锁赋值与非阻塞唤醒，不等待。调用处有
	// 六处，每拍由 reopenedThisTick 与 reopenG 共同保证跨拍去重且单拍至多写入一次：R1 载入边沿及占位串挂起兑现（拍首、换歌重读与
	// 补取重读各一处）、拍首占位串挂起兜底兑现（else if）、R2 暂扣放行按回跳补发（变化分支）与 R3 暂停在开头 1 秒以内恢复（if v.resume）。
	reopenSong := func(byLoading bool, progressMs uint32) {
		reopenG.noteReopen(progressMs)
		assocKey.reopen++
		lyricSt.reopened(assocKey.reopen)
		reader.Submit(assocKey)
		if byLoading {
			log.Detail("Same song reopened (QQ Music entered loading)")
		} else {
			log.Detail("Same song reopened (progress returned to the start)")
		}
		rumSess.onReopen(byLoading, &lyricSt)
	}

	// noteDetail 在两处 fetchCoverURL 之后打会话一侧的「核验不过」Detail（设计 §6.9，条件见 unconfirmedDetail：收到了响应而核验
	// 不过才打，请求失败不打），每首至多一条（detailNoted 换歌清掉）。封面的取用照现状，不看核验结果。
	var detailNoted bool
	noteDetail := func(queriedID uint32, queriedMid string, d songDetail) {
		if q, ok := unconfirmedDetail(assocKey, queriedID, queriedMid, d); ok && !detailNoted {
			detailNoted = true
			log.Detail("QQ Music song detail did not confirm the queried ID %s (response: %s)", q, d.seen)
		}
	}

	// runBackfillCover 执行补取成功后的封面请求、详情交接与异步下载（设计 §6.4 第 2 步「交接」、#229、#248）。
	// 补取成功分支开头已由 p.NewSongGen() 产出 coverGen 作废认领封面任务；此处取不到新地址时回退 currentCoverURL，
	// 并以该 coverGen 经 p.startCover 启动回写。补取之前已失败（封面请求失败或窗口曾用尽）、这次核验通过即改为已得；
	// 所查编号衔接不上时详情结果仍为尚无，随后由 exhaustDetail 记为已失败并写入，交读取者以 assocKey 已知编号兜底。
	// 非未决态在补取块内立即调用；本地首轮认定仍未决时由 pendingCover 暂缓到未决解除后的下一非暂扣非放行拍再调，
	// 避免阻塞 takeResult / expireWait。
	runBackfillCover := func(coverGen uint64, sid uint32, smid, name, singer, title string) {
		coverURL, det := fetchCoverURL(sid, smid)
		if k, ok := applyDetail(assocKey, sid, smid, det); ok {
			assocKey = k
			reader.Submit(assocKey)
		}
		if k, ok := exhaustDetail(assocKey); ok {
			assocKey = k
			reader.Submit(assocKey)
		}
		noteDetail(sid, smid, det)
		if coverURL == "" {
			coverURL = currentCoverURL // 认领时的任务已作废：取不到新地址时用认领时的地址，这首仍有封面
		}
		rumSess.bindCoverGen(assocKey.gen, coverGen, coverURL != "")
		p.startCover(realQQCoverOps, coverGen, coverURL, name, singer, title)
	}

	pollInterval := EffectivePollInterval(p.pollMs)
	loadOpen := newLoadEdge(pollInterval) // 载入值由无到有的察觉（#218 同一首重开）、改名证据与载入段的记录，见 loadEdge

	for {
		select {
		case <-p.StopCh:
			disconnectCause = "stopped"
			return
		default:
		}

		if p.sleepOrStop(lyricSt.pollSleep(pollInterval, time.Now())) {
			return
		}

		if !mem.CheckValid() {
			if qqProcessRunning() {
				disconnectCause = "disconnected"
			}
			return
		}

		meta, err := mem.ReadAllMetadata()
		if err != nil {
			loadOpen.missed() // 这一拍没有读到播放状态：改名证据不成立（设计 §6.8「改名证据」）
			if lastName != "" && lyricSt.undecided() {
				drainAssoc(time.Now())
			}
			continue
		}
		// sampleTime 是 meta（含进度读数）的读取时刻。落锚、seek 检测与停滞判定都按它计时，不取用到读数时
		// 的时刻：本轮若做过阻塞的取词（换歌分支、歌词补取），当前时刻比读数晚了取词耗时，这段时间里
		// 计时器走没走并不知道——把它算进停滞时长会误判暂停，把取词前的读数配到取词后的时刻上会让位置落后。
		// 取词返回后会重读一次进度，读到同一首歌时 meta.ProgressMs 与它一起换成新读数。
		sampleTime := time.Now()

		// --- 同一首重开（#218 设计 §6.8「重开」、#245 增补设计 §3.3）---
		// QQ 从另一入口重开同一首时歌名不变，换歌检测看不出，而它查关联所用的键可能随之改变。三条确定性规则由统一跨拍守卫
		// reopenG 协调并去重：R1 载入边沿及占位串挂起兑现（本处拍首、换歌重读与补取重读三处共用 reopenG.shouldReopenByLoading，
		// 先于 && !reopenedThisTick 调用以免短路漏掉边沿观测）、R2 暂扣放行回零兜底（变化分支，不受 !loadUsable 封锁，覆盖 20ms
		// 超短载入漏采）与 R3 暂停在开头 1 秒以内恢复兜底（if v.resume）。载入值以 judgeTick 所用的 classify 判定，三处 step 都传
		// 该次读数的时刻 sampleTime，供改名证据的窗口与载入段的记录（第 5 版）；reopenedThisTick 保证每拍至多写入一次。
		loadUsable := mem.Offsets().PlayState.loadingUsable(judge.bad, pollInterval)
		reopenedThisTick := false
		if edge := loadOpen.step(mem.Offsets().PlayState, meta.PlayState, meta.PlayStateOK, sampleTime); reopenG.shouldReopenByLoading(loadUsable, edge, meta.Name, lastName, assocKey.hasIDs()) && !reopenedThisTick {
			reopenedThisTick = true
			reopenSong(true, meta.ProgressMs)
		} else if !reopenedThisTick && reopenG.shouldReopenByFallback(false, meta.Name, lastName, assocKey.hasIDs()) {
			reopenedThisTick = true
			reopenSong(false, meta.ProgressMs)
		}

		// --- 切歌检测 ---
		// 用前缀挡掉 QQ 空闲/过渡态漏进 Name 字段的窗口标题串（"QQ音乐"、"QQ音乐 听我想听" 等）——
		// 真歌名极不可能以 "QQ音乐" 开头，否则会把占位串当成一首歌、推出错误标题。
		if meta.Name != lastName && meta.Name != "" && !strings.HasPrefix(meta.Name, "QQ音乐") {
			// 换歌时显示结构里的歌名与时长先换成新歌的，进度要晚一些才从上一首换过来（issue #96 的录音里
			// 换歌那一拍读到的仍是上一首的位置）。按它认领，all_lyrics 带的是上一首的进度，首条 lyric_update
			// 落在新歌中间的某一行，进度归零时又被判成一次回跳。进度换过来之前不认领，本轮其余判定一并跳过，
			// 上一首停在已发出的那一行；等待有界，等窗用尽按当时的读数认领。
			if progressCarriedOver(lastName, lastMemProgress, meta.ProgressMs) {
				if _, keepWaiting := progressWait.wait(meta.Name, sampleTime, progressSettleWindow); keepWaiting {
					continue
				}
			}

			// v20.05: songMid 从 URL 参数异步提取，换歌瞬间可能尚未就绪；struct+0x40 不是
			// 标准 songId，用它请求会拿到错歌词。此时**不能认领 lastName**——否则下次轮询
			// meta.Name==lastName 不再进本分支，songMid 就绪也永不重试，上一首的 currentLyrics
			// 会一直逐行滚在新歌上，直到再下次换歌。
			//
			// 但等待必须有界：songMid 也可能持久为空（本地文件播放），无界等 = 这首歌一个
			// 事件都不发、overlay 整首停在上一首。等窗用尽就认账，走下面的「无歌词」路径。
			// v22.41 取词键：优先用稳定字段里的数字 songID（ReadAllMetadata 已按时长核对，非 0
			// 即当前歌有效 ID）；换歌瞬间它常还没落位（读到 0），此时不 defer、不空扫内存——
			// 认领并发标题后交给下面的「补取块」在窗口内持续重试。只有 v20.05 仍走短暂 defer。
			usesSongMid := mem.Offsets().SongMidParamsOff != 0
			midUnavailable := shouldDeferForSongMid(usesSongMid, meta.SongMid)
			if midUnavailable {
				started, keepWaiting := midWait.wait(meta.Name, time.Now(), midWaitWindow)
				if started {
					currentLyrics = nil // 停止上一首歌词继续滚在新歌上
					lastLineIdx = -1
				}
				if keepWaiting {
					continue // 不认领 lastName：下次 songMid 就绪后重走完整换歌流程
				}
				log.Warn("songMid still not ready after waiting %v; continuing without lyrics (local file playback?)", midWaitWindow)
			}
			midWait.reset()
			progressWait.reset()
			hold.drop(sampleTime) // 暂扣中的回跳是换歌造成的，不发（#202）

			lastName = meta.Name
			lastLineIdx = -1
			anchorProgressMs = meta.ProgressMs
			anchorTime = sampleTime
			lastMemProgress = meta.ProgressMs
			isPaused = false
			repausePending = false
			judge.resume.reset()
			lyricsPending = false // 默认这一首已了结；仅下面取词失败时重新 arm
			detailNoted = false
			pendingCover.reset()
			reopenG.reset()

			log.Info("♪ song: %s - %s", meta.Name, meta.Singer)

			// 写给歌词关联的读取者，不等待（设计 §6.8「换歌拍」）：gen 加一；excludeLocal 是上一首最终采纳的本地文件（须在 reset
			// 之前取），有「同一次播放被 QQ 改名」的证据时为空（第 5 版，设计 §6.5 第 5 步、§6.8「改名证据」）：证据出自本拍的
			// loadUsable 与 loadOpen 自上一次认领以来记下的窗口，claim 求值并清零窗口，每次认领恰一次；补取块的 gen 加一与重开
			// 不碰它。v20.05 的结构体 songID 是伪值，只认 songMid（同补取块）。
			sid := meta.SongID
			if usesSongMid {
				sid = 0
			}
			prevLocal := lyricSt.localPath()
			renamed := loadOpen.claim(loadUsable, mem.Offsets().PlayState, pollInterval)
			logRenameEvidence(prevLocal, renamed)
			assocKey = songKey{gen: assocKey.gen + 1, songID: sid, songMid: meta.SongMid, name: meta.Name, excludeLocal: excludeFor(prevLocal, renamed)}
			lyricSt.reset(assocKey.gen, reader.LocalCheckPossible(), sampleTime.Add(localDecideWindow))
			reader.Submit(assocKey)
			lyricStart := time.Now()
			rumSess.beginTrack(assocKey.gen, qqClockMode(mem, judge.bad, pollInterval), qqIDSource(sid, meta.SongMid, false))

			cookie := mem.FindCookie()
			var lrcName, lrcSinger string
			var coverURL string
			var det songDetail // 封面请求带回的详情结果（设计 §6.4 第 2 步「交接」）

			if midUnavailable || (meta.SongID == 0 && meta.SongMid == "") {
				// songID/mid 尚未落位（v20.05 等 URL 参数；v22.41 等会话结构+堆上报）——不拿空/伪
				// ID 去空请求（v20.05 上会返回别的歌的歌词/封面）。按无歌词发标题+空歌词，下面
				// arm 补取块，ID 一落位就在窗口内补上。
				err = errSongMidUnavailable
				// **currentLyrics 必须一起清**：它是下方歌词行匹配的输入。不清的话 all_lyrics 说 count:0，
				// 本轮及其后各轮却仍按上一首的歌词、新歌的进度发 lyric_update；补取窗口用尽还取不到
				// （如播放本地文件）时，整首都在滚上一首。v20.05 的 midWait 路径在开始等待时清过，
				// songID 读到 0 的各版只经过这里。补取成功后由补取块写入新歌词。
				currentLyrics = nil
				lyricSt.setTitle(meta.Name + " - " + meta.Singer)
				// 显示的即是「无」，登记为已给出：读取者随后给出的「无」（本地文件未认定）不再重发。那会多发一条 index:-1，
				// 而编号还在补取，并非确定没有歌词。本地文件的歌词到达照常发出。
				lyricSt.refresh(assocKey.hasIDs(), offsetSec)
			} else {
				currentLyrics, lrcName, lrcSinger, err = fetchLRC(meta.SongID, meta.SongMid, cookie, meta.DurationMs)
				// 封面请求带回的详情结果随当前歌曲交给读取者，不等待（设计 §6.8「换歌拍」）：所查编号等于 assocKey 里的才写（v20.05 的 sid
				// 为 0 而所查为 meta.SongID，对不上即不写）。只是加锁赋值与非阻塞唤醒。
				coverURL, det = fetchCoverURL(meta.SongID, meta.SongMid)
				if k, ok := applyDetail(assocKey, meta.SongID, meta.SongMid, det); ok {
					assocKey = k
					reader.Submit(assocKey)
				}
				noteDetail(meta.SongID, meta.SongMid, det)
				// 取词与取封面地址是阻塞的网络请求，返回时本轮的进度读数已过时：其间可能暂停、恢复或拖动过。
				// 再读一次，读到的仍是这首歌就把它当作本轮的读数——进度与 sampleTime 成对替换；下面随歌词
				// 发出的位置、本轮其后的锚点更新、seek 检测与暂停检测都基于它。读不到、读到的已不是这首歌、或读数
				// 落回开头附近而会触发暂扣（取词期间换歌归零，#202），则沿用取词前的读数，留给下一轮。播放状态字段随读数
				// 一起换（#172）：本拍的判定用与读数同一时刻的标志。重读交给 loadOpen，由无到有即按载入支写入（#218，同补取块那一处）：
				// 认领拍首读不是载入值、重读才见到，即认领之后开始了一次新的载入，多半是取词期间从另一入口重开（换歌时的载入值可能
				// 早于歌名变化一拍出现，那时首读已是载入值，step 已记下）。QQ 若先换歌名、后开始载入，新歌在这里多写一次重开：读取者
				// 多扫一轮，此前给出的关联结果因代次不符被拒，关联歌词晚这一轮出现，最终显示相同。
				if fresh, rerr := mem.ReadAllMetadata(); rereadSameSong(fresh, rerr, lastName, lastMemProgress) {
					sampleTime = time.Now()
					meta.ProgressMs = fresh.ProgressMs
					meta.PlayState, meta.PlayStateOK = fresh.PlayState, fresh.PlayStateOK
					if edge := loadOpen.step(mem.Offsets().PlayState, meta.PlayState, meta.PlayStateOK, sampleTime); reopenG.shouldReopenByLoading(loadUsable, edge, meta.Name, lastName, assocKey.hasIDs()) && !reopenedThisTick {
						reopenedThisTick = true
						reopenSong(true, meta.ProgressMs)
					}
				}
				if lrcName != "" {
					meta.Name = lrcName
				}
				if lrcSinger != "" {
					meta.Singer = lrcSinger
				}
				lyricSt.setTitle(meta.Name + " - " + meta.Singer)
				// 取到的是在线原始行（未套 offset），交给歌词状态机换成要显示的那份：按在线、偏移 0 折算即现状；读取者已给出本首的
				// 期望状态（通常在取词返回之前）即按它，如换成关联的歌词。歌名、歌手仍取取词返回的，不取关联歌词的标签。
				if err == nil {
					lyricSt.setOnlineRaw(currentLyrics)
					currentLyrics, _ = lyricSt.refresh(assocKey.hasIDs(), offsetSec)
					if lines, changed := takeResult(&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec); changed {
						currentLyrics = lines
					} else if lines, changed := lyricSt.expireWait(time.Now(), assocKey.hasIDs(), offsetSec); changed {
						currentLyrics = lines
					}
				}
			}

			title := meta.Name + " - " + meta.Singer
			currentTitle = title
			currentName, currentSinger = meta.Name, meta.Singer
			currentDurationSec = float32(meta.DurationMs) / 1000.0
			// 先作废上一首的异步封面任务，再发本首宣告事件：否则上一首迟到的封面回写落在新歌宣告之后、
			// NewSongGen 之前时仍能通过代次守卫覆盖新歌（#229）。
			coverGen := p.NewSongGen()

			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: title})
			p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
				Name: meta.Name, Singer: meta.Singer, Title: title, Cover: coverURL,
			})

			// 随歌词发出的位置取此刻的（服务端以事件到达的时刻为采样时刻）：重读成功时即刚读到的进度
			// （sampleTime 就是此刻，读数没动就是没动，不必猜在不在播）；没有重读到则由取词前的读数按
			// 经过的时间推算。算式与下方歌词行匹配处的 progressSec 是同一个。
			playTimeSec := player.LivePosition(float32(meta.ProgressMs)/1000.0, sampleTime, time.Now(), !isPaused, currentDurationSec)
			progress := player.ClampProgress(playTimeSec, currentDurationSec)

			if err != nil {
				// 换歌瞬间 songID/mid 尚未落位是预期过渡态，不是失败——静默进入补取重试。
				// 计数清零：仅当重试满 lyricRetryLogAfter 次仍拿不到才打一行 Detail，快速落位的
				// 常见情形完全静默；真失败由下方 8s 超时上报。
				retryCount = 0
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					Position: playTimeSec,
					Progress: progress,
					Lyrics:   []player.LyricLine{}, Count: 0,
				})
				// songID/songMid 尚未落位（换歌瞬间常见）→ arm 补取，别永久放弃整首；下一拍即首次补取
				lyricsPending = true
				backfill.arm(time.Now())
			} else if len(currentLyrics) == 0 && !lyricsUndecided(currentLyrics, lyricsPending, lyricSt.source(), lyricSt.undecided()) {
				// 纯音乐：API 成功且非本地认定未决态，确认无歌词行
				log.Info("Detected instrumental/no lyrics; clearing lyrics; per-word: %s", detailedFlag(currentLyrics))
				p.emitNoLyrics(title, currentDurationSec, playTimeSec, progress, lyricSt.hint())
			} else {
				if len(currentLyrics) > 0 {
					log.Info("Lyrics loaded: %d lines; per-character: %s", len(currentLyrics), detailedFlag(currentLyrics))
				}
				lyricItems := toLyricLines(currentLyrics, offsetSec)
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					Position: playTimeSec,
					Progress: progress,
					Count:    len(lyricItems), Lyrics: lyricItems,
				})
			}

			// 异步封面下载（带代次守卫，理由同 cloudmusic：迟到的回写会盖住下一首）
			currentCoverURL = coverURL
			rumSess.bindCoverGen(assocKey.gen, coverGen, coverURL != "")
			p.startCover(realQQCoverOps, coverGen, coverURL, currentName, currentSinger, title)
			if !lyricsPending && !lyricsUndecided(currentLyrics, lyricsPending, lyricSt.source(), lyricSt.undecided()) {
				rumSess.onTrackLyricSettled(assocKey.gen, lyricSt.source(), currentLyrics, false, lyricSt.hint(), false, time.Since(lyricStart).Milliseconds())
			}
		} else if meta.Name == lastName {
			// 等进度的时候又切回了已认领的歌：这次等待作废。不清的话，之后再切到那首歌，闸门按歌名认出它、
			// 沿用早已过期的等窗，当拍就按上一首的进度认领。
			progressWait.reset()
		}

		// --- 连上后无当前歌曲（#231，设计 §4.3 第 1～2 条、§4.4 qqmusic 行）---
		// 尚未认领过歌曲（lastName == ""）且本拍歌名为空或为 QQ 窗口占位串时，当拍报 waiting_song / no_song 脱离未运行，
		// 先发状态后发 clear_song_data；经 LastStatus + TryEmit 在通道满或仅余 1 槽时于后续无歌拍按序补齐，
		// 成功入队一次后不再重复发。已认领过歌曲（lastName != ""）后中途读到空或占位串保持现行不报。
		if lastName == "" && (meta.Name == "" || strings.HasPrefix(meta.Name, "QQ音乐")) {
			p.EmitStatusIfChanged(player.NoSongStatus(player.ReasonNoSong, i18n.T("QQ Music has no current song")))
			if last, ok := p.LastStatus(); noSongClear && ok && last.Reason == player.ReasonNoSong {
				noSongClear = !p.TryEmit(player.EventClearSongData, nil)
			}
		}

		// --- 换歌前进度归零的暂扣（#202）---
		// 换歌时读数先归零、歌名稍后才换，归零这一拍按回跳发会闪出上一首的第 0 行。落在开头附近的回跳先暂扣，
		// 暂扣中的拍跳过本拍其余判定（补取、变化分支、暂停判定、行匹配、位置采样），不发这首歌的任何事件，
		// 读数与锚点都不动；其间认领新歌即丢弃（见上面的 hold.drop），期满未换歌即放行，由变化分支补发。
		// 必须在补取之前：暂扣中的拍不补取，阻塞的取词不吃掉暂扣时窗。
		// 暂扣期间同样抑制本首未决态的 index:-1（避免换歌前闪屏）；期满未换歌时放行拍经 drainAssoc(time.Now()) 立即补发，且未决态下 pollSleep 维持 50ms 步进不拖延放行（#248）。
		held, released := hold.step(lastMemProgress, meta.ProgressMs, sampleTime)
		if held {
			continue
		}

		// --- 换歌后歌词补取 ---
		// songID/songMid 落位滞后于显示结构体（v22.41：数字 songID 在独立会话结构、songMid 在
		// 堆上报 JSON，换歌瞬间都可能还是上一首或空）。上面认领并发了标题+空歌词，这里在有界
		// 窗口内持续重试：ID 一落位就补上完整歌词（中途替换 all_lyrics），窗口用尽才收场。
		// 暂扣放行的那一拍不补取（#202）：取词与取封面地址是阻塞的联网请求（各 10s 超时），排在变化分支之前，
		// 放行拍若补取，补发的跳转要等它们返回，推迟会超出约 0.4s 加一拍的上限。补取顺延到下一拍，重试节流照旧。
		// 窗口与节流只在 backfill.next 里判（#227）：窗口内到了间隔才重试；窗口用尽时已判暂停则保持，恢复时重设窗口（见下面
		// 判恢复处）：QQ 启动后载入上次的歌而未开播时 ID 不落位，开播才落位，按墙钟用尽即放弃的话这首歌整首没有歌词与封面；
		// 暂停不限时长，用尽后不在暂停中每 500 ms 做一次堆扫描（FindSongMid）。未判暂停时用尽即放弃。其余结果本拍不做。
		if lyricsPending && !released && meta.Name == lastName && meta.Name != "" {
			// 窗口用尽（放弃与保持两支）而本 gen 的详情结果仍为尚无，记为已失败并写入，交读取者兜底（设计 §6.8「补取块」）：补取只在取词
			// 成功之后请求封面，取词一直失败时本 gen 没有封面请求。保持期间每拍都进 backfillHold，只在由尚无改为已失败的那一拍写入。
			switch backfill.next(time.Now(), isPaused) {
			case backfillGiveUp:
				lyricsPending = false
				if k, ok := exhaustDetail(assocKey); ok {
					assocKey = k
					reader.Submit(assocKey)
				}
				if lyricSt.source() == srcLocal { // 已显示本地文件关联的歌词：「可能是本地文件」已成立，不是故障
					log.Detail("Lyric backfill timed out (%v); keeping the lyrics associated with the local file", lyricRetryWindow)
				} else {
					log.Warn("Lyric backfill timed out (%v); continuing without lyrics (local file playback?)", lyricRetryWindow)
				}
				rumSess.onTrackLyricSettled(assocKey.gen, lyricSt.source(), currentLyrics, lyricSt.source() != srcLocal, lyricSt.hint(), false, 0)
			case backfillHold:
				if k, ok := exhaustDetail(assocKey); ok {
					assocKey = k
					reader.Submit(assocKey)
				}
			case backfillRetry:
				retryCount++
				sid, smid := meta.SongID, meta.SongMid
				if mem.Offsets().SongMidParamsOff != 0 {
					sid = 0 // v20.05：结构体 songID 是伪值，只认 songMid
				}
				heapMid := false
				// songID 缺位时才回退堆扫描 songMid（FindSongMid 是全内存扫描，靠外层节流约束）
				if mem.Offsets().SongMidFromHeap && sid == 0 && smid == "" {
					smid = mem.FindSongMid(meta.DurationMs)
					heapMid = smid != ""
				}
				if sid != 0 || smid != "" {
					if !assocKey.hasIDs() {
						// 同一首歌补齐在线编号（#248，设计 §2.3）：保持 gen 与 reopen 不变、不调用 lyricSt.reset，保留本首已认定的
						// 本地文件关联与读取者本地认定日程。详情结果清为尚无（设计 §6.4 第 2 步「交接」）：编号缺位时窗口用尽记下的
						// 已失败不带进本次补取，否则读取者不等这次的封面请求就兜底。
						assocKey.songID, assocKey.songMid = sid, smid
						assocKey.detail = songDetail{}
						reader.Submit(assocKey)
					}
					cookie := mem.FindCookie()
					lines, lrcName, lrcSinger, ferr := fetchLRC(sid, smid, cookie, meta.DurationMs)
					// 同换歌分支：取词返回后再读一次进度，当作本轮的读数。这里不能改用锚点与 isPaused 推算：
					// 本块在本轮的锚点更新与暂停检测之前，它们还是上一轮的，本轮刚恢复或拖动过时位置会取到旧值。
					// 下面的 fetchCoverURL 仍会阻塞，本轮其后的判定按 sampleTime 计，不受它影响。
					// 重读落回开头附近而会触发暂扣时不采用（#202）。播放状态字段随读数一起换（#172）。取词期间 QQ 从另一入口重开同一首时，
					// 载入值可能只出现在这次重读里：交给 loadOpen，由无到有即按载入支写入（#218，条件同每拍开头与换歌认领那两处）。
					if fresh, rerr := mem.ReadAllMetadata(); rereadSameSong(fresh, rerr, lastName, lastMemProgress) {
						sampleTime = time.Now()
						meta.ProgressMs = fresh.ProgressMs
						meta.PlayState, meta.PlayStateOK = fresh.PlayState, fresh.PlayStateOK
						if edge := loadOpen.step(mem.Offsets().PlayState, meta.PlayState, meta.PlayStateOK, sampleTime); reopenG.shouldReopenByLoading(loadUsable, edge, meta.Name, lastName, assocKey.hasIDs()) && !reopenedThisTick {
							reopenedThisTick = true
							reopenSong(true, meta.ProgressMs)
						}
					}
					if ferr == nil {
						// 先作废认领时起的封面任务，再发任何新的元数据：它带着认领时的歌名、歌手，在下面阻塞的 fetchCoverURL
						// 期间完成即会回写，把补发的值盖回去（#229）。
						coverGen := p.NewSongGen()
						rumSess.bindCoverGen(assocKey.gen, coverGen, true)
						lyricsPending = false
						// 补取到的写法按 song-info-keep-first 取舍，标题恒为认领时的（#229）。
						readName, readSinger := lrcName, lrcSinger
						if readName == "" {
							readName = meta.Name
						}
						if readSinger == "" {
							readSinger = meta.Singer
						}
						prevName, prevSinger := currentName, currentSinger
						currentName, currentSinger = backfillNames(currentName, currentSinger, meta.Name, meta.Singer, lrcName, lrcSinger)
						rumSess.noteSupplement(prevName, prevSinger, readName, readSinger)
						rumSess.updateIDSource(assocKey.gen, qqIDSource(sid, smid, heapMid))
						if readName != prevName || readSinger != prevSinger {
							log.Detail("Song info read after the announcement: %s / %s; sent: %s / %s", readName, readSinger, currentName, currentSinger)
						}
						name, singer, title := currentName, currentSinger, currentTitle
						currentDurationSec = float32(meta.DurationMs) / 1000.0
						p.Emit(player.EventSongInfoUpdate, &player.SongInfo{Name: name, Singer: singer, Title: title})
						// 歌词经状态机（同换歌拍）：在线原始行未套 offset；在线双行无歌词提示按本次读到的歌词标签/内存回退标题识别，
						// 对外标题仍沿用认领时的 currentTitle（#228、#229、#231）。显示的那份不变（如已采纳关联的歌词）时仍以它重发：
						// 时长按本拍读数重算，服务端缓存的 all_lyrics 要跟上。由此每次补取成功都发一次，同接入关联之前。
						lyricSt.setOnlineTitle(readName + " - " + readSinger)
						lyricSt.setOnlineRaw(lines)
						shown, changed := lyricSt.refresh(assocKey.hasIDs(), offsetSec)
						if l, ok := takeResult(&lyricSt, reader.Results(), assocKey.hasIDs(), offsetSec); ok {
							shown, changed = l, true
						} else if l, ok := lyricSt.expireWait(time.Now(), assocKey.hasIDs(), offsetSec); ok {
							shown, changed = l, true
						}
						if !changed {
							shown = currentLyrics
						}
						if len(shown) > 0 {
							log.Info("Lyrics loaded (backfill): %d lines; per-word: %s", len(shown), detailedFlag(shown))
						}
						// 位置取本轮的读数：重读成功时即刚读到的进度；没有重读到则是取词前的读数，上一轮未判
						// 暂停时按经过的时间推算。
						showLyrics(shown, float32(meta.ProgressMs)/1000.0, sampleTime)
						if !lyricsUndecided(shown, lyricsPending, lyricSt.source(), lyricSt.undecided()) {
							rumSess.onTrackLyricSettled(assocKey.gen, lyricSt.source(), shown, false, lyricSt.hint(), true, 0)
						}
						// 本地首轮认定仍未决时不在此处同步阻塞调 fetchCoverURL（超时 10s），改为暂存到 pendingCover，
						// 确保本拍及后续拍的 takeResult / expireWait 在 1s 上界内解除未决（#248）；非未决态立即执行。
						if pendingCover.schedule(lyricSt.undecided(), assocKey.gen, coverGen, sid, smid, name, singer, title) {
							runBackfillCover(coverGen, sid, smid, name, singer, title)
						}
					}
				}
				// 只有重试满 lyricRetryLogAfter 次仍没取到才打一行（快速落位则完全静默）；
				// 成功已把 lyricsPending 置 false，不会误报。
				if lyricsPending && retryCount == lyricRetryLogAfter {
					log.Detail("songID/mid still not ready after %d retries; continuing backfill…", retryCount)
				}
			}
		}

		// 本地首轮认定解除后排空暂缓的补取封面请求（#248）：与补取块同享 !held、!released 与同歌名保护，
		// 避免同步封面请求吃掉暂扣窗或推迟放行拍的跳转补发；核对 gen 防止旧歌延后封面污染新歌。
		if pc, ok := pendingCover.take(released, lyricSt.undecided(), meta.Name == lastName && meta.Name != "", assocKey.gen); ok {
			runBackfillCover(pc.coverGen, pc.sid, pc.smid, pc.name, pc.singer, pc.title)
		}

		// --- 快速计时器锚点更新 + seek 检测 ---
		// 本拍的变化（changed、seek、step）交给其后的暂停与恢复判定；seek 这一拍已发的 playback_resume 记在 resumeEmitted。
		var changed, seek, resumeEmitted bool
		var step int64
		if meta.ProgressMs != lastMemProgress {
			// Seek 检测：进度大幅跳变（> 2s 超出正常播放推进）视为跳转
			// 暂扣放行的那一拍（#202）：读数低于暂扣前的读数即按回跳补发，不再比较 2s 门槛；不低于（从原处接着走，
			// 或向前跳）交给下面的前跳判据，接着走不发事件。lastMemProgress 在暂扣期间没动，仍是暂扣前的读数。
			seekDetected := released && meta.ProgressMs < lastMemProgress
			if lastMemProgress > 0 {
				// 回跳：进度回退超过 2s
				if seekBack(lastMemProgress, meta.ProgressMs) {
					log.Info("Seek-back detected: %.2fs → %.2fs", float32(lastMemProgress)/1000.0, float32(meta.ProgressMs)/1000.0)
					seekDetected = true
				}
				// 前跳：进度前进量超出实际经过时间 2s 以上
				if !seekDetected && meta.ProgressMs > lastMemProgress {
					advance := meta.ProgressMs - lastMemProgress
					elapsed := uint32(sampleTime.Sub(anchorTime).Milliseconds())
					if advance > elapsed+2000 {
						log.Info("forward seek detected: %.2fs → %.2fs", float32(lastMemProgress)/1000.0, float32(meta.ProgressMs)/1000.0)
						seekDetected = true
					}
				}
			}
			if seekDetected {
				lastLineIdx = -1 // 重置歌词行号
				rumSess.notePendingFallback(released && meta.ProgressMs < lastMemProgress, "reopen_r2")
				// 暂扣放行按回跳补发（回到开头 1 秒以内、暂扣期间没有换歌）：无载入边沿或漏采超短载入时的同一首重开兜底
				// （#245，设计 §3.3 规则 R2）；经 shouldReopenByFallback 核对歌名并在占位串拍挂起、由 reopenG.consumed 去重。
				if reopenG.shouldReopenByFallback(released && meta.ProgressMs < lastMemProgress, meta.Name, lastName, assocKey.hasIDs()) && !reopenedThisTick {
					reopenedThisTick = true
					reopenSong(false, meta.ProgressMs)
				}
				// 跳转之前已判暂停的，由判定决定发恢复（v.resume，位置为锚点即落点）还是在落点补发暂停（v.pausedSeek，
				// 设计 §6.7 第 7 条）。
				if !isPaused {
					posSec := float32(meta.ProgressMs) / 1000.0
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
						Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
					})
					resumeEmitted = true
				}
			}
			changed, seek, step = true, seekDetected, int64(meta.ProgressMs)-int64(lastMemProgress)
			anchorProgressMs = meta.ProgressMs
			anchorTime = sampleTime
			lastMemProgress = meta.ProgressMs
		}

		// --- 暂停与恢复判定（#172）---
		// 判定、去抖与两条交叉核对都在 judgeTick 里（回放夹具调用同一个函数）；这里只准备入参、按出参发事件与改锚点。
		// 停走时长量到本轮读数的时刻（见 sampleTime），不量到当前时刻。judge 是循环外声明的跨拍状态，必须用 = 写回：
		// 写成 := 会在循环体里新建一个同名变量遮蔽它，去抖、交叉核对乙与 resumeGate 都无法跨拍累计。
		var v verdict
		judge, v = judgeTick(judge, judgeIn{spec: mem.Offsets().PlayState, raw: meta.PlayState, rawOK: meta.PlayStateOK,
			poll: pollInterval, paused: isPaused, claimed: lastName != "", changed: changed, seek: seek, step: step,
			idle: sampleTime.Sub(anchorTime), now: sampleTime})
		if v.tripped != "" {
			// 交叉核对触发（每会话至多一次）：本次连接其后按停走判定。日志不落盘，野外只有上报能看出哪个版本的偏移不对。
			off, ms := mem.Offsets().PlayState.Off, sampleTime.Sub(anchorTime).Milliseconds()
			if v.tripped == "stalled" {
				log.Warn("QQ Music reports playing (QQMusic.dll+0x%X is %d) but progress has not changed for %dms; play state no longer used for this connection, pausing on progress stall instead",
					off, meta.PlayState, ms)
			} else {
				ms = sampleTime.Sub(judge.movedAt).Milliseconds() // 乙：本段暂停中两次变化的间隔，movedAt 是前一次的时刻
				log.Warn("QQ Music reports paused (QQMusic.dll+0x%X is %d) but progress changed twice within %dms; play state no longer used for this connection, pausing on progress stall instead",
					off, meta.PlayState, ms)
			}
			telemetry.ReportOnce("qqmusic.play_state_mismatch",
				"QQ 音乐播放状态与进度矛盾，本次连接改按进度停走判暂停（附哪一条核对、取值与时长，供核对该版本的偏移）",
				map[string]string{"qqmusic.version": mem.Version()},
				map[string]any{"check": v.tripped, "value": meta.PlayState, "ms": ms})
			rumSess.onPlayStateMismatch(v.tripped)
		}
		if v.flash > 0 {
			// 供真机核对去抖时长的余量（20.05 换歌那一拍的暂停值闪烁）。时长是上界，按拍取样可能大于去抖时长。
			log.Detail("QQ Music play state held its paused value for at most %dms (debounce %dms); not judged as a pause",
				v.flash.Milliseconds(), v.flashLimit.Milliseconds())
		}
		if v.resume {
			// 由标志判恢复时读数多半还没动（恢复之后停 0.6～1.2 s），不重锚 anchorTime 的话插值会把整段暂停的时长加进位置。
			// 位置取锚点：暂停中最后一次读数（含补写）；暂停中跳转而判恢复的，即变化分支刚落的落点。
			isPaused = false
			repausePending = false
			anchorTime = sampleTime
			posSec := float32(anchorProgressMs) / 1000.0
			if !resumeEmitted {
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
					Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
				})
			}
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: currentTitle})
			log.Info("resumed @ %.2fs", posSec)
			if lyricsPending {
				// 补取窗口重设（#227）：窗口在暂停中用尽时补取保持未了结，ID 要开播才落位；下一拍即重试。
				backfill.arm(sampleTime)
			}
			rumSess.notePendingFallback(anchorProgressMs < switchStartBandMs, "reopen_r3")
			// 暂停在开头 1 秒以内重开且漏采超短载入时的同一首重开兜底（#245，设计 §3.3 规则 R3）；
			// 经 shouldReopenByFallback 核对歌名并在占位串拍挂起（防切换新歌时误重开旧歌）、由 reopenG.consumed 去重。
			if reopenG.shouldReopenByFallback(anchorProgressMs < switchStartBandMs, meta.Name, lastName, assocKey.hasIDs()) && !reopenedThisTick {
				reopenedThisTick = true
				reopenSong(false, anchorProgressMs)
			}
		} else if isPaused && changed && !seek && v.st == stateUnknown {
			log.Detail("progress moved %+dms while paused; resume not yet confirmed", step)
		}
		if v.pause {
			// 位置取锚点（最后一次读数）；跳转之前未判暂停而本拍标志为暂停时（如 #202 放行拍），排在 seek 的 playback_resume 之后。
			isPaused = true
			reopenG.onPause()
			posSec := float32(anchorProgressMs) / 1000.0
			p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{
				Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
			})
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: currentTitle})
			log.Info("paused @ %.2fs", posSec)
		}
		if v.pausedSeek {
			// 暂停中跳转而 QQ 仍报暂停：前端停在落点；状态未变，不发 status_update。入队失败由下面的再试接手。
			repausePos = float32(anchorProgressMs) / 1000.0
			repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)
		}
		reopenG.stepProgress(meta.ProgressMs, held, loadOpen.loading)

		// 暂停中补发的 playback_pause 入队失败（通道满）时，仍在暂停中就按原位置每拍再试，直到入队。
		if repausePending && isPaused {
			repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)
		}

		// --- 歌词关联的结果（设计 §6.8「播放中」、#248）---
		// 已认领时每拍经 drainAssoc 非阻塞地取至多一条，无变化时检查首轮本地认定是否满 localDecideWindow，有变化即换词；
		// 不联网、不读文件，撤销关联后退回的在线歌词用本首保存的在线原始行。位置取锚点插值，与下面的行匹配同口径；行号随之重置，
		// 本拍即宣告当前行。
		if lastName != "" {
			drainAssoc(time.Now())
		}

		// --- 本地时钟插值 ---
		// 未判暂停时为锚点加自锚点起经过的时间，暂停中即锚点位置；不超过时长。
		progressSec := player.LivePosition(float32(anchorProgressMs)/1000.0, anchorTime, time.Now(), !isPaused, float32(meta.DurationMs)/1000.0)
		matchTime := player.LyricMatchTime(progressSec, offsetSec)

		// 歌词行匹配。行号只往前走，除非刚换歌、跳转或换了歌词（lastLineIdx 置 -1）：标志未知时暂停要等计时器停滞
		// pauseTimeout 才判得出（有播放状态字段时也要晚到标志翻转那一拍），其间插值照走、可能推过下一行的开头，判出暂停时
		// 位置退回锚点，按它匹配会补发上一行、恢复后再推回来（issue #96）。不足 2s 的回跳不判 seek，同样只重锚、不回退行号。
		if trueIdx, ok := nextLine(currentLyrics, matchTime, lastLineIdx); ok {
			lastLineIdx = trueIdx
			line := currentLyrics[trueIdx]

			// progressSec 是实时插值位置，进 Position、并据它算 Progress；play_time 由 BuildLyricUpdate 按
			// 歌词时间轴算。逐字（TextDetailed）与逐字音译（RomaTextDetailed）的 offset 已在
			// applyDetailedOffset 里套过。
			p.Emit(player.EventLyricUpdate, lineUpdate(trueIdx, line, offsetSec, progressSec, currentDurationSec))
		}

		// 位置采样（见 BaseEmitter.EmitPositionSample）：已认领歌曲且未判暂停时按固定间隔报一次，校准
		// 服务端缓存里的位置——不判暂停的停顿与 2s 以内的跳变只重锚、不发事件，无歌词的歌
		// 又没有歌词行可推。传 progressSec，与上面 lyric_update 的 position 同一个量；offset 只进了
		// matchTime，这里不带。
		if lastName != "" && !isPaused {
			p.EmitPositionSample(time.Now(), progressSec, currentDurationSec)
		}
	}
}

// qqCoverOps 是封面任务用到的外部操作，测试里换成假的。
type qqCoverOps struct {
	download            func(url string) string
	downloadWithOutcome func(url string) (string, player.CoverOutcome)
}

func (o qqCoverOps) fetch(u string) (string, player.CoverOutcome) {
	if o.downloadWithOutcome != nil {
		return o.downloadWithOutcome(u)
	}
	if o.download == nil {
		return "", player.CoverOutcome{Action: player.CoverActionNone}
	}
	start := time.Now()
	b64 := o.download(u)
	durMs := time.Since(start).Milliseconds()
	if b64 == "" {
		return "", player.CoverOutcome{Action: player.CoverActionErr, FallbackReason: "http_err", DurationMs: durMs}
	}
	return b64, player.CoverOutcome{Action: player.CoverActionResized800, DurationMs: durMs}
}

var realQQCoverOps = qqCoverOps{
	download: func(u string) string { return player.FetchCoverBase64("QQMusic", u, 5*time.Second) },
	downloadWithOutcome: func(u string) (string, player.CoverOutcome) {
		return player.FetchCoverBase64WithOutcome("QQMusic", u, 5*time.Second)
	},
}

// startCover 起封面任务：下载 url 的封面，取到后经 EmitForGen 以代次 gen 回写 name、singer、title 与封面。url 为空时
// 不起任务。返回的通道在任务结束时关闭，生产代码不用，测试据此等它收尾。
func (p *QQMusicPlayer) startCover(ops qqCoverOps, gen uint64, url, name, singer, title string) <-chan struct{} {
	done := make(chan struct{})
	if url == "" {
		if s, ok := p.rumGate.Current(); ok && s != nil {
			s.onCoverOutcome(gen, player.CoverOutcome{Action: player.CoverActionNone})
		}
		close(done)
		return done
	}
	go func() {
		defer telemetry.Guard()
		defer close(done)
		b64, out := ops.fetch(url)
		if s, ok := p.rumGate.Current(); ok && s != nil {
			s.onCoverOutcome(gen, out)
		}
		if b64 != "" {
			p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
				Name: name, Singer: singer, Title: title, Cover: url, CoverBase64: b64,
			})
		}
	}()
	return done
}

// backfillNames 返回补取到歌词时发出的歌名与歌手（#229）：读到的值是歌词标签的，为空时取本拍内存的
// （#228 之前的写法），再经 player.SupplementField 与认领时的值取舍。
func backfillNames(claimedName, claimedSinger, memName, memSinger, lrcName, lrcSinger string) (string, string) {
	readName, readSinger := lrcName, lrcSinger
	if readName == "" {
		readName = memName
	}
	if readSinger == "" {
		readSinger = memSinger
	}
	return player.SupplementField(claimedName, readName), player.SupplementField(claimedSinger, readSinger)
}

// rereadSameSong 判断取词之后重读到的元数据能否当作本轮的读数：读成功，歌名仍是 name（已认领的那一首），
// 且相对 prevMs 不构成暂扣的触发（startsHold，#202）。比较与轮询循环的换歌判据一致（只比歌名、逐字相等）；
// 空闲或过渡态漏进来的占位串、空串都与已认领的歌名不同，不采用。
//
// 第三条：取词期间换歌、读数先归零而只出现在重读里时，采用它就绕过了暂扣，变化分支随即判回跳，发上一首的
// playback_resume(0) 与第 0 行。prevMs 由调用方传 lastMemProgress：采用之后变化分支正是拿它比较，口径须一致；
// 以本拍首读为参照会漏掉「首读是带外回跳、重读归零」的情形（doc/design/qqmusic-switch-zero-design.md §4.6）。
// 暂扣放行的那一拍不补取（见补取块），认领拍也不是放行拍，这道检查遇不到放行拍。不采用时沿用取词前的读数，
// 归零由下一拍的暂扣判定处理。
func rereadSameSong(fresh *SongMetadata, err error, name string, prevMs uint32) bool {
	return err == nil && fresh != nil && name != "" && fresh.Name == name && !startsHold(prevMs, fresh.ProgressMs)
}

// takeResult 非阻塞地从读取者的结果通道取至多一条，交给 st.apply（设计 §6.8）；没有结果、results 为 nil（没有读取者）、
// 结果过时或为未知、指纹不变时 changed 为假。换歌拍、补取块与每拍都经它取结果，轮询线程上对结果通道的接收只在这里
// （assocwiring_test.go 守）。来源因此变化时打一条 Info。
func takeResult(st *lyricState, results <-chan assocResult, songHasIDs bool, offsetSec float32) (lines []lyricLine, changed bool) {
	select {
	case res := <-results:
		prev := st.source()
		if lines, changed = st.apply(res, songHasIDs, offsetSec); changed && st.source() != prev {
			log.Info("Lyrics source changed to %s: %d lines", st.source(), len(lines))
		}
	default:
	}
	return lines, changed
}

// lyricsUndecided 报告要显示的 lines 是否还没有结论：行为空、来源为在线，且编号补取未完（pending）或仍在等本首首轮本地
// 文件认定（localUndecided，#248）。此时不发 index:-1（下游读作没有歌词），等补取或首轮认定落定后再发歌词或无歌词。
func lyricsUndecided(lines []lyricLine, pending bool, src lyricSource, localUndecided bool) bool {
	return len(lines) == 0 && src == srcOnline && (pending || localUndecided)
}

// emitFrozenPause 在 posSec 发 playback_pause，返回是否入队（通道满时不阻塞，返回假）。runSession 在已判暂停时随
// all_lyrics 之后发它，posSec 取那条 all_lyrics 的位置；暂停中跳转而 QQ 仍报暂停时也发它，posSec 取落点。入队失败由调用方
// 记下、每拍再试，所以用 TryEmit 而不是会静默丢弃的 Emit。暂停中补发只经它，调用处有三处：showLyrics 的补发、暂停中跳转
// （if v.pausedSeek）与入队失败的再试（playstate_test.go 的接线门禁 d、g、m 守）。
func (p *QQMusicPlayer) emitFrozenPause(posSec, durationSec float32) bool {
	return p.TryEmit(player.EventPlaybackPause, &player.PlaybackTimeInfo{
		Position: posSec, Progress: player.ClampProgress(posSec, durationSec),
	})
}

// switchStartBandMs、switchConfirmAfter 管换歌前进度归零的暂扣（#202，doc/design/qqmusic-switch-zero-design.md
// §4）。QQ 音乐 22.81 换歌时进度读数先归零，歌名晚 31～249 ms 才换成新歌（2026-11-14 录音 ws/30-qqmusic-172，
// 10 次换歌 8 次，归零的读数都为 0）。只看读数，这一拍与拖回开头、单曲循环回到开头分不开，故落在开头附近的回跳
// 先暂扣，switchConfirmAfter 内认领了新歌即丢弃，期满仍未换歌才补发。
//
// switchStartBandMs：录音里的归零读数都为 0；1 s 给未见过的情形留余量，与 kuwo 同值。带宽只决定哪些真实的回跳
// 要等，读数恰为它时不暂扣。switchConfirmAfter：歌名晚于归零至多约 280 ms（最长一例 249 ms 加一拍，推断），
// 留约 120 ms 余量；代价是拖到开头 1 s 以内与单曲循环回到开头的跳转晚 400 ms 到 400 ms 加一拍。项目负责人决定
// 维持 400 ms，不为较慢的机器放宽（设计 §10）。
const (
	switchStartBandMs  uint32 = 1000
	switchConfirmAfter        = 400 * time.Millisecond
)

// seekBack 判读数相对上一次读数是否为回跳：回退超过 2 s。runSession 的回跳检测与暂扣的触发共用这一处。
func seekBack(prevMs, ms uint32) bool { return ms+2000 < prevMs }

// startsHold 判读数是否开始一次暂扣：相对 prevMs 构成回跳，且落在开头 switchStartBandMs 以内（不含端点）。
// 暂扣的触发与取词之后重读的采用（rereadSameSong）共用。
func startsHold(prevMs, ms uint32) bool { return seekBack(prevMs, ms) && ms < switchStartBandMs }

// switchHold 是换歌前进度归零的暂扣（#202）。since 为零值即没有暂扣。
type switchHold struct {
	since        time.Time // 暂扣起点：首个暂扣拍的读数时刻
	fromMs, toMs uint32    // 暂扣前的读数、首个暂扣拍的读数（日志用）
}

// step 在每拍换歌分支之后、补取之前调用：prevMs 为上一次读数（lastMemProgress），ms 为本拍读数，now 为本拍读数
// 的时刻。没有暂扣时 startsHold 成立即开始暂扣；暂扣中各拍一律暂扣、不再看读数，直到距起点不少于
// switchConfirmAfter 那一拍放行。held 为真时调用方跳过本拍其余判定；released 为真时调用方以本拍读数与 prevMs
// 判定补发（低于即按回跳补发，不低于交给前跳判据）。认领新歌时调用方先调 drop。
func (h *switchHold) step(prevMs, ms uint32, now time.Time) (held, released bool) {
	if h.since.IsZero() {
		if !startsHold(prevMs, ms) {
			return false, false
		}
		*h = switchHold{since: now, fromMs: prevMs, toMs: ms}
	}
	if now.Sub(h.since) < switchConfirmAfter {
		return true, false
	}
	heldMs := now.Sub(h.since).Milliseconds()
	log.Detail("progress went back at %s from %.2fs to %.2fs; held %dms, released (now %.2fs): no song change",
		h.since.Format("15:04:05.000"), float32(h.fromMs)/1000.0, float32(h.toMs)/1000.0,
		heldMs, float32(ms)/1000.0)
	emitQQSwitchHoldGuard("released", heldMs)
	*h = switchHold{}
	return false, true
}

// drop 在认领新歌时调用：暂扣中的回跳是换歌造成的，不发。日志的暂扣时长即归零到读到新歌名的时长（精度一拍），
// 供真机复测核对 switchConfirmAfter 的余量。
func (h *switchHold) drop(now time.Time) {
	if h.since.IsZero() {
		return
	}
	heldMs := now.Sub(h.since).Milliseconds()
	log.Detail("progress went back at %s from %.2fs to %.2fs; held %dms, discarded: song changed",
		h.since.Format("15:04:05.000"), float32(h.fromMs)/1000.0, float32(h.toMs)/1000.0,
		heldMs)
	emitQQSwitchHoldGuard("intercepted", heldMs)
	*h = switchHold{}
}

// resumeStepMs：已判暂停时，读数单次前进不小于它即判恢复。实测暂停后的补写最大 116ms、稳定播放单步最大
// 371ms，恢复首步最小 859ms（#171）。
const resumeStepMs = 500

// resumeGate 判定已判暂停时、读数的非 seek 变化是否为恢复（#171，doc/design/qqmusic-pause-tail-design.md §3）。
// QQ 音乐暂停后进度先停 1.85–3.0s，再补写一次 +93～116ms 才停住；按「一变即恢复」会把补写判成恢复、1s 后
// 再判暂停。小步先记为待确认，window 内再变一次才判恢复。
type resumeGate struct {
	pendingAt time.Time // 待确认变化的读数时刻；零值为没有
}

// observe 在已判暂停、读数变化且不是 seek 时调用：step 为本次读数减上次读数（ms，有符号），now 为本次读数的
// 时刻。返回 true 即判恢复，并清空待确认。
func (g *resumeGate) observe(step int64, now time.Time, window time.Duration) bool {
	if step >= resumeStepMs || (!g.pendingAt.IsZero() && now.Sub(g.pendingAt) <= window) {
		g.pendingAt = time.Time{}
		return true
	}
	g.pendingAt = now
	return false
}

func (g *resumeGate) reset() { g.pendingAt = time.Time{} }

// --- 暂停与恢复判定（#172，doc/design/qqmusic-start-stall-design.md §6）---
//
// QQ 音乐在换歌、拖动、单曲循环回到开头与恢复播放之后，进度读数先停约 1～2 s 才走，其间 QQ 自己的播放状态一直是
// 在播（2026-11-22 qqstateprobe 实测：22.82 停走最长按拍量 1.4 s，22.52 1.6 s，20.05 1.4 s；2026-11-23 第 5 轮另录的
// 9 个版本最长 2.0 s，其中 A 类版本偶尔夹一拍载入值）；按停走 1 s 判暂停会在用户没有暂停时多出一对暂停与恢复。
// 收录了播放状态字段（knownVersions 的 PlayState）、版本号精确匹配、实际轮询间隔不超过 1 s 的会话改按 QQ 的播放状态判；
// 其余情形（未收录、借用偏移表、读失败、取值不在集合里、交叉核对判为不可信、轮询超过 1 s）按停走 1 s 判暂停、经
// resumeGate 判恢复，逐事件同修复之前。判定、去抖与两条交叉核对只在 judgeTick 里写，runSession 与回放夹具都只调用它
// （runSession 的接线由 playstate_test.go 的源码扫描守）。

// playState 是一拍采用的播放状态（#172）。
type playState uint8

const (
	stateUnknown  playState = iota // 不用标志，按停走判定
	statePlaying                   // 在播值
	statePaused                    // 暂停值，已满去抖
	stateSettling                  // 暂停值，未满去抖：本拍不判暂停、不判恢复（seek 除外），不按停走判，不做核对
	stateLoading                   // 载入值（classify 的结果）：judgeTick 随即改按未知，停走自进入载入值的那一拍起量
)

const (
	// pauseTimeout：标志未知时，停走严格超过它判暂停（修复之前的 pauseTimeoutSec）；也是 resumeGate 的确认窗口。
	pauseTimeout = time.Second
	// playStateStallLimit：交叉核对甲的门槛。标志报在播而读数停走严格超过它，判本会话标志不可信。实测标志为在播时
	// 停走最长按拍量 2.0 s、实际至多约 2.2 s（12 个版本的录像），22.81 录音锚点到首步最长 2072 ms，留 0.8 s 以上；与
	// applemusic 的 startPauseThreshold 同值。标志正常时它不是暂停判定的宽限，只在偏移出错或 QQ 报在播而真停走时起作用。
	playStateStallLimit = 3 * time.Second
)

// judgeState 是判定跨拍保留的状态（runSession 的局部变量，本会话内有效，QQ 重启或本服务重连即重置）。
type judgeState struct {
	bad           bool       // 本会话已判标志不可信（§6.3）
	movedAt       time.Time  // 交叉核对乙：本段暂停中上一次非 seek 变化的读数时刻
	pausedSince   time.Time  // 去抖：连续读到暂停值的第一拍的读数时刻，零值为没有
	confirmed     bool       // 去抖：这一段暂停值已满 MinPause
	flashFrom     time.Time  // 去抖诊断：这一段暂停值开始时的 lastPlayingAt，零值为之前没有读到过在播值
	lastPlayingAt time.Time  // 最近一次读到在播值（分类之后 st 为在播）的读数时刻，零值为没有
	loadingSince  time.Time  // 连续读到载入值的第一拍的读数时刻，零值为没有
	resume        resumeGate // #171：标志未知的拍判恢复用
}

// judgeIn 是一拍的入参，由调用方在变化分支（seek 检测与重锚）之后准备。
type judgeIn struct {
	spec    playStateSpec // 本版本的字段描述（mem.Offsets().PlayState）
	raw     uint32        // 本拍标志原始取值（meta.PlayState，重读被采用时已换）
	rawOK   bool          // meta.PlayStateOK：读了且读成功
	poll    time.Duration // 实际轮询间隔（pollInterval）
	paused  bool          // 本拍判定之前是否已判暂停（isPaused）
	claimed bool          // lastName != ""
	changed bool          // 本拍读数与上一次不同
	seek    bool          // 变化分支判出 seek
	step    int64         // 本拍读数减上一次读数（ms，有符号）
	idle    time.Duration // sampleTime − anchorTime，在变化分支重锚之后量（读数变了即为 0）
	now     time.Time     // sampleTime
}

// verdict 是一拍的出参。
type verdict struct {
	st         playState     // 本拍采用的状态（日志用）
	resume     bool          // 判恢复
	pause      bool          // 判暂停
	pausedSeek bool          // 已判暂停、本拍判出 seek、状态为暂停（已满去抖）：不判恢复也不判暂停，runSession 在落点补发 playback_pause（不发 status_update）
	tripped    string        // 本拍触发的交叉核对："stalled"（甲）、"moved"（乙），没有为空
	flash      time.Duration // 一段未满去抖就结束的暂停值的持续时长上界（Detail 用），没有为 0；可能大于 flashLimit
	flashLimit time.Duration // flash 非 0 时为本版本的去抖时长（Detail 用：runSession 不读 MinPause）
}

// judgeTick 判一拍的暂停与恢复（§6.2）。
//
//   - 状态：本会话未判不可信、读成功、实际轮询间隔不超过 1 s 时按取值分类，否则为未知。
//   - 载入（各版 Loading 里的取值：E 类的 2，A 类的 2、3；换歌载入中，QQ 启动后未开播时一直是 2）：按未知判，判暂停
//     用的停走取自锚点与自进入载入值的那一拍两者的较短者；resumeGate 照旧，不做甲、乙。只自锚点量的话，起步停顿中又
//     换歌（占位串那一拍 0→0 不暂扣、不认领、不重锚）会把上一首的停顿算进来而误判；按在播的话，QQ 启动后未开播 3 s
//     即触发甲（设计 §6.1）。开播值（Opening，A 类的 3）每拍重新起量，即读到它的拍不按停走判暂停（#243）。
//   - 去抖：暂停值自读到它的那一拍起持续不少于 MinPause 才算暂停，未满为待定；恢复不去抖（录像只见过换歌时闪暂停值）。
//     未满就结束的一段报出持续时长的上界 flash：自这一段之前最近一次读到在播值的拍，到结束这一段的在播值的拍；两端
//     任一不是有效的在播读数（会话里此前没读到过在播值、以读失败或取值集合以外的值结束）时不报，判定照旧。
//   - 交叉核对乙（报暂停却在走）：已判暂停、状态为暂停时，读数的非 seek 变化距本段暂停中上一次这样的变化不超过
//     movedWindow，判不可信。真暂停中三份录像与 #171 的采样都至多补写一次。
//   - 交叉核对甲（报在播却停走）：本拍之前未判暂停、状态为在播、已认领，停走严格超过 playStateStallLimit，判不可信。
//   - 暂停中跳转（第 5 轮，设计 §6.7 第 7 条）：已判暂停、seek、状态为暂停时不判恢复也不判暂停，出参 pausedSeek；本段暂停
//     自跳转起重新计，清 movedAt 与 resumeGate 的待确认。
//   - 判恢复：已判暂停时，seek（暂停中跳转除外）、状态为在播、或状态未知而 resumeGate 确认（同 #171）。
//   - 判暂停：未判暂停（含本拍刚判恢复）、已认领时，状态为暂停，或状态未知而停走严格超过 pauseTimeout（同修复之前；
//     载入的停走见上）。
//
// 触发核对的那一拍状态即改为未知：甲那一拍停走已超过 1 s，当拍判暂停；乙那一拍经 resumeGate。
func judgeTick(s judgeState, in judgeIn) (judgeState, verdict) {
	var v verdict
	if !s.bad && in.rawOK && in.poll <= time.Second {
		v.st = in.spec.classify(in.raw)
	}
	// 载入：其后各处按未知；只有判暂停用的停走改量（idle），resumeGate、甲与乙不涉及
	idle := in.idle
	if v.st == stateLoading {
		// 开播值（#243）每拍重新起量：本拍停走为 0，其后转为别的载入值时自最后一拍开播值起量
		if s.loadingSince.IsZero() || slices.Contains(in.spec.Opening, in.raw) {
			s.loadingSince = in.now
		}
		v.st, idle = stateUnknown, min(idle, in.now.Sub(s.loadingSince))
	} else {
		s.loadingSince = time.Time{}
	}
	// 去抖：暂停值须持续不少于 MinPause；未满为待定，本拍不判暂停也不判恢复（seek 除外），不做核对
	if v.st == statePaused {
		if s.pausedSince.IsZero() {
			// 暂停值出现在最近一次读到在播值之后：持续时长的上界自那一拍起算。读失败、取值集合以外的拍不算（暂停值可能
			// 早于它们出现）；中间有不调判定函数的拍（#202 暂扣、#96 与 midWait 等待）时那一拍更早，上界只会放宽。
			s.pausedSince, s.flashFrom = in.now, s.lastPlayingAt
		}
		if in.now.Sub(s.pausedSince) >= in.spec.MinPause {
			s.confirmed = true
		} else {
			v.st = stateSettling
		}
	} else if !s.pausedSince.IsZero() {
		// 只在这一段以在播值结束、且开始之前读到过在播值时报：以读失败或未知值结束的，暂停值何时结束不知道。
		if !s.confirmed && v.st == statePlaying && !s.flashFrom.IsZero() {
			v.flash, v.flashLimit = in.now.Sub(s.flashFrom), in.spec.MinPause
		}
		s.pausedSince, s.confirmed = time.Time{}, false
	}
	if v.st == statePlaying {
		s.lastPlayingAt = in.now
	}
	// 乙：报暂停却在走
	if v.st == statePaused && in.paused && in.changed && !in.seek {
		if !s.movedAt.IsZero() && in.now.Sub(s.movedAt) <= movedWindow(in.poll) {
			s.bad, v.tripped, v.st = true, "moved", stateUnknown
		} else {
			s.movedAt = in.now
		}
	}
	paused := in.paused
	if paused && in.seek && v.st == statePaused {
		// 暂停中跳转而 QQ 仍报暂停（读数先跳、标志后变，或拖动后不起播）：不判恢复，落点由补发的 playback_pause 带出；
		// 本段暂停自跳转起重新计：清 movedAt（乙）与 resumeGate 的待确认，同原先「判恢复、随即再判暂停」两处各清一次的
		// 效果。不清后者时，跳转前一次标志未知的小步与跳转后又一次凑成两次而误判恢复（设计 §6.7 第 7 条，§7.2 J25）
		v.pausedSeek = true
		s.movedAt = time.Time{}
		s.resume.reset()
	} else if paused && (in.changed && (in.seek || v.st == stateUnknown && s.resume.observe(in.step, in.now, pauseTimeout)) ||
		v.st == statePlaying) {
		v.resume, paused = true, false
		s.resume.reset()
	}
	// 甲：报在播却停走。本拍之前已判暂停的不量：由标志判恢复时锚点随即重置
	if v.st == statePlaying && !in.paused && in.claimed && in.idle > playStateStallLimit {
		s.bad, v.tripped, v.st = true, "stalled", stateUnknown
	}
	if !paused && in.claimed && (v.st == statePaused || v.st == stateUnknown && idle > pauseTimeout) {
		v.pause = true
		s.resume.reset()
		s.movedAt = time.Time{}
	}
	return s, v
}

// loadingUsable 报告本会话的载入信号是否可用（#218，设计 §6.8「重开」；#245，增补设计 §3.3）：本版本的播放状态有载入取值、
// 本会话未判标志不可信（bad 即 judgeState.bad）、实际轮询间隔不超过 1 s。可用时同一首重开启用 R1 载入边沿及占位串挂起
// （loadEdge、reopenGuard.shouldReopenByLoading），与 R2/R3 兜底互补；runSession 每拍开头算一次，三处 R1 共用它。
func (s playStateSpec) loadingUsable(bad bool, poll time.Duration) bool {
	return len(s.Loading) > 0 && !bad && poll <= time.Second
}

// loadEdge 记播放状态的载入值（#218，设计 §6.8「重开」「改名证据」「载入段的记录」），runSession 的局部变量，由 newLoadEdge
// 建出：察觉载入值由无到有（同一首重开）；另记自上一次换歌认领以来的窗口（改名证据，claim 求值并清零）与每段载入值的四个时刻
// （段结束时经 onSegment 报出）。窗口与载入段的其余方法在 loadwindow.go。只做内存比较。
type loadEdge struct {
	loading   bool              // 上一次读数为载入值（重开的由无到有，读失败按非载入）
	poll      time.Duration     // 会话的轮询间隔：载入段的采样缺口为相邻读成的读数相隔超过 2×poll
	onSegment func(loadSegment) // 载入段结束时调用；nil 即不报
	// 改名证据的窗口（自上一次换歌认领以来，claim 清零）：见到载入边沿、有读不成的拍、相邻读数相隔不小于门槛 G
	sawEdge, sawFail, sawGap bool
	maxGap                   time.Duration
	lastAt                   time.Time // 上一次读成的播放状态读数的时刻；跨认领保留
	// 载入段
	inSeg               bool
	seg                 loadSegment
	preAt               time.Time // 最近一个读成的非载入样本
	failSince, gapSince bool      // 自上一个读成的样本以来有读失败、有采样缺口
}

// step 在每拍读到元数据之后以首读调用一次（暂扣中的拍、等进度与等 songMid 的拍也调用），取词之后的重读被采用时再以重读调用，
// at 为该次读数的时刻（首读为 sampleTime，重读为重读之后的 sampleTime）：这次读数是否为载入值经 judgeTick 所用的 classify
// 判定，读失败（rawOK 为假）按非载入值；返回是否由非载入值变为载入值。
// 另记改名证据的窗口与载入段（设计 §6.8「改名证据」「载入段的记录」）：返回真即见到载入边沿；读失败记一次读不成，不改 lastAt、
// 不动载入段（状态未知，不算非载入样本，不结束一段）；读成时量与上一次读成的读数的间隔（不小于门槛 G 即采样缺口，超过两倍轮询
// 间隔记进载入段），再按本次是否为载入值开段、续段或结束一段。
func (e *loadEdge) step(spec playStateSpec, raw uint32, rawOK bool, at time.Time) bool {
	loading := rawOK && spec.classify(raw) == stateLoading
	edge := loading && !e.loading
	e.loading = loading
	if edge {
		e.sawEdge = true
	}
	if !rawOK {
		e.sawFail, e.failSince = true, true
		return edge
	}
	if !e.lastAt.IsZero() {
		d := at.Sub(e.lastAt)
		e.maxGap = max(e.maxGap, d)
		if spec.LoadingMinGap > 0 && d >= spec.LoadingMinGap {
			e.sawGap = true
		}
		if e.poll > 0 && d > 2*e.poll {
			e.gapSince = true
		}
	}
	e.lastAt = at
	switch {
	case loading && !e.inSeg: // 开段：段前的读失败与缺口落在 t₀～t₁ 之间
		e.inSeg = true
		e.seg = loadSegment{pre: e.preAt, first: at, last: at, fail: e.failSince, gap: e.gapSince}
	case loading: // 续段：其间的读失败与缺口落在 t₁～t₂ 之间
		e.seg.failInside = e.seg.failInside || e.failSince
		e.seg.gapInside = e.seg.gapInside || e.gapSince
		e.seg.last = at
	case e.inSeg: // 段结束：落在 t₂～t₃ 之间
		e.seg.post = at
		e.seg.fail = e.seg.fail || e.failSince
		e.seg.gap = e.seg.gap || e.gapSince
		if e.onSegment != nil {
			e.onSegment(e.seg)
		}
		e.inSeg = false
		e.preAt = at
	default:
		e.preAt = at
	}
	e.failSince, e.gapSince = false, false
	return edge
}

// movedWindow 是交叉核对乙的窗口：1 s 与两倍轮询间隔的较大者（§6.3）。要盖住稳定播放中相邻两次读到的变化（默认轮询
// 下至多约 0.6～0.8 s，有一拍延迟时到两倍轮询）；轮询超过 1 s 时窗口会盖过暂停中补写的间隔，那时不用标志。
func movedWindow(poll time.Duration) time.Duration { return max(time.Second, 2*poll) }

// shouldDeferForSongMid 判断本次换歌是否**可能**需要等 songMid 就绪。
//
// usesSongMid 表示「本版本靠 songMid 取词」：v20.05（偏移表填了 SongMidParamsOff，从 URL
// 参数异步提取）与 v22.41（SongMidFromHeap，从堆 JSON 按时长扫出）都为 true。这两种
// songMid 在换歌瞬间都可能还没就绪，需要短暂等待重试。
//
// 反过来，靠数字 songID 取词的版本（22.16/22.22，usesSongMid==false）songMid 恒空但
// **绝不该等**——否则会永远等一个不会出现的 songMid，整版本加载不了歌词。这个
// usesSongMid 前提是本判定的要害，不能只看 songMid 是否为空。
//
// 注意它只说「可能要等」，等多久由 midWaitGate 决定——songMid 也可能**持久**为空。
func shouldDeferForSongMid(usesSongMid bool, songMid string) bool {
	return usesSongMid && songMid == ""
}

// midWaitWindow 是 v20.05 等 songMid 就绪的时间上界。用时间窗而非重试次数，因为 poll
// 间隔可由 config 逐播放器覆盖（per-player poll），按次数会让慢 poll 等上几十秒。
const midWaitWindow = 500 * time.Millisecond

// midWaitGate 管理「换歌时等 songMid」的**有界**等待。
//
// 为什么必须有界：songMid 未必是「还没提取到」，也可能**永远提不到**——本地文件播放时
// 流 URL 是本地路径，mem.go 的两条提取路径（basename 10-20 字节校验 / 参数串含 "0="）
// 都不成立，meta.SongMid 恒空。此时无界等待 = 这首歌一个事件都不发，overlay 整首停在
// 上一首，正是本该修掉的那个症状换了个触发源。等窗用尽就认账：认领这首歌、按「无歌词」
// 发标题，至少让主播看见在放什么。
type midWaitGate struct {
	name     string
	deadline time.Time
}

// wait 登记对 songName 的等待。started 表示这是该歌的第一次等待（调用方据此清掉上一首
// 的歌词，停止错滚）；keepWaiting 表示等窗未尽、应继续等下一轮。
func (g *midWaitGate) wait(songName string, now time.Time, window time.Duration) (started, keepWaiting bool) {
	if g.name != songName {
		g.name = songName
		g.deadline = now.Add(window)
		started = true
	}
	return started, now.Before(g.deadline)
}

// reset 在 songMid 就绪（或放弃等待）后清空，使下一首歌重新开窗。
func (g *midWaitGate) reset() { g.name = "" }

// lyricRetryWindow、lyricRetryInterval 是换歌后歌词补取的窗口与重试间隔：认领时歌曲 ID 未落位即开窗，窗口内每
// lyricRetryInterval 重试一次（重试含 FindSongMid 全内存扫描与 fetchLRC 网络请求）。
const (
	lyricRetryWindow   = 8 * time.Second
	lyricRetryInterval = 500 * time.Millisecond
)

// backfillWindow 是换歌后歌词补取的窗口与重试节流（#227，doc/design/qqmusic-start-stall-design.md §6.10），runSession 的
// 局部变量。截止与节流的比较只在 next 里：只把窗口判断写成纯函数的话，调用方在「保持」时照样会进重试分支。
type backfillWindow struct {
	deadline  time.Time // 窗口截止
	lastRetry time.Time // 上一次重试的时刻，零值为还没有
}

// backfillStep 是 next 对本拍的裁定。
type backfillStep uint8

const (
	backfillWait   backfillStep = iota // 窗口内、未到重试间隔：本拍不做
	backfillRetry                      // 窗口内、已到重试间隔：本拍重试（next 已记下 lastRetry）
	backfillHold                       // 窗口已用尽、已判暂停：不重试、不放弃
	backfillGiveUp                     // 窗口已用尽、未判暂停：放弃
)

// arm 开（或重开）窗口：截止为 now + lyricRetryWindow，清掉 lastRetry，下一次 next 即可重试。认领时歌曲 ID 未落位、
// 以及补取未了结而判恢复时调用（#227：QQ 启动后载入上次的歌而未开播，ID 要开播才落位）。
func (w *backfillWindow) arm(now time.Time) {
	*w = backfillWindow{deadline: now.Add(lyricRetryWindow)}
}

// next 裁定本拍的补取：now 不晚于截止（含端点）时，距上一次重试不少于 lyricRetryInterval 即重试并记下 now，否则等；
// 晚于截止时，paused（本拍判定之前的 isPaused）为真即保持，为假即放弃。窗口内暂停中照样重试：换歌后立刻暂停时 ID 在
// 换歌之后 1.1～3.1 s 落位（2026-11-22 真机 8 次），暂停中补到歌词与封面是一直有的行为；只在用尽之后才看是否暂停。
func (w *backfillWindow) next(now time.Time, paused bool) backfillStep {
	switch {
	case now.After(w.deadline) && paused:
		return backfillHold
	case now.After(w.deadline):
		return backfillGiveUp
	case now.Sub(w.lastRetry) < lyricRetryInterval: // 零值的 lastRetry 相距极远，首次即满足
		return backfillWait
	}
	w.lastRetry = now
	return backfillRetry
}

// backfillCoverGate 管理本地首轮认定未决时暂缓的补取封面请求（#248）：
// 补取返回 0 行且本地首轮认定尚未落定时，同步 fetchCoverURL（10s 超时）会阻塞轮询线程，推迟 takeResult / expireWait
// 解除未决态突破 1s 硬上界；暂存至此，待未决态解除后的下一非暂扣非放行拍再执行。
type backfillCoverGate struct {
	armed    bool
	gen      uint64
	coverGen uint64
	sid      uint32
	smid     string
	name     string
	singer   string
	title    string
}

// schedule 在补取成功经 showLyrics 发出后调用：undecided 为真时暂存参数并返回 false（本拍不阻塞请求封面）；
// 否则清空暂存并返回 true（立即请求封面）。
func (g *backfillCoverGate) schedule(undecided bool, gen, coverGen uint64, sid uint32, smid, name, singer, title string) bool {
	if undecided {
		*g = backfillCoverGate{armed: true, gen: gen, coverGen: coverGen, sid: sid, smid: smid, name: name, singer: singer, title: title}
		return false
	}
	*g = backfillCoverGate{}
	return true
}

// take 在非放行、已解除未决且仍为已认领曲目的拍取出暂存封面并清空：仅当代次 gen 等于 curGen 时返回 true，否则丢弃。
func (g *backfillCoverGate) take(released, undecided, sameSong bool, curGen uint64) (backfillCoverGate, bool) {
	if !g.armed || released || undecided || !sameSong {
		return backfillCoverGate{}, false
	}
	pc := *g
	*g = backfillCoverGate{}
	if pc.gen != curGen {
		return backfillCoverGate{}, false
	}
	return pc, true
}

// reset 在换歌认领时清空暂存，防止旧歌延后封面污染新歌。
func (g *backfillCoverGate) reset() { *g = backfillCoverGate{} }

// progressSettleWindow 是换歌后等进度从上一首换过来的时间上界。取值未经真机测量：录音只说明进度落后于
// 歌名不超过一次取词加一拍。真机上若换歌常等满这个时长、或等满之后读到的仍是上一首的进度，按实测调整。
const progressSettleWindow = 2 * time.Second

// carriedOverFloorMs 以内的读数不必等：按它匹配，落到的至多是新歌开头约 2 秒内起唱的行；取值同 seek 检测的
// 门槛。没有它，上一首几乎没走（读数在 0 附近）时新歌的读数总不低于它，这时换歌都要等满等窗。
const carriedOverFloorMs = 2000

// progressCarriedOver 判断换歌时读到的进度是否还是上一首的：本会话已认领过一首（prevName 非空），读数不低于
// 上一首最后一次读数 prevMs（在播时计时器只往前走，暂停时停着），且超过 carriedOverFloorMs。新歌从头播起时
// 读数归零、低于 prevMs。新歌从上次的位置续播、续播点又不早于上一首的位置时同样判为没换过来，要等满
// progressSettleWindow 才认领。
func progressCarriedOver(prevName string, prevMs, ms uint32) bool {
	return prevName != "" && ms >= prevMs && ms > carriedOverFloorMs
}

// nextLine 返回匹配时刻 t 所在的行（最后一个起点不晚于 t 的行；还没到第一行为 -1），ok 表示要为它发
// lyric_update：行号只往前走，last 为 -1 时不限。同 applemusic 的 nextLine。
func nextLine(lines []lyricLine, t float32, last int) (idx int, ok bool) {
	idx = -1
	for i := len(lines) - 1; i >= 0; i-- {
		if t >= lines[i].Time {
			idx = i
			break
		}
	}
	return idx, idx > last
}

// emitNoLyrics 发「这首歌没有歌词」：空的 all_lyrics 加 index:-1 的 lyric_update（平台给出无歌词提示语时
// 原样保留进 Text，无提示语时为空串，#231）。取词成功但零行（纯音乐）的两处——换歌时、歌曲 ID 晚到的补取
// （经 showLyrics，与歌词关联结果共用）——共用它，两处发出的事件必须一致。lyrics 用显式空 slice（nil 会
// 序列化成 null）；index:-1 的 lyric_update 必须带 Position 与 Progress（lyricupdate_lint 全仓强制）。
func (p *QQMusicPlayer) emitNoLyrics(title string, durationSec, positionSec, progress float32, hint string) {
	p.Emit(player.EventAllLyrics, &player.AllLyricsData{
		Title: title, Duration: durationSec,
		Position: positionSec,
		Progress: progress,
		Lyrics:   []player.LyricLine{}, Count: 0,
	})
	p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
		Index: -1, Text: hint, SubText: "", Timestamp: 0,
		Position: positionSec, Progress: progress,
	})
}

// toLyricLines converts internal lyricLine to player.LyricLine
// 逐字音译（RomaDetailed）进 roma_text_detailed、振假名（Furigana）进 furigana：BuildLyricLine 不填它们，这里赋值。
func toLyricLines(lines []lyricLine, offsetSec float32) []player.LyricLine {
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec)
		out[i].RomaTextDetailed = l.RomaDetailed
		out[i].Furigana = l.Furigana
	}
	return out
}

// lineUpdate 把一行歌词映射成 lyric_update 的载荷，字段口径同 toLyricLines：两者取自同一份
// currentLyrics，同一行的逐字音译与振假名在 all_lyrics 与 lyric_update 里相同。
func lineUpdate(index int, l lyricLine, offsetSec, playPos, duration float32) *player.LyricUpdate {
	u := player.BuildLyricUpdate(index, l.Time, l.Text, l.SubText, l.RomaText, l.Detailed, offsetSec, playPos, duration)
	u.RomaTextDetailed = l.RomaDetailed
	u.Furigana = l.Furigana
	return u
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time。
//
// **必须在 fetch 之后、存进 currentLyrics 之前做一次**：BuildLyricLine 只调行级 play_time，
// TextDetailed 是原样透传的（对照 cloudmusic 的 applyTextDetailedOffset）。漏了这步，逐字
// 高亮会整体偏 offset 那么多；而 lyric_update 直接用 currentLyrics 里的 TextDetailed，
// 不在这儿套就没有第二次机会。
func applyDetailedOffset(lines []lyricLine, offsetSec float32) {
	for i := range lines {
		// 主歌词的逐字与音译的逐字各是一条时间轴（解析时都没填 PlayTime），都要套。
		for _, d := range []*player.LyricTextDetailed{&lines[i].Detailed, &lines[i].RomaDetailed} {
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

// detailedFlag 报告这批歌词**实际**有没有逐字。
//
// 绝不写死——AGENTS.md §6.1：写死的「逐字：否」是断言不是报告，接通逐字的人会据此以为
// 自己没做成、回去改一个本来就对的实现。QQ 音乐的逐字只在 QRC 源（musicu.fcg + qrc:1）
// 上有；songMid 兜底走的 fcg_query_lyric_new.fcg 只回行级 LRC，那时这里如实报「否」。
func detailedFlag(lines []lyricLine) string {
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}
