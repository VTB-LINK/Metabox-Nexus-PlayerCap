package lyric

// 本文件只放本地歌词测试的共用夹具：由明文生成 krc1 原始字节、拼 KRC 的 [language:] 标签、
// 生成多行与多时间戳的歌词。本身不含测试。

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// testKRCKey 是酷狗 KRC 的异或密钥。与出货代码的 krcXORKey 各写一份：密钥被误改时夹具与出货代码
// 不再互逆，往返用例随之失败。
var testKRCKey = [16]byte{0x40, 0x47, 0x61, 0x77, 0x5E, 0x32, 0x74, 0x47, 0x51, 0x36, 0x31, 0x2D, 0xCE, 0xD2, 0x6E, 0x69}

// krcRaw 按酷狗歌词目录里 .krc 的格式加密明文：zlib 压缩、逐字节异或密钥、前加 "krc1"。
func krcRaw(t testing.TB, plain []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	out := append([]byte("krc1"), z.Bytes()...)
	for i := range out[4:] {
		out[4+i] ^= testKRCKey[i%16]
	}
	return out
}

// langBlock 是 [language:] 标签里的一条轨：Type 1 为翻译，0 为音译；LyricContent[i] 是第 i 个
// [start,dur] 行的片段。
type langBlock struct {
	LyricContent [][]string `json:"lyricContent"`
	Type         int        `json:"type"`
}

// languageTag 拼一条 KRC 头部的 [language:] 标签，只含一个 type 为 typ 的轨。
func languageTag(t testing.TB, typ int, content [][]string) string {
	t.Helper()
	return languageTags(t, langBlock{LyricContent: content, Type: typ})
}

// languageTags 拼一条含 blocks 各轨的 [language:] 标签。
func languageTags(t testing.TB, blocks ...langBlock) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Content []langBlock `json:"content"`
		Version int         `json:"version"`
	}{Content: blocks, Version: 1})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return "[language:" + base64.StdEncoding.EncodeToString(b) + "]\n"
}

// lrcTimestamp 返回第 i 个时间戳 [mm:ss.xx]，时刻为 i×10 ms，随 i 严格递增。
func lrcTimestamp(i int) string {
	return fmt.Sprintf("[%02d:%02d.%02d]", i/6000, i/100%60, i%100)
}

// lrcLines 生成 n 行 LRC，每行一个时间戳、文本为 "x"。
func lrcLines(n int) []byte {
	var b strings.Builder
	for i := range n {
		b.WriteString(lrcTimestamp(i))
		b.WriteString("x\n")
	}
	return []byte(b.String())
}

// lrcOneLine 生成一行 LRC：n 个时间戳后跟 text。
func lrcOneLine(n int, text string) []byte {
	var b strings.Builder
	for i := range n {
		b.WriteString(lrcTimestamp(i))
	}
	b.WriteString(text)
	b.WriteString("\n")
	return []byte(b.String())
}

// krcLines 生成 n 行 KRC 明文，每行一个字，字的文本为 word。
func krcLines(n int, word string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "[%d,10]<0,10,0>%s\n", i*10, word)
	}
	return b.String()
}
