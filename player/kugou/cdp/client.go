package cdp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/logger"

	"github.com/gorilla/websocket"
)

var log = logger.New("KuGouCDP")

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

// Connect fetches /json from port 12233, finds the desktop-popup page, and connects.
func Connect() (*Client, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json", cdpPort))
	if err != nil {
		return nil, fmt.Errorf("GET /json: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /json body: %w", err)
	}

	var pages []DevToolsPage
	if err := json.Unmarshal(body, &pages); err != nil {
		return nil, fmt.Errorf("parse /json: %w", err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no DevTools pages found on port %d", cdpPort)
	}

	// Prefer desktop-popup page (has KgSuperCall), otherwise fall back to first page
	wsUrl := ""
	for _, p := range pages {
		if strings.Contains(p.URL, "desktop-popup") || strings.Contains(p.Title, "desktop") {
			wsUrl = p.WebSocketDebuggerUrl
			log.Info("连接页面: %s (%s)", p.Title, p.URL)
			break
		}
	}
	if wsUrl == "" {
		wsUrl = pages[0].WebSocketDebuggerUrl
		log.Info("使用首页: %s (%s)", pages[0].Title, pages[0].URL)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial: %w", err)
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
		return nil, fmt.Errorf("parse PlayInfo: %w (raw: %s)", err, raw)
	}
	return &info, nil
}

// evaluateAsync executes a JS expression that returns a Promise and waits for it to resolve.
func (c *Client) evaluateAsync(expr string, timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return "", fmt.Errorf("connection closed")
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
		return "", fmt.Errorf("write: %w", err)
	}

	c.conn.SetReadDeadline(time.Now().Add(timeout + 500*time.Millisecond))
	for {
		var res cdpResult
		if err := c.conn.ReadJSON(&res); err != nil {
			c.closed = true
			return "", fmt.Errorf("read: %w", err)
		}
		if res.ID != id {
			continue
		}
		if res.Result.ExceptionDetails != nil {
			return "", fmt.Errorf("JS exception: %v", res.Result.ExceptionDetails)
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
