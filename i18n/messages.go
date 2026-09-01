package i18n

// english 是「中文原文 → 英文译文」表。
//
// 约定：
//   - key 必须与调用点的中文格式串逐字一致（含 %s/%d/%v 等占位符与中文标点）；
//     Go 在编译期把相邻字面量拼接，故跨行 `+` 拼接的消息其 key 是拼接后的整串。
//   - 占位符原样保留在译文里；英文语序与中文不同时用带序号的 %[n] 重排，由 fmt 处理。
//   - 未收录的 key 在英文环境下回退中文原文（见 T），不报错、不丢信息。
//
// 分期填充：P2 首启文案、P3 wire reason、P4 运营日志。本表为空时全流程输出中文，
// 引入本机制不改变任何现有输出。
var english = map[string]string{
	// —— P2 首启：Banner ——
	"VTB-TOOLS Metabox Nexus-PlayerCap 多播放器歌词实时推送服务 ": "VTB-TOOLS Metabox Nexus-PlayerCap Multi-Player Lyrics Push Service ",
	"   版本: v%s\n": "   Version: v%s\n",
	"   监听: %s\n":  "   Listening: %s\n",
	"   播放器: %s (offset=%dms poll=%dms)\n": "   Player: %s (offset=%dms poll=%dms)\n",
	"   优先播放器: %v (超时: %ds)\n":             "   Prior player: %v (timeout: %ds)\n",
	"全局 %ds":                               "global %ds",
	"全局关":                                  "global off",
	"   per-player 无活跃自动隐藏: %s\n":          "   per-player idle-hide: %s\n",

	// —— P2 首启：自动更新框 / 进度 ——
	"║  🆕 发现新版本: v%s → v%s\n":         "║  🆕 New version available: v%s → v%s\n",
	"║  📦 共 %d 个文件需要更新\n":             "║  📦 %d file(s) to update\n",
	"║  正在自动更新...":                    "║  Updating automatically...",
	"\n按回车键退出...":                     "\nPress Enter to exit...",
	"\r[*] 下载进度: %d%% (%.1f/%.1f MB)": "\r[*] Download: %d%% (%.1f/%.1f MB)",

	// —— P2 首启：隐私告示倒计时（正文段落是 telemetry 包的独立 const，不走本表）——
	"\r   %2d 秒后自动继续...": "\r   continuing in %2d s...",

	// —— P3 wire：StatusInfo.Detail（非歌名态；配套稳定码 player.Reason*）——
	"网易云音乐未启动":                 "NetEase Cloud Music not running",
	"网易云音乐 v%s 不支持（需 v3+）":     "NetEase Cloud Music v%s unsupported (requires v3+)",
	"网易云音乐已退出":                 "NetEase Cloud Music has exited",
	"酷狗音乐未启动或 CDP 未就绪":         "KuGou not running or CDP not ready",
	"未找到酷狗安装，已停止":              "KuGou installation not found, stopped",
	"酷狗 libcef.dll 版本不受支持，已停止": "KuGou libcef.dll version unsupported, stopped",
	"未取得管理员权限，已停止自动修复":         "Administrator rights not obtained, auto-repair stopped",
	"酷狗音乐 CDP 已断开":             "KuGou CDP disconnected",
	"QQ音乐未启动":                  "QQ Music not running",
	"QQ音乐已退出":                  "QQ Music has exited",
	"汽水音乐未启动或未连上":              "Soda Music not running or not connected",
	"汽水音乐 CDP 已断开":             "Soda Music CDP disconnected",
	"K歌客户端未启动":                 "WeSing client not running",
	"K歌客户端已退出":                 "WeSing client has exited",
	"K歌窗口未打开":                  "WeSing window not open",

	// —— P4c enum：被当数据注入日志的中文值（源头已改为经 i18n.T）——
	"是":  "yes",    // detailedFlag：本批歌词含逐字
	"否":  "no",     // detailedFlag：本批歌词无逐字
	"优先": "prior",  // router 组名，注入「%s播放器 …」
	"普通": "normal", // router 组名

	// —— P4 补漏：覆盖检查发现的控制台缺口（清单漏网 log / 注入回退值 / flag 帮助 / Error()）——
	"提取错误持续 %s（仍在重试）: %v": "Extraction error persisting for %s (still retrying): %v",
	"提取错误: %v":            "Extraction error: %v",
	"路径未知":                "path unknown",           // telemetry 程序信息回退
	"edition 未知":          "edition unknown",        // telemetry 系统信息回退
	"locale 未知":           "locale unknown",         // telemetry 系统信息回退
	"时区未知":                "timezone unknown",       // telemetry 系统信息回退
	"内置默认":                "built-in default",       // cfg.Sources（日志 + /service-status）
	"命令行参数":               "command-line arguments", // cfg.Sources
	"单行(30/45)":           "single-line(30/45)",     // wesing 计时器模式名（注入日志）
	"双行(28/42)":           "double-line(28/42)",
	"网易云音乐 v%s 不支持 CDP（需 v%d 及以上）": "NetEase Cloud Music v%s does not support CDP (requires v%d or later)", // UnsupportedVersionError.Error()

	// flag 帮助（CLI -h；在 Load() 里注册，晚于 SetLanguage）
	"WebSocket 监听地址": "WebSocket listen address",
	"歌词时间偏移（毫秒）":     "lyric time offset (ms)",
	"轮询间隔（毫秒）":       "poll interval (ms)",
	"优先播放器暂停超时（秒）；0=关闭全部超时（含普通组），慎用": "prior player pause timeout (s); 0 disables all timeouts including the normal group, use with caution",
	"指定播放器通道无活跃自动隐藏（秒，0=关）":          "per-player channel idle auto-hide (s, 0 = off)",
	"%s 歌词时间偏移（毫秒）":                  "%s lyric time offset (ms)",
	"%s 轮询间隔（毫秒）":                    "%s poll interval (ms)",
	"%s 无活跃自动隐藏（秒，0=关，不传=跟随全局）":      "%s idle auto-hide (s, 0 = off, omit = follow global)",
}
