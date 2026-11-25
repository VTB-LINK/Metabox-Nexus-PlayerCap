package assoc

import (
	"bytes"
	"slices"
	"strings"
	"unicode/utf8"
)

// QQ 内存里当前歌曲记录的读取上限（设计 §6.4 第 4 步）。
const (
	RecordTailMax  = 192 // 每处前缀命中自 http 起至多看的字节数（尾部窗口）
	RecordURLMax   = 128 // 网址须在尾部的前 128 字节内
	RecordParamMax = 32  // 参数 0 连同其后的 & 须在 0= 或 =&0= 之后的 32 字节内
	RecordHitsMax  = 64  // 前缀命中至多看 64 处，第 65 处即停、判不唯一
	RecordStrMax   = 256 // 专辑、歌手、歌名各至多读 256 个 UTF-16 码元，其内须有 NUL
)

// RecordNeedle 拼出当前歌曲记录的前缀（设计 §6.4 第 4 步「前缀」）：专辑、歌手、歌名三段 UTF-8 原样直接相接（不去空白、
// 不改大小写），再接 http。任一段为空、或含 U+FFFD（不成对的代理项解码所得）或非法 UTF-8 时为假，不扫：缺一段的前缀
// 可能命中别的记录的中段。
func RecordNeedle(album, singer, name string) ([]byte, bool) {
	for _, s := range []string{album, singer, name} {
		if s == "" || strings.ContainsRune(s, utf8.RuneError) {
			return nil, false
		}
	}
	return []byte(album + singer + name + "http"), true
}

// RecordTail 是一处前缀命中截取出的网址与参数 0。
type RecordTail struct{ URL, Param0 string }

// ParseRecordTail 自前缀命中处截取网址与参数 0（设计 §6.4 第 4 步「网址截取」）。b 自 http 起，只看前 RecordTailMax 字节：
// 前 RecordURLMax 字节内第一个 .wma（含）为止是网址，须经 ClassifySongURL 归为 mid 键或 id 键；.wma 之后先看 0=，
// 否则看 =&0=，两者都不是即不合。参数 0 自所认的写法之后起，到 & 为止，& 须在 RecordParamMax 字节内，参数 0 须为非空的
// ASCII 字母数字；网址为 mid 键时须与参数 0 相同。不合即为假（截取不合，记入 RecordScan.Malformed）。
func ParseRecordTail(b []byte) (RecordTail, bool) {
	b = b[:min(len(b), RecordTailMax)]
	if !bytes.HasPrefix(b, []byte("http")) {
		return RecordTail{}, false
	}
	i := bytes.Index(b[:min(len(b), RecordURLMax)], []byte(".wma"))
	if i < 0 {
		return RecordTail{}, false
	}
	u := string(b[:i+len(".wma")])
	kind, mid, _ := ClassifySongURL(u)
	after := b[i+len(".wma"):]
	rest, ok := bytes.CutPrefix(after, []byte("0="))
	if !ok {
		rest, ok = bytes.CutPrefix(after, []byte("=&0="))
	}
	if !ok || kind != KindMid && kind != KindID {
		return RecordTail{}, false
	}
	j := bytes.IndexByte(rest[:min(len(rest), RecordParamMax)], '&')
	if j <= 0 {
		return RecordTail{}, false
	}
	p := string(rest[:j])
	if strings.IndexFunc(p, notASCIIAlnum) >= 0 || kind == KindMid && p != mid {
		return RecordTail{}, false
	}
	return RecordTail{URL: u, Param0: p}, true
}

// RecordScan 是一次扫描的所得，由扫描一侧填。
type RecordScan struct {
	Tails     []RecordTail // 截取合格的命中，按地址顺序
	Malformed int          // 截取不合的命中数
	OverCap   bool         // 命中超过 RecordHitsMax 处，扫描已停
	Complete  bool         // 扫描读完了全部该读的区域：没有到读取总量上限，也没有块读失败或只读到一部分
}

// RecordVerdict 是当前歌曲记录的结论（设计 §6.4 第 4 步「结论」）。
type RecordVerdict int

const (
	RecordNone       RecordVerdict = iota // 尚未读
	RecordFound                           // 认出：有合格的 id 键网址时它只有一种，否则合格命中的网址只有一种
	RecordNotFound                        // 找不到：扫描完整而没有合格命中
	RecordAmbiguous                       // 不唯一：命中超过 RecordHitsMax 处，或合格的 id 键网址不止一种，或没有它而合格的 mid 键网址不止一种
	RecordPending                         // 待换算：只有 songID 而有 mid 键网址的命中，要补全 songmid 才核对得了
	RecordIncomplete                      // 扫描不完整
)

// String 返回结论名的英文原文，由注入日志处经 i18n.T 翻译。
func (v RecordVerdict) String() string {
	switch v {
	case RecordNone:
		return "not read"
	case RecordFound:
		return "found"
	case RecordNotFound:
		return "not found"
	case RecordAmbiguous:
		return "ambiguous"
	case RecordPending:
		return "pending song ID completion"
	case RecordIncomplete:
		return "incomplete scan"
	}
	return "unknown"
}

// RecordConclusion 是 ConcludeRecord 的结果。URL 只在认出时非空。Qualified、Voided 为合格与作废的命中数，Malformed 为截取
// 不合的命中数；待换算的命中两者都不计。SetAside 为按 id 键优先放下的合格 mid 键网址的种数，只在由 id 键网址认出时可能非 0，
// 待换算的命中不计入。
type RecordConclusion struct {
	Verdict                      RecordVerdict
	URL                          string
	Qualified, Voided, Malformed int
	SetAside                     int
}

// ConcludeRecord 由一次扫描的所得定当前歌曲记录的结论（设计 §6.4 第 4 步「归属核对」「结论」）。先逐处核对归属：
//   - 网址归不进 mid 键或 id 键：计入截取不合。
//   - songMid 已知：参数 0 须等于它；网址为 id 键而 songID 也已知时，id 须等于 songID。
//   - 只有 songID：id 键网址须等于它；mid 键网址一律待换算（补全 songmid 才核对得了）。
//   - 两者都缺：作废。
//
// 不合的作废，作废的命中一律不计。合格命中按网址种类分为 id 键与 mid 键两组，各自去重。再按次序定结论，先成立的为准：
//  1. OverCap：不唯一，完整与否都如此。
//  2. 扫描完整、有合格的 id 键网址：只看 id 键网址，一种即认出，多于一种为不唯一；合格的 mid 键网址与待换算的命中都不看
//     （id 键优先：两种键的记录并存时 QQ 所用的都是 id 键，是观察所得的经验规则，见设计 §6.4 第 4 步）。
//  3. 有待换算：待换算。
//  4. 扫描不完整：不完整。
//  5. 其余（完整，合格命中只有 mid 键网址或没有）：0 种为找不到，1 种为认出，多于 1 种为不唯一。
//
// 合格命中都经参数 0 或 id 核对过，补全的编号也经过核验、与已知编号同属一首，所以不看关联表：结论缓存到代次变化，关联表怎么变，
// 认出的网址选中的条目都属于当前歌曲。
func ConcludeRecord(sc RecordScan, songID uint32, songMid string) RecordConclusion {
	c := RecordConclusion{Malformed: sc.Malformed}
	var idURLs, midURLs []string
	var pending bool
	for _, t := range sc.Tails {
		kind, _, id := ClassifySongURL(t.URL)
		var void bool
		switch {
		case kind != KindMid && kind != KindID:
			c.Malformed++
			continue
		case songMid != "":
			void = t.Param0 != songMid || kind == KindID && songID != 0 && id != songID
		case songID == 0:
			void = true
		case kind == KindID:
			void = id != songID
		default: // 只有 songID、mid 键网址
			pending = true
			continue
		}
		if void {
			c.Voided++
			continue
		}
		c.Qualified++
		urls := &midURLs
		if kind == KindID {
			urls = &idURLs
		}
		if !slices.Contains(*urls, t.URL) {
			*urls = append(*urls, t.URL)
		}
	}
	switch {
	case sc.OverCap:
		c.Verdict = RecordAmbiguous
	case sc.Complete && len(idURLs) == 1:
		c.Verdict, c.URL, c.SetAside = RecordFound, idURLs[0], len(midURLs)
	case sc.Complete && len(idURLs) > 1:
		c.Verdict = RecordAmbiguous
	case pending:
		c.Verdict = RecordPending
	case !sc.Complete:
		c.Verdict = RecordIncomplete
	case len(midURLs) == 0:
		c.Verdict = RecordNotFound
	case len(midURLs) == 1:
		c.Verdict, c.URL = RecordFound, midURLs[0]
	default:
		c.Verdict = RecordAmbiguous
	}
	return c
}

// MergeConclusion 把本代次此前的结论 prev 与这一次的结论 cur 并成本代次的结论（设计 §6.4 第 4 步「结论」）：cur 为认出或不唯一
// 即取 cur（认出别的网址即改换，不唯一即作废此前认出的）；否则 prev 为认出即取 prev（找不到、不完整、待换算与读不出的零值只是
// 这一次没看到，不推翻已认出的）；否则取 cur。
func MergeConclusion(prev, cur RecordConclusion) RecordConclusion {
	if cur.Verdict != RecordFound && cur.Verdict != RecordAmbiguous && prev.Verdict == RecordFound {
		return prev
	}
	return cur
}
