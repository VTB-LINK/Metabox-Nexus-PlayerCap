package assoc

// 本文件只测读库对库旁文件的判定（设计 §6.4 第 1、4、6 步）：-wal 存在即不支持、不读；-journal 在读前、
// 两遍之间或读后存在即暂时失败；两者查不清（stat 报「不存在」以外的错误）一律暂时失败；其余旁文件不影响。
//
// 变异自证（实测）：去掉 -wal 检查，WAL 一例红；去掉读前、两遍之间、读后任一处 -journal 检查，对应一例红。

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"

	"golang.org/x/sys/windows"
)

func TestReadCandidatesWalUnsupported(t *testing.T) {
	hash, _ := tonghuaWant(t)
	m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
	m.side = func(suffix string, _ int) bool { return suffix == "-wal" }

	_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
	if !errors.Is(err, kgdb.ErrUnsupportedDB) {
		t.Fatalf("err = %v, want ErrUnsupportedDB", err)
	}
	if errors.Is(err, errTransient) {
		t.Errorf("err = %v, must not be transient", err)
	}
	if m.opens != 0 {
		t.Errorf("opened the database %d time(s) with -wal present", m.opens)
	}
}

func TestReadCandidatesJournal(t *testing.T) {
	hash, _ := tonghuaWant(t)
	for _, c := range []struct {
		name  string
		opens int // -journal 在此前已打开这么多次时出现
	}{
		{"before read", 0},
		{"between passes", 1},
		{"after read", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
			m.side = func(suffix string, opens int) bool { return suffix == "-journal" && opens == c.opens }

			_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
			if !errors.Is(err, errTransient) {
				t.Fatalf("err = %v, want errTransient", err)
			}
			if errors.Is(err, kgdb.ErrUnsupportedDB) {
				t.Errorf("err = %v, must not be unsupported", err)
			}
			if m.opens != c.opens {
				t.Errorf("opened %d time(s), want %d", m.opens, c.opens)
			}
			m.wantClosed(t)
		})
	}
}

// -wal 查不清（stat 返回「不存在」以外的错误，如目录暂时拒绝访问）：暂时失败、不判不支持、不打开库；
// 访问恢复（查得清、不存在）后再读即成功。-journal 在读前、两遍之间、读后查不清，同为暂时失败。
//
// 变异自证（实测）：-wal 查不清时判不支持，-wal 一例红（真文件的 TestReadCandidatesRealFilesSideFileUnknown
// 同时红）；-journal 查不清当作不存在（三处共用 journalCheck），三例红。
func TestReadCandidatesSideFileUnknown(t *testing.T) {
	hash, want := tonghuaWant(t)
	denied := &fs.PathError{Op: "GetFileAttributesEx", Path: testDBPath + "-wal", Err: windows.ERROR_ACCESS_DENIED}

	t.Run("-wal", func(t *testing.T) {
		m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
		failing := true
		m.sideErr = func(suffix string, _ int) error {
			if suffix == "-wal" && failing {
				return denied
			}
			return nil
		}
		_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
		wantTransient(t, err)
		if m.opens != 0 {
			t.Errorf("opened the database %d time(s) while -wal is unknown", m.opens)
		}

		failing = false
		got, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
		if err != nil {
			t.Fatalf("after access recovers: %v", err)
		}
		wantCandidates(t, got.cands, want)
	})

	for _, c := range []struct {
		name  string
		opens int // -journal 在此前已打开这么多次时查不清
	}{
		{"-journal before read", 0},
		{"-journal between passes", 1},
		{"-journal after read", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
			m.sideErr = func(suffix string, opens int) error {
				if suffix == "-journal" && opens == c.opens {
					return windows.ERROR_ACCESS_DENIED
				}
				return nil
			}
			_, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
			wantTransient(t, err)
			if m.opens != c.opens {
				t.Errorf("opened %d time(s), want %d", m.opens, c.opens)
			}
			m.wantClosed(t)
		})
	}
}

// 反例：-shm、.bak 等其他旁文件不影响读库。
func TestReadCandidatesIgnoresOtherSideFiles(t *testing.T) {
	hash, want := tonghuaWant(t)
	m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
	m.side = func(suffix string, _ int) bool { return suffix == "-shm" || suffix == ".bak" }

	got, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
	if err != nil {
		t.Fatalf("readCandidates: %v", err)
	}
	wantCandidates(t, got.cands, want)
}
