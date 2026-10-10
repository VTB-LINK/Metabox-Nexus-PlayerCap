// Package applemusic 接入 Apple Music for Windows（AppleMusic.exe）作为第 6 个 player。
//
// 与前 5 个 player 最大的不同：**数据源是纯进程内存扫描，没有 CDP/inspector，也没有显式的播放态
// 字段**。用户要求纯内存、零 WinRT 依赖（Apple Music 是 WinUI3+JSC，非 Node，汽水那套
// _debugProcess 开 9229 的路子不通）。三样数据各自的形态与判据见 mem 包；本包决定「哪份属于
// 当前歌」与「什么时候扫」。
//
// 两个廉价的信号源撑起整个会话（均为 2026-10-08 真机实测，换歌、暂停、恢复、重启各数次）：
//
//   - 位置结构 [时长][位置][位置]：同一进程内地址不变，确认之后每轮只读 24 字节。时长字段是换歌
//     信号（换歌时先归零，加载好再写入新歌时长；时长由 0 变正时刚开播，前后差不到 0.2s），位置字段是
//     播放态信号（整秒推进，停住即暂停）。确认它靠活性：全扫收集同形态候选，之后复读，像歌曲那样每秒
//     走一格的才算——Apple Music 刚启动时有个 0~100 的进度条形态相同、也在走，但走得比每秒一格快。
//   - 「正在播放」存储：一首歌播放期间它在原地不动，换歌时整条在新地址重建或就地改写。记下选中那份的地址与
//     开头几百字节，之后每 300ms 复读比对，对不上就是「存储换了」，立刻全扫一次。时长相同的两首歌
//     自然衔接时位置结构看不出换歌（归零只有几十毫秒），靠的就是它；封面晚到也是它报的。
//
// 全扫（mem.Reader.Scan，本机 0.12~0.24s）只在需要时发生，都有节流：存储换了；换歌后取标题、歌词
// 与封面（歌词缺失时头 3 秒约每 0.4 秒一次、到 15 秒约每 1.2 秒一次，之后退避到 60 秒一次）；尚未确认
// 位置结构时，没见到存储只找存储（不收候选，封顶 5s），见到了才收集候选（封顶 15s）；位置结构停住太久
// 或迟迟宣告不了时找别的。稳态播放零全扫。全扫途中照常读位置、推歌词行（scanTick），别的都等扫完。
//
// 稳定优先（AGENTS §0.1）：任何一步失败都降级重试，绝不 panic、纯只读、绝不写 Apple Music 一个字节。
package applemusic

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/lyric"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
	"Metabox-Nexus-PlayerCap/player/krc"
)

const PlayerName = "applemusic"

// AudioProcessName 是 Apple Music 真正出声的进程，音频波形捕获的目标（audiocap/procscan 用）。
//
// **不是 AppleMusic.exe**：播放由同一安装包里的 AMPLibraryAgent.exe 完成，它由系统 DCOM 服务
// （svchost）拉起，不在 AppleMusic.exe 的进程树内。2026-10-08 真机放歌时 WASAPI 进程 loopback 实测：
// AppleMusic.exe 树内帧在流动但 RMS 恒为 0；AMPLibraryAgent.exe 的 RMS 0.02~0.10。读内存仍是
// AppleMusic.exe（mem.TargetProcessName），两者各管各的。
const AudioProcessName = "AMPLibraryAgent.exe"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("AppleMusic")

// AppleMusicPlayer Apple Music for Windows 播放器（纯内存扫描实现）。
type AppleMusicPlayer struct {
	player.BaseEmitter
	offsetMs int
	pollMs   int
}

// New 创建 Apple Music 播放器。
func New(offsetMs, pollMs int) *AppleMusicPlayer {
	return &AppleMusicPlayer{
		BaseEmitter: player.NewBaseEmitter(PlayerName),
		offsetMs:    offsetMs,
		pollMs:      pollMs,
	}
}

const (
	// minPollInterval 轮询下限。每轮只读 24 字节（亚毫秒），而位置的跳变要在这一轮被看见、歌词行也在
	// 这一轮推出：轮询 300ms 时这两处各晚 0~300ms，叠在位置读数本身的滞后上，审查方要把 offset 从 400
	// 调到 1200 才对得上（见 clock）。50ms 把这两处压到可以忽略。
	minPollInterval = 50 * time.Millisecond

	// watchInterval 复读位置候选、盯「正在播放」存储的间隔。它们不需要跟歌词行一样准，候选有几百个，
	// 不必每轮都读。
	watchInterval = 300 * time.Millisecond

	// loadingGrace 位置结构时长为 0（换歌加载中）持续超过此值即判「没有歌」并清屏。真机换歌的加载
	// 1.3~4.4s 都常见（2026-10-08，10 次里 4 次超过 3s）；取 3s 时这些都会先清屏再宣告，根路径上还会
	// 被别的播放器插进来一下。审查方真机见过「正在连接…」持续 12~43 秒的慢加载，那种才该清。
	loadingGrace = 6 * time.Second

	// twinFailFor 已确认的位置结构连续读失败或形态不对超过这么久即作废、重新发现。
	twinFailFor = 900 * time.Millisecond

	// staleAfter 位置结构停住这么久后，开始节流地找有没有别的在走的结构（审查 P1-8：锁在静止的
	// 旧副本或巧合结构上时，旧实现整首判暂停且永不自愈）。正常暂停时这只是几次全扫。
	staleAfter = 20 * time.Second

	// unannouncedAfter 位置结构确认了却这么久还宣告不出歌（取不到标题）：多半认错了。之后按候选扫描的
	// 节奏继续找别的，找到在走的就换过去。
	unannouncedAfter = 5 * time.Second

	// lyricDurSlack TTML 的 <body dur> 最多比位置结构时长长这么多秒才可能属于当前歌（实测长 0.03~3.77s）。
	// **只有上限，没有下限**：<body dur> 是歌词正文的长度，尾奏长的歌它比音频短得多（2026-10-08 真机：
	// 231.752 对 245、130.820 对 159）。旧实现要求它不短于音频时长，这些歌整首被当成没有歌词。
	lyricDurSlack = 6.0

	// keptDocsMax 留存歌词文档全文的份数上限（见 session.docKept），超出时丢掉最久没在全扫里见到的。
	// 一份 TTML 解码后几千到几万字节（Line 型的《晴天》夹具 8KB，逐字的 Word 型更大），按几万字节一份算
	// 48 份约一两兆。
	keptDocsMax = 48

	// titleConfirmWindow 换歌后这段时间内读到另一个标题，按「首扫读到的是上一首」纠正（见 applyMeta）。
	titleConfirmWindow = 3 * time.Second

	// confirmAt 换歌后这么久再全扫一次核对标题。存储换了会被盯梢发现，这一次只防盯错了对象。
	confirmAt = 1500 * time.Millisecond

	// titleGiveUp 换歌后这么久仍取不到标题就先清屏（之后照常按退避继续找）。实测标题比时长早到。
	titleGiveUp = 10 * time.Second

	// lyricGrace 换歌后这么久还没有歌词，才发「无歌词」（index:-1）。歌词由 Apple Music 另行拉取：
	// 手动切歌时它比时长早到，自然播完切到下一首时在开播后 0.3~0.5s 才到（2026-10-08 真机），网络慢时
	// 审查方见过 10 秒以上。旧实现首扫没找到就立刻发 index:-1，下游当场按纯音乐处理。
	lyricGrace = 15 * time.Second

	// lyricGapFast 宽限期内找歌词的全扫间隔（从上一次扫完起算）；换歌后头 lyricRushFor 内用更密的
	// lyricGapRush。自然播完切到下一首时歌词在开播后 0.3~0.5s 才到，首扫赶不上：按 1.2s 再找，开头就有词
	// 的歌第一行要晚将近 1s 才出（2026-10-08 真机：第一行该在 1.05s 出，1.94s 才出）。
	lyricGapFast = 1200 * time.Millisecond
	lyricGapRush = 400 * time.Millisecond
	lyricRushFor = 3 * time.Second

	// minScanGap 任意两次全扫之间至少隔这么久（从上一次扫完起算）。
	minScanGap = 300 * time.Millisecond

	// storeFlapQuiet 存储抖动的安静期，两处共用：存储「出现 / 消失」距上一次这种翻转不足这么久的算抖动，
	// 不清零周期全扫的退避、不立即补扫（见 applySnapshot）；因存储变化触发的全扫间隔翻倍后，安静这么久才
	// 复原（见 maybeScan 的 dirtyGap）。取 10s：须大于 idleScanSteps 的封顶（5s），抖动时相邻两次全扫至多
	// 隔那么久（见 retableTwinScan），每次翻转才都落在安静期内。
	storeFlapQuiet = 10 * time.Second

	// tagScanAfter 加载中发现存储换了、扫过一次之后，隔这么久再扫一次：歌词比标题晚到 0.2~0.7s，
	// 趁用户还停在这首歌上把它记到这首歌名下（见 noteDocs）。
	tagScanAfter = 700 * time.Millisecond

	// coverHold 换歌后这段时间内，存储里的图若与上一首发出的那张逐字节相同而专辑不同，先不发（见 staleCover）。
	coverHold = 2500 * time.Millisecond

	// emptyArtistHold 歌手为空的存储要稳定这么久才采用。首次开播时系统媒体会话会先报约 250ms 的
	// 「正在连接…」、歌手为空（2026-10-08 真机），不能把它当歌名。
	emptyArtistHold = 1500 * time.Millisecond

	// 位置候选判活（见 watchCandidates）：连着前进 liveSteps 次，相邻两次相隔 liveMinGap ~ liveMaxGap。
	// 歌曲位置约每 1.02s 走一格（偶有 0.78s 的追平），每 watchInterval 复读一次时看到的间隔不小于 0.6s。
	liveSteps  = 2
	liveMinGap = 500 * time.Millisecond
	liveMaxGap = 2500 * time.Millisecond
)

var (
	// titleSteps 取不到标题时的全扫间隔。
	titleSteps = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	// lyricLateSteps 过了宽限期仍没有歌词时的全扫间隔，停在最后一档：用户中途打开歌词面板、或歌词
	// 很晚才到时，最多一分钟后补上。
	lyricLateSteps = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 60 * time.Second}
	// coverTries 宣告时还没有封面的，在换歌后这几个时刻各找一次，之后放弃。封面与标题同在一条存储里，
	// 通常一起到；见过晚 0.4s 的（首次开播），旧会话见过晚 4s 以上的。封面写进来时存储会重建、盯梢会
	// 发现，这几次只防没盯上（例如首扫时没有完整副本）。
	coverTries = []time.Duration{3 * time.Second, 7 * time.Second, 12 * time.Second, 20 * time.Second}
	// twinScanSteps 已见到「正在播放」存储、尚未确认位置结构时收集候选的全扫间隔（第一次立即扫），封顶
	// 15s。候选的时长变了也跟着（暂停中启动后恢复或点别的歌，不必等扫描），所以这档只管「还不是候选」
	// 的情形：存储在，真结构的时长却还是 0（暂停中点了歌、迟迟不按播放）。开头几档密一些；之后约每 15s
	// 一次（2026-10-09 真机复测，收候选的全扫 0.19~0.24s：0.22s / 15s，约占单核 1.5%）。存储换了（用户
	// 点了歌）会另外立刻触发一次。
	twinScanSteps = []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second, 5 * time.Second, 15 * time.Second}
	// idleScanSteps 没见到存储、也没确认位置结构时周期全扫的间隔（第一次立即扫），封顶 5s：Apple Music
	// 启动后还没放过歌，真结构尚未分配、内存里也没有存储可盯，只能靠它等第一首歌。这种全扫只找存储、
	// 不收候选（见 maybeScan），比收候选的快：2026-10-09 真机复测（可读区 436MB）0.12~0.16s 对 0.19~0.24s，
	// 逐窗找候选加预筛之前（2026-10-08，可读区 476MB）是 0.15s 对 0.40s。封顶 5s 时 0.14s / 5s，约占单核 3%，
	// 与那时每 15s 一次 0.40s 的收候选全扫相当，但读内存的总量约为那时的 3 倍。这个状态只在 Apple
	// Music 启动后、放第一首歌之前，或位置结构丢失且存储也消失时存在。认出存储后立即补一次收候选的全扫，
	// 之后改按 twinScanSteps；存储在 storeFlapQuiet 内反复出现与消失时只换表，不补扫、不回到第一档（见
	// applySnapshot）。
	idleScanSteps = []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second}
	// staleScanSteps 位置结构停住超过 staleAfter 后找替代者的全扫间隔（第一次立即扫）。
	staleScanSteps = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second}
)

// EffectivePollInterval 返回配置轮询间隔为 pollMs 毫秒时 Apple Music 实际采用的间隔：低于 50ms 抬到
// 50ms。轮询循环与 /service-status 的 config_effective 共用此函数，下限只在这里定义。
func EffectivePollInterval(pollMs int) time.Duration {
	d := time.Duration(pollMs) * time.Millisecond
	if d < minPollInterval {
		return minPollInterval
	}
	return d
}

// sleepOrStop 睡 d，期间可被 Stop 打断。返回 true 表示收到停止信号。
func (p *AppleMusicPlayer) sleepOrStop(d time.Duration) bool {
	select {
	case <-p.StopCh:
		return true
	case <-time.After(d):
		return false
	}
}

// Start 启动 Apple Music 轮询循环（阻塞，须在 goroutine 中调用）。
func (p *AppleMusicPlayer) Start() {
	for {
		select {
		case <-p.StopCh:
			return
		default:
		}

		r, err := mem.Attach()
		if err != nil {
			if errors.Is(err, mem.ErrAccessDenied) {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("Cannot read the Apple Music process (Apple Music may be running as administrator)"), Reason: player.ReasonNoAdmin})
			} else {
				p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Detail: i18n.T("Apple Music not running"), Reason: player.ReasonProcessNotRunning})
			}
			p.Emit(player.EventClearSongData, nil)
			if p.sleepOrStop(3 * time.Second) {
				return
			}
			continue
		}

		log.Info("Attached to AppleMusic.exe, monitoring playback state")
		p.runSession(r)
		r.Close()

		// 进程退出（Apple Music 常自重启，pid/地址会变）——回到外层重新 Attach。
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("Apple Music has exited"), Reason: player.ReasonProcessExited})
		p.Emit(player.EventClearSongData, nil)
		log.Info("Session ended; waiting for a new AppleMusic process...")
		if p.sleepOrStop(2 * time.Second) {
			return
		}
	}
}

// runSession 轮询内存并 emit 事件，直到目标进程退出或收到 Stop。
func (p *AppleMusicPlayer) runSession(r *mem.Reader) {
	s := newSession(p, r, time.Now)
	poll := EffectivePollInterval(p.pollMs)
	for {
		select {
		case <-p.StopCh:
			return
		default:
		}
		if !r.Valid() {
			return // 进程已退出 → 回外层重连
		}
		s.tick()
		if p.sleepOrStop(poll) {
			return
		}
	}
}

// ── 会话状态 ──

// songState 是当前曲目。dur 来自位置结构（0 = 未知）；resolved 表示已取到标题。
type songState struct {
	dur       float64
	title     string
	artist    string
	album     string
	resolved  bool
	lyrics    []krc.Line
	pureHint  string // TTML 只有一行「纯音乐」提示时的那行文本（lyrics 此时为空），随 index:-1 发出
	docHash   uint64 // 已采用的 TTML；0 = 未采用
	coverHash uint64 // 已发出的封面；0 = 还没发过封面
	coverURI  string
}

// preAttachOwner 是附加时就在内存里的 TTML 在归属表里的主人：来历不明。曲目 key 以标题开头，不会是这个串。
const preAttachOwner = "\x00附加前"

// metaKey 是 TTML 归属表里的曲目标识：标题 + 歌手。不带时长与专辑——文档可能在加载中（时长未知）
// 就记到这首歌名下；同一首歌的单曲版与专辑版歌词文档逐字相同，也应通用。
func metaKey(title, artist string) string { return title + "\x00" + artist }

func (s songState) key() string { return metaKey(s.title, s.artist) }

// keptDoc 是一份留存的歌词文档：全文，与最近一次在全扫里见到它的时刻（超出 keptDocsMax 时按它淘汰）。
type keptDoc struct {
	text   string
	seenAt time.Time
}

// memReader 是 session 对目标进程的全部需求（实现：*mem.Reader；测试注入假实现回放事件序列）。
type memReader interface {
	ReadTwin(addr uintptr) (mem.Twin, bool)
	ReadBytes(addr uintptr, n int) ([]byte, bool)
	Scan(wantTwins bool, during func()) mem.Snapshot
}

// cand 是一个位置候选的观察记录。steps 是「像歌曲那样」连着前进了几次（见 liveSteps）。
type cand struct {
	dur      float64
	lastPos  float64
	steps    int
	lastStep time.Time // 上一次前进的时刻（steps > 0 时有效）
	lastRead time.Time
}

func newCand(t mem.Twin, now time.Time) *cand {
	return &cand{dur: t.Dur, lastPos: t.Pos, lastRead: now}
}

// observe 记一次读数（sinceRead 是距上次读它的秒数），返回它是否已判活。
func (c *cand) observe(t mem.Twin, now time.Time) bool {
	sinceRead := now.Sub(c.lastRead).Seconds()
	c.lastRead = now
	switch {
	case t.Dur != c.dur, t.Pos < c.lastPos, t.Pos != math.Trunc(t.Pos):
		// 时长变了（含归零）、往回走、位置不是整数：从头计。时长变了不能把它从表里删掉——真结构换歌时
		// 就是这样，删了就跟丢它，暂停中启动后直接点另一首歌，要等下一次候选扫描才认得出。
		*c = cand{dur: t.Dur, lastPos: t.Pos, lastRead: now}
	case t.Pos > c.lastPos:
		gap := now.Sub(c.lastStep)
		switch {
		case t.Pos-c.lastPos > math.Floor(sinceRead)+1:
			c.steps = 0 // 一下跳了好几格：进度条、拖动
		case c.steps > 0 && gap >= liveMinGap && gap <= liveMaxGap:
			c.steps++
		default:
			c.steps = 1 // 第一次前进，或与上一次挨得太近 / 隔得太久：从这一次重新计
		}
		c.lastPos, c.lastStep = t.Pos, now
	}
	return c.dur > 0 && c.steps >= liveSteps
}

type session struct {
	p         *AppleMusicPlayer
	r         memReader
	now       func() time.Time // 时钟（测试注入假时钟）
	offsetSec float32

	// 位置源
	twin       uintptr   // 已确认的位置结构；0 = 未知
	twinFailAt time.Time // 已确认的位置结构从这一刻起一直读不出；零值 = 上一次读成功
	loading    bool      // 位置结构时长为 0（换歌加载中）
	loadingAt  time.Time
	cands      map[uintptr]*cand // 候选观察表
	watchAt    time.Time         // 上次复读候选、盯存储的时刻
	twinScan   backoff
	staleScan  backoff
	truncWarn  bool // 候选被截断已告警过

	// 元数据源
	store      mem.NowPlaying     // 上一次全扫认定的「正在播放」存储；Sig 为 nil = 没有
	flipAt     time.Time          // 上一次全扫认出它「出现 / 消失」的时刻（见 storeFlapQuiet）；零值 = 还没有过
	storeDirty bool               // 盯梢发现它换了：尽快全扫
	dirtyGap   time.Duration      // 因 storeDirty 触发的全扫之间的间隔（连着触发就翻倍，防存储抖动时连扫）
	dirtyAt    time.Time          // 上一次因 storeDirty 全扫的时刻
	tagScanAt  time.Time          // 加载中补扫一次的时刻（见 tagScanAfter）；零值 = 不需要
	prevChains map[uintptr]string // 上一首歌期间见过的存储（地址 → 内容标识），用来认出换歌后残留的旧存储
	curChains  map[uintptr]string

	// 当前曲目
	song          songState
	announced     bool // 已为当前曲目发过 song_info_update / all_lyrics
	noSongShown   bool // 已发「没有歌」，避免重复
	needTitle     bool
	needLyrics    bool
	needCover     bool
	needConfirm   bool
	verdictSent   bool // 已发过「无歌词」（index:-1）
	songChangedAt time.Time
	songScanAt    time.Time // 下一次为当前曲目全扫的时刻；零值 = 不需要
	titleTries    int
	lateTries     int
	lastScanEnd   time.Time
	emptyTitle    string // 正在等它稳住的「歌手为空」标题
	emptySince    time.Time
	prevCover     uint64 // 最近一次随别的歌发出的封面与那首歌的专辑（见 staleCover）；0 = 没有
	prevAlbum     string

	// 歌词归属
	docOwner   map[uint64]string    // TTML 全文哈希 → 它属于哪首歌（metaKey）；"" = 分不清；preAttachOwner = 附加前就在
	docSeenAt  map[uint64]time.Time // 最近一次给它记归属的时刻（第一次见到、或出现新副本）
	docAddrs   map[uint64][]uintptr // 上一次全扫时各文档的副本地址（那次没扫到的不在表里）
	docMissed  map[uint64]int       // 来历不明的文档连续几次全扫没见到
	docKept    map[uint64]keptDoc   // 记在具体歌名下的文档的全文（见 noteDocs）；归属仍以 docOwner 为准
	docDur     map[uint64]float64   // 文档最近一次被采用时的曲目时长（见 lyricCandidates）；没被采用过的不在表里
	scanned    bool                 // 本会话已做过第一次全扫
	songsBegun int                  // 本会话开始过几首歌

	clk         clock
	lastLineIdx int
}

func newSession(p *AppleMusicPlayer, r memReader, clock func() time.Time) *session {
	now := clock()
	s := &session{
		p:           p,
		r:           r,
		now:         clock,
		offsetSec:   float32(p.offsetMs) / 1000,
		docOwner:    map[uint64]string{},
		docSeenAt:   map[uint64]time.Time{},
		docMissed:   map[uint64]int{},
		docKept:     map[uint64]keptDoc{},
		docDur:      map[uint64]float64{},
		prevChains:  map[uintptr]string{},
		curChains:   map[uintptr]string{},
		dirtyGap:    minScanGap,
		lastLineIdx: -1,
		staleScan:   backoff{steps: staleScanSteps},
	}
	s.clk.lastRaw = -1
	// 附加成功后先报「没有歌」：此前外层报的是「未启动」，附加后若一直没有歌也不能沿用它。
	// 确认位置结构（即确认在放）之前不宣告任何歌：暂停中启动时报的就是这个，属实。
	s.showNoSong()
	s.restartTwinScan(now, true)
	return s
}

// twinScanTable 返回与此刻状态相符的 twinScan 间隔表：没见到存储、也没确认位置结构用 idleScanSteps
// （只找存储），其余用 twinScanSteps（收候选）。这两样状态变化的地方（dropTwin、applySnapshot 里存储
// 出现或消失）都经 restartTwinScan 或 retableTwinScan 重选，表与状态因此总是一致。
func (s *session) twinScanTable() []time.Duration {
	if s.twin == 0 && s.store.Sig == nil {
		return idleScanSteps
	}
	return twinScanSteps
}

// restartTwinScan 重排 twinScan：immediate 为真立即到期，否则从第一档起算。间隔表见 twinScanTable。
func (s *session) restartTwinScan(now time.Time, immediate bool) {
	s.twinScan.steps = s.twinScanTable()
	s.twinScan.reset(now)
	if !immediate {
		s.twinScan.advance(now)
	}
}

// retableTwinScan 只把 twinScan 的间隔表换成与此刻状态相符的那张（见 twinScanTable）：档位不清零、不立即
// 到期，已排定的下一次全扫照旧，只是不晚于新表的封顶。存储抖动时用它代替 restartTwinScan（见
// applySnapshot）。
//
// 封顶那一条管的是换到 idleScanSteps 的时候：下一次全扫是在认出存储消失之前按 twinScanSteps 排下的，
// 可达 15s。不收回来，没有存储的状态下要等 15s 才再找存储；并且这 15s 超出 storeFlapQuiet，下一次「出现」
// 又被当成头一回而清零补扫（离线回放见 TestScanBudgetStoreFlapping 的变异自证）。
func (s *session) retableTwinScan(now time.Time) {
	steps := s.twinScanTable()
	s.twinScan.steps = steps
	if latest := now.Add(steps[len(steps)-1]); s.twinScan.next.After(latest) {
		s.twinScan.next = latest
	}
}

// tick 是一轮轮询：读位置 → 复读候选与存储 → 到期的全扫 → 无歌词判决 → 歌词行 → 位置采样。
func (s *session) tick() {
	now := s.now()
	s.readPosition(now)
	if now.Sub(s.watchAt) >= watchInterval {
		s.watchAt = now
		s.watchCandidates(now)
		s.watchStore()
	}
	s.checkStale(now)
	s.maybeScan()
	s.checkVerdict()
	s.emitLyricLine()
	s.emitPositionSample()
}

// readPosition 读一次已确认的位置结构并处理；连续读不出超过 twinFailFor 即作废。还没有位置结构时什么都不做。
func (s *session) readPosition(now time.Time) {
	if s.twin == 0 {
		return
	}
	t, ok := s.r.ReadTwin(s.twin)
	switch {
	case ok:
		s.twinFailAt = time.Time{}
		s.onTwin(t, now)
	case s.twinFailAt.IsZero():
		s.twinFailAt = now
	case now.Sub(s.twinFailAt) >= twinFailFor:
		log.Warn("Position structure invalid; rediscovering")
		s.dropTwin(now)
	}
}

// scanTick 是全扫途中的一轮（mem.Reader.Scan 约每 20ms 调一次）：只读位置、推歌词行，别的都等扫完。
//
// 全扫要零点几秒，而换歌本身就会触发全扫：2026-10-08 真机，自然播完切到下一首时存储比时长早约 0.1s
// 换掉，盯梢发现后的那次全扫正好盖住开播，时长由 0 变正晚了 0.29s 才被看见——位置起点晚这么多，
// 这段时间里该推的歌词行也跟着晚。
func (s *session) scanTick() {
	s.readPosition(s.now())
	s.emitLyricLine()
}

// ── 位置源 ──

// onTwin 处理已确认位置结构的一次读数。
func (s *session) onTwin(t mem.Twin, now time.Time) {
	if t.Dur == 0 {
		if !s.loading {
			s.loading = true
			s.loadingAt = now
		}
		if (s.announced || s.song.dur > 0) && now.Sub(s.loadingAt) >= loadingGrace {
			log.Info("Song still loading after %v; treating as no song", loadingGrace)
			s.clearSong()
		}
		return
	}
	if s.loading || t.Dur != s.song.dur {
		s.loading = false
		if t.Pos == 0 {
			// 看到时长变成新值、位置还是 0：刚开播，位置从 0 起按墙钟走（见 clock.start）。
			s.beginSong(t, now, 0, true)
		} else {
			s.beginSong(t, now, t.Pos+flipLagMid, false)
		}
		return
	}
	before := s.clk.live(now)
	switch s.clk.observe(t.Pos, now) {
	case evSeek:
		s.lastLineIdx = -1
		if t.Pos < 3 && before > 10 {
			// 同时长的歌从头放：可能是重播，也可能是没看到归零的换歌。存储若换了盯梢会发现，这里再
			// 主动核对一次，不等它。
			s.storeDirty = true
		}
		if s.announced {
			if after := s.clk.live(now); after < before {
				log.Info("Seek-back detected: %.2fs → %.2fs", before, after)
			} else {
				log.Info("forward seek detected: %.2fs → %.2fs", before, after)
			}
			s.emitTime(player.EventPlaybackResume)
		}
	case evPausedSeek:
		s.lastLineIdx = -1 // 只重锚：暂停中拖动不发 playback_resume，恢复时再发（审查 P1-7）
	case evResume:
		if s.announced {
			log.Info("resumed @ %.2fs", s.clk.live(now))
			s.emitTime(player.EventPlaybackResume)
			s.emitStatus()
		}
	case evPause:
		if s.announced {
			log.Info("paused @ %.2fs", s.clk.live(now))
			s.emitTime(player.EventPlaybackPause)
			s.emitStatus()
		}
	}
	if s.clk.playing && s.announced && s.cands != nil {
		s.staleScan.stop()
		s.twinScan.stop()
		s.cands = nil // 在走、也宣告出去了：就是它，不必再复读候选
	}
}

// beginSong 位置结构给出了新时长：换歌（或会话里的第一首）。标题、歌词与封面交给 maybeScan，同一次
// 全扫配好再宣告。est 是此刻的位置估计；fromStart 表示看到的是开播那一刻（见 clock.start）。
//
// 起手一律按在放：时长变成非 0 就是开播（暂停中切歌时时长一直是 0，按了播放才写入，2026-10-08 真机）。
// 万一不是，位置不走，startPauseThreshold 内会判成暂停。
func (s *session) beginSong(t mem.Twin, now time.Time, est float64, fromStart bool) {
	log.Info("Song change: duration %.0fs", t.Dur)
	s.songsBegun++
	s.rotateChains()
	if s.song.coverHash != 0 {
		s.prevCover, s.prevAlbum = s.song.coverHash, s.song.album
	}
	s.song = songState{dur: t.Dur}
	s.announced = false
	s.needTitle, s.needLyrics, s.needCover, s.needConfirm = true, true, true, true
	s.verdictSent = false
	s.songChangedAt = now
	s.songScanAt = now
	s.titleTries, s.lateTries = 0, 0
	s.emptyTitle = ""
	s.lastLineIdx = -1
	if fromStart {
		s.clk.start(now, t.Dur)
	} else {
		s.clk.reset(t.Pos, est, now, true, t.Dur)
	}
}

// rotateChains 换歌了：此前见过的存储都算「上一首期间的」。
func (s *session) rotateChains() {
	s.prevChains = s.curChains
	s.curChains = make(map[uintptr]string, len(s.prevChains))
}

// dropTwin 作废位置结构：位置未知，播放态按暂停报（没有可靠位置源时不报 playing，审查 P1-5）。
func (s *session) dropTwin(now time.Time) {
	s.twin = 0
	s.twinFailAt = time.Time{}
	s.loading = false
	s.restartTwinScan(now, true)
	if s.clk.playing && s.announced {
		s.clk.freeze(now)
		s.emitTime(player.EventPlaybackPause)
		s.emitStatus()
	}
	s.clk.playing = false
}

// watchCandidates 复读候选表，认出像歌曲那样在走的那个：时长不变、位置是整数、每次只走一格、
// 连着 liveSteps 次且相邻两次相隔约一秒（见 cand.observe）。
//
// 只看「在不在走」不够（2026-10-08 真机，旧实现累计前进 2 格即认）：Apple Music 刚启动的头一两秒里，
// 有个 [100][进度][进度] 形态的进度条在走，被当成了时长 100 秒的位置结构；之后真的开始放歌，要等它
// 「停住 20 秒」才换过去。
func (s *session) watchCandidates(now time.Time) {
	if len(s.cands) == 0 {
		return
	}
	var live []uintptr
	for addr, c := range s.cands {
		t, ok := s.r.ReadTwin(addr)
		if !ok {
			delete(s.cands, addr)
			continue
		}
		if c.observe(t, now) && addr != s.twin {
			live = append(live, addr)
		}
	}
	if len(live) == 0 {
		return
	}
	if s.store.Sig == nil {
		// 有东西在走，却还没见过任何「正在播放」存储：没有歌名可宣告，先不认（刚启动时的进度条就是
		// 这种情形）。马上扫一次，有歌的话下一轮就认。
		s.storeDirty = true
		return
	}
	// map 迭代序随机：多个同时判活时按地址取最小的，结果确定（AGENTS §3.8）。
	sort.Slice(live, func(i, j int) bool { return live[i] < live[j] })
	addr := live[0]
	c := s.cands[addr]
	if s.twin != 0 {
		log.Warn("Position structure stalled while another one advances; switching to @%#x", addr)
	} else {
		log.Info("Position structure located @%#x (duration %.0fs)", addr, c.dur)
	}
	t := mem.Twin{Dur: c.dur, Pos: c.lastPos}
	// 最近那一跳是在某一轮复读里看到的，真正发生在它之前 0 ~ watchInterval：取中间。
	est := c.lastPos + flipLagMid + watchInterval.Seconds()/2 + now.Sub(c.lastStep).Seconds()
	sameSong := s.twin != 0 || s.announced // 曾有过位置结构、或歌已宣告：是「找回来」而不是「第一次认出」
	s.twin = addr
	s.twinFailAt = time.Time{}
	s.loading = false
	delete(s.cands, addr)
	s.twinScan.stop()
	s.staleScan.stop()
	if sameSong && s.announced && c.dur == s.song.dur {
		// 同一首（位置结构丢失或停住后找到在走的那个）：接上位置，按恢复处理。找回的位置可能在已推的
		// 行之前，行号复位——不复位则 nextLine 一直拒发，歌词停到播放位置越过原先那一行为止。
		s.cands = nil
		s.lastLineIdx = -1
		s.clk.reset(c.lastPos, est, now, true, c.dur)
		s.emitTime(player.EventPlaybackResume)
		s.emitStatus()
		return
	}
	s.beginSong(t, now, est, false)
}

// watchStore 盯住上一次认定的「正在播放」存储：读不出或与记下的字节对不上，就是它换了。
func (s *session) watchStore() {
	if s.store.Sig == nil || s.storeDirty {
		return
	}
	b, ok := s.r.ReadBytes(s.store.Addr, len(s.store.Sig))
	if !ok || !bytes.Equal(b, s.store.Sig) {
		s.storeDirty = true
	}
}

// checkStale 位置结构停住太久、或确认了却迟迟宣告不出歌时，节流地找替代者（候选进表后由
// watchCandidates 认出）。
func (s *session) checkStale(now time.Time) {
	if s.twin == 0 || s.loading {
		return
	}
	if !s.announced && now.Sub(s.songChangedAt) >= unannouncedAfter {
		if s.twinScan.next.IsZero() {
			s.restartTwinScan(now, true)
		}
		return
	}
	if s.clk.playing || now.Sub(s.clk.lastChange) < staleAfter {
		return
	}
	if s.staleScan.next.IsZero() {
		s.staleScan.reset(now)
	}
}

// ── 全扫 ──

// maybeScan 到期就全扫一次，一次满足所有需求（标题、歌词、封面、位置候选）。
//
// 标题与歌词只在「位置结构已确认、不在加载中、有时长」时用来宣告：加载中内存里可能是「正在连接…」
// 这类占位标题，那时扫到的东西配上一首的时长去宣告，正是要防的错（复查实测过快速连切时宣告出
// 「正在连接…」）。加载中发现存储换了也会扫，但只用来记歌词文档的归属（见 noteDocs）。
func (s *session) maybeScan() {
	now := s.now()
	songReady := s.twin != 0 && !s.loading && s.song.dur > 0
	// 换歌后迟迟取不到标题：上一首的标题与歌词不能一直挂在屏上，先报「没有歌」，取到再宣告。
	// 每轮都判，不等下一次扫描（扫描按退避排，等它会拖到十几秒）。
	if songReady && !s.announced && s.needTitle && now.Sub(s.songChangedAt) >= titleGiveUp && !s.noSongShown {
		log.Warn("No title %v after the song change; clearing the overlay for now", titleGiveUp)
		s.showNoSong()
		s.p.Emit(player.EventClearSongData, nil)
	}
	if now.Sub(s.lastScanEnd) < minScanGap && !s.lastScanEnd.IsZero() {
		return
	}
	wantSong := songReady && !s.songScanAt.IsZero() && !now.Before(s.songScanAt)
	wantDirty := s.storeDirty && now.Sub(s.dirtyAt) >= s.dirtyGap
	wantTag := !s.tagScanAt.IsZero() && !now.Before(s.tagScanAt)
	wantTwins := s.twinScan.due(now) || s.staleScan.due(now)
	if !wantSong && !wantDirty && !wantTag && !wantTwins {
		return
	}
	// 还没确认位置结构时，见到了存储的每一次全扫都顺带收候选：存储换了（用户点了歌）正是真结构要露面的
	// 时候。没见到存储就不收——watchCandidates 那时不认任何候选（「先见到存储才认」），而收候选的全扫
	// 更慢（耗时见 idleScanSteps）。本会话第一次全扫例外：附加时歌可能正在放，存储与候选要同一次拿到。
	noTwin := s.twin == 0
	collect := wantTwins
	if noTwin {
		collect = s.store.Sig != nil || s.lastScanEnd.IsZero()
	}
	snap := s.r.Scan(collect, s.scanTick)
	end := s.now()
	s.lastScanEnd = end
	s.tagScanAt = time.Time{}
	if wantDirty {
		// 存储若一直在变（抖动），别跟着连扫：间隔翻倍，封顶 5s；安静 storeFlapQuiet 后复原。
		if end.Sub(s.dirtyAt) > storeFlapQuiet {
			s.dirtyGap = minScanGap
		} else if s.dirtyGap *= 2; s.dirtyGap > 5*time.Second {
			s.dirtyGap = 5 * time.Second
		}
		s.dirtyAt = end
	}
	if collect {
		s.setCandidates(snap, end) // 没收候选的全扫不碰观察表：拿空列表去并会把已在观察的丢掉
	}
	if collect || noTwin { // 只找存储的那种全扫也由 twinScan 排期
		if s.twinScan.due(end) {
			s.twinScan.advance(end)
		}
		if s.staleScan.due(end) {
			s.staleScan.advance(end)
		}
	}
	// 扫描要零点几秒，其间可能已经开播、换歌或归零（scanTick 一直在读，再补读一次到扫完这一刻）：
	// 「歌就绪了没有」以此刻为准，不沿用扫描前的判断——否则扫到一半切了歌，这份快照还会被拿去给上一首
	// 配歌词；反过来，加载中扫到一半开播了，这份快照就能直接用来宣告，不必再等一次全扫。快照里的存储
	// 若在途中换了，applySnapshot 复读比对时会发现。
	s.readPosition(end)
	songReady = s.twin != 0 && !s.loading && s.song.dur > 0
	s.applySnapshot(snap, end, songReady, wantDirty, collect)
}

// setCandidates 把这次扫到的候选并进观察表。已在观察的一律留着——哪怕它此刻时长是 0、不在这次的
// 候选里：真结构换歌加载中就是这样，丢了它要等下一次候选扫描才认得出。
func (s *session) setCandidates(snap mem.Snapshot, now time.Time) {
	if snap.TwinsTruncated && !s.truncWarn {
		s.truncWarn = true
		log.Warn("More than %d position candidates; truncated", len(snap.Twins))
	}
	next := make(map[uintptr]*cand, len(snap.Twins)+len(s.cands))
	if len(s.cands) <= 2*len(snap.Twins)+64 { // 观察表只增不减会越攒越多：远多于这次扫到的就丢掉重来
		for addr, c := range s.cands {
			next[addr] = c
		}
	}
	for _, addr := range snap.Twins {
		if addr == s.twin {
			continue
		}
		if _, ok := next[addr]; ok {
			continue
		}
		if t, ok := s.r.ReadTwin(addr); ok && t.Dur > 0 {
			next[addr] = newCand(t, now)
		}
	}
	s.cands = next
}

// applySnapshot 用一次全扫的结果更新存储、歌词归属与当前曲目。collected 表示这次全扫收了位置候选。
func (s *session) applySnapshot(snap mem.Snapshot, now time.Time, songReady, dirty, collected bool) {
	for _, c := range snap.Songs {
		s.curChains[c.Addr] = chainID(c)
	}
	np, ok := pickNowPlaying(snap.Songs, s.prevChains, s.song)
	if ok {
		// 刚扫完它就变了：正在换歌的当口，这份快照不可信。下一轮重扫。
		if b, rok := s.r.ReadBytes(np.Addr, len(np.Sig)); !rok || !bytes.Equal(b, np.Sig) {
			s.storeDirty = true
			return
		}
	}
	changed := ok && (s.store.Sig == nil || metaKey(np.Title, np.Artist) != metaKey(s.store.Title, s.store.Artist))
	gone := !ok && s.store.Sig != nil
	// 存储「出现 / 消失」距上一次这种翻转不足 storeFlapQuiet 的算抖动；存储还在、只是换了歌名不是翻转。
	// 两次翻转的时刻都取认出它的那次全扫，不是存储实际出现或消失的时刻。
	flip := ok != (s.store.Sig != nil)
	flapping := flip && now.Sub(s.flipAt) < storeFlapQuiet
	if flip {
		s.flipAt = now
	}
	s.noteDocs(snap.Docs, np, ok, now)
	if ok {
		s.store = np
	} else {
		s.store = mem.NowPlaying{}
	}
	s.storeDirty = false
	if !songReady {
		switch {
		case s.twin != 0: // 位置结构已确认：twinScan 不归这里排
		case flapping:
			// 相邻几次全扫时而认出、时而认不出存储：每次翻转都重排的话，周期全扫停在「认出 → 立即补扫 →
			// 没认出 → 2s 后再扫」的循环里，约每 1.15s 一次（离线回放 10 分钟 521 次），到不了
			// idleScanSteps 或 twinScanSteps 的封顶。只换表，退避照原样往下走。代价：存储真的消失不久
			// 又回来时不立即补扫，等原先排定的下一次（至多 5s）。
			s.retableTwinScan(now)
		case changed:
			// 用户点了歌而位置结构还没认出来：它过一两秒才写入时长，按候选扫描的开头几档再扫。这次全扫
			// 若没收候选（此前没见到存储），不等那 2s，马上补一次收候选的。
			s.restartTwinScan(now, !collected)
		case gone:
			// 存储没了：回到只找存储的节奏，不等收候选时排下的那一档（可达 15s）走完。
			s.restartTwinScan(now, false)
		}
		if dirty && changed {
			s.tagScanAt = now.Add(tagScanAfter)
		}
		return
	}

	announce := false
	if ok {
		announce = s.applyMeta(np, now)
	}
	if s.song.resolved && s.needLyrics {
		if lines, h, found := pickTTML(s.lyricCandidates(snap.Docs), s.song.dur, s.docOwner, s.song.key(), s.songsBegun <= 1); found {
			s.applyLyrics(lines, h)
		}
	}
	if announce {
		s.announce()
	} else if ok {
		s.applyCover(np)
	}
	s.scheduleSongScan(now)
}

// scheduleSongScan 排下一次为当前曲目的全扫：还缺什么，就按它的节奏。
func (s *session) scheduleSongScan(now time.Time) {
	since := now.Sub(s.songChangedAt)
	if since >= confirmAt-300*time.Millisecond {
		s.needConfirm = false
	}
	var next time.Time
	earlier := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	switch {
	case s.needTitle:
		i := s.titleTries
		if i >= len(titleSteps) {
			i = len(titleSteps) - 1
		}
		s.titleTries++
		earlier(now.Add(titleSteps[i]))
	case s.needLyrics && since < lyricRushFor:
		earlier(now.Add(lyricGapRush))
	case s.needLyrics && since < lyricGrace:
		earlier(now.Add(lyricGapFast))
	case s.needLyrics:
		i := s.lateTries
		if i >= len(lyricLateSteps) {
			i = len(lyricLateSteps) - 1
		}
		s.lateTries++
		earlier(now.Add(lyricLateSteps[i]))
	}
	if s.needConfirm {
		earlier(s.songChangedAt.Add(confirmAt))
	}
	if s.needCover && s.announced {
		s.needCover = false
		for _, t := range coverTries {
			if t > since {
				s.needCover = true
				earlier(s.songChangedAt.Add(t))
				break
			}
		}
	}
	s.songScanAt = next
}

// noteDocs 给 TTML 文档记归属：**一份文档出现新副本时「正在播放」存储里是哪首歌，它就是哪首歌的**。
// 新副本 = 这次全扫里它有上一次全扫没有的地址，含文档第一次出现、整份消失后重新出现。
//
// TTML 里没有任何歌曲标识，时长又只能当上限用（见 lyricDurSlack），归属只能靠「什么时候出现的」。
// 真机（2026-10-08，歌词面板关闭，每秒采样各文档的副本地址）：
//
//   - 每次一首歌成为当前曲目，它的文档都会出现新地址的副本（重放《白月光》x1→x3、《爱如潮水》0→x3、
//     《别怕我伤心》x4→x5、《紅蓮華》0→x5）。手动点播时新副本与新存储同一采样出现，早于时长写入约 1s；
//     自然播完切到下一首时，文档在开播之后才出现（见过 0.3~0.5s，也见过约 2s）。
//   - 会预取：播放列表起播约 7s 时，当前歌与列表里另一首的文档在同一采样里一起出现。
//   - 不再是当前曲目后，副本在几十秒到几分钟内陆续回收，可能整份消失、重放时重新出现。
//
// 所以归属跟着最近一次新副本走，**覆盖原归属**：只在第一次见到时记的话，附加前就在内存里的文档
// （preAttachOwner）除第一首外永不采用，预取的文档则记在上一首名下、轮到它时被拒——两种都是等满
// lyricGrace 报无歌词（同日真机两种都复现了）。两种情形不改记：
//
//   - 本会话第一次全扫：只建基线。内存里已有的文档来历不明（附加前各首歌的都在，实测 7 份留了 7 分钟），
//     一律记 preAttachOwner，只有时长严格对得上才给第一首用（见 pickTTML）。
//   - 这次没认出任何存储：第一次出现的记 ""，已有归属的不动。
//
// 来历不明的文档连续两次全扫都不在内存里就忘掉：之后再出现是重新取回的，按第一次出现算（那次没认出
// 存储就记 ""，不再算「附加前就在」）。
//
// 记在具体歌名下的文档，全文留在 docKept 里，离开内存后仍可用（来历不明的两种不留）。文档可能只在内存里
// 待一秒（2026-10-09 真机，逐秒采样）：存储换成《信仰》的同一秒它的文档出现 2 份，1 秒后整份消失，这一次
// 播放期间再没出现过；而这首歌起播慢——时长写入约 1s 后又归零，其后约 9s 才开始走，超过 loadingGrace
// 走了清屏——就绪时文档早已不在内存里。归属在加载中的那次全扫就记下了，当时却只从当次快照里挑，等满
// lyricGrace 报了无歌词。文档的寿命差别很大：同一首再放时留了 42 秒，另一首的一直留着。挑歌词时留存的
// 与当次扫到的一起交给 pickTTML（见 lyricCandidates），归属照旧以 docOwner 为准、随改记与标题纠正转移；
// clearSong、换歌都不清它，超出 keptDocsMax 丢掉最久没在全扫里见到的。留存的副本只配给采用它时同一时长
// 的曲目：applyLyrics 把那时的曲目时长记进 docDur，文档改记到另一首歌名下时这条记录作废（同一歌名下出现
// 新副本、标题纠正、留存被淘汰时保留，见 dropOldestKept），核对在 lyricCandidates。
//
// 已知的缝（不用代码去堵）：① 当前歌没有自己的歌词时，其间出现新副本的别的文档（预取的下一首、很晚才到
// 的上一首）只要过得了时长上限，就会被当前歌采用，并留在它名下，直到这份文档在别的歌名下再出现新副本。
// ② 预取的文档轮到自己那首时若没有再出现新副本，仍记在别的歌名下、配不上（2026-10-08 五次观察都出现了）。
// ③ 附加时正在放的那首，文档比音频短就配不上。④ 留存只在本会话内：服务重启后是空的，重启前放过的歌轮到
// 它时若文档没有出现新副本，仍按来历不明处理（2026-10-09 真机：《crossing field》重启后再放，文档 3 份的
// 地址都没变；它的 <body dur> 239.054 比曲目时长 248 短 9 秒，严格窗口也配不上，报了无歌词）。⑤ 同名同
// 歌手的两个版本只靠时长分开，且只管留存的：时长相同的两个版本分不开；短版的文档还留在内存里时（由全扫
// 带入、不经时长核对），长版仍可能采用它，留存之前就是这样；留存的文档从未被采用过（只在加载中见过）时
// 没有时长可核对；两个时长不同的版本共用逐字相同的文档时，留存的那份只配给最近采用它的那个时长，另一个
// 版本要等文档在内存里再出现。
func (s *session) noteDocs(docs []mem.TTMLDoc, np mem.NowPlaying, haveStore bool, now time.Time) {
	addrs := make(map[uint64][]uintptr, len(docs))
	for _, d := range docs {
		h := hashDoc(d.Text)
		addrs[h] = d.Addrs
		delete(s.docMissed, h)
		prev, seenLast := s.docAddrs[h]
		_, known := s.docOwner[h]
		switch {
		case !s.scanned:
			s.docOwner[h], s.docSeenAt[h] = preAttachOwner, now
		case haveStore && (!seenLast || hasNewAddr(d.Addrs, prev)):
			key := metaKey(np.Title, np.Artist)
			if s.docOwner[h] != key {
				delete(s.docDur, h) // 改记到另一首歌名下：记着的时长是原先那首的
			}
			s.docOwner[h], s.docSeenAt[h] = key, now
		case !known:
			s.docOwner[h], s.docSeenAt[h] = "", now
		}
		if o := s.docOwner[h]; o != "" && o != preAttachOwner {
			s.docKept[h] = keptDoc{text: d.Text, seenAt: now}
		}
	}
	for len(s.docKept) > keptDocsMax {
		s.dropOldestKept()
	}
	s.docAddrs = addrs
	for h, o := range s.docOwner {
		if _, seen := addrs[h]; seen || (o != preAttachOwner && o != "") {
			continue
		}
		if s.docMissed[h]++; s.docMissed[h] >= 2 {
			delete(s.docOwner, h)
			delete(s.docSeenAt, h)
			delete(s.docMissed, h)
		}
	}
	s.scanned = true
}

// dropOldestKept 丢掉留存里最久没在全扫里见到的一份；同一次全扫见到的取哈希小的，结果不随 map 迭代序变。
// 它被采用时记下的时长（docDur）不删：这份文档日后在同一歌名下再被留存时，仍只配给那个时长——删了的话，
// 它若只在加载中被重新见到（没再被采用），就没有时长可核对，会被同名的另一个版本采用。每份 16 字节，
// 量级同 docOwner。
func (s *session) dropOldestKept() {
	var oldest uint64
	found := false
	for h, k := range s.docKept {
		if o := s.docKept[oldest]; !found || k.seenAt.Before(o.seenAt) || (k.seenAt.Equal(o.seenAt) && h < oldest) {
			oldest, found = h, true
		}
	}
	delete(s.docKept, oldest)
}

// lyricCandidates 返回交给 pickTTML 的候选：这次全扫里的文档，加上留存的。留存的都记在具体歌名下（见
// noteDocs），是不是当前曲目的、过不过得了时长上限，照旧由 pickTTML 按归属表判。这次也扫到的会重复一份：
// 全文与哈希相同，副本数记 0、排在扫到的那份之后，挑中哪份结果都一样。
//
// 留存的副本若被采用过（docDur 记着那时的曲目时长）、而当前曲目时长不同，不并入：归属键是（标题, 歌手），
// 分不出同名同歌手的两个版本，<body dur> 又只有上限——先放过 200 秒的短版，再放 300 秒的长版时，留下的
// 短版文档首扫就被采用，长版自己的文档随后出现也换不上（已有歌词就不再挑），整首用短版的字和时间轴
// （PR #60 审查 R21）。位置结构的时长是整数秒，同一首歌每次放都一样（2026-10-09 的记录里三首歌各放两三
// 次，每次相同），按相等比。没被采用过的（只在加载中见过）没有时长可核对，照旧并入。这次全扫里扫到的
// 文档不经这道核对，规则与留存之前相同。
func (s *session) lyricCandidates(docs []mem.TTMLDoc) []mem.TTMLDoc {
	out := slices.Clip(docs)
	for h, k := range s.docKept {
		if dur, used := s.docDur[h]; used && dur != s.song.dur {
			continue
		}
		out = append(out, mem.TTMLDoc{Text: k.text})
	}
	return out
}

// hasNewAddr 报告 cur 里有没有 prev 没有的地址。
func hasNewAddr(cur, prev []uintptr) bool {
	for _, a := range cur {
		if !slices.Contains(prev, a) {
			return true
		}
	}
	return false
}

// applyMeta 采用扫到的标题，返回是否该宣告曲目（标题变了，含换歌后首次取到）。宣告由调用方在同一次
// 扫描配完歌词后再做：先宣告再配歌词会先发一条空歌词的 all_lyrics，overlay 闪一下「无歌词」。
func (s *session) applyMeta(np mem.NowPlaying, now time.Time) bool {
	if np.Artist == "" {
		// 歌手为空：可能是加载占位（见 emptyArtistHold）。同一个标题稳住了再用。
		if s.emptyTitle != np.Title {
			s.emptyTitle, s.emptySince = np.Title, now
		}
		if now.Sub(s.emptySince) < emptyArtistHold {
			return false
		}
	} else {
		s.emptyTitle = ""
	}
	s.needTitle = false
	if s.song.resolved && np.Title == s.song.title && np.Artist == s.song.artist {
		return false
	}
	if s.song.resolved {
		log.Info("Title changed: %s → %s", buildTitle(s.song.title, s.song.artist), buildTitle(np.Title, np.Artist))
		if now.Sub(s.songChangedAt) <= titleConfirmWindow {
			// 换歌后头一次扫到的还是上一首的标题，现在才读到真的。这期间记到旧标题名下的歌词文档其实是
			// 新歌的，转给它；更早就属于旧标题的（上一首自己的歌词）不动——旧实现在这里把旧标题名下的
			// 认领全撤了，时长相同的两首歌相邻时，新歌当场领走上一首的歌词。
			old, key := s.song.key(), metaKey(np.Title, np.Artist)
			for h, k := range s.docOwner {
				if k == old && !s.docSeenAt[h].Before(s.songChangedAt) {
					s.docOwner[h] = key
				}
			}
		} else {
			// 时长没变、也没看到归零：时长相同的下一首（自然播完衔接时归零只有几十毫秒）。按新歌重来。
			s.songsBegun++
		}
		if s.song.coverHash != 0 {
			s.prevCover, s.prevAlbum = s.song.coverHash, s.song.album
		}
		s.song = songState{dur: s.song.dur}
		s.announced = false
		s.needLyrics, s.needCover, s.needConfirm = true, true, true
		s.verdictSent = false
		s.songChangedAt = now
		s.lateTries = 0
		s.lastLineIdx = -1
	}
	s.song.title, s.song.artist, s.song.album = np.Title, np.Artist, np.Album
	s.song.resolved = true
	return true
}

// applyLyrics 采用一份 TTML 并补发 all_lyrics（歌词晚到也要补发，审查 P1-3）。只有一行「纯音乐」提示
// 的 TTML 不当歌词：清空行、留下提示随 index:-1 发出——行里留着它会再推一条 index 0 的 lyric_update
// （kugou 踩过同一处）。
func (s *session) applyLyrics(lines []krc.Line, h uint64) {
	applyDetailedOffset(lines, s.offsetSec)
	s.docOwner[h] = s.song.key()
	s.docDur[h] = s.song.dur // 留存的副本之后只配给同一时长的曲目（见 lyricCandidates）
	pure := player.IsPureMusicOnly(toLyricLines(lines, s.offsetSec))
	if pure {
		s.song.pureHint = lines[0].Text
		lines = nil
	}
	s.song.lyrics = lines
	s.song.docHash = h
	s.needLyrics = false
	s.lastLineIdx = -1
	if s.announced {
		s.emitAllLyrics()
		if pure && !s.verdictSent {
			s.emitVerdict()
		}
	}
}

// applyCover 同一首歌的封面到了或换了（宣告时还没有、或当时存储里还是上一首的图）：补发一条 song_info_update。
func (s *session) applyCover(np mem.NowPlaying) {
	if !s.announced || np.Cover == nil || np.Title != s.song.title || np.Artist != s.song.artist {
		return
	}
	h := hashBytes(np.Cover)
	if h == s.song.coverHash || s.staleCover(np, h) {
		return
	}
	s.setCover(np, h)
	s.emitSongInfo()
}

// staleCover 判断存储里这张图是不是上一首留下的：换歌后新存储先带着新标题建出来，图却常常还是上一首的，
// 约 0.4s 后才换成自己的（2026-10-08 真机，5 次切歌 2 次：新歌先带着上一首的图宣告，0.4s 后再补一条）。
// 与上一首发出的那张逐字节相同、专辑又不同，就先不发；图换了盯梢会发现，立刻补发。同一张专辑连着放
// 是同一张图，照发；两张专辑真用了同一张图的，过了 coverHold 也照发。
func (s *session) staleCover(np mem.NowPlaying, h uint64) bool {
	return h == s.prevCover && np.Album != s.prevAlbum && s.now().Sub(s.songChangedAt) < coverHold
}

func (s *session) setCover(np mem.NowPlaying, h uint64) {
	s.song.coverHash = h
	s.song.coverURI = "data:" + np.CoverMIME + ";base64," + base64.StdEncoding.EncodeToString(np.Cover)
	s.needCover = false
}

// ── 事件 ──

// announce 宣告当前曲目：song_info_update + all_lyrics + 状态。
func (s *session) announce() {
	log.Info("♪ Song: %s", buildTitle(s.song.title, s.song.artist))
	if np := s.store; np.Cover != nil && np.Title == s.song.title && np.Artist == s.song.artist {
		if h := hashBytes(np.Cover); !s.staleCover(np, h) {
			s.setCover(np, h)
		}
	}
	s.emitSongInfo()
	s.announced = true
	s.noSongShown = false
	s.lastLineIdx = -1
	s.emitAllLyrics()
	if !s.needLyrics && len(s.song.lyrics) == 0 {
		s.emitVerdict() // 取到的是「纯音乐」提示：已经知道没有歌词，不必等宽限期
	}
	s.emitStatus()
}

// emitSongInfo 发歌曲信息。封面来自内存里的图片字节，没有 URL：cover 恒为空串，只有 cover_base64。
func (s *session) emitSongInfo() {
	s.p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
		Name: s.song.title, Singer: s.song.artist, Title: buildTitle(s.song.title, s.song.artist),
		CoverBase64: s.song.coverURI,
	})
}

// emitAllLyrics 发当前曲目的完整歌词（还没有歌词时发空列表：清掉上一首的歌词、带上新时长）。
func (s *session) emitAllLyrics() {
	pos := float32(s.clk.live(s.now()))
	dur := float32(s.song.dur)
	lyrics := s.song.lyrics
	items := toLyricLines(lyrics, s.offsetSec) // 空时是显式空 slice——nil 会序列化成 null
	if len(lyrics) > 0 {
		log.Info("Lyrics loaded: %d lines; per-character: %s", len(lyrics), detailedFlag(lyrics))
	}
	s.p.Emit(player.EventAllLyrics, &player.AllLyricsData{
		Title:    buildTitle(s.song.title, s.song.artist),
		Duration: dur,
		Position: pos,
		Progress: player.ClampProgress(pos, dur),
		Lyrics:   items,
		Count:    len(items),
	})
}

// emitVerdict 发「这首歌没有歌词」：index:-1 的 lyric_update，每首歌至多一次。之后歌词若到了，照常补发
// all_lyrics。
func (s *session) emitVerdict() {
	s.verdictSent = true
	log.Info("Instrumental / no lyrics; word-by-word: %s", detailedFlag(s.song.lyrics))
	pos := float32(s.clk.live(s.now()))
	// index:-1 的 lyric_update 必须显式带 Progress（契约：整首播到哪；lyricupdate_lint 全仓强制）。
	s.p.Emit(player.EventLyricUpdate, &player.LyricUpdate{
		Index: -1, Text: s.song.pureHint, Timestamp: 0, Position: pos, Progress: player.ClampProgress(pos, float32(s.song.dur)),
	})
}

// checkVerdict 宽限期到了还没有歌词：发「无歌词」。加载中、未宣告时不判。
func (s *session) checkVerdict() {
	if !s.announced || s.verdictSent || !s.needLyrics || s.loading {
		return
	}
	if s.now().Sub(s.songChangedAt) >= lyricGrace {
		s.emitVerdict()
	}
}

// emitStatus 按派生播放态发 playing / paused（Detail 是歌名）。没有位置结构时一律 paused。
func (s *session) emitStatus() {
	status := "paused"
	if s.twin != 0 && s.clk.playing {
		status = "playing"
	}
	s.p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: status, Detail: buildTitle(s.song.title, s.song.artist)})
}

// emitTime 发 playback_resume / playback_pause。
func (s *session) emitTime(evt string) {
	pos := float32(s.clk.live(s.now()))
	s.p.Emit(evt, &player.PlaybackTimeInfo{Position: pos, Progress: player.ClampProgress(pos, float32(s.song.dur))})
}

// showNoSong 报「进程在、没有歌」（waiting_song + no_song），只报一次。
func (s *session) showNoSong() {
	if s.noSongShown {
		return
	}
	s.noSongShown = true
	s.p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song", Detail: i18n.T("Apple Music not playing"), Reason: player.ReasonNoSong})
}

// clearSong 曲目没了：清屏并把当前曲目整个复位。复位是要害——旧实现只清屏不复位，同名歌再出现时
// 被当成同一首，歌曲信息、歌词与状态都不再发（审查 P1-6）。
//
// 「最近一次发出的封面」不随之清掉：清屏后的新歌同样可能先带着上一首的图建出存储（见 staleCover），
// 抹掉它，新歌就带着上一首的封面宣告。
func (s *session) clearSong() {
	if s.song.coverHash != 0 {
		s.prevCover, s.prevAlbum = s.song.coverHash, s.song.album
	}
	s.song = songState{}
	s.announced = false
	s.needTitle, s.needLyrics, s.needCover, s.needConfirm = false, false, false, false
	s.songScanAt = time.Time{}
	s.lastLineIdx = -1
	s.clk.playing = false
	s.showNoSong()
	s.p.Emit(player.EventClearSongData, nil)
}

// emitLyricLine 本地时钟插值 → 歌词行匹配 → lyric_update。
//
// 行号只往前走，除非刚发生跳转（lastLineIdx 被置 -1）：位置源是整秒采样，校正会把外推时钟往回拉
// 零点几秒，行阈值恰在其间时旧实现会在两行间来回切（审查方真机见过 41、42 行 0.6 秒内往返）。
func (s *session) emitLyricLine() {
	if !s.announced || s.loading || len(s.song.lyrics) == 0 {
		return
	}
	interp := float32(s.clk.live(s.now()))
	idx, ok := nextLine(s.song.lyrics, player.LyricMatchTime(interp, s.offsetSec), s.lastLineIdx)
	if !ok {
		return
	}
	s.lastLineIdx = idx
	line := s.song.lyrics[idx]
	// romaText 恒空：lyric 包不解析 TTML 里的音译轨。
	s.p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(
		idx, line.Time, line.Text, line.SubText, "", line.Detailed,
		s.offsetSec, interp, float32(s.song.dur),
	))
}

// emitPositionSample 播放中按固定间隔报一次位置采样（player.BaseEmitter.EmitPositionSample），只供
// 服务端校准缓存里的位置。读数走得比墙钟慢时 clock 把位置夹回去、不发事件（clock.observe 返回
// evNone，clock.live 停在上限）；无歌词的歌发过 index:-1 之后又没有别的带位置的事件，不报采样，
// 服务端按墙钟外推的位置会一直超前下去。
//
// 只在本曲已宣告、不在加载中、时钟在走时发：暂停中位置已由 playback_pause 给定；未宣告与加载中，
// 服务端缓存里的时长与歌词还是上一首的。位置结构丢失时 dropTwin 已把时钟置为不在走。
// 传的是 clk.live，与 emitLyricLine、emitTime、emitAllLyrics 的 position 同一个量，不含 offset。
func (s *session) emitPositionSample() {
	if !s.announced || s.loading || !s.clk.playing {
		return
	}
	now := s.now()
	s.p.EmitPositionSample(now, float32(s.clk.live(now)), float32(s.song.dur))
}

// nextLine 返回 t 时刻该推送的行：与上次相同、或比上次靠前（校正回拉）都不推；last 为 -1（刚换歌或
// 刚跳转）时任意行都推。
func nextLine(lines []krc.Line, t float32, last int) (int, bool) {
	idx := lineAt(lines, t)
	if idx < 0 || idx == last || (last >= 0 && idx < last) {
		return idx, false
	}
	return idx, true
}

// lineAt 返回 t 时刻所在的行（最后一个起点 ≤ t 的行）；还没到第一行返回 -1。
func lineAt(lines []krc.Line, t float32) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if t >= lines[i].Time {
			return i
		}
	}
	return -1
}

// ── 纯函数：挑当前歌的元数据与歌词 ──

// chainID 是一份存储的内容标识，配合地址判断「这还是上一首时就在的那份吗」。
func chainID(np mem.NowPlaying) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%d", np.Title, np.Artist, np.Album, np.Full, len(np.Cover))
}

// pickNowPlaying 从一次全扫的全部「正在播放」存储里挑当前歌，并返回代表它的那一份（盯梢与取封面用）。
// 按 (标题, 歌手, 专辑) 归并后依次比：
//
//  1. 不是上一首期间就在的完整副本（带媒体类别段，Full）数——换歌时新歌的完整副本在新地址重建，或把
//     原地址就地改写成新歌（内容标识变了，同样算新的）；上一首的若还没释放（地址与内容都没变，prev
//     里有它）不该压过新的；
//  2. 带封面的完整副本数——不带封面的完整副本会残留（见下）；
//  3. 完整副本数；
//  4. 歌手非空（加载占位的歌手是空的）；
//  5. 不是上一首期间就在的副本数、总票数；
//  6. 是否与当前曲目相同（保持稳定）、字典序。
//
// 结果确定，不随 map 迭代序变化（审查 P2：旧实现平票取任一，同一份内存可能先后选出不同标题）。
//
// 完整副本排在票数前面是真机逼出来的（2026-10-08）：内存里残留着早先播过的歌的 2 份非完整副本，
// 当前歌只有 1 份完整副本，按票数挑三次换歌三次宣告错。当晚 20 余次换歌里，所见任一时刻完整副本都只有
// 一份、且就是当前歌；上一首的完整副本在换歌后约 0.1s 内被释放，或丢了链头、退化成一份非完整副本。
//
// 2026-10-09 真机（逐秒采样全部存储，10 分钟、7 次换歌）见到了反例，「完整副本只有一份、就是当前歌」
// 不成立。完整副本分两种：
//
//   - 带封面的（地址都落在页边界）：全部采样里都是当前歌名。换歌时旧地址常被就地改写成新歌名——封面
//     要换的，这一份暂时还带着上一首的图、约 1s 内释放，同时另建一份带新封面的。
//   - 不带封面的：会残留。一份《crossing field》的留了 2 分 40 秒，其间当前歌依次换成《过火》《crossing
//     field》《信仰》。
//
// 于是《过火》播放中，内存里是「过火（完整、带封面）+ crossing field（完整、不带封面）」两份。服务恰在
// 这时重启：确认位置结构之前的全扫 prev 为空、两份都算新的，确认之后它们又都成了「上一首期间就在的」
// （那几次全扫已见过），两种情形下都是各一份完整副本、各一票，落到字典序，宣告成 crossing field。连续
// 运行时同一首从头再放、加载超时清屏后重新宣告，两份同样都是「上一首期间就在的」。带封面那一档为此而加。
//
// 它不排在第一位：换歌的头一瞬间可能只有新歌的不带封面副本是新的，而上一首带封面的还没被改写，这时
// 该取新的。
func pickNowPlaying(songs []mem.NowPlaying, prev map[uintptr]string, cur songState) (mem.NowPlaying, bool) {
	type tally struct {
		rep                                      mem.NowPlaying
		votes, full, fullCover, fresh, freshFull int
	}
	idx := map[[3]string]int{}
	var ts []tally
	for _, np := range songs {
		k := [3]string{np.Title, np.Artist, np.Album}
		i, ok := idx[k]
		if !ok {
			i = len(ts)
			idx[k] = i
			ts = append(ts, tally{rep: np})
		}
		t := &ts[i]
		t.votes++
		isFresh := prev[np.Addr] != chainID(np)
		if isFresh {
			t.fresh++
		}
		if np.Full {
			t.full++
			if isFresh {
				t.freshFull++
			}
			if np.Cover != nil {
				t.fullCover++
			}
		}
		// 代表：完整的优先，带封面的优先，再取地址小的。
		if betterRep(np, t.rep) {
			t.rep = np
		}
	}
	if len(ts) == 0 {
		return mem.NowPlaying{}, false
	}
	isCur := func(t tally) bool {
		return cur.resolved && t.rep.Title == cur.title && t.rep.Artist == cur.artist && t.rep.Album == cur.album
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.freshFull != b.freshFull {
			return a.freshFull > b.freshFull
		}
		if a.fullCover != b.fullCover {
			return a.fullCover > b.fullCover
		}
		if a.full != b.full {
			return a.full > b.full
		}
		if ae, be := a.rep.Artist != "", b.rep.Artist != ""; ae != be {
			return ae
		}
		if a.fresh != b.fresh {
			return a.fresh > b.fresh
		}
		if a.votes != b.votes {
			return a.votes > b.votes
		}
		if isCur(a) != isCur(b) {
			return isCur(a)
		}
		if a.rep.Title != b.rep.Title {
			return a.rep.Title < b.rep.Title
		}
		if a.rep.Artist != b.rep.Artist {
			return a.rep.Artist < b.rep.Artist
		}
		return a.rep.Album < b.rep.Album
	})
	return ts[0].rep, true
}

func betterRep(a, b mem.NowPlaying) bool {
	if a.Full != b.Full {
		return a.Full
	}
	if ac, bc := a.Cover != nil, b.Cover != nil; ac != bc {
		return ac
	}
	return a.Addr < b.Addr
}

// pickTTML 从候选的 TTML（一次全扫到的，加上留存的，见 lyricCandidates）里挑属于当前歌的那份并解析。
// 先过两道与归属无关的筛：<body dur> 不比位置结构时长长出 lyricDurSlack（只是上限，见该常量），最后
// 一行的起点不晚于音频结束。再看归属（owner，由 noteDocs 按文档最近一次出现新副本时的存储记下）：
//
//   - 记在当前歌名下的：用。
//   - 来历不明的（""）、附加时就在内存里的（preAttachOwner）：只有 <body dur> 落在
//     [时长, 时长+lyricDurSlack] 才用——这是别无凭据时唯一像样的证据；附加时就在的还要求 allowPreAttach
//     （只给本会话的第一首：它们能在内存里留几分钟、跨好几首）。
//   - 记在别的歌名下的：不用。上一首的 TTML 换歌后会残留数秒到十几秒，副本数还可能比新歌多（审查方
//     真机：两首各 4 份），旧实现按副本数挑，5 次换歌 4 次配上了第一首的歌词。
//
// 都满足的里依次比：记在当前歌名下、时长落在严格窗口内、与时长的差、副本数、全文长度、哈希——结果
// 确定。都不满足就按（暂时）没有歌词处理：宁可没有，不配错。
func pickTTML(docs []mem.TTMLDoc, dur float64, owner map[uint64]string, key string, allowPreAttach bool) ([]krc.Line, uint64, bool) {
	type cand struct {
		lines  []krc.Line
		hash   uint64
		mine   bool
		strict bool
		off    float64
		count  int
		size   int
	}
	var cs []cand
	for _, d := range docs {
		docDur, ok := lyric.DocDuration(d.Text)
		if !ok {
			continue
		}
		diff := float64(docDur) - dur
		if !(diff <= lyricDurSlack) {
			continue
		}
		strict := diff >= 0
		h := hashDoc(d.Text)
		o, known := owner[h]
		mine := known && o == key
		switch {
		case mine:
		case !known, o == "":
			if !strict {
				continue
			}
		case o == preAttachOwner:
			if !strict || !allowPreAttach {
				continue
			}
		default:
			continue
		}
		lines := lyric.ParseTTML(d.Text)
		if len(lines) == 0 {
			continue // 实测偶见解析出 0 行的变体文档
		}
		if !(float64(lines[len(lines)-1].Time) <= dur+1) {
			continue
		}
		cs = append(cs, cand{lines: lines, hash: h, mine: mine, strict: strict, off: math.Abs(diff), count: d.Count, size: len(d.Text)})
	}
	if len(cs) == 0 {
		return nil, 0, false
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.mine != b.mine {
			return a.mine
		}
		if a.strict != b.strict {
			return a.strict
		}
		if a.off != b.off {
			return a.off < b.off
		}
		if a.count != b.count {
			return a.count > b.count
		}
		if a.size != b.size {
			return a.size > b.size
		}
		return a.hash < b.hash
	})
	return cs[0].lines, cs[0].hash, true
}

func hashDoc(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// ── 纯逻辑：整秒位置读数 → 连续时钟 + 播放态 ──

// 位置读数相对真实播放位置的滞后（2026-10-08 真机：一边抓 AMPLibraryAgent.exe 的出声，一边每 5ms 读一次
// 位置结构；手动切歌 24 次、自然播完切歌 2 次、拖动 2 次）：
//
//   - 读数跳到 N 的时刻，真实位置是 N+0.38 ~ N+0.64 秒。Apple Music 约每 256ms 才刷新一次位置、且本身带
//     约 0.38s 的固定滞后：相邻两跳多为 1.02~1.03s，每一跳比上一跳多晚约 26ms，攒够一个刷新周期就来一次
//     约 0.77s 的追平。以出声为准量到的滞后在各种换歌方式下一致（同一首歌三次：0.25~0.51、0.25~0.48、
//     0.26~0.49s，未扣抓音频本身的延迟）。
//   - 时长由 0 变成新值的那一刻大致是开播，但不准：时长也是按刷新周期写的。那一刻的真实位置，自然播完
//     切到下一首时约 -0.05s，联网加载的歌约 -0.01s，已缓存的歌（归零恰好 0.5s）约 +0.18s。之后第一跳
//     （读数变 1）在 1.28~1.54s 来。
//   - 拖动进度条：读数先闪一下目标值又弹回原来的时间线，之后才真正落到目标；落下去的那一刻，真实位置
//     约为目标值 +0.3s（两次：+0.30、+0.33）。2026-10-09 另用每 2ms 读一次的采样量过各段的长短（播放中
//     拖动 13 次）：闪现持续到 Apple Music 下一次刷新位置为止，11 次是 10~14ms，2 次是 219、225ms；弹回
//     7 次是 9~101ms，5 次是 514~518ms（两个刷新周期），1 次没落下去就被下一次拖动取代；落点 6 次
//     直接是目标，2 次比目标少一、247ms 后进一格，3 次在目标上只停 11~12ms 就退一格、约 760ms 后进
//     回来，1 次落在目标减一、534ms 后再退一格、245ms 后进回来。暂停中拖动的读数见 clock.observe。
//
// 旧实现把读数 N 当成位置 N，再加上 300ms 轮询在「看见跳变」与「推歌词行」两处各晚 0~0.3s。新旧两版
// 对同一段播放并排跑（2026-10-08）：歌词行相对真实位置，旧版平均晚 0.80s（0.57~0.94），新版 +0.05s
// ——审查方实测要把 offset 从 400 调到 1200 才对得上，差的就是这 0.8s。
const (
	flipLagMin = 0.38
	flipLagMax = 0.63
	flipLagMid = 0.5
	// seekLag 播放中跳转，读数变成目标值的那一刻真实位置比它多这么多秒（见上）。
	seekLag = 0.3
)

// pauseThreshold 位置停更多久判为暂停。播放中相邻两跳最长约 1.06s；取 1.6s 留足余量。代价是暂停
// 最长约 1.6s 后才被发现。
const pauseThreshold = 1600 * time.Millisecond

// startPauseThreshold / startWindow 开播（clock.start / reset）或播放中跳转确认后 startWindow 内用更宽的
// 暂停门槛（起点记在 clock.relaxAt）：开播后第一跳 1.4~1.5s 才来，新歌开头缓冲时位置还会走一两秒又停住
// 一两秒。2026-10-08 端到端真机复跑：一首新歌「暂停 @1s → 恢复 @2s」，就是开头卡了 1.6s 以上被当成暂停，
// overlay 闪一下 paused。跳转之后同样：同日真机拖动一次进度条，音频与波形都没断，却在跳转约 2s 后报了
// 暂停、再过 1s 报恢复，其间接着的拖动都被当成暂停中拖动。代价是这几秒里的真暂停最多晚 1.4s 发现。
const (
	startPauseThreshold = 3 * time.Second
	startWindow         = 8 * time.Second
)

// seekJumpSec 播放中新读数与外推位置差超过此秒数判为跳转。
const seekJumpSec = 3.0

// jumpHold 跳转目标的读数要持续这么久才认（见 clock.holdJump）。须长于拖动时目标值闪现的时间：闪现
// 持续到 Apple Music 下一次刷新位置为止（约每 256ms 刷新一次），实测最长 225ms（见 seekLag 上方）。
// 原先取 40ms，只挡得住十几毫秒的闪现：约 220ms 的闪现被认成跳转，随后约 515ms 的弹回被认成跳回，
// 落到目标再认一次，一次拖动发出三条 playback_resume、原位置的歌词行又被推一遍（2026-10-09 真机：
// 52.35 → 160.35 → 52.35；238.35 → 221.35 → 238.35）。
//
// 代价：跳转在读数稳定约 0.3s 后才报（原先约 0.05s），playback_resume 与目标位置的歌词行都晚这么多；
// 位置仍从第一次读到目标值的时刻推算，不因此变差。暂停中拖动同此；拖完立刻播放、读数在这 0.3s 内
// 又进了一格的，这一格并入待确认的目标、不算恢复的第一跳，恢复晚一跳才判出（2026-10-09 的采样里
// 没有这样的时序）。
const jumpHold = 300 * time.Millisecond

// tickSlackSec 暂停中位置前进不超过「距上次读数的墙钟秒数 + tickSlackSec」才算「一跳」（播放推进），
// 其它变化是暂停中拖动。按墙钟比而不是固定门槛：轮询调慢时（配置上限 2s）播放中每次读数就跳 2~3s，
// 固定 2.5s 门槛会把恢复一直当成拖动（复查实测轮询 3s 时永不恢复）。1.5 覆盖整秒量化与抖动。
const tickSlackSec = 1.5

// resumeConfirm 暂停中两跳相隔不超过「此值 + 一个读数间隔」才判恢复。只凭一跳会把暂停中往前拖一两秒
// 当成恢复，报出 1.6s 假 playing、让路由把根输出切过来（复查实测）。播放中两跳相隔约 1s，留足余量；
// 代价是恢复晚报约 1s，位置不受影响（判定那一刻落锚）。
const resumeConfirm = 2500 * time.Millisecond

type clockEvent int

const (
	evNone       clockEvent = iota
	evResume                // 暂停 → 播放
	evPause                 // 播放 → 暂停
	evSeek                  // 播放中跳转
	evPausedSeek            // 暂停中跳转：只重锚，不发事件
)

// clock 把整秒读数变成连续时钟：位置 = 锚点 + 墙钟流逝，读数每跳一次，把它**夹**回读数允许的范围
// [N+flipLagMin, N+flipLagMax]，在范围内就不动——不是每跳一次都落锚到固定值，那样位置会随跳变时刻的
// 抖动每秒被前后拽 0.25s（歌词行跟着早晚不定）。
//
// 起点（开播、中途接上、恢复、跳转）只是个估计，差 0.1~0.3s 都可能；靠的是之后的夹：滞后最小的那一跳
// （追平之后）把偏慢的拉上来，滞后最大的那一跳（追平之前）把偏快的压下去，一轮追平（约 10 秒）之内
// 收敛到 -0.03 ~ +0.06s，此后整首不再动。
type clock struct {
	lastRaw    float64 // 上一次见到的读数；<0 = 未见过
	lastChange time.Time
	anchorPos  float64
	anchorAt   time.Time
	playing    bool
	dur        float64
	tickAt     time.Time // 暂停中第一跳的时刻；零值 = 没有待确认的一跳
	lastObs    time.Time // 上一次 observe 的时刻（不论读数变没变）
	relaxAt    time.Time // 开播（start / reset）或播放中跳转确认的时刻，见 startWindow
	jumpRaw    float64   // 待确认的跳转目标：播放中的跳转、暂停中的拖动共用（见 holdJump）
	jumpAt     time.Time // 第一次读到它的时刻；零值 = 没有待确认的跳转
}

// start 从开播起算：now 是看到时长由 0（或上一首的值）变成新值的那一轮，位置从 0 起按墙钟走。那一刻的
// 真实位置实测在 -0.05 ~ +0.18s，不必更细：第一跳就会把它夹进范围。
func (c *clock) start(now time.Time, dur float64) {
	*c = clock{lastRaw: 0, lastChange: now, anchorPos: 0, anchorAt: now, playing: true, dur: dur, lastObs: now, relaxAt: now}
}

// reset 中途接上（附加时歌已在放、位置结构丢了又找到）：读数是 raw，位置按 est 估。
func (c *clock) reset(raw, est float64, now time.Time, playing bool, dur float64) {
	*c = clock{lastRaw: raw, lastChange: now, anchorPos: est, anchorAt: now, playing: playing, dur: dur, lastObs: now, relaxAt: now}
}

// live 返回 now 时刻的整曲位置：播放中 = 锚点 + 墙钟流逝，暂停 = 锚点；有时长则钳到时长。
//
// 播放中不超过 lastRaw+1+flipLagMax：真实位置若过了它，读数早该跳到下一格了。读数不跳（缓冲、暂停
// 尚未判出）时位置就停在这里，不往前冲。
func (c *clock) live(now time.Time) float64 {
	v := c.anchorPos
	if c.playing {
		v += now.Sub(c.anchorAt).Seconds()
		if lim := c.lastRaw + 1 + flipLagMax; c.lastRaw >= 0 && v > lim {
			v = lim
		}
	}
	if c.dur > 0 && v > c.dur {
		v = c.dur
	}
	return v
}

// freeze 按暂停冻结：读数停在 lastRaw 之后，真实位置在 lastRaw+flipLagMin ~ lastRaw+1+flipLagMax 之间
// 的某处停下，取中间（lastRaw+1），且不超过已经外推到的位置。
func (c *clock) freeze(now time.Time) {
	c.anchorPos = math.Min(c.live(now), c.lastRaw+1)
	c.anchorAt = now
	c.playing = false
	c.jumpAt = time.Time{}
}

// observe 喂一次读数，返回由此引起的状态变化。seek 判定只在播放中做：暂停时外推不走，拿阈值去判会
// 把暂停中拖动和恢复后的第一跳混在一起。暂停中拖动只重锚、不发事件；恢复要连着两跳（见 resumeConfirm），
// 判定时只发一次 playback_resume（审查 P1-7：旧实现暂停中拖动会在同一轮发两次）。
//
// 播放中的跳转要目标读数持续满 jumpHold 才认（见 holdJump）：拖动时读数会先闪一下目标值又弹回原来的
// 时间线（见 seekLag 上方的实测），把闪现也认了，一次拖动会连发三条 playback_resume、歌词行跟着来回跳
// （2026-10-08 真机：285.5 → 24.5 → 285.5，半秒之内）。代价是跳转在读数稳定约 0.3s 后才报（见 jumpHold）。
//
// 暂停中的拖动走同一道确认：读数同样会闪，变化即重锚时一次拖动依次推出目标行、原位置的行、目标行
// （2026-10-08 真机：暂停在 212s 拖到约 63s，lyric_update 的 index 依次为 6、29、6）。暂停中的「一跳」
// （判恢复用的那种前进，见 tickSlackSec）不经这道确认，处理照旧。
//
// 已知现象，不处理：暂停中拖动一次，可能先后推出两处的歌词行。2026-10-09 每 2ms 读一次的采样里，三次
// 暂停中拖动的读数依次是「目标 T 闪现 12~13ms → 原值 0.6~1.7s → 落点 V 闪现 11~12ms → T 约 1.1s → V 约
// 245ms → V−1 约 512ms → V」，V 比 T 靠前 3~10s（三次拖完都接着播放了，此后每约 1.02s 进一格）。T 停了
// 约 1.1s，长于 jumpHold，分不出它是过渡：先按 T 重锚，V−1 停满 jumpHold 后再按它重锚。暂停不足
// pauseThreshold 就拖动时还按播放中处理，这两处各发一条 playback_resume。
func (c *clock) observe(raw float64, now time.Time) clockEvent {
	sinceObs := now.Sub(c.lastObs)
	c.lastObs = now
	// 待确认的跳转须每一次读数都指向它：先取下，这一次读数仍指向它才由 holdJump 续上——弹回原值、
	// 落回范围内、判了暂停，都就此作废。
	pendingAt := c.jumpAt
	c.jumpAt = time.Time{}
	switch {
	case raw == c.lastRaw:
		threshold := pauseThreshold
		if now.Sub(c.relaxAt) < startWindow {
			threshold = startPauseThreshold
		}
		if c.playing && now.Sub(c.lastChange) >= threshold {
			c.freeze(now)
			return evPause
		}
		return evNone
	case c.playing:
		// 这一跳发生在上一次读数之后的某一刻：真实位置在 [raw+flipLagMin, raw+flipLagMax+距上次读数]。
		est := c.anchorPos + now.Sub(c.anchorAt).Seconds()
		lo, hi := raw+flipLagMin, raw+flipLagMax+sinceObs.Seconds()
		if est < lo-seekJumpSec || est > hi+seekJumpSec {
			if !c.holdJump(raw, now, pendingAt) {
				return evNone // 还没持续够：读数与位置都不动
			}
			// 位置从第一次读到目标值的那一刻推算（那一刻是 目标+seekLag）。持续期间读数进了一格的（落点
			// 有时比目标少一、约 250ms 后进一格），进这一格时真实位置不低于 lo，只按第一次读到的值会慢约 1s。
			c.anchorPos, c.anchorAt = math.Max(c.jumpRaw+seekLag+now.Sub(c.jumpAt).Seconds(), lo), now
			c.lastRaw, c.lastChange, c.jumpAt = raw, now, time.Time{}
			c.relaxAt = now // 跳转后的下一跳与开播后的第一跳一样来得晚，见 startWindow
			return evSeek
		}
		c.lastRaw, c.lastChange = raw, now
		switch {
		case est < lo:
			c.anchorPos, c.anchorAt = lo, now
		case est > hi:
			c.anchorPos, c.anchorAt = hi, now
		}
		return evNone
	default:
		d := raw - c.lastRaw
		tick := c.lastRaw >= 0 && d > 0 && d <= sinceObs.Seconds()+tickSlackSec
		if !tick && !c.holdJump(raw, now, pendingAt) {
			return evNone // 暂停中拖动，目标值还没持续够：读数与位置都不动
		}
		c.lastRaw, c.lastChange, c.jumpAt = raw, now, time.Time{}
		c.anchorPos, c.anchorAt = raw+flipLagMid, now
		if tick {
			if !c.tickAt.IsZero() && now.Sub(c.tickAt) <= resumeConfirm+sinceObs {
				c.playing, c.tickAt = true, time.Time{}
				return evResume
			}
			c.tickAt = now // 第一跳：可能是恢复，也可能是往前拖了一点，等下一跳
			return evNone
		}
		c.tickAt = time.Time{}
		return evPausedSeek
	}
}

// holdJump 记一次指向跳转目标 raw 的读数，返回该目标是否已持续满 jumpHold。pendingAt 是此前待确认的
// 目标第一次读到的时刻（零值 = 没有）；raw 不比它靠前、也没超出它这段时间里自己能走到的地方，才算
// 同一个目标（落点比目标少一、约 250ms 后进一格的，仍是同一个），否则从这一次重新记。
//
// 看持续多久而不是读到几次：闪现最长约 220ms（见 jumpHold），按 50ms 轮询能连着读到四五次，全扫途中
// 约每 20ms 读一次位置（scanTick）时更多。播放中的跳转与暂停中的拖动共用。
func (c *clock) holdJump(raw float64, now, pendingAt time.Time) bool {
	if d := raw - c.jumpRaw; pendingAt.IsZero() || d < 0 || d > now.Sub(pendingAt).Seconds()+tickSlackSec {
		c.jumpRaw, c.jumpAt = raw, now
		return false
	}
	c.jumpAt = pendingAt
	return now.Sub(pendingAt) >= jumpHold
}

// ── 退避 ──

// backoff 是一个按档递增的定时器：reset 后立即到期；每次 advance 从调用时刻（扫描结束）起算下一档，
// 档用完停在最后一档。next 为零值表示停用。
type backoff struct {
	steps []time.Duration
	next  time.Time
	step  int
}

func (b *backoff) reset(now time.Time) { b.step, b.next = 0, now }
func (b *backoff) stop()               { b.next = time.Time{} }
func (b *backoff) due(now time.Time) bool {
	return !b.next.IsZero() && !now.Before(b.next)
}
func (b *backoff) advance(now time.Time) {
	i := b.step
	if i >= len(b.steps) {
		i = len(b.steps) - 1
	}
	b.next = now.Add(b.steps[i])
	b.step++
}

// ── 小工具 ──

// buildTitle 组「歌手 - 歌名」（与 kugou/soda 一致）。歌手为空则只留歌名。
func buildTitle(name, singer string) string {
	if singer == "" {
		return name
	}
	return fmt.Sprintf("%s - %s", singer, name)
}

// toLyricLines 把解析结果映射成对外 LyricLine（subText/detailed 透传给 BuildLyricLine）。
func toLyricLines(lines []krc.Line, offsetSec float32) []player.LyricLine {
	if len(lines) == 0 {
		return []player.LyricLine{}
	}
	out := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		// romaText 恒空：lyric 包不解析 TTML 里的音译轨，roma_text 对 applemusic 恒为 ""。
		out[i] = player.BuildLyricLine(l.Index, l.Time, l.Text, l.SubText, "", l.Detailed, offsetSec)
	}
	return out
}

// applyDetailedOffset 把 offset 同时作用到行级与逐字的 play_time（解析后、存进当前曲目前套一次）。
// BuildLyricLine/BuildLyricUpdate 只算行级 play_time，TextDetailed 原样透传，故逐字须在此单独套
// （对照 kugou/soda 的同名函数），否则逐字高亮整体偏一个 offset。
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
			return i18n.T("yes")
		}
	}
	return i18n.T("no")
}
