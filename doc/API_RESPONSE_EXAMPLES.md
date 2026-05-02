# Metabox-Nexus-PlayerCap API 响应示例

> **多播放器架构：** 所有 HTTP 响应和 WS/SSE 事件均包含 `player` 字段，标识数据来源的播放器（如 `"wesing"`、`"cloudmusicv3"`、`"qqmusic"`）。  
> **空数据约定：** 所有事件在无数据时统一返回 `"data": {}`（空对象），而非 `null`。  
> **Per-player 端点：** 除 `/health-check` 和 `/service-status` 外，所有端点均有播放器专属路径版本（如 `/wesing/ws`、`/cloudmusicv3/all_lyrics`、`/qqmusic/ws`）。根端点跟随活跃播放器，Per-player 端点始终返回指定播放器数据。

---

## HTTP 接口（静态数据）

### 1. `/health-check` - 健康检查
```json
{
  "code": 0,
  "msg": "success",
  "player": "internal",
  "data": {
    "now_time": "2026-03-19T12:34:56+08:00"
  }
}
```

---

### 2. `/service-status` - 服务状态信息
```json
{
  "code": 0,
  "msg": "success",
  "player": "internal",
  "data": {
    "version": "3.0.0-beta.1",
    "addr": "0.0.0.0:8765",
    "now_time": "2026-03-19T12:34:56+08:00",
    "config_sources": ["config.yml", "命令行参数"],
    "config": {
      "addr": "0.0.0.0:8765",
      "offset": 200,
      "poll": 30,
      "prior-player": [
        "wesing"
      ],
      "prior-player-expire": 15,
      "wesing-offset": 200,
      "wesing-poll": 30,
      "cloudmusicv3-offset": 500,
      "cloudmusicv3-poll": 30,
      "qqmusic-offset": 200,
      "qqmusic-poll": 50
    },
    "config_overwritten": ["offset", "wesing-poll"],
    "player_support": ["wesing", "cloudmusicv3", "qqmusic"],
    "player_running": ["wesing"],
    "player_status": {
      "wesing": "playing",
      "cloudmusicv3": "waiting_process",
      "qqmusic": "waiting_process"
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
        "song_info-SSE": "http://0.0.0.0:8765/cloudmusicv3/song_info-SSE"
      },
      "qqmusic": {
        "ws": "ws://0.0.0.0:8765/qqmusic/ws",
        "all_lyrics": "http://0.0.0.0:8765/qqmusic/all_lyrics",
        "lyric_update": "http://0.0.0.0:8765/qqmusic/lyric_update",
        "status_update": "http://0.0.0.0:8765/qqmusic/status_update",
        "song_info": "http://0.0.0.0:8765/qqmusic/song_info",
        "lyric_update-SSE": "http://0.0.0.0:8765/qqmusic/lyric_update-SSE",
        "song_info-SSE": "http://0.0.0.0:8765/qqmusic/song_info-SSE"
      }
    },
    "client_count": 2,
    "ws_connected": {
      "connected": true,
      "clients": [
        "192.168.1.100:54321",
        "192.168.1.200:54322"
      ]
    }
  }
}
```

**version 说明：**
- 编译时通过 `-ldflags "-X main.Version=3.0.0-beta.1"` 注入
- 默认值为 `0.0.0`
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
- 返回所有可用接口的完整地址（含 Per-player 端点）
- WebSocket 使用 `ws://`，HTTP/SSE 使用 `http://`

---

### 3. `/all_lyrics` - 完整歌词列表

**正常响应（有歌词时）：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {
    "song_title": "告白 - 花澤香菜",
    "duration": 236.0,
    "play_time": 1.2,
    "count": 12,
    "lyrics": [
      {"index": 0, "time": 0.5, "text": "いつもそばにいるのに", "sub_text": ""},
      {"index": 1, "time": 2.1, "text": "ふと気付くと遠すぎて", "sub_text": ""},
      {"index": 2, "time": 3.8, "text": "手を伸ばしても届かない", "sub_text": ""},
      {"index": 3, "time": 5.5, "text": "深い森の奥へ迷い込む", "sub_text": ""},
      {"index": 4, "time": 7.2, "text": "君に逢いたい", "sub_text": ""},
      {"index": 5, "time": 9.0, "text": "君に嘘をついていた", "sub_text": ""},
      {"index": 6, "time": 11.2, "text": "心は静かに落ち着かず", "sub_text": ""},
      {"index": 7, "time": 13.5, "text": "何もかもが手から零れ落ちる", "sub_text": ""},
      {"index": 8, "time": 15.8, "text": "ずっと歩いてくよ", "sub_text": ""},
      {"index": 9, "time": 18.2, "text": "迷えるまま", "sub_text": ""},
      {"index": 10, "time": 20.5, "text": "君を探す", "sub_text": ""},
      {"index": 11, "time": 22.8, "text": "その先へ", "sub_text": ""}
    ]
  }
}
```

**说明：**
- `player` - 数据来源播放器（根端点时为当前活跃播放器，Per-player 端点时为指定播放器）
- `duration` - 歌曲总时长（秒）
- `play_time` - 发送时的当前播放时间（秒），用于前端插值计时的初始锚点
- `count` - 歌词行数
- `lyrics` - 按 index 排序的歌词数组
- `lyrics[].sub_text` - 副歌词文本（翻译/音译等，无时为空字符串）

**无歌词时：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {}
}
```

---

### 4. `/lyric_update` - 当前歌词（最新一条）

**正常响应（播放中）：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {
    "line_index": 5,
    "text": "君に嘘をついていた",
    "sub_text": "",
    "timestamp": 9.0,
    "play_time": 9.15,
    "progress": 0.4167
  }
}
```

**说明：**
- `line_index` - 歌词行号
- `text` - 主歌词文本
- `sub_text` - 副歌词文本（翻译/音译等，无时为空字符串）
- `timestamp` - 歌词时间戳（秒）
- `play_time` - 实际播放时间（秒）；根据偏移量调整后的时间
- `progress` - 播放进度（0-1）

**无歌词时：**
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {}
}
```

---

### 5. `/status_update` - 播放状态
```json
{
  "code": 0,
  "msg": "success",
  "player": "wesing",
  "data": {
    "status": "playing",
    "detail": "告白 - 花澤香菜"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - 播放器进程未启动
- `"waiting_song"` - 播放器已启动但未选择歌曲
- `"loading"` - 歌曲加载中，detail 为歌曲名称
- `"playing"` - 播放中，detail 为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail 为歌曲标题
- `"standby"` - 待机状态，播放器已退出

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
    "name": "告白",
    "singer": "花澤香菜",
    "title": "告白 - 花澤香菜",
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

---

### Per-player 端点

除 `/health-check` 和 `/service-status` 外，所有端点均有播放器专属路径：

```
/wesing/all_lyrics        /cloudmusicv3/all_lyrics
/wesing/lyric_update      /cloudmusicv3/lyric_update
/wesing/status_update     /cloudmusicv3/status_update
/wesing/song_info         /cloudmusicv3/song_info
/wesing/lyric_update-SSE  /cloudmusicv3/lyric_update-SSE
/wesing/song_info-SSE     /cloudmusicv3/song_info-SSE
```

Per-player 端点始终返回指定播放器的数据，不受路由切换影响。响应格式与根端点相同，`player` 字段固定为对应播放器名。

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
data: {"type":"lyric_update","player":"wesing","data":{"line_index":5,"text":"君に嘘をついていた","sub_text":"","timestamp":9.0,"play_time":9.15,"progress":0.5}}
```

**播放过程中，每当歌词更新时接收：**
```
data: {"type":"lyric_update","player":"wesing","data":{"line_index":3,"text":"手を伸ばしても届かない","sub_text":"","timestamp":3.8,"play_time":3.85,"progress":0.25}}

data: {"type":"lyric_update","player":"wesing","data":{"line_index":4,"text":"深い森の奥へ迷い込む","sub_text":"","timestamp":5.5,"play_time":5.6,"progress":0.3333}}
```

**完整生命周期示例：**
```
（连接建立，当前无歌词）
data: {"type":"lyric_update","player":"wesing","data":{}}

（用户开始播放歌曲）
data: {"type":"lyric_update","player":"wesing","data":{"line_index":0,"text":"男：摘一颗苹果","sub_text":"","timestamp":18.326,"play_time":18.15,"progress":0.05}}
data: {"type":"lyric_update","player":"wesing","data":{"line_index":1,"text":"男：等你从门前经过","sub_text":"","timestamp":20.198,"play_time":20.05,"progress":0.1}}

（切歌 — 服务端不发送清空消息，前端自行根据新歌数据重置显示）
data: {"type":"lyric_update","player":"wesing","data":{"line_index":0,"text":"新歌第一行歌词","sub_text":"","timestamp":15.0,"play_time":15.1,"progress":0.04}}
```

**特性：**
- ✅ 完全支持 UTF-8 编码（中文、日文、韩文、俄文等所有语言）
- ✅ 服务器设置了 `Content-Type: text/event-stream; charset=utf-8`
- ✅ 连接时始终立即发送当前状态
- 实时推送，延迟极低
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
data: {"type":"song_info_update","player":"wesing","data":{"name":"告白","singer":"花澤香菜","title":"告白 - 花澤香菜","cover":"http://imgcache.qq.com/music/photo/mid_album_500/a/b/001aBcDe23FgHi.jpg","cover_base64":"data:image/jpeg;base64,..."}}
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
- ✅ 完全支持 UTF-8 编码
- ✅ 连接时始终立即发送当前状态
- 实时推送，延迟极低
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
    "detail": "告白 - 花澤香菜"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - 播放器进程未启动
- `"waiting_song"` - 播放器已启动但未选择歌曲
- `"loading"` - 歌曲加载中，detail 为歌曲名称
- `"playing"` - 播放中，detail 为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail 为歌曲标题
- `"standby"` - 待机状态，播放器已退出

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
    "name": "告白",
    "singer": "花澤香菜",
    "title": "告白 - 花澤香菜",
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
    "line_index": 5,
    "text": "君に嘘をついていた",
    "sub_text": "",
    "timestamp": 9.0,
    "play_time": 9.15,
    "progress": 0.4167
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

#### 4. `all_lyrics` - 完整歌词列表

**有歌词时：**
```json
{
  "type": "all_lyrics",
  "player": "wesing",
  "data": {
    "song_title": "告白 - 花澤香菜",
    "duration": 236.0,
    "play_time": 1.2,
    "count": 12,
    "lyrics": [
      {"index": 0, "time": 0.5, "text": "いつもそばにいるのに", "sub_text": ""},
      {"index": 1, "time": 2.1, "text": "ふと気付くと遠すぎて", "sub_text": ""},
      {"index": 2, "time": 3.8, "text": "手を伸ばしても届かない", "sub_text": ""}
    ]
  }
}
```

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

当 play_time 连续多次不变时检测为暂停：
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
- 服务端会自动抑制 FullState 已推送的事件类型，避免后续实时事件重复到达。仅抑制 FullState 实际包含的类型，不会吞掉缓存中不存在的首次数据

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
← {"type":"all_lyrics","player":"wesing","data":{"song_title":"有点甜 - 汪苏泷/BY2","duration":236.0,"play_time":0.5,"count":28,"lyrics":[...]}}
← {"type":"lyric_update","player":"wesing","data":{"line_index":0,"text":"男：摘一颗苹果","sub_text":"","timestamp":18.326,"play_time":18.15,"progress":0.05}}
← {"type":"lyric_update","player":"wesing","data":{"line_index":1,"text":"男：等你从门前经过","sub_text":"","timestamp":20.198,"play_time":20.05,"progress":0.1}}
...
← {"type":"song_info_update","player":"wesing","data":{"name":"有点甜","singer":"汪苏泷/BY2","title":"有点甜 - 汪苏泷/BY2","cover":"http://...","cover_base64":"data:image/jpeg;base64,..."}}  ← 异步封面下载完成后补发
...

（cloudmusicv3 开始播放，且 wesing 不是优先播放器 —— 触发播放器切换）
← {"type":"player_switch","player":"cloudmusicv3","data":{"from":"wesing","to":"cloudmusicv3"}}
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"playing","detail":"如愿 - 王菲"}}
← {"type":"song_info_update","player":"cloudmusicv3","data":{"name":"如愿","singer":"王菲","title":"如愿 - 王菲","cover":"http://...","cover_base64":"data:image/jpeg;base64,..."}}
← {"type":"all_lyrics","player":"cloudmusicv3","data":{"song_title":"如愿 - 王菲","duration":280.0,"play_time":0.3,"count":35,"lyrics":[...]}}
← {"type":"lyric_update","player":"cloudmusicv3","data":{"line_index":0,"text":"我在时间尽头等你","sub_text":"","timestamp":25.5,"play_time":25.3,"progress":0.03}}
...

（用户暂停播放）
← {"type":"playback_pause","player":"cloudmusicv3","data":{"play_time":45.2}}

（用户恢复播放）
← {"type":"playback_resume","player":"cloudmusicv3","data":{"play_time":45.2}}
← {"type":"lyric_update","player":"cloudmusicv3","data":{"line_index":5,"text":"在时间里等你","sub_text":"","timestamp":46.0,"play_time":46.1,"progress":0.16}}
...

（歌曲播放完毕）
← {"type":"lyric_idle","player":"cloudmusicv3","data":{}}

（播放器退出）
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"standby","detail":"播放器已退出"}}
← {"type":"status_update","player":"cloudmusicv3","data":{"status":"waiting_process","detail":"播放器未启动"}}

（所有播放器均无活动 —— 活跃播放器清除）
← {"type":"player_switch","player":"","data":{"from":"cloudmusicv3","to":""}}
← {"type":"player_clear","player":"","data":{}}
```

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
        console.log(`[${msg.player}] [${msg.data.line_index}] ${msg.data.text}`);
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

## 空数据判断规则速查

| 事件类型 | 有数据时 `data` | 无数据时 `data` | 客户端判断有无数据 |
|---|---|---|---|
| `status_update` | `{"status":"...","detail":"..."}` | `{}` | `msg.data && msg.data.status` |
| `song_info_update` | `{"name":"...","singer":"...","title":"...","cover":"...","cover_base64":"..."}` | `{}` | `msg.data && msg.data.title` |
| `all_lyrics` | `{"song_title":"...","duration":N,"play_time":N,"count":N,"lyrics":[...]}` | `{}` | `msg.data && msg.data.lyrics` |
| `lyric_update` | `{"line_index":N,"text":"...","sub_text":"...","timestamp":N,...}` | `{}` | `msg.data && msg.data.text` |
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
| 歌曲播放结束 | `lyric_idle` | 可选：清空歌词或保持最后一行显示 |

> **推荐做法：** 用 `status_update` 的 `status` 字段作为主判断依据。当 status 为 `playing` 或 `paused` 时显示歌词，其他状态时清空。

### `lyric_idle` 的定位

`lyric_idle` 是**纯通知事件**（`data` 始终为 `{}`），表示当前歌曲的歌词轮询已结束（可能是歌曲播放完毕、切歌、或窗口关闭）。服务端不会随此事件清空任何缓存数据。

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

前端只需监听这些事件并用新数据覆盖即可，**不需要等待或处理任何清空消息**。

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
| OBS 直播画面 | 根端点 `/ws` | 自动跟随活跃播放器，一个源搞定 |
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
