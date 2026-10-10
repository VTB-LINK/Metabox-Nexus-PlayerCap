package applemusic

// 本文件只测会话里的封面：随歌曲信息一起发、晚到或换了就补发一条、不把别的歌的图配上来。

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 封面与标题同在一条存储里，宣告时一起发：cover_base64 是 data URI，cover（URL）恒为空。
//
// 变异自证：删掉 announce 里取封面的那一段（setCover），先发一条不带封面的、下一次全扫再补一条，
// 共两条而红。
func TestSessionCoverWithAnnounce(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{store("带封面", "图片字节")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	si := songInfos(h.evs)
	if len(si) != 1 {
		t.Fatalf("应只发一条 song_info_update，got %d", len(si))
	}
	const want = "data:image/jpeg;base64,5Zu+54mH5a2X6IqC" // base64("图片字节")
	if si[0].CoverBase64 != want || si[0].Cover != "" {
		t.Fatalf("cover_base64=%q cover=%q，want %q 与空 URL", si[0].CoverBase64, si[0].Cover, want)
	}
}

// 宣告时存储里还没有封面（真机首次开播时封面比标题晚约 0.4s，旧会话见过晚 4s 以上）：先发不带封面的，
// 封面写进来时存储重建、盯梢发现，补发一条带封面的——同一首歌，不重新发歌词。
//
// 变异自证：删掉 applySnapshot 里对 applyCover 的调用，封面到了也不补发而红。
func TestSessionLateCoverReemitted(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{store("封面晚到", "")}
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.4, "第一行"), Count: 1}}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	if si := songInfos(h.evs); len(si) != 1 || si[0].CoverBase64 != "" {
		t.Fatalf("封面没到时应先发一条不带封面的，got %d 条", len(si))
	}

	from := h.mark()
	h.f.songs = []mem.NowPlaying{store("封面晚到", "到了")}
	h.run(2 * time.Second)
	evs := h.since(from)
	si := songInfos(evs)
	if len(si) != 1 || !strings.HasPrefix(si[0].CoverBase64, "data:image/jpeg;base64,") || si[0].Name != "封面晚到" {
		t.Fatalf("封面到了应补发一条带封面的 song_info_update，got %d 条", len(si))
	}
	if n := len(allLyrics(evs)); n != 0 {
		t.Fatalf("补封面不应重发歌词，got %d 条 all_lyrics", n)
	}
	// 同一张图不再重复发；之后一直没有封面的也不会一直扫下去（由 scanbudget 用例量）。
	from = h.mark()
	h.run(30 * time.Second)
	if n := len(songInfos(h.since(from))); n != 0 {
		t.Fatalf("封面没再变，不应再发 song_info_update，got %d", n)
	}
}

// 一直没有封面的歌：按 coverTries 找几次就放弃，之后不再为它全扫。
//
// 变异自证：把 scheduleSongScan 里封面那一段改成取不到就一直按最后一档重排，30s 之后仍在全扫而红。
func TestSessionNoCoverGivesUp(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{store("没有封面", "")}
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(400.4, "第一行"), Count: 1}}
	h.setTwin(400, 10, true)
	h.run(30 * time.Second)
	before := h.f.scans
	h.run(2 * time.Minute)
	if n := h.f.scans - before; n != 0 {
		t.Fatalf("放弃找封面之后不应再全扫，got %d 次", n)
	}
	if si := songInfos(h.evs); len(si) != 1 || si[0].CoverBase64 != "" {
		t.Fatalf("应只发一条不带封面的 song_info_update，got %d 条", len(si))
	}
}

// 换歌：新歌带着自己的封面宣告，不沿用上一首的。
//
// 变异自证：同上一条的变异（删掉 announce 里取封面的那一段），第二首先不带封面宣告、再补发，共两条而红。
func TestSessionCoverFollowsSong(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{store("第一首", "图一")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	first := songInfos(h.evs)[0].CoverBase64

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = []mem.NowPlaying{store("第二首", "图二")}
	h.run(1300 * time.Millisecond)
	h.setTwin(260, 0, true)
	h.run(3 * time.Second)
	si := songInfos(h.since(from))
	if len(si) != 1 || si[0].Name != "第二首" || si[0].CoverBase64 == "" || si[0].CoverBase64 == first {
		t.Fatalf("第二首应带自己的封面宣告一次，got %d 条", len(si))
	}
}

// skipTo 从正在放的歌切到下一首：归零 1.3s（其间存储换成 first），然后开播。
func skipTo(h *harness, first mem.NowPlaying, dur float64) {
	h.setTwin(0, 0, false)
	h.f.songs = []mem.NowPlaying{first}
	h.run(1300 * time.Millisecond)
	h.setTwin(dur, 0, true)
}

// 换歌后新存储先带着新标题建出来，图却还是上一首的，约 0.4s 后才换成自己的（2026-10-08 真机，5 次切歌
// 2 次）：上一首的图不能配到新歌上——先不带图宣告，图换了补发。
//
// 变异自证：把 announce 里的 `!s.staleCover(np, h)` 去掉，新歌的第一条 song_info_update 带着上一首的图
// 而红。
func TestSessionStaleCoverNotSent(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{storeOf("上一首", "专辑甲", "图甲")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	old := songInfos(h.evs)[0].CoverBase64

	from := h.mark()
	skipTo(h, storeOf("新歌", "专辑乙", "图甲"), 260) // 新标题、旧图
	h.run(400 * time.Millisecond)
	h.f.songs = []mem.NowPlaying{storeOf("新歌", "专辑乙", "图乙")} // 图换成自己的
	h.run(2 * time.Second)
	si := songInfos(h.since(from))
	if len(si) != 2 || si[0].Name != "新歌" || si[0].CoverBase64 != "" {
		t.Fatalf("应先不带图宣告新歌、图换了再补一条，got %d 条（首条带图=%v）", len(si), len(si) > 0 && si[0].CoverBase64 != "")
	}
	if si[1].CoverBase64 == "" || si[1].CoverBase64 == old {
		t.Fatal("补发的那条应带新歌自己的图")
	}
}

// 加载超过 loadingGrace 清屏之后，「上一首发出的是哪张图、哪张专辑」仍要记着：新歌的存储同样可能先带着
// 上一首的图建出来。清屏时把它抹掉，新歌就带着上一首的封面宣告。
//
// 变异自证：把 clearSong 改回 `s.prevCover, s.prevAlbum = 0, ""`，「新歌先带着上一首的图」一例第一条
// song_info_update 带了图甲、「同一张图不同专辑」一例没等 coverHold 就发、「清掉的那首没发过封面」一例
// 带了更早那首的图，三例变红；把 clearSong 里的记录改成只记图、专辑置空，「同一首重新宣告」一例被当成
// 换了专辑、先不带图宣告而红；去掉 clearSong 里的 `s.song.coverHash != 0` 条件（没发过也照记），
// 「清掉的那首没发过封面」一例变红。
func TestSessionCoverIdentityKeptAcrossClear(t *testing.T) {
	// playThenClear 放 first（放之前若给了 before，先放它再切到 first），然后时长归零 7s 触发清屏。
	playThenClear := func(t *testing.T, first mem.NowPlaying, before ...mem.NowPlaying) *harness {
		h := newHarness(t)
		h.f.songs = []mem.NowPlaying{first}
		if len(before) > 0 {
			h.f.songs = before[:1]
		}
		h.setTwin(200, 10, true)
		h.run(5 * time.Second)
		if len(before) > 0 {
			skipTo(h, first, 220)
			h.run(5 * time.Second)
		}
		h.setTwin(0, 0, false)
		h.run(7 * time.Second)
		if count(h.evs, player.EventClearSongData) != 1 {
			t.Fatalf("前置：加载超过 %v 应清屏一次，got %d", loadingGrace, count(h.evs, player.EventClearSongData))
		}
		return h
	}
	// startNext 清屏之后下一首开播：存储先换成 next，0.3s 后时长写入。
	startNext := func(h *harness, next mem.NowPlaying, dur float64) {
		h.f.songs = []mem.NowPlaying{next}
		h.run(300 * time.Millisecond)
		h.setTwin(dur, 0, true)
	}
	uri := func(cover string) string {
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte(cover))
	}
	imgA, imgB := uri("图甲"), uri("图乙")

	t.Run("新歌先带着上一首的图", func(t *testing.T) {
		h := playThenClear(t, storeOf("上一首", "专辑甲", "图甲"))
		from := h.mark()
		startNext(h, storeOf("新歌", "专辑乙", "图甲"), 260) // 新标题、旧图
		h.run(400 * time.Millisecond)
		h.f.songs = []mem.NowPlaying{storeOf("新歌", "专辑乙", "图乙")} // 图换成自己的
		h.run(2 * time.Second)
		si := songInfos(h.since(from))
		if len(si) != 2 || si[0].Name != "新歌" || si[0].CoverBase64 != "" || si[1].CoverBase64 != imgB {
			t.Fatalf("应先不带图宣告新歌、图换了再补一条带图乙的，got %d 条（首条带图=%v）", len(si), len(si) > 0 && si[0].CoverBase64 != "")
		}
	})
	t.Run("同一首重新宣告", func(t *testing.T) {
		h := playThenClear(t, storeOf("上一首", "专辑甲", "图甲"))
		from := h.mark()
		startNext(h, storeOf("上一首", "专辑甲", "图甲"), 200)
		h.run(5 * time.Second)
		if si := songInfos(h.since(from)); len(si) != 1 || si[0].CoverBase64 != imgA {
			t.Fatalf("同一首重新宣告应随宣告带图甲、只发一条，got %d 条", len(si))
		}
	})
	t.Run("同一张图不同专辑", func(t *testing.T) {
		h := playThenClear(t, storeOf("单曲版", "专辑甲", "图甲"))
		from := h.mark()
		startNext(h, storeOf("专辑版", "专辑乙", "图甲"), 260)
		h.run(2 * time.Second)
		if si := songInfos(h.since(from)); len(si) != 1 || si[0].CoverBase64 != "" {
			t.Fatalf("coverHold 内应只有一条不带图的宣告，got %d 条", len(si))
		}
		h.run(3 * time.Second)
		if si := songInfos(h.since(from)); len(si) != 2 || si[1].CoverBase64 != imgA {
			t.Fatalf("过了 coverHold 应补上这张图，got %d 条", len(si))
		}
	})
	t.Run("上一首从未发过封面", func(t *testing.T) {
		h := playThenClear(t, storeOf("没有封面", "专辑甲", ""))
		from := h.mark()
		startNext(h, storeOf("新歌", "专辑乙", "图乙"), 260)
		h.run(5 * time.Second)
		if si := songInfos(h.since(from)); len(si) != 1 || si[0].CoverBase64 != imgB {
			t.Fatalf("没有可比对的上一张图时，新歌应带着自己的图宣告一次，got %d 条", len(si))
		}
	})
	t.Run("清掉的那首没发过封面", func(t *testing.T) { // 沿用更早那首发出的图
		h := playThenClear(t, storeOf("没有封面", "专辑乙", ""), storeOf("更早那首", "专辑甲", "图甲"))
		from := h.mark()
		startNext(h, storeOf("新歌", "专辑丙", "图甲"), 260)
		h.run(400 * time.Millisecond)
		h.f.songs = []mem.NowPlaying{storeOf("新歌", "专辑丙", "图乙")}
		h.run(2 * time.Second)
		si := songInfos(h.since(from))
		if len(si) != 2 || si[0].CoverBase64 != "" || si[1].CoverBase64 != imgB {
			t.Fatalf("应先不带图宣告、图换了再补一条带图乙的，got %d 条（首条带图=%v）", len(si), len(si) > 0 && si[0].CoverBase64 != "")
		}
	})
}

// 同一张专辑连着两首，图本来就是同一张：照常随宣告发，不扣。
//
// 变异自证：把 staleCover 里的 `np.Album != s.prevAlbum` 去掉，第二首先不带图宣告而红。
func TestSessionSameAlbumCoverSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{storeOf("第一首", "同一张专辑", "专辑图")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	first := songInfos(h.evs)[0].CoverBase64

	from := h.mark()
	skipTo(h, storeOf("第二首", "同一张专辑", "专辑图"), 260)
	h.run(5 * time.Second)
	if si := songInfos(h.since(from)); len(si) != 1 || si[0].CoverBase64 != first || first == "" {
		t.Fatalf("同专辑的下一首应带着同一张图宣告一次，got %d 条", len(si))
	}
}

// 两张专辑真用了同一张图（图一直不换）：过了 coverHold 照发，不会一直没有封面。
//
// 变异自证：把 staleCover 里 `< coverHold` 的时限去掉（只要与上一首相同就一直不发），5s 后仍没有带图
// 的 song_info_update 而红。
func TestSessionSameImageDifferentAlbumSentAfterHold(t *testing.T) {
	h := newHarness(t)
	h.f.songs = []mem.NowPlaying{storeOf("单曲版", "某单曲", "同一张图")}
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	first := songInfos(h.evs)[0].CoverBase64

	from := h.mark()
	skipTo(h, storeOf("专辑版", "某专辑", "同一张图"), 260)
	h.run(5 * time.Second)
	si := songInfos(h.since(from))
	if len(si) != 2 || si[0].CoverBase64 != "" || si[1].CoverBase64 != first {
		t.Fatalf("应先不带图宣告、过了 coverHold 补上这张图，got %d 条", len(si))
	}
}
