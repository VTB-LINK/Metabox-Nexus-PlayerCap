package mem

// 本文件只做真机烟测：附加正在运行的 AppleMusic.exe，全扫一次，打印取到的元数据、TTML 与位置候选。
//
// 默认跳过，PLAYERCAP_LIVE=1 才跑：开发机常开着 Apple Music，不加开关时 go test ./... 会真的去扫
// 别的进程内存，结果还随本机状态变化（没在放歌时变红）。
//
//	PLAYERCAP_LIVE=1 go test ./player/applemusic/mem/ -run TestLiveSmoke -v -count=1

import (
	"os"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/applemusic/lyric"
)

func TestLiveSmoke(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("真机测试，设 PLAYERCAP_LIVE=1 才跑")
	}
	r, err := Attach()
	if err != nil {
		t.Skipf("AppleMusic 未运行或打不开: %v", err)
	}
	defer r.Close()

	t0 := time.Now()
	snap := r.Scan(true, nil)
	t.Logf("全扫耗时 %v：正在播放存储 %d 份，TTML %d 种，位置候选 %d 个（截断=%v）",
		time.Since(t0), len(snap.Songs), len(snap.Docs), len(snap.Twins), snap.TwinsTruncated)
	for _, s := range snap.Songs {
		t.Logf("  歌名=%q 歌手=%q 专辑=%q 完整=%v", s.Title, s.Artist, s.Album, s.Full)
	}
	for _, d := range snap.Docs {
		dur, _ := lyric.DocDuration(d.Text)
		t.Logf("  TTML ×%d dur=%.3f 行=%d", d.Count, dur, len(lyric.ParseTTML(d.Text)))
	}
	if len(snap.Songs) == 0 {
		t.Skip("内存里没有正在播放存储（Apple Music 未放歌？），不判定")
	}
	for _, s := range snap.Songs {
		if s.Title == "" || s.Artist == "" {
			t.Errorf("存储解析出空歌名或空歌手: %+v", s)
		}
	}
	if len(snap.Twins) == 0 {
		t.Error("一个位置候选都没有（形态判据失效？）")
	}
}
