package kgdb

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

// 本文件只测取页的契约：页号越界、读失败的错误分类、每次取页恰好一次 ReadAt 且不缓存。

func TestPagerOutOfRange(t *testing.T) {
	enc := kgdbtest.Encrypt(plainDB(3))
	reads := 0
	r := readerFunc(func(p []byte, off int64) (int, error) {
		reads++
		return bytes.NewReader(enc).ReadAt(p, off)
	})
	p, err := NewPager(r, int64(len(enc)))
	if err != nil {
		t.Fatal(err)
	}
	reads = 0
	for _, n := range []uint32{0, p.PageCount() + 1, math.MaxUint32} {
		got, err := p.Page(n)
		if got != nil || !errors.Is(err, ErrUnsupportedDB) {
			t.Errorf("Page(%d) = (%v, %v), want (nil, ErrUnsupportedDB)", n, got, err)
		}
	}
	if reads != 0 {
		t.Errorf("out-of-range pages caused %d reads, want 0", reads)
	}
}

// 读失败按暂时性处理：原样包装返回，不归为 ErrUnsupportedDB。
func TestPagerReadError(t *testing.T) {
	enc := kgdbtest.Encrypt(plainDB(3))
	full := bytes.NewReader(enc)

	t.Run("page 1 read fails", func(t *testing.T) {
		r := readerFunc(func([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF })
		_, err := NewPager(r, int64(len(enc)))
		if err == nil || errors.Is(err, ErrUnsupportedDB) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want wrapped io.ErrUnexpectedEOF only", err)
		}
	})

	t.Run("later page read fails", func(t *testing.T) {
		boom := errors.New("boom")
		r := readerFunc(func(p []byte, off int64) (int, error) {
			if off != 0 {
				return 0, boom
			}
			return full.ReadAt(p, off)
		})
		p, err := NewPager(r, int64(len(enc)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.Page(2)
		if got != nil || errors.Is(err, ErrUnsupportedDB) || !errors.Is(err, boom) {
			t.Fatalf("Page(2) = (%v, %v), want (nil, wrapped boom)", got, err)
		}
	})

	t.Run("file shorter than size", func(t *testing.T) {
		p, err := NewPager(full, int64(len(enc))+PageSize)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Page(4)
		if errors.Is(err, ErrUnsupportedDB) || !errors.Is(err, io.EOF) {
			t.Fatalf("Page(4) err = %v, want wrapped io.EOF only", err)
		}
	})

	t.Run("short read without error", func(t *testing.T) {
		r := readerFunc(func(p []byte, off int64) (int, error) {
			n, _ := full.ReadAt(p[:PageSize/2], off)
			return n, nil
		})
		_, err := NewPager(r, int64(len(enc)))
		if errors.Is(err, ErrUnsupportedDB) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want wrapped io.ErrUnexpectedEOF only", err)
		}
	})

	// io.ReaderAt 允许读满时同时返回 io.EOF，这不是失败。
	t.Run("full read with EOF", func(t *testing.T) {
		r := readerFunc(func(p []byte, off int64) (int, error) {
			n, err := full.ReadAt(p, off)
			if err == nil && off+int64(n) == int64(len(enc)) {
				err = io.EOF
			}
			return n, err
		})
		p, err := NewPager(r, int64(len(enc)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Page(3); err != nil {
			t.Fatalf("Page(3): %v", err)
		}
	})
}

func TestPagerOneReadAtPerPage(t *testing.T) {
	enc := kgdbtest.Encrypt(plainDB(3))
	type call struct {
		off int64
		n   int
	}
	var calls []call
	r := readerFunc(func(p []byte, off int64) (int, error) {
		calls = append(calls, call{off, len(p)})
		return bytes.NewReader(enc).ReadAt(p, off)
	})
	p, err := NewPager(r, int64(len(enc)))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (call{0, PageSize}) {
		t.Fatalf("NewPager reads = %v, want [{0 1024}]", calls)
	}

	calls = nil
	for range 2 {
		if _, err := p.Page(2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Page(1); err != nil {
		t.Fatal(err)
	}
	want := []call{{PageSize, PageSize}, {PageSize, PageSize}, {0, PageSize}}
	if len(calls) != len(want) {
		t.Fatalf("reads = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("reads = %v, want %v", calls, want)
		}
	}
}
