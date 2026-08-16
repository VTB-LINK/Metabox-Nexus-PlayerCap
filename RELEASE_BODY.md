### 3.1.1

在 3.0 基础上的功能性小版本：为「指定播放器」通道补上无活跃自动隐藏，编辑器暴露关闭换行动画的开关（歌词 / 歌名各一），并订正 SSE 文档。**无破坏性变更，默认行为与 3.0.1 一致。**

#### 新增能力

- **指定播放器（per-player）通道「无活跃自动隐藏」（默认关）**：走 per-player 通道（`/<player>/ws`、对应 SSE / HTTP）时，歌停后末行歌词此前会一直留在屏幕上——该通道是绕过路由的「我就要盯这个播放器」逃生舱，不受根路径 15s 无活跃清屏（`prior-player-expire`）管辖。现新增可选的无活跃自动隐藏：某 per-player 通道静默超过阈值、且不在播放态时，向该通道推清屏隐藏停留的末行歌词；播放器一恢复即自动复原。三种传输给出一致的「已清空」结果：WS 发 `player_clear`，类型过滤的 SSE 发 in-band 清除（`lyric_update` 的 `index:-1` / 空 `song_info_update`），HTTP 返回 `data:{}`。**只作用于 per-player 端点，不影响跟随活跃播放器的根路径**（根路径的自动隐藏仍由 `prior-player-expire` 控制，两者独立）。
  - 配置：全局 `per-player-idle-hide`（秒，`0` = 关闭，默认 `0`）；每个播放器可用 `<player>-idle-hide` 覆盖（不设 = 跟随全局，`0` = 该播放器关闭，`N` = 该播放器 N 秒）。三层合并（命令行 > config.yml > 默认）齐备，对应 CLI flag `-per-player-idle-hide` / `-<player>-idle-hide`。
  - 可观测：启动横幅新增该配置一行；触发清屏时按「per-player [player] 无活跃达 Ns」记一条日志，与根路径「清除活跃播放器」同级。
- **编辑器「关闭换行动画」开关（歌词 / 歌名独立）**：歌词行切换的进入 / 退场过渡此前只能手改 URL 参数关闭，配置编辑器无入口。现「显示」分组末尾新增开关；因歌词与歌名各有自己的进入动画，拆为两个独立参数——`line_anim=0` 关歌词行动画（逐字 / 非逐字 / 首行 / seek 全覆盖硬切），`title_anim=0` 关 title 模式的歌名入场动画。默认不勾、动画不变。

#### 接口 / 文档

- **CLI flag 补齐**：新增 `-per-player-idle-hide` 与每播放器 `-<player>-idle-hide`；顺带补上此前缺失的 `-prior-player-expire`。
- **SSE 文档订正**：`/lyric_update-SSE` 与 `/{player}/lyric_update-SSE` 的示例此前一直用空 `text_detailed`（`{}`）的样本、从未展示填充的逐字结构；改用真实的 cloudmusicv3 逐字样本，并标注无逐字的行为 `{}`。`player_clear` 相关端点 / 事件说明同步更新：per-player 通道在无活跃自动隐藏时也会收到 `player_clear`（单发、不配对 `player_switch`、`player` 为该播放器名）。

#### 破坏性变更

- 无。per-player 无活跃自动隐藏默认关闭；编辑器开关默认不勾；SSE 改动仅为文档示例。

#### 已知限制

- Windows 专用。
- 无活跃自动隐藏仅作用于 per-player 端点；根路径（跟随活跃播放器）的清屏仍由 `prior-player-expire` 控制，两者相互独立。
- 隐藏为「live 流的一次性信号」：已在线的 WS / SSE 客户端在播放器恢复后，到下一条 `lyric_update` 到达前可能短暂空白；HTTP 与重连取回的是当时缓存。
