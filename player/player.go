package player

// LyricLine 歌词行
type LyricLine struct {
	Index   int     `json:"index"`
	Timestamp float32 `json:"timestamp"`
	Text    string  `json:"text"`
	SubText string  `json:"sub_text"`
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
	Index     int     `json:"index"`
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
	Title     string      `json:"title,omitempty"`
	Duration  float32     `json:"duration"`
	PlayTime  float32     `json:"play_time"`
	Progress  float32     `json:"progress"`
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
