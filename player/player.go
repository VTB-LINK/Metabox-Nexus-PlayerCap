package player

import (
	"encoding/json"
	"strings"
	"unicode"
)

// LyricTextDetailedWord 逐字歌词片段
type LyricTextDetailedWord struct {
	Timestamp float32 `json:"timestamp"`
	PlayTime  float32 `json:"play_time"`
	Duration  float32 `json:"duration"`
	Text      string  `json:"text"`
}

// LyricTextDetailed 是 text 的逐字/细粒度扩展，零值序列化为 {}。
type LyricTextDetailed struct {
	Timestamp float32                 `json:"timestamp"`
	PlayTime  float32                 `json:"play_time"`
	Duration  float32                 `json:"duration"`
	Words     []LyricTextDetailedWord `json:"words"`
}

func (d LyricTextDetailed) MarshalJSON() ([]byte, error) {
	if len(d.Words) == 0 {
		return []byte("{}"), nil
	}
	type alias LyricTextDetailed
	return json.Marshal(alias(d))
}

// LyricDetailedLine 是 all_lyrics 的完整逐字歌词集合项，仅从逐字源数据派生。
type LyricDetailedLine struct {
	LyricIndex int                     `json:"lyric_index"`
	Timestamp  float32                 `json:"timestamp"`
	PlayTime   float32                 `json:"play_time"`
	Duration   float32                 `json:"duration"`
	Text       string                  `json:"text"`
	Words      []LyricTextDetailedWord `json:"words"`
}

// LyricLine 歌词行
type LyricLine struct {
	Index        int               `json:"index"`
	Timestamp    float32           `json:"timestamp"`
	PlayTime     float32           `json:"play_time"`
	Text         string            `json:"text"`
	SubText      string            `json:"sub_text"`
	TextDetailed LyricTextDetailed `json:"text_detailed"`
}

// SongInfo 歌曲信息
type SongInfo struct {
	Name        string `json:"name"`
	Singer      string `json:"singer"`
	Title       string `json:"title"`
	Cover       string `json:"cover"`
	CoverBase64 string `json:"cover_base64"`
}

// StatusInfo 播放器状态
type StatusInfo struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// LyricUpdate 歌词更新
type LyricUpdate struct {
	Index        int               `json:"index"`
	Text         string            `json:"text"`
	SubText      string            `json:"sub_text"`
	Timestamp    float32           `json:"timestamp"`
	PlayTime     float32           `json:"play_time"`
	Progress     float32           `json:"progress"`
	TextDetailed LyricTextDetailed `json:"text_detailed"`
}

// PlaybackTimeInfo 播放暂停/恢复事件载荷（仅 play_time）
type PlaybackTimeInfo struct {
	PlayTime float32 `json:"play_time"`
}

// AllLyricsData 完整歌词
type AllLyricsData struct {
	Title    string      `json:"title,omitempty"`
	Duration float32     `json:"duration"`
	PlayTime float32     `json:"play_time"`
	Progress float32     `json:"progress"`
	Count    int         `json:"count"`
	Lyrics   []LyricLine `json:"lyrics"`
}

func (d AllLyricsData) MarshalJSON() ([]byte, error) {
	type alias AllLyricsData
	out := struct {
		alias
		LyricsDetailed []LyricDetailedLine `json:"lyrics_detailed"`
	}{
		alias:          alias(d),
		LyricsDetailed: BuildLyricsDetailed(d.Lyrics),
	}
	return json.Marshal(out)
}

func BuildLyricsDetailed(lyrics []LyricLine) []LyricDetailedLine {
	detailed := make([]LyricDetailedLine, 0)
	for _, line := range lyrics {
		if len(line.TextDetailed.Words) == 0 {
			continue
		}
		detailed = append(detailed, LyricDetailedLine{
			LyricIndex: line.Index,
			Timestamp:  line.TextDetailed.Timestamp,
			PlayTime:   line.TextDetailed.PlayTime,
			Duration:   line.TextDetailed.Duration,
			Text:       BuildDetailedText(line.TextDetailed.Words),
			Words:      line.TextDetailed.Words,
		})
	}
	return detailed
}

func BuildDetailedText(words []LyricTextDetailedWord) string {
	var text string
	for _, word := range words {
		text += word.Text
	}
	return text
}

// Event 播放器事件
type Event struct {
	PlayerName string      // 播放器标识名
	Type       string      // 事件类型
	Data       interface{} // 具体载荷
}

// 事件类型常量
const (
	EventStatusUpdate   = "status_update"
	EventSongInfoUpdate = "song_info_update"
	EventLyricUpdate    = "lyric_update"
	EventAllLyrics      = "all_lyrics"
	EventPlaybackPause  = "playback_pause"
	EventPlaybackResume = "playback_resume"
	EventLyricIdle      = "lyric_idle"
	EventClearSongData  = "clear_song_data"
	EventPlayerSwitch   = "player_switch"
	EventPlayerClear    = "player_clear"
)

// PlayerSwitchInfo 播放器切换事件载荷
type PlayerSwitchInfo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Player 播放器接口
type Player interface {
	// Name 返回播放器标识名，如 "wesing", "cloudmusicv3"
	Name() string

	// Start 启动播放器轮询循环（阻塞，应在 goroutine 中调用）
	Start()

	// Stop 停止播放器
	Stop()

	// Events 返回事件通道，由主循环消费
	Events() <-chan Event
}

// BaseEmitter 公共事件发射器，可嵌入各播放器结构体以复用 Emit/Events/Stop/Name 方法。
type BaseEmitter struct {
	PlayerName string
	EventCh    chan Event
	StopCh     chan struct{}
}

// NewBaseEmitter 创建 BaseEmitter
func NewBaseEmitter(playerName string) BaseEmitter {
	return BaseEmitter{
		PlayerName: playerName,
		EventCh:    make(chan Event, 128),
		StopCh:     make(chan struct{}),
	}
}

func (b *BaseEmitter) Name() string         { return b.PlayerName }
func (b *BaseEmitter) Events() <-chan Event { return b.EventCh }
func (b *BaseEmitter) Stop()                { close(b.StopCh) }

// Emit 向事件通道发送事件，通道满时丢弃（非阻塞）。
func (b *BaseEmitter) Emit(evtType string, data interface{}) {
	select {
	case b.EventCh <- Event{PlayerName: b.PlayerName, Type: evtType, Data: data}:
	default:
	}
}

// ClampFloat32 将值限制在 [min, max] 范围内
func ClampFloat32(v, min, max float32) float32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// AdjustLyricPlayTime returns the offset-adjusted lyric timestamp in seconds.
func AdjustLyricPlayTime(timestamp, offsetSec float32) float32 {
	adjusted := timestamp - offsetSec
	if adjusted < 0 {
		return 0
	}
	return adjusted
}

// LyricDisplayStart returns the earliest time a line should appear.
// If the line has word-level timing that starts before the LRC line time, use that.
func LyricDisplayStart(lineTime float32, detailed LyricTextDetailed) float32 {
	if len(detailed.Words) > 0 && detailed.Timestamp < lineTime {
		return detailed.Timestamp
	}
	return lineTime
}

// BuildLyricLine constructs a LyricLine with displayStart-aware PlayTime.
// All players should use this to ensure consistent play_time semantics.
func BuildLyricLine(index int, lineTime float32, text, subText string, detailed LyricTextDetailed, offsetSec float32) LyricLine {
	ds := LyricDisplayStart(lineTime, detailed)
	return LyricLine{
		Index:        index,
		Timestamp:    lineTime,
		PlayTime:     AdjustLyricPlayTime(ds, offsetSec),
		Text:         text,
		SubText:      subText,
		TextDetailed: detailed,
	}
}

// ── 文本归一化工具（跨播放器共享） ──

// NormalizeLyricText strips all non-letter/digit characters and lowercases,
// producing a canonical form for comparing lyric texts across sources
// (LRC vs YRC, different punctuation conventions like , vs ' vs !).
func NormalizeLyricText(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// SameLyricText returns true if two lyric texts match after normalization.
// It also tolerates small middle insertions/deletions when both normalized texts
// share reliable beginning and ending boundaries, which helps align line-level
// lyrics with word-level lyrics from different sources.
func SameLyricText(left, right string) bool {
	left = NormalizeLyricText(left)
	right = NormalizeLyricText(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	if lyricTextOneEditMatch(left, right) {
		return true
	}
	return lyricTextBoundaryMatch(left, right)
}

func lyricTextOneEditMatch(left, right string) bool {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	minLen := len(leftRunes)
	if len(rightRunes) < minLen {
		minLen = len(rightRunes)
	}
	if minLen < 8 {
		return false
	}

	lengthDiff := len(leftRunes) - len(rightRunes)
	if lengthDiff < 0 {
		lengthDiff = -lengthDiff
	}
	if lengthDiff > 1 {
		return false
	}

	leftIndex, rightIndex := 0, 0
	edits := 0
	for leftIndex < len(leftRunes) && rightIndex < len(rightRunes) {
		if leftRunes[leftIndex] == rightRunes[rightIndex] {
			leftIndex++
			rightIndex++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		switch {
		case len(leftRunes) > len(rightRunes):
			leftIndex++
		case len(rightRunes) > len(leftRunes):
			rightIndex++
		default:
			leftIndex++
			rightIndex++
		}
	}
	if leftIndex < len(leftRunes) || rightIndex < len(rightRunes) {
		edits++
	}
	return edits <= 1
}

func lyricTextBoundaryMatch(left, right string) bool {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	minLen := len(leftRunes)
	if len(rightRunes) < minLen {
		minLen = len(rightRunes)
	}
	if minLen < 8 {
		return false
	}

	prefixLen := commonPrefixLength(leftRunes, rightRunes)
	suffixLen := commonSuffixLength(leftRunes[prefixLen:], rightRunes[prefixLen:])
	minEdgeLen := minLyricBoundaryEdgeLength(minLen)
	if prefixLen < minEdgeLen || suffixLen < minEdgeLen {
		return false
	}

	leftGap := len(leftRunes) - prefixLen - suffixLen
	rightGap := len(rightRunes) - prefixLen - suffixLen
	if leftGap < 0 || rightGap < 0 {
		return false
	}
	if leftGap == 0 && rightGap == 0 {
		return true
	}
	maxGap := minLen / 5
	if maxGap < 4 {
		maxGap = 4
	}
	if leftGap == 0 {
		return rightGap <= maxGap
	}
	if rightGap == 0 {
		return leftGap <= maxGap
	}
	return leftGap <= 1 && rightGap <= 1
}

func minLyricBoundaryEdgeLength(length int) int {
	edgeLen := length / 4
	if edgeLen < 4 {
		return 4
	}
	if edgeLen > 8 {
		return 8
	}
	return edgeLen
}

func commonPrefixLength(left, right []rune) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[i] != right[i] {
			return i
		}
	}
	return limit
}

func commonSuffixLength(left, right []rune) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[len(left)-1-i] != right[len(right)-1-i] {
			return i
		}
	}
	return limit
}

// NormalizeSongName strips punctuation and extra spaces for fuzzy song name matching.
// Less aggressive than NormalizeLyricText: preserves spaces between words for readability,
// but removes punctuation that varies across sources (', !, -, etc).
func NormalizeSongName(value string) string {
	var b strings.Builder
	lastSpace := true // suppress leading space
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			lastSpace = false
		} else if unicode.IsSpace(r) && !lastSpace {
			b.WriteRune(' ')
			lastSpace = true
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// SameSongName returns true if two song names match after normalization.
func SameSongName(left, right string) bool {
	return NormalizeSongName(left) == NormalizeSongName(right)
}
