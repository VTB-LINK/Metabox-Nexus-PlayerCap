package server

// 本文件只测 /service-status 的 player_running 判据（#231，设计 §3.2）：它表示「进程在运行」，排除 offline
// 与 waiting_process，其余一律计入（standby 是进程在运行而读不了，计入；error、loading 是保留值，按字面计入）。

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestServiceStatusPlayerRunning 八个播放器各置一种状态，按注册顺序断言 player_running 与 player_status；
// 再把状态整体轮换一位做一遍，免得哪个名字恰好配上了期望的状态。
//
// 变异自证（实测变红）：判据改回排除 standby（`st != "standby"` 加回去）；判据漏掉 waiting_process 的排除。
func TestServiceStatusPlayerRunning(t *testing.T) {
	names := []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic", "applemusic", "spotify", "kuwo"}
	statuses := []string{"offline", "waiting_process", "standby", "waiting_song", "playing", "paused", "error", "loading"}
	counted := map[string]bool{"standby": true, "waiting_song": true, "playing": true, "paused": true, "error": true, "loading": true}

	for shift := 0; shift < 2; shift++ {
		s := NewServer(names)
		s.SetServiceInfo(&ServiceInfo{Version: "9.9.9", PlayerSupport: names})
		want := []string{}
		wantStatus := map[string]string{}
		for i, name := range names {
			st := statuses[(i+shift)%len(statuses)]
			wantStatus[name] = st
			if counted[st] {
				want = append(want, name)
			}
			if st == "offline" {
				// 从未上报过状态：缓存里只有别的事件也仍是 offline。
				s.UpdatePlayerState(player.Event{PlayerName: name, Type: player.EventSongInfoUpdate, Data: &player.SongInfo{Name: "x"}})
				continue
			}
			s.UpdatePlayerState(player.Event{PlayerName: name, Type: player.EventStatusUpdate,
				Data: &player.StatusInfo{Status: st, Detail: "d", Reason: "r"}})
		}

		rec := httptest.NewRecorder()
		s.handleServiceStatus(rec, httptest.NewRequest("GET", "/service-status", nil))
		var resp struct {
			Data struct {
				PlayerRunning []string          `json:"player_running"`
				PlayerStatus  map[string]string `json:"player_status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
		}
		if !reflect.DeepEqual(resp.Data.PlayerRunning, want) {
			t.Errorf("轮换 %d：player_running = %q，期望 %q（按注册顺序，排除 offline 与 waiting_process）", shift, resp.Data.PlayerRunning, want)
		}
		if !reflect.DeepEqual(resp.Data.PlayerStatus, wantStatus) {
			t.Errorf("轮换 %d：player_status = %v，期望 %v", shift, resp.Data.PlayerStatus, wantStatus)
		}
	}
}
