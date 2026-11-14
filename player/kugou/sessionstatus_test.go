package kugou

// 本文件经按脚本返回读数的桩跑 runSession，只守会话（一次 CDP 连接）开头的状态（#181）：第一首的播放态
// 既非播放也非暂停时报 paused（不另发 playback_pause），连上时没有歌报一次 waiting_song；会话中途换歌的
// 过渡态、中途读到没有歌，仍不发状态。不碰酷狗、不碰 CDP：客户端换成 scriptedSource，取词换成返回无歌词
// 的桩。
//
// 会话在 testing/synctest 的泡里跑（runScript）：runSession 为每首歌起的封面 goroutine 退出时只被取消、
// 不被等待，留在泡外的话会在后续用例里读包级的 fetchCoverBase64，与 TestRunCoverFetchRespectsCancel 对它
// 的替换竞争（-race -shuffle=on -count=5 下实测报 DATA RACE）。synctest.Test 等泡内的 goroutine 全部退出
// 才返回。泡内不联网，读数的封面 URL 为空：封面 goroutine 停在等 URL 的 select 上，泡内的假时钟在会话
// 跑完之前不走，它只会由会话结束时的取消唤醒退出，不会走到下载或 API 兜底。
//
// 脚本按 runSession 读取的次数写：换歌的那一轮读两次（取词之后重读一次），其余每轮读一次；读完即返回
// 错误，会话随之结束。
//
// 变异自证（本 worktree 里逐条注入、跑完即还原，均实测）记在各用例的注释里。

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

type scriptedSource struct{ reads []*cdp.PlayInfo }

func (s *scriptedSource) GetPlayInfo() (*cdp.PlayInfo, error) {
	if len(s.reads) == 0 {
		return nil, errors.New("script exhausted")
	}
	r := s.reads[0]
	s.reads = s.reads[1:]
	return r, nil // nil 即「没有歌」，同 cdp.Client.GetPlayInfo
}

func (s *scriptedSource) IsClosed() bool { return false }

// song 是 hash 这首歌某一刻的读数，标题为 sessionTitle(hash)。封面 URL 为空，理由见文件头。
func song(hash, playStatus string) *cdp.PlayInfo {
	return &cdp.PlayInfo{
		Filename: "歌手 - 歌" + hash, PlayStatus: playStatus,
		Progress: "0", Duration: "200000", Hash: hash,
	}
}

func sessionTitle(hash string) string { return "歌手 - 歌" + hash }

func playingOf(hash string) string { return "playing|" + sessionTitle(hash) + "|" }
func pausedOf(hash string) string  { return "paused|" + sessionTitle(hash) + "|" }

var waitingSong = "waiting_song|" + i18n.T("KuGou has no current song") + "|" + player.ReasonNoSong

// runScript 以 reads 跑一次会话，返回其间发出的状态类事件（见 drainStatus）。会话与它起的封面 goroutine
// 都在 synctest 的泡里，返回时它们已全部退出。
func runScript(t *testing.T, p *KuGouPlayer, reads ...*cdp.PlayInfo) []string {
	t.Helper()
	orig := fetchLyrics
	fetchLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	defer func() { fetchLyrics = orig }()

	synctest.Test(t, func(*testing.T) { p.runSession(&scriptedSource{reads: reads}) })
	return drainStatus(p)
}

// drainStatus 取走通道里已有的事件，按先后返回状态类事件：status_update 记为「状态|detail|reason」，
// playback_pause / playback_resume 记事件名。其余事件（歌词、歌曲信息、位置采样）不记。
func drainStatus(p *KuGouPlayer) []string {
	var got []string
	for {
		select {
		case evt := <-p.EventCh:
			switch evt.Type {
			case player.EventStatusUpdate:
				st := evt.Data.(*player.StatusInfo)
				got = append(got, st.Status+"|"+st.Detail+"|"+st.Reason)
			case player.EventPlaybackPause, player.EventPlaybackResume:
				got = append(got, evt.Type)
			}
		default:
			return got
		}
	}
}

func checkEvents(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("状态类事件\n got  %q\n want %q", got, want)
	}
}

// 第一首的播放态既非播放也非暂停（已加载、未播放）：发一条 paused，detail 为标题，不另发 playback_pause；
// 之后开始播放照常发 playback_resume 与 playing。判据按 statusStr 的归类，三种原始取值同样处理。
//
// 变异自证：删掉换歌分支里 `if s == "standby" && firstSong { s = "paused" }`，本例各子例红；firstSong 改在
// `lastHash = info.Hash` 之后求值（恒为假），同样红。
func TestFirstSongNotPlayingReportsPaused(t *testing.T) {
	for _, raw := range []string{"stopped", "", "loading"} {
		t.Run("原始播放态="+raw, func(t *testing.T) {
			p := New(0, 0)
			got := runScript(t, p,
				song("A", raw), song("A", raw), // 换歌一轮 + 取词后重读
				song("A", raw),
				song("A", "playing"),
			)
			checkEvents(t, got, []string{pausedOf("A"), player.EventPlaybackResume, playingOf("A")})
		})
	}
}

// 反例：第一首在播放或暂停时与修复前相同——按当时的播放态直接发一条状态，暂停也不另发 playback_pause，
// 不出现 waiting_song。别名（play、pasued）按 statusStr 归类。
func TestFirstSongPlayingOrPausedUnchanged(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"playing", playingOf("A")},
		{"play", playingOf("A")},
		{"paused", pausedOf("A")},
		{"pasued", pausedOf("A")},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			p := New(0, 0)
			got := runScript(t, p, song("A", c.raw), song("A", c.raw), song("A", c.raw))
			checkEvents(t, got, []string{c.want})
		})
	}
}

// 反例：会话中途换歌，换歌那一拍读到的过渡态不发状态（不得发 paused），等读到播放再发；第一首无论在播、
// 未播放还是暂停，第二首起都不按第一首处理。
//
// 变异自证：firstSong 恒为真（不看 lastHash），三个子例都多出第二首的 paused 而红。
func TestMidSessionTransientStaysSilent(t *testing.T) {
	cases := []struct {
		name  string
		reads []*cdp.PlayInfo
		want  []string
	}{
		{"上一首在播：过渡态不发，起播后发 playing",
			[]*cdp.PlayInfo{song("A", "playing"), song("A", "playing"), song("B", "stopped"), song("B", "stopped"), song("B", "playing")},
			[]string{playingOf("A"), player.EventPlaybackResume, playingOf("B")}},
		{"第一首未播放、第二首的过渡态",
			[]*cdp.PlayInfo{song("A", "stopped"), song("A", "stopped"), song("B", "stopped"), song("B", "stopped")},
			[]string{pausedOf("A")}},
		{"第一首暂停、第二首读到空播放态",
			[]*cdp.PlayInfo{song("A", "paused"), song("A", "paused"), song("B", ""), song("B", "")},
			[]string{pausedOf("A")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 0)
			checkEvents(t, runScript(t, p, c.reads...), c.want)
		})
	}
}

// 连上时没有歌：发一条 waiting_song（reason 为 no_song），持续无歌不重发（nil 与空 hash 两种读数都算
// 没有歌）；随后加载的歌按第一首处理。
//
// 变异自证：删掉无歌分支里的 EmitStatusIfChanged，两个子例都缺 waiting_song 而红；改成 p.Emit 直接发，
// 两个子例都多出重复的 waiting_song 而红。
func TestNoSongAtConnectReportsWaitingSongOnce(t *testing.T) {
	t.Run("随后加载的歌未播放", func(t *testing.T) {
		p := New(0, 0)
		got := runScript(t, p, nil, &cdp.PlayInfo{}, nil, song("A", "stopped"), song("A", "stopped"))
		checkEvents(t, got, []string{waitingSong, pausedOf("A")})
	})
	t.Run("随后加载的歌在播放", func(t *testing.T) {
		p := New(0, 0)
		got := runScript(t, p, nil, nil, song("A", "playing"), song("A", "playing"))
		checkEvents(t, got, []string{waitingSong, playingOf("A")})
	})
}

// 反例：会话中途（读到过歌之后）读到没有歌，不发 waiting_song，也不改动已发出的状态；同一首回来不算换歌。
//
// 变异自证：去掉无歌分支的 `if lastHash == ""`，两个子例都多出 waiting_song 而红。
func TestNoSongMidSessionStaysSilent(t *testing.T) {
	t.Run("在播中途无歌", func(t *testing.T) {
		p := New(0, 0)
		got := runScript(t, p, song("A", "playing"), song("A", "playing"), nil, &cdp.PlayInfo{}, song("A", "playing"))
		checkEvents(t, got, []string{playingOf("A")})
	})
	t.Run("第一首未播放后无歌", func(t *testing.T) {
		p := New(0, 0)
		got := runScript(t, p, song("A", "stopped"), song("A", "stopped"), nil, nil)
		checkEvents(t, got, []string{pausedOf("A")})
	})
}

// 断开重连后的新会话从头判定：同一个播放器、同一首歌（hash 相同），新会话里仍按第一首处理；无歌也重新报
// waiting_song。两次会话之间照 Start 的写法发出断开与等待重连两条状态。
//
// 变异自证：删掉第一首按 paused 发的分支、或删掉无歌分支的 EmitStatusIfChanged，第二次会话红。
func TestReconnectedSessionStartsOver(t *testing.T) {
	p := New(0, 0)
	checkEvents(t, runScript(t, p, nil, song("A", "playing"), song("A", "playing")),
		[]string{waitingSong, playingOf("A")})

	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby", Detail: i18n.T("KuGou CDP disconnected"), Reason: player.ReasonCDPDisconnected})
	p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Detail: i18n.T("KuGou not running or CDP not ready"), Reason: player.ReasonProcessNotRunning})
	drainStatus(p)

	checkEvents(t, runScript(t, p, nil, nil, song("A", "stopped"), song("A", "stopped"), song("A", "playing")),
		[]string{waitingSong, pausedOf("A"), player.EventPlaybackResume, playingOf("A")})
}
