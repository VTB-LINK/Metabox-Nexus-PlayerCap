package qqmusic

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// QRC 3DES key from QQ Music
var qrc3DESKey = []byte("!@#)(*$%123ZXC!@!@#)(NHL")

// lyricLine 内部歌词行（解析中间结果）
type lyricLine struct {
	Index int
	Time  float32
	Text  string
}

// Musicu API response structures
type musicuResponse struct {
	Req0 struct {
		Code int `json:"code"`
		Data struct {
			SongID   int    `json:"songID"`
			SongName string `json:"songName"`
			Lyric    string `json:"lyric"`
			Trans    string `json:"trans"`
			Roma     string `json:"roma"`
			Crypt    int    `json:"crypt"`
		} `json:"data"`
	} `json:"req_0"`
}

// qrcDecrypt decrypts QQ Music's QRC-encrypted lyrics using the ported 3DES algorithm.
// Process: hex string → custom 3DES-ECB decrypt → zlib decompress → UTF-8 string
func qrcDecrypt(encrypted string) (string, error) {
	if encrypted == "" {
		return "", nil
	}

	ciphertext, err := hex.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("hex decode: %w", err)
	}

	if len(ciphertext)%8 != 0 {
		return "", fmt.Errorf("ciphertext length %d not multiple of 8", len(ciphertext))
	}

	// Use the ported QQ Music 3DES implementation (in qrc_decrypt.go)
	schedule := tripleDesKeySetup(qrc3DESKey, desDecrypt)

	plaintext := make([]byte, 0, len(ciphertext))
	for i := 0; i < len(ciphertext); i += 8 {
		block := tripleDesCrypt(ciphertext[i:i+8], schedule)
		plaintext = append(plaintext, block...)
	}

	// zlib decompress
	reader, err := zlib.NewReader(bytes.NewReader(plaintext))
	if err != nil {
		return "", fmt.Errorf("zlib init: %w", err)
	}
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("zlib decompress: %w", err)
	}

	return string(decompressed), nil
}

// isHexString checks if a string contains only valid hex characters
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

// fetchLRC fetches lyrics using QQ Music's native client API (musicu.fcg)
// This uses songId directly from memory - no search API needed!
func fetchLRC(songID uint32, cookie string, durationMs uint32) ([]lyricLine, string, string, error) {
	// Build the same request QQ Music client sends internally
	payload := map[string]interface{}{
		"comm": map[string]interface{}{
			"ct": 19,
			"cv": 2216,
		},
		"req_0": map[string]interface{}{
			"module": "music.musichallSong.PlayLyricInfo",
			"method": "GetPlayLyricInfo",
			"param": map[string]interface{}{
				"songId":  songID,
				"crypt":   1, // Tell server we can handle QRC encryption
				"lrc_t":   0,
				"qrc":     1,
				"qrc_t":   0,
				"roma":    0,
				"roma_t":  0,
				"trans":   1,
				"trans_t": 0,
				"type":    1,
				"ct":      19,
				"cv":      2216,
			},
		},
	}

	jsonPayload, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QQMusic/2216 CFNetwork/1.0 Darwin/23.0.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var musicuResp musicuResponse
	if err := json.Unmarshal(body, &musicuResp); err != nil {
		return nil, "", "", fmt.Errorf("json parse: %w", err)
	}
	if musicuResp.Req0.Code != 0 {
		return nil, "", "", fmt.Errorf("API error code: %d", musicuResp.Req0.Code)
	}

	data := musicuResp.Req0.Data
	rawLyric := data.Lyric
	log.Detail("API 响应: songID=%d crypt=%d lyricLen=%d", data.SongID, data.Crypt, len(rawLyric))

	if rawLyric == "" {
		return nil, "", "", fmt.Errorf("no lyric data for songId=%d", songID)
	}

	// Try QRC decrypt if crypt flag is set OR if data looks like hex
	isHex := len(rawLyric) > 32 && isHexString(rawLyric[:32])
	if data.Crypt == 1 || isHex {
		decrypted, err := qrcDecrypt(rawLyric)
		if err != nil {
			log.Warn("QRC 解密失败: %v", err)
		} else {
			rawLyric = decrypted
			log.Detail("QRC 解密成功 (%d 字符)", len(rawLyric))
		}
	}

	rawLyric = html.UnescapeString(rawLyric)

	lines, name, singer := parseLRC(rawLyric, durationMs)
	return lines, name, singer, nil
}

func parseLRC(lrc string, durationMs uint32) ([]lyricLine, string, string) {
	var lines []lyricLine
	var rawTextLines []string
	var songName, singer string
	re := regexp.MustCompile(`\[(\d{2}):(\d{2})\.(\d{2,3})\](.*)`)
	qqRe := regexp.MustCompile(`^\[(\d+),(\d+)\](.+)`)
	qqCharTimingRe := regexp.MustCompile(`\(\d+,\d+\)`)
	tiRe := regexp.MustCompile(`\[ti:(.*?)\]`)
	arRe := regexp.MustCompile(`\[ar:(.*?)\]`)

	for _, line := range strings.Split(lrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if match := tiRe.FindStringSubmatch(line); len(match) > 1 {
			songName = match[1]
			continue
		}
		if match := arRe.FindStringSubmatch(line); len(match) > 1 {
			singer = match[1]
			continue
		}

		// Standard LRC [mm:ss.xx]
		matches := re.FindStringSubmatch(line)
		if len(matches) == 5 {
			min, _ := strconv.Atoi(matches[1])
			sec, _ := strconv.Atoi(matches[2])
			ms, _ := strconv.Atoi(matches[3])
			if len(matches[3]) == 2 {
				ms *= 10
			}
			text := strings.TrimSpace(matches[4])
			if text != "" {
				lines = append(lines, lyricLine{
					Index: len(lines),
					Time:  float32(min)*60 + float32(sec) + float32(ms)/1000.0,
					Text:  text,
				})
			}
			continue
		}

		// QQ proprietary [startMs,durationMs]text(charTs,charDur)...
		qqMatches := qqRe.FindStringSubmatch(line)
		if len(qqMatches) == 4 {
			startMs, _ := strconv.Atoi(qqMatches[1])
			rawText := qqMatches[3]
			plainText := qqCharTimingRe.ReplaceAllString(rawText, "")
			plainText = strings.TrimSpace(plainText)
			if plainText != "" {
				lines = append(lines, lyricLine{
					Index: len(lines),
					Time:  float32(startMs) / 1000.0,
					Text:  plainText,
				})
			}
		}

		// Plain text (no timestamp)
		if !strings.HasPrefix(line, "[") {
			rawTextLines = append(rawTextLines, line)
		}
	}

	// Fallback: distribute plain text evenly across duration
	if len(lines) == 0 && len(rawTextLines) > 0 {
		intervalSec := float32(4.0)
		if durationMs > 0 && len(rawTextLines) > 0 {
			intervalSec = (float32(durationMs) / 1000.0) / float32(len(rawTextLines))
		}
		for i, text := range rawTextLines {
			if text != "" {
				lines = append(lines, lyricLine{
					Index: len(lines),
					Time:  float32(i) * intervalSec,
					Text:  text,
				})
			}
		}
	}
	return lines, songName, singer
}

// songDetailResponse holds the relevant fields from get_song_detail_yqq
type songDetailResponse struct {
	Req0 struct {
		Code int `json:"code"`
		Data struct {
			TrackInfo struct {
				Album struct {
					Mid  string `json:"mid"`
					Pmid string `json:"pmid"`
					Name string `json:"name"`
				} `json:"album"`
				Singer []struct {
					Mid  string `json:"mid"`
					Pmid string `json:"pmid"`
					Name string `json:"name"`
				} `json:"singer"`
				Mid  string `json:"mid"`
				Name string `json:"name"`
			} `json:"track_info"`
		} `json:"data"`
	} `json:"req_0"`
}

// fetchCoverURL gets the album cover URL for a song using QQ Music's native API.
// Returns the cover URL (800x800) or empty string on failure.
func fetchCoverURL(songID uint32) string {
	payload := map[string]interface{}{
		"comm": map[string]interface{}{
			"ct": 19,
			"cv": 2216,
		},
		"req_0": map[string]interface{}{
			"module": "music.pf_song_detail_svr",
			"method": "get_song_detail_yqq",
			"param": map[string]interface{}{
				"song_id": songID,
			},
		},
	}

	jsonPayload, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(jsonPayload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QQMusic/2216 CFNetwork/1.0 Darwin/23.0.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var detail songDetailResponse
	if err := json.Unmarshal(body, &detail); err != nil || detail.Req0.Code != 0 {
		return ""
	}

	track := detail.Req0.Data.TrackInfo

	// Priority: album cover → first singer cover
	if mid := track.Album.Mid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("封面(专辑): %s → %s", track.Album.Name, url)
		return url
	}
	if mid := track.Album.Pmid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("封面(专辑pmid): %s", url)
		return url
	}
	for _, s := range track.Singer {
		if mid := s.Mid; mid != "" {
			url := buildCoverURL("T001", mid, 800)
			log.Detail("封面(歌手): %s → %s", s.Name, url)
			return url
		}
	}
	return ""
}

// buildCoverURL constructs a QQ Music CDN cover URL.
// kind: "T001" for singer, "T002" for album
// size: 150, 300, 500, 800
func buildCoverURL(kind string, mid string, size int) string {
	return fmt.Sprintf("https://y.gtimg.cn/music/photo_new/%sR%dx%dM000%s.jpg", kind, size, size, mid)
}
