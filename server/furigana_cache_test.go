package server

// 本文件只测一件事：lyric_update 经缓存回放（buildInitEvents 与 HTTP /qqmusic/lyric_update）时，
// furigana（振假名）必须原值保留。
//
// UpdatePlayerState 的 EventLyricUpdate 分支手动逐字段将收到的 *player.LyricUpdate 复制进
// ps.LyricUpdate；buildInitEvents、player_switch 的 FullState 与 HTTP /lyric_update 共用这一处缓存。
// 漏搬 Furigana 会导致上述缓存出口一律序列化为 `"furigana":[]`，与实时推送不一致。
//
// 变异自证：把 UpdatePlayerState EventLyricUpdate 分支里的 `Furigana: msg.Furigana` 删掉，本例即红。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func TestLyricUpdateCachePreservesFurigana(t *testing.T) {
	want := player.LyricFurigana{
		{Text: "こっち"},
		{Text: "向", Ruby: "む"},
	}
	const wantJSON = `"furigana":[{"text":"こっち"},{"text":"向","ruby":"む"}]`

	s := NewServer([]string{"qqmusic"})
	s.UpdatePlayerState(player.Event{
		PlayerName: "qqmusic",
		Type:       player.EventLyricUpdate,
		Data: &player.LyricUpdate{
			Index:     1,
			Text:      "こっち向",
			Timestamp: 10.5,
			PlayTime:  10.5,
			Position:  10.6,
			Progress:  0.1,
			Furigana:  want,
		},
	})

	// 1. buildInitEvents（连接补发 / FullState 共用入口）
	lu := initLyricUpdate(t, s, "qqmusic")
	if !reflect.DeepEqual(lu.Furigana, want) {
		t.Errorf("buildInitEvents 的 furigana = %+v，期望 %+v", lu.Furigana, want)
	}
	initJSON, err := json.Marshal(lu)
	if err != nil {
		t.Fatalf("序列化 buildInitEvents 的 lyric_update 失败: %v", err)
	}
	if !strings.Contains(string(initJSON), wantJSON) {
		t.Errorf("buildInitEvents wire 缺 furigana 原值，期望含 %s，实得: %s", wantJSON, initJSON)
	}

	// 2. HTTP /qqmusic/lyric_update 快照
	rec := httptest.NewRecorder()
	s.handleLyricUpdate("qqmusic")(rec, httptest.NewRequest(http.MethodGet, "/qqmusic/lyric_update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP /qqmusic/lyric_update 状态码 = %d，期望 %d", rec.Code, http.StatusOK)
	}
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("HTTP 响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	if !strings.Contains(string(resp.Data), wantJSON) {
		t.Errorf("HTTP /qqmusic/lyric_update 缺 furigana 原值，期望含 %s，实得: %s", wantJSON, resp.Data)
	}
	var httpLU LyricUpdate
	if err := json.Unmarshal(resp.Data, &httpLU); err != nil {
		t.Fatalf("反序列化 HTTP lyric_update data 失败: %v", err)
	}
	if !reflect.DeepEqual(httpLU.Furigana, want) {
		t.Errorf("HTTP /qqmusic/lyric_update 的 furigana = %+v，期望 %+v", httpLU.Furigana, want)
	}
}
