package applemusic

// 本文件只测 pickTTML：一次全扫的多份 TTML 里挑属于当前歌的那份（时长上限 + 归属规则）。

import (
	"fmt"
	"testing"

	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

func ttmlDoc(dur float64, firstLine string) string {
	return fmt.Sprintf(`<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="%.3f"><div><p begin="1.0" end="2.0" itunes:key="L1">%s</p><p begin="3.0" end="4.0" itunes:key="L2">第二行</p></div></body></tt>`, dur, firstLine)
}

// 归属规则。审查 P1-2 / 真机复测第 3 条：换歌后上一首的 TTML 残留、副本数还可能更多，旧实现按副本数挑，
// 5 次换歌 4 次配错。
//
// 变异自证：把 pickTTML 的 default 分支（记在别的歌名下的不用）改成放行，「上一首的排除」一例选中 A 的
// 歌词而红；把来历不明分支的 `!strict` 去掉，「来历不明的要时长严格对得上」一例选中而红；把
// `!allowPreAttach` 去掉，「附加前就在的只给第一首」一例选中而红。
func TestPickTTMLOwnership(t *testing.T) {
	docA := ttmlDoc(200.5, "A 的歌词")
	docB := ttmlDoc(200.3, "B 的歌词")
	keyA, keyB := metaKey("A", "x"), metaKey("B", "x")

	t.Run("上一首的排除，即使副本更多", func(t *testing.T) {
		owner := map[uint64]string{hashDoc(docA): keyA, hashDoc(docB): keyB}
		docs := []mem.TTMLDoc{{Text: docA, Count: 5}, {Text: docB, Count: 1}}
		lines, h, ok := pickTTML(docs, 200, owner, keyB, false)
		if !ok || h != hashDoc(docB) || lines[0].Text != "B 的歌词" {
			t.Fatalf("应选 B 的歌词，got ok=%v %v", ok, lines)
		}
		if _, _, ok := pickTTML(docs[:1], 200, owner, keyB, false); ok {
			t.Fatal("只剩上一首的 TTML 时应判无歌词")
		}
	})
	t.Run("重播同一首沿用记在它名下的", func(t *testing.T) {
		owner := map[uint64]string{hashDoc(docA): keyA}
		if _, h, ok := pickTTML([]mem.TTMLDoc{{Text: docA, Count: 1}}, 200, owner, keyA, false); !ok || h != hashDoc(docA) {
			t.Fatal("A 重播时应沿用记在 A 名下的 TTML")
		}
	})
	t.Run("记在当前歌名下的不要求时长不短于音频", func(t *testing.T) {
		short := ttmlDoc(231.752, "尾奏很长的歌")
		owner := map[uint64]string{hashDoc(short): keyB}
		if _, _, ok := pickTTML([]mem.TTMLDoc{{Text: short, Count: 3}}, 245, owner, keyB, false); !ok {
			t.Fatal("<body dur> 231.752 对音频 245：记在这首歌名下就该用")
		}
	})
	t.Run("来历不明的要时长严格对得上", func(t *testing.T) {
		short, exact := ttmlDoc(231.752, "短的"), ttmlDoc(245.4, "对得上的")
		for _, o := range []map[uint64]string{{}, {hashDoc(short): "", hashDoc(exact): ""}} {
			if _, _, ok := pickTTML([]mem.TTMLDoc{{Text: short, Count: 3}}, 245, o, keyB, true); ok {
				t.Fatal("来历不明、时长又比音频短的文档不应采用")
			}
			if _, h, ok := pickTTML([]mem.TTMLDoc{{Text: short, Count: 3}, {Text: exact, Count: 1}}, 245, o, keyB, true); !ok || h != hashDoc(exact) {
				t.Fatal("来历不明但时长落在 [245, 251] 的文档应采用")
			}
		}
	})
	t.Run("附加前就在的只给第一首", func(t *testing.T) {
		owner := map[uint64]string{hashDoc(docA): preAttachOwner}
		docs := []mem.TTMLDoc{{Text: docA, Count: 1}}
		if _, _, ok := pickTTML(docs, 200, owner, keyB, true); !ok {
			t.Fatal("附加时那首（allowPreAttach）时长对得上应采用")
		}
		if _, _, ok := pickTTML(docs, 200, owner, keyB, false); ok {
			t.Fatal("第二首起不应采用附加前就在内存里的 TTML")
		}
	})
}

// 与归属无关的两道筛：<body dur> 最多比音频长 lyricDurSlack；最后一行的起点不晚于音频结束。
//
// 变异自证：把时长上限 `diff <= lyricDurSlack` 去掉，206.5 一例被采用而红；去掉「最后一行起点」的筛，
// 「歌词比音频还长」一例被采用而红。
func TestPickTTMLPlausibility(t *testing.T) {
	key := metaKey("B", "x")
	cases := []struct {
		docDur float64
		ok     bool
	}{
		{200.0, true}, {203.77, true}, {206.0, true}, {206.5, false}, {180.0, true}, {130.82, true},
	}
	for _, c := range cases {
		doc := ttmlDoc(c.docDur, "x")
		_, _, ok := pickTTML([]mem.TTMLDoc{{Text: doc, Count: 1}}, 200, map[uint64]string{hashDoc(doc): key}, key, false)
		if ok != c.ok {
			t.Errorf("TTML 时长 %.2f 对音频 200（记在当前歌名下）：ok=%v, want %v", c.docDur, ok, c.ok)
		}
	}
	// 文档的行（1s、3s）排到了音频（1.5s）结束之后：不是这首歌的。
	doc := ttmlDoc(2.5, "x")
	if _, _, ok := pickTTML([]mem.TTMLDoc{{Text: doc, Count: 1}}, 1.5, map[uint64]string{hashDoc(doc): key}, key, false); ok {
		t.Error("歌词行的起点晚于音频结束的文档不应采用")
	}
}

// 多份都够格时的先后：记在当前歌名下的、时长落在严格窗口内的、与音频时长差得少的、副本多的优先；
// 结果与输入顺序无关。
//
// 变异自证：把排序里 `a.off != b.off` 那一档删掉，「差得少的优先」一例随副本数选中差得多的而红。
func TestPickTTMLOrder(t *testing.T) {
	key := metaKey("B", "x")
	near, far := ttmlDoc(200.2, "差 0.2"), ttmlDoc(204.0, "差 4")
	owner := map[uint64]string{hashDoc(near): key, hashDoc(far): key}
	docs := []mem.TTMLDoc{{Text: far, Count: 5}, {Text: near, Count: 1}}
	if _, h, ok := pickTTML(docs, 200, owner, key, false); !ok || h != hashDoc(near) {
		t.Fatal("差得少的优先")
	}
	d1, d2 := ttmlDoc(200.4, "一"), ttmlDoc(200.4, "二")
	same := map[uint64]string{hashDoc(d1): key, hashDoc(d2): key}
	_, first, _ := pickTTML([]mem.TTMLDoc{{Text: d1, Count: 2}, {Text: d2, Count: 2}}, 200, same, key, false)
	for i := 0; i < 20; i++ {
		if _, h, _ := pickTTML([]mem.TTMLDoc{{Text: d2, Count: 2}, {Text: d1, Count: 2}}, 200, same, key, false); h != first {
			t.Fatal("输入顺序不同时选中了不同的文档")
		}
	}
	empty := `<tt><body dur="200.5"><div></div></body></tt>`
	if _, _, ok := pickTTML([]mem.TTMLDoc{{Text: empty, Count: 9}}, 200, map[uint64]string{hashDoc(empty): key}, key, false); ok {
		t.Fatal("0 行的文档不应被采用")
	}
}
