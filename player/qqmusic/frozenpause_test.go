package qqmusic

// 本文件只测 emitFrozenPause：已判暂停时随 all_lyrics 补发的 playback_pause。它报告是否入队，通道满时不阻塞、不入队、返回假，
// runSession 据此记下、每拍再试（再试与清除的接线由 assocwiring_test.go 守）。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func TestEmitFrozenPause(t *testing.T) {
	p := New(0, 500)
	if !p.emitFrozenPause(12.5, 200) {
		t.Fatal("通道有空位时应入队")
	}
	e := <-p.EventCh
	info, ok := e.Data.(*player.PlaybackTimeInfo)
	if e.Type != player.EventPlaybackPause || !ok || info.Position != 12.5 || info.Progress != player.ClampProgress(12.5, 200) {
		t.Fatalf("应为 playback_pause @12.5、progress 0.0625，实得 %s %+v", e.Type, e.Data)
	}

	// 反例：位置超出时长时 progress 截到 1。
	if !p.emitFrozenPause(250, 200) {
		t.Fatal("通道有空位时应入队")
	}
	if info := (<-p.EventCh).Data.(*player.PlaybackTimeInfo); info.Position != 250 || info.Progress != 1 {
		t.Fatalf("progress 应截到 1，实得 %+v", info)
	}

	// 反例：通道满时不阻塞、不入队、返回假；腾出一格后再试即入队。
	for len(p.EventCh) < cap(p.EventCh) {
		p.Emit(player.EventPositionSample, nil)
	}
	if p.emitFrozenPause(12.5, 200) {
		t.Fatal("通道满时应返回假")
	}
	if len(p.EventCh) != cap(p.EventCh) {
		t.Fatalf("通道满时不应入队，通道里 %d 条", len(p.EventCh))
	}
	<-p.EventCh
	if !p.emitFrozenPause(12.5, 200) {
		t.Fatal("腾出一格后应入队")
	}
}
