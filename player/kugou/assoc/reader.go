package assoc

// 本文件是后台读取者（设计 §6.2～§6.7）：每个酷狗会话一个 goroutine，承担确认、内存只读快照、
// 换歌双拍归属核验、单飞后台文件/队列表 I/O、以及降级后的数据库解析与监视。
// 会话的轮询线程只写入 hash、非阻塞地取结果，不做文件或进程内存 I/O。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// 读取者的节奏与上限（设计 §6.2、§6.3、§6.4.2、§6.7、§9 第 3 条）。
const (
	statEvery         = time.Second     // 确认之后 stat 库文件的间隔，也是库的（大小, 修改时间）须稳定的时长
	retryEvery        = 5 * time.Second // 结果带 Retry 时重新解析的间隔
	backoffMin        = time.Second     // 暂时失败的退避：1、2、4、8 秒，之后保持 8 秒
	backoffMax        = 8 * time.Second
	confirmEvery      = 10 * time.Second // 确认未就绪时的重试间隔
	maxNotReady       = 30               // 确认未就绪累计这么多次即放弃
	maxDetailsPerSong = 4                // 每首歌至多打这么多条 Detail，同一原因只打一条
	dbFileName        = "KGMusicV3.db"

	memPollEvery   = 200 * time.Millisecond // 内存模式常规轮询间隔
	memFastRetry   = 50 * time.Millisecond  // 内存未对齐/撕裂/搜词中/虚表异常的快速重试间隔
	memSettleDelay = 25 * time.Millisecond  // 换歌归属双拍稳定窗口
	playGrace      = 5 * time.Second        // PlayedAt 相对切歌时刻的容差
	maxMemNotReady = 10                     // 打开进程内存未就绪的上限次数
	maxBadVtable   = 3                      // 运行期连续虚表校验失败降级阈值
)

// cacheFileRe 匹配酷狗缓存歌词文件名：<name>-<32hex>-<digits>-<8hex>.(krc|lrc)。
var cacheFileRe = regexp.MustCompile(`(?i)^.+-[0-9a-f]{32}-\d+-[0-9a-f]{8}\.(?:krc|lrc)$`)

// fileTime 把时刻换算成 LastPlayTime 的单位：1601-01-01 UTC 起的 100 ns 计数。
func fileTime(t time.Time) int64 {
	const unixFrom1601 = 11644473600
	return (t.Unix()+unixFrom1601)*10_000_000 + int64(t.Nanosecond()/100)
}

// isCacheLyricPath 判断 path 是否位于 lyricDir 之内且文件名符合酷狗缓存命名格式（不校验内嵌 32hex 是否等于当前流式 Hash）。
func isCacheLyricPath(lyricDir, path string) bool {
	if lyricDir == "" || path == "" {
		return false
	}
	dir := filepath.Clean(lyricDir)
	p := filepath.Clean(path)
	if !filepath.IsAbs(dir) || !filepath.IsAbs(p) {
		return false
	}
	prefix := strings.ToLower(dir)
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if !strings.HasPrefix(strings.ToLower(p), prefix) {
		return false
	}
	return cacheFileRe.MatchString(filepath.Base(p))
}

// readers 是存活的读取者 goroutine 数，start 保证至多为 1。
var readers atomic.Int32

// diskIOActive 是全局存活的后台磁盘 I/O goroutine 数，全局同时至多为 1（设计 §6.6.1）。
var diskIOActive atomic.Int32

// reportOnce 即 telemetry.ReportOnce，测试替换以计数。
var reportOnce = telemetry.ReportOnce

// deps 是读取者的全部外部访问，测试以桩替换。stat 返回文件的大小与修改时间。
type deps struct {
	confirm    func() (watchdog.Confirmed, error)
	candidates func(ctx context.Context, dbPath, hash string) (collected, error)
	playingRow func(ctx context.Context, dbPath, hash string) (playing, error)
	openMem    func(pid uint32) (memSource, memOpenKind, string, error)
	readFile   func(string) ([]byte, error)
	stat       func(string) (int64, time.Time, error)
}

// Reader 是会话与读取者 goroutine 之间的交接：会话以 SetHash 写入当前 hash，从 Results 取结果。
type Reader struct {
	mu      sync.Mutex
	pending string        // 读取者尚未取走的 hash
	wake    chan struct{} // 容量 1，有新 hash 时唤醒读取者
	results chan Result   // 容量 1，只留最新一条
}

// SetHash 写入当前播放的 hash，覆盖读取者尚未取走的旧值；空串忽略。不阻塞，读取者退出之后调用也不阻塞。
func (r *Reader) SetHash(hash string) {
	if hash == "" {
		return
	}
	r.mu.Lock()
	r.pending = hash
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default: // 已有一个未处理的唤醒，读取者届时取到的就是这个新值
	}
}

// Results 返回结果通道：容量 1，读取者发送前先清空旧结果，只留最新一条；不关闭。
func (r *Reader) Results() <-chan Result { return r.results }

func (r *Reader) take() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.pending
	r.pending = ""
	return h
}

// send 清空旧结果后发出 res。只有读取者 goroutine 调用，清空之后必有空位；不阻塞是兜底。
func (r *Reader) send(res Result) {
	select {
	case <-r.results:
	default:
	}
	select {
	case r.results <- res:
	default:
	}
}

// Start 以生产依赖启动本会话的读取者，读取者启动即确认一次；ctx 取消即退出，阻塞在文件操作或确认里的在其返回后退出。
// 上一个读取者 goroutine 尚未退出时不启动，返回 nil。
func Start(ctx context.Context) *Reader {
	return start(ctx, deps{
		confirm: watchdog.Confirm,
		candidates: func(ctx context.Context, dbPath, hash string) (collected, error) {
			return readCandidates(ctx, dbPath, hash, realFS)
		},
		playingRow: func(ctx context.Context, dbPath, hash string) (playing, error) {
			return readPlayingRow(ctx, dbPath, hash, realFS)
		},
		openMem:  openProcessMem,
		readFile: readLyricFile,
		stat:     statFile,
	})
}

func statFile(path string) (int64, time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return fi.Size(), fi.ModTime(), nil
}

func statWith(fn func(string) (int64, time.Time, error), path string) fileStat {
	if fn == nil {
		return fileStat{}
	}
	size, mod, err := fn(path)
	if err != nil {
		return fileStat{}
	}
	return fileStat{size: size, mod: mod.UnixNano(), ok: true}
}

func start(ctx context.Context, d deps) *Reader {
	if !readers.CompareAndSwap(0, 1) {
		return nil
	}
	r := &Reader{wake: make(chan struct{}, 1), results: make(chan Result, 1)}
	go func() {
		defer telemetry.Guard() // 兜底：只有下面 recoverDisable 自身 panic 才会走到它
		defer readers.Add(-1)
		w := &worker{r: r, d: d}
		defer w.recoverDisable()
		w.run(ctx)
	}()
	return r
}

// fileStat 是库文件的（大小, 修改时间）；ok 为假即 stat 失败，失败的值不触发重新解析。
type fileStat struct {
	size int64
	mod  int64 // 修改时间的 UnixNano
	ok   bool
}

// settleState 是换歌归属双拍稳定窗口的基准快照。
type settleState struct {
	handle    songHandle
	clyricPtr uintptr
	taskID    uint32
	unassoc   bool
	assocPath string
	at        time.Time
	armed     bool
}

// parsedCacheKey 标识内存模式下已解析的一份本地歌词文件。
type parsedCacheKey struct {
	clyricPtr uintptr
	assocPath string
	path      string
}

// parsedFileCache 是内存模式下的单文件解析缓存（零 stat 复用）。
type parsedFileCache struct {
	valid   bool
	key     parsedCacheKey
	lines   []klyric.Line
	retry   bool
	retryAt time.Time
}

// memSentSig 是内存模式上次发出的结果签名，用于 200ms 轮询去重。
type memSentSig struct {
	sent           bool
	hash           string
	kind           Kind
	cacheKey       parsedCacheKey
	linesLen       int
	lyricOffsetMs  int
	hasMemoryLyric bool
	memoryNoLyric  bool
	source         Source
	retry          bool
	playedAt       int64
	localFile      bool
}

// dialogUnassocGuard 是磁盘降级模式下关联弹窗取消关联哨兵的代次锁存状态（设计 §6.4.3）。
type dialogUnassocGuard struct {
	hash            string
	playedAt        int64
	sawNonDialogKey bool
	latched         bool
}

type fileIOReq struct {
	gen  uint64
	hash string
	key  parsedCacheKey
	path string
}

type dbIOReq struct {
	gen        uint64
	hash       string
	dbPath     string
	forceRead  bool
	prevStat   fileStat
	parsedStat fileStat
}

type diskIOEvent struct {
	isFile bool

	gen   uint64
	hash  string
	key   parsedCacheKey
	lines []klyric.Line
	retry bool

	dbGen      uint64
	dbHash     string
	prevStat   fileStat
	parsedStat fileStat
	playing    playing
	hasPlaying bool
	dbFailed   bool
}

// diskIO 是内存模式下解耦磁盘 I/O 的单飞执行器：全局同时至多运行 1 个后台 goroutine，
// 以两个覆盖槽承接歌词文件读取与队列表 stat/查询，避免失联网络盘阻塞纯内存轮询。
type diskIO struct {
	readFile   func(string) ([]byte, error)
	stat       func(string) (int64, time.Time, error)
	playingRow func(ctx context.Context, dbPath, hash string) (playing, error)

	doneCh chan diskIOEvent

	mu          sync.Mutex
	pendingFile *fileIOReq
	pendingDB   *dbIOReq
	running     bool
}

func newDiskIO(d deps) *diskIO {
	return &diskIO{
		readFile:   d.readFile,
		stat:       d.stat,
		playingRow: d.playingRow,
		doneCh:     make(chan diskIOEvent, 4),
	}
}

func (io *diskIO) submitFile(ctx context.Context, req fileIOReq) bool {
	io.mu.Lock()
	if io.running {
		io.pendingFile = &req
		io.mu.Unlock()
		return true
	}
	if !diskIOActive.CompareAndSwap(0, 1) {
		io.pendingFile = nil
		io.mu.Unlock()
		return false
	}
	io.pendingFile = &req
	io.running = true
	io.mu.Unlock()
	go func() {
		defer telemetry.Guard()
		io.run(ctx)
	}()
	return true
}

func (io *diskIO) clearPendingFile() {
	io.mu.Lock()
	io.pendingFile = nil
	io.mu.Unlock()
}

func (io *diskIO) clearPendingDB() {
	io.mu.Lock()
	io.pendingDB = nil
	io.mu.Unlock()
}

func (io *diskIO) submitDB(ctx context.Context, req dbIOReq) bool {
	io.mu.Lock()
	if io.running {
		io.pendingDB = &req
		io.mu.Unlock()
		return true
	}
	if !diskIOActive.CompareAndSwap(0, 1) {
		io.pendingDB = nil
		io.mu.Unlock()
		return false
	}
	io.pendingDB = &req
	io.running = true
	io.mu.Unlock()
	go func() {
		defer telemetry.Guard()
		io.run(ctx)
	}()
	return true
}

func (io *diskIO) run(ctx context.Context) {
	cleared := false
	defer func() {
		if !cleared {
			io.mu.Lock()
			io.running = false
			diskIOActive.Add(-1)
			io.mu.Unlock()
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		io.mu.Lock()
		var fReq *fileIOReq
		var dbReq *dbIOReq
		if io.pendingFile != nil {
			fReq = io.pendingFile
			io.pendingFile = nil
		} else if io.pendingDB != nil {
			dbReq = io.pendingDB
			io.pendingDB = nil
		} else {
			io.running = false
			diskIOActive.Add(-1)
			cleared = true
			io.mu.Unlock()
			return
		}
		io.mu.Unlock()

		var ev diskIOEvent
		if fReq != nil {
			ev = io.execFile(ctx, *fReq)
		} else {
			ev = io.execDB(ctx, *dbReq)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case io.doneCh <- ev:
		case <-ctx.Done():
			return
		}
	}
}

func (io *diskIO) execFile(ctx context.Context, req fileIOReq) diskIOEvent {
	ev := diskIOEvent{isFile: true, gen: req.gen, hash: req.hash, key: req.key}
	if ctx.Err() != nil || io.readFile == nil {
		return ev
	}
	b, err := io.readFile(req.path)
	if ctx.Err() != nil {
		return ev
	}
	if err != nil {
		ev.retry = !errors.Is(err, klyric.ErrLyricTooLarge) && !errors.Is(err, errNotDiskFile)
		return ev
	}
	lines, err := klyric.ParseLocal(b)
	if err == nil && len(lines) > 0 {
		ev.lines = lines
	}
	return ev
}

func (io *diskIO) execDB(ctx context.Context, req dbIOReq) diskIOEvent {
	ev := diskIOEvent{
		isFile:     false,
		dbGen:      req.gen,
		dbHash:     req.hash,
		prevStat:   req.prevStat,
		parsedStat: req.parsedStat,
	}
	if ctx.Err() != nil || io.playingRow == nil {
		return ev
	}
	s := statWith(io.stat, req.dbPath)
	needRead := req.forceRead || (s.ok && s == req.prevStat && s != req.parsedStat)
	ev.prevStat = s
	if !needRead {
		return ev
	}
	p, err := io.playingRow(ctx, req.dbPath, req.hash)
	if ctx.Err() != nil {
		return ev
	}
	if err != nil {
		ev.dbFailed = true
		return ev
	}
	ev.parsedStat = s
	ev.playing = p
	ev.hasPlaying = true
	return ev
}

// worker 是读取者 goroutine 的状态，只由该 goroutine 访问。
//
// 状态：未确认（conf 为 nil）→ 已确认，或停用（disabled）。已确认后优先进入内存读取模式（mem != nil），
// 不支持或连续虚表失败时自动降级回数据库读取模式（mem == nil）。
type worker struct {
	r *Reader
	d deps

	conf        *watchdog.Confirmed // 确认通过之前为 nil
	dbPath      string
	lastPID     uint32
	notReady    int       // 确认未就绪的累计次数
	lastConfirm time.Time // 上次确认返回的时刻
	disabled    bool

	hash     string        // 当前 hash；为空即会话还没写入过
	parsed   fileStat      // 最近一次解析之前 stat 所得（磁盘模式）
	prev     fileStat      // 最近一拍 stat 所得（磁盘模式）
	nextStat time.Time     // 下一次 stat 的时刻（磁盘模式）
	retryAt  time.Time     // 重新解析当前 hash 的时刻（磁盘模式）；零值即不重试
	backoff  time.Duration // 最近一次暂时失败的退避（磁盘模式）；0 即下一次从 backoffMin 起
	logged   map[string]bool

	dialogGuard dialogUnassocGuard

	// 内存模式生命周期与换歌归属状态
	mem            memSource
	memUnsupported bool
	memNotReady    int
	badVtableCount int

	prevHash            string
	hashSettled         bool
	memFoundPtr         uintptr
	memRevoked          bool
	settleBase          settleState
	songStart           time.Time
	prevSongPlayedAt    int64
	curPlayedAt         int64
	curLocalFile        bool
	lastMemSent         memSentSig
	lastMemSnap         memSnapshot
	hasLastMemSnap      bool
	memNextPoll         time.Time
	memNextDBStat       time.Time
	memPrevDBStat       fileStat
	memParsedDBStat     fileStat
	memDBGen            uint64
	memDBInFlight       bool
	memDBForcePending   bool
	memDBBackoff        time.Duration
	memNoLyricRequested bool

	disk            *diskIO
	memFileGen      uint64
	memFileInFlight *fileIOReq
	memParsedCache  parsedFileCache
}

func (w *worker) run(ctx context.Context) {
	defer w.closeMem()
	for {
		var due <-chan time.Time
		var timer *time.Timer
		if at, ok := w.next(); ok {
			timer = time.NewTimer(time.Until(at))
			due = timer.C
		}
		var diskDone <-chan diskIOEvent
		if w.disk != nil {
			diskDone = w.disk.doneCh
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-w.r.wake:
			if timer != nil {
				timer.Stop()
			}
			if h := w.r.take(); h != "" {
				w.onHash(ctx, h)
			}
		case ev := <-diskDone:
			if timer != nil {
				timer.Stop()
			}
			w.onDiskIO(ctx, ev)
		case <-due:
			if h := w.r.take(); h != "" {
				w.onHash(ctx, h) // 新 hash 优先，不再为旧 hash 做定时处理
			} else {
				w.onTimer(ctx)
			}
		}
	}
}

// next 返回下一次定时处理的时刻：未确认时为上次确认之后 confirmEvery（启动时 lastConfirm 为零值，即立刻确认），
// 不论有没有 hash；已确认而没有 hash、或已停用时没有。
func (w *worker) next() (time.Time, bool) {
	switch {
	case w.disabled:
		return time.Time{}, false
	case w.conf == nil:
		return w.lastConfirm.Add(confirmEvery), true
	case w.hash == "":
		return time.Time{}, false
	case w.mem != nil:
		at := w.memNextPoll
		if w.d.playingRow != nil && !w.memDBInFlight && !w.memNextDBStat.IsZero() && (at.IsZero() || w.memNextDBStat.Before(at)) {
			at = w.memNextDBStat
		}
		return at, true
	case !w.retryAt.IsZero() && w.retryAt.Before(w.nextStat):
		return w.retryAt, true
	}
	return w.nextStat, true
}

func (w *worker) closeMem() {
	if w.mem != nil {
		w.mem.Close()
		w.mem = nil
	}
}

func (w *worker) ensureDiskIO() *diskIO {
	if w.disk == nil {
		w.disk = newDiskIO(w.d)
	}
	return w.disk
}

func (w *worker) tryOpenMem() {
	if w.conf == nil || w.memUnsupported || w.mem != nil || w.d.openMem == nil {
		return
	}
	src, kind, fp, err := w.d.openMem(w.conf.PID)
	switch kind {
	case memOpenOK:
		if src == nil {
			return
		}
		w.mem = src
		w.memNotReady = 0
		w.badVtableCount = 0
		log.Info("KuGou memory lyric reader enabled (KuGou %s, %s)", w.conf.Version, fp)
	case memOpenUnsupported:
		w.memUnsupported = true
		log.Info("KuGou memory lyric reader does not recognize this version (KuGou %s, %s); falling back to database mode", w.conf.Version, fp)
	case memOpenNotReady:
		w.memNotReady++
		if w.memNotReady >= maxMemNotReady {
			w.memUnsupported = true
		}
	default: // memOpenFailed
		if err != nil {
			w.detailOnce("Failed to open KuGou process memory; using database mode: %v", err)
		}
	}
}

// onHash 处理会话写入的新 hash：复位退避、待重试与日志额度，停用即回停用，未确认先确认，确认通过即按当前模式启动。
func (w *worker) onHash(ctx context.Context, h string) {
	if h != w.hash {
		if w.hash != "" {
			w.prevHash = w.hash
		}
		w.hash = h
		w.hashSettled = false
		w.memFoundPtr = 0
		w.memRevoked = false
		w.settleBase = settleState{}
		w.songStart = time.Now()
		w.prevSongPlayedAt = max(w.prevSongPlayedAt, w.curPlayedAt)
		w.curPlayedAt = 0
		w.curLocalFile = false
		w.lastMemSent = memSentSig{}
		w.lastMemSnap = memSnapshot{}
		w.hasLastMemSnap = false
		w.memNextPoll = time.Time{}
		w.memFileGen++
		w.memFileInFlight = nil
		w.memParsedCache = parsedFileCache{}
		w.memDBGen++
		w.memDBInFlight = false
		w.memDBForcePending = false
		if w.disk != nil {
			w.disk.clearPendingFile()
			w.disk.clearPendingDB()
		}
		w.memNoLyricRequested = false
		w.memNextDBStat = time.Time{}
		w.memPrevDBStat = fileStat{}
		w.memParsedDBStat = fileStat{}
		w.memDBBackoff = 0
	}
	w.retryAt, w.backoff, w.logged = time.Time{}, 0, nil
	switch {
	case w.disabled:
		w.r.send(Result{Hash: h, Kind: KindDisabled})
	case w.conf != nil || w.confirm(ctx):
		w.startHashInCurrentMode(ctx)
	case !w.disabled && ctx.Err() == nil:
		w.r.send(Result{Hash: h, Kind: KindNotConfirmed})
	}
}

func (w *worker) startHashInCurrentMode(ctx context.Context) {
	if w.mem == nil && !w.memUnsupported && w.d.openMem != nil {
		w.tryOpenMem()
	}
	if w.mem != nil {
		if w.d.playingRow != nil {
			w.submitMemDB(ctx, true)
		}
		w.pollMem(ctx)
		return
	}
	w.parse(ctx)
}

// onTimer 做到期的定时处理：未确认时再确认一次；内存模式下执行纯内存轮询与 1s 后台 DB stat 提交；
// 磁盘模式下 stat 库文件，（大小, 修改时间）稳定变化或到 retryAt 时重新解析。
func (w *worker) onTimer(ctx context.Context) {
	if w.conf == nil {
		if w.confirm(ctx) && w.hash != "" {
			w.startHashInCurrentMode(ctx)
		}
		return
	}
	if w.mem != nil {
		now := time.Now()
		if w.d.playingRow != nil && !w.memDBInFlight && !w.memNextDBStat.IsZero() && !now.Before(w.memNextDBStat) {
			force := w.memDBForcePending
			w.memDBForcePending = false
			if !w.submitMemDB(ctx, force) && force {
				w.memDBForcePending = true
			}
		}
		if w.memNextPoll.IsZero() || !now.Before(w.memNextPoll) {
			w.pollMem(ctx)
		}
		return
	}
	now := time.Now()
	due := !w.retryAt.IsZero() && !now.Before(w.retryAt)
	if !now.Before(w.nextStat) {
		if !w.memUnsupported && w.d.openMem != nil {
			w.tryOpenMem()
			if w.mem != nil {
				w.startHashInCurrentMode(ctx)
				return
			}
		}
		s := w.stat()
		w.nextStat = now.Add(statEvery)
		due = due || (s.ok && s == w.prev && s != w.parsed)
		w.prev = s
	}
	if due {
		w.parse(ctx)
	}
}

func (w *worker) pollMem(ctx context.Context) {
	if w.mem == nil || w.conf == nil || w.hash == "" {
		return
	}
	snap, st := w.mem.ReadSnapshot()
	now := time.Now()
	switch st {
	case memStatusProcessGone:
		w.closeMem()
		w.conf = nil
		w.notReady = 0
		w.memNotReady = 0
		w.badVtableCount = 0
		w.memUnsupported = false
		w.hashSettled = false
		w.memFoundPtr = 0
		w.memRevoked = false
		w.settleBase = settleState{}
		w.lastMemSnap = memSnapshot{}
		w.hasLastMemSnap = false
		w.lastMemSent = memSentSig{}
		w.memNextPoll = time.Time{}
		w.memNextDBStat = time.Time{}
		w.memPrevDBStat = fileStat{}
		w.memParsedDBStat = fileStat{}
		w.memDBGen++
		w.memDBInFlight = false
		w.memDBForcePending = false
		w.memDBBackoff = 0
		w.memFileGen++
		w.memFileInFlight = nil
		w.memParsedCache = parsedFileCache{}
		if w.disk != nil {
			w.disk.clearPendingFile()
			w.disk.clearPendingDB()
		}
		return
	case memStatusBadVtable:
		w.badVtableCount++
		if w.badVtableCount >= maxBadVtable {
			w.closeMem()
			log.Warn("KuGou memory lyric reader vtable check failed %d times in a row; falling back to database mode", w.badVtableCount)
			w.memUnsupported = true
			w.memFoundPtr = 0
			w.memRevoked = false
			w.hasLastMemSnap = false
			w.memNextPoll = time.Time{}
			w.memNextDBStat = time.Time{}
			w.memDBGen++
			w.memDBInFlight = false
			w.memDBForcePending = false
			w.memDBBackoff = 0
			if w.disk != nil {
				w.disk.clearPendingDB()
			}
			w.parse(ctx)
			return
		}
		w.memNextPoll = now.Add(memFastRetry)
		return
	case memStatusNotReady, memStatusTorn:
		w.badVtableCount = 0
		w.memNextPoll = now.Add(memFastRetry)
		return
	}

	w.badVtableCount = 0
	targetHash := strings.TrimSpace(w.hash)

	// 第一关：当前播放句柄有效且流式 Hash 精确匹配（跨音质缓存不再比对内嵌 <32hex>）。
	if snap.Handle.ListPtr == 0 || !strings.EqualFold(strings.TrimSpace(snap.CurrentSongHash), targetHash) {
		w.settleBase = settleState{}
		w.memNextPoll = now.Add(memFastRetry)
		return
	}

	// 第二关：切歌进入的双拍稳定窗口启发式。
	if w.prevHash != "" && !w.hashSettled {
		if !w.settleBase.armed {
			w.settleBase = settleState{
				handle:    snap.Handle,
				clyricPtr: snap.CLyricPtr,
				taskID:    snap.TaskID,
				unassoc:   snap.IsUserUnassociated,
				assocPath: snap.AssocPath,
				at:        now,
				armed:     true,
			}
			w.memNextPoll = now.Add(memSettleDelay)
			return
		}
		same := w.settleBase.handle == snap.Handle &&
			w.settleBase.clyricPtr == snap.CLyricPtr &&
			w.settleBase.taskID == snap.TaskID &&
			w.settleBase.unassoc == snap.IsUserUnassociated &&
			w.settleBase.assocPath == snap.AssocPath
		if !same {
			w.settleBase = settleState{
				handle:    snap.Handle,
				clyricPtr: snap.CLyricPtr,
				taskID:    snap.TaskID,
				unassoc:   snap.IsUserUnassociated,
				assocPath: snap.AssocPath,
				at:        now,
				armed:     true,
			}
			w.memNextPoll = now.Add(memSettleDelay)
			return
		}
		if deadline := w.settleBase.at.Add(memSettleDelay); now.Before(deadline) {
			w.memNextPoll = deadline
			return
		}
		w.hashSettled = true
	}

	w.lastMemSnap = snap
	w.hasLastMemSnap = true
	interval := w.evaluateMemSnap(ctx, snap, now)
	w.memNextPoll = now.Add(interval)
}

func (w *worker) cancelMemFileIO() {
	if w.memFileInFlight != nil {
		w.memFileGen++
		w.memFileInFlight = nil
	}
	if w.disk != nil {
		w.disk.clearPendingFile()
	}
}

func (w *worker) evaluateMemSnap(ctx context.Context, snap memSnapshot, now time.Time) time.Duration {
	// 1. 取消关联最高优先：立即作废在飞文件读并清词，不受卡死文件 I/O 阻塞。
	if snap.IsUserUnassociated || snap.AssocPath == "用户取消关联" {
		w.cancelMemFileIO()
		w.memFoundPtr = 0
		w.memRevoked = false
		w.sendMem(Result{
			Hash:      w.hash,
			Kind:      KindUnassociated,
			PlayedAt:  w.curPlayedAt,
			LocalFile: w.curLocalFile,
		}, parsedCacheKey{})
		return memPollEvery
	}

	// 2. 已加载 CLyric 且存在磁盘路径或内存行向量。
	if snap.CLyricPtr != 0 && (snap.LyricFilePath != "" || snap.VecBegin != snap.VecEnd) {
		off := clampLyricOffsetMs(snap.LyricOffsetMs)
		src := SourceAssociation
		if snap.KrcID != 0 && isCacheLyricPath(w.conf.LyricDir, snap.LyricFilePath) {
			src = SourceCache
		}
		if snap.LyricFilePath != "" {
			key := parsedCacheKey{clyricPtr: snap.CLyricPtr, assocPath: snap.AssocPath, path: snap.LyricFilePath}
			if w.memParsedCache.valid && w.memParsedCache.key == key {
				if len(w.memParsedCache.lines) > 0 {
					w.memFoundPtr = snap.CLyricPtr
					w.memRevoked = false
					w.sendMem(Result{
						Hash:          w.hash,
						Kind:          KindFound,
						Lines:         w.memParsedCache.lines,
						LyricOffsetMs: off,
						Source:        src,
						PlayedAt:      w.curPlayedAt,
						LocalFile:     w.curLocalFile,
					}, key)
				} else {
					if w.memFoundPtr != 0 && snap.CLyricPtr != w.memFoundPtr {
						w.memFoundPtr = 0
						w.memRevoked = true
					}
					w.sendMem(Result{
						Hash:           w.hash,
						Kind:           KindNone,
						LyricOffsetMs:  off,
						HasMemoryLyric: snap.VecBegin != snap.VecEnd,
						MemoryNoLyric:  w.memRevoked,
						Retry:          w.memParsedCache.retry,
						PlayedAt:       w.curPlayedAt,
						LocalFile:      w.curLocalFile,
					}, key)
				}
				if w.memParsedCache.retry && !now.Before(w.memParsedCache.retryAt) {
					w.requestMemFile(ctx, key, snap.LyricFilePath)
				}
				return memPollEvery
			}
			w.requestMemFile(ctx, key, snap.LyricFilePath)
			return memPollEvery
		}
		if w.memFoundPtr != 0 {
			w.memFoundPtr = 0
			w.memRevoked = true
		}
		w.sendMem(Result{
			Hash:           w.hash,
			Kind:           KindNone,
			LyricOffsetMs:  off,
			HasMemoryLyric: true,
			MemoryNoLyric:  w.memRevoked,
			PlayedAt:       w.curPlayedAt,
			LocalFile:      w.curLocalFile,
		}, parsedCacheKey{})
		return memPollEvery
	}

	// 3. 明确无歌词：作废在飞文件读取，立即发 MemoryNoLyric=true 的 KindNone，并触发一次队列读盘以补齐 PlayedAt/LocalFile。
	if snap.NoLyricFound {
		w.cancelMemFileIO()
		if w.memFoundPtr != 0 {
			w.memFoundPtr = 0
			w.memRevoked = true
		}
		w.sendMem(Result{
			Hash:          w.hash,
			Kind:          KindNone,
			MemoryNoLyric: true,
			PlayedAt:      w.curPlayedAt,
			LocalFile:     w.curLocalFile,
		}, parsedCacheKey{})
		if !w.memNoLyricRequested && w.d.playingRow != nil {
			if w.submitMemDB(ctx, true) || w.memDBInFlight {
				w.memNoLyricRequested = true
			}
		}
		return memPollEvery
	}

	// 4. 其余（搜词进行中）：若本首已发过 KindFound 或处于已撤销态，发 MemoryNoLyric=true 撤销误采纳旧词，并继续快速复检。
	if w.memFoundPtr != 0 || w.memRevoked {
		if w.memFoundPtr != 0 {
			w.cancelMemFileIO()
			w.memFoundPtr = 0
			w.memRevoked = true
		}
		w.sendMem(Result{
			Hash:          w.hash,
			Kind:          KindNone,
			MemoryNoLyric: true,
			PlayedAt:      w.curPlayedAt,
			LocalFile:     w.curLocalFile,
		}, parsedCacheKey{})
	}
	return memFastRetry
}

func (w *worker) requestMemFile(ctx context.Context, key parsedCacheKey, path string) {
	if w.memFileInFlight != nil && w.memFileInFlight.gen == w.memFileGen && w.memFileInFlight.key == key {
		return
	}
	if w.memFileInFlight != nil && w.memFileInFlight.key != key {
		w.memFileGen++
	}
	req := fileIOReq{gen: w.memFileGen, hash: w.hash, key: key, path: path}
	if w.ensureDiskIO().submitFile(ctx, req) {
		w.memFileInFlight = &req
	}
}

func (w *worker) submitMemDB(ctx context.Context, forceRead bool) bool {
	if w.d.playingRow == nil || w.dbPath == "" || w.hash == "" {
		return false
	}
	if w.memDBInFlight {
		if forceRead {
			w.memDBForcePending = true
		}
		return false
	}
	req := dbIOReq{
		gen:        w.memDBGen,
		hash:       w.hash,
		dbPath:     w.dbPath,
		forceRead:  forceRead,
		prevStat:   w.memPrevDBStat,
		parsedStat: w.memParsedDBStat,
	}
	if w.ensureDiskIO().submitDB(ctx, req) {
		w.memDBInFlight = true
		w.memNextDBStat = time.Time{}
		return true
	}
	w.memNextDBStat = time.Now().Add(statEvery)
	return false
}

func (w *worker) onDiskIO(ctx context.Context, ev diskIOEvent) {
	if w.mem == nil || w.disabled || ctx.Err() != nil {
		return
	}
	if ev.isFile {
		if ev.gen != w.memFileGen || ev.hash != w.hash {
			return
		}
		now := time.Now()
		var retryAt time.Time
		if ev.retry {
			retryAt = now.Add(retryEvery)
		}
		w.memParsedCache = parsedFileCache{
			valid:   true,
			key:     ev.key,
			lines:   ev.lines,
			retry:   ev.retry,
			retryAt: retryAt,
		}
		if w.memFileInFlight != nil && w.memFileInFlight.gen == ev.gen && w.memFileInFlight.key == ev.key {
			w.memFileInFlight = nil
		}
		if w.hasLastMemSnap {
			w.evaluateMemSnap(ctx, w.lastMemSnap, now)
		}
		return
	}

	if ev.dbGen != w.memDBGen || ev.dbHash != w.hash {
		return
	}
	w.memDBInFlight = false
	w.memPrevDBStat = ev.prevStat
	if ev.dbFailed {
		w.memDBBackoff = min(max(2*w.memDBBackoff, backoffMin), backoffMax)
		w.memNextDBStat = time.Now().Add(w.memDBBackoff)
		return
	}
	w.memParsedDBStat = ev.parsedStat
	if ev.hasPlaying {
		w.memDBBackoff = 0
		p := ev.playing
		if p.played > 0 &&
			p.played > w.prevSongPlayedAt &&
			p.played >= fileTime(w.songStart.Add(-playGrace)) &&
			p.played > w.curPlayedAt {
			w.curPlayedAt = p.played
			w.curLocalFile = filepath.IsAbs(p.path)
			if w.hasLastMemSnap {
				w.evaluateMemSnap(ctx, w.lastMemSnap, time.Now())
			}
		}
	}
	if !w.memDBInFlight {
		if w.memDBForcePending {
			w.memDBForcePending = false
			if !w.submitMemDB(ctx, true) {
				w.memDBForcePending = true
			}
		} else {
			w.memNextDBStat = time.Now().Add(statEvery)
		}
	}
}

func (w *worker) sendMem(res Result, cacheKey parsedCacheKey) {
	sig := memSentSig{
		sent:           true,
		hash:           res.Hash,
		kind:           res.Kind,
		cacheKey:       cacheKey,
		linesLen:       len(res.Lines),
		lyricOffsetMs:  res.LyricOffsetMs,
		hasMemoryLyric: res.HasMemoryLyric,
		memoryNoLyric:  res.MemoryNoLyric,
		source:         res.Source,
		retry:          res.Retry,
		playedAt:       res.PlayedAt,
		localFile:      res.LocalFile,
	}
	if sig == w.lastMemSent {
		return
	}
	w.lastMemSent = sig
	w.r.send(res)
}

// confirm 确认一次，返回是否已确认。未就绪累计到 maxNotReady 次、或不支持，即停用（停用时已对当前 hash 发停用）。
// 其余错误按未就绪处理。ctx 已取消时返回假，不发任何结果。
func (w *worker) confirm(ctx context.Context) bool {
	c, err := w.d.confirm()
	w.lastConfirm = time.Now()
	if ctx.Err() != nil {
		return false
	}
	switch {
	case err == nil:
		if (w.mem != nil && c.PID != w.mem.PID()) || (w.lastPID != 0 && c.PID != w.lastPID) {
			w.closeMem()
			w.memUnsupported = false
			w.memNotReady = 0
			w.badVtableCount = 0
			w.memFoundPtr = 0
			w.memRevoked = false
		}
		w.conf, w.dbPath, w.lastPID = &c, filepath.Join(c.DataDir, dbFileName), c.PID
		log.Info("Local lyric association enabled (KuGou %s, data directory %s)", c.Version, c.DataDir)
		if !w.memUnsupported && w.mem == nil && w.d.openMem != nil {
			w.tryOpenMem()
		}
		return true
	case errors.Is(err, watchdog.ErrConfirmUnsupported):
		w.disable(err)
		return false
	}
	w.notReady++
	if w.notReady >= maxNotReady {
		w.disable(i18n.Errorf("%w; gave up after %d attempts", err, w.notReady))
		return false
	}
	w.detailOnce("Local lyric association not confirmed yet: %v", err)
	return false
}

// parse 读库解析当前 hash 并发出结果。结构错误（不支持、损坏）即停用并上报；其余错误为暂时失败，按退避重试。
// ctx 已取消时不发结果。
func (w *worker) parse(ctx context.Context) {
	w.parsed = w.stat() // 解析之前取：解析期间库若又变了，之后的节拍会发现
	col, err := w.d.candidates(ctx, w.dbPath, w.hash)
	if ctx.Err() != nil {
		return
	}
	var res Result
	switch {
	case err == nil:
		if col.playedAt > 0 {
			g := &w.dialogGuard
			if !strings.EqualFold(w.hash, g.hash) || col.playedAt != g.playedAt {
				*g = dialogUnassocGuard{
					hash:            w.hash,
					playedAt:        col.playedAt,
					sawNonDialogKey: col.pKey != unassocDialogKey,
					latched:         false,
				}
			} else if col.pKey != unassocDialogKey {
				g.sawNonDialogKey = true
				g.latched = false
			} else if g.sawNonDialogKey {
				g.latched = true
			}
		}
		if !col.menuUnassoc && col.pKey == unassocDialogKey && w.dialogGuard.latched &&
			strings.EqualFold(w.hash, w.dialogGuard.hash) && col.playedAt == w.dialogGuard.playedAt {
			res = Result{Kind: KindUnassociated, PlayedAt: col.playedAt, LocalFile: col.localFile}
		} else {
			res = pick(col, w.conf.LyricDir, w.readUnlessCanceled(ctx))
		}
		if ctx.Err() != nil {
			// 取消之后 pick 把各候选的读取都记作暂缺（置 Retry）；结果不发，状态不改，无妨。
			return
		}
		res.Hash = w.hash
		w.backoff = 0
	case errors.Is(err, kgdb.ErrUnsupportedDB) || errors.Is(err, kgdb.ErrCorrupt):
		reportOnce("kugou.lyricdb_unsupported", "酷狗歌词数据库不受支持或已损坏，本地歌词关联已停用",
			map[string]string{"kugou.version": w.conf.Version},
			map[string]any{"error": w.redact(err.Error())})
		w.disable(err)
		return
	default:
		res = Result{Hash: w.hash, Kind: KindTransient}
		w.backoff = min(max(2*w.backoff, backoffMin), backoffMax)
		w.detailOnce("Reading the KuGou lyric database failed; will retry: %v", err)
	}
	now := time.Now()
	w.nextStat = now.Add(statEvery)
	switch {
	case res.Kind == KindTransient:
		w.retryAt = now.Add(w.backoff)
	case res.Retry:
		w.retryAt = now.Add(retryEvery)
	default:
		w.retryAt = time.Time{}
	}
	w.r.send(res)
}

// readUnlessCanceled 包装 readFile：每次读之前与读返回之后各查一次 ctx，已取消即返回 ctx.Err()，读之前已取消的
// 不调用底层读取。会话取消后 pick 不再打开后续候选（下一个文件可能在失联的网络盘上，再次阻塞会一直占住单实例）。
func (w *worker) readUnlessCanceled(ctx context.Context) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := w.d.readFile(path)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return b, err
	}
}

// disable 停用本会话的读取：打一条 Warn，对当前 hash 发停用。此后只对写入的 hash 回停用。
func (w *worker) disable(reason error) {
	w.disabled = true
	w.closeMem()
	log.Warn("Local lyric association disabled: %v", reason)
	w.r.send(Result{Hash: w.hash, Kind: KindDisabled})
}

// recoverDisable 接住读取者内部的 panic：上报并停用，goroutine 随后退出，不外泄（设计 §6.7）。
// 须由 defer 直接调用，recover 才拿得到 panic。
func (w *worker) recoverDisable() {
	p := recover()
	if p == nil {
		return
	}
	version := ""
	if w.conf != nil {
		version = w.conf.Version
	}
	reportOnce("kugou.lyricdb_reader_panic", "酷狗本地歌词读取者 panic，本会话已停用本地歌词关联",
		map[string]string{"kugou.version": version},
		map[string]any{"panic": w.redact(fmt.Sprint(p)), "stack": string(debug.Stack())})
	w.disable(i18n.Errorf("internal error in the local lyric reader: %v", p))
}

// redact 去掉上报文本里含 Windows 用户名的路径（数据目录、歌词目录、用户主目录），见 redactPaths。
// 只用于上报，本地日志照原样。
func (w *worker) redact(s string) string {
	home, _ := os.UserHomeDir()
	var data, lyric string
	if w.conf != nil {
		data, lyric = w.conf.DataDir, w.conf.LyricDir
	}
	return redactPaths(s, data, lyric, home)
}

// redactPaths 把 s 中的 dataDir、lyricDir、home 依次替换为 <data>、<lyric>、<home>：不分大小写（Windows 路径
// 不分），按字面匹配，空串跳过。数据目录通常在主目录之下，故先替换它。
func redactPaths(s, dataDir, lyricDir, home string) string {
	for _, r := range [...]struct{ path, mark string }{{dataDir, "<data>"}, {lyricDir, "<lyric>"}, {home, "<home>"}} {
		if r.path != "" {
			s = regexp.MustCompile("(?i)"+regexp.QuoteMeta(r.path)).ReplaceAllLiteralString(s, r.mark)
		}
	}
	return s
}

func (w *worker) stat() fileStat {
	return statWith(w.d.stat, w.dbPath)
}

// detailOnce 打一条 Detail，同一首歌里同一原因（错误文本）只打一条、至多 maxDetailsPerSong 条。
func (w *worker) detailOnce(format string, err error) {
	msg := err.Error()
	if w.logged[msg] || len(w.logged) >= maxDetailsPerSong {
		return
	}
	if w.logged == nil {
		w.logged = map[string]bool{}
	}
	w.logged[msg] = true
	log.Detail(format, err)
}
