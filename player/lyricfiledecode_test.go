package player

// 本文件只测 DecodeLyricFileText：歌词文件的字节按 BOM、UTF-8 合法性与 GB18030 解码为文本。

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

const decodeSample = "[ti:测试标题]\n[00:01.00]第一行歌词\n[00:05.50]Hello 世界\n"

func mustDecode(t *testing.T, name string, b []byte) string {
	t.Helper()
	s, err := DecodeLyricFileText(b)
	if err != nil {
		t.Fatalf("%s：DecodeLyricFileText: %v", name, err)
	}
	return s
}

func TestDecodeLyricFileTextEncodings(t *testing.T) {
	encode := func(enc interface{ String(string) (string, error) }) []byte {
		s, err := enc.String(decodeSample)
		if err != nil {
			t.Fatalf("编码夹具: %v", err)
		}
		return []byte(s)
	}
	le := encode(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder())
	be := encode(unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder())
	gbk := encode(simplifiedchinese.GBK.NewEncoder())
	if !bytes.HasPrefix(le, []byte{0xFF, 0xFE}) || !bytes.HasPrefix(be, []byte{0xFE, 0xFF}) {
		t.Fatal("夹具：UTF-16 编码未带 BOM")
	}
	if utf8.Valid(gbk) {
		t.Fatal("夹具：GBK 编码恰为合法 UTF-8，测不到 GB18030 一支")
	}
	for name, b := range map[string][]byte{
		"UTF-8":        []byte(decodeSample),
		"UTF-8 BOM":    append([]byte("\xEF\xBB\xBF"), decodeSample...),
		"UTF-16LE BOM": le,
		"UTF-16BE BOM": be,
		"GBK":          gbk,
	} {
		if got := mustDecode(t, name, b); got != decodeSample {
			t.Fatalf("%s：得 %q，want %q", name, got, decodeSample)
		}
	}
	// CRLF 原样保留，不改换行。
	crlf := strings.ReplaceAll(decodeSample, "\n", "\r\n")
	if got := mustDecode(t, "UTF-8 CRLF", []byte(crlf)); got != crlf {
		t.Fatalf("UTF-8 CRLF：得 %q，want %q", got, crlf)
	}

	// 无 BOM 的 UTF-16 不猜编码：解出的不是原文。
	for name, enc := range map[string]interface{ String(string) (string, error) }{
		"无 BOM 的 UTF-16LE": unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder(),
		"无 BOM 的 UTF-16BE": unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewEncoder(),
	} {
		if got := mustDecode(t, name, encode(enc)); got == decodeSample {
			t.Fatalf("%s：按 UTF-16 解出了原文，应不猜编码", name)
		}
	}
}

// 空输入与只有 BOM：得空串，不报错。
func TestDecodeLyricFileTextEmpty(t *testing.T) {
	for name, b := range map[string][]byte{
		"nil":             nil,
		"空字节":             {},
		"只有 UTF-8 BOM":    []byte("\xEF\xBB\xBF"),
		"只有 UTF-16LE BOM": {0xFF, 0xFE},
		"只有 UTF-16BE BOM": {0xFE, 0xFF},
	} {
		if got := mustDecode(t, name, b); got != "" {
			t.Fatalf("%s：得 %q，want 空串", name, got)
		}
	}
}
