package lyric

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// download fetches the LRC content for a candidate (lyrics.kugou.com endpoint
// still accepts ids issued by krcs).
func download(id, accessKey string) ([]Line, error) {
	dlURL := fmt.Sprintf(
		"https://lyrics.kugou.com/download?ver=1&client=pc&id=%s&accesskey=%s&fmt=lrc&charset=utf8",
		id, accessKey,
	)
	resp, err := httpClient.Get(dlURL)
	if err != nil {
		return nil, fmt.Errorf("lyric download: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lyric download body: %w", err)
	}
	var dr downloadResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("lyric download parse: %w", err)
	}
	if dr.Status != 200 || dr.Content == "" {
		return nil, nil
	}
	lrcBytes, err := base64.StdEncoding.DecodeString(dr.Content)
	if err != nil {
		return nil, fmt.Errorf("lyric base64 decode: %w", err)
	}
	return parseLRC(string(lrcBytes)), nil
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
