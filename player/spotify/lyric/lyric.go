// Package lyric 取并解析 Spotify 自家的歌词（spclient 的 color-lyrics/v2）。
//
// 为什么用 Spotify 自己的歌词而不是第三方源：它就是 App 里显示的那一份，与曲目一一对应、
// 行级时间戳由平台校准过（provider 实测为 MusixMatch）。实测（2026-10）：
//   - 带 Bearer 时有歌词的曲目都能取：`/color-lyrics/v2/track/<base62 id>` 200，**连 /image/ 段都不需要**；
//     平台没有歌词的曲目回 404（2026-10-13 真机日志，见 IsNotFound）；
//   - 不带 Bearer 一律 401，与 cookie 无关（HTTP-only cookie 在这一路上不被接受）；
//   - syncType 实测全部是 LINE_SYNCED，lines[].syllables 恒为空数组。
//
// **逐字（syllables）有意不使用**：真机上没抓到任何一份 SYLLABLE_SYNCED 的样本，其
// startTimeMs 是绝对时间还是相对行首**未经验证**。按 AGENTS §0.1「绝不盲干」，
// 这里不猜它的语义 —— 拿不到样本就不发 text_detailed，只发行级歌词。
// 哪天抓到样本，在 Parse 里加一节并补上用例即可。
package lyric

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// endpoint 是歌词端点。format=json 与 market=from_token 照抄 App 自己的请求（实测原样可用）；
// vocalRemoval=false 是 App 每次都带的开关，保持同形以免将来服务端按它分流。
const endpoint = "https://spclient.wg.spotify.com/color-lyrics/v2/track/%s?format=json&vocalRemoval=false&market=from_token"

// maxBodyBytes 是响应体上限。真机歌词 JSON 在 2~8KB 量级，256KB 足够宽容又能挡住异常响应。
const maxBodyBytes = 256 * 1024

// maxStartMs 是行首时间戳的上界（不含）：100000 秒约 27.8 小时，与 wesing 的 IsPlausiblePlayTime
// 同值。远超任何曲目的时长，且这个量级的秒数转 float32 后精度仍在 10ms 以内；超出的只会是坏数据。
const maxStartMs = 100000 * 1000

// Line 是一行歌词：时间戳（秒）与文本。
type Line struct {
	Index int
	Time  float32
	Text  string
}

// rawLine 是 color-lyrics 的一行。**时间戳是字符串**（"18300"），不是数字。
type rawLine struct {
	StartTimeMs         string `json:"startTimeMs"`
	Words               string `json:"words"`
	EndTimeMs           string `json:"endTimeMs"`
	TransliteratedWords string `json:"transliteratedWords"`
	Syllables           []struct {
		StartTimeMs string `json:"startTimeMs"`
		EndTimeMs   string `json:"endTimeMs"`
		Words       string `json:"words"`
	} `json:"syllables"`
}

// Response 是 color-lyrics 的响应。
type Response struct {
	Lyrics struct {
		SyncType            string    `json:"syncType"`
		Lines               []rawLine `json:"lines"`
		Provider            string    `json:"provider"`
		ProviderDisplayName string    `json:"providerDisplayName"`
	} `json:"lyrics"`
}

// StatusError 是歌词请求得到非 200 答复时的错误：Status 是 HTTP 状态码，Err 是给日志的本地化文案。
// Go 直取（Fetch）与页面内取词（cdp 包）都返回它，上层以 IsNotFound 区分「平台没有这首歌的歌词」与其余失败。
type StatusError struct {
	Status int
	Err    error
}

func (e *StatusError) Error() string { return e.Err.Error() }

// IsNotFound 报告 err 是不是歌词请求得到的 HTTP 404。平台对没有歌词的曲目回 404（2026-10-13 真机日志，页面内取词）；
// 401 / 403 / 400 / 5xx 与网络错误都不是。
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// TrackIDFromURI 从 "spotify:track:<id>" 里取出 <id>；不是曲目 URI 时返回空串。
func TrackIDFromURI(uri string) string {
	const prefix = "spotify:track:"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	id := strings.TrimPrefix(uri, prefix)
	if id == "" || strings.ContainsAny(id, ": /") {
		return ""
	}
	return id
}

// Fetch 取一份歌词。token 是页面上抓到的 Bearer（形如 "Bearer BQ..."）；
// platform / version 是同源抓到的两个请求头，可为空。
func Fetch(trackID, token, platform, version string, timeout time.Duration) (*Response, error) {
	if trackID == "" {
		return nil, i18n.Errorf("empty track id")
	}
	if token == "" {
		return nil, i18n.Errorf("no access token available")
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(endpoint, trackID), nil)
	if err != nil {
		return nil, i18n.Errorf("failed to build lyrics request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	if platform != "" {
		req.Header.Set("app-platform", platform)
	}
	if version != "" {
		req.Header.Set("spotify-app-version", version)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, i18n.Errorf("lyrics request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, Err: i18n.Errorf("lyrics endpoint returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, i18n.Errorf("failed to read lyrics body: %w", err)
	}
	return ParseBody(body)
}

// ParseBody 解析一份歌词 JSON（Go 侧取回来的，或页面缓存里拿到的原文）。
func ParseBody(body []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, i18n.Errorf("failed to parse lyrics JSON: %w", err)
	}
	return &resp, nil
}

// Parse 把响应解析成行级歌词。
//
//   - UNSYNCED（没有时间轴）返回 0 行：它的 startTimeMs 每行都是 0，发出去只会让下游把整首
//     歌词挤在第一秒。没有时间轴的歌词对本服务没有用，按「没有歌词」处理。
//   - 空行（words 为空或纯空白）跳过：Spotify 用空行表示间奏，它没有可显示的文本。
//   - 时间戳解不出来或不合理（空串/非数字/负数/NaN/无穷大/不小于 maxStartMs）的行跳过，但
//     **不影响后续行的 Index 递增**：Index 是「发出去的第几行」，必须与 LyricLine/lyric_update
//     用的下标一致。ParseFloat 接受 "NaN"、"Inf"，"1e50" 转 float32 又会溢出成 +Inf，二者进了
//     载荷 json.Marshal 都会报错，所以用接受式判定（NaN 的任何比较都为 false）。
func Parse(resp *Response) []Line {
	if resp == nil || len(resp.Lyrics.Lines) == 0 {
		return nil
	}
	if strings.EqualFold(resp.Lyrics.SyncType, "UNSYNCED") {
		return nil
	}
	lines := make([]Line, 0, len(resp.Lyrics.Lines))
	for _, raw := range resp.Lyrics.Lines {
		text := strings.TrimSpace(raw.Words)
		if text == "" {
			continue
		}
		ms, err := strconv.ParseFloat(strings.TrimSpace(raw.StartTimeMs), 64)
		if err != nil || !(ms >= 0 && ms < maxStartMs) {
			continue
		}
		lines = append(lines, Line{
			Index: len(lines),
			Time:  float32(ms / 1000.0),
			Text:  text,
		})
	}
	return lines
}

// Provider 返回歌词来源名（供日志用），没有则空串。
func Provider(resp *Response) string {
	if resp == nil {
		return ""
	}
	if resp.Lyrics.ProviderDisplayName != "" {
		return resp.Lyrics.ProviderDisplayName
	}
	return resp.Lyrics.Provider
}

// SyncType 返回时间轴类型（LINE_SYNCED / SYLLABLE_SYNCED / UNSYNCED），供日志用。
func SyncType(resp *Response) string {
	if resp == nil {
		return ""
	}
	return resp.Lyrics.SyncType
}
