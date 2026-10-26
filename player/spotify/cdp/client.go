// Package cdp 连 Spotify 桌面版 CEF 的远程调试端口（9223），从它的 xpui 页面里读播放状态，
// 并顺手抓一个可复用的 Bearer —— 取 Spotify 自家歌词要用它（见下面「三条取数路径」）。
//
// 与 cloudmusic/cdp、kugou/cdp、sodamusic/cdp 的关键差异：
//   - 端口 9223、端点 /json/list；目标是**页面**（xpui.app.spotify.com），有完整 DOM。
//   - 播放状态直接从 DOM 读，**不注入任何东西**：标题/歌手/封面取自底栏的 now-playing-widget-v2
//     （1.3.6.1921 起；歌名没有 data-testid，见 snapshotJS）；位置取 [data-testid="progress-bar"]
//     的 `--progress-bar-transform`（每秒刷新的浮点百分比，乘 duration 即毫秒位置），而不是 <input type=range> 的 value —— 后者实测只有 5 秒粒度。
//   - 播放/暂停用**图标的 path 常量**判：按钮的 aria-label 随系统语言变（中文「播放/暂停」），
//     path 的 d 不变。两个常量见 iconPlayingPrefix / iconPausedPrefix。
//   - 收发改成「一个读 goroutine + 按 id 派发」：因为除了 RPC 回复，还要持续接
//     Network.requestWillBeSent 事件来抓 Bearer（同步读写的那套写法收不到事件）。
//
// 三条取数路径（歌词，按优先级）：
//  1. token + track id → Go 侧直接 GET color-lyrics/v2（实测：带 Bearer 时有歌词的曲目都 200，
//     连 /image/ 段都不需要，没有歌词的曲目回 404；不带 Bearer 一律 401，与 cookie 无关）。
//  2. 没有 token 时，走页面的**HTTP 缓存**：App 自己取过的歌词 URL 在缓存里（max-age=3600），
//     用同一个 URL 在页面里 fetch 会命中缓存拿到 200 —— 见 FetchViaPageCache。
//  3. 两条都不成 → 这一首没有歌词，轮询循环稍后重试（见 spotify.go 的补取）。
package cdp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
	"Metabox-Nexus-PlayerCap/telemetry"

	"github.com/gorilla/websocket"
)

var log = logger.New("Spotify] [CDP") // 渲染为 [Spotify] [CDP]

// debugPort 是 watchdog 指定的端口。**两处必须一致**：watchdog.DebugFlag 里的取值是同一件事
// （与 cloudmusic 把 9222 分别写在 watchdog 与 cdp 两处同形）。
const debugPort = 9223

// 单次 RPC 的时间预算。取数走本地回环，实测 evaluate 往返 1~3ms；5s 是「页面卡住」的兜底。
// 超过它 call 会关掉整条会话，所以页面里会等网络的表达式必须自带更短的预算（lyricsFetchBudget）。
const evalTimeout = 5 * time.Second

// lyricsFetchBudget 是页面里取歌词的 fetch（含读 body）的时间预算，到点由 AbortController 中止，
// 按普通失败返回、会话保留。没有它时网络一慢，表达式就撞上 evalTimeout 把会话拆掉（真机：给页面加 8s
// 网络延迟，约每 12s 一圈断开重连，歌名与歌词被清）。与 evalTimeout 之间留 2s：后台页面的定时器
// 可能按 1s 对齐延后触发，另有 evaluate 往返与序列化。
const lyricsFetchBudget = 3 * time.Second

// 连接后开域的时间预算。
const controlTimeout = 3 * time.Second

// pageURLMarker 是目标页面的 URL 特征。CEF 的 /json/list 里还会列出 chrome://omnibox-popup 这类
// browser_ui 目标，它们没有我们的 DOM。
const pageURLMarker = "xpui.app.spotify.com"

// iconPlayingPrefix / iconPausedPrefix 是播放/暂停按钮里那个 svg path 的开头（Encore 图标，语言无关）。
//
// 按钮显示的是「点一下会做什么」：**正在播放时显示暂停图标**（两根竖条），暂停时显示播放三角。
// 完整 d（实测 1.3.4.258，viewBox 0 0 16 16）：
//
//	playing: M2.7 1a.7.7 0 0 0-.7.7v12.6a.7.7 0 0 0 .7.7h2.6a.7.7 0 0 0 .7-.7V1.7a.7.7 0 0 0-.7-.7zm8 0a.7.7 0 0 0-.7.7v12.6a.7.7 0 0 0 .7.7h2.6a.7.7 0 0 0 .7-.7V1.7a.7.7 0 0 0-.7-.7z
//	paused:  M3 1.713a.7.7 0 0 1 1.05-.607l10.89 6.288a.7.7 0 0 1 0 1.212L4.05 14.894A.7.7 0 0 1 3 14.288z
//
// 只取开头就够（足以区分），也让内嵌 JS 小得多。两条都对不上时 state 为 unknown，
// 由上层退回「位置有没有在推进」的判据（见 Snapshot.PlayingKnown）。
const (
	iconPlayingPrefix = "M2.7 1a.7.7 0 0 0-.7.7v12.6"
	iconPausedPrefix  = "M3 1.713a.7.7 0 0 1 1.05-.607l10.89"
)

// target 是 /json/list 的一个条目。
type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

// Snapshot 是一次页面播放状态快照。
type Snapshot struct {
	OK         bool     `json:"ok"`
	Err        string   `json:"err"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Cover      string   `json:"cover"`
	DurationMs float64  `json:"durationMs"`
	PositionMs float64  `json:"positionMs"`
	State      string   `json:"state"` // playing / paused / unknown
}

// Playing 返回播放态与它是否可信（false = 图标不认识，上层用位置推进兜底）。
func (s *Snapshot) Playing() (playing bool, known bool) {
	switch s.State {
	case "playing":
		return true, true
	case "paused":
		return false, true
	default:
		return false, false
	}
}

// Client 是一条到 xpui 页面的 CDP 会话。
type Client struct {
	conn *websocket.Conn

	writeMu sync.Mutex // 串行化对 conn 的写
	mu      sync.Mutex // 保护 msgID / pending / closed
	msgID   int
	pending map[int]chan []byte
	closed  bool
	done    chan struct{}

	headersMu sync.Mutex
	token     string
	appPlat   string
	appVer    string
}

// Connect 连 9223：GET /json/list → 取 xpui 页面 → Dial → 开 Runtime/Network 两个域。
func Connect() (*Client, error) {
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", debugPort))
	if err != nil {
		return nil, i18n.Errorf("failed to fetch /json/list: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, i18n.Errorf("failed to read /json/list: %w", err)
	}
	var targets []target
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, i18n.Errorf("failed to parse /json/list: %w", err)
	}

	wsURL := ""
	for _, t := range targets {
		if t.Type == "page" && strings.Contains(t.URL, pageURLMarker) && t.WebSocketDebuggerUrl != "" {
			wsURL = t.WebSocketDebuggerUrl
			break
		}
	}
	if wsURL == "" {
		// 页面还没建起来（刚启动）或 URL 变了：拿第一个 page 型目标兜底，让上面的取数自证成功与否。
		for _, t := range targets {
			if t.Type == "page" && t.WebSocketDebuggerUrl != "" {
				wsURL = t.WebSocketDebuggerUrl
				break
			}
		}
	}
	if wsURL == "" {
		return nil, i18n.Errorf("no usable page target on port %d", debugPort)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, i18n.Errorf("WebSocket connection failed: %w", err)
	}
	c := &Client{
		conn:    conn,
		msgID:   0,
		pending: make(map[int]chan []byte),
		done:    make(chan struct{}),
	}
	go c.readLoop()

	// Runtime.enable 让页面把 console/异常事件推过来（我们不看，但保持与其它 player 一致的会话形态）。
	if _, err := c.call("Runtime.enable", nil, controlTimeout); err != nil {
		c.Close()
		return nil, i18n.Errorf("Runtime.enable failed: %w", err)
	}
	// Network.enable 是抓 Bearer 的唯一手段（Network.requestWillBeSent 里带 authorization）。
	// 失败不致命：没有 token 时还有页面缓存那条兜底路径，故只告警。
	if _, err := c.call("Network.enable", nil, controlTimeout); err != nil {
		log.Warn("Network.enable failed; lyric access token capture is unavailable this session: %v", err)
	}
	return c, nil
}

// readLoop 持续读帧：带 id 的是 RPC 回复（整帧派发给等待者），不带 id 的是事件（只关心请求头）。
//
// **它同时是「连接已死」的唯一发现者**：读失败即 markClosed，等待中的 RPC 会因 done 关闭而返回。
// 用 ReadMessage 取原始字节而不是 ReadJSON：回复里的载荷在顶层 `result` 字段，事件在 `params`，
// 整帧原样转交最省事（各自按需解）。
func (c *Client) readLoop() {
	// 出货代码的每个 goroutine 都要挂 Guard（AGENTS §1.2）：漏挂不改变行为（panic 照常杀进程），
	// 只是这次崩溃不进 Sentry。✅ telemetry/goroutineguard_test.go。
	defer telemetry.Guard()
	defer close(c.done)
	for {
		_, frame, err := c.conn.ReadMessage()
		if err != nil {
			c.markClosed()
			return
		}
		var head struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(frame, &head); err != nil {
			continue
		}
		if head.ID != 0 {
			c.deliver(head.ID, frame)
			continue
		}
		if head.Method == "Network.requestWillBeSent" {
			c.noteRequestHeaders(frame)
		}
	}
}

// deliver 把回复交给等待者。等待者可能已超时离开（channel 已满或没人收）——非阻塞发送，丢了就算了。
func (c *Client) deliver(id int, frame []byte) {
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- frame:
	default:
	}
}

// noteRequestHeaders 从页面自己发出的请求里抓一个可复用的 Bearer 与两个请求头。
//
// **这是只读的**：不注入 JS、不改请求、不改响应，只是旁观 CDP 已经在推送的事件。
// 页面对 spclient/api 的调用都带这三个头（实测 1.3.4.258：authorization 长 515 字符、
// app-platform=Win32_x86_64、spotify-app-version=1.3.4.258），照抄它们去取歌词即可。
func (c *Client) noteRequestHeaders(frame []byte) {
	var ev struct {
		Params struct {
			Request struct {
				Headers map[string]string `json:"headers"`
			} `json:"request"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &ev); err != nil {
		return
	}
	var token, platform, version string
	for k, v := range ev.Params.Request.Headers {
		switch {
		case strings.EqualFold(k, "authorization") && strings.HasPrefix(v, "Bearer "):
			token = v
		case strings.EqualFold(k, "app-platform"):
			platform = v
		case strings.EqualFold(k, "spotify-app-version"):
			version = v
		}
	}
	if token == "" && platform == "" && version == "" {
		return
	}
	c.headersMu.Lock()
	defer c.headersMu.Unlock()
	if token != "" && token != c.token {
		c.token = token
	}
	if platform != "" {
		c.appPlat = platform
	}
	if version != "" {
		c.appVer = version
	}
}

// Token 返回当前抓到的 Bearer（没有则空串）。歌词 HTTP 请求直接用它。
func (c *Client) Token() string {
	c.headersMu.Lock()
	defer c.headersMu.Unlock()
	return c.token
}

// AppPlatform / AppVersion 返回抓到的两个请求头（可能为空，调用方按需附带）。
func (c *Client) AppPlatform() string {
	c.headersMu.Lock()
	defer c.headersMu.Unlock()
	return c.appPlat
}

func (c *Client) AppVersion() string {
	c.headersMu.Lock()
	defer c.headersMu.Unlock()
	return c.appVer
}

// call 发一条 CDP 命令并等回复。
func (c *Client) call(method string, params map[string]interface{}, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, i18n.Errorf("session already closed")
	}
	c.msgID++
	id := c.msgID
	ch := make(chan []byte, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.writeMu.Lock()
	err := c.conn.WriteJSON(req)
	c.writeMu.Unlock()
	if err != nil {
		c.markClosed()
		return nil, i18n.Errorf("failed to write CDP request: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case raw := <-ch:
		if raw == nil {
			// markClosed 会 close 所有等待中的 channel，收到 nil 即会话已断。
			return nil, i18n.Errorf("session already closed")
		}
		return raw, nil
	case <-timer.C:
		// 超时即认为这条会话不可信：关掉它，让上层重连（与 sodamusic 的 markClosed 同理 ——
		// 只置标志不关连接会让 socket 与对端调试会话一起滞留）。
		c.markClosed()
		return nil, i18n.Errorf("CDP request %s timed out after %s", method, timeout)
	case <-c.done:
		return nil, i18n.Errorf("session already closed")
	}
}

// evaluateReply 是 Runtime.evaluate 的回复壳。
type evaluateReply struct {
	Result struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

// Evaluate 跑一次 JS 并返回它的字符串结果（表达式须自己 JSON.stringify）。
func (c *Client) Evaluate(expr string) (string, error) {
	raw, err := c.call("Runtime.evaluate", map[string]interface{}{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	}, evalTimeout)
	if err != nil {
		return "", err
	}
	var reply evaluateReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", i18n.Errorf("failed to parse evaluate reply: %w", err)
	}
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		return "", i18n.Errorf("evaluate error: %s", reply.Error)
	}
	if len(reply.Result.ExceptionDetails) > 0 && string(reply.Result.ExceptionDetails) != "null" {
		// 用 %s 而非把内容当格式串（内容里可能含 %）。
		return "", i18n.Errorf("evaluation exception: %s", reply.Result.ExceptionDetails)
	}
	if len(reply.Result.Result.Value) == 0 || string(reply.Result.Result.Value) == "null" {
		return "", nil
	}
	// 表达式返回 JSON.stringify(...)，故 value 是一个 JSON 字符串。
	var out string
	if err := json.Unmarshal(reply.Result.Result.Value, &out); err != nil {
		return "", i18n.Errorf("evaluation returned non-string (type=%s): %w", reply.Result.Result.Type, err)
	}
	return out, nil
}

// Snapshot 取一次页面播放状态。页面里没有曲目时 Title 为空、State 多为 paused —— 由上层判「无歌」。
func (c *Client) Snapshot() (*Snapshot, error) {
	out, err := c.Evaluate(snapshotJS())
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, i18n.Errorf("snapshot returned empty")
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		return nil, i18n.Errorf("failed to parse snapshot: %w", err)
	}
	if !snap.OK {
		return nil, i18n.Errorf("snapshot unsuccessful: %s", snap.Err)
	}
	return &snap, nil
}

// FetchViaPageCache 用页面自己取过的歌词 URL 去命中 HTTP 缓存，返回原始歌词 JSON（拿不到返回空串；
// 缓存答复 404 时返回状态码为 404 的 lyric.StatusError，见 cachedLyricsJS）。
//
// 存在的理由：Bearer 只在 App 发请求时才被抓到（App 空闲时可能几分钟没有一条），而**服务刚起来
// 那一刻**恰恰是最需要歌词的时候。App 每首歌都会自己取一次歌词（换歌时取当前 + 预取下一首），
// 其响应是 public/max-age=3600 的，用同一个 URL 在页面里 fetch 就命中缓存、不需要 token。
// 换一个 URL（哪怕只差 /image/ 段）就是另一次真实请求 → 401。故这里只在 performance 里找
// **完全相同**的 URL。
func (c *Client) FetchViaPageCache(trackID string) (string, error) {
	if trackID == "" {
		return "", i18n.Errorf("empty track id")
	}
	out, err := c.Evaluate(cachedLyricsJS(trackID, lyricsFetchBudget))
	if err != nil {
		return "", err
	}
	return parseCachedLyricsResult(out)
}

// parseCachedLyricsResult 解 cachedLyricsJS 的返回串：缓存答复 404 时返回带状态码的错误，其余原样返回（空串 = 没取到）。
func parseCachedLyricsResult(out string) (string, error) {
	if out == cacheNotFound {
		return "", &lyric.StatusError{Status: http.StatusNotFound, Err: i18n.Errorf("page-cache lyrics fetch returned HTTP %d", http.StatusNotFound)}
	}
	return out, nil
}

// FetchLyrics 在**页面上下文里**发一次带 Bearer 的歌词请求，返回原始歌词 JSON（拿不到返回空串）。
//
// 为什么不让 Go 侧直接发这个请求（lyric.Fetch 保留着那条路，作为第一选择）：真机实测中
// Go 侧带同一个 Bearer 直连 spclient 会稳定拿到 **HTTP 403**，而同一时刻从页面里发同样的
// 请求是 200 —— 差别在请求来源（Origin / Referer / UA / TLS 会话都在页面那一侧，且是 App
// 自己的会话）。这一路是「照 App 的做法再发一次」，不注入、不改页面任何状态。
//
// 返回的 JSON 里 ok=false 时 err 是错误码或浏览器异常文本，经 parseLyricsFetchResult 译成带原因的错误
// （HTTP 状态码、超出 lyricsFetchBudget 的超时这类），供上层限流打日志。
func (c *Client) FetchLyrics(trackID, token string) (string, error) {
	if trackID == "" {
		return "", i18n.Errorf("empty track id")
	}
	if token == "" {
		return "", i18n.Errorf("no access token")
	}
	out, err := c.Evaluate(lyricsFetchJS(trackID, token, c.AppPlatform(), c.AppVersion(), lyricsFetchBudget))
	if err != nil {
		return "", err
	}
	return parseLyricsFetchResult(out)
}

// parseLyricsFetchResult 解 lyricsFetchJS 的返回串：ok=true 时返回 body，否则返回带原因的错误。
func parseLyricsFetchResult(out string) (string, error) {
	if out == "" {
		return "", i18n.Errorf("lyrics fetch returned empty")
	}
	var res struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Body   string `json:"body"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return "", i18n.Errorf("failed to parse lyrics fetch result: %w", err)
	}
	if !res.OK {
		// err 里是 lyricsFetchJS 自己生成的错误码时，在这里译成带译表的文案（注入日志的值不会被 logger 翻译，
		// agent-doc/06 §6.4 ⑤）；其余是浏览器抛出的外部异常文本，原样带出。
		switch {
		case res.Err == jsErrTimeout:
			return "", i18n.Errorf("page-side lyrics fetch timed out after %s", lyricsFetchBudget)
		case res.Err == jsErrUnexpectedBody:
			return "", i18n.Errorf("page-side lyrics fetch returned an unexpected body")
		case strings.HasPrefix(res.Err, jsErrHTTPPrefix):
			// HTTP 状态：与只带 status 的情形一样在下面返回，状态码取结构化的 status 字段，不从文案里解析
		case res.Err != "":
			return "", i18n.Errorf("page-side lyrics fetch failed: %s", res.Err)
		}
		return "", &lyric.StatusError{Status: res.Status, Err: i18n.Errorf("page-side lyrics fetch returned HTTP %d", res.Status)}
	}
	return res.Body, nil
}

// lyricsFetchJS 返回的错误码；parseLyricsFetchResult 据此生成本地化的错误文案。
const (
	jsErrTimeout        = "timeout"
	jsErrUnexpectedBody = "unexpected body"
	jsErrHTTPPrefix     = "http " // 后接状态码
	// cacheNotFound 是 cachedLyricsJS 的「缓存答复 404」：不含 "lyrics"，不会与歌词原文混淆。
	cacheNotFound = jsErrHTTPPrefix + "404"
)

// markClosed 关掉底层连接并置标志（幂等）。调用方不得持有 c.mu。
func (c *Client) markClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.conn.Close()
	// 唤醒所有等待者（call 也能靠 done 返回，这里让 channel 路径也立刻结束）。
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}

// Close 关闭会话（幂等）。
func (c *Client) Close() { c.markClosed() }

// IsClosed 报告会话是否已断。轮询循环靠它决定要不要退出重连。
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// snapshotJS 构造取状态表达式。**内嵌的图标前缀必须保持纯 ASCII**（见包注释与常量注释）。
//
// 标题/歌手/封面只在底栏组件 now-playing-widget-v2 里找（实测 1.3.6.1921）。歌名与每位歌手名各在一个
// dir="auto" 的元素上：可链接时它包在 <a> 里，不可链接时包在 <span> 里或直接是 <span>，故不按 <a> 取。
// 组件里第一个 dir="auto" 是歌名；歌名缺席时第一个会落到歌手行或音质标签（context-item-info-quality）上，
// 两者都排除。封面也限定在组件内：「当前播放」侧栏另有一个 cover-art-image，给的是另一档。
// 歌名为空即上层判「无歌」，故组件结构一变就不出消息，只有上层的 untitledWatch 告警一次。
func snapshotJS() string {
	return "(function(){try{" +
		"var q=function(s){return document.querySelector(s)};" +
		"var w=q('[data-testid=\"now-playing-widget-v2\"]');" +
		"var bl=w?w.querySelector('[data-testid=\"now-playing-widget-v2-byline\"]'):null;" +
		"var ql=w?w.querySelector('[data-testid=\"context-item-info-quality\"]'):null;" +
		"var te=w?w.querySelector('[dir=\"auto\"]'):null;" +
		"var title=(te&&!(bl&&bl.contains(te))&&!(ql&&ql.contains(te)))?(te.textContent||'').trim():'';" +
		"var artists=[];if(bl){var an=bl.querySelectorAll('[dir=\"auto\"]');" +
		"for(var i=0;i<an.length;i++){var t=(an[i].textContent||'').trim();if(t)artists.push(t);}}" +
		"var img=w?w.querySelector('[data-testid=\"cover-art-image\"]'):null;" +
		"var cover=img?(img.currentSrc||img.src||''):'';" +
		"var inp=q('[data-testid=\"playback-progressbar\"] input');" +
		"var durationMs=inp?Number(inp.max):0;" +
		"var positionMs=inp?Number(inp.value):0;" +
		"var bar=q('[data-testid=\"progress-bar\"]');" +
		"if(bar){var st=bar.getAttribute('style')||'';" +
		"var m=st.match(/--progress-bar-transform:\\s*([0-9.]+)%/);" +
		"if(m&&durationMs>0){positionMs=parseFloat(m[1])*durationMs/100;}}" +
		"var state='unknown';var btn=q('[data-testid=\"control-button-playpause\"]');" +
		"if(btn){var p=btn.querySelector('svg path');var d=p?(p.getAttribute('d')||''):'';" +
		"if(d.indexOf(" + strconv.Quote(iconPlayingPrefix) + ")===0){state='playing';}" +
		"else if(d.indexOf(" + strconv.Quote(iconPausedPrefix) + ")===0){state='paused';}}" +
		"return JSON.stringify({ok:true,err:'',title:title,artists:artists,cover:cover," +
		"durationMs:durationMs,positionMs:positionMs,state:state});" +
		"}catch(e){return JSON.stringify({ok:false,err:String((e&&e.message)||e)});}})()"
}

// cachedLyricsJS 构造「用 App 自己取过的 URL 命中 HTTP 缓存」的表达式（见 FetchViaPageCache）。
// budget 管整个表达式：所有候选 URL 共用一个 AbortController，到点后余下的 fetch 立即失败、返回空串。
// 没有一个候选取到歌词、而其中有一个答复 404 时返回 cacheNotFound：不带 token 的请求一旦真的发到服务端一律 401，
// 404 只能是缓存里 App 自己那次请求的答复，即平台没有这首歌的歌词。
func cachedLyricsJS(trackID string, budget time.Duration) string {
	return "(async function(){var ac,tm,nf=false;try{ac=new AbortController();" +
		"tm=setTimeout(function(){ac.abort();}," + strconv.FormatInt(budget.Milliseconds(), 10) + ");" +
		"var id=" + strconv.Quote(trackID) + ";" +
		"var urls=[];var es=performance.getEntriesByType('resource');" +
		"for(var i=0;i<es.length;i++){var n=es[i].name||'';" +
		"if(n.indexOf('/color-lyrics/v2/track/'+id+'/')>=0)urls.push(n);}" +
		"for(var j=urls.length-1;j>=0;j--){" +
		"try{var r=await fetch(urls[j],{signal:ac.signal});" +
		"if(r.ok){var t=await r.text();if(t.indexOf('\"lyrics\"')>=0)return t;}else if(r.status===404){nf=true;}}catch(e){}}" +
		"return nf?" + strconv.Quote(cacheNotFound) + ":'';}catch(e){return '';}finally{clearTimeout(tm);}})()"
}

// lyricsFetchJS 构造「在页面里带 Bearer 取歌词」的表达式（见 FetchLyrics）。
// 返回 JSON 字符串：{ok,status,body,err}。内嵌的 token 是一串 ASCII，用 strconv.Quote 转义。
//
// **请求头必须与 App 自己的请求同形**（真机实测：只带 authorization 时服务端回 **HTTP 400**，
// 带上 accept / app-platform / spotify-app-version 才是 200）。platform 与 version 是从页面
// 自己的请求里旁观到的原值，抓不到时留空（空串就不下发该头）。
//
// budget 到点即中止请求（读 body 阶段同样中止），返回 err=jsErrTimeout。
func lyricsFetchJS(trackID, token, platform, version string, budget time.Duration) string {
	headers := "{authorization:" + strconv.Quote(token) + ",accept:'application/json'"
	if platform != "" {
		headers += ",'app-platform':" + strconv.Quote(platform)
	}
	if version != "" {
		headers += ",'spotify-app-version':" + strconv.Quote(version)
	}
	headers += "}"
	ms := strconv.FormatInt(budget.Milliseconds(), 10)
	return "(async function(){var ac,tm;try{ac=new AbortController();" +
		"tm=setTimeout(function(){ac.abort();}," + ms + ");" +
		"var u='https://spclient.wg.spotify.com/color-lyrics/v2/track/'" +
		"+encodeURIComponent(" + strconv.Quote(trackID) + ")+'?format=json&vocalRemoval=false&market=from_token';" +
		"var r=await fetch(u,{signal:ac.signal,credentials:'omit',cache:'no-store',headers:" + headers + "});" +
		"if(!r.ok)return JSON.stringify({ok:false,status:r.status,body:'',err:'" + jsErrHTTPPrefix + "'+r.status});" +
		"var t=await r.text();" +
		"if(t.indexOf('\"lyrics\"')<0)return JSON.stringify({ok:false,status:r.status,body:'',err:'" + jsErrUnexpectedBody + "'});" +
		"return JSON.stringify({ok:true,status:r.status,body:t,err:''});" +
		"}catch(e){return JSON.stringify({ok:false,status:0,body:''," +
		"err:(ac&&ac.signal.aborted)?'" + jsErrTimeout + "':String((e&&e.message)||e)});}" +
		"finally{clearTimeout(tm);}})()"
}
