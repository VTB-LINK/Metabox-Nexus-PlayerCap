package cloudmusic

// 本文件只测 runSession 两处无歌词提示识别的接线（换歌拍取词与 Redux 歌词晚到，#231，
// 设计 doc/design/converge-player-behavior-231-design.md §7.4、§7.5）：
// 经 player.PureMusicHint 认出单行提示（含 kuwo 形状）与「歌名行 + 提示句」两行，
// 发空的 all_lyrics 与 index:-1 的 lyric_update，且 text 填提示句原文而非歌名行；
// 识别后清掉 activeLyrics，后续推进不再推出歌词行。反例：两行但第 0 行与标题不同时照常按歌词发出。
//
// 变异自证（均实测变红）：两处调用点仍读 lyricItems[0].Text 而非 PureMusicHint 返回的 hint →
// 「换歌拍 CDP：歌名行加提示取第 1 行原文」「Redux 晚到：歌名行加提示取第 1 行原文」红；
// 两处改回只认「纯音乐，请欣赏」的旧判断 → 全部四条正例红。

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
)

type hintClient struct {
	snap   func(now time.Duration) *cdp.ExtractionData
	lrc    *cdp.LyricFetchResult
	lrcErr error
	end    time.Duration
	t0     time.Time
	closed bool
}

func (c *hintClient) Extract() (*cdp.ExtractionData, error) {
	now := time.Since(c.t0)
	if now >= c.end {
		c.closed = true
		return nil, errors.New("websocket: close 1006")
	}
	return c.snap(now), nil
}
func (c *hintClient) IsClosed() bool                 { return c.closed }
func (c *hintClient) ForceFetchLyricsInRedux() error { return nil }
func (c *hintClient) FetchLyricsViaCDP(string) (*cdp.LyricFetchResult, error) {
	if c.lrcErr != nil {
		return nil, c.lrcErr
	}
	return c.lrc, nil
}
func (c *hintClient) FetchCoverViaCDP(string) (string, error) { return "", nil }
func (c *hintClient) ExtractTlyricLines() map[int]string      { return nil }

func runHintSession(t *testing.T, c *hintClient) []player.Event {
	t.Helper()
	var evs []player.Event
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 100)
		c.t0 = time.Now()
		done := make(chan struct{})
		got := make(chan []player.Event)
		go func() {
			var out []player.Event
			for {
				select {
				case e := <-p.EventCh:
					out = append(out, e)
				case <-done:
					got <- out
					return
				}
			}
		}()
		p.runSession(c)
		synctest.Wait()
		close(done)
		evs = <-got
	})
	return evs
}

func hintSongSnapshot(lyrics []cdp.ExtractedLyric) *cdp.ExtractionData {
	d := newSnapshot("Go Time", "Mark Petrie")
	d.CurPlaying.ID = "1001"
	d.CurPlaying.Track.Duration = 200000
	d.PlayingState = 2
	d.DomTimeSec = -1
	d.Lyrics = lyrics
	return d
}

func TestPureMusicHintSessionPaths(t *testing.T) {
	const (
		kuwoPureHint = "此歌曲为没有填词的纯音乐，请您欣赏"
		kuwoGaiHint  = "该歌曲为纯音乐请欣赏"
	)

	cases := []struct {
		name          string
		viaRedux      bool
		cdpLRC        string
		reduxLyrics   []cdp.ExtractedLyric
		wantNoLyrics  bool
		wantHint      string
		wantAllLyrics int
	}{
		{
			name:          "换歌拍 CDP：kuwo 形状单行提示",
			cdpLRC:        "[00:00.00]" + kuwoPureHint + "\n",
			wantNoLyrics:  true,
			wantHint:      kuwoPureHint,
			wantAllLyrics: 0,
		},
		{
			name:          "换歌拍 CDP：歌名行加提示取第 1 行原文",
			cdpLRC:        "[00:00.00]Go Time-Mark Petrie\n[00:00.50]" + kuwoGaiHint + "\n",
			wantNoLyrics:  true,
			wantHint:      kuwoGaiHint,
			wantAllLyrics: 0,
		},
		{
			name:          "换歌拍 CDP 反例：首行与标题不同按普通歌词",
			cdpLRC:        "[00:00.00]Other Song-Mark Petrie\n[00:00.50]" + kuwoGaiHint + "\n",
			wantNoLyrics:  false,
			wantAllLyrics: 2,
		},
		{
			name:     "Redux 晚到：kuwo 形状单行提示",
			viaRedux: true,
			reduxLyrics: []cdp.ExtractedLyric{
				{Index: 0, Time: 0, Text: kuwoPureHint},
			},
			wantNoLyrics:  true,
			wantHint:      kuwoPureHint,
			wantAllLyrics: 0,
		},
		{
			name:     "Redux 晚到：歌名行加提示取第 1 行原文",
			viaRedux: true,
			reduxLyrics: []cdp.ExtractedLyric{
				{Index: 0, Time: 0, Text: "Go Time-Mark Petrie"},
				{Index: 1, Time: 0.5, Text: kuwoGaiHint},
			},
			wantNoLyrics:  true,
			wantHint:      kuwoGaiHint,
			wantAllLyrics: 0,
		},
		{
			name:     "Redux 晚到反例：首行与标题不同按普通歌词",
			viaRedux: true,
			reduxLyrics: []cdp.ExtractedLyric{
				{Index: 0, Time: 0, Text: "Other Song-Mark Petrie"},
				{Index: 1, Time: 0.5, Text: kuwoGaiHint},
			},
			wantNoLyrics:  false,
			wantAllLyrics: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &hintClient{end: 2500 * time.Millisecond}
			if c.viaRedux {
				cli.lrcErr = errors.New("cdp lyrics unavailable")
				cli.snap = func(now time.Duration) *cdp.ExtractionData {
					if now < 1600*time.Millisecond {
						return hintSongSnapshot(nil)
					}
					return hintSongSnapshot(c.reduxLyrics)
				}
			} else {
				cli.lrc = &cdp.LyricFetchResult{Lrc: c.cdpLRC}
				cli.snap = func(time.Duration) *cdp.ExtractionData {
					return hintSongSnapshot(nil)
				}
			}

			evs := runHintSession(t, cli)
			var all []*player.AllLyricsData
			var updates []*player.LyricUpdate
			for _, e := range evs {
				switch d := e.Data.(type) {
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
					t.Fatalf("提示语应仅发 1 条 index:-1 的 lyric_update（不得继续推出歌词行），实得 %+v", updates)
				}
				u := updates[0]
				if u.Index != -1 || u.Text != c.wantHint {
					t.Fatalf("lyric_update = {Index:%d Text:%q}，期望 {Index:-1 Text:%q}", u.Index, u.Text, c.wantHint)
				}
			} else {
				for _, u := range updates {
					if u.Index == -1 {
						t.Fatalf("非提示语不应发 index:-1，实得 %+v", u)
					}
				}
			}
		})
	}
}
