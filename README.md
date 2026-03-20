<div align="center">

![Banner](title.png)

# VTB-TOOLS Metabox-Nexus-WesingCap

从全民K歌 (WeSing) 进程中实时提取歌词与歌曲信息，通过 WebSocket 广播给外部应用。

</div>

---

## 功能特性

- ✅ **自动等待进程** — WeSing 未启动时持续等待，启动后自动开始
- ✅ **三态窗口检测** — 基于 CLyricRenderWnd + 播放窗口标题区分待机/加载中/播放中
- ✅ **智能切歌检测** — 通过窗口标题变化检测切歌，自动重载歌词
- ✅ **暂停/恢复检测** — play_time 停滞自动判定暂停，恢复推进时广播恢复事件
- ✅ **歌曲信息提取** — 从内存 JSON 提取歌名+歌手，窗口标题交叉验证
- ✅ **歌曲时长提取** — 从 UI 进度文本 "mm:ss | mm:ss" 解析歌曲总时长，支持播放进度计算
- ✅ **实时歌词推送** — 可调轮询频率，通过 WebSocket/SSE 广播当前歌词行
- ✅ **状态广播** — 实时推送 6 种状态（等待进程/等待歌曲/加载中/播放中/暂停/待机）
- ✅ **进程断线重连** — WeSing 退出后自动回到等待状态，重新启动后自动恢复推送
- ✅ **时间地址缓存** — 切歌时复用已定位的播放时间地址，避免重复 AOB 搜索
- ✅ **时间偏移** — 支持正/负毫秒偏移，微调歌词同步
- ✅ **配置文件** — config.yml 支持，优先级：命令行 > 配置文件 > 默认值
- ✅ **健康检查** — HTTP `/health-check` 端点
- ✅ **多语言歌词** — 支持所有 UTF-8 编码的语言（中文、日文、韓文、俄文、英文等）
- ✅ **无需 Cheat Engine** — 直接调用 Windows API 读取进程内存
- ✅ **跨重启稳定** — AOB 特征搜索，地址动态定位

### 运行

```bash
# 双击或直接运行（使用 config.yml 或默认配置）
.\Metabox-Nexus-WesingCap.exe

# 歌词提前 500ms 显示
.\Metabox-Nexus-WesingCap.exe -offset 500

# 歌词延后 200ms + 30ms 高频轮询
.\Metabox-Nexus-WesingCap.exe -offset -200 -poll 30
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-offset` | `200` | 时间偏移（毫秒），正值=歌词提前，负值=延后 |
| `-poll` | `30` | 轮询间隔（毫秒），范围 10~2000 |
| `-addr` | `0.0.0.0:8765` | WebSocket/HTTP 监听地址 |

### 配置文件

优先级：**命令行参数** > **config.yml** > **内置默认值**

程序启动时自动加载同目录下的 `config.yml`，若不存在则自动生成：

```yaml
# Metabox-Nexus-WesingCap 配置文件
addr: "0.0.0.0:8765"
offset: 200
poll: 30
```

### 预期输出

```
[*] 已加载 config.yml
===========================================================
   VTB-TOOLS Metabox-Nexus 全民K歌 歌词实时推送服务 (Go v2)
===========================================================
[*] 等待 WeSing.exe 启动...

========== HTTP/WebSocket 服务接口 ==========
[*] WebSocket: ws://0.0.0.0:8765/ws

--- HTTP 接口 (静态数据) ---
[*] 健康检查: http://0.0.0.0:8765/health-check
[*] 服务状态: http://0.0.0.0:8765/service-status
[*] 完整歌词: http://0.0.0.0:8765/all_lyrics
[*] 当前歌词: http://0.0.0.0:8765/lyric_update
[*] 播放状态: http://0.0.0.0:8765/status_update
[*] 歌曲信息: http://0.0.0.0:8765/song_info

--- SSE 接口 (实时推送) ---
[*] 歌词推送: http://0.0.0.0:8765/lyric_update-SSE
[*] 歌曲推送: http://0.0.0.0:8765/song_info-SSE

[*] 等待客户端连接...

[+] 找到 WeSing.exe (PID: 31260)
[+] 找到 192 个模块

[♪] 歌曲开始播放: Whiplash
[+] KSongsLyric.dll 基址: 0x60A20000 (大小: 0xC000)
[+] CreateLyricHost: 0x60A24ACB
[+] 构造函数: 0x60A2101F
[+] vtable: 0x60A26334
[+] LyricHost 实例: 0x0991E420
[+] 歌词子结构: 0x0991E42C
[*] 加载歌词数据...
[+] 加载了 114 行歌词
    [ 0]    7.8s  One look give 'em Whiplash
    [ 1]    9.6s  Beat drop with a big flash
...
[+] 播放时间地址: 0x04D0BB08 (当前: 1.20s)
[♪] 歌曲: Whiplash  歌手: aespa
[*] 开始歌词轮询 (30ms 间隔, 偏移 +200ms)...
[♪] [0] One look give 'em Whiplash (7.8s)
[♪] [1] Beat drop with a big flash (9.6s)
...
[*] 标题变化: "Whiplash" → "三生石下"
[*] 检测到切歌，重新加载...
```

---

## 开发

接口细节详见 [API 响应示例文档](https://wesingcap.nexus.metabox.apifox.vtb.link/)

### WebSocket 客户端

连接 `ws://localhost:8765/ws`，接收 JSON 消息：

```jsonc
// 连接时收到当前状态
{"type": "status_update", "data": {"status": "playing", "detail": "三生石下 - 大欢"}}
// status 可能的值：
//   "waiting_process" - K歌客户端未启动
//   "loading" - 歌曲加载中
//   "playing" - 播放中
//   "paused" - 暂停中（play_time 停止推进时自动检测）
//   "waiting_song" - K歌窗口未打开/焦点丢失
//   "standby" - 待机状态

// 连接时收到歌曲信息（无数据时 data 为 {}）
{"type": "song_info_update", "data": {"name": "三生石下", "singer": "大欢", "title": "三生石下 - 大欢"}}

// 连接时收到完整歌词列表（含时长和当前播放时间）
{"type": "all_lyrics", "data": {"song_title": "三生石下 - 大欢", "duration": 236.0, "play_time": 1.2, "lyrics": [{"index": 0, "time": 1.7, "text": "前世的尘"}], "count": 36}}

// 歌词变化时收到更新（含进度条 progress）
{"type": "lyric_update", "data": {"line_index": 1, "text": "无情的岁月笑我痴", "timestamp": 6.9, "play_time": 7.2, "progress": 0.03}}

// 暂停播放（play_time 连续不变时触发）
{"type": "playback_pause", "data": {"play_time": 45.2}}

// 恢复播放（play_time 重新推进时触发）
{"type": "playback_resume", "data": {"play_time": 45.2}}

// 歌曲播放结束
{"type": "lyric_idle", "data": {}}
```

### HTTP/SSE 接口

除了 WebSocket 外，还提供 HTTP 和 SSE 接口供不同场景使用：

**HTTP 接口（查询静态数据）：**
- `/health-check` - 健康检查
- `/service-status` - 服务状态信息（版本、配置、客户端列表）
- `/all_lyrics` - 完整歌词列表
- `/lyric_update` - 当前歌词（可能为 null）
- `/status_update` - 播放状态
- `/song_info` - 歌曲信息

**SSE 接口（实时推送，支持所有 UTF-8 语言）：**
- `/lyric_update-SSE` - 实时歌词推送流
- `/song_info-SSE` - 实时歌曲信息推送流

---

### 示例 HTML 页面

详见Release配套的 `lyric_display.html`（本地文件，直接用浏览器打开即可，无需通过 HTTP 服务器访问）

#### HTML 页面 URL 参数

打开 `lyric_display.html` 时可通过 URL 参数调整展示效果：

| 参数 | 说明 | 示例 |
|------|------|------|
| `pure` | 纯净模式 - 仅显示歌词，隐藏头部/状态栏。非播放状态时屏幕保持空白 | `?pure` |
| `one_line` | 单行模式 - 仅显示当前歌词行，隐藏上下文 | `?one_line` |
| `color` | 自定义歌词颜色（仅在 `pure` 模式下生效） | `?pure&color=red` 或 `?pure&color=%23ff6b6b` |

**使用示例（直接用浏览器打开本地文件）：**
- 基础模式：`lyric_display.html`
- 纯净模式：`lyric_display.html?pure`
- 单行模式：`lyric_display.html?one_line`
- 纯净单行模式：`lyric_display.html?pure&one_line`
- 自定义颜色（纯净模式）：`lyric_display.html?pure&color=yellow`
- 复合使用：`lyric_display.html?pure&one_line&color=%23ff0000`
