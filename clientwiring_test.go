package main

// 本文件守**接线**，不守逻辑。
//
// clientid 包自己的测试全部直接调 Apply / derive —— 它们证明那些函数是对的，
// 但对「main 有没有真的去调它们」一无所知。把 newVersionCheckRequest 里的
// clientid.Apply 整行删掉，clientid 那二十来条用例照样全绿，而线上发出去的请求
// 会退回成一个不带任何身份的裸 GET，网关侧的 DAU 直接归零。
//
// 这正是「N 个变异全红但全打在纯函数上」那种假安全感。下面几条专打接线。

import (
	"net/http"
	"os"
	"testing"

	"Metabox-Nexus-PlayerCap/clientid"
)

// TestVersionCheckRequestCarriesClientHeaders 钉死组装出来的请求真的带着客户端标注。
//
// 不断言 HeaderID：它要读注册表，在没有 MachineGuid 的环境下合法地为空（见 clientid.ID）。
// 断言的三项都只依赖 ldflags 与常量，任何机器上都成立。
//
// 变异自证：删掉 newVersionCheckRequest 里的 clientid.Apply 那行，本用例当场红。
func TestVersionCheckRequestCarriesClientHeaders(t *testing.T) {
	req, err := newVersionCheckRequest(clientid.PingStart)
	if err != nil {
		t.Fatalf("组装版本检查请求失败: %v", err)
	}

	if req.Method != http.MethodGet {
		t.Errorf("方法是 %s，期望 GET", req.Method)
	}
	if req.URL.String() != versionCheckURL {
		t.Errorf("URL 是 %s，期望 %s", req.URL, versionCheckURL)
	}

	if got := req.Header.Get(clientid.HeaderPing); got != clientid.PingStart {
		t.Errorf("%s = %q，期望 %q —— 网关靠它区分「启动」与「每日在线」",
			clientid.HeaderPing, got, clientid.PingStart)
	}
	if got := req.Header.Get(clientid.HeaderVersion); got != Version {
		t.Errorf("%s = %q，期望 %q", clientid.HeaderVersion, got, Version)
	}
	if got := req.Header.Get(clientid.HeaderUA); got == "" {
		t.Errorf("%s 为空 —— 请求会退回 Go 默认 UA，网关日志里认不出是谁", clientid.HeaderUA)
	}
}

// TestHeartbeatRequestIsMarkedDaily 钉死心跳与启动用的是不同的时机标记。
//
// 两者共用同一个 URL，X-Client-Ping 是网关侧唯一能把它们分开的东西。
// 它要是恒为 start，「每日启动次数」这个数会被心跳灌成「在线机器数」，
// 而两个数看起来都很正常。
func TestHeartbeatRequestIsMarkedDaily(t *testing.T) {
	req, err := newVersionCheckRequest(clientid.PingDaily)
	if err != nil {
		t.Fatalf("组装心跳请求失败: %v", err)
	}
	if got := req.Header.Get(clientid.HeaderPing); got != clientid.PingDaily {
		t.Errorf("%s = %q，期望 %q", clientid.HeaderPing, got, clientid.PingDaily)
	}
}

// TestCheckAndUpdateUsesTheAnnotatedRequest 钉死启动那次版本检查走的是带标注的请求。
//
// 它是 AST 断言，不是行为断言 —— 那次请求要真发出去才能观察，而它打的是线上网关。
// 能守住的形状：有人把 client.Do(req) 改回 client.Get(versionCheckURL)（少一行、
// 看着更简洁），于是**启动那一次**不带身份，只剩心跳在报数。DAU 会腰斩，
// 而所有测试照样绿。
func TestCheckAndUpdateUsesTheAnnotatedRequest(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}

	calls, err := callsInFunc(src, "checkAndUpdate")
	if err != nil {
		t.Fatalf("解析 checkAndUpdate 失败: %v", err)
	}
	if calls == nil {
		t.Fatal("main.go 里找不到 checkAndUpdate")
	}

	for _, must := range []struct{ name, why string }{
		{"newVersionCheckRequest", "启动那次检查必须带客户端标注，否则网关只数得到心跳"},
		{"markPinged", "启动已经报过一次在线，不记下来的话心跳会在同一天重复发"},
	} {
		if !contains(calls, must.name) {
			t.Errorf("checkAndUpdate 没有调用 %s —— %s", must.name, must.why)
		}
	}
}

// TestMainStartsHeartbeat 钉死心跳真的被起起来了。
//
// 同样是 AST 断言：main() 跑起来会绑端口、起五个采集 goroutine，测不了。
// 能守住的形状：startDailyHeartbeat 定义得好好的、测试全绿，但没人调它 ——
// 长期挂机的机器一天都不上报，而这个缺失在日志里也看不出来（它本来就只打 Detail）。
func TestMainStartsHeartbeat(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}

	calls, err := callsInFunc(src, "main")
	if err != nil {
		t.Fatalf("解析 main 失败: %v", err)
	}
	if !contains(calls, "startDailyHeartbeat") {
		t.Error("main() 没有调用 startDailyHeartbeat —— 心跳定义了却没起，" +
			"长期挂机的机器只有开播当天算活跃")
	}
}

// TestHeartbeatSkipsNonReleaseBuilds 钉死开发构建不上报。
//
// 判据必须与 checkAndUpdate 同一个（isReleaseVersion）：不一致的话，开发机上每跑一次
// 调试都会往网关灌一条假的「在线」，而那条数据混进 DAU 之后无法事后剔除。
func TestHeartbeatSkipsNonReleaseBuilds(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}

	calls, err := callsInFunc(src, "startDailyHeartbeat")
	if err != nil {
		t.Fatalf("解析 startDailyHeartbeat 失败: %v", err)
	}
	if !contains(calls, "isReleaseVersion") {
		t.Error("startDailyHeartbeat 没有用 isReleaseVersion 门控 —— " +
			"开发构建会往网关灌假的在线数据")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
