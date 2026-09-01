//go:build windows

// Command sodaprobe 连汽水音乐主进程 inspector 取当前歌的明文 KRC、经 krc.ParsePlainKRC
// 解析，打印每行 text/sub_text/roma_text，用于验证汽水 KRC 是否内嵌 type=0 音译轨。
// 只读、仅本地调试。复用生产的 watchdog（激活 inspector）+ cdp（Connect/Extract）。
//
// 验证结论（2026-09）：汽水**平台自身**无音译源——实测 KRC 不含 [language:] 内嵌轨，
// sharedState 的 ExtractionData 也无独立音译字段（只有 TranslationLRC 翻译），前端亦不渲染
// 音译。故 parseSodaLyrics 走内嵌轨解出的 roma_text 恒空；代码共用 kugou 的 KRC 解析、
// 防御性就绪（平台日后若在 KRC 内嵌 type=0 会自动生效）。本工具为「平台无内嵌轨」这一结论
// 的可复现证据留档。
//
// 注：平台无内嵌轨的结论不变，但 sodamusic 的 roma_text 已不再恒空——fetchKugouRoma 会事后
// 从酷狗按歌名/时长「借」音译、按主歌词文本对齐补入（非平台原生，见 player/sodamusic/roma.go）。
// 本工具只探测平台内嵌轨、不含借来的音译。
package main

import (
	"fmt"
	"strings"

	"Metabox-Nexus-PlayerCap/player/krc"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
	"Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
)

func main() {
	pid, err := watchdog.FindMainPID()
	if err != nil {
		fmt.Println("找汽水主进程失败:", err, "（汽水开着吗？）")
		return
	}
	fmt.Println("汽水主进程 pid:", pid)
	if err := watchdog.EnsureInspector(pid); err != nil {
		fmt.Println("激活 inspector 失败:", err, "（可能需要管理员权限）")
		return
	}
	client, err := cdp.Connect()
	if err != nil {
		fmt.Println("连接 inspector 失败:", err)
		return
	}
	data, err := client.Extract()
	if err != nil {
		fmt.Println("提取播放态失败:", err)
		return
	}
	fmt.Printf("歌曲: %q - %v  (lyricType=%q, KRC 长度=%d)\n", data.Name, data.Artists, data.LyricType, len(data.LyricContent))
	if data.LyricType != "krc" || data.LyricContent == "" {
		fmt.Println("非 KRC 或空歌词——换一首带歌词的歌重试")
		return
	}
	if strings.Contains(data.LyricContent, "[language:") {
		fmt.Println("KRC 含 [language:] 标签（有内嵌翻译/音译轨）")
	} else {
		fmt.Println("KRC 无 [language:] 标签（无内嵌翻译/音译轨）——这首没有 type=0")
	}

	lines := krc.ParsePlainKRC(data.LyricContent)
	romaN, subN := 0, 0
	for _, l := range lines {
		if l.RomaText != "" {
			romaN++
		}
		if l.SubText != "" {
			subN++
		}
	}
	fmt.Printf("ParsePlainKRC 解析 %d 行；有 roma_text %d 行、有 sub_text(内嵌译轨) %d 行\n\n", len(lines), romaN, subN)
	for i, l := range lines {
		if i >= 16 {
			fmt.Printf("...另 %d 行略\n", len(lines)-16)
			break
		}
		fmt.Printf("[%2d] %6.2fs\n  text: %s\n  sub : %s\n  roma: %s\n", l.Index, l.Time, l.Text, l.SubText, l.RomaText)
	}
}
