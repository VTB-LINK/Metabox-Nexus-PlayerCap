package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
)

// 页面里取词的预算必须明显小于 evalTimeout：表达式一旦撞上 evalTimeout，call 会关掉整条会话。
func TestLyricsFetchBudgetBelowEvalTimeout(t *testing.T) {
	if lyricsFetchBudget <= 0 {
		t.Fatalf("lyricsFetchBudget = %v, want > 0", lyricsFetchBudget)
	}
	if margin := evalTimeout - lyricsFetchBudget; margin < 2*time.Second {
		t.Fatalf("evalTimeout - lyricsFetchBudget = %v, want >= 2s (timer alignment + RPC round trip)", margin)
	}
}

// 两处页面内 fetch 都要挂 signal、到点 abort、finally 清计时器，且毫秒数取自传入的预算。
func TestPageFetchJSAbortWiring(t *testing.T) {
	for _, budget := range []time.Duration{1234 * time.Millisecond, lyricsFetchBudget} {
		ms := budget.Milliseconds()
		gens := map[string]string{
			"lyricsFetchJS":  lyricsFetchJS("4uLU6hMCjMI75M1A2tKUQC", "Bearer x", "Win32_x86_64", "1.3.4.258", budget),
			"cachedLyricsJS": cachedLyricsJS("4uLU6hMCjMI75M1A2tKUQC", budget),
		}
		for name, js := range gens {
			for _, want := range []string{
				"new AbortController()",
				"signal:ac.signal",
				"ac.abort()",
				"finally{clearTimeout(tm);}",
				"}," + itoa(ms) + ");",
			} {
				if !strings.Contains(js, want) {
					t.Errorf("%s(budget=%v) missing %q", name, budget, want)
				}
			}
		}
		if js := gens["lyricsFetchJS"]; !strings.Contains(js, "?'"+jsErrTimeout+"':") {
			t.Errorf("lyricsFetchJS(budget=%v) missing timeout error code", budget)
		}
	}
	// 反例：预算不同，生成的毫秒数必须跟着变（不是写死在 JS 里）。
	a := lyricsFetchJS("id", "Bearer x", "", "", 1*time.Second)
	b := lyricsFetchJS("id", "Bearer x", "", "", 2*time.Second)
	if strings.Contains(a, "2000") || strings.Contains(b, "1000") || a == b {
		t.Errorf("budget not threaded into lyricsFetchJS")
	}
	c := cachedLyricsJS("id", 1*time.Second)
	d := cachedLyricsJS("id", 2*time.Second)
	if strings.Contains(c, "2000") || strings.Contains(d, "1000") || c == d {
		t.Errorf("budget not threaded into cachedLyricsJS")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// englishErrors 把错误文案切到英文：errPart 按英文原文断言，测试默认在中文下跑，错误文案会被译成中文。
func englishErrors(t *testing.T) {
	t.Helper()
	i18n.SetLanguage(i18n.English)
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
}

// HTTP 状态的错误带出结构化的状态码（lyric.StatusError，errors.As 可取），文案不变；其余错误不带状态码。
// 变异自证（均实测）：HTTP 状态改回普通错误 → 带状态码的各例红；状态码改从 err 文案里解析 → 「err and status disagree」
// 与三条只带 status 的例红。
func TestParseLyricsFetchResult(t *testing.T) {
	englishErrors(t)
	cases := []struct {
		name    string
		in      string
		body    string
		errPart string // 空 = 期望成功
		status  int    // 0 = 不应是 lyric.StatusError
	}{
		{"ok", `{"ok":true,"status":200,"body":"{\"lyrics\":{}}","err":""}`, `{"lyrics":{}}`, "", 0},
		{"timeout", `{"ok":false,"status":0,"body":"","err":"timeout"}`, "", "timed out after " + lyricsFetchBudget.String(), 0},
		{"http 403 with err", `{"ok":false,"status":403,"body":"","err":"http 403"}`, "", "page-side lyrics fetch returned HTTP 403", 403},
		{"http 404 with err (no lyrics for the track)", `{"ok":false,"status":404,"body":"","err":"http 404"}`, "", "page-side lyrics fetch returned HTTP 404", 404},
		{"http 401 with err (token expired)", `{"ok":false,"status":401,"body":"","err":"http 401"}`, "", "page-side lyrics fetch returned HTTP 401", 401},
		{"err and status disagree: status wins", `{"ok":false,"status":401,"body":"","err":"http 404"}`, "", "HTTP 401", 401},
		{"status only", `{"ok":false,"status":400,"body":""}`, "", "HTTP 400", 400},
		{"status only 404", `{"ok":false,"status":404,"body":""}`, "", "HTTP 404", 404},
		{"ok false with body", `{"ok":false,"status":200,"body":"x","err":"unexpected body"}`, "", "unexpected body", 0},
		{"browser exception text passes through", `{"ok":false,"status":0,"body":"","err":"Failed to fetch"}`, "", "failed: Failed to fetch", 0},
		{"browser exception mentioning 404 is not a status", `{"ok":false,"status":0,"body":"","err":"404 Not Found"}`, "", "failed: 404 Not Found", 0},
		{"empty", ``, "", "returned empty", 0},
		{"not json", `timeout`, "", "failed to parse", 0},
		{"missing ok", `{"status":200,"body":"x"}`, "", "HTTP 200", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := parseLyricsFetchResult(tc.in)
			if tc.errPart == "" {
				if err != nil || body != tc.body {
					t.Fatalf("got (%q, %v), want (%q, nil)", body, err, tc.body)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errPart) || body != "" {
				t.Fatalf("got (%q, %v), want error containing %q", body, err, tc.errPart)
			}
			var se *lyric.StatusError
			if got := errors.As(err, &se); got != (tc.status != 0) || (got && se.Status != tc.status) {
				t.Fatalf("StatusError: as=%v %+v, want status %d", got, se, tc.status)
			}
			if lyric.IsNotFound(err) != (tc.status == 404) {
				t.Fatalf("IsNotFound = %v, want %v", lyric.IsNotFound(err), tc.status == 404)
			}
		})
	}
}

// 缓存层的返回串：cacheNotFound 译成状态码 404 的错误，其余原样返回。
// 变异自证：cacheNotFound 不译（原样返回）→ 本例红（实测）。
func TestParseCachedLyricsResult(t *testing.T) {
	englishErrors(t)
	body, err := parseCachedLyricsResult(cacheNotFound)
	if body != "" || !lyric.IsNotFound(err) || err.Error() != "page-cache lyrics fetch returned HTTP 404" {
		t.Fatalf("cacheNotFound: got (%q, %v), want a 404 StatusError", body, err)
	}
	for _, in := range []string{"", lyricsBody, "http 401", "HTTP 404", "http 404 "} {
		if body, err := parseCachedLyricsResult(in); body != in || err != nil {
			t.Errorf("%q: got (%q, %v), want it unchanged", in, body, err)
		}
	}
}

// ---- 以下用 node 跑生成的 JS（带桩 fetch），找不到 node 就跳过 ----

// nodeStubs：hang 返回一个只在 signal 中止时才失败的 promise（与浏览器 fetch 的中止语义同形）。
const nodeStubs = `
function hang(sig){return new Promise(function(_,rej){
  if(!sig){return;}
  if(sig.aborted){rej(new DOMException('This operation was aborted','AbortError'));return;}
  sig.addEventListener('abort',function(){rej(new DOMException('This operation was aborted','AbortError'));});
});}
function resp(status,text){return {ok:status>=200&&status<300,status:status,text:function(){return Promise.resolve(text);}};}
var calls=0;
`

const lyricsBody = `{"lyrics":{"syncType":"LINE_SYNCED","lines":[]}}`

// runNode 在 node 里跑 setup 后求值 expr，返回表达式结果与表达式自身耗时；进程墙钟另行返回。
func runNode(t *testing.T, setup, expr string) (string, time.Duration, time.Duration, int) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping JS behaviour check")
	}
	script := nodeStubs + setup + `
var t0=Date.now();
Promise.resolve(` + expr + `).then(function(v){process.stdout.write(JSON.stringify({v:v,ms:Date.now()-t0,calls:calls}));});
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-")
	cmd.Stdin = strings.NewReader(script)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("node failed: %v\nstderr: %s", err, stderr.String())
	}
	wall := time.Since(start)
	var res struct {
		V     string `json:"v"`
		Ms    int64  `json:"ms"`
		Calls int    `json:"calls"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("bad node output %q: %v", out.String(), err)
	}
	return res.V, time.Duration(res.Ms) * time.Millisecond, wall, res.Calls
}

func TestLyricsFetchJSBehaviour(t *testing.T) {
	englishErrors(t)
	const short = 150 * time.Millisecond
	cases := []struct {
		name    string
		fetch   string
		budget  time.Duration
		body    string
		errPart string
	}{
		{"headers never arrive", `function(u,o){return hang(o.signal);}`, short, "", "timed out after"},
		{"body never finishes", `function(u,o){return Promise.resolve({ok:true,status:200,text:function(){return hang(o.signal);}});}`, short, "", "timed out after"},
		{"fast success", `function(u,o){return Promise.resolve(resp(200,` + jsQuote(lyricsBody) + `));}`, time.Minute, lyricsBody, ""},
		{"http 403 is not timeout", `function(u,o){return Promise.resolve(resp(403,''));}`, time.Minute, "", "HTTP 403"},
		{"http 404 (no lyrics for the track)", `function(u,o){return Promise.resolve(resp(404,''));}`, time.Minute, "", "HTTP 404"},
		{"http 401 (token expired)", `function(u,o){return Promise.resolve(resp(401,''));}`, time.Minute, "", "HTTP 401"},
		{"network error is not timeout", `function(u,o){return Promise.reject(new TypeError('Failed to fetch'));}`, time.Minute, "", "Failed to fetch"},
		{"unexpected body", `function(u,o){return Promise.resolve(resp(200,'<html>'));}`, time.Minute, "", "unexpected body"},
		{"request carries the signal", `function(u,o){if(!o||!o.signal)throw new Error('no signal');return hang(o.signal);}`, short, "", "timed out after"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, took, wall, _ := runNode(t, "globalThis.fetch="+tc.fetch+";",
				lyricsFetchJS("4uLU6hMCjMI75M1A2tKUQC", "Bearer x", "Win32_x86_64", "1.3.4.258", tc.budget))
			body, err := parseLyricsFetchResult(v)
			if tc.errPart == "" {
				if err != nil || body != tc.body {
					t.Fatalf("got (%q, %v), want (%q, nil)", body, err, tc.body)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("got (%q, %v), want error containing %q", body, err, tc.errPart)
			}
			if want := strings.HasSuffix(tc.errPart, "HTTP 404"); lyric.IsNotFound(err) != want {
				t.Fatalf("IsNotFound = %v, want %v (err %v)", !want, want, err)
			}
			if tc.errPart != "" && strings.HasPrefix(tc.errPart, "timed out") && took > tc.budget+time.Second {
				t.Fatalf("aborted after %v, budget %v", took, tc.budget)
			}
			// 预算一分钟的用例：计时器没被清掉的话 node 要等满一分钟才退出（会撞上 20s 的上限）。
			if tc.budget == time.Minute && wall > 10*time.Second {
				t.Fatalf("node lingered %v; timer not cleared", wall)
			}
		})
	}
}

func TestCachedLyricsJSBehaviour(t *testing.T) {
	const short = 150 * time.Millisecond
	entries := `Object.defineProperty(globalThis,'performance',{configurable:true,writable:true,value:{getEntriesByType:function(){return [
  {name:'https://spclient.wg.spotify.com/color-lyrics/v2/track/ID/image/a?format=json'},
  {name:'https://spclient.wg.spotify.com/color-lyrics/v2/track/OTHER/image/b'},
  {name:'https://spclient.wg.spotify.com/color-lyrics/v2/track/ID/image/c?format=json'}];}}});`
	cases := []struct {
		name      string
		fetch     string
		budget    time.Duration
		want      string
		wantCalls int
	}{
		// 两个候选共用一个预算：第一个挂住到点后，第二个立即失败，总耗时不叠加。
		{"every fetch hangs", `function(u,o){calls++;return hang(o&&o.signal);}`, short, "", 2},
		{"body hangs", `function(u,o){calls++;return Promise.resolve({ok:true,status:200,text:function(){return hang(o&&o.signal);}});}`, short, "", 2},
		{"newest url hits", `function(u,o){calls++;return Promise.resolve(resp(200,u.indexOf('/c?')>=0?` + jsQuote(lyricsBody) + `:''));}`, time.Minute, lyricsBody, 1},
		{"newest misses, older hits", `function(u,o){calls++;return Promise.resolve(u.indexOf('/a?')>=0?resp(200,` + jsQuote(lyricsBody) + `):resp(401,''));}`, time.Minute, lyricsBody, 2},
		{"all miss", `function(u,o){calls++;return Promise.resolve(resp(401,''));}`, time.Minute, "", 2},
		// 404：没有一个候选取到歌词、而其中有一个答复 404 时返回 cacheNotFound；取到歌词的优先。
		// 变异自证：去掉 `nf=true` → 本组返回 cacheNotFound 的五例红；404 也照 ok 读 body → 「404 with a lyrics-like body」红（均实测）。
		{"newest 404, older 401", `function(u,o){calls++;return Promise.resolve(resp(u.indexOf('/c?')>=0?404:401,''));}`, time.Minute, cacheNotFound, 2},
		{"newest 401, older 404", `function(u,o){calls++;return Promise.resolve(resp(u.indexOf('/a?')>=0?404:401,''));}`, time.Minute, cacheNotFound, 2},
		{"all 404", `function(u,o){calls++;return Promise.resolve(resp(404,''));}`, time.Minute, cacheNotFound, 2},
		{"newest 404, older hits", `function(u,o){calls++;return Promise.resolve(u.indexOf('/a?')>=0?resp(200,` + jsQuote(lyricsBody) + `):resp(404,''));}`, time.Minute, lyricsBody, 2},
		{"404 then the older one hangs past the budget", `function(u,o){calls++;return u.indexOf('/c?')>=0?Promise.resolve(resp(404,'')):hang(o&&o.signal);}`, short, cacheNotFound, 2},
		{"404 with a lyrics-like body is still 404", `function(u,o){calls++;return Promise.resolve(resp(404,` + jsQuote(lyricsBody) + `));}`, time.Minute, cacheNotFound, 2},
		{"403 and 410 are not 404", `function(u,o){calls++;return Promise.resolve(resp(u.indexOf('/c?')>=0?403:410,''));}`, time.Minute, "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, took, wall, calls := runNode(t, entries+"globalThis.fetch="+tc.fetch+";", cachedLyricsJS("ID", tc.budget))
			if v != tc.want || calls != tc.wantCalls {
				t.Fatalf("got %q after %d fetches, want %q after %d", v, calls, tc.want, tc.wantCalls)
			}
			if tc.budget == short && took > tc.budget+time.Second {
				t.Fatalf("returned after %v, budget %v", took, tc.budget)
			}
			if tc.budget == time.Minute && wall > 10*time.Second {
				t.Fatalf("node lingered %v; timer not cleared", wall)
			}
		})
	}
}

func jsQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
