package lyric

// 本文件只测播放时间结构的事后复核（matchesTimePattern）：结构开头的字节是不是已知的两种排版参数之一。

import (
	"encoding/binary"
	"math"
	"testing"
)

// timeHead 拼出一个播放时间结构的开头 16 字节：+0x00 时间、+0x04 不固定、+0x08 字号、+0x0C 行高。
func timeHead(t float32, unk, font, lineHeight uint32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint32(b[0:], math.Float32bits(t))
	binary.LittleEndian.PutUint32(b[4:], unk)
	binary.LittleEndian.PutUint32(b[8:], font)
	binary.LittleEndian.PutUint32(b[12:], lineHeight)
	return b
}

// 落选的候选会被带到之后的歌里去盯（wesing.go 的 cachedAltAddrs），改用之前要确认那块内存此刻仍是一个
// 播放时间结构：读数合理、会走的 float 到处都是，排版参数才是这个结构的特征。时间与 +0x04 是通配的。
//
// 变异自证：把 matchesTimePattern 里逐字节比对的 `ok = false` 删掉（什么都算符合），「别的内容」
// 「字号对、行高不对」两例被认成结构而红；把模式列表里的 timePatternDouble 去掉，「双行排版」一例
// 不被认可而红。
func TestMatchesTimePattern(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"单行排版(30/45)", timeHead(239.79, 0, 0x1E, 0x2D), true},
		{"双行排版(28/42)", timeHead(49.8, 4, 0x1C, 0x2A), true},
		{"时间为 0 也算", timeHead(0, 0, 0x1E, 0x2D), true},
		{"别的内容", timeHead(12.5, 0, 0x00503A10, 0x00503A40), false},
		{"字号对、行高不对", timeHead(12.5, 0, 0x1E, 0x2A), false},
		{"读到的不够长", timeHead(12.5, 0, 0x1E, 0x2D)[:12], false},
	}
	for _, c := range cases {
		if got := matchesTimePattern(c.head); got != c.want {
			t.Errorf("%s：got %v，want %v", c.name, got, c.want)
		}
	}
}
