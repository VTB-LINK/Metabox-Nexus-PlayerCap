package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/rum"
	"Metabox-Nexus-PlayerCap/telemetry"
)

type capturedRUMBatch struct {
	APIKey string `json:"api_key"`
	Batch  []struct {
		Event      string         `json:"event"`
		DistinctID string         `json:"distinct_id"`
		UUID       string         `json:"uuid"`
		Timestamp  string         `json:"timestamp"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type rumTestCollector struct {
	srv      *httptest.Server
	mu       sync.Mutex
	rawBody  []string
	events   []capturedRUMEvent
	flagsHit atomic.Int64
	batchHit atomic.Int64
}

type capturedRUMEvent struct {
	Event      string
	DistinctID string
	Properties map[string]any
}

func newRUMTestCollector(t *testing.T) *rumTestCollector {
	t.Helper()
	c := &rumTestCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			c.flagsHit.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			c.batchHit.Add(1)
			body, _ := io.ReadAll(r.Body)
			var parsed capturedRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.rawBody = append(c.rawBody, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, capturedRUMEvent{
					Event:      ev.Event,
					DistinctID: ev.DistinctID,
					Properties: ev.Properties,
				})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *rumTestCollector) allRawJSON() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.rawBody, "\n")
}

func (c *rumTestCollector) snapshotEvents() []capturedRUMEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedRUMEvent, len(c.events))
	copy(out, c.events)
	return out
}

func findMainOrderingViolations(fset *token.FileSet, f *ast.File) []string {
	var violations []string
	var mainFn, ensureFn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil {
			continue
		}
		switch fd.Name.Name {
		case "main":
			mainFn = fd
		case "ensureCanonicalName":
			ensureFn = fd
		}
	}
	if mainFn == nil {
		return []string{"找不到 main() 函数"}
	}

	callName := func(c *ast.CallExpr) string {
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok {
				return id.Name + "." + fn.Sel.Name
			}
			return fn.Sel.Name
		}
		return ""
	}

	// 检查早退 helper 分支与 ensureCanonicalName 不调用 rum.Init/Start/Track。
	checkNoRUMInNode := func(label string, n ast.Node) {
		ast.Inspect(n, func(cur ast.Node) bool {
			c, ok := cur.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callName(c) {
			case "rum.Init", "rum.Start", "rum.Track", "rum.TrackOnce":
				violations = append(violations, label+" 内禁止调用 "+callName(c))
			}
			return true
		})
	}
	if ensureFn != nil {
		checkNoRUMInNode("ensureCanonicalName", ensureFn.Body)
	}

	posOf := map[string]token.Pos{}
	var kugouIfPos, audioIfPos token.Pos
	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		if ifStmt, ok := n.(*ast.IfStmt); ok {
			ast.Inspect(ifStmt.Cond, func(cn ast.Node) bool {
				lit, ok := cn.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				switch lit.Value {
				case `"--kugou-patch-helper"`:
					kugouIfPos = ifStmt.Pos()
					checkNoRUMInNode("--kugou-patch-helper 分支", ifStmt.Body)
				case `"--audio-capture-helper"`:
					audioIfPos = ifStmt.Pos()
					checkNoRUMInNode("--audio-capture-helper 分支", ifStmt.Body)
				}
				return true
			})
		}
		if c, ok := n.(*ast.CallExpr); ok {
			name := callName(c)
			if _, seen := posOf[name]; !seen {
				posOf[name] = c.Pos()
			}
		}
		return true
	})

	requireCall := func(name string) token.Pos {
		p, ok := posOf[name]
		if !ok {
			violations = append(violations, "main() 缺少调用 "+name)
		}
		return p
	}

	pEnsure := requireCall("ensureCanonicalName")
	pInit := requireCall("rum.Init")
	pNotice := requireCall("telemetry.PrintPrivacyNotice")
	pStart := requireCall("rum.Start")
	pUpdate := requireCall("checkAndUpdate")
	pAppStart := requireCall("emitAppStart")

	if kugouIfPos == 0 {
		violations = append(violations, "main() 缺少 --kugou-patch-helper 早退分支")
	} else if pInit != 0 && kugouIfPos >= pInit {
		violations = append(violations, "--kugou-patch-helper 分支必须早于 rum.Init")
	}
	if audioIfPos == 0 {
		violations = append(violations, "main() 缺少 --audio-capture-helper 早退分支")
	} else if pInit != 0 && audioIfPos >= pInit {
		violations = append(violations, "--audio-capture-helper 分支必须早于 rum.Init")
	}
	if pEnsure != 0 && pInit != 0 && pEnsure >= pInit {
		violations = append(violations, "ensureCanonicalName 必须早于 rum.Init")
	}
	if pInit != 0 && pNotice != 0 && pInit >= pNotice {
		violations = append(violations, "rum.Init 必须位于 telemetry.PrintPrivacyNotice 之前")
	}
	if pNotice != 0 && pStart != 0 && pStart <= pNotice {
		violations = append(violations, "rum.Start 必须位于 telemetry.PrintPrivacyNotice 之后")
	}
	if pStart != 0 && pUpdate != 0 && pStart >= pUpdate {
		violations = append(violations, "rum.Start 必须位于 checkAndUpdate 之前")
	}
	if pUpdate != 0 && pAppStart != 0 && pAppStart <= pUpdate {
		violations = append(violations, "emitAppStart 必须严格位于 checkAndUpdate 之后")
	}
	return violations
}

// TestMainRUMStartupOrdering AST 钉死 main.go 启动与隐私时序红线。
func TestMainRUMStartupOrdering(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}
	if vs := findMainOrderingViolations(fset, f); len(vs) > 0 {
		t.Errorf("main.go RUM 启动时序违规：\n  %s", strings.Join(vs, "\n  "))
	}
}

// TestMainRUMOrderingLintCatchesViolations 反向自证 AST 时序门禁有效。
func TestMainRUMOrderingLintCatchesViolations(t *testing.T) {
	const base = `package main
func ensureCanonicalName() {}
func main() {
	if os.Args[1] == "--kugou-patch-helper" { return }
	if os.Args[1] == "--audio-capture-helper" { return }
	ensureCanonicalName()
	rum.Init(rum.Options{})
	telemetry.PrintPrivacyNotice(0, true, true)
	rum.Start()
	checkAndUpdate(&cfg)
	emitAppStart()
}
`
	cases := map[string]string{
		"rum.Start 早于 PrintPrivacyNotice": strings.Replace(base, "telemetry.PrintPrivacyNotice(0, true, true)\n\trum.Start()", "rum.Start()\n\ttelemetry.PrintPrivacyNotice(0, true, true)", 1),
		"emitAppStart 早于 checkAndUpdate":  strings.Replace(base, "checkAndUpdate(&cfg)\n\temitAppStart()", "emitAppStart()\n\tcheckAndUpdate(&cfg)", 1),
		"helper 分支内调 rum.Start":           strings.Replace(base, `if os.Args[1] == "--audio-capture-helper" { return }`, `if os.Args[1] == "--audio-capture-helper" { rum.Start(); return }`, 1),
		"ensureCanonicalName 内调 rum.Init": strings.Replace(base, "func ensureCanonicalName() {}", "func ensureCanonicalName() { rum.Init(rum.Options{}) }", 1),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "snippet.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if vs := findMainOrderingViolations(fset, f); len(vs) == 0 {
				t.Errorf("时序门禁漏报了违规：%s", name)
			}
		})
	}
}

// TestAppStartStripsUnknownPriorPlayerStrings 验证 app_start 与 config_lifecycle 严格白名单过滤，
// 原始路径文本与未知 YAML 键名零出网。
func TestAppStartStripsUnknownPriorPlayerStrings(t *testing.T) {
	col := newRUMTestCollector(t)
	restore := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restore)

	cfg := config.DefaultConfig()
	cfg.PriorPlayer = []string{`D:\Secret\song.mp3`, "wesing"}
	cfg.ConfigFileState = config.FileLoaded
	cfg.HasCWDConflict = true
	cfg.OrphanBackup = "config.backup-v3-20261127-120000-1.yml"
	cfg.FileRewrite = config.RewriteMigratePending
	cfg.RewriteFailReason = "read_only"
	cfg.MigratedKeys = []string{"offset", "secret_unknown_migrated"}
	cfg.ExplicitKeys = map[string]bool{
		"poll":                    true,
		"custom_unknown_yaml_key": true,
	}
	cfg.WrittenValues = map[string]any{
		"poll":            1,
		"unknown_clamped": 999,
	}
	cfg.RejectedValues = map[string]any{
		"poll":            "bad_str",
		"secret_path_key": `C:\Users\Alice\private.mp3`,
	}

	sysProfile := telemetry.SystemProfile{
		OSVersion:    "10.0.22631",
		OSBuild:      22631,
		OSEdition:    "Professional",
		OSCompatShim: false,
		Arch:         "x64",
		CPUCores:     8,
		Locale:       "zh-CN",
		TimeZone:     "China Standard Time",
		IsElevated:   true,
	}

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		BaseProps:  buildRUMBaseProps(sysProfile),
		HTTPClient: col.srv.Client(),
	})
	rum.Start()

	rum.Track(rum.CatLifecycle, "app_start", buildAppStartProps(&cfg, sysProfile, "fadeout"))
	rum.Track(rum.CatUpdateCfg, "config_lifecycle", buildConfigLifecycleProps(&cfg, "write_failed", false, "", nil))
	rum.Flush(2 * time.Second)

	raw := col.allRawJSON()
	if raw == "" {
		t.Fatal("未收到任何 /batch/ 请求")
	}
	for _, forbidden := range []string{
		"Secret",
		"song.mp3",
		"secret_path_key",
		"custom_unknown_yaml_key",
		"unknown_clamped",
		"secret_unknown_migrated",
		"Alice",
		"private.mp3",
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("出网 JSON 泄露了违禁/未知文本 %q：\n%s", forbidden, raw)
		}
	}

	events := col.snapshotEvents()
	var gotStart, gotCfg map[string]any
	for _, ev := range events {
		switch ev.Event {
		case "app_start":
			gotStart = ev.Properties
		case "config_lifecycle":
			gotCfg = ev.Properties
		}
	}
	if gotStart == nil || gotCfg == nil {
		t.Fatalf("期望收到 app_start 与 config_lifecycle，实际事件：%v", events)
	}

	if !reflect.DeepEqual(gotStart["prior_players"], []any{"wesing"}) {
		t.Errorf("prior_players = %#v，期望 [\"wesing\"]", gotStart["prior_players"])
	}
	if gotStart["unknown_prior_player_count"] != float64(1) {
		t.Errorf("app_start unknown_prior_player_count = %#v，期望 1", gotStart["unknown_prior_player_count"])
	}
	if gotStart["orphans_swept"] != float64(0) {
		t.Errorf("orphans_swept = %#v，期望 0", gotStart["orphans_swept"])
	}
	if gotStart["audio_loopback_supported"] != true {
		t.Errorf("audio_loopback_supported = %#v，期望 true", gotStart["audio_loopback_supported"])
	}
	if !reflect.DeepEqual(gotStart["customized_keys"], []any{"poll"}) {
		t.Errorf("customized_keys = %#v，期望 [\"poll\"]", gotStart["customized_keys"])
	}

	if gotCfg["has_cwd_conflict"] != true || gotCfg["has_orphan_backup"] != true {
		t.Errorf("has_cwd_conflict=%v has_orphan_backup=%v，期望均 true", gotCfg["has_cwd_conflict"], gotCfg["has_orphan_backup"])
	}
	if gotCfg["rewrite_state"] != "migrate_pending" || gotCfg["rewrite_fail_reason"] != "read_only" {
		t.Errorf("rewrite_state=%v rewrite_fail_reason=%v", gotCfg["rewrite_state"], gotCfg["rewrite_fail_reason"])
	}
	if !reflect.DeepEqual(gotCfg["migrated_keys"], []any{"offset"}) {
		t.Errorf("migrated_keys = %#v，期望 [\"offset\"]", gotCfg["migrated_keys"])
	}
	if !reflect.DeepEqual(gotCfg["clamped_keys"], []any{"poll"}) {
		t.Errorf("clamped_keys = %#v，期望 [\"poll\"]", gotCfg["clamped_keys"])
	}
	if !reflect.DeepEqual(gotCfg["rejected_key_types"], map[string]any{"poll": "string"}) {
		t.Errorf("rejected_key_types = %#v，期望 {\"poll\":\"string\"}", gotCfg["rejected_key_types"])
	}
	if gotCfg["readme_sync_err"] != "write_failed" {
		t.Errorf("readme_sync_err = %#v，期望 write_failed", gotCfg["readme_sync_err"])
	}
}

// TestNonReleaseAndEmptyDistinctIDSilent 钉死非发布版或空 DistinctID 时零出网。
func TestNonReleaseAndEmptyDistinctIDSilent(t *testing.T) {
	col := newRUMTestCollector(t)
	restore := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restore)

	cases := []struct {
		name       string
		version    string
		distinctID string
	}{
		{"默认开发版本号_0.0.0", "0.0.0", "0123456789abcdef0123456789abcdef"},
		{"非semver开发版_dev-build", "dev-build", "0123456789abcdef0123456789abcdef"},
		{"发布版但空DistinctID", "4.0.0", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rum.Init(rum.Options{
				IsRelease:  isReleaseVersion(tc.version),
				AppVersion: tc.version,
				DistinctID: tc.distinctID,
				HTTPClient: col.srv.Client(),
			})
			rum.Start()
			rum.Track(rum.CatLifecycle, "app_start", rum.Props{"is_elevated": false})
			rum.Flush(500 * time.Millisecond)
			rum.Shutdown(500 * time.Millisecond)
		})
	}

	if col.flagsHit.Load() != 0 || col.batchHit.Load() != 0 {
		t.Errorf("非发布版或空 DistinctID 必须零出网，实际 /flags=%d /batch=%d", col.flagsHit.Load(), col.batchHit.Load())
	}
}

// TestSendHeartbeatEmitsRUMOnGatewayFailure 模拟网关失败，验证跨日保底上报 app_heartbeat 且同日去重。
func TestSendHeartbeatEmitsRUMOnGatewayFailure(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)

	// 可切换状态的模拟网关：先返回 503 模拟失败，后返回 200。
	var gwStatus atomic.Int32
	gwStatus.Store(http.StatusServiceUnavailable)
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := int(gwStatus.Load())
		if code == 0 {
			// 关闭连接模拟网络断开
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(code)
	}))
	defer gwSrv.Close()

	origEndpoint := versionCheckEndpoint
	origNow := heartbeatNow
	origStart := appStartTime
	versionCheckEndpoint = gwSrv.URL
	withPingDate(t, "")
	resetRUMHeartbeatStateForTest()
	t.Cleanup(func() {
		versionCheckEndpoint = origEndpoint
		heartbeatNow = origNow
		appStartTime = origStart
		resetRUMHeartbeatStateForTest()
		setRUMStatsHooksForTest(nil, nil)
	})

	var clockMu sync.Mutex
	curTime := time.Date(2026, 11, 27, 9, 0, 0, 0, time.Local)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return curTime
	}
	setTime := func(ts time.Time) {
		clockMu.Lock()
		curTime = ts
		clockMu.Unlock()
	}

	appStartTime = curTime.Add(-2 * time.Hour)
	heartbeatNow = nowFn

	var tracksTotal, routeSwitches atomic.Uint64
	tracksTotal.Store(12)
	routeSwitches.Store(5)
	setRUMStatsHooksForTest(
		func() rumRuntimeStats {
			return rumRuntimeStats{
				RouteSwitches:     routeSwitches.Load(),
				PeakWSClients:     3,
				PeakAudioClients:  2,
				PeakEffectClients: 1,
			}
		},
		func() uint64 { return tracksTotal.Load() },
	)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
		Now:        nowFn,
	})
	rum.Start()

	// Day 1 09:00：网络断开（连接直接关闭），markPinged 不应置位，但应发出 1 条 app_heartbeat{gateway_ping_ok:false}
	gwStatus.Store(0)
	rum.AddDropped("ws_sub", 4)
	sendHeartbeat()
	if !shouldPing(nowFn()) {
		t.Error("网关连接失败时不应 markPinged")
	}

	// Day 1 10:00：同日再次失败，不应产生第 2 条 app_heartbeat
	setTime(nowFn().Add(time.Hour))
	tracksTotal.Store(20)
	sendHeartbeat()

	// Day 2 00:30：跨本地日历日仍断网，应产生 Day 2 的 app_heartbeat{gateway_ping_ok:false}
	setTime(time.Date(2026, 11, 28, 0, 30, 0, 0, time.Local))
	routeSwitches.Store(9)
	sendHeartbeat()

	// Day 3 01:00：跨本地日历日且网关恢复 200，应 markPinged 并产生 Day 3 的 app_heartbeat{gateway_ping_ok:true}
	gwStatus.Store(http.StatusOK)
	setTime(time.Date(2026, 11, 29, 1, 0, 0, 0, time.Local))
	tracksTotal.Store(25)
	sendHeartbeat()
	if shouldPing(nowFn()) {
		t.Error("网关恢复 200 后应调用 markPinged")
	}

	rum.Flush(2 * time.Second)

	var heartbeats []capturedRUMEvent
	for _, ev := range col.snapshotEvents() {
		if ev.Event == "app_heartbeat" {
			heartbeats = append(heartbeats, ev)
		}
	}
	if len(heartbeats) != 3 {
		t.Fatalf("期望 3 条跨日 app_heartbeat（同日去重），实际收到 %d 条：%#v", len(heartbeats), heartbeats)
	}

	byDate := map[string]map[string]any{}
	for _, hb := range heartbeats {
		d, _ := hb.Properties["local_date"].(string)
		byDate[d] = hb.Properties
	}
	d1, ok1 := byDate["2026-11-27"]
	d2, ok2 := byDate["2026-11-28"]
	d3, ok3 := byDate["2026-11-29"]
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("缺少期望日期心跳，实得日期键：%v", byDate)
	}
	if d1["gateway_ping_ok"] != false || d1["tracks_recognized_delta"] != float64(12) || d1["route_switches_delta"] != float64(5) {
		t.Errorf("Day 1 心跳属性异常：%#v", d1)
	}
	if d2["gateway_ping_ok"] != false || d2["tracks_recognized_delta"] != float64(8) || d2["route_switches_delta"] != float64(4) {
		t.Errorf("Day 2 心跳属性异常（增量结算应扣除 Day 1）：%#v", d2)
	}
	if d3["gateway_ping_ok"] != true || d3["tracks_recognized_delta"] != float64(5) {
		t.Errorf("Day 3 心跳属性异常：%#v", d3)
	}
}

// TestSyncChineseReadmeReturnsErrorCode 验证说明同步失败时返回不含路径的稳定短码。
func TestSyncChineseReadmeReturnsErrorCode(t *testing.T) {
	dir := t.TempDir()
	if got := syncChineseReadme(dir); got != "" {
		t.Errorf("无 README.html 时应返回空串，实际 %q", got)
	}
	writeFile(t, filepath.Join(dir, readmeName), "<html>ok</html>")
	if got := syncChineseReadme(dir); got != "" {
		t.Errorf("同步成功时应返回空串，实际 %q", got)
	}
	// 目标位置换成目录触发覆盖失败
	_ = os.Remove(filepath.Join(dir, chineseReadmeName))
	if err := os.Mkdir(filepath.Join(dir, chineseReadmeName), 0o755); err != nil {
		t.Fatal(err)
	}
	got := syncChineseReadme(dir)
	if got == "" || strings.Contains(got, dir) || strings.Contains(got, string(os.PathSeparator)) {
		t.Errorf("同步失败应返回不含路径的稳定短码，实际 %q", got)
	}
}

// TestAppendEnvOverridesCaseInsensitive 验证 MBX_RENAMED_LAUNCH 追加不丢失原有环境变量且覆盖同名项。
func TestAppendEnvOverridesCaseInsensitive(t *testing.T) {
	base := []string{"PATH=C:\\Windows", "mbx_renamed_launch=0", "FOO=bar"}
	got := appendEnv(base, []string{envRenamedLaunch + "=1"})
	foundFoo, foundRenamed := false, 0
	for _, kv := range got {
		if kv == "FOO=bar" {
			foundFoo = true
		}
		if strings.EqualFold(strings.SplitN(kv, "=", 2)[0], envRenamedLaunch) {
			foundRenamed++
			if kv != envRenamedLaunch+"=1" {
				t.Errorf("环境变量值异常：%q", kv)
			}
		}
	}
	if !foundFoo || foundRenamed != 1 {
		t.Errorf("appendEnv 结果异常：%v", got)
	}
}

// TestCheckAndUpdateRebuildEmitsEventsAndFlushesWithoutAppStart 验证强重建跳板进程在重启前
// 上报 update_check 与 config_lifecycle（diff_keys 经白名单过滤）、冲刷出网，且绝不发 app_start。
func TestCheckAndUpdateRebuildEmitsEventsAndFlushesWithoutAppStart(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()

	probe := fakeGateway(t, "4.0.0", http.StatusOK, gatewayBody("v4.0.0", "v4.0.0"))
	dir := t.TempDir()
	writeConfigIn(t, dir, v3ConfigText+"secret_free_key: D:\\Private\\x.txt\n")
	cfg := loadConfigIn(t, dir, "4.0.0")

	res := checkAndUpdate(&cfg)
	if probe.restarts != 1 || !res.configLifecycleEmitted {
		t.Fatalf("期望强重建触发重启并标记 configLifecycleEmitted=true，restarts=%d emitted=%v", probe.restarts, res.configLifecycleEmitted)
	}

	raw := col.allRawJSON()
	if strings.Contains(raw, "secret_free_key") || strings.Contains(raw, "Private") {
		t.Errorf("强重建 config_lifecycle 泄露了未知键或路径：\n%s", raw)
	}

	events := col.snapshotEvents()
	var sawCheck, sawCfg, sawAppStart bool
	for _, ev := range events {
		switch ev.Event {
		case "app_start":
			sawAppStart = true
		case "update_check":
			sawCheck = true
			if ev.Properties["outcome"] != "same_version" || ev.Properties["http_status"] != float64(200) {
				t.Errorf("update_check 属性异常：%#v", ev.Properties)
			}
		case "config_lifecycle":
			sawCfg = true
			if ev.Properties["rebuilt"] != true || ev.Properties["rebuild_fail_kind"] != "" {
				t.Errorf("config_lifecycle 重建状态异常：%#v", ev.Properties)
			}
		}
	}
	if sawAppStart {
		t.Error("强重建跳板进程在重启前绝不可发 app_start")
	}
	if !sawCheck || !sawCfg {
		t.Errorf("强重建跳板进程重启前应已冲刷 update_check(%v) 与 config_lifecycle(%v)", sawCheck, sawCfg)
	}
}

// TestRebuildRestartFailureShutsDownRUMWithoutAppStart 验证强重建重启失败路径会调用 rum.Shutdown
// 完成关停冲刷，以退出码 1 退出，且绝不发 app_start。
func TestRebuildRestartFailureShutsDownRUMWithoutAppStart(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()
	if !rum.Enabled() {
		t.Fatal("前置条件：RUM 启动后应处于启用状态")
	}

	_ = fakeGateway(t, "4.0.0", http.StatusOK, gatewayBody("v4.0.0", "v4.0.0"))
	origRestartAttempt, origExit := restartSelfAttempt, exitProcess
	var gotExitCode int
	var exited bool
	// 恢复真实 restartSelf，但把底层重启执行与进程退出替换为可观测桩。
	restartForRebuild = restartSelf
	restartSelfAttempt = func() error { return io.ErrUnexpectedEOF }
	exitProcess = func(code int) {
		exited = true
		gotExitCode = code
	}
	t.Cleanup(func() {
		restartSelfAttempt = origRestartAttempt
		exitProcess = origExit
	})

	dir := t.TempDir()
	writeConfigIn(t, dir, v3ConfigText)
	cfg := loadConfigIn(t, dir, "4.0.0")

	_ = checkAndUpdate(&cfg)

	if !exited || gotExitCode != 1 {
		t.Fatalf("强重建重启失败应以退出码 1 退出，实际 exited=%v code=%d", exited, gotExitCode)
	}
	if rum.Enabled() {
		t.Error("强重建重启失败退出前必须调用 rum.Shutdown，但 RUM 仍处于启用状态")
	}
	events := col.snapshotEvents()
	var sawCheck, sawCfg, sawAppStart bool
	for _, ev := range events {
		switch ev.Event {
		case "app_start":
			sawAppStart = true
		case "update_check":
			sawCheck = true
		case "config_lifecycle":
			sawCfg = true
		}
	}
	if sawAppStart {
		t.Error("强重建重启失败路径绝不可发 app_start")
	}
	if !sawCheck || !sawCfg {
		t.Errorf("强重建重启失败路径应已冲刷 update_check(%v) 与 config_lifecycle(%v)", sawCheck, sawCfg)
	}
}

// TestEmitAppExitReportsPortAndReason 验证 app_exit 携带退出原因、端口、运行秒数与丢包账单。
func TestEmitAppExitReportsPortAndReason(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)

	origStart := appStartTime
	appStartTime = time.Now().Add(-15 * time.Second)
	t.Cleanup(func() {
		appStartTime = origStart
		setRUMStatsHooksForTest(nil, nil)
	})
	setRUMStatsHooksForTest(nil, func() uint64 { return 7 })

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()
	rum.AddDropped("audio_sub", 3)

	emitAppExit("port_bind_failed", listenPortFromAddr("0.0.0.0:8765"))
	rum.Shutdown(2 * time.Second)

	var exitEv *capturedRUMEvent
	for _, ev := range col.snapshotEvents() {
		if ev.Event == "app_exit" {
			cp := ev
			exitEv = &cp
		}
	}
	if exitEv == nil {
		t.Fatal("未收到 app_exit 事件")
	}
	if exitEv.Properties["exit_reason"] != "port_bind_failed" ||
		exitEv.Properties["listen_port"] != float64(8765) ||
		exitEv.Properties["tracks_total"] != float64(7) {
		t.Errorf("app_exit 属性异常：%#v", exitEv.Properties)
	}
}

// TestUpdateTransferMetricsCapture 验证双源竞速胜者、换源切换、锁竞争与错误分类枚举。
func TestUpdateTransferMetricsCapture(t *testing.T) {
	// 1. 锁竞争观测：本进程先持锁，goroutine 等锁并验证 lock_contended=true。
	lockDir := t.TempDir()
	rel1, st1, err := acquireUpdateLockWithStats(lockDir)
	if err != nil {
		t.Fatalf("首次取更新锁失败: %v", err)
	}
	if st1.Contended {
		t.Errorf("无竞争时 Contended 应为 false")
	}
	doneCh := make(chan updateLockStats, 1)
	go func() {
		rel2, st2, e := acquireUpdateLockWithStats(lockDir)
		if e == nil {
			rel2()
		}
		doneCh <- st2
	}()
	time.Sleep(60 * time.Millisecond)
	rel1()
	st2 := <-doneCh
	if !st2.Contended || st2.WaitMs <= 0 {
		t.Errorf("竞争取锁应记录 Contended=true 且 WaitMs>0，实得 %+v", st2)
	}

	// 2. 双源测速 + 换源：china 源返回 404，global 源返回正常附件，应记录 github 胜者或切源成功。
	payload := []byte("hello-update-binary")
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer badSrv.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer goodSrv.Close()

	sources := []updateSource{
		{Name: "gitee.com", Prefix: badSrv.URL + "/"},
		{Name: "github.com", Prefix: goodSrv.URL + "/"},
	}
	assets := []updateAsset{{Name: "a.bin", Size: int64(len(payload))}}
	_, _, fStats, err := fetchAssetsWithStats(goodSrv.Client(), testFetchParams, sources, "v4.0.0", assets, t.TempDir())
	if err != nil {
		t.Fatalf("fetchAssetsWithStats 失败: %v", err)
	}
	if fStats.RaceWinner != "github" {
		t.Errorf("RaceWinner = %q，期望 github", fStats.RaceWinner)
	}

	// 3. 错误分类枚举：SHA256 不符 / 大小不符 / 放置失败 / 来源失败 / 成功。
	if got := classifyUpdateOutcome(nil); got != "ok" {
		t.Errorf("nil 应判 ok，实得 %q", got)
	}
	if got := classifyUpdateOutcome(&placeError{err: io.EOF}); got != "place_err" {
		t.Errorf("placeError 应判 place_err，实得 %q", got)
	}
	if got := classifyUpdateOutcome(&sha256MismatchError{err: io.EOF}); got != "sha256_mismatch" {
		t.Errorf("sha256MismatchError 应判 sha256_mismatch，实得 %q", got)
	}
	if got := classifyUpdateOutcome(&sourceError{err: &sizeMismatchError{err: io.EOF}}); got != "size_mismatch" {
		t.Errorf("sizeMismatchError 应判 size_mismatch，实得 %q", got)
	}
	if got := classifyUpdateOutcome(&sourceError{err: io.EOF}); got != "source_err" {
		t.Errorf("普通 sourceError 应判 source_err，实得 %q", got)
	}
}

// TestHeartbeatPeakResetOnlyOnNewDayNotOnExitOrSameDay 验证周期峰值仅在跨日确认发出 app_heartbeat 后重置，
// 同日重复调用 emitRUMHeartbeatOnce 与退出 emitAppExit 均不得误清周期峰值。
func TestHeartbeatPeakResetOnlyOnNewDayNotOnExitOrSameDay(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)

	origStart := appStartTime
	resetRUMHeartbeatStateForTest()
	t.Cleanup(func() {
		appStartTime = origStart
		resetRUMHeartbeatStateForTest()
		setRUMStatsHooksForTest(nil, nil)
	})

	var takes atomic.Int64
	var wsPeak atomic.Int64
	wsPeak.Store(4)
	setRUMServerStatsHooks(
		func() rumRuntimeStats {
			return rumRuntimeStats{PeakWSClients: wsPeak.Load()}
		},
		func() rumRuntimeStats {
			takes.Add(1)
			p := wsPeak.Swap(1)
			return rumRuntimeStats{PeakWSClients: p}
		},
	)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()

	day1 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.Local)
	appStartTime = day1.Add(-time.Hour)

	// 1. 退出结算只读快照，不得清零周期峰值
	emitAppExit("signal_interrupt", 8765)
	if takes.Load() != 0 || wsPeak.Load() != 4 {
		t.Fatalf("emitAppExit 不得重置周期峰值：takes=%d wsPeak=%d", takes.Load(), wsPeak.Load())
	}

	// 2. Day 1 首次心跳：单步原子取峰（4）并回落为当前在连数 1
	emitRUMHeartbeatOnce(day1, true, 12)
	if takes.Load() != 1 || wsPeak.Load() != 1 {
		t.Fatalf("Day 1 首次心跳后应原子取峰重置 1 次：takes=%d wsPeak=%d", takes.Load(), wsPeak.Load())
	}

	// 3. 同日再次调用 emitRUMHeartbeatOnce：日期去重拦截，不得再次重置峰值
	wsPeak.Store(7)
	emitRUMHeartbeatOnce(day1.Add(time.Hour), true, 10)
	if takes.Load() != 1 || wsPeak.Load() != 7 {
		t.Fatalf("同日重复心跳检查不得重置周期峰值：takes=%d wsPeak=%d", takes.Load(), wsPeak.Load())
	}

	// 4. Day 2 首次心跳：携带 Day 1 后续积累的峰值 7 并重置
	emitRUMHeartbeatOnce(day1.Add(24*time.Hour), true, 11)
	if takes.Load() != 2 || wsPeak.Load() != 1 {
		t.Fatalf("Day 2 首次心跳应原子取峰第 2 次：takes=%d wsPeak=%d", takes.Load(), wsPeak.Load())
	}

	rum.Flush(2 * time.Second)
	peaks := map[string]float64{}
	for _, ev := range col.snapshotEvents() {
		if ev.Event == "app_heartbeat" {
			d, _ := ev.Properties["local_date"].(string)
			p, _ := ev.Properties["peak_ws_clients"].(float64)
			peaks[d] = p
		}
	}
	if peaks["2026-11-27"] != 4 || peaks["2026-11-28"] != 7 {
		t.Fatalf("跨日心跳峰值上报异常：%#v，期望 Day1=4 Day2=7", peaks)
	}
}

// TestTracksTotalHookWiredToPlayerCounter 验证 player.NoteTrackRecognized 经 setRUMTracksTotalHook 贯通心跳与退出汇总，
// 且 main.go 确实完成 player.TracksRecognizedTotal 接线。
func TestTracksTotalHookWiredToPlayerCounter(t *testing.T) {
	t.Cleanup(func() { setRUMStatsHooksForTest(nil, nil) })
	setRUMTracksTotalHook(player.TracksRecognizedTotal)

	before := rumStatsSnapshot().TracksRecognized
	player.NoteTrackRecognized()
	player.NoteTrackRecognized()
	afterExit := rumStatsSnapshot().TracksRecognized
	afterHB := rumHeartbeatStatsSnapshot().TracksRecognized
	if afterExit-before != 2 || afterHB-before != 2 {
		t.Fatalf("识曲总数钩子未贯通：before=%d exit=%d hb=%d", before, afterExit, afterHB)
	}

	// AST 验证 main() 中接入了 setRUMTracksTotalHook(player.TracksRecognizedTotal)。
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "setRUMTracksTotalHook" && len(c.Args) == 1 {
			if sel, ok := c.Args[0].(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "player" && sel.Sel.Name == "TracksRecognizedTotal" {
					wired = true
				}
			}
		}
		return true
	})
	if !wired {
		t.Fatal("main.go 缺少 setRUMTracksTotalHook(player.TracksRecognizedTotal) 接线")
	}
}

type fakeFlushablePlayer struct {
	player.BaseEmitter
	flushed atomic.Int32
}

func (f *fakeFlushablePlayer) Start() {}

func (f *fakeFlushablePlayer) FlushRUM() {
	if f.flushed.Add(1) != 1 {
		return
	}
	player.NoteTrackRecognized()
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":           "wesing",
		"action":           "detached",
		"disconnect_cause": "stopped",
		"duration_sec":     int64(12),
		"tracks_played":    1,
	})
}

// TestShutdownFlushesPlayersBeforeEmitAppExit 验证退出路径先经 flushPlayersRUM 结算活动播放器会话与待报曲目，
// 再由 emitAppExit 汇总包含已结算曲目的 tracks_total 并完成 rum.Shutdown；同时 AST 校验 main.go 信号退出次序。
func TestShutdownFlushesPlayersBeforeEmitAppExit(t *testing.T) {
	col := newRUMTestCollector(t)
	restoreCreds := rum.OverrideCredentialsForTest("phc_test_key", col.srv.URL)
	t.Cleanup(restoreCreds)
	t.Cleanup(func() { setRUMStatsHooksForTest(nil, nil) })
	setRUMTracksTotalHook(player.TracksRecognizedTotal)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()

	before := player.TracksRecognizedTotal()
	fp := &fakeFlushablePlayer{BaseEmitter: player.NewBaseEmitter("wesing")}
	flushPlayersRUM(fp, nil)
	emitAppExit("signal_interrupt", 8765)
	rum.Shutdown(2 * time.Second)

	if fp.flushed.Load() != 1 {
		t.Fatalf("flushPlayersRUM 应调用 FlushRUM 1 次，实得 %d", fp.flushed.Load())
	}

	var sawDetached, sawExit bool
	var exitTracks float64
	for _, ev := range col.snapshotEvents() {
		if ev.Event == "player_session" && ev.Properties["action"] == "detached" && ev.Properties["disconnect_cause"] == "stopped" {
			sawDetached = true
		}
		if ev.Event == "app_exit" {
			sawExit = true
			exitTracks, _ = ev.Properties["tracks_total"].(float64)
		}
	}
	if !sawDetached || !sawExit {
		t.Fatalf("退出路径缺少 detached(%v) 或 app_exit(%v)", sawDetached, sawExit)
	}
	if uint64(exitTracks) < before+1 {
		t.Errorf("app_exit tracks_total = %.0f，应已计入 FlushRUM 结清的曲目（>= %d）", exitTracks, before+1)
	}

	// AST 验证 main.go 的 <-sigCh 退出协程次序：
	// <-sigCh -> audioMgr.BeginShutdown -> effectCapturer.RestoreChrome -> flushPlayersRUM -> srv.FlushRUM -> emitAppExit -> rum.Shutdown
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}
	var sigBlock *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		b, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for _, s := range b.List {
			es, ok := s.(*ast.ExprStmt)
			if !ok {
				continue
			}
			u, ok := es.X.(*ast.UnaryExpr)
			if ok && u.Op == token.ARROW {
				if id, ok := u.X.(*ast.Ident); ok && id.Name == "sigCh" {
					sigBlock = b
					return false
				}
			}
		}
		return true
	})
	if sigBlock == nil {
		t.Fatal("main.go 中未找到 <-sigCh 所在退出块")
	}
	posOf := map[string]token.Pos{}
	ast.Inspect(sigBlock, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok {
				name = id.Name + "." + fn.Sel.Name
			}
		}
		if name != "" {
			if _, seen := posOf[name]; !seen {
				posOf[name] = c.Pos()
			}
		}
		return true
	})
	order := []string{
		"audioMgr.BeginShutdown",
		"effectCapturer.RestoreChrome",
		"effectCapturer.FlushRUM",
		"flushPlayersRUM",
		"srv.FlushRUM",
		"emitAppExit",
		"rum.Shutdown",
	}
	for i, name := range order {
		if posOf[name] == 0 {
			t.Fatalf("main.go 信号退出块缺少 %s 调用", name)
		}
		if i > 0 && posOf[order[i-1]] >= posOf[name] {
			t.Errorf("main.go 信号退出块次序错误：%s 必须早于 %s", order[i-1], name)
		}
	}
}
