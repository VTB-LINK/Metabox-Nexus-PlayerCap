package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Channel suffixes observed on real installations; the fake page directory and
// bridge version are parameterized by them instead of being fixed to W1.
var testBuilds = []string{"W1", "UG1"}

// A local fake CEF server exercises the production discovery and socket paths;
// it never contacts the running music player. pageBuild is the suffix of the
// served page directory, version the bridge's GetVer result.
func fakeCEF(t *testing.T, pageBuild, version string, reply func(*websocket.Conn, uint64)) *httptest.Server {
	t.Helper()
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_ = json.NewEncoder(w).Encode([]target{{Type: "page", URL: "file:///C:/Kuwo/9.6.0.0_" + pageBuild + "/bin/html/webdata/netsong/index.html", WebSocketDebuggerURL: "ws" + strings.TrimPrefix(s.URL, "http") + "/devtools/page/1"}})
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID     uint64 `json:"id"`
				Params struct {
					Expression string `json:"expression"`
				} `json:"params"`
			}
			kind, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage || !bytes.Equal(frame, bytes.TrimSpace(frame)) {
				t.Error("request must be a compact text frame without edge whitespace")
			}
			if json.Unmarshal(frame, &req) != nil {
				return
			}
			if !strings.HasPrefix(req.Params.Expression, "JSON.stringify({") {
				t.Error("native bridge values must stay inside JSON.stringify")
			}
			if req.Params.Expression == versionExpression {
				payload, _ := json.Marshal(map[string]string{"version": version})
				_ = conn.WriteJSON(map[string]interface{}{"id": req.ID, "result": map[string]interface{}{"result": map[string]interface{}{"value": string(payload)}}})
			} else {
				reply(conn, req.ID)
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestTransportReadsValueAndSkipsEvents(t *testing.T) {
	for _, build := range testBuilds {
		t.Run(build, func(t *testing.T) {
			s := fakeCEF(t, build, "MUSIC_9.6.0.0_"+build, func(c *websocket.Conn, id uint64) {
				_ = c.WriteJSON(map[string]interface{}{"method": "Runtime.consoleAPICalled"})
				payload, _ := json.Marshal(map[string]string{"before": "Status=1&SongId=1", "song": encodedSong("MUSIC_1", "title", "artist"), "after": "Status=1&SongId=1"})
				_ = c.WriteJSON(map[string]interface{}{"id": id, "result": map[string]interface{}{"result": map[string]interface{}{"value": string(payload)}}})
			})
			c, err := connect(context.Background(), s.URL+"/json")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if c.Build() != build {
				t.Fatalf("build=%q", c.Build())
			}
			got, err := c.Snapshot(context.Background())
			if err != nil || got.Title != "title" || got.RID != "MUSIC_1" {
				t.Fatalf("snapshot=%+v err=%v", got, err)
			}
		})
	}
}

func TestTransportCancellationClosesSocket(t *testing.T) {
	for _, build := range testBuilds {
		t.Run(build, func(t *testing.T) { testCancellationClosesSocket(t, build) })
	}
}

func testCancellationClosesSocket(t *testing.T, build string) {
	entered := make(chan struct{}, 1)
	s := fakeCEF(t, build, "MUSIC_9.6.0.0_"+build, func(c *websocket.Conn, id uint64) { entered <- struct{}{} })
	c, err := connect(context.Background(), s.URL+"/json")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Snapshot(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not sent")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled RPC hung")
	}
	if _, err := c.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed connection reused: %v", err)
	}
}

// Bridge versions are compared with the selected page directory.
// 变异自证（隔离副本，实测）：交叉校验改成页面后缀与自身比较 → 三个不一致的用例连上而红；
// bridgeVersionPattern 的数字部分放宽为 `[0-9.]+` → 9.7.0.0 一例红。
func TestTransportRejectsUnverifiedOrMismatchedBridge(t *testing.T) {
	for _, tc := range []struct{ page, version string }{
		{"W1", "MUSIC_10.0"},
		{"W1", "MUSIC_9.7.0.0_W1"},
		{"W1", "MUSIC_9.6.0.0"},
		{"W1", ""},
		{"W1", "MUSIC_9.6.0.0_UG1"},
		{"UG1", "MUSIC_9.6.0.0_W1"},
		{"UG1", "MUSIC_9.6.0.0_UG2"},
	} {
		t.Run(tc.page+"/"+tc.version, func(t *testing.T) {
			s := fakeCEF(t, tc.page, tc.version, func(*websocket.Conn, uint64) { t.Error("snapshot on unverified bridge") })
			c, err := connect(context.Background(), s.URL+"/json")
			if err == nil {
				c.Close()
				t.Fatal("unverified bridge accepted")
			}
			if !errors.Is(err, ErrVersionUnsupported) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("want ErrVersionUnsupported, got %v", err)
			}
		})
	}
	// Suffix agreement is case-insensitive: the page directory is lower-cased.
	s := fakeCEF(t, "ug1", "MUSIC_9.6.0.0_UG1", func(*websocket.Conn, uint64) {})
	c, err := connect(context.Background(), s.URL+"/json")
	if err != nil {
		t.Fatalf("case-only difference rejected: %v", err)
	}
	c.Close()
	// An unverified page directory is rejected before the bridge is queried.
	s = fakeCEF(t, "W1/../../9.7.0.0_W1", "MUSIC_9.6.0.0_W1", func(*websocket.Conn, uint64) { t.Error("snapshot on unverified page") })
	if c, err := connect(context.Background(), s.URL+"/json"); !errors.Is(err, ErrVersionUnsupported) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("unverified page directory: %v", err)
	}
}

// Connection-level failures stay distinct from version rejections.
func TestTransportUnreachableIsNotVersionRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"no targets", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) }},
		{"bad json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.h)
			defer s.Close()
			if _, err := connect(context.Background(), s.URL+"/json"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrVersionUnsupported) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
		})
	}
	s := httptest.NewServer(http.NotFoundHandler())
	endpoint := s.URL + "/json"
	s.Close()
	if _, err := connect(context.Background(), endpoint); err == nil || errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("refused connection: %v", err)
	}
}

func TestTransportRejectsThrownEvaluation(t *testing.T) {
	s := fakeCEF(t, "W1", "MUSIC_9.6.0.0_W1", func(c *websocket.Conn, id uint64) {
		_ = c.WriteJSON(map[string]interface{}{"id": id, "result": map[string]interface{}{"wasThrown": true, "result": map[string]interface{}{"value": "bad"}}})
	})
	c, err := connect(context.Background(), s.URL+"/json")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("thrown exception accepted: %v", err)
	}
}
