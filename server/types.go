package server

import (
	"bytes"
	"encoding/json"

	"Metabox-Nexus-PlayerCap/player"
)

// OrderedMap 是一个保留插入顺序的 JSON 序列化键值表。
type OrderedMap struct {
	keys []string
	vals map[string]interface{}
}

// NewOrderedMap 创建一个新的空 OrderedMap。
func NewOrderedMap() *OrderedMap {
	return &OrderedMap{vals: make(map[string]interface{})}
}

// Set 添加或更新键值对，保持插入顺序（重复 key 只更新值，不移动位置）。
func (om *OrderedMap) Set(key string, val interface{}) {
	if _, exists := om.vals[key]; !exists {
		om.keys = append(om.keys, key)
	}
	om.vals[key] = val
}

// MarshalJSON 按插入顺序序列化。
func (om *OrderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range om.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		valBytes, err := json.Marshal(om.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(valBytes)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// WSEvent WebSocket/SSE 统一事件包装器（含 player 字段）
type WSEvent struct {
	Type   string      `json:"type"`
	Player string      `json:"player"`
	Data   interface{} `json:"data"`
}

// HTTPResponse 统一 HTTP JSON 响应
type HTTPResponse struct {
	Code   int         `json:"code"`
	Msg    string      `json:"msg"`
	Player string      `json:"player"`
	Data   interface{} `json:"data"`
}

// Re-export player types for convenience
type LyricItem = player.LyricLine
type LyricUpdate = player.LyricUpdate
type SongInfoUpdate = player.SongInfo
type StatusMessage = player.StatusInfo
type AllLyrics = player.AllLyricsData
