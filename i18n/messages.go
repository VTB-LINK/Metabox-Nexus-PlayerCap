package i18n

// chinese 是「英文原文 → 中文译文」表。
//
// 约定：
//   - key 必须与调用点的英文格式串逐字一致（含 %s/%d/%v 等占位符与标点），且不含汉字；
//     Go 在编译期把相邻字面量拼接，故跨行 `+` 拼接的消息其 key 是拼接后的整串。
//   - 占位符原样保留在译文里；两种语言语序不同时用带序号的 %[n] 重排，由 fmt 处理。
//     现存带序号的都在英文一侧：issue #61 改向之前中文是原文、英文是译文，改向时逐条对调，
//     没有改写任何一句。
//   - 未收录的 key 在中文环境下回退英文原文（见 T），不报错、不丢信息；出货代码里直接传入的
//     字面量漏收由 catalogcoverage_test.go 拦。
//   - 键与值相同的条目表示该文案在中文环境下原样输出。
//
// 分节沿用引入时的分期：P2 首启文案、P3 wire reason、P4 运营日志。messages_p4_err.go 与
// messages_p4_log.go 的两张表由各自的 init 并入本表，本表已有的键不覆盖。
var chinese = map[string]string{
	// —— P2 首启：Banner ——
	"VTB-TOOLS Metabox Nexus-PlayerCap Multi-Player Lyrics Push Service ": "VTB-TOOLS Metabox Nexus-PlayerCap 多播放器歌词实时推送服务 ",
	"   Version: v%s\n": "   版本: v%s\n",
	// 配置文件行（bannerConfigFileLine）：路径后接加载结果的附注，正常加载时附注为空
	"   Config file: %s%s\n":                                        "   配置文件: %s%s\n",
	" (newly generated)":                                            "（本次新生成）",
	" (read failed; config.yml not applied)":                        "（读取失败，config.yml 未生效）",
	" (parse failed; config.yml not applied)":                       "（解析失败，config.yml 未生效）",
	" (not found and could not be created; config.yml not applied)": "（不存在且生成失败，config.yml 未生效）",
	// 配置文件行之下另起的附注行，缩进由 bannerConfigFileLine 加：按模板生成而目录里另有备份的提示
	"the backup %s in this folder may hold your previous settings": "目录里另有备份 %s，原有设置可能在其中",
	// 同上：版本键与自动迁移的附注（bannerRewriteNote），文件版本 → 盖章值；末条在未写成而留有备份时另起一行
	"%s → %s, migrated; original kept as %s":                                    "%s → %s，已迁移，原文件备份为 %s",
	"%s → %s could not be written":                                              "%s → %s 未能写入",
	"%s → %s: migration could not be written; converted in memory for this run": "%s → %s：应迁移而未能写入，本次在内存中换算",
	"written by a newer version %s; read as is":                                 "文件版本 %s 高于本程序，按当前程序的语义读取",
	"the previous file is kept as %s":                                           "原先的文件留在 %s",
	// 配置行（bannerConfigLines）：%s 是「[*]名字=写下的值[ → 生效值]」各项，键名不译
	"   Listening: %s\n":                 "   监听: %s\n",
	"   Global offset & poll: %s\n":      "   全局偏移与轮询: %s\n",
	"   Prior player: %s\n":              "   优先播放器: %s\n",
	"   Per-player idle auto-hide: %s\n": "   per-player 无活跃自动隐藏: %s\n",
	"   Audio normalization: %s\n":       "   音量归一化: %s\n",
	"   Audio waveform frame rate: %s\n": "   音频波形帧率: %s\n",
	"   Player: %s (%s)\n":               "   播放器: %s (%s)\n",
	"   * = set explicitly (config.yml or command line); a → b = written a, in effect b\n": "   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b\n",

	// —— P2 首启：自动更新框 / 进度 ——
	"║  🆕 New version available: v%s → v%s\n": "║  🆕 发现新版本: v%s → v%s\n",
	"║  📦 %d file(s) to update\n":             "║  📦 共 %d 个文件需要更新\n",
	"║  Updating automatically...":            "║  正在自动更新...",
	"\nPress Enter to exit...":                "\n按回车键退出...",
	"\r[*] Download: %d%% (%.1f/%.1f MB)":     "\r[*] 下载进度: %d%% (%.1f/%.1f MB)",

	// —— 首启：config.yml 强制重建的提示框（configrebuild.go）与前后两条日志 ——
	"config.yml (version %s) is older than %s, the version below which the gateway requires it to be rebuilt; rebuilding it from the template of this version": "config.yml（版本 %s）低于 %s：网关要求低于该版本的配置须重建，现按当前版本的模板重建",
	"config.yml was rebuilt from the template of this version during this start; the previous file is kept as %s":                                              "本次启动已按当前版本的模板重建 config.yml，原先的文件备份为 %s",
	"║  ⚠ config.yml has been rebuilt from the template of this version":                                                                                       "║  ⚠ config.yml 已按当前版本的模板重建",
	"║  Previous file kept as %s (same folder as config.yml)":                                                                                                  "║  旧配置备份: %s（与 config.yml 同目录）",
	"║  The previous file could not be parsed, so its settings are not listed":                                                                                 "║  旧配置解析不了，不列出其中的设置",
	"║  None of its settings differed from the template":                                                                                                       "║  旧配置里的设置与模板没有不同",
	"║  Settings that differed from the template (previous → now):":                                                                                            "║  与模板取值不同的设置（原值 → 重建后的值）:",
	"(not set in the template)": "（模板中未设置）",
	"║  Offsets in the backup still use the old form (positive = earlier); use the values listed above": "║  备份里的 offset 仍是旧写法（正值=提前），请以上面列出的值为准",
	"║  Offsets in the backup already use the new form (positive = later)":                              "║  备份里的 offset 已是新写法（正值=延后）",
	"║  The program will restart to load the new config.yml":                                            "║  程序将重启以加载新的 config.yml",

	// —— P2 首启：隐私告示倒计时（正文段落是 telemetry 包的独立 const，不走本表）；config.yml 重建提示框之后的倒计时同用这一条 ——
	"\r   continuing in %2d s...": "\r   %2d 秒后自动继续...",

	// —— P3 wire：StatusInfo.Detail（非歌名态；配套稳定码 player.Reason*）——
	"NetEase Cloud Music not running":                        "网易云音乐未启动",
	"NetEase Cloud Music v%s unsupported (requires v3+)":     "网易云音乐 v%s 不支持（需 v3+）",
	"NetEase Cloud Music has exited":                         "网易云音乐已退出",
	"KuGou not running or CDP not ready":                     "酷狗音乐未启动或 CDP 未就绪",
	"KuGou installation not found, stopped":                  "未找到酷狗安装，已停止",
	"KuGou libcef.dll version unsupported, stopped":          "酷狗 libcef.dll 版本不受支持，已停止",
	"Administrator rights not obtained, auto-repair stopped": "未取得管理员权限，已停止自动修复",
	"KuGou CDP disconnected":                                 "酷狗音乐 CDP 已断开",
	"QQ Music not running":                                   "QQ音乐未启动",
	"QQ Music has exited":                                    "QQ音乐已退出",
	"Soda Music not running or not connected":                "汽水音乐未启动或未连上",
	"Soda Music CDP disconnected":                            "汽水音乐 CDP 已断开",
	"WeSing client not running":                              "K歌客户端未启动",
	"WeSing client has exited":                               "K歌客户端已退出",
	"WeSing window not open":                                 "K歌窗口未打开",
	"Apple Music not running":                                "Apple Music 未启动",
	"Apple Music has exited":                                 "Apple Music 已退出",
	"Apple Music not playing":                                "Apple Music 未在播放",
	"Cannot read the Apple Music process (Apple Music may be running as administrator)": "无权读取 Apple Music 进程（Apple Music 可能以管理员身份运行）",

	// —— P4c enum：被当数据注入日志的值（在源头经 i18n.T）——
	"yes":    "是",  // detailedFlag：本批歌词含逐字
	"no":     "否",  // detailedFlag：本批歌词无逐字
	"prior":  "优先", // router 组名，注入「%s播放器 …」
	"normal": "普通", // router 组名

	// —— P4 补漏：覆盖检查发现的控制台缺口（清单漏网 log / 注入回退值 / flag 帮助 / Error()）——
	"Extraction error persisting for %s (still retrying): %v": "提取错误持续 %s（仍在重试）: %v",
	"Extraction error: %v":   "提取错误: %v",
	"path unknown":           "路径未知",       // telemetry 程序信息回退
	"edition unknown":        "edition 未知", // telemetry 系统信息回退
	"locale unknown":         "locale 未知",  // telemetry 系统信息回退
	"timezone unknown":       "时区未知",       // telemetry 系统信息回退
	"built-in default":       "内置默认",       // cfg.Sources（日志 + /service-status）
	"command-line arguments": "命令行参数",      // cfg.Sources
	"single-line(30/45)":     "单行(30/45)",  // wesing 计时器模式名（注入日志）
	"double-line(28/42)":     "双行(28/42)",
	"NetEase Cloud Music v%s does not support CDP (requires v%d or later)": "网易云音乐 v%s 不支持 CDP（需 v%d 及以上）", // UnsupportedVersionError.Error()

	// flag 帮助（CLI -h；在 Load() 里注册，晚于 SetLanguage）
	"WebSocket listen address":                               "WebSocket 监听地址",
	"lyric delay (ms); positive = later, negative = earlier": "歌词延迟（毫秒），正值=延后，负值=提前",
	"poll interval (ms)":                                     "轮询间隔（毫秒）",
	"prior player pause timeout (s); 0 disables all timeouts including the normal group, use with caution": "优先播放器暂停超时（秒）；0=关闭全部超时（含普通组），慎用",
	"per-player channel idle auto-hide (s, 0 = off)":                                                       "指定播放器通道无活跃自动隐藏（秒，0=关）",
	"volume normalization switch":                                                                          "音量归一化开关",
	"volume normalization target loudness (dBFS), range [-60,0]":                                           "音量归一化目标响度（dBFS），范围 [-60,0]",
	"volume normalization maximum gain (dB), range [0,45]":                                                 "音量归一化最大增益（dB），范围 [0,45]",
	"audio waveform frame rate (frames per second), range [15,60]":                                         "音频波形帧率（帧每秒），范围 [15,60]",
	"prior player list, comma-separated (e.g. wesing,kugou); an empty string means no prior player":        "优先播放器列表，逗号分隔（如 wesing,kugou）；传空串=无优先播放器",
	"NetEase effect minimize strategy: park (off-screen keep-alive) | fadeout (fade out)":                  "网易云特效最小化策略：park（屏外保活）| fadeout（淡出）",
	"%s lyric delay (ms); positive = later, negative = earlier":                                            "%s 歌词延迟（毫秒），正值=延后，负值=提前",
	"%s poll interval (ms)":                                 "%s 轮询间隔（毫秒）",
	"%s idle auto-hide (s, 0 = off, omit = follow global)":  "%s 无活跃自动隐藏（秒，0=关，不传=跟随全局）",
	"%s volume normalization switch (omit = follow global)": "%s 音量归一化开关（不传=跟随全局）",

	// 命令行里多余的词（config.Load 解析之后还有剩余实参：报错、输出用法、以 2 退出）
	"unexpected argument %q: it is neither a flag nor a flag's value; nothing from it onward was applied": "命令行里多余的词 %q：它不是参数，也不是某个参数的值；自它起的内容均未被采用",

	// config 取值校验告警（cloudmusicv3-effect-strategy：config.yml 与命令行各一条）
	"cloudmusicv3-effect-strategy expects park or fadeout, got %T (%v); ignored": "cloudmusicv3-effect-strategy 期望 park 或 fadeout，实际 %T（%v），已忽略",
	"-cloudmusicv3-effect-strategy expects park or fadeout, got %q; ignored":     "-cloudmusicv3-effect-strategy 期望 park 或 fadeout，实际 %q，已忽略",

	// 音量归一化的类型告警与钳制告警（config.go 的 mergeYAML 与 clampAudioNormalize）
	"audio-normalize expects a boolean, got %T (%v); ignored":            "audio-normalize 期望布尔值，实际 %T（%v），已忽略",
	"audio-normalize-target expects an integer, got %T (%v); ignored":    "audio-normalize-target 期望整数，实际 %T（%v），已忽略",
	"audio-normalize-max-gain expects an integer, got %T (%v); ignored":  "audio-normalize-max-gain 期望整数，实际 %T（%v），已忽略",
	"%s-audio-normalize expects a boolean, got %T (%v); ignored":         "%s-audio-normalize 期望布尔值，实际 %T（%v），已忽略",
	"audio-normalize-target=%d is out of range [%d,%d]; clamped to %d":   "audio-normalize-target=%d 超出范围 [%d,%d]，已钳制为 %d",
	"audio-normalize-max-gain=%d is out of range [%d,%d]; clamped to %d": "audio-normalize-max-gain=%d 超出范围 [%d,%d]，已钳制为 %d",

	// config.yml 类型不符告警（mergeYAML 早期分支补上的几条；其余键的同类告警在上一段与 messages_p4_log.go）
	"addr expects a string, got %T (%v); ignored":                  "addr 期望字符串，实际 %T（%v），已忽略",
	"offset expects an integer, got %T (%v); ignored":              "offset 期望整数，实际 %T（%v），已忽略",
	"poll expects an integer, got %T (%v); ignored":                "poll 期望整数，实际 %T（%v），已忽略",
	"prior-player expects a list, got %T (%v); ignored":            "prior-player 期望列表，实际 %T（%v），已忽略",
	"prior-player item %v is not a string (%T); item ignored":      "prior-player 里的 %v 不是字符串（%T），该项已忽略",
	"prior-player-expire expects an integer, got %T (%v); ignored": "prior-player-expire 期望整数，实际 %T（%v），已忽略",
	"%s-offset expects an integer, got %T (%v); ignored":           "%s-offset 期望整数，实际 %T（%v），已忽略",
	"%s-poll expects an integer, got %T (%v); ignored":             "%s-poll 期望整数，实际 %T（%v），已忽略",

	// config.yml 的版本键与改写（config/rewrite.go：不能按行改写的原因，注入告警的 %v）
	"the file is not UTF-8 encoded":                                       "文件不是 UTF-8 编码",
	"the file contains more than one YAML document":                       "文件里有不止一个 YAML 文档",
	"more than one line looks like the %s key":                            "有不止一行形如 %s 键",
	"no line in a recognized form for the %s key":                         "找不到写法可识别的 %s 键行",
	"the line for %s does not carry its value as a plain decimal integer": "%s 所在的行不是以十进制整数写出它的取值",
	"no single line in a recognized form for the %s key":                  "找不到恰好一行写法可识别的 %s 键行",
	"the rewritten text does not parse back to the expected values":       "改写后的文本重新解析的结果与预期不符",
	"the file is read-only":                                               "文件是只读的",

	// 放置 config.yml 的退路（config/replacefile_other.go，注入告警的 %v）：只有非 Windows 的构建会用到
	"hard links are unavailable and this platform has no fallback for placing the file": "硬链接不可用，此平台上放置文件没有别的办法",

	// config.yml 的版本处理（config/upgrade.go）：自动迁移、改写版本键与按模板生成
	"%s was last written by a newer version (%s) than this program (%s); it is read as is and left unchanged. Settings whose meaning changed in between may not behave as written": "%s 上一次由更高的版本（%s）写入，高于本程序（%s）；按原样读取、不作改动。两个版本之间含义有变的设置，效果可能与写下时不同",
	"%s in %s is not a valid version (%v); treating the file as already migrated and resetting the key to %s":                                                                      "%s（位于 %s）不是合法的版本号（%v），按已迁移过的文件对待，并把该键重置为 %s",
	"%s still holds its %s content; the migration to %s is applied in memory for this run and will be retried at the next start":                                                   "%s 仍是 %s 的内容；到 %s 的迁移本次在内存中换算，下次启动时重试",
	"%s cannot be rewritten safely (%v); file left unchanged":                                                                                                                      "%s 无法安全改写（%v），文件保持原样",
	"Migrated %s from %s to %s; the original is kept as %s":                                                                                                                        "已把 %s 从 %s 迁移到 %s，原文件备份为 %s",
	"Cannot rewrite %s (%v); file left unchanged":                                                                                                                                  "无法改写 %s（%v），文件保持原样",
	"%s was modified while it was being loaded; the saved content is left in place":                                                                                                "%s 在加载期间被改动过，保留刚保存的内容",
	"%s was modified while it was being loaded and could not be put back; the saved content is kept as %s":                                                                         "%s 在加载期间被改动过且未能放回原位，刚保存的内容留在 %s",
	"%s was created anew by another program while it was being rewritten; it is not overwritten, and the file it replaced is kept as %s":                                           "%s 在改写期间被别的程序重新建了出来，不予覆盖；原先的文件留在 %s",
	"Cannot write the new %s (%v) and the original could not be put back; it is kept as %s":                                                                                        "无法写入新的 %s（%v），原文件也未能放回原位，留在 %s",
	"%s did not exist and was generated from the template, but the backup %s is in the same directory; your previous settings may be in it":                                        "%s 不存在，已按模板生成；同目录里另有备份 %s，原有设置可能在其中",

	// config.yml 的强制重建（config/rebuild.go）
	"Rebuilt %s from the template of this version; the previous file is kept as %s": "已按当前版本的模板重建 %s，原先的文件备份为 %s",
	"Forced rebuild of %s was abandoned: the file was created anew by another program and is not overwritten; the file it replaced is kept as %s. The rebuild is not retried automatically; to rebuild, delete %s and restart": "强制重建 %s 已放弃：该文件被别的程序重新建了出来，不予覆盖；原先的文件留在 %s。重建不会自动重试；如需重建，请删除 %s 后重启",
	"Forced rebuild of %s could not write the new file (%v); the previous file is kept as %s, and a new one is generated from the template at the next start":                                                                  "强制重建 %s 时未能写入新文件（%v）；原先的文件留在 %s，下次启动时按模板生成新文件",
	"Forced rebuild of %s failed (%v); the file is unchanged and no backup was made. The rebuild is not retried automatically; to rebuild, delete %s and restart":                                                              "强制重建 %s 失败（%v）；文件没有改动，也没有留下备份。重建不会自动重试；如需重建，请删除 %s 后重启",

	// 中文环境下原样输出的格式串，键与值相同。issue #61 改向之前它们就不含汉字、不在表里，
	// 两种语言下输出相同；收进表里是为了让 catalogcoverage_test.go 分得清「定为原样输出」与「漏译」。
	"CreateLyricHost: 0x%08X": "CreateLyricHost: 0x%08X",
	"vtable: 0x%08X":          "vtable: 0x%08X",
	"[%2d] %6.1fs  %s":        "[%2d] %6.1fs  %s",
	"WS Upgrade error: %v":    "WS Upgrade error: %v",
}
