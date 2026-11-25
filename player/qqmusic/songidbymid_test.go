package qqmusic

// 本文件只测 fetchSongIDByMid：按 songmid 经单曲接口 fcg_play_single_song.fcg 查数字 songid。不联网：httpClient 换成
// 假 Transport，响应照真机抓到的形状（{"code":0,"data":[{"id":…,"mid":…}]}，查无此歌时 data 为空数组）手写。

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeSingleSong 把 httpClient 换成只回 body（或 err）的假 Transport，用例结束时还原；返回记下的请求地址。
func fakeSingleSong(t *testing.T, body string, err error) *[]string {
	t.Helper()
	var urls []string
	orig := httpClient
	httpClient = &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String())
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
	t.Cleanup(func() { httpClient = orig })
	return &urls
}

func TestFetchSongIDByMid(t *testing.T) {
	urls := fakeSingleSong(t, `{"code":0,"data":[{"action":{},"id":268000001,"mid":"001TestMid0001","name":"测试歌"}],"url":{}}`, nil)
	id, err := fetchSongIDByMid("001TestMid0001")
	if err != nil || id != 268000001 {
		t.Fatalf("fetchSongIDByMid = %d, %v; want 268000001", id, err)
	}
	if len(*urls) != 1 || !strings.HasPrefix((*urls)[0], "https://c.y.qq.com/v8/fcg-bin/fcg_play_single_song.fcg?songmid=001TestMid0001&") {
		t.Fatalf("请求地址 %q，want 单曲接口带 songmid", *urls)
	}
}

// 反例：查无此歌（data 为空）、code 非 0、返回的 mid 不是所查的、id 为 0、不是 JSON、请求失败，都报错、返回 0。
func TestFetchSongIDByMidFailures(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		err  error
	}{
		"查无此歌":      {body: `{"code":0,"data":[],"url":null}`},
		"code 非 0":  {body: `{"code":-1,"data":[{"id":1,"mid":"001TestMid0001"}]}`},
		"mid 不符":    {body: `{"code":0,"data":[{"id":268000002,"mid":"001OtherMid001"}]}`},
		"mid 大小写不同": {body: `{"code":0,"data":[{"id":268000002,"mid":"001testmid0001"}]}`},
		"id 为 0":    {body: `{"code":0,"data":[{"id":0,"mid":"001TestMid0001"}]}`},
		"不是 JSON":   {body: `<html>502</html>`},
		"请求失败":      {err: errors.New("dial tcp: i/o timeout")},
	} {
		t.Run(name, func(t *testing.T) {
			fakeSingleSong(t, c.body, c.err)
			if id, err := fetchSongIDByMid("001TestMid0001"); err == nil || id != 0 {
				t.Fatalf("fetchSongIDByMid = %d, %v; want 0 与错误", id, err)
			}
		})
	}
	// mid 的各种异常写法（oddMids）：该首不合格，同样报错、返回 0。
	for name, raw := range oddMids {
		midField := ""
		if raw != "" {
			midField = `,"mid":` + raw
		}
		t.Run("mid "+name, func(t *testing.T) {
			fakeSingleSong(t, `{"code":0,"data":[{"id":268000001`+midField+`}]}`, nil)
			if id, err := fetchSongIDByMid("001TestMid0001"); err == nil || id != 0 {
				t.Fatalf("fetchSongIDByMid = %d, %v; want 0 与错误", id, err)
			}
		})
	}
}
