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
)

var log = logger.New("CloudMusic")

type LyricLine struct {
	Index int
	Time  float32
	Text  string
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
}

var lrcTimeRegex = regexp.MustCompile(`\[(\d+):(\d+)\.(\d+)\](.*)`)

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
func FetchLyrics(songID string) ([]LyricLine, error) {
	url := fmt.Sprintf("https://music.163.com/api/song/lyric?id=%s&lv=1&tv=1", songID)

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
	log.Info("歌词加载完成(API): %d 行 (ID=%s)", len(lines), songID)
	return lines, nil
}

// SearchSongID searches NetEase API by song name (+ optional artist) and returns the song ID.
func SearchSongID(songName string, artist string) (string, error) {
	query := songName
	if artist != "" {
		query = songName + " " + artist
	}
	searchURL := fmt.Sprintf("https://music.163.com/api/search/get?s=%s&type=1&limit=5", url.QueryEscape(query))

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
				ID      int    `json:"id"`
				Name    string `json:"name"`
				Artists []struct {
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

	// Prefer exact name match
	for _, s := range searchResp.Result.Songs {
		if s.Name == songName {
			id := fmt.Sprintf("%d", s.ID)
			log.Detail("搜索: 精确匹配 %s (ID: %s)", s.Name, id)
			return id, nil
		}
	}

	// Fallback to first result
	id := fmt.Sprintf("%d", searchResp.Result.Songs[0].ID)
	log.Detail("搜索: 使用首个结果 %s (ID: %s)", searchResp.Result.Songs[0].Name, id)
	return id, nil
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
