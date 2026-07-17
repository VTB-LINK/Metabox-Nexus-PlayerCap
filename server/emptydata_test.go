package server

// 本文件只测一件事：**对外事件的空载荷必须是 {}，绝不是 null**（openapi.yaml:10 的全局约定）。
//
// 真实缺陷：wesing 的 lyric_idle 两处传 nil，实测发出 {"type":"lyric_idle","data":null}，
// 而文档三处声称「data 始终为 {}」。它没有任何症状——前端只看 type，不碰 data——所以
// 躺到实测录音才暴露。
//
// 修法是双保险：播放器侧传 struct{}{}（源头），NotifySubscribers 出口再兜一次（这里）。
// 出口那道是承重的：指望每个 Emit 调用点各自记得，正是它当初破掉的原因。

import (
	"encoding/json"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestNilDataBecomesEmptyObject 钉死出口把 nil 兜成 {}。
//
// 用 json.Marshal 实测序列化结果，而不是断言 Data != nil：约定说的是**线上的字节**
// 是 {} 而非 null，中间用什么类型表达无所谓。
//
// 变异自证：删掉 NotifySubscribers 里的 `if wsEvt.Data == nil { wsEvt.Data = struct{}{} }` 即红。
func TestNilDataBecomesEmptyObject(t *testing.T) {
	s := NewServer([]string{"wesing"})
	sub := s.subscribe("wesing", nil, true, "per-player")

	// lyric_idle 是唯一会传 nil 的对外事件（clear_song_data 传 nil 但被 internalEvents 拦住）
	s.NotifySubscribers(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricIdle,
		Data:       nil,
	}, false)

	select {
	case got := <-sub.ch:
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		var probe struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			t.Fatalf("反解析失败: %v", err)
		}
		if string(probe.Data) != "{}" {
			t.Errorf("data 序列化成 %s，期望 {} —— 「空数据一律 {}，绝不 null」是全局约定"+
				"（openapi.yaml:10），下游写 data.xxx 会 TypeError", probe.Data)
		}
	default:
		t.Fatal("订阅者没收到 lyric_idle —— 它是对外事件，不该被拦")
	}
}

// TestNonNilDataUntouched 钉死兜底只碰 nil，不动真载荷。
//
// 没有这条，把兜底写成「无条件覆盖成 {}」也能让上面那条绿——而那会清空所有事件的数据。
func TestNonNilDataUntouched(t *testing.T) {
	s := NewServer([]string{"wesing"})
	sub := s.subscribe("wesing", nil, true, "per-player")

	s.NotifySubscribers(player.Event{
		PlayerName: "wesing",
		Type:       player.EventStatusUpdate,
		Data:       &player.StatusInfo{Status: "playing", Detail: "某歌 - 某人"},
	}, false)

	select {
	case got := <-sub.ch:
		si, ok := got.Data.(*player.StatusInfo)
		if !ok {
			t.Fatalf("载荷类型是 %T，期望 *player.StatusInfo —— 兜底不该碰非 nil 的数据", got.Data)
		}
		if si.Status != "playing" || si.Detail != "某歌 - 某人" {
			t.Errorf("载荷被改动了: %+v", si)
		}
	default:
		t.Fatal("订阅者没收到 status_update")
	}
}
