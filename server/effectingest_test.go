package server

// 本文件测 effect-ingest 收帧：toBlob 的二进制 JPEG 原样交出，被拖住时当场编的 data URL 文本帧在此解码
// （issue #98）；取不出 JPEG 的消息丢弃且不断开连接。对外 effect-ws 的帧格式不在此测（仍是二进制 JPEG）。

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}

// TestIngestJPEG 逐条反例：只有带 JPEG data URL 前缀、base64 合法且非空的文本帧才解码。
//
// 变异自证：去掉文本分支（文本帧一律丢弃），「data URL」一例红；不查前缀直接解码剩余部分，png 与
// 无前缀两例红；解码出错也交出，非法 base64 一例红。
func TestIngestJPEG(t *testing.T) {
	dataURL := ingestDataURLPrefix + base64.StdEncoding.EncodeToString(testJPEG)
	cases := []struct {
		name string
		mt   int
		data string
		want []byte
	}{
		{"binary JPEG passes through", websocket.BinaryMessage, string(testJPEG), testJPEG},
		{"empty binary", websocket.BinaryMessage, "", nil},
		{"data URL", websocket.TextMessage, dataURL, testJPEG},
		{"data URL of a single byte", websocket.TextMessage, ingestDataURLPrefix + "/w==", []byte{0xFF}},
		{"prefix only", websocket.TextMessage, ingestDataURLPrefix, nil},
		{"png data URL", websocket.TextMessage, "data:image/png;base64," + base64.StdEncoding.EncodeToString(testJPEG), nil},
		{"toDataURL fallback for an empty canvas", websocket.TextMessage, "data:,", nil},
		{"bare base64 without prefix", websocket.TextMessage, base64.StdEncoding.EncodeToString(testJPEG), nil},
		{"upper-case scheme", websocket.TextMessage, strings.ToUpper(ingestDataURLPrefix) + base64.StdEncoding.EncodeToString(testJPEG), nil},
		{"leading space", websocket.TextMessage, " " + dataURL, nil},
		{"invalid base64", websocket.TextMessage, ingestDataURLPrefix + "@@@@", nil},
		{"truncated base64", websocket.TextMessage, dataURL[:len(dataURL)-1], nil},
		{"url-safe alphabet", websocket.TextMessage, ingestDataURLPrefix + "_9g=", nil},
		{"ping frame", websocket.PingMessage, string(testJPEG), nil},
		{"close frame", websocket.CloseMessage, string(testJPEG), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ingestJPEG(tc.mt, []byte(tc.data))
			if ok != (tc.want != nil) || !bytes.Equal(got, tc.want) {
				t.Fatalf("ingestJPEG = (%x, %v), want (%x, %v)", got, ok, tc.want, tc.want != nil)
			}
		})
	}
}

// TestEffectIngestDeliversBothFrameKinds 经真实的 handleEffectIngest：二进制帧与 data URL 文本帧按到达
// 顺序交给 handler，坏消息丢弃后连接照常收后续帧。
//
// 变异自证：handleEffectIngest 改回只收二进制（不经 ingestJPEG），红。
func TestEffectIngestDeliversBothFrameKinds(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	got := make(chan []byte, 16)
	s.SetEffectIngestHandler(func(b []byte) { got <- b })
	srv := httptest.NewServer(http.HandlerFunc(s.handleEffectIngest))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/?t="+s.effectHub.ingestToken, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	second := append([]byte{}, testJPEG...)
	second[len(second)-3] = 0x01
	third := []byte{0xFF, 0xD8, 0x03}
	msgs := []struct {
		mt   int
		data []byte
	}{
		{websocket.BinaryMessage, testJPEG},
		{websocket.TextMessage, []byte("data:image/png;base64,AAAA")},
		{websocket.TextMessage, []byte(ingestDataURLPrefix + base64.StdEncoding.EncodeToString(second))},
		{websocket.BinaryMessage, nil},
		{websocket.TextMessage, []byte(ingestDataURLPrefix + "!!")},
		{websocket.BinaryMessage, third},
	}
	for _, m := range msgs {
		if err := c.WriteMessage(m.mt, m.data); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for i, want := range [][]byte{testJPEG, second, third} {
		select {
		case b := <-got:
			if !bytes.Equal(b, want) {
				t.Fatalf("frame %d = %x, want %x", i, b, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("frame %d never arrived", i)
		}
	}
	select {
	case b := <-got:
		t.Fatalf("unexpected extra frame %x", b)
	case <-time.After(100 * time.Millisecond):
	}
}
