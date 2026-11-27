package kuwo

// 本文件钉住 kuwo 同一次宣告（同一 RID）内的标题保持、歌名/歌手取舍、Detail 日志去重与提示语歌词识别（#229，
// 设计 doc/design/songinfo-keep-first-design.md §6.4、§6.6、§9.2、§10）：
//   - K1–K5：同一 RID 内 title 恒为宣告时的值，name/singer 经 player.SupplementField 取舍，已补上的值即新基准；
//   - K6–K7：换 RID 或 clear() 后同 RID 属于新宣告，重置基准与 title；
//   - K8：封面补发带取舍后的 name/singer 与宣告时的 title；
//   - K9：读数与基准不同时按上一拍读数去重打 Detail 日志，连续多拍相同读数不刷屏；
//   - K10–K12：提示语歌词识别仍以平台本拍标题（s.metaTitle）匹配，不因对外 title 冻结失配，也不放宽到宣告时的标题。
//
// 变异自证（隔离临时目录实测，均变红）：
//   - 不重设 info.Title（同一 RID 内照旧重拼）→ K1 关、K2、K3 关、K4、K5、K8、K9 关、K10、K11 关、K12 关红；
//   - 用本拍读数算 metadataChanged（取舍放在比较之后）→ K1 开、K3 开、K5 开、K9 开红；
//   - 取舍挪到 changed 分支之外（if !changed 改为恒真，新 RID 也取舍）→ K6、K7（及 K1–K5、K8–K12）红；
//   - 日志不按读数去重（去掉 lastRead 条件）→ K9 开红；
//   - 提示语识别仍用 s.song.Title（不加 metaTitle）→ K10、K11、K12 红；
//   - s.metaTitle 取覆写之后的 info.Title（宣告时的标题）→ K10、K11、K12 红；
//   - 提示语识别同时认宣告时的标题（放宽到再查 s.song.Title）→ K12 红。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

func setKeepFirst(t *testing.T, on bool) {
	t.Helper()
	player.SetSongInfoKeepFirst(on)
	t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
}

func observeMeta(s *session, rid, title, artist string, ms int32, at time.Time) bool {
	return s.observe(
		cdp.Snapshot{RID: rid, Title: title, Artist: artist, Status: 1},
		pos.Sample{RID: rid, PositionMs: ms, DurationMs: 100000, ObservedAt: at},
	)
}

func assertNoMetaEvents(t *testing.T, events []player.Event) {
	t.Helper()
	for _, typ := range []string{player.EventSongInfoUpdate, player.EventStatusUpdate, player.EventAllLyrics} {
		if got := ofType(events, typ); len(got) != 0 {
			t.Fatalf("不应发出 %s，实得 %+v", typ, got)
		}
	}
}

func assertMetaEvents(t *testing.T, events []player.Event, wantName, wantSinger, wantTitle string) {
	t.Helper()
	si := ofType(events, player.EventSongInfoUpdate)
	if len(si) != 1 {
		t.Fatalf("应恰有 1 条 song_info_update，实得 %d 条：%+v", len(si), events)
	}
	info := si[0].Data.(*player.SongInfo)
	if info.Name != wantName || info.Singer != wantSinger || info.Title != wantTitle {
		t.Fatalf("song_info_update = {%q, %q, %q}，want {%q, %q, %q}",
			info.Name, info.Singer, info.Title, wantName, wantSinger, wantTitle)
	}

	st := ofType(events, player.EventStatusUpdate)
	if len(st) != 1 {
		t.Fatalf("应恰有 1 条 status_update，实得 %d 条：%+v", len(st), events)
	}
	status := st[0].Data.(*player.StatusInfo)
	if status.Detail != wantTitle {
		t.Fatalf("status_update.detail = %q，want %q", status.Detail, wantTitle)
	}

	all := ofType(events, player.EventAllLyrics)
	if len(all) != 1 {
		t.Fatalf("应恰有 1 条 all_lyrics，实得 %d 条：%+v", len(all), events)
	}
	lyrics := all[0].Data.(*player.AllLyricsData)
	if lyrics.Title != wantTitle {
		t.Fatalf("all_lyrics.title = %q，want %q", lyrics.Title, wantTitle)
	}
}

func countDetailLogLines(logs, wantBody string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "[+]") && strings.Contains(line, wantBody) {
			n++
		}
	}
	return n
}

func TestSongInfoKeepFirst(t *testing.T) {
	t.Run("K1_same_RID_both_changed", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A2", "X2", 1050, after(t0, 50))
				ev := drainEvents(p)
				if keep {
					assertNoMetaEvents(t, ev)
				} else {
					assertMetaEvents(t, ev, "A2", "X2", "A - X")
				}
			})
		}
	})

	t.Run("K2_same_RID_fill_empty_singer", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A", "X", 1050, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "A", "X", "A")
			})
		}
	})

	t.Run("K3_same_RID_clear_singer", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A", "", 1050, after(t0, 50))
				ev := drainEvents(p)
				if keep {
					assertNoMetaEvents(t, ev)
				} else {
					assertMetaEvents(t, ev, "A", "", "A - X")
				}
			})
		}
	})

	t.Run("K4_same_RID_fill_empty_name", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "", "X", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A", "X", 1050, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "A", "X", " - X")
			})
		}
	})

	t.Run("K5_filled_singer_becomes_baseline", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A", "X", 1050, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "A", "X", "A")

				observeMeta(s, "MUSIC_1", "A", "Y", 1100, after(t0, 100))
				ev := drainEvents(p)
				if keep {
					assertNoMetaEvents(t, ev)
				} else {
					assertMetaEvents(t, ev, "A", "Y", "A")
				}
			})
		}
	})

	t.Run("K6_new_RID_resets_announcement", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_2", "B", "Y", 1000, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "B", "Y", "B - Y")
			})
		}
	})

	t.Run("K7_clear_then_same_RID_resets_announcement", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				if !s.clear() {
					t.Fatal("clear 未清空会话")
				}
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A2", "X2", 1000, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "A2", "X2", "A2 - X2")
			})
		}
	})

	t.Run("K8_cover_carries_filled_singer_and_announced_title", func(t *testing.T) {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "", 1000, t0)
				drainEvents(p)

				observeMeta(s, "MUSIC_1", "A", "X", 1050, after(t0, 50))
				assertMetaEvents(t, drainEvents(p), "A", "X", "A")

				s.accept(assetResult{
					gen:   s.gen,
					rid:   s.rid,
					cover: true,
					art:   cover.Result{URL: "http://cover/a.jpg", DataURI: "data:image/jpeg;base64,AAA"},
				}, after(t0, 100))
				ev := drainEvents(p)
				si := ofType(ev, player.EventSongInfoUpdate)
				if len(si) != 1 {
					t.Fatalf("封面应发 1 条 song_info_update，实得 %+v", ev)
				}
				info := si[0].Data.(*player.SongInfo)
				if info.Name != "A" || info.Singer != "X" || info.Title != "A" ||
					info.Cover != "http://cover/a.jpg" || info.CoverBase64 != "data:image/jpeg;base64,AAA" {
					t.Fatalf("封面 song_info_update = %+v，期望 Name=A Singer=X Title=A 带封面", info)
				}
			})
		}
	})

	t.Run("K9_repeated_read_dedupes_detail_log", func(t *testing.T) {
		const logFmt = "Song info read after the announcement: %s / %s; sent: %s / %s"
		t.Run("keep=true", func(t *testing.T) {
			setKeepFirst(t, true)
			buf := captureLog(t)
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
			drainEvents(p)

			for k := 1; k <= 3; k++ {
				observeMeta(s, "MUSIC_1", "A2", "X2", int32(1000+50*k), after(t0, 50*k))
				assertNoMetaEvents(t, drainEvents(p))
			}
			wantLine := fmt.Sprintf(i18n.T(logFmt), "A2", "X2", "A", "X")
			if n := countDetailLogLines(buf.String(), wantLine); n != 1 {
				t.Fatalf("连续 3 拍读到 A2/X2 应恰打 1 条 Detail 日志 %q，实得 %d 条；日志：\n%s", wantLine, n, buf.String())
			}

			// 读数再次变化时再打 1 条；随后再重复同一读数不再打。
			observeMeta(s, "MUSIC_1", "A3", "X3", 1200, after(t0, 200))
			assertNoMetaEvents(t, drainEvents(p))
			observeMeta(s, "MUSIC_1", "A3", "X3", 1250, after(t0, 250))
			assertNoMetaEvents(t, drainEvents(p))
			wantLine2 := fmt.Sprintf(i18n.T(logFmt), "A3", "X3", "A", "X")
			if n := countDetailLogLines(buf.String(), wantLine2); n != 1 {
				t.Fatalf("读数变为 A3/X3 后连续 2 拍应恰打 1 条 Detail 日志 %q，实得 %d 条；日志：\n%s", wantLine2, n, buf.String())
			}
		})

		t.Run("keep=false", func(t *testing.T) {
			setKeepFirst(t, false)
			buf := captureLog(t)
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
			drainEvents(p)

			observeMeta(s, "MUSIC_1", "A2", "X2", 1050, after(t0, 50))
			assertMetaEvents(t, drainEvents(p), "A2", "X2", "A - X")
			for k := 2; k <= 3; k++ {
				observeMeta(s, "MUSIC_1", "A2", "X2", int32(1000+50*k), after(t0, 50*k))
				assertNoMetaEvents(t, drainEvents(p))
			}
			wantLine := fmt.Sprintf(i18n.T(logFmt), "A2", "X2", "A2", "X2")
			if n := countDetailLogLines(buf.String(), wantLine); n != 1 {
				t.Fatalf("关闭开关下连续 3 拍 A2/X2 应恰打 1 条 Detail 日志 %q，实得 %d 条；日志：\n%s", wantLine, n, buf.String())
			}
		})
	})

	t.Run("K10_pure_music_hint_uses_latest_metaTitle_after_singer_fill", func(t *testing.T) {
		lines := []lyric.Line{
			{TimeMs: 0, Text: "Go Time-Mark Petrie"},
			{TimeMs: 110, Text: "该歌曲为纯音乐请欣赏"},
		}
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "Go Time", "", 1000, t0)
				drainEvents(p)

				t1 := after(t0, 50)
				observeMeta(s, "MUSIC_1", "Go Time", "Mark Petrie", 1050, t1)
				drainEvents(p)

				s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines}, t1)
				s.emitLine(t1)
				ev := drainEvents(p)

				all := ofType(ev, player.EventAllLyrics)
				if len(all) != 1 {
					t.Fatalf("应恰有 1 条 all_lyrics，实得 %+v", ev)
				}
				ad := all[0].Data.(*player.AllLyricsData)
				if ad.Count != 0 || ad.Title != "Go Time" {
					t.Fatalf("all_lyrics = {Count:%d Title:%q}，want {Count:0 Title:\"Go Time\"}", ad.Count, ad.Title)
				}
				lu := ofType(ev, player.EventLyricUpdate)
				if len(lu) != 1 {
					t.Fatalf("应恰有 1 条 lyric_update，实得 %+v", lu)
				}
				u := lu[0].Data.(*player.LyricUpdate)
				if u.Index != -1 || u.Text != "该歌曲为纯音乐请欣赏" {
					t.Fatalf("lyric_update = {Index:%d Text:%q}，want {Index:-1 Text:\"该歌曲为纯音乐请欣赏\"}", u.Index, u.Text)
				}
			})
		}
	})

	t.Run("K11_pure_music_hint_uses_latest_metaTitle_after_rename", func(t *testing.T) {
		lines := []lyric.Line{
			{TimeMs: 0, Text: "B-Y"},
			{TimeMs: 110, Text: "该歌曲为纯音乐请欣赏"},
		}
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				t1 := after(t0, 50)
				observeMeta(s, "MUSIC_1", "B", "Y", 1050, t1)
				drainEvents(p)

				s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines}, t1)
				s.emitLine(t1)
				ev := drainEvents(p)

				all := ofType(ev, player.EventAllLyrics)
				if len(all) != 1 {
					t.Fatalf("应恰有 1 条 all_lyrics，实得 %+v", ev)
				}
				ad := all[0].Data.(*player.AllLyricsData)
				if ad.Count != 0 || ad.Title != "A - X" {
					t.Fatalf("all_lyrics = {Count:%d Title:%q}，want {Count:0 Title:\"A - X\"}", ad.Count, ad.Title)
				}
				lu := ofType(ev, player.EventLyricUpdate)
				if len(lu) != 1 {
					t.Fatalf("应恰有 1 条 lyric_update，实得 %+v", lu)
				}
				u := lu[0].Data.(*player.LyricUpdate)
				if u.Index != -1 || u.Text != "该歌曲为纯音乐请欣赏" {
					t.Fatalf("lyric_update = {Index:%d Text:%q}，want {Index:-1 Text:\"该歌曲为纯音乐请欣赏\"}", u.Index, u.Text)
				}
			})
		}
	})

	t.Run("K12_pure_music_hint_does_not_match_stale_announced_title", func(t *testing.T) {
		lines := []lyric.Line{
			{TimeMs: 0, Text: "A-X"},
			{TimeMs: 110, Text: "该歌曲为纯音乐请欣赏"},
		}
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
				setKeepFirst(t, keep)
				p := New(0, 50)
				s := newSession(p)
				t0 := time.Now()
				observeMeta(s, "MUSIC_1", "A", "X", 1000, t0)
				drainEvents(p)

				t1 := after(t0, 50)
				observeMeta(s, "MUSIC_1", "B", "Y", 1050, t1)
				drainEvents(p)

				s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines}, t1)
				s.emitLine(t1)
				ev := drainEvents(p)

				all := ofType(ev, player.EventAllLyrics)
				if len(all) != 1 {
					t.Fatalf("应恰有 1 条 all_lyrics，实得 %+v", ev)
				}
				ad := all[0].Data.(*player.AllLyricsData)
				if ad.Count != 2 || ad.Title != "A - X" {
					t.Fatalf("all_lyrics = {Count:%d Title:%q}，want {Count:2 Title:\"A - X\"}", ad.Count, ad.Title)
				}
				for _, e := range ofType(ev, player.EventLyricUpdate) {
					if u := e.Data.(*player.LyricUpdate); u.Index == -1 {
						t.Fatalf("不应过滤为无歌词，却发出了 index:-1：%+v", u)
					}
				}
			})
		}
	})
}
