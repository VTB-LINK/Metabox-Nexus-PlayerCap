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
		// Detail 级；method 是英文的方法标识，整句原样输出，免得中文句子里夹英文。
		"Kuwo output transient filter engaged: method %s, base %d ms": "Kuwo output transient filter engaged: method %s, base %d ms",
		// 包级 sentinel（player/kuwo/watchdog、player/kuwo/cdp；「Kuwo is not running」与状态文案共用上面那条）
		"Kuwo version or executable is not verified":                                             "酷我版本或可执行文件未经验证",
		"Kuwo debug port is not ready yet":                                                       "酷我调试端口尚未就绪",
		"Kuwo process identity is ambiguous":                                                     "酷我进程身份不明确",
		"Kuwo has already been restarted once in this service run; no further automatic restart": "本次服务已尝试重启酷我，不再自动重启",
		"This Kuwo build is not on the verified allowlist; no automatic restart":                 "酷我当前构建不在已验证白名单内，不自动重启",
		"Kuwo song or state changed during the snapshot":                                         "酷我快照期间歌曲或状态变化",
		"Kuwo CDP is not ready, or its response does not match the verified protocol":            "酷我 CDP 尚未就绪或响应不符合已验证协议",
		"Kuwo CDP bridge version or page directory is not verified":                              "酷我 CDP 桥版本或页面目录未经验证",
		// 包级 sentinel（player/kuwo/lyric、player/kuwo/pos；player/kuwo/cover 的在 messages_kuwo_cover.go）
		"kuwo lyric: no lyrics for this song":                    "酷我歌词：这首歌没有歌词",
		"kuwo lyric: response exceeds size limit":                "酷我歌词：响应超出大小上限",
		"kuwo lyric: invalid song identifier":                    "酷我歌词：歌曲标识无效",
		"kuwo: unsupported playback core":                        "酷我：播放核心不受支持",
		"kuwo: playback core song changed":                       "酷我：播放核心的歌曲已变化",
		"kuwo: invalid playback core sample":                     "酷我：播放核心采样无效",
		"kuwo: playback core position stalled while playing":     "酷我：播放中播放核心位置停滞不前",
		"kuwo: seek-lag correction not available for this build": "酷我：此构建不支持拖动偏差修正",
		"kuwo pos: song info structure not found":                "酷我播放位置：未找到歌曲信息结构",
		"kuwo pos: only supported on windows":                    "酷我播放位置：仅支持 Windows",
		// 函数内的错误（player/kuwo/lyric、player/kuwo/pos，issue #144）
		"kuwo lyric: unexpected status %d":                     "酷我歌词：状态码 %d 不符合预期",
		"kuwo lyric: missing header separator (body %d bytes)": "酷我歌词：缺少头部分隔符（响应体 %d bytes）",
		"%w（tp=%q payload=%d bytes）":                           "%w（tp=%q payload=%d bytes）",
		"kuwo lyric: zlib open: %w":                            "酷我歌词：zlib 打开：%w",
		"kuwo lyric: zlib read: %w":                            "酷我歌词：zlib 读取：%w",
		"kuwo lyric: base64: %w":                               "酷我歌词：base64：%w",
		"kuwo lyric: gbk encode: %w":                           "酷我歌词：GBK 编码：%w",
		"kuwo lyric: gb18030 decode: %w":                       "酷我歌词：GB18030 解码：%w",
		"not a PE32 image":                                     "不是 PE32 映像",
		"no executable section":                                "没有可执行节",
		"image in memory: %w":                                  "内存中的映像：%w",
		"file: %w":                                             "文件：%w",
		"image in memory (TimeDateStamp %#x, SizeOfImage %#x, ModBaseSize %#x) does not match the file (TimeDateStamp %#x, SizeOfImage %#x)": "内存中的映像（TimeDateStamp %#x，SizeOfImage %#x，ModBaseSize %#x）与文件（TimeDateStamp %#x，SizeOfImage %#x）不一致",
		"file larger than %d bytes":     "文件超过 %d bytes",
		"%w: unknown build: %s":         "%w：未知构建：%s",
		"kuwo: seek-lag: %s: %w":        "酷我：拖动偏差：%s：%w",
		"kuwo pos: open process %d: %w": "酷我播放位置：打开进程 %d：%w",
		"kuwo pos: read at 0x%X: %w":    "酷我播放位置：读取 0x%X：%w",
	} {
		chinese[key] = value
	}
}
