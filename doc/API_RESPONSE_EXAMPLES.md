# Metabox-Nexus-PlayerCap API 响应示例

> **多播放器架构：** 所有 HTTP 响应和 WS/SSE 事件均包含 `player` 字段，标识数据来源的播放器（如 `"wesing"`、`"cloudmusicv3"`、`"qqmusic"`）。  
> **空数据约定：** 所有事件在无数据时统一返回 `"data": {}`（空对象），而非 `null`。  
> **Per-player 端点：** 除 `/health-check` 和 `/service-status` 外，所有端点均有播放器专属路径版本（如 `/wesing/ws`、`/cloudmusicv3/all_lyrics`、`/qqmusic/ws`）。根端点跟随活跃播放器，Per-player 端点始终返回指定播放器数据。

---

## HTTP 接口（静态数据）

### 1. `/health-check` - 健康检查

真实响应原文：

```json
{
  "code": 0,
  "msg": "success",
  "player": "internal",
  "data": {
    "now_time": "2026-07-17T05:53:58+08:00"
  }
}
```

`now_time` 是 RFC3339 带时区。这个端点不依赖任何播放器，服务活着就返回 200。

---

### 2. `/service-status` - 服务状态信息
```json
{
  "code": 0,
  "msg": "success",
  "player": "internal",
  "data": {
    "version": "0.0.0",
    "addr": "0.0.0.0:8765",
    "now_time": "2026-07-17T05:53:58+08:00",
    "config_sources": [
      "config.yml"
    ],
    "config": {
      "addr": "0.0.0.0:8765",
      "offset": 200,
      "poll": 30,
      "prior-player": [
        "wesing"
      ],
      "prior-player-expire": 15,
      "cloudmusicv3-effect-strategy": "fadeout",
      "wesing-offset": 200,
      "wesing-poll": 30,
      "cloudmusicv3-offset": 500,
      "cloudmusicv3-poll": 30,
      "qqmusic-offset": 400,
      "qqmusic-poll": 30,
      "kugou-offset": 430,
      "kugou-poll": 30
    },
    "config_overwritten": [
      "addr",
      "offset",
      "poll",
      "prior-player",
      "prior-player-expire",
      "cloudmusicv3-effect-strategy",
      "cloudmusicv3-offset",
      "qqmusic-offset",
      "kugou-offset"
    ],
    "player_support": [
      "wesing",
      "cloudmusicv3",
      "qqmusic",
      "kugou"
    ],
    "player_running": [],
    "player_status": {
      "wesing": "waiting_process",
      "cloudmusicv3": "waiting_process",
      "qqmusic": "waiting_process",
      "kugou": "waiting_process"
    },
    "endpoints": {
      "health-check": "http://0.0.0.0:8765/health-check",
      "service-status": "http://0.0.0.0:8765/service-status",
      "ws": "ws://0.0.0.0:8765/ws",
      "all_lyrics": "http://0.0.0.0:8765/all_lyrics",
      "lyric_update": "http://0.0.0.0:8765/lyric_update",
      "status_update": "http://0.0.0.0:8765/status_update",
      "song_info": "http://0.0.0.0:8765/song_info",
      "lyric_update-SSE": "http://0.0.0.0:8765/lyric_update-SSE",
      "song_info-SSE": "http://0.0.0.0:8765/song_info-SSE",
      "wesing": {
        "ws": "ws://0.0.0.0:8765/wesing/ws",
        "all_lyrics": "http://0.0.0.0:8765/wesing/all_lyrics",
        "lyric_update": "http://0.0.0.0:8765/wesing/lyric_update",
        "status_update": "http://0.0.0.0:8765/wesing/status_update",
        "song_info": "http://0.0.0.0:8765/wesing/song_info",
        "lyric_update-SSE": "http://0.0.0.0:8765/wesing/lyric_update-SSE",
        "song_info-SSE": "http://0.0.0.0:8765/wesing/song_info-SSE"
      },
      "cloudmusicv3": {
        "ws": "ws://0.0.0.0:8765/cloudmusicv3/ws",
        "all_lyrics": "http://0.0.0.0:8765/cloudmusicv3/all_lyrics",
        "lyric_update": "http://0.0.0.0:8765/cloudmusicv3/lyric_update",
        "status_update": "http://0.0.0.0:8765/cloudmusicv3/status_update",
        "song_info": "http://0.0.0.0:8765/cloudmusicv3/song_info",
        "lyric_update-SSE": "http://0.0.0.0:8765/cloudmusicv3/lyric_update-SSE",
        "song_info-SSE": "http://0.0.0.0:8765/cloudmusicv3/song_info-SSE",
        "effect-ws": "ws://0.0.0.0:8765/cloudmusicv3/effect-ws",
        "effect-ingest": "ws://0.0.0.0:8765/cloudmusicv3/effect-ingest"
      },
      "qqmusic": {
        "ws": "ws://0.0.0.0:8765/qqmusic/ws",
        "all_lyrics": "http://0.0.0.0:8765/qqmusic/all_lyrics",
        "lyric_update": "http://0.0.0.0:8765/qqmusic/lyric_update",
        "status_update": "http://0.0.0.0:8765/qqmusic/status_update",
        "song_info": "http://0.0.0.0:8765/qqmusic/song_info",
        "lyric_update-SSE": "http://0.0.0.0:8765/qqmusic/lyric_update-SSE",
        "song_info-SSE": "http://0.0.0.0:8765/qqmusic/song_info-SSE"
      },
      "kugou": {
        "ws": "ws://0.0.0.0:8765/kugou/ws",
        "all_lyrics": "http://0.0.0.0:8765/kugou/all_lyrics",
        "lyric_update": "http://0.0.0.0:8765/kugou/lyric_update",
        "status_update": "http://0.0.0.0:8765/kugou/status_update",
        "song_info": "http://0.0.0.0:8765/kugou/song_info",
        "lyric_update-SSE": "http://0.0.0.0:8765/kugou/lyric_update-SSE",
        "song_info-SSE": "http://0.0.0.0:8765/kugou/song_info-SSE"
      }
    },
    "client_count": 1,
    "ws_connected": {
      "clients": [
        "[::1]:62354"
      ],
      "connected": true
    }
  }
}
```

> **以上为真实响应原文**（2026-07-17 录制，本机开发构建、四个播放器均未启动）。所以你会看到
> `version: "0.0.0"`（未注入版本号）、`player_running: []`、`player_status` 全是 `waiting_process`
> ——那是「服务起了但没人放歌」的**常态**，不是残缺。
>
> **这个端点是端点全表的运行时真源。** 它比本文档更新得快：新增播放器时 `player_support`
> 与 `endpoints` 自动跟着变，而文档要靠人改。**拿不准有哪些端点时，直接问它。**

**version 说明：**
- 编译时通过 `-ldflags "-X main.Version=3.0.0-beta.1"` 注入
- 默认值为 `0.0.0`（如上例——本机 `go build` 不注入版本）
- `tag_name` 使用完整 semver；若 release 标题以 `-force` 结尾，则客户端允许强制同步到更低版本

**config_sources 说明：**
- 显示配置来源的完整链路，按优先级顺序排列
- 可能的值：`"内置默认"`、`"config.yml"`、`"命令行参数"`
- 示例：
  - `["内置默认"]` - 使用所有默认值
  - `["config.yml"]` - 所有值来自 config.yml
  - `["config.yml", "命令行参数"]` - 从 config.yml 加载，部分被命令行参数覆盖

**root WS 初始化说明：**
- 若连入 `/ws` 时存在活跃播放器，服务端会先补发该播放器当前缓存的 `status_update` / `song_info_update` / `all_lyrics` / `lyric_update`
- 若连入 `/ws` 时当前没有活跃播放器，服务端会立即发送一个 `player_clear` 事件，而不是静默不输出

**root HTTP 端点说明**（与上面的 WS 规定对称，两者对同一状态给出等价答案）：
- 四个根端点（`/all_lyrics` `/lyric_update` `/status_update` `/song_info`）跟随活跃播放器
- **没有活跃播放器时，根端点返回 `"player": ""` + `"data": {}`**，等价于 WS 侧的 `player_clear`。
  服务端**不会**改为挑一个「有状态的播放器」顶上——待机中（`waiting_process` / `standby`）
  的播放器不是活跃播放器，其残留数据不得从根端点漏出
- 「没有活跃播放器」是常态而非异常：开机后没放歌时即处于该状态

**config_overwritten 说明：**
- 列出被更高优先级来源覆盖的配置键名
- 仅在有覆盖时出现非空数组

**player_support 说明：**
- 系统编译时注册的所有播放器标识名列表

**player_running 说明：**
- 当前正在运行（非 `offline` / `standby` / `waiting_process`）的播放器列表

**player_status 说明：**
- 所有支持的播放器及其当前状态（按注册顺序），值为 status 字符串
- 可能的状态值：`"offline"` / `"waiting_process"` / `"waiting_song"` / `"loading"` / `"playing"` / `"paused"` / `"standby"`

**ws_connected 说明：**
- `connected` - 布尔值，表示是否有客户端连接
- `clients` - 字符串数组，已连接的客户端 IP 地址列表（RemoteAddr 格式）

**endpoints 说明：**
- WebSocket 使用 `ws://`，HTTP/SSE 使用 `http://`

---

### 3. `/all_lyrics` - 完整歌词列表

**正常响应（有歌词时）** — 以下为真实响应原文（cloudmusicv3，2026-07-17 录制）：

```json
{
  "code": 0,
  "msg": "success",
  "player": "cloudmusicv3",
  "data": {
    "title": "Cold - Maroon 5 / Future",
    "duration": 234.308,
    "play_time": 7.07,
    "progress": 0.030527605,
    "count": 88,
    "lyrics": [
      {
        "index": 0,
        "timestamp": 3.6100001,
        "play_time": 3.1100001,
        "text": "Cold enough to chill my bones",
        "sub_text": "冷得足以冻到我的骨头",
        "text_detailed": {
          "timestamp": 4.44,
          "play_time": 3.94,
          "duration": 3.48,
          "words": [
            {"timestamp": 4.44, "play_time": 3.94, "duration": 1.08, "text": "Cold "},
            {"timestamp": 5.52, "play_time": 5.02, "duration": 0.36, "text": "enough "}
          ]
        }
      },
      {
        "index": 1,
        "timestamp": 7.57,
        "play_time": 7.07,
        "text": "It feels like I don't know you anymore",
        "sub_text": "感觉就像我已经不再认识你了",
        "text_detailed": {
          "timestamp": 7.92,
          "play_time": 7.42,
          "duration": 3.48,
          "words": [
            {"timestamp": 7.92, "play_time": 7.42, "duration": 0.06, "text": "It "},
            {"timestamp": 7.98, "play_time": 7.48, "duration": 0.3, "text": "feels "}
          ]
        }
      }
    ],
    "lyrics_detailed": [
      {
        "lyric_index": 0,
        "timestamp": 4.44,
        "play_time": 3.94,
        "duration": 3.48,
        "text": "Cold enough to chill my bones",
        "words": [
          {"timestamp": 4.44, "play_time": 3.94, "duration": 1.08, "text": "Cold "},
          {"timestamp": 5.52, "play_time": 5.02, "duration": 0.36, "text": "enough "}
        ]
      }
    ]
  }
}
```

> **示例已裁剪，只删元素不改值**：`lyrics` 实际 **88** 行（此处留 2）、`lyrics_detailed` 实际 **88** 项（留 1）、每行 `words` 实际 6 个（留 2）。
> 所以 `count: 88` 与上面 `lyrics` 的长度对不上——**那是裁剪造成的，真实响应中两者一致**。数值一律是录制原文，未经修改。

**说明：**
- `duration` - 歌曲总时长（秒）
- `play_time` - **最近一行歌词的播出时间**（秒），不是「现在播到哪」，详见下方
- `progress` - **整首播到哪**（0–1，实时播放位置 / 总时长）
- `count` - 歌词行数（`lyrics` 的真实长度）
- `title` - 歌曲标题（格式：歌曲名 - 歌手）
- `lyrics` - 按 index 排序的歌词数组
- `lyrics[].timestamp` - 该歌词行的起始时间戳（秒）
- `lyrics[].play_time` - 该歌词行应用 offset 后的展示时间（秒）
- `lyrics[].sub_text` - 副歌词文本（翻译，无时为空字符串）——**并非所有平台都有**，见下方能力矩阵
- `lyrics[].text_detailed` - 该行的逐字扩展；无逐字时为空对象 `{}`。**同一首歌里可以逐行不同**（空行、纯人声段落常常没有）
- `lyrics_detailed` - 逐字歌词集合，**仅含有逐字数据的行**；无逐字时为空数组 `[]`
  - `lyric_index` - **对应 `lyrics[].index`**，用它关联回歌词行
  - `text` - 完全由 `words[].text` 拼接而来

#### `play_time` 与 `progress` 是两个不同的量

别把它们当同一个数的两种写法（示例里 `play_time=7.07`、`progress=0.0305`，而 `7.07/234.308 = 0.0302`——**差值不是误差**）：

| | 来源 | 含义 |
|---|---|---|
| `play_time` | 歌词时间轴 | 最近一行歌词**何时该播出**（= 该行 displayStart − offset） |
| `progress` | 实时时钟 | **整首播到哪**（实时位置 / 总时长） |

二者在常规行上只差一个轮询滞后（毫秒级），所以**写反了长期看不出症状**——直到某行的逐字时间轴早于行时间戳时才会突然错开数秒。**画进度条请用 `progress`，做插值锚点请用 `progress × duration`，不要用 `play_time`。**

#### 平台能力矩阵（实测）

| | `sub_text`（翻译） | `text_detailed`（逐字） |
|---|---|---|
| **cloudmusicv3** | 有 | 有（YRC） |
| **qqmusic** | 有 | 有（QRC） |
| **wesing** | 恒 `""` | 有（内存字级）\* |
| **kugou** | 有（KRC）\* | 有（KRC）\* |

**逐字**四家都有（cloudmusicv3 YRC / qqmusic QRC / kugou KRC / wesing 内存字级）；**翻译**三家有（cloudmusicv3 / qqmusic / kugou）、wesing 无。字段一律存在（值可能为空），下游不必按平台分支取值。

\* 逐字均**逐行**判定：kugou 拿不到 KRC、回落行级 LRC 时该行 `text_detailed` 为 `{}`；wesing 的逐字来自进程内存的卡拉OK字级时间，某行字级时间轴不合法（NaN / 越界 / 非单调）时该行退回行级、为 `{}`。kugou 翻译另需 KRC 头部含中文译轨（无译轨时 `sub_text` 为空，逐字不受影响）。

#### 歌词数组前几行可能是元数据

```text
lyrics[0]  {"index": 0, "text": "Taylor Swift - Cruel Summer"}          ← 标题行
lyrics[1]  {"index": 1, "text": "Written by：Annie Clark、Taylor Swift…"} ← 作词行
```

平台返回的歌词文件常把标题/词/曲塞在最前面（QQ 音乐是 `詞：` / `曲：`，酷狗是 `Written by：` / `作词：`）。**但不是每首都有**——实测同一平台有的歌 `lyrics[0]` 直接就是第一句歌词。**下游既不能假设前几行是元数据，也不能假设不是**，这是平台数据的原样透传。

**`text_detailed` 字段结构（有逐字数据时）** — 真实原文（cloudmusicv3 `Cold` 第一行，未裁剪）：
```json
{
  "timestamp": 4.44,
  "play_time": 3.94,
  "duration": 3.48,
  "words": [
    {"timestamp": 4.44, "play_time": 3.94, "duration": 1.08, "text": "Cold "},
    {"timestamp": 5.52, "play_time": 5.02, "duration": 0.36, "text": "enough "},
    {"timestamp": 5.88, "play_time": 5.38, "duration": 0.39, "text": "to "},
    {"timestamp": 6.27, "play_time": 5.77, "duration": 0.27, "text": "chill "},
    {"timestamp": 6.54, "play_time": 6.04, "duration": 0.33, "text": "my "},
    {"timestamp": 6.87, "play_time": 6.37, "duration": 1.05, "text": "bones"}
  ]
}
```

> **注意 `words[].text` 的尾随空格**：英文按词切分，空格已经含在 `text` 里（`"Cold "`、`"enough "`），最后一个词没有。
> 拼接时用 `words.map(w => w.text).join('')`——**用 `join(' ')` 会出双空格**。中文/日文按字切分，无空格（`"薄"`、`"紅"`）。

**`text_detailed` 字段说明：**
- `timestamp` - 逐字行原始起始时间（秒），来自 YRC 源数据
- `play_time` - 逐字行应用 offset 后的展示时间（秒），前端应使用此值做逐字动画驱动
- `duration` - 逐字行总持续时间（秒）
- `words[]` - 按时间顺序的逐字片段数组
- `words[].timestamp` - 该字/词原始起始时间（秒）
- `words[].play_time` - 该字/词应用 offset 后的展示时间（秒），前端高亮判定应使用此值
- `words[].duration` - 该字/词持续时间（秒），用于 reveal 动画的进度计算
- `words[].text` - 该字/词的文本内容（含尾部空格）

**`lyrics_detailed` 数组项结构** — 真实原文（`words` 已裁剪至 2 个，实际 6 个）：
```json
{
  "lyric_index": 0,
  "timestamp": 4.44,
  "play_time": 3.94,
  "duration": 3.48,
  "text": "Cold enough to chill my bones",
  "words": [
    {"timestamp": 4.44, "play_time": 3.94, "duration": 1.08, "text": "Cold "},
    {"timestamp": 5.52, "play_time": 5.02, "duration": 0.36, "text": "enough "}
  ]
}
```

**`lyrics_detailed` 字段说明：**
- `lyric_index` - 对应 `lyrics[]` 中的行索引，用于关联逐行与逐字数据
- `timestamp` / `play_time` / `duration` - 同 `text_detailed`
- `text` - 由 `words[].text` 拼接得到的逐字源文本（可能与 `lyrics[].text` 存在标点差异）
- `words[]` - 同 `text_detailed.words`
- 仅包含有逐字数据的行（无逐字的行不出现在此数组中）

**逐字数据可用性（实测）：**
- 网易云音乐（YRC）、QQ 音乐（QRC）、酷狗音乐（KRC）提供逐字数据。三者的 `text_detailed` 复用同一结构，下游无需按平台分支解析。酷狗在拿不到 KRC、回落到行级 LRC 时无逐字
- 并非所有歌曲都有逐字数据。**同一首歌里也可以逐行不同**——实测一首 51 行的歌里 42 行有逐字，其余 9 行（8 个空行 + 1 行纯人声 `"Woo...Yeah..."`）为 `{}`
- `lyrics_detailed[].text` 与对应的 `lyrics[].text` 可能有标点差异，**不可用于相等比较**

**无歌词时** — 真实原文（qqmusic 纯音乐，`lyric_update` 同时发 `index: -1`）：
```json
{
  "code": 0,
  "msg": "success",
  "player": "qqmusic",
  "data": {
    "title": "Road to You - Ryan Farish",
    "duration": 235,
    "play_time": 0,
    "progress": 0,
    "count": 0,
    "lyrics": [],
    "lyrics_detailed": []
  }
}
```

> ⚠️ **`data` 不是 `{}`**——它是完整对象，只是 `lyrics` 为空数组、`count` 为 `0`。
> `title` / `duration` 照常有值。判断有无歌词请用 `data.count === 0` 或 `data.lyrics.length === 0`，
> **不要用 `!data.lyrics`**（空数组是 truthy）。
>
> 另有一个**中间态**同样长这样：歌曲信息已读到、歌词还在拉的瞬间（`title` 有值、`count: 0`）。
> 二者在这个端点上无法区分，需要配合 `lyric_update` 的 `index`（纯音乐时为 `-1`）。

---

### 4. `/lyric_update` - 当前歌词（最新一条）

**正常响应（播放中）** — 真实原文（cloudmusicv3，`words` 裁剪至 2 个、实际 8 个）：
```json
{
  "code": 0,
  "msg": "success",
  "player": "cloudmusicv3",
  "data": {
    "index": 1,
    "text": "It feels like I don't know you anymore",
    "sub_text": "感觉就像我已经不再认识你了",
    "timestamp": 7.57,
    "play_time": 7.07,
    "progress": 0.030527605,
    "text_detailed": {
      "timestamp": 7.92,
      "play_time": 7.42,
      "duration": 3.48,
      "words": [
        {"timestamp": 7.92, "play_time": 7.42, "duration": 0.06, "text": "It "},
        {"timestamp": 7.98, "play_time": 7.48, "duration": 0.3,  "text": "feels "}
      ]
    }
  }
}
```

**说明：**
- `index` - 歌词行号（`-1` = 平台没有歌词，见下方）
- `text` - 主歌词文本
- `sub_text` - 副歌词文本（翻译，无时为空字符串）。**cloudmusicv3 / qqmusic / kugou 有，wesing 恒为空**（kugou 仅 KRC 含中文译轨时有值）
- `timestamp` - 该行的原始时间戳（秒）
- `play_time` - **本行的播出时间**（秒）= `timestamp − offset`，**恒小于 `timestamp`**
- `progress` - **整首播到哪**（0–1，实时位置 / 总时长）
- `text_detailed` - 该行的逐字扩展；无逐字时为空对象 `{}`

> ⚠️ **`play_time` 与 `progress` 不是一个量**。示例里 `play_time=7.07`、`progress=0.0305`，
> 而 `7.07/234.308 = 0.0302`——**差值不是误差**：前者来自歌词时间轴，后者来自实时时钟。
> 二者在常规行上只差一个轮询滞后（毫秒级），**写反了长期没有症状**，直到某行的逐字时间轴
> 早于行时间戳时才会突然错开数秒。
>
> **画进度条 / 做插值锚点请用 `progress`（× `duration` 反推位置），不要用 `play_time`。**

**无缓存时**（服务刚起、或该播放器还没播过歌）— 真实原文：
```json
{"code":0,"msg":"success","player":"","data":{}}
```

根端点在没有活跃播放器时，`player` 也是空字符串。

**平台没有歌词时**（`index: -1`）— 真实原文（qqmusic 纯音乐，播到 28 秒）：
```json
{
  "index": -1,
  "text": "",
  "sub_text": "",
  "timestamp": 0,
  "play_time": 28.151,
  "progress": 0.12511556,
  "text_detailed": {}
}
```

**`index: -1` 说明：**

- 含义是**平台完全没有返回歌词数据**，不是「这是纯音乐」——两者不等价，见下
- `text` / `sub_text` 为空字符串，`timestamp` 为 `0`
- **`play_time` 与 `progress` 照常跟着歌走**（示例：`28.151` / `0.125` = `28.151/225`）。歌没有歌词，但它还在播
- 与它同时，服务端会发一条 `all_lyrics`，其 `count: 0`、`lyrics: []`
- **判断有无歌词用 `msg.data.index === -1`**，而非 `msg.data.text`

#### 「纯音乐」不一定触发 `index: -1`

各平台对没有歌词的歌，返回的数据不一样：

| 平台 | 平台返回什么 | 我们发什么 |
|---|---|---|
| **qqmusic** | API 返回零行 | `index: -1`，`text: ""` |
| **cloudmusicv3** | 一行「纯音乐，请欣赏」 | `index: -1`，**`text: "纯音乐，请欣赏"`** |
| **kugou** | 一行「纯音乐，请欣赏」（与网易云一字不差） | 同上 |
| **wesing** | —— | **不会出现**：K 歌平台曲库内所有歌都带词 |

服务端已把三家**归一成 `index: -1`**，下游只需认这一个判据。但**平台的提示语原样保留在 `text` 里**——它是平台的数据，不是我们编的。如何处理该文本由下游决定（本项目自带的 `lyric_page.html` 忽略它）。

> **所以 `index === -1` 时 `text` 可能非空。** 别写成 `if (data.text) 显示歌词`——那会把「纯音乐，请欣赏」当歌词渲染。判据永远是 `index`。

---

### 5. `/status_update` - 播放状态
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {
    "status": "playing",
    "detail": "晚风 - 陈婧霏"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - 播放器进程未启动
- `"waiting_song"` - 播放器已启动但未选择歌曲
- `"loading"` - 歌曲加载中，detail 为歌曲名称
- `"playing"` - 播放中，detail 为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail 为歌曲标题
- `"standby"` - **不只是「播放器已退出」**，`detail` 有五种语义，见下

##### ⚠️ `standby` 的 `detail` 有五种

**别把 `standby` 一律当成「播放器已退出」**——那会让你提示主播「请启动网易云」，而他的网易云开着：

| `detail` | 真实含义 |
|---|---|
| `"网易云音乐已退出"` / `"QQ音乐已退出"` / `"K歌客户端已退出"` | 进程真的没了 → 提示启动 |
| `"酷狗音乐 CDP 已断开"` | 进程可能还在，是调试端口断了 → 提示重启 |
| **`"网易云音乐 v2.10.13.6067 不支持（需 v3+）"`** | **进程开着，版本太老** → 提示**升级**，让他去「启动」是错的 |

实测同框（同一次录制，间隔 16 秒）：

```text
standby  '网易云音乐已退出'
standby  '网易云音乐 v2.10.13.6067 不支持（需 v3+）'
standby  '网易云音乐 v2.10.13.6067 不支持（需 v3+）'    ← 30.0 秒后重发
```

版本不支持时该事件**每 30 秒重发一次且不去重**（服务端每 30 秒重探版本）。升级到 v3 后**最长等 30 秒**才恢复取词——不是没重试。

> 区分这五种目前只能**匹配 `detail` 文本**（服务端没有更细的状态码）。`"不支持"` 是版本问题的稳定特征。

**尚未获取到状态时：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {}
}
```

---

### 6. `/song_info` - 歌曲信息

**正常响应（有歌曲信息时）：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {
    "name": "晚风",
    "singer": "陈婧霏",
    "title": "晚风 - 陈婧霏",
    "cover": "http://imgcache.qq.com/music/photo/mid_album_500/a/b/001aBcDe23FgHi.jpg",
    "cover_base64": "data:image/jpeg;base64,/9j/4AAQSkZJRg..."
  }
}
```

**无歌曲信息时：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {}
}
```

**说明：**
- 直接切歌（A→B）时，不会先返回空再返回 B，而是直接返回 B 的信息

#### ⚠️ 每首歌会推**两条** `song_info_update`，第一条没有封面

这是 WS/SSE 侧的行为，但直接决定你怎么用这个端点。实测（四首歌全中）：

```text
ms=103007  红日   cover=有  cover_base64=(空)        ← 第一条
ms=103195  红日   cover=有  cover_base64=232427     ← 第二条，188ms 后
```

**这是设计**：先把文字信息给前端渲染，封面下载完再补一条。**别等第二条再渲染歌名**。

> **而且可能永远没有第二条。** 实测有歌曲的 `cover` 字段本身就是空的（封面 URL 获取失败，命令行会打 `[!] 封面 URL 获取失败`），此时不会发起下载，也就没有第二条：
>
> ```text
> ms=38880   都是夜归人   cover=(空)   cover_base64=(空)   ← 只有这一条
> ```
>
> 写「等到 `cover_base64` 再显示」的前端，在这些歌上会永远白屏。**拿到就用，没有就算**。

（背景：wesing 的内存同步不一致，封面偶尔会拿到上一首或拿不到，那是平台行为。）

---

### Per-player 端点

除 `/health-check` 和 `/service-status` 外，所有端点均有播放器专属路径。**四个播放器都有**：

```
/wesing/all_lyrics          /cloudmusicv3/all_lyrics
/wesing/lyric_update        /cloudmusicv3/lyric_update
/wesing/status_update       /cloudmusicv3/status_update
/wesing/song_info           /cloudmusicv3/song_info
/wesing/lyric_update-SSE    /cloudmusicv3/lyric_update-SSE
/wesing/song_info-SSE       /cloudmusicv3/song_info-SSE
/wesing/ws                  /cloudmusicv3/ws

/qqmusic/all_lyrics         /kugou/all_lyrics
/qqmusic/lyric_update       /kugou/lyric_update
/qqmusic/status_update      /kugou/status_update
/qqmusic/song_info          /kugou/song_info
/qqmusic/lyric_update-SSE   /kugou/lyric_update-SSE
/qqmusic/song_info-SSE      /kugou/song_info-SSE
/qqmusic/ws                 /kugou/ws
```

Per-player 端点始终返回指定播放器的数据，不受路由切换影响。响应格式与根端点相同，`player` 字段固定为对应播放器名。

> 端点全表也可以直接问服务：`GET /service-status` 的 `endpoints` 字段列出全部可用地址（含 per-player 与网易云特效通道），`player_support` 列出所有播放器。**那是运行时的真源，比本文档更新得快。**

---

## SSE 接口（实时推送）

### 7. `/lyric_update-SSE` - 实时歌词推送

**连接建立时：** 始终立即发送一条当前歌词状态（有歌词时发送歌词数据，无歌词时发送 `"data":{}`）。

**初始发送（无歌词时）：**
```
data: {"type":"lyric_update","player":"wesing","data":{}}
```

**初始发送（有歌词时）：**
```
data: {"type":"lyric_update","player":"wesing","data":{"index":5,"text":"更不应舍弃","sub_text":"","timestamp":30.429,"play_time":30.229,"progress":0.10236486,"text_detailed":{}}}
```

**播放过程中，每当歌词更新时接收：**
```
data: {"type":"lyric_update","player":"wesing","data":{"index":3,"text":"手を伸ばしても届かない","sub_text":"","timestamp":3.8,"play_time":3.85,"progress":0.25,"text_detailed":{}}}

data: {"type":"lyric_update","player":"wesing","data":{"index":4,"text":"深い森の奥へ迷い込む","sub_text":"","timestamp":5.5,"play_time":5.6,"progress":0.3333,"text_detailed":{}}}
```

**完整生命周期示例：**
```
（连接建立，当前无歌词）
data: {"type":"lyric_update","player":"wesing","data":{}}

（用户开始播放歌曲）
data: {"type":"lyric_update","player":"wesing","data":{"index":0,"text":"男：摘一颗苹果","sub_text":"","timestamp":18.326,"play_time":18.15,"progress":0.05,"text_detailed":{}}}
data: {"type":"lyric_update","player":"wesing","data":{"index":1,"text":"男：等你从门前经过","sub_text":"","timestamp":20.198,"play_time":20.05,"progress":0.1,"text_detailed":{}}}

（切歌 — 服务端不发送清空消息，前端自行根据新歌数据重置显示）
data: {"type":"lyric_update","player":"wesing","data":{"index":0,"text":"新歌第一行歌词","sub_text":"","timestamp":15.0,"play_time":15.1,"progress":0.04,"text_detailed":{}}}
```

**特性：**
- 响应头 `Content-Type: text/event-stream; charset=utf-8`
- **严格单类型**：本端点只推送 `lyric_update`，**不推送 `all_lyrics`** —— 需要歌词全文请调用 `/all_lyrics` 或改用 WebSocket
- 载荷与 WebSocket 逐字节同构，**含 `type` 字段**（`data: {"type":"lyric_update","player":"...","data":{...}}`）
- 支持跨域（CORS）

**客户端使用示例（JavaScript）：**
```javascript
const eventSource = new EventSource('http://localhost:8765/lyric_update-SSE');

eventSource.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  console.log(`[${msg.player}]`, msg.data.text || '(empty)');
};
```

---

### 8. `/song_info-SSE` - 实时歌曲信息推送

**连接建立时：** 始终立即发送一条当前歌曲信息状态。

**初始发送（无歌曲信息时）：**
```
data: {"type":"song_info_update","player":"wesing","data":{}}
```

**初始发送（有歌曲信息时）：**
```
data: {"type":"song_info_update","player":"wesing","data":{"name":"晚风","singer":"陈婧霏","title":"晚风 - 陈婧霏","cover":"http://imgcache.qq.com/music/photo/mid_album_800/r/k/003JA09X2m9xrk.jpg","cover_base64":"data:image/jpeg;base64,..."}}
```

**播放过程中，歌曲切换时接收：**
```
data: {"type":"song_info_update","player":"wesing","data":{"name":"Winter Night Fantasy","singer":"Azuki Azusa","title":"Winter Night Fantasy - Azuki Azusa","cover":"http://...","cover_base64":"data:image/jpeg;base64,..."}}
```

**歌曲结束/窗口关闭时：**
```
data: {"type":"song_info_update","player":"wesing","data":{}}
```
- 直接切歌（A→B）时，不会先发送空再发送 B，而是直接发送 B 的信息

**特性：**
- 响应以 UTF-8 编码
- 支持跨域（CORS）

**客户端使用示例（JavaScript）：**
```javascript
const eventSource = new EventSource('http://localhost:8765/song_info-SSE');

eventSource.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.data && msg.data.title) {
    console.log(`♪ [${msg.player}] ${msg.data.title}`);
  }
};
```

---

## cURL 使用示例

### HTTP 接口测试

```bash
# 健康检查
curl http://localhost:8765/health-check

# 服务状态
curl http://localhost:8765/service-status

# 根端点（活跃播放器）
curl http://localhost:8765/all_lyrics
curl http://localhost:8765/lyric_update
curl http://localhost:8765/status_update
curl http://localhost:8765/song_info

# Per-player 端点（指定播放器）
curl http://localhost:8765/wesing/all_lyrics
curl http://localhost:8765/cloudmusicv3/lyric_update
```

### SSE 接口测试

```bash
# 实时歌词推送（持续连接）
curl -N http://localhost:8765/lyric_update-SSE

# 指定播放器的歌曲信息推送
curl -N http://localhost:8765/cloudmusicv3/song_info-SSE
```

---

## WebSocket 接口

### `/ws` - WebSocket 连接（根端点，跟随活跃播放器）

> **统一事件格式：** 所有 WebSocket 和 SSE 推送的消息均使用 `{"type": "事件名", "player": "播放器名", "data": 载荷}` 格式。  
> 所有事件无数据时 `data` 均为 `{}`（空对象）。  
> 下游客户端统一按 `msg.type` 分发，`msg.player` 识别来源，`msg.data` 读取载荷即可。

**Per-player WebSocket：** `/wesing/ws`、`/cloudmusicv3/ws` — 始终接收指定播放器事件，不受路由切换影响。

**连接建立时立即接收以下 4 条消息（始终全部发送，无数据时 data 为 {}）：**

#### 1. `status_update` - 状态更新

**有状态时：**
```json
{
  "type": "status_update",
  "player": "wesing",
  "data": {
    "status": "playing",
    "detail": "晚风 - 陈婧霏"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - 播放器进程未启动
- `"waiting_song"` - 播放器已启动但未选择歌曲
- `"loading"` - 歌曲加载中，detail 为歌曲名称
- `"playing"` - 播放中，detail 为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail 为歌曲标题
- `"standby"` - **不只是「播放器已退出」**，`detail` 有五种语义，见下

##### ⚠️ `standby` 的 `detail` 有五种

**别把 `standby` 一律当成「播放器已退出」**——那会让你提示主播「请启动网易云」，而他的网易云开着：

| `detail` | 真实含义 |
|---|---|
| `"网易云音乐已退出"` / `"QQ音乐已退出"` / `"K歌客户端已退出"` | 进程真的没了 → 提示启动 |
| `"酷狗音乐 CDP 已断开"` | 进程可能还在，是调试端口断了 → 提示重启 |
| **`"网易云音乐 v2.10.13.6067 不支持（需 v3+）"`** | **进程开着，版本太老** → 提示**升级**，让他去「启动」是错的 |

实测同框（同一次录制，间隔 16 秒）：

```text
standby  '网易云音乐已退出'
standby  '网易云音乐 v2.10.13.6067 不支持（需 v3+）'
standby  '网易云音乐 v2.10.13.6067 不支持（需 v3+）'    ← 30.0 秒后重发
```

版本不支持时该事件**每 30 秒重发一次且不去重**（服务端每 30 秒重探版本）。升级到 v3 后**最长等 30 秒**才恢复取词——不是没重试。

> 区分这五种目前只能**匹配 `detail` 文本**（服务端没有更细的状态码）。`"不支持"` 是版本问题的稳定特征。

**无状态时（服务刚启动尚未获取到状态）：**
```json
{
  "type": "status_update",
  "player": "wesing",
  "data": {}
}
```

#### 2. `song_info_update` - 歌曲信息更新

**有歌曲信息时：**
```json
{
  "type": "song_info_update",
  "player": "wesing",
  "data": {
    "name": "晚风",
    "singer": "陈婧霏",
    "title": "晚风 - 陈婧霏",
    "cover": "http://imgcache.qq.com/music/photo/mid_album_500/a/b/001aBcDe23FgHi.jpg",
    "cover_base64": "data:image/jpeg;base64,/9j/4AAQSkZJRg..."
  }
}
```

**无歌曲信息时：**
```json
{
  "type": "song_info_update",
  "player": "wesing",
  "data": {}
}
```

**异步封面：** 部分播放器的封面 base64 通过异步 HTTP 下载获取。歌曲开始播放时会先发送一条**不含 `cover_base64`** 的 `song_info_update`（仅含 `cover` URL），待封面下载完成后再补发一条含 `cover_base64` 的完整版本。前端应使用最新收到的数据覆盖即可。

#### 3. `lyric_update` - 实时歌词更新

**播放中（有歌词时）：**
```json
{
  "type": "lyric_update",
  "player": "wesing",
  "data": {
    "index": 5,
    "text": "更不应舍弃",
    "sub_text": "",
    "timestamp": 30.429,
    "play_time": 30.229,
    "progress": 0.10236486,
    "text_detailed": {}
  }
}
```

**无歌词时：**
```json
{
  "type": "lyric_update",
  "player": "wesing",
  "data": {}
}
```

**纯音乐：**
```json
{
  "type": "lyric_update",
  "player": "cloudmusicv3",
  "data": {
    "index": -1,
    "text": "",
    "sub_text": "",
    "timestamp": 0,
    "play_time": 45.2,
    "progress": 0,
    "text_detailed": {}
  }
}
```

> `index: -1` 表示播放器确认当前歌曲为纯音乐（无歌词）。此情况下 `data` 不为 `{}`，客户端用 `msg.data.index === -1` 判断，而非依赖 `msg.data.text`。与此事件同时到达的 `all_lyrics` 中 `count: 0`，`lyrics: []`。

#### 4. `all_lyrics` - 完整歌词列表

**有歌词时：**
```json
{
  "type": "all_lyrics",
  "player": "wesing",
  "data": {
    "title": "都是夜归人 - 许美静",
    "duration": 296,
    "play_time": 0,
    "progress": 0,
    "count": 38,
    "lyrics": [
      {"index": 0, "timestamp": 25.44, "play_time": 25.24, "text": "是冰冻的时分 已过夜深的夜晚", "sub_text": "", "text_detailed": {}},
      {"index": 1, "timestamp": 31.483, "play_time": 31.282999, "text": "往事就像流星刹那划过心房", "sub_text": "", "text_detailed": {}},
      {"index": 2, "timestamp": 37.716, "play_time": 37.516, "text": "灰暗的深夜 是寂寞的世界", "sub_text": "", "text_detailed": {}}
    ],
    "lyrics_detailed": []
  }
}
```

> **示例已裁剪**：`lyrics` 实际 38 行（此处留 3），`count: 38` 是真实值。数值一律取自录制原文。
>
> `play_time: 0` / `progress: 0` 不是占位——**WS 的 `all_lyrics` 只在切歌时发**，那一刻歌刚开始。
> 而 **HTTP 的 `/all_lyrics` 会给实时值**（它读缓存，跟着 `lyric_update` 走）。同一个端点名，两种传输的语义不同。
>
> wesing 的 `sub_text` 恒为 `""`——无翻译源（其曲库 mid 非 QQ 系，走不通 QQ 译源）；但**逐字有**，来自进程内存的卡拉OK字级时间，某行时间轴不合法时该行退回行级 `{}`。kugou 的翻译与逐字都有（KRC 源；回落行级 LRC 时 `sub_text` 为空、`text_detailed` / `lyrics_detailed` 为 `{}` / `[]`）。

**无歌词时：**
```json
{
  "type": "all_lyrics",
  "player": "wesing",
  "data": {}
}
```

#### 5. `lyric_idle` - 歌词空闲通知

当歌曲播放结束、切歌或窗口关闭时发送：
```json
{
  "type": "lyric_idle",
  "player": "wesing",
  "data": {}
}
```

> 注：`lyric_idle` 为纯通知事件，`data` 始终为 `{}`。服务端**不会**发送清空歌词数据的消息，前端可自行决定是否响应（如切歌时，新歌词数据会自然覆盖旧数据）。

#### 6. `playback_pause` - 暂停播放

播放暂停时发送，`data.play_time` 为暂停时刻的播放时间：
```json
{
  "type": "playback_pause",
  "player": "wesing",
  "data": {
    "play_time": 45.2
  }
}
```

#### 7. `playback_resume` - 恢复播放

play_time 重新推进时发送：
```json
{
  "type": "playback_resume",
  "player": "wesing",
  "data": {
    "play_time": 45.2
  }
}
```

> 注：前端收到 `playback_pause` 应停止时间插值，收到 `playback_resume` 应以 `play_time` 为锚点重新开始插值。

#### 8. `player_switch` - 播放器切换（仅根订阅者收到）

当活跃播放器发生变化时，根订阅者（`/ws`）会收到此事件：
```json
{
  "type": "player_switch",
  "player": "cloudmusicv3",
  "data": {
    "from": "wesing",
    "to": "cloudmusicv3"
  }
}
```

**活跃播放器清除时（`to` 为空）：**
```json
{
  "type": "player_switch",
  "player": "",
  "data": {
    "from": "cloudmusicv3",
    "to": ""
  }
}
```

**说明：**
- `player` 字段为切换后的新播放器
- `from` - 切换前的播放器标识名
- `to` - 切换后的播放器标识名；**当所有普通组播放器均无活动时，`to` 为空字符串 `""`**，表示没有活跃播放器
- 当 `to` 为空时，紧随其后会收到一条 `player_clear` 事件（见下方第 9 条）
- Per-player 订阅者（如 `/wesing/ws`）**不会**收到此事件
- 当 `to` 非空时，紧随其后会收到新播放器的**已缓存**状态事件（`status_update` + `song_info_update` + `all_lyrics` + `lyric_update` 中已有的部分）。如果新播放器刚启动、缓存尚未建立（如正处于 loading 阶段），则只会收到已有的事件（可能仅 `status_update`），其余事件在播放器实际上报后才会到达
- 切换后随 FullState 推送过的事件类型不会立即重复推送第二遍；FullState 未包含的类型，其首次数据仍会正常到达

#### 9. `player_clear` - 活跃播放器清除（仅根订阅者收到）

当所有播放器均无活动、活跃播放器被清空时发送。始终在 `player_switch`（`to=""`）之后紧跟发送：
```json
{
  "type": "player_clear",
  "player": "",
  "data": {}
}
```

**说明：**
- `player` 字段为空字符串（无活跃播放器）
- `data` 始终为 `{}`（纯通知事件）
- 前端收到后应清空所有歌词、歌曲信息、进度等显示
- 常见触发场景：优先播放器唱完后释放给普通组，但普通组所有播放器也都处于暂停/空闲状态
- Per-player 订阅者**不会**收到此事件

---

### WS 完整生命周期示例

```
（客户端连接根端点 /ws，当前活跃播放器为 wesing，尚无歌曲）
← {"type":"status_update","player":"wesing","data":{"status":"waiting_song","detail":"等待打开K歌窗口"}}
← {"type":"song_info_update","player":"wesing","data":{}}
← {"type":"lyric_update","player":"wesing","data":{}}
← {"type":"all_lyrics","player":"wesing","data":{}}

（wesing 开始播放歌曲A）
← {"type":"status_update","player":"wesing","data":{"status":"loading","detail":"有点甜"}}
← {"type":"status_update","player":"wesing","data":{"status":"playing","detail":"有点甜 - 汪苏泷/BY2"}}
← {"type":"song_info_update","player":"wesing","data":{"name":"有点甜","singer":"汪苏泷/BY2","title":"有点甜 - 汪苏泷/BY2","cover":"http://...","cover_base64":""}}
← {"type":"all_lyrics","player":"wesing","data":{"title":"有点甜 - 汪苏泷/BY2","duration":236.0,"play_time":0.5,"progress":0.0021,"count":28,"lyrics":[...],"lyrics_detailed":[...]}}
← {"type":"lyric_update","player":"wesing","data":{"index":0,"text":"男：摘一颗苹果","sub_text":"","timestamp":18.326,"play_time":18.15,"progress":0.05,"text_detailed":{}}}
← {"type":"lyric_update","player":"wesing","data":{"index":1,"text":"男：等你从门前经过","sub_text":"","timestamp":20.198,"play_time":20.05,"progress":0.1,"text_detailed":{}}}
...
← {"type":"song_info_update","player":"wesing","data":{"name":"有点甜","singer":"汪苏泷/BY2","title":"有点甜 - 汪苏泷/BY2","cover":"http://...","cover_base64":"data:image/jpeg;base64,..."}}  ← 异步封面下载完成后补发
...

（cloudmusicv3 开始播放，且 wesing 不是优先播放器 —— 触发播放器切换）
← {"type":"player_switch","player":"cloudmusicv3","data":{"from":"wesing","to":"cloudmusicv3"}}
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"playing","detail":"如愿 - 王菲"}}
← {"type":"song_info_update","player":"cloudmusicv3","data":{"name":"如愿","singer":"王菲","title":"如愿 - 王菲","cover":"http://...","cover_base64":"data:image/jpeg;base64,..."}}
← {"type":"all_lyrics","player":"cloudmusicv3","data":{"title":"如愿 - 王菲","duration":280.0,"play_time":0.3,"progress":0.0011,"count":35,"lyrics":[...],"lyrics_detailed":[...]}}
← {"type":"lyric_update","player":"cloudmusicv3","data":{"index":0,"text":"我在时间尽头等你","sub_text":"","timestamp":25.5,"play_time":25.3,"progress":0.03,"text_detailed":{}}}
...

（用户暂停播放）
← {"type":"playback_pause","player":"cloudmusicv3","data":{"play_time":45.2}}

（用户恢复播放）
← {"type":"playback_resume","player":"cloudmusicv3","data":{"play_time":45.2}}
← {"type":"lyric_update","player":"cloudmusicv3","data":{"index":5,"text":"在时间里等你","sub_text":"","timestamp":46.0,"play_time":46.1,"progress":0.16,"text_detailed":{}}}
...

（歌曲播放完毕 —— **仅 wesing 会发 lyric_idle**，网易云/QQ/酷狗不发此事件）
← {"type":"lyric_idle","player":"wesing","data":{}}

（播放器退出 —— detail 逐家不同，不是「播放器已退出」这种泛称）
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"standby","detail":"网易云音乐已退出"}}
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"waiting_process","detail":"网易云音乐未启动"}}

（若装的是 v2 —— 进程开着，但版本不支持。每 30 秒重发一次）
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"standby","detail":"网易云音乐 v2.10.13.6067 不支持（需 v3+）"}}

（所有播放器均无活动 —— 活跃播放器清除。这两条**必定成对、按此顺序**）
← {"type":"player_switch","player":"","data":{"from":"cloudmusicv3","to":""}}
← {"type":"player_clear","player":"","data":{}}
```

> **上面的事件顺序取自真实录音**（05-cloudmusic）。几处容易想当然的地方：
> - **`player_switch` 先于 `status_update`**，不是反过来
> - **切歌时 `song_info_update` 发两次**（第二条带封面），中间夹着 `all_lyrics`
> - **`player_switch(to="")` 与 `player_clear` 必定成对且按此顺序**，中间不会插别的
> - 歌词行的 `lyric_update` 未在此列出（太密），实测一首歌 40~90 条

---

### 客户端使用示例（JavaScript）

```javascript
const ws = new WebSocket('ws://localhost:8765/ws');

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  
  switch (msg.type) {
    case 'status_update':
      if (msg.data && msg.data.status) {
        console.log(`[${msg.player}] 状态: ${msg.data.status} - ${msg.data.detail}`);
      }
      break;
      
    case 'song_info_update':
      if (msg.data && msg.data.title) {
        console.log(`[${msg.player}] ♪ ${msg.data.name} - ${msg.data.singer}`);
      } else {
        console.log(`[${msg.player}] 歌曲信息已清空`);
      }
      break;
      
    case 'all_lyrics':
      if (msg.data && msg.data.lyrics) {
        console.log(`[${msg.player}] 共 ${msg.data.count} 行歌词`);
      }
      break;
      
    case 'lyric_update':
      if (msg.data && msg.data.text) {
        console.log(`[${msg.player}] [${msg.data.index}] ${msg.data.text}`);
      }
      break;
      
    case 'lyric_idle':
      console.log(`[${msg.player}] 歌词空闲（仅通知）`);
      break;
      
    case 'playback_pause':
      console.log(`[${msg.player}] 暂停 @ ${msg.data.play_time}s`);
      break;
      
    case 'playback_resume':
      console.log(`[${msg.player}] 恢复 @ ${msg.data.play_time}s`);
      break;
      
    case 'player_switch':
      if (msg.data.to) {
        console.log(`播放器切换: ${msg.data.from} → ${msg.data.to}`);
      } else {
        console.log(`活跃播放器已清除（原: ${msg.data.from}）`);
      }
      break;
      
    case 'player_clear':
      console.log('无活跃播放器，清空显示');
      break;
  }
};
```

---

## 网易云特效歌词镜像

把网易云「特效歌词」WebGL 画面镜像给前端/OBS。与 JSON 事件通道分离的专用二进制通道。

### `/cloudmusicv3/effect-ws` — 特效画面订阅（WebSocket）

OBS/前端连接此端点，收到两类消息：

| 消息类型 | 内容 | 说明 |
|---|---|---|
| **二进制帧** | JPEG 字节 | 一帧特效画面（~30fps，满则丢最新优先）。`ws.binaryType='arraybuffer'` 后用 `createImageBitmap` 解码 |
| **文本状态** | JSON | `{"type":"status","cmActive":bool,"showing":bool}` |

文本状态字段：

- `cmActive` — 网易云是否为当前活跃输出播放器（`activePlayer == cloudmusicv3`）。
- `showing` — 网易云是否正打开**特效歌词**详情页（详情页可见、未最小化、且特效 canvas 存在；黑胶/标准模式或退出详情页时为 `false`）。

前端据此决定显示/淡出：`cmActive && showing` 才显示实时帧，否则按 `offmode` 处理。

> 连接 query 可携带截帧/注入参数（透传后端）：`quality`、`header_clickable`、`footer_clickable`。`effect_page.html` 会自动按需附加。

### `/cloudmusicv3/effect-ingest` — 帧回传（WebSocket，内部）

注入网易云页面的抓帧脚本把特效 canvas 的 JPEG 字节推到此端点，后端门控后再广播给 `effect-ws` 订阅者。**非用户接口**，仅供注入脚本使用。

### 前端页面 URL 参数（`effect_display.html` / `effect_page.html`）

`effect_display.html` 是防 OBS 缓存的引导页（透传全部参数 + 时间戳）。参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `host` / `port` | `localhost` / `8765` | 后端地址；或用 `ws=` 直接给完整 ws URL |
| `quality` | `95` | JPEG 质量 1–100（纯层为原生分辨率，细线条建议 ≥90 以压色块） |
| `opacity` | `1` | 整块画面不透明度 0–1（背景烤进 canvas，无法单独关背景） |
| `fit` | `cover` | `contain` 留边 / `cover` 裁切填满 / `fill` 拉伸 |
| `offmode` | `fade` | 非活跃输出时：`fade` 淡出 / `hold` 定格 |
| `bg` | （无）透明 | `<hex>`（如 `00ff00`）淡出到纯色（黑白=亮度键、绿=色度键）；`lyrics`=回退到纯净歌词 |
| `lyrics` | — | 当 `bg=lyrics` 时携带的「歌词」模式完整 query（命名空间隔离，由编辑器自动生成） |
| `transition` | `fade` | `fade` / `slide` / `both` |
| `fadein_ms` / `fadeout_ms` | `600` / `600` | 淡入/淡出时长 |
| `fadein_delay_ms` | `1000` | 进详情页后延迟再淡入（等网易云冷渲染） |
| `resume_ms` | =`fadein_ms` | 「冻结→实时」交叉淡入时长（park/帧门控恢复），独立于进场淡入 |
| `header_clickable` | `1` | 双击隐藏顶栏后是否保留点击/拖动 |
| `footer_clickable` | `0` | 双击隐藏底栏后是否保留点击 |

> 最小化策略（`fadeout` / `park`）不是 URL 参数，由服务端 `config.yml` 的 `cloudmusicv3-effect-strategy` 决定，可在 `/service-status` 查看。

### cURL / wscat 测试

```bash
# 订阅特效画面（二进制帧 + 文本状态）
wscat -c "ws://localhost:8765/cloudmusicv3/effect-ws"

# 查看当前策略与端点
curl http://localhost:8765/service-status
```

---

## 空数据判断规则速查

| 事件类型 | 有数据时 `data` | 无数据时 `data` | 客户端判断有无数据 |
|---|---|---|---|
| `status_update` | `{"status":"...","detail":"..."}` | `{}` | `msg.data && msg.data.status` |
| `song_info_update` | `{"name":"...","singer":"...","title":"...","cover":"...","cover_base64":"..."}` | `{}` | `msg.data && msg.data.title` |
| `all_lyrics` | `{"title":"...","duration":N,"play_time":N,"progress":N,"count":N,"lyrics":[...],"lyrics_detailed":[...]}` | `{}` | `msg.data && msg.data.lyrics` |
| `lyric_update` | `{"index":N,"text":"...","sub_text":"...","timestamp":N,...}` | `{}`（无缓存）或 `{"index":-1,"text":"",... }`（纯音乐） | `msg.data && msg.data.index !== undefined && msg.data.index !== -1` |
| `lyric_idle` | — | `{}`（始终） | 收到即为空闲通知（前端自行决定是否响应） |
| `playback_pause` | `{"play_time":N}` | — | 收到即为暂停 |
| `playback_resume` | `{"play_time":N}` | — | 收到即为恢复 |
| `player_switch` | `{"from":"...","to":"..."}` | — | 收到即为切换；`to` 为空时表示清除 |
| `player_clear` | — | `{}`（始终） | 收到即清空显示 |

---

## 前端集成建议

### 歌词清空时机

服务端**不会**主动发送清空歌词的消息。前端应根据以下事件自行决定何时重置显示：

| 场景 | 触发事件 | 建议处理 |
|------|----------|----------|
| 切歌 | 收到新的 `all_lyrics` + `song_info_update` | 用新数据直接覆盖旧数据即可，无需先清空 |
| 播放器退出 | `status_update` → `standby` / `waiting_process` | 清空歌词与歌曲信息 |
| 播放器切换 | `player_switch`（`to` 非空） | 重置显示，等待紧随其后的新播放器初始状态 |
| 所有播放器无活动 | `player_switch`（`to=""`）+ `player_clear` | 清空所有显示（歌词、封面、进度等） |
| 歌曲播放结束 | `lyric_idle` — **仅 wesing 会发** | 可选：清空歌词或保持最后一行显示 |

> **推荐做法：** 用 `status_update` 的 `status` 字段作为主判断依据。当 status 为 `playing` 或 `paused` 时显示歌词，其他状态时清空。
>
> **别把 `lyric_idle` 当主判据**——只有 wesing 发它（见下），另外三家一次都不发，你的清空逻辑会在它们身上完全不触发。

### `lyric_idle` 的定位

**只有 wesing 会发这个事件。** cloudmusicv3 / qqmusic / kugou **一次都不发**（实测 + 代码：`EventLyricIdle` 全仓仅两处，都在 `player/wesing/`）。订阅另外三家的客户端永远等不到它。

`lyric_idle` 是**纯通知事件**（`data` 为 `{}`），表示 wesing 当前歌曲的歌词轮询已结束（歌曲播放完毕、切歌、或 K 歌窗口关闭）。服务端不会随此事件清空任何缓存数据。

前端可以：
- **忽略** — 等后续 `status_update` 或新歌数据自然覆盖（推荐）
- **做 UI 过渡** — 如淡出当前歌词、显示"等待下一首"等
- **清空显示** — 如果你的场景需要在歌曲间隙显示空白

### 时间插值

服务端每次推送 `all_lyrics` 和 `lyric_update` 时携带 `play_time`（实际播放时间，秒）。建议前端实现本地时间插值以获得流畅的进度条/歌词高亮：

```
收到 all_lyrics → 记录 play_time 为初始锚点，立即开始插值（不必等 lyric_update）
收到 lyric_update → 用新 play_time 校正锚点（消除累积误差）
每帧更新 → 当前播放时间 = play_time + (now - 收到时间)
收到 playback_pause → 停止插值，冻结显示
收到 playback_resume → 以新 play_time 为锚点重新开始插值
```

> **为什么用 `all_lyrics` 的 `play_time` 起步？** 部分歌曲从开始播放到第一条 `lyric_update` 可能有较长的前奏间隔（如 15-30 秒）。`all_lyrics` 在歌曲加载完成后立即推送，其 `play_time` 可作为插值的首个锚点，让进度条在前奏阶段就开始推进。

### 切歌与 Replay

**切歌**时服务端会依次发送：
1. `status_update`（`loading` → `playing`）
2. `song_info_update`（新歌曲元信息）
3. `all_lyrics`（新歌词列表）
4. `lyric_update`（新歌第一行）


**Replay（重播同一首歌）** 时行为因播放器而异：

| 播放器 | Replay 行为 | 前端收到的事件 |
|--------|-------------|----------------|
| wesing | 歌曲不中断，`play_time` 回跳到开头 | `playback_resume`（新 `play_time`）→ `lyric_update`（从第一行开始） |
| cloudmusicv3 | 无 replay 操作，仅支持进度条跳转 | `playback_resume`（跳转后的 `play_time`） |

> wesing 的 replay 和 cloudmusicv3 的进度条跳转均复用 `playback_resume` 事件。前端收到 `playback_resume` 后应以其 `play_time` 为锚点重置插值，下一条 `lyric_update` 会自然匹配到正确的歌词行。

### 断线重连

WebSocket 断线后重新连接时，服务端会立即发送已缓存的初始状态消息（最多 4 条：`status_update` + `song_info_update` + `all_lyrics` + `lyric_update`，仅包含已有缓存的类型）。前端只需据此重建完整状态，无需额外的恢复逻辑。

建议重连策略：
```
断线 → 等待 1s → 重连 → 成功则重置状态
                 → 失败 → 等待 2s → 重连 → 失败 → 等待 4s → ...（指数退避，上限 30s）
```

### Per-player 与根端点的选择

| 场景 | 推荐端点 | 理由 |
|------|----------|------|
| OBS 直播画面 | 根端点 `/ws` | 自动跟随活跃播放器，单个连接即可覆盖全部播放器 |
| 调试特定播放器 | Per-player `/<player>/ws` | 不受路由切换干扰 |
| 多播放器同时展示 | 分别连接各 Per-player | 各自独立，互不影响 |

---

## 错误响应格式

所有 HTTP 接口在出错时返回：
```json
{
  "code": -1,
  "msg": "error message",
  "player": "",
  "data": {}
}
```

当前实现中，HTTP 接口总是返回 code 为 0 和对应的数据。
