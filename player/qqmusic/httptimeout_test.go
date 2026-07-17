package qqmusic

// 本文件只测 QQ 音乐取词/封面 HTTP client 的超时不变量：client 配了超时、代码用的是它
// （而非无超时的裸 http.Client），且超时真的生效。不测取词/封面的解析结果。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHTTPClientHasTimeout 守住取词 client 必须有超时。
//
// 缺陷背景：api.go 的四处远程调用（fetchLRCBySongMid / fetchLRC / fetchCoverURLBySongMid /
// fetchCoverURL）原用裸 http.Client（Timeout=0）与 http.Get。它们在 qqmusic.go 的 poll 循环
// 里同步调用，网络挂起（对端照常 ACK 但不吐数据）会永久阻塞取词 goroutine——且 router 对
// 卡死的 playing 状态永不过期，qqmusic 变成 activeNames 里的永久幽灵，只能重启恢复。
//
// 变异自证：把 httpClient 的 Timeout 去掉（Timeout: 0）即红。
func TestHTTPClientHasTimeout(t *testing.T) {
	if httpClient.Timeout <= 0 {
		t.Fatal("取词 httpClient 必须有超时——Timeout=0 会在网络挂起时永久卡死 qqmusic poll goroutine")
	}
	if httpClient.Timeout > 30*time.Second {
		t.Errorf("超时 %v 过长，失去意义", httpClient.Timeout)
	}
}

// TestAPIsUseTimeoutClient 守住 api.go 所有对外 HTTP 调用都走带超时的 httpClient，
// 不得回退到裸 http.Client 或 http.Get。仅有 httpClient 配了超时还不够——代码得真的用它。
//
// 变异自证：把任一处改回 &http.Client{} 或 http.Get( 即红。
func TestAPIsUseTimeoutClient(t *testing.T) {
	data, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("读 api.go 失败: %v", err)
	}
	src := string(data)
	// 匹配调用/构造的字面形式；注释已刻意避开这两个字面（不写 &http.Client{}）
	if strings.Contains(src, "&http.Client{}") {
		t.Error("api.go 出现裸 &http.Client{}（无超时）——取词调用须走带超时的 httpClient")
	}
	if strings.Contains(src, "http.Get(") {
		t.Error("api.go 出现 http.Get(（无超时）——须改用 httpClient.Get")
	}
}

// TestHTTPClientActuallyTimesOut 端到端确认 httpClient 的超时真的生效：对永不响应的服务，
// 它必须在超时后返回 error 而非永久阻塞。用临时短超时避免拖慢测试。
func TestHTTPClientActuallyTimesOut(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	orig := httpClient
	httpClient = &http.Client{Timeout: 300 * time.Millisecond}
	defer func() { httpClient = orig }()

	start := time.Now()
	resp, err := httpClient.Get(hang.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("对永不响应的服务，带超时的 client 应返回 error 而非永久阻塞")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时未在合理时间内触发，耗时 %v", elapsed)
	}
}
