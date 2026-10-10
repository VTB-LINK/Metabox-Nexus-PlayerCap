package server

// 本文件只测一件事：/service-status 顶层的 capabilities 能力判据（issue #63 第 3 条）。
//
// 前端据它判断连接补发与 HTTP 快照里的 position 是否已外推到发送时刻；旧后端没有这个字段，
// 前端先忽略补发位置、等第一条真实事件再校准。字段丢失或取值拼错时，新前端会把新后端当旧后端，
// 静默退回保守行为，不报错。

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestServiceStatusCapabilities(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.SetServiceInfo(&ServiceInfo{Version: "9.9.9"})

	rec := httptest.NewRecorder()
	s.handleServiceStatus(rec, httptest.NewRequest("GET", "/service-status", nil))
	body := rec.Body.String()

	var resp struct {
		Data struct {
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	if want := []string{"init_position_live"}; !reflect.DeepEqual(resp.Data.Capabilities, want) {
		t.Errorf("capabilities = %q，期望 %q", resp.Data.Capabilities, want)
	}
	// 位置紧随 version：openapi 与 API_RESPONSE_EXAMPLES.md 的示例按此顺序列出。
	if !strings.Contains(body, `"version":"9.9.9","capabilities":["init_position_live"],"addr"`) {
		t.Errorf("capabilities 不在 version 之后、addr 之前：%s", body)
	}
}
