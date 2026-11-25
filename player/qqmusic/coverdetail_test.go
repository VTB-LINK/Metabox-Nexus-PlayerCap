package qqmusic

// 本文件只测取封面的请求顺带交出的详情结果（设计 §6.4 第 2 步、§7「编号补全的核验」「请求计数」）：fetchCoverURL 与
// fetchCoverURLBySongMid 的封面地址与改动之前逐例相同（核验不过也照常给封面），另从同一响应交出核验过的另一个编号；
// 每次调用恰一次请求。不联网：httpClient 换成假 Transport，记下每次请求的地址与请求体。

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAPI 把 httpClient 换成回 status 与 body（或 err）的假 Transport，用例结束时还原；返回记下的请求（「地址 请求体」）。
func fakeAPI(t *testing.T, status int, body string, err error) *[]string {
	t.Helper()
	var reqs []string
	orig := httpClient
	httpClient = &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		reqs = append(reqs, r.URL.String()+" "+string(b))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
	t.Cleanup(func() { httpClient = orig })
	return &reqs
}

// trackBody 拼一份 get_song_detail_yqq 的响应；track 是 track_info 的内容（不含花括号）。
func trackBody(code int, track string) string {
	return `{"code":0,"req_0":{"code":` + strconv.Itoa(code) + `,"data":{"track_info":{` + track + `}}}}`
}

const (
	testAlbumMid  = "002TestAlbum01"
	testSingerMid = "003TestSinger1"
	testAlbum     = `"album":{"mid":"` + testAlbumMid + `","pmid":"` + testAlbumMid + `_1","name":"专辑"}`
	testSinger    = `"singer":[{"mid":"` + testSingerMid + `","name":"歌手"}]`
)

func TestFetchCoverURLDetail(t *testing.T) {
	albumCover := buildCoverURL("T002", testAlbumMid, 800)
	pmidCover := buildCoverURL("T002", testAlbumMid+"_1", 800)
	singerCover := buildCoverURL("T001", testSingerMid, 800)
	const mid = "000TestMid0001"
	got := songDetail{state: detailGot, songMid: mid}

	for _, c := range []struct {
		name      string
		status    int
		body      string
		err       error
		wantCover string
		want      songDetail // 已失败时只比 state，seen 另断言
		seenEmpty bool       // 已失败时 seen 应为空（没收到可解析的响应）
	}{
		{"核验通过", 200, trackBody(0, `"id":100,"mid":"`+mid+`",`+testAlbum+`,`+testSinger), nil, albumCover, got, false},
		{"核验通过、封面取专辑 pmid", 200, trackBody(0, `"id":100,"mid":"`+mid+`","album":{"pmid":"`+testAlbumMid+`_1"},`+testSinger), nil, pmidCover, got, false},
		{"核验通过、封面取歌手", 200, trackBody(0, `"id":100,"mid":"`+mid+`",`+testSinger), nil, singerCover, got, false},
		{"核验通过、没有封面", 200, trackBody(0, `"id":100,"mid":"`+mid+`"`), nil, "", got, false},
		// 核验不过：封面照常
		{"id 是另一首的", 200, trackBody(0, `"id":200,"mid":"000TestMid0002",`+testAlbum), nil, albumCover, songDetail{state: detailFailed}, false},
		{"缺 id", 200, trackBody(0, `"mid":"`+mid+`",`+testAlbum), nil, albumCover, songDetail{state: detailFailed}, false},
		{"mid 为空", 200, trackBody(0, `"id":100,"mid":"",`+testAlbum), nil, albumCover, songDetail{state: detailFailed}, false},
		{"mid 全是数字", 200, trackBody(0, `"id":100,"mid":"123",`+testSinger), nil, singerCover, songDetail{state: detailFailed}, false},
		// code 非 0：封面同改动之前为空，详情为已失败、seen 写明 code
		{"code 非 0", 200, trackBody(-1, `"id":100,"mid":"`+mid+`",`+testAlbum), nil, "", songDetail{state: detailFailed}, false},
		// 请求失败、读不出、不是 JSON：封面为空，详情为已失败、seen 为空
		{"HTTP 500", 500, ``, nil, "", songDetail{state: detailFailed}, true},
		{"超时", 0, ``, errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), "", songDetail{state: detailFailed}, true},
		{"body 不是 JSON", 200, `<html>502</html>`, nil, "", songDetail{state: detailFailed}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			reqs := fakeAPI(t, c.status, c.body, c.err)
			cover, d := fetchCoverURL(100, "")
			if cover != c.wantCover {
				t.Fatalf("封面 = %q，应为 %q", cover, c.wantCover)
			}
			if len(*reqs) != 1 || !strings.HasPrefix((*reqs)[0], "https://u.y.qq.com/cgi-bin/musicu.fcg ") ||
				!strings.Contains((*reqs)[0], `"method":"get_song_detail_yqq"`) || !strings.Contains((*reqs)[0], `"song_id":100`) {
				t.Fatalf("请求 %q，应为一次按 songid 100 的 get_song_detail_yqq", *reqs)
			}
			if c.want.state == detailGot {
				if d != c.want {
					t.Fatalf("详情 = %+v，应为 %+v", d, c.want)
				}
				return
			}
			if d.state != detailFailed || d.songID != 0 || d.songMid != "" || (d.seen == "") != c.seenEmpty {
				t.Fatalf("详情 = %+v，应为已失败、编号为零值、seen 为空 %v", d, c.seenEmpty)
			}
		})
	}
}

// singleBody 拼一份 fcg_play_single_song.fcg 的响应；data 是 data 数组的内容（不含方括号）。
func singleBody(code int, data string) string {
	return `{"code":` + strconv.Itoa(code) + `,"data":[` + data + `],"url":{}}`
}

func TestFetchCoverURLBySongMidDetail(t *testing.T) {
	albumCover := buildCoverURL("T002", testAlbumMid, 800)
	singerCover := buildCoverURL("T001", testSingerMid, 800)
	const mid = "000TestMid0001"
	got := songDetail{state: detailGot, songID: 268000001}

	for _, c := range []struct {
		name      string
		body      string
		err       error
		wantCover string
		want      songDetail
		seenEmpty bool
	}{
		{"核验通过", singleBody(0, `{"id":268000001,"mid":"`+mid+`",`+testAlbum+`,`+testSinger+`}`), nil, albumCover, got, false},
		{"核验通过、封面取歌手", singleBody(0, `{"id":268000001,"mid":"`+mid+`",`+testSinger+`}`), nil, singerCover, got, false},
		// 封面照旧取第一首，编号取 mid 相同的那一首
		{"第二首才是所查的", singleBody(0, `{"id":268000002,"mid":"000OtherMid01",`+testAlbum+`},{"id":268000001,"mid":"`+mid+`"}`), nil, albumCover, got, false},
		// 核验不过：封面照常
		{"mid 只差大小写", singleBody(0, `{"id":268000001,"mid":"000testmid0001",`+testAlbum+`}`), nil, albumCover, songDetail{state: detailFailed}, false},
		{"id 为 0", singleBody(0, `{"id":0,"mid":"`+mid+`",`+testAlbum+`}`), nil, albumCover, songDetail{state: detailFailed}, false},
		{"缺 id", singleBody(0, `{"mid":"`+mid+`",`+testSinger+`}`), nil, singerCover, songDetail{state: detailFailed}, false},
		// code 非 0、data 为空：封面同改动之前为空
		{"code 非 0", singleBody(-1, `{"id":268000001,"mid":"`+mid+`",`+testAlbum+`}`), nil, "", songDetail{state: detailFailed}, false},
		{"data 为空", singleBody(0, ``), nil, "", songDetail{state: detailFailed}, false},
		// 请求失败、不是 JSON：seen 为空
		{"请求失败", ``, errors.New("dial tcp: i/o timeout"), "", songDetail{state: detailFailed}, true},
		{"body 不是 JSON", `<html>502</html>`, nil, "", songDetail{state: detailFailed}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			// 经 fetchCoverURL 进入：songmid 非空即走单曲接口，songid 一并给出也不改走详情接口。
			reqs := fakeAPI(t, 200, c.body, c.err)
			cover, d := fetchCoverURL(100, mid)
			if cover != c.wantCover {
				t.Fatalf("封面 = %q，应为 %q", cover, c.wantCover)
			}
			if len(*reqs) != 1 || !strings.HasPrefix((*reqs)[0], "https://c.y.qq.com/v8/fcg-bin/fcg_play_single_song.fcg?songmid="+mid+"&") {
				t.Fatalf("请求 %q，应为一次单曲接口带 songmid", *reqs)
			}
			if c.want.state == detailGot {
				if d != c.want {
					t.Fatalf("详情 = %+v，应为 %+v", d, c.want)
				}
				return
			}
			if d.state != detailFailed || d.songID != 0 || d.songMid != "" || (d.seen == "") != c.seenEmpty {
				t.Fatalf("详情 = %+v，应为已失败、编号为零值、seen 为空 %v", d, c.seenEmpty)
			}
		})
	}
}

// oddIDs 是响应里 id 的各种异常写法（JSON 原文；空串表示缺省）：都不是十进制无符号 32 位整数，核验不过。
// 改动之前封面请求不解析 id，这些响应照常给封面；id 的解码失败不得连带丢掉封面（第 1 轮代码审）。
var oddIDs = map[string]string{
	"字符串":                  `"268359544"`,
	"负数":                   `-268359544`,
	"超出 uint32":            `4294967296`,
	"超出 uint32、低 32 位等于所查": `4563326840`, // 2^32 + 268359544：按 64 位解析再截成 uint32 即误认

	"小数":      `1.5`,
	"小数写法的整数": `268359544.0`,
	"指数写法":    `2.68359544e8`,
	"null":    `null`,
	"数组":      `[268359544]`,
	"缺省":      ``,
}

func TestFetchCoverURLOddID(t *testing.T) {
	albumCover := buildCoverURL("T002", testAlbumMid, 800)
	for name, raw := range oddIDs {
		idField := ""
		if raw != "" {
			idField = `"id":` + raw + `,`
		}
		t.Run("按 songid/"+name, func(t *testing.T) {
			fakeAPI(t, 200, trackBody(0, idField+`"mid":"000TestMid0001",`+testAlbum), nil)
			cover, d := fetchCoverURL(268359544, "")
			if cover != albumCover || d.state != detailFailed || d.songID != 0 || d.songMid != "" || d.seen == "" {
				t.Fatalf("fetchCoverURL = %q, %+v；应为 %q 与已失败", cover, d, albumCover)
			}
		})
		t.Run("按 songmid/"+name, func(t *testing.T) {
			fakeAPI(t, 200, singleBody(0, `{`+idField+`"mid":"000TestMid0001",`+testAlbum+`}`), nil)
			cover, d := fetchCoverURL(0, "000TestMid0001")
			if cover != albumCover || d.state != detailFailed || d.songID != 0 || d.songMid != "" || d.seen == "" {
				t.Fatalf("fetchCoverURL = %q, %+v；应为 %q 与已失败", cover, d, albumCover)
			}
		})
	}
}

// oddMids 是单曲响应里歌曲 mid 的各种异常写法（JSON 原文；空串表示缺省）：都不是 JSON 字符串，该首核验不过。
var oddMids = map[string]string{
	"数字":   `123`,
	"null": `null`,
	"数组":   `["000TestMid0001"]`,
	"对象":   `{"mid":"000TestMid0001"}`,
	"布尔":   `true`,
	"缺省":   ``,
}

// 单曲响应里歌曲的 mid 写法异常（终审 v4 第 3 条）：改动之前封面请求不解析歌曲层级的 mid，这些响应照常给封面；mid 的解码
// 失败不得连带丢掉封面，只使该首核验不过（已失败、seen 写明原文）。同一份 data 里另一首的 mid 正常时，封面照旧取第一首、
// 编号取 mid 相同的那一首。
// 变异自证（实测，仓库外的副本）：singleSong.Mid 改回 string，数字、数组、对象、布尔四种写法的封面为空、「另一首正常」也取不到
// 编号而红；null 的 seen 不写原文而红；缺省两种做法相同。
func TestFetchCoverURLOddMid(t *testing.T) {
	albumCover := buildCoverURL("T002", testAlbumMid, 800)
	singerCover := buildCoverURL("T001", testSingerMid, 800)
	const mid = "000TestMid0001"
	for name, raw := range oddMids {
		midField := ""
		if raw != "" {
			midField = `"mid":` + raw + `,`
		}
		for _, c := range []struct {
			name, cover string
			fields      string
		}{
			{"封面取专辑", albumCover, testAlbum},
			{"封面取歌手", singerCover, testSinger},
		} {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				reqs := fakeAPI(t, 200, singleBody(0, `{"id":268000001,`+midField+c.fields+`}`), nil)
				cover, d := fetchCoverURL(0, mid)
				if cover != c.cover || d.state != detailFailed || d.songID != 0 || d.songMid != "" || d.seen == "" {
					t.Fatalf("fetchCoverURL = %q, %+v；应为 %q 与已失败、带 seen", cover, d, c.cover)
				}
				if raw != "" && !strings.Contains(d.seen, raw) {
					t.Fatalf("seen = %q，应写明 mid 的原文 %s", d.seen, raw)
				}
				if len(*reqs) != 1 {
					t.Fatalf("请求 %d 次，应恰一次", len(*reqs))
				}
			})
		}
		t.Run(name+"/另一首正常", func(t *testing.T) {
			fakeAPI(t, 200, singleBody(0, `{"id":268000002,`+midField+testAlbum+`},{"id":268000001,"mid":"`+mid+`"}`), nil)
			cover, d := fetchCoverURL(0, mid)
			if want := (songDetail{state: detailGot, songID: 268000001}); cover != albumCover || d != want {
				t.Fatalf("fetchCoverURL = %q, %+v；应为 %q 与 %+v", cover, d, albumCover, want)
			}
		})
	}
}

// 每次调用恰一次请求，不为取编号另发请求；核验不过、请求失败也不重试。
func TestFetchCoverURLRequestCount(t *testing.T) {
	for _, c := range []struct {
		name string
		sid  uint32
		smid string
		body string
		err  error
	}{
		{"按 songid、核验通过", 100, "", trackBody(0, `"id":100,"mid":"000TestMid0001",`+testAlbum), nil},
		{"按 songid、核验不过", 100, "", trackBody(0, `"id":200,"mid":"000TestMid0002",`+testAlbum), nil},
		{"按 songid、请求失败", 100, "", ``, errors.New("dial tcp: i/o timeout")},
		{"按 songmid、核验通过", 0, "000TestMid0001", singleBody(0, `{"id":268000001,"mid":"000TestMid0001",`+testAlbum+`}`), nil},
		{"按 songmid、核验不过", 0, "000TestMid0001", singleBody(0, `{"id":0,"mid":"000TestMid0001",`+testAlbum+`}`), nil},
		{"按 songmid、请求失败", 0, "000TestMid0001", ``, errors.New("dial tcp: i/o timeout")},
	} {
		t.Run(c.name, func(t *testing.T) {
			reqs := fakeAPI(t, 200, c.body, c.err)
			fetchCoverURL(c.sid, c.smid)
			if len(*reqs) != 1 {
				t.Fatalf("请求 %d 次（%q），应恰一次", len(*reqs), *reqs)
			}
		})
	}
}
