package player

// LyricLine 歌词行
type LyricLine struct {
	Index int     `json:"index"`
	Time  float32 `json:"time"`
	Text  string  `json:"text"`
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
	LineIndex int     `json:"line_index"`
	Text      string  `json:"text"`
	SubText   string  `json:"sub_text"`
	Timestamp float32 `json:"timestamp"`
	PlayTime  float32 `json:"play_time"`
	Progress  float32 `json:"progress"`
}

// PlaybackTimeInfo 播放暂停/恢复事件载荷（仅 play_time）
type PlaybackTimeInfo struct {
	PlayTime float32 `json:"play_time"`
}

// AllLyricsData 完整歌词
type AllLyricsData struct {
	SongTitle string      `json:"song_title,omitempty"`
	Duration  float32     `json:"duration"`
	PlayTime  float32     `json:"play_time"`
	Lyrics    []LyricLine `json:"lyrics"`
	Count     int         `json:"count"`
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
