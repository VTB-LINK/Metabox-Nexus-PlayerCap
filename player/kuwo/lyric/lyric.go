// Package lyric 抓取并解析酷我音乐的歌词。
//
// 协议（实测于酷我 9.6.0.0，2026-10 在主人本机反出）：
//
//	GET http://newlyric.kuwo.cn/newlyric.lrc?<base64(xor(明文))>
//	明文 = user=<uid>,<版本>,<exe>,KwMusic&requester=localhost&req=1
//	       &songname=<GBK 编码>&artist=<GBK 编码>&rid=MUSIC_<数字 id>&lrcx=1&olrc=1
//	xor  = 明文字节逐字节异或循环密钥 "yeelion"（酷我内部代号，7 字节，周期 7）
//
// 响应 = 文本头 + CRLF CRLF + zlib( base64( xor(LRC 原文, "yeelion") ) )
// 文本头是 key=value 逐行；LRC 原文是 GB18030 编码（GBK 的超集：韩文、生僻字、emoji 是四字节
// 序列，按 GBK 解会变成乱码），行时间 [mm:ss.xxx]。
//
// 行内 <a,b> 的系数来自 [kuwo:八进制数]：十位 C1、个位 C2。
// 行内开始=(a+b)/(2*C1)，持续=(a-b)/(2*C2)，单位毫秒。
// 这是客户端 KwModLyric.dll 的整数运算，不是固定 a/6；详见 words.go。
//
// 没有歌词的歌（`tp=none`、零载荷）返回 ErrNoLyrics。
//
// 逆出这条链的证据链（供复查，见 doc/design/kuwo-integration-recon.md）：
//   - KwLib.dll 导出的 Entrypt::Encrypt/Decrypt 是**另一套**（按长度整体移位 + 循环异或，
//     密钥 "KoOtOiTvINGwd" / "_Y8g2E6n0E1i7L5t2IoOoNk"），歌词通道不用它；
//   - 歌词通道的密钥来自两处实测：客户端自己发出的请求（密文）与其内存里的请求明文逐字节
//     异或，以及同一首歌的响应密文与其在客户端内存里的 LRC 明文异或——两边都得到
//     "yeelion" 的循环，周期 7，且请求/响应共用同一把。
package lyric

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	// Endpoint 是酷我的歌词服务地址。
	Endpoint = "http://newlyric.kuwo.cn/newlyric.lrc?"

	// xorKey 是请求/响应两侧共用的循环异或密钥。
	xorKey = "yeelion"

	// clientName / exeName 是请求明文里自称的客户端标识；实测服务端不校验。
	clientName = "KwMusic"
	exeName    = "kwmusic_web_1.exe"

	// DefaultVersion 是请求里缺省自称的版本串。
	DefaultVersion = "MUSIC_9.6.0.0_W1"
)

// ErrNoLyrics 表示这首歌没有歌词（响应 `tp=none` 且载荷为空）。实测四首歌都是这个形态。
// 调用方应把它当"没有"而不是故障：重试没有意义，换歌才有。
var ErrNoLyrics = errors.New("kuwo lyric: no lyrics for this song")

// ErrTooLarge rejects oversized network/decompression input before parsing.
var ErrTooLarge = errors.New("kuwo lyric: response exceeds size limit")

// ErrInvalidRID rejects non-numeric identifiers before building a query.
var ErrInvalidRID = errors.New("kuwo lyric: invalid song identifier")

// Request 描述一次歌词请求。
type Request struct {
	// Version 是酷我版本串，默认 DefaultVersion。
	Version string
	// UID 是登录用户 id；未登录用 "0"（实测 0 也能取到歌词）。
	UID string
	// Song / Artist 是歌名与歌手，按 GBK 编码进请求。
	Song   string
	Artist string
	// RID 是数字 id（带不带 "MUSIC_" 前缀都行），来自 CDP 桥的 GetNowplaySong。
	RID string
}

// PlainBytes 返回明文参数串的**字节**。
//
// 歌名/歌手是 GBK 编码，所以这里刻意返回 []byte 而不是 string：以 Go 的 UTF-8 字符串
// 传下去，「按字节异或」就会错位（本包最初就踩过，夹具当场抓红）。
func (r Request) PlainBytes() ([]byte, error) {
	ver := r.Version
	if ver == "" {
		ver = DefaultVersion
	}
	uid := r.UID
	if uid == "" {
		uid = "0"
	}
	rid := strings.TrimPrefix(r.RID, "MUSIC_")
	if len(rid) == 0 || len(rid) > 20 {
		return nil, ErrInvalidRID
	}
	for _, ch := range rid {
		if ch < '0' || ch > '9' {
			return nil, ErrInvalidRID
		}
	}
	rid = "MUSIC_" + rid
	// RID is sufficient for this endpoint. Optional metadata outside GBK must
	// not turn a valid RID into a permanent lyric failure or send partial text.
	song, songErr := gbkBytes(r.Song)
	artist, artistErr := gbkBytes(r.Artist)
	// 注意 req=1 后面那个空格是客户端原样带的，照抄。
	head := fmt.Sprintf("user=%s,%s,%s,%s&requester=localhost&req=1 ",
		uid, ver, exeName, clientName)
	out := make([]byte, 0, len(head)+len(song)+len(artist)+64)
	out = append(out, head...)
	if songErr == nil {
		out = append(out, "&songname="...)
		out = append(out, song...)
	}
	if artistErr == nil {
		out = append(out, "&artist="...)
		out = append(out, artist...)
	}
	out = append(out, fmt.Sprintf("&rid=%s&lrcx=1&olrc=1", rid)...)
	return out, nil
}

// Plain 返回明文参数串的 UTF-8 视图，只用于日志与排查；参与编码的必须是 PlainBytes。
func (r Request) Plain() (string, error) {
	b, err := r.PlainBytes()
	if err != nil {
		return "", err
	}
	u, err := gb18030Decode(b)
	if err != nil {
		return "", err
	}
	return string(u), nil
}

// URL 返回完整请求地址。
func (r Request) URL() (string, error) {
	plain, err := r.PlainBytes()
	if err != nil {
		return "", err
	}
	return Endpoint + EncodeQuery(plain), nil
}

// EncodeQuery 把明文字节编码成 URL 查询串：XOR "yeelion" 后 base64。
func EncodeQuery(plain []byte) string {
	return base64.StdEncoding.EncodeToString(xorCycle(plain))
}

// DecodeQuery 是 EncodeQuery 的逆运算，返回明文字节（GBK）；排查用。
func DecodeQuery(q string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(q)
	if err != nil {
		return nil, err
	}
	return xorCycle(raw), nil
}

// Response 是解码后的歌词响应。
type Response struct {
	// Header 是响应的文本头（tp / path / lrc_length / lrcx / show ...）。
	Header map[string]string
	// LRC 是 GB18030 解码后的 LRC 原文。
	LRC string
}

// Client 是歌词抓取器。
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// NewClient 返回带默认超时的抓取器。
func NewClient() *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		BaseURL: Endpoint,
	}
}

// Fetch 抓取并解码一首歌的歌词。
func (c *Client) Fetch(ctx context.Context, req Request) (Response, error) {
	var out Response
	plain, err := req.PlainBytes()
	if err != nil {
		return out, err
	}
	base := c.BaseURL
	if base == "" {
		base = Endpoint
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+EncodeQuery(plain), nil)
	if err != nil {
		return out, err
	}
	// 客户端自称的 UA（libcef 里那一串）；实测不影响结果，保留以免服务端改策略。
	hreq.Header.Set("User-Agent", "Mozilla/4.0 (compatible; MSIE 7.0; Windows NT 6.2; WOW64)")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("kuwo lyric: unexpected status %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxLRCBytes)
	if err != nil {
		return out, err
	}
	return DecodeResponse(body)
}

// DecodeResponse 解码响应体：文本头 + CRLFCRLF + zlib(base64(xor(LRC)))。
//
// 没有歌词的歌返回 `tp=none` 且**载荷为空**（实测四首歌都是 68 字节头、0 字节正文），
// 这种情况返回 ErrNoLyrics，不要当成解码失败（否则日志里全是 zlib 错误，掩盖真故障）。
func DecodeResponse(body []byte) (Response, error) {
	var out Response
	if len(body) > maxLRCBytes {
		return out, ErrTooLarge
	}
	sep := bytes.Index(body, []byte("\r\n\r\n"))
	if sep < 0 {
		return out, fmt.Errorf("kuwo lyric: missing header separator (body %d bytes)", len(body))
	}
	out.Header = parseHeader(string(body[:sep]))
	payload := body[sep+4:]
	if len(payload) == 0 || strings.EqualFold(out.Header["tp"], "none") {
		return out, fmt.Errorf("%w（tp=%q payload=%d bytes）", ErrNoLyrics, out.Header["tp"], len(payload))
	}

	zr, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return out, fmt.Errorf("kuwo lyric: zlib open: %w", err)
	}
	defer zr.Close()
	inner, err := readLimited(zr, maxLRCBytes*2)
	if err != nil {
		return out, fmt.Errorf("kuwo lyric: zlib read: %w", err)
	}
	encoded := strings.TrimSpace(string(inner))
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return out, fmt.Errorf("kuwo lyric: base64: %w", err)
	}
	lrc, err := gb18030Decode(xorCycle(raw))
	if err != nil {
		return out, err
	}
	if len(lrc) > maxLRCBytes {
		return out, ErrTooLarge
	}
	out.LRC = string(lrc)
	return out, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

// parseHeader 解析响应文本头。
func parseHeader(s string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(s, "\r\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// Line 是一行歌词。
type Line struct {
	// SubText is the translation paired with this original in known ml:1.0 lrcx.
	SubText string
	// RawTimeMs 是源文件行标，尚未并入 LRC 的 [offset:]。
	RawTimeMs int
	// TimeMs 是行时间（毫秒），已并入 LRC 的 [offset:]。
	// 它不包含用户在本程序配置的播放器延迟。
	TimeMs int
	// Text 是去掉逐字 <a,b> 标记后的正文。
	Text string
	// Words 保留源文本片段；nil 表示无逐字或该行逐字数据不可靠。
	Words []Word
}

// ParseLRC 解析酷我格式的 LRC：[mm:ss.xxx] 行时间；[offset:±ms] 累加到每一行；
// [kuwo:nnn]/[ver:]/[ti:] 等标签行跳过；行内逐字 <a,b> 标记整段去掉。
func ParseLRC(lrc string) []Line {
	return parseLines(lrc)
}

// cutTimestamp 取出行首的 [mm:ss.xxx]，返回毫秒与余下正文。
func cutTimestamp(s string) (int, string, bool) {
	if !strings.HasPrefix(s, "[") {
		return 0, s, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return 0, s, false
	}
	inner := s[1:end]
	colon := strings.IndexByte(inner, ':')
	if colon < 0 || !timestampDigits(inner[:colon], 4) {
		return 0, s, false
	}
	mm, err := strconv.Atoi(inner[:colon])
	if err != nil || mm < 0 || mm > 1440 {
		return 0, s, false
	}
	rest := inner[colon+1:]
	secPart, fracPart := rest, ""
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		secPart, fracPart = rest[:dot], rest[dot+1:]
		if !timestampDigits(fracPart, 9) {
			return 0, s, false
		}
	}
	if !timestampDigits(secPart, 2) {
		return 0, s, false
	}
	ss, err := strconv.Atoi(secPart)
	if err != nil || ss < 0 || ss >= 60 {
		return 0, s, false
	}
	ms := 0
	if fracPart != "" {
		for len(fracPart) < 3 {
			fracPart += "0"
		}
		n, err := strconv.Atoi(fracPart[:3])
		if err != nil || n < 0 {
			return 0, s, false
		}
		ms = n
	}
	ts := mm*60000 + ss*1000 + ms
	if ts > maxTimeMs {
		return 0, s, false
	}
	return ts, s[end+1:], true
}

func timestampDigits(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// stripWordMarks 去掉逐字标记 <a,b>（它是时间，不是坐标，见包注释与
// doc/design/kuwo-lyric-protocol.md §1.3）。
func stripWordMarks(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '<' {
			// Match only <signed-decimal,signed-decimal>. Bounded lookahead
			// keeps repeated malformed '<' input linear; literal text survives.
			end := i + 25
			if end > len(s) {
				end = len(s)
			}
			if n := strings.IndexByte(s[i:end], '>'); n > 0 {
				left, right, ok := strings.Cut(s[i+1:i+n], ",")
				if ok && signedDecimal(left) && signedDecimal(right) {
					i += n + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func signedDecimal(s string) bool {
	// Match parseWords, including leading zeros and signed int32 bounds.
	_, err := strconv.ParseInt(s, 10, 32)
	return err == nil
}

// tagValue 取形如 [name:value] 的标签值。
func tagValue(s, name string) (string, bool) {
	prefix := "[" + name + ":"
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	i := strings.IndexByte(rest, ']')
	if i < 0 {
		return "", false
	}
	return rest[:i], true
}

// xorCycle 按循环密钥异或（加解密同一运算）。
func xorCycle(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ xorKey[i%len(xorKey)]
	}
	return out
}

// gbkBytes 把 UTF-8 字符串编码成 GBK 字节。
func gbkBytes(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("kuwo lyric: gbk encode: %w", err)
	}
	return b, nil
}

// gb18030Decode 把 GB18030 字节解码成 UTF-8。响应里的歌词按 GB18030 编码，GBK 解码器遇到
// 四字节序列（如韩文音节）会产出替换字符；GB18030 是 GBK 的超集，旧歌词解码结果不变。
func gb18030Decode(b []byte) ([]byte, error) {
	out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
	if err != nil {
		return nil, fmt.Errorf("kuwo lyric: gb18030 decode: %w", err)
	}
	return out, nil
}
