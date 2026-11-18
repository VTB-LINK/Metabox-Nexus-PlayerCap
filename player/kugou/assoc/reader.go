package assoc

// 本文件是后台读取者（设计 §6.7「读取者」）：每个酷狗会话一个 goroutine，承担确认（§6.2）、读库（§6.4）、
// 解析（§6.5、§6.6）与播放中监视库文件的全部文件操作。会话的轮询线程只写入 hash、非阻塞地取结果，不做文件 IO。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// 读取者的节奏与上限（设计 §6.2「时机」、§6.7、§9 第 3 条）。
const (
	statEvery         = time.Second     // 确认之后 stat 库文件的间隔，也是库的（大小, 修改时间）须稳定的时长
	retryEvery        = 5 * time.Second // 结果带 Retry 时重新解析的间隔
	backoffMin        = time.Second     // 暂时失败的退避：1、2、4、8 秒，之后保持 8 秒
	backoffMax        = 8 * time.Second
	confirmEvery      = 10 * time.Second // 确认未就绪时的重试间隔
	maxNotReady       = 30               // 确认未就绪累计这么多次即放弃
	maxDetailsPerSong = 4                // 每首歌至多打这么多条 Detail，同一原因只打一条
	dbFileName        = "KGMusicV3.db"
)

// readers 是存活的读取者 goroutine 数，start 保证至多为 1。
var readers atomic.Int32

// reportOnce 即 telemetry.ReportOnce，测试替换以计数。
var reportOnce = telemetry.ReportOnce

// deps 是读取者的全部外部访问，测试以桩替换。stat 返回文件的大小与修改时间。
type deps struct {
	confirm    func() (watchdog.Confirmed, error)
	candidates func(ctx context.Context, dbPath, hash string) (collected, error)
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

// worker 是读取者 goroutine 的状态，只由该 goroutine 访问。
//
// 状态：未确认（conf 为 nil）→ 已确认，或停用（disabled）。未确认时启动即确认，此后每次写入 hash 与每
// confirmEvery 各确认一次；已确认且有 hash 时每 statEvery stat 一次库文件，到 retryAt 重新解析当前 hash；
// 停用后只对写入的 hash 回停用。没有 hash 时停用发出的结果 Hash 为空，会话按 hash 不符丢弃。
type worker struct {
	r *Reader
	d deps

	conf        *watchdog.Confirmed // 确认通过之前为 nil
	dbPath      string
	notReady    int       // 确认未就绪的累计次数
	lastConfirm time.Time // 上次确认返回的时刻
	disabled    bool

	hash     string        // 当前 hash；为空即会话还没写入过
	parsed   fileStat      // 最近一次解析之前 stat 所得
	prev     fileStat      // 最近一拍 stat 所得
	nextStat time.Time     // 下一次 stat 的时刻
	retryAt  time.Time     // 重新解析当前 hash 的时刻（待重试或暂时失败的退避）；零值即不重试
	backoff  time.Duration // 最近一次暂时失败的退避；0 即下一次从 backoffMin 起
	logged   map[string]bool
}

func (w *worker) run(ctx context.Context) {
	for {
		var due <-chan time.Time
		if at, ok := w.next(); ok {
			due = time.After(time.Until(at))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.r.wake:
			if h := w.r.take(); h != "" {
				w.onHash(ctx, h)
			}
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
	case !w.retryAt.IsZero() && w.retryAt.Before(w.nextStat):
		return w.retryAt, true
	}
	return w.nextStat, true
}

// onHash 处理会话写入的新 hash：复位退避、待重试与日志额度，停用即回停用，未确认先确认，确认通过即解析。
func (w *worker) onHash(ctx context.Context, h string) {
	w.hash, w.retryAt, w.backoff, w.logged = h, time.Time{}, 0, nil
	switch {
	case w.disabled:
		w.r.send(Result{Hash: h, Kind: KindDisabled})
	case w.conf != nil || w.confirm(ctx):
		w.parse(ctx)
	case !w.disabled && ctx.Err() == nil:
		w.r.send(Result{Hash: h, Kind: KindNotConfirmed})
	}
}

// onTimer 做到期的定时处理：未确认时再确认一次，通过且已有 hash 即解析（没有 hash 只记下确认结果、不发结果）；
// 已确认时 stat 库文件，（大小, 修改时间）与上次解析时不同、且与上一拍相同即重新解析，到了 retryAt 同样重新解析。
func (w *worker) onTimer(ctx context.Context) {
	if w.conf == nil {
		if w.confirm(ctx) && w.hash != "" {
			w.parse(ctx)
		}
		return
	}
	now := time.Now()
	due := !w.retryAt.IsZero() && !now.Before(w.retryAt)
	if !now.Before(w.nextStat) {
		s := w.stat()
		w.nextStat = now.Add(statEvery)
		due = due || (s.ok && s == w.prev && s != w.parsed)
		w.prev = s
	}
	if due {
		w.parse(ctx)
	}
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
		w.conf, w.dbPath = &c, filepath.Join(c.DataDir, dbFileName)
		log.Info("Local lyric association enabled (KuGou %s, data directory %s)", c.Version, c.DataDir)
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
		res = pick(col, w.conf.LyricDir, w.readUnlessCanceled(ctx))
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
	size, mod, err := w.d.stat(w.dbPath)
	if err != nil {
		return fileStat{}
	}
	return fileStat{size: size, mod: mod.UnixNano(), ok: true}
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
