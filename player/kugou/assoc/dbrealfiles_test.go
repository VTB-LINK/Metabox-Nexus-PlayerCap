package assoc

// 本文件只测 realFS 接到真文件上的读库：t.TempDir() 里的加密夹具能读出候选，-journal、-wal 与库不存在
// 按设计 §6.4 判定。不碰真实的酷狗数据库。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

func TestReadCandidatesRealFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "KGMusicV3.db")
	if err := os.WriteFile(path, kgdbtest.Encrypt(fixturePlain(t, "kugou_like.db")), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, want := tonghuaWant(t)
	read := func() (collected, error) { return readCandidates(context.Background(), path, hash, realFS) }

	got, err := read()
	if err != nil {
		t.Fatalf("readCandidates: %v", err)
	}
	wantCandidates(t, got.cands, want)

	touch := func(p string) {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	touch(path + "-journal")
	if _, err := read(); !errors.Is(err, errTransient) {
		t.Errorf("with -journal: err = %v, want errTransient", err)
	}
	os.Remove(path + "-journal")
	touch(path + "-wal")
	if _, err := read(); !errors.Is(err, kgdb.ErrUnsupportedDB) || errors.Is(err, errTransient) {
		t.Errorf("with -wal: err = %v, want ErrUnsupportedDB", err)
	}
	os.Remove(path + "-wal")
	// 库不存在（如酷狗正在替换它）为暂时失败，不是不支持。
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the database after reading: %v", err)
	}
	if _, err := read(); !errors.Is(err, errTransient) {
		t.Errorf("missing database: err = %v, want errTransient", err)
	}
}

// realFS.exists 分三态：存在为 (true, nil)，不存在（含上级目录不存在）为 (false, nil)，查不清为 (false, 错误)。
func TestRealFSExistsThreeStates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "KGMusicV3.db-journal")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path string
		want, err  bool
	}{
		{"existing", file, true, false},
		{"missing", filepath.Join(dir, "KGMusicV3.db-wal"), false, false},
		{"missing directory", filepath.Join(dir, "missing", "KGMusicV3.db-wal"), false, false},
		{"unknown", filepath.Join(dir, "a\x00", "KGMusicV3.db-wal"), false, true},
	} {
		got, err := realFS.exists(c.path)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: exists = %v, %v; want %v, error %v", c.name, got, err, c.want, c.err)
		}
	}
}

// 旁文件查不清（os.Stat 返回「不存在」以外的错误，这里以路径里的 NUL 造出）：暂时失败，不判不支持。
//
// 变异自证（实测）：realFS.exists 把查不清当作存在（审查第 1 轮之前的写法），本用例红（ErrUnsupportedDB）。
func TestReadCandidatesRealFilesSideFileUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a\x00", "KGMusicV3.db")
	hash, _ := tonghuaWant(t)
	_, err := readCandidates(context.Background(), path, hash, realFS)
	if !errors.Is(err, errTransient) || errors.Is(err, kgdb.ErrUnsupportedDB) {
		t.Errorf("err = %v, want errTransient and not ErrUnsupportedDB", err)
	}
}
