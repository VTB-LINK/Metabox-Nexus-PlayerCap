package kugou

// 本文件是会话这一侧的本地歌词（#205，设计 §6.7）：换歌拍写入 hash、至多等 localWait 取酷狗为这首歌显示的歌词
// （手动关联或酷狗的歌词缓存），播放中非阻塞地接收读取者的结果。确认、读库与解析都在 assoc 包的读取者 goroutine 里，
// 会话的轮询线程不做文件 IO。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// 换歌拍等本地结果的上限，与读取者疑似阻塞的判据（设计 §6.7、§9 第 3 条）。
const (
	localWait  = 300 * time.Millisecond
	stuckAfter = 5 * time.Second
)

// playGrace 是判 P 属于这一次播放时开始时刻的余量（设计 §6.7「换歌拍」）：真机上酷狗写入 LastPlayTime 比会话认出换歌
// 早 0～约 2.2 秒。
const playGrace = 5 * time.Second

// fileTime 把时刻换算成 LastPlayTime 的单位：1601-01-01 UTC 起的 100 ns 计数（设计 §2.3）。两者的换算只在这里。
func fileTime(t time.Time) int64 {
	const unixFrom1601 = 11644473600 // 1601-01-01 至 1970-01-01 的秒数
	return (t.Unix()+unixFrom1601)*10_000_000 + int64(t.Nanosecond()/100)
}

// localLyrics 是会话用到的读取者方法。生产恒为 *assoc.Reader，会话测试注入假读取者。
type localLyrics interface {
	SetHash(hash string)
	Results() <-chan assoc.Result
}

// localLyricsStarter 启动本会话的读取者，ctx 取消即令其退出；返回 nil 即本会话只在线取词。
type localLyricsStarter func(ctx context.Context) localLyrics

// assocStart 是 assoc.Start 的可替换 seam：仅供测试让它返回 nil，不起真读取者、不碰本机酷狗。生产恒为 assoc.Start，不导出。
var assocStart = assoc.Start

// startLocalLyrics 是生产的启动器，Start 在 EnsurePatched 返回 nil 时交给 runSession。上一个会话的读取者还没退出时
// assoc.Start 返回 nil，本会话只在线取词。
//
// 此时须返回无类型的 nil，不得写成 return assocStart(ctx)：(*assoc.Reader)(nil) 装进接口后接口不等于 nil，
// 会话会去调 nil 读取者的方法。
func startLocalLyrics(ctx context.Context) localLyrics {
	r := assocStart(ctx)
	if r == nil {
		log.Detail("The previous session's local lyric reader has not exited; using online lyrics only this session")
		return nil
	}
	return r
}

// lyricFingerprint 是一份歌词的指纹：行序列化后的 SHA-256，播放中据此判酷狗给的歌词与当前的是否相同。须在
// applyDetailedOffset 之前取：它就地改写逐字的 PlayTime，套过与没套 offset 的同一份歌词指纹不同。
// json.Marshal 只在浮点为 NaN 或无穷时出错，解析出的时间都是有限值。
func lyricFingerprint(lines []klyric.Line) [32]byte {
	b, _ := json.Marshal(lines)
	return sha256.Sum256(b)
}

// localSession 是会话这一侧的读取者状态。r 为 nil 即本会话不读本地歌词：没有启用，或读取者回了停用。
type localSession struct {
	r localLyrics
	// unanswered 是最早一次还没收到任何结果的 SetHash 的时刻，零值即此后收到过结果。从最早一次算起：读取者卡住时
	// 连续快速换歌，从最近一次算起的话每首都要再等 localWait。
	unanswered time.Time
	// cur 是当前这首最近一次被采纳的本地歌词的来源（内容相同、未重发的也算），零值即当前歌词不是本地取到的（在线、
	// 无歌词或同曲目复用）；curPlayedAt 是同一条结果所带的 P 的 LastPlayTime（assoc.Result.PlayedAt），与 cur 同时记。
	// 播放中据这两项在同一次播放内不让关联退成缓存，见 poll。
	cur         assoc.Source
	curPlayedAt int64
	// 以下供本地文件的两条规则（设计 §6.7）：start 是这一首的开始时刻（换歌拍写入 hash 时记下）；bound 是这一首的界限，
	// 0 即没有；own 是这一首收到过的、属于这一次播放的 P 的 LastPlayTime 最大值，0 即没有，换歌拍转作下一首的界限；
	// carried 为当前歌词是换歌拍沿用的上一首歌词、其后还没有采纳过本地歌词，由调用方在同名复用时置真。
	start   time.Time
	bound   int64
	own     int64
	carried bool
}

// results 安全返回读取者的结果通道：r 为 nil 时返回 nil，供 select 自动屏蔽且绝不对 nil 接口调 Results()。
func (s *localSession) results() <-chan assoc.Result {
	if s.r == nil {
		return nil
	}
	return s.r.Results()
}

// ownPlay 判 P 的 LastPlayTime 是否属于这一首的这一次播放（设计 §6.7「P 属于这一次播放」）：不早于开始时刻减
// playGrace，这一首有界限时还须大于界限。没有 P（为 0）时不属于。
func (s *localSession) ownPlay(played int64) bool {
	return played > 0 && played >= fileTime(s.start.Add(-playGrace)) && (s.bound == 0 || played > s.bound)
}

// localNone 判 res 是否为本地文件两条规则所指的结果：这一首有界限，res 为没有、酷狗内存无已加载歌词行
// （!HasMemoryLyric）、不待重试、带本地文件标记，且 P 属于这一次播放，即酷狗为这份磁盘文件确实没有歌词
// （设计 §6.4.4、§6.7）。
func (s *localSession) localNone(res assoc.Result) bool {
	return res.Kind == assoc.KindNone && !res.HasMemoryLyric && !res.Retry && res.LocalFile && s.bound > 0 && s.ownPlay(res.PlayedAt)
}

// seen 记下这一首的一条结果：P 属于这一次播放的，计入 own；若当前本地歌词采纳时 PlayedAt 尚未落盘（为 0），
// 同步补记 curPlayedAt 以衔接后续同次播放保护与单曲循环重播清词。
func (s *localSession) seen(res assoc.Result) {
	if s.ownPlay(res.PlayedAt) {
		s.own = max(s.own, res.PlayedAt)
		if s.cur != 0 && s.curPlayedAt == 0 {
			s.curPlayedAt = res.PlayedAt
		}
	}
}

// note 记下收到的一条结果：任何结果都说明读取者还在响应。停用是整个会话的，不看 hash：此后不再写入 hash、不再等待
// （停用的 Warn 与上报由读取者自己做）。
func (s *localSession) note(res assoc.Result) {
	s.unanswered = time.Time{}
	if res.Kind == assoc.KindDisabled {
		s.r = nil
	}
}

// localAwait 是换歌拍 await 等待本地读取者的结论。
type localAwait struct {
	lines          []klyric.Line
	src            assoc.Source
	lyricOffsetMs  int
	found          bool
	unassociated   bool
	none           bool // 合 localNone（本地文件确实无词，换歌拍不沿用同名上一首）
	hasMemoryLyric bool
	stopped        bool
}

// await 在换歌拍写入 hash，在 stop、localWait 与结果通道上等这一首的结果（其他 hash 的丢弃）：
//   - KindUnassociated：立即返回 unassociated（不在线取词、不复用）；
//   - KindFound（非空行）：记下来源与 PlayedAt，返回 found 与 lyricOffsetMs；
//   - KindNone：返回 localNone 判定、lyricOffsetMs 与 hasMemoryLyric，由调用方进入在线取词；
//   - 其余结局（停用、暂时失败、未确认、空行 KindFound、超时、读取者疑似阻塞）：返回未命中（偏移 0），走在线取词；
//   - stop：返回 stopped，调用方结束会话。
//
// 换歌拍同时开始新的一首：记下开始时刻，上一首的 own 转作这一首的界限，来源与沿用标记清零。读取者为 nil 时同样如此。
func (s *localSession) await(stop <-chan struct{}, hash string) localAwait {
	s.cur, s.curPlayedAt, s.carried = 0, 0, false
	s.start, s.bound, s.own = time.Now(), s.own, 0
	if s.r == nil || hash == "" {
		return localAwait{}
	}
	stuck := !s.unanswered.IsZero() && time.Since(s.unanswered) >= stuckAfter
	s.r.SetHash(hash)
	if s.unanswered.IsZero() {
		s.unanswered = time.Now()
	}
	if stuck {
		return localAwait{}
	}
	timer := time.NewTimer(localWait)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return localAwait{stopped: true}
		case <-timer.C:
			return localAwait{}
		case res := <-s.results():
			s.note(res)
			switch {
			case s.r == nil: // 停用
				return localAwait{}
			case res.Hash != hash:
				continue
			}
			s.seen(res)
			switch res.Kind {
			case assoc.KindUnassociated:
				return localAwait{unassociated: true}
			case assoc.KindFound:
				if len(res.Lines) > 0 {
					s.cur, s.curPlayedAt = res.Source, res.PlayedAt
					return localAwait{
						lines:         res.Lines,
						src:           res.Source,
						lyricOffsetMs: res.LyricOffsetMs,
						found:         true,
					}
				}
				return localAwait{}
			case assoc.KindNone:
				off := 0
				if res.HasMemoryLyric {
					off = res.LyricOffsetMs
				}
				return localAwait{
					none:           s.localNone(res),
					lyricOffsetMs:  off,
					hasMemoryLyric: res.HasMemoryLyric,
				}
			default:
				return localAwait{}
			}
		}
	}
}

// pollKind 表示播放中 poll 从本地读取者收到的待处理动作种类。
type pollKind uint8

const (
	pollEmpty          pollKind = iota // 无结果、非本首、停用、暂时失败、未确认或被防关联降级守卫忽略
	pollUnassociated                   // 用户显式取消关联：清空歌词且禁止回退
	pollFound                          // 收到新的本地/缓存歌词或偏移变化候选（已过防关联降级守卫）
	pollClearLocalNone                 // #205 本地文件确实无歌词：清掉沿用或不同 PlayedAt 的旧本地歌词
	pollNone                           // 其余 KindNone：供会话按设计 §6.4.4 解除取消关联、重算偏移或触发异步在线恢复
)

// localPoll 是播放中 poll 非阻塞收一条结果后的返回结构。
type localPoll struct {
	kind           pollKind
	lines          []klyric.Line
	src            assoc.Source
	lyricOffsetMs  int
	hasMemoryLyric bool
}

// classify 按设计 §6.4.4 与 #205 规则分类一条播放中收到的本地读取者结果，并返回待执行动作。
//
// 防关联降级守卫（设计 §6.7「播放中」）：当前歌词来自关联、新结果不是关联且带 AssocSkipped 标记、且 PlayedAt 与
// 采纳当前歌词时记下的相同（同一次播放）时忽略，保持当前歌词到下一首；其余情形照常更新来源与 PlayedAt 并返回
// pollFound，由调用方按指纹与偏移决定是否重发。
//
// 清词（设计 §6.7「播放中」）：非 HasMemoryLyric 且合 localNone，且当前歌词是换歌拍沿用的（carried）或采纳自本地
// 而 PlayedAt 不同时，清零来源与沿用标记、记下新 PlayedAt 并返回 pollClearLocalNone。
func (s *localSession) classify(hash string, res assoc.Result) localPoll {
	s.note(res)
	if s.r == nil || res.Hash != hash {
		return localPoll{}
	}
	s.seen(res)
	switch res.Kind {
	case assoc.KindUnassociated:
		s.cur, s.curPlayedAt, s.carried = 0, res.PlayedAt, false
		return localPoll{kind: pollUnassociated}
	case assoc.KindFound:
		if len(res.Lines) == 0 {
			return localPoll{}
		}
		if s.cur == assoc.SourceAssociation && res.Source != assoc.SourceAssociation && res.AssocSkipped &&
			res.PlayedAt == s.curPlayedAt {
			return localPoll{}
		}
		s.cur, s.curPlayedAt, s.carried = res.Source, res.PlayedAt, false
		return localPoll{
			kind:          pollFound,
			lines:         res.Lines,
			src:           res.Source,
			lyricOffsetMs: res.LyricOffsetMs,
		}
	case assoc.KindNone:
		if s.localNone(res) && (s.carried || (s.cur != 0 && res.PlayedAt != s.curPlayedAt)) {
			s.cur, s.curPlayedAt, s.carried = 0, res.PlayedAt, false
			return localPoll{kind: pollClearLocalNone}
		}
		return localPoll{
			kind:           pollNone,
			lyricOffsetMs:  res.LyricOffsetMs,
			hasMemoryLyric: res.HasMemoryLyric,
		}
	}
	return localPoll{}
}

// poll 非阻塞地取一条结果并交给 classify 分类。
func (s *localSession) poll(hash string) localPoll {
	if s.r == nil {
		return localPoll{}
	}
	select {
	case res := <-s.results():
		return s.classify(hash, res)
	default:
	}
	return localPoll{}
}
