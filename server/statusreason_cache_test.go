package server

// 本文件只测一件事：status_update 经**缓存回放**时，reason（稳定机器码）必须原值保留。
//
// UpdatePlayerState 的 EventStatusUpdate 分支手动逐字段把 *player.StatusInfo 复制进 ps.Status。
// 这个手复制曾只搬 Status 与 Detail、漏了 Reason，后果是 HTTP /status_update、/<player>/status_update、
// 新连接的 buildInitEvents 初始补发与 player_switch 的 FullState 都不带 reason——只有实时推送的那一条
// 带。文档要求下游按 reason 判断（detail 随界面语言变），缓存出口拿不到它就只能回去匹配中文。
// buildInitEvents 与 HTTP handler 都直接透传同一个 ps.Status，故测 buildInitEvents 即测全部缓存出口。
//
// 变异自证：把 UpdatePlayerState EventStatusUpdate 分支里的 `Reason: msg.Reason` 删掉，本例即红。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func TestStatusCachePreservesReason(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventStatusUpdate,
		Data:       &player.StatusInfo{Status: "waiting_song", Detail: "K歌窗口未打开", Reason: player.ReasonWindowNotOpen},
	})
	for _, e := range s.buildInitEvents("wesing", true) {
		if e.Type != player.EventStatusUpdate {
			continue
		}
		st, ok := e.Data.(*StatusMessage)
		if !ok {
			t.Fatalf("status_update 载荷类型是 %T，期望 *StatusMessage", e.Data)
		}
		if st.Reason != player.ReasonWindowNotOpen || st.Status != "waiting_song" {
			t.Fatalf("缓存回放的 status_update = %+v，reason 应为 %q", st, player.ReasonWindowNotOpen)
		}
		return
	}
	t.Fatal("buildInitEvents 里没有 status_update")
}
