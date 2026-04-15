### 重构

- **统一播放器事件发射体系**：新增 `BaseEmitter` 公共结构体，CloudMusic 和 WeSing 均嵌入复用，消除重复的事件通道与发射逻辑。
- **CloudMusic 事件字段补齐**：`lyric_update` 补充 `progress` 字段（播放进度 0~1）；`all_lyrics` 补充 `play_time` 字段，与 WeSing 对齐。
- **日志体系统一**：所有运行时日志改为中文；WeSing 封面获取 goroutine 增加重试成功/失败日志；Watchdog 日志同步中文化。
- **冷启动顺序优化**：HTTP 服务从 `http.ListenAndServe` 拆为 `net.Listen` + `http.Serve`，通过 `readyCh` 信号确保端口绑定成功后才启动播放器，消除早期事件丢失的竞态。

### 修复

- **switchSkip 机制重构**：播放器切换时的事件抑制从时间窗口判定改为内容哈希比对去重，避免切换瞬间吞掉不同内容的首条事件 (fix #29)。