package sodamusic

// 本文件只测 runSession 对无歌词提示语的识别与发出（#231，设计 doc/design/converge-player-behavior-231-design.md §7.4、§7.5）：
// 经 player.PureMusicHint 认出 kuwo 形状单行提示与「歌名行 + 提示句」两行，发空 all_lyrics 与 index:-1 的
// lyric_update 且 text 为提示原文而非歌名行，并清掉 currentLyrics 使后续推进不发普通歌词行；
// 反例：首行与 currentTitle 不同时照常按普通歌词处理。
//
// 变异自证（均实测变红）：runSession 仍读 lyricItems[0].Text 而非 PureMusicHint 返回的 hint →
// 「歌名行加提示取第 1 行原文」红；改回只认「纯音乐，请欣赏」的旧判断 → 「kuwo 形状单行提示」「歌名行加提示取第 1 行原文」红。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
)

func TestPureMusicHintInSession(t *testing.T) {
	origFetch := fetchKugouLyrics
	fetchKugouLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	defer func() { fetchKugouLyrics = origFetch }()

	const (
		kuwoPureHint = "此歌曲为没有填词的纯音乐，请您欣赏"
		kuwoGaiHint  = "该歌曲为纯音乐请欣赏"
	)

	cases := []struct {
		name          string
		krcContent    string
		wantNoLyrics  bool
		wantHint      string
		wantAllLyrics int
	}{
		{
			name:          "kuwo 形状单行提示",
			krcContent:    "[0,5000]<0,5000,0>" + kuwoPureHint + "\n",
			wantNoLyrics:  true,
			wantHint:      kuwoPureHint,
			wantAllLyrics: 0,
		},
		{
			// Name="Go Time", Artists=["Mark Petrie"] → currentTitle="Mark Petrie - Go Time"
			name:          "歌名行加提示取第 1 行原文",
			krcContent:    "[0,1000]<0,1000,0>Mark Petrie-Go Time\n[1000,5000]<0,5000,0>" + kuwoGaiHint + "\n",
			wantNoLyrics:  true,
			wantHint:      kuwoGaiHint,
			wantAllLyrics: 0,
		},
		{
			name:          "反例：首行与标题不同按普通歌词",
			krcContent:    "[0,1000]<0,1000,0>Other Artist-Go Time\n[1000,5000]<0,5000,0>" + kuwoGaiHint + "\n",
			wantNoLyrics:  false,
			wantAllLyrics: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
				if e >= 3*time.Second {
					return nil, errLost
				}
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 2 + float32(e/time.Second),
					DurationSeconds: 200,
					MediaID:         "pm-1",
					Name:            "Go Time",
					Artists:         []string{"Mark Petrie"},
					LyricType:       "krc",
					LyricContent:    c.krcContent,
				}, nil
			})

			var all []*player.AllLyricsData
			var updates []*player.LyricUpdate
			for _, te := range evs {
				switch d := te.evt.Data.(type) {
				case *player.AllLyricsData:
					all = append(all, d)
				case *player.LyricUpdate:
					updates = append(updates, d)
				}
			}
			if len(all) != 1 || all[0].Count != c.wantAllLyrics || len(all[0].Lyrics) != c.wantAllLyrics {
				t.Fatalf("all_lyrics = %+v，期望 1 条、count=%d", all, c.wantAllLyrics)
			}
			if c.wantNoLyrics {
				if len(updates) != 1 {
					t.Fatalf("提示语应仅发 1 条 index:-1 的 lyric_update，实得 %+v", updates)
				}
				u := updates[0]
				if u.Index != -1 || u.Text != c.wantHint {
					t.Fatalf("lyric_update = {Index:%d Text:%q}，期望 {Index:-1 Text:%q}", u.Index, u.Text, c.wantHint)
				}
			} else {
				if len(updates) == 0 {
					t.Fatalf("普通歌词推进中应推出歌词行，实得 0 条")
				}
				for _, u := range updates {
					if u.Index == -1 {
						t.Fatalf("非提示语不应发 index:-1，实得 %+v", u)
					}
				}
			}
		})
	}
}

// replayLyricCache 按 server/server.go UpdatePlayerState 的规则回放事件流中的 LyricUpdate 缓存：
// EventAllLyrics 与 EventClearSongData 清空缓存，EventLyricUpdate 写入缓存。
func replayLyricCache(evs []timedEvent) *player.LyricUpdate {
	var cached *player.LyricUpdate
	for _, te := range evs {
		switch te.evt.Type {
		case player.EventAllLyrics, player.EventClearSongData:
			cached = nil
		case player.EventLyricUpdate:
			if u, ok := te.evt.Data.(*player.LyricUpdate); ok {
				cp := *u
				cached = &cp
			}
		}
	}
	return cached
}

// 时长晚到（超过 durationGrace 才出现 DurationSeconds > 0）重发空的 all_lyrics 时，服务端会清空
// LyricUpdate 缓存；无歌词曲目必须紧跟补发 index:-1（保留当前曲目的提示原文，并带更新后的位置与正进度）。
// 换歌到普通无歌词曲目、或无歌清空后再播无歌词曲目时，currentPureHint 必须已复位，不得串用上一首的提示（#231）。
//
// 变异自证（均实测）：
//  1. 去掉 sodamusic.go 时长晚到分支中 `if len(currentLyrics) == 0` 的 index:-1 补发，前三个子用例均红；
//  2. 去掉换歌分支的 `currentPureHint = ""` 复位，「换到无提示无歌词曲目且时长晚到不串用上一首提示」红；
//  3. 去掉无歌清空分支的 `currentPureHint = ""` 复位，「无歌清空后收到缺 MediaID 的异常快照不沿用旧提示」红。
func TestPureMusicHintLateDurationReemitsVerdict(t *testing.T) {
	origFetch := fetchKugouLyrics
	fetchKugouLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	defer func() { fetchKugouLyrics = origFetch }()

	const (
		kuwoGaiHint = "该歌曲为纯音乐请欣赏"
		titleHint   = "[0,1000]<0,1000,0>Mark Petrie-Go Time\n[1000,5000]<0,5000,0>" + kuwoGaiHint + "\n"
	)

	t.Run("提示型 KRC 时长晚到补发带提示原文与正进度的 index:-1", func(t *testing.T) {
		evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
			if e >= 6*time.Second {
				return nil, errLost
			}
			var dur float32
			if e >= 3*time.Second {
				dur = 200
			}
			return &cdp.ExtractionData{
				OK:              true,
				IsPlaying:       true,
				ProgressSeconds: 2 + float32(e/time.Second),
				DurationSeconds: dur,
				MediaID:         "pm-late-1",
				Name:            "Go Time",
				Artists:         []string{"Mark Petrie"},
				LyricType:       "krc",
				LyricContent:    titleHint,
			}, nil
		})

		var all []*player.AllLyricsData
		var updates []*player.LyricUpdate
		for _, te := range evs {
			switch d := te.evt.Data.(type) {
			case *player.AllLyricsData:
				all = append(all, d)
			case *player.LyricUpdate:
				updates = append(updates, d)
			}
		}
		if len(all) != 2 || all[0].Duration != 0 || all[1].Duration != 200 || all[1].Count != 0 {
			t.Fatalf("应发 2 条空 all_lyrics（先 duration=0，补齐后 duration=200），实得 %+v", all)
		}
		if len(updates) != 2 {
			t.Fatalf("换歌与时长补齐各应发 1 条 index:-1，实得 %d 条: %+v", len(updates), updates)
		}
		last := updates[1]
		if last.Index != -1 || last.Text != kuwoGaiHint || last.Position <= 0 || last.Progress <= 0 {
			t.Fatalf("时长补齐补发的 index:-1 = %+v，期望 Text=%q 且 Position/Progress > 0", last, kuwoGaiHint)
		}
		cached := replayLyricCache(evs)
		if cached == nil || cached.Index != -1 || cached.Text != kuwoGaiHint || cached.Progress <= 0 {
			t.Fatalf("服务端最终缓存应为带提示原文与正进度的 index:-1，实得 %+v", cached)
		}
	})

	t.Run("换到无提示无歌词曲目且时长晚到不串用上一首提示", func(t *testing.T) {
		evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
			switch {
			case e < 2*time.Second:
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 1 + float32(e/time.Second),
					DurationSeconds: 200,
					MediaID:         "pm-1",
					Name:            "Go Time",
					Artists:         []string{"Mark Petrie"},
					LyricType:       "krc",
					LyricContent:    titleHint,
				}, nil
			case e < 8*time.Second:
				var dur float32
				if e >= 5*time.Second {
					dur = 180
				}
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 1 + float32((e-2*time.Second)/time.Second),
					DurationSeconds: dur,
					MediaID:         "pm-2",
					Name:            "No Lyrics Track",
					Artists:         []string{"Artist B"},
				}, nil
			}
			return nil, errLost
		})

		cached := replayLyricCache(evs)
		if cached == nil || cached.Index != -1 || cached.Text != "" || cached.Progress <= 0 {
			t.Fatalf("第二首无提示无歌词时长补齐后，服务端缓存应为 text=\"\" 且 progress>0 的 index:-1，实得 %+v", cached)
		}
	})

	t.Run("无歌清空后再播无歌词曲目且时长晚到不串用上一首提示", func(t *testing.T) {
		evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
			switch {
			case e < 2*time.Second:
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 1 + float32(e/time.Second),
					DurationSeconds: 200,
					MediaID:         "pm-1",
					Name:            "Go Time",
					Artists:         []string{"Mark Petrie"},
					LyricType:       "krc",
					LyricContent:    titleHint,
				}, nil
			case e < 3*time.Second:
				return noSong, nil
			case e < 9*time.Second:
				var dur float32
				if e >= 6*time.Second {
					dur = 180
				}
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 1 + float32((e-3*time.Second)/time.Second),
					DurationSeconds: dur,
					MediaID:         "pm-3",
					Name:            "After Clear Track",
					Artists:         []string{"Artist C"},
				}, nil
			}
			return nil, errLost
		})

		cached := replayLyricCache(evs)
		if cached == nil || cached.Index != -1 || cached.Text != "" || cached.Progress <= 0 {
			t.Fatalf("无歌清空后新曲无提示时长补齐后，服务端缓存应为 text=\"\" 且 progress>0 的 index:-1，实得 %+v", cached)
		}
	})

	t.Run("无歌清空后收到缺 MediaID 的异常快照不沿用旧提示", func(t *testing.T) {
		evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
			switch {
			case e < 2*time.Second:
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 1 + float32(e/time.Second),
					DurationSeconds: 200,
					MediaID:         "pm-1",
					Name:            "Go Time",
					Artists:         []string{"Mark Petrie"},
					LyricType:       "krc",
					LyricContent:    titleHint,
				}, nil
			case e < 3*time.Second:
				return noSong, nil
			case e < 4*time.Second:
				// MediaID 为空而 Name/Duration 非空：不走换歌分支，但会命中时长补取条件；
				// 无歌清空必须已复位 currentPureHint，不得将上一首的提示发出。
				return &cdp.ExtractionData{
					OK:              true,
					IsPlaying:       true,
					ProgressSeconds: 5,
					DurationSeconds: 180,
					MediaID:         "",
					Name:            "Transient",
				}, nil
			}
			return nil, errLost
		})

		if cached := replayLyricCache(evs); cached != nil && cached.Text != "" {
			t.Fatalf("无歌清空后不得再发出上一首的提示语，实得 %+v", cached)
		}
	})
}
