package qqmusic

import (
	"errors"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

const PlayerName = "qqmusic"

// errSongMidUnavailable 表示 v20.05 上 songMid 等窗已尽仍为空（多半是本地文件播放）。
// 走 err 分支即可——那条路径会发标题 + 空歌词，是这种情况下唯一诚实的降级。
var errSongMidUnavailable = errors.New("songMid 不可用（可能是本地文件播放）")

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

		p.InvalidateSongGen() // 作废在飞的封面回写，别让它在 ClearSongData 之后复活已清空的 SongInfo
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: "QQ音乐已退出"})
		p.Emit(player.EventClearSongData, nil)
		log.Info("会话结束，等待新的 QQMusic 进程...")
		time.Sleep(2 * time.Second)
	}
}

func (p *QQMusicPlayer) runSession(mem *QQMusicMem, offsetSec float32) {
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
	// 恢复：取消下面三行注释即可。InjectSliderAOB 及其逆向实现原样保留在 mem.go。
	// 关闭期间 m.sliderPointer 保持 0，mem.go 的 `if m.sliderPointer != 0` 守卫会让
	// sliderVal 停在 0，无其他副作用。
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
	var midWait midWaitGate // v20.05 等 songMid 就绪的有界等待
	var lastLineIdx int = -1
	var currentDurationSec float32 = 0
	var isPaused bool = false
	var currentTitle string

	// 换歌后歌词补取：songID/songMid 落位可能滞后于显示结构体几秒（v22.41 实测），换歌那帧
	// 常取不到歌词。认领并发标题后，在窗口内持续重试，ID 一落位就补上，避免整首空白。
	var lyricsPending bool
	var pendingDeadline time.Time
	var lastRetry time.Time // 补取尝试的节流锚点（含 FindSongMid 全内存扫描与 fetchLRC 网络请求）
	var retryCount int      // 本首歌已做的补取尝试次数（用于「多次未果才打日志」）
	const lyricRetryWindow = 8 * time.Second
	const lyricRetryInterval = 500 * time.Millisecond
	const lyricRetryLogAfter = 4 // 重试满这么多次仍未就绪才打一行 Detail（快速落位则完全静默）

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
		// 用前缀挡掉 QQ 空闲/过渡态漏进 Name 字段的窗口标题串（"QQ音乐"、"QQ音乐 听我想听" 等）——
		// 真歌名极不可能以 "QQ音乐" 开头，否则会把占位串当成一首歌、推出错误标题。
		if meta.Name != lastName && meta.Name != "" && !strings.HasPrefix(meta.Name, "QQ音乐") {
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
				log.Warn("songMid 等待 %v 仍未就绪，按无歌词继续（本地文件播放？）", midWaitWindow)
			}
			midWait.reset()

			lastName = meta.Name
			lastLineIdx = -1
			anchorProgressMs = meta.ProgressMs
			anchorTime = time.Now()
			lastMemProgress = meta.ProgressMs
			isPaused = false
			lyricsPending = false // 默认这一首已了结；仅下面取词失败时重新 arm

			log.Info("♪ 歌曲: %s - %s", meta.Name, meta.Singer)

			cookie := mem.FindCookie()
			var lrcName, lrcSinger string
			var coverURL string

			if midUnavailable || (meta.SongID == 0 && meta.SongMid == "") {
				// songID/mid 尚未落位（v20.05 等 URL 参数；v22.41 等会话结构+堆上报）——不拿空/伪
				// ID 去空请求（v20.05 上会返回别的歌的歌词/封面）。按无歌词发标题+空歌词，下面
				// arm 补取块，ID 一落位就在窗口内补上。
				err = errSongMidUnavailable
			} else {
				currentLyrics, lrcName, lrcSinger, err = fetchLRC(meta.SongID, meta.SongMid, cookie, meta.DurationMs)
				applyDetailedOffset(currentLyrics, offsetSec)
				coverURL = fetchCoverURL(meta.SongID, meta.SongMid)
			}

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
				// 换歌瞬间 songID/mid 尚未落位是预期过渡态，不是失败——静默进入补取重试。
				// 计数清零：仅当重试满 lyricRetryLogAfter 次仍拿不到才打一行 Detail，快速落位的
				// 常见情形完全静默；真失败由下方 8s 超时上报。
				retryCount = 0
				progress := player.ClampProgress(float32(meta.ProgressMs)/1000.0, currentDurationSec)
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					Position: float32(meta.ProgressMs) / 1000.0,
					Progress: progress,
					Lyrics:   []player.LyricLine{}, Count: 0,
				})
				// songID/songMid 尚未落位（换歌瞬间常见）→ arm 补取，别永久放弃整首
				lyricsPending = true
				pendingDeadline = time.Now().Add(lyricRetryWindow)
				lastRetry = time.Time{} // 允许下一 tick 立即首次补取
			} else if len(currentLyrics) == 0 {
				// 纯音乐：API 成功但无歌词行
				log.Info("检测到纯音乐/无歌词，清空歌词；逐字：%s", detailedFlag(currentLyrics))
				playTimeSec := float32(meta.ProgressMs) / 1000.0
				progress := player.ClampProgress(playTimeSec, currentDurationSec)
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					Position: playTimeSec,
					Progress: progress,
					Lyrics:   []player.LyricLine{}, Count: 0,
				})
				p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
					Index: -1, Text: "", SubText: "", Timestamp: 0,
					Position: playTimeSec, Progress: progress,
				})
			} else {
				log.Info("歌词加载完成: %d 行；逐字：%s", len(currentLyrics), detailedFlag(currentLyrics))
				lyricItems := toLyricLines(currentLyrics, offsetSec)
				progress := player.ClampProgress(float32(meta.ProgressMs)/1000.0, currentDurationSec)
				p.Emit(player.EventAllLyrics, &player.AllLyricsData{
					Title: title, Duration: currentDurationSec,
					Position: float32(meta.ProgressMs) / 1000.0,
					Progress: progress,
					Count:    len(lyricItems), Lyrics: lyricItems,
				})
			}

			// 异步封面下载（带代次守卫，理由同 cloudmusic：迟到的回写会盖住下一首）
			coverGen := p.NewSongGen()
			go func(gen uint64, url, name, singer, t string) {
				if url == "" {
					return
				}
				if b64 := player.FetchCoverBase64("QQMusic", url, 5*time.Second); b64 != "" {
					p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
						Name: name, Singer: singer, Title: t, Cover: url, CoverBase64: b64,
					})
				}
			}(coverGen, coverURL, meta.Name, meta.Singer, title)
		}

		// --- 换歌后歌词补取 ---
		// songID/songMid 落位滞后于显示结构体（v22.41：数字 songID 在独立会话结构、songMid 在
		// 堆上报 JSON，换歌瞬间都可能还是上一首或空）。上面认领并发了标题+空歌词，这里在有界
		// 窗口内持续重试：ID 一落位就补上完整歌词（中途替换 all_lyrics），窗口用尽才收场。
		if lyricsPending && meta.Name == lastName && meta.Name != "" {
			if time.Now().After(pendingDeadline) {
				lyricsPending = false
				log.Warn("歌词补取超时（%v），按无歌词继续（本地文件播放？）", lyricRetryWindow)
			} else if time.Since(lastRetry) >= lyricRetryInterval {
				lastRetry = time.Now()
				retryCount++
				sid, smid := meta.SongID, meta.SongMid
				if mem.Offsets().SongMidParamsOff != 0 {
					sid = 0 // v20.05：结构体 songID 是伪值，只认 songMid
				}
				// songID 缺位时才回退堆扫描 songMid（FindSongMid 是全内存扫描，靠外层节流约束）
				if mem.Offsets().SongMidFromHeap && sid == 0 && smid == "" {
					smid = mem.FindSongMid(meta.DurationMs)
				}
				if sid != 0 || smid != "" {
					cookie := mem.FindCookie()
					lines, lrcName, lrcSinger, ferr := fetchLRC(sid, smid, cookie, meta.DurationMs)
					if ferr == nil {
						applyDetailedOffset(lines, offsetSec)
						lyricsPending = false
						currentLyrics = lines
						lastLineIdx = -1
						name, singer := meta.Name, meta.Singer
						if lrcName != "" {
							name = lrcName
						}
						if lrcSinger != "" {
							singer = lrcSinger
						}
						title := name + " - " + singer
						currentTitle = title
						currentDurationSec = float32(meta.DurationMs) / 1000.0
						psec := float32(meta.ProgressMs) / 1000.0
						prog := player.ClampProgress(psec, currentDurationSec)
						p.Emit(player.EventSongInfoUpdate, &player.SongInfo{Name: name, Singer: singer, Title: title})
						if len(lines) == 0 {
							log.Info("检测到纯音乐/无歌词，清空歌词；逐字：%s", detailedFlag(lines))
							p.Emit(player.EventAllLyrics, &player.AllLyricsData{
								Title: title, Duration: currentDurationSec, Position: psec, Progress: prog,
								Lyrics: []player.LyricLine{}, Count: 0,
							})
						} else {
							log.Info("歌词加载完成(补取): %d 行；逐字：%s", len(lines), detailedFlag(lines))
							items := toLyricLines(lines, offsetSec)
							p.Emit(player.EventAllLyrics, &player.AllLyricsData{
								Title: title, Duration: currentDurationSec, Position: psec, Progress: prog,
								Count: len(items), Lyrics: items,
							})
						}
						coverURL := fetchCoverURL(sid, smid)
						coverGen := p.NewSongGen()
						go func(gen uint64, url, nm, sg, t string) {
							if url == "" {
								return
							}
							if b64 := player.FetchCoverBase64("QQMusic", url, 5*time.Second); b64 != "" {
								p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
									Name: nm, Singer: sg, Title: t, Cover: url, CoverBase64: b64,
								})
							}
						}(coverGen, coverURL, name, singer, title)
					}
				}
				// 只有重试满 lyricRetryLogAfter 次仍没取到才打一行（快速落位则完全静默）；
				// 成功已把 lyricsPending 置 false，不会误报。
				if lyricsPending && retryCount == lyricRetryLogAfter {
					log.Detail("songID/mid 已重试 %d 次仍未就绪，继续补取…", retryCount)
				}
			}
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
				posSec := float32(meta.ProgressMs) / 1000.0
				p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
					Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
				})
				resumeEmitted = true
			}

			if isPaused {
				isPaused = false
				if !resumeEmitted {
					posSec := float32(meta.ProgressMs) / 1000.0
					p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{
						Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
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
			posSec := float32(anchorProgressMs) / 1000.0
			p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{
				Position: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),
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

		progressSec := interpolatedMs / 1000.0
		matchTime := progressSec + offsetSec

		// 歌词行匹配
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

			// progressSec 是实时插值位置，进 Position、并据它算 Progress；play_time 由 BuildLyricUpdate 按
			// 歌词时间轴算。逐字（TextDetailed）的 offset 已在 applyDetailedOffset 里套过。
			p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
				trueIdx, line.Time, line.Text, line.SubText, line.Detailed,
				offsetSec, progressSec, currentDurationSec,
			))
		}
	}
}

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

// toLyricLines converts internal lyricLine to player.LyricLine
func toLyricLines(lines []lyricLine, offsetSec float32) []player.LyricLine {
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, l.Detailed, offsetSec)
	}
	return out
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time。
//
// **必须在 fetch 之后、存进 currentLyrics 之前做一次**：BuildLyricLine 只调行级 play_time，
// TextDetailed 是原样透传的（对照 cloudmusic 的 applyTextDetailedOffset）。漏了这步，逐字
// 高亮会整体偏 offset 那么多；而 lyric_update 直接用 currentLyrics 里的 TextDetailed，
// 不在这儿套就没有第二次机会。
func applyDetailedOffset(lines []lyricLine, offsetSec float32) {
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

// detailedFlag 报告这批歌词**实际**有没有逐字。
//
// 绝不写死——AGENTS.md §6.1：写死的「逐字：否」是断言不是报告，接通逐字的人会据此以为
// 自己没做成、回去改一个本来就对的实现。QQ 音乐的逐字只在 QRC 源（musicu.fcg + qrc:1）
// 上有；songMid 兜底走的 fcg_query_lyric_new.fcg 只回行级 LRC，那时这里如实报「否」。
func detailedFlag(lines []lyricLine) string {
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			return "是"
		}
	}
	return "否"
}
