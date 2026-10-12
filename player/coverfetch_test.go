package player

// 本文件只测共享下载路径 FetchCoverBase64：下载之后接上了规范化（设计 §9 第 21 条，AST 门禁管不到这一处，
// 门禁允许整个 cover.go 拼 data URI），以及它的 SSRF 拨号检查（第 22 条，此前没有任何用例）。
//
// 变异自证（均实测，逐条记在各用例注释里）。

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// coverServer 起一个回环 HTTP 服务：路径 → (Content-Type, 响应体)；hits 记收到的请求数。
func coverServer(t *testing.T, files map[string][2]string) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		f, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", f[0])
		w.Write([]byte(f[1]))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// 经 FetchCoverBase64 下载的封面被规范化：640² 放大到 800²；800² 原字节放行；800² PNG 按格式标 image/png；
// 非图片字节原样、MIME 沿用 Content-Type；超过 5MB 的不发 base64。
//
// 变异自证：FetchCoverBase64 末尾改回直接 base64 的旧写法，640² 那一步即红；规范化原样退回时把 MIME 写死成
// image/jpeg，「非图片字节」那一步红。
func TestFetchCoverBase64Normalizes(t *testing.T) {
	orig := newCoverClient
	newCoverClient = func(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }
	t.Cleanup(func() { newCoverClient = orig })

	jpg640, jpg800, png800 := photoJPEG(t, 640, 640), photoJPEG(t, 800, 800), photoPNG(t, 800, 800)
	srv, _ := coverServer(t, map[string][2]string{
		"/640.jpg":  {"image/jpeg", string(jpg640)},
		"/800.jpg":  {"image/jpeg", string(jpg800)},
		"/800.png":  {"image/jpeg", string(png800)},
		"/fake.jpg": {"image/jpg", "不是图片"},
		"/big.jpg":  {"image/jpeg", strings.Repeat("x", 5<<20+1)},
	})
	fetch := func(path string) (mime string, raw []byte) {
		t.Helper()
		got := FetchCoverBase64("Test", srv.URL+path, 5*time.Second)
		mime, b64, ok := strings.Cut(strings.TrimPrefix(got, "data:"), ";base64,")
		if !ok {
			t.Fatalf("%s: 应返回 data URI，got %q", path, got[:min(len(got), 40)])
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		return mime, raw
	}

	if mime, raw := fetch("/640.jpg"); mime != "image/jpeg" {
		t.Fatalf("640²: MIME 应为 image/jpeg，got %q", mime)
	} else if _, w, h := decodeSize(t, raw); w != 800 || h != 800 {
		t.Fatalf("640² 应放大到 800×800，got %dx%d", w, h)
	}
	if mime, raw := fetch("/800.jpg"); mime != "image/jpeg" || string(raw) != string(jpg800) {
		t.Fatalf("800² JPEG 应原字节放行，got %q %d 字节", mime, len(raw))
	}
	if mime, raw := fetch("/800.png"); mime != "image/png" || string(raw) != string(png800) {
		t.Fatalf("800² PNG 应原字节放行并标 image/png，got %q %d 字节", mime, len(raw))
	}
	if mime, raw := fetch("/fake.jpg"); mime != "image/jpg" || string(raw) != "不是图片" {
		t.Fatalf("非图片字节应原样、MIME 沿用 Content-Type，got %q %q", mime, raw)
	}
	if got := FetchCoverBase64("Test", srv.URL+"/big.jpg", 5*time.Second); got != "" {
		t.Fatalf("超过 5MB 应返回空串，got %d 字节", len(got))
	}
}

// 未替换的生产客户端（newCoverClient 的默认值）连监听在 127.0.0.1 的服务：拨号检查拒绝，返回空串，服务端
// 收不到请求。非 http(s) 的 URL 不发请求。
//
// 变异自证：删掉生产客户端 Dialer 的 Control，或让 isBlockedCoverIP 恒返回 false，服务端收到请求、返回非空
// 而红。
func TestFetchCoverBase64RefusesLoopback(t *testing.T) {
	srv, hits := coverServer(t, map[string][2]string{"/800.jpg": {"image/jpeg", string(photoJPEG(t, 800, 800))}})
	for _, u := range []string{srv.URL + "/800.jpg", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/800.jpg", "file:///C:/Windows/win.ini"} {
		if got := FetchCoverBase64("Test", u, 2*time.Second); got != "" {
			t.Fatalf("%s: 应拒绝并返回空串，got %d 字节", u, len(got))
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("服务端不应收到请求，got %d 次", n)
	}
}

// isBlockedCoverIP 的分类：回环、RFC1918、链路本地、unspecified、CGNAT、0.0.0.0/8、IPv6 回环 / ULA / 链路本地
// 拦；公网地址与各段的紧邻边界不拦。
//
// 变异自证：CGNAT 的次字节上界改成 `<= 128`，100.128.0.0 红；下界改成 `>= 63`，100.63.255.255 红；去掉
// 0.0.0.0/8 那一支，0.1.2.3 红。
func TestIsBlockedCoverIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "127.255.255.254", "::1", "::ffff:127.0.0.1", "10.0.0.1", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "169.254.1.1", "fe80::1", "0.0.0.0", "::", "100.64.0.0", "100.127.255.255", "0.1.2.3", "fc00::1", "fd12::1"}
	allowed := []string{"8.8.8.8", "1.1.1.1", "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.1", "192.169.0.1",
		"169.255.0.1", "11.0.0.1", "2001:4860:4860::8888"}
	for _, s := range blocked {
		if !isBlockedCoverIP(net.ParseIP(s)) {
			t.Errorf("%s 应拦", s)
		}
	}
	for _, s := range allowed {
		if isBlockedCoverIP(net.ParseIP(s)) {
			t.Errorf("%s 不应拦", s)
		}
	}
}

// 下载完成后，这次下载的空闲连接要被回收：封面客户端每次新建一个 Transport，Transport 默认没有空闲超时，对端保活时
// 连接（与它的两个 goroutine）会一直留着。服务端用 ConnState 数仍然打开的连接，FetchCoverBase64 返回后应降到 0。
//
// 变异自证：去掉 FetchCoverBase64 里的 `defer client.CloseIdleConnections()`，连接数停在 1，等满 2 秒后红。
func TestFetchCoverBase64ClosesIdleConnections(t *testing.T) {
	orig := newCoverClient
	newCoverClient = func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, Transport: &http.Transport{}} // 与生产同形：每次一个新 Transport，保活开着
	}
	t.Cleanup(func() { newCoverClient = orig })

	var open atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(photoJPEG(t, 800, 800))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	for i := 0; i < 3; i++ {
		if got := FetchCoverBase64("Test", srv.URL+"/a.jpg", 5*time.Second); !strings.HasPrefix(got, "data:image/jpeg;base64,") {
			t.Fatalf("第 %d 次下载应返回 data URI", i+1)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("3 次下载之后服务端仍有 %d 条打开的连接，空闲连接没有回收", n)
	}
}
