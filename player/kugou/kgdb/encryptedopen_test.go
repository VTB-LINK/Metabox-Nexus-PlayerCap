package kgdb

import (
	"bytes"
	"slices"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

// 本文件只测读取器经真实取页层（NewPager）读按酷狗格式加密的库，与读明文得到同样的结果。

func TestOpenEncryptedFixture(t *testing.T) {
	plain := loadFixture(t, "basic.db")
	enc := kgdbtest.Encrypt(plain)
	db := mustOpen(t, mustPager(t, enc))
	if got, want := db.Tables(), mustOpen(t, plainPager(plain)).Tables(); !slices.Equal(got, want) {
		t.Fatalf("Tables() = %q, want %q", got, want)
	}
	rows, err := scanAll(t, db, "LocalFile")
	if err != nil || rows != 900 {
		t.Fatalf("LocalFile: %d rows, err %v; want 900", rows, err)
	}

	// 解密之后的一页被改坏，读取器照样判结构异常（不是取页层的不支持）。
	root := rootOf(t, plain, "LocalFile")
	bad := bytes.Clone(plain)
	page(bad, root)[0] = 0x0A
	_, err = scanAll(t, mustOpen(t, mustPager(t, kgdbtest.Encrypt(bad))), "LocalFile")
	wantErr(t, err, ErrCorrupt)
}
