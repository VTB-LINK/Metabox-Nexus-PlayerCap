package lyric

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Line represents a single lyric line
type Line struct {
	Index int
	Time  float32 // seconds
	Text  string
}

// httpClient with timeout for lyric API calls
var httpClient = &http.Client{Timeout: 8 * time.Second}

type searchResponse struct {
	Status     int `json:"status"`
	Candidates []struct {
		ID        string `json:"id"`
		AccessKey string `json:"accesskey"`
		Song      string `json:"song"`
		Singer    string `json:"singer"`
		Duration  int    `json:"duration"` // seconds
	} `json:"candidates"`
}

type downloadResponse struct {
	Status  int    `json:"status"`
	Content string `json:"content"` // base64-encoded LRC
	Charset string `json:"charset"`
}

// Fetch retrieves lyrics for a song by hash and duration (in milliseconds).
// Returns nil, nil when no lyrics are found (not an error).
func Fetch(hash string, durationMs int) ([]Line, error) {
	if hash == "" {
		return nil, nil
	}
	durationSec := durationMs / 1000

	// Step 1: search
	searchURL := fmt.Sprintf(
		"https://lyrics.kugou.com/search?ver=1&man=yes&client=pc&keyword=&duration=%d&hash=%s",
		durationSec, hash,
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
	if sr.Status != 200 || len(sr.Candidates) == 0 {
		return nil, nil // no lyrics available
	}

	cand := sr.Candidates[0]

	// Step 2: download
	dlURL := fmt.Sprintf(
		"https://lyrics.kugou.com/download?ver=1&client=pc&id=%s&accesskey=%s&fmt=lrc&charset=utf8",
		cand.ID, cand.AccessKey,
	)
	resp2, err := httpClient.Get(dlURL)
	if err != nil {
		return nil, fmt.Errorf("lyric download: %w", err)
	}
	defer resp2.Body.Close()
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, fmt.Errorf("lyric download body: %w", err)
	}

	var dr downloadResponse
	if err := json.Unmarshal(body2, &dr); err != nil {
		return nil, fmt.Errorf("lyric download parse: %w", err)
	}
	if dr.Status != 200 || dr.Content == "" {
		return nil, nil
	}

	// Step 3: decode base64 → LRC text
	lrcBytes, err := base64.StdEncoding.DecodeString(dr.Content)
	if err != nil {
		return nil, fmt.Errorf("lyric base64 decode: %w", err)
	}

	return parseLRC(string(lrcBytes)), nil
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
