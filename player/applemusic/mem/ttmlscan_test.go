package mem

// 本文件只测从内存字节里夹出 TTML 文档（findTTML / addDocs）：UTF-16 解码、只要含 <p 行的完整文档、
// 多份都返回，每份副本的位置各自记对。

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

const sampleTTML = `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="206.744"><div><p begin="1.0" end="2.0" itunes:key="L1">今天是个美妙的夜晚 🎶</p></div></body></tt>`

// 两份文档夹在垃圾里都要夹出来，内容逐字还原（含代理对），各自的起始偏移是 <tt 的位置；只有头部、
// 没有 <p 的不要。
//
// 变异自证：去掉 findTTML 里 strings.Contains(doc, "<p ") 的过滤，headerOnly 那份被收进来、长度变 3 而红；
// 把返回的偏移由 start（<tt 的位置）换成 a（锚点的位置），两份的偏移都对不上而红。
func TestFindTTML(t *testing.T) {
	headerOnly := `<tt xmlns:itunes="http://music.apple.com/lyric-ttml-internal"><head></head></tt>`
	var mem []byte
	mem = append(mem, bytes.Repeat([]byte{0xAB}, 101)...) // 奇数长度垃圾：文档起点落在奇地址也要能解
	first := len(mem)
	mem = append(mem, utf16LE(sampleTTML)...)
	mem = append(mem, 0, 0, 0xCD)
	mem = append(mem, utf16LE(headerOnly)...)
	second := len(mem)
	mem = append(mem, utf16LE(sampleTTML)...)
	got := findTTML(mem)
	if len(got) != 2 || got[0].Text != sampleTTML || got[1].Text != sampleTTML {
		t.Fatalf("got %d 份 %q", len(got), got)
	}
	if got[0].Off != first || got[1].Off != second {
		t.Fatalf("两份的起始偏移=%d、%d，want %d、%d", got[0].Off, got[1].Off, first, second)
	}
}

// 一次全扫逐块汇总 TTML：按全文去重，每份副本记下进程内的绝对地址（块基址 + 块内偏移），Count 等于
// 地址数。同一份文档的两份副本在同一块的不同偏移、以及分处两块，地址都各是各的——player 层靠「出现了
// 上一次全扫没有的地址」认出文档被重新取回。
//
// 变异自证：把 addDocs 里的 `base+uintptr(f.Off)` 改成 `uintptr(f.Off)`（丢了块基址），地址对不上而红；
// 把已见过的文档那一支里的 `Addrs = append(...)` 去掉（只数不记），三份副本只记下一个地址而红。
func TestAddDocsAddresses(t *testing.T) {
	other := strings.Replace(sampleTTML, "206.744", "180.250", 1)
	const baseA, baseB uintptr = 0x7ff600010000, 0x7ff6000a0000

	var blockA []byte
	blockA = append(blockA, bytes.Repeat([]byte{0x11}, 64)...)
	a1 := len(blockA)
	blockA = append(blockA, utf16LE(sampleTTML)...)
	blockA = append(blockA, bytes.Repeat([]byte{0x22}, 30)...)
	o1 := len(blockA)
	blockA = append(blockA, utf16LE(other)...)
	blockA = append(blockA, 0, 0)
	a2 := len(blockA)
	blockA = append(blockA, utf16LE(sampleTTML)...)

	var blockB []byte
	blockB = append(blockB, bytes.Repeat([]byte{0x33}, 7)...)
	a3 := len(blockB)
	blockB = append(blockB, utf16LE(sampleTTML)...)

	idx := map[string]int{}
	docs := addDocs(nil, idx, baseA, blockA)
	docs = addDocs(docs, idx, baseB, blockB)
	if len(docs) != 2 || docs[0].Text != sampleTTML || docs[1].Text != other {
		t.Fatalf("应按全文去重成两份、顺序为首次出现的顺序，got %d 份", len(docs))
	}
	wantA := []uintptr{baseA + uintptr(a1), baseA + uintptr(a2), baseB + uintptr(a3)}
	if !slices.Equal(docs[0].Addrs, wantA) {
		t.Fatalf("三份副本的地址=%#x，want %#x", docs[0].Addrs, wantA)
	}
	if want := []uintptr{baseA + uintptr(o1)}; !slices.Equal(docs[1].Addrs, want) {
		t.Fatalf("另一份文档的地址=%#x，want %#x", docs[1].Addrs, want)
	}
	for _, d := range docs {
		if d.Count != len(d.Addrs) {
			t.Fatalf("Count=%d 应等于地址数 %d", d.Count, len(d.Addrs))
		}
	}
}

// 有锚点却找不到 </tt>（文档被截断在区尾）时不返回半份，也不 panic。
func TestFindTTMLTruncated(t *testing.T) {
	b := utf16LE(sampleTTML)
	for n := 0; n < len(b); n += 7 {
		if got := findTTML(b[:n]); len(got) != 0 {
			t.Fatalf("截到 %d 字节仍返回 %q", n, got)
		}
	}
}
