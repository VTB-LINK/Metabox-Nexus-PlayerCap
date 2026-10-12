package lyric

// 本文件是 color-lyrics 解析的离线用例。夹具取自真机响应（Spotify 1.3.4.258 抓到的
// 《一起去跑步 — 宇宙人》，只删了用不到的 colors 段），时间戳与文本逐字未改。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// realResponse 是实测响应的裁剪版：保留 syncType / provider / lines 三段与真实取值。
const realResponse = `{
  "lyrics": {
    "syncType": "LINE_SYNCED",
    "lines": [
      {"startTimeMs": "15580", "words": "有多久了 沒有流汗", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""},
      {"startTimeMs": "21980", "words": "有多少運動鞋 還沒有穿", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""},
      {"startTimeMs": "28900", "words": "有多少問題 在追纏", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""},
      {"startTimeMs": "93410", "words": "♪", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""}
    ],
    "provider": "MusixMatch",
    "providerLyricsId": "0",
    "providerDisplayName": "MusixMatch"
  },
  "colors": {"background": "#000000", "text": "#FFFFFF", "highlightText": "#FFFFFF"},
  "hasVocalRemoval": false
}`

func TestParseRealResponse(t *testing.T) {
	resp, err := ParseBody([]byte(realResponse))
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	lines := Parse(resp)
	if len(lines) != 4 {
		t.Fatalf("解析出 %d 行，期望 4 行", len(lines))
	}
	if lines[0].Text != "有多久了 沒有流汗" || lines[0].Time != 15.58 {
		t.Errorf("首行 = %q @ %.2fs，期望 %q @ 15.58s", lines[0].Text, lines[0].Time, "有多久了 沒有流汗")
	}
	if lines[3].Text != "♪" {
		t.Errorf("间奏行 = %q，期望 ♪（平台数据原样保留）", lines[3].Text)
	}
	for i, l := range lines {
		if l.Index != i {
			t.Errorf("第 %d 行的 Index = %d，期望 %d（Index 是「发出去的第几行」）", i, l.Index, i)
		}
	}
	if got := Provider(resp); got != "MusixMatch" {
		t.Errorf("Provider = %q，期望 MusixMatch", got)
	}
	if got := SyncType(resp); got != "LINE_SYNCED" {
		t.Errorf("SyncType = %q，期望 LINE_SYNCED", got)
	}
}

func TestParseSkipsEmptyAndBadTimes(t *testing.T) {
	body := `{"lyrics":{"syncType":"LINE_SYNCED","lines":[
	  {"startTimeMs":"1000","words":"第一行"},
	  {"startTimeMs":"2000","words":""},
	  {"startTimeMs":"3000","words":"   "},
	  {"startTimeMs":"","words":"没有时间戳"},
	  {"startTimeMs":"abc","words":"时间戳不是数字"},
	  {"startTimeMs":"-5","words":"负时间戳"},
	  {"startTimeMs":"4000","words":"第二行"}
	]}}`
	resp, err := ParseBody([]byte(body))
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	lines := Parse(resp)
	if len(lines) != 2 {
		t.Fatalf("解析出 %d 行，期望 2 行（空行与坏时间戳都跳过）", len(lines))
	}
	if lines[0].Text != "第一行" || lines[1].Text != "第二行" {
		t.Errorf("保留下来的行 = %q, %q", lines[0].Text, lines[1].Text)
	}
	if lines[1].Index != 1 {
		t.Errorf("第二行的 Index = %d，期望 1（Index 按发出去的行连续编号）", lines[1].Index)
	}
}

// 每个坏时间戳夹在两行正常行之间：坏行必须被跳过，前后两行的 Index 连续、时间不受影响，
// 结果整体能 json.Marshal（NaN 与无穷大进了载荷会让它报 unsupported value）。
func TestParseRejectsNonFiniteAndOutOfRangeTimes(t *testing.T) {
	bad := []string{
		"NaN", "nan", "+NaN", "Inf", "+Inf", "-Inf", "inf", "infinity", "-Infinity",
		"1e50",  // float64 有限，转 float32 后溢出成 +Inf
		"1e308", // float64 有限的上沿
		"1e400", // ParseFloat 报 ErrRange 并返回 +Inf
		"-1e400",
		"100000000", "100000001", // 恰好上界与超上界
		"-5", "-0.001", "-1e50",
		"", " ", "abc", "12abc", "0x", "1,000",
	}
	for _, s := range bad {
		body := fmt.Sprintf(`{"lyrics":{"syncType":"LINE_SYNCED","lines":[
		  {"startTimeMs":"1000","words":"前一行"},
		  {"startTimeMs":%q,"words":"坏时间戳"},
		  {"startTimeMs":"2500","words":"后一行"}
		]}}`, s)
		resp, err := ParseBody([]byte(body))
		if err != nil {
			t.Fatalf("%q: ParseBody 失败: %v", s, err)
		}
		lines := Parse(resp)
		if len(lines) != 2 {
			t.Errorf("%q: 解析出 %d 行 %v，期望 2 行（坏时间戳那行跳过）", s, len(lines), lines)
			continue
		}
		want := []Line{{Index: 0, Time: 1, Text: "前一行"}, {Index: 1, Time: 2.5, Text: "后一行"}}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("%q: 第 %d 行 = %+v，期望 %+v", s, i, lines[i], want[i])
			}
		}
		if _, err := json.Marshal(lines); err != nil {
			t.Errorf("%q: json.Marshal 失败: %v", s, err)
		}
	}
}

// 接受一侧的反例：零、带空白、长曲目与上界内最大的整毫秒都必须保留，且时间准确。
func TestParseAcceptsLargeButValidTimes(t *testing.T) {
	cases := []struct {
		ms   string
		want float32
	}{
		{"0", 0},
		{" 1000 ", 1},
		{"18300.5", 18.3005},
		{"7200000", 7200},       // 两小时的长曲目
		{"36000000", 36000},     // 十小时
		{"99999999", 99999.999}, // 上界内最大的整毫秒
		{"1e7", 10000},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`{"lyrics":{"syncType":"LINE_SYNCED","lines":[{"startTimeMs":%q,"words":"行"}]}}`, c.ms)
		resp, err := ParseBody([]byte(body))
		if err != nil {
			t.Fatalf("%q: ParseBody 失败: %v", c.ms, err)
		}
		lines := Parse(resp)
		if len(lines) != 1 {
			t.Errorf("%q: 解析出 %d 行，期望 1 行", c.ms, len(lines))
			continue
		}
		if lines[0].Time != c.want || lines[0].Index != 0 {
			t.Errorf("%q: 得到 %+v，期望 Time=%v Index=0", c.ms, lines[0], c.want)
		}
		if _, err := json.Marshal(lines); err != nil {
			t.Errorf("%q: json.Marshal 失败: %v", c.ms, err)
		}
	}
}

// UNSYNCED 的时间轴没有意义（每行 startTimeMs 都是 0），按「没有歌词」处理 —— 原样发出去
// 只会让下游把整首歌词挤在第一秒。
func TestParseRejectsUnsynced(t *testing.T) {
	body := `{"lyrics":{"syncType":"UNSYNCED","lines":[{"startTimeMs":"0","words":"第一行"},{"startTimeMs":"0","words":"第二行"}]}}`
	resp, err := ParseBody([]byte(body))
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	if lines := Parse(resp); len(lines) != 0 {
		t.Fatalf("UNSYNCED 应解析出 0 行，得到 %d 行", len(lines))
	}
}

func TestParseNilAndEmpty(t *testing.T) {
	if lines := Parse(nil); lines != nil {
		t.Errorf("nil 响应应返回 nil，得到 %v", lines)
	}
	resp, err := ParseBody([]byte(`{"lyrics":{"syncType":"LINE_SYNCED","lines":[]}}`))
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	if lines := Parse(resp); lines != nil {
		t.Errorf("空 lines 应返回 nil，得到 %v", lines)
	}
}

func TestParseBodyRejectsGarbage(t *testing.T) {
	if _, err := ParseBody([]byte("not json")); err == nil {
		t.Fatal("非 JSON 应报错")
	}
}

func TestTrackIDFromURI(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"spotify:track:2gXE7Vm07yABwOWxE3TRKY", "2gXE7Vm07yABwOWxE3TRKY"},
		{"spotify:album:3vJXuFG4qRGiZO9BU3XBtF", ""},
		{"spotify:track:", ""},
		{"spotify:track:bad id", ""},
		{"", ""},
		{"2gXE7Vm07yABwOWxE3TRKY", ""},
	}
	for _, c := range cases {
		if got := TrackIDFromURI(c.uri); got != c.want {
			t.Errorf("TrackIDFromURI(%q) = %q，期望 %q", c.uri, got, c.want)
		}
	}
}

// 端点与请求头是「照抄 App 自己的请求」这一约定的落点：format=json 与 market=from_token
// 缺一个都可能被服务端按另一条分支处理，改这里要同步 cdp 包抓到的头。
func TestEndpointShape(t *testing.T) {
	if !strings.Contains(endpoint, "format=json") || !strings.Contains(endpoint, "market=from_token") {
		t.Fatalf("端点查询串缺少 App 同形的参数: %s", endpoint)
	}
	if !strings.HasPrefix(endpoint, "https://spclient.wg.spotify.com/color-lyrics/v2/track/%s?") {
		t.Fatalf("端点路径变了: %s", endpoint)
	}
}
