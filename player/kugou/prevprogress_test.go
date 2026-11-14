package kugou

// 本文件只测换歌过渡态读数里上一首的进度不落锚（#195，doc/design/kugou-double-announce-design.md §4 方案 A-2）：
// 换歌拍或取词后的重读读到的播放态既非播放也非暂停时，进度与可信基准吻合即判为上一首的，锚点取 0；
// 基准 (a) 是最近一首已确认（读到过播放或暂停；会话第一首在换歌时即算）的歌的时钟，(b) 是自 (a) 设定以来
// 最近一次直接与 (a) 吻合的读数。读到播放或暂停的读数不比对。判为上一首的读数不参与本轮的进度更新与跳转检测；
// 换歌之后各拍的过渡态读数只在过渡期内（当前歌尚未确认）同样判，且只读、不改写 (b)；换歌拍读到播放而重读判中时，
// 撤销确认、过渡期延续。
//
// 夹具同 sessionstatus_test.go：scriptedSource 按脚本返回读数，会话在 synctest 的泡里跑，取词换成返回无歌词的
// 桩。轮询间隔为 0 时泡内时间不走，上一首的推算位置即它最后一次落锚的进度；轮询间隔为 1 s 时每轮推进 1 s。
// 读数不带封面 URL，假时钟走过 coverTotalBudget 时封面协程会调 API 兜底，故把 kugouAPIClient 换成返回空响应
// 的桩，泡内不联网。
//
// 只记带位置的对外事件：all_lyrics 记标题与 position，playback_resume / playback_pause 记 position。
//
// 变异自证（2026-11-14，隔离克隆里逐条注入 kugou.go，实测；括号里是变红的用例，子例写作 用例/子例）：
//   - 去掉新加的判断、回到现行：PrevProgressAnnouncesZero 两子例、ChainKeepsConfirmedBase、FrozenPrevProgress、
//     ChainDoesNotDrift、FrozenMatchDoesNotRebase、BaseSkipsUnconfirmed、BaseFollowsConfirmation 三子例、
//     FirstSong/第一首算已确认、PrevProgressSkipsSeek 前两子例、Tolerances 的 (a) 0.9 与 (b) 两例；
//   - 基准不看确认、每次换歌都取上一首的锚点（设计第 1 版）：ChainKeepsConfirmedBase、FrozenPrevProgress、
//     ChainDoesNotDrift、FrozenMatchDoesNotRebase、BaseSkipsUnconfirmed、FirstSong/0.5、Tolerances/(b) 0.005；
//   - 去掉基准 (b)：FrozenPrevProgress、FrozenMatchDoesNotRebase、Tolerances/(b) 0.005；
//   - (b) 的容差放宽到 1 s：ChainDoesNotDrift、FrozenMatchDoesNotRebase、LaterReadsKeepFrozen、Tolerances/(b) 0.02；
//   - 只与 (b) 吻合时也改写 (b)（设计第 2 版）：FrozenMatchDoesNotRebase；
//   - 状态变化检测不记确认：BaseFollowsConfirmation 的下一轮确认、下一轮确认后不再与 Z 比对、重读确认；
//   - 换歌拍读到播放或暂停不记确认：BaseFollowsConfirmation/换歌拍确认、FilterEndsOnConfirmation；
//   - 会话第一首不算已确认：FirstSong/第一首算已确认；
//   - 退化成过渡态一律置 0（设计的 A-1）：ChainDoesNotDrift、FrozenMatchDoesNotRebase、BaseSkipsUnconfirmed、
//     BaseFollowsConfirmation 前两子例、OwnProgressKept、PrevProgressSkipsSeek/过渡期内的过渡态读数不是上一首的，
//     照旧判前跳、LaterReadsKeepFrozen、Tolerances 的 (a) 1.1 与 (b) 0.02；
//   - 没有基准时按零时钟比对：FirstSong/0.5；
//   - 重读的过渡态判据改看取词前的状态：OnlyTransientReadsCompared/重读读到播放、进度与上一首相同、
//     PrevProgressSkipsSeek 前两子例；
//   - 播放、暂停读数也比对（换歌拍、重读与之后各拍三处）：OnlyTransientReadsCompared 的上一条与「换歌拍读到暂停、
//     进度与上一首相同」；
//   - (a) 的容差比较写反：PrevProgressAnnouncesZero 两子例、ChainKeepsConfirmedBase、FrozenPrevProgress、
//     ChainDoesNotDrift、FrozenMatchDoesNotRebase、BaseSkipsUnconfirmed、BaseFollowsConfirmation 四子例、
//     OwnProgressKept、FirstSong/第一首算已确认、PrevProgressSkipsSeek 前三子例、LaterReadsKeepFrozen、Tolerances
//     四例（不红的是 OnlyTransientReadsCompared 各子例、FirstSong 的两个单首子例、FilterEndsOnConfirmation 与
//     PrevProgressSkipsSeek/过渡态读数不是上一首的，照旧判前跳——它们的读数不经比对或比对结果不影响期望）；
//   - (b) 的容差比较写反：FrozenPrevProgress、ChainDoesNotDrift、FrozenMatchDoesNotRebase、PrevProgressSkipsSeek/
//     过渡期内的过渡态读数不是上一首的，照旧判前跳、LaterReadsKeepFrozen、Tolerances 的 (b) 两例；
//   - 跳转检测不看 prevReading（代码审第 1 轮指出的缺陷）：PrevProgressSkipsSeek 前三子例；
//   - 重读的判定只用于落锚、不传给后续：PrevProgressSkipsSeek 前三子例；
//   - 换歌之后的各拍不判：PrevProgressSkipsSeek/之后各拍仍读到上一首的进度；
//   - 过渡期内的过渡态读数一律不参与跳转检测：PrevProgressSkipsSeek/过渡期内的过渡态读数不是上一首的，照旧判前跳；
//   - 去掉「当前歌尚未确认」的限制，确认之后各拍仍判（代码审第 2 轮）：FilterEndsOnConfirmation；
//   - 之后各拍改回会改写 (b) 的 isPrev（代码审第 2 轮）：LaterReadsKeepFrozen；
//   - looksPrev 不恢复 (b)：LaterReadsKeepFrozen；
//   - 重读判中时不撤销换歌拍记下的确认：PrevProgressSkipsSeek/之后各拍仍读到上一首的进度。
// 实测时点：「回到现行」「重读改看取词前的状态」「播放、暂停读数也比对」三条在加入 prevReading 之后（ce19010 的
// 代码）实测，限定过渡期、改为只读之后未重跑；最后六条在限定过渡期、改为只读之后（62136c0 的代码）实测；其余各条
// （基准不看确认至没有基准时按零时钟比对、两个容差写反、跳转检测不看 prevReading、重读的判定只用于落锚）在 1f360cb
// 的代码上逐条重跑，跑的是本文件全部用例，所列即全部变红的用例。

import (
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// read 是 hash 这首歌某一刻的读数：播放态 status、进度 sec 秒、时长 durSec 秒。标题同 sessionTitle(hash)。
func read(hash, status string, sec, durSec float64) *cdp.PlayInfo {
	return &cdp.PlayInfo{
		Filename: "歌手 - 歌" + hash, PlayStatus: status, Hash: hash,
		Progress: strconv.FormatFloat(sec*1e7, 'f', 0, 64),
		Duration: strconv.FormatFloat(durSec*1000, 'f', 0, 64),
	}
}

// twice 是换歌那一轮的两次读数：换歌拍与取词后的重读读到同一个值。
func twice(r *cdp.PlayInfo) []*cdp.PlayInfo { return []*cdp.PlayInfo{r, r} }

// once 是一轮的一次读数。
func once(r *cdp.PlayInfo) []*cdp.PlayInfo { return []*cdp.PlayInfo{r} }

// posEvt 是一条带位置的对外事件。kind 为 all_lyrics 时 title 是它的标题，其余为空。
type posEvt struct {
	kind, title string
	pos         float32
}

func lyrics(hash string, pos float32) posEvt {
	return posEvt{player.EventAllLyrics, sessionTitle(hash), pos}
}
func resumed(pos float32) posEvt { return posEvt{player.EventPlaybackResume, "", pos} }

// runPositions 以 rounds 跑一次会话（每个元素是一轮的读数），返回其间发出的带位置的对外事件。
func runPositions(t *testing.T, pollMs int, rounds ...[]*cdp.PlayInfo) []posEvt {
	t.Helper()
	var reads []*cdp.PlayInfo
	for _, r := range rounds {
		reads = append(reads, r...)
	}
	origLyrics, origAPI := fetchLyrics, kugouAPIClient
	fetchLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	kugouAPIClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
	})}
	defer func() { fetchLyrics, kugouAPIClient = origLyrics, origAPI }()

	p := New(0, pollMs)
	synctest.Test(t, func(*testing.T) { p.runSession(&scriptedSource{reads: reads}) })

	var got []posEvt
	for {
		select {
		case evt := <-p.EventCh:
			switch d := evt.Data.(type) {
			case *player.AllLyricsData:
				got = append(got, posEvt{evt.Type, d.Title, d.Position})
			case *player.PlaybackTimeInfo:
				if evt.Type != player.EventPositionSample {
					got = append(got, posEvt{evt.Type, "", d.Position})
				}
			}
		default:
			return got
		}
	}
}

func checkPositions(t *testing.T, got, want []posEvt) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(got); i++ {
		ok = got[i].kind == want[i].kind && got[i].title == want[i].title && math.Abs(float64(got[i].pos-want[i].pos)) < 1e-3
	}
	if !ok {
		t.Fatalf("带位置的事件\n got  %+v\n want %+v", got, want)
	}
}

// 换歌拍与重读都是过渡态、进度与上一首当时的位置吻合（#195 的浮游天地第一次宣告）：宣告的位置为 0，不是上一首的
// 1.5 s；随后读到播放，按真实进度发 playback_resume。上一首暂停时同样认得出。
func TestTransientPrevProgressAnnouncesZero(t *testing.T) {
	t.Run("上一首在播", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "playing", 1.5, 200)),
			twice(read("B", "stopped", 1.5, 300)),
			once(read("B", "playing", 0.2, 300)),
		), []posEvt{lyrics("A", 1.5), lyrics("B", 0), resumed(0.2)})
	})
	t.Run("上一首暂停中切歌", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			twice(read("B", "stopped", 40, 300)),
		), []posEvt{lyrics("A", 40), lyrics("B", 0)})
	})
}

// 连续两次未起播的换歌（设计第 2 轮的反例）：B1 未确认，B2 换歌时基准仍是 A 的时钟，不是 B1 被置 0 的锚点，
// 两次宣告的位置都是 0。
func TestTransientChainKeepsConfirmedBase(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("A", "playing", 1.5, 200)),
		twice(read("B1", "stopped", 1.5, 300)),
		twice(read("B2", "stopped", 1.5, 300)),
		once(read("B2", "playing", 0, 300)),
	), []posEvt{lyrics("A", 1.5), lyrics("B1", 0), lyrics("B2", 0), resumed(0)})
}

// 酷狗把上一首的进度停在切歌那一刻（基准 b）：A 在播，1 s 后 B1 读到 2.4（与 A 推算的 2.5 吻合）；此后读数停在
// 2.4，B2 换歌时 A 已推算到 5.5，只有 (b) 认得出。
func TestTransientFrozenPrevProgress(t *testing.T) {
	checkPositions(t, runPositions(t, 1000,
		twice(read("A", "playing", 1.5, 200)),
		twice(read("B1", "stopped", 2.4, 300)),
		once(read("B1", "stopped", 2.4, 300)),
		once(read("B1", "stopped", 2.4, 300)),
		twice(read("B2", "stopped", 2.4, 300)),
	), []posEvt{lyrics("A", 1.5), lyrics("B1", 0), lyrics("B2", 0)})
}

// 反例（设计第 3 轮）：连续未确认的换歌，合法起播位置逐首递增 0.8 s。只有离 A 不到 1 s 的 B1 被置 0（设计写明的
// 残余），B2、B3 不因与 (b) 链式吻合而归零。
func TestTransientChainDoesNotDrift(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("A", "paused", 50, 300)),
		twice(read("B1", "stopped", 50.8, 300)),
		twice(read("B2", "stopped", 51.6, 300)),
		twice(read("B3", "stopped", 52.4, 300)),
	), []posEvt{lyrics("A", 50), lyrics("B1", 0), lyrics("B2", 51.6), lyrics("B3", 52.4)})
}

// 反例：只与 (b) 吻合的读数不改写 (b)。B2 的 2.408 只与 (b) 的 2.4 吻合，置 0，(b) 仍是 2.4；B3 的 2.416 与 2.4
// 相差 0.016 s，超出浮点余量，照旧落锚。
func TestTransientFrozenMatchDoesNotRebase(t *testing.T) {
	checkPositions(t, runPositions(t, 1000,
		twice(read("A", "playing", 1.5, 200)),
		twice(read("B1", "stopped", 2.4, 300)),
		twice(read("B2", "stopped", 2.408, 300)),
		twice(read("B3", "stopped", 2.416, 300)),
	), []posEvt{lyrics("A", 1.5), lyrics("B1", 0), lyrics("B2", 0), lyrics("B3", 2.416)})
}

// 基准只取已确认的歌：未确认的 A（过渡态宣告、位置 84.57 不与 Z 吻合、照旧落锚）之后，B 仍与 Z 的时钟比对。
func TestTransientBaseSkipsUnconfirmed(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("Z", "playing", 50, 300)),
		twice(read("A", "stopped", 84.57, 300)),
		twice(read("B", "stopped", 50, 300)),
	), []posEvt{lyrics("Z", 50), lyrics("A", 84.57), lyrics("B", 0)})
}

// 反例：确认之后基准换成当前歌。三种确认时机：下一轮读到播放、取词后的重读读到播放（经同一轮的状态变化检测）、
// 换歌拍即读到播放（状态变化检测不触发，须在换歌分支里记）。
func TestTransientBaseFollowsConfirmation(t *testing.T) {
	t.Run("下一轮确认", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("Z", "playing", 50, 300)),
			twice(read("A", "stopped", 84.57, 300)),
			once(read("A", "playing", 84.57, 300)),
			twice(read("B", "stopped", 84.57, 300)),
		), []posEvt{lyrics("Z", 50), lyrics("A", 84.57), resumed(84.57), lyrics("B", 0)})
	})
	t.Run("下一轮确认后不再与 Z 比对", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("Z", "playing", 50, 300)),
			twice(read("A", "stopped", 84.57, 300)),
			once(read("A", "playing", 84.57, 300)),
			twice(read("B", "stopped", 50, 300)),
		), []posEvt{lyrics("Z", 50), lyrics("A", 84.57), resumed(84.57), lyrics("B", 50)})
	})
	t.Run("重读确认", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("Z", "playing", 50, 300)),
			[]*cdp.PlayInfo{read("A", "stopped", 0, 300), read("A", "playing", 84.57, 300)},
			twice(read("B", "stopped", 84.57, 300)),
		), []posEvt{lyrics("Z", 50), lyrics("A", 84.57), resumed(84.57), lyrics("B", 0)})
	})
	t.Run("换歌拍确认", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("Z", "playing", 50, 300)),
			twice(read("A", "playing", 84.57, 300)),
			twice(read("B", "stopped", 84.57, 300)),
		), []posEvt{lyrics("Z", 50), lyrics("A", 84.57), lyrics("B", 0)})
	})
}

// 反例：从非 0 位置起播的新歌（1.6 的 YELL 形态），过渡态读数不与上一首吻合，照旧落锚。不比对、一律置 0 的写法
// （设计的 A-1）在这里给出 0。
func TestTransientOwnProgressKept(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("A", "playing", 155, 300)),
		twice(read("B", "stopped", 84.57, 300)),
	), []posEvt{lyrics("A", 155), lyrics("B", 84.57)})
}

// 反例：会话第一首。之前没有基准、不比对，过渡态的进度照旧落锚（#181：酷狗恢复上次的歌时停住的位置）；0.5 s
// 一例防「没有基准当作零时钟」。第一首在换歌时即算已确认，下一首过渡态读到同样的进度时判为上一首的。
func TestTransientFirstSong(t *testing.T) {
	for _, sec := range []float64{83, 0.5} {
		t.Run(strconv.FormatFloat(sec, 'f', -1, 64), func(t *testing.T) {
			checkPositions(t, runPositions(t, 0, twice(read("A", "stopped", sec, 300))),
				[]posEvt{lyrics("A", float32(sec))})
		})
	}
	t.Run("第一首算已确认", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "stopped", 83, 300)),
			twice(read("B", "stopped", 83, 300)),
		), []posEvt{lyrics("A", 83), lyrics("B", 0)})
	})
}

// 反例：读到播放或暂停的读数不比对，照旧落锚——哪怕进度恰与上一首相同。
func TestTransientOnlyTransientReadsCompared(t *testing.T) {
	t.Run("重读已起播", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "playing", 1.5, 200)),
			[]*cdp.PlayInfo{read("B", "stopped", 1.5, 300), read("B", "playing", 0.2, 300)},
		), []posEvt{lyrics("A", 1.5), lyrics("B", 0.2), resumed(0.2)})
	})
	t.Run("重读读到播放、进度与上一首相同", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "playing", 1.5, 200)),
			[]*cdp.PlayInfo{read("B", "stopped", 1.5, 300), read("B", "playing", 1.5, 300)},
		), []posEvt{lyrics("A", 1.5), lyrics("B", 1.5), resumed(1.5)})
	})
	t.Run("换歌拍读到暂停、进度与上一首相同", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			twice(read("B", "paused", 40, 300)),
		), []posEvt{lyrics("A", 40), lyrics("B", 40)})
	})
	t.Run("换歌拍在播", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "playing", 1.5, 200)),
			twice(read("B", "playing", 0.09, 300)),
		), []posEvt{lyrics("A", 1.5), lyrics("B", 0.09)})
	})
	t.Run("过渡态且进度不合法", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "playing", 1.5, 200)),
			twice(read("B", "stopped", 400, 300)),
		), []posEvt{lyrics("A", 1.5), lyrics("B", 0)})
	})
}

// 取词期间从播放转为过渡态：换歌拍读到 B 在播、进度 0，重读读到过渡态、进度是上一首的 40 s。判为上一首的读数不得
// 在同一轮、也不得在之后各拍由跳转检测重新落锚——此前 isPlaying 仍为真，40 被当成前跳，紧跟 all_lyrics(0) 发出
// playback_resume(40)（代码审第 1 轮的反例）。之后各拍能接着过滤，是因为重读判中时撤销了换歌拍记下的确认，
// B 仍在过渡期里。反例：过渡态读数不是上一首的进度时，跳转检测照旧。
func TestTransientPrevProgressSkipsSeek(t *testing.T) {
	playingThenPrev := []*cdp.PlayInfo{read("B", "playing", 0, 300), read("B", "stopped", 40, 300)}
	t.Run("同一轮", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			playingThenPrev,
		), []posEvt{lyrics("A", 40), lyrics("B", 0)})
	})
	t.Run("之后各拍仍读到上一首的进度", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			playingThenPrev,
			once(read("B", "stopped", 40, 300)),
			once(read("B", "stopped", 40, 300)),
		), []posEvt{lyrics("A", 40), lyrics("B", 0)})
	})
	t.Run("过渡期内的过渡态读数不是上一首的，照旧判前跳", func(t *testing.T) {
		// 重读判中撤销了确认，B 仍在过渡期里；60 与 A 的 40、与 (b) 都不吻合，照旧进跳转检测。
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			playingThenPrev,
			once(read("B", "stopped", 60, 300)),
		), []posEvt{lyrics("A", 40), lyrics("B", 0), resumed(60)})
	})
	t.Run("过渡态读数不是上一首的，照旧判前跳", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0,
			twice(read("A", "paused", 40, 200)),
			twice(read("B", "playing", 0, 300)),
			once(read("B", "stopped", 60, 300)),
		), []posEvt{lyrics("A", 40), lyrics("B", 0), resumed(60)})
	})
}

// 换歌之后各拍的判定只在过渡期内（当前歌尚未确认）进行（代码审第 2 轮）。反例：B 从 100 s 正常起播、确认之后，
// 用户回拖到 40 s，接口暂时报 B stopped 40——这与 A 暂停的位置相同，但已过了过渡期，必须按真实的回跳发
// playback_resume(40)，不得当成上一首的进度过滤掉。
func TestTransientFilterEndsOnConfirmation(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("A", "paused", 40, 300)),
		twice(read("B", "playing", 100, 300)),
		once(read("B", "playing", 100, 300)),
		once(read("B", "stopped", 40, 300)),
		once(read("B", "stopped", 40, 300)),
	), []posEvt{lyrics("A", 40), lyrics("B", 100), resumed(40)})
}

// 换歌之后各拍的判定是只读的，不改写 (b)（代码审第 2 轮）。B1 的换歌拍把 (b) 设为 50.8；之后一拍 50.995 与 (a)
// 吻合、被过滤，但不得把 (b) 改成 50.995，否则未确认换歌到 B2 时，B2 合法的起播位置 51.001 会因离 (b) 不到
// 0.01 s 被置 0。
func TestTransientLaterReadsKeepFrozen(t *testing.T) {
	checkPositions(t, runPositions(t, 0,
		twice(read("A", "paused", 50, 300)),
		twice(read("B1", "stopped", 50.8, 300)),
		once(read("B1", "stopped", 50.995, 300)),
		twice(read("B2", "stopped", 51.001, 300)),
	), []posEvt{lyrics("A", 50), lyrics("B1", 0), lyrics("B2", 51.001)})
}

// 边界：与 (a) 相差 0.9 s / 1.1 s；(a) 已远离时与 (b) 相差 0.005 s / 0.02 s。
func TestTransientTolerances(t *testing.T) {
	t.Run("(a) 0.9", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0, twice(read("A", "paused", 50, 300)), twice(read("B", "stopped", 50.9, 300))),
			[]posEvt{lyrics("A", 50), lyrics("B", 0)})
	})
	t.Run("(a) 1.1", func(t *testing.T) {
		checkPositions(t, runPositions(t, 0, twice(read("A", "paused", 50, 300)), twice(read("B", "stopped", 51.1, 300))),
			[]posEvt{lyrics("A", 50), lyrics("B", 51.1)})
	})
	frozen := func(t *testing.T, sec float64, want float32) {
		checkPositions(t, runPositions(t, 1000,
			twice(read("A", "playing", 1.5, 200)),
			twice(read("B1", "stopped", 2.4, 300)),
			twice(read("B2", "stopped", sec, 300)),
		), []posEvt{lyrics("A", 1.5), lyrics("B1", 0), lyrics("B2", want)})
	}
	t.Run("(b) 0.005", func(t *testing.T) { frozen(t, 2.405, 0) })
	t.Run("(b) 0.02", func(t *testing.T) { frozen(t, 2.42, 2.42) })
}
