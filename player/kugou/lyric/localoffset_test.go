package lyric

// 本文件只测 ParseLocalWithOffset：在各种本地歌词格式与编码下提取 [offset:]，同时保持返回行与
// ParseLocal 逐字一致，且不受伪标签行误导。

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"golang.org/x/text/encoding/unicode"
)

func TestParseLocalWithOffsetKRCAndLRC(t *testing.T) {
	krcWithOffset := krcRaw(t, []byte("[ti:晴天]\n[offset:1500]\n[1000,2000]<0,500,0>听<500,500,0>说\n"))

	utf8BOMLRC := append([]byte("\xEF\xBB\xBF"), "[offset:-1000]\n[00:05.00]听说白雪公主在逃跑\n"...)

	utf16LEText := "[offset:0]\n[00:10.00]小红帽\n"
	utf16LEStr, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().String(utf16LEText)
	if err != nil {
		t.Fatalf("编码 UTF-16LE 夹具失败: %v", err)
	}
	utf16LELRC := []byte(utf16LEStr)
	if !bytes.HasPrefix(utf16LELRC, []byte{0xFF, 0xFE}) {
		t.Fatal("夹具：UTF-16LE 未带 BOM")
	}

	cases := []struct {
		name       string
		raw        []byte
		wantLines  int
		wantOff    int
		wantHasOff bool
	}{
		{
			name:       "加密 krc1 含正 offset",
			raw:        krcWithOffset,
			wantLines:  1,
			wantOff:    1500,
			wantHasOff: true,
		},
		{
			name:       "UTF-8 BOM LRC 含负 offset",
			raw:        utf8BOMLRC,
			wantLines:  1,
			wantOff:    -1000,
			wantHasOff: true,
		},
		{
			name:       "UTF-16LE LRC 含零 offset",
			raw:        utf16LELRC,
			wantLines:  1,
			wantOff:    0,
			wantHasOff: true,
		},
		{
			name:       "无标签 LRC",
			raw:        []byte("[00:01.00]无偏移行\n"),
			wantLines:  1,
			wantOff:    0,
			wantHasOff: false,
		},
		{
			name:       "伪标签_时间戳后紧跟offset与正文",
			raw:        []byte("[00:12.00][offset:9999]散落的月光"),
			wantLines:  1,
			wantOff:    0,
			wantHasOff: false,
		},
		{
			name:       "伪标签_正文末尾含offset",
			raw:        []byte("[00:12.00]散落的月光[offset:9999]"),
			wantLines:  1,
			wantOff:    0,
			wantHasOff: false,
		},
		{
			name:       "伪标签_时间戳后紧跟offset无正文",
			raw:        []byte("[00:12.00][offset:9999]"),
			wantLines:  0,
			wantOff:    0,
			wantHasOff: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines, off, hasOff, err := ParseLocalWithOffset(c.raw)
			if err != nil {
				t.Fatalf("ParseLocalWithOffset 返回错误: %v", err)
			}
			if len(lines) != c.wantLines {
				t.Fatalf("len(lines) = %d, want %d (%+v)", len(lines), c.wantLines, lines)
			}
			if off != c.wantOff || hasOff != c.wantHasOff {
				t.Fatalf("offset = (%d, %v), want (%d, %v)", off, hasOff, c.wantOff, c.wantHasOff)
			}

			origLines, origErr := ParseLocal(c.raw)
			if origErr != nil {
				t.Fatalf("ParseLocal 返回错误: %v", origErr)
			}
			if !reflect.DeepEqual(lines, origLines) {
				t.Fatalf("ParseLocalWithOffset 返回行与 ParseLocal 不一致：\ngot  = %+v\nwant = %+v", lines, origLines)
			}
		})
	}

	t.Run("超上限文件返回 ErrLyricTooLarge", func(t *testing.T) {
		big := make([]byte, MaxLocalFileBytes+1)
		lines, off, hasOff, err := ParseLocalWithOffset(big)
		if lines != nil || off != 0 || hasOff || !errors.Is(err, ErrLyricTooLarge) {
			t.Fatalf("得 (%v, %d, %v, %v), want (nil, 0, false, ErrLyricTooLarge)", lines, off, hasOff, err)
		}
	})
}
