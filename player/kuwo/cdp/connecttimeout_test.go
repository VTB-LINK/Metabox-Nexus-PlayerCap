package cdp

// 本文件只测 Connect 的期限与握手取消（#231）：端口接受连接、发出响应头而不结束响应体时，到期即返回错误；调用方取消
// （停止）时提前返回；WebSocket 握手挂住时同样随取消在期限之前快速返回。连本地的 httptest 服务，真实时钟，期限取得很短。
// 做法同 player/kugou/cdp/connecttimeout_test.go。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// connectWithin 在 limit 之内跑完 connect 并返回耗时与错误；超过 limit 仍未返回即判挂住，连上了也判失败。
func connectWithin(t *testing.T, ctx context.Context, endpoint string, timeout, limit time.Duration) (time.Duration, error) {
	t.Helper()
	type result struct {
		c   *Client
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		c, err := connect(ctx, endpoint, timeout)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.c != nil {
			r.c.Close()
			t.Fatal("应返回错误，却连上了")
		}
		return time.Since(start), r.err
	case <-time.After(limit):
		t.Fatalf("connect 超过 %v 仍未返回", limit)
		return 0, nil
	}
}

// TestConnectBounded
//
// 变异自证（均实测变红）：connectTimeout 改回 3s（默认期限断言红）；io.ReadAll 遇到 ctx 结束仍吞错返 ErrUnavailable
// （「响应体不结束：到期返回」「响应体不结束：停止即返回」均红）；握手改回直接调 DialContext（「握手不完成：停止即返回」
// 等满 3s 握手超时而红）。
func TestConnectBounded(t *testing.T) {
	if connectTimeout != 5*time.Second {
		t.Errorf("connectTimeout = %v，期望 5s（对齐四家 CDP 握手上限）", connectTimeout)
	}

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"type":"page"`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	defer close(release) // 先于 hung.Close 放行仍挂着的处理函数

	t.Run("响应体不结束：到期返回", func(t *testing.T) {
		el, err := connectWithin(t, context.Background(), hung.URL+"/json", 300*time.Millisecond, 10*time.Second)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("错误 %v，期望读响应体到期（context.DeadlineExceeded）", err)
		}
		if el < 300*time.Millisecond {
			t.Errorf("%v 就返回了，早于期限：响应体并没有挂住，用例失效", el)
		}
	})
	t.Run("响应体不结束：停止即返回", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		_, err := connectWithin(t, ctx, hung.URL+"/json", time.Minute, 10*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("错误 %v，期望随调用方取消（context.Canceled）", err)
		}
	})

	// /json 返回符合 selectTarget 校验的本机 netsong 页面，WebSocket 握手请求被接过去之后不回应：
	// 停止须在期限（本例 1 分钟，生产 5 s）之前中止握手（wsdial.Dial）。
	handshakeReached := make(chan struct{}, 1)
	hungHandshake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			fmt.Fprintf(w, `[{"type":"page","url":"file:///C:/Kuwo/9.6.0.0_W1/bin/html/webdata/netsong/index.html","webSocketDebuggerUrl":"ws://%s/devtools/page/1"}]`, r.Host)
			return
		}
		select {
		case handshakeReached <- struct{}{}:
		default:
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	defer hungHandshake.Close()
	t.Run("握手不完成：停止即返回", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		el, err := connectWithin(t, ctx, hungHandshake.URL+"/json", time.Minute, 10*time.Second)
		select {
		case <-handshakeReached:
		default:
			t.Error("未到达 WebSocket 握手请求：selectTarget 可能拦截了测试目标")
		}
		if err == nil || el > 1500*time.Millisecond {
			t.Errorf("%v 返回、错误 %v，期望随调用方取消即返回（不等待握手超时）", el, err)
		}
	})

	// 对照：响应完整而没有页面，读完即返回 ErrUnavailable，不等期限。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer empty.Close()
	t.Run("对照：响应完整、没有页面", func(t *testing.T) {
		el, err := connectWithin(t, context.Background(), empty.URL+"/json", time.Minute, 10*time.Second)
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Errorf("错误 %v，期望 ErrUnavailable", err)
		}
		if el > 5*time.Second {
			t.Errorf("用了 %v，响应完整时不应等期限", el)
		}
	})
}
