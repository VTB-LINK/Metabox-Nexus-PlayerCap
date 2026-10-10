package applemusic

// 本文件只做真机集成测试：启动播放器收 12 秒事件，断言有歌在放时发出了歌曲信息、完整歌词与歌名态。
//
// 默认跳过，PLAYERCAP_LIVE=1 才跑（理由同 mem 包的 live_smoke_test.go）。需 Apple Music 正在放歌。
//
//	PLAYERCAP_LIVE=1 go test ./player/applemusic/ -run TestLivePlayer -v -count=1

import (
	"os"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

func TestLivePlayer(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("真机测试，设 PLAYERCAP_LIVE=1 才跑")
	}
	r, err := mem.Attach()
	if err != nil {
		t.Skipf("AppleMusic 未运行或打不开: %v", err)
	}
	songs := len(r.Scan(false, nil).Songs)
	r.Close()
	if songs == 0 {
		t.Skip("AppleMusic 未在播放任何歌曲，跳过（本测试需有歌在播）")
	}

	p := New(400, 300)
	go p.Start()
	defer p.Stop()

	deadline := time.After(12 * time.Second)
	var gotSongInfo, gotAllLyrics, gotStatus bool
	var songName string
	n := 0
loop:
	for {
		select {
		case <-deadline:
			break loop
		case ev := <-p.Events():
			n++
			switch d := ev.Data.(type) {
			case *player.SongInfo:
				gotSongInfo = true
				songName = d.Name
				t.Logf("[song_info] name=%q singer=%q", d.Name, d.Singer)
			case *player.StatusInfo:
				if d.Status == "playing" || d.Status == "paused" {
					gotStatus = true
				}
				t.Logf("[status] %s %q reason=%q", d.Status, d.Detail, d.Reason)
			case *player.AllLyricsData:
				gotAllLyrics = true
				t.Logf("[all_lyrics] dur=%.1f pos=%.1f count=%d", d.Duration, d.Position, d.Count)
			case *player.LyricUpdate:
				t.Logf("[lyric_update] idx=%d pos=%.1f %q", d.Index, d.Position, d.Text)
			case *player.PlaybackTimeInfo:
				t.Logf("[%s] pos=%.1f", ev.Type, d.Position)
			}
		}
	}

	t.Logf("共收到 %d 个事件", n)
	if !gotSongInfo {
		t.Error("未收到 song_info_update")
	}
	if !gotAllLyrics {
		t.Error("未收到 all_lyrics")
	}
	if !gotStatus {
		t.Error("未收到 playing/paused 状态")
	}
	if gotSongInfo && songName == "" {
		t.Error("song_info_update 的 Name 为空")
	}
}
