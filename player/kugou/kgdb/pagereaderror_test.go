package kgdb

import (
	"errors"
	"testing"
)

// 本文件只测取页失败的上报：Pager 的错误原样上抛，不归入 ErrCorrupt 或 ErrUnsupportedDB
// （调用方按暂时性失败处理，设计 §6.4）。

var errRead = errors.New("injected read failure")

// failingPager 在取第 failAt 页时返回 errRead。
type failingPager struct {
	plainPager
	failAt uint32
}

func (f failingPager) Page(n uint32) ([]byte, error) {
	if n == f.failAt {
		return nil, errRead
	}
	return f.plainPager.Page(n)
}

func wantReadErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errRead) || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrUnsupportedDB) {
		t.Fatalf("err = %v, want the injected read error only", err)
	}
}

func TestOpenPassesPageReadError(t *testing.T) {
	_, err := Open(failingPager{plainPager(loadFixture(t, "basic.db")), 1})
	wantReadErr(t, err)
}

func TestScanPassesPageReadError(t *testing.T) {
	b := loadFixture(t, "basic.db")
	leaves := leafPages(b, rootOf(t, b, "LocalFile"))
	db := mustOpen(t, failingPager{plainPager(b), leaves[len(leaves)/2]})
	_, err := scanAll(t, db, "LocalFile")
	wantReadErr(t, err)
}

func TestScanPassesOverflowReadError(t *testing.T) {
	b := loadFixture(t, "basic.db")
	db := mustOpen(t, failingPager{plainPager(b), overflowHead(t, b, "LyricInfo")})
	_, err := scanAll(t, db, "LyricInfo")
	wantReadErr(t, err)
}
