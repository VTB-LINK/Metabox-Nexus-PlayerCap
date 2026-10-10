package qqmusic

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// httpClient 用于所有 QQ 音乐公开 API 调用（c.y.qq.com / u.y.qq.com 取词与封面）。
// **必须有超时**：不设 Timeout 字段的 http.Client 其 Timeout 零值为 0，走 DefaultTransport（只有
// DialContext 30s + TLSHandshakeTimeout 10s，无 ResponseHeaderTimeout）——握手成功但对端
// 不再吐数据时（强制门户 / 挂死透明代理照常 ACK 但不转发），Do()/io.ReadAll 无界阻塞。
// 而 fetchLRC / fetchCoverURL 在 qqmusic.go 的 poll 循环体内**同步**调用，卡死后该
// goroutine 再也回不到 StopCh 检查；更坏的是 router 对卡死的 playing 状态永不过期，
// qqmusic 会变成 activeNames 里的永久幽灵，只能重启进程恢复。10s 足够覆盖慢网络。
var httpClient = &http.Client{Timeout: 10 * time.Second}

// errNotCiphertext 表示输入**根本不是** QRC 密文（连 hex 或 DES 分组长度都不满足），
// 区别于「确实是密文、但解不开」。这两类必须区分对待：
//   - 不是密文 → 内容很可能就是明文 LRC（crypt 标志误报 / isHex 假阳性）→ 原样放行
//   - 是密文但坏了 → 绝不能把密文当歌词往下传（会被 parseLRC 当成一行 hex 推上 OBS）
//
// 不区分的代价是实测过的：把两类都当错误，会让「crypt=1 但服务端回明文」这种
// 本来能正常显示歌词的情况变成 0 行。
var errNotCiphertext = errors.New("输入不是 QRC 密文")

// QRC 3DES key from QQ Music
var qrc3DESKey = []byte("!@#)(*$%123ZXC!@!@#)(NHL")

// lyricLine 内部歌词行（解析中间结果）
type lyricLine struct {
	Index int
	Time  float32
	Text  string
	// SubText 翻译（QQ 的 trans 字段，与主歌词同一次响应返回；无翻译时为空）。
	// 由 mergeTrans 按行序对齐填入（见 alignByTime）。
	SubText string
	// RomaText 音译/罗马音（QQ 的 roma 字段，与主歌词同一次响应返回；无音译时为空）。
	// 由 mergeRoma 按时间就近填入。**与 SubText 是相互独立的两条轨，绝不互相覆盖**。
	RomaText string
	// Detailed 逐字时间轴（QRC 才有；LRC 源为零值，序列化成 {}）。
	// 此处 PlayTime 尚未套 offset，由 applyDetailedOffset 统一处理。
	Detailed player.LyricTextDetailed
}

// transToleranceMs 是翻译行与主歌词行配对时允许的最大时间差。配对本身由 alignByTime 按
// 行序做，本容差只是兜底：超出它的两行不许配。
//
// QQ 的 trans 是标准 LRC（`[mm:ss.xx]`，10ms 精度），主歌词是 QRC（毫秒精度）。trans 的
// 时间戳**不一定取自 QRC 的时间轴**——2026-10 用 musicu.fcg 实取日/韩/欧美榜 1210 首有翻译
// 的歌，按行序配对后：
//
//	1181 首  同一行差 0~10ms（trans = QRC 时间截断到 10ms）
//	  24 首  有歌词行差 11~99ms（另一套时间轴；如 songID=1163799「結ンデ開イテ羅刹ト骸」）
//	   3 首  只有片头 "//" 署名行偏（两首差 50ms 级、一首差到秒级），歌词行 ≤10ms
//	   2 首  翻译取自另一版歌词（断行不同、行数更多），正确配对的时差实测到 ~460ms
//
// 旧实现按 ±20ms 就近匹配：第二类 24 首全部静默丢翻译，共丢 178/1022 行（1163799 丢 26/55），
// OBS 上表现为副歌词「有几句、没几句」；另有 4 首因平局取先或一句配多行而丢或错配（见
// alignByTime）。精确匹配更糟，连第一类都丢九成（cloudmusic 的 MergeTlyric 用精确匹配，
// 那是因为网易云的 lrc 与 tlyric 同源同精度，不可照抄）。
//
// 取 300 是保守取值，不是实测边界：覆盖同轴最大偏差 99ms 并留三倍余量。放到 500 在上述语料里
// 能给异版本那两首多显示 6 行翻译且无错配；不追，是因为容差越大，某行的真实对应落在容差外时，
// 「配对数优先」能从越远处把邻行的翻译拉过来（见 alignByTime 的已知局限）——错配在 OBS 上
// 比留空更糟（TestMergeTransRejectsBeyondTolerance）。
const transToleranceMs = 300

// transNoLyricMark 是 QQ 的「本行不翻译」占位符（上述 1210 首中位数每首 4 行，多则数十行，
// 集中在片头署名与间奏）。不过滤会让 OBS 的副歌词行直接显示 "//"。
const transNoLyricMark = "//"

// transMetaKeywords 命中的翻译行是 QQ 自己的产品文案，不是翻译内容，必须丢弃。
//
// QQ 会在翻译轨的**首行**（t=0.00）塞入声明，实测两种文案：
//
//	"QQ音乐享有本翻译作品的著作权"    —— 版权声明
//	"以下歌词翻译由文曲大模型提供"     —— AI 翻译来源声明（较新，16 首取样中 2 首命中）
//
// 它们会被配到主歌词第一行上（多为标题行），直接推成 OBS 的副歌词。
//
// **用关键词而非全文匹配**：这是随时会改的产品文案——单是取样就已经有两种句式，
// 全文匹配挡得住今天这条、挡不住下一条。关键词取三个词根，覆盖「版权」与「翻译来源」
// 两类句式；歌词的中文翻译里出现它们的概率极低，误杀风险可忽略。
//
// 局限（写在这里免得下次当成 bug 查）：若 QQ 换成不含这三个词根的句式（如「翻译：某模型」），
// 本过滤会漏。届时补词根即可，不必改结构。
var transMetaKeywords = []string{"著作权", "版权", "歌词翻译"}

// transIsDroppable 判断这行翻译该不该丢：不翻译占位符，或 QQ 自己的声明文案。
func transIsDroppable(text string) bool {
	if text == transNoLyricMark {
		return true
	}
	for _, kw := range transMetaKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// mergeTrans 把 QQ 的翻译歌词（标准 LRC）合并进主歌词的 SubText：先 alignByTime 按行序
// 配对，再丢掉占位符与 QQ 自家文案。
//
// 占位行（"//"、版权声明）必须**先参与配对、后丢弃**：它们在翻译轨里占着对应主歌词行
// （标题行、词曲署名行）的位置。先滤掉再配，紅蓮華那种「编曲署名行与首句歌词只差 2ms」的
// 地方，首句的翻译会被配给署名行。
//
// 匹配不上或翻译缺失一律保持 SubText 为空：翻译是纯增量的锦上添花，任何情况下都不能
// 反过来影响主歌词。
func mergeTrans(lines []lyricLine, transLRC string) {
	if transLRC == "" || len(lines) == 0 {
		return
	}
	transLines, _, _ := parseLRC(transLRC, 0)
	if len(transLines) == 0 {
		return
	}
	for i, j := range alignByTime(lines, transLines, transToleranceMs) {
		if j < 0 || transIsDroppable(transLines[j].Text) {
			continue
		}
		lines[i].SubText = transLines[j].Text
	}
}

// alignByTime 把 trans 的行按行序一对一配到 lines 上，返回 match：match[i] 是 lines[i]
// 配上的 trans 下标，-1 = 不配。
//
// 约束：配对不交叉（保序）、一对一、两行时间差 ≤ tolMs；在此之下**先求配上的行数最多，
// 再求总时差最小**（LCS 式 DP）。QQ 的翻译轨与主歌词逐行平行（见 transToleranceMs），
// 行数相等且每行都在容差内时，唯一满足的就是逐行配对，与时间戳差多少无关。
//
// 为什么不是旧实现的「每行找时间最近的那条」——它在偏差之外还有两种错法，旧实现上都实测到过：
//   - 平局取先：紅蓮華（songID=233911619）的翻译轨在同一时间戳 1220 有两行（"//" 与首句
//     翻译），首句选中先出现的 "//"，翻译丢失。保序配对按行序各取各的。
//   - 一句配多行：songID=326486987 的署名行「Mastered by…」（789ms）离下一行的翻译「疯掉」
//     （790）比离自己的 "//"（780）更近，于是「疯掉」同时出现在两行上。容差放宽后更甚：
//     songID=244191667 有三行主歌词同为 63679，按 300ms 就近会把同一句翻译贴到三行上。
//     一对一下只有一行能配上，其余留空。
//
// 为什么不是双指针贪心（容差内遇到就配）：1210 首上结果与本函数逐行一致，但它在「容差内
// 先出现一条多余的翻译行」时会抢先配上、显示错译；DP 在同样行数下选总时差更小的那种。
//
// 已知局限：「配对数优先」意味着某行的真实对应落在容差外时，容差内的邻行翻译会被配上来。
// 整条翻译轨相对主歌词平移超过 tolMs 时即如此（构造数据：songID=102193601 整轨提前 350ms，
// 「Oh」「Yup」与署名行配上了邻行翻译，共 5 行错配，旧实现留空）。实测语料里没有这种平移：
// 24 首偏差歌每首的平均偏差在 -12~+5ms，是围绕 0 的抖动。纯按时间无法区分「平移到邻行」与
// 「真实偏差」，真遇到再考虑先估全局偏移。
//
// 回溯时依次优先「配对」「lines 行不配」「trans 行不配」——任一与表值相符的前驱都通向
// 最优解，固定顺序让结果确定。表大小 (len(lines)+1)×(len(trans)+1)，一首百行量级，
// 每次取词只跑一次。
func alignByTime(lines, trans []lyricLine, tolMs int) []int {
	type score struct{ pairs, cost int }
	better := func(a, b score) bool {
		return a.pairs > b.pairs || a.pairs == b.pairs && a.cost < b.cost
	}
	ms := func(l lyricLine) int { return int(l.Time*1000 + 0.5) }
	n, m := len(lines), len(trans)
	dp := make([]score, (n+1)*(m+1)) // dp[i*(m+1)+j]：lines[:i] 与 trans[:j] 的最优配法
	at := func(i, j int) score { return dp[i*(m+1)+j] }
	// pair 返回把 lines[i-1] 配给 trans[j-1] 后的得分，以及时差是否在容差内。
	pair := func(i, j int) (score, bool) {
		d := ms(lines[i-1]) - ms(trans[j-1])
		if d < 0 {
			d = -d
		}
		p := at(i-1, j-1)
		return score{p.pairs + 1, p.cost + d}, d <= tolMs
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			best := at(i-1, j)
			if s := at(i, j-1); better(s, best) {
				best = s
			}
			if s, ok := pair(i, j); ok && better(s, best) {
				best = s
			}
			dp[i*(m+1)+j] = best
		}
	}
	match := make([]int, n)
	for i := range match {
		match[i] = -1
	}
	for i, j := n, m; i > 0 && j > 0; {
		if s, ok := pair(i, j); ok && s == at(i, j) {
			match[i-1] = j - 1
			i, j = i-1, j-1
		} else if at(i-1, j) == at(i, j) {
			i--
		} else {
			j--
		}
	}
	return match
}

// qrcWordRegex 匹配 QRC 的逐字时间戳 (起始ms,持续ms)。
var qrcWordRegex = regexp.MustCompile(`\((\d+),(\d+)\)`)

// parseQRCWords 从 QRC 行体解析逐字片段。行体形如：
//
//	风(28837,439)已(29276,441)经(29717,500)息 (30716,1500)
//
// **与网易云 YRC 的顺序相反**：QRC 是「文字在前、时间戳在后」，YRC 是 (时间)文字。故不能
// 复用 ParseYRC——每个片段的文本 = 上一个时间戳末尾到本时间戳开头之间那段。
//
// 最后一个时间戳之后的残留一律丢弃：这天然挡掉了 QRC 外层 XML 的 `"/>` 尾巴，无需另外剥。
//
// 不对片段文本做 TrimSpace：空格是逐字排版的一部分（"息 " 的尾空格决定断字位置）。
// PlayTime 此处不填（offset 未知），由 applyDetailedOffset 统一套。
//
// 抽成纯函数是为了可测：这是「一错就让整首逐字高亮错位」的路径。
func parseQRCWords(body string) []player.LyricTextDetailedWord {
	tuples := qrcWordRegex.FindAllStringSubmatchIndex(body, -1)
	if len(tuples) == 0 {
		return nil
	}
	words := make([]player.LyricTextDetailedWord, 0, len(tuples))
	prevEnd := 0
	for _, m := range tuples {
		text := body[prevEnd:m[0]]
		prevEnd = m[1]
		if text == "" {
			continue
		}
		startMs, err := strconv.Atoi(body[m[2]:m[3]])
		if err != nil {
			continue
		}
		durMs, err := strconv.Atoi(body[m[4]:m[5]])
		if err != nil {
			continue
		}
		words = append(words, player.LyricTextDetailedWord{
			Timestamp: float32(startMs) / 1000.0,
			Duration:  float32(durMs) / 1000.0,
			Text:      text,
		})
	}
	return words
}

// Musicu API response structures
type musicuResponse struct {
	Req0 struct {
		Code int `json:"code"`
		Data struct {
			SongID   int    `json:"songID"`
			SongName string `json:"songName"`
			Lyric    string `json:"lyric"`
			Trans    string `json:"trans"`
			Roma     string `json:"roma"`
			Crypt    int    `json:"crypt"`
		} `json:"data"`
	} `json:"req_0"`
}

// qrcDecrypt decrypts QQ Music's QRC-encrypted lyrics using the ported 3DES algorithm.
// Process: hex string → custom 3DES-ECB decrypt → zlib decompress → UTF-8 string
func qrcDecrypt(encrypted string) (string, error) {
	if encrypted == "" {
		return "", nil
	}

	// 以下两个判据都在真正解密**之前**，failure 含义是「它根本不是密文」，
	// 而不是「密文坏了」——故包 errNotCiphertext 供调用方区分。
	ciphertext, err := hex.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("%w: hex decode: %v", errNotCiphertext, err)
	}

	if len(ciphertext)%8 != 0 {
		return "", i18n.Errorf("%w: ciphertext length %d is not a multiple of 8", errNotCiphertext, len(ciphertext))
	}

	// Use the ported QQ Music 3DES implementation (in qrc_decrypt.go)
	schedule := tripleDesKeySetup(qrc3DESKey, desDecrypt)

	plaintext := make([]byte, 0, len(ciphertext))
	for i := 0; i < len(ciphertext); i += 8 {
		block := tripleDesCrypt(ciphertext[i:i+8], schedule)
		plaintext = append(plaintext, block...)
	}

	// zlib decompress
	reader, err := zlib.NewReader(bytes.NewReader(plaintext))
	if err != nil {
		return "", fmt.Errorf("zlib init: %w", err)
	}
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("zlib decompress: %w", err)
	}

	return string(decompressed), nil
}

// isHexString checks if a string contains only valid hex characters
// decryptIfNeeded 判断 rawLyric 是否为 QRC 密文并解密。返回 (明文, 是否做了解密, error)。
//
// 解密失败分两类，**必须区别对待**：
//
//	不是密文（errNotCiphertext：hex 解不开 / 长度非 8 倍数）
//	    → 内容多半就是明文 LRC，crypt 标志误报或 isHex 假阳性 → 原样放行 + Warn
//	是密文但解不开（3DES/zlib 失败）
//	    → 绝不能原样放行：密文进 parseLRC 后，整段 hex 无换行、不匹配任何时间戳格式，
//	      会落进「不以 [ 开头」分支并命中末尾的「无时间戳按时长均分」兜底，
//	      于是一整串十六进制成为一行歌词推上 OBS → 必须返回 error
//
// 返回 error 后 qqmusic.go 的调用方走既有的 err != nil 分支 emit 空歌词，主播看到
// 「无歌词」而不是一屏乱码。
//
// 两类必须分开的代价是实测过的：一视同仁地报错，会让「crypt=1 但服务端回明文」
// 从「正常显示歌词」退化成「0 行」——见 TestPlaintextWithCryptFlagStillWorks。
//
// isHex 启发式保留：部分响应 crypt 标志缺失但内容确实是密文。它的假阳性由上面的
// errNotCiphertext 分支兜住，不会造成损失。
func decryptIfNeeded(rawLyric string, crypt int) (string, bool, error) {
	isHex := len(rawLyric) > 32 && isHexString(rawLyric[:32])
	if crypt != 1 && !isHex {
		return rawLyric, false, nil
	}
	decrypted, err := qrcDecrypt(rawLyric)
	if err != nil {
		if errors.Is(err, errNotCiphertext) {
			log.Warn("crypt=%d but content is not QRC ciphertext; treating as plaintext: %v", crypt, err)
			return rawLyric, false, nil
		}
		return "", false, i18n.Errorf("QRC decryption failed (crypt=%d, len=%d): %w", crypt, len(rawLyric), err)
	}
	return decrypted, true, nil
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

// fetchLRCBySongMid 使用老版 fcg API 通过 songMid 获取歌词（无需认证）
// 适用于 v20.05: fcg_query_lyric_new.fcg?songmid=<MID>&format=json&nobase64=1
func fetchLRCBySongMid(songMid string, durationMs uint32) ([]lyricLine, string, string, error) {
	url := "https://c.y.qq.com/lyric/fcgi-bin/fcg_query_lyric_new.fcg?songmid=" + songMid + "&g_tk=5381&format=json&nobase64=1"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Referer", "https://y.qq.com/")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var fcgResp struct {
		Retcode int    `json:"retcode"`
		Lyric   string `json:"lyric"`
		Trans   string `json:"trans"`
	}
	if err := json.Unmarshal(body, &fcgResp); err != nil {
		return nil, "", "", fmt.Errorf("json parse: %w", err)
	}
	if fcgResp.Retcode != 0 {
		return nil, "", "", fmt.Errorf("no lyric data for songMid=%s (retcode=%d)", songMid, fcgResp.Retcode)
	}
	if fcgResp.Lyric == "" {
		// 纯音乐：API 成功但返回空歌词，不是错误
		return nil, "", "", nil
	}

	rawLyric := html.UnescapeString(fcgResp.Lyric)
	lines, name, singer := parseLRC(rawLyric, durationMs)
	// 老 fcg 路径的 trans 是明文（nobase64=1，响应无 crypt 字段），无需解密。
	mergeTrans(lines, html.UnescapeString(fcgResp.Trans))
	return lines, name, singer, nil
}

// fetchLRC fetches lyrics using QQ Music's native client API (musicu.fcg)
// When songMid is non-empty (v20.05), uses fcg_query_lyric_new.fcg instead.
func fetchLRC(songID uint32, songMid string, cookie string, durationMs uint32) ([]lyricLine, string, string, error) {
	if songMid != "" {
		return fetchLRCBySongMid(songMid, durationMs)
	}
	// Build the same request QQ Music client sends internally
	payload := map[string]interface{}{
		"comm": map[string]interface{}{
			"ct": 19,
			"cv": 2216,
		},
		"req_0": map[string]interface{}{
			"module": "music.musichallSong.PlayLyricInfo",
			"method": "GetPlayLyricInfo",
			"param": map[string]interface{}{
				"songId":  songID,
				"crypt":   1, // Tell server we can handle QRC encryption
				"lrc_t":   0,
				"qrc":     1,
				"qrc_t":   0,
				"roma":    1, // 请求逐字罗马音（音译）；解析见 attachRoma
				"roma_t":  0,
				"trans":   1,
				"trans_t": 0,
				"type":    1,
				"ct":      19,
				"cv":      2216,
			},
		},
	}

	jsonPayload, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QQMusic/2216 CFNetwork/1.0 Darwin/23.0.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var musicuResp musicuResponse
	if err := json.Unmarshal(body, &musicuResp); err != nil {
		return nil, "", "", fmt.Errorf("json parse: %w", err)
	}
	if musicuResp.Req0.Code != 0 {
		return nil, "", "", fmt.Errorf("API error code: %d", musicuResp.Req0.Code)
	}

	data := musicuResp.Req0.Data
	rawLyric := data.Lyric
	log.Detail("API response: songID=%d crypt=%d lyricLen=%d", data.SongID, data.Crypt, len(rawLyric))

	if rawLyric == "" {
		// 纯音乐：API 成功但返回空歌词，不是错误
		return nil, "", "", nil
	}

	plain, didDecrypt, err := decryptIfNeeded(rawLyric, data.Crypt)
	if err != nil {
		// 「预期之外」。能走到这里**只可能是「是密文但解不开」**：decryptIfNeeded 把
		// 「不是密文」那一类（errNotCiphertext）在内部消化成了明文放行、返回 nil error，
		// 到不了这儿（见它 :245-252 的两类语义）。所以这里没有误报的空间。
		//
		// 3DES/zlib 解不开意味着 QQ 换了加密、或我们的密钥失效 —— 正常情况一次都不该发生。
		// 后果是调用方 emit 空歌词，**主播整首看不到歌词**，比翻译那条重得多，故单独一个 key。
		telemetry.ReportOnce("qqmusic.lyric_decrypt_failed",
			"QQ 音乐主歌词解密失败（是密文但解不开）—— 主播看到无歌词",
			nil,
			map[string]any{
				"error":   err.Error(), // decryptIfNeeded 已把 crypt 与 len 拼进来了
				"song_id": data.SongID,
			})
		return nil, "", "", err
	}
	if didDecrypt {
		log.Detail("QRC decryption succeeded (%d chars)", len(plain))
	}
	rawLyric = plain

	rawLyric = html.UnescapeString(rawLyric)

	lines, name, singer := parseLRC(rawLyric, durationMs)

	attachTrans(lines, data.Trans, data.Crypt)
	attachRoma(lines, data.Roma, data.Crypt)

	return lines, name, singer, nil
}

// attachTrans 解密并合并翻译歌词。翻译与主歌词同一次响应返回，共用 crypt 标志。
//
// 失败**只跳过翻译、绝不阻断主歌词**：sub_text 缺失是少一行副歌词，主歌词断供才是直播
// 事故（稳定 > 功能完整度），故本函数不返回 error。特别地，解不开的密文绝不能放行给
// parseLRC——整串 hex 无时间戳，会命中它末尾「无时间戳按时长均分」的兜底，凭空造出一批
// 假翻译行去乱配 sub_text；这与 decryptIfNeeded 注释里主歌词那条是同一形状的坑，
// 区别只是后果从「一屏乱码」变成「整首错位的副歌词」。
func attachTrans(lines []lyricLine, rawTrans string, crypt int) {
	if rawTrans == "" {
		return
	}
	transPlain, _, err := decryptIfNeeded(rawTrans, crypt)
	if err != nil {
		log.Warn("Translation lyric decryption failed; no sub_text for this song (main lyrics unaffected): %v", err)

		// 「预期之外」，但比主歌词那条轻：只少一行副歌词，主歌词照常。
		//
		// 值得单独一个 key 而不是与主歌词合并：trans 与主歌词**共用同一个 crypt 标志、
		// 由同一次响应返回**。所以「主歌词解开了、trans 没解开」是个比「两个都失败」更奇怪
		// 的信号 —— 前者指向数据本身有问题，后者才指向算法/密钥变更。合并成一个 key 就分不出
		// 这两种，而它们要采取的行动完全不同。
		telemetry.ReportOnce("qqmusic.trans_decrypt_failed",
			"QQ 音乐翻译歌词解密失败（是密文但解不开）—— 本首无 sub_text",
			nil,
			map[string]any{"error": err.Error()})
		return
	}
	mergeTrans(lines, html.UnescapeString(transPlain))
}

// romaToleranceMs 是音译行（roma）与主歌词行的时间匹配容差（毫秒）。
//
// **不与 transToleranceMs 合用一个常量**：两者成因不同、数值也不同。翻译 trans 的时间戳不一定
// 取自 QRC 的时间轴（同一行实测差到 99ms，见 transToleranceMs）；roma 与主歌词 lyric
// **同源同为 QRC 毫秒精度**（同一次 musicu.fcg 响应、共用 crypt），行首时间戳理论上逐毫秒
// 相等（2026-10 实测 58 首与主歌词行数相等的音译轨，非占位行最大差 1ms），正确匹配的时间差
// 本就 ≈0，容差在这里只是「防意外抖动」的安全网，不覆盖任何系统性误差。
//
// 为什么取 20 而不「更严」：既然同源、正确匹配 diff≈0，收紧容差没有收益（正确那行照样命中），
// 反而在 roma 与 lyric 万一有个位数毫秒抖动时把该行音译静默丢掉——与「稳定 > 功能完整度」
// 相悖。为什么不取更大：QRC 相邻行间隔以秒计，20ms 远低于误配到相邻行的阈值，放宽只是白扩
// 风险窗口。（真机核对由主会话 qqromaprobe 端到端做，见 attachRoma 的临时日志。）
const romaToleranceMs = 20

// mergeRoma 把 QQ 的音译歌词按时间就近合并进主歌词的 RomaText。
//
// 每行取容差内时间最近的那条（切片顺序扫描、平局取先出现者）。与 mergeTrans 的区别：
//   - 写 RomaText 而非 SubText —— 音译与翻译是两条独立轨，任一条的失败/缺失绝不能波及另一条
//     或主歌词（roma_text 与 sub_text 互不污染，AGENTS 硬约束）。
//   - 用 romaToleranceMs（同源，理由见其注释）。
//   - 仍是就近匹配，未改用 alignByTime。roma 同源无时间轴偏差，但 alignByTime 注释里那两种
//     错法（平局取先、一句配多行）照样会犯：片头署名行密集处会被贴上邻行的音译（2026-10 实测
//     743 首有音译的歌中 15 首、共 32 行与 alignByTime 的结果不同）。已知未修。
//
// 复用 parseLRC 天然做到「格式兼容」：QQ 的 roma 实测为 QRC 逐字（`[start,dur]文字(ts,dur)…`，
// 与主歌词 lyric 同格式），parseLRC 的 qqRe 分支解析它、剥掉字级时间戳得每行罗马音文本；
// 万一某版本回普通 LRC，则走 re 分支。两种都不必在此分叉判断。
//
// 匹配不上或音译缺失一律保持 RomaText 为空：音译是纯增量，任何情况下都不能反过来影响主歌词。
func mergeRoma(lines []lyricLine, romaLRC string) {
	if romaLRC == "" || len(lines) == 0 {
		return
	}
	romaLines, _, _ := parseLRC(romaLRC, 0)
	if len(romaLines) == 0 {
		return
	}
	for i := range lines {
		lineMs := int(lines[i].Time*1000 + 0.5)
		best := -1
		bestDiff := romaToleranceMs + 1
		for j := range romaLines {
			diff := int(romaLines[j].Time*1000+0.5) - lineMs
			if diff < 0 {
				diff = -diff
			}
			// 严格 < ：平局保留先出现的那条，结果不依赖遍历起点。
			if diff < bestDiff {
				best = j
				bestDiff = diff
			}
		}
		// 复用 transIsDroppable：`//` 不发音占位对两条轨同样适用；QQ 的版权/来源文案是中文，
		// 音译轨是罗马音（拉丁/拼音）几乎不可能命中那几个词根，套上零误伤、还多一层防护
		// （万一 QQ 也往 roma 轨塞声明）。
		if best < 0 || transIsDroppable(romaLines[best].Text) {
			continue
		}
		lines[i].RomaText = romaLines[best].Text
	}
}

// attachRoma 解密并合并音译歌词。音译与主歌词同一次响应返回，共用 crypt 标志。
//
// 失败**只跳过音译、绝不阻断主歌词或翻译**：roma_text 缺失是少一行音译，主歌词/翻译断供才是
// 直播事故（稳定 > 功能完整度），故本函数不返回 error、也绝不触碰 sub_text。特别地，解不开的
// 密文绝不能放行给 parseLRC——整串 hex 无时间戳，会命中它末尾「无时间戳按时长均分」的兜底，
// 凭空造出一批假音译行乱配 RomaText；这与 attachTrans 注释里那条是同一形状的坑。decryptIfNeeded
// 已把「是密文但解不开」挡在 error 分支（见其两类语义），到不了 mergeRoma。
func attachRoma(lines []lyricLine, rawRoma string, crypt int) {
	if rawRoma == "" {
		return
	}
	romaPlain, _, err := decryptIfNeeded(rawRoma, crypt)
	if err != nil {
		log.Warn("Romanization lyric decryption failed; no roma_text for this song (main lyrics and translation unaffected): %v", err)

		// 「预期之外」，与 trans 同级：只少一行音译。单独一个 key 而不与 trans/主歌词合并——
		// roma 与主歌词共用同一 crypt、同一次响应返回，「主歌词解开了、roma 没解开」比「都失败」
		// 更奇怪（前者指向 roma 数据本身有问题，后者才指向算法/密钥变更），合并就分不出这两种、
		// 而它们要采取的行动不同。
		telemetry.ReportOnce("qqmusic.roma_decrypt_failed",
			"QQ 音乐音译歌词解密失败（是密文但解不开）—— 本首无 roma_text",
			nil,
			map[string]any{"error": err.Error()})
		return
	}
	romaPlain = html.UnescapeString(romaPlain)

	// 临时诊断日志（主会话 qqromaprobe 端到端核对用）：解密后 roma 明文前 120 字符，供确认
	// 「roma 确为 QRC 逐字、格式假设成立」。rune 安全截断，避免切断多字节字符。核对通过后可删。
	if head := []rune(romaPlain); len(head) > 120 {
		log.Detail("roma plaintext (first 120 chars): %s", string(head[:120]))
	} else {
		log.Detail("roma plaintext (first 120 chars): %s", romaPlain)
	}

	mergeRoma(lines, romaPlain)

	// 临时诊断日志（同上）：attachRoma 后有 roma_text 的行数 / 总行数，供与屏幕逐字比对。
	withRoma := 0
	for i := range lines {
		if lines[i].RomaText != "" {
			withRoma++
		}
	}
	log.Detail("attachRoma: %d/%d lines have roma_text", withRoma, len(lines))
}

func parseLRC(lrc string, durationMs uint32) ([]lyricLine, string, string) {
	var lines []lyricLine
	var rawTextLines []string
	var songName, singer string
	// 分钟位用 \d+ 而非 \d{2}：写死两位时，时长超 99 分钟的音频（长混音 / DJ 串烧 /
	// 有声书类）行首是 [100:05.00]，整行匹配不上 → 被静默丢弃。秒与毫秒保持原样
	// （\d{2} 与 \d{2,3}，后者兼容三位毫秒）。
	re := regexp.MustCompile(`\[(\d+):(\d{2})\.(\d{2,3})\](.*)`)
	qqRe := regexp.MustCompile(`^\[(\d+),(\d+)\](.+)`)
	qqCharTimingRe := regexp.MustCompile(`\(\d+,\d+\)`)
	tiRe := regexp.MustCompile(`\[ti:(.*?)\]`)
	arRe := regexp.MustCompile(`\[ar:(.*?)\]`)

	for _, line := range strings.Split(lrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if match := tiRe.FindStringSubmatch(line); len(match) > 1 {
			songName = match[1]
			continue
		}
		if match := arRe.FindStringSubmatch(line); len(match) > 1 {
			singer = match[1]
			continue
		}

		// Standard LRC [mm:ss.xx]
		matches := re.FindStringSubmatch(line)
		if len(matches) == 5 {
			min, _ := strconv.Atoi(matches[1])
			sec, _ := strconv.Atoi(matches[2])
			ms, _ := strconv.Atoi(matches[3])
			if len(matches[3]) == 2 {
				ms *= 10
			}
			text := strings.TrimSpace(matches[4])
			if text != "" {
				lines = append(lines, lyricLine{
					Index: len(lines),
					Time:  float32(min)*60 + float32(sec) + float32(ms)/1000.0,
					Text:  text,
				})
			}
			continue
		}

		// QQ proprietary [startMs,durationMs]text(charTs,charDur)...
		qqMatches := qqRe.FindStringSubmatch(line)
		if len(qqMatches) == 4 {
			startMs, _ := strconv.Atoi(qqMatches[1])
			lineDurMs, _ := strconv.Atoi(qqMatches[2])
			rawText := qqMatches[3]
			plainText := qqCharTimingRe.ReplaceAllString(rawText, "")
			plainText = strings.TrimSpace(plainText)
			if plainText != "" {
				lineTimestamp := float32(startMs) / 1000.0
				// 逐字：QRC 的字级时间戳原本在这里被 qqCharTimingRe 整段丢弃，只留行文本。
				// 现在同时留下来喂 text_detailed / lyrics_detailed（契约见 player.LyricTextDetailed）。
				// 非 QRC 源（普通 LRC）走不到本分支，Detailed 保持零值 → 序列化成 {}。
				var detailed player.LyricTextDetailed
				if words := parseQRCWords(rawText); len(words) > 0 {
					lineDuration := float32(lineDurMs) / 1000.0
					if lineDurMs == 0 {
						// 行时长缺省时用字级时长求和兜底（对照 cloudmusic 的 ParseYRC）
						for _, w := range words {
							lineDuration += w.Duration
						}
					}
					detailed = player.LyricTextDetailed{
						Timestamp: lineTimestamp,
						Duration:  lineDuration,
						Words:     words,
					}
				}
				lines = append(lines, lyricLine{
					Index:    len(lines),
					Time:     lineTimestamp,
					Text:     plainText,
					Detailed: detailed,
				})
			}
		}

		// Plain text (no timestamp)
		if !strings.HasPrefix(line, "[") {
			rawTextLines = append(rawTextLines, line)
		}
	}

	// Fallback: distribute plain text evenly across duration
	if len(lines) == 0 && len(rawTextLines) > 0 {
		intervalSec := float32(4.0)
		if durationMs > 0 && len(rawTextLines) > 0 {
			intervalSec = (float32(durationMs) / 1000.0) / float32(len(rawTextLines))
		}
		for i, text := range rawTextLines {
			if text != "" {
				lines = append(lines, lyricLine{
					Index: len(lines),
					Time:  float32(i) * intervalSec,
					Text:  text,
				})
			}
		}
	}
	return lines, songName, singer
}

// songDetailResponse holds the relevant fields from get_song_detail_yqq
type songDetailResponse struct {
	Req0 struct {
		Code int `json:"code"`
		Data struct {
			TrackInfo struct {
				Album struct {
					Mid  string `json:"mid"`
					Pmid string `json:"pmid"`
					Name string `json:"name"`
				} `json:"album"`
				Singer []struct {
					Mid  string `json:"mid"`
					Pmid string `json:"pmid"`
					Name string `json:"name"`
				} `json:"singer"`
				Mid  string `json:"mid"`
				Name string `json:"name"`
			} `json:"track_info"`
		} `json:"data"`
	} `json:"req_0"`
}

// fetchCoverURLBySongMid 通过 songMid 获取封面 URL（v20.05 使用）
// 使用 fcg_play_single_song.fcg?songmid=<MID> 获取 album mid 后构建封面 URL
func fetchCoverURLBySongMid(songMid string) string {
	resp, err := httpClient.Get("https://c.y.qq.com/v8/fcg-bin/fcg_play_single_song.fcg?songmid=" + songMid + "&g_tk=5381&format=json")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Code int `json:"code"`
		Data []struct {
			Album struct {
				Mid  string `json:"mid"`
				Pmid string `json:"pmid"`
				Name string `json:"name"`
			} `json:"album"`
			Singer []struct {
				Mid  string `json:"mid"`
				Name string `json:"name"`
			} `json:"singer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Code != 0 || len(result.Data) == 0 {
		return ""
	}
	track := result.Data[0]
	if mid := track.Album.Mid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("Cover (MID album): %s → %s", track.Album.Name, url)
		return url
	}
	for _, s := range track.Singer {
		if mid := s.Mid; mid != "" {
			url := buildCoverURL("T001", mid, 800)
			log.Detail("Cover (MID singer): %s → %s", s.Name, url)
			return url
		}
	}
	return ""
}

// fetchCoverURL gets the album cover URL for a song using QQ Music's native API.
// When songMid is non-empty (v20.05), uses fcg_play_single_song.fcg instead.
// Returns the cover URL (800x800) or empty string on failure.
func fetchCoverURL(songID uint32, songMid string) string {
	if songMid != "" {
		return fetchCoverURLBySongMid(songMid)
	}
	payload := map[string]interface{}{
		"comm": map[string]interface{}{
			"ct": 19,
			"cv": 2216,
		},
		"req_0": map[string]interface{}{
			"module": "music.pf_song_detail_svr",
			"method": "get_song_detail_yqq",
			"param": map[string]interface{}{
				"song_id": songID,
			},
		},
	}

	jsonPayload, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(jsonPayload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QQMusic/2216 CFNetwork/1.0 Darwin/23.0.0")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var detail songDetailResponse
	if err := json.Unmarshal(body, &detail); err != nil || detail.Req0.Code != 0 {
		return ""
	}

	track := detail.Req0.Data.TrackInfo

	// Priority: album cover → first singer cover
	if mid := track.Album.Mid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("Cover (album): %s → %s", track.Album.Name, url)
		return url
	}
	if mid := track.Album.Pmid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("Cover (album pmid): %s", url)
		return url
	}
	for _, s := range track.Singer {
		if mid := s.Mid; mid != "" {
			url := buildCoverURL("T001", mid, 800)
			log.Detail("Cover (singer): %s → %s", s.Name, url)
			return url
		}
	}
	return ""
}

// buildCoverURL constructs a QQ Music CDN cover URL.
// kind: "T001" for singer, "T002" for album
// size: 150, 300, 500, 800
func buildCoverURL(kind string, mid string, size int) string {
	return fmt.Sprintf("https://y.gtimg.cn/music/photo_new/%sR%dx%dM000%s.jpg", kind, size, size, mid)
}
