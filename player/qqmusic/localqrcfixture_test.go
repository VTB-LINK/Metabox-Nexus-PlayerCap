package qqmusic

// 本文件只放本地歌词测试的共用夹具：测试自带的 qmc1 静态表与 qrc 文件的加密方向、写临时文件。本身不含测试。

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"testing"
)

// testQMC1Key 是 qmc1 静态表（jixunmoe/qmc-decode，LDDC 的 qmc1.py 沿用）。与出货代码的 qmc1Key 各写一份：
// 出货的表被误改时夹具与解密不再互逆，往返用例随之失败。前 11 字节由 TestQRCHeaderIsQMC1OfOffsetTag 对照实测的文件头。
var testQMC1Key = [128]byte{
	0xc3, 0x4a, 0xd6, 0xca, 0x90, 0x67, 0xf7, 0x52, 0xd8, 0xa1, 0x66, 0x62, 0x9f, 0x5b, 0x09, 0x00,
	0xc3, 0x5e, 0x95, 0x23, 0x9f, 0x13, 0x11, 0x7e, 0xd8, 0x92, 0x3f, 0xbc, 0x90, 0xbb, 0x74, 0x0e,
	0xc3, 0x47, 0x74, 0x3d, 0x90, 0xaa, 0x3f, 0x51, 0xd8, 0xf4, 0x11, 0x84, 0x9f, 0xde, 0x95, 0x1d,
	0xc3, 0xc6, 0x09, 0xd5, 0x9f, 0xfa, 0x66, 0xf9, 0xd8, 0xf0, 0xf7, 0xa0, 0x90, 0xa1, 0xd6, 0xf3,
	0xc3, 0xf3, 0xd6, 0xa1, 0x90, 0xa0, 0xf7, 0xf0, 0xd8, 0xf9, 0x66, 0xfa, 0x9f, 0xd5, 0x09, 0xc6,
	0xc3, 0x1d, 0x95, 0xde, 0x9f, 0x84, 0x11, 0xf4, 0xd8, 0x51, 0x3f, 0xaa, 0x90, 0x3d, 0x74, 0x47,
	0xc3, 0x0e, 0x74, 0xbb, 0x90, 0xbc, 0x3f, 0x92, 0xd8, 0x7e, 0x11, 0x13, 0x9f, 0x23, 0x95, 0x5e,
	0xc3, 0x00, 0x09, 0x5b, 0x9f, 0x62, 0x66, 0xa1, 0xd8, 0x52, 0xf7, 0x67, 0x90, 0xca, 0xd6, 0x4a,
}

// testQMC1 按 qmc1 的规则就地异或整份文件 b：第 i 字节取表的 (i > 0x7fff ? i % 0x7fff : i) & 0x7f 项。
func testQMC1(b []byte) {
	for i := range b {
		k := i
		if k > 0x7fff {
			k %= 0x7fff
		}
		b[i] ^= testQMC1Key[k&0x7f]
	}
}

// qrcFileFromZlib 把 zlib 数据 z 造成 QQ 缓存目录里的 qrc 文件：补零到 8 的整数倍、QRC 3DES 加密、
// 前加「[offset:0]\n」，再整份按 qmc1 异或（设计 §2.3 解法的逆过程）。
func qrcFileFromZlib(z []byte) []byte {
	z = append(bytes.Clone(z), make([]byte, (8-len(z)%8)%8)...)
	schedule := tripleDesKeySetup(qrc3DESKey, desEncrypt)
	out := []byte("[offset:0]\n")
	for i := 0; i < len(z); i += 8 {
		out = append(out, tripleDesCrypt(z[i:i+8], schedule)...)
	}
	testQMC1(out)
	return out
}

// zlibOf 返回 plain 的 zlib 压缩。
func zlibOf(t testing.TB, plain []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return z.Bytes()
}

// qrcFile 把明文 plain 造成 qrc 文件。
func qrcFile(t testing.TB, plain string) []byte {
	t.Helper()
	return qrcFileFromZlib(zlibOf(t, []byte(plain)))
}

// zeroQRCFile 造一个明文为 n 个零字节的 qrc 文件，分块压缩，不在内存里放整份明文。
func zeroQRCFile(t testing.TB, n int) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	chunk := make([]byte, 1<<20)
	for n > 0 {
		c := chunk[:min(n, len(chunk))]
		if _, err := zw.Write(c); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		n -= len(c)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return qrcFileFromZlib(z.Bytes())
}

// qrcXML 把 QRC 正文包成 QQ 缓存的 qrc 明文与 GetPlayLyricInfo 解密后的形状（设计 §2.3）。
func qrcXML(content string) string {
	return "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<QrcInfos>\n<QrcHeadInfo SaveTime=\"0\" Version=\"100\"/>\n" +
		"<LyricInfo LyricCount=\"1\">\n<Lyric_1 LyricType=\"1\" LyricContent=\"" + content + "\"/>\n</LyricInfo>\n</QrcInfos>\n"
}

// writeLyricFile 把 b 写进 dir 下的 name，返回完整路径。
func writeLyricFile(t testing.TB, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
