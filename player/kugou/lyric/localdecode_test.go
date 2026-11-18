package lyric

// 本文件只测本地歌词文件按内容判读格式与文本编码（ParseLocal）：krc1 原始字节、明文 KRC、
// 各种编码的 LRC，以及解不出行的输入。上限另见 locallimits_test.go。

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

const twoLineKRC = "[ti:无名指]\n[ar:门尼]\n" +
	"[1000,2000]<0,500,0>听<500,500,0>说<1000,1000,0>你\n" +
	"[3000,1500]<0,700,0>好<700,800,0>吗\n"

func mustParseLocal(t *testing.T, b []byte) []Line {
	t.Helper()
	lines, err := ParseLocal(b)
	if err != nil {
		t.Fatalf("ParseLocal: %v", err)
	}
	return lines
}

func TestParseLocalKRCRaw(t *testing.T) {
	lines := mustParseLocal(t, krcRaw(t, []byte(twoLineKRC)))
	if len(lines) != 2 {
		t.Fatalf("得 %d 行，want 2", len(lines))
	}
	want := []struct {
		time  float32
		text  string
		words []float32
	}{
		{1.0, "听说你", []float32{1.0, 1.5, 2.0}},
		{3.0, "好吗", []float32{3.0, 3.7}},
	}
	for i, w := range want {
		l := lines[i]
		if l.Index != i || l.Time != w.time || l.Text != w.text {
			t.Fatalf("第 %d 行 = {%d %v %q}，want {%d %v %q}", i, l.Index, l.Time, l.Text, i, w.time, w.text)
		}
		if len(l.Detailed.Words) != len(w.words) {
			t.Fatalf("第 %d 行逐字 %d 个，want %d", i, len(l.Detailed.Words), len(w.words))
		}
		for j, ts := range w.words {
			if got := l.Detailed.Words[j].Timestamp; got != ts {
				t.Fatalf("第 %d 行第 %d 字起始 %v，want %v", i, j, got, ts)
			}
		}
	}
}

// 同一份明文不加密直接给（含带 UTF-8 BOM、CRLF 换行的写法），结果与 krc1 原始字节相同。
func TestParseLocalPlainKRC(t *testing.T) {
	want := mustParseLocal(t, krcRaw(t, []byte(twoLineKRC)))
	for name, b := range map[string][]byte{
		"UTF-8":     []byte(twoLineKRC),
		"UTF-8 BOM": append([]byte("\xEF\xBB\xBF"), twoLineKRC...),
		"CRLF":      []byte(strings.ReplaceAll(twoLineKRC, "\n", "\r\n")),
	} {
		if got := mustParseLocal(t, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s：得 %+v，want %+v", name, got, want)
		}
	}
}

const sampleLRC = "[ti:童话镇]\n[ar:暗杠]\n" +
	"[00:01.00]听说白雪公主在逃跑\n" +
	"[00:05.50][00:20.00]小红帽在担心大灰狼\n" +
	"[00:10.12]Hello 世界\n"

func TestParseLocalLRCEncodings(t *testing.T) {
	want := mustParseLocal(t, []byte(sampleLRC))
	wantLines := []struct {
		time float32
		text string
	}{
		{1, "听说白雪公主在逃跑"},
		{5.5, "小红帽在担心大灰狼"},
		{10.12, "Hello 世界"},
		{20, "小红帽在担心大灰狼"},
	}
	if len(want) != len(wantLines) {
		t.Fatalf("UTF-8 无 BOM 得 %d 行，want %d", len(want), len(wantLines))
	}
	for i, w := range wantLines {
		if want[i].Index != i || want[i].Time != w.time || want[i].Text != w.text {
			t.Fatalf("第 %d 行 = {%d %v %q}，want {%d %v %q}", i, want[i].Index, want[i].Time, want[i].Text, i, w.time, w.text)
		}
	}

	encode := func(enc interface{ String(string) (string, error) }) []byte {
		s, err := enc.String(sampleLRC)
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
		"UTF-8 BOM":    append([]byte("\xEF\xBB\xBF"), sampleLRC...),
		"UTF-8 CRLF":   []byte(strings.ReplaceAll(sampleLRC, "\n", "\r\n")),
		"UTF-16LE BOM": le,
		"UTF-16BE BOM": be,
		"GBK":          gbk,
	} {
		if got := mustParseLocal(t, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s：得 %+v，want %+v", name, got, want)
		}
	}

	// 无 BOM 的 UTF-16 不猜编码：按 UTF-8 或 GB18030 解出的文本里没有合法时间戳。
	noBOM := encode(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder())
	if got := mustParseLocal(t, noBOM); got != nil {
		t.Fatalf("无 BOM 的 UTF-16LE：得 %d 行，want nil", len(got))
	}
}

func TestParseLocalLRCThreeDigitMillis(t *testing.T) {
	lines := mustParseLocal(t, []byte("[00:00.000]开始\n[00:14.870]听说\n"))
	if len(lines) != 2 || lines[0].Time != 0 || lines[1].Time != 14.87 || lines[1].Text != "听说" {
		t.Fatalf("得 %+v，want [0 开始] [14.87 听说]", lines)
	}
}

// 解不出行的输入一律 (nil, nil)：调用方据此判不可用，不报错。
func TestParseLocalEmptyAndMetaOnly(t *testing.T) {
	random := make([]byte, 64<<10)
	r := rand.New(rand.NewPCG(205, 3))
	for i := range random {
		random[i] = byte(r.Uint32())
	}
	if bytes.HasPrefix(random, []byte("krc1")) || bytes.HasPrefix(random, []byte{0xFF, 0xFE}) ||
		bytes.HasPrefix(random, []byte{0xFE, 0xFF}) || bytes.HasPrefix(random, []byte("\xEF\xBB\xBF")) {
		t.Fatal("夹具：随机字节恰以魔数或 BOM 开头")
	}
	cases := map[string][]byte{
		"nil":         nil,
		"空字节":         {},
		"只有空白":        []byte(" \r\n\t\n"),
		"只有 BOM":      []byte("\xEF\xBB\xBF"),
		"只有元数据":       []byte("[ti:童话镇]\n[ar:暗杠]\n[al:]\n[by:]\n[offset:0]\n"),
		"时间戳后无文本":     []byte("[00:01.00]\n[00:02.00]   \n"),
		"无时间戳的文本":     []byte("听说白雪公主在逃跑\n"),
		"KRC 行无逐字标记":  []byte("[1000,2000]听说\n"),
		"随机字节":        random,
		"krc1 后为乱码":   append([]byte("krc1"), random...),
		"krc1 解出无歌词行": krcRaw(t, []byte("[ti:t]\n[ar:a]\n")),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			lines, err := ParseLocal(b)
			if lines != nil || err != nil {
				t.Fatalf("得 (%d 行, %v)，want (nil, nil)", len(lines), err)
			}
		})
	}
}
