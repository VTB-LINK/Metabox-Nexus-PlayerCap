//go:build windows

// Command qqromaprobe 连 QQ 音乐进程拿当前歌 songID+cookie，复刻 musicu.fcg 请求把
// roma=1 打开，打印 raw lyric/trans/roma 的长度、是否 hex（加密）、前若干字符，用于
// 判断 QQ 音译 roma 的格式（hex 加密的 QRC？明文 QRC？明文 LRC？）。只读、仅本地调试。
//
// 复用生产 qqmusic.ConnectQQMusic + ReadAllMetadata + FindCookie（只读内存原语）。
// 不做 3DES 解密——先看 raw 形态定格式，再决定接线怎么解。
//
// 验证结论（2026-09）：实测 roma 字段确为 hex 加密（crypt=1、len 6592 ≈ 主歌词 lyric 7568），
// 与 lyric/trans 共用同一 3DES；解密后是 QRC XML 逐字罗马音（<QrcInfos>…、与主歌词同格式）。
// 据此接线（见 commit e868ff0）：复用 decryptIfNeeded + parseLRC，按行首毫秒就近对齐主歌词
// 行写 RomaText。端到端实测：日文歌 33/33 行满覆盖；韩英混排歌纯英文行留空、韩文行给罗马字，
// 逐行对齐正确。本工具为该结论的可复现证据留档。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"Metabox-Nexus-PlayerCap/player/qqmusic"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func isHexPrefix(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, c := range s[:16] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func main() {
	mem, err := qqmusic.ConnectQQMusic()
	if err != nil {
		fmt.Println("连 QQ 音乐失败:", err, "（QQ 开着吗？版本在 knownVersions 里吗？需管理员）")
		return
	}
	fmt.Println("QQ 版本:", mem.Version())
	meta, err := mem.ReadAllMetadata()
	if err != nil {
		fmt.Println("读 metadata 失败:", err)
		return
	}
	fmt.Printf("当前歌: songID=%d songMid=%q dur=%dms\n", meta.SongID, meta.SongMid, meta.DurationMs)
	cookie := mem.FindCookie()
	fmt.Printf("cookie 长度: %d\n\n", len(cookie))

	if meta.SongID == 0 {
		fmt.Println("songID=0（musicu.fcg 需要 songID；换歌瞬间或该版本用 songMid，稍等或换歌重试）")
		return
	}

	// 复刻 fetchLRC 的 musicu.fcg 请求，唯一区别：roma 从 0 改成 1
	payload := map[string]interface{}{
		"comm": map[string]interface{}{"ct": 19, "cv": 2216},
		"req_0": map[string]interface{}{
			"module": "music.musichallSong.PlayLyricInfo",
			"method": "GetPlayLyricInfo",
			"param": map[string]interface{}{
				"songId": meta.SongID, "crypt": 1, "lrc_t": 0, "qrc": 1, "qrc_t": 0,
				"roma": 1, "roma_t": 0, "trans": 1, "trans_t": 0, "type": 1, "ct": 19, "cv": 2216,
			},
		},
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QQMusic/2216 CFNetwork/1.0 Darwin/23.0.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Println("musicu.fcg 请求失败:", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var r struct {
		Req0 struct {
			Code int `json:"code"`
			Data struct {
				Lyric string `json:"lyric"`
				Trans string `json:"trans"`
				Roma  string `json:"roma"`
				Crypt int    `json:"crypt"`
			} `json:"data"`
		} `json:"req_0"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		fmt.Println("响应解析失败:", err, "  body 前 200:", string(body[:min(200, len(body))]))
		return
	}
	d := r.Req0.Data
	fmt.Printf("code=%d  crypt=%d\n", r.Req0.Code, d.Crypt)
	fmt.Printf("lyric: len=%d hex=%v\n  %.100s\n", len(d.Lyric), isHexPrefix(d.Lyric), d.Lyric)
	fmt.Printf("trans: len=%d hex=%v\n  %.100s\n", len(d.Trans), isHexPrefix(d.Trans), d.Trans)
	fmt.Printf("ROMA : len=%d hex=%v\n  %.300s\n", len(d.Roma), isHexPrefix(d.Roma), d.Roma)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
