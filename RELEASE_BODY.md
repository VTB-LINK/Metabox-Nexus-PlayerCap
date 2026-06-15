### 3.0.0-beta.14

这是 3.0.0-beta.14 预发布版本，引入逐字歌词（Word-level Lyrics）功能，为所有播放器预置逐字架构，并显著改善歌词文本匹配稳健性。

#### 新功能

- **逐字歌词（Per-word Lyrics）**：网易云音乐播放器现支持 YRC 逐字歌词解析与输出。API 响应新增 `text_detailed`（每行逐字数据）和 `lyrics_detailed`（全量逐字集合）字段。
- **悬浮歌词页逐字特效**：内置 Fade（渐显）和 Reveal（擦除）两种逐字动画模式，默认 Fade；通过编辑器下拉选单或 `word_fx` URL 参数控制。
- **displayStart 逻辑**：当逐字首词时间早于逐行时间时，`play_time` 使用更早的时间触发显示，确保前端不漏字。
- **全局 `BuildLyricLine` 工厂函数**：所有播放器统一使用共享工厂构建 `LyricLine`，为未来其他播放器引入逐字做好架构准备。

#### 改进

- **歌词文本匹配大幅增强**：引入 `NormalizeLyricText`（去标点+小写）全局归一化，修复 LRC 与 YRC 标点差异导致的逐字匹配失败（如 `,` vs `'`、行尾 `!` 等）。
- **歌名匹配增强**：引入 `NormalizeSongName` / `SameSongName` 共享函数，网易云搜索结果比对和酷狗关键字搜索均升级为去标点模糊匹配。
- **回跳 Seek 检测优化**：CloudMusic 回跳阈值从 1.5s 降至 1.0s（前跳保持 1.5s），改善逐字场景下的同行内 seek 响应。
- **酷狗歌手匹配改进**：`singerMatches` 使用 `NormalizeSongName` 对比，容忍标点差异。

#### 悬浮歌词页变更

- **逐字特效选单**：编辑器「显示」组新增「逐字特效」下拉（渐显 / 擦除 / 关闭），替代原布尔开关。
- **URL 参数**：`word_fx=fade`（默认，可省略）/ `word_fx=reveal` / `word_fx=0`（关闭）。
- **Reveal 模式 Glow 限制**：Reveal 使用 CSS mask，与 text-shadow 冲突，因此 Reveal 模式下自动禁用 glow 以避免方形裁切。Fade 模式不受影响。

#### 架构变更

- `player/player.go` 新增：`BuildLyricLine`、`LyricDisplayStart`、`NormalizeLyricText`、`SameLyricText`、`NormalizeSongName`、`SameSongName` 全局工具函数。
- 所有播放器（WeSing / QQ Music / Kugou / CloudMusic）的 `toLyricLines` 和 `LyricUpdate` 发射统一使用 `BuildLyricLine`，为逐字扩展做好占位。
- `cloudmusic/lyric/fetch.go` 本地归一化函数改为代理全局 `player.NormalizeLyricText` / `player.NormalizeSongName`。
- `kugou/lyric/lyric.go` 歌名匹配升级为 `player.SameSongName`。

#### 升级提示

- **API 新增字段（向后兼容）**：`text_detailed` 和 `lyrics_detailed` 为新增字段，无逐字时分别为 `{}` 和 `[]`，不影响现有客户端解析。
- **悬浮歌词页**：逐字特效默认开启（Fade），无需额外配置；如需关闭使用 `word_fx=0`。

#### 已知限制

- 逐字歌词目前仅网易云音乐可用（依赖 NetEase YRC 数据）。
- 并非所有网易云歌曲都有 YRC（部分仅有 LRC），此时退化为逐行显示。
- Reveal 模式与 glow 效果互斥（CSS mask 限制）。
