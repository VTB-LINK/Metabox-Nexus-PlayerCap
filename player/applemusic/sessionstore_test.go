package applemusic

// 本文件只测会话里「正在播放」存储的选取：内存里并存几首歌的完整存储时，宣告的是哪一首。

import (
	"encoding/base64"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// storeBy 同 storeOf，另给歌手。
func storeBy(title, artist, album, cover string) mem.NowPlaying {
	np := storeOf(title, album, cover)
	np.Artist = artist
	return np
}

// 内存里除了当前歌带封面的完整存储，还留着另一首歌不带封面的完整存储：宣告的是带封面的那首。
//
// 2026-10-09 真机：《过火》播放中，《crossing field》的一份不带封面的完整副本一直残留。服务在这时重启，
// 宣告成了 crossing field——确认位置结构之前的全扫已见过这两份，开始这首歌时它们都转成「上一首期间
// 就在的」，两份都完整、各一票，落到字典序。连续运行时同一首从头再放、加载超时清屏后重新宣告，走的是
// 同一条路。
//
// 变异自证：删掉 pickNowPlaying 排序里 `fullCover` 一档，「确认位置结构之前认定的存储」一例认定成
// crossing field 而红，「附加时正在放」与「连续运行」两例都在附加后的第一次宣告就成了 crossing field
// 而红（三例的第一次宣告走的是同一条路，该变异下到不了各自后面的断言）；把这一档挪到 `freshFull`
// 之前，「新歌先只有不带封面的」一例先宣告成上一首的歌名而红；把 announce 里的 `!s.staleCover(np, h)`
// 去掉，「随后切到残留的那首」一段的第一条带着《过火》的图而红。
func TestSessionCoveredStoreAnnouncedOverCoverlessResidue(t *testing.T) {
	residue := storeBy("crossing field", "LiSA", "LANDSPACE", "")
	playing := storeBy("过火", "张信哲", "从开始到现在", "封面:过火")
	uri := func(cover string) string {
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte(cover))
	}
	// attach 附加时正在放《过火》，内存里另有《crossing field》不带封面的完整副本。
	attach := func(t *testing.T) *harness {
		h := newHarness(t)
		h.f.songs = []mem.NowPlaying{residue, playing}
		h.setTwin(301, 50, true)
		h.run(5 * time.Second)
		si := songInfos(h.evs)
		if len(si) != 1 || si[0].Name != "过火" || si[0].Singer != "张信哲" || si[0].CoverBase64 != uri("封面:过火") || h.s.song.album != "从开始到现在" {
			t.Fatalf("应宣告带封面的《过火》一次，got %d 条：%+v（专辑 %q）", len(si), names(si), h.s.song.album)
		}
		return h
	}

	t.Run("附加时正在放，随后切到残留的那首", func(t *testing.T) {
		h := attach(t)
		// 照真机 03:49:00：《过火》带封面的那份被就地改写成《crossing field》（封面还是《过火》的，约 1s
		// 后释放），同时另建一份带新封面的、地址更高；残留的那份还在。代表取地址小的，即改写的那份：
		// 它带的是上一首的图，先不带图宣告，释放后补上自己的。
		rewritten := storeBy("crossing field", "LiSA", "LANDSPACE", "封面:过火")
		rewritten.Addr = playing.Addr
		own := storeBy("crossing field", "LiSA", "LANDSPACE", "封面:crossing field")
		own.Addr = playing.Addr + 0x1000
		if own.Addr == residue.Addr {
			t.Fatal("前置：夹具地址相撞")
		}
		from := h.mark()
		h.setTwin(0, 0, false)
		h.f.songs = []mem.NowPlaying{residue, rewritten, own}
		h.run(1300 * time.Millisecond)
		h.setTwin(248, 0, true)
		h.run(time.Second)
		h.f.songs = []mem.NowPlaying{residue, own}
		h.run(4 * time.Second)
		si := songInfos(h.since(from))
		if len(si) != 2 || h.s.song.album != "LANDSPACE" {
			t.Fatalf("应宣告《crossing field》一次、再补一条封面，got %d 条：%+v（专辑 %q）", len(si), names(si), h.s.song.album)
		}
		for _, s := range si {
			if s.Name != "crossing field" || s.Singer != "LiSA" {
				t.Fatalf("每一条都应是《crossing field》，got %q / %q", s.Name, s.Singer)
			}
		}
		if si[0].CoverBase64 != "" || si[1].CoverBase64 != uri("封面:crossing field") {
			t.Fatalf("应先不带图（改写的那份带的是《过火》的图）、再补上自己的封面，got 首条带图=%v 末条是自己的图=%v",
				si[0].CoverBase64 != "", si[1].CoverBase64 == uri("封面:crossing field"))
		}
	})

	// 确认位置结构之前的全扫（此时还没有「上一首期间就在的」可比，两份都算新的）认定的也应是带封面的
	// 那首：盯梢盯的、歌词文档记到谁名下，都跟着它。
	t.Run("确认位置结构之前认定的存储", func(t *testing.T) {
		h := newHarness(t)
		h.f.songs = []mem.NowPlaying{residue, playing}
		h.setTwin(301, 50, true)
		h.run(time.Second)
		if h.f.scans == 0 || h.s.twin != 0 || count(h.evs, player.EventSongInfoUpdate) != 0 {
			t.Fatalf("前置：1s 内应已全扫过、尚未确认位置结构，got 全扫 %d 次 twin=%#x", h.f.scans, h.s.twin)
		}
		if h.s.store.Title != "过火" || h.s.store.Addr != playing.Addr {
			t.Fatalf("认定的存储应是带封面的《过火》，got %q", h.s.store.Title)
		}
	})

	for _, tc := range []struct {
		name   string
		gap    time.Duration
		clears int
	}{
		{"同一首从头再放", 1300 * time.Millisecond, 0},
		{"加载超时清屏后重新宣告", 7 * time.Second, 1},
	} {
		t.Run("连续运行："+tc.name, func(t *testing.T) {
			h := attach(t)
			from := h.mark()
			h.setTwin(0, 0, false) // 两份存储的地址与内容都不变
			h.run(tc.gap)
			if n := count(h.since(from), player.EventClearSongData); n != tc.clears {
				t.Fatalf("前置：时长归零 %v 应清屏 %d 次，got %d", tc.gap, tc.clears, n)
			}
			h.setTwin(301, 0, true)
			h.run(5 * time.Second)
			if si := songInfos(h.since(from)); len(si) != 1 || si[0].Name != "过火" || si[0].Singer != "张信哲" {
				t.Fatalf("两份存储都是此前就在的，仍应宣告带封面的《过火》一次，got %+v", names(si))
			}
		})
	}

	// 换歌的头一瞬间：新歌只有一份不带封面的完整副本，上一首带封面的那份还没被改写。新的那份胜出
	// （「不是上一首期间就在的完整副本数」排在「带封面」之前）。
	t.Run("新歌先只有不带封面的", func(t *testing.T) {
		h := newHarness(t)
		old := storeBy("上一首", "歌手甲", "专辑甲", "封面:上一首")
		h.f.songs = []mem.NowPlaying{old}
		h.setTwin(200, 50, true)
		h.run(5 * time.Second)

		from := h.mark()
		h.f.songs = []mem.NowPlaying{old, storeBy("新歌", "歌手乙", "专辑乙", "")}
		h.setTwin(260, 0, true) // 自然衔接：没看到归零，时长直接变了
		h.run(time.Second)
		rewritten := storeBy("新歌", "歌手乙", "专辑乙", "封面:上一首")
		rewritten.Addr = old.Addr
		h.f.songs = []mem.NowPlaying{rewritten, storeBy("新歌", "歌手乙", "专辑乙", ""), storeBy("新歌", "歌手乙", "专辑乙", "封面:新歌")}
		h.run(4 * time.Second)
		si := songInfos(h.since(from))
		if len(si) == 0 {
			t.Fatal("应宣告《新歌》")
		}
		for _, s := range si {
			if s.Name != "新歌" || s.Singer != "歌手乙" || s.CoverBase64 == uri("封面:上一首") {
				t.Fatalf("每一条都应是《新歌》，且不带上一首的封面，got %+v", names(si))
			}
		}
	})
}

// names 返回各条 song_info_update 的「歌手 - 歌名」。
func names(si []*player.SongInfo) []string {
	out := make([]string, len(si))
	for i, s := range si {
		out[i] = s.Title
	}
	return out
}
