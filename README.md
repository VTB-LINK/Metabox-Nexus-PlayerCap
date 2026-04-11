<div align="center">

![Banner](title.png)

# VTB-TOOLS Metabox-Nexus-PlayerCap

多播放器歌词实时推送服务 —— 从多个音乐播放器中提取歌词与歌曲信息，通过 WebSocket / HTTP / SSE 广播给外部应用。
</br>
**纯 Go 实现** · Windows 专用 · 支持优先级路由 · 自动更新

</div>

## 支持的播放器

| 播放器 | 标识名 | 提取方式 |
|--------|--------|----------|
| 全民K歌 (WeSing) | `wesing` | 进程内存读取（PE 导出表 + vtable + AOB 扫描） |
| 网易云音乐 | `cloudmusicv3` | CDP 远程调试（Chrome DevTools Protocol） |

## 原理

### WeSing（进程内存读取）

```
WeSing.exe 进程
├─ KSongsLyric.dll → LyricHost 对象 → 歌词文本 + 时间戳
├─ 音频引擎 → float 播放时间（秒）
├─ 内存 JSON → "songname":"歌名","singername":"歌手"
├─ UI 进度文本 → "mm:ss | mm:ss"（歌曲总时长）
└─ 窗口层级:
   ├─ "全民K歌"（主窗口，TXGuiFoundation）
   ├─ "全民K歌 - 歌名"（播放窗口）
   └─ "CLyricRenderWnd"（歌词渲染窗口，歌曲加载完毕后出现）

PlayerCap (wesing 模块)
├─ 通过 PE 导出表 + vtable 搜索定位 LyricHost
├─ 解码歌词数据结构 (UTF-16LE)
├─ AOB 特征搜索定位播放时间（结构体固定字段 0x1E/0x2D）
├─ AOB 搜索 UI 进度文本提取歌曲总时长
├─ AOB 搜索内存 JSON 提取歌名+歌手
├─ 窗口状态机检测播放阶段（单次 EnumWindows）
├─ play_time 停滞检测 → 暂停/恢复事件
└─ 进程存活检测 → 断线自动重连
```

### CloudMusic（CDP 远程调试）

```
cloudmusic.exe 进程（Electron）
├─ --remote-debugging-port=9222
├─ React / Redux 状态 → 歌曲信息、播放时间
└─ DOM → 歌词文本

PlayerCap (cloudmusicv3 模块)
├─ Watchdog 确保进程带调试端口启动（注册表注入自启参数）
├─ WebSocket CDP 客户端连接浏览器
├─ JS 求值 → React Fiber 遍历 → 提取 Redux 状态
├─ 网易云 API 获取歌词（LRC 解析）+ 封面
├─ 本地时钟锚定 + seek 检测
└─ play_time 停滞检测 → 暂停/恢复事件
```

### 多播放器路由

```
Router（事件合并主循环）
├─ 优先播放器（prior-player）播放/加载时立即抢占输出
├─ 优先播放器暂停时保持 holding，超时（prior-player-expire）后释放
├─ 优先播放器空闲时释放控制权给普通播放器
├─ 普通播放器也有独立的状态追踪和组级超时（与优先组对称）
├─ 优先组释放时强制清除普通组 holding 状态，仅 playing/loading 的普通播放器存活
├─ 普通组全员无活动时清空输出（player_clear 事件）
├─ 根订阅者（/ws）只收活跃播放器事件
├─ 单播放器订阅者（/<player>/ws）始终收对应播放器事件
└─ 播放器切换时推送 player_switch 事件 + 新播放器完整初始状态
```

## 功能特性

- ✅ **多播放器支持** — 同时监控全民K歌和网易云音乐，优先级路由自动切换
- ✅ **三种接口** — WebSocket（双向实时）、SSE（单向推送）、HTTP（静态查询）
- ✅ **Per-player 端点** — 每个播放器独立端点（`/wesing/ws`、`/cloudmusicv3/ws` 等）
- ✅ **播放器切换事件** — 活跃播放器变化时推送 `player_switch` + 新播放器完整状态
- ✅ **自动等待进程** — 目标播放器未启动时持续等待，启动后自动开始
- ✅ **暂停/恢复检测** — play_time 停滞自动判定暂停，恢复推进时广播恢复事件
- ✅ **歌曲信息提取** — 歌名、歌手、封面 URL、封面 Base64
- ✅ **实时歌词推送** — 可调轮询频率，广播当前歌词行（含播放进度）
- ✅ **状态广播** — 6 种状态（等待进程 / 等待歌曲 / 加载中 / 播放中 / 暂停 / 待机）
- ✅ **进程断线重连** — 播放器退出后自动回到等待状态，重新启动后自动恢复
- ✅ **时间偏移** — 支持全局和 per-player 正/负毫秒偏移，微调歌词同步
- ✅ **配置文件** — config.yml + CLI flag 三层合并（CLI > YAML > 默认值）
- ✅ **自动更新** — 启动时检查新版本，自动下载 + SHA256 校验 + 热重启
- ✅ **多语言歌词** — 支持所有 UTF-8 编码的语言（中文、日文、韩文、俄文、英文等）
- ✅ **跨重启稳定** — AOB 特征搜索，地址动态定位（WeSing）；CDP 远程连接（CloudMusic）

## 快速开始

### 前置条件

- Go 1.21+
- Windows 10/11
- 全民K歌桌面版 和/或 网易云音乐桌面版

### 编译运行

```bash
# 编译
go build -ldflags "-s -w" -o Metabox-Nexus-PlayerCap.exe .

# 编译并注入版本号（可选）
go build -ldflags "-X main.Version=3.0.0" -o Metabox-Nexus-PlayerCap.exe .
```

> ⚠️ 需要**管理员权限**运行（读取其他进程内存需要 `PROCESS_VM_READ` 权限）

```bash
# 直接运行（使用 config.yml 或默认配置）
.\Metabox-Nexus-PlayerCap.exe

# 歌词提前 500ms 显示
.\Metabox-Nexus-PlayerCap.exe -offset 500

# 指定网易云音乐的偏移量
.\Metabox-Nexus-PlayerCap.exe -cloudmusicv3-offset 300
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `0.0.0.0:8765` | HTTP/WebSocket/SSE 监听地址 |
| `-offset` | `200` | 全局时间偏移（毫秒），正值=歌词提前，负值=延后 |
| `-poll` | `30` | 全局轮询间隔（毫秒），范围 10~2000 |
| `-wesing-offset` | *(沿用全局)* | 全民K歌专属时间偏移 |
| `-wesing-poll` | *(沿用全局)* | 全民K歌专属轮询间隔 |
| `-cloudmusicv3-offset` | *(沿用全局)* | 网易云音乐专属时间偏移 |
| `-cloudmusicv3-poll` | *(沿用全局)* | 网易云音乐专属轮询间隔 |

> 播放器专属参数由 `config.RegisterPlayer()` 动态生成，未设置时自动沿用全局值。

### 配置文件

优先级：**命令行参数** > **config.yml** > **内置默认值**

程序启动时自动加载同目录下的 `config.yml`，若不存在则自动生成：

```yaml
# Metabox-Nexus-PlayerCap 配置文件
# 优先级：命令行参数 > config.yml > 内置默认值

# HTTP/WebSocket/SSE 监听地址
addr: "0.0.0.0:8765"

# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后
offset: 200

# 轮询间隔（毫秒），范围 10~2000
poll: 30

# 优先播放器
prior-player:
- wesing

# 优先播放器暂停超过n秒，自动切换到最后一个普通播放器
prior-player-expire: 15

# 全民K歌专属配置（可选，不设置则沿用全局值）
# wesing-offset: 0
# wesing-poll: 30

# 网易云音乐专属配置（可选）
cloudmusicv3-offset: 500
# cloudmusicv3-poll: 30
```

### 预期输出

```
===========================================================
   Metabox-Nexus-PlayerCap 多播放器歌词实时推送服务
===========================================================
   版本: v3.0.0
   监听: 0.0.0.0:8765
   播放器: wesing (offset=200ms poll=30ms)
   播放器: cloudmusicv3 (offset=500ms poll=30ms)
   优先播放器: [wesing] (超时: 15s)
===========================================================
```

---

## 开发

接口细节详见 [API 响应示例文档](./doc/API_RESPONSE_EXAMPLES.md)

### WebSocket 客户端

连接 `ws://localhost:8765/ws`（根端点，跟随活跃播放器），接收 JSON 消息：

```jsonc
// 所有事件均包含 player 字段，标识来源播放器
// 连接时收到当前状态
{"type": "status_update", "player": "wesing", "data": {"status": "playing", "detail": "三生石下 - 大欢"}}

// 连接时收到歌曲信息（无数据时 data 为 {}）
{"type": "song_info_update", "player": "wesing", "data": {"name": "三生石下", "singer": "大欢", "title": "三生石下 - 大欢", "cover": "http://...", "cover_base64": "data:image/jpeg;base64,..."}}

// 连接时收到完整歌词列表
{"type": "all_lyrics", "player": "wesing", "data": {"song_title": "三生石下 - 大欢", "duration": 236.0, "play_time": 1.2, "lyrics": [...], "count": 36}}

// 歌词变化时收到更新
{"type": "lyric_update", "player": "wesing", "data": {"line_index": 1, "text": "无情的岁月笑我痴", "sub_text": "", "timestamp": 6.9, "play_time": 7.2, "progress": 0.03}}

// 暂停 / 恢复
{"type": "playback_pause", "player": "wesing", "data": {"play_time": 45.2}}
{"type": "playback_resume", "player": "wesing", "data": {"play_time": 45.2}}

// 歌曲播放结束
{"type": "lyric_idle", "player": "wesing", "data": {}}

// 活跃播放器切换（仅根订阅者收到）
{"type": "player_switch", "player": "cloudmusicv3", "data": {"from": "wesing", "to": "cloudmusicv3"}}
// 紧随其后会收到新播放器的完整初始状态（status_update + song_info_update + all_lyrics + lyric_update）

// 活跃播放器清除（所有播放器都停止输出时，仅根订阅者收到）
{"type": "player_switch", "player": "", "data": {"from": "wesing", "to": ""}}
{"type": "player_clear", "player": "", "data": {}}
```

**status 可能的值：** `waiting_process` · `waiting_song` · `loading` · `playing` · `paused` · `standby`

### HTTP/SSE 接口

**根端点**（返回当前活跃播放器数据）：

| 端点 | 类型 | 说明 |
|------|------|------|
| `/health-check` | HTTP | 健康检查 |
| `/service-status` | HTTP | 服务状态（版本、配置、播放器状态、客户端列表） |
| `/ws` | WebSocket | 实时事件推送（全部事件） |
| `/all_lyrics` | HTTP | 完整歌词列表 |
| `/lyric_update` | HTTP | 当前歌词行 |
| `/status_update` | HTTP | 播放状态 |
| `/song_info` | HTTP | 歌曲信息 |
| `/lyric_update-SSE` | SSE | 实时歌词推送流 |
| `/song_info-SSE` | SSE | 实时歌曲信息推送流 |

**Per-player 端点**（始终返回指定播放器数据，不受路由切换影响）：

所有根端点（除 `/health-check` 和 `/service-status`）均有对应的播放器路径版本：

```
/wesing/ws                /cloudmusicv3/ws
/wesing/all_lyrics        /cloudmusicv3/all_lyrics
/wesing/lyric_update      /cloudmusicv3/lyric_update
/wesing/status_update     /cloudmusicv3/status_update
/wesing/song_info         /cloudmusicv3/song_info
/wesing/lyric_update-SSE  /cloudmusicv3/lyric_update-SSE
/wesing/song_info-SSE     /cloudmusicv3/song_info-SSE
```

---

### 示例 HTML 页面

详见 `lyric_display.html`（本地文件，直接用浏览器打开即可）

#### HTML 页面 URL 参数

| 参数 | 说明 | 示例 |
|------|------|------|
| `pure` | 纯净模式 - 仅显示歌词，隐藏头部/状态栏/进度条 | `?pure` |
| `one_line` | 单行模式 - 仅显示当前歌词行 | `?one_line` |
| `color` | 自定义歌词颜色（`pure` 模式下生效） | `?pure&color=%23ff6b6b` |
| `font` | 自定义字体（Google Fonts 名称或系统字体） | `?font=Noto+Serif+SC` |
| `glow` | 启用发光效果（默认关闭） | `?pure&glow` |
| `glow_color` | 发光颜色 | `?pure&glow&glow_color=%23ff0000` |
| `stroke` | 启用文字描边（默认关闭） | `?stroke` |
| `stroke_width` | 描边厚度（px，默认 `1`） | `?stroke&stroke_width=2` |
| `stroke_color` | 描边颜色（默认 `#000`） | `?stroke&stroke_color=%23ff0000` |
| `bg` | 预览背景色（`pure` 模式下，默认透明） | `?pure&bg=%23333333` |

**使用示例：**
- 基础模式：`lyric_display.html`
- OBS 纯净源：`lyric_display.html?pure&one_line`
- 自定义样式：`lyric_display.html?pure&one_line&color=yellow&font=LXGW+WenKai&glow&glow_color=%23ff6b6b`
- 描边 + 发光：`lyric_display.html?pure&one_line&stroke&stroke_width=2&stroke_color=%23000000&glow`

---

## 项目结构

```
Metabox-Nexus-PlayerCap/
├── main.go                # 入口：启动、自动更新、播放器调度、事件路由主循环
├── config/
│   └── config.go          # 配置加载（YAML + CLI flag + 默认值三层合并）
├── logger/
│   └── logger.go          # 统一日志包（5 级别）
├── player/
│   ├── player.go          # Player 接口、公共类型（Event / LyricLine / SongInfo 等）
│   ├── cover.go           # 公共封面下载（HTTP → base64，含大小校验与截断检测）
│   ├── wesing/            # 全民K歌 —— 基于内存读取
│   │   ├── wesing.go      # 主轮询循环、状态机、暂停/恢复检测
│   │   ├── proc/
│   │   │   └── memory.go  # Windows API 封装：进程发现、内存读写、AOB 扫描、窗口枚举
│   │   └── lyric/
│   │       ├── finder.go  # PE 导出表解析 → vtable → 堆扫描定位 LyricHost
│   │       ├── reader.go  # 歌词数据结构解码（UTF-16LE → []LyricLine）
│   │       ├── timer.go   # 播放时间地址定位（结构体签名扫描）+ 歌曲时长提取
│   │       └── songinfo.go# 歌曲元信息提取（歌名/歌手/MID/封面 URL 定位）
│   └── cloudmusic/        # 网易云音乐 —— 基于 CDP
│       ├── cloudmusic.go  # 主轮询循环、本地时钟同步、seek 检测
│       ├── cdp/
│       │   └── client.go  # WebSocket CDP 客户端：连接、JS 求值、React Fiber 遍历
│       ├── lyric/
│       │   └── fetch.go   # 网易云 API 调用（歌词/搜索/详情）、LRC 格式解析
│       └── watchdog/
│           ├── process.go # 确保 cloudmusic.exe 带 --remote-debugging-port=9222 启动
│           └── registry.go# 注册表自启项注入调试端口参数
├── server/
│   ├── server.go          # HTTP/WS/SSE 统一服务器：订阅者管理、状态缓存、广播
│   ├── router.go          # 多播放器优先级路由 + 超时状态机
│   └── types.go           # WSEvent / HTTPResponse 传输层类型
├── config.yml             # 默认配置文件（首次运行自动生成）
├── doc/
│   └── API_RESPONSE_EXAMPLES.md  # API 响应示例（离线参考）
└── build-assets/
    └── winicon/           # Windows .exe 图标资源 (.syso)
```

## 依赖

| 依赖 | 用途 |
|------|------|
| `github.com/gorilla/websocket` | WebSocket 服务 |
| `github.com/shirou/gopsutil/v3` | 进程管理（CloudMusic Watchdog） |
| `golang.org/x/sys` | Windows 系统调用 |
| `gopkg.in/yaml.v3` | YAML 配置文件解析 |

## License

MIT
