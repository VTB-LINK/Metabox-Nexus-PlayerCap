package kugou

// 本文件只测 fetchCoverFromAPI 从 API 响应里取出的封面地址：取哪个字段、{size} 换成 800、http 改为 https（#194）。
// 经替换 kugouAPIClient 的 Transport 喂固定响应，不联网。

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// apiCoverWith 让 fetchCoverFromAPI 收到 body 作为 API 响应，返回它给出的封面地址。
func apiCoverWith(t *testing.T, body string) string {
	t.Helper()
	orig := kugouAPIClient
	t.Cleanup(func() { kugouAPIClient = orig })
	kugouAPIClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
	return fetchCoverFromAPI("hash123")
}

// TestFetchCoverFromAPIUsesHTTPS：API 给的 http 地址改成 https 再交出去。singerimg.kugou.com 对 Go 默认 User-Agent 的
// http 请求回 403（2026-11-14 本机实测），不改的话 API 兜底这条路上的 cover_base64 一律为空（#194 的录音即此）。
// 反例：已是 https 的不动；查询串里出现的 http:// 不动；空响应、两个字段都空、不是 JSON 都返回空串。
//
// 变异自证（2026-11-14，隔离克隆，实测）：去掉改 https 的三行，三条 http 开头的用例红；改成
// strings.ReplaceAll(cover, "http://", "https://")，「查询串里的 http:// 不动」一例红。
func TestFetchCoverFromAPIUsesHTTPS(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			"union_cover 的 http 地址改 https、{size} 换 800",
			`{"imgUrl":"http://singerimg.kugou.com/uploadpic/softhead/{size}/a.jpg","trans_param":{"union_cover":"http://singerimg.kugou.com/uploadpic/softhead/{size}/20260702/20260702120041592690.jpg"}}`,
			"https://singerimg.kugou.com/uploadpic/softhead/800/20260702/20260702120041592690.jpg",
		},
		{
			"union_cover 为空时用 imgUrl，同样改 https",
			`{"imgUrl":"http://singerimg.kugou.com/uploadpic/softhead/{size}/b.jpg","trans_param":{"union_cover":""}}`,
			"https://singerimg.kugou.com/uploadpic/softhead/800/b.jpg",
		},
		{
			"已是 https 的不动",
			`{"imgUrl":"https://m.kugou.com/static/images/share2014/default_singer.jpg"}`,
			"https://m.kugou.com/static/images/share2014/default_singer.jpg",
		},
		{
			"只改开头的协议，查询串里的 http:// 不动",
			`{"trans_param":{"union_cover":"http://singerimg.kugou.com/x/{size}/c.jpg?from=http://kugou.com/"}}`,
			"https://singerimg.kugou.com/x/800/c.jpg?from=http://kugou.com/",
		},
		{"两个字段都空", `{"imgUrl":"","trans_param":{"union_cover":""}}`, ""},
		{"空响应", ``, ""},
		{"不是 JSON", `<html>403</html>`, ""},
	}
	for _, c := range cases {
		if got := apiCoverWith(t, c.body); got != c.want {
			t.Errorf("%s：得到 %q，应为 %q", c.name, got, c.want)
		}
	}
}
