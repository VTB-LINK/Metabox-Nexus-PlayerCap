<div align="center">

![Banner](doc/title.png)

# VTB-TOOLS Metabox-Nexus-PlayerCap

多播放器歌词实时推送服务 —— 从多个音乐播放器中提取歌词、歌曲信息与实时音频波形，并镜像网易云音乐的特效歌词画面，通过 WebSocket / HTTP / SSE 广播给外部应用。
</br>
**纯 Go 实现** · Windows 专用 · 支持优先级路由 · 自动更新

</div>

## 支持的播放器

| 播放器 | 标识名 | 提取方式 |
|--------|--------|----------|
| 全民K歌 | `wesing` | 进程内存读取 |
| 网易云音乐 | `cloudmusicv3` | CDP 远程调试 |
| QQ 音乐 | `qqmusic` | 进程内存读取 + AOB Hook 注入 |
| 酷狗音乐 | `kugou` | CDP 远程调试 |
| 汽水音乐 | `sodamusic` | CDP 远程调试 |
| Apple Music | `applemusic` | 进程内存读取 |

### 能力对照

`✔` 完整支持 · `⚠` 部分/受限支持 · `🚫` 不支持

| 播放器 | 逐字歌词 | 逐行歌词 | 翻译歌词 | 音译歌词 | 歌名 | 歌手 | 封面 | 播放状态 | 进度/跳转 | 音频波形 | 本地文件 |
|--------|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| 全民K歌 `wesing` | ✔ | ✔ | 🚫 | ✔ | ✔ | ✔ | ✔ | ✔ | ⚠ | ✔ | 🚫 |
| 网易云音乐 `cloudmusicv3` | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | 🚫 |
| QQ 音乐 `qqmusic` | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | 🚫 |
| 酷狗音乐 `kugou` | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ⚠ |
| 汽水音乐 `sodamusic` | ✔ | ✔ | ✔ | ⚠ | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | 🚫 |
| Apple Music `applemusic` | ⚠ | ⚠ | ⚠ | 🚫 | ✔ | ✔ | ⚠ | ✔ | ✔ | ✔ | 🚫 |

<details>
<summary>⚠ 与 🚫 各项的原因</summary>

`✔` 表示能取到；某首歌有没有逐字、翻译、音译，取决于平台是否提供。
「本地文件」指播放本地音频文件时能否取到歌词，标 `🚫` 的五家暂不支持，下面不逐个列出。

**全民K歌 `wesing`**

- 翻译歌词 `🚫`：平台无翻译歌词。
- 进度/跳转 `⚠`：只报回跳与重播，向前跳转不报。

**酷狗音乐 `kugou`**

- 本地文件 `⚠`：歌词来自酷狗按文件名自动搜索的结果，可能搜不到，也可能不准。

**汽水音乐 `sodamusic`**

- 音译歌词 `⚠`：平台无音译歌词，改用酷狗的音译按歌名与时长匹配补上，不保证每首都有、都准。

**Apple Music `applemusic`**

- 逐字 / 逐行 / 翻译歌词 `⚠`：歌词取自 Apple Music 的进程内存，换歌后可能晚几秒才出现，少数情况下取不到或配错；
  逐字只有平台提供逐字的歌才有。详见 [doc/core.md](doc/core.md) 的 Apple Music 一节。
- 音译歌词 `🚫`：平台无音译歌词。
- 封面 `⚠`：只有封面图片数据（`cover_base64`），没有封面 URL；换歌时封面可能稍晚补发。

</details>

## 功能特性

- ✅ **多播放器支持** — 同时监控六个播放器，按优先级自动切换输出
- ✅ **自动等待进程** — 播放器未启动时持续等待，启动后自动接入
- ✅ **进程断线重连** — 播放器退出后回到等待状态，重新启动后自动恢复
- ✅ **跨重启稳定** — 不依赖固定的内存地址，播放器重启后重新定位
- ✅ **酷狗自动接入** — 自动检测并修补 `libcef.dll` 以开启 CDP 端口，按需提权并重启酷狗
- ✅ **歌曲信息提取** — 歌名、歌手、封面 URL 与封面 Base64
- ✅ **实时歌词推送** — 广播当前歌词行与播放进度，轮询频率可调
- ✅ **逐字歌词** — 每个字的时间戳与持续时间（`text_detailed`），可做卡拉 OK 式逐字高亮
- ✅ **翻译与音译歌词** — 译文（`sub_text`）与音译（`roma_text`），各播放器的支持情况见能力对照
- ✅ **UTF-8 文本** — 歌词与歌曲信息以 UTF-8 编码传输，不限语种
- ✅ **时间偏移** — 全局与 per-player 的 offset（毫秒），正值=延后、负值=提前
- ✅ **暂停/恢复检测** — 暂停与恢复时广播对应事件
- ✅ **状态广播** — 等待进程 / 等待歌曲 / 播放中 / 暂停 / 待机
- ✅ **三种接口** — WebSocket（实时推送，含二进制的音频波形与特效画面）、SSE（实时推送）、HTTP（查询）
- ✅ **Per-player 端点** — 每个播放器有独立端点（如 `/kugou/ws`），不受活跃播放器切换影响
- ✅ **播放器切换事件** — 活跃播放器变化时推送 `player_switch` 与新播放器的完整状态
- ✅ **无活跃自动隐藏** — per-player 通道在歌停后可自动清屏（`per-player-idle-hide`，默认关）
- ✅ **网易云特效歌词镜像** — 把网易云「特效歌词」画面镜像给 OBS，不改动源画面；
  接口与参数见 [API 响应示例文档](./doc/API_RESPONSE_EXAMPLES.md#网易云特效歌词镜像)
- ✅ **实时音频波形** — 全部播放器支持，帧含 RMS、左右声道峰值与 512 点频谱；可归一化音量、可调帧率
- ✅ **配置文件** — `config.yml` 与命令行参数，优先级：命令行 > config.yml > 默认值
- ✅ **自动更新** — 启动时检查新版本，自动下载、校验并重启

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

## 快速开始

### 前置条件

- Go 1.25+
- Windows 10/11（音频波形要求 Windows 10 2004 / build 19041 及以上）
- 任意支持的播放器

### 编译

```bash
# 编译
go build -ldflags "-s -w" -o Metabox-Nexus-PlayerCap.exe .

# 编译并注入版本号（可选）
go build -ldflags "-X main.Version=3.0.0-beta.5" -o Metabox-Nexus-PlayerCap.exe .
```

### 运行

<details>
<summary>关于管理员权限</summary>

- 需要以**管理员权限**运行：读取其他进程的内存需要它。
- 酷狗音乐首次接入时会自动修补 `libcef.dll` 以开启 CDP 端口，可能弹出 UAC；遇到未适配的酷狗版本不会强行修改。

</details>

<details>
<summary>关于音频波形端点</summary>

有客户端连接 `/audio-ws` 或 `/<player>/audio-ws` 时，程序会再启动一个自身的子进程来捕获音频，任务管理器里因此有两个同名进程。
它不弹 UAC，不需要也不应手动启动；波形连接全部断开或主程序退出后自动结束。它的日志带 `[helper]` 前缀。

</details>

<details>
<summary>关于 exe 文件名</summary>

文件名不是 `Metabox-Nexus-PlayerCap.exe` 时，程序会把自己复制成这个名字再启动。
这时（以及启动时完成自动更新之后）最初启动的进程要等实际提供服务的进程结束才退出，任务管理器里多一个进程。
旧文件名 `Metabox-Nexus-WesingCap.exe` 例外：它启动复制品后立即退出，随后被自动删除。

</details>

```bash
# 直接运行（使用 config.yml 或默认配置）
.\Metabox-Nexus-PlayerCap.exe

# 歌词提前 500ms 显示（offset 是延迟量，提前写作负值）
.\Metabox-Nexus-PlayerCap.exe -offset -500

# 指定网易云音乐的 offset：提前 300ms
.\Metabox-Nexus-PlayerCap.exe -cloudmusicv3-offset -300
```

### 命令行参数

格式 `-参数 值` 或 `-参数=值`（如 `-poll 50`；负值照写，`-offset -200` 或 `-offset=-200`），优先级最高（> config.yml > 内置默认）；
`-h` 打印全部参数。开关类参数（`-audio-normalize`、`-<player>-audio-normalize`）只写 `-参数` 为开，
`-参数 false` 或 `-参数=false` 为关。命令行里多余的词（如漏了横线的 `offset -200`）不会被忽略：程序报出该词、打印用法后退出。

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `0.0.0.0:8765` | HTTP/WebSocket/SSE 监听地址 |
| `-offset` | `-200` | 全局歌词延迟（毫秒），正值=延后，负值=提前 |
| `-poll` | `30` | 全局轮询间隔（毫秒），范围 10~2000 |
| `-prior-player` | `wesing` | 优先播放器列表，逗号分隔（如 `wesing,kugou`）；传空串=无优先播放器 |
| `-prior-player-expire` | `15` | 优先播放器暂停超过 N 秒后释放主输出；`0`=关闭全部超时，慎用 |
| `-per-player-idle-hide` | `0` | per-player 通道无活跃 N 秒后自动清屏；`0`=关 |
| `-audio-normalize` | `true` | 音频波形音量归一化，只影响波形数据，不改变实际播放音量 |
| `-audio-normalize-target` | `-14` | 归一化目标响度（dBFS），范围 -60~0 |
| `-audio-normalize-max-gain` | `30` | 归一化最大增益（dB），范围 0~45 |
| `-audio-frame-rate` | `25` | 音频波形帧率（帧每秒），范围 15~60 |
| `-cloudmusicv3-effect-strategy` | `fadeout` | 网易云特效最小化策略：`park`（屏外渲染保活）/ `fadeout`（自动淡出） |
| `-<player>-offset` | *(沿用 `-offset`)* | 指定播放器的歌词延迟（毫秒） |
| `-<player>-poll` | *(沿用 `-poll`)* | 指定播放器的轮询间隔（毫秒） |
| `-<player>-idle-hide` | *(沿用 `-per-player-idle-hide`)* | 指定播放器的无活跃自动隐藏（秒，`0`=关） |
| `-<player>-audio-normalize` | *(沿用 `-audio-normalize`)* | 指定播放器的归一化开关 |

> `<player>` ∈ `wesing` / `cloudmusicv3` / `qqmusic` / `kugou` / `sodamusic` / `applemusic`，未设置时沿用全局值。
> 例：`-cloudmusicv3-offset -300`、`-wesing-idle-hide 15`、`-qqmusic-audio-normalize false`。
>
> 越界的取值自动钳到边界。各播放器另有自己的轮询下限，低于它的取值会被抬高：cloudmusicv3 低于 50 抬到 100、
> qqmusic 低于 30 抬到 50、sodamusic 低于 200 抬到 200、applemusic 低于 50 抬到 50。
> 实际生效的值见启动横幅与 `/service-status` 的 `config_effective`。

### 配置文件

优先级：**命令行参数** > **config.yml** > **内置默认值**

程序启动时自动加载同目录下的 `config.yml`，若不存在则自动生成；本次实际读取的是哪一份，见启动横幅的「配置文件」一行。
各键的含义写在 `config.yml` 的注释里；完整示例，以及版本、自动迁移与重建的说明，见 [CONFIG.README.md](CONFIG.README.md)。

### 预期输出

```
===========================================================
VTB-TOOLS Metabox Nexus-PlayerCap 多播放器歌词实时推送服务
===========================================================
   版本: v0.0.0
   配置文件: C:\PlayerCap\config.yml
   监听: *addr=0.0.0.0:8765
   全局偏移与轮询: *offset=-200ms *poll=30ms
   优先播放器: *prior-player=[wesing] *prior-player-expire=15s
   per-player 无活跃自动隐藏: *per-player-idle-hide=0s
   音量归一化: *audio-normalize=true *audio-normalize-target=-14dBFS *audio-normalize-max-gain=30dB
   音频波形帧率: *audio-frame-rate=25fps
   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: cloudmusicv3 (*offset=-500ms poll=30ms → 100ms idle-hide=0s audio-normalize=true *effect-strategy=fadeout)
   播放器: qqmusic (*offset=-400ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: kugou (*offset=-430ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: sodamusic (*offset=-340ms poll=30ms → 200ms idle-hide=0s audio-normalize=true)
   播放器: applemusic (*offset=-400ms poll=30ms → 50ms idle-hide=0s audio-normalize=true)
   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b
===========================================================
```

> `v0.0.0` 是默认的开发构建版本，不参与自动更新检查。
>
> 上例是自动生成的 `config.yml` 原样加载时的输出，各配置行与 `/service-status` 的配置回显同源：
>
> - 带 `*` 的是在 `config.yml` 或命令行里显式设置的键。
> - 写下的值没有按原样生效时（越界被钳制、低于播放器的轮询下限、取值非法等）写成 `写下的值 → 生效值`。
> - 「配置文件」一行是本次实际读取的那一份的绝对路径；新生成、读取或解析失败时在行末注明，
>   本次启动改写了版本、做了迁移或目录里另有备份时在下一行注明。

---

## 下游接入

> 各接口事件类型、字段结构及完整消息示例请参阅 [在线 API 文档](https://playercap.nexus.metabox.apifox.vtb.link/)，README 不再同步维护此部分内容。

### 接口清单参考

**根端点**（返回当前活跃播放器数据）：

| 端点 | 类型 | 说明 |
|------|------|------|
| `/health-check` | HTTP | 健康检查 |
| `/service-status` | HTTP | 服务状态（版本、配置、播放器状态、已连接的客户端） |
| `/ws` | WebSocket | 实时事件推送（全部事件） |
| `/audio-ws` | WebSocket | 音频波形帧流（二进制），跟随活跃播放器 |
| `/all_lyrics` | HTTP | 完整歌词列表 |
| `/lyric_update` | HTTP | 当前歌词行 |
| `/status_update` | HTTP | 播放状态 |
| `/song_info` | HTTP | 歌曲信息 |
| `/lyric_update-SSE` | SSE | 实时歌词推送流 |
| `/song_info-SSE` | SSE | 实时歌曲信息推送流 |
| `/cloudmusicv3/effect-ws` | WebSocket | 网易云特效画面镜像（二进制 JPEG 帧 + 文本状态） |
| `/cloudmusicv3/effect-ingest` | WebSocket | 内部使用，非用户接口 |

**Per-player 端点**（始终返回指定播放器数据，不受路由切换影响）：

除 `/health-check`、`/service-status` 与两个 `/cloudmusicv3/effect-*` 端点外，所有根端点均有对应的播放器路径版本，
`<player>` 换成播放器的标识名即可（如 `/kugou/ws`）：

```
/<player>/ws
/<player>/audio-ws
/<player>/all_lyrics
/<player>/lyric_update
/<player>/status_update
/<player>/song_info
/<player>/lyric_update-SSE
/<player>/song_info-SSE
```

---

## 依赖

| 依赖 | 用途 |
|------|------|
| `github.com/getsentry/sentry-go` | 崩溃与错误上报（未注入 DSN 的构建自动禁用） |
| `github.com/go-ole/go-ole` | 任务栏进度显示（ITaskbarList3 COM） |
| `github.com/gorilla/websocket` | WebSocket 服务 |
| `github.com/josephspurrier/goversioninfo` | 构建期生成 exe 图标与版本信息资源（`tools/winicon`） |
| `github.com/shirou/gopsutil/v3` | 进程枚举与命令行读取（网易云 / 汽水 watchdog、网易云 park、音频捕获子进程孤儿清理） |
| `golang.org/x/image` | 构建期图标缩放（`tools/winicon`） |
| `golang.org/x/mod` | 自动更新的版本号比较（semver） |
| `golang.org/x/sys` | Windows 系统调用 |
| `gopkg.in/yaml.v3` | YAML 配置文件解析 |

## License

MIT
