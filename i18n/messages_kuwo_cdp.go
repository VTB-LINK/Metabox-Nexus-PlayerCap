package i18n

func init() {
	for en, zh := range map[string]string{
		"kuwo: CDP request failed: %w":          "酷我：CDP 请求失败：%w",
		"kuwo: CDP connection failed: %w":       "酷我：CDP 连接失败：%w",
		"kuwo: CDP response failed: %w":         "酷我：CDP 响应失败：%w",
		"kuwo: bridge version query failed: %w": "酷我：桥接版本查询失败：%w",
		"kuwo: controlled restart failed: %w":   "酷我：受控重启失败：%w",
		"Kuwo needs its debug port; restarting the verified instance once (playback may pause)": "酷我需要调试端口；将已验证的实例重启一次（播放可能暂停）",
	} {
		chinese[en] = zh
	}
}
