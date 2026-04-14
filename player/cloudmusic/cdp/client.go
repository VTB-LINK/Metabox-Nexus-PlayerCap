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

var log = logger.New("CDP")

// DevToolsPage represents a page returned by the /json endpoint
type DevToolsPage struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

// ForceFetchLyricsInRedux sends a one-off Redux dispatch to fetch lyrics.
// Uses {force: true} payload, matching what NetEase's own cover-click triggers.
func (c *Client) ForceFetchLyricsInRedux() error {
	js := `(async () => {
		try {
			const root = document.querySelector('#root');
			if (!root) return 'err: no root';
			let fiber = null;
			for (let k of Object.getOwnPropertyNames(root)) {
				if (k.startsWith('__reactContainer')) {
					fiber = root[k]; break;
				}
			}
			if (!fiber) return 'err: no react';
			let store = null;
			function walk(node, depth) {
				if (!node || depth > 80 || store) return;
				if (node.memoizedProps && node.memoizedProps.store && typeof node.memoizedProps.store.getState === 'function') {
					store = node.memoizedProps.store;
					return;
				}
				walk(node.child, depth + 1);
				if (!store) walk(node.sibling, depth + 1);
			}
			walk(fiber, 0);
			if (store) {
				store.dispatch({type: 'async:lyric/fetchLyric', payload: {force: true}});
				return 'ok';
			}
			return 'err: no store';
		} catch(e) { return 'err:' + e.message; }
	})()`

	res, err := c.EvaluateAsync(js)
	if err != nil {
		return err
	}
	if strings.HasPrefix(res, "err:") {
		return fmt.Errorf(res)
	}
	return nil
}

// Client manages the CDP connection
type Client struct {
	wsUrl  string
	conn   *websocket.Conn
	msgID  int
	mu     sync.Mutex
	closed bool
}

// Result represents the CDP RPC response
type Result struct {
	ID     int `json:"id"`
	Result struct {
		Result struct {
			Type  string      `json:"type"`
			Value interface{} `json:"value"`
		} `json:"result"`
		ExceptionDetails interface{} `json:"exceptionDetails,omitempty"`
	} `json:"result"`
}

type ExtractionData struct {
	PlayingState      int              `json:"playingState"`
	CurPlaying        *CurPlayingObj   `json:"curPlaying"`
	CurrentProgress   float32          `json:"currentProgress"`
	CurrentLyricIndex int              `json:"currentLyricIndex"`
	Lyrics            []ExtractedLyric `json:"lyrics"`
	DomTimeSec        int              `json:"domTimeSec"`
	DomSongName       string           `json:"domSongName"`
	DomArtist         string           `json:"domArtist"`
	DomCoverUrl       string           `json:"domCoverUrl"`
}

type CurPlayingObj struct {
	ID    string `json:"id"`
	Track struct {
		Name  string `json:"name"`
		Album struct {
			PicUrl string `json:"picUrl"`
		} `json:"album"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Duration int `json:"duration"` // ms
	} `json:"track"`
}

type ExtractedLyric struct {
	Index int     `json:"index"`
	Time  float32 `json:"time"`
	Text  string  `json:"text"`
}

const jsPayload = `(() => {
	try {
		const root = document.querySelector('#root');
		if (!root) return 'null: no root';

		let fiber = null;
		const ownKeys = Object.getOwnPropertyNames(root);
		for (let i = 0; i < ownKeys.length; i++) {
			if (ownKeys[i].startsWith('__reactContainer')) {
				fiber = root[ownKeys[i]];
				break;
			}
		}
		if (!fiber) return 'null: no react container';
		
		let result = {
			playingState: 0,
			curPlaying: null,
			currentProgress: 0,
			currentLyricIndex: -1,
			lyrics: [],
			domTimeSec: -1,
			domSongName: '',
			domArtist: '',
			domCoverUrl: ''
		};
		
		// Read time from DOM
		try {
			let th = document.querySelector('.curtime-thumb');
			if (th) {
				let parts = (th.innerText || th.textContent).split('/');
				if (parts.length > 0) {
					let timeParts = parts[0].trim().split(':');
					if (timeParts.length === 2) {
						result.domTimeSec = parseInt(timeParts[0]) * 60 + parseInt(timeParts[1]);
					}
				}
			}
		} catch(e) {}

		// Read song name, artist, cover from bottom bar DOM
		try {
			let bar = document.querySelector('.default-bar-wrapper');
			if (bar) {
				let titleEl = bar.querySelector('.main-title');
				if (titleEl) {
					let t = titleEl.querySelector('.title');
					result.domSongName = (t || titleEl).textContent.trim();
				}
				let authorEl = bar.querySelector('.author');
				if (authorEl) result.domArtist = authorEl.textContent.trim();
				let coverImg = bar.querySelector('.miniVinylWrapper img');
				if (coverImg && coverImg.src) {
					let u = coverImg.src;
					let idx = u.indexOf('thumbnail=');
					if (idx > -1) {
						let end = u.indexOf('&', idx);
						u = u.substring(0, idx) + 'thumbnail=300y300' + (end > -1 ? u.substring(end) : '');
					}
					result.domCoverUrl = u;
				}
			}
		} catch(e) {}
		
		// Walk fiber tree with DFS (ORIGINAL STABLE pattern - stop at first store)
		let storeFound = false;
		function walk(node, depth) {
			if (!node || depth > 80 || storeFound) return;
			if (node.memoizedProps && node.memoizedProps.store && typeof node.memoizedProps.store.getState === 'function') {
				try {
					const st = node.memoizedProps.store.getState();
					if (st['playing']) {
						const playing = st['playing'];
						
						result.playingState = playing.playingState;
						result.curPlaying = playing.curPlaying;
						result.currentLyricIndex = playing.playingLyricLineNumber !== undefined ? playing.playingLyricLineNumber : -1;
						
						// async:lyric is optional (only exists when lyrics page is open)
						const lyricInfo = st['async:lyric'];
						if (lyricInfo && lyricInfo.lyricLines && Array.isArray(lyricInfo.lyricLines)) {
							lyricInfo.lyricLines.forEach((l, idx) => {
								result.lyrics.push({
									index: idx,
									time: l.time,
									text: l.lyric
								});
							});
						}
						storeFound = true;
						return;
					}
				} catch(e) {}
			}
			walk(node.child, depth + 1);
			if (!storeFound) walk(node.sibling, depth + 1);
		}
		walk(fiber, 0);

		if (!storeFound) {
			return 'null: store not found (depth 80)';
		}

		if (result.currentLyricIndex >= 0 && result.currentLyricIndex < result.lyrics.length) {
			result.currentProgress = result.lyrics[result.currentLyricIndex].time;
		}

		return JSON.stringify(result);
	} catch(e) {
		return 'Exception: ' + e.message;
	}
})()`

// Connect fetches the debugger URL and connects WebSocket
func Connect() (*Client, error) {
	resp, err := http.Get("http://127.0.0.1:9222/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pages []DevToolsPage
	if err := json.Unmarshal(body, &pages); err != nil {
		return nil, err
	}

	log.Info("Available pages:")
	for _, p := range pages {
		log.Detail("- Type: %s, URL: %s", p.Type, p.URL)
	}

	var wsUrl string
	var matchUrl string
	// Try to find the core UI. "orpheus://orpheus/pub/core.html" or similar.
	// For now, we will prefer pages that are likely the main shell.
	for _, p := range pages {
		// NetEase main shell is typically the one with the core/app structure.
		// Avoid connecting to iframes when possible.
		if strings.HasPrefix(p.URL, "orpheus://") && p.Type == "page" && !strings.Contains(p.URL, "notrack=true") {
			wsUrl = p.WebSocketDebuggerUrl
			matchUrl = p.URL
			// Let's not immediately break, maybe we can find one with 'core'
			if strings.Contains(p.URL, "core") || strings.Contains(p.URL, "main") {
				break
			}
		}
	}

	if wsUrl == "" {
		for _, p := range pages {
			if strings.HasPrefix(p.URL, "orpheus://") {
				wsUrl = p.WebSocketDebuggerUrl
				matchUrl = p.URL
				break
			}
		}
	}
	if wsUrl == "" {
		if len(pages) > 0 {
			wsUrl = pages[0].WebSocketDebuggerUrl
			matchUrl = pages[0].URL
		} else {
			return nil, fmt.Errorf("no DevTools page found")
		}
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsUrl, nil)
	if err != nil {
		return nil, err
	}

	log.Info("Target URL: %s", matchUrl)

	return &Client{
		wsUrl: wsUrl,
		conn:  conn,
		msgID: 1,
	}, nil
}

// Evaluate runs JS and parses our custom JSON payload
func (c *Client) Extract() (*ExtractionData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.msgID
	c.msgID++

	req := map[string]interface{}{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    jsPayload,
			"returnByValue": true,
		},
	}

	if err := c.conn.WriteJSON(req); err != nil {
		c.closed = true
		return nil, err
	}

	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var res Result
		if err := c.conn.ReadJSON(&res); err != nil {
			c.closed = true
			return nil, err
		}

		if res.ID == id {
			if res.Result.ExceptionDetails != nil {
				return nil, fmt.Errorf("JS Exception: %v", res.Result.ExceptionDetails)
			}

			valStr, ok := res.Result.Result.Value.(string)
			if !ok || valStr == "" || valStr == "null" {
				return nil, fmt.Errorf("extraction returned null or invalid. Got: %v", res.Result.Result.Value)
			}

			var data ExtractionData
			if err := json.Unmarshal([]byte(valStr), &data); err != nil {
				return nil, fmt.Errorf("json parse error: %v, raw string: %s", err, valStr)
			}
			return &data, nil
		}
	}
}

func (c *Client) Close() {
	if !c.closed {
		c.conn.Close()
		c.closed = true
	}
}

func (c *Client) IsClosed() bool {
	return c.closed
}

// EvaluateAsync runs async JS (e.g. fetch) and returns the string result
func (c *Client) EvaluateAsync(expression string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.msgID
	c.msgID++

	req := map[string]interface{}{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]interface{}{
			"expression":    expression,
			"returnByValue": true,
			"awaitPromise":  true,
		},
	}

	if err := c.conn.WriteJSON(req); err != nil {
		c.closed = true
		return "", err
	}

	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var res Result
		if err := c.conn.ReadJSON(&res); err != nil {
			c.closed = true
			return "", err
		}

		if res.ID == id {
			if res.Result.ExceptionDetails != nil {
				return "", fmt.Errorf("JS Exception: %v", res.Result.ExceptionDetails)
			}

			valStr, ok := res.Result.Result.Value.(string)
			if !ok {
				return "", fmt.Errorf("unexpected return type: %v", res.Result.Result.Value)
			}
			return valStr, nil
		}
	}
}

// SearchSongViaCDP uses the app's own fetch to search for a song and return ID
func (c *Client) SearchSongViaCDP(songName string, artist string) (string, error) {
	query := songName
	if artist != "" {
		query = songName + " " + artist
	}
	// Escape for JS string
	query = strings.ReplaceAll(query, `\`, `\\`)
	query = strings.ReplaceAll(query, `'`, `\'`)

	js := fmt.Sprintf(`(async () => {
		try {
			const r = await fetch('https://music.163.com/api/search/get?s=' + encodeURIComponent('%s') + '&type=1&limit=5');
			const d = await r.json();
			if (d.result && d.result.songs && d.result.songs.length > 0) {
				const target = '%s';
				for (let s of d.result.songs) {
					if (s.name === target) return String(s.id);
				}
				return String(d.result.songs[0].id);
			}
			return '';
		} catch(e) { return 'err:' + e.message; }
	})()`, query, strings.ReplaceAll(songName, `'`, `\'`))

	result, err := c.EvaluateAsync(js)
	if err != nil {
		return "", err
	}
	if result == "" || strings.HasPrefix(result, "err:") {
		return "", fmt.Errorf("search failed: %s", result)
	}
	return result, nil
}

// FetchLyricsViaCDP uses the app's own fetch to get lyrics by song ID
func (c *Client) FetchLyricsViaCDP(songID string) (string, error) {
	js := fmt.Sprintf(`(async () => {
		try {
			const r = await fetch('https://music.163.com/api/song/lyric?id=%s&lv=1&tv=1');
			const d = await r.json();
			if (d.pureMusic) return '[PURE_MUSIC]';
			if (d.nolyric) return '[NO_LYRIC]';
			if (d.lrc && d.lrc.lyric) return d.lrc.lyric;
			return '';
		} catch(e) { return 'err:' + e.message; }
	})()`, songID)

	result, err := c.EvaluateAsync(js)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(result, "err:") {
		return "", fmt.Errorf("lyrics fetch failed: %s", result)
	}
	if result == "" {
		return "", fmt.Errorf("lyrics fetch failed: empty result")
	}
	return result, nil
}

// FetchCoverViaCDP uses the app's own fetch to get album cover URL by song ID
func (c *Client) FetchCoverViaCDP(songID string) (string, error) {
	js := fmt.Sprintf(`(async () => {
		try {
			const r = await fetch('https://music.163.com/api/song/detail/?ids=[%s]&id=%s');
			const d = await r.json();
			if (d.songs && d.songs.length > 0 && d.songs[0].album && d.songs[0].album.picUrl) {
				let url = d.songs[0].album.picUrl;
				if (url && !url.includes('param=')) {
					url += (url.includes('?') ? '&' : '?') + 'param=800y800';
				}
				return url;
			}
			return '';
		} catch(e) { return 'err:' + e.message; }
	})()`, songID, songID)

	result, err := c.EvaluateAsync(js)
	if err != nil {
		return "", err
	}
	if result == "" || strings.HasPrefix(result, "err:") {
		return "", fmt.Errorf("cover fetch failed: %s", result)
	}
	return result, nil
}
