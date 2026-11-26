package cdp

// 本文件只测 Connect 的期限（#231 代码审）：端口接受连接而不完成响应（响应体不结束、WebSocket 握手不完成、握手完成而开域
// 不回复）时，到期即返回错误；调用方取消（停止）时提前返回。连本地的 httptest 服务，真实时钟，期限取得很短。做法同
// player/cloudmusic/cdp/connecttimeout_test.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// connectWithin 在 limit 之内跑完 connect 并返回耗时与错误；超过 limit 仍未返回即判挂住，连上了也判失败。
func connectWithin(t *testing.T, ctx context.Context, base string, timeout, limit time.Duration) (time.Duration, error) {
	t.Helper()
	type result struct {
		c   *Client
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		c, err := connect(ctx, base, timeout)
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

// serve 起一个本地测试服务，返回 http:// 地址。release 在服务关闭之前关闭，放行仍挂着的处理函数。
func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, r, release) }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // 先于 srv.Close 执行
	return srv.URL
}

// pageList 是一份只有 xpui 页面的 /json/list，WebSocket 地址指向本服务的 path。
func pageList(w http.ResponseWriter, r *http.Request, path string) {
	fmt.Fprintf(w, `[{"type":"page","url":"https://xpui.app.spotify.com/index.html","webSocketDebuggerUrl":"ws://%s%s"}]`, r.Host, path)
}

// TestConnectBounded
//
// 变异自证（均实测变红）：connect 不设期限（「响应体不结束：到期」「握手不完成：到期」两例挂住，「开域不回复：到期」等满 3 s）；
// 请求不带 ctx（「响应体不结束」两例挂住）；握手改回 DefaultDialer.DialContext（「握手不完成：停止即返回」一直等到期限：gorilla 的
// DialContext 只按 ctx 的期限设读写期限，取消打断不了握手）；读响应体不设上限（「超过上限」一例）；开域不随 ctx 中止（去掉
// AfterFunc，「开域不回复：到期」一例等满 3 s 才返回）。
func TestConnectBounded(t *testing.T) {
	hungBody := serve(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"type":"page"`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// /json/list 正常，WebSocket 握手请求被接过去之后不回应。
	hungHandshake := serve(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		if r.URL.Path == "/json/list" {
			pageList(w, r, "/ws")
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	})
	// 握手完成，开域的请求只收不回。
	silent := serve(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		if r.URL.Path == "/json/list" {
			pageList(w, r, "/ws")
			return
		}
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		<-release
	})
	// 响应体超过读取上限：只读前 maxJSONBytes 字节，截断的 JSON 解析失败；读完整个响应体会解析成空数组、报「没有页面」。
	big := serve(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		fmt.Fprint(w, "["+strings.Repeat(" ", maxJSONBytes)+"]")
	})
	empty := serve(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) { fmt.Fprint(w, `[]`) })

	expired := func(t *testing.T, base string) {
		el, err := connectWithin(t, context.Background(), base, 300*time.Millisecond, 10*time.Second)
		if err == nil || el < 300*time.Millisecond {
			t.Errorf("%v 返回、错误 %v，期望到期（不早于 300ms）返回错误", el, err)
		}
		if el > 2*time.Second {
			t.Errorf("用了 %v，期望期限一到即返回（开域各有 3 s 的预算，不应等它）", el)
		}
	}
	canceled := func(t *testing.T, base string) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		el, err := connectWithin(t, ctx, base, time.Minute, 10*time.Second)
		if err == nil || el > 5*time.Second {
			t.Errorf("%v 返回、错误 %v，期望随调用方取消即返回", el, err)
		}
	}

	t.Run("响应体不结束：到期返回", func(t *testing.T) {
		_, err := connectWithin(t, context.Background(), hungBody, 300*time.Millisecond, 10*time.Second)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("错误 %v，期望读响应体到期（context.DeadlineExceeded）", err)
		}
	})
	t.Run("响应体不结束：停止即返回", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		_, err := connectWithin(t, ctx, hungBody, time.Minute, 10*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("错误 %v，期望随调用方取消（context.Canceled）", err)
		}
	})
	t.Run("握手不完成：到期返回", func(t *testing.T) { expired(t, hungHandshake) })
	t.Run("握手不完成：停止即返回", func(t *testing.T) { canceled(t, hungHandshake) })
	t.Run("握手完成而开域不回复：到期返回", func(t *testing.T) { expired(t, silent) })
	t.Run("握手完成而开域不回复：停止即返回", func(t *testing.T) { canceled(t, silent) })
	t.Run("响应体超过上限：截断", func(t *testing.T) {
		_, err := connectWithin(t, context.Background(), big, time.Minute, 10*time.Second)
		var se *json.SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("错误 %v，期望截断后的 JSON 解析错误", err)
		}
	})
	// 对照：响应完整而没有页面，读完即返回「没有页面」，不等期限。
	t.Run("对照：响应完整、没有页面", func(t *testing.T) {
		el, err := connectWithin(t, context.Background(), empty, time.Minute, 10*time.Second)
		if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Errorf("错误 %v，期望「没有页面」", err)
		}
		if el > 5*time.Second {
			t.Errorf("用了 %v，响应完整时不应等期限", el)
		}
	})
}
