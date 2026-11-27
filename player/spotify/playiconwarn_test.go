package spotify

// 本文件只测播放/暂停图标缺失与不认识时的告警判定（shouldWarnUnknownIcon 与 runSession 会话级告警接线）。
//
// 变异自证（在隔离副本里逐条注入、跑完即清理，均实测变红）：
//   - M11a（shouldWarnUnknownIcon 对 State == "missing" 且无曲目仍返回 true）→
//     TestShouldWarnUnknownIcon/I3_* 与 TestSessionUnknownIconWarn/I3_cold_start_missing_then_ready_no_warn、
//     I4b_missing_first_sample_no_advance_no_warn 红；
//   - M11b（shouldWarnUnknownIcon 对 State == "missing" 一律返回 false）→
//     TestShouldWarnUnknownIcon/I4a_*、I4b_missing_with_playing_seen 与
//     TestSessionUnknownIconWarn/I4a_*、I4b_missing_position_advances_warns_once 红；
//   - M11c（State == "missing" 忽略 playingSeen）→
//     TestShouldWarnUnknownIcon/I4b_missing_with_playing_seen 与
//     TestSessionUnknownIconWarn/I4b_missing_position_advances_warns_once 红；
//   - M11d（runSession 去掉 && shouldWarnUnknownIcon(snap, playingSeen)）→
//     TestSessionUnknownIconWarn/I3_cold_start_missing_then_ready_no_warn、
//     I4b_missing_first_sample_no_advance_no_warn 红。

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/cdp"
)

func TestShouldWarnUnknownIcon(t *testing.T) {
	cases := []struct {
		name        string
		snap        *cdp.Snapshot
		playingSeen bool
		want        bool
	}{
		{"nil_snapshot", nil, false, false},
		{"I1_playing", &cdp.Snapshot{State: "playing", Title: "Song", DurationMs: 192965}, true, false},
		{"I2_paused", &cdp.Snapshot{State: "paused", Title: "Song", DurationMs: 192965}, false, false},
		{"I3_missing_empty_page", &cdp.Snapshot{State: "missing"}, false, false},
		{"I3_missing_nan_duration", &cdp.Snapshot{State: "missing", DurationMs: math.NaN()}, false, false},
		{"I3_missing_inf_duration", &cdp.Snapshot{State: "missing", DurationMs: math.Inf(1)}, false, false},
		{"I4a_missing_with_title", &cdp.Snapshot{State: "missing", Title: "Song"}, false, true},
		{"I4a_missing_with_duration", &cdp.Snapshot{State: "missing", DurationMs: 192965}, false, true},
		{"I4b_missing_with_playing_seen", &cdp.Snapshot{State: "missing"}, true, true},
		{"I5_unknown_empty_page", &cdp.Snapshot{State: "unknown"}, false, true},
		{"I5_unknown_with_track", &cdp.Snapshot{State: "unknown", Title: "Song", DurationMs: 192965}, true, true},
		{"I5_empty_state", &cdp.Snapshot{State: ""}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldWarnUnknownIcon(tc.snap, tc.playingSeen); got != tc.want {
				t.Errorf("shouldWarnUnknownIcon(%+v, playingSeen=%v) = %v, want %v", tc.snap, tc.playingSeen, got, tc.want)
			}
		})
	}
}

func TestSessionUnknownIconWarn(t *testing.T) {
	iconWarnText := i18n.T("play/pause icon not recognised; falling back to position-advance detection")
	d := spotifyDetails()
	noTrackDetail := i18n.T("Spotify has no current track")
	unnamedDetail := i18n.T("Cannot read the Spotify track title")

	missingEmpty := func(time.Duration) *cdp.Snapshot {
		return &cdp.Snapshot{OK: true, State: "missing"}
	}

	cases := []struct {
		name      string
		until     float64
		snap      func(time.Duration) *cdp.Snapshot
		wantWarns int
		statuses  []string // nil 即不核对状态序列
	}{
		{
			name:      "I3_cold_start_missing_then_ready_no_warn",
			until:     4,
			snap:      sequence(missingEmpty, 2, named),
			wantWarns: 0,
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, waitingSong, player.ReasonNoSong, noTrackDetail),
				at(2, "playing", "", songTitle),
			},
		},
		{
			name:      "I4b_missing_first_sample_no_advance_no_warn",
			until:     0.5,
			snap:      missingEmpty,
			wantWarns: 0,
		},
		{
			name:  "I4b_missing_position_advances_warns_once",
			until: 3,
			snap: func(now time.Duration) *cdp.Snapshot {
				return &cdp.Snapshot{OK: true, PositionMs: float64(now / time.Second * 1000), State: "missing"}
			},
			wantWarns: 1,
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, waitingSong, player.ReasonNoSong, noTrackDetail),
				at(1, standby, player.ReasonMetadataUnavailable, unnamedDetail),
			},
		},
		{
			name:  "I4a_missing_with_title_warns_once",
			until: 1,
			snap: func(time.Duration) *cdp.Snapshot {
				return &cdp.Snapshot{OK: true, Title: "Song", State: "missing"}
			},
			wantWarns: 1,
		},
		{
			name:  "I4a_missing_with_duration_warns_once",
			until: 1,
			snap: func(time.Duration) *cdp.Snapshot {
				return &cdp.Snapshot{OK: true, DurationMs: 192965, State: "missing"}
			},
			wantWarns: 1,
		},
		{
			name:  "I5_unknown_empty_page_warns_once",
			until: 1,
			snap: func(time.Duration) *cdp.Snapshot {
				return &cdp.Snapshot{OK: true, State: "unknown"}
			},
			wantWarns: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runStartScript(t, startScript{
				until:   tc.until,
				running: always(true),
				connect: connectOnce(map[float64]func() *fakeClient{
					0: func() *fakeClient { return &fakeClient{snap: tc.snap, end: 1e9} },
				}),
			})
			if got := strings.Count(r.logs, iconWarnText); got != tc.wantWarns {
				t.Errorf("图标不认识告警打了 %d 次，期望 %d 次；日志：%s", got, tc.wantWarns, r.logs)
			}
			if tc.statuses != nil {
				if got := r.statusesBefore(secDur(tc.until)); !slices.Equal(got, tc.statuses) {
					t.Errorf("状态序列\n got  %v\n want %v", got, tc.statuses)
				}
			}
		})
	}
}
