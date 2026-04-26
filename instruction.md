# Metabox-Nexus-PlayerCap 开发指令手册

> 本文件为 LLM 与开发者提供项目开发规范与接入参考。随项目演进持续更新。

---

## 1. 项目概览

| 字段 | 值 |
|------|------|
| 模块路径 | `Metabox-Nexus-PlayerCap` |
| Go 版本 | 1.21+ |
| 目标平台 | Windows (amd64) |
| 用途 | 多播放器歌词实时推送服务（HTTP / WebSocket / SSE） |
| 依赖 | `gorilla/websocket`、`gopsutil/v3`、`golang.org/x/sys`、`gopkg.in/yaml.v3` |
| 构建 | `go build -ldflags "-X main.Version=x.y.z-beta.1" .` |

### 目录结构

```
├── main.go                # 入口：启动、自动更新、播放器调度、事件路由主循环
├── config/
│   └── config.go          # 配置加载（YAML + CLI flag + 默认值三层合并）
├── logger/
│   └── logger.go          # 统一日志包（5 级别）
├── player/
│   ├── player.go          # Player 接口、BaseEmitter、公共类型（Event / LyricLine / SongInfo 等）、ClampFloat32
│   ├── cover.go           # 公共封面下载（HTTP → base64，含大小校验与截断检测）
│   ├── wesing/            # 全民K歌播放器 —— 基于内存读取
│   │   ├── wesing.go      # 主轮询循环、状态机、暂停/恢复检测
│   │   ├── proc/
│   │   │   └── memory.go  # Windows API 封装：进程发现、内存读写、AOB 扫描、窗口枚举
│   │   └── lyric/
│   │       ├── finder.go  # PE 导出表解析 → vtable → 堆扫描定位 LyricHost
│   │       ├── reader.go  # 从内存中读取歌词向量（UTF-16LE → []LyricLine）
│   │       ├── timer.go   # 播放时间地址定位（结构体签名扫描）+ 歌曲时长提取
│   │       └── songinfo.go# 歌曲元信息提取（歌名/歌手/MID/封面 URL）
│   ├── cloudmusic/        # 网易云音乐播放器 —— 基于 CDP (Chrome DevTools Protocol)
│   │   ├── cloudmusic.go  # 主轮询循环、本地时钟同步、seek 检测
│   │   ├── cdp/
│   │   │   └── client.go  # WebSocket CDP 客户端：连接、JS 求值、React Fiber 遍历
│   │   ├── lyric/
│   │   │   └── fetch.go   # 网易云 API 调用（歌词/搜索/详情）、LRC 格式解析
│   │   └── watchdog/
│   │       ├── process.go # 确保 cloudmusic.exe 带 --remote-debugging-port=9222 启动
│   │       └── registry.go# 注册表自启项注入调试端口参数
│   └── qqmusic/           # QQ 音乐播放器 —— 基于内存读取 + AOB Hook
│       ├── qqmusic.go     # 主轮询循环、双源融合插值、暂停/恢复/seek 检测
│       ├── mem.go         # 进程连接、内存读写、AOB Hook 注入（滑块 + 进度 + KUSER）
│       ├── api.go         # QQ 音乐 API 调用（歌词/封面）、QRC 解析
│       └── qrc_decrypt.go # QRC 3DES 自定义解密算法
├── server/
│   ├── server.go          # Server 核心：订阅者管理、状态缓存、广播分发
│   ├── router.go          # 多播放器优先级路由 + 超时状态机 + HTTP/WS/SSE 端点注册
│   └── types.go           # WSEvent / HTTPResponse 传输层类型 + player 类型别名
├── config.yml             # 默认配置文件（首次运行自动生成）
├── doc/
│   └── API_RESPONSE_EXAMPLES.md  # API 响应示例（离线参考）
└── build-assets/
    └── winicon/           # Windows .exe 图标资源 (.syso)
```

### 数据消费 / API 文档

如果你需要了解 HTTP / WebSocket / SSE 端点的请求方式和响应格式，请参阅在线 API 文档：

> **http://playercap.nexus.metabox.apifox.vtb.link/**

仓库内的 `doc/API_RESPONSE_EXAMPLES.md` 为离线快速参考，但可能滞后于在线版本。

---

## 2. 架构与数据流

### 2.1 整体流程

```
main()
 ├─ config.Load()                        // 三层配置合并
 ├─ server.NewServer()                   // 创建 HTTP/WS/SSE 服务
 ├─ go srv.Start(addr, readyCh)          // [goroutine] 启动 HTTP 服务
 ├─ <-readyCh                            // 阻塞等待端口绑定成功
 ├─ server.NewRouter(cfg, srv, players)  // 创建多播放器路由器
 ├─ wp.Start() / cp.Start()             // [goroutine] 各播放器轮询循环
 └─ router.Run()                         // [阻塞] 事件合并主循环
      ├─ srv.UpdatePlayerState(evt)      // 更新状态缓存（含 PlayTime 实时同步）
      ├─ router.updateRouting(evt)       // 优先级路由决策
      │    └─ switchTo()                 // [同步] 推 FullState → 返回已发送类型集合 → 设置 switchSkip
      ├─ switchSkip 抑制检查             // 仅跳过 FullState 已包含的类型，避免重复
      └─ srv.NotifySubscribers(evt)      // 广播到 WS/SSE 订阅者
```

### 2.2 播放器优先级路由

Router 维护 `activePlayer`（当前主输出播放器），规则：

1. **优先播放器**（`prior-player` 列表，默认 `["wesing"]`）播放/加载时立即抢占。
2. 优先播放器**暂停**时保持抢占（holding），直到超过 `prior-player-expire` 秒自动释放。
3. 优先播放器**空闲**（进程退出/待机）时释放控制权给普通播放器。
4. **普通播放器**也有独立的状态追踪（playing/loading/paused/idle）和组级超时机制，与优先组完全对称。
5. 优先组释放时，**强制清除所有普通组 holding 状态**（prior 播放期间暂停的 normal 播放器不应被切入），仅 playing/loading 的普通播放器存活。
6. 普通组全员 inert 时，`activePlayer` 设为空，向根订阅者推送 `player_switch`(to="") + `player_clear` 双重通知。
7. 根订阅者（`/ws`）只收到 `activePlayer` 的事件；单播放器订阅者（`/wesing/ws`）始终收到对应播放器事件。
8. 播放器切换时，`switchTo()` 同步调用 `NotifySubscribersFullState()`，向根订阅者推送 `player_switch` 事件 + 新播放器**已缓存的**初始状态（缓存为空则仅推 `player_switch`）。函数返回实际推送过的事件类型集合，用于后续 `switchSkip` 抑制（避免实时事件与 FullState 重复，但不吞首次数据）。

### 2.3 并发模型

| 组件 | 运行方式 | 说明 |
|------|----------|------|
| `router.Run()` | 阻塞主 goroutine | 合并所有播放器事件通道 |
| `player.Start()` | 独立 goroutine | 每个播放器独立轮询 |
| `srv.Start()` | 独立 goroutine | `net.Listen` + `http.Serve`（通过 `readyCh` 通知端口就绪） |
| `watchExpire()` | 独立 goroutine + 1s Ticker | 播放器超时检查（prior + normal） |
| WS 写出 | 每连接一个 goroutine | 从 `subscriber.ch` 读取并写入 |
| SSE 处理 | 阻塞在请求 handler 中 | 通过 `Flusher` 推送 |

**同步原语：** `server.mu`（状态缓存）、`router.mu`（路由状态）、`server.subMu`（订阅者集合）、`proc` 包级 `sync.Mutex`（Windows 回调全局变量）。

**通道缓冲：** 播放器事件 128、订阅者推送 64（溢出时 `select default` 丢弃）。

---

## 3. 统一日志规范

### 3.1 核心规则

所有运行时日志输出**必须**使用 `logger` 包，**禁止**直接使用 `fmt.Println`、`fmt.Printf`、`log.Printf` 进行状态/调试输出。

```go
import "Metabox-Nexus-PlayerCap/logger"

var log = logger.New("模块名")

log.Info("服务启动，监听 %s", addr)
log.Success("配置加载完成")
log.Warn("连接超时，%d 秒后重试", seconds)
log.Error("无法打开文件: %v", err)
log.Detail("读取 %d 字节", n)
```

### 3.2 日志级别

| 方法 | 图标 | 用途 |
|------|------|------|
| `Info` | `[*]` | 一般运行信息：启动、连接、状态变化 |
| `Success` | `[✓]` | 操作成功：配置加载完成、连接建立 |
| `Warn` | `[!]` | 警告：文件缺失用默认值、超时重试、非致命异常 |
| `Error` | `[✗]` | 错误：操作失败但程序可继续运行 |
| `Detail` | `[+]` | 详细/调试信息：数据内容、中间步骤 |

### 3.3 输出格式

```
2026/04/08 12:00:00 [模块名] [图标] 消息内容
```

时间戳由 `log.SetFlags(log.Ldate | log.Ltime)` 在 `main()` 中统一设定，各模块无需关注。

### 3.4 模块命名

logger 实例的模块名应与功能区域对应，当前已注册的模块名：

| 模块名 | 位置 | 说明 |
|--------|------|------|
| `Main` | `main.go` | 程序入口、自动更新 |
| `Config` | `config/config.go` | 配置加载 |
| `Router` | `server/router.go` | 多播放器路由 |
| `Server` | `server/server.go` | WS/SSE 服务与订阅管理 |
| `Cover` | `player/cover.go` | 公共封面下载 |
| `Wesing` | `player/wesing/wesing.go`、`player/wesing/lyric/` | 全民K歌（主模块 + 歌词子包共用） |
| `CloudMusic` | `player/cloudmusic/cloudmusic.go`、`player/cloudmusic/lyric/` | 网易云音乐（主模块 + 歌词子包共用） |
| `QQMusic` | `player/qqmusic/qqmusic.go`、`player/qqmusic/api.go`、`player/qqmusic/mem.go` | QQ 音乐（所有文件共用同一 logger） |
| `CDP` | `player/cloudmusic/cdp/client.go` | Chrome DevTools 客户端 |
| `Watchdog` | `player/cloudmusic/watchdog/` | 进程监控与注册表 |

### 3.5 同包多文件的 logger 变量

- **一个包只有一个文件需要日志**：直接使用 `var log = logger.New("Name")`。
- **同 package 下多个文件共享 logger**：某一文件声明 `var log`，同包其余文件直接引用（如 `lyric` 包中 `finder.go` 声明，`timer.go` / `songinfo.go` / `reader.go` 共用；`watchdog` 包中 `process.go` 声明，`registry.go` 共用）。
- **同 package 下多个文件各需独立 logger**：使用不同变量名避免冲突（如 `server` 包中 `routerLog` 和 `serverLog`）。

### 3.6 允许使用 `fmt` 的例外场景

| 场景 | 原因 |
|------|------|
| 启动 Banner（`═══` 装饰框） | 纯装饰性输出，不属于日志 |
| 更新通知框（`╔═══╗`） | 专用 UI 区块 |
| 进度条（`\r` 覆写） | 需要行内覆写，logger 不支持 |
| 用户交互提示（"按回车键退出"） | 等待用户输入的 prompt |
| SSE/HTTP 协议输出（`fmt.Fprintf(w, ...)`） | 向 `http.ResponseWriter` 写协议数据 |

### 3.7 跨播放器日志文本统一规范

所有播放器的命令行日志消息**必须全部使用中文**，且相同语义的场景应采用统一的措辞格式，确保新播放器接入时输出风格一致。

| 场景 | 统一格式 | 示例 |
|------|----------|------|
| 切歌 | `♪ 歌曲: <title>` | `♪ 歌曲: Lost my Love - 10th Avenue Cafe` |
| 歌词加载完成 | `歌词加载完成[来源]: %d 行` | `歌词加载完成: 44 行`、`歌词加载完成(Redux): 38 行` |
| 歌词获取失败 | `歌词获取失败: %v` | `歌词获取失败: empty result` |
| 纯音乐/无歌词 | `检测到纯音乐/无歌词，清空歌词` | — |
| 封面获取 | `封面已获取 (%d bytes → base64)` | 由公共 `player/cover.go` 统一输出 |
| 进程等待 | `等待 <进程名> 启动...` | `等待 WeSing.exe 启动...` |
| 连接/会话建立 | `<方式>已连接，开始轮询...` | `CDP 已连接，开始轮询...` |
| 窗口/进程退出 | `<描述>已关闭` | `K歌窗口已关闭` |
| seek/回跳检测 | `检测到回跳: %.2fs → %.2fs` | — |

**规则：**
- 禁止混用中英文表述同一语义（如同时出现 "Fetched lyrics" 和 "歌词加载完成"）。
- 来源标注放在括号内作为后缀（如 `(CDP)`、`(Redux)`、`(API)`），无特殊来源时省略。
- 新播放器接入时，参照上表对齐已有播放器的输出格式。

---

## 4. 播放器接入规范

### 4.1 Player 接口

新播放器**必须**实现 `player.Player` 接口（定义在 `player/player.go`）：

```go
type Player interface {
    Name() string            // 返回播放器标识名，如 "wesing", "cloudmusicv3"
    Start()                  // 启动轮询循环（阻塞，应在 goroutine 中调用）
    Stop()                   // 停止播放器（通过 StopCh 通知）
    Events() <-chan Event    // 返回只读事件通道，由 Router 消费
}
```

`player.go` 同时提供 `BaseEmitter` 公共结构体，已实现 `Name()`、`Stop()`、`Events()`、`Emit()` 四个方法。新播放器只需嵌入 `BaseEmitter` 并实现 `Start()` 即可满足接口。

### 4.2 推荐结构体模版（嵌入 BaseEmitter）

```go
type MyPlayer struct {
    player.BaseEmitter        // 嵌入公共发射器（提供 Name/Stop/Events/Emit）
    offsetMs int
    pollMs   int
}

func New(offsetMs, pollMs int) *MyPlayer {
    return &MyPlayer{
        BaseEmitter: player.NewBaseEmitter(PlayerName),
        offsetMs:    offsetMs,
        pollMs:      pollMs,
    }
}

// 只需实现 Start()，其余接口方法由 BaseEmitter 提供。
// 在 Start() 内通过 p.Emit(eventType, data) 发射事件，
// 通过 p.StopCh 监听停止信号。
```

### 4.3 事件类型与载荷

通过 `Events()` 通道发送 `player.Event{PlayerName, Type, Data}`：

| 常量 | Data 类型 | 说明 |
|------|-----------|------|
| `EventStatusUpdate` | `StatusInfo` | 播放器状态变化 |
| `EventSongInfoUpdate` | `SongInfo` | 歌曲元信息（歌名、歌手、封面） |
| `EventLyricUpdate` | `LyricUpdate` | 当前歌词行（含进度、`sub_text` 副歌词） |
| `EventAllLyrics` | `AllLyricsData` | 完整歌词列表 + 时长（每行含 `sub_text`） |
| `EventPlaybackPause` | `PlaybackTimeInfo` | 播放暂停 |
| `EventPlaybackResume` | `PlaybackTimeInfo` | 播放恢复（也用于 seek/时间跳转通知，见 §4.7） |
| `EventLyricIdle` | `nil` | 歌词空闲 |
| `EventClearSongData` | `nil` | 清除歌曲数据 |
| `EventPlayerSwitch` | `PlayerSwitchInfo` | 播放器切换（由 Router 发出） |
| `EventPlayerClear` | `struct{}{}` | 活跃播放器清除，无播放器输出（由 Router 发出） |

#### 副歌词字段（`sub_text`）

`LyricUpdate` 和 `LyricLine`（`AllLyricsData.Lyrics` 中的每行）均包含 `sub_text` 字段，用于携带翻译、音译等副歌词。该字段为 future-proof 设计，当前始终为空字符串 `""`。JSON 序列化时**保留字段**（不使用 `omitempty`），确保下游消费者无需判断字段是否存在。

### 4.4 StatusInfo 状态值约定

播放器应通过 `EventStatusUpdate` 报告以下标准状态：

| Status 值 | 场景 |
|-----------|------|
| `waiting_process` | 等待目标进程启动 |
| `waiting_song` | 进程在线但无歌曲播放 |
| `loading` | 歌曲加载中 |
| `playing` | 正在播放 |
| `paused` | 已暂停 |
| `standby` | 会话结束待重连 |

Router 内部会将这些状态归一化为四类：`playing` / `loading` / `paused` / `idle`。

### 4.5 Start() 主循环模式

三种已有参考实现：

**模式 A — WeSing（进程内存读取）：**
```
Start():
  loop:
    emit status("waiting_process")
    waitForProcess()          // 阻塞直到进程出现
    runSession():             // 会话：查找数据结构 → 轮询
      initSong()              // PE 解析 + 堆扫描定位歌词和播放时间
      pollLyrics()            // 高频读内存 → 比对行号 → emit 事件
    emit status("standby")
    sleep 2s
```

**模式 B — CloudMusic（CDP 远程调试）：**
```
Start():
  patchRegistry()             // 注册表注入调试端口
  loop:
    emit status("waiting_process")
    ensureDebugMode()          // 确保带 --remote-debugging-port 启动
    cdp.Connect()              // WebSocket 连接浏览器
    runSession():              // 会话：Ticker 驱动
      ticker.C:
        cdp.Extract()          // 执行 JS → 取 Redux + DOM 数据
        detectSongChange()     // 歌名变化 → 获取歌词/封面
        syncClock()            // 本地时钟锚定 + seek 检测
        matchLyricLine()       // 行号比对 → emit 事件
    emit status("standby")
    sleep 2s
```

**模式 C — QQMusic（进程内存读取 + AOB Hook 注入）：**
```
Start():
  loop:
    emit status("waiting_process")
    ConnectQQMusic()           // 找 QQMusic.exe + QQMusic.dll → OpenProcess
    runSession():              // 会话：Hook 注入 → 轮询
      InjectSliderAOB()        // 滑块 Hook：捕获伴奏滑块控件对象指针
      InjectProgressAOB()      // 进度 Hook：捕获精确进度(ms) + KUSER 时间戳
      pollLoop():
        ReadAllMetadata()      // 读歌名/歌手/SongID/进度/时长
        detectSongChange()     // 歌名变化 → fetchLRC + fetchCoverURL
        hybridInterpolation()  // 快速计时器锚点 + Hook 时间戳插值
        detectPause/Resume     // 快速计时器停滞 1s → 暂停
        detectSeek()           // Hook timestamp delta > 3s → seek
        matchLyricLine()       // 行号比对 → emit lyric_update
    emit status("standby")
    sleep 2s
```

### 4.6 目录与命名约定

- 播放器实现放在 `player/<name>/` 下，子功能可用子目录。
- 播放器标识名（`Name()` 返回值）必须与配置中的 YAML key 前缀一致。
- 配置字段遵循 `<name>-offset` / `<name>-poll` 命名模式。

### 4.7 播放器时间跳转（seek）复用 `playback_resume`

播放中检测到大幅时间跳变（seek/回放）时，播放器**不发送独立的 seek 事件**，而是复用 `EventPlaybackResume`（携带跳转后的 `PlayTime`）。下游消费者通过 `PlaybackTimeInfo.PlayTime` 即可获取最新播放位置，无需区分"恢复播放"与"时间跳转"。

| 播放器 | 检测方式 | 阈值 |
|--------|----------|------|
| WeSing | `playTime < lastPlayTime - 2.0`（内存读取的播放时间回退） | 2 秒 |
| CloudMusic | `abs(domTimeSec - clock) > 1.5`（DOM 进度条与本地时钟偏差） | 1.5 秒 |
| QQMusic | `abs(progressDelta - timeDelta) > 3000`（Hook 进度与时间戳差值） | 3 秒 |

两端检测到跳转后均重置内部时钟锚点并发射 `playback_resume`，使 `lyric_update` 后续行号同步到新位置。

### 4.8 完整接入步骤

1. 在 `player/<name>/` 下创建包，嵌入 `player.BaseEmitter` 并实现 `Start()` 方法。
2. 声明 `const PlayerName = "<name>"` 和 `var log = logger.New("DisplayName")`。
3. **自动注册配置**：在包中添加 `init()` 调用 `config.RegisterPlayer(PlayerName)`。
   这会自动完成：
   - 生成 `-<name>-offset` / `-<name>-poll` CLI flag。
   - 支持 `config.yml` 中 `<name>-offset` / `<name>-poll` YAML 字段。
   - `GetPlayerOffset()` / `GetPlayerPoll()` 自动识别新播放器。
   - **无需手动修改 `config/config.go`**。
4. 在 `main.go` 中：
   - import 新包，创建实例 `newPlayer := newpkg.New(offset, poll)`。
   - 将实例加入 `router.Register()` 调用。
   - 在 Banner 中添加播放器信息行。
5. （可选）更新 `config.yml` 默认模板中的注释。

---

## 5. 配置系统

### 5.1 三层优先级

```
CLI flag（最高） > config.yml > DefaultConfig()（最低）
```

### 5.2 Config 结构体

```go
// PlayerConfig 单播放器配置覆盖
type PlayerConfig struct {
    Offset *int   // nil = 沿用全局
    Poll   *int
}

type Config struct {
    Addr              string                   `yaml:"addr"`                  // "0.0.0.0:8765"
    Offset            int                      `yaml:"offset"`                // 200 (全局歌词偏移，毫秒)
    Poll              int                      `yaml:"poll"`                  // 30  (全局轮询间隔，毫秒，夹紧到 [10, 2000])
    PriorPlayer       []string                 `yaml:"prior-player"`          // ["wesing"]
    PriorPlayerExpire int                      `yaml:"prior-player-expire"`   // 15 (秒)
    Players           map[string]*PlayerConfig `yaml:"-"`                     // 各播放器专属配置（动态注册）
    Sources           []string                 `yaml:"-"`                     // 内部字段，记录配置来源
}
```

### 5.3 关键设计

- **动态播放器注册**：播放器包在 `init()` 中调用 `config.RegisterPlayer(name)`，配置系统自动为其生成 CLI flag（`-<name>-offset` / `-<name>-poll`）和 YAML 字段支持，无需修改 `config.go`。
- **指针字段区分"未设置"与"设置为 0"**：`PlayerConfig` 中的 offset/poll 使用 `*int`，`nil` 表示沿用全局值。
- **`mergeYAML()` 按字段选择性合并**：只覆盖 YAML 文件中实际存在的字段，避免默认值覆盖 CLI 参数；自动遍历已注册播放器检查 `<name>-offset` / `<name>-poll`。
- **`GetPlayerOffset(name)` / `GetPlayerPoll(name)`**：运行时统一入口，从 `Players` map 查找专属值后 fallback 到全局。
- **自动生成**：`config.yml` 不存在时自动生成默认模板，含中文注释。

### 5.4 CLI Flags

| Flag | 说明 |
|------|------|
| `-addr` | 监听地址（覆盖配置文件） |
| `-offset` | 全局歌词偏移（毫秒） |
| `-poll` | 轮询间隔（毫秒，夹紧到 10–2000） |
| `-<player>-offset` | 播放器专属歌词偏移（自动注册） |
| `-<player>-poll` | 播放器专属轮询间隔（自动注册） |

> 播放器专属 flag 由 `config.RegisterPlayer()` 动态生成，当前包括 `-wesing-offset`、`-wesing-poll`、`-cloudmusicv3-offset`、`-cloudmusicv3-poll`。

---

## 6. 服务层规范

### 6.1 传输层类型

```go
// WebSocket / SSE 事件包装
type WSEvent struct {
    Type   string      `json:"type"`    // 事件类型常量
    Player string      `json:"player"`  // 来源播放器名
    Data   interface{} `json:"data"`    // 载荷
}

// HTTP JSON 响应
type HTTPResponse struct {
    Code   int         `json:"code"`    // 0 = 成功
    Msg    string      `json:"msg"`     // "success"
    Player string      `json:"player"`  // 播放器名或 "internal"
    Data   interface{} `json:"data"`    // 响应数据（无数据时为 {} 空对象，永远不返回 null）
}
```

### 6.2 订阅者模型

- **根订阅者**（连到 `/ws`、`/lyric_update-SSE` 等）：只收 `activePlayer` 的事件。
- **单播放器订阅者**（连到 `/wesing/ws`、`/cloudmusicv3/lyric_update-SSE` 等）：只收指定播放器事件。
- SSE 端点支持按 `eventTypes` 过滤（如 `/lyric_update-SSE` 只推 `lyric_update`）。
- 新连接建立时自动推送缓存的初始状态（status + songinfo + all_lyrics）。

### 6.3 端点命名规则

路由采用声明式定义，自动为每个播放器生成命名空间：

```
/               → 根（activePlayer）
/<player>/      → 单播放器命名空间

示例：
/ws             → 根 WebSocket
/wesing/ws      → 全民K歌 WebSocket
/all_lyrics     → 根 HTTP（返回 activePlayer 的歌词）
/cloudmusicv3/all_lyrics → 网易云 HTTP
```

### 6.4 CORS

全局中间件设置 `Access-Control-Allow-Origin: *` + `Access-Control-Allow-Headers: *`。

---

## 7. 自动更新机制

`main.go` 中的 `checkAndUpdate()` 在启动时运行：

1. **版本检查**：GET 远程版本 JSON，读取 `tag_name` 作为真实目标版本，按完整 semver 比较。
2. **CDN 选择**：`pickFastestCDNPrefix()` 测速，< 10 KB/s 自动切换到中国镜像。
3. **下载验证**：逐文件下载 → SHA256 校验 → `.exe` 优先处理（备份为 `.old`）。
4. **重启**：`exec.Command(self).Start()` + `os.Exit(0)`。
5. **跳过条件**：Version == `"0.0.0"` 或非 semver 格式的开发构建跳过更新检查。
6. **预发布顺序**：`alpha < beta < rc < stable`，允许如 `v3.0.0-beta.32 -> v3.1.0-alpha.13` 这样的纯 semver 自动升级。
7. **强制同步**：仅当 release `name` 以 `-force` 结尾时，客户端才允许同步到更低版本；`tag_name` 始终保持真实版本号，不携带 `-force`。

### 7.1 发布流程

本项目的更新接口实际镜像 GitHub 的 `/releases/latest` 内容，因此**是否会被客户端看到**，取决于该 Release 是否成为 GitHub 认定的 latest。

正常发布步骤：

1. 创建真实版本 tag，格式使用完整 semver，例如 `v3.0.0`、`v3.0.0-beta.32`、`v3.1.0-alpha.13`。
2. 推送 tag，等待 [.github/workflows/release.yml](.github/workflows/release.yml) 生成 Release Draft。
3. 检查 Draft 的 `tag_name`、标题 `name`、附件和发布说明。默认情况下，`tag_name` 与 `name` 应保持一致，不追加 `-force`。
4. 如果希望该版本被网关推出并成为客户端更新目标，发布该 Release，并确保它不是 Draft，也**不要**勾选 GitHub 的 `pre-release` 选项。即使 `tag_name` 带有 `-alpha`、`-beta`、`-rc` 后缀，只要 GitHub Release 被标记为 `pre-release`，`/releases/latest` 也不会返回它。
5. 将该 Release 设为 latest release。缓存清理工作流会在 latest 变化后自动刷新网关缓存。
6. 发布完成后，客户端会按完整 semver 自动决定是否升级。

### 7.2 回退流程

回退依赖两个条件同时成立：

1. 目标 Release 必须能被 GitHub `/releases/latest` 返回。
2. 目标 Release 的标题 `name` 必须以 `-force` 结尾，忽略大小写。

回退步骤：

1. 选择要回退到的历史 Release。该 Release 必须已发布、不是 Draft，且不能勾选 GitHub 的 `pre-release` 选项。
2. 编辑该 Release 的标题 `name`，在末尾追加 `-force`，例如把 `v3.0.0` 改成 `v3.0.0-force`。`tag_name` 保持原样，不能追加 `-force`。
3. 将这个已追加 `-force` 的旧版 Release 设为 latest release。
4. 等待网关缓存刷新完成。由于更新接口镜像的是 `/releases/latest`，一旦 latest 指向变化，客户端下一次检查更新时就会拿到这个目标版本。
5. 客户端行为如下：
    - 当前版本低于目标版本：按正常升级处理。
    - 当前版本高于目标版本：因为 `name` 以 `-force` 结尾，允许强制同步到较低版本。
    - 当前版本等于目标版本：不更新。

### 7.3 操作约束

1. `-force` 只允许出现在 Release 标题 `name` 的末尾，不进入 `tag_name`。
2. 默认情况下，Release 标题应与 `tag_name` 一致；只有在需要强制回退时才手动追加 `-force`。
3. 如果后续重新切换到更高版本，只需把新的目标 Release 设为 latest；旧 Release 上残留的 `-force` 不会影响新的 latest。

---

## 8. Windows 特有机制

### 8.1 WeSing 内存读取 (`player/wesing/proc/memory.go`)

| 功能 | API / 方法 |
|------|------------|
| 进程发现 | `CreateToolhelp32Snapshot` + `Process32First/Next` |
| 模块枚举 | `Module32First/Next` |
| 内存读取 | `ReadProcessMemory`（支持 `uint32` / `float32` / C-string / UTF-16LE） |
| 可写区域枚举 | `VirtualQueryEx` 循环（`MEM_COMMIT` + `PAGE_READWRITE`） |
| AOB 扫描 | 线性字节匹配，支持通配符 mask，单区域 64MB 安全上限 |
| 窗口枚举 | `EnumWindows` + 回调（单实例 `syscall.NewCallback` + 包级 Mutex 防泄漏） |
| 窗口状态 | `IsIconic`（最小化）、`GetGUIThreadInfo`（`GUI_INMOVESIZE` 拖动检测） |

### 8.2 CloudMusic Watchdog

- **进程管理**：通过 `gopsutil` 查找 `cloudmusic.exe` → 检查命令行是否含 `--remote-debugging-port=9222` → 缺失则 `taskkill /F` 后以新参数重启。
- **注册表注入**：修改 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 中 `cloudmusic` 自启项，追加调试端口参数。

### 8.3 回调安全

Go 的 `syscall.NewCallback` 有**全局数量上限**。本项目通过包级变量复用同一个回调实例 + `sync.Mutex` 保护全局结果变量，避免反复创建回调导致 "too many callbacks" panic。

---

## 9. 代码风格与约定

### 9.1 import 分组

```go
import (
    // 标准库
    "fmt"
    "sync"

    // 项目内部包
    "Metabox-Nexus-PlayerCap/logger"
    "Metabox-Nexus-PlayerCap/player"

    // 第三方
    "github.com/gorilla/websocket"
)
```

三组之间用空行分隔。

### 9.2 错误处理

- **系统边界**（用户输入、文件 IO、网络请求）：检查并处理错误。
- **内部调用**：信任上层保证，不做防御性校验。
- **播放器轮询**：内存读取失败 → 跳过本轮、下轮重试，不 panic。
- **更新下载**：SHA256 校验失败 → 删除损坏文件、终止更新流程。

### 9.3 通用原则

- **不过度工程**：不为单次操作抽象 helper；不添加未被使用的功能。
- **不擅自添加**：不添加原代码中没有的注释、docstring、类型注解。
- **平台限定**：仅面向 Windows，可直接使用 `golang.org/x/sys/windows` 等平台 API，无需跨平台抽象。
- **二进制名**：编译产物必须为 `Metabox-Nexus-PlayerCap.exe`，运行时通过 `ensureCanonicalName()` 强制校验。

### 9.4 构建与发布

```powershell
# 开发构建
go build .

# 发布构建（注入版本号）
go build -ldflags "-X main.Version=1.2.3-beta.1" .

# 发布构建（附带图标资源）
copy build-assets\winicon\release\resource_windows_amd64.syso .
go build -ldflags "-X main.Version=1.2.3-beta.1 -H windowsgui" .
```
