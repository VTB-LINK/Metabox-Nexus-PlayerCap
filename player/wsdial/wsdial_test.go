package wsdial

// 本文件测 Dial 的中止（#231 代码审）：对端接下连接而不回应握手时，ctx 取消（停止）即返回、期限到即返回；握手完成之后
// ctx 再结束不关连接。连本地的 httptest 服务，真实时钟。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialWithin 在 limit 之内跑完 Dial 并返回耗时、连接与错误；超过 limit 仍未返回即判挂住。
func dialWithin(t *testing.T, ctx context.Context, url string, limit time.Duration) (time.Duration, *websocket.Conn, error) {
	t.Helper()
	type result struct {
		c   *websocket.Conn
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		c, err := Dial(ctx, url)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return time.Since(start), r.c, r.err
	case <-time.After(limit):
		t.Fatalf("Dial 超过 %v 仍未返回", limit)
		return 0, nil, nil
	}
}

// TestDial
//
// 变异自证（均实测变红）：Dial 改回 websocket.DefaultDialer.DialContext（「握手挂住：停止即返回」等到 45 s 的握手上限，
// 判挂住）；握手完成后不撤销 ctx 结束时的关闭（去掉 stop()，「握手完成之后取消」一例连接被关）。
func TestDial(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	defer hung.Close()
	defer close(release) // 先于 hung.Close 放行仍挂着的处理函数
	hungURL := "ws" + strings.TrimPrefix(hung.URL, "http")

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, b); err != nil {
				return
			}
		}
	}))
	defer echo.Close()
	echoURL := "ws" + strings.TrimPrefix(echo.URL, "http")

	t.Run("握手挂住：停止即返回", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		el, c, err := dialWithin(t, ctx, hungURL, 10*time.Second)
		if err == nil || c != nil || el > 5*time.Second {
			t.Errorf("%v 返回、错误 %v，期望随取消即返回错误（不等期限）", el, err)
		}
	})
	t.Run("握手挂住：期限到即返回", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		el, c, err := dialWithin(t, ctx, hungURL, 10*time.Second)
		if err == nil || c != nil || el < 300*time.Millisecond {
			t.Errorf("%v 返回、错误 %v，期望到期（不早于 300ms）返回错误", el, err)
		}
	})
	t.Run("握手完成之后取消：连接照常可用", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_, c, err := dialWithin(t, ctx, echoURL, 10*time.Second)
		if err != nil {
			t.Fatalf("握手失败：%v", err)
		}
		defer c.Close()
		cancel()
		time.Sleep(100 * time.Millisecond) // 留出 ctx 结束时的回调若被误触发的时间
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, []byte("合成")); err != nil {
			t.Fatalf("取消之后写失败：%v", err)
		}
		if _, b, err := c.ReadMessage(); err != nil || string(b) != "合成" {
			t.Fatalf("取消之后读到 %q、错误 %v，期望回显", b, err)
		}
	})
	t.Run("对照：连不上即返回", func(t *testing.T) {
		el, c, err := dialWithin(t, context.Background(), "ws://127.0.0.1:1/", 10*time.Second)
		if err == nil || c != nil || el > 5*time.Second {
			t.Errorf("%v 返回、错误 %v，期望连接被拒即返回错误", el, err)
		}
	})
}
