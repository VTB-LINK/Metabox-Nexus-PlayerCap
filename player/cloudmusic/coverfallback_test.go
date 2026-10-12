package cloudmusic

// 本文件只测封面下载的兜底（issue #109）：fetchCoverWithFallback 先用改写后的 URL 取，取不到且改写过时
// 退回改写前的 URL 再取一次。下载函数注入桩，不联网。

import (
	"slices"
	"testing"
)

// 变异自证（实测）：去掉兜底（只取改写后的）→「原 URL 取到」「两者都取不到」两例红；不判改写前后是否相同
// →「改写前后相同、取不到」「空 URL」两例红（下载两次）；改写后的取到了仍去取原 URL →「改写后的取到」一例红；
// 兜底时取的仍是改写后的 →「原 URL 取到」「两者都取不到」两例红。
func TestFetchCoverWithFallback(t *testing.T) {
	const (
		normalized = neteaseCover + "?param=800y800"
		raw        = neteaseCover
	)
	cases := []struct {
		name       string
		normalized string
		raw        string
		serves     map[string]string // URL → 桩返回的 b64，缺省为取不到
		want       string
		wantCalls  []string
	}{
		{"改写后的取到", normalized, raw, map[string]string{normalized: "b64-800", raw: "b64-raw"}, "b64-800", []string{normalized}},
		{"改写后的取不到、原 URL 取到", normalized, raw, map[string]string{raw: "b64-raw"}, "b64-raw", []string{normalized, raw}},
		{"两者都取不到", normalized, raw, nil, "", []string{normalized, raw}},
		{"改写前后相同、取不到：只下载一次", normalized, normalized, nil, "", []string{normalized}},
		{"改写前后相同、取到：只下载一次", normalized, normalized, map[string]string{normalized: "b64-800"}, "b64-800", []string{normalized}},
		{"空 URL：只下载一次", "", "", nil, "", []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			fetch := func(u string) string {
				calls = append(calls, u)
				return c.serves[u]
			}
			if got := fetchCoverWithFallback(fetch, c.normalized, c.raw); got != c.want {
				t.Errorf("返回 %q，应为 %q", got, c.want)
			}
			if !slices.Equal(calls, c.wantCalls) {
				t.Errorf("下载顺序 %q，应为 %q", calls, c.wantCalls)
			}
		})
	}
}
