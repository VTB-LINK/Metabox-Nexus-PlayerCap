// Package cdp reads Kuwo's tested CEF bridge commands. It never navigates pages,
// calls playback controls, or installs hooks in the renderer. Admission accepts
// any channel suffix of the numeric build 9.6.0.0 (W1, UG1, ...), cross-checking
// the bridge version against the selected page directory's suffix; everything
// else is ErrVersionUnsupported. A connection failure is ErrUnavailable.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"

	"github.com/gorilla/websocket"
)

const DebugPort = 9224
const maxMessage = 1 << 20

// Only the numeric version 9.6.0.0 is verified. The suffix after it is a
// distribution channel label (W1, UG1, ...) and is accepted, but the suffix of
// the selected page directory must equal the suffix reported by the bridge.
var (
	bridgeVersionPattern = regexp.MustCompile(`^MUSIC_9\.6\.0\.0_([A-Za-z0-9]{1,16})$`)
	buildDirPattern      = regexp.MustCompile(`^9\.6\.0\.0_([a-z0-9]{1,16})$`)
)

const netsongDir = "/bin/html/webdata/netsong/"

var (
	ErrSongChanged = errors.New("酷我快照期间歌曲或状态变化")
	ErrUnavailable = errors.New("酷我 CDP 尚未就绪或响应不符合已验证协议")
	// ErrVersionUnsupported means the debugger answered, but its bridge version,
	// page directory or their suffix agreement is not the verified 9.6.0.0 build.
	ErrVersionUnsupported = errors.New("酷我 CDP 桥版本或页面目录未经验证")
)

// Snapshot preserves the native status enum. Callers must not interpret an
// unknown status as playing. RID is canonical MUSIC_<digits>, or empty if idle.
type Snapshot struct {
	RID, Title, Artist, Album string
	Status                    int
}

type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Client serializes RPCs without a background reader. A canceled RPC closes the
// connection so no stale reply can be mistaken for a subsequent snapshot.
type Client struct {
	conn      *websocket.Conn
	gate      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	id        uint64
	build     string
}

// Build returns the channel suffix reported by the bridge, such as "W1".
func (c *Client) Build() string { return c.build }

func Connect(ctx context.Context) (*Client, error) {
	return connect(ctx, "http://127.0.0.1:9224/json")
}

func connect(ctx context.Context, endpoint string) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, i18n.Errorf("kuwo: CDP request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil || len(body) > maxMessage {
		return nil, ErrUnavailable
	}
	var targets []target
	if json.Unmarshal(body, &targets) != nil {
		return nil, ErrUnavailable
	}
	ws, pageBuild, err := selectTarget(targets, endpoint)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, Proxy: nil}
	conn, _, err := dialer.DialContext(ctx, ws, nil)
	if err != nil {
		return nil, i18n.Errorf("kuwo: CDP connection failed: %w", err)
	}
	conn.SetReadLimit(maxMessage)
	c := &Client{conn: conn, gate: make(chan struct{}, 1), closed: make(chan struct{})}
	var version struct {
		Version string `json:"version"`
	}
	if err = c.evaluate(ctx, versionExpression, &version); err != nil {
		c.Close()
		return nil, i18n.Errorf("kuwo: bridge version query failed: %w", err)
	}
	build, ok := bridgeBuild(version.Version)
	if !ok || !strings.EqualFold(build, pageBuild) {
		c.Close()
		return nil, ErrVersionUnsupported
	}
	c.build = build
	return c, nil
}

// bridgeBuild returns the channel suffix of a verified 9.6.0.0 bridge version.
func bridgeBuild(version string) (string, bool) {
	m := bridgeVersionPattern.FindStringSubmatch(version)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// selectTarget deliberately has no first-page fallback. Only a local Kuwo
// netsong document of a 9.6.0.0 build directory and a same-port IPv4-loopback
// debugger are eligible. It returns the lower-case suffix of that directory.
// A netsong page in any other build directory yields ErrVersionUnsupported
// when no eligible page exists; other pages are ignored.
func selectTarget(targets []target, endpoint string) (string, string, error) {
	e, err := url.Parse(endpoint)
	if err != nil || e.Scheme != "http" || e.Hostname() != "127.0.0.1" || e.User != nil {
		return "", "", ErrUnavailable
	}
	var selected, build string
	unsupported := false
	for _, t := range targets {
		u, err := url.Parse(t.URL)
		if err != nil || t.Type != "page" || u.Scheme != "file" || u.Host != "" || u.User != nil {
			continue
		}
		w, err := url.Parse(t.WebSocketDebuggerURL)
		if err != nil || w.Scheme != "ws" || w.Hostname() != "127.0.0.1" || w.Port() != e.Port() || w.User != nil || w.RawQuery != "" || w.Fragment != "" || !strings.HasPrefix(w.Path, "/devtools/page/") {
			continue
		}
		// ASCII-only folding: strings.ToLower maps U+212A KELVIN SIGN to "k".
		p := strings.Map(asciiLower, path.Clean(u.Path))
		n := strings.Count(p, netsongDir)
		if n == 0 || !strings.HasSuffix(p, ".html") {
			continue
		}
		m := buildDirPattern.FindStringSubmatch(path.Base(p[:strings.Index(p, netsongDir)]))
		if n != 1 || m == nil {
			unsupported = true
			continue
		}
		// Prefer the persistent index page when Kuwo exposes several local tabs.
		if selected == "" || strings.HasSuffix(p, "/index.html") {
			selected, build = w.String(), m[1]
		}
	}
	if selected == "" {
		if unsupported {
			return "", "", ErrVersionUnsupported
		}
		return "", "", ErrUnavailable
	}
	return selected, build, nil
}

func asciiLower(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() { close(c.closed); err = c.conn.Close() })
	return err
}

func (c *Client) evaluate(ctx context.Context, expression string, out interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrUnavailable
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.closed:
		return ErrUnavailable
	default:
	}
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	// AfterFunc only closes the socket; Close is idempotent and can interrupt
	// ReadMessage safely. There is no retained worker after a completed RPC.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	c.id++
	// The tested Kuwo libcef JSON parser requires tokenEnd == messageEnd and
	// rejects trailing whitespace. The tested libcef dispatch then dereferences
	// the null parse result (RVA 0xA5DA88). Gorilla WriteJSON appends '\n': use a
	// compact single text frame instead. See framing_test.go and the recon notes.
	request, err := json.Marshal(map[string]interface{}{"id": c.id, "method": "Runtime.evaluate", "params": map[string]interface{}{"expression": expression, "returnByValue": true}})
	if err != nil {
		return err
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, request); err != nil {
		c.Close()
		return i18n.Errorf("kuwo: CDP request failed: %w", err)
	}
	for {
		_, frame, err := c.conn.ReadMessage()
		if err != nil {
			c.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return i18n.Errorf("kuwo: CDP response failed: %w", err)
		}
		var envelope struct {
			ID     uint64          `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result struct {
				ExceptionDetails json.RawMessage `json:"exceptionDetails"`
				WasThrown        bool            `json:"wasThrown"`
				Result           struct {
					Value json.RawMessage `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if json.Unmarshal(frame, &envelope) != nil {
			c.Close()
			return ErrUnavailable
		}
		if envelope.ID != c.id {
			continue
		}
		if len(envelope.Error) > 0 || len(envelope.Result.ExceptionDetails) > 0 || envelope.Result.WasThrown {
			return ErrUnavailable
		}
		var payload string
		if json.Unmarshal(envelope.Result.Result.Value, &payload) != nil || json.Unmarshal([]byte(payload), out) != nil {
			return ErrUnavailable
		}
		return nil
	}
}

// Keep bridge results inside JSON.stringify, matching the tested runtime probe
// and giving the transport a plain JSON-string boundary. The recorded crash
// occurred in incoming protocol JSON parsing, not bridge-result serialization.
const versionExpression = `JSON.stringify({status:window.external.callkwmusic('GetPlayStatus'),song:window.external.callkwmusic('GetNowplaySong'),version:window.external.callkwmusic('GetVer')})`

const snapshotExpression = `JSON.stringify({before:window.external.callkwmusic('GetPlayStatus'),song:window.external.callkwmusic('GetNowplaySong'),after:window.external.callkwmusic('GetPlayStatus')})`

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var raw struct {
		Before string `json:"before"`
		Song   string `json:"song"`
		After  string `json:"after"`
	}
	if err := c.evaluate(ctx, snapshotExpression, &raw); err != nil {
		return Snapshot{}, err
	}
	return parseSnapshot(raw.Before, raw.Song, raw.After)
}

func canonicalRID(s string) (string, bool) {
	s = strings.TrimPrefix(s, "MUSIC_")
	if len(s) == 0 || len(s) > 20 {
		return "", false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return "MUSIC_" + s, true
}

func parseStatus(raw string) (int, string, error) {
	v, err := url.ParseQuery(raw)
	if err != nil || len(v["Status"]) != 1 || len(v["SongId"]) != 1 {
		return 0, "", ErrUnavailable
	}
	n, err := strconv.Atoi(v.Get("Status"))
	if err != nil || n < 0 || n > 255 {
		return 0, "", ErrUnavailable
	}
	rid := v.Get("SongId")
	if rid != "" {
		var ok bool
		rid, ok = canonicalRID(rid)
		if !ok {
			return 0, "", ErrUnavailable
		}
	}
	return n, rid, nil
}

func parseSnapshot(before, song, after string) (Snapshot, error) {
	// Between songs the bridge answers GetPlayStatus with an empty string for a few
	// hundred milliseconds while GetNowplaySong already shows the new song (seen on
	// every switch, 207-498ms). That is a state change in progress, not a protocol
	// violation. A non-empty malformed status is still ErrUnavailable even when the
	// other side is empty, so a real protocol violation is never retried as a switch.
	var status, status2 int
	var rid, rid2 string
	var err error
	if before != "" {
		if status, rid, err = parseStatus(before); err != nil {
			return Snapshot{}, err
		}
	}
	if after != "" {
		if status2, rid2, err = parseStatus(after); err != nil {
			return Snapshot{}, err
		}
	}
	if before == "" || after == "" {
		return Snapshot{}, ErrSongChanged
	}
	if rid != rid2 || status != status2 {
		return Snapshot{}, ErrSongChanged
	}
	if rid == "" {
		return Snapshot{Status: status}, nil
	}
	outer, err := url.ParseQuery(song)
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	if len(outer["song"]) != 1 {
		return Snapshot{}, ErrUnavailable
	}
	fields, err := url.ParseQuery(outer.Get("song"))
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	if len(fields["rid"]) != 1 || len(fields["name"]) != 1 || len(fields["artist"]) != 1 {
		return Snapshot{}, ErrUnavailable
	}
	if len(fields["album"]) > 1 {
		return Snapshot{}, ErrUnavailable
	}
	for _, key := range []string{"name", "artist", "album"} {
		value := fields.Get(key)
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return Snapshot{}, ErrUnavailable
		}
	}
	metaRID, ok := canonicalRID(fields.Get("rid"))
	if !ok {
		return Snapshot{}, ErrUnavailable
	}
	if metaRID != rid {
		return Snapshot{}, ErrSongChanged
	}
	return Snapshot{RID: rid, Status: status, Title: fields.Get("name"), Artist: fields.Get("artist"), Album: fields.Get("album")}, nil
}
