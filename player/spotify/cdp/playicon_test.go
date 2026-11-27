package cdp

// 本文件只测 snapshotJS 对播放/暂停按钮图标与缺失状态的识别及 Snapshot.Playing 的映射。
//
// 变异自证（在隔离副本里逐条注入、跑完即清理，均实测变红）：
//   - CDP-M1（把 snapshotJS 改回 btn == null 也返回 'unknown'）→
//     TestSnapshotJSPlayIconStructure 及 TestSnapshotJSPlayIcon/I3_cold_start_missing_button、
//     I4a_track_present_but_missing_button 红；
//   - CDP-M2a（破坏 iconPlayingPrefix 匹配）→ TestSnapshotJSPlayIcon/I1_playing_full_svg_path 红；
//   - CDP-M2b（破坏 iconPausedPrefix 匹配）→ TestSnapshotJSPlayIcon/I2_paused_full_svg_path 红。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

const (
	// 实测 Spotify 1.3.4.258 与 1.3.10.2586 逐字节一致的双态完整 SVG path d（viewBox 0 0 16 16）。
	fullPlayingPathD = "M2.7 1a.7.7 0 0 0-.7.7v12.6a.7.7 0 0 0 .7.7h2.6a.7.7 0 0 0 .7-.7V1.7a.7.7 0 0 0-.7-.7zm8 0a.7.7 0 0 0-.7.7v12.6a.7.7 0 0 0 .7.7h2.6a.7.7 0 0 0 .7-.7V1.7a.7.7 0 0 0-.7-.7z"
	fullPausedPathD  = "M3 1.713a.7.7 0 0 1 1.05-.607l10.89 6.288a.7.7 0 0 1 0 1.212L4.05 14.894A.7.7 0 0 1 3 14.288z"
)

func TestSnapshotPlayingMapping(t *testing.T) {
	cases := []struct {
		state       string
		wantPlaying bool
		wantKnown   bool
	}{
		{"playing", true, true},
		{"paused", false, true},
		{"unknown", false, false},
		{"missing", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		s := Snapshot{State: tc.state}
		gotPlaying, gotKnown := s.Playing()
		if gotPlaying != tc.wantPlaying || gotKnown != tc.wantKnown {
			t.Errorf("Snapshot{State: %q}.Playing() = (%v, %v), want (%v, %v)",
				tc.state, gotPlaying, gotKnown, tc.wantPlaying, tc.wantKnown)
		}
	}
}

// TestSnapshotJSPlayIconStructure 在无 node 环境下兜底校验 snapshotJS 的按钮缺失与前缀结构。
func TestSnapshotJSPlayIconStructure(t *testing.T) {
	js := snapshotJS()
	for _, want := range []string{
		"var state='missing';",
		`[data-testid="control-button-playpause"]`,
		"if(btn){state='unknown';",
		"d.indexOf(" + strconv.Quote(iconPlayingPrefix) + ")===0){state='playing';}",
		"d.indexOf(" + strconv.Quote(iconPausedPrefix) + ")===0){state='paused';}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("snapshotJS() missing %q", want)
		}
	}
}

// TestSnapshotJSPlayIcon 用本机 node 在最小 DOM 桩上运行真实的 snapshotJS，覆盖 I1~I5。
func TestSnapshotJSPlayIcon(t *testing.T) {
	// buildDOMStub 构造最小 document 桩：支持播放按钮存在/缺失、无 <path>、空/任意 d，以及可选曲目信息。
	buildDOMStub := func(hasBtn, hasPath, hasD bool, pathD string, title string, durationMs int) string {
		btnExpr := "null"
		if hasBtn {
			if !hasPath {
				btnExpr = "{querySelector:function(){return null;}}"
			} else {
				dExpr := "null"
				if hasD {
					dExpr = jsQuote(pathD)
				}
				btnExpr = "{querySelector:function(s){return s==='svg path'?{getAttribute:function(a){return a==='d'?" + dExpr + ":null;}}:null;}}"
			}
		}
		widgetExpr := "null"
		if title != "" {
			widgetExpr = "{querySelector:function(s){return s==='[dir=\"auto\"]'?{textContent:" + jsQuote(title) + "}:null;}}"
		}
		inpExpr := "null"
		if durationMs > 0 {
			inpExpr = "{max:" + strconv.Itoa(durationMs) + ",value:0}"
		}
		return "globalThis.document={querySelector:function(s){" +
			"if(s==='[data-testid=\"control-button-playpause\"]')return " + btnExpr + ";" +
			"if(s==='[data-testid=\"now-playing-widget-v2\"]')return " + widgetExpr + ";" +
			"if(s==='[data-testid=\"playback-progressbar\"] input')return " + inpExpr + ";" +
			"return null;}};"
	}

	cases := []struct {
		name        string
		setup       string
		wantState   string
		wantPlaying bool
		wantKnown   bool
		wantTitle   string
		wantDurMs   float64
	}{
		{
			name:        "I1_playing_full_svg_path",
			setup:       buildDOMStub(true, true, true, fullPlayingPathD, "Song", 192965),
			wantState:   "playing",
			wantPlaying: true,
			wantKnown:   true,
			wantTitle:   "Song",
			wantDurMs:   192965,
		},
		{
			name:        "I2_paused_full_svg_path",
			setup:       buildDOMStub(true, true, true, fullPausedPathD, "Song", 192965),
			wantState:   "paused",
			wantPlaying: false,
			wantKnown:   true,
			wantTitle:   "Song",
			wantDurMs:   192965,
		},
		{
			name:        "I3_cold_start_missing_button",
			setup:       buildDOMStub(false, false, false, "", "", 0),
			wantState:   "missing",
			wantPlaying: false,
			wantKnown:   false,
		},
		{
			name:        "I4a_track_present_but_missing_button",
			setup:       buildDOMStub(false, false, false, "", "Song", 192965),
			wantState:   "missing",
			wantPlaying: false,
			wantKnown:   false,
			wantTitle:   "Song",
			wantDurMs:   192965,
		},
		{
			name:        "I5_button_without_svg_path",
			setup:       buildDOMStub(true, false, false, "", "Song", 192965),
			wantState:   "unknown",
			wantPlaying: false,
			wantKnown:   false,
			wantTitle:   "Song",
			wantDurMs:   192965,
		},
		{
			name:        "I5_button_with_null_d",
			setup:       buildDOMStub(true, true, false, "", "", 0),
			wantState:   "unknown",
			wantPlaying: false,
			wantKnown:   false,
		},
		{
			name:        "I5_button_with_empty_d",
			setup:       buildDOMStub(true, true, true, "", "", 0),
			wantState:   "unknown",
			wantPlaying: false,
			wantKnown:   false,
		},
		{
			name:        "I5_button_with_unknown_d",
			setup:       buildDOMStub(true, true, true, "M0 0h16v16H0z", "", 0),
			wantState:   "unknown",
			wantPlaying: false,
			wantKnown:   false,
		},
		{
			name:        "I5_prefix_not_at_index_zero",
			setup:       buildDOMStub(true, true, true, "M0 0 "+iconPlayingPrefix, "", 0),
			wantState:   "unknown",
			wantPlaying: false,
			wantKnown:   false,
		},
	}

	js := snapshotJS()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _, _ := runNode(t, tc.setup, js)
			var snap Snapshot
			if err := json.Unmarshal([]byte(out), &snap); err != nil {
				t.Fatalf("failed to unmarshal snapshot %q: %v", out, err)
			}
			if !snap.OK || snap.Err != "" {
				t.Fatalf("snapshot not OK: %+v", snap)
			}
			if snap.State != tc.wantState {
				t.Fatalf("snap.State = %q, want %q (raw=%s)", snap.State, tc.wantState, out)
			}
			if snap.Title != tc.wantTitle || snap.DurationMs != tc.wantDurMs {
				t.Fatalf("snap title/dur = (%q, %v), want (%q, %v)", snap.Title, snap.DurationMs, tc.wantTitle, tc.wantDurMs)
			}
			gotPlaying, gotKnown := snap.Playing()
			if gotPlaying != tc.wantPlaying || gotKnown != tc.wantKnown {
				t.Fatalf("snap.Playing() = (%v, %v), want (%v, %v)", gotPlaying, gotKnown, tc.wantPlaying, tc.wantKnown)
			}
		})
	}
}
