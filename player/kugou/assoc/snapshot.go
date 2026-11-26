package assoc

// 本文件是读库（设计 §6.4）与打开酷狗的文件（设计 §3「Windows 打开方式」）。读的是酷狗正在使用的库与文件：
// 不加锁、不映射，以读、写、删除三种共享打开，酷狗读写、改名、删除自己的文件都不受妨碍
// （player/kugoudbread_lint_test.go 守这一条）。库按页读、走两遍，两遍读到的页逐字节相同才采纳。

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// errTransient 表示本次读库暂时失败：读取期间库在变或有写事务、库的大小不对、打开或读取失败。
// 稍后重试可能成功。具体原因以文本附在其后，不以 %w 包装结构错误。
var errTransient = i18n.Sentinel("KuGou database changed during read")

// errNotDiskFile 表示路径指向的不是磁盘文件（字符设备、管道等）。这是路径本身的性质，不是暂时的：
// pick 据此判该候选不可用、不待重试，免得每次重试都再打开一次设备。
var errNotDiskFile = i18n.Sentinel("not a disk file")

// dbFS 是读库用到的文件操作，测试以内存实现代替。open 返回的 Closer 在一遍读完后关闭。
// exists 分三态：存在为 (true, nil)，不存在为 (false, nil)，查不清为 (false, 错误)。
type dbFS struct {
	open   func(path string) (io.ReaderAt, io.Closer, error)
	size   func(path string) (int64, error)
	exists func(path string) (bool, error)
}

var realFS = dbFS{
	open: func(path string) (io.ReaderAt, io.Closer, error) {
		f, err := openShared(path)
		if err != nil {
			return nil, nil, err
		}
		return f, f, nil
	},
	size: func(path string) (int64, error) {
		fi, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	},
	exists: func(path string) (bool, error) {
		_, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	},
}

// readCandidates 按设计 §6.4 读 dbPath 处的酷狗库，返回 hash 的歌词候选及 collect 的其余结局（collected）。
//
// 不整库读入，经 kgdb 只读解析要用的页；走两遍，每遍重新打开、读完关闭。每遍把读到的页按读取顺序以
// 「LE32 页号 ‖ 该页读入的 1024 字节（解密之前）」喂进一个 SHA-256。两遍摘要相同，即两遍读到的是同一批页、
// 同一顺序、同一内容，结局由这些页确定地得出，取第二遍的。结局含 P 的 LastPlayTime 与是否为本地文件（取自 P 的
// MusicFilePath），同样由这些页得出：摘要相同则两遍的 playedAt、localFile 必然相同，不另行比对。
//
// 结局依次判定：
//   - dbPath-wal 确认存在：包装 kgdb.ErrUnsupportedDB 的错误，不读。查不清（如目录暂时拒绝访问）只是
//     暂时失败：停用整个会话要有确证。
//   - ctx 取消：ctx.Err() 原样返回。每读一页前检查。
//   - -wal 查不清；读前、两遍之间或读后 dbPath-journal 存在或查不清；大小取不到、为 0、不是页大小的整数倍
//     或读后与读前不同；打开失败、读页失败或读不满；两遍摘要不同：包装 errTransient 的错误。
//   - 否则为第二遍的结局：候选，或结构错误（包装 kgdb.ErrUnsupportedDB 或 kgdb.ErrCorrupt，即不支持或损坏）
//     原样返回；其余错误按暂时失败包装（现有的 kgdb 与 collect 不产生这类错误，是兜底，保证调用方只见
//     取消、暂时失败、结构错误三类）。结构错误不在第一遍提前返回：酷狗写到一半的页可能让一遍看到异常，
//     两遍一致才认定。
//
// 不能完全排除的时序（未提交的页在两遍期间不变、在读后检查之前回滚）及其后果见设计 §6.4 末。
func readCandidates(ctx context.Context, dbPath, hash string, fs dbFS) (collected, error) {
	return readConsistent(ctx, dbPath, fs, func(db *kgdb.DB) (collected, error) {
		return collect(db, hash)
	})
}

// readPlayingRow 经两遍页一致骨架只读查询 hash 对应的正在播放队列行 P，仅扫描 PlayQueueAssistItem* 表，
// 跳过 LyricInfo 与其余歌单表。
func readPlayingRow(ctx context.Context, dbPath, hash string, fs dbFS) (playing, error) {
	h := strings.TrimSpace(hash)
	if strings.Trim(h, "0") == "" {
		return playing{}, nil
	}
	return readConsistent(ctx, dbPath, fs, func(db *kgdb.DB) (playing, error) {
		return playingRow(db, h)
	})
}

func readConsistent[T any](ctx context.Context, dbPath string, fs dbFS, fn func(db *kgdb.DB) (T, error)) (T, error) {
	var zero T
	wal := dbPath + "-wal"
	switch found, err := fs.exists(wal); {
	case err != nil:
		return zero, i18n.Errorf("%w: %v", errTransient, err)
	case found:
		return zero, i18n.Errorf("%w: WAL file %s exists", kgdb.ErrUnsupportedDB, wal)
	}
	journal := dbPath + "-journal"
	if err := journalCheck(fs, journal); err != nil {
		return zero, err
	}
	size, err := fs.size(dbPath)
	if err != nil {
		return zero, i18n.Errorf("%w: %v", errTransient, err)
	}
	if size <= 0 || size%kgdb.PageSize != 0 {
		return zero, i18n.Errorf("%w: database size %d is not a positive multiple of %d", errTransient, size, kgdb.PageSize)
	}

	var sums [2][sha256.Size]byte
	var last pass[T]
	for i := range sums {
		if i > 0 {
			if err := journalCheck(fs, journal); err != nil {
				return zero, err
			}
		}
		last = readPass(ctx, dbPath, size, fs, fn)
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if last.readErr != nil {
			return zero, i18n.Errorf("%w: %v", errTransient, last.readErr)
		}
		sums[i] = last.sum
	}

	if err := journalCheck(fs, journal); err != nil {
		return zero, err
	}
	now, err := fs.size(dbPath)
	if err != nil {
		return zero, i18n.Errorf("%w: %v", errTransient, err)
	}
	if now != size {
		return zero, i18n.Errorf("%w: database size changed from %d to %d", errTransient, size, now)
	}
	if sums[0] != sums[1] {
		return zero, i18n.Errorf("%w: pages read in the two passes differ", errTransient)
	}
	if last.err != nil && !errors.Is(last.err, kgdb.ErrUnsupportedDB) && !errors.Is(last.err, kgdb.ErrCorrupt) {
		return zero, i18n.Errorf("%w: %v", errTransient, last.err)
	}
	return last.res, last.err
}

// journalCheck 在 journal 确认不存在时返回 nil；存在（有写事务在进行）或查不清时返回包装 errTransient 的错误。
func journalCheck(fs dbFS, journal string) error {
	found, err := fs.exists(journal)
	if err != nil {
		return i18n.Errorf("%w: %v", errTransient, err)
	}
	if found {
		return i18n.Errorf("%w: journal file %s exists", errTransient, journal)
	}
	return nil
}

// pass 是一遍读取的结局：回调的结局或解析的错误，以及读到的页的摘要。readErr 非空表示打开或读页失败
// （含读不满、取消），本遍作废。
type pass[T any] struct {
	res     T
	err     error
	readErr error
	sum     [sha256.Size]byte
}

// readPass 打开库读一遍：取页层、读 sqlite_master、执行 fn，读完关闭。size 是读前取得的库大小。
func readPass[T any](ctx context.Context, dbPath string, size int64, fs dbFS, fn func(db *kgdb.DB) (T, error)) pass[T] {
	if err := ctx.Err(); err != nil {
		return pass[T]{readErr: err}
	}
	r, c, err := fs.open(dbPath)
	if err != nil {
		return pass[T]{readErr: err}
	}
	defer c.Close()
	pr := &pageReader{ctx: ctx, r: r, sum: sha256.New()}
	var p pass[T]
	p.res, p.err = readDBWith(pr, size, fn)
	p.readErr = pr.err
	pr.sum.Sum(p.sum[:0])
	return p
}

func readDBWith[T any](r io.ReaderAt, size int64, fn func(db *kgdb.DB) (T, error)) (T, error) {
	var zero T
	pg, err := kgdb.NewPager(r, size)
	if err != nil {
		return zero, err
	}
	db, err := kgdb.Open(pg)
	if err != nil {
		return zero, err
	}
	return fn(db)
}

// pageReader 包装一遍读取的 ReaderAt：每次读前检查取消；读满的页以「LE32 页号 ‖ 读入的字节」喂进本遍的
// 摘要（kgdb 的取页层在 ReadAt 返回之后才原地解密）。第一处读失败（含读不满、取消）记在 err：
// kgdb 会把它包装后上抛，这里另记一份，不靠从错误文本里分辨。
type pageReader struct {
	ctx context.Context
	r   io.ReaderAt
	sum hash.Hash
	err error
}

func (p *pageReader) ReadAt(b []byte, off int64) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, p.fail(err)
	}
	n, err := p.r.ReadAt(b, off)
	if n < len(b) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, p.fail(err)
	}
	var no [4]byte
	binary.LittleEndian.PutUint32(no[:], uint32(off/kgdb.PageSize)+1)
	p.sum.Write(no[:])
	p.sum.Write(b)
	return n, nil
}

func (p *pageReader) fail(err error) error {
	if p.err == nil {
		p.err = err
	}
	return err
}

// openShared 只读打开 path，共享读、写、删除：持有句柄期间，酷狗照常读写、改名、删除该文件
// （os.Open 的共享模式不含删除，设计 §3「Windows 打开方式」）。不加锁、不映射。只接受磁盘文件：
// 管道与字符设备读起来可能一直不返回，打开后即关闭并返回包装 errNotDiskFile 的错误。路径按 longPath 处理长度。
func openShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, i18n.Errorf("open %s: %w", path, err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, i18n.Errorf("open %s: %w", path, err)
	}
	if t, _ := windows.GetFileType(h); t != windows.FILE_TYPE_DISK {
		windows.CloseHandle(h)
		return nil, i18n.Errorf("open %s: %w", path, errNotDiskFile)
	}
	return os.NewFile(uintptr(h), path), nil
}

// longPath 给 UTF-16 长度不小于 MAX_PATH（260，含结尾的 NUL 即超限）的绝对路径加 \\?\ 前缀（UNC 路径为
// \\?\UNC\），使系统未开启长路径支持时也能打开；带前缀的路径不再经 Win32 规范化，故先 Clean。
// 其余路径，以及已带 \\?\ 或 \\.\ 前缀的，原样返回。
func longPath(p string) string {
	n := 0
	for _, r := range p {
		n += utf16.RuneLen(r)
	}
	if n < windows.MAX_PATH || !filepath.IsAbs(p) || strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	p = filepath.Clean(p)
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// readLyricFile 以 openShared 读 path 的内容，至多读 klyric.MaxLocalFileBytes+1 字节：超过上限返回
// klyric.ErrLyricTooLarge，不读完整个文件。
func readLyricFile(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, klyric.MaxLocalFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > klyric.MaxLocalFileBytes {
		return nil, klyric.ErrLyricTooLarge
	}
	return b, nil
}
