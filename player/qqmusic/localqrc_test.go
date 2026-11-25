package qqmusic

// 本文件只测 QQ 缓存目录里 qrc 文件的解密（decryptLocalQRC，设计 §2.3、§6.6 第 2 步）：文件头的判读、
// qmc1 静态表与下标规则、解不开的几种形状、明文上限与解压时的内存。

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
)

// 实测的文件头即「[offset:0]\n」与 qmc1 表前 11 字节的逐字节异或；钉住测试的表与出货的文件头常量。
func TestQRCHeaderIsQMC1OfOffsetTag(t *testing.T) {
	want := []byte{0x98, 0x25, 0xb0, 0xac, 0xe3, 0x02, 0x83, 0x68, 0xe8, 0xfc, 0x6c}
	tag := []byte("[offset:0]\n")
	got := make([]byte, len(tag))
	for i := range tag {
		got[i] = tag[i] ^ testQMC1Key[i]
	}
	if !bytes.Equal(got, want) {
		t.Errorf("[offset:0]\\n XOR testQMC1Key[:11] = % x, want % x", got, want)
	}
	if !bytes.Equal(qrcFileHeader, want) {
		t.Errorf("qrcFileHeader = % x, want % x", qrcFileHeader, want)
	}
}

// 变异自证（实测）：出货表改动一个字节即红。
func TestDecryptLocalQRCRoundTrip(t *testing.T) {
	plain := qrcXML("[ti:测试歌]\n[1000,2000]第(1000,500)一(1500,500)行(2000,1000)\n")
	got, isQRC, err := decryptLocalQRC(qrcFile(t, plain))
	if err != nil || !isQRC || got != plain {
		t.Fatalf("decryptLocalQRC = %q, %v, %v; want the plaintext, true, nil", got, isQRC, err)
	}
}

// 文件长于 0x7fff + 0x80 字节：下标走到 i % 0x7fff 那一支。
// 变异自证（实测）：`i > 0x7fff` 改为 `i >= 0x7fff`、去掉取模只留 `& 0x7f`，各自即红。
func TestDecryptLocalQRCIndexWrapsAt0x7fff(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	plain := make([]byte, 48<<10)
	for i := range plain {
		plain[i] = byte(r.Uint32())
	}
	file := qrcFileFromZlib(zlibOf(t, plain))
	if len(file) <= 0x7fff+0x80 {
		t.Fatalf("fixture: file is %d bytes, want more than %d", len(file), 0x7fff+0x80)
	}
	got, isQRC, err := decryptLocalQRC(file)
	if err != nil || !isQRC || got != string(plain) {
		t.Fatalf("decryptLocalQRC of a %d-byte file: isQRC=%v err=%v equal=%v; want the plaintext", len(file), isQRC, err, got == string(plain))
	}
}

// 前 11 字节不是文件头：不是 qrc，交给调用方按文本解码，不报错。
func TestDecryptLocalQRCWithoutHeader(t *testing.T) {
	valid := qrcFile(t, qrcXML("[0,1]a(0,1)\n"))
	flipped := bytes.Clone(valid)
	flipped[10] ^= 1
	for name, b := range map[string][]byte{
		"nil":                     nil,
		"plain LRC":               []byte("[00:01.00]第一行\n"),
		"UTF-16LE BOM":            {0xff, 0xfe, '[', 0},
		"first 10 header bytes":   valid[:10],
		"one header byte flipped": flipped,
		"ciphertext without the qmc1 layer": func() []byte {
			b := bytes.Clone(valid)
			testQMC1(b)
			return b[11:]
		}(),
	} {
		got, isQRC, err := decryptLocalQRC(b)
		if got != "" || isQRC || err != nil {
			t.Errorf("%s: decryptLocalQRC = %q, %v, %v; want \"\", false, nil", name, got, isQRC, err)
		}
	}
}

// 带文件头而解不开：都报错，第二个返回值仍为真。
func TestDecryptLocalQRCRejects(t *testing.T) {
	valid := qrcFile(t, qrcXML("[0,1]a(0,1)\n"))
	notZlib := func() []byte {
		b := append([]byte("[offset:0]\n"), bytes.Repeat([]byte("not zlib"), 16)...)
		testQMC1(b)
		return b
	}()
	truncatedZlib := func() []byte {
		z := zlibOf(t, []byte(strings.Repeat("[0,1]a(0,1)\n", 200)))
		return qrcFileFromZlib(z[:len(z)/2])
	}()
	for name, b := range map[string][]byte{
		"header only":                   qrcFileHeader,
		"length minus 11 is 1":          append(bytes.Clone(qrcFileHeader), 0),
		"one byte past a block":         append(bytes.Clone(valid), 0),
		"one byte short of a block":     valid[:len(valid)-1],
		"seven bytes past a block":      append(bytes.Clone(valid), make([]byte, 7)...),
		"3DES output is not zlib":       notZlib,
		"zlib stream cut in the middle": truncatedZlib,
	} {
		got, isQRC, err := decryptLocalQRC(b)
		if err == nil || !isQRC || got != "" {
			t.Errorf("%s: decryptLocalQRC = %q, %v, %v; want \"\", true, an error", name, got, isQRC, err)
		}
	}
}

// 明文恰好 4 MiB 可用，多 1 字节即超限。
// 变异自证（实测）：上限判断 `>` 改为 `>=`，4 MiB 一例红。
func TestDecryptLocalQRCPlainLimit(t *testing.T) {
	got, isQRC, err := decryptLocalQRC(zeroQRCFile(t, 4<<20))
	if err != nil || !isQRC || len(got) != 4<<20 {
		t.Errorf("4 MiB plaintext: len=%d isQRC=%v err=%v; want 4 MiB, true, nil", len(got), isQRC, err)
	}
	got, isQRC, err = decryptLocalQRC(zeroQRCFile(t, 4<<20+1))
	if !errors.Is(err, errLocalTooLarge) || !isQRC || got != "" {
		t.Errorf("4 MiB + 1 plaintext: len=%d isQRC=%v err=%v; want \"\", true, errLocalTooLarge", len(got), isQRC, err)
	}
}

// 高压缩比：64 KB 量级的文件解压出 64 MiB。读到上限加一字节即停，整个调用分配的内存不到 32 MiB。
// 变异自证（实测）：去掉解压的 io.LimitReader（保留长度判断）即红。
func TestDecryptLocalQRCBombStopsEarly(t *testing.T) {
	bomb := zeroQRCFile(t, 64<<20)
	if len(bomb) > 1<<20 {
		t.Fatalf("fixture: bomb is %d bytes, want a high compression ratio", len(bomb))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, isQRC, err := decryptLocalQRC(bomb)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errLocalTooLarge) || !isQRC {
		t.Errorf("64 MiB plaintext: isQRC=%v err=%v; want true, errLocalTooLarge", isQRC, err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n >= 32<<20 {
		t.Errorf("decryptLocalQRC allocated %d bytes for a %d-byte file, want less than 32 MiB", n, len(bomb))
	}
}
