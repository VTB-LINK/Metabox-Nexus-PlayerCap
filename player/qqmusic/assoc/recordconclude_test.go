package assoc

// 本文件只测 ConcludeRecord：由扫描所得的各处命中做归属核对，定当前歌曲记录一次扫描的结论（设计 §6.4 第 4 步
// 「归属核对」「结论」，§7「归属核对」「后缀误命中」「结论（单次）」「结论（单次，id 键优先）」）。结论不看关联表。

import "testing"

// id 键优先各例的合成编号与网址（设计 §7「结论（单次，id 键优先）」）：当前歌曲 songid prioID、songmid prioMid；prioIDA 与
// prioIDB 是同一 id 的两种写法，prioMidA 与 prioMidB 是同一 mid 的两种写法，参数 0 都为 prioMid。prioOtherID 是另一首的 id。
const (
	prioID      = 268000001
	prioMid     = "000TestMid0001"
	prioIDA     = `http://streamundefined.qqmusic.qq.com/268000001.wma`
	prioIDB     = `http://stream4.qqmusic.qq.com/268000001.wma`
	prioMidA    = `http://stream4.qqmusic.qq.com/000TestMid0001.wma`
	prioMidB    = `http://stream12.qqmusic.qq.com/000TestMid0001.wma`
	prioOtherID = `http://streamundefined.qqmusic.qq.com/268000002.wma`
)

// prioTail 是参数 0 为 prioMid 的一处命中。
func prioTail(u string) RecordTail { return RecordTail{URL: u, Param0: prioMid} }

// prioFull 是一次完整、不超 64 处的扫描，命中依次为 urls（参数 0 都为 prioMid）。
func prioFull(urls ...string) RecordScan {
	sc := RecordScan{Complete: true}
	for _, u := range urls {
		sc.Tails = append(sc.Tails, prioTail(u))
	}
	return sc
}

// 变异自证（实测，第 5 版增改用例后复测）：只有 songID 时把 mid 键网址计为合格，含待换算命中的九例红；songMid 已知时不比
// id 键网址的 id，「songMid 与 songID 都已知、id 不同」一例红。
func TestConcludeRecord(t *testing.T) {
	const (
		uMid4    = `http://stream4.qqmusic.qq.com/000AbCd.wma`
		uMid12   = `http://stream12.qqmusic.qq.com/000AbCd.wma`
		uIDu     = `http://streamundefined.qqmusic.qq.com/123.wma`
		uOther   = `http://stream4.qqmusic.qq.com/000Other.wma`
		uOtherID = `http://streamundefined.qqmusic.qq.com/456.wma`
		uID0     = `http://stream4.qqmusic.qq.com/0.wma`
	)
	tl := func(u, p0 string) RecordTail { return RecordTail{URL: u, Param0: p0} }
	full := func(ts ...RecordTail) RecordScan { return RecordScan{Tails: ts, Complete: true} }
	hitMid := tl(uMid4, "000AbCd")
	hitID := tl(uIDu, "000AbCd")
	// 另一首（id 268000002、mid 000TestMid0002）的 id 键记录在前，当前歌曲的 mid 键记录在后
	sufPrio := RecordScan{Tails: []RecordTail{tl(prioOtherID, "000TestMid0002"), prioTail(prioMidA)}, Complete: true}

	cases := []struct {
		name   string
		sc     RecordScan
		songID uint32
		mid    string
		want   RecordConclusion
	}{
		// 归属核对：songMid 已知时参数 0 须等于它；id 键网址而 songID 也已知时 id 须等于它。
		{"songMid 已知、mid 键网址参数 0 相同", full(hitMid), 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uMid4, Qualified: 1}},
		{"songMid 已知、mid 键网址参数 0 不同", full(tl(uOther, "000Other")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		{"songMid 已知、id 键网址参数 0 相同", full(hitID), 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uIDu, Qualified: 1}},
		{"songMid 已知、id 键网址参数 0 不同", full(tl(uIDu, "000Other")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		{"songMid 已知、参数 0 只差大小写", full(tl(`http://stream4.qqmusic.qq.com/000abcd.wma`, "000abcd")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		{"songMid 与 songID 都已知、id 键网址参数 0 相同而 id 不同", full(tl(uOtherID, "000AbCd")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		// 只有 songMid：id 键网址无从比 id，参数 0 相同即合格（第 3 版的不变式会作废它）。
		{"只有 songMid、id 键网址参数 0 相同", full(hitID), 0, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uIDu, Qualified: 1}},
		{"只有 songMid、id 键网址参数 0 不同", full(tl(uOtherID, "000Other")), 0, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},

		// 只有 songID：id 键网址须等于它。
		{"只有 songID、id 键网址相同", full(hitID), 123, "",
			RecordConclusion{Verdict: RecordFound, URL: uIDu, Qualified: 1}},
		{"只有 songID、id 键网址不同", full(tl(uOtherID, "000AbCd")), 123, "",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		// 只有 songID：mid 键网址一律待换算，不计合格、不计作废（不凭表里有没有它的条目定）。
		{"只有 songID、mid 键网址", full(hitMid), 123, "",
			RecordConclusion{Verdict: RecordPending}},
		{"只有 songID、mid 键网址只差主机号", full(tl(uMid12, "000AbCd")), 123, "",
			RecordConclusion{Verdict: RecordPending}},
		// 第 5 版：扫描完整而有合格的 id 键网址即只看 id 键，待换算的 mid 键网址不看、不等补全（第 4 版这两例为待换算）。
		{"只有 songID、id 键网址合格而另有同一首的 mid 键网址：认出 id 键", full(hitID, hitMid), 123, "",
			RecordConclusion{Verdict: RecordFound, URL: uIDu, Qualified: 1}},
		{"只有 songID、id 键网址合格而另有不同的 mid 键网址：认出 id 键，不判不唯一", full(hitID, tl(uOther, "000Other")), 123, "",
			RecordConclusion{Verdict: RecordFound, URL: uIDu, Qualified: 1}},
		{"同上而扫描不完整：待换算", RecordScan{Tails: []RecordTail{hitID, tl(uOther, "000Other")}}, 123, "",
			RecordConclusion{Verdict: RecordPending, Qualified: 1}},
		{"只有 songID、待换算先于不完整", RecordScan{Tails: []RecordTail{hitMid}}, 123, "",
			RecordConclusion{Verdict: RecordPending}},
		{"只有 songID、超过 64 处且有待换算", RecordScan{Tails: []RecordTail{hitMid}, OverCap: true, Complete: true}, 123, "",
			RecordConclusion{Verdict: RecordAmbiguous}},

		// 两者都缺：无从核对，一律作废，mid 键网址也不算待换算。
		{"songID 为 0 且 songMid 为空", full(tl(uID0, "000AbCd"), tl(uOther, "000Other"), hitMid), 0, "",
			RecordConclusion{Verdict: RecordNotFound, Voided: 3}},

		// 后缀误命中：另一首的专辑以本首专辑结尾、歌手与歌名相同，当前记录没命中。
		{"后缀误命中、songMid 已知、另一首参数 0 不同", full(tl(uOther, "000Other")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		{"后缀误命中、只有 songID、另一首的 mid 键网址", full(tl(uOther, "000Other")), 123, "",
			RecordConclusion{Verdict: RecordPending}},
		{"后缀误命中、只有 songID、另一首的 id 键网址", full(tl(uOtherID, "000Other")), 123, "",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1}},
		// id 键优先的反例（第 5 版）：另一首的 id 键记录与当前歌曲的 mid 键记录同在，前者作废，不得因它放下后者。
		{"后缀误命中、只有 songID、另一首的 id 键记录与当前的 mid 键记录", sufPrio, prioID, "",
			RecordConclusion{Verdict: RecordPending, Voided: 1}},
		{"同上，补上 songMid 再求一次", sufPrio, prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioMidA, Qualified: 1, Voided: 1}},
		{"后缀误命中、只有 songMid、另一首的 id 键记录与当前的 mid 键记录", sufPrio, 0, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioMidA, Qualified: 1, Voided: 1}},

		// 单次结论。
		{"没有命中", RecordScan{Complete: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound}},
		{"1 处合格", full(hitMid), 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uMid4, Qualified: 1}},
		{"2 处同网址", full(hitMid, hitMid), 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uMid4, Qualified: 2}},
		// 第 5 版 id 键与 mid 键网址并存即取 id 键（见 TestConcludeRecordIDPriority），这里改用同一 mid 的两种写法造不唯一。
		{"2 处不同网址", full(hitMid, tl(uMid12, "000AbCd")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"合格与作废混合只看合格的", full(tl(uOtherID, "000Other"), hitMid, tl(uOther, "000Other")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uMid4, Qualified: 1, Voided: 2}},
		{"合格加截取不合", RecordScan{Tails: []RecordTail{hitMid}, Malformed: 2, Complete: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordFound, URL: uMid4, Qualified: 1, Malformed: 2}},
		{"全为截取不合", RecordScan{Malformed: 3, Complete: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Malformed: 3}},
		{"全被作废", full(tl(uOtherID, "000Other"), tl(uOther, "000Other")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 2}},
		{"作废加截取不合", RecordScan{Tails: []RecordTail{tl(uOtherID, "000Other")}, Malformed: 1, Complete: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Voided: 1, Malformed: 1}},
		{"超过 64 处、完整", RecordScan{Tails: []RecordTail{hitMid}, OverCap: true, Complete: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 1}},
		{"超过 64 处、不完整", RecordScan{Tails: []RecordTail{hitMid}, OverCap: true}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 1}},
		// 不完整：不认出，也不判找不到。
		{"1 处合格、不完整", RecordScan{Tails: []RecordTail{hitID}}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordIncomplete, Qualified: 1}},
		{"没有命中、不完整", RecordScan{}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordIncomplete}},
		{"全被作废、不完整", RecordScan{Tails: []RecordTail{tl(uOtherID, "000Other")}}, 123, "000AbCd",
			RecordConclusion{Verdict: RecordIncomplete, Voided: 1}},

		// Tails 由扫描一侧经 ParseRecordTail 填入；归不了类的网址按截取不合计。
		{"Tails 里归不了类的网址", full(tl(`http://example.com/a.wma`, "a")), 123, "000AbCd",
			RecordConclusion{Verdict: RecordNotFound, Malformed: 1}},
	}
	for _, c := range cases {
		if got := ConcludeRecord(c.sc, c.songID, c.mid); got != c.want {
			t.Errorf("%s：ConcludeRecord = %+v，应为 %+v", c.name, got, c.want)
		}
	}
}

// id 键优先（设计 §6.4 第 4 步「结论」第 2 条，§7「结论（单次，id 键优先）」）：扫描完整、不超 64 处，合格命中里有 id 键网址
// 时只看 id 键网址，一种即认出、多于一种为不唯一，合格的 mid 键网址按种数记入 SetAside，待换算的命中不看也不计。除注明外两个
// 编号都已知。
// 变异自证（实测）：去掉 id 键优先（并存仍判不唯一），取 id 键的前三例、待换算组的前两例与只有 songmid 的并存例红；id 键优先
// 不要求扫描完整，两例「……而扫描不完整」红；id 键优先挪到 OverCap 之前，「超过 64 处、完整」一例红；待换算挪到 id 键优先
// 之前，待换算组的前两例红；作废的 id 键命中也计入，两例「id 键命中作废」与 TestConcludeRecord 里 id 键优先的后缀误命中反例
// 三例红；多于一种 id 键网址时取第一种，四例不唯一红；SetAside 计入待换算的命中，「不等补全」一例红。
func TestConcludeRecordIDPriority(t *testing.T) {
	incomplete := func(urls ...string) RecordScan {
		sc := prioFull(urls...)
		sc.Complete = false
		return sc
	}
	overCap := func(complete bool, urls ...string) RecordScan {
		sc := prioFull(urls...)
		sc.OverCap, sc.Complete = true, complete
		return sc
	}
	withTail := func(sc RecordScan, u, p0 string) RecordScan {
		sc.Tails = append([]RecordTail{{URL: u, Param0: p0}}, sc.Tails...)
		return sc
	}
	for _, c := range []struct {
		name   string
		sc     RecordScan
		songID uint32
		mid    string
		want   RecordConclusion
	}{
		// 取 id 键。
		{"idA 与 midA（11-24 专辑页，mid 键那条在低地址）", prioFull(prioMidA, prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 2, SetAside: 1}},
		{"idA 两处与 midA", prioFull(prioIDA, prioMidA, prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 3, SetAside: 1}},
		{"idA 与 midA、midB", prioFull(prioMidA, prioIDA, prioMidB), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 3, SetAside: 2}},
		{"只有 idA（同第 4 版）", prioFull(prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 1}},

		// 仍不唯一或不认出。
		{"idA 与 idB", prioFull(prioIDA, prioIDB), prioID, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"idA、idB 与 midA", prioFull(prioIDA, prioMidA, prioIDB), prioID, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 3}},
		{"只有 midA、midB（同第 4 版）", prioFull(prioMidA, prioMidB), prioID, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"idA 与 midA 而扫描不完整", incomplete(prioMidA, prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordIncomplete, Qualified: 2}},
		{"idA 与 midA 而超过 64 处、完整", overCap(true, prioMidA, prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"idA 与 midA 而超过 64 处、不完整", overCap(false, prioMidA, prioIDA), prioID, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"id 键命中作废（id 不等）与 midA", withTail(prioFull(prioMidA), prioOtherID, prioMid), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioMidA, Qualified: 1, Voided: 1}},
		{"id 键命中作废（参数 0 不等）与 midA", withTail(prioFull(prioMidA), prioIDA, "000TestMid0002"), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioMidA, Qualified: 1, Voided: 1}},
		{"只有 midA（同第 4 版）", prioFull(prioMidA), prioID, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioMidA, Qualified: 1}},

		// 与待换算（只有 songid）：mid 键网址待换算，不计合格，也不计入 SetAside。
		{"只有 songid、idA 合格、midA 待换算：不等补全", prioFull(prioMidA, prioIDA), prioID, "",
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 1}},
		{"只有 songid、idA 与 idB 合格、midA 待换算", prioFull(prioIDA, prioMidA, prioIDB), prioID, "",
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
		{"只有 songid、没有 id 键、midA 待换算", prioFull(prioMidA), prioID, "",
			RecordConclusion{Verdict: RecordPending}},
		{"只有 songid、idA 合格、midA 待换算而扫描不完整", incomplete(prioIDA, prioMidA), prioID, "",
			RecordConclusion{Verdict: RecordPending, Qualified: 1}},

		// 只有 songmid：id 键网址参数 0 相同即合格。
		{"只有 songmid、idA 与 midA", prioFull(prioMidA, prioIDA), 0, prioMid,
			RecordConclusion{Verdict: RecordFound, URL: prioIDA, Qualified: 2, SetAside: 1}},
		{"只有 songmid、两种 id 不同的 id 键网址（参数 0 相同）", prioFull(prioOtherID, prioIDA), 0, prioMid,
			RecordConclusion{Verdict: RecordAmbiguous, Qualified: 2}},
	} {
		if got := ConcludeRecord(c.sc, c.songID, c.mid); got != c.want {
			t.Errorf("%s：ConcludeRecord = %+v，应为 %+v", c.name, got, c.want)
		}
	}
}
