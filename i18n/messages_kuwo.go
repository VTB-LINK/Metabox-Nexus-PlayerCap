package i18n

func init() {
	for key, value := range map[string]string{
		"Kuwo connection lost; retrying":           "酷我连接已断开，正在重试",
		"Kuwo is not running":                      "酷我尚未运行",
		"This Kuwo playback core is not supported": "暂不支持此酷我播放核心",
		"Kuwo has no current song":                 "酷我当前没有歌曲",
		"Kuwo capture unavailable: %v":             "酷我采集暂不可用：%v",
		"Kuwo cover unavailable: %v":               "酷我封面暂不可用：%v",
		"Kuwo lyrics unavailable: %v":              "酷我歌词暂不可用：%v",
		"Kuwo must be started with debug port 9224; automatic restart is limited to verified builds, please launch Kuwo manually with --remote-debugging-port=9224": "酷我需要以调试端口 9224 启动；自动重启只对已验证的构建开放，请手动带 --remote-debugging-port=9224 启动酷我",
		"Kuwo core bound: build %s, PlayerCore timestamp 0x%08X, read slot %d":                                                                                      "酷我已绑定播放核心：构建 %s，PlayerCore 时间戳 0x%08X，读取槽位 %d",
		"Kuwo is playing but the core position has not advanced for %s; failing closed":                                                                             "酷我正在播放但核心位置已 %s 未前进；降级结束会话",
		"Kuwo seek-lag correction enabled: build %s":                                                                                                                "酷我拖动偏差修正已启用：构建 %s",
		"Kuwo seek-lag correction disabled: %v":                                                                                                                     "酷我拖动偏差修正未启用：%v",
		"Kuwo seek-lag correction adopted: %.1f ms, segment ctx 0x%08X ring 0x%08X base %d ms":                                                                      "酷我拖动偏差修正采用 %.1f ms，段：上下文 0x%08X，环形缓冲 0x%08X，起点 %d ms",
		"Kuwo seek-lag correction stopped for this song: the lag drifted to %.1f ms, segment ctx 0x%08X ring 0x%08X base %d ms":                                     "酷我拖动偏差修正本首停用：偏差漂移至 %.1f ms，段：上下文 0x%08X，环形缓冲 0x%08X，起点 %d ms",
	} {
		chinese[key] = value
	}
}
