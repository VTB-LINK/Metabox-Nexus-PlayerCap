### 4.0.0

以下为相对 v3.3.4 的变化。

> [!IMPORTANT]
> **升级后首次启动会重建一次 `config.yml`**：旧文件改名为同目录备份 `config.backup-*.yml`，控制台列出与新模板不同的设置，请照列表重新填写，随后程序自动重启。从 v3.3.4 升级会多留一份 `config.backup-v3-*.yml`（3.x 原文件，其中的 `offset` 仍是旧语义，不要照抄）。

> [!IMPORTANT]
> `offset` 改为延迟语义（正值=延后，默认值同时取反，全局为 -200）。命令行里的 `-offset`、`-<player>-offset` 不会换算，须自行取反。

> [!IMPORTANT]
> v3.3.4 与预发布版启动时会自动更新到本版本，v3.3.4 没有关闭开关。不能降回 3.x：3.x 联网启动会自动更新回来，断网运行则把已取反的 `offset` 按旧语义读取，歌词偏差为两倍。自动更新也会覆盖本地的 `lyric_display.html`、`lyric_page.html`、`effect_display.html`，它们改为跳转到线上 v3，使用时需要联网，见「变更」。

#### 新增

- 新增 Apple Music 支持，只读进程内存，不改启动参数
- 新增 Spotify 支持，歌词仅逐行原文
  + 发现运行中的 Spotify 缺调试端口或防后台节流参数时，会强制结束并带参数重启，正在播放的歌会中断
- 全民K歌提供音译
- 新增逐字音译字段 `roma_text_detailed`
- 适配 QQ 音乐 22.76
- 音频波形支持 Apple Music、Spotify
- 新增 `audio-frame-rate`，范围 15～60，默认 25
- `/audio-ws` 帧头新增 WASAPI 会话信号，可据此识别 ASIO 等非 WASAPI 输出（此时捕获无声）
- 新增 `config-version`，由程序维护，请勿手改
- `/service-status` 新增 `capabilities`；补发与 HTTP 快照中的播放位置外推到发送时刻
- `status_update` 的缓存出口带 `reason`，新增取值 `no_song`
- 七家播放器无歌词时都发 `index:-1`

#### 变更

- `cover_base64` 统一为 800×800，非正方形居中裁切，多数为 JPEG；超过 1600 或无法处理的原样发出
- Apple Music 的 `cover` 恒为空串，封面只在 `cover_base64`，可能先发空串再补发
- Apple Music、Spotify 的歌词取到之前先发空的 `all_lyrics`，请以最新一条为准
- 命令行开关按字面取值（`-audio-normalize false` 现在真的关闭）；多余的词报错，以退出码 2 退出
- `/service-status` 回显的 `offset` 变号
- 解除无活跃隐藏后，向该播放器的订阅者补发缓存中的整套事件
- 根通道切换播放器时的去重纳入封面与逐字音译
- `/audio-ws` 按采样数分帧，请以帧内 `ts` 计时
- 网易云音乐按界面进度对表，会额外发 `playback_resume`，`position` 可能回退，只用于重新落锚
- 网易云音乐特效镜像在页面忙时不再停帧
- 启动横幅新增配置文件路径与逐键生效值，类型写错的键会告警
- 改名启动与更新后重启时，Ctrl+C 可正常停止，不再残留后台进程；更新后重启失败以退出码 1 退出
- 四个 html 页面（`lyric_display.html`、`lyric_page.html`、`effect_display.html`、`effect_page.html`）改为跳转页：带着全部查询参数跳到线上 v3 的同名页面（`https://playercap.cfg.vtb.tools/v3/`），使用时需要联网
  + `lyric_page.html` 不带参数（`lang` 除外）时打开线上的新编辑器 `https://playercap.cfg.vtb.tools/`
  + `github.com` 发布包不再附带 `effect_page.html`，自动更新不会改动本地已有的该文件
  + v3 是 3.x 页面的冻结副本，另补了逐字揭示模式下的两处显示修正，并对样式类参数加了取值校验：含花括号、分号、引号、反斜杠或 `url(` 的值按未提供处理
- 新增 `player` 取值 `applemusic`、`spotify` 与帧头 flags 的两位：严格校验字段的客户端须更新，flags 须按位读取

#### 修复

- 修复全民K歌开音译时歌词不走
- 修复 QQ 音乐换歌时闪出新歌中间一行、沿用上一首歌词滚动、暂停时回跳
- 修复酷狗音乐、QQ 音乐换歌时位置偏差与误报暂停、恢复、跳转
- 修复波形起步先出现满幅、延迟随运行时间累积
- 修复酷狗音乐封面晚到时被改回 120 档
- 修复英文 Windows 上日志显示模块名而非播放器名

#### 已知限制

- Windows 专用
- 配置文件重建只判定一次：首次启动没取得版本检查结果或重建失败时，之后不再重建，删除 `config.yml` 后重启即按模板生成
- Apple Music 须与本服务以相同权限运行；歌词偶有归属偏差，暂停判定约 1.6 秒
- Spotify 只在 1.3.4.258 上实测过；9223 端口被占用时无法接入
- 本地音频文件只有酷狗音乐可能取到歌词
- ASIO 与 WASAPI 独占捕获不到声音；会话信号能识别 ASIO，区分不了独占
- 音频波形需要 Windows 10 2004 及以上
- 封面重新编码会丢掉 EXIF 方向与 ICC 配置，PNG 透明处变黑
- OBS 页面：特效页只镜像网易云音乐
