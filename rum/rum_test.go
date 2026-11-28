package rum

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type capturedEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	UUID       string         `json:"uuid"`
	Timestamp  string         `json:"timestamp"`
	Properties map[string]any `json:"properties"`
}

type capturedBatch struct {
	APIKey string          `json:"api_key"`
	Batch  []capturedEvent `json:"batch"`
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)
	fc.mu.Unlock()
}

func withInjectedCreds(t *testing.T, key, h string) {
	t.Helper()
	oldKey, oldHost := apiKey, host
	apiKey, host = key, h
	t.Cleanup(func() {
		Shutdown(200 * time.Millisecond)
		apiKey, host = oldKey, oldHost
		resetForTest()
	})
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）：%s", timeout, desc)
}

// TestDisabledWhenNonReleaseOrEmptyDistinctID 校验非发布版、DistinctID 为空、或未注入 apiKey/host 时，
// 所有接口均为纯内存 no-op，/flags 与 /batch 请求数恒为 0。
//
// 变异自证：放宽 IsRelease 判据、对空 DistinctID 退回随机 UUID 或在 Init 内提前发请求，本测试变红。
func TestDisabledWhenNonReleaseOrEmptyDistinctID(t *testing.T) {
	var flagsHits, batchHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			flagsHits.Add(1)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			batchHits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cases := []struct {
		name       string
		key        string
		host       string
		isRelease  bool
		distinctID string
		wantConf   bool
	}{
		{"非发布版禁用", "phc_test", srv.URL, false, "0123456789abcdef0123456789abcdef", true},
		{"空DistinctID禁用", "phc_test", srv.URL, true, "", true},
		{"空白DistinctID禁用", "phc_test", srv.URL, true, "   ", true},
		{"空apiKey禁用", "", srv.URL, true, "0123456789abcdef0123456789abcdef", false},
		{"空host禁用", "phc_test", "", true, "0123456789abcdef0123456789abcdef", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withInjectedCreds(t, tc.key, tc.host)
			if got := Configured(); got != tc.wantConf {
				t.Errorf("Configured() = %v，期望 %v", got, tc.wantConf)
			}
			Init(Options{
				IsRelease:  tc.isRelease,
				AppVersion: "4.0.0",
				DistinctID: tc.distinctID,
				BaseProps:  Props{"os_build": 22631},
				HTTPClient: srv.Client(),
			})
			if Enabled() {
				t.Errorf("禁用条件下 Enabled() 必须为 false")
			}
			Start()
			Track(CatLifecycle, "app_start", Props{"a": 1})
			TrackOnce("k1", CatPlayer, "player_blocked", Props{"player": "kugou"})
			Flush(100 * time.Millisecond)
			Shutdown(100 * time.Millisecond)

			if flagsHits.Load() != 0 || batchHits.Load() != 0 {
				t.Errorf("禁用条件下发起了网络请求：flags=%d batch=%d", flagsHits.Load(), batchHits.Load())
			}
		})
	}
}

// TestParseFlagsResponseVariants 校验 /flags/?v=2 响应的多种形态：
// v2 原生 flags 结构（线上真实 JSON、字符串/内嵌对象 payload、enabled:false、payload 内 enabled:false、
// failed:true、errorsWhileComputingFlags、quotaLimited、null/缺失/畸形防御）、
// 旧版 featureFlags/featureFlagPayloads 向后兼容，以及采样率钳制与 NaN/Inf 保护。
//
// 变异自证：不识别顶层 flags、未防 null/failed/errorsWhileComputingFlags/quotaLimited 或破坏旧版兼容，本测试变红。
func TestParseFlagsResponseVariants(t *testing.T) {
	prev := &remoteConfig{
		enabled:               true,
		personProfilesOnStart: false,
		disabledCategories:    map[string]struct{}{"diag_fine": {}},
		disabledEvents:        map[string]struct{}{"http_snapshot_summary": {}},
		sampleRates:           map[string]float64{"lyric_track": 0.5},
	}

	// 1. 线上 /flags/?v=2 真实响应（仅含顶层 flags，无旧版 featureFlags/featureFlagPayloads）
	prodV2Body := []byte(`{
		"errorsWhileComputingFlags": false,
		"flags": {
			"playercap-rum-config": {
				"key": "playercap-rum-config",
				"enabled": true,
				"variant": null,
				"reason": {"code": "condition_match", "condition_index": 0, "description": "Matched condition set 1"},
				"metadata": {
					"id": 1138933,
					"version": 1,
					"description": null,
					"payload": "{\n  \"enabled\": true,\n  \"disabled_categories\": [],\n  \"disabled_events\": [],\n  \"sample_rates\": {}\n}",
					"has_experiment": false
				}
			}
		},
		"requestId": "a166ad32-f290-4f3c-be19-1fe1bd82c2e3",
		"evaluatedAt": 1795809474029
	}`)
	cfgProd, ok := parseFlagsResponse(prodV2Body, prev)
	if !ok {
		t.Fatalf("线上真实 v2 响应应返回 ok=true")
	}
	if !cfgProd.enabled || cfgProd.personProfilesOnStart || len(cfgProd.disabledCategories) != 0 || len(cfgProd.disabledEvents) != 0 || len(cfgProd.sampleRates) != 0 {
		t.Errorf("线上真实 v2 响应解析结果不符：%+v", cfgProd)
	}

	// 2. v2 字符串 payload（含字段、采样率钳制）与非 feature_flags 的 quotaLimited 不误拦
	v2StrBody := []byte(`{
		"errorsWhileComputingFlags": false,
		"quotaLimited": ["recordings"],
		"flags": {
			"playercap-rum-config": {
				"key": "playercap-rum-config",
				"enabled": true,
				"metadata": {
					"payload": "{\"enabled\":true,\"person_profiles_on_start\":true,\"disabled_categories\":[\"audio_effect\"],\"disabled_events\":[\"track_lyric_updated\"],\"sample_rates\":{\"lyric_track\":0.25,\"neg\":-0.5,\"over\":3.5}}"
				}
			}
		}
	}`)
	cfgV2Str, ok := parseFlagsResponse(v2StrBody, prev)
	if !ok {
		t.Fatalf("v2 字符串 payload 应返回 ok=true")
	}
	if !cfgV2Str.enabled || !cfgV2Str.personProfilesOnStart {
		t.Errorf("v2 字符串 payload enabled/personProfilesOnStart 不符：%+v", cfgV2Str)
	}
	if _, off := cfgV2Str.disabledCategories["audio_effect"]; !off {
		t.Errorf("v2 disabled_categories 未包含 audio_effect")
	}
	if _, off := cfgV2Str.disabledEvents["track_lyric_updated"]; !off {
		t.Errorf("v2 disabled_events 未包含 track_lyric_updated")
	}
	if cfgV2Str.sampleRates["lyric_track"] != 0.25 || cfgV2Str.sampleRates["neg"] != 0.0 || cfgV2Str.sampleRates["over"] != 1.0 {
		t.Errorf("v2 sample_rates 钳制结果不符：%v", cfgV2Str.sampleRates)
	}

	// 3. v2 内嵌对象 payload
	v2ObjBody := []byte(`{
		"flags": {
			"playercap-rum-config": {
				"enabled": true,
				"metadata": {
					"payload": {
						"disabled_categories": ["route_client"],
						"sample_rates": {"diag_fine": 0.1}
					}
				}
			}
		}
	}`)
	cfgV2Obj, ok := parseFlagsResponse(v2ObjBody, cfgV2Str)
	if !ok {
		t.Fatalf("v2 内嵌对象 payload 应返回 ok=true")
	}
	if !cfgV2Obj.enabled || cfgV2Obj.personProfilesOnStart {
		t.Errorf("v2 内嵌对象缺省字段应回内置默认：%+v", cfgV2Obj)
	}
	if _, off := cfgV2Obj.disabledCategories["route_client"]; !off || cfgV2Obj.sampleRates["diag_fine"] != 0.1 {
		t.Errorf("v2 内嵌对象解析异常：%+v", cfgV2Obj)
	}

	// 4. v2 enabled:false 应返回 prev.cloneWithEnabled(false)（保留 prev 其他策略）
	for _, body := range [][]byte{
		[]byte(`{"flags":{"playercap-rum-config":{"enabled":false}}}`),
		[]byte(`{"flags":{"playercap-rum-config":{"enabled":false,"metadata":null}}}`),
		[]byte(`{"flags":{"playercap-rum-config":{"enabled":false,"metadata":{"payload":{"enabled":true}}}}}`),
	} {
		cfgOff, ok := parseFlagsResponse(body, prev)
		if !ok || cfgOff.enabled {
			t.Errorf("v2 enabled:false 必须关闭：ok=%v enabled=%v body=%s", ok, cfgOff.enabled, string(body))
		}
		if _, hasDiag := cfgOff.disabledCategories["diag_fine"]; !hasDiag || !prev.enabled {
			t.Errorf("v2 enabled:false 应克隆 prev 而不原地改写 prev：got=%+v prev=%+v", cfgOff, prev)
		}
	}

	// 5. v2 metadata.payload 内含 {"enabled":false} 应将 cfg.enabled 置为 false
	for _, body := range [][]byte{
		[]byte(`{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":false,\"disabled_categories\":[]}"}}}}`),
		[]byte(`{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":{"enabled":false}}}}}`),
	} {
		cfgOff, ok := parseFlagsResponse(body, prev)
		if !ok || cfgOff.enabled {
			t.Errorf("v2 payload enabled:false 应返回 enabled=false：ok=%v cfg=%+v", ok, cfgOff)
		}
	}

	// 6. v2 异常与防御场景均保留 prev 且返回 ok=false
	for _, tc := range []struct {
		name string
		body string
	}{
		{"v2_errors_computing_enabled", `{"errorsWhileComputingFlags":true,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":{"enabled":false}}}}}`},
		{"v2_errors_computing_disabled", `{"errorsWhileComputingFlags":true,"flags":{"playercap-rum-config":{"enabled":false}}}`},
		{"v2_quota_limited_single", `{"quotaLimited":["feature_flags"],"flags":{"playercap-rum-config":{"enabled":false}}}`},
		{"v2_quota_limited_multi", `{"quotaLimited":["recordings","feature_flags"],"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":{"enabled":false}}}}}`},
		{"v2_empty_flags", `{"flags":{}}`},
		{"v2_empty_flags_no_legacy_fallback", `{"flags":{},"featureFlags":{"playercap-rum-config":false}}`},
		{"v2_missing_key", `{"flags":{"other-flag":{"enabled":true}}}`},
		{"v2_null_entry", `{"flags":{"playercap-rum-config":null}}`},
		{"v2_non_object_entry_bool", `{"flags":{"playercap-rum-config":true}}`},
		{"v2_non_object_entry_array", `{"flags":{"playercap-rum-config":[1,2]}}`},
		{"v2_failed_true_enabled", `{"flags":{"playercap-rum-config":{"enabled":true,"failed":true,"metadata":{"payload":{"enabled":false}}}}}`},
		{"v2_failed_true_disabled", `{"flags":{"playercap-rum-config":{"enabled":false,"failed":true}}}`},
		{"v2_missing_enabled", `{"flags":{"playercap-rum-config":{"metadata":{"payload":{"enabled":false}}}}}`},
		{"v2_null_enabled", `{"flags":{"playercap-rum-config":{"enabled":null,"metadata":{"payload":{"enabled":false}}}}}`},
		{"v2_missing_metadata", `{"flags":{"playercap-rum-config":{"enabled":true}}}`},
		{"v2_null_metadata", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":null}}}`},
		{"v2_missing_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{}}}}`},
		{"v2_null_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":null}}}}`},
		{"v2_empty_string_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":""}}}}`},
		{"v2_null_string_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"null"}}}}`},
		{"v2_malformed_string_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{not-json"}}}}`},
		{"v2_non_object_payload", `{"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":[1,2,3]}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFlagsResponse([]byte(tc.body), prev)
			if ok {
				t.Errorf("%s 不应视为有效更新", tc.name)
			}
			if got != prev || !got.enabled {
				t.Errorf("%s 应保持上一次有效配置 prev(enabled=true) 不变：got=%+v", tc.name, got)
			}
		})
	}

	// 7. 旧版兼容：JSON 字符串 payload + 采样率越界钳制（顶层无 flags）
	strBody := []byte(`{
		"featureFlags": {"playercap-rum-config": true},
		"featureFlagPayloads": {
			"playercap-rum-config": "{\"enabled\":true,\"person_profiles_on_start\":true,\"disabled_categories\":[\"audio_effect\"],\"disabled_events\":[\"track_lyric_updated\"],\"sample_rates\":{\"lyric_track\":0.25,\"neg\":-0.5,\"over\":3.5}}"
		}
	}`)
	cfg1, ok := parseFlagsResponse(strBody, prev)
	if !ok {
		t.Fatalf("合法字符串 payload 应返回 ok=true")
	}
	if !cfg1.enabled || !cfg1.personProfilesOnStart {
		t.Errorf("enabled/personProfilesOnStart 解析错误：%+v", cfg1)
	}
	if _, off := cfg1.disabledCategories["audio_effect"]; !off {
		t.Errorf("disabled_categories 未包含 audio_effect")
	}
	if _, off := cfg1.disabledEvents["track_lyric_updated"]; !off {
		t.Errorf("disabled_events 未包含 track_lyric_updated")
	}
	if cfg1.sampleRates["lyric_track"] != 0.25 || cfg1.sampleRates["neg"] != 0.0 || cfg1.sampleRates["over"] != 1.0 {
		t.Errorf("sample_rates 钳制结果不符：%v", cfg1.sampleRates)
	}

	// 8. 旧版兼容：直接内嵌 JSON 对象 payload（缺省字段回内置默认，且顶层 flags:null 时也兼容）
	objBody := []byte(`{
		"flags": null,
		"featureFlags": {"playercap-rum-config": true},
		"featureFlagPayloads": {
			"playercap-rum-config": {
				"disabled_categories": ["route_client"],
				"sample_rates": {"diag_fine": 0.1}
			}
		}
	}`)
	cfg2, ok := parseFlagsResponse(objBody, cfg1)
	if !ok {
		t.Fatalf("合法对象 payload 应返回 ok=true")
	}
	if !cfg2.enabled || cfg2.personProfilesOnStart {
		t.Errorf("缺省字段应回内置默认（enabled=true, person_profiles_on_start=false）：%+v", cfg2)
	}
	if _, off := cfg2.disabledCategories["route_client"]; !off {
		t.Errorf("对象 payload 的 disabled_categories 未生效")
	}
	if cfg2.sampleRates["diag_fine"] != 0.1 {
		t.Errorf("对象 payload 的 sample_rates 未生效：%v", cfg2.sampleRates)
	}

	// 9. 旧版兼容：featureFlags["playercap-rum-config"] == false 优先关闭（即使 payload 缺失或 enabled:true）
	for _, body := range [][]byte{
		[]byte(`{"featureFlags":{"playercap-rum-config":false},"featureFlagPayloads":{}}`),
		[]byte(`{"featureFlags":{"playercap-rum-config":false},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`),
		[]byte(`{"featureFlags":{"playercap-rum-config":false},"featureFlagPayloads":{"playercap-rum-config":"{bad"}}`),
	} {
		cfgOff, ok := parseFlagsResponse(body, cfg2)
		if !ok || cfgOff.enabled {
			t.Errorf("flag=false 时必须关闭：ok=%v enabled=%v body=%s", ok, cfgOff.enabled, string(body))
		}
	}

	// 10. 旧版兼容：errorsWhileComputingFlags / quotaLimited / null / 缺失 / 畸形均保留上一次有效配置不变
	for _, tc := range []struct {
		name string
		body string
	}{
		{"legacy_errors_computing", `{"errorsWhileComputingFlags":true,"featureFlags":{"playercap-rum-config":false}}`},
		{"legacy_quota_limited", `{"quotaLimited":["feature_flags"],"featureFlags":{"playercap-rum-config":false}}`},
		{"null_flag", `{"featureFlags":{"playercap-rum-config":null}}`},
		{"null_flag_empty_payloads", `{"featureFlags":{"playercap-rum-config":null},"featureFlagPayloads":{}}`},
		{"null_payload", `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":null}}`},
		{"missing_payload", `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{}}`},
		{"malformed_string_payload", `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{not-json"}}`},
		{"non_object_payload", `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":[1,2,3]}}`},
		{"malformed_body", `{broken json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFlagsResponse([]byte(tc.body), prev)
			if ok {
				t.Errorf("%s 不应视为有效更新", tc.name)
			}
			if got != prev || !got.enabled {
				t.Errorf("%s 应保持上一次有效配置 prev(enabled=true) 不变：got=%+v", tc.name, got)
			}
		})
	}

	// 11. NaN / Inf 采样率忽略并按默认 1.0 处理
	rates := normalizeSampleRates(map[string]float64{
		"nan":  math.NaN(),
		"pinf": math.Inf(1),
		"ninf": math.Inf(-1),
		"half": 0.5,
	})
	if _, exists := rates["nan"]; exists {
		t.Errorf("NaN 采样率应忽略")
	}
	if _, exists := rates["pinf"]; exists {
		t.Errorf("+Inf 采样率应忽略")
	}
	if _, exists := rates["ninf"]; exists {
		t.Errorf("-Inf 采样率应忽略")
	}
	if rates["half"] != 0.5 {
		t.Errorf("合法采样率丢失：%v", rates)
	}
}

// TestRemoteConfigDisablesAndSamplesExceptLifecycle 校验总开关、分类关闭、事件关闭、采样率控制、
// CatLifecycle 豁免 sample_rates、Person Profile 保护及 pre-Start 缓存事件首拉复筛。
//
// 变异自证：对 CatLifecycle 应用 sample_rates、跳过 pre-Start 复筛或错误开启其他事件的 $process_person_profile，本测试变红。
func TestRemoteConfigDisablesAndSamplesExceptLifecycle(t *testing.T) {
	t.Run("分类事件开关与采样及Lifecycle豁免和PreStart复筛", func(t *testing.T) {
		var mu sync.Mutex
		var received []capturedEvent
		var flagsReqBody map[string]any

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/flags"):
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				_ = json.Unmarshal(b, &flagsReqBody)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"featureFlags": {"playercap-rum-config": true},
					"featureFlagPayloads": {
						"playercap-rum-config": {
							"enabled": true,
							"person_profiles_on_start": true,
							"disabled_categories": ["audio_effect"],
							"disabled_events": ["track_lyric_updated"],
							"sample_rates": {
								"lifecycle": 0.0,
								"app_start": 0.0,
								"diag_fine": 0.0
							}
						}
					}
				}`))
			case strings.HasPrefix(r.URL.Path, "/batch"):
				var req capturedBatch
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				received = append(received, req.Batch...)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_live_key", srv.URL+"/")
		clk := newFakeClock(time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			BaseProps:  Props{"arch": "x64", "os_build": 22631},
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})

		// Pre-Start 入队事件：应受首拉配置复筛管控（diag_fine 被滤，lifecycle 与 player_health 保留）
		Track(CatLifecycle, "app_start", Props{"post_rebuild": false})
		Track(CatDiagFine, "scan_perf_sample", Props{"scan_kind": "pre_start"})
		Track(CatPlayer, "player_session", Props{"player": "qqmusic", "phase": "pre_start"})

		Start()
		waitUntil(t, 2*time.Second, func() bool {
			return current.Load().flagsReady.Load()
		}, "等待首拉 /flags 完成")

		// 首拉完成后继续发各类事件
		Track(CatLifecycle, "app_heartbeat", Props{"uptime_sec": 120})
		Track(CatDiagFine, "playback_clock_guard", Props{"player": "qqmusic"})
		Track(CatAudioEffect, "audio_capture_session", Props{"player": "kugou"})
		Track(CatLyricTrack, "track_lyric_updated", Props{"player": "kugou"})
		Track(CatLyricTrack, "track_recognized", Props{"player": "wesing"})
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "signal_term"})

		Shutdown(2 * time.Second)

		mu.Lock()
		defer mu.Unlock()

		if flagsReqBody["api_key"] != "phc_live_key" || flagsReqBody["distinct_id"] != "abcdef0123456789abcdef0123456789" {
			t.Errorf("/flags 请求体不符：%v", flagsReqBody)
		}

		gotByName := map[string]capturedEvent{}
		for _, ev := range received {
			gotByName[ev.Event] = ev
		}

		for _, mustHave := range []string{"app_start", "app_heartbeat", "app_exit", "player_session", "track_recognized"} {
			if _, ok := gotByName[mustHave]; !ok {
				t.Errorf("期望事件 %q 送达，实际收到：%v", mustHave, gotByName)
			}
		}
		for _, mustDrop := range []string{"scan_perf_sample", "playback_clock_guard", "audio_capture_session", "track_lyric_updated"} {
			if _, ok := gotByName[mustDrop]; ok {
				t.Errorf("应被远端配置过滤的事件 %q 不应送达", mustDrop)
			}
		}

		// 验证 Person Profile 计费保护：仅 app_start 为 true 且带 $set/$set_once
		startProps := gotByName["app_start"].Properties
		if startProps["$process_person_profile"] != true {
			t.Errorf("person_profiles_on_start=true 时 app_start 的 $process_person_profile 应为 true：%v", startProps)
		}
		if _, ok := startProps["$set"].(map[string]any); !ok {
			t.Errorf("app_start 缺少 $set：%v", startProps)
		}
		if _, ok := startProps["$set_once"].(map[string]any); !ok {
			t.Errorf("app_start 缺少 $set_once：%v", startProps)
		}
		if startProps["$lib"] != "playercap-rum" || startProps["$lib_version"] != "4.0.0" || startProps["app_version"] != "4.0.0" {
			t.Errorf("app_start 系统属性缺失：%v", startProps)
		}

		for _, other := range []string{"app_heartbeat", "app_exit", "track_recognized"} {
			p := gotByName[other].Properties
			if p["$process_person_profile"] != false {
				t.Errorf("事件 %q 的 $process_person_profile 必须为 false：%v", other, p)
			}
			if _, ok := p["$set"]; ok {
				t.Errorf("非 app_start 事件 %q 不得附带 $set", other)
			}
		}
	})

	t.Run("远端总开关关闭拦截PreStart事件", func(t *testing.T) {
		var batchHits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":false}}`))
				return
			}
			batchHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
		})
		Track(CatLifecycle, "app_start", Props{"a": 1})
		Track(CatPlayer, "player_session", Props{"player": "kugou"})

		Start()
		waitUntil(t, 2*time.Second, func() bool {
			return current.Load().flagsReady.Load()
		}, "等待首拉关闭配置")

		if Enabled() {
			t.Errorf("远端关闭后 Enabled() 应为 false")
		}
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "signal_term"})
		Shutdown(1 * time.Second)

		if batchHits.Load() != 0 {
			t.Errorf("总开关关闭后仍发起了 /batch/ 请求：%d 次", batchHits.Load())
		}
	})

	t.Run("v2真实响应payload内enabled为false时停发batch", func(t *testing.T) {
		var batchHits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"errorsWhileComputingFlags": false,
					"flags": {
						"playercap-rum-config": {
							"key": "playercap-rum-config",
							"enabled": true,
							"variant": null,
							"reason": {"code": "condition_match", "condition_index": 0, "description": "Matched condition set 1"},
							"metadata": {
								"id": 1138933,
								"version": 1,
								"description": null,
								"payload": "{\n  \"enabled\": false,\n  \"disabled_categories\": [],\n  \"disabled_events\": [],\n  \"sample_rates\": {}\n}",
								"has_experiment": false
							}
						}
					},
					"requestId": "a166ad32-f290-4f3c-be19-1fe1bd82c2e3",
					"evaluatedAt": 1795809474029
				}`))
				return
			}
			batchHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
		})
		Track(CatLifecycle, "app_start", Props{"a": 1})
		Track(CatPlayer, "player_session", Props{"player": "kugou"})

		Start()
		waitUntil(t, 2*time.Second, func() bool {
			return current.Load().flagsReady.Load()
		}, "等待首拉 v2 关闭配置")

		if Enabled() {
			t.Errorf("v2 payload enabled:false 后 Enabled() 应为 false")
		}
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "signal_term"})
		Shutdown(1 * time.Second)

		if batchHits.Load() != 0 {
			t.Errorf("v2 payload enabled:false 后仍发起了 /batch/ 请求：%d 次", batchHits.Load())
		}
	})
}

// TestLifecycleMultiDayBacklogAndEviction 校验核心生命周期保底槽：
// 1. 连续 503 且跨 10 个日历日后恢复 200，app_start 与全部 10 日心跳以原 uuid/timestamp 完整补发；
// 2. 连续 35 个日历日断网时仅淘汰最旧 2 日心跳，保留 app_start 与 app_exit，并递增 lifecycle_evicted；
// 3. 服务端返回 400/413 时移除已拒发事件，不陷入死循环。
//
// 变异自证：503 时丢弃 lifecycleBuf、重生成 uuid、淘汰 app_start 或 400/413 无限重发，本测试变红。
func TestLifecycleMultiDayBacklogAndEviction(t *testing.T) {
	t.Run("10日503断网恢复后原uuid与原timestamp完整送达", func(t *testing.T) {
		var mu sync.Mutex
		var failMode atomic.Bool
		failMode.Store(true)
		var failedStartUUIDs []string
		var failedStartTSs []string
		var failedHits atomic.Int64
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if failMode.Load() {
				mu.Lock()
				for _, ev := range req.Batch {
					if ev.Event == "app_start" {
						failedStartUUIDs = append(failedStartUUIDs, ev.UUID)
						failedStartTSs = append(failedStartTSs, ev.Timestamp)
					}
				}
				mu.Unlock()
				failedHits.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 16)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		waitBatchDone := func(desc string) {
			t.Helper()
			select {
			case <-batchDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("等待超时（2s）：%s", desc)
			}
		}

		// 启动前先构造 app_start 与跨 10 个日历日的心跳（含同日重复），确保首轮 503 携带完整积压并精准验证 20s/40s 退避。
		Track(CatLifecycle, "app_start", Props{"post_rebuild": false})
		for day := 1; day <= 10; day++ {
			clk.Advance(24 * time.Hour)
			dateStr := clk.Now().Format("2006-01-02")
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": dateStr, "day": day})
			// 同日再发一次，应幂等去重
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": dateStr, "day": day})
		}

		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")
		waitBatchDone("等待第 1 次 503 结算完成")
		if got := failedHits.Load(); got != 1 {
			t.Fatalf("第 1 轮后期望 failedHits == 1，实际 %d", got)
		}

		// 首阶退避 20s：前 19s 不得重发
		clk.Advance(19 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if got := failedHits.Load(); got != 1 {
			t.Fatalf("503 后仅推进 19s 不应重发，实际 failedHits=%d", got)
		}

		// 满 20s 后触发第 2 次 503，且携带相同原 uuid 与 timestamp
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待第 2 次 503 结算完成")
		if got := failedHits.Load(); got != 2 {
			t.Fatalf("满 20s 后期望 failedHits == 2，实际 %d", got)
		}
		mu.Lock()
		if len(failedStartUUIDs) != 2 || failedStartUUIDs[0] == "" || failedStartUUIDs[0] != failedStartUUIDs[1] || failedStartTSs[0] != failedStartTSs[1] {
			t.Fatalf("第 2 次 503 重试未保留原 uuid/timestamp：uuids=%v tss=%v", failedStartUUIDs, failedStartTSs)
		}
		failedStartUUID, failedStartTS := failedStartUUIDs[0], failedStartTSs[0]
		mu.Unlock()

		// 网络恢复 200，二阶退避 40s：推进 39s 不得补发
		failMode.Store(false)
		clk.Advance(39 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		deliveredAt39s := len(delivered)
		mu.Unlock()
		if deliveredAt39s != 0 {
			t.Fatalf("二阶退避 40s 内（39s）不应重发，实际已送达 %d 条", deliveredAt39s)
		}

		// 满 40s 后触发补发，全部 11 条一次送达
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待二阶退避满 40s 后补发结算完成")

		Shutdown(1 * time.Second)

		mu.Lock()
		defer mu.Unlock()

		if len(delivered) != 11 {
			t.Fatalf("期望送达 11 条（1 app_start + 10 app_heartbeat），实际 %d 条", len(delivered))
		}
		seenDates := map[string]bool{}
		seenUUIDs := map[string]bool{}
		var gotStart capturedEvent
		for _, ev := range delivered {
			if ev.UUID == "" || seenUUIDs[ev.UUID] {
				t.Errorf("事件 %q 缺少或重复 UUID：%q", ev.Event, ev.UUID)
			}
			seenUUIDs[ev.UUID] = true
			switch ev.Event {
			case "app_start":
				gotStart = ev
			case "app_heartbeat":
				d, _ := ev.Properties["local_date"].(string)
				if seenDates[d] {
					t.Errorf("重复送达同一日期心跳：%s", d)
				}
				seenDates[d] = true
			}
		}
		if gotStart.UUID == "" || gotStart.UUID != failedStartUUID || gotStart.Timestamp != failedStartTS {
			t.Errorf("app_start 补发未保留原 uuid/timestamp：got=(%s,%s) failed=(%s,%s)",
				gotStart.UUID, gotStart.Timestamp, failedStartUUID, failedStartTS)
		}
		if len(seenDates) != 10 {
			t.Errorf("期望送达 10 个不同日历日心跳，实际 %d 个：%v", len(seenDates), seenDates)
		}
	})

	t.Run("35日连续断网淘汰最旧2日并保留start和exit", func(t *testing.T) {
		var mu sync.Mutex
		var failMode atomic.Bool
		failMode.Store(true)
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			if failMode.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		c := current.Load()
		c.tickInterval = 10 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"boot": true})
		var allDates []string
		for i := 0; i < 35; i++ {
			dateStr := clk.Now().Format("2006-01-02")
			allDates = append(allDates, dateStr)
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": dateStr})
			clk.Advance(24 * time.Hour)
		}
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "signal_term"})

		failMode.Store(false)
		Shutdown(2 * time.Second)

		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != 2 {
			t.Errorf("期望 lifecycle_evicted == 2，实际 %v", byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 35 {
			t.Fatalf("期望送达 35 条（1 start + 33 heartbeat + 1 exit），实际 %d 条", len(delivered))
		}
		var hasStart, hasExit bool
		gotDates := map[string]bool{}
		for _, ev := range delivered {
			switch ev.Event {
			case "app_start":
				hasStart = true
			case "app_exit":
				hasExit = true
			case "app_heartbeat":
				d, _ := ev.Properties["local_date"].(string)
				gotDates[d] = true
			}
		}
		if !hasStart || !hasExit {
			t.Errorf("app_start(%v) 或 app_exit(%v) 被误淘汰", hasStart, hasExit)
		}
		if gotDates[allDates[0]] || gotDates[allDates[1]] {
			t.Errorf("最旧 2 日心跳（%s, %s）应被淘汰", allDates[0], allDates[1])
		}
		for _, d := range allDates[2:] {
			if !gotDates[d] {
				t.Errorf("较新日期心跳 %s 缺失", d)
			}
		}
	})

	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run("客户端错误放弃防死循环_"+http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/flags") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
					return
				}
				hits.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			withInjectedCreds(t, "phc_key", srv.URL)
			clk := newFakeClock(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
			Init(Options{
				IsRelease:  true,
				AppVersion: "4.0.0",
				DistinctID: "abcdef0123456789abcdef0123456789",
				HTTPClient: srv.Client(),
				Now:        clk.Now,
			})
			c := current.Load()
			c.tickInterval = 10 * time.Millisecond
			Start()
			waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

			Track(CatLifecycle, "app_start", Props{"a": 1})
			Flush(1 * time.Second)
			if hits.Load() != 1 {
				t.Fatalf("期望首轮发送 1 次，实际 %d 次", hits.Load())
			}

			// 推进时间并再次 Flush，已放弃的事件不应重发
			clk.Advance(5 * time.Minute)
			Flush(500 * time.Millisecond)
			Shutdown(500 * time.Millisecond)

			if hits.Load() != 1 {
				t.Errorf("收到 HTTP %d 后不应死循环重发，实际请求次数：%d", status, hits.Load())
			}
		})
	}

	t.Run("普通事件503重试1次仍失败后丢弃并计send_failed_events而lifecycle保留", func(t *testing.T) {
		var failMode atomic.Bool
		failMode.Store(true)
		var failedHits atomic.Int64
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			if failMode.Load() {
				failedHits.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 16)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		waitBatchDone := func(desc string) {
			t.Helper()
			select {
			case <-batchDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("等待超时（2s）：%s", desc)
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"a": 1})
		Track(CatPlayer, "player_session", Props{"idx": 1})
		Track(CatPlayer, "player_session", Props{"idx": 2})
		Track(CatPlayer, "player_session", Props{"idx": 3})
		// 确保 3 条普通事件全部进入 worker 待发队列后再推时钟，避免首轮 503 漏带尚未 drain 的普通事件。
		waitUntil(t, 2*time.Second, func() bool { return len(c.normalCh) == 0 }, "等待普通事件进入待发队列")

		// 第 1 次 503：普通事件应保留以待第 1 次重试
		clk.Advance(30 * time.Second)
		c.nudge()
		waitBatchDone("等待第 1 次 503 结算完成")
		if got := failedHits.Load(); got != 1 {
			t.Fatalf("第 1 轮后期望 failedHits == 1，实际 %d", got)
		}

		// 首阶退避 20s：前 19s 不得重发
		clk.Advance(19 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if got := failedHits.Load(); got != 1 {
			t.Fatalf("503 后仅推进 19s 不应重发，实际 failedHits=%d", got)
		}

		// 满 20s 触发第 2 次 503：普通事件重试 1 次仍失败，丢弃并计 send_failed_events=3
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待第 2 次 503 结算完成")
		if got := failedHits.Load(); got != 2 {
			t.Fatalf("第 2 轮后期望 failedHits == 2，实际 %d", got)
		}

		// 切回 200，二阶退避 40s：推进 39s 不得补发，满 40s 后仅 app_start 补发成功
		failMode.Store(false)
		clk.Advance(39 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		deliveredBefore40s := len(delivered)
		mu.Unlock()
		if deliveredBefore40s != 0 {
			t.Fatalf("二阶退避 40s 内（39s）不应补发，实际已送达 %d 条", deliveredBefore40s)
		}
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待 app_start 补发结算完成")

		Shutdown(1 * time.Second)

		_, byScope := DrainDropped()
		if byScope["send_failed_events"] != 3 {
			t.Errorf("期望 send_failed_events == 3，实际 %v", byScope)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].Event != "app_start" {
			t.Errorf("恢复后期望仅补发 app_start，实际：%v", delivered)
		}
	})
}

// TestRateBudget48KiBSplitAnd429RetryAfter 校验单条 8 KiB 丢弃、单批 50 条与 48 KiB 拆分、
// 20s 最小发送间隔限速，以及 HTTP 429 Retry-After（秒数、HTTP-date、无头）冷却与恢复。
//
// 变异自证：取消 48 KiB 拆分、取消 20s 间隔、放行 >8 KiB 事件或忽略 Retry-After，本测试变红。
func TestRateBudget48KiBSplitAnd429RetryAfter(t *testing.T) {
	t.Run("RetryAfter解析与钳制", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		cases := []struct {
			header string
			want   time.Duration
		}{
			{"", 65 * time.Second},
			{"invalid", 65 * time.Second},
			{"5", 20 * time.Second},
			{"-10", 20 * time.Second},
			{"-9223372036854775808", 20 * time.Second},
			{"45", 45 * time.Second},
			{"900", 300 * time.Second},
			{"9223372036854775807", 300 * time.Second},
			{"99999999999999999999999", 300 * time.Second},
			{now.Add(50 * time.Second).UTC().Format(http.TimeFormat), 50 * time.Second},
			{now.Add(-10 * time.Second).UTC().Format(http.TimeFormat), 20 * time.Second},
			{now.Add(2 * time.Hour).UTC().Format(http.TimeFormat), 300 * time.Second},
		}
		for _, tc := range cases {
			if got := parseRetryAfter(tc.header, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v，期望 %v", tc.header, got, tc.want)
			}
		}
	})

	t.Run("RateLimitReset解析与RetryAfter优先级", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		resetCases := []struct {
			header string
			want   time.Duration
		}{
			{"", 65 * time.Second},
			{"   ", 65 * time.Second},
			{"0", 65 * time.Second},
			{"000000000000000000000000", 65 * time.Second},
			{"-1", 65 * time.Second},
			{"-10", 65 * time.Second},
			{"-9223372036854775808", 65 * time.Second},
			{"-99999999999999999999999", 65 * time.Second},
			{"+45", 65 * time.Second},
			{"12.5", 65 * time.Second},
			{"invalid", 65 * time.Second},
			{"99999999999999999999999a", 65 * time.Second},
			{now.Add(50 * time.Second).UTC().Format(http.TimeFormat), 65 * time.Second},
			{"1", 20 * time.Second},
			{"5", 20 * time.Second},
			{"19", 20 * time.Second},
			{"20", 20 * time.Second},
			{"45", 45 * time.Second},
			{" 55 ", 55 * time.Second},
			{"300", 300 * time.Second},
			{"900", 300 * time.Second},
			{"9223372036854775807", 300 * time.Second},
			{"18446744073709551615", 300 * time.Second},
			{"99999999999999999999999", 300 * time.Second},
		}
		for _, tc := range resetCases {
			if got := parseRateLimitReset(tc.header); got != tc.want {
				t.Errorf("parseRateLimitReset(%q) = %v，期望 %v", tc.header, got, tc.want)
			}
		}

		// Retry-After 非空白时始终优先；仅在 Retry-After 空白时回退 RateLimit-Reset
		delayCases := []struct {
			retryAfter     string
			rateLimitReset string
			want           time.Duration
		}{
			{"45", "120", 45 * time.Second},
			{"5", "120", 20 * time.Second},
			{"invalid", "45", 65 * time.Second},
			{"", "45", 45 * time.Second},
			{"   ", "5", 20 * time.Second},
			{"", "99999999999999999999999", 300 * time.Second},
			{"", "0", 65 * time.Second},
			{"", "", 65 * time.Second},
		}
		for _, tc := range delayCases {
			if got := parseRateLimitDelay(tc.retryAfter, tc.rateLimitReset, now); got != tc.want {
				t.Errorf("parseRateLimitDelay(%q, %q) = %v，期望 %v", tc.retryAfter, tc.rateLimitReset, got, tc.want)
			}
		}
	})

	t.Run("120条突发与48KiB拆分和8KiB单条丢弃及20s最小间隔", func(t *testing.T) {
		type batchRecord struct {
			at    time.Time
			bytes int
			count int
		}
		var mu sync.Mutex
		var records []batchRecord
		var totalEvents int

		clk := newFakeClock(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req capturedBatch
			if err := json.Unmarshal(body, &req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			records = append(records, batchRecord{
				at:    clk.Now(),
				bytes: len(body),
				count: len(req.Batch),
			})
			totalEvents += len(req.Batch)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		// 1 条超大事件（超过 8 KiB：80 个字段 × 120 字节 ≈ 10 KiB）
		hugeProps := Props{}
		for i := 0; i < 80; i++ {
			hugeProps["k_"+strings.Repeat("x", 4)+string(rune('a'+i%26))+string(rune('0'+i/26))] = strings.Repeat("v", 120)
		}
		Track(CatPlayer, "huge_event", hugeProps)

		// 120 条正常事件，同时每条带约 1.5 KiB 属性以触发 48 KiB 与 50 条双上限拆分
		for i := 0; i < 120; i++ {
			p := Props{"seq": i}
			for j := 0; j < 12; j++ {
				p["f"+string(rune('a'+j))] = strings.Repeat("z", 110)
			}
			Track(CatLyricTrack, "track_recognized", p)
		}
		c.nudge()

		// 首批立即可发
		waitUntil(t, 2*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(records) == 1
		}, "等待第 1 批发出")

		// 仅推进 19s 不得发送第 2 批（20s 最小间隔）
		clk.Advance(19 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		gotAfter19s := len(records)
		mu.Unlock()
		if gotAfter19s != 1 {
			t.Fatalf("距上次发送仅 19s 不应发第 2 批，实际已发 %d 批", gotAfter19s)
		}

		// 逐次推进时间直到全部 120 条发完（每推进一次等待本批落盘后再推进，避免时钟与 HTTP handler 竞态）
		for step := 0; step < 20; step++ {
			mu.Lock()
			done := totalEvents >= 120
			prevBatches := len(records)
			mu.Unlock()
			if done {
				break
			}
			clk.Advance(30 * time.Second)
			c.nudge()
			waitUntil(t, 2*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(records) > prevBatches || totalEvents >= 120
			}, "等待下一批分片落盘")
		}

		Shutdown(1 * time.Second)

		_, byScope := DrainDropped()
		if byScope["oversize_dropped"] != 1 {
			t.Errorf("期望 oversize_dropped == 1，实际 %v", byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		if totalEvents != 120 {
			t.Fatalf("期望 120 条合法事件全部送达，实际 %d 条（共 %d 批）", totalEvents, len(records))
		}
		if len(records) < 4 {
			t.Errorf("120 条 × ~1.5KiB 应拆分为至少 4 批，实际仅 %d 批", len(records))
		}
		for i, rec := range records {
			if rec.count > 50 {
				t.Errorf("第 %d 批条数 %d 超过 50", i, rec.count)
			}
			if rec.bytes > 48*1024 {
				t.Errorf("第 %d 批字节数 %d 超过 48 KiB", i, rec.bytes)
			}
			if i > 0 {
				gap := rec.at.Sub(records[i-1].at)
				if gap < 20*time.Second {
					t.Errorf("第 %d 批与前批间隔 %v < 20s", i, gap)
				}
			}
		}
	})

	t.Run("HTTP_429_RetryAfter冷却期内禁发且冷却后补发", func(t *testing.T) {
		var attempts atomic.Int64
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
				return
			}
			n := attempts.Add(1)
			if n == 1 {
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if n == 2 {
				// 无 Retry-After 头，默认冷却 65s
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"a": 1})
		clk.Advance(30 * time.Second)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool { return attempts.Load() == 1 }, "等待第 1 次 429")

		// 冷却 45s：推进 44s 不得发
		clk.Advance(44 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if attempts.Load() != 1 {
			t.Fatalf("Retry-After=45s 冷却期内不应发包，实际 attempts=%d", attempts.Load())
		}

		// 再推进 2s（总计 46s > 45s），触发第 2 次（无头 429 → 默认 65s）
		clk.Advance(2 * time.Second)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool { return attempts.Load() == 2 }, "等待第 2 次无头 429")

		// 冷却 65s：推进 64s 不得发
		clk.Advance(64 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if attempts.Load() != 2 {
			t.Fatalf("无头 429 默认 65s 冷却期内不应发包，实际 attempts=%d", attempts.Load())
		}

		// 再推进 2s，成功送达 app_start
		clk.Advance(2 * time.Second)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(delivered) == 1
		}, "等待冷却结束后补发成功")

		Shutdown(500 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].Event != "app_start" {
			t.Errorf("app_start 应零丢失送达：%v", delivered)
		}
	})

	t.Run("常态503遵守minSendGap且Flush豁免最小间隔与退避", func(t *testing.T) {
		var attempts atomic.Int64
		var failMode atomic.Bool
		failMode.Store(true)
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			attempts.Add(1)
			if failMode.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 27, 13, 30, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.minSendGap = 25 * time.Second // 故意大于首阶退避 20s，验证常态 503 仍受 minGapUntil 约束
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"a": 1})
		clk.Advance(30 * time.Second)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 1 次 503 结算超时")
		}

		// 推进 24s（已过 20s 退避但未过 25s minSendGap），不得重发
		clk.Advance(24 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if got := attempts.Load(); got != 1 {
			t.Fatalf("503 后未满 minSendGap(25s) 不应重发，实际 attempts=%d", got)
		}

		// 推进到 25s，触发第 2 次 503
		clk.Advance(1 * time.Second)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 2 次 503 结算超时")
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("满 minSendGap(25s) 后期望 attempts == 2，实际 %d", got)
		}

		// 不推进时钟（仍处 40s 退避与 25s 最小间隔内），切回 200 并调用 Flush 应豁免并立即冲刷成功
		failMode.Store(false)
		Flush(1 * time.Second)
		Shutdown(500 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].Event != "app_start" {
			t.Errorf("Flush 应豁免退避与最小间隔并冲刷 app_start，实际：%v", delivered)
		}
	})

	t.Run("HTTP_429_RateLimitReset冷却期内禁发且冷却后补发", func(t *testing.T) {
		var attempts atomic.Int64
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			switch attempts.Add(1) {
			case 1:
				// 仅返回 Kong RateLimit-Reset: 50（无 Retry-After）
				w.Header().Set("RateLimit-Reset", "50")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			case 2:
				// RateLimit-Reset: 0 无效，回落默认 65s
				w.Header().Set("RateLimit-Reset", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			case 3:
				// 同时返回 Retry-After 与 RateLimit-Reset，优先 Retry-After: 30
				w.Header().Set("Retry-After", "30")
				w.Header().Set("RateLimit-Reset", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		waitBatchDone := func(desc string) {
			t.Helper()
			select {
			case <-batchDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("等待超时（2s）：%s", desc)
			}
		}

		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"a": 1})
		clk.Advance(30 * time.Second)
		c.nudge()
		waitBatchDone("等待第 1 次 RateLimit-Reset=50 结算")
		if attempts.Load() != 1 {
			t.Fatalf("期望 attempts=1，实际 %d", attempts.Load())
		}

		// RateLimit-Reset=50s：49s 内不重发，满 50s 后发第 2 次
		clk.Advance(49 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if attempts.Load() != 1 {
			t.Fatalf("RateLimit-Reset=50s 冷却期内（49s）不应重发，实际 attempts=%d", attempts.Load())
		}
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待第 2 次 RateLimit-Reset=0 结算")
		if attempts.Load() != 2 {
			t.Fatalf("满 50s 后期望 attempts=2，实际 %d", attempts.Load())
		}

		// RateLimit-Reset=0 回落 65s：64s 内不重发，满 65s 后发第 3 次
		clk.Advance(64 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if attempts.Load() != 2 {
			t.Fatalf("RateLimit-Reset=0 回落 65s 冷却期内（64s）不应重发，实际 attempts=%d", attempts.Load())
		}
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待第 3 次 Retry-After=30 优先结算")
		if attempts.Load() != 3 {
			t.Fatalf("满 65s 后期望 attempts=3，实际 %d", attempts.Load())
		}

		// Retry-After=30s 优先于 RateLimit-Reset=120s：29s 内不重发，满 30s 后补发成功
		clk.Advance(29 * time.Second)
		c.nudge()
		time.Sleep(40 * time.Millisecond)
		if attempts.Load() != 3 {
			t.Fatalf("Retry-After=30s 冷却期内（29s）不应重发，实际 attempts=%d", attempts.Load())
		}
		clk.Advance(1 * time.Second)
		c.nudge()
		waitBatchDone("等待第 4 次补发成功结算")

		Shutdown(500 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].Event != "app_start" {
			t.Errorf("429 冷却结束后 app_start 应完整送达：%v", delivered)
		}
	})
}

// TestShutdownCancelsSlowFlagsAndFlushesTail 校验首拉 /flags 慢响应（阻塞 2s）遇上早退 Shutdown(800ms)：
// 最多宽限 250ms 后取消在途 /flags，并在剩余退出预算内把 app_start 和 app_exit 冲刷至 /batch/。
//
// 变异自证：不取消在途 /flags（占满 800ms 退出预算导致尾批丢失）或宽限超过 250ms，本测试变红。
func TestShutdownCancelsSlowFlagsAndFlushesTail(t *testing.T) {
	flagsStarted := make(chan struct{}, 1)
	flagsCanceled := make(chan time.Time, 1)
	var mu sync.Mutex
	var delivered []capturedEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/flags") {
			// 先消费完 POST 请求体，net/http 服务端才会启动后台断连探测并及时关闭 r.Context()。
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case flagsStarted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				select {
				case flagsCanceled <- time.Now():
				default:
				}
				return
			case <-time.After(2 * time.Second):
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		var req capturedBatch
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		delivered = append(delivered, req.Batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	withInjectedCreds(t, "phc_key", srv.URL)
	Init(Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "abcdef0123456789abcdef0123456789",
		HTTPClient: srv.Client(),
	})

	Track(CatLifecycle, "app_start", Props{"a": 1})
	Start()

	select {
	case <-flagsStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("首拉 /flags 未启动")
	}

	time.Sleep(100 * time.Millisecond)
	Track(CatLifecycle, "app_exit", Props{"exit_reason": "port_bind_failed"})

	shutdownBegin := time.Now()
	Shutdown(800 * time.Millisecond)
	shutdownDur := time.Since(shutdownBegin)

	var canceledAt time.Time
	select {
	case canceledAt = <-flagsCanceled:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("在途慢速 /flags 未被取消")
	}
	cancelDelay := canceledAt.Sub(shutdownBegin)
	if cancelDelay > 450*time.Millisecond {
		t.Errorf("取消在途 /flags 耗时过长（期望 <=250ms 宽限）：%v", cancelDelay)
	}
	if shutdownDur > 800*time.Millisecond {
		t.Errorf("Shutdown(800ms) 超时返回：%v", shutdownDur)
	}

	mu.Lock()
	defer mu.Unlock()
	names := map[string]bool{}
	for _, ev := range delivered {
		names[ev.Event] = true
	}
	if !names["app_start"] || !names["app_exit"] {
		t.Errorf("尾批应冲刷 app_start 与 app_exit，实际收到：%v", delivered)
	}
}

// TestTrackOnceDeduplicates 校验同 key 在进程内仅发一次，不同 key 各自发送。
//
// 变异自证：去掉 onceKeys 去重或错误共用全局标志，本测试变红。
func TestTrackOnceDeduplicates(t *testing.T) {
	var mu sync.Mutex
	var delivered []capturedEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/flags") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
			return
		}
		var req capturedBatch
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		delivered = append(delivered, req.Batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	withInjectedCreds(t, "phc_key", srv.URL)
	Init(Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "abcdef0123456789abcdef0123456789",
		HTTPClient: srv.Client(),
	})
	Start()
	waitUntil(t, 2*time.Second, func() bool { return current.Load().flagsReady.Load() }, "首拉完成")

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				TrackOnce("blocked:kugou:store", CatPlayer, "player_blocked", Props{"player": "kugou"})
				TrackOnce("blocked:spotify:flag", CatPlayer, "player_blocked", Props{"player": "spotify"})
			}
		}()
	}
	wg.Wait()

	Shutdown(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("期望仅发 2 条不同 key 事件，实际 %d 条：%v", len(delivered), delivered)
	}
	players := map[string]int{}
	for _, ev := range delivered {
		p, _ := ev.Properties["player"].(string)
		players[p]++
	}
	if players["kugou"] != 1 || players["spotify"] != 1 {
		t.Errorf("去重结果异常：%v", players)
	}
}

type panicRoundTripper struct {
	panicOnBatch bool
	panics       atomic.Int64
}

func (p *panicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.Path, "/flags") {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`)),
		}, nil
	}
	if p.panicOnBatch {
		p.panics.Add(1)
		panic("injected worker panic")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

// TestTrackNonBlockingUnderHungServerAndWorkerPanic 校验：
// 1. 服务端挂死 5s 且队列打满时，10,000 次并发 Track 总耗时 < 50ms，Shutdown(200ms) 准时返回；
// 2. worker 内部注入 panic 被本地 recover 捕获且主进程存活。
//
// 变异自证：Track 阻塞写通道、Shutdown 等待挂死请求到 5s 或去掉 worker 本地 recover，本测试变红或直接崩溃。
func TestTrackNonBlockingUnderHungServerAndWorkerPanic(t *testing.T) {
	t.Run("服务端挂死5s下10000次并发Track零阻塞且Shutdown准时返回", func(t *testing.T) {
		batchEntered := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case batchEntered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		// 先灌入 50 条触发挂死发送，把 worker 卡在挂死的 /batch/ 上
		for i := 0; i < 50; i++ {
			Track(CatLyricTrack, "track_recognized", Props{"i": i})
		}
		select {
		case <-batchEntered:
		case <-time.After(1 * time.Second):
			t.Fatal("worker 未发起 /batch/ 请求")
		}

		// 10,000 次并发 Track
		start := time.Now()
		var wg sync.WaitGroup
		const workers = 10
		const perWorker = 1000
		for g := 0; g < workers; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					Track(CatLyricTrack, "track_recognized", Props{"g": id, "i": i})
				}
			}(g)
		}
		wg.Wait()
		trackDur := time.Since(start)
		if trackDur >= nonBlockingTrackBudget {
			t.Errorf("10,000 次并发 Track 总耗时 %v >= %v", trackDur, nonBlockingTrackBudget)
		}

		rumDropped, _ := DrainDropped()
		if rumDropped == 0 {
			t.Errorf("队列打满时 rumDropped 应 > 0")
		}

		// 同时验证 AddDropped / DrainDropped 原子清零语义
		AddDropped("ws_sub", 3)
		AddDropped("ws_sub", 4)
		AddDropped("audio_sub", 2)
		rd2, scopes := DrainDropped()
		if rd2 != 0 || scopes["ws_sub"] != 7 || scopes["audio_sub"] != 2 {
			t.Errorf("DrainDropped 账单异常：rd=%d scopes=%v", rd2, scopes)
		}
		rd3, scopes3 := DrainDropped()
		if rd3 != 0 || len(scopes3) != 0 {
			t.Errorf("二次 DrainDropped 应已清零：rd=%d scopes=%v", rd3, scopes3)
		}

		shutStart := time.Now()
		Shutdown(200 * time.Millisecond)
		shutDur := time.Since(shutStart)
		if shutDur > 450*time.Millisecond {
			t.Errorf("Shutdown(200ms) 未准时返回：%v", shutDur)
		}
	})

	t.Run("worker内部panic被本地recover捕获进程存活", func(t *testing.T) {
		rt := &panicRoundTripper{panicOnBatch: true}
		withInjectedCreds(t, "phc_key", "https://rum.example.invalid")
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: &http.Client{Transport: rt, Timeout: 2 * time.Second},
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"a": 1})
		Flush(1 * time.Second)

		if rt.panics.Load() == 0 {
			t.Errorf("未触发预期的 worker panic")
		}
		Shutdown(500 * time.Millisecond)
	})
}

// assertTypeHasNoMutex 递归检查结构体/指针/容器类型是否直接或间接包含 sync.Mutex 或 sync.RWMutex。
func assertTypeHasNoMutex(t *testing.T, root reflect.Type, path string, visited map[reflect.Type]bool) {
	t.Helper()
	if root == nil {
		return
	}
	mutexType := reflect.TypeOf(sync.Mutex{})
	rwMutexType := reflect.TypeOf(sync.RWMutex{})
	if root == mutexType || root == rwMutexType {
		t.Errorf("%s 包含互斥锁类型 %v", path, root)
		return
	}
	switch root.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		assertTypeHasNoMutex(t, root.Elem(), path, visited)
	case reflect.Map:
		assertTypeHasNoMutex(t, root.Key(), path+".<key>", visited)
		assertTypeHasNoMutex(t, root.Elem(), path+".<val>", visited)
	case reflect.Struct:
		if visited[root] {
			return
		}
		visited[root] = true
		for i := 0; i < root.NumField(); i++ {
			f := root.Field(i)
			assertTypeHasNoMutex(t, f.Type, path+"."+f.Name, visited)
		}
	}
}

// TestLifecycleTrackNonBlockingWhileWorkerBlockedInNetwork 校验 CatLifecycle 入队 100% 无锁非阻塞及完整去重/保留语义：
//  1. lifecycleBuffer 与 client 生命周期入队相关字段不含互斥锁，也不依赖通道或单槽覆盖；
//  2. 后台 worker 阻塞在慢速网络请求期间，并发高频调用 Track(CatLifecycle, ...) 立即无锁返回，
//     恢复后保留首条 app_start 原 uuid/timestamp、33 日去重与淘汰计数以及最新 app_exit；
//  3. 阻塞期间同日刷屏 128 条心跳后追加新日期，全部保留且零误淘汰；
//  4. 阻塞期间顺序入队 200 个不同连续日期，恢复后精确交付最新 33 日完整集合、每日期保留首次 uuid/timestamp 且 lifecycle_evicted == 167；
//  5. 首批 200 在途阻塞期间重复 Track app_start 与同日心跳去重不重发，同时并发入队的新日期正常送达；
//  6. 非常规 CatLifecycle 事件超过上限时淘汰最旧并保留最新 maxOtherLifecycle 条。
//
// 变异自证：引入互斥锁、退回有限通道/单槽覆盖或过早清除在途去重状态，本测试变红。
func TestLifecycleTrackNonBlockingWhileWorkerBlockedInNetwork(t *testing.T) {
	t.Run("反射核验生命周期缓冲与入队字段无锁且无残留通道单槽", func(t *testing.T) {
		assertTypeHasNoMutex(t, reflect.TypeOf(lifecycleBuffer{}), "lifecycleBuffer", map[reflect.Type]bool{})
		assertTypeHasNoMutex(t, reflect.TypeOf(lifecycleSnap{}), "lifecycleSnap", map[reflect.Type]bool{})

		ct := reflect.TypeOf(client{})
		forbiddenFields := map[string]bool{
			"lifecycleCh":       true,
			"pendingStart":      true,
			"pendingExit":       true,
			"overflowHeartbeat": true,
		}
		foundLifecycle := false
		for i := 0; i < ct.NumField(); i++ {
			f := ct.Field(i)
			if forbiddenFields[f.Name] {
				t.Errorf("client 不应保留已废弃的生命周期中间字段 %q", f.Name)
			}
			if strings.Contains(strings.ToLower(f.Name), "lifecycle") {
				foundLifecycle = true
				assertTypeHasNoMutex(t, f.Type, "client."+f.Name, map[reflect.Type]bool{})
				if f.Type.Kind() == reflect.Chan {
					t.Errorf("client.%s 不应为通道类型 %v", f.Name, f.Type)
				}
			}
		}
		if !foundLifecycle {
			t.Fatalf("client 中未找到生命周期缓冲字段")
		}
	})

	t.Run("后台网络发送阻塞时并发Lifecycle入队零阻塞且恢复语义完好", func(t *testing.T) {
		batchBlocked := make(chan struct{}, 1)
		releaseFirstBatch := make(chan struct{})
		var batchAttempts atomic.Int64
		var mu sync.Mutex
		var firstStartUUID, firstStartTS string
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			n := batchAttempts.Add(1)
			if n == 1 {
				mu.Lock()
				for _, ev := range req.Batch {
					if ev.Event == "app_start" {
						firstStartUUID = ev.UUID
						firstStartTS = ev.Timestamp
					}
				}
				mu.Unlock()
				select {
				case batchBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseFirstBatch:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		baseDate := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
		clk := newFakeClock(baseDate)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"origin": "first"})
		clk.Advance(30 * time.Second)
		c.nudge()

		select {
		case <-batchBlocked:
		case <-time.After(2 * time.Second):
			t.Fatal("worker 未进入首个 /batch/ 阻塞点")
		}

		allDates := make([]string, 35)
		for i := 0; i < 35; i++ {
			allDates[i] = baseDate.AddDate(0, 0, i).Format("2006-01-02")
		}

		// worker 卡在网络发送期间，并发入队重复 app_start、35 日去重心跳及多次 app_exit。
		// 先并发写入前 33 日（含同日重复），再并发写入第 34/35 日（含同日重复），保证淘汰最旧 2 日的计数确定为 2。
		trackStart := time.Now()
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					Track(CatLifecycle, "app_start", Props{"origin": "dup", "g": id})
					Track(CatLifecycle, "app_exit", Props{"exit_reason": "early", "g": id})
				}
			}(g)
		}
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, d := range allDates[:33] {
					Track(CatLifecycle, "app_heartbeat", Props{"local_date": d})
				}
			}()
		}
		wg.Wait()
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, d := range allDates[33:] {
					Track(CatLifecycle, "app_heartbeat", Props{"local_date": d})
				}
			}()
		}
		wg.Wait()
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "final"})
		trackDur := time.Since(trackStart)
		if trackDur >= nonBlockingTrackBudget {
			t.Errorf("worker 阻塞期间并发 CatLifecycle 入队耗时过长：%v >= %v", trackDur, nonBlockingTrackBudget)
		}

		// 放行首轮 503 并等待该批结算完成，再推进时钟跨过退避窗恢复 200 补发。
		close(releaseFirstBatch)
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待首轮 503 结算超时")
		}

		clk.Advance(1 * time.Minute)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待恢复补发结算超时")
		}

		Shutdown(1 * time.Second)

		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != 2 {
			t.Errorf("期望 lifecycle_evicted == 2，实际 %v", byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 35 {
			t.Fatalf("期望补发 35 条（1 start + 33 heartbeat + 1 exit），实际 %d 条", len(delivered))
		}
		var gotStart, gotExit capturedEvent
		var startCount, exitCount int
		gotDates := make(map[string]bool, 33)
		for _, ev := range delivered {
			switch ev.Event {
			case "app_start":
				startCount++
				gotStart = ev
			case "app_exit":
				exitCount++
				gotExit = ev
			case "app_heartbeat":
				d, _ := ev.Properties["local_date"].(string)
				if gotDates[d] {
					t.Errorf("重复送达同日心跳：%s", d)
				}
				gotDates[d] = true
			}
		}
		if startCount != 1 || gotStart.UUID == "" || gotStart.UUID != firstStartUUID || gotStart.Timestamp != firstStartTS || gotStart.Properties["origin"] != "first" {
			t.Errorf("app_start 未保留首次原 uuid/timestamp：count=%d got=%+v first=(%s,%s)", startCount, gotStart, firstStartUUID, firstStartTS)
		}
		if exitCount != 1 || gotExit.Properties["exit_reason"] != "final" {
			t.Errorf("app_exit 未保留最新一条：count=%d got=%+v", exitCount, gotExit)
		}
		if gotDates[allDates[0]] || gotDates[allDates[1]] {
			t.Errorf("最旧 2 日心跳（%s, %s）应被淘汰", allDates[0], allDates[1])
		}
		for _, d := range allDates[2:] {
			if !gotDates[d] {
				t.Errorf("较新日期心跳 %s 缺失", d)
			}
		}
	})

	t.Run("同日刷屏128条心跳后新日期心跳全部送达且零误淘汰", func(t *testing.T) {
		batchBlocked := make(chan struct{}, 1)
		releaseFirstBatch := make(chan struct{})
		var batchAttempts atomic.Int64
		var mu sync.Mutex
		var retry25UUID, retry25TS string
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch batchAttempts.Add(1) {
			case 1:
				select {
				case batchBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseFirstBatch:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case 2:
				// 再返回一次 503 记录 2026-11-25 首次入队后的原 uuid/timestamp，验证后续恢复补发保持一致。
				mu.Lock()
				for _, ev := range req.Batch {
					if ev.Event == "app_heartbeat" {
						if d, _ := ev.Properties["local_date"].(string); d == "2026-11-25" {
							retry25UUID = ev.UUID
							retry25TS = ev.Timestamp
						}
					}
				}
				mu.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"boot": true})
		clk.Advance(30 * time.Second)
		c.nudge()
		select {
		case <-batchBlocked:
		case <-time.After(2 * time.Second):
			t.Fatal("worker 未进入阻塞点")
		}

		first25TS := clk.Now().UTC().Format(time.RFC3339Nano)
		trackStart := time.Now()
		for i := 0; i < 128; i++ {
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-25", "seq": i})
			clk.Advance(1 * time.Second)
		}
		Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-26", "seq": 1})
		clk.Advance(1 * time.Second)
		Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-27", "seq": 1})
		if dur := time.Since(trackStart); dur >= nonBlockingTrackBudget {
			t.Errorf("阻塞期间同日刷屏入队耗时过长：%v >= %v", dur, nonBlockingTrackBudget)
		}

		// 放行首批 503，等待结算。
		close(releaseFirstBatch)
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 1 轮 503 结算超时")
		}

		// 跨过首阶 20s 退避窗触发第 2 轮 503，采集原 uuid/timestamp。
		clk.Advance(30 * time.Second)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 2 轮 503 结算超时")
		}

		// 再次重复发送 2026-11-25，验证不覆写原 uuid/timestamp。
		clk.Advance(1 * time.Second)
		Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-25", "seq": 999})

		// 跨过二阶 40s 退避窗恢复 200 补发。
		clk.Advance(1 * time.Minute)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 3 轮 200 补发结算超时")
		}

		Shutdown(1 * time.Second)

		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != 0 {
			t.Errorf("仅有 3 个不同日期不应产生淘汰，实际 lifecycle_evicted=%d（%v）", byScope["lifecycle_evicted"], byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		gotByDate := map[string]capturedEvent{}
		hbCount := 0
		for _, ev := range delivered {
			if ev.Event == "app_heartbeat" {
				hbCount++
				d, _ := ev.Properties["local_date"].(string)
				if _, dup := gotByDate[d]; dup {
					t.Errorf("重复送达同日心跳：%s", d)
				}
				gotByDate[d] = ev
			}
		}
		if hbCount != 3 {
			t.Fatalf("期望送达 3 个不同日期心跳，实际 %d 条：%v", hbCount, gotByDate)
		}
		for _, wantDate := range []string{"2026-11-25", "2026-11-26", "2026-11-27"} {
			if _, ok := gotByDate[wantDate]; !ok {
				t.Errorf("期望日期 %s 送达，实际集合：%v", wantDate, gotByDate)
			}
		}
		ev25 := gotByDate["2026-11-25"]
		if ev25.UUID == "" || ev25.UUID != retry25UUID || ev25.Timestamp != first25TS || ev25.Timestamp != retry25TS || ev25.Properties["seq"] != float64(0) {
			t.Errorf("2026-11-25 未保持首次 uuid/timestamp/seq：got=(%s,%s,%v) retry=(%s,%s) firstTS=%s",
				ev25.UUID, ev25.Timestamp, ev25.Properties["seq"], retry25UUID, retry25TS, first25TS)
		}
	})

	t.Run("超出33日溢出精确保留最新33日完整集合与首次元数据", func(t *testing.T) {
		batchBlocked := make(chan struct{}, 1)
		releaseFirstBatch := make(chan struct{})
		var batchAttempts atomic.Int64
		var mu sync.Mutex
		retryUUIDByDate := map[string]string{}
		retryTSByDate := map[string]string{}
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch batchAttempts.Add(1) {
			case 1:
				select {
				case batchBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseFirstBatch:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case 2:
				mu.Lock()
				for _, ev := range req.Batch {
					if ev.Event == "app_heartbeat" {
						d, _ := ev.Properties["local_date"].(string)
						retryUUIDByDate[d] = ev.UUID
						retryTSByDate[d] = ev.Timestamp
					}
				}
				mu.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		baseDate := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
		clk := newFakeClock(baseDate)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"boot": true})
		clk.Advance(30 * time.Second)
		c.nudge()
		select {
		case <-batchBlocked:
		case <-time.After(2 * time.Second):
			t.Fatal("worker 未进入阻塞点")
		}

		// 阻塞期间顺序入队 200 个连续日期（每日附带 1 次同日重复以校验保留首次 uuid/timestamp 且不重复计淘汰）。
		const totalDays = 200
		allDates := make([]string, totalDays)
		firstTSByDate := make(map[string]string, totalDays)

		trackStart := time.Now()
		for i := 0; i < totalDays; i++ {
			d := baseDate.AddDate(0, 0, i).Format("2006-01-02")
			allDates[i] = d
			firstTSByDate[d] = clk.Now().UTC().Format(time.RFC3339Nano)
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": d, "seq": 1})
			clk.Advance(1 * time.Second)
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": d, "seq": 2})
			clk.Advance(1 * time.Second)
		}
		if dur := time.Since(trackStart); dur >= nonBlockingTrackBudget {
			t.Errorf("阻塞期间 200 日突发入队耗时过长：%v >= %v", dur, nonBlockingTrackBudget)
		}

		close(releaseFirstBatch)
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 1 轮 503 结算超时")
		}

		clk.Advance(30 * time.Second)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 2 轮 503 结算超时")
		}

		clk.Advance(1 * time.Minute)
		c.nudge()
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待第 3 轮 200 补发结算超时")
		}
		Shutdown(1 * time.Second)

		_, byScope := DrainDropped()
		const wantEvicted = uint64(totalDays - maxHeartbeatDays)
		if byScope["lifecycle_evicted"] != wantEvicted {
			t.Errorf("期望 lifecycle_evicted == %d，实际 %v", wantEvicted, byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		var hasStart bool
		gotByDate := make(map[string]capturedEvent, maxHeartbeatDays)
		hbCount := 0
		for _, ev := range delivered {
			switch ev.Event {
			case "app_start":
				hasStart = true
			case "app_heartbeat":
				hbCount++
				d, _ := ev.Properties["local_date"].(string)
				if _, dup := gotByDate[d]; dup {
					t.Errorf("重复送达同日心跳：%s", d)
				}
				gotByDate[d] = ev
			}
		}
		if !hasStart {
			t.Errorf("app_start 丢失")
		}
		if hbCount != maxHeartbeatDays || len(gotByDate) != maxHeartbeatDays {
			t.Fatalf("期望精确送达 %d 日心跳，实际 count=%d distinct=%d", maxHeartbeatDays, hbCount, len(gotByDate))
		}

		wantDates := make(map[string]bool, maxHeartbeatDays)
		for _, d := range allDates[totalDays-maxHeartbeatDays:] {
			wantDates[d] = true
		}
		for d := range gotByDate {
			if !wantDates[d] {
				t.Errorf("送达了应被淘汰的旧日期心跳：%s", d)
			}
		}
		for _, d := range allDates[totalDays-maxHeartbeatDays:] {
			ev, ok := gotByDate[d]
			if !ok {
				t.Errorf("期望最新 33 日集合中的日期 %s 送达，实际缺失", d)
				continue
			}
			if ev.UUID == "" || ev.UUID != retryUUIDByDate[d] {
				t.Errorf("日期 %s 未保留原 uuid：got=%q retry=%q", d, ev.UUID, retryUUIDByDate[d])
			}
			if ev.Timestamp != firstTSByDate[d] || ev.Timestamp != retryTSByDate[d] {
				t.Errorf("日期 %s 未保留首次 timestamp：got=%q first=%q retry=%q", d, ev.Timestamp, firstTSByDate[d], retryTSByDate[d])
			}
			if ev.Properties["seq"] != float64(1) {
				t.Errorf("日期 %s 应保留首次事件属性 seq=1，实际 %v", d, ev.Properties["seq"])
			}
		}
	})

	t.Run("首批200在途期间重复入队去重且并发新日期不丢失", func(t *testing.T) {
		batchBlocked := make(chan struct{}, 1)
		releaseFirstBatch := make(chan struct{})
		var batchAttempts atomic.Int64
		var mu sync.Mutex
		var firstStartUUID, firstStartTS string
		var firstHB25UUID, firstHB25TS string
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			if batchAttempts.Add(1) == 1 {
				mu.Lock()
				for _, ev := range req.Batch {
					switch ev.Event {
					case "app_start":
						firstStartUUID = ev.UUID
						firstStartTS = ev.Timestamp
					case "app_heartbeat":
						if d, _ := ev.Properties["local_date"].(string); d == "2026-11-25" {
							firstHB25UUID = ev.UUID
							firstHB25TS = ev.Timestamp
						}
					}
				}
				mu.Unlock()
				select {
				case batchBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseFirstBatch:
				case <-r.Context().Done():
				}
				mu.Lock()
				delivered = append(delivered, req.Batch...)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		batchDone := make(chan struct{}, 8)
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		c.testAfterBatchHook = func() {
			select {
			case batchDone <- struct{}{}:
			default:
			}
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		Track(CatLifecycle, "app_start", Props{"origin": "first"})
		Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-25", "seq": 1})
		clk.Advance(30 * time.Second)
		c.nudge()

		select {
		case <-batchBlocked:
		case <-time.After(2 * time.Second):
			t.Fatal("worker 未进入首批 /batch/ 在途阻塞点")
		}

		// 首批在途阻塞期间，从另一协程重复入队 app_start 与 2026-11-25 心跳，并新增 2026-11-26 心跳。
		clk.Advance(1 * time.Minute)
		doneInflightTrack := make(chan struct{})
		go func() {
			defer close(doneInflightTrack)
			Track(CatLifecycle, "app_start", Props{"origin": "inflight_dup"})
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-25", "seq": 2})
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-26", "seq": 1})
		}()
		select {
		case <-doneInflightTrack:
		case <-time.After(1 * time.Second):
			t.Fatal("在途期间重复入队阻塞超时")
		}

		// 放行首批返回 200 OK，等待首批结算，再 Flush 与 Shutdown 冲刷剩余新日期事件。
		close(releaseFirstBatch)
		select {
		case <-batchDone:
		case <-time.After(2 * time.Second):
			t.Fatal("等待首批 200 结算超时")
		}
		Flush(2 * time.Second)
		Shutdown(2 * time.Second)

		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != 0 {
			t.Errorf("期望 lifecycle_evicted == 0，实际 %v", byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		var starts []capturedEvent
		hbByDate := map[string][]capturedEvent{}
		for _, ev := range delivered {
			switch ev.Event {
			case "app_start":
				starts = append(starts, ev)
			case "app_heartbeat":
				d, _ := ev.Properties["local_date"].(string)
				hbByDate[d] = append(hbByDate[d], ev)
			}
		}
		if len(starts) != 1 || starts[0].UUID == "" || starts[0].UUID != firstStartUUID || starts[0].Timestamp != firstStartTS || starts[0].Properties["origin"] != "first" {
			t.Errorf("app_start 应全程仅交付 1 次且保持原 uuid/timestamp：got=%+v first=(%s,%s)", starts, firstStartUUID, firstStartTS)
		}
		hb25 := hbByDate["2026-11-25"]
		if len(hb25) != 1 || hb25[0].UUID == "" || hb25[0].UUID != firstHB25UUID || hb25[0].Timestamp != firstHB25TS || hb25[0].Properties["seq"] != float64(1) {
			t.Errorf("2026-11-25 心跳应全程仅交付 1 次且保持原 uuid/timestamp：got=%+v first=(%s,%s)", hb25, firstHB25UUID, firstHB25TS)
		}
		hb26 := hbByDate["2026-11-26"]
		if len(hb26) != 1 || hb26[0].Properties["seq"] != float64(1) {
			t.Errorf("2026-11-26 心跳应正常交付 1 次：got=%+v", hb26)
		}
		if len(delivered) != 3 {
			t.Errorf("期望全程精确交付 3 条事件，实际 %d 条：%v", len(delivered), delivered)
		}
	})

	t.Run("非常规Lifecycle事件超限淘汰最旧保留最新8条", func(t *testing.T) {
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errorsWhileComputingFlags":false,"flags":{"playercap-rum-config":{"enabled":true,"metadata":{"payload":"{\"enabled\":true}"}}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		for i := 0; i < 10; i++ {
			Track(CatLifecycle, "custom_lifecycle_probe", Props{"seq": i})
		}
		Start()
		waitUntil(t, 2*time.Second, func() bool { return current.Load().flagsReady.Load() }, "首拉完成")
		Shutdown(2 * time.Second)

		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != uint64(10-maxOtherLifecycle) {
			t.Errorf("期望 lifecycle_evicted == %d，实际 %v", 10-maxOtherLifecycle, byScope)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != maxOtherLifecycle {
			t.Fatalf("期望送达最新 %d 条非常规事件，实际 %d 条", maxOtherLifecycle, len(delivered))
		}
		for i, ev := range delivered {
			wantSeq := float64(i + (10 - maxOtherLifecycle))
			if ev.Properties["seq"] != wantSeq {
				t.Errorf("第 %d 条非常规事件 seq=%v，期望 %v", i, ev.Properties["seq"], wantSeq)
			}
		}
	})

	t.Run("lifecycleBuffer不可变快照与早日期淘汰及并发CAS无竞态", func(t *testing.T) {
		drops.reset()
		t.Cleanup(drops.reset)

		var buf lifecycleBuffer
		base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
		buf.put(queuedEvent{cat: CatLifecycle, event: "app_start", uuid: "u-start", ts: base, cfgGen: 1})
		buf.put(queuedEvent{cat: CatLifecycle, event: "app_exit", uuid: "u-exit", ts: base, cfgGen: 1})
		buf.put(queuedEvent{cat: CatLifecycle, event: "custom_ev", uuid: "u-other", ts: base, cfgGen: 1})
		for i := 0; i < maxHeartbeatDays; i++ {
			d := base.AddDate(0, 0, i).Format("2006-01-02")
			buf.put(queuedEvent{cat: CatLifecycle, event: "app_heartbeat", uuid: "u-hb-" + d, ts: base.Add(time.Duration(i) * time.Second), localDate: d, cfgGen: 1})
		}

		// 早于当前 33 日窗口的日期不改状态，仅计 1 次 lifecycle_evicted。
		beforeOldDate := buf.state.Load()
		buf.put(queuedEvent{cat: CatLifecycle, event: "app_heartbeat", uuid: "u-old", ts: base.Add(-24 * time.Hour), localDate: "2026-05-01", cfgGen: 1})
		if buf.state.Load() != beforeOldDate {
			t.Errorf("早于 33 日窗口的心跳不应改动状态指针")
		}
		_, byScope := DrainDropped()
		if byScope["lifecycle_evicted"] != 1 {
			t.Errorf("早于 33 日窗口的心跳应计 lifecycle_evicted == 1，实际 %v", byScope)
		}

		// markRefiltered 必须拷贝副本而不原地改写旧快照中的 *queuedEvent。
		oldSnap := buf.state.Load()
		ids := map[string]struct{}{
			"u-start":                           {},
			"u-exit":                            {},
			"u-other":                           {},
			"u-hb-" + base.Format("2006-01-02"): {},
		}
		buf.markRefiltered(ids, 2)
		if oldSnap.start.cfgGen != 1 || oldSnap.exit.cfgGen != 1 || oldSnap.other[0].cfgGen != 1 || oldSnap.heartbeats[base.Format("2006-01-02")].cfgGen != 1 {
			t.Errorf("markRefiltered 原地改写了旧快照事件代次")
		}
		newSnap := buf.state.Load()
		if newSnap.start.cfgGen != 2 || newSnap.exit.cfgGen != 2 || newSnap.other[0].cfgGen != 2 || newSnap.heartbeats[base.Format("2006-01-02")].cfgGen != 2 {
			t.Errorf("markRefiltered 未在新快照中更新事件代次")
		}

		// 高并发混合 put / snapshot / markRefiltered / removeByUUIDs / len / oldestTS，-race 下零数据竞争。
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 60; i++ {
					d := base.AddDate(0, 0, (id*17+i)%50).Format("2006-01-02")
					buf.put(queuedEvent{cat: CatLifecycle, event: "app_heartbeat", uuid: newUUIDv4(), ts: base.Add(time.Duration(i) * time.Second), localDate: d, cfgGen: 1})
					buf.put(queuedEvent{cat: CatLifecycle, event: "app_exit", uuid: newUUIDv4(), ts: base.Add(time.Duration(i) * time.Second), cfgGen: 1})
					buf.put(queuedEvent{cat: CatLifecycle, event: "custom_ev", uuid: newUUIDv4(), ts: base.Add(time.Duration(i) * time.Second), cfgGen: 1})
				}
			}(g)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				snap := buf.snapshot()
				_ = buf.len()
				_ = buf.oldestTS()
				if len(snap) > 0 {
					refIDs := map[string]struct{}{snap[0].uuid: {}}
					buf.markRefiltered(refIDs, uint64(i+3))
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				snap := buf.snapshot()
				if len(snap) > 0 {
					rmIDs := map[string]struct{}{snap[len(snap)-1].uuid: {}}
					buf.removeByUUIDs(rmIDs)
				}
			}
		}()
		wg.Wait()
	})
}

// TestTrackIsolatesUnknownNestedContainersUnderConcurrentMutation 校验通过 Track 传入 []map[string]any 等
// 未知嵌套容器时：敏感键与路径在出网 JSON 中 0 残留，且调用方随后并发改写原嵌套 map 不会污染快照或触发数据竞争。
//
// 变异自证：default 分支原样返回调用方 slice/map，本测试在 -race 下报竞争或出网 JSON 泄露敏感数据。
func TestTrackIsolatesUnknownNestedContainersUnderConcurrentMutation(t *testing.T) {
	var mu sync.Mutex
	var rawBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/flags") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		rawBodies = append(rawBodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	withInjectedCreds(t, "phc_key", srv.URL)
	Init(Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "abcdef0123456789abcdef0123456789",
		HTTPClient: srv.Client(),
	})
	Start()
	waitUntil(t, 2*time.Second, func() bool { return current.Load().flagsReady.Load() }, "首拉完成")

	nested := map[string]any{
		"username": "Alice",
		"detail":   `C:\Users\Alice\private.txt`,
		"stable":   "ok",
	}
	items := []map[string]any{nested}
	Track(CatPlayer, "player_compat_signal", Props{"items": items})

	// Track 返回后立即并发改写调用方原始嵌套 map，验证后台序列化期间无数据竞争且快照不受污染。
	stopMutate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stopMutate:
				return
			default:
				nested["stable"] = "mutated"
				nested["race_key"] = i
			}
		}
	}()

	Flush(2 * time.Second)
	close(stopMutate)
	wg.Wait()
	Shutdown(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(rawBodies) == 0 {
		t.Fatalf("未收到 /batch/ 请求")
	}
	joined := strings.Join(rawBodies, "\n")
	for _, leak := range []string{"Alice", "private.txt", "username", "mutated", "race_key"} {
		if strings.Contains(joined, leak) {
			t.Errorf("出网 JSON 残留敏感字段或被并发改写污染 %q：%s", leak, joined)
		}
	}
	if !strings.Contains(joined, `"stable":"ok"`) || !strings.Contains(joined, `"detail":"\u003cpath\u003e"`) {
		t.Errorf("合法清洗字段缺失：%s", joined)
	}
}

// TestSendRechecksRemoteDisableAndConcurrentTrackDuringFlags 校验：
// 1. 6 小时刷新新增 disabled_events 后，已在 lifecycleBuf 重试中的历史事件出网前被剔除不再发送；
// 2. Track 按默认配置通过后挂起、首拉复筛完成再恢复入队，仍受最新远端关闭规则拦截，无 TOCTOU 逃逸。
//
// 变异自证：发包前不复核最新 disabled_events/disabled_categories 或只靠首拉一次性 refilter，本测试变红。
func TestSendRechecksRemoteDisableAndConcurrentTrackDuringFlags(t *testing.T) {
	t.Run("6小时刷新禁用app_start后历史503重试项不再发出", func(t *testing.T) {
		var flagsCalls atomic.Int64
		var batchFail atomic.Bool
		batchFail.Store(true)
		var failedHits atomic.Int64
		var mu sync.Mutex
		var delivered []capturedEvent

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				n := flagsCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
					return
				}
				_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true,"disabled_events":["app_start"]}}}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			if batchFail.Load() {
				failedHits.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		Start()
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉完成")

		// 首拉允许 app_start，首次发送遭遇 503 留在 lifecycleBuf
		Track(CatLifecycle, "app_start", Props{"boot": 1})
		clk.Advance(30 * time.Second)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool { return failedHits.Load() >= 1 }, "等待首次 503")

		// 恢复 200 并推进 6 小时触发远端配置刷新（返回 disabled_events: ["app_start"]）
		batchFail.Store(false)
		clk.Advance(6*time.Hour + time.Minute)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool { return flagsCalls.Load() >= 2 }, "等待 6 小时配置刷新")

		// 再发一条允许的 app_exit 并 Shutdown，确认仅 app_exit 送达，app_start 被最新规则剔除
		Track(CatLifecycle, "app_exit", Props{"exit_reason": "signal_term"})
		Shutdown(2 * time.Second)

		mu.Lock()
		defer mu.Unlock()
		for _, ev := range delivered {
			if ev.Event == "app_start" {
				t.Errorf("6 小时刷新禁用 app_start 后不应再补发历史 app_start：%v", delivered)
			}
		}
		if len(delivered) != 1 || delivered[0].Event != "app_exit" {
			t.Errorf("期望仅送达 app_exit，实际：%v", delivered)
		}
	})

	t.Run("Track与首拉复筛并发交错不逃逸", func(t *testing.T) {
		var mu sync.Mutex
		var delivered []capturedEvent

		flagsRelease := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/flags") {
				<-flagsRelease
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"featureFlags": {"playercap-rum-config": true},
					"featureFlagPayloads": {
						"playercap-rum-config": {
							"enabled": true,
							"disabled_categories": ["diag_fine"],
							"disabled_events": ["player_session", "app_heartbeat"]
						}
					}
				}`))
				return
			}
			var req capturedBatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delivered = append(delivered, req.Batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		clk := newFakeClock(time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC))
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
			Now:        clk.Now,
		})
		c := current.Load()
		c.tickInterval = 5 * time.Millisecond
		enteredHook := make(chan string, 3)
		resumeTrack := make(chan struct{})
		c.testBeforeEnqueueHook = func(evName string) {
			if evName == "scan_perf_sample" || evName == "player_session" || evName == "app_heartbeat" {
				enteredHook <- evName
				<-resumeTrack
			}
		}

		// 三个并发 Track（覆盖普通队列与 lifecycleBuf）在首拉前通过默认 cfg.allow() 后挂起在入队前
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			Track(CatDiagFine, "scan_perf_sample", Props{"k": 1})
		}()
		go func() {
			defer wg.Done()
			Track(CatPlayer, "player_session", Props{"player": "kugou"})
		}()
		go func() {
			defer wg.Done()
			Track(CatLifecycle, "app_heartbeat", Props{"local_date": "2026-11-27"})
		}()

		for i := 0; i < 3; i++ {
			select {
			case <-enteredHook:
			case <-time.After(1 * time.Second):
				t.Fatal("并发 Track 未进入入队前挂起点")
			}
		}

		// 启动 worker 并放行首拉，等待首拉与 refilter 完全结束
		Start()
		close(flagsRelease)
		waitUntil(t, 2*time.Second, func() bool { return c.flagsReady.Load() }, "首拉及复筛完成")

		// 此时再放行挂起的 Track 写入队列，同时入队一条允许的 app_start，走常态 30s 发批路径验证
		close(resumeTrack)
		wg.Wait()
		Track(CatLifecycle, "app_start", Props{"ok": true})
		clk.Advance(30 * time.Second)
		c.nudge()
		waitUntil(t, 2*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(delivered) >= 1
		}, "等待常态发批送达 app_start")

		Shutdown(2 * time.Second)

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].Event != "app_start" {
			t.Errorf("首拉前通过但后入队的被禁事件不应逃逸发出，期望仅 app_start，实际：%v", delivered)
		}
	})
}

// TestStartShutdownConcurrencyNoLeakOrNetworkAfterClose 校验 Start 与 Shutdown 并发竞态下：
// 绝不在 Shutdown 完成后遗留运行中的 worker 或发起 /flags 网络请求。
//
// 变异自证：Start 与 Shutdown 无 stateMu 串行化，关闭后可能仍启动 worker 并发起 /flags 请求或泄漏协程。
func TestStartShutdownConcurrencyNoLeakOrNetworkAfterClose(t *testing.T) {
	t.Run("Shutdown之后再Start绝不启动worker或发请求", func(t *testing.T) {
		var flagsHits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flagsHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		Init(Options{
			IsRelease:  true,
			AppVersion: "4.0.0",
			DistinctID: "abcdef0123456789abcdef0123456789",
			HTTPClient: srv.Client(),
		})
		c := current.Load()
		Shutdown(100 * time.Millisecond)
		Start()
		time.Sleep(30 * time.Millisecond)

		if c.started.Load() {
			t.Errorf("Shutdown 后调用 Start 不应标记 started")
		}
		if flagsHits.Load() != 0 {
			t.Errorf("Shutdown 后调用 Start 不应发起网络请求：%d", flagsHits.Load())
		}
	})

	t.Run("Start与Shutdown高并发竞态无遗留worker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":{"enabled":true}}}`))
		}))
		defer srv.Close()

		withInjectedCreds(t, "phc_key", srv.URL)
		for iter := 0; iter < 40; iter++ {
			Init(Options{
				IsRelease:  true,
				AppVersion: "4.0.0",
				DistinctID: "abcdef0123456789abcdef0123456789",
				HTTPClient: srv.Client(),
			})
			c := current.Load()

			gate := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-gate
				Start()
			}()
			go func() {
				defer wg.Done()
				<-gate
				Shutdown(500 * time.Millisecond)
			}()
			close(gate)
			wg.Wait()

			if c.started.Load() {
				select {
				case <-c.doneCh:
				case <-time.After(1 * time.Second):
					t.Fatalf("第 %d 轮并发竞争后遗留未退出 worker 协程", iter)
				}
			}
		}
	})
}
