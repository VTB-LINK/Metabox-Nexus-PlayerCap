package lyric

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// Line represents a single lyric line
type Line struct {
	Index int
	Time  float32 // seconds
	Text  string
	// Detailed 逐字时间轴（仅 KRC 源有；LRC 源为零值，序列化成 {}）。
	// PlayTime 此处不填，由调用方按 offset 统一套（见 kugou.go 的 applyDetailedOffset）。
	Detailed player.LyricTextDetailed
}

// httpClient with timeout for lyric API calls
var httpClient = &http.Client{Timeout: 8 * time.Second}

// searchResponse mirrors the krcs.kugou.com/search response.
// Note: candidate `duration` is in MILLISECONDS, not seconds.
type searchResponse struct {
	Status     int    `json:"status"`
	Proposal   string `json:"proposal"` // server-recommended candidate ID
	Candidates []struct {
		ID          string `json:"id"`
		AccessKey   string `json:"accesskey"`
		Song        string `json:"song"`
		Singer      string `json:"singer"`
		Duration    int    `json:"duration"`     // milliseconds
		ProductFrom string `json:"product_from"` // "官方推荐歌词" / "第三方歌词"
		Score       int    `json:"score"`
	} `json:"candidates"`
}

type downloadResponse struct {
	Status  int    `json:"status"`
	Content string `json:"content"` // base64-encoded LRC
	Charset string `json:"charset"`
}

// Fetch retrieves lyrics for a song by hash and duration (in milliseconds).
//
// Strategy (3 tiers):
//  1. Search by hash via krcs.kugou.com.
//  2. If hash returns nothing (e.g. 伴唱 mode produces a client-side mixed
//     audio whose hash KuGou's lyric DB doesn't know), resolve the canonical
//     hash by searching the song catalog (mobilecdn.kugou.com) with
//     "singer name" — take the first hit whose songname/singer match — then
//     retry krcs hash search with that canonical hash.
//  3. Last-resort strict keyword fallback against krcs (handles cases where
//     hash mapping fails but the song does have lyrics indexed by keyword).
//
// Returns nil, nil when no lyrics are found (not an error).
func Fetch(hash string, durationMs int, name, singer string) ([]Line, error) {
	// Tier 1: lyric search by playing hash.
	cand, err := searchByHash(hash, durationMs)
	if err != nil {
		return nil, err
	}

	normName := normalizeName(name)
	normSinger := strings.TrimSpace(singer)

	// Tier 2: resolve canonical hash via song catalog and retry.
	if cand == nil && normName != "" {
		canonical, cerr := resolveCanonicalHash(normName, normSinger, durationMs)
		if cerr == nil && canonical != "" && !strings.EqualFold(canonical, hash) {
			cand, err = searchByHash(canonical, durationMs)
			if err != nil {
				return nil, err
			}
		}
	}

	// Tier 3: strict keyword fallback.
	if cand == nil && normName != "" {
		cand, err = searchByKeyword(normName, normSinger, durationMs)
		if err != nil {
			return nil, err
		}
	}

	if cand == nil {
		return nil, nil
	}

	return download(cand.ID, cand.AccessKey)
}

type searchCand struct {
	ID        string
	AccessKey string
}

// searchByHash queries krcs by hash and returns the server-recommended candidate
// (via `proposal` field), falling back to the first candidate.
func searchByHash(hash string, durationMs int) (*searchCand, error) {
	if hash == "" {
		return nil, nil
	}
	sr, err := callSearch("", durationMs, hash)
	if err != nil || sr == nil || len(sr.Candidates) == 0 {
		return nil, err
	}
	// Prefer the proposal candidate if present
	if sr.Proposal != "" {
		for _, c := range sr.Candidates {
			if c.ID == sr.Proposal {
				return &searchCand{ID: c.ID, AccessKey: c.AccessKey}, nil
			}
		}
	}
	c := sr.Candidates[0]
	return &searchCand{ID: c.ID, AccessKey: c.AccessKey}, nil
}

// searchByKeyword does a STRICT fallback search. A candidate is accepted only
// if its song name + singer match the normalized inputs AND its duration is
// within ±5s of the playback duration. This is meant for 伴奏/伴唱/Live
// variants where the original lyrics apply; it intentionally fails for DJ
// remixes/covers by other artists where original lyrics would mistime.
func searchByKeyword(normName, normSinger string, targetDurationMs int) (*searchCand, error) {
	keyword := normName
	if normSinger != "" {
		keyword = normSinger + " " + normName
	}
	sr, err := callSearch(keyword, targetDurationMs, "")
	if err != nil || sr == nil || len(sr.Candidates) == 0 {
		return nil, err
	}

	const toleranceMs = 5000
	type scored struct {
		id        string
		accessKey string
		official  int
		delta     int
		score     int
	}
	var matched []scored
	for _, c := range sr.Candidates {
		// Require exact normalized name match.
		if !player.SameSongName(c.Song, normName) {
			continue
		}
		// Require singer match when we know it.
		if normSinger != "" && !singerMatches(c.Singer, normSinger) {
			continue
		}
		// Require duration to be close (伴奏/伴唱 should be same length).
		if c.Duration <= 0 {
			continue
		}
		d := c.Duration - targetDurationMs
		if d < 0 {
			d = -d
		}
		if d > toleranceMs {
			continue
		}
		official := 0
		if strings.Contains(c.ProductFrom, "官方") {
			official = 1
		}
		matched = append(matched, scored{
			id: c.ID, accessKey: c.AccessKey,
			official: official, delta: d, score: c.Score,
		})
	}
	if len(matched) == 0 {
		return nil, nil
	}
	// Rank: official desc, delta asc, score desc.
	best := matched[0]
	for _, m := range matched[1:] {
		if m.official > best.official ||
			(m.official == best.official && m.delta < best.delta) ||
			(m.official == best.official && m.delta == best.delta && m.score > best.score) {
			best = m
		}
	}
	return &searchCand{ID: best.id, AccessKey: best.accessKey}, nil
}

// singerMatches compares candidate.Singer against the playing singer field.
// KuGou may list multiple singers separated by 、/,// for collaborations;
// require at least one segment to equal the target (case-insensitive).
func singerMatches(candidateSinger, target string) bool {
	t := strings.TrimSpace(strings.ToLower(target))
	if t == "" {
		return true
	}
	cs := strings.ToLower(candidateSinger)
	for _, sep := range []string{"、", "/", ",", "，", "&", " feat. ", " feat ", " ft. ", " ft "} {
		cs = strings.ReplaceAll(cs, sep, "|")
	}
	for _, part := range strings.Split(cs, "|") {
		if player.NormalizeSongName(part) == player.NormalizeSongName(t) {
			return true
		}
	}
	return false
}

// callSearch hits krcs.kugou.com/search and returns the parsed response.
func callSearch(keyword string, durationMs int, hash string) (*searchResponse, error) {
	searchURL := fmt.Sprintf(
		"https://krcs.kugou.com/search?ver=1&man=yes&client=mobi&keyword=%s&duration=%d&hash=%s",
		url.QueryEscape(keyword), durationMs, hash,
	)
	resp, err := httpClient.Get(searchURL)
	if err != nil {
		return nil, fmt.Errorf("lyric search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lyric search body: %w", err)
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("lyric search parse: %w", err)
	}
	if sr.Status != 200 {
		return nil, nil
	}
	return &sr, nil
}

// download fetches lyrics for a candidate (lyrics.kugou.com endpoint still
// accepts ids issued by krcs).
//
// 先要 KRC（字级），拿不到再回落 LRC（行级）。搜索端点本就是 krcs.kugou.com（KRC 搜索），
// id/accesskey 两种格式通用，只是此前一直写死 fmt=lrc、把字级时间轴整个放弃了。
//
// **回落不是可选项**：KRC 可能缺失/解不开（魔数不符、密钥换代、zlib 坏），那时必须仍能出
// 行级歌词——绝不因为逐字拿不到就让主播整首没词。逐字有没有由 Line.Detailed 如实反映。
func download(id, accessKey string) ([]Line, error) {
	if content, err := fetchContent(id, accessKey, "krc"); err == nil && content != "" {
		if plain, derr := krcDecrypt(content); derr == nil {
			if lines := parseKRC(plain); len(lines) > 0 {
				return lines, nil
			}
		}
	}
	// 回落：行级 LRC（既有行为，一字未改）
	content, err := fetchContent(id, accessKey, "lrc")
	if err != nil {
		return nil, err
	}
	if content == "" {
		return nil, nil
	}
	lrcBytes, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("lyric base64 decode: %w", err)
	}
	return parseLRC(string(lrcBytes)), nil
}

// fetchContent 按指定格式（krc/lrc）取回 base64 的歌词内容；无内容时返回空串而非错误。
func fetchContent(id, accessKey, format string) (string, error) {
	dlURL := fmt.Sprintf(
		"https://lyrics.kugou.com/download?ver=1&client=pc&id=%s&accesskey=%s&fmt=%s&charset=utf8",
		id, accessKey, format,
	)
	resp, err := httpClient.Get(dlURL)
	if err != nil {
		return "", fmt.Errorf("lyric download: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("lyric download body: %w", err)
	}
	var dr downloadResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return "", fmt.Errorf("lyric download parse: %w", err)
	}
	if dr.Status != 200 {
		return "", nil
	}
	return dr.Content, nil
}

// normalizeName strips common KuGou version suffixes (伴奏/伴唱/Live) so the
// playing variant maps back to the original song title for keyword search.
// IMPORTANT: only suffixes that share the original recording's lyrics +
// timing are stripped. DJ/Remix/Cover variants are NOT stripped — their
// candidate.song field will retain the suffix, causing strict match to fail
// (which is desired: KuGou itself shows no lyrics for those).
func normalizeName(name string) string {
	n := strings.TrimSpace(name)
	for _, suf := range []string{
		" (伴奏)", " (伴唱)", " (Live)", " (live)", " (LIVE)",
		"（伴奏）", "（伴唱）", "（Live）", "（live）",
	} {
		n = strings.TrimSuffix(n, suf)
	}
	return strings.TrimSpace(n)
}

// songCatalogResponse mirrors mobilecdn.kugou.com/api/v3/search/song.
type songCatalogResponse struct {
	Status int `json:"status"`
	Data   struct {
		Info []struct {
			Hash         string `json:"hash"`
			Filename     string `json:"filename"`
			Songname     string `json:"songname"`
			Singername   string `json:"singername"`
			Duration     int    `json:"duration"` // seconds
			AlbumAudioID int64  `json:"album_audio_id"`
		} `json:"info"`
	} `json:"data"`
}

// resolveCanonicalHash searches the KuGou song catalog by "singer name" and
// returns the hash of the first entry whose songname + singername match (with
// duration within ±5s). This recovers the canonical audio hash when the
// playing hash is a client-side 伴唱 mix not registered in the lyric DB.
func resolveCanonicalHash(normName, normSinger string, targetDurationMs int) (string, error) {
	keyword := normName
	if normSinger != "" {
		keyword = normSinger + " " + normName
	}
	searchURL := fmt.Sprintf(
		"http://mobilecdn.kugou.com/api/v3/search/song?format=json&keyword=%s&page=1&pagesize=10",
		url.QueryEscape(keyword),
	)
	resp, err := httpClient.Get(searchURL)
	if err != nil {
		return "", fmt.Errorf("catalog search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("catalog search body: %w", err)
	}
	var sr songCatalogResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return "", fmt.Errorf("catalog search parse: %w", err)
	}
	if sr.Status != 1 || len(sr.Data.Info) == 0 {
		return "", nil
	}

	const toleranceMs = 5000
	targetSec := targetDurationMs / 1000
	for _, entry := range sr.Data.Info {
		// Songname must exactly match the normalized playing name.
		if !strings.EqualFold(strings.TrimSpace(entry.Songname), normName) {
			continue
		}
		if normSinger != "" && !singerMatches(entry.Singername, normSinger) {
			continue
		}
		// Duration must be close (±5s). Catalog duration is in seconds.
		if targetSec > 0 && entry.Duration > 0 {
			d := entry.Duration*1000 - targetDurationMs
			if d < 0 {
				d = -d
			}
			if d > toleranceMs {
				continue
			}
		}
		if entry.Hash != "" {
			return entry.Hash, nil
		}
	}
	return "", nil
}

// krcXORKey 是酷狗 KRC 的固定异或密钥（16 字节，循环使用）。
var krcXORKey = [16]byte{0x40, 0x47, 0x61, 0x77, 0x5E, 0x32, 0x74, 0x47, 0x51, 0x36, 0x31, 0x2D, 0xCE, 0xD2, 0x6E, 0x69}

var (
	// krcLineRegex 匹配 KRC 行首：[行起始ms,行时长ms]
	krcLineRegex = regexp.MustCompile(`^\[(\d+),(\d+)\](.*)$`)
	// krcWordRegex 匹配 KRC 字级标记：<相对偏移ms,时长ms,0>
	krcWordRegex = regexp.MustCompile(`<(\d+),(\d+),\d+>`)
)

// krcDecrypt 解开酷狗 KRC：base64 → 4 字节 "krc1" 魔数 → 逐字节异或定长密钥 → zlib → UTF-8。
//
// 与 QQ 的 QRC（3DES + zlib）是两套完全不同的东西，别混用。
func krcDecrypt(contentB64 string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return "", fmt.Errorf("krc base64: %w", err)
	}
	if len(blob) <= 4 || string(blob[:4]) != "krc1" {
		return "", fmt.Errorf("krc 魔数不符（前 4 字节非 krc1）")
	}
	body := blob[4:]
	dec := make([]byte, len(body))
	for i, b := range body {
		dec[i] = b ^ krcXORKey[i%16]
	}
	zr, err := zlib.NewReader(bytes.NewReader(dec))
	if err != nil {
		return "", fmt.Errorf("krc zlib init: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("krc zlib decompress: %w", err)
	}
	return string(out), nil
}

// parseKRCWords 从 KRC 行体解析逐字片段。行体形如：
//
//	<0,450,0>词<450,450,0>：<900,450,0>周
//
// **三家三个样，照抄任何一家都会错**：
//
//	QQ QRC  ：文字在前、时间戳在后，偏移**绝对**    风(28837,439)
//	网易云 YRC：时间戳在前、文字在后，偏移**绝对**    (28837,439,0)风
//	酷狗 KRC ：时间戳在前、文字在后，偏移**相对行首** <0,439,0>风   ← 本函数
//
// 故绝对时间 = lineStartMs + 偏移。把 KRC 的偏移当绝对时间用，每行的字会全挤到歌曲开头
// （行级却是对的），症状极像"前端没对齐"，最难查。
//
// 实证：《晴天》行 [2250,2250] 的末字 <1800,450,0> → 2250+1800+450 = 4500，正好等于下一行
// 起始 [4500,...]，逐字与行级严丝合缝。
//
// 不对片段文本做 TrimSpace：空格是逐字排版的一部分。
// PlayTime 此处不填（offset 未知），由 kugou.go 的 applyDetailedOffset 统一套。
func parseKRCWords(body string, lineStartMs int) []player.LyricTextDetailedWord {
	tuples := krcWordRegex.FindAllStringSubmatchIndex(body, -1)
	if len(tuples) == 0 {
		return nil
	}
	words := make([]player.LyricTextDetailedWord, 0, len(tuples))
	for i, m := range tuples {
		offMs, err := strconv.Atoi(body[m[2]:m[3]])
		if err != nil {
			continue
		}
		durMs, err := strconv.Atoi(body[m[4]:m[5]])
		if err != nil {
			continue
		}
		// 文本 = 本标记之后、到下一个标记开头之间那段（同 YRC 的取法）
		textEnd := len(body)
		if i+1 < len(tuples) {
			textEnd = tuples[i+1][0]
		}
		text := body[m[1]:textEnd]
		if text == "" {
			continue
		}
		words = append(words, player.LyricTextDetailedWord{
			Timestamp: float32(lineStartMs+offMs) / 1000.0, // ← 相对转绝对
			Duration:  float32(durMs) / 1000.0,
			Text:      text,
		})
	}
	return words
}

// parseKRC 解析 KRC 全文为 Line（含逐字）。
//
// 元数据标签（[ti:]/[ar:]/[al:]/[by:]/[offset:]）一律跳过——与既有 parseLRC 的行为一致：
// 它对 [offset:] 也是丢弃的（parseTimestamp 解不出 "offset" 这个分钟数 → 整行没文本 → skip）。
// 这里保持同样处理，不引入未经验证的偏移符号约定；真要支持需先找到 offset 非 0 的样本验证方向。
func parseKRC(krc string) []Line {
	var lines []Line
	for _, raw := range strings.Split(krc, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		m := krcLineRegex.FindStringSubmatch(raw)
		if m == nil {
			continue // 元数据标签行 / 空行
		}
		lineStartMs, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		lineDurMs, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		words := parseKRCWords(m[3], lineStartMs)
		if len(words) == 0 {
			continue
		}
		var text strings.Builder
		for _, w := range words {
			text.WriteString(w.Text)
		}
		plain := strings.TrimSpace(text.String())
		if plain == "" {
			continue
		}
		lineTimestamp := float32(lineStartMs) / 1000.0
		lineDuration := float32(lineDurMs) / 1000.0
		if lineDurMs == 0 {
			for _, w := range words {
				lineDuration += w.Duration
			}
		}
		lines = append(lines, Line{
			Index: len(lines),
			Time:  lineTimestamp,
			Text:  plain,
			Detailed: player.LyricTextDetailed{
				Timestamp: lineTimestamp,
				Duration:  lineDuration,
				Words:     words,
			},
		})
	}
	return lines
}

// parseLRC parses an LRC-format string into Line slices.
// Supports [mm:ss.xx] and [mm:ss:xx] timestamp formats.
// Lines without timestamps, or metadata tags, are skipped.
func parseLRC(lrc string) []Line {
	var lines []Line
	index := 0
	for _, raw := range strings.Split(lrc, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Parse all timestamps on this line (multiple allowed)
		rest := raw
		var timestamps []float32
		for strings.HasPrefix(rest, "[") {
			end := strings.Index(rest, "]")
			if end < 0 {
				break
			}
			tag := rest[1:end]
			rest = rest[end+1:]

			t, ok := parseTimestamp(tag)
			if ok {
				timestamps = append(timestamps, t)
			}
		}
		text := strings.TrimSpace(rest)
		if len(timestamps) == 0 || text == "" {
			continue
		}
		for _, ts := range timestamps {
			lines = append(lines, Line{Index: index, Time: ts, Text: text})
			index++
		}
	}
	// Sort by time
	sortLines(lines)
	// Re-index after sort
	for i := range lines {
		lines[i].Index = i
	}
	return lines
}

// parseTimestamp parses "mm:ss.xx" or "mm:ss:xx" into seconds.
func parseTimestamp(tag string) (float32, bool) {
	// Replace first ':' separator after minutes with ':'
	// Handle mm:ss.xx and mm:ss:xx variants
	tag = strings.ReplaceAll(tag, ",", ".")
	parts := strings.SplitN(tag, ":", 3)
	if len(parts) < 2 {
		return 0, false
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, false
	}
	secStr := strings.TrimSpace(parts[1])
	// Replace ':' within seconds (mm:ss:xx → mm:ss.xx)
	if len(parts) == 3 {
		secStr = secStr + "." + strings.TrimSpace(parts[2])
	}
	sec, err := strconv.ParseFloat(secStr, 32)
	if err != nil {
		return 0, false
	}
	return float32(min)*60 + float32(sec), true
}

// sortLines sorts lyric lines by timestamp (simple insertion sort for small slices).
func sortLines(lines []Line) {
	for i := 1; i < len(lines); i++ {
		key := lines[i]
		j := i - 1
		for j >= 0 && lines[j].Time > key.Time {
			lines[j+1] = lines[j]
			j--
		}
		lines[j+1] = key
	}
}
