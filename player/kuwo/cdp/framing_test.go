package cdp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The tested Blink parser (libcef RVA 0x7E08EE/0x7E0966) returns null when
// tokenEnd != end, including a valid JSON object followed by '\n'. Its dispatch
// dereferences that result at 0xA5DA88. This test captures REAL Gorilla frames,
// then models only that final boundary predicate using Decoder.InputOffset.
// It does not load/execute Kuwo DLLs or claim the standard Go JSON parser itself
// rejects trailing whitespace. The production sender is exercised separately
// by transport_test.go; restoring WriteJSON there makes those tests fail.
func TestLegacyWriteJSONAddsRejectedTail(t *testing.T) {
	frames := make(chan []byte, 2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for i := 0; i < 2; i++ {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			frames <- b
		}
	}))
	defer s.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	request := map[string]interface{}{"id": 1, "method": "Runtime.evaluate", "params": map[string]interface{}{"expression": `window.external.callkwmusic("GetVer")`, "returnByValue": true}}
	if err = c.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	readFrame := func() []byte {
		t.Helper()
		select {
		case b := <-frames:
			return b
		case <-time.After(3 * time.Second):
			t.Fatal("frame capture timed out")
			return nil
		}
	}
	legacy := readFrame()
	compact, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.WriteMessage(websocket.TextMessage, compact); err != nil {
		t.Fatal(err)
	}
	current := readFrame()
	if !bytes.Equal(legacy, append(append([]byte(nil), current...), '\n')) {
		t.Fatalf("unexpected frame difference: legacy=%q compact=%q", legacy, current)
	}
	for _, sample := range []struct {
		name string
		b    []byte
		want bool
	}{{"legacy", legacy, false}, {"compact", current, true}, {"space suffix", append(append([]byte(nil), current...), ' '), false}} {
		decoder := json.NewDecoder(bytes.NewReader(sample.b))
		var v interface{}
		if err = decoder.Decode(&v); err != nil {
			t.Fatal(err)
		}
		accepted := decoder.InputOffset() == int64(len(sample.b))
		if accepted != sample.want {
			t.Fatalf("%s: tokenEnd=%d length=%d", sample.name, decoder.InputOffset(), len(sample.b))
		}
	}
}
