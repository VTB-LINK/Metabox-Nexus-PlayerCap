package assoc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

type assocRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type assocRUMEvent struct {
	Event string
	Props map[string]any
}

func startAssocRUMCollector(t *testing.T) func() (string, []assocRUMEvent) {
	t.Helper()
	var mu sync.Mutex
	var raw []string
	var evs []assocRUMEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed assocRUMBatch
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			raw = append(raw, string(body))
			for _, ev := range parsed.Batch {
				evs = append(evs, assocRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_kugou_assoc", srv.URL)
	t.Cleanup(restore)
	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: srv.Client(),
	})
	rum.Start()
	t.Cleanup(func() { rum.Shutdown(time.Second) })

	return func() (string, []assocRUMEvent) {
		rum.Flush(2 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		out := make([]assocRUMEvent, len(evs))
		copy(out, evs)
		return strings.Join(raw, "\n"), out
	}
}

type fakeBadVtableMem struct{}

func (fakeBadVtableMem) ReadSnapshot() (memSnapshot, memStatus) {
	return memSnapshot{}, memStatusBadVtable
}
func (fakeBadVtableMem) PID() uint32 { return 1235 }
func (fakeBadVtableMem) Close()      {}

// TestAssocRUMMemFallbackAndWALCompat 验证 assoc/reader.go 中的内存降级到 DB 与 SQLite WAL/锁冲突信号，
// 且按会话限频不刷屏。
func TestAssocRUMMemFallbackAndWALCompat(t *testing.T) {
	flush := startAssocRUMCollector(t)

	// 1. memOpenUnsupported 降级 DB
	w1 := &worker{
		r:    &Reader{wake: make(chan struct{}, 1), results: make(chan Result, 1)},
		conf: &watchdog.Confirmed{PID: 1234, Version: "20.1.22.27795"},
		d: deps{
			openMem: func(uint32) (memSource, memOpenKind, string, error) {
				return nil, memOpenUnsupported, "fp_x", nil
			},
		},
	}
	w1.tryOpenMem()

	// 2. maxBadVtable 连续失败降级 DB + WAL/锁冲突（KindTransient 重复触发只报 1 次）
	w2 := &worker{
		r:    &Reader{wake: make(chan struct{}, 1), results: make(chan Result, 1)},
		conf: &watchdog.Confirmed{PID: 1235, Version: "20.1.22.27795", DataDir: `C:\Users\Alice\AppData\KuGou`},
		hash: "ABC",
		mem:  fakeBadVtableMem{},
		d: deps{
			candidates: func(context.Context, string, string) (collected, error) {
				return collected{}, errors.New("database locked transient")
			},
		},
	}
	for i := 0; i < maxBadVtable; i++ {
		w2.pollMem(context.Background())
	}
	// 再次触发 transient 锁冲突，应被本会话限频去重
	w2.parse(context.Background())

	// 3. WAL / UnsupportedDB 触发
	w3 := &worker{
		r:    &Reader{wake: make(chan struct{}, 1), results: make(chan Result, 1)},
		conf: &watchdog.Confirmed{PID: 1236, Version: "20.1.22.27795", DataDir: `C:\Users\Alice\AppData\KuGou`},
		hash: "DEF",
		d: deps{
			candidates: func(context.Context, string, string) (collected, error) {
				return collected{}, kgdb.ErrUnsupportedDB
			},
		},
	}
	origReport := reportOnce
	reportOnce = func(string, string, map[string]string, map[string]any) {}
	t.Cleanup(func() { reportOnce = origReport })
	w3.parse(context.Background())

	raw, evs := flush()
	if strings.Contains(raw, "Alice") || strings.Contains(raw, "KGMusicV3.db") {
		t.Errorf("assoc RUM 泄露了敏感路径：%s", raw)
	}

	var fallbackOpen, fallbackVtable, walUnsupported, lockTransient int
	for _, ev := range evs {
		if ev.Event != "player_compat_signal" {
			continue
		}
		sig, _ := ev.Props["signal"].(string)
		dc, _ := ev.Props["detail_code"].(string)
		switch sig {
		case "kugou_mem_fallback_to_db":
			if dc == "open_unsupported" {
				fallbackOpen++
			} else if dc == "bad_vtable" {
				fallbackVtable++
			}
		case "sqlite_wal_or_lock_conflict":
			if dc == "wal_or_unsupported" {
				walUnsupported++
			} else if dc == "transient_lock" {
				lockTransient++
			}
		}
	}
	if fallbackOpen != 1 || fallbackVtable != 1 || walUnsupported != 1 || lockTransient != 1 {
		t.Fatalf("compat 信号计数异常：open=%d vtable=%d wal=%d transient=%d",
			fallbackOpen, fallbackVtable, walUnsupported, lockTransient)
	}
}
