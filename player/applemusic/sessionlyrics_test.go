package applemusic

// 本文件只测会话里的歌词归属与补发：歌词晚到要补发 all_lyrics（审查 P1-3），上一首残留的 TTML
// 不得配给新歌（审查 P1-2 / 真机复测第 3 条），歌词文档按「最近一次出现新副本时放的是哪首歌」归属，
// 记在歌名下的文档离开内存后仍可用。

import (
	"fmt"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 歌词比标题晚到：先发无歌词的 all_lyrics（带时长），歌词出现后补发一次带歌词的；宽限期内不发
// 「无歌词」（index:-1）。
//
// 变异自证：删掉 applyLyrics 里 `if s.announced { s.emitAllLyrics() ... }`，第二次 all_lyrics 不出现而红。
func TestSessionLateLyricsReemitted(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("晚到")
	h.setTwin(200, 50, true)
	h.run(5 * time.Second)
	al := allLyrics(h.evs)
	if len(al) != 1 || al[0].Count != 0 || al[0].Duration != 200 {
		t.Fatalf("先应有一条无歌词、时长 200 的 all_lyrics，got %+v", al)
	}

	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.4, "终于来了"), Count: 1}}
	from := h.mark()
	h.run(3 * time.Second) // 宽限期内约每 1.2s 找一次
	al = allLyrics(h.since(from))
	if len(al) != 1 || al[0].Count != 2 || al[0].Lyrics[0].Text != "终于来了" || al[0].Duration != 200 {
		t.Fatalf("歌词出现后应补发一次带歌词的 all_lyrics，got %+v", al)
	}
	if v := verdicts(h.evs); len(v) != 0 {
		t.Fatalf("歌词在宽限期内到了，不应发过 index:-1，got %d 条", len(v))
	}
}

// 没有歌词的歌：宣告时只发空的 all_lyrics；「无歌词」（index:-1）要等满 lyricGrace 才发、只发一次。
// 之后歌词若到了照常补发（真机见过 10 秒以上才到的）。
//
// 变异自证：把 announce 里的 emitVerdict 改成无条件调用（宣告时就判），第一段出现 index:-1 而红；
// 删掉 tick 里对 checkVerdict 的调用，第二段没有 index:-1 而红。
func TestSessionNoLyricsVerdictAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("无词")
	h.setTwin(300, 0, true)
	h.s.twin = realTwin // 直接从已确认的位置结构起步：本例量的是从开播起算的宽限期
	h.run(lyricGrace - time.Second)
	if al := allLyrics(h.evs); len(al) != 1 || al[0].Count != 0 || al[0].Duration != 300 {
		t.Fatalf("宣告时应发一条空的 all_lyrics，got %+v", al)
	}
	if v := verdicts(h.evs); len(v) != 0 {
		t.Fatalf("宽限期内不应发 index:-1，got %d 条", len(v))
	}

	h.run(2 * time.Second)
	v := verdicts(h.evs)
	if len(v) != 1 {
		t.Fatalf("宽限期满应发一次 index:-1，got %d 条", len(v))
	}
	if v[0].Position < 14 || v[0].Progress <= 0 {
		t.Fatalf("index:-1 应带当时的位置与进度，got pos=%v progress=%v", v[0].Position, v[0].Progress)
	}

	from := h.mark()
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(300.2, "很晚才到"), Count: 1}}
	h.run(12 * time.Second)
	if al := allLyrics(h.since(from)); len(al) != 1 || al[0].Count == 0 {
		t.Fatalf("判了无歌词之后歌词才到，也应补发 all_lyrics，got %+v", al)
	}
	if v := verdicts(h.evs); len(v) != 1 {
		t.Fatalf("index:-1 每首歌至多一次，got %d 条", len(v))
	}
}

// <body dur> 比音频时长短得多的歌词文档（尾奏长的歌）：在这首歌期间第一次露面，就是它的歌词。
// 真机 2026-10-08：《焰火青年》音频 245s、歌词文档 231.752s；另一首 159s 对 130.820s。旧实现要求
// 文档时长不短于音频时长，这两首整首被当成没有歌词。
//
// 变异自证：把 pickTTML 里对「记在当前歌名下」的文档也加上 `diff >= 0` 的要求，本例无歌词而红。
func TestSessionShortBodyDurLyricsAccepted(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("上一首")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("焰火青年")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(231.752, "你推开了门 挥舞着青春"), Count: 3}}
	h.run(1300 * time.Millisecond)
	h.setTwin(245, 0, true)
	h.run(3 * time.Second)
	al := allLyrics(h.since(from))
	if len(al) != 1 || al[0].Count == 0 || al[0].Lyrics[0].Text != "你推开了门 挥舞着青春" || al[0].Duration != 245 {
		t.Fatalf("应配上这首歌期间出现的歌词文档（时长 231.752 对音频 245），got %+v", al)
	}
}

// 换到一首同时长的歌，上一首的 TTML 还在且副本更多：新歌先按无歌词，自己的 TTML 出现后再配上。
//
// 变异自证：把 pickTTML 里「记在别的歌名下的不用」那个 default 分支改成放行，新歌第一条 all_lyrics 就
// 配上「A 的歌词」而红。
func TestSessionPreviousSongLyricsNotReused(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.5, "A 的歌词"), Count: 1}}
	h.setTwin(200, 30, true)
	h.run(6 * time.Second)
	if al := allLyrics(h.evs); len(al) == 0 || al[len(al)-1].Count == 0 {
		t.Fatalf("A 应配上自己的歌词，got %+v", al)
	}

	// 换歌：时长先归零，再写入同样的 200；A 的 TTML 残留且副本更多。
	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("B")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.5, "A 的歌词"), Count: 5}}
	h.run(1300 * time.Millisecond)
	h.setTwin(200, 0, true)
	h.run(5 * time.Second)
	for _, al := range allLyrics(h.since(from)) {
		if al.Count != 0 {
			t.Fatalf("B 不应配上 A 残留的歌词，got %+v", al.Lyrics)
		}
	}

	from = h.mark()
	h.f.docs = append(h.f.docs, mem.TTMLDoc{Text: ttmlDoc(200.2, "B 的歌词"), Count: 1})
	h.run(3 * time.Second)
	al := allLyrics(h.since(from))
	if len(al) != 1 || al[0].Count == 0 || al[0].Lyrics[0].Text != "B 的歌词" {
		t.Fatalf("B 的 TTML 出现后应配上它，got %+v", al)
	}
}

// 换歌时歌词已在内存里：只发一条 all_lyrics，且带歌词——不能先发一条空的再补（overlay 会闪「无歌词」，
// 2026-10-08 端到端实测 4 次换歌都出现过）。
//
// 变异自证：把 applySnapshot 里的 announce 挪到配歌词（pickTTML）之前，第一条 all_lyrics 是空的、共两条而红。
func TestSessionAnnounceWithLyricsOnce(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("一")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("二")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(260.5, "二的歌词"), Count: 1}}
	h.run(1300 * time.Millisecond)
	h.setTwin(260, 0, true)
	h.run(5 * time.Second)
	al := allLyrics(h.since(from))
	if len(al) != 1 || al[0].Count == 0 {
		t.Fatalf("应只发一条带歌词的 all_lyrics，got %d 条", len(al))
	}
}

// 连切：切到 M，M 的歌词文档到了，没等它开播又切到 N；N 是纯音乐、时长又比 M 的文档长。M 的文档是在
// M 的名下第一次露面的，不能给 N（旧会话把这条记为「无法解决」：当时拿不到文档出现的时刻）。之后真去
// 放 M，M 拿得到自己的歌词——它没有被永久作废。
//
// 变异自证：把 noteDocs 里改记归属时写入的 `metaKey(np.Title, np.Artist)` 改成 ""（都算来历不明），
// 「M 之后真放时有歌词」一段无歌词而红；改成当前曲目的 key（`s.song.key()`），M 的歌词记到了
// 别的歌名下、它真放时同样无歌词而红。
func TestSessionSkippedSongLyricsStayWithIt(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("M")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(180.3, "M 的歌词"), Count: 3}}
	h.run(1500 * time.Millisecond) // 停在 M 上一秒半：够把它的文档记到它名下
	h.f.songs = np("N")            // 没等 M 开播又切走
	h.run(1300 * time.Millisecond)
	h.setTwin(250, 0, true) // N 开播
	h.run(6 * time.Second)
	evs := h.since(from)
	if si := songInfos(evs); len(si) != 1 || si[0].Name != "N" {
		t.Fatalf("应只宣告 N，got %d 条", len(si))
	}
	for _, al := range allLyrics(evs) {
		if al.Count != 0 {
			t.Fatalf("N 不应配上 M 的歌词，got %q", al.Lyrics[0].Text)
		}
	}

	from = h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("M")
	h.run(1300 * time.Millisecond)
	h.setTwin(190, 0, true) // 这回真的放 M（音频 190s，文档 180.3s）
	h.run(3 * time.Second)
	al := allLyrics(h.since(from))
	if len(al) != 1 || al[0].Count == 0 || al[0].Lyrics[0].Text != "M 的歌词" {
		t.Fatalf("M 真放的时候应配上它自己的歌词，got %+v", al)
	}
}

// 换歌后第一次扫描还读到上一首的标题（旧存储没来得及换），新歌的 TTML 被记到了旧标题名下；随后读到
// 真标题时，这期间记到旧标题名下的文档要转给新歌（复查实测旧实现新歌整首无歌词）。
//
// 变异自证：删掉 applyMeta 里把文档从旧标题转到新标题的循环，B 的 all_lyrics 一直是空的而红。
func TestSessionCorrectedTitleKeepsLyrics(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	h.setTwin(0, 0, false)
	h.run(600 * time.Millisecond)
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(260.5, "B 的歌词"), Count: 1}}
	h.setTwin(260, 0, true) // 时长已是 B 的，标题存储还是 A
	h.run(900 * time.Millisecond)
	from := h.mark()
	h.f.songs = np("B")
	h.run(5 * time.Second)
	al := allLyrics(h.since(from))
	if len(al) == 0 || al[len(al)-1].Count == 0 || al[len(al)-1].Lyrics[0].Text != "B 的歌词" {
		t.Fatalf("标题纠正为 B 后应配上 B 的歌词，got %+v", al)
	}
}

// 附加时就在内存里的 TTML 来历不明：附加时那首（第一首）可以用——但只有时长严格对得上的；之后的歌
// 一律不用——它们属于更早的歌，能在内存里留几分钟、跨好几首（复查实测 7 份留了 7 分钟）。
//
// 变异自证：把 applySnapshot 里传给 pickTTML 的 `s.songsBegun <= 1` 改成 true，第二首（纯音乐）配上旧歌词而红。
func TestSessionPreAttachLyricsNotReused(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("附加时在放")
	h.f.docs = []mem.TTMLDoc{
		{Text: ttmlDoc(200.5, "很早以前那首的歌词"), Count: 1},
		{Text: ttmlDoc(300.4, "附加时那首的歌词"), Count: 1},
	}
	h.setTwin(300, 50, true)
	h.run(6 * time.Second)
	if al := allLyrics(h.evs); len(al) == 0 || al[len(al)-1].Count == 0 || al[len(al)-1].Lyrics[0].Text != "附加时那首的歌词" {
		t.Fatalf("附加时那首应配上内存里已有的、时长对得上的 TTML，got %+v", al)
	}

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("纯音乐")
	h.run(1300 * time.Millisecond)
	h.setTwin(200, 0, true) // 时长恰好落进很早那首 TTML 的窗口
	h.run(10 * time.Second)
	for _, al := range allLyrics(h.since(from)) {
		if al.Count != 0 {
			t.Fatalf("第二首不应配上附加前就在内存里的 TTML，got %v", al.Lyrics)
		}
	}
}

// TTML 只有一行「纯音乐，请欣赏」：按无歌词发（all_lyrics 空、index:-1 带提示，不必等宽限期），之后不得
// 再推一条 index 0 的 lyric_update。
//
// 变异自证：删掉 applyLyrics 里 pure 时清空行的处理，出现 index 0 的 lyric_update 而红。
func TestSessionPureMusicHintNotPushedAsLine(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("纯音乐")
	h.f.docs = []mem.TTMLDoc{{Text: `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="200.500"><div><p begin="0.0" end="200.0" itunes:key="L1">纯音乐，请欣赏</p></div></body></tt>`, Count: 1}}
	h.setTwin(200, 10, true)
	h.run(8 * time.Second)
	al := allLyrics(h.evs)
	if len(al) != 1 || al[0].Count != 0 {
		t.Fatalf("应只发一条空的 all_lyrics，got %+v", al)
	}
	var sawHint bool
	for _, e := range h.evs {
		if lu, ok := e.Data.(*player.LyricUpdate); ok {
			if lu.Index >= 0 {
				t.Fatalf("纯音乐提示不应作为第 %d 行推出", lu.Index)
			}
			sawHint = sawHint || lu.Text == "纯音乐，请欣赏"
		}
	}
	if !sawHint {
		t.Fatal("index:-1 应带上纯音乐提示")
	}
}

// lastLyrics 返回 evs 里最后一条 all_lyrics 的首行文本；没有 all_lyrics 或它是空的返回 ""。
func lastLyrics(evs []player.Event) string {
	al := allLyrics(evs)
	if len(al) == 0 || al[len(al)-1].Count == 0 {
		return ""
	}
	return al[len(al)-1].Lyrics[0].Text
}

// 服务附加前就在内存里的歌词文档，轮到它那首歌时会出现新地址的副本（2026-10-08 真机，歌词面板关闭，
// 每秒采样副本地址：重放《白月光》x1→x3、《爱如潮水》0→x3、《别怕我伤心》x4→x5）。归属跟着新副本走：
// 出现新副本的那次全扫里存储是哪首歌，文档就改记到哪首歌名下——否则附加前就在的文档除第一首外永不
// 采用，《别怕我伤心》等满 15 秒报了无歌词。地址集合没变的不改记，照旧按来历不明处理。
//
// 变异自证：把 noteDocs 里改记归属的条件收回到「上一次全扫没有这份文档」（去掉
// `|| hasNewAddr(d.Addrs, prev)`），前两例无歌词而红；把 hasNewAddr 改成恒真（每次全扫都把文档改记到
// 当时那首歌名下），「地址集合不变」一例配上了而红；去掉 pickTTML 里的时长上限，「出现新副本的是别的歌的
// 文档」一例配上了 C 的歌词而红。
func TestSessionNewCopyReassignsPreAttachDoc(t *testing.T) {
	docA, docB, docC := ttmlDoc(200.4, "A 的歌词"), ttmlDoc(259.8, "B 的歌词"), ttmlDoc(300.3, "C 的歌词")
	// start 附加时正在放 A，内存里已有 A、B、C 三首的文档；A 配上自己那份（时长落在严格窗口内）。
	start := func(t *testing.T) *harness {
		h := newHarness(t)
		h.f.songs = np("A")
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000), docAt(docB, 0xB000), docAt(docC, 0xC000)}
		h.setTwin(200, 50, true)
		h.run(6 * time.Second)
		if got := lastLyrics(h.evs); got != "A 的歌词" {
			t.Fatalf("前置：附加时那首应配上内存里时长对得上的文档，got %q", got)
		}
		return h
	}
	withNewB := []mem.TTMLDoc{docAt(docA, 0xA000), docAt(docB, 0xB000, 0xB800), docAt(docC, 0xC000)}

	t.Run("开播后出现新副本", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		skipTo(h, store("B", "封面:B"), 260)
		h.run(2 * time.Second)
		if got := lastLyrics(h.since(from)); got != "" {
			t.Fatalf("新副本出现之前 B 不应配上附加前就在的文档，got %q", got)
		}
		h.f.docs = withNewB // B 的文档多出一份新地址的副本（<body dur> 比音频短 0.2s）
		h.run(3 * time.Second)
		if got := lastLyrics(h.since(from)); got != "B 的歌词" {
			t.Fatalf("B 的文档出现新副本后应配上，got %q", got)
		}
		if v := verdicts(h.since(from)); len(v) != 0 {
			t.Fatalf("不应发 index:-1，got %d 条", len(v))
		}
	})
	t.Run("加载中出现新副本", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		h.f.docs = withNewB // 新副本与新存储同时出现，早于时长写入（真机手动点播的常态）
		skipTo(h, store("B", "封面:B"), 260)
		h.run(3 * time.Second)
		if al := allLyrics(h.since(from)); len(al) != 1 || al[0].Count == 0 || al[0].Lyrics[0].Text != "B 的歌词" {
			t.Fatalf("新副本在加载中就记到了 B 名下，开播后应只发一条带歌词的 all_lyrics，got %+v", al)
		}
	})
	t.Run("地址集合不变", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		skipTo(h, store("B", "封面:B"), 260)
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if got := lastLyrics(evs); got != "" {
			t.Fatalf("B 的文档没有出现新副本，不应配上，got %q", got)
		}
		if v := verdicts(evs); len(v) != 1 {
			t.Fatalf("宽限期满应发一次 index:-1，got %d 条", len(v))
		}
	})
	t.Run("出现新副本的是别的歌的文档", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000), docAt(docB, 0xB000), docAt(docC, 0xC000, 0xC800)}
		skipTo(h, store("B", "封面:B"), 260)
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if got := lastLyrics(evs); got != "" {
			t.Fatalf("C 的文档比 B 长出 40s，改记到 B 名下也过不了时长上限，不应配上，got %q", got)
		}
		if v := verdicts(evs); len(v) != 1 {
			t.Fatalf("宽限期满应发一次 index:-1，got %d 条", len(v))
		}
	})
}

// 本会话第一次全扫只建基线：内存里已有的文档地址都是「没见过的」，但不能因此记到附加时那首歌名下——
// 它们来历不明，只有时长严格对得上才给第一首用。附加时正在放的歌，文档若比音频短就配不上（已知的缝）。
//
// 变异自证：删掉 noteDocs 里 `!s.scanned` 那一支（第一次全扫也按新副本记到存储里那首歌名下），附加时
// 那首配上了时长比音频短的文档而红。
func TestSessionFirstScanIsBaselineOnly(t *testing.T) {
	short := ttmlDoc(231.752, "附加前就在、比音频短")
	h := newHarness(t)
	h.f.songs = np("附加时在放")
	h.f.docs = []mem.TTMLDoc{docAt(short, 0xA000, 0xA800)}
	h.setTwin(245, 50, true)
	h.run(lyricGrace + 5*time.Second)
	if got := lastLyrics(h.evs); got != "" {
		t.Fatalf("第一次全扫见到的文档来历不明、时长又比音频短，不应配上，got %q", got)
	}
	if o := h.s.docOwner[hashDoc(short)]; o != preAttachOwner {
		t.Fatalf("第一次全扫见到的文档应记为附加前就在，got %q", o)
	}
}

// 歌词会预取（2026-10-08 真机：播放列表起播《winding road》约 7 秒时，它与列表里《紅蓮華》的文档在
// 同一采样里一起出现；轮到《紅蓮華》时它的文档重新出现 5 份）。预取的文档先记在当时那首歌名下，
// 轮到它自己时出现新副本，归属要**覆盖**过去——只在第一次见到时记、具名归属不改主，《紅蓮華》等满
// 15 秒报了无歌词。
//
// 变异自证：把 noteDocs 里改记归属的那一支加上「原归属不是具名的才改」（具名归属不改主），「出现新副本」
// 「整份消失后重新出现」两例无歌词而红；把「上一次全扫没有的地址」换成「从来没见过的地址」（docAddrs
// 只增不减），「整份消失后重新出现」一例在原地址重新出现、不算新副本而红；把 hasNewAddr 改成恒真，
// 「没有再出现新副本」一例配上了而红。
func TestSessionPrefetchedDocFollowsNewCopy(t *testing.T) {
	docA, docB := ttmlDoc(200.4, "A 的歌词"), ttmlDoc(203.3, "B 的歌词")
	// start 放 A（本会话第二首），A 的文档与预取的 B 的文档在同一次全扫里第一次出现：都记在 A 名下，
	// A 用时长最贴的自己那份。
	start := func(t *testing.T) *harness {
		h := newHarness(t)
		h.f.songs = np("更早一首")
		h.setTwin(180, 10, true)
		h.run(5 * time.Second)
		skipTo(h, store("A", "封面:A"), 200)
		h.run(2 * time.Second)
		h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000), docAt(docA, 0xA000)}
		h.run(3 * time.Second)
		if got := lastLyrics(h.evs); got != "A 的歌词" {
			t.Fatalf("前置：A 应配上时长最贴的自己那份（另一份只长 3.3s，也过得了上限），got %q", got)
		}
		return h
	}

	t.Run("轮到它时出现新副本", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000, 0xB800, 0xB900), docAt(docA, 0xA000)}
		skipTo(h, store("B", "封面:B"), 203)
		h.run(3 * time.Second)
		if al := allLyrics(h.since(from)); len(al) != 1 || al[0].Count == 0 || al[0].Lyrics[0].Text != "B 的歌词" {
			t.Fatalf("B 的文档出现新副本，应改记到 B 名下并配上，got %+v", al)
		}
	})
	t.Run("整份消失后重新出现", func(t *testing.T) {
		h := start(t)
		// 中间隔一首没有歌词的歌，B 的文档被回收：这首歌找歌词的全扫一次次都见不到它。
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000)}
		scans := h.f.scans
		skipTo(h, store("中间一首", "封面:中间"), 150)
		h.run(4 * time.Second)
		if h.f.scans-scans < 2 {
			t.Fatalf("前置：其间至少应有两次全扫见不到 B 的文档，got %d 次", h.f.scans-scans)
		}
		from := h.mark()
		h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000), docAt(docA, 0xA000)} // 在原地址重新出现
		skipTo(h, store("B", "封面:B"), 203)
		h.run(3 * time.Second)
		if got := lastLyrics(h.since(from)); got != "B 的歌词" {
			t.Fatalf("B 的文档整份消失后重新出现，应改记到 B 名下并配上，got %q", got)
		}
	})
	t.Run("没有再出现新副本", func(t *testing.T) { // 已知的缝：仍记在 A 名下，配不上
		h := start(t)
		from := h.mark()
		skipTo(h, store("B", "封面:B"), 203)
		h.run(5 * time.Second)
		if got := lastLyrics(h.since(from)); got != "" {
			t.Fatalf("B 的文档没有再出现新副本，仍记在 A 名下，不应配上，got %q", got)
		}
	})
}

// 当前歌已经有歌词：其间别的文档出现新副本（预取下一首），当前歌的歌词不变、不重发 all_lyrics。
//
// 变异自证：把 applySnapshot 里配歌词的条件 `s.song.resolved && s.needLyrics` 的 `&& s.needLyrics` 去掉
// （每次全扫都重新挑），这次全扫又发了一条 all_lyrics 而红。
func TestSessionNewCopyOfOtherDocKeepsCurrentLyrics(t *testing.T) {
	docA, docB := ttmlDoc(200.4, "A 的歌词"), ttmlDoc(201.0, "B 的歌词")
	h := newHarness(t)
	h.f.songs = np("A")
	h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000)}
	h.setTwin(200, 50, true)
	h.run(6 * time.Second)
	if got := lastLyrics(h.evs); got != "A 的歌词" {
		t.Fatalf("前置：A 应配上自己的歌词，got %q", got)
	}

	from, scans := h.mark(), h.f.scans
	h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000), docAt(docB, 0xB000, 0xB800)}
	h.f.songs = []mem.NowPlaying{store("A", "换了一张图")} // 存储换了 → 全扫，扫到 B 的文档
	h.run(3 * time.Second)
	evs := h.since(from)
	if h.f.scans == scans || h.s.docOwner[hashDoc(docB)] != metaKey("A", "歌手") {
		t.Fatalf("前置：应有一次全扫把新出现的文档记到当时那首歌名下，got 全扫 %d 次 owner=%q", h.f.scans-scans, h.s.docOwner[hashDoc(docB)])
	}
	if n := count(evs, player.EventAllLyrics); n != 0 || len(lineIdxs(evs)) != 0 {
		t.Fatalf("A 已有歌词，不应重发 all_lyrics 或重推歌词行，got all_lyrics=%d 歌词行=%v", n, lineIdxs(evs))
	}
}

// 换歌后第一次全扫还读到上一首的标题，新歌的文档恰在这次全扫出现新副本、被改记到旧标题名下；随后读到
// 真标题（titleConfirmWindow 内），这期间改记到旧标题名下的文档要随纠正转给新标题。
//
// 变异自证：把 noteDocs 改记归属时对 docSeenAt 的更新去掉（只改 docOwner），该文档的「记下时刻」仍是
// 附加时，纠正时不被转走，B 无歌词而红。
func TestSessionCorrectedTitleMovesReassignedDoc(t *testing.T) {
	docB := ttmlDoc(259.8, "B 的歌词")
	h := newHarness(t)
	h.f.songs = np("A")
	h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000)} // 附加前就在
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	h.setTwin(0, 0, false)
	h.run(600 * time.Millisecond)
	h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000, 0xB800)} // B 的文档出现新副本
	h.setTwin(260, 0, true)                               // 时长已是 B 的，标题存储还是 A
	h.run(900 * time.Millisecond)
	if o := h.s.docOwner[hashDoc(docB)]; o != metaKey("A", "歌手") {
		t.Fatalf("前置：新副本出现时存储还是 A，应先记到 A 名下，got %q", o)
	}
	from := h.mark()
	h.f.songs = np("B")
	h.run(5 * time.Second)
	if got := lastLyrics(h.since(from)); got != "B 的歌词" {
		t.Fatalf("标题纠正为 B 后，这期间改记到 A 名下的文档应转给 B 并配上，got %q", got)
	}
}

// 新副本出现时没认出任何存储：已有归属的不动，第一次出现的记为来历不明（""）。之后认出了存储、再出现
// 新副本，照常改记。
//
// 变异自证：把 noteDocs 里改记归属那一支的 `haveStore &&` 去掉（没认出存储也改记，记成空标题空歌手），
// 附加前就在的那份被改记而红。
func TestSessionNewCopyWithoutStoreKeepsOwner(t *testing.T) {
	old, late := ttmlDoc(180.3, "附加前就在"), ttmlDoc(150.2, "没认出存储时出现")
	h := newHarness(t)
	h.f.songs = np("A")
	h.f.docs = []mem.TTMLDoc{docAt(old, 0xA000)}
	h.setTwin(300, 50, true)
	h.run(5 * time.Second)

	h.f.songs = nil // 存储认不出了；A 还没有歌词，仍在按节奏全扫
	h.run(2 * time.Second)
	if h.s.store.Sig != nil {
		t.Fatal("前置：应已认定存储消失")
	}
	h.f.docs = []mem.TTMLDoc{docAt(old, 0xA000, 0xA800), docAt(late, 0xD000)}
	h.run(3 * time.Second)
	if o := h.s.docOwner[hashDoc(old)]; o != preAttachOwner {
		t.Fatalf("没认出存储时出现新副本，原归属不应改动，got %q", o)
	}
	if o, known := h.s.docOwner[hashDoc(late)]; !known || o != "" {
		t.Fatalf("没认出存储时第一次出现的文档应记为来历不明，got %q（known=%v）", o, known)
	}

	h.f.songs = np("A")
	h.run(3 * time.Second)
	h.f.docs = []mem.TTMLDoc{docAt(old, 0xA000, 0xA800, 0xA900), docAt(late, 0xD000)}
	h.run(3 * time.Second)
	if o := h.s.docOwner[hashDoc(old)]; o != metaKey("A", "歌手") {
		t.Fatalf("认出存储后再出现新副本，应改记到当时那首歌名下，got %q", o)
	}
	if o := h.s.docOwner[hashDoc(late)]; o != "" {
		t.Fatalf("地址没变的文档不改记，got %q", o)
	}
}

// 歌词文档只在加载中（时长为 0、存储已是新歌）的那次全扫里出现，开播前已从内存消失：归属那时已记到
// 这首歌名下，全文留着，宣告时带着歌词，不报「无歌词」。
//
// 2026-10-09 真机（逐秒采样）：存储换成《信仰》的同一秒，它的文档（<body dur> 256.000）出现 2 份，1 秒后
// 整份消失，这一次播放期间再没出现过；位置结构的时长写入 256 约 1s 后又归零，其后约 9s 才开始走（超过
// loadingGrace，清屏），其间存储也消失、再在新地址重建。歌就绪时文档早已不在内存里，等满 15s 报了无歌词。
//
// 变异自证：把 applySnapshot 里传给 pickTTML 的 `s.lyricCandidates(snap.Docs)` 换回 `snap.Docs`（候选
// 不并入留存），两例都宣告成空歌词而红；在 clearSong 里清空 docKept，「中间加载超时清屏」一例重新宣告时
// 没有歌词而红；把 noteDocs 里留存那一句删掉，两例同样变红。
func TestSessionLyricsSeenOnlyWhileLoading(t *testing.T) {
	doc := ttmlAt(256.0, 10, 20, 30)
	// start 放着上一首时切到《信仰》：时长归零、存储换掉的同时文档出现 2 份，1 秒后整份消失。
	start := func(t *testing.T) *harness {
		h := newHarness(t)
		h.f.songs = np("上一首")
		h.setTwin(248, 10, true)
		h.run(5 * time.Second)
		h.setTwin(0, 0, false)
		h.f.songs = np("信仰")
		h.f.docs = []mem.TTMLDoc{docAt(doc, 0xD000, 0xD800)}
		h.run(time.Second)
		if o := h.s.docOwner[hashDoc(doc)]; o != metaKey("信仰", "歌手") {
			t.Fatalf("前置：加载中的那次全扫应把文档记到新歌名下，got %q", o)
		}
		h.f.docs = nil
		return h
	}

	t.Run("开播前已从内存消失", func(t *testing.T) {
		h := start(t)
		from := h.mark()
		h.run(time.Second)
		h.setTwin(256, 0, true)
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if al := allLyrics(evs); len(al) != 1 || al[0].Count != 3 || al[0].Duration != 256 {
			t.Fatalf("应只发一条带 3 行歌词、时长 256 的 all_lyrics，got %+v", al)
		}
		if v := verdicts(evs); len(v) != 0 {
			t.Fatalf("不应发 index:-1，got %d 条", len(v))
		}
	})
	t.Run("中间加载超时清屏", func(t *testing.T) {
		h := start(t)
		h.setTwin(256, 0, false) // 时长写入了，位置不走
		h.run(900 * time.Millisecond)
		if got := lastLyrics(h.evs); got != "第 0 行" {
			t.Fatalf("前置：时长写入后应带着歌词宣告，got %q", got)
		}
		h.setTwin(0, 0, false) // 又归零
		h.run(time.Second)
		h.f.songs = nil // 存储也消失
		h.run(6 * time.Second)
		if n := count(h.evs, player.EventClearSongData); n != 1 {
			t.Fatalf("前置：加载超过 %v 应清屏一次，got %d", loadingGrace, n)
		}
		from := h.mark()
		h.f.songs = []mem.NowPlaying{store("信仰", "另一张图")} // 存储在新地址重建
		h.run(3 * time.Second)
		h.setTwin(256, 1, true) // 真正开播
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if si := songInfos(evs); len(si) != 1 || si[0].Name != "信仰" {
			t.Fatalf("清屏后应重新宣告《信仰》一次，got %d 条", len(si))
		}
		if al := allLyrics(evs); len(al) != 1 || al[0].Count != 3 {
			t.Fatalf("重新宣告时应带着留下的歌词、只发一条 all_lyrics，got %+v", al)
		}
		if v := verdicts(evs); len(v) != 0 {
			t.Fatalf("不应发 index:-1，got %d 条", len(v))
		}
	})
}

// 歌已宣告之后文档才出现，只被一次全扫见到又消失：当次就配上、补发一条 all_lyrics，之后不重发、不报
// 「无歌词」（留存之前就有的行为，不得退化）。
//
// 变异自证：删掉 applyLyrics 里 `if s.announced { s.emitAllLyrics() ... }`，不补发而红。
func TestSessionLateLyricsSeenOnceThenGone(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("晚到")
	h.setTwin(200, 50, true)
	h.run(5 * time.Second)
	if al := allLyrics(h.evs); len(al) != 1 || al[0].Count != 0 {
		t.Fatalf("前置：先应有一条空的 all_lyrics，got %+v", al)
	}

	from, scans := h.mark(), h.f.scans
	h.f.docs = []mem.TTMLDoc{docAt(ttmlDoc(200.4, "终于来了"), 0xD000)}
	for i := 0; i < 30 && h.f.scans == scans; i++ {
		h.run(step)
	}
	h.f.docs = nil
	if h.f.scans != scans+1 || lastLyrics(h.since(from)) != "终于来了" {
		t.Fatalf("见到它的那次全扫就该补发，got 全扫 %d 次、歌词 %q", h.f.scans-scans, lastLyrics(h.since(from)))
	}
	h.run(lyricGrace)
	if al := allLyrics(h.since(from)); len(al) != 1 || al[0].Count != 2 || al[0].Duration != 200 {
		t.Fatalf("应只补发一次带歌词的 all_lyrics，got %+v", al)
	}
	if v := verdicts(h.evs); len(v) != 0 {
		t.Fatalf("不应发 index:-1，got %d 条", len(v))
	}
}

// 本会话里放过、配上过歌词的歌，之后文档从内存消失，再放时没有重新出现：用留下的那份。留下的文档只给
// 它名下的那首歌——归属以归属表为准，后来被改记到别的歌名下的，跟着走；并且只配给采用它时同一时长的
// 曲目，同名同歌手的另一个版本不用。
//
// 变异自证：候选不并入留存（同 TestSessionLyricsSeenOnlyWhileLoading 的第一条变异），「再放时文档没有
// 重新出现」的再放 A 一段、「改记到别的歌名下」的再放 B 一段无歌词而红；把 pickTTML 里「记在别的歌名
// 下的不用」那个 default 分支改成放行，「再放时文档没有重新出现」里 B 配上了 A 的歌词、「改记到别的
// 歌名下」里再放 A 配上了已改记给 B 的那份而红；去掉 pickTTML 里的时长上限 `diff <= lyricDurSlack`，
// 「同名而时长短得多」一例配上了长版的歌词而红；把 docKept 改成各会话共用的包级表（newSession 不新建），
// 「新会话从空开始」一例配上了上一个会话留下的文档而红。
//
// 时长核对的变异自证：去掉 lyricCandidates 里的时长核对、或 applyLyrics 不记时长，「同名同歌手的更长
// 版本」首次宣告就带了短版的歌词、「同一歌名下再出现新副本」里长版采用了短版而红；noteDocs 把文档改记
// 到别的歌名下时不删时长，「改记后不按原先的时长核对」无歌词而红；同一歌名下出现新副本时也删时长，
// 「同一歌名下再出现新副本」一例长版采用了短版而红；把核对也加到这次全扫里扫到的文档上，「短版文档还在
// 内存里」一例无歌词而红（没有别的用例守这一条）；把核对写成没记过时长的也不并入，「改记后不按原先的
// 时长核对」与 TestSessionLyricsSeenOnlyWhileLoading 的两例无歌词而红。
func TestSessionKeptLyricsFollowOwner(t *testing.T) {
	docA := ttmlDoc(200.4, "A 的歌词")
	// start 放过 A（本会话第二首），文档随它的存储一起出现、配上。
	start := func(t *testing.T) *harness {
		h := newHarness(t)
		h.f.songs = np("更早一首")
		h.setTwin(180, 10, true)
		h.run(5 * time.Second)
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000)}
		skipTo(h, store("A", "封面:A"), 200)
		h.run(3 * time.Second)
		if got := lastLyrics(h.evs); got != "A 的歌词" {
			t.Fatalf("前置：A 应配上自己的歌词，got %q", got)
		}
		return h
	}
	// play 切到 np 这首（时长 dur），放满宽限期，返回这期间最后一条 all_lyrics 的首行与 index:-1 的条数。
	play := func(h *harness, np mem.NowPlaying, dur float64) (string, int) {
		from := h.mark()
		skipTo(h, np, dur)
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		return lastLyrics(evs), len(verdicts(evs))
	}

	t.Run("再放时文档没有重新出现", func(t *testing.T) {
		h := start(t)
		h.f.docs = nil // 放别的歌期间，A 的文档从内存消失
		// B 的时长（203）容得下 A 的文档（200.4）：挡住它的只有归属。
		if got, v := play(h, store("B", "封面:B"), 203); got != "" || v != 1 {
			t.Fatalf("留下的文档记在 A 名下，B 不应配上，got %q（index:-1 %d 条）", got, v)
		}
		from := h.mark()
		if got, v := play(h, store("A", "封面:A"), 200); got != "A 的歌词" || v != 0 {
			t.Fatalf("再放 A，文档没有重新出现，应配上留下的那份，got %q（index:-1 %d 条）", got, v)
		}
		if al := allLyrics(h.since(from)); len(al) != 1 {
			t.Fatalf("应随宣告只发一条 all_lyrics，got %d 条", len(al))
		}
	})
	t.Run("改记到别的歌名下", func(t *testing.T) {
		h := start(t)
		skipTo(h, store("B", "封面:B"), 203)
		h.run(2 * time.Second)
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000, 0xA800)} // 放 B 时这份文档出现新副本
		h.run(3 * time.Second)
		if o := h.s.docOwner[hashDoc(docA)]; o != metaKey("B", "歌手") {
			t.Fatalf("前置：出现新副本时存储是 B，应改记到 B 名下，got %q", o)
		}
		h.f.docs = nil
		if got, v := play(h, store("A", "封面:A"), 200); got != "" || v != 1 {
			t.Fatalf("文档已改记到 B 名下，再放 A 不应配上，got %q（index:-1 %d 条）", got, v)
		}
		if got, v := play(h, store("B", "封面:B"), 203); got != "A 的歌词" || v != 0 {
			t.Fatalf("再放 B，应配上留在它名下的那份，got %q（index:-1 %d 条）", got, v)
		}
	})
	// 留存只在本会话内（已知的缝）：服务重启或 Apple Music 重启后是新会话，从空开始。
	t.Run("新会话从空开始", func(t *testing.T) {
		h := start(t)
		h.f.docs = nil
		h.s = newSession(h.p, h.f, func() time.Time { return h.now })
		from := h.mark()
		h.setTwin(200, 37, true) // A 还在放
		h.run(lyricGrace + 5*time.Second)
		evs := h.since(from)
		if si := songInfos(evs); len(si) != 1 || si[0].Name != "A" {
			t.Fatalf("前置：新会话应宣告 A 一次，got %d 条", len(si))
		}
		if got, v := lastLyrics(evs), verdicts(evs); got != "" || len(v) != 1 {
			t.Fatalf("新会话没有上一个会话留下的文档，应按无歌词，got %q（index:-1 %d 条）", got, len(v))
		}
	})
	// 当前曲目与留下的文档同名同歌手、时长却短得多（同名的另一个版本）：过不了时长上限，不用。
	t.Run("同名而时长短得多", func(t *testing.T) {
		long := ttmlDoc(300.4, "长版的歌词")
		h := newHarness(t)
		h.f.songs = np("更早一首")
		h.setTwin(180, 10, true)
		h.run(5 * time.Second)
		h.f.docs = []mem.TTMLDoc{docAt(long, 0xA000)}
		skipTo(h, store("同名", "封面:长版"), 300)
		h.run(3 * time.Second)
		if got := lastLyrics(h.evs); got != "长版的歌词" {
			t.Fatalf("前置：长版应配上自己的歌词，got %q", got)
		}
		h.f.docs = nil
		if got, v := play(h, storeOf("同名", "另一张专辑", "封面:短版"), 200); got != "" || v != 1 {
			t.Fatalf("留下的文档比曲目长 100s，不应配上，got %q（index:-1 %d 条）", got, v)
		}
	})

	// 以下四例：留下的文档只配给采用它时同一时长的曲目。归属键是（标题, 歌手），分不出同名同歌手的两个
	// 版本，而 <body dur> 只有上限，短版的文档过得了长版的筛。
	longStore := storeOf("A", "加长版专辑", "封面:A 加长版") // 与 A 同名同歌手，时长 300

	// PR #60 审查 R21：先放 200 秒的短版并配上，文档从内存消失，再放 300 秒的长版。不核对时长时，长版
	// 首扫就采用留下的短版文档，自己的文档随后出现也换不上（已有歌词就不再挑），整首用短版的字和时间轴。
	t.Run("同名同歌手的更长版本", func(t *testing.T) {
		long := ttmlAt(300.4, 5, 150, 290)
		h := start(t)
		h.f.docs = nil // 短版的文档从内存消失
		from := h.mark()
		skipTo(h, longStore, 300)
		h.run(3 * time.Second)
		if al := allLyrics(h.since(from)); len(al) != 1 || al[0].Count != 0 || al[0].Duration != 300 {
			t.Fatalf("长版首次宣告不得带短版的歌词，应是一条空的、时长 300 的 all_lyrics，got %+v", al)
		}
		h.f.docs = []mem.TTMLDoc{docAt(long, 0xB000)} // 长版自己的文档出现
		h.run(3 * time.Second)
		evs := h.since(from)
		al := allLyrics(evs)
		if len(al) != 2 || al[1].Count != 3 || al[1].Lyrics[0].Text != "第 0 行" || al[1].Lyrics[2].Timestamp != 290 {
			t.Fatalf("长版自己的文档出现后应补发它的歌词（3 行，末行起点 290s），got %+v", al)
		}
		if v := verdicts(evs); len(v) != 0 {
			t.Fatalf("不应发 index:-1，got %d 条", len(v))
		}
		// 反方向不退化：两份文档都已不在内存里，各配各的。
		h.f.docs = nil
		if got, v := play(h, store("A", "封面:A"), 200); got != "A 的歌词" || v != 0 {
			t.Fatalf("切回短版，应仍用留下的短版文档，got %q（index:-1 %d 条）", got, v)
		}
		if got, v := play(h, longStore, 300); got != "第 0 行" || v != 0 {
			t.Fatalf("再放长版，应用留下的长版文档，got %q（index:-1 %d 条）", got, v)
		}
	})
	// 现状，不是承诺：短版的文档还留在内存里时，它由这次全扫带入、不经时长核对，长版仍会采用它——留存
	// 之前就有的行为，时长核对只管已经离开内存的。
	t.Run("短版文档还在内存里时长版仍采用它", func(t *testing.T) {
		h := start(t) // A 的文档仍在内存里，地址不变
		if got, v := play(h, longStore, 300); got != "A 的歌词" || v != 0 {
			t.Fatalf("现状：这次全扫里扫到的文档不核对时长，got %q（index:-1 %d 条）", got, v)
		}
	})
	// 文档改记到别的歌名下，原先那首歌的时长记录随之作废：新归属的歌时长不同也可用。
	t.Run("改记后不按原先的时长核对", func(t *testing.T) {
		h := start(t)
		h.f.docs = nil
		h.setTwin(0, 0, false) // 切到 B：加载中这份文档出现新副本，开播前又从内存消失
		h.f.songs = np("B")
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA800)}
		h.run(time.Second)
		if o := h.s.docOwner[hashDoc(docA)]; o != metaKey("B", "歌手") {
			t.Fatalf("前置：出现新副本时存储是 B，应改记到 B 名下，got %q", o)
		}
		h.f.docs = nil
		from := h.mark()
		h.run(500 * time.Millisecond)
		h.setTwin(203, 0, true) // B 的时长 203；这份文档上一次是配给 200 秒的 A
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if got, v := lastLyrics(evs), verdicts(evs); got != "A 的歌词" || len(v) != 0 {
			t.Fatalf("文档已改记到 B 名下，B 的时长与它上次配给的曲目不同也应配上，got %q（index:-1 %d 条）", got, len(v))
		}
	})
	// 同一歌名下再出现新副本（短版播放途中，真机上副本会陆续回收又重新出现），归属没变，时长记录保留：
	// 之后放长版，仍不采用它。
	t.Run("同一歌名下再出现新副本不作废时长记录", func(t *testing.T) {
		h := start(t)
		scans := h.f.scans
		h.f.docs = []mem.TTMLDoc{docAt(docA, 0xA000, 0xA800)}
		h.f.songs = []mem.NowPlaying{store("A", "换了一张图")} // 存储换了 → 全扫，见到新副本
		h.run(3 * time.Second)
		if o := h.s.docOwner[hashDoc(docA)]; h.f.scans == scans || o != metaKey("A", "歌手") {
			t.Fatalf("前置：应有一次全扫见到新副本、归属仍是 A，got 全扫 %d 次 owner=%q", h.f.scans-scans, o)
		}
		h.f.docs = nil
		if got, v := play(h, longStore, 300); got != "" || v != 1 {
			t.Fatalf("短版的文档上次配给的是 200 秒的曲目，长版不应采用，got %q（index:-1 %d 条）", got, v)
		}
	})
}

// 换歌后第一次全扫还读到上一首的标题，新歌的文档只在这一次全扫里出现、被记到旧标题名下，随后从内存
// 消失；titleConfirmWindow 内读到真标题时归属转给新标题，留下的全文跟着归属走，新歌有歌词。
//
// 变异自证：候选不并入留存，B 无歌词而红；删掉 applyMeta 里把文档从旧标题转到新标题的循环，留下的那份
// 仍记在 A 名下、B 无歌词而红。
func TestSessionCorrectedTitleMovesKeptDoc(t *testing.T) {
	docB := ttmlDoc(260.5, "B 的歌词")
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	h.setTwin(0, 0, false)
	h.run(600 * time.Millisecond)
	scans := h.f.scans
	h.f.docs = []mem.TTMLDoc{docAt(docB, 0xB000)}
	h.setTwin(260, 0, true) // 时长已是 B 的，标题存储还是 A
	for i := 0; i < 5 && h.f.scans == scans; i++ {
		h.run(step)
	}
	h.f.docs = nil // 只在这一次全扫里出现
	if o := h.s.docOwner[hashDoc(docB)]; h.f.scans != scans+1 || o != metaKey("A", "歌手") {
		t.Fatalf("前置：首扫时存储还是 A，文档应先记到 A 名下，got 全扫 %d 次 owner=%q", h.f.scans-scans, o)
	}
	h.run(500 * time.Millisecond)
	from := h.mark()
	h.f.songs = np("B")
	h.run(lyricGrace + 2*time.Second)
	evs := h.since(from)
	if si := songInfos(evs); len(si) != 1 || si[0].Name != "B" {
		t.Fatalf("应纠正为 B 并宣告一次，got %d 条", len(si))
	}
	if got := lastLyrics(evs); got != "B 的歌词" {
		t.Fatalf("标题纠正为 B 后，留下的那份应随归属转给 B 并配上，got %q", got)
	}
	if v := verdicts(evs); len(v) != 0 {
		t.Fatalf("不应发 index:-1，got %d 条", len(v))
	}
}

// 留存有上限：超过 keptDocsMax 份时丢掉最久没在全扫里见到的，最近见到的仍可用；按最近一次见到算，
// 最早出现而不久前还见到过的不算最久。同一次全扫见到的几份里要丢一份时取哈希小的，结果确定。被丢的
// 那份，采用时记下的时长（docDur）留着：它日后再被留存时仍只配给那个时长。
//
// 变异自证：删掉 noteDocs 里超出上限时的淘汰，留了 50 份而红；把 dropOldestKept 里的 `Before` 改成
// `After`（丢最近见到的），最早的两份还在而红；把 noteDocs 里留存那一句改成只在第一次见到时记（再见到
// 不更新时刻），最早出现的那份被当成最久没见到的丢掉而红——它须在超出上限之前就从内存消失，否则被丢
// 之后的下一次全扫又把它留下，看不出来；去掉 dropOldestKept 里时刻相同时比哈希的那一条，「同一次全扫
// 见到的」一段随 map 迭代序丢掉不同的一份而红；dropOldestKept 连 docDur 一并删，被丢的那份不再记着
// 时长而红（这一条直接看 docDur）。
func TestSessionKeptLyricsCap(t *testing.T) {
	title := func(i int) string { return fmt.Sprintf("歌 %d", i) }
	text := func(i int) string { return ttmlDoc(200.4, fmt.Sprintf("歌 %d 的歌词", i)) }
	kept := func(h *harness, i int) bool {
		_, ok := h.s.docKept[hashDoc(text(i))]
		return ok
	}
	// scan 是一次全扫：存储是第 i 首，见到 also 与第 i 首的文档（各一份，地址固定）。
	scan := func(h *harness, i int, also ...int) {
		h.now = h.now.Add(time.Second)
		var docs []mem.TTMLDoc
		for _, j := range append(also, i) {
			docs = append(docs, docAt(text(j), uintptr(0x10000+j*0x100)))
		}
		h.s.noteDocs(docs, store(title(i), ""), true, h.now)
	}
	last := keptDocsMax + 1 // 共 keptDocsMax+2 首：第 0 ~ last 首

	h := newHarness(t)
	h.s.noteDocs(nil, mem.NowPlaying{}, false, h.now) // 本会话第一次全扫只建基线
	scan(h, 0)
	for i := 1; i <= last; i++ {
		if i < keptDocsMax {
			scan(h, i, 0) // 第 0 首的文档此前一直留在内存里，每次全扫都见到
		} else {
			scan(h, i) // 最后两次全扫（留存超出上限的那两次）之前它才消失
		}
		if i == 3 {
			// 当作第 1、3 首的文档被采用过（applyLyrics 记下当时的曲目时长）：前者稍后被丢，后者留着。
			h.s.docDur[hashDoc(text(1))], h.s.docDur[hashDoc(text(3))] = 200, 200
		}
	}
	if len(h.s.docKept) != keptDocsMax {
		t.Fatalf("留存应封顶在 %d 份，got %d", keptDocsMax, len(h.s.docKept))
	}
	if d, ok := h.s.docDur[hashDoc(text(1))]; !ok || d != 200 {
		t.Fatalf("被丢掉的留存，采用时记下的时长应留着，got %v（在=%v）", d, ok)
	}
	if d, ok := h.s.docDur[hashDoc(text(3))]; !ok || d != 200 {
		t.Fatalf("还留着的那份，采用时记下的时长不应变，got %v（在=%v）", d, ok)
	}
	if !kept(h, 0) {
		t.Fatal("最早出现、但不久前还见到过的文档不算最久没见到的，不应被丢")
	}
	if kept(h, 1) || kept(h, 2) {
		t.Fatalf("最久没见到的两份（第 1、2 首）应被丢，got 第 1 首在=%v 第 2 首在=%v", kept(h, 1), kept(h, 2))
	}
	for i := 3; i <= last; i++ {
		if !kept(h, i) {
			t.Fatalf("第 %d 首的文档比被丢的见到得晚，应还在", i)
		}
	}
	// 用起来：文档都已不在内存里，最近见到的仍配得上，被丢掉的配不上。
	h.f.songs = []mem.NowPlaying{store(title(last), "")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	if got, want := lastLyrics(h.evs), fmt.Sprintf("歌 %d 的歌词", last); got != want {
		t.Fatalf("最近见到的那份应仍可用，got %q, want %q", got, want)
	}
	from := h.mark()
	skipTo(h, store(title(1), ""), 200)
	h.run(lyricGrace + 2*time.Second)
	if got, v := lastLyrics(h.since(from)), verdicts(h.since(from)); got != "" || len(v) != 1 {
		t.Fatalf("被丢掉的那份不可用，应按无歌词，got %q（index:-1 %d 条）", got, len(v))
	}

	// 同一次全扫见到的两份（第 0、1 首）是最久的，超出一份时丢哈希小的那份。
	small, big := 0, 1
	if hashDoc(text(small)) > hashDoc(text(big)) {
		small, big = big, small
	}
	for n := 0; n < 20; n++ { // 多跑几遍：结果不能依赖 map 迭代序
		h := newHarness(t)
		h.s.noteDocs(nil, mem.NowPlaying{}, false, h.now)
		scan(h, 1, 0)
		for i := 2; i <= keptDocsMax; i++ {
			scan(h, i)
		}
		if len(h.s.docKept) != keptDocsMax || kept(h, small) || !kept(h, big) {
			t.Fatalf("同一次全扫见到的两份应丢哈希小的（第 %d 首），got 共 %d 份、第 %d 首在=%v、第 %d 首在=%v",
				small, len(h.s.docKept), small, kept(h, small), big, kept(h, big))
		}
	}
}

// 来历不明的文档（附加前就在内存里的、出现时没认出存储的）不留：从内存消失后，即使时长严格对得上
// 也不可用。
//
// 变异自证：把 noteDocs 里留存的条件去掉（凡扫到的都留），两例都配上了已不在内存里的文档而红。
func TestSessionUnownedLyricsNotKept(t *testing.T) {
	t.Run("附加前就在", func(t *testing.T) {
		doc := ttmlDoc(245.4, "附加前就在")
		h := newHarness(t)
		h.f.songs = np("第一首")
		h.f.docs = []mem.TTMLDoc{docAt(doc, 0xA000)}
		h.setTwin(245, 37, false) // 暂停中附加：位置不走，认不出位置结构
		h.run(3 * time.Second)
		if o := h.s.docOwner[hashDoc(doc)]; o != preAttachOwner {
			t.Fatalf("前置：第一次全扫见到的文档应记为附加前就在，got %q", o)
		}
		h.f.docs = nil // 从内存消失
		from := h.mark()
		h.setTwin(245, 37, true) // 按了播放：本会话第一首，时长也严格对得上
		h.run(lyricGrace + 5*time.Second)
		evs := h.since(from)
		if count(evs, player.EventSongInfoUpdate) != 1 {
			t.Fatalf("前置：恢复播放后应宣告一次，got %d", count(evs, player.EventSongInfoUpdate))
		}
		if got := lastLyrics(evs); got != "" {
			t.Fatalf("附加前就在的文档已不在内存里，不应配上，got %q", got)
		}
		if v := verdicts(evs); len(v) != 1 {
			t.Fatalf("宽限期满应发一次 index:-1，got %d 条", len(v))
		}
		if len(h.s.docKept) != 0 {
			t.Fatalf("附加前就在的文档不应留存，got %d 份", len(h.s.docKept))
		}
	})
	t.Run("出现时没认出存储", func(t *testing.T) {
		doc := ttmlDoc(260.4, "没认出存储时出现")
		h := newHarness(t)
		h.f.songs = np("A")
		h.setTwin(300, 50, true)
		h.run(5 * time.Second)
		h.f.songs = nil // 存储认不出了；A 还没有歌词，仍在按节奏全扫
		h.run(2 * time.Second)
		h.f.docs = []mem.TTMLDoc{docAt(doc, 0xD000)}
		h.run(3 * time.Second)
		if o, known := h.s.docOwner[hashDoc(doc)]; !known || o != "" {
			t.Fatalf("前置：没认出存储时出现的文档应记为来历不明，got %q（known=%v）", o, known)
		}
		h.f.docs = nil // 从内存消失
		from := h.mark()
		skipTo(h, store("B", "封面:B"), 260) // 下一首的时长恰好落进它的严格窗口
		h.run(lyricGrace + 2*time.Second)
		evs := h.since(from)
		if got := lastLyrics(evs); got != "" {
			t.Fatalf("来历不明的文档已不在内存里，不应配上，got %q", got)
		}
		if v := verdicts(evs); len(v) != 1 {
			t.Fatalf("宽限期满应发一次 index:-1，got %d 条", len(v))
		}
		if len(h.s.docKept) != 0 {
			t.Fatalf("来历不明的文档不应留存，got %d 份", len(h.s.docKept))
		}
	})
}
