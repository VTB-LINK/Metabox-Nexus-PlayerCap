package assoc

// 本文件只测读库的结局判定（设计 §6.4 第 2～7 步）：两遍读到的页不同、库的大小不对或变了、打开或读页失败，
// 一律暂时失败；结构错误只有两遍读到同一份内容时才原样返回（不支持或损坏），否则同样暂时失败。
//
// 变异自证（实测，各自注入后对应用例红）：不比较两遍摘要（改页的前三例、首页撕裂、只在一遍出现的结构错误
// 三例）；摘要只喂页号不喂内容（改页的前三例）；结构错误不等第二遍、第一遍即返回（首页撕裂与 StableBadPage
// 全部五例）；读后不核对大小（多一页、少一页）；读前不查页大小的整数倍（1023 等四例）；读失败不作废本遍（打开失败、
// 首遍读失败的几例因多开了一遍）；暂时失败以 %w 带上第二遍的结构错误（只在第二遍出现的两例）；每遍读完不关闭
// （凡打开过的用例）。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"

	"golang.org/x/sys/windows"
)

// wantTransient 断言 err 是暂时失败，且不带结构错误（调用方据后者停用本会话）。
func wantTransient(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want errTransient", err)
	}
	if errors.Is(err, kgdb.ErrUnsupportedDB) || errors.Is(err, kgdb.ErrCorrupt) {
		t.Fatalf("err = %v, transient error must not wrap a structural error", err)
	}
}

func TestReadCandidatesPageChangedBetweenPasses(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	enc := kgdbtest.Encrypt(plain)
	local := tablePages(t, plain, "LocalFile")
	hash, want := tonghuaWant(t)
	for _, c := range []struct {
		name    string
		page    uint32
		changed bool // 改的页是否被读到
	}{
		{"used interior page", pageWithKind(t, plain, local, interiorPage), true},
		{"used leaf page", pageWithKind(t, plain, local, leafPage), true},
		// LyricInfo 的溢出页只装 ExternalInfo，改了它取出的候选不变，只有摘要能发现。
		{"used overflow page", pageWithKind(t, plain, tablePages(t, plain, "LyricInfo"), 0), true},
		{"unused leaf page of ShareFileItems", pageWithKind(t, plain, tablePages(t, plain, "ShareFileItems"), leafPage), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &memFS{t: t, versions: [][]byte{enc, withPageChanged(enc, c.page)}}
			got, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
			if c.changed {
				wantTransient(t, err)
			} else {
				if err != nil {
					t.Fatalf("readCandidates: %v", err)
				}
				wantCandidates(t, got.cands, want)
			}
			m.wantClosed(t)
		})
	}
}

func TestReadCandidatesSizeChanges(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	enc := kgdbtest.Encrypt(plain)
	hash, _ := tonghuaWant(t)
	grown := append(bytes.Clone(enc), make([]byte, kgdb.PageSize)...)
	for _, c := range []struct {
		name      string
		versions  [][]byte
		sizeOf    func(opens int) (int64, error)
		wantOpens int
	}{
		{"one more page before the second pass", [][]byte{enc, grown}, nil, 2},
		{"one page less before the second pass", [][]byte{enc, enc[:len(enc)-kgdb.PageSize]}, nil, 2},
		{"size 1023", [][]byte{enc}, constSize(1023), 0},
		{"size 0", [][]byte{enc}, constSize(0), 0},
		{"size 1025", [][]byte{enc}, constSize(1025), 0},
		{"size not a multiple of the page size", [][]byte{enc}, constSize(int64(len(enc)) - 1), 0},
		{"size unknown before read", [][]byte{enc}, func(int) (int64, error) { return 0, windows.ERROR_SHARING_VIOLATION }, 0},
		{"size unknown after read", [][]byte{enc}, func(opens int) (int64, error) {
			if opens == 2 {
				return 0, windows.ERROR_FILE_NOT_FOUND
			}
			return int64(len(enc)), nil
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &memFS{t: t, versions: c.versions, sizeOf: c.sizeOf}
			_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
			wantTransient(t, err)
			if m.opens != c.wantOpens {
				t.Errorf("opened %d time(s), want %d", m.opens, c.wantOpens)
			}
			m.wantClosed(t)
		})
	}
}

func constSize(n int64) func(int) (int64, error) {
	return func(int) (int64, error) { return n, nil }
}

func TestReadCandidatesOpenFails(t *testing.T) {
	hash, _ := tonghuaWant(t)
	for _, failAt := range []int{0, 1} {
		m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
		m.openErr = func(i int) error {
			if i == failAt {
				return windows.ERROR_SHARING_VIOLATION
			}
			return nil
		}
		_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
		wantTransient(t, err)
		if m.opens != failAt+1 {
			t.Errorf("open #%d fails: opened %d time(s), want %d", failAt, m.opens, failAt+1)
		}
		m.wantClosed(t)
	}
}

// 读页出错或读不满（含库被截短到页中间）为暂时失败，不论是第几遍、哪一页。
func TestReadCandidatesReadFails(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	hash, _ := tonghuaWant(t)
	leaf := pageWithKind(t, plain, tablePages(t, plain, "LocalFile"), leafPage)
	injected := errors.New("injected read failure")
	for _, c := range []struct {
		name string
		pass int    // 第几遍（0 起）出错，-1 为每一遍都在同一处出错
		page uint32 // 读哪一页时出错
		n    int    // 读到的字节数
		err  error
	}{
		{"error on page 1 in the first pass", 0, 1, 0, injected},
		{"error on a leaf page in the second pass", 1, leaf, 0, injected},
		{"short read without error", 0, leaf, kgdb.PageSize / 2, nil},
		{"short read with EOF in the second pass", 1, 1, 100, errors.New("EOF")},
		// 两遍读到的页相同（摘要相同），读失败也不当作库的结局。
		{"error on the same leaf page in every pass", -1, leaf, 0, injected},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := encMemFS(t, plain)
			fs := m.fs()
			open := fs.open
			fs.open = func(path string) (io.ReaderAt, io.Closer, error) {
				pass := m.opens
				r, cl, err := open(path)
				if err != nil {
					return nil, nil, err
				}
				return readerAtFunc(func(p []byte, off int64) (int, error) {
					if (c.pass < 0 || pass == c.pass) && uint32(off/kgdb.PageSize)+1 == c.page {
						return c.n, c.err
					}
					return r.ReadAt(p, off)
				}), cl, nil
			}
			_, err := readCandidates(context.Background(), testDBPath, hash, fs)
			wantTransient(t, err)
			if want := max(c.pass, 0) + 1; m.opens != want {
				t.Errorf("opened %d time(s), want %d (stop at the failing pass)", m.opens, want)
			}
			m.wantClosed(t)
		})
	}
}

// tornPage1 返回 enc 的副本，第 1 页 [16:24] 的明文头被改坏，取页层的头部校验通不过。
func tornPage1(enc []byte) []byte {
	b := bytes.Clone(enc)
	b[21] ^= 0xFF // 最大内嵌载荷比例：64 → 191
	return b
}

func TestReadCandidatesTornFirstPage(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	enc := kgdbtest.Encrypt(plain)
	hash, want := tonghuaWant(t)
	m := &memFS{t: t, versions: [][]byte{tornPage1(enc), enc}}

	_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
	wantTransient(t, err)

	got, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	wantCandidates(t, got.cands, want)
	m.wantClosed(t)
}

// withCycle 返回 plain 的副本：LocalFile 根页（内部页）的第一个子指针指回根页自身。
func withCycle(t *testing.T, plain []byte) []byte {
	b := bytes.Clone(plain)
	root := pageWithKind(t, b, tablePages(t, b, "LocalFile"), interiorPage)
	pg := pageOf(b, root)
	cell := binary.BigEndian.Uint16(pg[12:]) // 内部页的单元格指针数组从页头第 12 字节起
	binary.BigEndian.PutUint32(pg[cell:], root)
	return b
}

func TestReadCandidatesStableBadPage(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	enc := kgdbtest.Encrypt(plain)
	hash, _ := tonghuaWant(t)
	cyclic := kgdbtest.Encrypt(withCycle(t, plain))
	for _, c := range []struct {
		name     string
		versions [][]byte
		want     error // nil 表示暂时失败
	}{
		{"bad page 1 in both passes", [][]byte{tornPage1(enc)}, kgdb.ErrUnsupportedDB},
		{"cyclic interior page in both passes", [][]byte{cyclic}, kgdb.ErrCorrupt},
		// 反例：只在一遍里出现的结构错误不认定。
		{"cyclic interior page in the first pass only", [][]byte{cyclic, enc}, nil},
		{"cyclic interior page in the second pass only", [][]byte{enc, cyclic}, nil},
		{"bad page 1 in the second pass only", [][]byte{enc, tornPage1(enc)}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &memFS{t: t, versions: c.versions}
			_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
			if c.want == nil {
				wantTransient(t, err)
			} else {
				if !errors.Is(err, c.want) {
					t.Fatalf("err = %v, want %v", err, c.want)
				}
				if errors.Is(err, errTransient) {
					t.Errorf("err = %v, must not be transient", err)
				}
			}
			if m.opens != 2 {
				t.Errorf("opened %d time(s), want 2: a structural error is confirmed by the second pass", m.opens)
			}
			m.wantClosed(t)
		})
	}
}
