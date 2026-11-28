package qqmusic

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
	"Metabox-Nexus-PlayerCap/rum"
)

type qqRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type qqRUMEvent struct {
	Event string
	Props map[string]any
}

type qqRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []qqRUMEvent
}

func startQQRUMCollector(t *testing.T) *qqRUMCollector {
	t.Helper()
	c := &qqRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed qqRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, qqRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_qqmusic", c.srv.URL)
	t.Cleanup(restore)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: c.srv.Client(),
	})
	rum.Start()
	return c
}

func (c *qqRUMCollector) flushAndRead() (string, []qqRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]qqRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertQQNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret QQ Song",
		"Alice QQ Singer",
		"Secret QQ Lyric Body",
		"C:\\Users\\Alice\\Music\\qq_secret.flac",
		"Alice",
		"qq_secret.flac",
		"secret_qq_cover.jpg",
		"y.gtimg.cn",
		"秘密の歌詞",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

// TestQQRUMSessionBlockedCompatAndTracks 验证 QQ 音乐会话、受阻边沿、兼容信号、识曲结算与曲中刷新埋点及零隐私泄露。
func TestQQRUMSessionBlockedCompatAndTracks(t *testing.T) {
	col := startQQRUMCollector(t)

	// 1. 受阻与恢复边沿：no_admin 与 cdp_port_not_ready
	var bt qqBlockTracker
	bt.onBlocked(player.ReasonNoAdmin)
	bt.onBlocked(player.ReasonNoAdmin) // 去重
	bt.onRecovered()
	bt.onBlocked("cdp_port_not_ready")
	bt.onRecovered()

	// 2. 会话、版本回退、play_state_mismatch、track_recognized、track_lyric_updated
	beforeTotal := player.TracksRecognizedTotal()

	sess := newQQSessionRUM("22.99", "22.82", 18)
	sess.onPlayStateMismatch("stalled")
	sess.onPlayStateMismatch("stalled") // 同会话去重
	sess.onPlayStateMismatch("moved")

	// 触发一次未知版本回退信号
	emitQQVersionFallbackRUM("unknown", "22.82")

	// 触发一次完整能力曲目（含逐字、翻译、音译、逐字音译、振假名 + 补读决策 + 异步封面结果）
	richLines := []lyricLine{
		{
			Index:    0,
			Time:     1.0,
			Text:     "Secret QQ Lyric Body",
			SubText:  "Secret Translation",
			RomaText: "secret roma",
			Detailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				Words:     []player.LyricTextDetailedWord{{Timestamp: 1.0, Text: "Secret"}},
			},
			RomaDetailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				Words:     []player.LyricTextDetailedWord{{Timestamp: 1.0, Text: "secret"}},
			},
			Furigana: player.LyricFurigana{{Text: "秘密", Ruby: "ひみつ"}},
		},
	}
	sess.beginTrack(1, "native_state", "song_id")
	sess.noteSupplement("Secret QQ Song", "", "Secret QQ Song", "Alice QQ Singer")
	sess.bindCoverGen(1, 101, true)
	sess.onTrackLyricSettled(1, srcOnline, richLines, false, "", false, 35)
	// 模拟 startCover 异步回传封面规范化结果后完成 track_recognized 结算
	sess.onCoverOutcome(101, player.CoverOutcome{
		Action:     player.CoverActionPassthrough800,
		DurationMs: 11,
	})
	// 重复回传同一 coverGen 不得重发 track_recognized
	sess.onCoverOutcome(101, player.CoverOutcome{
		Action:     player.CoverActionPassthrough800,
		DurationMs: 11,
	})

	// 曲中刷新：reopen_r1 / reopen_r2 / reopen_r3 / assoc_changed / offset_changed / local_window_resolved / local_window_timeout_1s
	var st lyricState
	st.reset(1, true, time.Now().Add(time.Second))
	sess.onReopen(true, &st)
	sess.notePendingFallback(true, "reopen_r2")
	sess.onReopen(false, &st)
	sess.notePendingFallback(true, "reopen_r3")
	sess.onReopen(false, &st)

	// 模拟未决窗落定与超时、改关联、改偏移
	var contentA, contentB [32]byte
	contentA[0] = 1
	contentB[0] = 2
	st.adopted = true
	st.shown = lyricPrint{src: srcLocal, lyricOffsetMs: 500, content: contentA}
	sess.onDrainChanged(1, srcOnline, 0, [32]byte{}, true, false, &st, richLines) // local_window_resolved
	sess.onDrainChanged(1, srcOnline, 0, [32]byte{}, true, true, &st, nil)        // local_window_timeout_1s
	sess.onDrainChanged(1, srcLocal, 0, contentA, false, false, &st, richLines)   // offset_changed (同源同内容仅偏移变)
	st.shown = lyricPrint{src: srcAssoc, lyricOffsetMs: 500, content: contentB}
	sess.onDrainChanged(1, srcLocal, 500, contentA, false, false, &st, richLines) // assoc_changed

	// 第 2 首：纯音乐无封面，直接结算
	sess.beginTrack(2, "stall", "song_mid")
	sess.bindCoverGen(2, 102, false)
	sess.onTrackLyricSettled(2, srcOnline, nil, false, "纯音乐，请欣赏", false, 15)

	sess.end("process_exited")

	if delta := player.TracksRecognizedTotal() - beforeTotal; delta != 2 {
		t.Fatalf("TracksRecognizedTotal 增量 = %d，期望 2", delta)
	}

	// 3. QRC 解密失败（main/trans/roma）与 [kana:] 振假名守恒失败/超预算
	badCipherHex := strings.Repeat("deadbeef", 16) // 64 字节（8 字节块对齐）无效密文 -> 3DES 解后触发 zlib invalid header
	attachTrans(richLines, badCipherHex, 1)        // -> qrc_decrypt_failed(trans)
	attachRoma(richLines, badCipherHex, 1)         // -> qrc_decrypt_failed(roma)
	emitQQQRCDecryptFailed("main")

	// [kana:] 全曲守恒失配（目标汉字 2 个，但 kana 只有 1 条）
	parseLRC("[kana:1ひ]\n[00:01.00]秘密の歌詞", 10000)
	// [kana:] 超 256 KiB 预算
	hugeKana := "[kana:" + strings.Repeat("1あ", (maxKanaTagBytes/4)+100) + "]\n[00:01.00]秘密"
	parseLRC(hugeKana, 10000)

	// 4. switchHold 400ms 拦截与放行
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var h switchHold
	h.step(80000, 0, t0)
	h.drop(t0.Add(150 * time.Millisecond)) // intercepted
	h.step(80000, 0, t0)
	h.step(80000, 0, t0.Add(420*time.Millisecond)) // released

	raw, evs := col.flushAndRead()
	assertQQNoSecrets(t, raw)

	blockedReasons := map[string]int{}
	recoveredReasons := map[string]int{}
	compatKeys := map[string]int{}
	updateReasons := map[string]int{}
	guardOutcomes := map[string]int{}
	var tracks []map[string]any
	var attached, detached map[string]any

	for _, ev := range evs {
		if ev.Props["player"] != PlayerName {
			continue
		}
		switch ev.Event {
		case "player_session":
			if ev.Props["action"] == "attached" {
				attached = ev.Props
			} else if ev.Props["action"] == "detached" {
				detached = ev.Props
			}
		case "player_blocked":
			r, _ := ev.Props["reason"].(string)
			if ev.Props["phase"] == "blocked" {
				blockedReasons[r]++
			} else if ev.Props["phase"] == "recovered" {
				recoveredReasons[r]++
			}
		case "player_compat_signal":
			sig, _ := ev.Props["signal"].(string)
			det, _ := ev.Props["detail_code"].(string)
			compatKeys[sig+":"+det]++
		case "track_recognized":
			tracks = append(tracks, ev.Props)
		case "track_lyric_updated":
			r, _ := ev.Props["reason"].(string)
			updateReasons[r]++
		case "playback_clock_guard":
			if ev.Props["guard_kind"] == "switch_hold_400ms" {
				o, _ := ev.Props["outcome"].(string)
				guardOutcomes[o]++
			}
		}
	}

	if attached == nil || detached == nil || attached["player_version"] != "22.99" || detached["tracks_played"] != float64(2) {
		t.Errorf("player_session 异常：attached=%#v detached=%#v", attached, detached)
	}
	if blockedReasons["no_admin"] != 1 || recoveredReasons["no_admin"] != 1 ||
		blockedReasons["cdp_port_not_ready"] != 1 || recoveredReasons["cdp_port_not_ready"] != 1 {
		t.Errorf("player_blocked 边沿异常：blocked=%v recovered=%v", blockedReasons, recoveredReasons)
	}
	for _, k := range []string{
		"nearest_version_fallback:",
		"unknown_version_fallback:",
		"play_state_mismatch:stalled",
		"play_state_mismatch:moved",
		"qrc_decrypt_failed:main",
		"qrc_decrypt_failed:trans",
		"qrc_decrypt_failed:roma",
		"furigana_conservation_dropped:",
		"furigana_budget_exceeded:",
	} {
		if compatKeys[k] != 1 {
			t.Errorf("player_compat_signal[%s] = %d，期望 1（实得 %v）", k, compatKeys[k], compatKeys)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("track_recognized 条数 = %d，期望 2", len(tracks))
	}
	tr0 := tracks[0]
	if tr0["clock_mode"] != "native_state" ||
		tr0["id_source"] != "song_id" ||
		tr0["lyric_source"] != "online" ||
		tr0["lyric_format"] != "qrc" ||
		tr0["line_count"] != float64(1) ||
		tr0["has_verbatim"] != true ||
		tr0["has_trans"] != true ||
		tr0["has_roma"] != true ||
		tr0["has_roma_detailed"] != true ||
		tr0["has_furigana"] != true ||
		tr0["is_pure_music"] != false ||
		tr0["cover_action"] != player.CoverActionPassthrough800 ||
		tr0["supplement_decision"] != "filled_empty" {
		t.Errorf("第 1 首 track_recognized 属性异常：%#v", tr0)
	}
	tr1 := tracks[1]
	if tr1["is_pure_music"] != true || tr1["line_count"] != float64(0) || tr1["cover_action"] != player.CoverActionNone {
		t.Errorf("第 2 首纯音乐 track_recognized 属性异常：%#v", tr1)
	}
	for _, r := range []string{
		"reopen_r1", "reopen_r2", "reopen_r3",
		"local_window_resolved", "local_window_timeout_1s",
		"offset_changed", "assoc_changed",
	} {
		if updateReasons[r] != 1 {
			t.Errorf("track_lyric_updated[%s] = %d，期望 1（实得 %v）", r, updateReasons[r], updateReasons)
		}
	}
	if guardOutcomes["intercepted"] != 1 || guardOutcomes["released"] != 1 {
		t.Errorf("switch_hold_400ms outcome 异常：%v", guardOutcomes)
	}
}

// TestQQRUMAssocWorkerScansAndCompatSignals 验证 assocWorker 触发的 .dat 停用、panic、RM 致命失败
// 以及 1.5GiB 歌曲记录扫描与 RM 本地文件认定 scan_perf_sample 埋点（零路径泄露）。
func TestQQRUMAssocWorkerScansAndCompatSignals(t *testing.T) {
	col := startQQRUMCollector(t)

	origReport := reportOnce
	reportOnce = func(string, string, map[string]string, map[string]any) {}
	t.Cleanup(func() { reportOnce = origReport })

	// 1. RM 本地文件认定成功 + 致命失败
	secretPath := `C:\Users\Alice\Music\qq_secret.flac`
	env := newReaderEnv()
	env.fs.put(secretPath, []byte("audio"))
	ctx := context.Background()
	r := &assocReader{results: make(chan assocResult, 2)}
	w := &assocWorker{
		ctx: ctx,
		r:   r,
		d: assocDeps{
			qqPID:     1234,
			qqVersion: "22.82",
			now:       time.Now,
			stat:      env.fs.stat,
			holders: func(...string) ([]uint32, error) {
				return []uint32{1234}, nil
			},
		},
		tableOK: true,
		entries: []assoc.Entry{{Kind: assoc.KindLocal, SongURL: secretPath, LyricURL: secretPath + ".lrc"}},
		cur:     songKey{gen: 1, name: "Secret QQ Song"},
		hasSong: true,
	}
	if !w.attemptLocal() {
		t.Fatal("前置：首次 attemptLocal 应认定成功")
	}

	// 致命 RM 错误 -> rm_local_disabled + scan_perf_sample(verdict=fatal_err)
	w.localPath = ""
	w.d.holders = func(...string) ([]uint32, error) {
		return nil, fmt.Errorf("RmStartSession failed: %w", syscall.Errno(121))
	}
	if w.attemptLocal() {
		t.Fatal("前置：致命 RM 错误不应认定成功")
	}
	if !w.localOff {
		t.Fatal("致命 RM 错误应置 localOff=true")
	}

	// 2. .dat 关联表两次相同结构错误 -> assoc_dat_disabled
	w.badStat = fileStat{ok: true, size: 64}
	w.badMsg = assoc.ErrStructure.Error()
	w.parseFailed(fileStat{ok: true, size: 64}, assoc.ErrStructure)

	// 3. recoverStop -> assoc_reader_panic
	func() {
		defer w.recoverStop()
		panic("boom in C:\\Users\\Alice\\Music\\qq_secret.flac")
	}()

	// 4. 歌曲记录扫描样本上报（经辅助函数覆盖 capped/bytes_scanned_kb/verdict）
	emitQQSongRecordScanSample(recordScanStats{
		read:    12 * 1024 * 1024,
		capped:  true,
		elapsed: 85 * time.Millisecond,
	}, assoc.RecordIncomplete)

	raw, evs := col.flushAndRead()
	assertQQNoSecrets(t, raw)

	compat := map[string]int{}
	var songRecScan map[string]any
	rmVerdicts := map[string]int{}
	for _, ev := range evs {
		switch ev.Event {
		case "player_compat_signal":
			sig, _ := ev.Props["signal"].(string)
			compat[sig]++
		case "scan_perf_sample":
			switch ev.Props["scan_kind"] {
			case "qq_song_record":
				songRecScan = ev.Props
			case "qq_rm_local":
				v, _ := ev.Props["verdict"].(string)
				rmVerdicts[v]++
			}
		}
	}
	for _, sig := range []string{"rm_local_disabled", "assoc_dat_disabled", "assoc_reader_panic"} {
		if compat[sig] < 1 {
			t.Errorf("缺少 player_compat_signal[%s]，实得 %v", sig, compat)
		}
	}
	if songRecScan == nil || songRecScan["bytes_scanned_kb"] != float64(12*1024) || songRecScan["capped"] != true || songRecScan["verdict"] != "incomplete" {
		t.Errorf("qq_song_record 样本异常：%#v", songRecScan)
	}
	if rmVerdicts["found"] != 1 || rmVerdicts["fatal_err"] != 1 {
		t.Errorf("qq_rm_local verdict 异常：%v", rmVerdicts)
	}
}

// TestQQFlushRUMSettlesPendingAndIdempotent 验证 QQMusicPlayer 实现 player.RUMFlusher，
// 退出冲刷时先结清待报曲目并递增 TracksRecognizedTotal，再发单次 detached(disconnect_cause="stopped")。
func TestQQFlushRUMSettlesPendingAndIdempotent(t *testing.T) {
	var _ player.RUMFlusher = (*QQMusicPlayer)(nil)
	col := startQQRUMCollector(t)

	before := player.TracksRecognizedTotal()
	p := New(0, 50)
	sess := p.startSessionRUM("22.82", "", 15)

	sess.beginTrack(1, "native_state", "song_id")
	sess.bindCoverGen(1, 201, true) // 封面仍在飞，曲目尚未结算

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.FlushRUM()
		}()
	}
	wg.Wait()
	// 模拟 runSession 后续退出 defer，不得重复上报
	sess.endWithStopCh(p.StopCh, "process_exited")
	p.FlushRUM()

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("FlushRUM 应结清 1 首待报曲目，实得增量 %d", delta)
	}

	raw, evs := col.flushAndRead()
	assertQQNoSecrets(t, raw)

	var trackIdx, detachedIdx = -1, -1
	var detachedCnt int
	var detachedProps map[string]any
	for i, ev := range evs {
		if ev.Event == "track_recognized" {
			trackIdx = i
		}
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detachedCnt++
			detachedIdx = i
			detachedProps = ev.Props
		}
	}
	if detachedCnt != 1 || trackIdx < 0 || trackIdx > detachedIdx {
		t.Fatalf("FlushRUM 应先发 track_recognized(%d) 再发唯一 detached(%d, cnt=%d)", trackIdx, detachedIdx, detachedCnt)
	}
	if detachedProps["disconnect_cause"] != "stopped" || detachedProps["tracks_played"] != float64(1) {
		t.Errorf("detached 属性异常：%#v", detachedProps)
	}
}

// TestQQRUMLocalSongWithoutOnlineIDsSettlesOnFirstLocalLyrics 回归 Finding 2：
// 无在线编号本地歌按 srcNone 初始化后，首次经 drainAssoc 取得有效 srcLocal 歌词即刻结算 track_recognized，
// 不误发 assoc_changed；后续二次换关联才发 assoc_changed。
func TestQQRUMLocalSongWithoutOnlineIDsSettlesOnFirstLocalLyrics(t *testing.T) {
	col := startQQRUMCollector(t)
	before := player.TracksRecognizedTotal()

	sess := newQQSessionRUM("22.82", "", 10)
	sess.beginTrack(1, "stall", "none")
	sess.bindCoverGen(1, 301, false)

	localLines := []lyricLine{{Index: 0, Time: 1.0, Text: "Secret QQ Local Lyric"}}
	var st lyricState
	st.reset(1, false, time.Time{})
	prevSrc, prevShift, prevContent, prevUndecided := st.source(), st.shown.lyricOffsetMs, st.shown.content, st.undecided()
	if prevUndecided {
		t.Fatal("前置：无在线编号本地歌初始 undecided 应为 false")
	}

	var d1, d2 [32]byte
	d1[0] = 1
	d2[0] = 2
	shown1, changed1 := st.apply(assocResult{
		kind:   resDesired,
		gen:    1,
		source: srcLocal,
		lines:  localLines,
		digest: d1,
	}, false, 0)
	if !changed1 {
		t.Fatal("前置：首次应用 srcLocal 歌词应 changed=true")
	}
	sess.onDrainChanged(1, prevSrc, prevShift, prevContent, prevUndecided, false, &st, shown1)

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("无在线编号本地歌首轮取到 srcLocal 应立即计入识曲，实得增量 %d", delta)
	}

	raw1, evs1 := col.flushAndRead()
	assertQQNoSecrets(t, raw1)
	var tracks1, assocUpdates1 int
	var trackProps map[string]any
	for _, ev := range evs1 {
		switch ev.Event {
		case "track_recognized":
			tracks1++
			trackProps = ev.Props
		case "track_lyric_updated":
			if ev.Props["reason"] == "assoc_changed" {
				assocUpdates1++
			}
		}
	}
	if tracks1 != 1 || trackProps["lyric_source"] != "local_assoc_db" {
		t.Fatalf("首轮 srcLocal 应产生 1 条 local_assoc_db track_recognized，实得 cnt=%d props=%#v", tracks1, trackProps)
	}
	if assocUpdates1 != 0 {
		t.Errorf("首轮 srcLocal 结算不应误发 assoc_changed，实得 %d", assocUpdates1)
	}

	// 后续二次换关联才应发出 assoc_changed，且不重复发 track_recognized
	prevSrc2, prevShift2, prevContent2, prevUndecided2 := st.source(), st.shown.lyricOffsetMs, st.shown.content, st.undecided()
	shown2, changed2 := st.apply(assocResult{
		kind:   resDesired,
		gen:    1,
		source: srcAssoc,
		lines:  []lyricLine{{Index: 0, Time: 2.0, Text: "Secret QQ Second Assoc"}},
		digest: d2,
	}, false, 0)
	if !changed2 {
		t.Fatal("前置：二次换关联应 changed=true")
	}
	sess.onDrainChanged(1, prevSrc2, prevShift2, prevContent2, prevUndecided2, false, &st, shown2)

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("二次换关联不应重复递增识曲计数，实得增量 %d", delta)
	}
	raw2, evs2 := col.flushAndRead()
	assertQQNoSecrets(t, raw2)
	var tracks2, assocUpdates2 int
	for _, ev := range evs2 {
		switch ev.Event {
		case "track_recognized":
			tracks2++
		case "track_lyric_updated":
			if ev.Props["reason"] == "assoc_changed" {
				assocUpdates2++
			}
		}
	}
	if tracks2 != 1 || assocUpdates2 != 1 {
		t.Errorf("二次换关联后期望 track_recognized=1 assoc_changed=1，实得 %d / %d", tracks2, assocUpdates2)
	}
}

// TestQQRUMBackfillCoverGenRejectsStaleOldCover 回归 Finding 3：
// 补取切新 coverGen 后立即作废旧封面统计代次，旧封面回传不得提前错误结算，最终以新封面结果结算。
func TestQQRUMBackfillCoverGenRejectsStaleOldCover(t *testing.T) {
	col := startQQRUMCollector(t)
	before := player.TracksRecognizedTotal()

	p := New(0, 50)
	sess := p.startSessionRUM("22.82", "", 10)

	// 1. 认领启动旧封面下载并在飞阻塞
	sess.beginTrack(1, "native_state", "none")
	oldGen := p.NewSongGen()
	sess.bindCoverGen(1, oldGen, true)

	oldGate := make(chan struct{})
	oldDone := p.startCover(qqCoverOps{
		downloadWithOutcome: func(string) (string, player.CoverOutcome) {
			<-oldGate
			return "data:image/jpeg;base64,OLD", player.CoverOutcome{
				Action:     player.CoverActionPassthrough800,
				DurationMs: 12,
			}
		},
	}, oldGen, "https://example.invalid/old_secret.jpg", "Secret QQ Song", "Alice QQ Singer", "Secret QQ Song - Alice QQ Singer")

	// 2. 补取成功：立即产出新 coverGen 并更新 RUM 期望 coverGen（早于同步 fetchCoverURL）
	newGen := p.NewSongGen()
	sess.bindCoverGen(1, newGen, true)
	lines := []lyricLine{{Index: 0, Time: 1.0, Text: "Secret QQ Backfill Lyric"}}
	sess.onTrackLyricSettled(1, srcOnline, lines, false, "", true, 30)

	// 3. 此时旧封面下载完成回传：必须按代次被拒绝，绝不能提前把新歌结算成旧封面结果
	close(oldGate)
	<-oldDone
	if delta := player.TracksRecognizedTotal() - before; delta != 0 {
		t.Fatalf("旧 coverGen 回传必须被拒绝，但 TracksRecognizedTotal 已增至 %d", delta)
	}

	// 4. 随后新封面下载失败回传：按新封面 outcome(err) 结算
	newDone := p.startCover(qqCoverOps{
		downloadWithOutcome: func(string) (string, player.CoverOutcome) {
			return "", player.CoverOutcome{
				Action:         player.CoverActionErr,
				FallbackReason: "http_err",
				DurationMs:     45,
			}
		},
	}, newGen, "https://example.invalid/new_secret.jpg", "Secret QQ Song", "Alice QQ Singer", "Secret QQ Song - Alice QQ Singer")
	<-newDone

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("新封面完成后应结算 1 首，实得增量 %d", delta)
	}

	// 5. 若新封面地址为空（经 bindCoverGen(gen, newGen, false) 解除等待），同样立即结算 cover_action=none
	sess.beginTrack(2, "native_state", "song_id")
	genEmpty := p.NewSongGen()
	sess.bindCoverGen(2, genEmpty, true)
	sess.onTrackLyricSettled(2, srcOnline, lines, false, "", true, 15)
	sess.bindCoverGen(2, genEmpty, false)

	if delta := player.TracksRecognizedTotal() - before; delta != 2 {
		t.Fatalf("无新封面地址解除等待后应结算第 2 首，实得增量 %d", delta)
	}

	raw, evs := col.flushAndRead()
	assertQQNoSecrets(t, raw)

	var tracks []map[string]any
	for _, ev := range evs {
		if ev.Event == "track_recognized" {
			tracks = append(tracks, ev.Props)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("期望 2 条 track_recognized，实得 %d", len(tracks))
	}
	if tracks[0]["cover_action"] != player.CoverActionErr || tracks[0]["cover_fallback_reason"] != "http_err" {
		t.Errorf("第 1 首应记录新封面失败结果 err/http_err，实得 %#v", tracks[0])
	}
	if tracks[1]["cover_action"] != player.CoverActionNone {
		t.Errorf("第 2 首无新封面应记录 cover_action=none，实得 %#v", tracks[1])
	}

	// 6. AST 核验 qqmusic.go 补取成功分支在 NewSongGen 之后、onTrackLyricSettled 与 pendingCover.schedule 之前
	// 立即调用 rumSess.bindCoverGen(assocKey.gen, coverGen, true) 作废旧封面统计代次。
	anchors, fail := findQQSessionAnchors(readQQMusicSource(t))
	if fail != "" {
		t.Fatalf("解析 qqmusic.go 锚点失败：%s", fail)
	}
	var genPos, bindPos, settlePos, schedPos token.Pos
	ast.Inspect(anchors.success.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch types.ExprString(c.Fun) {
		case "p.NewSongGen":
			if genPos == 0 {
				genPos = c.Pos()
			}
		case "rumSess.bindCoverGen":
			if bindPos == 0 && len(c.Args) == 3 &&
				types.ExprString(c.Args[0]) == "assocKey.gen" &&
				types.ExprString(c.Args[1]) == "coverGen" &&
				types.ExprString(c.Args[2]) == "true" {
				bindPos = c.Pos()
			}
		case "rumSess.onTrackLyricSettled":
			if settlePos == 0 {
				settlePos = c.Pos()
			}
		case "pendingCover.schedule":
			if schedPos == 0 {
				schedPos = c.Pos()
			}
		}
		return true
	})
	if bindPos == 0 || genPos == 0 || settlePos == 0 || schedPos == 0 ||
		!(genPos < bindPos && bindPos < settlePos && bindPos < schedPos) {
		t.Fatalf("补取分支接线次序异常：NewSongGen(%d) < bindCoverGen(%d) < onTrackLyricSettled(%d) < pendingCover.schedule(%d)",
			genPos, bindPos, settlePos, schedPos)
	}
}

// TestQQBlockTrackerClearsOnNotRunning 验证进程未运行清零阻断计时：
// no_admin 阻断 -> 进程未运行 -> 再次 no_admin 阻断 -> 恢复，产出 2 条 blocked 且 blocked_sec 仅计第二次阻断时长。
func TestQQBlockTrackerClearsOnNotRunning(t *testing.T) {
	col := startQQRUMCollector(t)

	cur := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	bt := qqBlockTracker{now: func() time.Time { return cur }}
	bt.onBlocked(player.ReasonNoAdmin)
	cur = cur.Add(5 * time.Second)
	bt.onNotRunning()
	cur = cur.Add(60 * time.Second)
	bt.onBlocked(player.ReasonNoAdmin)
	cur = cur.Add(3 * time.Second)
	bt.onRecovered()

	_, evs := col.flushAndRead()
	var blockedCnt int
	var recovered []map[string]any
	for _, ev := range evs {
		if ev.Event == "player_blocked" && ev.Props["player"] == PlayerName {
			switch ev.Props["phase"] {
			case "blocked":
				blockedCnt++
			case "recovered":
				recovered = append(recovered, ev.Props)
			}
		}
	}
	if blockedCnt != 2 {
		t.Fatalf("跨未运行间隔再次阻断应产生 2 条 blocked，实得 %d", blockedCnt)
	}
	if len(recovered) != 1 || recovered[0]["blocked_sec"] != float64(3) {
		t.Fatalf("recovered 应仅计第二次阻断 3s，实得 %#v", recovered)
	}
}

// TestQQFlushRUMWaitsForInFlightTrackAndDetachBarrier 回归 code-t4-r2 Finding 2：
// 并发退出时，FlushRUM 必须等待在途 onCoverOutcome/emitQQTrackRecognized 与竞争方 end 完成入队后再返回。
func TestQQFlushRUMWaitsForInFlightTrackAndDetachBarrier(t *testing.T) {
	col := startQQRUMCollector(t)

	t.Run("等待在途track_recognized完成", func(t *testing.T) {
		before := player.TracksRecognizedTotal()
		p := New(0, 50)
		sess := p.startSessionRUM("22.82", "", 10)
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		sess.mu.Lock()
		sess.testBeforeTrackEmit = func() {
			close(inHook)
			<-releaseHook
		}
		sess.mu.Unlock()

		sess.beginTrack(1, "native_state", "song_id")
		sess.bindCoverGen(1, 501, true)
		sess.onTrackLyricSettled(1, srcOnline, []lyricLine{{Index: 0, Time: 1, Text: "Secret QQ Line"}}, false, "", false, 15)

		coverDone := make(chan struct{})
		go func() {
			defer close(coverDone)
			sess.onCoverOutcome(501, player.CoverOutcome{Action: player.CoverActionPassthrough800, DurationMs: 9})
		}()
		<-inHook

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("onCoverOutcome 尚未完成 NoteTrackRecognized/rum.Track，FlushRUM 不得提前返回")
		case <-time.After(30 * time.Millisecond):
		}

		close(releaseHook)
		<-flushDone
		<-coverDone
		if delta := player.TracksRecognizedTotal() - before; delta != 1 {
			t.Fatalf("FlushRUM 返回时 TracksRecognizedTotal 增量应已为 1，实得 %d", delta)
		}
	})

	t.Run("等待竞争方detached完成", func(t *testing.T) {
		p := New(0, 50)
		sess := p.startSessionRUM("22.82", "", 10)
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		sess.mu.Lock()
		sess.testBeforeDetachEmit = func() {
			close(inHook)
			<-releaseHook
		}
		sess.mu.Unlock()

		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			p.endSessionRUM(sess, "process_exited")
		}()
		<-inHook

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("竞争方尚未发出 detached，FlushRUM 不得提前返回")
		case <-time.After(30 * time.Millisecond):
		}

		close(releaseHook)
		<-flushDone
		<-endDone
	})

	raw, evs := col.flushAndRead()
	assertQQNoSecrets(t, raw)
	var detachedCnt int
	for _, ev := range evs {
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detachedCnt++
		}
	}
	if detachedCnt != 2 {
		t.Fatalf("两个子场景各应产生恰好 1 条 detached（共 2 条），实得 %d", detachedCnt)
	}
}

// TestQQRUMGateRejectsPostFlushAndWaitsAttach 回归测试：
// FlushRUM 置位后拒绝新会话（连带 version fallback 兼容信号亦不孤儿发出）；
// 登记发 attached 前暂停时并发 FlushRUM 等待成对产出 attached + detached。
func TestQQRUMGateRejectsPostFlushAndWaitsAttach(t *testing.T) {
	t.Run("Flush后拒绝新会话", func(t *testing.T) {
		col := startQQRUMCollector(t)
		p := New(0, 50)
		p.FlushRUM()
		sess := p.startSessionRUM("22.99", "22.82", 12)
		if sess != nil {
			t.Fatalf("FlushRUM 后 startSessionRUM 应返回 nil，实得 %#v", sess)
		}
		sess.beginTrack(1, "native_state", "song_id")
		p.endSessionRUM(sess, "process_exited")

		_, evs := col.flushAndRead()
		for _, ev := range evs {
			if ev.Event == "player_session" || ev.Event == "track_recognized" {
				t.Fatalf("FlushRUM 后拒绝的会话不应发事件：%#v", ev)
			}
		}
	})

	t.Run("登记中并发Flush等待成对结算", func(t *testing.T) {
		col := startQQRUMCollector(t)
		p := New(0, 50)
		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		p.testBeforeAttach = func() {
			close(inAttach)
			<-releaseAttach
		}

		regDone := make(chan *qqSessionRUM, 1)
		go func() {
			regDone <- p.startSessionRUM("22.82", "", 10)
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
		case <-time.After(35 * time.Millisecond):
		}

		close(releaseAttach)
		sess := <-regDone
		if sess == nil {
			t.Fatal("前置：并发 Flush 前已占位的 startSessionRUM 应返回非 nil")
		}
		<-flushDone
		p.endSessionRUM(sess, "process_exited")

		_, evs := col.flushAndRead()
		var actions []string
		for _, ev := range evs {
			if ev.Event == "player_session" {
				if act, ok := ev.Props["action"].(string); ok {
					actions = append(actions, act)
				}
			}
		}
		if len(actions) != 2 || actions[0] != "attached" || actions[1] != "detached" {
			t.Fatalf("期望严格成对 [attached detached]，实得 %v", actions)
		}
	})
}

// TestQQRUMInitialFetchFailIgnoresEmptyOnlineDrain 回归：
// 有在线 ID、无本地关联的歌曲首轮 fetchLRC 失败进入补取窗口时，关联读取者首次返回空 srcOnline 使 takeResult
// 返回 changed=true，onDrainChanged 不得将其误报为曲中 track_lyric_updated(assoc_changed)，也不得提前结算
// track_recognized；待补取成功落定后再发生曲中关联变更，才正常发出 assoc_changed。
func TestQQRUMInitialFetchFailIgnoresEmptyOnlineDrain(t *testing.T) {
	col := startQQRUMCollector(t)
	before := player.TracksRecognizedTotal()

	sess := newQQSessionRUM("22.82", "", 10)
	sess.beginTrack(1, "native_state", "song_id")
	sess.bindCoverGen(1, 401, false)

	// 换歌拍有在线 ID 但 fetchLRC 失败：lyricSt 已 reset 且未调用 refresh（adopted=false，prevUndecided=false），
	// 未调用 onTrackLyricSettled（needInitialSettle=true）。
	var st lyricState
	st.reset(1, true, time.Now().Add(localDecideWindow))
	st.setTitle("Secret QQ Song - Alice QQ Singer")
	prevSrc, prevShift, prevContent, prevUndecided := st.source(), st.shown.lyricOffsetMs, st.shown.content, st.undecided()
	if prevSrc != srcNone || prevUndecided {
		t.Fatalf("前置：首轮 fetchLRC 失败未 refresh 时应为 srcNone 且非未决，实得 src=%v undecided=%v", prevSrc, prevUndecided)
	}

	// 1. 关联读取者首次返回同代次空 srcOnline 结果，经 takeResult 触发 changed=true 后调用 onDrainChanged。
	ch := make(chan assocResult, 1)
	ch <- assocResult{kind: resDesired, gen: 1, source: srcOnline}
	lines1, changed1 := takeResult(&st, ch, true, 0)
	if !changed1 || len(lines1) != 0 || st.source() != srcOnline || st.undecided() {
		t.Fatalf("前置：首次空 srcOnline 应经 takeResult 触发 changed=true，实得 changed=%v lines=%d src=%v undecided=%v",
			changed1, len(lines1), st.source(), st.undecided())
	}
	sess.onDrainChanged(1, prevSrc, prevShift, prevContent, prevUndecided, false, &st, lines1)

	if delta := player.TracksRecognizedTotal() - before; delta != 0 {
		t.Fatalf("首轮仍在补取窗口中收到空 srcOnline 不得提前计入识曲，实得增量 %d", delta)
	}
	raw1, evs1 := col.flushAndRead()
	assertQQNoSecrets(t, raw1)
	var tracks1, updates1 int
	var updateReasons1 []any
	for _, ev := range evs1 {
		switch ev.Event {
		case "track_recognized":
			tracks1++
		case "track_lyric_updated":
			updates1++
			updateReasons1 = append(updateReasons1, ev.Props["reason"])
		}
	}
	if tracks1 != 0 {
		t.Fatalf("首轮未落定不得提前发出 track_recognized，实得 %d 条", tracks1)
	}
	if updates1 != 0 {
		t.Fatalf("首轮未落定收到空 srcOnline 不得发出任何 track_lyric_updated，实得 %d 条（reasons=%v）", updates1, updateReasons1)
	}

	// 2. 补取成功：更新在线原始行并调用 onTrackLyricSettled 发出首轮 track_recognized。
	onlineLines := []lyricLine{{Index: 0, Time: 1.0, Text: "Secret QQ Lyric Body"}}
	st.setOnlineRaw(onlineLines)
	shown, _ := st.refresh(true, 0)
	sess.onTrackLyricSettled(1, st.source(), shown, false, st.hint(), false, 40)

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("补取成功后应结算 1 首曲目，实得增量 %d", delta)
	}
	raw2, evs2 := col.flushAndRead()
	assertQQNoSecrets(t, raw2)
	var tracks2, updates2 int
	var trackProps map[string]any
	for _, ev := range evs2 {
		switch ev.Event {
		case "track_recognized":
			tracks2++
			trackProps = ev.Props
		case "track_lyric_updated":
			updates2++
		}
	}
	if tracks2 != 1 || trackProps["lyric_source"] != "online" || trackProps["line_count"] != float64(1) {
		t.Fatalf("补取成功后应产出 1 条 online track_recognized，实得 cnt=%d props=%#v", tracks2, trackProps)
	}
	if updates2 != 0 {
		t.Fatalf("补取成功首轮落定阶段不应产生 track_lyric_updated，实得 %d 条", updates2)
	}

	// 3. 首轮落定后曲中关联变更触发 onDrainChanged，正常发出 track_lyric_updated(reason="assoc_changed")。
	prevSrc2, prevShift2, prevContent2, prevUndecided2 := st.source(), st.shown.lyricOffsetMs, st.shown.content, st.undecided()
	var dAssoc [32]byte
	dAssoc[0] = 9
	ch <- assocResult{
		kind:   resDesired,
		gen:    1,
		source: srcAssoc,
		lines:  []lyricLine{{Index: 0, Time: 2.0, Text: "Secret QQ Assoc Lyric"}},
		digest: dAssoc,
	}
	lines2, changed2 := takeResult(&st, ch, true, 0)
	if !changed2 {
		t.Fatal("前置：首轮落定后曲中切换 srcAssoc 应 changed=true")
	}
	sess.onDrainChanged(1, prevSrc2, prevShift2, prevContent2, prevUndecided2, false, &st, lines2)

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("曲中关联变更不应重复递增识曲计数，实得增量 %d", delta)
	}
	raw3, evs3 := col.flushAndRead()
	assertQQNoSecrets(t, raw3)
	var tracks3, assocUpdates3, otherUpdates3 int
	for _, ev := range evs3 {
		switch ev.Event {
		case "track_recognized":
			tracks3++
		case "track_lyric_updated":
			if ev.Props["reason"] == "assoc_changed" {
				assocUpdates3++
			} else {
				otherUpdates3++
			}
		}
	}
	if tracks3 != 1 || assocUpdates3 != 1 || otherUpdates3 != 0 {
		t.Fatalf("曲中关联变更后期望 track_recognized=1 assoc_changed=1 other=0，实得 %d / %d / %d",
			tracks3, assocUpdates3, otherUpdates3)
	}
}
