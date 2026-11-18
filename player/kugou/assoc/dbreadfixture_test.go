package assoc

// 本文件只放读库（readCandidates）用例共用的夹具：读入并加密 testdata 下的明文库、以内存实现 dbFS 的
// memFS、在明文库上定位某张表的页。本身不含用例。所有用例都不碰真实的酷狗文件。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

// testDBPath 是交给 readCandidates 的库路径。memFS 只核对传入的路径，不访问它。
const testDBPath = `C:\Users\user\AppData\Roaming\KuGou8\KGMusicV3.db`

// fixturePlain 读入 testdata 下的明文夹具（由 kgdb/testdata/gen_fixtures.py 生成）。
func fixturePlain(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// fixtureFor 返回 designSamples 里一个样本所在的明文库：关联之后的状态在 kugou_like.db，之前在
// kugou_like_unassociated.db（设计 §2.3 样本 1、2 是同一首歌关联前后的两个状态）。
func fixtureFor(t testing.TB, associated bool) []byte {
	if associated {
		return fixturePlain(t, "kugou_like.db")
	}
	return fixturePlain(t, "kugou_like_unassociated.db")
}

// sampleNamed 按名取 designSamples 里的样本。
func sampleNamed(t testing.TB, name string) (hash string, want []candidate) {
	t.Helper()
	for _, s := range designSamples {
		if s.name == name {
			return s.hash, s.want
		}
	}
	t.Fatalf("no design sample %q", name)
	return "", nil
}

// tonghuaWant 返回童话镇样本（在 kugou_like.db 里）的 hash 与期望候选。多数用例以它作「读库成功」的判据：
// 它的关联在 PlayQueueAssistItem_0 里，取到它要扫歌曲条目表与 LyricInfo。
func tonghuaWant(t testing.TB) (string, []candidate) {
	return sampleNamed(t, "在线歌曲、手动关联 lrc")
}

func pageOf(b []byte, n uint32) []byte { return b[(n-1)*kgdb.PageSize : n*kgdb.PageSize] }

// readerAtFunc 把函数当 io.ReaderAt 用。
type readerAtFunc func(p []byte, off int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// memFS 以内存里的加密库实现 dbFS，按打开的次数注入各种时序。
//
// versions[i] 是第 i 次打开（0 起）读到的内容，打开次数多于份数时取最后一份；size 默认返回最近一次打开
// 读到的那份（还没打开过时为第一份）的长度。openErr、sizeOf、side 非空时分别决定第 i 次打开是否失败、
// size 的结果、dbPath 加某后缀的文件是否存在（opens 为此前打开的次数）；sideErr 非空且返回非 nil 时，
// 该旁文件查不清（exists 返回该错误），先于 side 判。
type memFS struct {
	t        *testing.T
	versions [][]byte
	openErr  func(i int) error
	sizeOf   func(opens int) (int64, error)
	side     func(suffix string, opens int) bool
	sideErr  func(suffix string, opens int) error

	opens int      // 打开的次数，含失败的
	live  int      // 已打开、尚未关闭的句柄数
	reads []uint32 // 各次 ReadAt 的页号，按顺序
}

func (m *memFS) current() []byte {
	return m.versions[min(max(m.opens-1, 0), len(m.versions)-1)]
}

func (m *memFS) checkPath(path string) {
	if path != testDBPath {
		m.t.Errorf("path = %q, want %q", path, testDBPath)
	}
}

func (m *memFS) fs() dbFS {
	return dbFS{
		open: func(path string) (io.ReaderAt, io.Closer, error) {
			m.checkPath(path)
			i := m.opens
			m.opens++
			if m.openErr != nil {
				if err := m.openErr(i); err != nil {
					return nil, nil, err
				}
			}
			b := m.current()
			m.live++
			r := readerAtFunc(func(p []byte, off int64) (int, error) {
				m.reads = append(m.reads, uint32(off/kgdb.PageSize)+1)
				return bytes.NewReader(b).ReadAt(p, off)
			})
			return r, closerFunc(func() error { m.live--; return nil }), nil
		},
		size: func(path string) (int64, error) {
			m.checkPath(path)
			if m.sizeOf != nil {
				return m.sizeOf(m.opens)
			}
			return int64(len(m.current())), nil
		},
		exists: func(path string) (bool, error) {
			suffix, ok := strings.CutPrefix(path, testDBPath)
			if !ok || suffix == "" {
				m.t.Errorf("exists(%q): not a side file of %s", path, testDBPath)
			}
			if m.sideErr != nil {
				if err := m.sideErr(suffix, m.opens); err != nil {
					return false, err
				}
			}
			return m.side != nil && m.side(suffix, m.opens), nil
		},
	}
}

// encMemFS 以夹具 plain 的加密内容建 memFS，各次打开读到的都是它。
func encMemFS(t *testing.T, plain []byte) *memFS {
	return &memFS{t: t, versions: [][]byte{kgdbtest.Encrypt(plain)}}
}

// wantClosed 断言 readCandidates 返回时每遍打开的句柄都已关闭。
func (m *memFS) wantClosed(t *testing.T) {
	t.Helper()
	if m.live != 0 {
		t.Errorf("%d handle(s) left open", m.live)
	}
}

// plainPager 直接按页切明文库，按顺序记下取过的页号。
type plainPager struct {
	b     []byte
	pages []uint32
}

func (p *plainPager) PageCount() uint32 { return uint32(len(p.b) / kgdb.PageSize) }

func (p *plainPager) Page(n uint32) ([]byte, error) {
	p.pages = append(p.pages, n)
	return bytes.Clone(pageOf(p.b, n)), nil
}

// tablePages 返回明文库里 table 的全部页（内部页、叶页与溢出页）：从 sqlite_master 登记的根页出发，
// 经 kgdb 的表 b-tree 遍历（其正确性由 kgdb 的用例覆盖）记下取过的页。
func tablePages(t testing.TB, plain []byte, table string) []uint32 {
	t.Helper()
	p := &plainPager{b: plain}
	db, err := kgdb.Open(p)
	if err != nil {
		t.Fatalf("open plaintext fixture: %v", err)
	}
	cols, err := db.Columns(table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	p.pages = nil
	if err := db.Scan(table, cols[:1], func([]kgdb.Value) bool { return true }); err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	return p.pages
}

// 表 b-tree 页的类型字节（非第 1 页在页首）；溢出页的页首是下一页的页号，小库里首字节为 0。
const (
	interiorPage = 0x05
	leafPage     = 0x0D
)

// pageWithKind 返回 pages 中第一个类型字节为 kind 的页；kind 为 0 时找溢出页（既非内部页也非叶页）。
func pageWithKind(t testing.TB, plain []byte, pages []uint32, kind byte) uint32 {
	t.Helper()
	for _, n := range pages {
		if n == 1 {
			continue
		}
		k := pageOf(plain, n)[0]
		if k == kind || kind == 0 && k != interiorPage && k != leafPage {
			return n
		}
	}
	t.Fatalf("fixture: no page of kind 0x%02X among %v", kind, pages)
	return 0
}

// withPageChanged 返回 enc 的副本，第 n 页中间一个字节取反（密文改动，解密后该页面目全非）。
func withPageChanged(enc []byte, n uint32) []byte {
	b := bytes.Clone(enc)
	pageOf(b, n)[kgdb.PageSize/2] ^= 0xFF
	return b
}
