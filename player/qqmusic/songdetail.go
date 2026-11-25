package qqmusic

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

// detailState 是会话交给读取者的详情结果（设计 §6.4 第 2 步「交接」）。
type detailState uint8

const (
	detailNone   detailState = iota // 尚无：还没请求或请求中
	detailGot                       // 已得：核验过的另一个编号
	detailFailed                    // 已失败：请求失败、响应里没有编号或核验不过
)

// songDetail 是一次详情结果。已得时 songID 与 songMid 恰有一个非零，是补上的那个编号；其余状态两者都为零值。
// seen 只在收到了响应而核验不过时非空，写明响应里的编号，供会话一侧的 Detail（设计 §6.9）；请求失败时为空。
type songDetail struct {
	state   detailState
	songID  uint32
	songMid string
	seen    string
}

// applyDetail 在 fetchCoverURL 返回之后写详情结果（设计 §6.4 第 2 步「交接」）。所查编号（queriedID、queriedMid）与 k 里的相同时
// d 即 k 的详情结果；不同时经 bridgeDetail 以核验结果衔接，衔接不上即不写（同一拍里先换歌、assocKey 已整体换新时，上一首的结果
// 带不进新歌；v20.05 的结构体 songid 是伪值，所查与 k 对不上即不写）。按状态转移写入并返回真：尚无 → 已得或已失败，已失败 →
// 已得；其余原样返回、假，已得不被覆盖，已失败到已失败不写。代次与其余字段不动。
func applyDetail(k songKey, queriedID uint32, queriedMid string, d songDetail) (songKey, bool) {
	if queriedID != k.songID || queriedMid != k.songMid {
		d = bridgeDetail(k, queriedID, queriedMid, d)
	}
	if k.detail.state == detailNone && d.state != detailNone || k.detail.state == detailFailed && d.state == detailGot {
		k.detail = d
		return k, true
	}
	return k, false
}

// bridgeDetail 把按另一组编号查得的详情 d 换算成 k 的详情结果（补取每拍重读编号，k 只在编号缺位时更新，所查的组合可以与 k 的
// 不同）：k 只有 songmid、按 songid 查得（queriedMid 为空，同 fetchCoverURL 的分派）的已得其 mid 等于 k 的 songmid，即补上了
// songid，为已得、songID 为所查的 songid；k 只有 songid、按 songmid 查得的已得其 songid 等于 k 的，即补上了 songmid，为已得、
// songMid 为所查的 songmid。其余（核验不过、编号不等、k 两个编号都缺或都有、按 songmid 查而 k 只有 songmid）衔接不上，为尚无。
func bridgeDetail(k songKey, queriedID uint32, queriedMid string, d songDetail) songDetail {
	switch {
	case d.state != detailGot:
	case k.songID == 0 && k.songMid != "" && queriedID != 0 && queriedMid == "" && d.songMid == k.songMid:
		return songDetail{state: detailGot, songID: queriedID}
	case k.songID != 0 && k.songMid == "" && queriedMid != "" && d.songID == k.songID:
		return songDetail{state: detailGot, songMid: queriedMid}
	}
	return songDetail{}
}

// exhaustDetail 在补取窗口用尽时（放弃，或暂停中保持）与补取成功、写过封面带回的详情结果之后写详情结果：尚无即记为已失败、
// 返回真，交读取者兜底（设计 §6.4 第 2 步「交接」）；已得与已失败原样返回、假。保持期间每拍都调用，只有由尚无改为已失败的那一拍
// 为真。
func exhaustDetail(k songKey) (songKey, bool) {
	if k.detail.state != detailNone {
		return k, false
	}
	k.detail = songDetail{state: detailFailed}
	return k, true
}

// unconfirmedDetail 报告会话一侧是否该为这次详情结果打「核验不过」的 Detail（设计 §6.9）：所查编号与 k 里的相同、d 为已失败且
// 收到了响应（seen 非空），k 里也不是已得（已得时读取者不再请求）。返回所查编号的写法：同 fetchCoverURL 的分派，有 songmid
// 即按它查，否则按 songid。请求失败（seen 为空）不打。每首至多一条由调用方管。
func unconfirmedDetail(k songKey, queriedID uint32, queriedMid string, d songDetail) (string, bool) {
	if queriedID != k.songID || queriedMid != k.songMid || d.state != detailFailed || d.seen == "" || k.detail.state == detailGot {
		return "", false
	}
	if queriedMid != "" {
		return queriedMid, true
	}
	return strconv.FormatUint(uint64(queriedID), 10), true
}

// singleSong 是单曲接口 fcg_play_single_song.fcg 的 data 里一首歌的编号。id 与 mid 都保留 JSON 原文、在核验时才解析：
// 写法异常的编号只使该首核验不过，不让整份响应解不开而连带丢掉封面。
type singleSong struct {
	ID  json.RawMessage `json:"id"`
	Mid json.RawMessage `json:"mid"`
}

// String 供 seen 列出一首的编号。
func (s singleSong) String() string { return fmt.Sprintf("{id %s, mid %s}", rawID(s.ID), rawID(s.Mid)) }

// songIDOf 把响应里 id 的原文按十进制无符号 32 位整数解析；字符串、负数、小数、指数写法、超出 uint32、null 与缺省
// 都解析不出。
func songIDOf(raw json.RawMessage) (uint32, bool) {
	n, err := strconv.ParseUint(string(raw), 10, 32)
	return uint32(n), err == nil
}

// songMidOf 把响应里 mid 的原文按 JSON 字符串解析（转义照 JSON 解码）；数字、null、数组、对象、布尔与缺省都解析不出。
func songMidOf(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// detailFromTrack 核验按 songid 查得的详情（get_song_detail_yqq 的 track_info）：所查编号非 0、code 为 0、id 解析得出且
// 等于所查、mid 合 songmid 的形式，才交出 mid；否则为已失败。接口给出另一首的编号时据此不用（设计 §7「错误编号与后缀误命中」）。
func detailFromTrack(queried uint32, code int, trackID json.RawMessage, trackMid string) songDetail {
	if id, ok := songIDOf(trackID); ok && queried != 0 && code == 0 && id == queried && assoc.IsSongMid(trackMid) {
		return songDetail{state: detailGot, songMid: trackMid}
	}
	return songDetail{state: detailFailed, seen: fmt.Sprintf(i18n.T("track id %s, mid %q, code %d"), rawID(trackID), clip(trackMid), code)}
}

// detailFromSingle 核验按 songmid 查得的单曲：code 为 0 且 data 里有 mid 是 JSON 字符串、与所查相同（区分大小写）、id 解析
// 得出且非 0 的一首，才交出它的 id；否则为已失败。所查为空串一律已失败。
func detailFromSingle(queried string, code int, data []singleSong) songDetail {
	for _, d := range data {
		mid, isStr := songMidOf(d.Mid)
		if id, ok := songIDOf(d.ID); ok && isStr && queried != "" && code == 0 && mid == queried && id != 0 {
			return songDetail{state: detailGot, songID: id}
		}
	}
	// seen 只列前 4 首与总数：接口异常时 data 可能很长，不把整份响应写进日志。
	return songDetail{state: detailFailed, seen: fmt.Sprintf(i18n.T("%d songs %v, code %d"), len(data), data[:min(len(data), 4)], code)}
}

// rawID 是 seen 里 id 与单曲 mid 的写法：JSON 原文（截短），缺省写作 -。
func rawID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	return clip(string(raw))
}

// clip 把写进 seen 的响应原文截到至多 32 字节，截断处不留半个字符，截过即加 …。
func clip(s string) string {
	if len(s) <= 32 {
		return s
	}
	return strings.ToValidUTF8(s[:32], "") + "…"
}
