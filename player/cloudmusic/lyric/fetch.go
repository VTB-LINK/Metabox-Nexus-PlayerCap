package lyric

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

var log = logger.New("CloudMusic")

// httpClient 用于所有网易云公开 HTTP API 调用。**必须有超时**：http.DefaultClient 的
// Timeout 为 0（无限），网络挂起（TCP 连上但服务端不再响应）会永久阻塞——而这些调用
// 都在轮询循环的同步取词路径上（FetchLyrics / SearchSongID 由 runSession 直接调用），
// 卡死后取词 goroutine 再也不返回，歌词永远停住，不崩不报错。10s 足够覆盖慢网络，
// 超时后失败会在下次轮询重试。
var httpClient = &http.Client{Timeout: 10 * time.Second}

type LyricLine struct {
	Index        int
	Time         float32
	Text         string
	SubText      string
	RomaText     string
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
	Romalrc struct {
		Lyric string `json:"lyric"`
	} `json:"romalrc"`
	Yrc struct {
		Lyric string `json:"lyric"`
	} `json:"yrc"`
}

// lrcLineRegex 把一行拆成「行首连续时间戳串」+「正文」。用 `+` 匹配连续多个时间戳，是为了
// 支持**压缩型 LRC**（一句配多个时间戳，如 `[02:04.45][00:47.19]文字`，语义是「这句在这几个
// 时刻各出现一次」——网易云客户端确实在每个时刻各显示一次）。
// 不匹配的行（`[ti:]` 等元数据、网易云混在 lrc 里的 `{"t":0,...}` JSON 行）自然被跳过。
var lrcLineRegex = regexp.MustCompile(`^((?:\[\d+:\d+\.\d+\])+)(.*)$`)

// lrcTimeRegex 从上面的时间戳串里逐个取出 mm/ss/ms。
var lrcTimeRegex = regexp.MustCompile(`\[(\d+):(\d+)\.(\d+)\]`)
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

	resp, err := httpClient.Do(req)
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

	resp, err := httpClient.Do(req)
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
	MergeRomalrc(lines, apiResp.Romalrc.Lyric)
	MergeYRC(lines, apiResp.Yrc.Lyric, offsetSec)
	log.Info("歌词加载完成(API): %d 行 (ID=%s)；逐字：%s", len(lines), songID, detailedFlag(lines))
	return lines, nil
}

func detailedFlag(lines []LyricLine) string {
	for _, line := range lines {
		if len(line.TextDetailed.Words) > 0 {
			return i18n.T("是")
		}
	}
	return i18n.T("否")
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

	resp, err := httpClient.Do(req)
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

// MergeRomalrc merges romanized lyrics (romalrc LRC) into main lyrics as RomaText.
//
// 与 MergeTlyric 完全同构：网易云的 romalrc 是逐行罗马音（音译），格式与 tlyric（翻译）一样
// 是标准 LRC，按毫秒时间戳就近对齐。唯一区别是写入 RomaText 而非 SubText——音译与翻译是两条
// 独立的轨，绝不能互相覆盖。romalrc 为空、或某行匹配不上时 RomaText 留空，不强塞。
func MergeRomalrc(lyrics []LyricLine, romalrcLRC string) {
	if romalrcLRC == "" || len(lyrics) == 0 {
		return
	}
	rLines := ParseLRC(romalrcLRC)
	if len(rLines) == 0 {
		return
	}
	rmap := make(map[int]string, len(rLines))
	for _, rl := range rLines {
		rmap[int(rl.Time*1000+0.5)] = rl.Text
	}
	for i := range lyrics {
		if text, ok := rmap[int(lyrics[i].Time*1000+0.5)]; ok {
			lyrics[i].RomaText = text
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
	const toleranceMs = 3000
	bestKey := 0
	bestDiff := toleranceMs + 1
	for yrcTimeMs, detail := range details {
		if used[yrcTimeMs] {
			continue
		}
		if !player.SameLyricText(lyricText, detailText(detail)) {
			continue
		}
		diff := yrcTimeMs - lyricTimeMs
		if diff < 0 {
			diff = -diff
		}
		// 平局取时间戳小的那个：`details` 是 map，遍历序随机，只用 `diff < bestDiff` 的话
		// 同一份输入每次跑可能选中不同条目（实测 200 次得 10:183 / 14:17 这种分布）——
		// 逐字高亮层因此随机错位，且 bug 不可复现。tie-break 让它变成确定的。
		//
		// 无初值缺陷：bestDiff 初值为 toleranceMs+1，首个候选要么走 `diff < bestDiff`
		// 更新 bestKey，要么其 diff 已 > toleranceMs、最终被下面的 `bestDiff > toleranceMs`
		// 拦掉 —— bestKey 不会停在 0 被误用。
		if diff < bestDiff || (diff == bestDiff && yrcTimeMs < bestKey) {
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

// ParseLRC 解析 LRC 歌词。
//
// **支持压缩型**：一行可以有多个行首时间戳（`[02:04.45][00:47.19]文字`），语义是「这句在
// 这几个时刻各出现一次」——网易云客户端确实在每个时刻各显示一次，故每个时间戳各生成一条。
// 同一句叠句在整首里出现几次，行首就有几个时间戳，不限于两个。
//
// **结果按时间升序，Index 按时间顺序重排**。不能沿用「lrc 里的出现顺序」：网易云把压缩型的
// 时间戳按**降序**排（`[02:04.45][00:47.19]`），照原顺序输出会让 Index 与时间对不上。普通
// LRC 本就升序，排序对它们是 no-op。
func ParseLRC(lrc string) []LyricLine {
	var result []LyricLine

	for _, raw := range strings.Split(lrc, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		// 拆成「行首连续时间戳串」+「正文」。无行首时间戳的行（[ti:] 等元数据、
		// 网易云混在 lrc 里的 {"t":0,...} JSON 行）不匹配，跳过。
		m := lrcLineRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		// **正文为空也保留** —— 这是 intentional blank，不是垃圾。作词者用它表示「此刻把
		// 歌词清掉」（间奏 / 句与句之间的停顿）。真实数据实证（网易云 id=5277704）：有一条
		// blank 自己就是压缩型（两个时刻各清一次），且下一句在它之后 0.18s 就来——即精确
		// 要求「上句显示 1.97s → 空 0.18s → 下句」。吞掉它，前端在该空的地方会一直糊着上
		// 一句。原实现（含本函数改造前）跳过空正文，是既有缺陷。
		//
		// 注意：blank 只是 Text 为空，**不是**「没有歌词」。判断一首歌有没有歌词要看有没有
		// 实词行，别用 len(结果) —— 见 cloudmusic.go 的 resolveCDPLyrics。
		text := strings.TrimSpace(m[2])

		for _, tm := range lrcTimeRegex.FindAllStringSubmatch(m[1], -1) {
			minutes, _ := strconv.Atoi(tm[1])
			seconds, _ := strconv.Atoi(tm[2])
			msStr := tm[3]
			// Normalize milliseconds: "123" -> 123, "12" -> 120, "1" -> 100
			for len(msStr) < 3 {
				msStr += "0"
			}
			ms, _ := strconv.Atoi(msStr[:3])

			result = append(result, LyricLine{
				Time: float32(minutes)*60 + float32(seconds) + float32(ms)/1000.0,
				Text: text,
			})
		}
	}

	// 按时间升序 + 重排 Index。SliceStable 让同一时刻的多条保持原有先后。
	sort.SliceStable(result, func(i, j int) bool { return result[i].Time < result[j].Time })
	for i := range result {
		result[i].Index = i
	}

	return result
}
