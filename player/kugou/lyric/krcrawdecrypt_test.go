package lyric

// 本文件只测 krc1 原始字节的解密（DecryptKRC）：魔数核对、zlib 明文上限，以及在线取词的
// krcDecrypt 改为委托它之后结果不变。

import (
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

type countingReader struct {
	r io.Reader
	n *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// countKRCPlain 让 DecryptKRC 从 zlib 读出的明文字节数累计到返回的计数里，测试结束时还原钩子。
func countKRCPlain(t *testing.T) *int64 {
	t.Helper()
	var n int64
	orig := wrapKRCPlain
	wrapKRCPlain = func(r io.Reader) io.Reader { return countingReader{r: r, n: &n} }
	t.Cleanup(func() { wrapKRCPlain = orig })
	return &n
}

// 不以 krc1 开头、只有魔数、zlib 坏：都报错，且不是超限。
func TestDecryptKRCBadMagic(t *testing.T) {
	valid := krcRaw(t, []byte("[0,10]<0,10,0>x"))
	cases := map[string][]byte{
		"nil":           nil,
		"短于魔数":          []byte("krc"),
		"只有魔数":          []byte("krc1"),
		"魔数大写":          append([]byte("KRC1"), valid[4:]...),
		"魔数不符":          append([]byte("krc0"), valid[4:]...),
		"缺魔数的合法正文":      valid[4:],
		"base64 而非原始字节": []byte(base64.StdEncoding.EncodeToString(valid)),
		"魔数后为乱码":        []byte("krc1 not zlib at all"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			plain, err := DecryptKRC(raw)
			if err == nil {
				t.Fatalf("DecryptKRC 应报错，得明文 %q", plain)
			}
			if errors.Is(err, ErrLyricTooLarge) {
				t.Fatalf("格式错误不应报超限：%v", err)
			}
		})
	}
}

// 压缩后很小、解压后超过 maxKRCPlainBytes 的 krc：读到上限即停并报超限；恰好 maxKRCPlainBytes 仍可解。
func TestDecryptKRCBomb(t *testing.T) {
	read := countKRCPlain(t)
	for _, size := range []int{maxKRCPlainBytes + 1, 4 * maxKRCPlainBytes} {
		raw := krcRaw(t, make([]byte, size))
		if len(raw) >= MaxLocalFileBytes {
			t.Fatalf("夹具压缩后 %d bytes，应小于 %d", len(raw), MaxLocalFileBytes)
		}
		*read = 0
		if _, err := DecryptKRC(raw); !errors.Is(err, ErrLyricTooLarge) {
			t.Fatalf("明文 %d bytes：err = %v，want ErrLyricTooLarge", size, err)
		}
		if *read > maxKRCPlainBytes+1 {
			t.Fatalf("明文 %d bytes：读出 %d bytes，超过 %d", size, *read, maxKRCPlainBytes+1)
		}
		if lines, err := ParseLocal(raw); !errors.Is(err, ErrLyricTooLarge) || lines != nil {
			t.Fatalf("ParseLocal 明文 %d bytes：(%d 行, %v)，want (nil, ErrLyricTooLarge)", size, len(lines), err)
		}
	}

	plain, err := DecryptKRC(krcRaw(t, make([]byte, maxKRCPlainBytes)))
	if err != nil || len(plain) != maxKRCPlainBytes {
		t.Fatalf("明文恰好 %d bytes：(%d bytes, %v)，want 成功", maxKRCPlainBytes, len(plain), err)
	}
}

// 在线取词的 krcDecrypt（base64 → krc1）与 DecryptKRC 对同一份 krc 得出同一明文；非 base64 仍报错。
func TestKRCDecryptOnlineUnchanged(t *testing.T) {
	want := "[ti:t]\n[1000,500]<0,500,0>听说\n"
	raw := krcRaw(t, []byte(want))
	got, err := krcDecrypt(base64.StdEncoding.EncodeToString(raw))
	if err != nil || got != want {
		t.Fatalf("krcDecrypt = (%q, %v)，want %q", got, err, want)
	}
	if got, err := DecryptKRC(raw); err != nil || got != want {
		t.Fatalf("DecryptKRC = (%q, %v)，want %q", got, err, want)
	}
	if _, err := krcDecrypt("!!!not-base64!!!"); err == nil {
		t.Fatal("非 base64 时应报错")
	}
}
