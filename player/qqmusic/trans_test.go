package qqmusic

// 本文件只测翻译歌词（QQ 的 trans → sub_text）这条链：mergeTrans / alignByTime 的保序配对、
// attachTrans 的解密与降级、以及 toLyricLines 的接线。
//
// 这条链踩过两个坑，都是「按时间戳找同一行」：
//   - **照抄 cloudmusic 的 MergeTlyric 用精确时间戳匹配**——那对网易云成立（lrc 与 tlyric
//     同源同精度），对 QQ 则会静默丢掉九成翻译：trans 是标准 LRC（10ms 精度），主歌词是
//     QRC（毫秒精度），多数歌二者差 0~10ms。
//   - **按 ±20ms 就近匹配**——少数歌的 trans 取自另一套时间轴，同一行差到 99ms，翻译行
//     成片静默丢失（songID=1163799 丢 26/55 行；实测与统计见 transToleranceMs 的注释）。
//
// 故下面的样本刻意全部取自实测数据，且**没有一行是精确对齐的**。

import (
	"strings"
	"testing"
)

// 取自实测解密结果（songID=384442134 "Change Of Coast"）。注意时间：
//
//	QRC [0,5450]     ↔ trans [00:00.00]  差 0ms（"//" 占位，必须丢弃）
//	QRC [5451,...]   ↔ trans [00:05.45]  差 1ms
//	QRC [14943,...]  ↔ trans [00:14.94]  差 3ms
//	QRC [26488,...]  ↔ trans [00:26.48]  差 8ms
//
// [00:12.17] 是 trans 里的空文本行（QRC 无对应行），parseLRC 会跳过它。
const realQRC = `[ti:Change of Coast]
[ar:Neon Indian]
[offset:0]
[0,5450]Change(0,495) (495,495)of(990,495) (1485,495)Coast(1980,495)
[5451,6724]You  (5451,823)never (8099,188)knew (8287,436)me (8723,250)
[14943,2073]A-A-All (14943,814)the (15757,249)details (16006,1010)
[26488,3586]I (26488,881)don't (27369,250)know (27619,694)`

const realTrans = `[ti:Change of Coast]
[ar:Neon Indian]
[offset:0]
[00:00.00]//
[00:05.45]你永远不会了解现在的我
[00:12.17]
[00:14.94]所有的细节
[00:26.48]我不知道`

// 主用例：翻译必须落到正确的行上。
//
// 变异自证：把 transToleranceMs 改成 0（等价于精确匹配），本例立刻变红——这正是照抄
// MergeTlyric 会踩的坑，也是本文件存在的全部理由。
func TestMergeTransMatchesWithinTolerance(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	if len(lines) != 4 {
		t.Fatalf("len(lines) = %d, want 4；QRC 样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeTrans(lines, realTrans)

	want := []string{"", "你永远不会了解现在的我", "所有的细节", "我不知道"}
	for i, w := range want {
		if lines[i].SubText != w {
			t.Fatalf("lines[%d].SubText = %q, want %q（行 t=%.3f）；"+
				"trans 与 QRC 差 0~10ms，必须带容差配对而非精确匹配",
				i, lines[i].SubText, w, lines[i].Time)
		}
	}
}

// 取自实测解密结果（songID=1163799「結ンデ開イテ羅刹ト骸」，行首时间戳原样，逐字时间轴
// 简化为整行一段）。这首的 trans 不是 QRC 截断到 10ms，而是另一套时间轴：
//
//	QRC [28780] ↔ [00:28.75] 差 -30    QRC [37076] ↔ [00:37.03] 差 -46
//	QRC [31507] ↔ [00:31.48] 差 -27    QRC [40085] ↔ [00:40.10] 差 +15
//	QRC [34420] ↔ [00:34.39] 差 -30    QRC [42900] ↔ [00:42.94] 差 +40
//
// 旧的 ±20ms 就近匹配只配得上 40085 那一行。片头四行署名对应 "//"，[00:09.49] 是空行。
const driftQRC = `[618,5936]結ンデ開イテ羅刹ト骸 - 米津玄師(618,5936)
[6780,589]词：ハチ(6780,589)
[7594,713]曲：ハチ(7594,713)
[8514,983]编曲：ハチ(8514,983)
[28780,2559]片足無くした猫が笑う(28780,2559)
[31507,2681]「ソコ行ク御嬢サン遊ビマショ」(31507,2681)
[34420,2504]首輪に繋がる赤い紐は(34420,2504)
[37076,2648]片足の代わりになっちゃいない(37076,2648)
[40085,2474]や や や や 嫌 嫌 嫌(40085,2474)
[42900,2505]列成す卒塔婆の群れが歌う(42900,2505)`

const driftTrans = `[00:00.61]//
[00:06.78]//
[00:07.59]//
[00:08.51]//
[00:09.49]
[00:28.75]少了一只脚的猫笑着
[00:31.48]要往那边去的小姐我们来玩吧
[00:34.39]绑在项圈上的红绳子
[00:37.03]根本没办法代替一只脚
[00:40.10]讨厌讨厌讨厌
[00:42.94]排成一排的墓地牌子唱着歌`

// 翻译轨走另一套时间轴（同一行差几十毫秒）时，每行仍须配上自己的翻译。
//
// 变异自证：把 transToleranceMs 改回 20，本例变红（只剩 40085 一行配得上）——这正是
// 「两句有、两句没有」那个线上症状。
func TestMergeTransDriftedTimeline(t *testing.T) {
	lines, _, _ := parseLRC(driftQRC, 0)
	if len(lines) != 10 {
		t.Fatalf("len(lines) = %d, want 10；样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeTrans(lines, driftTrans)

	want := []string{"", "", "", "",
		"少了一只脚的猫笑着", "要往那边去的小姐我们来玩吧", "绑在项圈上的红绳子",
		"根本没办法代替一只脚", "讨厌讨厌讨厌", "排成一排的墓地牌子唱着歌"}
	for i, w := range want {
		if lines[i].SubText != w {
			t.Fatalf("lines[%d].SubText = %q, want %q（行 t=%.3f）；"+
				"trans 与 QRC 时间轴不同的歌，同一行差到几十毫秒，必须按行序配对",
				i, lines[i].SubText, w, lines[i].Time)
		}
	}
}

// 取自实测解密结果（songID=233911619「紅蓮華」，行首时间戳原样，逐字时间轴简化）。片头三行
// 署名挤在 1206~1224ms，首句歌词在 1226ms；翻译轨在同一时间戳 [00:01.22] 有两行——编曲
// 署名的 "//" 与首句的翻译。
const tieQRC = `[0,1205]紅蓮華 - LiSA(0,1205)
[1206,4]词：LiSA(1206,4)
[1211,12]曲：草野華余子(1211,12)
[1224,1]编曲：江口亮(1224,1)
[1226,6107]強くなれる理由を知った(1226,6107)
[7806,11346]僕を連れて進め(7806,11346)`

const tieTrans = `[00:00.00]QQ音乐享有本翻译作品的著作权
[00:01.20]//
[00:01.21]//
[00:01.22]//
[00:01.22]我已知晓能够变强的理由
[00:07.80]带着我 前进吧`

// 同一时间戳上有两行翻译时，按行序各归各的：首句拿到自己的翻译，署名行拿到 "//"（丢弃）。
//
// 变异自证：把 mergeTrans 里的 alignByTime 换回「每行取容差内最近、平局取先」，本例变红
// （首句与编曲署名行都选中先出现的 "//"）；把占位行改成配对前先滤掉，本例同样变红（首句的
// 翻译被配给只差 2ms 的编曲署名行）。
func TestMergeTransTieAtSameTimestamp(t *testing.T) {
	lines, _, _ := parseLRC(tieQRC, 0)
	if len(lines) != 6 {
		t.Fatalf("len(lines) = %d, want 6；样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeTrans(lines, tieTrans)

	want := []string{"", "", "", "", "我已知晓能够变强的理由", "带着我 前进吧"}
	for i, w := range want {
		if lines[i].SubText != w {
			t.Fatalf("lines[%d].SubText = %q, want %q（行 %q）；"+
				"同一时间戳的两行翻译必须按行序配给各自的主歌词行",
				i, lines[i].SubText, w, lines[i].Text)
		}
	}
}

// 取自实测（songID=244191667，主歌词为 LRC）：三行主歌词共用同一时间戳 01:03.679，而翻译轨
// 是另一版歌词、只有一行落在附近。
const dupLyric = `[00:57.866]You're my real life fantasy
[01:03.679]You complete my rhapsody
[01:03.679]Like the moon and the stars above
[01:03.679]Never try to admit this
[01:17.927]Right or wrong`

const dupTrans = `[00:57.79]你是我的白日幻想
[01:03.56]你完整了我的狂想曲
[01:09.22]就像星星和月亮
[01:14.76]不去计较
[01:17.85]对与错`

// 一行翻译只能配给一行主歌词：同时间戳的另外两行宁可留空，也不能贴上同一句翻译。
//
// 变异自证：把 mergeTrans 里的 alignByTime 换回「每行取容差内最近」（容差仍为 300），
// 本例变红（三行都贴上「你完整了我的狂想曲」）；把 alignByTime 的回溯改成先退 trans 再退
// lines，本例同样变红（翻译落到三行中的最后一行）。
func TestMergeTransOneTranslationPerLine(t *testing.T) {
	lines, _, _ := parseLRC(dupLyric, 0)
	if len(lines) != 5 {
		t.Fatalf("len(lines) = %d, want 5；样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeTrans(lines, dupTrans)

	want := []string{"你是我的白日幻想", "你完整了我的狂想曲", "", "", "对与错"}
	for i, w := range want {
		if lines[i].SubText != w {
			t.Fatalf("lines[%d].SubText = %q, want %q（行 %q）；"+
				"一行翻译只能配给一行主歌词",
				i, lines[i].SubText, w, lines[i].Text)
		}
	}
}

// 翻译轨在容差内、本行翻译之前多出一行（异版本翻译会这样）时，不能被它抢走本行：配对数
// 相同，取时差更小的那条。构造数据：在 realTrans 的第 1 行翻译前 200ms 插一行。
//
// 变异自证：把 alignByTime 换成双指针贪心（容差内遇到就配），或去掉它 better 里的总时差
// 比较，本例均变红（第 1 行配上多出来的那行）。
func TestMergeTransExtraLineDoesNotSteal(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	mergeTrans(lines, `[00:00.00]//
[00:05.25]多出来的一行
[00:05.45]你永远不会了解现在的我
[00:14.94]所有的细节
[00:26.48]我不知道`)

	want := []string{"", "你永远不会了解现在的我", "所有的细节", "我不知道"}
	for i, w := range want {
		if lines[i].SubText != w {
			t.Fatalf("lines[%d].SubText = %q, want %q；多出来的翻译行抢走了本行的翻译",
				i, lines[i].SubText, w)
		}
	}
}

// "//" 是 QQ 的「本行不翻译」占位符，绝不能进 sub_text，否则 OBS 副歌词行显示 "//"。
//
// 变异自证：删掉 mergeTrans 里的 transNoLyricMark 判断，本例变红。
func TestMergeTransDropsNoLyricMark(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	mergeTrans(lines, realTrans)
	if lines[0].SubText != "" {
		t.Fatalf("lines[0].SubText = %q, want \"\"；\"//\" 是不翻译占位符，不是翻译内容",
			lines[0].SubText)
	}
}

// QQ 塞在翻译轨首行的自家文案（版权声明 / AI 翻译来源声明）绝不能进 sub_text。
// 两条文案都取自实测：前者 songID=9109354(Blank Space)，后者 songID=106557047 / 322960。
//
// 变异自证：把 transMetaKeywords 清空，本例变红。
func TestMergeTransDropsQQMetaLines(t *testing.T) {
	for _, meta := range []string{
		"QQ音乐享有本翻译作品的著作权",
		"以下歌词翻译由文曲大模型提供",
	} {
		lines, _, _ := parseLRC(realQRC, 0)
		// 让它精确落在第一行（t=0.000）上——真实数据里就是这么配上的
		mergeTrans(lines, "[00:00.00]"+meta+"\n[00:05.45]你永远不会了解现在的我")
		if lines[0].SubText != "" {
			t.Fatalf("lines[0].SubText = %q, want \"\"；这是 QQ 的产品文案不是翻译，"+
				"会被配到主歌词首行（多为标题行）直接推上 OBS", lines[0].SubText)
		}
		// 同一次调用里，真翻译必须照常合上——过滤不能误伤正常行
		if lines[1].SubText != "你永远不会了解现在的我" {
			t.Fatalf("lines[1].SubText = %q；过滤 QQ 文案时误伤了真翻译", lines[1].SubText)
		}
	}
}

// 关键词是子串匹配：文案改了措辞、但词根还在时仍须挡住（这正是不用全文匹配的理由）。
func TestMergeTransMetaKeywordIsSubstring(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	// 杜撰一条 QQ 尚未用过、但含同一词根的文案
	mergeTrans(lines, "[00:00.00]本作品版权归腾讯音乐所有")
	if lines[0].SubText != "" {
		t.Fatalf("lines[0].SubText = %q, want \"\"；关键词须按子串匹配，"+
			"否则 QQ 一改文案就漏", lines[0].SubText)
	}
}

// 超出容差的翻译行绝不能被拉过来配上：宁可没有 sub_text，也不能配错行。
//
// 变异自证：把 transToleranceMs 放大到 500，本例变红（差 499ms）。
func TestMergeTransRejectsBeyondTolerance(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	// 一条翻译落在第 1 行后 499ms，超出 300ms 容差
	mergeTrans(lines, `[00:05.95]不该被配上的翻译`)
	for i := range lines {
		if lines[i].SubText != "" {
			t.Fatalf("lines[%d].SubText = %q, want \"\"；差 500ms 的翻译行不该匹配", i, lines[i].SubText)
		}
	}
}

// 无翻译（中文歌 trans 恒为空）时不得改动任何东西。
func TestMergeTransEmptyIsNoop(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	mergeTrans(lines, "")
	for i := range lines {
		if lines[i].SubText != "" {
			t.Fatalf("lines[%d].SubText = %q, want \"\"；空 trans 必须是 no-op", i, lines[i].SubText)
		}
	}
}

// 接线：attachTrans 必须真的把明文 trans 合进去。
//
// 变异自证：把 fetchLRC 里的 attachTrans 调用删掉不会被本例发现（那需要联网），但把
// attachTrans 内部的 mergeTrans 调用删掉，本例变红。
func TestAttachTransMergesPlaintext(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	attachTrans(lines, realTrans, 0) // crypt=0 且非 hex → 按明文处理
	if lines[1].SubText != "你永远不会了解现在的我" {
		t.Fatalf("lines[1].SubText = %q, want 翻译文本；attachTrans 没把明文 trans 合进去",
			lines[1].SubText)
	}
}

// 降级：trans「是密文但解不开」时，必须跳过翻译且**不得**污染主歌词。
//
// 这是本文件最重要的一条：坏密文若被放行给 parseLRC，整串 hex 无时间戳会命中它末尾
// 「无时间戳按时长均分」的兜底，凭空造出假翻译行乱配 sub_text。
//
// 变异自证：把 attachTrans 里 err != nil 分支的 return 删掉（改成继续 mergeTrans），
// 本例变红。
func TestAttachTransBadCiphertextDoesNotPoisonLyrics(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	wantText := lines[1].Text

	// 合法 hex、长度是 8 的倍数，但 3DES 解出来不是 zlib → 「是密文但坏了」
	badCipher := strings.Repeat("ab", 64)
	attachTrans(lines, badCipher, 1)

	for i := range lines {
		if lines[i].SubText != "" {
			t.Fatalf("lines[%d].SubText = %q, want \"\"；解不开的 trans 必须整条跳过，"+
				"绝不能放行给 parseLRC 触发均分兜底造出假翻译行", i, lines[i].SubText)
		}
	}
	if lines[1].Text != wantText {
		t.Fatalf("主歌词被翻译解密失败带坏了: %q, want %q；翻译失败绝不能影响主歌词",
			lines[1].Text, wantText)
	}
}

// 接线：SubText 必须从内部 lyricLine 流到对外的 player.LyricLine（all_lyrics 的载荷）。
//
// 变异自证：把 toLyricLines 里的 l.SubText 改回字面量 ""（即修复前的样子），本例变红。
// 这条是「接线测试」——mergeTrans 再正确，断在这里 sub_text 一样出不去。
func TestToLyricLinesCarriesSubText(t *testing.T) {
	lines, _, _ := parseLRC(realQRC, 0)
	mergeTrans(lines, realTrans)

	out := toLyricLines(lines, -0.4)
	if len(out) != len(lines) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(lines))
	}
	if out[1].SubText != "你永远不会了解现在的我" {
		t.Fatalf("out[1].SubText = %q, want 翻译文本；toLyricLines 没把 SubText 传给 BuildLyricLine",
			out[1].SubText)
	}
	// 顺带钉住：sub_text 的加入不能动到既有的 play_time 语义（timestamp + offset，offset 为负即提前）
	if got, want := out[1].PlayTime, float32(5.451-0.4); got != want {
		t.Fatalf("out[1].PlayTime = %v, want %v；SubText 不该影响 play_time", got, want)
	}
}
