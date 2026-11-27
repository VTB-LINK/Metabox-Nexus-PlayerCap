package qqmusic

// 本文件是歌词关联的后台读取者（设计 §6.8「读取者」）：每个会话一个 goroutine，承担定位缓存目录（§6.2）、读关联表与
// 歌词索引（§6.3）、编号补全的兜底请求与当前歌曲记录的读取（§6.4，读内存的部分在 songrecord.go）、本地文件认定（§6.5）与
// 读关联歌词（§6.6）的全部 IO，按会话写入的当前歌曲给出期望状态。会话的轮询线程只 Submit、非阻塞地取结果，不做其中任何一项。

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
	"Metabox-Nexus-PlayerCap/player/restartmgr"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// 读取者的节奏与上限（设计 §6.2～§6.5、§6.8、§9 第 5 项）。
const (
	assocTick         = time.Second     // stat 关联表的间隔，也是（大小，修改时间）须稳定的时长
	assocPendingEvery = 5 * time.Second // 有文件暂缺时重读的间隔
	assocBackoffMin   = time.Second     // 关联表暂时性失败的退避：1、2、4、8 秒，之后保持 8 秒
	assocBackoffMax   = 8 * time.Second
	fillBackoffMin    = 2 * time.Second // 编号兜底请求失败的退避：2、4、8…秒，至多 60 秒（设计 §6.4 第 2 步「兜底」）
	fillBackoffMax    = time.Minute
	relocateEvery     = 10 * time.Second // 关联表不存在时重新定位的间隔
	maxDatBytes       = 16 << 20         // 三个 .dat 各自的上限
	maxAssocDetails   = 4                // 每首歌至多打这么多条 Detail，同一原因只打一条
	recordReadsMax    = 6                // 每个代次读当前歌曲记录的次数上限：日程 5 次、补读 1 次（设计 §6.4 第 4 步「何时读」）
)

// localAttemptAt 是换歌（或关联表变化）之后做本地文件认定的各个时刻（设计 §6.5 第 3 步），也是一轮当前歌曲记录读取的
// 各个时刻（设计 §6.4 第 4 步「何时读」，自本代次首次有候选时起）。
var localAttemptAt = [...]time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// assocReaders 是存活的读取者 goroutine 数，startAssocReader 保证至多为 1。
var assocReaders atomic.Int32

// reportOnce 即 telemetry.ReportOnce，测试替换以计数。
var reportOnce = telemetry.ReportOnce

// songKey 是会话写入的当前歌曲。gen 在每次换歌时加一，reopen 随之归零；同一首补齐在线编号时 gen 与 reopen 不变（#248）；
// reopen 在同一首重开时加一，gen 不变（设计 §6.8「输入」「重开」）。（gen，reopen）合称代次，结果原样带回；读取者每个代次
// 读一轮当前歌曲记录。songID 为 0、songMid 为空即缺位；本地文件认定不论有无编号都做（设计 §6.5）。name 是会话认领的歌名
// （经 sanitizeString）。excludeLocal 是上一首最终采纳的本地文件，认定时不注册它（设计 §6.5 第 5 步）。detail 是会话按本
// gen 的封面请求补全另一个编号的详情结果（设计 §6.4 第 2 步「交接」），零值即尚无；代次不变时会话可补齐编号或更新它再写一次。
type songKey struct {
	gen, reopen   uint64
	songID        uint32
	songMid, name string
	excludeLocal  string
	detail        songDetail
}

// hasIDs 报告编号是否已知（songid 或 songmid 至少有一个）：本地文件未认定时，已知按在线歌曲求结果，否则为无。会话传给
// lyricState 的 songHasIDs 也取它，两边口径一致。
func (k songKey) hasIDs() bool { return k.songID != 0 || k.songMid != "" }

// resultKind 是结果的种类（设计 §6.8「输出」）。
type resultKind int

const (
	resUnknown resultKind = iota // 关联表读不出、暂时性失败或尚未定位：会话保持当前歌词不动
	resStopped                   // 本会话不再使用本功能：会话按「无条目」处理；读取者发出后即退出
	resDesired                   // 期望状态
)

// lyricSource 是期望状态的歌词来源。
type lyricSource int

const (
	srcOnline lyricSource = iota // 在线取词的歌词：行由会话保存，结果不带行
	srcAssoc                     // 在线歌曲手动关联的歌词
	srcLocal                     // 本地文件关联的歌词
	srcNone                      // 没有歌词
)

// assocResult 是读取者给会话的一条结果，带求结果时的代次（gen，reopen）。lines 未做时间平移、未套 offset；pending 为真时
// 有文件暂缺，读取者每拍看文件变没变、另每 5 秒兜底重读，读到后发新的结果；localPath 是来源为本地文件时采纳的音频文件；
// digest 是来源、偏移与读成的歌词文件（原文、翻译、音译）内容的 SHA-256，输入相同即相同，供会话判断是否需要重发。种类不是
// 期望状态时只有代次有意义。
type assocResult struct {
	gen, reopen   uint64
	kind          resultKind
	source        lyricSource
	lyricOffsetMs int
	lines         []lyricLine
	pending       bool
	localPath     string
	digest        [32]byte
}

// assocDeps 是读取者的全部外部访问，测试以桩替换。read 的约定同 readShared，stat 同 os.Stat，holders 同
// restartmgr.Holders；resolveID 同 fetchSongMidByID、resolveMid 同 fetchSongIDByMid，自带核验，只用于编号补全的兜底请求
// （设计 §6.4 第 2 步「兜底」）。
// configDir 是 QQ 的配置目录（%APPDATA%\Tencent\QQMusic）；qqPID 是会话所连的 QQ 进程，本地文件认定按它过滤占用者；
// qqVersion 只用于上报。record 是读 QQ 内存里当前歌曲记录的来源（songrecord.go），nil 即本会话不读（本版本未核实记录格式或
// 复制句柄失败），有候选的在线歌曲一律按无条目；它的句柄由读取者退出时关闭，读取者没有起来时由 startAssocReader 当即关闭。
type assocDeps struct {
	configDir  string
	read       func(path string, max int64) ([]byte, error)
	stat       func(path string) (os.FileInfo, error)
	holders    assoc.Holders
	resolveID  func(songID uint32) (string, error)
	resolveMid func(mid string) (uint32, error)
	now        func() time.Time
	qqPID      uint32
	qqVersion  string
	record     *recordSource
}

// defaultAssocDeps 是出货代码的依赖。取不到 %APPDATA% 时配置目录不是绝对路径，读取者启动即停用。rec 见 assocDeps.record。
func defaultAssocDeps(pid uint32, version string, rec *recordSource) assocDeps {
	dir, _ := os.UserConfigDir()
	return assocDeps{
		configDir: filepath.Join(dir, "Tencent", "QQMusic"),
		read:      readShared, stat: os.Stat, holders: restartmgr.Holders,
		resolveID: fetchSongMidByID, resolveMid: fetchSongIDByMid,
		now: time.Now, qqPID: pid, qqVersion: version, record: rec,
	}
}

// assocReader 是会话与读取者 goroutine 之间的交接：会话以 Submit 写入当前歌曲，从 Results 取结果，经 LocalCheckPossible
// 查询本会话是否仍有可能认出本地文件关联（#248）。没有读取者（startAssocReader 返回 nil）时，nil 上的 Submit 什么也不做，
// Results 返回 nil 通道，LocalCheckPossible 返回假。
type assocReader struct {
	mu            sync.Mutex
	key           songKey // 读取者尚未取走的当前歌曲
	hasKey        bool
	wake          chan struct{}    // 容量 1，有新歌时唤醒读取者
	results       chan assocResult // 容量 1，只留最新一条
	localPossible atomic.Bool      // 关联表可能存在本地音频条目且本会话未停做本地认定（#248）
}

// LocalCheckPossible 报告本会话是否仍有可能认出本地文件关联（#248，设计 §2.3）：读取者启动时初值为真，首次表解析后按
// 「表可用、含至少一条本地条目、Restart Manager 未报致命错误且未停用」维护；nil 接收者返回假。
func (r *assocReader) LocalCheckPossible() bool {
	if r == nil {
		return false
	}
	return r.localPossible.Load()
}

// Submit 写入当前歌曲，覆盖读取者尚未取走的旧值。不阻塞，读取者退出之后调用也不阻塞。
func (r *assocReader) Submit(k songKey) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.key, r.hasKey = k, true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default: // 已有一个未处理的唤醒，读取者届时取到的就是这个新值
	}
}

// Results 返回结果通道：容量 1，只留最新一条（未知不顶掉同一代次尚未取走的结果，见 send）；不关闭。
func (r *assocReader) Results() <-chan assocResult {
	if r == nil {
		return nil
	}
	return r.results
}

func (r *assocReader) take() (songKey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.key, r.hasKey
	r.hasKey = false
	return k, ok
}

// pending 报告会话是否写入了读取者尚未取走的歌。扫描当前歌曲记录时在块间查它，有即中止扫描；只查不取，新写入留给 run 取走。
func (r *assocReader) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hasKey
}

// send 清空旧结果后发出 res。会话对未知不动作，旧结果与 res 同属一个代次（gen 与 reopen 都相同）而 res 为未知时保留旧结果，
// 免得顶掉尚未取走的期望状态；代次不同时照常顶掉，重开前残留的结果不留到重开之后。只有读取者 goroutine 发送，清空之后
// 必有空位；不阻塞是兜底。
func (r *assocReader) send(res assocResult) {
	select {
	case old := <-r.results:
		if res.kind == resUnknown && old.gen == res.gen && old.reopen == res.reopen {
			res = old
		}
	default:
	}
	select {
	case r.results <- res:
	default:
	}
}

// startAssocReader 启动本会话的读取者；ctx 取消即退出，阻塞在读文件、Restart Manager 查询、网络请求或内存读取里的在其返回后
// 退出、不再发结果。读当前歌曲记录的句柄（d.record）由读取者退出时关闭，先于计数归零。上一个读取者 goroutine 尚未退出时
// 不启动，当即关闭该句柄，返回 nil 与假，打一条 Detail。
func startAssocReader(ctx context.Context, d assocDeps) (*assocReader, bool) {
	if !assocReaders.CompareAndSwap(0, 1) {
		d.record.release()
		log.Detail("The previous session's lyric association reader has not exited; QQ Music lyric associations are not followed this session")
		return nil, false
	}
	r := &assocReader{wake: make(chan struct{}, 1), results: make(chan assocResult, 1)}
	r.localPossible.Store(true)
	go func() {
		defer telemetry.Guard() // 兜底：只有下面 recoverStop 自身 panic 才会走到它
		defer assocReaders.Add(-1)
		defer r.localPossible.Store(false)
		defer d.record.release()
		w := &assocWorker{r: r, d: d, ctx: ctx}
		defer w.recoverStop()
		w.run()
	}()
	return r, true
}

// fileStat 是一个文件的（大小，修改时间）；ok 为假即 stat 失败、不是普通文件，或没有 stat（ctx 已取消）。
type fileStat struct {
	size, mod int64 // mod 为修改时间的 UnixNano
	ok        bool
}

// watchedFile 是看守中的一个文件（设计 §6.4 第 6 步「重读看文件变没变」）：read 是上次读之前 stat 所得，prev 是最近一拍
// stat 所得。
type watchedFile struct{ read, prev fileStat }

// indexCache 是歌词索引的缓存（设计 §6.6 第 4 步）：ok 为真时 files 是读之前 stat 为 st 时解析所得的映射；ok 为假而 st.ok
// 为真即稳定的结构错误（两次读之前的 stat 都是 st、同一结构错误）。两者都在 stat 仍为 st 时直接用、不读。badStat、badMsg
// 是最近一次结构错误时读之前的 stat 与错误文本，供判稳定。
type indexCache struct {
	st      fileStat
	files   map[string]assoc.LyricFiles
	ok      bool
	badStat fileStat
	badMsg  string
}

// assocWorker 是读取者 goroutine 的状态，只由该 goroutine 访问。
//
// 关联表：启动时定位并解析一次；此后每拍 stat，变化且稳定即重读；读坏按退避重读；不存在时每 relocateEvery 重新定位。
// 解析成功之前与暂时性失败期间，给歌曲的结果为未知；判为不支持或内部 panic 即停用，发停用结果后退出。
// 歌曲：每次写入都重新求结果；读过的歌词文件与歌词索引（不论读成没读成）每拍 stat，变了且稳定即再求（watch），有文件暂缺时
// 另每 5 秒再求；本地文件不论有无编号都按 localAttemptAt 认定，认定时再求，认定之后（其条目仍在表里）以本地关联为准。本地
// 未认定的在线歌曲有候选时，每个代次按 localAttemptAt 读一轮当前歌曲记录（rec），本代次认出的网址变了时再求。缺的编号取会话
// 交来的详情结果，会话报已失败时自己兜底请求（fill、fillIDs），补上即再求；本地已认定时不读记录、不兜底。歌词索引按 stat
// 缓存（index），判为不可用的歌词文件按读前 stat 记下（unusable），未变都不再读。
type assocWorker struct {
	r   *assocReader
	d   assocDeps
	ctx context.Context

	tablePath  string // 定位所得的关联表路径，歌词索引在同一目录
	lastLocate time.Time
	locateErr  string // 上一次定位退回默认目录的原因；原因不变不再打 Detail
	entries    []assoc.Entry
	tableOK    bool     // 最近一次解析成功；为假时结果为未知
	parsed     fileStat // 最近一次解析之前 stat 所得
	prev       fileStat // 最近一拍 stat 所得
	nextTick   time.Time
	retryAt    time.Time     // 暂时性失败后重读的时刻；零值即不重读
	backoff    time.Duration // 最近一次暂时性失败的退避；0 即下一次从 assocBackoffMin 起
	badStat    fileStat      // 最近一次结构错误时读之前的（大小，修改时间），与 badMsg 一起见 parseFailed
	badMsg     string
	stopped    bool

	cur       songKey
	hasSong   bool
	pendingAt time.Time       // 有文件暂缺时重求结果的时刻；零值即不重求
	localPath string          // 本首认定的本地文件；空即未认定
	localBase time.Time       // 本地认定日程的起点：换歌或关联表变化之时
	localStep int             // 下一次本地认定在 localAttemptAt 中的下标
	localOff  bool            // Restart Manager 报致命错误后，本会话不再做本地认定
	logged    map[string]bool // 本首歌已打过 Detail 的原因
	rec       recordState     // 本代次读当前歌曲记录的状态，代次变化即清空
	fill      fillState       // 本 gen 编号兜底的状态，gen 变化即清空，reopen 变化保留

	watch    map[string]watchedFile // 最近一次求结果读过的各轨与歌词索引（至多 4 个，不论读成没读成），键为路径；evaluate 清空、loadLyrics 重建
	unusable map[string]fileStat    // 最近一次读关联歌词判为文件本身不可用的各文件的读前 stat，键为路径；loadLyrics 重建
	index    indexCache             // 歌词索引的缓存
}

// fillState 是一个 gen 编号兜底请求的状态（设计 §6.4 第 2 步「兜底」）。at 是下一次请求的时刻：本 gen 首次收到已失败时为当时，
// 失败后按 backoff 推后；got 是兜底所得，已得即不再请求。
type fillState struct {
	at      time.Time
	backoff time.Duration
	got     songDetail
}

// recordState 是一个代次读当前歌曲记录的状态（设计 §6.4 第 4 步「何时读」）：本代次首次有候选时当即读第 0 次，此后按
// localAttemptAt 读完，认出之后照读（复扫），至多 recordReadsMax 次；各次的结论经 assoc.MergeConclusion 并成本代次的结论，
// 缓存到代次变化，关联表变化只按它重选、不重读。
type recordState struct {
	base     time.Time              // 本轮日程的起点；零值即本轮尚未开始
	step     int                    // 下一次按日程读在 localAttemptAt 中的下标
	reads    int                    // 本代次已读的次数（中止的不计）
	conc     assoc.RecordConclusion // 本代次的结论；尚未读、或各次都读不出前缀或歌名不符时为零值（RecordNone）
	rereadAt time.Time              // 补读的时刻（idsUpdated）：待换算之后所缺的编号补上，或已认出而已在用的编号换了值之时；零值即没有
}

// run 定位并解析关联表，然后等会话写入歌曲与各项定时处理，直到取消或停用。每轮先处理新写入的歌，否则只做一项到期的
// 定时处理就回到等待，新歌不必等其余各项做完。
func (w *assocWorker) run() {
	if !filepath.IsAbs(w.d.configDir) {
		w.stop(i18n.Errorf("the QQ Music configuration directory is unknown"))
		return
	}
	w.locate()
	if w.ctx.Err() != nil {
		return
	}
	log.Info("QQ Music lyric association enabled (lyric directory %s)", filepath.Dir(w.tablePath))
	w.prev = w.statTable()
	w.nextTick = w.d.now().Add(assocTick)
	if w.prev.ok {
		w.parse()
	} else {
		w.updateLocalPossible()
	}
	for !w.stopped {
		timer := time.NewTimer(w.next().Sub(w.d.now()))
		select {
		case <-w.ctx.Done():
		case <-w.r.wake:
		case <-timer.C:
		}
		timer.Stop()
		if w.ctx.Err() != nil {
			return
		}
		if k, ok := w.r.take(); ok {
			w.onSong(k)
		} else {
			w.onTimer()
		}
	}
}

// next 返回下一项定时处理的时刻：节拍、关联表重读、本地认定、编号兜底请求、当前歌曲记录的读取与待重试中最早的。
func (w *assocWorker) next() time.Time {
	at := w.nextTick
	for _, t := range [...]time.Time{w.retryAt, w.localDue(), w.fillDue(), w.recordDue(), w.pendingAt} {
		if !t.IsZero() && t.Before(at) {
			at = t
		}
	}
	return at
}

// onTimer 做一项到期的定时处理，次序为节拍、关联表重读、本地认定、编号兜底请求、当前歌曲记录的读取、待重试。兜底先于读记录：
// 同时到期时先补编号，这次读即按补上的编号核对。
func (w *assocWorker) onTimer() {
	now := w.d.now()
	due := func(t time.Time) bool { return !t.IsZero() && !now.Before(t) }
	switch {
	case !now.Before(w.nextTick):
		w.tick(now)
	case due(w.retryAt):
		w.parse()
	case due(w.localDue()):
		if w.attemptLocal() {
			w.evaluate()
		}
	case due(w.fillDue()):
		w.fillIDs(now)
	case due(w.recordDue()):
		if changed, ok := w.readRecord(now); ok && changed {
			w.evaluate()
		}
	case due(w.pendingAt):
		w.evaluate()
	}
}

// tick 是 1 秒一次的节拍：stat 关联表，（大小，修改时间）与上次解析时不同、且与上一拍相同即重读，并复位退避。关联表不存在
// （stat 失败）时：此前解析成功的表作废、结果改为未知（如 QQ 清了缓存），再出现时稳定 1 秒即重读，即使（大小，修改时间）
// 与作废前相同；距上次定位已满 relocateEvery 即重新定位。关联表没有重读的拍另看守中的文件（checkWatched），变了且稳定即
// 求结果；重读了关联表的拍已求结果、重建了看守，不再看。下一拍从这一拍处理完的时刻排起：stat 或重读慢于 1 秒时
// （如缓存目录在失联的网络盘上），节拍不会一直到期而饿死其余定时处理。
func (w *assocWorker) tick(now time.Time) {
	defer func() { w.nextTick = w.d.now().Add(assocTick) }()
	st := w.statTable()
	changed := st.ok && st == w.prev && st != w.parsed
	w.prev = st
	switch {
	case changed:
		w.backoff = 0
		w.parse()
		return
	case !st.ok:
		if w.tableOK {
			w.tableOK, w.parsed = false, fileStat{}
			w.updateLocalPossible()
			w.evaluate()
		}
		if !now.Before(w.lastLocate.Add(relocateEvery)) {
			w.locate()
		}
	}
	if w.checkWatched() {
		w.evaluate()
	}
}

func (w *assocWorker) statTable() fileStat { return w.statFile(w.tablePath) }

// statFile 返回 path 的（大小，修改时间）；stat 失败或不是普通文件时为零值；ctx 已取消即返回零值、不 stat。
func (w *assocWorker) statFile(path string) fileStat {
	if w.ctx.Err() != nil {
		return fileStat{}
	}
	fi, err := w.d.stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return fileStat{}
	}
	return fileStat{size: fi.Size(), mod: fi.ModTime().UnixNano(), ok: true}
}

// checkWatched 逐个 stat 看守中的文件（至多 4 个，设计 §6.4 第 6 步）：与读之前（read）不同、且与上一拍（prev）相同者
// 计为变了，并更新 prev；返回是否有变了的。ctx 已取消即返回假。
func (w *assocWorker) checkWatched() bool {
	changed := false
	for path, f := range w.watch {
		if w.ctx.Err() != nil {
			return false
		}
		st := w.statFile(path)
		changed = changed || st != f.read && st == f.prev
		f.prev = st
		w.watch[path] = f
	}
	return changed
}

// watchFile 把 path 加入看守，read 与 prev 都取读之前的 stat st；路径不是绝对路径的不入。
func (w *assocWorker) watchFile(path string, st fileStat) {
	if !filepath.IsAbs(path) {
		return
	}
	if w.watch == nil {
		w.watch = map[string]watchedFile{}
	}
	w.watch[path] = watchedFile{read: st, prev: st}
}

// locate 定位关联表（设计 §6.2）：缓存目录取配置 ConfigInfoXML1.dat 里 CachePath.ini 的 Path；读不出、解不开、没有该节点
// 或不是绝对路径时退回 <配置目录>\QQMusicCache\，退回的原因与上一次不同时打一条 Detail。
func (w *assocWorker) locate() {
	w.lastLocate = w.d.now()
	dir, err := w.cachePath()
	if w.ctx.Err() != nil {
		return
	}
	if err != nil {
		dir = filepath.Join(w.d.configDir, "QQMusicCache")
		if msg := err.Error(); msg != w.locateErr {
			w.locateErr = msg
			log.Detail("Cannot read the QQ Music cache directory setting; using the default %s: %v", dir, err)
		}
	} else {
		w.locateErr = ""
	}
	path := filepath.Join(dir, "QQMusicLyricNew", "LyricAssociation.dat")
	if path != w.tablePath {
		w.index = indexCache{} // 歌词索引随之换了目录，缓存只认 stat
	}
	w.tablePath = path
}

func (w *assocWorker) cachePath() (string, error) {
	plain, err := w.readDat(filepath.Join(w.d.configDir, "ConfigInfoXML1.dat"))
	if err != nil {
		return "", err
	}
	return assoc.ParseCachePath(plain)
}

// readDat 读入并解密一个 .dat 文件，至多 maxDatBytes 字节。
func (w *assocWorker) readDat(path string) ([]byte, error) {
	raw, err := w.read(path, maxDatBytes)
	if err != nil {
		return nil, err
	}
	return assoc.DecryptDat(raw)
}

// read 包装 d.read：读之前与返回之后各查一次 ctx，已取消即返回 ctx.Err()，读之前已取消的不调用底层读取。会话取消后不再打开
// 后续文件（下一个文件可能在失联的网络盘上，再次阻塞会一直占住单实例）。
func (w *assocWorker) read(path string, max int64) ([]byte, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	b, err := w.d.read(path, max)
	if cerr := w.ctx.Err(); cerr != nil {
		return nil, cerr
	}
	return b, err
}

// parse 读入并解析关联表。成功即打一条 Detail（条目数、文件字节数与读入、解密、解析的耗时，不计每首的额度，设计 §6.9），
// 换用新表、复位退避、本地文件未认定时从头认定，再给当前歌曲求结果；失败见 parseFailed。ctx 已取消时不改状态、不发结果。
func (w *assocWorker) parse() {
	st := w.statTable() // 读之前取：读的期间关联表若又变了，之后的节拍会发现
	start := time.Now()
	raw, err := w.read(w.tablePath, maxDatBytes)
	var es []assoc.Entry
	if err == nil {
		var plain []byte
		if plain, err = assoc.DecryptDat(raw); err == nil {
			es, err = assoc.ParseAssociations(plain)
		}
	}
	if w.ctx.Err() != nil {
		return
	}
	w.parsed = st
	if err != nil {
		w.parseFailed(st, err)
		return
	}
	log.Detail("Parsed the QQ Music lyric association table: %d entries, %d bytes in %v", len(es), len(raw), time.Since(start).Round(time.Millisecond))
	w.entries, w.tableOK = es, true
	w.updateLocalPossible()
	w.backoff, w.retryAt, w.badStat, w.badMsg = 0, time.Time{}, fileStat{}, ""
	w.restartLocal()
	w.evaluate()
}

// updateLocalPossible 按当前表状态与本地认定开关更新只读原子提示 localPossible（#248）。
func (w *assocWorker) updateLocalPossible() {
	possible := !w.stopped && w.tableOK && !w.localOff &&
		slices.ContainsFunc(w.entries, func(e assoc.Entry) bool { return e.Kind == assoc.KindLocal })
	w.r.localPossible.Store(possible)
}

// parseFailed 处理关联表读取或解析的失败（设计 §6.3 第 4、5 步）。结构错误（含超出 16 MiB）与上一次结构错误的文本相同、
// 且两次读之前的（大小，修改时间）相同，即判为不支持：上报并停用。其余为暂时性失败：结果为未知，按 1、2、4、8 秒退避重读、
// 之后保持 8 秒。QQ 写到一半时读到的文件同样是结构错误，写完即变，所以要两次相同才判不支持。
func (w *assocWorker) parseFailed(st fileStat, err error) {
	if errors.Is(err, assoc.ErrStructure) || errors.Is(err, errLocalTooLarge) {
		if st.ok && st == w.badStat && err.Error() == w.badMsg {
			reportOnce("qqmusic.assoc_unsupported", "QQ 音乐歌词关联表两次读到同一份无法解析的文件，本会话已停用歌词关联",
				map[string]string{"qqmusic.version": w.d.qqVersion},
				map[string]any{"error": redactHome(err.Error())})
			w.stop(i18n.Errorf("the lyric association table is not supported: %w", err))
			return
		}
		w.badStat, w.badMsg = st, err.Error()
	}
	w.tableOK = false
	w.updateLocalPossible()
	w.backoff = min(max(2*w.backoff, assocBackoffMin), assocBackoffMax)
	w.retryAt = w.d.now().Add(w.backoff)
	if w.once("table " + err.Error()) {
		log.Detail("Reading the QQ Music lyric association table failed; will retry: %v", err)
	}
	w.evaluate()
}

// onSong 换用会话写入的歌曲，再求结果。代次（gen，reopen）不变的写入用于同首补齐在线编号（#248）或更新详情结果（设计 §6.4
// 第 2 步「交接」）：保留已认定的本地文件、本地认定日程与 Detail 额度；若此前编号缺位时窗口用尽记过 detailFailed、本次清为
// detailNone，同步清零兜底状态。代次变了：清掉上一首的认定、待重试与 Detail 额度，本地文件从第 0 秒起认定，读当前歌曲记录的
// 状态清空、新的一轮在求结果时开始；gen 变了另清兜底的状态，只变 reopen 时保留兜底所得。详情结果为已失败而本 gen 尚未排过
// 兜底请求即当即排一次；编号因此变了按 idsUpdated 作废认出或排补读。
func (w *assocWorker) onSong(k songKey) {
	now := w.d.now()
	id0, mid0 := w.ids()
	if !w.hasSong || k.gen != w.cur.gen || k.reopen != w.cur.reopen {
		if !w.hasSong || k.gen != w.cur.gen {
			w.fill = fillState{}
		}
		w.rec = recordState{}
		w.cur, w.hasSong = k, true
		w.localPath, w.logged = "", nil
		w.restartLocal()
	} else {
		if w.cur.detail.state == detailFailed && k.detail.state == detailNone {
			w.fill = fillState{}
		}
		w.cur.songID, w.cur.songMid = k.songID, k.songMid
	}
	w.cur.detail = k.detail
	if k.detail.state == detailFailed && w.fill.at.IsZero() {
		w.fill.at = now
	}
	w.idsUpdated(id0, mid0, now)
	w.evaluate()
}

func (w *assocWorker) online() bool { return w.cur.hasIDs() }

// ids 返回当前歌曲的两个编号：w.cur 缺的那一个依次取会话交来的已得、兜底所得，会话的优先（设计 §6.4 第 2 步）；都没有即仍缺。
// 详情结果不是已得时两个编号都为零值，不必再看状态。候选、归属核对与选条目一律用它。
func (w *assocWorker) ids() (songID uint32, songMid string) {
	songID, songMid = w.cur.songID, w.cur.songMid
	for _, d := range [...]songDetail{w.cur.detail, w.fill.got} {
		if songID == 0 {
			songID = d.songID
		}
		if songMid == "" {
			songMid = d.songMid
		}
	}
	return songID, songMid
}

// fillDue 返回下一次编号兜底请求的时刻（设计 §6.4 第 2 步「兜底」）：本会话读记录、w.cur 恰有一个编号、会话报已失败、兜底尚未
// 得到、本地文件未认定（或其条目已不在表里，设计 §6.5 第 6 步）时为 w.fill.at，否则为零值。会话尚无（封面请求还没回来）与已得
// 时都不请求。
func (w *assocWorker) fillDue() time.Time {
	if w.d.record == nil || (w.cur.songID != 0) == (w.cur.songMid != "") || w.cur.detail.state != detailFailed ||
		w.fill.got.state == detailGot {
		return time.Time{}
	}
	if _, ok := w.localEntry(); ok {
		return time.Time{}
	}
	return w.fill.at
}

// fillIDs 做一次编号兜底请求：只有 songid 时按 songid 查 songmid（resolveID），只有 songmid 时按 songmid 查 songid（resolveMid；
// songmid 不合形式即不请求，按失败处理）。两者都自带核验，只交出与所查编号同属一首的。成功即记为兜底所得，按 idsUpdated
// 排补读、再求结果；失败按 2、4、8…秒退避、至多 60 秒，从请求返回时排起，Detail 经 once。请求在读取者循环里阻塞
// （httpClient 超时 10 秒）；返回时 ctx 已取消即作罢，不改状态、不求结果。
func (w *assocWorker) fillIDs(now time.Time) {
	var got songDetail
	var err error
	switch {
	case w.cur.songID != 0:
		got.songMid, err = w.d.resolveID(w.cur.songID)
	case !assoc.IsSongMid(w.cur.songMid):
		err = i18n.Errorf("malformed songmid %q", w.cur.songMid)
	default:
		got.songID, err = w.d.resolveMid(w.cur.songMid)
	}
	if w.ctx.Err() != nil {
		return
	}
	if err != nil {
		w.fill.backoff = min(max(2*w.fill.backoff, fillBackoffMin), fillBackoffMax)
		w.fill.at = w.d.now().Add(w.fill.backoff)
		if w.once("fill " + err.Error()) {
			log.Detail("Completing the other ID of the current QQ Music song failed; will retry: %v", err)
		}
		return
	}
	id0, mid0 := w.ids()
	got.state = detailGot
	w.fill.got = got
	w.idsUpdated(id0, mid0, now)
	w.evaluate()
}

// idsUpdated 在同一代次里编号可能变了之后调用（会话交来详情结果、兜底成功），id0、mid0 是之前的 ids()（设计 §6.4 第 4 步
// 「何时读」）。都不清日程与次数，补读不占日程、计入 recordReadsMax：
//   - 已在用的编号换了值（如兜底所得被不同的会话已得取代）而本代次已认出：认出只经旧编号核对过，作废（结论置零值，求结果时
//     按无条目），排一次补读按新编号重新核对；已读满即不再读，保持无条目。
//   - 补上原本缺的编号不作废已认出的：认出的网址已经过当时已知编号的核对，属于当前歌曲（§6.4 第 4 步「归属核对」），作废只会让
//     显示在关联与在线之间来回一次。
//   - 两个编号都有了而本代次的结论为待换算（合并后为待换算即最近一次读得待换算、此前没有认出，assoc.MergeConclusion）：排一次补读。
func (w *assocWorker) idsUpdated(id0 uint32, mid0 string, now time.Time) {
	id, mid := w.ids()
	switch {
	case w.rec.conc.Verdict == assoc.RecordFound && (id0 != 0 && id != id0 || mid0 != "" && mid != mid0):
		w.rec.conc, w.rec.rereadAt = assoc.RecordConclusion{}, now
	case id != 0 && mid != "" && w.rec.conc.Verdict == assoc.RecordPending:
		w.rec.rereadAt = now
	}
}

// restartLocal 从头开始本地文件认定的日程（换歌或关联表变化时），需要认定时当即做第 0 秒的那一次。
func (w *assocWorker) restartLocal() {
	w.localBase, w.localStep = w.d.now(), 0
	if !w.localDue().IsZero() {
		w.attemptLocal()
	}
}

// localDue 返回下一次本地认定的时刻；不需要认定（没有歌、已认定、本会话已停做、关联表不可用、日程已满）时为零值。不论有无
// 编号（设计 §6.5 第 6 步）：QQ 把本地文件匹配成在线歌曲之后仍放这个文件。
func (w *assocWorker) localDue() time.Time {
	if !w.hasSong || w.localPath != "" || w.localOff || !w.tableOK || w.localStep >= len(localAttemptAt) {
		return time.Time{}
	}
	return w.localBase.Add(localAttemptAt[w.localStep])
}

// attemptLocal 做一次本地文件认定（设计 §6.5 第 1～3 步），返回是否认定了文件。认定即打一条 Detail（路径、当前歌曲有无编号、
// 被 QQ 打开的候选数，设计 §6.9；认定即停，每个代次至多一条，不计每首的额度）。Restart Manager 报致命错误即本会话停做
// （Warn 一次）；候选过多、消歧不成与其余错误各打一条 Detail，按日程照常再查。
func (w *assocWorker) attemptLocal() bool {
	w.localStep++
	cands, ok := assoc.LocalCandidates(w.entries, w.cur.name, w.cur.excludeLocal, w.isFile)
	if !ok {
		if w.once("candidates") {
			log.Detail("Over 256 local files in the QQ Music lyric association table and the song name does not narrow them to 1-256; not identifying the playing file")
		}
		return false
	}
	path, held, err := assoc.IdentifyLocal(cands, w.cur.name, w.d.qqPID, w.holders)
	switch {
	case w.ctx.Err() != nil:
	case restartmgr.IsFatal(err):
		w.localOff = true
		w.updateLocalPossible()
		log.Warn("Restart Manager failed; not identifying the local file QQ Music is playing for this session: %v", err)
	case err != nil:
		if w.once("identify " + err.Error()) {
			log.Detail("Cannot identify the local file QQ Music is playing: %v", err)
		}
	case path != "":
		w.localPath = path
		if w.online() {
			log.Detail("Identified the local file QQ Music is playing: %s (the song has online IDs; %d candidates open in QQ Music)", path, held)
		} else {
			log.Detail("Identified the local file QQ Music is playing: %s (the song has no online IDs; %d candidates open in QQ Music)", path, held)
		}
		return true
	}
	return false
}

// holders 包装 d.holders：ctx 已取消即返回 ctx.Err()、不查询，IdentifyLocal 的对分随之中止。
func (w *assocWorker) holders(paths ...string) ([]uint32, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	return w.d.holders(paths...)
}

// isFile 报告 path 是否为普通文件，供 LocalCandidates 筛候选；ctx 已取消即返回假、不 stat。
func (w *assocWorker) isFile(path string) bool { return w.statFile(path).ok }

// recordWanted 报告当前歌曲是否要靠记录认键：本会话读记录（d.record 非 nil）、在线歌曲、关联表可用、本地文件未认定（或其条目
// 已不在表里，设计 §6.5 第 6 步）、按已知的编号（ids）有候选，不论 byUser 与偏移（设计 §6.4 第 2、3 步）。
func (w *assocWorker) recordWanted() bool {
	if w.d.record == nil || !w.hasSong || !w.online() || !w.tableOK {
		return false
	}
	if _, ok := w.localEntry(); ok {
		return false
	}
	id, mid := w.ids()
	_, _, need := assoc.SelectOnline(w.entries, id, mid, "")
	return need
}

// recordDue 返回下一次读当前歌曲记录的时刻：有补读即补读的时刻，否则为日程里下一次的时刻，认出之后照旧。本轮尚未开始、
// 本代次已读满 recordReadsMax 次、不再需要（如关联表变得没有候选）或日程已满而没有补读时为零值。
func (w *assocWorker) recordDue() time.Time {
	switch {
	case w.rec.base.IsZero() || w.rec.reads >= recordReadsMax || !w.recordWanted():
		return time.Time{}
	case !w.rec.rereadAt.IsZero():
		return w.rec.rereadAt
	case w.rec.step < len(localAttemptAt):
		return w.rec.base.Add(localAttemptAt[w.rec.step])
	}
	return time.Time{}
}

// readRecord 读一次当前歌曲记录，把这一次的结论经 assoc.MergeConclusion 并入本代次的结论（设计 §6.4 第 4 步「结论」），
// 计一次读取；changed 为合并前后本代次认出的网址不同，即要重新求结果。合并前已认出而网址变了（复扫认出别的网址或判不唯一）
// 时打一条 Detail，不计每首的额度。now 是这一次的时刻：日程里不晚于它的各次都由这一次抵过（扫描慢于日程间隔、或中途没有
// 候选而错过的，不补读），都未到期即为补读、不占日程。日程因这一次用完而本代次仍不是认出时上报一次
// qqmusic.song_record_unresolved（带 QQ 版本、本代次的结论与命中数，不带歌名）。扫描在块间察觉 ctx 取消或有新写入的歌即
// 中止：这一次作废、不计次数、不占日程，状态不变，ok 为假。
func (w *assocWorker) readRecord(now time.Time) (changed, ok bool) {
	cur, ok := w.scanCurrentRecord()
	if !ok {
		return false, false
	}
	prev := w.rec.conc
	conc := assoc.MergeConclusion(prev, cur)
	w.rec.conc, w.rec.rereadAt = conc, time.Time{}
	w.rec.reads++
	changed = conc.URL != prev.URL
	if changed && prev.Verdict == assoc.RecordFound {
		to := conc.URL
		if conc.Verdict != assoc.RecordFound {
			to = i18n.T(conc.Verdict.String())
		}
		log.Detail("A rescan changed the current QQ Music song record from %s to %s", prev.URL, to)
	}
	before := w.rec.step
	for w.rec.step < len(localAttemptAt) && !now.Before(w.rec.base.Add(localAttemptAt[w.rec.step])) {
		w.rec.step++
	}
	if conc.Verdict != assoc.RecordFound && before < len(localAttemptAt) && w.rec.step == len(localAttemptAt) {
		reportOnce("qqmusic.song_record_unresolved", "QQ 音乐当前歌曲记录一轮日程用完仍认不出，这首有候选的在线歌曲按无条目处理",
			map[string]string{"qqmusic.version": w.d.qqVersion},
			map[string]any{"verdict": conc.Verdict.String(), "qualified": conc.Qualified, "voided": conc.Voided, "malformed": conc.Malformed})
	}
	return changed, true
}

// scanCurrentRecord 是 readRecord 的读取部分：读 Struct1 的专辑、歌手、歌名；歌名经 sanitizeString 须等于会话写入的歌名，
// 否则歌已换或读到一半，本次作罢；拼出前缀扫描（块间查 ctx 与新写入），按已知的编号（ids）下结论。读不出前缀、歌名不符时
// 结论为零值（RecordNone）。扫描中止时 ok 为假。
func (w *assocWorker) scanCurrentRecord() (c assoc.RecordConclusion, ok bool) {
	rec := w.d.record
	album, singer, name, read := readSongPrefix(rec.mem, rec.struct1, rec.offs)
	var needle []byte
	if read {
		if sanitizeString(name) != w.cur.name {
			return assoc.RecordConclusion{}, true
		}
		needle, read = assoc.RecordNeedle(album, singer, name)
	}
	if !read {
		if w.once("record prefix") {
			log.Detail("Cannot read the album, singer and title of the current song from QQ Music memory")
		}
		return assoc.RecordConclusion{}, true
	}
	sc, st, aborted := scanRecord(rec.mem, needle, recordScanLimits, func() bool { return w.ctx.Err() != nil || w.r.pending() })
	if aborted {
		return assoc.RecordConclusion{}, false
	}
	id, mid := w.ids()
	c = assoc.ConcludeRecord(sc, id, mid)
	w.logRecord(c, sc.Complete, st)
	return c, true
}

// logRecord 记下一次扫描（设计 §6.9）：不完整（原因、失败块数）与认出（网址、合格作废与截取不合的命中数、读取量、耗时；
// 按 id 键优先放下了合格的 mid 键网址时另记放下的种数）每次各一条，不计每首的额度，供真机核对；其余认不出的结论经 once，
// 同一结论每首一条。
func (w *assocWorker) logRecord(c assoc.RecordConclusion, complete bool, st recordScanStats) {
	mib, took := float64(st.read)/(1<<20), st.elapsed.Round(time.Millisecond)
	if !complete {
		log.Detail("The scan for the current QQ Music song record was incomplete (%s, %d failed chunks); read %.1f MiB in %v",
			incompleteReason(st), st.failedChunks, mib, took)
	}
	switch c.Verdict {
	case assoc.RecordFound:
		if c.SetAside > 0 {
			log.Detail("Identified the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits; %d mid-key URLs set aside for the id key); read %.1f MiB in %v",
				c.URL, c.Qualified, c.Voided, c.Malformed, c.SetAside, mib, took)
			break
		}
		log.Detail("Identified the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits); read %.1f MiB in %v",
			c.URL, c.Qualified, c.Voided, c.Malformed, mib, took)
	case assoc.RecordIncomplete: // 上面已记
	default:
		if w.once("record " + c.Verdict.String()) {
			log.Detail("Cannot identify the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits)",
				i18n.T(c.Verdict.String()), c.Qualified, c.Voided, c.Malformed)
		}
	}
}

// incompleteReason 是扫描不完整的原因，在源头经 i18n.T 注入日志：到了读取上限；区域查询失败或区域不前进；或有块读失败、
// 只读到一部分（失败块数另记）。
func incompleteReason(st recordScanStats) string {
	switch {
	case st.capped:
		return i18n.T("read limit reached")
	case st.queryFailed:
		return i18n.T("memory region query failed or did not advance")
	}
	return i18n.T("chunk reads failed")
}

// evaluate 给当前歌曲求结果并发出：关联表不可用时为未知，否则为期望状态；有文件暂缺时 5 秒后再求。看守先清空，读关联歌词
// 时重建（loadLyrics）。要读当前歌曲记录而本代次的这一轮尚未开始时（刚换歌，或此前没有候选、已认定的本地文件的条目随后从表里
// 消失），当即开始、读第 0 次；这次读被中止（ctx 取消或有新写入的歌）即作罢、不发结果，由 run 转去处理新歌或退出。没有歌、
// ctx 已取消时不发。
func (w *assocWorker) evaluate() {
	w.pendingAt, w.watch = time.Time{}, nil
	if !w.hasSong {
		return
	}
	if w.rec.base.IsZero() && w.recordWanted() {
		w.rec.base = w.d.now()
		if _, ok := w.readRecord(w.rec.base); !ok {
			return
		}
	}
	res := assocResult{gen: w.cur.gen, reopen: w.cur.reopen}
	if w.tableOK {
		res = w.desired()
	}
	if w.ctx.Err() != nil {
		return
	}
	if res.pending {
		w.pendingAt = w.d.now().Add(assocPendingEvery)
	}
	w.r.send(res)
}

// desired 求当前歌曲的期望状态（设计 §6.8 的表），先看本地文件认定，再看在线一侧。已认定的本地文件在表里有条目即读其歌词
// （不论 byUser 与有无编号，设计 §6.5 第 4、6 步）：可用即本地文件、条目的偏移。否则有编号（songid 或 songmid）时按已知的编号
// （ids）有候选即按本代次认出的当前歌曲记录的网址逐字节选条目，没认出（本版本不读记录、还在日程中或认不出）即无条目（设计
// §6.4 第 3～5 步）；选中的条目不论 byUser 都读其歌词，可用即在线关联、条目的偏移（设计 §6.4 第 6 步）。读的歌词不可用或
// 读不到、无条目时，有编号为在线、偏移 0，编号缺位为无。读不到（暂缺）置待重试。
func (w *assocWorker) desired() assocResult {
	res := assocResult{gen: w.cur.gen, reopen: w.cur.reopen, kind: resDesired, source: srcNone}
	if w.online() {
		res.source = srcOnline
	}
	var e assoc.Entry
	var load, local bool
	if le, ok := w.localEntry(); ok {
		e, load, local = le, true, true
	} else if w.online() {
		id, mid := w.ids()
		e, load, _ = assoc.SelectOnline(w.entries, id, mid, w.rec.conc.URL)
	}
	var content [32]byte
	if load {
		lines, sum, pending, err := w.loadLyrics(e.LyricURL)
		res.pending = pending
		switch {
		case err != nil:
			if w.once("lyrics " + err.Error()) {
				log.Detail("Cannot use the associated lyrics: %v", err)
			}
		case local:
			res.source, res.lyricOffsetMs, res.lines, content, res.localPath = srcLocal, e.LyricOffsetMs, lines, sum, w.localPath
		default:
			res.source, res.lyricOffsetMs, res.lines, content = srcAssoc, e.LyricOffsetMs, lines, sum
		}
	}
	res.digest = sha256.Sum256(fmt.Appendf(nil, "%d %d %x", res.source, res.lyricOffsetMs, content))
	return res
}

// localEntry 返回已认定的本地文件在关联表里的条目（路径不区分大小写，取第一条）；未认定或条目已不在表里时为假，即按未认定
// （设计 §6.5 第 4 步）。
func (w *assocWorker) localEntry() (assoc.Entry, bool) {
	i := slices.IndexFunc(w.entries, func(e assoc.Entry) bool {
		return w.localPath != "" && e.Kind == assoc.KindLocal && strings.EqualFold(e.SongURL, w.localPath)
	})
	if i < 0 {
		return assoc.Entry{}, false
	}
	return w.entries[i], true
}

// loadLyrics 读关联歌词：原文为 lyricURL，翻译、音译按歌词索引（设计 §6.6）。content 是三轨的路径与读成的内容的摘要，读不成
// 的轨不计内容（它不影响给出的行，判为不可用之后不再读也不改变摘要）。pending 为真即有文件暂缺：原文读不到；或原文可用，而
// 翻译音译读不到、或歌词索引这次没读成（这两种先给出原文，翻译音译缺着）。不可用（解不开、解析不出、超限、跳过）不算暂缺。
// 读过的各轨不论读成没读成都按读之前的 stat 入看守（设计 §6.4 第 6 步；索引由 lyricFiles 入看守）：「搜索歌词」把所选的歌词
// 写进该曲自己的缓存文件、路径不变，先读到旧内容时文件一变即重读。文件本身不可用的另记进 w.unusable，下次读之前的 stat 未变
// 即直接给出不可用、不读不解密，不论作哪一轨、原文变没变。因合并规模或音译的复制预算跳过的轨不记：它取决于原文，文件本身可用
// （同一文件可能同时是原文）。
func (w *assocWorker) loadLyrics(lyricURL string) (lines []lyricLine, content [32]byte, pending bool, err error) {
	files, indexMissing := w.lyricFiles(lyricURL)
	known := w.unusable
	w.unusable = nil
	before, sums := map[string]fileStat{}, map[string][32]byte{}
	read := func(path string, max int64) ([]byte, error) {
		st := w.statFile(path)
		before[path] = st
		if u, ok := known[path]; ok && st.ok && st == u {
			return nil, i18n.Errorf("%w: unchanged since it was found unusable", errLyricFileUnusable)
		}
		b, err := w.read(path, max)
		if err == nil {
			sums[path] = sha256.Sum256(b)
		}
		return b, err
	}
	lines, failed, err := loadLocalLyrics(read, lyricURL, files)
	for path, st := range before {
		w.watchFile(path, st)
	}
	for _, f := range failed {
		pending = pending || f.missing
		if st := before[f.path]; !f.missing && !f.skipped && st.ok {
			if w.unusable == nil {
				w.unusable = map[string]fileStat{}
			}
			w.unusable[f.path] = st
		}
	}
	h := sha256.New()
	fmt.Fprintf(h, "%q %x\n", lyricURL, sums[lyricURL])
	for _, p := range [...]string{files.Trans, files.Roma} {
		var sum [32]byte
		if !slices.ContainsFunc(failed, func(f trackFailure) bool { return f.path == p }) {
			sum = sums[p]
		}
		fmt.Fprintf(h, "%q %x\n", p, sum)
	}
	copy(content[:], h.Sum(nil))
	return lines, content, pending || err == nil && indexMissing, err
}

// lyricFiles 从歌词索引取 lyricURL 的翻译、音译路径（不区分大小写），按读之前的 stat 缓存（w.index，设计 §6.6 第 4 步）：
// 解析成功的映射与稳定的结构错误（两次读之前的 stat 相同、同一结构错误，含超出 16 MiB）在 stat 未变时直接用、不读。索引不论
// 读成没读成都按读之前的 stat 入看守，变了且稳定一拍即重新求结果：QQ 补入该曲的翻译音译可能晚于这次读。索引不存在即没有翻译
// 音译（读前 stat 为零值，出现即可由 stat 察觉），不按暂缺。missing 为真即这次没读成：IO 错误或第一次结构错误，不缓存，由调用方
// 按暂缺重读。
func (w *assocWorker) lyricFiles(lyricURL string) (files assoc.LyricFiles, missing bool) {
	path := filepath.Join(filepath.Dir(w.tablePath), "lyricinfo.dat")
	st := w.statFile(path)
	w.watchFile(path, st)
	c := &w.index
	if st.ok && st == c.st {
		return c.files[strings.ToLower(lyricURL)], false
	}
	plain, err := w.readDat(path)
	var index map[string]assoc.LyricFiles
	if err == nil {
		index, err = assoc.ParseLyricIndex(plain)
	}
	c.st, c.files, c.ok = fileStat{}, nil, false
	switch {
	case err == nil:
		*c = indexCache{st: st, files: index, ok: true}
		return index[strings.ToLower(lyricURL)], false
	case errors.Is(err, fs.ErrNotExist):
		return assoc.LyricFiles{}, false
	case errors.Is(err, assoc.ErrStructure) || errors.Is(err, errLocalTooLarge):
		stable := st.ok && st == c.badStat && err.Error() == c.badMsg
		c.badStat, c.badMsg = st, err.Error()
		if stable {
			c.st = st
			return assoc.LyricFiles{}, false
		}
	}
	if w.once("index " + err.Error()) {
		log.Detail("Reading the QQ Music lyric index failed; translation and romanization will be retried: %v", err)
	}
	return assoc.LyricFiles{}, true
}

// stop 停用本会话的读取：打一条 Warn，对最新写入的歌发停用结果，带它的代次（还没有歌时为（0，0））。此后 run 返回、
// goroutine 退出，停用只发这一次。
func (w *assocWorker) stop(reason error) {
	w.stopped = true
	w.updateLocalPossible()
	log.Warn("QQ Music lyric association disabled: %v", reason)
	if k, ok := w.r.take(); ok {
		w.cur = k
	}
	w.r.send(assocResult{gen: w.cur.gen, reopen: w.cur.reopen, kind: resStopped})
}

// recoverStop 接住读取者内部的 panic：上报并停用，goroutine 随后退出，不外泄（设计 §6.8「生命周期」）。
// 须由 defer 直接调用，recover 才拿得到 panic。
func (w *assocWorker) recoverStop() {
	p := recover()
	if p == nil {
		return
	}
	reportOnce("qqmusic.assoc_reader_panic", "QQ 音乐歌词关联读取者 panic，本会话已停用歌词关联",
		map[string]string{"qqmusic.version": w.d.qqVersion},
		map[string]any{"panic": redactHome(fmt.Sprint(p)), "stack": redactHome(string(debug.Stack()))})
	w.stop(i18n.Errorf("internal error in the lyric association reader: %v", p))
}

// redactHome 把 s 中的用户主目录换成 <home>，用于上报；本地日志照原样。不分大小写，路径分隔符认 \ 与 /（Go 的栈里
// 源文件路径用 /）。
func redactHome(s string) string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return s
	}
	pattern := strings.ReplaceAll(regexp.QuoteMeta(filepath.ToSlash(home)), "/", `[\\/]`)
	return regexp.MustCompile("(?i)"+pattern).ReplaceAllLiteralString(s, "<home>")
}

// once 报告本首歌是否该为 reason 打一条 Detail：同一原因只一次，至多 maxAssocDetails 条；ctx 已取消时不打。
func (w *assocWorker) once(reason string) bool {
	if w.ctx.Err() != nil || w.logged[reason] || len(w.logged) >= maxAssocDetails {
		return false
	}
	if w.logged == nil {
		w.logged = map[string]bool{}
	}
	w.logged[reason] = true
	return true
}
