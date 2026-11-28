package watchdog

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/rum"
)

func TestEnsureInspectorInterventionRUM(t *testing.T) {
	var (
		mu     sync.Mutex
		events []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed struct {
				Batch []struct {
					Event      string         `json:"event"`
					Properties map[string]any `json:"properties"`
				} `json:"batch"`
			}
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			for _, ev := range parsed.Batch {
				if ev.Event == "player_intervention" {
					events = append(events, ev.Properties)
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_soda_wd", srv.URL)
	t.Cleanup(restore)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: srv.Client(),
	})
	rum.Start()
	t.Cleanup(func() { rum.Shutdown(time.Second) })

	// 1. 门禁放行、9229 未开且目标 PID 不存在 -> activateInspector 报错 -> outcome: "err"
	stubGate(t, nil, false)
	if err := EnsureInspector(noSodaPID); err == nil {
		t.Fatal("不存在的 PID 应返回激活错误")
	}

	// 2. 等待 9229 就绪：ok 与 port_timeout
	stubGate(t, nil, true)
	if err := waitInspectorReady(noSodaPID, time.Now(), 100*time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("9229 已就绪应返回 nil，实得 %v", err)
	}
	stubGate(t, nil, false)
	if err := waitInspectorReady(noSodaPID, time.Now(), 15*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Fatal("9229 未就绪超时应返回错误")
	}

	rum.Flush(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()

	outcomes := map[string]int{}
	for _, props := range events {
		if props["player"] == "sodamusic" && props["kind"] == "sodamusic_activate_9229" {
			if out, ok := props["outcome"].(string); ok {
				outcomes[out]++
			}
		}
	}
	if outcomes["err"] != 1 || outcomes["ok"] != 1 || outcomes["port_timeout"] != 1 {
		t.Fatalf("sodamusic_activate_9229 outcome 计数异常：%v", outcomes)
	}
}
