package player

import (
	"encoding/json"
	"strings"
	"sync/atomic"
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

	// songGen 是「当前是第几首歌」的代次计数，供异步回写做淘汰判定。
	// 用裸 uint64 + sync/atomic 包函数而非 atomic.Uint64/sync.Mutex：后两者含 noCopy，
	// 而 NewBaseEmitter 是值返回、会被各播放器按值嵌入，vet 的 copylocks 会报。
	songGen uint64
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

// NewSongGen 标记「换歌了」：递增代次并返回新代次。**只应由播放器的轮询循环在检测到
// 换歌时调用**（单写者）。
//
// 它存在的理由：封面等数据靠 goroutine 异步取，取完回来才补发 song_info_update。而
// song_info_update 每首歌只在换歌分支发一次，轮询循环不会重发——所以一个迟到的回写会
// 把上一首的标题+封面盖在当前歌曲上，**整首歌都不自愈**。异步回写者应在 spawn 时捕获
// 本代次，回写时走 EmitForGen。
//
// go vet 对这类缺陷结构性失明：裸 go func 没有 CancelFunc 可丢，lostcancel 无从下手。
func (b *BaseEmitter) NewSongGen() uint64 {
	return atomic.AddUint64(&b.songGen, 1)
}

// InvalidateSongGen 作废当前代次，使一切在飞的异步回写在 EmitForGen 处被丢弃。
//
// 用于**会话结束**（播放器进程退出）：此时最后一首歌的封面 goroutine 可能仍在下载
// （cloudmusic/qqmusic 5s、wesing 最长约 10s）。它回来时若无人作废，会在 Start() 已
// Emit(EventClearSongData) 之后把已清空的歌曲信息重新贴回缓存，而 buildInitEvents 会把
// 这份复活数据发给此后每个新连接——OBS 上一直显示已经停掉的那首歌，直到进程重启换歌。
// kugou 靠 ca253cd 的 coverCancel() 在三条早退路径上做对了这半，本方法把它提升到 BaseEmitter。
//
// 与 NewSongGen 共享 songGen、语义不同：NewSongGen 是「换到新歌」（返回新代次给回写者
// 捕获），本方法是「当前歌曲上下文失效」（只作废、无新歌，故不返回）。两者的写入者都是
// 播放器自己的 Start/runSession goroutine（单写者不变），只是调用点不同。
func (b *BaseEmitter) InvalidateSongGen() {
	atomic.AddUint64(&b.songGen, 1)
}

// EmitForGen 仅当 gen 仍是当前代次时才 Emit，返回是否真的发出。异步回写的标准出口。
//
// 残留竞态：load 与 send 之间若恰好换歌，仍可能发出过期事件。该窗口是纳秒级，而它要防的
// 是秒级的下载窗口（5s，wesing 最长约 10s），二者差约 8 个数量级；消除它需要在 Emit 热
// 路径上加锁，不划算。故这里是「把窗口压到可忽略」，不是「消除」。
func (b *BaseEmitter) EmitForGen(gen uint64, evtType string, data interface{}) bool {
	if atomic.LoadUint64(&b.songGen) != gen {
		return false
	}
	b.Emit(evtType, data)
	return true
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

// ClampProgress 返回 playTime/duration 的播放进度，钳在 [0,1]；duration 非正时返回 0。
//
// 四个播放器共用。原先 cloudmusic 与 kugou 各存一份逐字相同的私有副本，wesing 与 qqmusic
// 则把它手写展开成 `p := float32(0); if d > 0 { p = ClampFloat32(x/d, 0, 1) }`——同一个
// 判据散在四处，改一处就会分叉。
//
// **`duration <= 0` 这道闸不是可选的防御性编程**：duration 未知是常态入口（内存/Redux 尚未
// 读到、纯音乐无时长、切歌瞬间），而 `0/0 = NaN` 会**穿过** ClampFloat32——NaN 的两个比较
// 都为 false，函数把 NaN 原样返回。NaN 进了载荷，WS 的 json.Marshal 就会报错 → 写 goroutine
// 退出但连接不关 → 订阅者僵死却仍在册。这与 wesing 的 IsPlausiblePlayTime 防的是同一个坑
// （AGENTS.md 3.7）。注意 `正数/0 = +Inf` 反而是安全的（+Inf > 1 会被钳成 1），
// 唯独 playTime 与 duration 同时为 0 才漏出 NaN——恰恰是切歌那一瞬间最容易撞上的组合。
func ClampProgress(playTime, duration float32) float32 {
	if duration <= 0 {
		return 0
	}
	return ClampFloat32(playTime/duration, 0, 1)
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

// LyricPlayTime 返回一行歌词的播出时间：`displayStart - offset`，下限 0。
//
// 这是 play_time 的**唯一公式**，四个播放器的 all_lyrics 与 lyric_update 都必须经由它，
// 否则同一行会在两个事件里拿到两个值。它被单拎出来，是因为 lyric_update 只需要这一个
// 数值、不需要整个 LyricLine——而此前 cloudmusic 正是为了取它而构造了一整个 LyricLine，
// 另外三家则干脆绕过公式直接发「当前实时播放位置」，四家四种写法。
//
// 注意它取的是 displayStart 而非 lineTime：逐字时间轴早于行时间戳时（网易云 YRC 的长引子
// 行，如 "A-A-All"：YRC 首字 10.92s 而 LRC 行标 14.58s），本行按第一个字发声的时刻上屏，
// 故 play_time 那时**不等于** `timestamp - offset`。这是刻意的，见 LyricDisplayStart。
func LyricPlayTime(lineTime float32, detailed LyricTextDetailed, offsetSec float32) float32 {
	return AdjustLyricPlayTime(LyricDisplayStart(lineTime, detailed), offsetSec)
}

// pureMusicHints 各平台对「这首歌没有歌词」返回的提示语。
//
// 网易云与酷狗**不返回空歌词**，而是返回一行「纯音乐，请欣赏」——两家一字不差。
// QQ 音乐的 API 则直接返回零行。同一个语义、三种表达，若原样透传给下游，
// `index: -1` 这个「无歌词」信号就只有 QQ 音乐会发，下游必须逐平台特判。
//
// 认定后仍**原样保留 text**：那七个字是平台的数据，我们只是搬运工；下游想显示、
// 想换成 "Instrumental"、想直接忽略，都由它自己决定（lyric_page.html 选择忽略）。
var pureMusicHints = map[string]bool{
	"纯音乐，请欣赏": true,
}

// IsPureMusicOnly 判断这份歌词是不是「平台其实没有歌词，只给了一句提示语」。
//
// **len(lyrics) == 1 那道闸是承重的**：没有它，这就成了纯关键词匹配——某首歌真的唱
// 「纯音乐，请欣赏」时会被整首吞掉。加上它之后，误判需要「整首歌只有一行**且**那行
// 恰好是这七个字」，可以忽略。
//
// 平台若改文案，这里会失配 → 退回「按普通歌词处理」，即本函数引入前的行为。
// 那是降级不是故障：下游看到的是一行歌词而非 index:-1，与今天的现状相同。
func IsPureMusicOnly(lyrics []LyricLine) bool {
	return len(lyrics) == 1 && pureMusicHints[lyrics[0].Text]
}

// BuildLyricUpdate 构造 lyric_update 载荷。**四个播放器都必须经由它**。
//
// 它存在的唯一理由是把这一对易错的语义锁在一处：
//
//	PlayTime —— 本行的**播出时间**（displayStart - offset），与 all_lyrics 里同一行逐字节相同
//	Progress —— **整首播到哪**（实时播放位置 / 总时长），与 play_time 无关
//
// 两者一个来自歌词时间轴、一个来自实时时钟，恰好在常规行上只差一个轮询滞后（毫秒级），
// 所以写反了不会有任何症状——直到某行的逐字时间轴早于行时间戳时才会突然错开数秒。
// 四家原先各写各的（cloudmusic 走公式，qqmusic/wesing/kugou 直接把实时位置塞进 play_time），
// 正是这种「看不出差别」的分叉。
//
// playPos 传当前实时播放位置（各家的内存直读值或插值时钟），只用于算 Progress。
func BuildLyricUpdate(index int, lineTime float32, text, subText string, detailed LyricTextDetailed, offsetSec, playPos, duration float32) *LyricUpdate {
	return &LyricUpdate{
		Index:        index,
		Text:         text,
		SubText:      subText,
		Timestamp:    lineTime,
		PlayTime:     LyricPlayTime(lineTime, detailed, offsetSec),
		Progress:     ClampProgress(playPos, duration),
		TextDetailed: detailed,
	}
}

// BuildLyricLine constructs a LyricLine with displayStart-aware PlayTime.
// All players should use this to ensure consistent play_time semantics.
func BuildLyricLine(index int, lineTime float32, text, subText string, detailed LyricTextDetailed, offsetSec float32) LyricLine {
	return LyricLine{
		Index:        index,
		Timestamp:    lineTime,
		PlayTime:     LyricPlayTime(lineTime, detailed, offsetSec),
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
