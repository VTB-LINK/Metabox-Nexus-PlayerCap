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

// ownPlay 判 P 的 LastPlayTime 是否属于这一首的这一次播放（设计 §6.7「P 属于这一次播放」）：不早于开始时刻减
// playGrace，这一首有界限时还须大于界限。没有 P（为 0）时不属于。
func (s *localSession) ownPlay(played int64) bool {
	return played > 0 && played >= fileTime(s.start.Add(-playGrace)) && (s.bound == 0 || played > s.bound)
}

// localNone 判 res 是否为本地文件两条规则所指的结果：这一首有界限，res 为没有、不待重试、带本地文件标记，且 P 属于
// 这一次播放，即酷狗为这份磁盘文件确实没有歌词（设计 §6.7）。
func (s *localSession) localNone(res assoc.Result) bool {
	return res.Kind == assoc.KindNone && !res.Retry && res.LocalFile && s.bound > 0 && s.ownPlay(res.PlayedAt)
}

// seen 记下这一首的一条结果：P 属于这一次播放的，计入 own。
func (s *localSession) seen(res assoc.Result) {
	if s.ownPlay(res.PlayedAt) {
		s.own = max(s.own, res.PlayedAt)
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

// await 在换歌拍写入 hash，在 stop、localWait 与结果通道上等这一首的结果（其他 hash 的丢弃）：取到歌词返回行、
// 来源与 found；没有、暂时失败、未确认、停用与超时 found 为假，调用方在线取词；收到 stop 返回 stopped，调用方结束会话，
// 不再在线取词（单次至多 8 s，不受 stop 中断）。读取者疑似阻塞（最早一次未获结果的写入已过 stuckAfter，如读取者
// panic 后已退出）时照常写入、不等。
//
// 结果合 localNone（这一首有界限，结果为没有、不待重试、带本地文件标记，P 属于这一次播放）时 none 为真：调用方照常
// 在线取词，但取不到或出错时不沿用上一首的歌词（设计 §6.7「换歌拍」例外）。
//
// 换歌拍同时开始新的一首：记下开始时刻，上一首的 own 转作这一首的界限，来源与沿用标记清零。读取者为 nil 时同样如此。
func (s *localSession) await(stop <-chan struct{}, hash string) (lines []klyric.Line, src assoc.Source, found, none, stopped bool) {
	s.cur, s.curPlayedAt, s.carried = 0, 0, false
	s.start, s.bound, s.own = time.Now(), s.own, 0
	if s.r == nil || hash == "" {
		return nil, 0, false, false, false
	}
	stuck := !s.unanswered.IsZero() && time.Since(s.unanswered) >= stuckAfter
	s.r.SetHash(hash)
	if s.unanswered.IsZero() {
		s.unanswered = time.Now()
	}
	if stuck {
		return nil, 0, false, false, false
	}
	timer := time.NewTimer(localWait)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return nil, 0, false, false, true
		case <-timer.C:
			return nil, 0, false, false, false
		case res := <-s.r.Results():
			s.note(res)
			switch {
			case s.r == nil: // 停用
				return nil, 0, false, false, false
			case res.Hash != hash:
				continue
			}
			s.seen(res)
			if res.Kind == assoc.KindFound && len(res.Lines) > 0 {
				s.cur, s.curPlayedAt = res.Source, res.PlayedAt
				return res.Lines, res.Source, true, false, false
			}
			return nil, 0, false, s.localNone(res), false
		}
	}
}

// poll 非阻塞地取一条结果：是 hash 这一首的歌词、且指纹与 fp 不同，返回行、来源、新指纹与 true；没有结果、上一首的、
// 内容相同的与不是歌词的返回 false，调用方不发事件。
//
// 以下三条同时满足时同样返回 false（设计 §6.7「播放中」）：当前歌词来自关联；新结果不是关联且带「关联未取用」标记
// （res.AssocSkipped）；新结果的 P 的 LastPlayTime（res.PlayedAt）与采纳当前歌词时记下的相同，即仍是同一次播放。
// 关联在播放中失效（被酷狗拒绝、文件改名或删除、判为不可用）时，酷狗仍显示原来的歌词，按用户决定「2甲」保持当前
// 歌词到下一首。其余情形照换：不带标记即第 1 步本就没有关联键；PlayedAt 不同，说明当初采纳的关联属于换歌拍时
// 酷狗尚未落盘、被误选为 P 的另一行，或已是新的一次播放，应换成缓存（设计 §8）。
//
// 清词（设计 §6.7「播放中」）：结果合 localNone，且当前歌词是换歌拍沿用的（carried），或采纳自本地、而结果的 PlayedAt
// 与记下的不同，即清掉：返回空的行、空歌词的指纹与 true，调用方据此发无歌词；来源清零、记下结果的 PlayedAt、沿用标记
// 清除，同样的结果再来时两条都不成立，不再发。采纳自本地、PlayedAt 相同的仍按「2甲」保持；当前歌词是在线取到的不清。
// carried 只在沿用了非空的歌词时为真，cur 只在采纳了非空的歌词时非零，故清掉的总是沿用或采纳来的非空歌词。
func (s *localSession) poll(hash string, fp [32]byte) ([]klyric.Line, assoc.Source, [32]byte, bool) {
	if s.r == nil {
		return nil, 0, fp, false
	}
	select {
	case res := <-s.r.Results():
		s.note(res)
		if res.Hash != hash {
			break
		}
		s.seen(res)
		if s.localNone(res) {
			if !s.carried && (s.cur == 0 || res.PlayedAt == s.curPlayedAt) {
				break
			}
			s.cur, s.curPlayedAt, s.carried = 0, res.PlayedAt, false
			return nil, 0, lyricFingerprint(nil), true
		}
		if res.Kind != assoc.KindFound || len(res.Lines) == 0 {
			break
		}
		if s.cur == assoc.SourceAssociation && res.Source != assoc.SourceAssociation && res.AssocSkipped &&
			res.PlayedAt == s.curPlayedAt {
			break
		}
		s.cur, s.curPlayedAt, s.carried = res.Source, res.PlayedAt, false
		if nfp := lyricFingerprint(res.Lines); nfp != fp {
			return res.Lines, res.Source, nfp, true
		}
	default:
	}
	return nil, 0, fp, false
}
