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
	// 由 mergeTrans 按时间就近填入。
	SubText string
	// Detailed 逐字时间轴（QRC 才有；LRC 源为零值，序列化成 {}）。
	// 此处 PlayTime 尚未套 offset，由 applyDetailedOffset 统一处理。
	Detailed player.LyricTextDetailed
}

// transToleranceMs 是翻译行与主歌词行的时间匹配容差。
//
// **绝不能改成精确匹配**（cloudmusic 的 MergeTlyric 用精确匹配，那是因为网易云的 lrc 与
// tlyric 同源同精度，不可照抄）：QQ 的 trans 是标准 LRC（`[mm:ss.xx]` 两位毫秒 → 10ms
// 精度），主歌词是 QRC 毫秒精度（`[5451,...]`），trans 相当于把 QRC 时间截断到 10ms，
// 二者恒差 0~10ms。实测四首有翻译的歌，精确匹配命中 3/35、10/69、10/88、7/57
// —— 九成翻译会静默丢失；±20ms 则 100% 命中（实测最大差 10ms）。
//
// 取 20 而非更大：只需覆盖 10ms 截断误差并留一倍余量。再放宽没有收益，只会抬高
// 把翻译配到相邻行的风险。
const transToleranceMs = 20

// transNoLyricMark 是 QQ 的「本行不翻译」占位符（实测每首 1~4 行，多在前奏/间奏）。
// 不过滤会让 OBS 的副歌词行直接显示 "//"。
const transNoLyricMark = "//"

// transMetaKeywords 命中的翻译行是 QQ 自己的产品文案，不是翻译内容，必须丢弃。
//
// QQ 会在翻译轨的**首行**（t=0.00）塞入声明，实测两种文案：
//
//	"QQ音乐享有本翻译作品的著作权"    —— 版权声明
//	"以下歌词翻译由文曲大模型提供"     —— AI 翻译来源声明（较新，16 首取样中 2 首命中）
//
// 它们会被时间匹配配到主歌词第一行上（多为标题行），直接推成 OBS 的副歌词。
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

// mergeTrans 把 QQ 的翻译歌词（标准 LRC）按时间就近合并进主歌词的 SubText。
//
// 用切片顺序扫描而非 map 查找：map 迭代序随机，平局时选中的条目会随机变化，而结果
// 直接进 sub_text 推上 OBS —— 这正是 AGENTS.md 3.8 记录的「map 迭代结果逃出循环」
// 那类缺陷的形状（本仓已踩三次）。parseLRC 返回切片、天然有序，平局取先出现者即确定。
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
	for i := range lines {
		lineMs := int(lines[i].Time*1000 + 0.5)
		best := -1
		bestDiff := transToleranceMs + 1
		for j := range transLines {
			diff := int(transLines[j].Time*1000+0.5) - lineMs
			if diff < 0 {
				diff = -diff
			}
			// 严格 < ：平局保留先出现的那条，结果不依赖遍历起点。
			if diff < bestDiff {
				best = j
				bestDiff = diff
			}
		}
		if best < 0 || transIsDroppable(transLines[best].Text) {
			continue
		}
		lines[i].SubText = transLines[best].Text
	}
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
		return "", fmt.Errorf("%w: 密文长度 %d 不是 8 的倍数", errNotCiphertext, len(ciphertext))
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
			log.Warn("crypt=%d 但内容不是 QRC 密文，按明文处理: %v", crypt, err)
			return rawLyric, false, nil
		}
		return "", false, fmt.Errorf("QRC 解密失败 (crypt=%d, len=%d): %w", crypt, len(rawLyric), err)
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
				"roma":    0,
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
	log.Detail("API 响应: songID=%d crypt=%d lyricLen=%d", data.SongID, data.Crypt, len(rawLyric))

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
		log.Detail("QRC 解密成功 (%d 字符)", len(plain))
	}
	rawLyric = plain

	rawLyric = html.UnescapeString(rawLyric)

	lines, name, singer := parseLRC(rawLyric, durationMs)

	attachTrans(lines, data.Trans, data.Crypt)

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
		log.Warn("翻译歌词解密失败，本首无 sub_text（不影响主歌词）: %v", err)

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
		log.Detail("封面(MID专辑): %s → %s", track.Album.Name, url)
		return url
	}
	for _, s := range track.Singer {
		if mid := s.Mid; mid != "" {
			url := buildCoverURL("T001", mid, 800)
			log.Detail("封面(MID歌手): %s → %s", s.Name, url)
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
		log.Detail("封面(专辑): %s → %s", track.Album.Name, url)
		return url
	}
	if mid := track.Album.Pmid; mid != "" {
		url := buildCoverURL("T002", mid, 800)
		log.Detail("封面(专辑pmid): %s", url)
		return url
	}
	for _, s := range track.Singer {
		if mid := s.Mid; mid != "" {
			url := buildCoverURL("T001", mid, 800)
			log.Detail("封面(歌手): %s → %s", s.Name, url)
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
