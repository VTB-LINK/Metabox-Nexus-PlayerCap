// Command kugouprobe 直连酷狗歌词 API 端到端验证 type=0 音译接线：
// catalog 搜到 hash+duration → krcs 按 hash 取候选 → 下载 KRC → 解密 → krc.ParsePlainKRC，
// 打印每行 text/sub_text/roma_text。只读、仅本地调试。
//
// 验证结论（2026-09）：酷狗 KRC 内嵌 [language:] type=0 即逐行音译（实测 aespa 韩文歌为汉语
// 谐音，如 저기 멀리 → 凑gi 摸里），已由 krc.parseRomanization 接入 roma_text、与翻译 type=1
// 各自独立。本工具复现该链路，供回归/调试留档。
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/player/krc"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// krcXORKey 抄自 player/kugou/lyric/lyric.go。
var krcXORKey = [16]byte{0x40, 0x47, 0x61, 0x77, 0x5E, 0x32, 0x74, 0x47, 0x51, 0x36, 0x31, 0x2D, 0xCE, 0xD2, 0x6E, 0x69}

func krcDecrypt(b64 string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(blob) <= 4 || string(blob[:4]) != "krc1" {
		return "", fmt.Errorf("krc 魔数不符（前4字节非 krc1）")
	}
	dec := make([]byte, len(blob)-4)
	for i, b := range blob[4:] {
		dec[i] = b ^ krcXORKey[i%16]
	}
	zr, err := zlib.NewReader(bytes.NewReader(dec))
	if err != nil {
		return "", err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	return string(out), err
}

func getJSON(u string, v interface{}) error {
	resp, err := httpClient.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return json.Unmarshal(b, v)
}

func main() {
	keyword := "aespa Dreams Come True"
	if len(os.Args) > 1 {
		keyword = os.Args[1]
	}

	// 1. catalog 搜索拿 hash + duration（秒）
	var cat struct {
		Status int `json:"status"`
		Data   struct {
			Info []struct {
				Hash       string `json:"hash"`
				Songname   string `json:"songname"`
				Singername string `json:"singername"`
				Duration   int    `json:"duration"` // 秒
			} `json:"info"`
		} `json:"data"`
	}
	catURL := "http://mobilecdn.kugou.com/api/v3/search/song?format=json&keyword=" + url.QueryEscape(keyword) + "&page=1&pagesize=10"
	if err := getJSON(catURL, &cat); err != nil {
		fmt.Println("catalog 搜索失败:", err)
		return
	}
	fmt.Printf("catalog 搜索 %q: status=%d, %d 条\n", keyword, cat.Status, len(cat.Data.Info))
	if len(cat.Data.Info) == 0 {
		fmt.Println("catalog 无结果")
		return
	}
	for i, e := range cat.Data.Info {
		if i >= 6 {
			break
		}
		fmt.Printf("  #%d %s - %s (%ds) hash=%s\n", i, e.Songname, e.Singername, e.Duration, e.Hash)
	}
	e := cat.Data.Info[0]
	durMs := e.Duration * 1000
	fmt.Printf("\n取 #0: %s - %s, hash=%s dur=%dms\n", e.Songname, e.Singername, e.Hash, durMs)

	// 2. krcs 按 hash 搜候选
	var sr struct {
		Status     int    `json:"status"`
		Proposal   string `json:"proposal"`
		Candidates []struct {
			ID        string `json:"id"`
			AccessKey string `json:"accesskey"`
			Song      string `json:"song"`
		} `json:"candidates"`
	}
	krcsURL := fmt.Sprintf("https://krcs.kugou.com/search?ver=1&man=yes&client=mobi&keyword=&duration=%d&hash=%s", durMs, e.Hash)
	if err := getJSON(krcsURL, &sr); err != nil {
		fmt.Println("krcs 搜索失败:", err)
		return
	}
	fmt.Printf("krcs hash 搜索: status=%d, %d 候选\n", sr.Status, len(sr.Candidates))
	if len(sr.Candidates) == 0 {
		fmt.Println("krcs 无候选")
		return
	}
	cand := sr.Candidates[0]
	for _, c := range sr.Candidates {
		if c.ID == sr.Proposal {
			cand = c
			break
		}
	}

	// 3. 下载 KRC
	var dr struct {
		Status  int    `json:"status"`
		Content string `json:"content"`
	}
	dlURL := fmt.Sprintf("https://lyrics.kugou.com/download?ver=1&client=pc&id=%s&accesskey=%s&fmt=krc&charset=utf8", cand.ID, cand.AccessKey)
	if err := getJSON(dlURL, &dr); err != nil {
		fmt.Println("下载失败:", err)
		return
	}
	if dr.Content == "" {
		fmt.Println("无 KRC 内容（可能只有 LRC）")
		return
	}

	// 4. 解密 + 解析 + 打印三轨
	plain, err := krcDecrypt(dr.Content)
	if err != nil {
		fmt.Println("KRC 解密失败:", err)
		return
	}
	if strings.Contains(plain, "[language:") {
		fmt.Println("KRC 含 [language:] 标签（有翻译/音译轨）")
	} else {
		fmt.Println("KRC 无 [language:] 标签")
	}
	lines := krc.ParsePlainKRC(plain)
	romaN := 0
	for _, l := range lines {
		if l.RomaText != "" {
			romaN++
		}
	}
	fmt.Printf("ParsePlainKRC 解析 %d 行，有 roma_text 的 %d 行\n\n", len(lines), romaN)
	for i, l := range lines {
		if i >= 14 {
			fmt.Printf("...另 %d 行略\n", len(lines)-14)
			break
		}
		fmt.Printf("[%2d] %6.2fs\n  text: %s\n  sub : %s\n  roma: %s\n", l.Index, l.Time, l.Text, l.SubText, l.RomaText)
	}
}
