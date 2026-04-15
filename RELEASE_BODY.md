### 修复

- **switchSkip 机制重构**：播放器切换后的事件抑制从时间窗口（1s/2s）改为 FNV-1a 内容哈希比对，相同内容才抑制，不同内容立即放行，修复切歌首条事件被误吞的问题 (fix #29)。
- **异步封面补发被吞**：`song_info_update` 哈希加入 `CoverBase64` 存在标志，无 base64 和有 base64 版本产生不同哈希，异步封面补发不再被 switchSkip 误抑制。
- **切歌双重 resume**：切歌后 500ms 内抑制 seek 检测，避免 DOM 进度条残留旧歌时间导致连续触发两次 `playback_resume`。
- **all_lyrics 重复发送**：CDP 已成功获取歌词时跳过 Redux fallback，避免两者行数差异触发误判导致重复推送。

