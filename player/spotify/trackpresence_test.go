package spotify

// 本文件测连上时一拍快照的分类与要声明的状态（#231，设计 doc/design/converge-player-behavior-231-design.md 4.3 第 6 条、
// 4.4 spotify 的判据）：歌名为空不等于没有曲目，时长有效或在播放即「曲目在、歌名读不到」。

import (
	"math"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

// TestClassifyTrack
//
// 变异自证（均实测变红）：去掉 playing 条件（「播放中」的各例；TestStartStatus 的「图标认不出、位置推进…」同红）；时长判定改为
// durationMs != 0（NaN、±Inf、负数各例）；时长判定改为只看 durationMs > 0（+Inf 一例）。
func TestClassifyTrack(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	cases := []struct {
		name       string
		title      string
		durationMs float64
		playing    bool
		want       trackPresence
	}{
		{"歌名非空、时长 0、暂停", "合成歌名", 0, false, trackNamed},
		{"歌名非空、时长 NaN、播放中", "合成歌名", nan, true, trackNamed},
		{"歌名非空、时长有效、暂停", "合成歌名", 200000, false, trackNamed},
		{"歌名非空、时长有效、播放中", "合成歌名", 200000, true, trackNamed},
		{"没有曲目：时长 0", "", 0, false, trackAbsent},
		{"没有曲目：时长 NaN", "", nan, false, trackAbsent},
		{"没有曲目：时长 +Inf", "", inf, false, trackAbsent},
		{"没有曲目：时长 -Inf", "", math.Inf(-1), false, trackAbsent},
		{"没有曲目：时长为负", "", -1, false, trackAbsent},
		{"歌名读不到：时长有效、暂停", "", 200000, false, trackUnnamed},
		{"歌名读不到：时长最小正数", "", math.SmallestNonzeroFloat64, false, trackUnnamed},
		{"歌名读不到：播放中、时长 0", "", 0, true, trackUnnamed},
		{"歌名读不到：播放中、时长 NaN", "", nan, true, trackUnnamed},
		{"歌名读不到：播放中、时长为负", "", -1, true, trackUnnamed},
	}
	for _, c := range cases {
		if got := classifyTrack(c.title, c.durationMs, c.playing); got != c.want {
			t.Errorf("%s：classifyTrack(%q, %v, %v) = %d，期望 %d", c.name, c.title, c.durationMs, c.playing, got, c.want)
		}
	}
}

// TestPreAnnounceStatus
//
// 变异自证（均实测变红，TestStartStatus 的对应子例同红）：trackUnnamed 改报 NoSongStatus（waiting_song）；trackAbsent 改报
// metadata_unavailable。
func TestPreAnnounceStatus(t *testing.T) {
	absent := preAnnounceStatus(trackAbsent)
	if absent == nil || *absent != (player.StatusInfo{Status: "waiting_song", Detail: i18n.T("Spotify has no current track"), Reason: player.ReasonNoSong}) {
		t.Errorf("没有曲目：%+v，期望 waiting_song / no_song", absent)
	}
	unnamed := preAnnounceStatus(trackUnnamed)
	if unnamed == nil || *unnamed != (player.StatusInfo{Status: "standby", Detail: i18n.T("Cannot read the Spotify track title"), Reason: player.ReasonMetadataUnavailable}) {
		t.Errorf("曲目在、歌名读不到：%+v，期望 standby / metadata_unavailable，不是 waiting_song", unnamed)
	}
	if named := preAnnounceStatus(trackNamed); named != nil {
		t.Errorf("有歌名：%+v，期望 nil（照常宣告，不声明非歌名状态）", named)
	}
}
