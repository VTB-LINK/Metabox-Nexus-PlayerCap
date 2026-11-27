package assoc

import (
	"context"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

// MemTestSnapshot 是跨包会话集成测试向真实内存模式 *Reader 注入的酷狗内存状态快照，字段与内部 memSnapshot 对齐。
type MemTestSnapshot struct {
	SongID             uint32
	ListIdx            uint32
	ListPtr            uintptr
	IsMV               bool
	CurrentSongHash    string
	CLyricPtr          uintptr
	TaskID             uint32
	KrcID              uint32
	NoLyricFound       bool
	IsUserUnassociated bool
	AssocPath          string
	LyricOffsetMs      int
	VecBegin           uintptr
	VecEnd             uintptr
	LyricFilePath      string
}

// MemTestPlayingRow 是跨包会话集成测试注入的播放队列行（P）读数。
type MemTestPlayingRow struct {
	PlayedAt int64
	Path     string
}

// MemTestConfig 定义启动测试用内存模式 *Reader 所需的纯内存桩依赖。
type MemTestConfig struct {
	Version    string
	DataDir    string
	LyricDir   string
	PID        uint32
	Snapshot   func() MemTestSnapshot
	PlayingRow func(hash string) (MemTestPlayingRow, error)
	ReadFile   func(path string) ([]byte, error)
	StatDB     func() (int64, time.Time, error)
}

type funcMemSource struct {
	pid uint32
	fn  func() MemTestSnapshot
}

func (s *funcMemSource) ReadSnapshot() (memSnapshot, memStatus) {
	if s.fn == nil {
		return memSnapshot{}, memStatusNotReady
	}
	ms := s.fn()
	return memSnapshot{
		Handle: songHandle{
			SongID:  ms.SongID,
			ListIdx: ms.ListIdx,
			ListPtr: ms.ListPtr,
		},
		IsMV:               ms.IsMV,
		CurrentSongHash:    ms.CurrentSongHash,
		CLyricPtr:          ms.CLyricPtr,
		TaskID:             ms.TaskID,
		KrcID:              ms.KrcID,
		NoLyricFound:       ms.NoLyricFound,
		IsUserUnassociated: ms.IsUserUnassociated,
		AssocPath:          ms.AssocPath,
		LyricOffsetMs:      ms.LyricOffsetMs,
		VecBegin:           ms.VecBegin,
		VecEnd:             ms.VecEnd,
		LyricFilePath:      ms.LyricFilePath,
	}, memStatusOK
}

func (s *funcMemSource) PID() uint32 { return s.pid }
func (s *funcMemSource) Close()      {}

// StartMemoryForTest 使用调用方提供的纯内存回调直接经 start(ctx, d) 启动真实内存模式 *Reader，供跨包会话集成测试复用。
func StartMemoryForTest(ctx context.Context, cfg MemTestConfig) *Reader {
	pid := cfg.PID
	if pid == 0 {
		pid = 1000
	}
	version := cfg.Version
	if version == "" {
		version = "20.1.22.27795"
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = `C:\KuGouData`
	}
	lyricDir := cfg.LyricDir
	if lyricDir == "" {
		lyricDir = `C:\KuGouLyric`
	}
	d := deps{
		confirm: func() (watchdog.Confirmed, error) {
			return watchdog.Confirmed{Version: version, DataDir: dataDir, LyricDir: lyricDir, PID: pid}, nil
		},
		openMem: func(p uint32) (memSource, memOpenKind, string, error) {
			return &funcMemSource{pid: p, fn: cfg.Snapshot}, memOpenOK, "test-fp", nil
		},
		readFile: cfg.ReadFile,
	}
	if cfg.PlayingRow != nil {
		d.playingRow = func(_ context.Context, _ string, hash string) (playing, error) {
			row, err := cfg.PlayingRow(hash)
			return playing{played: row.PlayedAt, path: row.Path}, err
		}
	}
	if cfg.StatDB != nil {
		d.stat = func(string) (int64, time.Time, error) {
			return cfg.StatDB()
		}
	}
	return start(ctx, d)
}
