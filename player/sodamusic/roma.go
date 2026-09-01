package sodamusic

import (
	"fmt"
	"strings"
	"unicode"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// —— 汽水音译：从酷狗「借」，字级对齐 + 行级回落两层 ————————————————————————————
//
// 汽水平台自身无音译源（issue #34：KRC 不含 [language:] 内嵌轨，sharedState 也无独立音译
// 字段），故 parseSodaLyrics 解出的行 RomaText 恒空。本文件在主歌词/翻译发出之后，用当前
// 歌的歌名 / 歌手 / 时长去酷狗歌词接口借一份带 type=0 音译轨的歌词（klyric.Fetch 传空 hash
// 即走「catalog 按歌手+歌名搜 canonical hash + krcs 按 keyword ±5s 时长严格匹配」，全程公网
// HTTP、不依赖酷狗进程），再把音译搬进汽水行的 RomaText。
//
// **这是借来的、非平台原生的能力**：跨平台（字节曲库 vs 酷狗曲库）歌名搜索无法保证命中——
// 可能搜不到对应曲、或匹配到不同版本 / 母带 / 翻唱；命中后两边曲库的歌词文本也未必逐字一致。
// 故 README 能力表标 ⚠ 而非 ✔，匹配不上的行一律留空。
//
// **为什么要字级对齐**：汽水与酷狗**断行边界不同**（实测 BoA - Only One：酷狗 2 行 vs 汽水
// 3 行等 M:N）。纯行级归一化文本匹配对断行不同的行整段对不上、留空（实测 56/65≈86%）。字级
// 对齐把酷狗音译**保留到字**，按**字序**贴到汽水主歌词的字上，再按汽水行边界重拼成行级
// RomaText，从而覆盖断行差异段。**主歌词永远是汽水、不可变**，我们只往汽水行写 roma_text。

// maxAlignRunes 是字级对齐（LCS）两侧序列的规模上限。LCS 是 O(n*m) 时空，一首歌的主歌词
// 撑死几百个字，2000 已是极宽的余量；真超了（异常 / 拼接错的超长轨）就跳过字级、只走行级
// 回落，绝不让一次借音译吃掉一大块内存。每首歌只跑一次、非热路径（AGENTS §0.1 稳定优先）。
const maxAlignRunes = 2000

// mergeKugouRoma 把酷狗行的音译对齐、搬进汽水行的 RomaText，就地改写 sodaLines，返回对齐
// 上的总行数（字级 + 行级）。分层降级见 mergeKugouRomaStats。
func mergeKugouRoma(sodaLines []krc.Line, kugouLines []klyric.Line) int {
	return mergeKugouRomaStats(sodaLines, kugouLines).matched
}

// romaMergeStats 是一次借音译合并的分层战果，供临时诊断日志端到端核对。
type romaMergeStats struct {
	charLines int    // 字级对齐产出的行数（覆盖断行差异 M:N 的主战场）
	lineLines int    // 行级归一化文本回落产出的行数
	matched   int    // = charLines + lineLines，实际写进 RomaText 的总行数
	sample    string // 首个字级对齐行的「[汽水行文本] → [重拼音译]」，供肉眼核对与屏幕逐字比对
}

// mergeKugouRomaStats 是分层降级的核心（AGENTS §2 承重不变量；**绝不劣化现状**）：
//
//	第一层  字级对齐：酷狗音译保留到字（krc.Line.RomaWords），按字序对齐到汽水主歌词的字，
//	                 再按汽水行边界重拼行级 RomaText。覆盖断行差异 M:N 段。见 alignCharLevel。
//	第二层  行级回落：字级没覆盖的行，退回**现有**归一化文本匹配（现状 86% 的逻辑原样保留）。
//	第三层  留空：两层都不中的行 RomaText 保持空。
//
// **为什么这样分层不会劣化**：两层对同一行若都能填，字级优先——而断行一致的行两层结果本就
// 相同（字级重拼 = 该酷狗行片段 Join = 行级 RomaText），断行不同的行只有字级能填、行级本就
// 空。故合并结果是行级单独结果的**超集**：行级能填的行合并后一定也填了，字级只多不少。
// 酷狗无字级音译（老 KRC / LRC 回落）时第一层自动空转，整体退回纯行级 = 引入本改动前的现状。
//
// **只写 RomaText，绝不碰 Text（主歌词）/ SubText（翻译）/ Detailed（逐字）**——三轨独立，
// 任一层失败 / 未命中 / 对不上一律安全降级为「该行留空」，绝不 panic、绝不波及主歌词与翻译。
func mergeKugouRomaStats(sodaLines []krc.Line, kugouLines []klyric.Line) romaMergeStats {
	var st romaMergeStats
	if len(sodaLines) == 0 || len(kugouLines) == 0 {
		return st
	}

	// ── 第一层：字级对齐 ──（汽水行下标 → 重拼行级音译，只含整行每字都对齐上的行）
	charRoma := alignCharLevel(sodaLines, kugouLines)

	// ── 第二层：行级归一化文本匹配（现状逻辑，原样保留作回落）──
	// 建「归一化主歌词文本 → 音译」索引，只收酷狗侧**有行级音译**的行。归一化用
	// player.NormalizeLyricText（抹掉标点 / 大小写 / 空白差异），与 all_lyrics 跨源对齐同口径。
	// 副歌重复行文本相同 → 音译也相同，用 map 后写覆盖前写无妨。
	romaByText := make(map[string]string, len(kugouLines))
	for _, kl := range kugouLines {
		if kl.RomaText == "" {
			continue
		}
		key := player.NormalizeLyricText(kl.Text)
		if key == "" {
			continue
		}
		romaByText[key] = kl.RomaText
	}

	for i := range sodaLines {
		// 字级优先；字级没覆盖的行回落行级；两层都不中则留空。**只写 RomaText**。
		if r, ok := charRoma[i]; ok {
			sodaLines[i].RomaText = r
			st.charLines++
			if st.sample == "" {
				st.sample = fmt.Sprintf("[%s] → [%s]", sodaLines[i].Text, r)
			}
			continue
		}
		key := player.NormalizeLyricText(sodaLines[i].Text)
		if key == "" {
			continue
		}
		if r, ok := romaByText[key]; ok {
			sodaLines[i].RomaText = r
			st.lineLines++
		}
	}
	st.matched = st.charLines + st.lineLines
	return st
}

// runeRef 是展平后的一个字级 rune 及其归属：汽水侧 id = 行 slice 下标，酷狗侧 id = token 全局下标。
type runeRef struct {
	r  rune
	id int
}

// lcsPair 是 LCS 对齐出的一对下标（a=汽水 rune 下标，b=酷狗 rune 下标）。
type lcsPair struct{ a, b int }

// alignCharLevel 按**字序**把酷狗音译对齐到汽水主歌词的字，再按汽水行边界重拼行级音译，返回
// 「汽水行 slice 下标 → 重拼行级 RomaText」，只含**整行每字都对齐上、且用到的酷狗 token 无一
// 跨行**的行。酷狗无字级音译 / 汽水无字 / 规模超限 → 返回 nil（交给行级回落，不劣化）。
//
// 算法（双方是「同一首歌的主歌词字序」，仅断行 / 个别字 / 标点不同）：
//  1. 酷狗侧展平成字级 rune 序列，每 rune 标注所属 token（token 携带该字的音译片段）。只收
//     **字级可靠**的行：有逐字且 len(RomaWords)==len(Detailed.Words)（音译片段与字一一对应）。
//     不成立的行（老 KRC 无逐字、LRC 回落、片段数与字数不符）整行不贡献字级，但其行级 RomaText
//     仍留给第二层回落，不丢信息。
//  2. 汽水侧展平成字级 rune 序列，每 rune 标注所属汽水行。
//  3. 两序列做 LCS（按归一化 rune 相等），得字序配对。LCS 天然容忍两侧的插入 / 删除（标点差、
//     个别字差、断行差），**不要求全等**。
//  4. 聚合：每个汽水行对齐了几个字、用到哪些酷狗 token；每个 token 的字落到了哪些汽水行。
//  5. 接受判据（宁缺毋错）：整行**每个字**都对齐了（full coverage，保证重拼不缺字）**且**用到
//     的 token 无一跨行（no straddle，保证不把同一 token 的音译重复贴到相邻两行）。满足则把该行
//     用到的 token 的音译片段按序 Join + TrimSpace 作为该行重拼音译；否则该行不产字级、回落。
//     断行差异（M:N）主场景恰好满足：文本逐字相同、只断行不同 → 每行全覆盖、token 不跨行。
func alignCharLevel(sodaLines []krc.Line, kugouLines []klyric.Line) map[int]string {
	// ── 酷狗侧：token 音译 + 字级 rune 序列（只收字级可靠的行）──
	var tokenRoma []string // tokenRoma[t] = 第 t 个字的音译片段（未 Trim，保留排版空格）
	var kugouRunes []runeRef
	for _, kl := range kugouLines {
		words := kl.Detailed.Words
		frags := kl.RomaWords
		if len(words) == 0 || len(frags) != len(words) {
			continue // 字级不可靠：整行不参与字级（行级 RomaText 仍留给第二层）
		}
		for j, w := range words {
			tokID := len(tokenRoma)
			tokenRoma = append(tokenRoma, frags[j])
			for _, r := range w.Text {
				if nr, ok := normLyricRune(r); ok {
					kugouRunes = append(kugouRunes, runeRef{nr, tokID})
				}
			}
		}
	}
	if len(kugouRunes) == 0 {
		return nil // 酷狗无任何字级音译（老 KRC / LRC 回落）——字级层空转，交给行级
	}

	// ── 汽水侧：字级 rune 序列，每 rune 标注所属行下标；并记每行的总字数（供 full coverage 判据）──
	var sodaRunes []runeRef
	lineRuneTotal := make([]int, len(sodaLines))
	for i := range sodaLines {
		for _, r := range sodaLines[i].Text {
			if nr, ok := normLyricRune(r); ok {
				sodaRunes = append(sodaRunes, runeRef{nr, i})
				lineRuneTotal[i]++
			}
		}
	}
	if len(sodaRunes) == 0 {
		return nil
	}

	// 规模闸：LCS O(n*m)，超限跳过字级（行级回落兜底），绝不吃大内存。
	if len(sodaRunes) > maxAlignRunes || len(kugouRunes) > maxAlignRunes {
		return nil
	}

	// ── LCS 对齐 ──
	sa := make([]rune, len(sodaRunes))
	for i, x := range sodaRunes {
		sa[i] = x.r
	}
	ka := make([]rune, len(kugouRunes))
	for i, x := range kugouRunes {
		ka[i] = x.r
	}
	pairs := lcsPairs(sa, ka)

	// ── 聚合 ──
	tokenLines := make(map[int]map[int]bool) // tokenID → set(汽水行下标)：判 straddle
	lineAligned := make([]int, len(sodaLines))
	lineTokens := make([][]int, len(sodaLines)) // 汽水行 → 有序去重的 token 序列（同 token 的字连续）
	for _, p := range pairs {
		li := sodaRunes[p.a].id
		tok := kugouRunes[p.b].id
		lineAligned[li]++
		if tokenLines[tok] == nil {
			tokenLines[tok] = make(map[int]bool)
		}
		tokenLines[tok][li] = true
		ts := lineTokens[li]
		if len(ts) == 0 || ts[len(ts)-1] != tok {
			lineTokens[li] = append(ts, tok)
		}
	}

	// ── 接受判据：full coverage + no straddle ──
	out := make(map[int]string)
	for li := 0; li < len(sodaLines); li++ {
		total := lineRuneTotal[li]
		if total == 0 || lineAligned[li] != total {
			continue // 有字没对齐上 → 重拼会缺字，不接受，回落行级
		}
		straddle := false
		for _, tok := range lineTokens[li] {
			if len(tokenLines[tok]) > 1 {
				straddle = true // 该 token 的字跨了汽水行 → 会把同一音译贴到两行，不接受
				break
			}
		}
		if straddle {
			continue
		}
		var b strings.Builder
		for _, tok := range lineTokens[li] {
			b.WriteString(tokenRoma[tok])
		}
		roma := strings.TrimSpace(b.String())
		if roma == "" {
			continue
		}
		out[li] = roma
	}
	return out
}

// lcsPairs 求 x、y 两 rune 序列的最长公共子序列，返回配对下标（按 x、y 升序、彼此保序）。
// O(n*m) 时空；n、m 已被 maxAlignRunes 钳到 ≤2000，dp 表用单块扁平 []int32（一次分配）。
func lcsPairs(x, y []rune) []lcsPair {
	n, m := len(x), len(y)
	if n == 0 || m == 0 {
		return nil
	}
	w := m + 1
	dp := make([]int32, (n+1)*w) // dp[i*w+j] = LCS(x[i:], y[j:]) 长度
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case x[i] == y[j]:
				dp[i*w+j] = dp[(i+1)*w+(j+1)] + 1
			case dp[(i+1)*w+j] >= dp[i*w+(j+1)]:
				dp[i*w+j] = dp[(i+1)*w+j]
			default:
				dp[i*w+j] = dp[i*w+(j+1)]
			}
		}
	}
	var pairs []lcsPair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			pairs = append(pairs, lcsPair{i, j})
			i++
			j++
		case dp[(i+1)*w+j] >= dp[i*w+(j+1)]:
			i++
		default:
			j++
		}
	}
	return pairs
}

// smallToLargeKana 把**小书写体假名（sutegana）**折叠到对应的**大书写体假名**，只在
// normLyricRune 的字级归一化里用。汽水歌词源常用小写假名异体（实测 Aimer - ninelie：汽水
// サィレンス 的 ィ(U+30A3 小写) vs 酷狗 サイレンス 的 イ(U+30A4 大写)），二者**同音、仅书写体
// 大小不同**，字级对齐时应视作同字，否则整行对不上而留空。
//
// **本表只收「同音的大小书写变体」，绝不含清音 / 浊音 / 半浊音之别。** 浊点 / 半浊点是**不同
// 音**：つ↔づ、は↔ば↔ぱ、ふ↔ぶ↔ぷ、し↔じ、ツ↔ヅ、シ↔ジ 等归一化会把不同音的字误配（实测
// 汽水 並ふ / イメーヅ vs 酷狗 並ぶ / イメージ 就该保持不等、留空，宁缺毋错）。故 っ(小 tsu,
// U+3063)→つ(U+3064) 收入本表（同音的小写体），而 づ(U+3065) / ヅ(U+30C5，均 zu 浊音)绝不出现
// 在表里任何一侧。假名大小写在 Unicode 里不规则、无法用范围偏移，故显式逐对枚举。
var smallToLargeKana = map[rune]rune{
	// 平假名：小书写体 → 大书写体
	'ぁ': 'あ', 'ぃ': 'い', 'ぅ': 'う', 'ぇ': 'え', 'ぉ': 'お',
	'っ': 'つ',
	'ゃ': 'や', 'ゅ': 'ゆ', 'ょ': 'よ',
	'ゎ': 'わ',
	'ゕ': 'か', 'ゖ': 'け',
	// 片假名：小书写体 → 大书写体
	'ァ': 'ア', 'ィ': 'イ', 'ゥ': 'ウ', 'ェ': 'エ', 'ォ': 'オ',
	'ッ': 'ツ',
	'ャ': 'ヤ', 'ュ': 'ユ', 'ョ': 'ヨ',
	'ヮ': 'ワ',
	'ヵ': 'カ', 'ヶ': 'ケ',
}

// normLyricRune 是 player.NormalizeLyricText 的**逐 rune 版**，只多一步小写假名折叠：先把小书写
// 体假名折叠到大书写体（见 smallToLargeKana），再按同一口径「字母 / 数字 → 小写并保留，其余
// （标点 / 空白 / CJK 标点）→ 丢弃（ok=false）」处理。折叠只发生在本函数（sodamusic 字级对齐），
// 行级 player.NormalizeLyricText 不变——两者对普通字仍同口径，仅小写假名在字级侧多折叠一层。
// CJK / 假名 / 谚文 IsLetter 为真、ToLower 恒等，照常保留。
func normLyricRune(r rune) (rune, bool) {
	if large, ok := smallToLargeKana[r]; ok {
		r = large // 小书写体假名 → 大书写体，再走下面的通用归一化
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return unicode.ToLower(r), true
	}
	return 0, false
}

// fetchKugouRoma 异步向酷狗借音译并补进 sodaLines，把结果经 romaCh 交回主循环。每首歌一个。
//
// **绝不阻塞、绝不波及主歌词/翻译**：换歌分支已先把（音译为空的）主歌词与翻译发出，本 goroutine
// 事后补音译。酷狗那趟是秒级网络调用（多级搜索 + 下载），任何失败 / 未命中 / 对不上都只是让音译
// 留空，主歌词首帧不受一丝拖累（AGENTS §0.1：主歌词断供是直播事故，稳定 > 功能完整度）。
//
// **为什么把结果交回主循环、而不在这里直接 Emit**：currentLyrics 由 runSession 单写者独占
// （lyric_update 每轮读它取当前行）。若本 goroutine 直接 Emit 一份带音译的 all_lyrics，主循环
// 手里的 currentLyrics 仍是空音译版，其后每条 lyric_update 继续发空 roma_text——两个事件自相
// 矛盾。交回主循环由它统一换掉 currentLyrics 再重发 all_lyrics，才能让 all_lyrics 与其后每条
// lyric_update 都带音译（与其余四家一致：两个事件同源）。
//
// **为什么无需 ctx 取消 / 代次淘汰**：本 goroutine 从不 Emit，只往 romaCh（每首歌一个、容量 1）
// 送一次结果。换歌时主循环给下一首建新 romaCh，上一首的迟到结果落进被弃置的旧 channel 随之 GC，
// 主循环只认当前 romaCh，故过期结果不可能被误用（这正是它与封面 goroutine 的关键区别——封面
// 直接 Emit，才必须靠 coverCancel 挡住迟到回写盖掉当前歌曲）。缓冲容量 1 保证送出不阻塞、
// goroutine 必能退出。
func (p *SodaMusicPlayer) fetchKugouRoma(romaCh chan<- []krc.Line, sodaLines []krc.Line, name, singer string, durationMs int) {
	// 传空 hash → 走 Tier 2/3（按歌手+歌名 catalog 搜 + ±5s 时长严格匹配），不依赖酷狗 hash。
	kugouLines, err := klyric.Fetch("", durationMs, name, singer)
	if err != nil {
		log.Detail("酷狗音译借取失败：按「%s - %s」搜索出错：%v", singer, name, err)
		return
	}
	if len(kugouLines) == 0 {
		log.Detail("酷狗音译未命中：按「%s - %s」搜不到对应曲，音译留空", singer, name)
		return
	}
	st := mergeKugouRomaStats(sodaLines, kugouLines)
	if st.matched == 0 {
		log.Detail("酷狗命中「%s - %s」%d 行，但与汽水主歌词无一对齐（版本/母带/翻唱不同？），音译留空",
			singer, name, len(kugouLines))
		return
	}
	sample := st.sample
	if sample == "" {
		sample = romaSample(sodaLines) // 全靠行级回落时字级无样本，退回首个带音译行
	}
	log.Detail("酷狗音译已对齐：按「%s - %s」命中酷狗 %d 行；字级 %d + 行级回落 %d = 共 %d/%d 行、留空 %d 行；样本 %s",
		singer, name, len(kugouLines), st.charLines, st.lineLines, st.matched, len(sodaLines), len(sodaLines)-st.matched, sample)
	romaCh <- sodaLines
}

// romaSample 取首个带音译的行拼成「[主歌词] → [音译]」，供临时诊断日志肉眼核对对齐是否正确。
func romaSample(lines []krc.Line) string {
	for _, l := range lines {
		if l.RomaText != "" {
			return fmt.Sprintf("[%s] → [%s]", l.Text, l.RomaText)
		}
	}
	return "(无)"
}
