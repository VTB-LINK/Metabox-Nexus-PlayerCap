package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/cdp"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
	"Metabox-Nexus-PlayerCap/player/spotify/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

type spRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type spRUMEvent struct {
	Event string
	Props map[string]any
}

type spRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []spRUMEvent
}

func startSpotifyRUMCollector(t *testing.T) *spRUMCollector {
	t.Helper()
	c := &spRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed spRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, spRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_spotify", c.srv.URL)
	t.Cleanup(restore)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: c.srv.Client(),
	})
	rum.Start()
	t.Cleanup(func() { rum.Shutdown(time.Second) })
	return c
}

func (c *spRUMCollector) flushAndRead() (string, []spRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]spRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertSpotifyNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret Spotify Song",
		"Alice Spotify Singer",
		"Secret Spotify Lyric Line",
		"C:\\Users\\Alice\\AppData\\Roaming\\Spotify\\Spotify.exe",
		"Alice",
		"i.scdn.co",
		"ab67616d0000b273secret",
		"Bearer SECRET_TOKEN_VALUE",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

type stubSpotifyLyricsClient struct {
	token     string
	pageBody  string
	pageErr   error
	cacheBody string
	cacheErr  error
}

func (s *stubSpotifyLyricsClient) Token() string       { return s.token }
func (s *stubSpotifyLyricsClient) AppPlatform() string { return "Win32_x86_64" }
func (s *stubSpotifyLyricsClient) AppVersion() string  { return "1.3.4.258" }
func (s *stubSpotifyLyricsClient) FetchLyrics(string, string) (string, error) {
	return s.pageBody, s.pageErr
}
func (s *stubSpotifyLyricsClient) FetchViaPageCache(string) (string, error) {
	return s.cacheBody, s.cacheErr
}

// TestSpotifyRUMBlockedZeroPathAndFlagAbsent 验证 store_install 零路径、spotify_flag_absent 与 onNotRunning 清零。
func TestSpotifyRUMBlockedZeroPathAndFlagAbsent(t *testing.T) {
	col := startSpotifyRUMCollector(t)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	bt := spotifyBlockTracker{now: func() time.Time { return now }}

	bt.onBlocked("store_install")
	bt.onBlocked("store_install") // 去重
	now = now.Add(30 * time.Second)
	bt.onNotRunning() // 静默清零，不发 recovered

	now = now.Add(10 * time.Second)
	bt.onBlocked("spotify_flag_absent")
	now = now.Add(7 * time.Second)
	bt.onRecovered()

	raw, evs := col.flushAndRead()
	assertSpotifyNoSecrets(t, raw)

	var storeCnt, flagBlocked, flagRec int
	for _, ev := range evs {
		if ev.Event != "player_blocked" {
			continue
		}
		if _, hasExePath := ev.Props["exe_path"]; hasExePath {
			t.Errorf("player_blocked 严禁携带 exe_path：%#v", ev.Props)
		}
		switch {
		case ev.Props["reason"] == "store_install" && ev.Props["phase"] == "blocked":
			storeCnt++
		case ev.Props["reason"] == "spotify_flag_absent" && ev.Props["phase"] == "blocked":
			flagBlocked++
		case ev.Props["reason"] == "spotify_flag_absent" && ev.Props["phase"] == "recovered":
			flagRec++
			if ev.Props["blocked_sec"] != float64(7) {
				t.Errorf("spotify_flag_absent blocked_sec 应为 7，实得 %#v", ev.Props["blocked_sec"])
			}
		}
	}
	if storeCnt != 1 || flagBlocked != 1 || flagRec != 1 {
		t.Fatalf("阻断事件计数异常：store=%d flagBlocked=%d flagRec=%d", storeCnt, flagBlocked, flagRec)
	}
}

// TestSpotifyRUMThreeTierFetchAndLateTokenRetry 验证 L1/L2/L3 取词来源标识、late_token_retry、
// 兼容信号、退出 FlushRUM 完成屏障与零敏感泄露。
func TestSpotifyRUMThreeTierFetchAndLateTokenRetry(t *testing.T) {
	col := startSpotifyRUMCollector(t)

	origDirect := fetchLyricsDirect
	t.Cleanup(func() { fetchLyricsDirect = origDirect })

	const syncedJSON = `{"lyrics":{"syncType":"LINE_SYNCED","lines":[{"startTimeMs":"1000","words":"Secret Spotify Lyric Line"}]}}`
	parsedResp, err := lyric.ParseBody([]byte(syncedJSON))
	if err != nil {
		t.Fatalf("解析样本歌词失败: %v", err)
	}

	// L1 Go 直取命中
	fetchLyricsDirect = func(string, string, string, string, time.Duration) (*lyric.Response, error) {
		return parsedResp, nil
	}
	lines1, _, _, _, src1 := fetchLyricsWithSource(context.Background(), &stubSpotifyLyricsClient{token: "Bearer SECRET_TOKEN_VALUE"}, "trk1")
	if len(lines1) == 0 || src1 != "spotify_l1_go" {
		t.Fatalf("L1 应返回 spotify_l1_go，实得 len=%d src=%q", len(lines1), src1)
	}

	// L2 页面 fetch 命中（L1 403）
	fetchLyricsDirect = func(string, string, string, string, time.Duration) (*lyric.Response, error) {
		return nil, errors.New("HTTP 403")
	}
	lines2, _, _, _, src2 := fetchLyricsWithSource(context.Background(), &stubSpotifyLyricsClient{
		token:    "Bearer SECRET_TOKEN_VALUE",
		pageBody: syncedJSON,
	}, "trk2")
	if len(lines2) == 0 || src2 != "spotify_l2_page" {
		t.Fatalf("L2 应返回 spotify_l2_page，实得 len=%d src=%q", len(lines2), src2)
	}

	// L3 页面缓存命中（无 token）
	lines3, _, _, _, src3 := fetchLyricsWithSource(context.Background(), &stubSpotifyLyricsClient{
		cacheBody: syncedJSON,
	}, "trk3")
	if len(lines3) == 0 || src3 != "spotify_l3_cache" {
		t.Fatalf("L3 应返回 spotify_l3_cache，实得 len=%d src=%q", len(lines3), src3)
	}

	// 会话级 track_recognized + late_token_retry + compat + FlushRUM 完成屏障
	beforeTotal := player.TracksRecognizedTotal()
	p := New(0, 200)
	var _ player.RUMFlusher = p

	sess := p.startSessionRUM("1.3.4.258", 28)

	sess.onCompat("unknown_play_icon", "", 0)
	sess.onCompat("cli_state_mismatch", "", 0)
	sess.onCompat("spotify_cli_error", "now_playing_err", 0)
	sess.onCompat("passive_token_captured", "", 420)
	sess.onCompat("passive_token_captured", "", 999) // 会话去重

	// 第 1 首：首轮缺 token 落定 none，随后 late_token_retry 补取命中（不得重复递增 NoteTrackRecognized）
	sess.beginTrack(1, "dom_icon")
	sess.onTrackLyricSettled(1, "none", nil, false, 60, false)
	sess.onLateTokenRetry()

	// 第 2 首：L2 命中 + 退出屏障交错验证
	inTrack := make(chan struct{})
	releaseTrack := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeTrackEmit = func() {
		close(inTrack)
		<-releaseTrack
	}
	sess.mu.Unlock()

	sess.beginTrack(2, "stall")
	go sess.onTrackLyricSettled(2, src2, lines2, false, 95, false)
	<-inTrack

	flushDone := make(chan struct{})
	go func() {
		p.FlushRUM()
		close(flushDone)
	}()
	select {
	case <-flushDone:
		t.Fatal("FlushRUM 在途 track_recognized 完成前提前返回，违背完成屏障")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseTrack)
	select {
	case <-flushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("释放屏障后 FlushRUM 超时未返回")
	}
	p.FlushRUM() // 幂等

	raw, evs := col.flushAndRead()
	assertSpotifyNoSecrets(t, raw)

	if player.TracksRecognizedTotal() < beforeTotal+2 {
		t.Fatalf("TracksRecognizedTotal 应递增 2（late_token_retry 不重复记）：before=%d after=%d", beforeTotal, player.TracksRecognizedTotal())
	}

	var sawL2, sawLateRetry, sawTokenCap, sawDetached int
	for _, ev := range evs {
		switch ev.Event {
		case "track_recognized":
			if ev.Props["lyric_source"] == "spotify_l2_page" {
				sawL2++
				if ev.Props["clock_mode"] != "stall" || ev.Props["lyric_format"] != "lrc" ||
					ev.Props["has_verbatim"] != false || ev.Props["has_trans"] != false ||
					ev.Props["has_roma"] != false || ev.Props["has_furigana"] != false {
					t.Errorf("track_recognized 属性异常：%#v", ev.Props)
				}
			}
		case "track_lyric_updated":
			if ev.Props["reason"] == "late_token_retry" {
				sawLateRetry++
			}
		case "player_compat_signal":
			if ev.Props["signal"] == "passive_token_captured" {
				sawTokenCap++
				if ev.Props["elapsed_ms"] != float64(420) {
					t.Errorf("passive_token_captured elapsed_ms 异常：%#v", ev.Props)
				}
			}
		case "player_session":
			if ev.Props["action"] == "detached" {
				sawDetached++
				if ev.Props["tracks_played"] != float64(2) || ev.Props["disconnect_cause"] != "stopped" {
					t.Errorf("detached 属性异常：%#v", ev.Props)
				}
			}
		}
	}
	if sawL2 != 1 || sawLateRetry != 1 || sawTokenCap != 1 || sawDetached != 1 {
		t.Fatalf("事件计数异常：l2=%d lateRetry=%d tokenCap=%d detached=%d", sawL2, sawLateRetry, sawTokenCap, sawDetached)
	}
}

type rumSessionStubClient struct {
	mu       sync.Mutex
	snap     *cdp.Snapshot
	closed   bool
	token    string
	pageBody string
	pageErr  error
}

func (c *rumSessionStubClient) Snapshot() (*cdp.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("closed")
	}
	cp := *c.snap
	return &cp, nil
}
func (c *rumSessionStubClient) IsClosed() bool      { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }
func (c *rumSessionStubClient) Close()              { c.mu.Lock(); c.closed = true; c.mu.Unlock() }
func (c *rumSessionStubClient) Token() string       { return c.token }
func (c *rumSessionStubClient) AppPlatform() string { return "Win32_x86_64" }
func (c *rumSessionStubClient) AppVersion() string  { return "1.3.4.258" }
func (c *rumSessionStubClient) FetchLyrics(string, string) (string, error) {
	return c.pageBody, c.pageErr
}
func (c *rumSessionStubClient) FetchViaPageCache(string) (string, error) {
	return "", errors.New("no cache")
}

// TestSpotifyRUMUnsyncedAnd404NotMarkedPureMusic 回归验证：UNSYNCED（有正文无时间轴）与 HTTP 404
// 均不得误计为 is_pure_music=true，仅明确命中纯音乐提示（s.hint != ""）才标为 is_pure_music=true。
func TestSpotifyRUMUnsyncedAnd404NotMarkedPureMusic(t *testing.T) {
	col := startSpotifyRUMCollector(t)

	origFindPID, origFindCLI, origReadNP, origDirect := findMainPID, findCLI, readNowPlaying, fetchLyricsDirect
	t.Cleanup(func() {
		findMainPID, findCLI, readNowPlaying, fetchLyricsDirect = origFindPID, origFindCLI, origReadNP, origDirect
	})
	findMainPID = func() (int, error) { return 4242, nil }
	findCLI = func() (string, error) { return `C:\fake\spotify_cli.exe`, nil }

	cases := []struct {
		name     string
		trackID  string
		direct   func() (*lyric.Response, error)
		pageErr  error
		wantPure bool
	}{
		{
			name:     "UNSYNCED_not_pure",
			trackID:  "trk_unsynced",
			direct:   func() (*lyric.Response, error) { return mustParse(t, unsyncedBody), nil },
			wantPure: false,
		},
		{
			name:     "HTTP_404_not_pure",
			trackID:  "trk_404",
			direct:   func() (*lyric.Response, error) { return nil, httpErr(404, "direct 404") },
			pageErr:  httpErr(404, "page 404"),
			wantPure: false,
		},
		{
			name:     "pure_music_hint_is_pure",
			trackID:  "trk_hint",
			direct:   func() (*lyric.Response, error) { return mustParse(t, hintBody), nil },
			wantPure: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readNowPlaying = func(string) (watchdog.NowPlaying, error) {
				return watchdog.NowPlaying{URI: "spotify:track:" + tc.trackID, IsPlaying: true, HasTrack: true}, nil
			}
			fetchLyricsDirect = func(string, string, string, string, time.Duration) (*lyric.Response, error) {
				return tc.direct()
			}

			p := New(0, 50)
			client := &rumSessionStubClient{
				snap: &cdp.Snapshot{
					OK: true, Title: "Secret Spotify Song", Artists: []string{"Alice Spotify Singer"},
					DurationMs: 200000, PositionMs: 1000, State: "playing",
				},
				token:   "Bearer SECRET_TOKEN_VALUE",
				pageErr: tc.pageErr,
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				p.runSession(client)
			}()

			// 等到首轮取词落定（发出 index:-1 的 lyric_update）再关闭会话
			deadline := time.After(3 * time.Second)
			settled := false
			for !settled {
				select {
				case evt := <-p.EventCh:
					if evt.Type == player.EventLyricUpdate {
						if u, ok := evt.Data.(*player.LyricUpdate); ok && u.Index == -1 {
							settled = true
						}
					}
				case <-deadline:
					client.Close()
					<-done
					t.Fatal("等待首轮无歌词 lyric_update(index:-1) 超时")
				}
			}
			client.Close()
			<-done

			raw, evs := col.flushAndRead()
			assertSpotifyNoSecrets(t, raw)

			var lastTrack map[string]any
			for _, ev := range evs {
				if ev.Event == "track_recognized" {
					lastTrack = ev.Props
				}
			}
			if lastTrack == nil {
				t.Fatal("未收到 track_recognized 事件")
			}
			if lastTrack["is_pure_music"] != tc.wantPure {
				t.Errorf("%s: is_pure_music = %#v，期望 %v（全量属性：%#v）", tc.name, lastTrack["is_pure_music"], tc.wantPure, lastTrack)
			}
			if lastTrack["line_count"] != float64(0) || lastTrack["lyric_format"] != "none" {
				t.Errorf("%s: line_count=%#v lyric_format=%#v，期望 0 / none", tc.name, lastTrack["line_count"], lastTrack["lyric_format"])
			}
		})
	}
}

// TestSpotifyRUMGateRejectsPostFlushAndWaitsInFlightRegister 回归测试：
// 1) FlushRUM 置位后新进入 runSession 拒绝登记新 RUM 会话（rumSess==nil 安全不 panic，零孤儿 attached/track_recognized）；
// 2) startSessionRUM 登记后、发 attached 入队前暂停时，并发 FlushRUM 等待 attachDone 后再发 detached，成对结算不丢事件。
func TestSpotifyRUMGateRejectsPostFlushAndWaitsInFlightRegister(t *testing.T) {
	t.Run("Flush后runSession拒绝新会话且nil安全", func(t *testing.T) {
		col := startSpotifyRUMCollector(t)

		origFindPID, origFindCLI, origReadNP, origDirect := findMainPID, findCLI, readNowPlaying, fetchLyricsDirect
		t.Cleanup(func() {
			findMainPID, findCLI, readNowPlaying, fetchLyricsDirect = origFindPID, origFindCLI, origReadNP, origDirect
		})
		findMainPID = func() (int, error) { return 4242, nil }
		findCLI = func() (string, error) { return `C:\fake\spotify_cli.exe`, nil }
		readNowPlaying = func(string) (watchdog.NowPlaying, error) {
			return watchdog.NowPlaying{URI: "spotify:track:trk_post_flush", IsPlaying: true, HasTrack: true}, nil
		}
		fetchLyricsDirect = func(string, string, string, string, time.Duration) (*lyric.Response, error) {
			return mustParse(t, hintBody), nil
		}

		p := New(0, 50)
		// 先触发退出结算闸门
		p.FlushRUM()

		client := &rumSessionStubClient{
			snap: &cdp.Snapshot{
				OK: true, Title: "Secret Spotify Song", Artists: []string{"Alice Spotify Singer"},
				DurationMs: 200000, PositionMs: 1000, State: "playing",
			},
			token: "Bearer SECRET_TOKEN_VALUE",
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.runSession(client)
		}()

		deadline := time.After(3 * time.Second)
		settled := false
		for !settled {
			select {
			case evt := <-p.EventCh:
				if evt.Type == player.EventLyricUpdate {
					settled = true
				}
			case <-deadline:
				client.Close()
				<-done
				t.Fatal("等待 post-flush runSession 产出事件超时")
			}
		}
		client.Close()
		<-done

		_, evs := col.flushAndRead()
		for _, ev := range evs {
			if ev.Event == "player_session" || ev.Event == "track_recognized" {
				t.Fatalf("FlushRUM 后新进入的 runSession 不应再发出任何会话/识曲事件，实得：%#v", ev)
			}
		}
	})

	t.Run("登记中并发Flush等待attached入队后成对发detached", func(t *testing.T) {
		col := startSpotifyRUMCollector(t)

		p := New(0, 50)
		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		p.testBeforeAttach = func() {
			close(inAttach)
			<-releaseAttach
		}

		regDone := make(chan *spotifySessionRUM, 1)
		go func() {
			regDone <- p.startSessionRUM("1.3.4.258", 12)
		}()
		<-inAttach

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("attached 尚未完成入队，FlushRUM 不得提前返回")
		case <-time.After(40 * time.Millisecond):
		}

		close(releaseAttach)
		sess := <-regDone
		if sess == nil {
			t.Fatal("前置：并发 Flush 前已占位的 startSessionRUM 应返回非 nil 会话")
		}
		<-flushDone
		p.endSessionRUM(sess, "cdp_closed")

		rum.Shutdown(2 * time.Second)

		col.mu.Lock()
		evs := append([]spRUMEvent(nil), col.events...)
		col.mu.Unlock()

		var actions []string
		for _, ev := range evs {
			if ev.Event == "player_session" {
				if act, ok := ev.Props["action"].(string); ok {
					actions = append(actions, act)
				}
			}
		}
		if len(actions) != 2 || actions[0] != "attached" || actions[1] != "detached" {
			t.Fatalf("期望严格成对 [attached detached]，实得 %v（全部：%#v）", actions, evs)
		}
	})
}
