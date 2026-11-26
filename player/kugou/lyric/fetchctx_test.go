package lyric

// 本文件只测 FetchContext 的在途取消语义：在搜词、目录检索与下载请求阻塞时取消 ctx，须在短时间内
// 返回包装 context.Canceled 的错误，且不继续触发后续回退请求。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// rewriteHostTransport 把发往公网酷狗域名的请求重写到本地 httptest.Server，不访问外网、不走代理。
type rewriteHostTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = rt.target.Scheme
	cloned.URL.Host = rt.target.Host
	cloned.Host = rt.target.Host
	return rt.base.RoundTrip(cloned)
}

func withMockLyricServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	target, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("解析测试服务地址失败: %v", err)
	}
	orig := httpClient
	httpClient = &http.Client{
		Transport: &rewriteHostTransport{
			base:   srv.Client().Transport,
			target: target,
		},
		Timeout: 8 * time.Second,
	}
	t.Cleanup(func() {
		httpClient = orig
		srv.Close()
	})
}

// cancelAfterEntered 等服务端收到阻塞请求后延时 50ms 取消 ctx，并通过返回的 channel 传递取消时刻。
func cancelAfterEntered(t *testing.T, entered <-chan struct{}, cancel context.CancelFunc) <-chan time.Time {
	t.Helper()
	canceled := make(chan time.Time, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	go func() {
		defer close(done)
		select {
		case <-entered:
		case <-stop:
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			canceled <- time.Now()
			cancel()
		case <-stop:
		}
	}()
	return canceled
}

func assertCanceledQuickly(t *testing.T, lines []Line, err error, canceled <-chan time.Time) {
	t.Helper()
	if lines != nil {
		t.Fatalf("取消后不应返回歌词行，得 %d 行", len(lines))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	select {
	case canceledAt := <-canceled:
		if elapsed := time.Since(canceledAt); elapsed > 200*time.Millisecond {
			t.Fatalf("取消后返回耗时 %v（应 <= 200ms）", elapsed)
		}
	default:
		t.Fatal("请求尚未触发 50ms 延迟取消即已返回")
	}
}

func TestFetchContextCancelsInFlightRequest(t *testing.T) {
	t.Run("搜词请求阻塞时取消", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		withMockLyricServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/search" {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			http.NotFound(w, r)
		})
		t.Cleanup(func() { close(release) })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled := cancelAfterEntered(t, entered, cancel)

		lines, err := FetchContext(ctx, "DEADBEF01234567890ABCDEF12345678", 200000, "晴天", "周杰伦")
		assertCanceledQuickly(t, lines, err, canceled)
	})

	t.Run("下载请求阻塞时取消且不回退LRC", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var lrcHits atomic.Int32
		withMockLyricServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/search":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":200,"proposal":"cand1","candidates":[{"id":"cand1","accesskey":"key1","song":"晴天","singer":"周杰伦","duration":200000}]}`))
			case "/download":
				if r.URL.Query().Get("fmt") == "lrc" {
					lrcHits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			default:
				http.NotFound(w, r)
			}
		})
		t.Cleanup(func() { close(release) })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled := cancelAfterEntered(t, entered, cancel)

		lines, err := FetchContext(ctx, "DEADBEF01234567890ABCDEF12345678", 200000, "晴天", "周杰伦")
		assertCanceledQuickly(t, lines, err, canceled)
		if got := lrcHits.Load(); got != 0 {
			t.Fatalf("KRC 下载取消后仍回退请求了 LRC %d 次，want 0", got)
		}
	})

	t.Run("目录检索阻塞时取消且不进入关键词回退", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var keywordSearchHits atomic.Int32
		withMockLyricServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/search":
				if r.URL.Query().Get("keyword") != "" {
					keywordSearchHits.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":200,"candidates":[]}`))
			case "/api/v3/search/song":
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			default:
				http.NotFound(w, r)
			}
		})
		t.Cleanup(func() { close(release) })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled := cancelAfterEntered(t, entered, cancel)

		lines, err := FetchContext(ctx, "DEADBEF01234567890ABCDEF12345678", 200000, "晴天", "周杰伦")
		assertCanceledQuickly(t, lines, err, canceled)
		if got := keywordSearchHits.Load(); got != 0 {
			t.Fatalf("目录检索取消后仍触发了关键词回退搜索 %d 次，want 0", got)
		}
	})
}
