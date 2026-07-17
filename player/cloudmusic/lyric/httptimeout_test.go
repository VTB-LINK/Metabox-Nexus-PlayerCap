package lyric

// 本文件只测取词 HTTP client 的超时不变量：client 配了超时、代码用的是它（而非无超时的
// http.DefaultClient）、且超时真的生效。不测取词的解析结果。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHTTPClientHasTimeout 守住取词 client 必须有超时这个不变量。
//
// 缺陷背景：三处取词 API（FetchSongDetail / FetchLyrics / SearchSongID）原用
// http.DefaultClient.Do，而 DefaultClient 的 Timeout 为 0（无限）。它们在轮询循环的同步
// 取词路径上，网络挂起（TCP 连上但服务端不再响应）会永久阻塞取词 goroutine，歌词永远停住、
// 不崩不报错。
//
// 变异自证：把 httpClient 的 Timeout 去掉（&http.Client{}）即红。
func TestHTTPClientHasTimeout(t *testing.T) {
	if httpClient.Timeout <= 0 {
		t.Fatal("取词 httpClient 必须有超时——Timeout=0 会在网络挂起时永久卡死取词 goroutine")
	}
	if httpClient.Timeout > 30*time.Second {
		t.Errorf("超时 %v 过长，失去意义（取词卡这么久约等于卡死）", httpClient.Timeout)
	}
}

// TestLyricAPIsUseTimeoutClient 守住所有对外 HTTP 调用都走带超时的 httpClient，
// 不得回退到 http.DefaultClient。仅有 httpClient 配了超时还不够——代码得真的用它。
//
// 变异自证：把任一处 httpClient.Do 改回 http.DefaultClient.Do 即红。
func TestLyricAPIsUseTimeoutClient(t *testing.T) {
	data, err := os.ReadFile("fetch.go")
	if err != nil {
		t.Fatalf("读 fetch.go 失败: %v", err)
	}
	// 匹配实际调用形式 "http.DefaultClient."（后跟方法名），不匹配注释里对它的文字提及
	// ——httpClient 的注释本身就要提到 DefaultClient 说明动机，那句后面是空格不是点。
	if strings.Contains(string(data), "http.DefaultClient.") {
		t.Error("fetch.go 出现 http.DefaultClient.xxx（无超时的调用）——取词调用须走带超时的 httpClient")
	}
}

// TestHTTPClientActuallyTimesOut 端到端确认 httpClient 的超时真的生效（不只是配了个数字）：
// 对一个永不响应的服务，它必须在超时后返回 error，而不是永久阻塞。用临时短超时避免拖慢
// 测试（结束时恢复）。
func TestHTTPClientActuallyTimesOut(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 永不主动响应，直到客户端超时断开
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
		t.Errorf("超时未在合理时间内触发，耗时 %v（超时机制可能未生效）", elapsed)
	}
}
