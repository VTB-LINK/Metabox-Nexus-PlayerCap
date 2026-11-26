package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/wsdial"

	"github.com/gorilla/websocket"
)

var log = logger.New("KuGou] [CDP") // 渲染为 [KuGou] [CDP]

const cdpPort = 12233

// DevToolsPage represents one entry from /json
type DevToolsPage struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

// PlayInfo holds the result of KgSuperCall.getPlayInfo (SuperCall 864)
type PlayInfo struct {
	Filename   string `json:"filename"`    // "Artist - Title"
	PlayStatus string `json:"play_status"` // "playing" | "paused" | "stopped"
	Progress   string `json:"progress"`    // ms as string
	Duration   string `json:"duration"`    // ms as string
	Cover      string `json:"cover"`       // cover image URL
	Hash       string `json:"hash"`        // song hash for lyrics API
	AlbumID    string `json:"album_id"`
	MixSongID  string `json:"mix_song_id"`
}

// cdpResult mirrors the CDP Runtime.evaluate response
type cdpResult struct {
	ID     int `json:"id"`
	Result struct {
		Result struct {
			Type  string      `json:"type"`
			Value interface{} `json:"value"`
		} `json:"result"`
		ExceptionDetails interface{} `json:"exceptionDetails,omitempty"`
	} `json:"result"`
}

// Client manages a CDP WebSocket connection to KuGou's desktop-popup page
type Client struct {
	wsUrl  string
	conn   *websocket.Conn
	msgID  int
	mu     sync.Mutex
	closed bool
}

// connectTimeout 是一次 Connect（取 /json、读完响应体、WebSocket 握手）的上限。调用方 waitForCDP 跑在 kugou 的轮询
// goroutine 上：端口接受连接而不完成响应时，没有上限它就永久停在这里，进程检查、90 s 期限与停止都不再执行。
const connectTimeout = 5 * time.Second

// maxJSONBytes 是 /json 响应体的读取上限。实际只有几个页面的条目，远小于此。
const maxJSONBytes = 1 << 20

// Connect fetches /json from port 12233, finds the desktop-popup page, and connects.
//
// ctx 取消（调用方停止、或它的等待期限到了）即中止；此外整个过程不超过 connectTimeout。
func Connect(ctx context.Context) (*Client, error) {
	return connect(ctx, fmt.Sprintf("http://127.0.0.1:%d", cdpPort), connectTimeout)
}

// connect 是 Connect 的实体：base 为 http://主机:端口，timeout 为整个过程的上限。测试经它连本地的测试服务。
func connect(ctx context.Context, base string, timeout time.Duration) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 请求、读响应体与 WebSocket 握手都随 ctx 中止（握手见 wsdial）。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json", nil)
	if err != nil {
		return nil, i18n.Errorf("GET /json: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, i18n.Errorf("GET /json: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, i18n.Errorf("read /json body: %w", err)
	}

	var pages []DevToolsPage
	if err := json.Unmarshal(body, &pages); err != nil {
		return nil, i18n.Errorf("parse /json: %w", err)
	}
	if len(pages) == 0 {
		return nil, i18n.Errorf("no DevTools pages found on port %d", cdpPort)
	}

	// Prefer desktop-popup page (has KgSuperCall), otherwise fall back to first page
	wsUrl := ""
	for _, p := range pages {
		if strings.Contains(p.URL, "desktop-popup") || strings.Contains(p.Title, "desktop") {
			wsUrl = p.WebSocketDebuggerUrl
			log.Info("Connecting to page: %s (%s)", p.Title, p.URL)
			break
		}
	}
	if wsUrl == "" {
		wsUrl = pages[0].WebSocketDebuggerUrl
		log.Info("Using first page: %s (%s)", pages[0].Title, pages[0].URL)
	}

	conn, err := wsdial.Dial(ctx, wsUrl)
	if err != nil {
		return nil, i18n.Errorf("WebSocket dial: %w", err)
	}

	c := &Client{wsUrl: wsUrl, conn: conn, msgID: 1}

	// Enable Runtime domain
	if err := c.sendNoReply(map[string]interface{}{
		"id":     0,
		"method": "Runtime.enable",
	}); err != nil {
		conn.Close()
		return nil, err
	}
	time.Sleep(200 * time.Millisecond)

	return c, nil
}

func (c *Client) sendNoReply(msg interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(msg)
}

// GetPlayInfo calls KgSuperCall.getPlayInfo (SuperCall 864) and returns the result.
// Uses a Promise wrapper so the async callback resolves before returning.
const jsGetPlayInfo = `
new Promise(function(resolve) {
    var jname = "kgtmp_gpi_" + Date.now();
    window[jname] = function(data) {
        window[jname] = null;
        resolve(typeof data === "string" ? data : JSON.stringify(data));
    };
    try {
        external.SuperCall(864, JSON.stringify({callback: jname}));
    } catch(e) {
        resolve("");
    }
    setTimeout(function() { resolve(""); }, 3000);
})
`

// GetPlayInfo retrieves the current play state from KuGou.
// Returns nil, nil when no song is loaded.
func (c *Client) GetPlayInfo() (*PlayInfo, error) {
	raw, err := c.evaluateAsync(jsGetPlayInfo, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}

	var info PlayInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, i18n.Errorf("parse PlayInfo: %w (raw: %s)", err, raw)
	}
	return &info, nil
}

// evaluateAsync executes a JS expression that returns a Promise and waits for it to resolve.
func (c *Client) evaluateAsync(expr string, timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return "", i18n.Errorf("connection closed")
	}

	id := c.msgID
	c.msgID++

	req := map[string]interface{}{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    expr,
			"returnByValue": true,
			"awaitPromise":  true,
		},
	}

	if err := c.conn.WriteJSON(req); err != nil {
		c.closed = true
		return "", i18n.Errorf("write: %w", err)
	}

	c.conn.SetReadDeadline(time.Now().Add(timeout + 500*time.Millisecond))
	for {
		var res cdpResult
		if err := c.conn.ReadJSON(&res); err != nil {
			c.closed = true
			return "", i18n.Errorf("read: %w", err)
		}
		if res.ID != id {
			continue
		}
		if res.Result.ExceptionDetails != nil {
			return "", i18n.Errorf("JS exception: %v", res.Result.ExceptionDetails)
		}
		valStr, ok := res.Result.Result.Value.(string)
		if !ok {
			return "", nil
		}
		return valStr, nil
	}
}

// Close closes the WebSocket connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.conn.Close()
		c.closed = true
	}
}

// IsClosed returns whether the connection is closed.
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
