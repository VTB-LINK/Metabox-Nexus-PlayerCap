package lyric

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

var log = logger.New("CloudMusic")

type LyricLine struct {
	Index        int
	Time         float32
	Text         string
	SubText      string
	TextDetailed player.LyricTextDetailed
}

type SongDetail struct {
	Name     string
	Artist   string
	Album    string
	CoverUrl string
	Duration int // ms
}

type apiResponse struct {
	Lrc struct {
		Lyric string `json:"lyric"`
	} `json:"lrc"`
	Tlyric struct {
		Lyric string `json:"lyric"`
	} `json:"tlyric"`
	Yrc struct {
		Lyric string `json:"lyric"`
	} `json:"yrc"`
}

var lrcTimeRegex = regexp.MustCompile(`\[(\d+):(\d+)\.(\d+)\](.*)`)
var yrcLineRegex = regexp.MustCompile(`^\[(\d+),(\d+)\](.*)$`)
var yrcWordRegex = regexp.MustCompile(`\((\d+),(\d+),(\d+)\)`)

// FetchSongDetail fetches song name, artist, cover from the NetEase API.
func FetchSongDetail(songID string) (*SongDetail, error) {
	url := fmt.Sprintf("https://music.163.com/api/song/detail/?ids=[%s]&id=%s", songID, songID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://music.163.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Try the v3 format first
	var v3Resp struct {
		Songs []struct {
			Name    string `json:"name"`
			Artists []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name   string `json:"name"`
				PicUrl string `json:"picUrl"`
			} `json:"album"`
			Duration int `json:"duration"`
		} `json:"songs"`
	}
	if err := json.Unmarshal(body, &v3Resp); err == nil && len(v3Resp.Songs) > 0 {
		s := v3Resp.Songs[0]
		var artists []string
		for _, a := range s.Artists {
			artists = append(artists, a.Name)
		}
		detail := &SongDetail{
			Name:     s.Name,
			Artist:   strings.Join(artists, " / "),
			Album:    s.Album.Name,
			CoverUrl: s.Album.PicUrl,
			Duration: s.Duration,
		}
		log.Detail("歌曲详情: %s - %s, 封面: %s", detail.Name, detail.Artist, detail.CoverUrl)
		return detail, nil
	}

	return nil, fmt.Errorf("failed to parse song detail for %s", songID)
}

// FetchLyrics fetches lyrics from the NetEase Cloud Music API by song ID.
func FetchLyrics(songID string, offsetSec float32) ([]LyricLine, error) {
	url := fmt.Sprintf("https://music.163.com/api/song/lyric/v1?id=%s&cp=false&tv=0&lv=0&rv=0&kv=0&yv=0&ytv=0&yrv=0", songID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://music.163.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("lyrics API JSON parse error: %v", err)
	}

	if apiResp.Lrc.Lyric == "" {
		return nil, fmt.Errorf("no lyrics found for song %s", songID)
	}

	lines := ParseLRC(apiResp.Lrc.Lyric)
	MergeTlyric(lines, apiResp.Tlyric.Lyric)
	MergeYRC(lines, apiResp.Yrc.Lyric, offsetSec)
	log.Info("歌词加载完成(API): %d 行 (ID=%s)；逐字：%s", len(lines), songID, detailedFlag(lines))
	return lines, nil
}

func detailedFlag(lines []LyricLine) string {
	for _, line := range lines {
		if len(line.TextDetailed.Words) > 0 {
			return "是"
		}
	}
	return "否"
}

// SearchSongID searches NetEase API by song name (+ optional artist) and returns the song ID.
func SearchSongID(songName string, artist string, targetDurationMs int) (string, error) {
	query := songName
	if artist != "" {
		query = songName + " " + artist
	}
	searchURL := fmt.Sprintf("https://music.163.com/api/search/get?s=%s&type=1&limit=10", url.QueryEscape(query))

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://music.163.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var searchResp struct {
		Result struct {
			Songs []struct {
				ID       int    `json:"id"`
				Name     string `json:"name"`
				Duration int    `json:"duration"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"songs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return "", fmt.Errorf("search API parse error: %v", err)
	}

	if len(searchResp.Result.Songs) == 0 {
		return "", fmt.Errorf("no results for '%s'", query)
	}

	// Prefer exact name + artist + duration match.
	var exactNameFallback string
	for _, s := range searchResp.Result.Songs {
		if !sameSongName(s.Name, songName) {
			continue
		}
		id := fmt.Sprintf("%d", s.ID)
		if exactNameFallback == "" {
			exactNameFallback = id
		}
		if !artistMatches(s.Artists, artist) {
			continue
		}
		if targetDurationMs > 0 {
			if !durationWithinTolerance(s.Duration, targetDurationMs, 0.05) {
				log.Detail("搜索: 跳过 %s (ID: %s, duration=%dms, target=%dms)", s.Name, id, s.Duration, targetDurationMs)
				continue
			}
			log.Detail("搜索: 名称/歌手/时长匹配 %s (ID: %s, duration=%dms)", s.Name, id, s.Duration)
			return id, nil
		}
		log.Detail("搜索: 名称/歌手匹配 %s (ID: %s)", s.Name, id)
		return id, nil
	}

	if targetDurationMs > 0 {
		return "", fmt.Errorf("no duration match for '%s' within ±5%% (%dms)", query, targetDurationMs)
	}

	if exactNameFallback != "" {
		log.Detail("搜索: 使用精确歌名兜底 (ID: %s)", exactNameFallback)
		return exactNameFallback, nil
	}

	// Fallback to first result
	id := fmt.Sprintf("%d", searchResp.Result.Songs[0].ID)
	log.Detail("搜索: 使用首个结果 %s (ID: %s)", searchResp.Result.Songs[0].Name, id)
	return id, nil
}

func sameSongName(candidate, target string) bool {
	return player.SameSongName(candidate, target)
}

func artistMatches(artists []struct {
	Name string `json:"name"`
}, target string) bool {
	target = normalizeSearchText(target)
	if target == "" {
		return true
	}
	targetParts := strings.FieldsFunc(target, func(r rune) bool {
		return r == '/' || r == '、' || r == ',' || r == '&' || r == '，'
	})
	for _, artist := range artists {
		candidate := normalizeSearchText(artist.Name)
		if candidate == "" {
			continue
		}
		if candidate == target || strings.Contains(target, candidate) || strings.Contains(candidate, target) {
			return true
		}
		for _, part := range targetParts {
			part = strings.TrimSpace(part)
			if part != "" && candidate == part {
				return true
			}
		}
	}
	return false
}

func durationWithinTolerance(candidateMs, targetMs int, tolerance float64) bool {
	if targetMs <= 0 {
		return true
	}
	if candidateMs <= 0 {
		return false
	}
	diff := candidateMs - targetMs
	if diff < 0 {
		diff = -diff
	}
	return float64(diff) <= float64(targetMs)*tolerance
}

func normalizeSearchText(value string) string {
	return player.NormalizeSongName(value)
}

// MergeTlyric merges translated lyrics (tlyric LRC) into main lyrics as SubText.
func MergeTlyric(lyrics []LyricLine, tlyricLRC string) {
	if tlyricLRC == "" || len(lyrics) == 0 {
		return
	}
	tLines := ParseLRC(tlyricLRC)
	if len(tLines) == 0 {
		return
	}
	tmap := make(map[int]string, len(tLines))
	for _, tl := range tLines {
		tmap[int(tl.Time*1000+0.5)] = tl.Text
	}
	for i := range lyrics {
		if text, ok := tmap[int(lyrics[i].Time*1000+0.5)]; ok {
			lyrics[i].SubText = text
		}
	}
}

func MergeYRC(lyrics []LyricLine, yrc string, offsetSec float32) {
	if yrc == "" || len(lyrics) == 0 {
		return
	}
	details := ParseYRC(yrc, 0)
	if len(details) == 0 {
		return
	}
	used := make(map[int]bool, len(details))
	for i := range lyrics {
		lyricTimeMs := int(lyrics[i].Time*1000 + 0.5)
		if detail, ok := findBestYRCDetail(lyricTimeMs, lyrics[i].Text, details, used); ok {
			detail = applyTextDetailedOffset(detail, offsetSec)
			lyrics[i].TextDetailed = detail
		}
	}
}

func findBestYRCDetail(lyricTimeMs int, lyricText string, details map[int]player.LyricTextDetailed, used map[int]bool) (player.LyricTextDetailed, bool) {
	const toleranceMs = 1250
	bestKey := 0
	bestDiff := toleranceMs + 1
	for yrcTimeMs, detail := range details {
		if used[yrcTimeMs] {
			continue
		}
		if !sameLyricText(lyricText, detailText(detail)) {
			continue
		}
		diff := yrcTimeMs - lyricTimeMs
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestKey = yrcTimeMs
			bestDiff = diff
		}
	}
	if bestDiff > toleranceMs {
		return player.LyricTextDetailed{}, false
	}
	used[bestKey] = true
	return details[bestKey], true
}

func applyTextDetailedOffset(detail player.LyricTextDetailed, offsetSec float32) player.LyricTextDetailed {
	detail.PlayTime = player.AdjustLyricPlayTime(detail.Timestamp, offsetSec)
	for i := range detail.Words {
		detail.Words[i].PlayTime = player.AdjustLyricPlayTime(detail.Words[i].Timestamp, offsetSec)
	}
	return detail
}

func detailText(detail player.LyricTextDetailed) string {
	var b strings.Builder
	for _, word := range detail.Words {
		b.WriteString(word.Text)
	}
	return b.String()
}

func sameLyricText(left, right string) bool {
	return player.SameLyricText(left, right)
}

func normalizeLyricText(value string) string {
	return player.NormalizeLyricText(value)
}

func ParseYRC(yrc string, offsetSec float32) map[int]player.LyricTextDetailed {
	result := make(map[int]player.LyricTextDetailed)
	for _, line := range strings.Split(yrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "{") {
			continue
		}
		matches := yrcLineRegex.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		lineStartMs, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		lineDurationMs, err := strconv.Atoi(matches[2])
		if err != nil {
			continue
		}
		body := matches[3]

		wordMatches := yrcWordRegex.FindAllStringSubmatchIndex(body, -1)
		if len(wordMatches) == 0 {
			continue
		}

		words := make([]player.LyricTextDetailedWord, 0, len(wordMatches))
		for i, match := range wordMatches {
			wordStartMs, err := strconv.Atoi(body[match[2]:match[3]])
			if err != nil {
				continue
			}
			wordDurationMs, err := strconv.Atoi(body[match[4]:match[5]])
			if err != nil {
				continue
			}
			textEnd := len(body)
			if i+1 < len(wordMatches) {
				textEnd = wordMatches[i+1][0]
			}
			text := body[match[1]:textEnd]
			if text == "" {
				continue
			}

			wordTimestamp := float32(wordStartMs) / 1000.0
			words = append(words, player.LyricTextDetailedWord{
				Timestamp: wordTimestamp,
				PlayTime:  player.AdjustLyricPlayTime(wordTimestamp, offsetSec),
				Duration:  float32(wordDurationMs) / 1000.0,
				Text:      text,
			})
		}
		if len(words) == 0 {
			continue
		}

		lineDuration := float32(lineDurationMs) / 1000.0
		if lineDurationMs == 0 {
			for _, word := range words {
				lineDuration += word.Duration
			}
		}
		lineTimestamp := float32(lineStartMs) / 1000.0
		result[lineStartMs] = player.LyricTextDetailed{
			Timestamp: lineTimestamp,
			PlayTime:  player.AdjustLyricPlayTime(lineTimestamp, offsetSec),
			Duration:  lineDuration,
			Words:     words,
		}
	}
	return result
}

func ParseLRC(lrc string) []LyricLine {
	var result []LyricLine
	idx := 0

	for _, line := range strings.Split(lrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := lrcTimeRegex.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		minutes, _ := strconv.Atoi(matches[1])
		seconds, _ := strconv.Atoi(matches[2])
		msStr := matches[3]
		// Normalize milliseconds: "123" -> 123, "12" -> 120, "1" -> 100
		for len(msStr) < 3 {
			msStr += "0"
		}
		ms, _ := strconv.Atoi(msStr[:3])

		text := strings.TrimSpace(matches[4])
		if text == "" {
			continue
		}

		timeSec := float32(minutes)*60 + float32(seconds) + float32(ms)/1000.0

		result = append(result, LyricLine{
			Index: idx,
			Time:  timeSec,
			Text:  text,
		})
		idx++
	}

	return result
}
