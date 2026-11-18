package assoc

// 本文件只测读库的资源边界（设计 §6.4、§6.9、§6.10）：库再大，内存也不随页数增长；读到中途可取消，
// 取消后不再读页、不开第二遍，返回 context.Canceled。

import (
	"context"
	"encoding/binary"
	"io"
	"runtime"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

const (
	// widePages 是合成库的页数（约 250 MB，为内存上限 32 MiB 的近 8 倍）。任务书原定 1,000,000 页，实测两遍读完
	// 单例 9.8 s（开发机 i9-11900KF，CI 为 2 核），按控制方裁定 B 降到不少于 200,000 页，断言不变。
	widePages    = 250_000
	wideFanout   = 100     // 内部页的子页数
	wideLeafRows = 8       // 每个叶页的行数
	cancelAtRead = 100_000 // 取消用例在第几次读页时取消
)

// wideDB 是按页号逐页生成的加密合成库，不在内存里持有整库。第 1 页为文件头与 sqlite_master（LyricInfo、
// LocalFile 两张表），第 2 页是 LyricInfo 的空叶页，第 3 页起是 LocalFile 的表 b-tree：按堆序排列的完全
// wideFanout 叉树，节点 i 在第 3+i 页，子节点为 wideFanout*i+1 … wideFanout*i+wideFanout 中小于节点总数者，
// 没有子节点的是叶页。叶页各 wideLeafRows 行，hash 与任何样本都不同，LyricKey 为空。
type wideDB struct {
	pages      uint32
	page1      []byte // 第 1、2 页的密文，内容固定
	page2      []byte
	leafPlain  []byte // 叶页明文，各叶页相同
	reads      int    // 累计读页次数
	opens      int
	beforeRead func(reads int) // 非空时在每次读页前调用，reads 含本次
}

func newWideDB(pages uint32) *wideDB {
	w := &wideDB{pages: pages}

	p1 := make([]byte, kgdb.PageSize)
	copy(p1, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(p1[16:], kgdb.PageSize)
	p1[18], p1[19] = 1, 1                          // 读写版本：回滚日志
	p1[20], p1[21], p1[22], p1[23] = 0, 64, 32, 32 // 保留字节 0、载荷比例 64/32/32
	binary.BigEndian.PutUint32(p1[56:], 1)         // 文本编码 UTF-8
	putBTreePage(p1, 100, leafPage, [][]byte{
		tableLeafCell(1, record("table", "LyricInfo", "LyricInfo", 2, "CREATE TABLE LyricInfo(Key, LyricPath, SongHash, UpdateTime)")),
		tableLeafCell(2, record("table", "LocalFile", "LocalFile", 3, "CREATE TABLE LocalFile(MusicFileHash, LyricKey)")),
	}, 0)
	w.page1 = kgdbtest.EncryptPage(1, p1)

	p2 := make([]byte, kgdb.PageSize)
	putBTreePage(p2, 0, leafPage, nil, 0)
	w.page2 = kgdbtest.EncryptPage(2, p2)

	w.leafPlain = make([]byte, kgdb.PageSize)
	var rows [][]byte
	for i := range wideLeafRows {
		rows = append(rows, tableLeafCell(int64(i+1), record(hex32("ff", i), "")))
	}
	putBTreePage(w.leafPlain, 0, leafPage, rows, 0)
	return w
}

// encrypted 返回第 n 页（n ≥ 3 时现场生成）的密文。
func (w *wideDB) encrypted(n uint32) []byte {
	switch n {
	case 1:
		return w.page1
	case 2:
		return w.page2
	}
	total := uint64(w.pages - 2)
	first := wideFanout*uint64(n-3) + 1
	if first >= total {
		return kgdbtest.EncryptPage(n, w.leafPlain)
	}
	last := min(first+wideFanout, total) - 1
	var cells [][]byte
	for c := first; c < last; c++ {
		cell := binary.BigEndian.AppendUint32(nil, uint32(3+c))
		cells = append(cells, append(cell, sqliteVarint(c-first+1)...)) // 键只占 1 字节；读取器不看键
	}
	b := make([]byte, kgdb.PageSize)
	putBTreePage(b, 0, interiorPage, cells, uint32(3+last))
	return kgdbtest.EncryptPage(n, b)
}

func (w *wideDB) fs() dbFS {
	return dbFS{
		open: func(string) (io.ReaderAt, io.Closer, error) {
			w.opens++
			return readerAtFunc(w.readAt), closerFunc(func() error { return nil }), nil
		},
		size:   func(string) (int64, error) { return int64(w.pages) * kgdb.PageSize, nil },
		exists: func(string) (bool, error) { return false, nil },
	}
}

func (w *wideDB) readAt(p []byte, off int64) (int, error) {
	w.reads++
	if w.beforeRead != nil {
		w.beforeRead(w.reads)
	}
	n := uint32(off/kgdb.PageSize) + 1
	if n > w.pages {
		return 0, io.EOF
	}
	return copy(p, w.encrypted(n)), nil
}

// sqliteVarint 按 SQLite 的大端 varint 编码 v（不用 9 字节的形式，v < 2^56）。
func sqliteVarint(v uint64) []byte {
	b := []byte{byte(v & 0x7F)}
	for v >>= 7; v > 0; v >>= 7 {
		b = append([]byte{byte(v&0x7F) | 0x80}, b...)
	}
	return b
}

// record 按 SQLite 记录格式编码一行；列为文本（string）或 0～127 的整数（int）。记录头须短于 128 字节。
func record(cols ...any) []byte {
	var types, body []byte
	for _, c := range cols {
		switch v := c.(type) {
		case string:
			types = append(types, sqliteVarint(uint64(13+2*len(v)))...)
			body = append(body, v...)
		case int:
			types = append(types, 1)
			body = append(body, byte(v))
		}
	}
	return append(append([]byte{byte(1 + len(types))}, types...), body...)
}

// tableLeafCell 编码表叶页的单元格：载荷长度、rowid、载荷（须全在本页）。
func tableLeafCell(rowid int64, payload []byte) []byte {
	c := append(sqliteVarint(uint64(len(payload))), sqliteVarint(uint64(rowid))...)
	return append(c, payload...)
}

// putBTreePage 在 b 的 hdr 偏移处写一个表 b-tree 页：页头、单元格指针数组，单元格从页尾往前放。
func putBTreePage(b []byte, hdr int, kind byte, cells [][]byte, right uint32) {
	b[hdr] = kind
	ptrs := hdr + 8
	if kind == interiorPage {
		ptrs = hdr + 12
		binary.BigEndian.PutUint32(b[hdr+8:], right)
	}
	binary.BigEndian.PutUint16(b[hdr+3:], uint16(len(cells)))
	end := len(b)
	for i, c := range cells {
		end -= len(c)
		copy(b[end:], c)
		binary.BigEndian.PutUint16(b[ptrs+2*i:], uint16(end))
	}
	if end < ptrs+2*len(cells) {
		panic("cells overflow the page")
	}
	binary.BigEndian.PutUint16(b[hdr+5:], uint16(end))
}

// widePages 页、两遍读完，整个调用期间堆占用（HeapInuse，每 4096 次读页采样一次）的增量不超过 32 MiB；
// 在第 cancelAtRead 次读页时取消，readCandidates 不再读页、不开第二遍，返回 context.Canceled。
//
// 实测增量约 3.4 MiB。变异自证（实测）：读页层留住读过的页，增量 857 MiB，红；读页前不查取消，取消一例读满
// 250,002 页，红。
func TestReadCandidatesBoundedMemory(t *testing.T) {
	t.Run("whole database", func(t *testing.T) {
		w := newWideDB(widePages)
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		base, peak := ms.HeapInuse, ms.HeapInuse
		w.beforeRead = func(reads int) {
			if reads%4096 == 0 {
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapInuse)
			}
		}
		got, err := readCandidates(context.Background(), testDBPath, sampleSpectre, w.fs())
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapInuse)
		if err != nil {
			t.Fatalf("readCandidates: %v", err)
		}
		if len(got.cands) != 0 {
			t.Errorf("candidates = %+v, want none", got.cands)
		}
		if w.reads < 2*(widePages-2) {
			t.Fatalf("read %d pages, want both passes over all %d pages", w.reads, widePages)
		}
		growth := int64(peak) - int64(base)
		t.Logf("%d page reads, HeapInuse grew by at most %.1f MiB", w.reads, float64(growth)/(1<<20))
		if growth > 32<<20 {
			t.Errorf("HeapInuse grew by %d bytes during the read, want at most 32 MiB", growth)
		}
	})

	t.Run("canceled midway", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := newWideDB(widePages)
		w.beforeRead = func(reads int) {
			if reads == cancelAtRead {
				cancel()
			}
		}
		_, err := readCandidates(ctx, testDBPath, sampleSpectre, w.fs())
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if w.reads != cancelAtRead || w.opens != 1 {
			t.Errorf("after cancel: %d reads, %d opens; want %d reads, 1 open", w.reads, w.opens, cancelAtRead)
		}
	})
}

// 反例：调用前已取消，不打开库，原样返回 ctx 的错误。
func TestReadCandidatesCanceledBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hash, _ := tonghuaWant(t)
	m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
	_, err := readCandidates(ctx, testDBPath, hash, m.fs())
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if m.opens != 0 {
		t.Errorf("opened %d time(s) after cancel", m.opens)
	}
}
