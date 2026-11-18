package kgdb

import (
	"bytes"
	"encoding/binary"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

// 本文件只测解密结果是否正确：往返、走到 IV 递推修正分支的页、单页库、第 1 页不参与解密的前缀、
// 篡改只影响所在页，以及真实酷狗库的已知答案。

func TestPagerRoundTrip(t *testing.T) {
	plain := plainDB(3)
	enc := kgdbtest.Encrypt(plain)
	if bytes.Equal(enc[:16], sqliteMagic16) || bytes.Equal(page(enc, 2), page(plain, 2)) {
		t.Fatal("fixture not encrypted")
	}

	p := mustPager(t, enc)
	if got := p.PageCount(); got != 3 {
		t.Fatalf("PageCount = %d, want 3", got)
	}
	for n := uint32(1); n <= 3; n++ {
		got, err := p.Page(n)
		if err != nil {
			t.Fatalf("Page(%d): %v", n, err)
		}
		if !bytes.Equal(got, page(plain, n)) {
			t.Errorf("Page(%d) differs from plaintext", n)
		}
	}
	got, _ := p.Page(1)
	if !bytes.Equal(got[:16], sqliteMagic16) {
		t.Errorf("Page(1)[:16] = %q, want SQLite magic", got[:16])
	}
}

// 第 7 页是 IV 种子递推首次走到「最高位为 1 时加 0x7FFFFF07」那一支的页号（第 4 次递推），
// 前 6 页都走不到，3 页的往返测试覆盖不了这一支。
func TestPagerIVSeedHighBitPage(t *testing.T) {
	plain := plainDB(8)
	p := mustPager(t, kgdbtest.Encrypt(plain))
	if got, err := p.Page(7); err != nil || !bytes.Equal(got, page(plain, 7)) {
		t.Fatalf("Page(7) = (equal=%v, %v), want plaintext", bytes.Equal(got, page(plain, 7)), err)
	}
}

// 单页库是最小的合法库。
func TestPagerSinglePage(t *testing.T) {
	plain := plainDB(1)
	p := mustPager(t, kgdbtest.Encrypt(plain))
	if p.PageCount() != 1 {
		t.Fatalf("PageCount = %d, want 1", p.PageCount())
	}
	if got, err := p.Page(1); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Page(1) = (equal=%v, %v), want plaintext", bytes.Equal(got, plain), err)
	}
}

// 第 1 页的 [0:8] 不参与解密，改了不影响结果。
func TestPagerPage1PrefixIgnored(t *testing.T) {
	plain := plainDB(2)
	enc := kgdbtest.Encrypt(plain)
	copy(enc[:8], []byte{0xEA, 0xB6, 0xAE, 0x18, 0xCC, 0x71, 0x36, 0xD9})
	got, err := mustPager(t, enc).Page(1)
	if err != nil || !bytes.Equal(got, page(plain, 1)) {
		t.Fatalf("Page(1) = (equal=%v, %v), want plaintext", bytes.Equal(got, page(plain, 1)), err)
	}
}

func TestPagerTamperedPageIsolated(t *testing.T) {
	plain := plainDB(3)
	enc := kgdbtest.Encrypt(plain)
	enc[2*PageSize+500] ^= 0x01
	p := mustPager(t, enc)

	if got, err := p.Page(2); err != nil || !bytes.Equal(got, page(plain, 2)) {
		t.Errorf("Page(2) = (equal=%v, %v), want plaintext", bytes.Equal(got, page(plain, 2)), err)
	}
	// 第 n≥2 页没有完整性校验：解出的是错的内容，而不是错误。
	if got, err := p.Page(3); err != nil || bytes.Equal(got, page(plain, 3)) {
		t.Errorf("Page(3) = (equal=%v, %v), want different content and nil error", bytes.Equal(got, page(plain, 3)), err)
	}
}

// 真实酷狗库（KGMusicV3.db，16785 页，2026-11 取自本机酷狗）第 1 页的原始前 32 字节，其余补零。
// CBC 解密第一个分组只依赖第一个密文分组，所以完整性校验与解密后的 [16:32] 都能由这 32 字节核对；
// 往返测试的加密与解密出自同一份规格，只有这一例把密钥与 IV 的派生钉在酷狗的实际输出上。
func TestPagerKuGouKnownAnswer(t *testing.T) {
	raw := make([]byte, PageSize)
	copy(raw, []byte{
		0xEA, 0xB6, 0xAE, 0x18, 0xCC, 0x71, 0x36, 0xD9, // 任意
		0x18, 0x48, 0x6C, 0x97, 0xB4, 0xA4, 0xBF, 0xAA, // 密文分组的前 8 字节
		0x04, 0x00, 0x01, 0x01, 0x00, 0x40, 0x20, 0x20, // 明文头
		0x4F, 0x25, 0x0B, 0xC6, 0xE3, 0x12, 0xA6, 0xCF, // 密文分组的后 8 字节
	})
	got, err := mustPager(t, raw).Page(1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:16], sqliteMagic16) {
		t.Errorf("[:16] = %q, want SQLite magic", got[:16])
	}
	// 偏移 28 是头中记录的库页数，与该文件的大小 17187840 字节 ÷ 1024 一致。
	if n := binary.BigEndian.Uint32(got[28:32]); n != 16785 {
		t.Errorf("header page count = %d, want 16785", n)
	}
}
