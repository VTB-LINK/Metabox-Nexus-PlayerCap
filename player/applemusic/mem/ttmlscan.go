package mem

// 本文件是纯函数：从一段内存字节里夹出 iTunes TTML 歌词文档。不判断哪份属于当前歌——TTML 里没有
// 任何歌曲标识，归属由 player 层裁决：文档最近一次出现新副本时「正在播放」存储里是哪首歌，就记在哪首歌
// 名下（见 applemusic 包的 session.noteDocs），选用时再以时长为上限核对（pickTTML）。

import (
	"bytes"
	"strings"
	"unicode/utf16"
)

var (
	// Apple 歌词专属命名空间（UTF-16），唯一锚点，不会误配到别的 XML。
	ttmlAnchor = utf16LE("lyric-ttml-internal")
	ttOpen     = utf16LE("<tt")
	ttClose    = utf16LE("</tt>")
)

const (
	ttmlLookback  = 8 << 10   // 锚点往前找 <tt 的上限
	ttmlLookahead = 512 << 10 // 锚点往后找 </tt> 的上限
)

// utf16LE 把字符串编成 UTF-16LE 字节（用于在内存里搜锚点）。
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return b
}

// ttmlFound 是一段内存里夹出的一份 TTML：全文，与它（<tt 起点）在这段内存里的偏移。
type ttmlFound struct {
	Off  int
	Text string
}

// addDocs 把一块内存（基址 base）里夹出的 TTML 并进 docs：按全文去重（idx 是全文 → docs 下标），
// 每份副本记下进程内的绝对地址。
func addDocs(docs []TTMLDoc, idx map[string]int, base uintptr, data []byte) []TTMLDoc {
	for _, f := range findTTML(data) {
		addr := base + uintptr(f.Off)
		if i, ok := idx[f.Text]; ok {
			docs[i].Count++
			docs[i].Addrs = append(docs[i].Addrs, addr)
		} else {
			idx[f.Text] = len(docs)
			docs = append(docs, TTMLDoc{Text: f.Text, Count: 1, Addrs: []uintptr{addr}})
		}
	}
	return docs
}

// findTTML 返回一段内存里所有完整的 TTML 文档（含 <p 行的；只有头部的小片不要）及各自的偏移。
func findTTML(d []byte) []ttmlFound {
	var out []ttmlFound
	for pos := 0; ; {
		ai := bytes.Index(d[pos:], ttmlAnchor)
		if ai < 0 {
			return out
		}
		a := pos + ai
		lo := a - ttmlLookback
		if lo < 0 {
			lo = 0
		}
		if bk := bytes.LastIndex(d[lo:a], ttOpen); bk >= 0 {
			start := lo + bk
			hi := a + ttmlLookahead
			if hi > len(d) {
				hi = len(d)
			}
			if fw := bytes.Index(d[a:hi], ttClose); fw >= 0 {
				end := a + fw + len(ttClose)
				// UTF-16 必须从偶数偏移起解，否则整段错位成乱码。
				if (a-start)%2 == 0 {
					if doc := decodeUTF16Z(d[start:end]); strings.Contains(doc, "<p ") {
						out = append(out, ttmlFound{Off: start, Text: doc})
					}
				}
				pos = end
				continue
			}
		}
		pos = a + len(ttmlAnchor)
	}
}
