package kgdbtest

import (
	"bytes"
	"testing"
)

// 本文件只测 Encrypt 与 EncryptPage 的约定：输入长度约束、不改动输入、第 1 页的明文头与密文布局、
// 逐页加密与整库加密逐字节相同。

func TestEncryptRejectsBadLength(t *testing.T) {
	for _, n := range []int{0, 1, 1023, 1025, 2047} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Encrypt(len=%d) did not panic", n)
				}
			}()
			Encrypt(make([]byte, n))
		}()
	}
}

func TestEncryptLayout(t *testing.T) {
	plain := make([]byte, 2*pageSize)
	for i := range plain {
		plain[i] = byte(i % 251)
	}
	orig := bytes.Clone(plain)

	enc := Encrypt(plain)

	if !bytes.Equal(plain, orig) {
		t.Fatal("Encrypt modified its input")
	}
	if len(enc) != len(plain) {
		t.Fatalf("len = %d, want %d", len(enc), len(plain))
	}
	if !bytes.Equal(enc[:8], make([]byte, 8)) {
		t.Errorf("page 1 [0:8] = % X, want zeros", enc[:8])
	}
	if !bytes.Equal(enc[16:24], plain[16:24]) {
		t.Errorf("page 1 [16:24] = % X, want plaintext % X", enc[16:24], plain[16:24])
	}
	if bytes.Equal(enc[24:pageSize], plain[24:pageSize]) {
		t.Error("page 1 [24:1024] left unencrypted")
	}
	if bytes.Equal(enc[pageSize:], plain[pageSize:]) {
		t.Error("page 2 left unencrypted")
	}
}

func TestEncryptPageMatchesEncrypt(t *testing.T) {
	plain := make([]byte, 3*pageSize)
	for i := range plain {
		plain[i] = byte(i % 253)
	}
	enc := Encrypt(plain)
	for n := uint32(1); n <= 3; n++ {
		src := plain[(n-1)*pageSize : n*pageSize]
		orig := bytes.Clone(src)
		if got := EncryptPage(n, src); !bytes.Equal(got, enc[(n-1)*pageSize:n*pageSize]) {
			t.Errorf("EncryptPage(%d) differs from page %d of Encrypt", n, n)
		}
		if !bytes.Equal(src, orig) {
			t.Errorf("EncryptPage(%d) modified its input", n)
		}
	}
	// 同一份明文放在不同页号上，密文不同：页号确实进了密钥与 IV。
	if bytes.Equal(EncryptPage(2, plain[pageSize:2*pageSize]), EncryptPage(3, plain[pageSize:2*pageSize])) {
		t.Error("EncryptPage ignores the page number")
	}
}

func TestEncryptPageRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		n   uint32
		len int
	}{{0, pageSize}, {1, 0}, {2, pageSize - 1}, {2, pageSize + 1}, {2, 2 * pageSize}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("EncryptPage(n=%d, len=%d) did not panic", c.n, c.len)
				}
			}()
			EncryptPage(c.n, make([]byte, c.len))
		}()
	}
}
