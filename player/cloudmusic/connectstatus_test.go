package cloudmusic

// 本文件只测连上之后按快照如实报状态的两个判据（#231，设计 doc/design/converge-player-behavior-231-design.md 4.3 第 2 条、
// 4.4 cloudmusicv3 行）：snapshotHasSong 判这一拍有没有当前歌曲（本会话还没宣告过歌时没有即报 waiting_song），
// songStatus 给出换歌拍按播放态报的状态（暂停中连上报 paused）。runSession 里的接线由 startstatus_test.go 经客户端桩守。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
)

// TestSnapshotHasSong：DOM 与 Redux 任一处有歌名即有歌（DOM 为空的取 Redux）；没有快照、没有 CurPlaying、两处都没有歌名
// 都算没有歌。身份键只看歌名：只有歌手、歌名只有空白都算没有歌（按 buildSongIdentityKey 的现行行为）。
//
// 变异自证（均实测变红）：不判 CurPlaying 为空（「CurPlaying 为空而 DOM 有歌名」）；只看 DOM、不取 Redux 的回退
// （「DOM 为空而 Redux 有」「只有 Redux 的歌名」）；歌名或歌手任一非空即算有歌（「只有歌手」两例、「歌名只有空白」）。
func TestSnapshotHasSong(t *testing.T) {
	dom := func(d *cdp.ExtractionData, name, artist string) *cdp.ExtractionData {
		d.DomSongName, d.DomArtist = name, artist
		return d
	}
	cases := []struct {
		name string
		data *cdp.ExtractionData
		want bool
	}{
		{"快照为 nil", nil, false},
		{"CurPlaying 为空而 DOM 有歌名", &cdp.ExtractionData{DomSongName: "合成歌名", DomArtist: "合成歌手"}, false},
		{"DOM 与 Redux 都没有歌名与歌手", newSnapshot("", ""), false},
		{"只有 DOM 的歌名", dom(newSnapshot("", ""), "合成歌名", ""), true},
		{"只有 Redux 的歌名", newSnapshot("合成歌名", ""), true},
		{"只有 DOM 的歌手", dom(newSnapshot("", ""), "", "合成歌手"), false},
		{"只有 Redux 的歌手", newSnapshot("", "合成歌手"), false},
		{"DOM 为空而 Redux 有", newSnapshot("合成歌名", "合成歌手"), true},
		{"DOM 与 Redux 都有", dom(newSnapshot("合成歌名乙", "合成歌手乙"), "合成歌名", "合成歌手"), true},
		{"歌名只有空白", dom(newSnapshot(" \t", ""), "  ", "合成歌手"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := snapshotHasSong(c.data); got != c.want {
				t.Errorf("snapshotHasSong() = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestSongStatus：PlayingState 为 2 报 playing，其余（含未知值）报 paused；detail 为标题。
//
// 变异自证（均实测变红）：恒报 playing；判据写成 != 1；报 paused 时不带标题。
func TestSongStatus(t *testing.T) {
	const title = "合成歌名 - 合成歌手"
	cases := []struct {
		state int
		want  string
	}{
		{2, "playing"},
		{1, "paused"},
		{0, "paused"},
		{-1, "paused"},
		{3, "paused"},
	}
	for _, c := range cases {
		s := songStatus(c.state, title)
		if s == nil || s.Status != c.want || s.Detail != title || s.Reason != "" {
			t.Errorf("songStatus(%d) = %+v，期望 %s、detail 为标题、没有 reason", c.state, s, c.want)
		}
	}
}
