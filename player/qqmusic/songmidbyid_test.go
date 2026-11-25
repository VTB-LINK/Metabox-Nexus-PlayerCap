package qqmusic

// 本文件只测 fetchSongMidByID：按 songid 经 get_song_detail_yqq（同 fetchCoverURL 的请求）查 songmid，供读取者兜底
// （设计 §6.4 第 2 步「兜底」）。只交出核验过的 mid。不联网：httpClient 换成假 Transport（fakeAPI）。

import (
	"errors"
	"strings"
	"testing"
)

func TestFetchSongMidByID(t *testing.T) {
	reqs := fakeAPI(t, 200, trackBody(0, `"id":268000001,"mid":"000TestMid0001",`+testAlbum), nil)
	mid, err := fetchSongMidByID(268000001)
	if err != nil || mid != "000TestMid0001" {
		t.Fatalf("fetchSongMidByID = %q, %v；应为 000TestMid0001", mid, err)
	}
	if len(*reqs) != 1 || !strings.HasPrefix((*reqs)[0], "https://u.y.qq.com/cgi-bin/musicu.fcg ") ||
		!strings.Contains((*reqs)[0], `"method":"get_song_detail_yqq"`) || !strings.Contains((*reqs)[0], `"song_id":268000001`) {
		t.Fatalf("请求 %q，应为一次按 songid 的 get_song_detail_yqq", *reqs)
	}

	// 反例：id 是另一首的、缺 id、code 非 0、mid 不像 songmid、不是 JSON、HTTP 错误，都报错、返回空串。
	for name, c := range map[string]struct {
		body string
		err  error
	}{
		"id 不等":    {body: trackBody(0, `"id":268000002,"mid":"000TestMid0002"`)},
		"缺 id":     {body: trackBody(0, `"mid":"000TestMid0001"`)},
		"id 为字符串":  {body: trackBody(0, `"id":"268000001","mid":"000TestMid0001"`)},
		"code 非 0": {body: trackBody(-1, `"id":268000001,"mid":"000TestMid0001"`)},
		"mid 为空":   {body: trackBody(0, `"id":268000001,"mid":""`)},
		"mid 全是数字": {body: trackBody(0, `"id":268000001,"mid":"268000001"`)},
		"不是 JSON":  {body: `<html>502</html>`},
		"HTTP 错误":  {err: errors.New("dial tcp: i/o timeout")},
	} {
		t.Run(name, func(t *testing.T) {
			reqs := fakeAPI(t, 200, c.body, c.err)
			if mid, err := fetchSongMidByID(268000001); err == nil || mid != "" {
				t.Fatalf("fetchSongMidByID = %q, %v；应为空串与错误", mid, err)
			}
			if len(*reqs) != 1 {
				t.Fatalf("请求 %d 次，应恰一次（失败不在这里重试）", len(*reqs))
			}
		})
	}

	// 所查为 0：同样只交核验过的，响应即便带 id 0 与合规的 mid 也报错。
	fakeAPI(t, 200, trackBody(0, `"mid":"000TestMid0001"`), nil)
	if mid, err := fetchSongMidByID(0); err == nil || mid != "" {
		t.Fatalf("fetchSongMidByID(0) = %q, %v；应为空串与错误", mid, err)
	}
}
