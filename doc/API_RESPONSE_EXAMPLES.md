# Metabox-Nexus-WesingCap API 响应示例

> **空数据约定：** 所有事件在无数据时统一返回 `"data": {}`（空对象），而非 `null`。

---

## HTTP 接口（静态数据）

### 1. `/health-check` - 健康检查
```json
{
  "code": 0,
  "msg": "success",
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
  "data": {
    "version": "2.1.0",
    "addr": "0.0.0.0:8765",
    "config_sources": ["config.yml", "命令行参数"],
    "config": {
      "addr": "0.0.0.0:8765",
      "offset": 100,
      "poll": 50
    },
    "endpoints": {
      "ws": "ws://0.0.0.0:8765/ws",
      "health-check": "http://0.0.0.0:8765/health-check",
      "service-status": "http://0.0.0.0:8765/service-status",
      "all_lyrics": "http://0.0.0.0:8765/all_lyrics",
      "lyric_update": "http://0.0.0.0:8765/lyric_update",
      "status_update": "http://0.0.0.0:8765/status_update",
      "song_info": "http://0.0.0.0:8765/song_info",
      "lyric_update-SSE": "http://0.0.0.0:8765/lyric_update-SSE",
      "song_info-SSE": "http://0.0.0.0:8765/song_info-SSE"
    },
    "status": "playing",
    "now_time": "2026-03-19T12:34:56+08:00",
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
- 编译时通过 `-ldflags "-X main.Version=2.1.0"` 注入
- 默认值为 `0.0.0`
- 示例: `go build -ldflags "-X main.Version=2.1.0"`

**config_sources 说明：**
- 显示配置来源的完整链路
- 按优先级顺序排列，后面的会覆盖前面的
- 可能的值：`"内置默认"`、`"config.yml"`、`"命令行参数"`
- 示例：
  - `["内置默认"]` - 使用所有默认值
  - `["config.yml"]` - 所有值来自config.yml
  - `["config.yml", "命令行参数"]` - 从config.yml加载，部分被命令行参数覆盖

**ws_connected 说明：**
- `connected` - 布尔值，表示是否有客户端连接
- `clients` - 字符串数组，已连接的客户端IP地址列表（RemoteAddr格式）
- 当无客户端连接时，`clients` 为空数组

**endpoints 说明：**
- 返回所有可用接口的完整地址（带协议）
- WebSocket接口使用 `ws://` 或 `wss://` 协议
- HTTP接口使用 `http://` 或 `https://` 协议
- SSE接口使用 `http://` 或 `https://` 协议

**status 可能的值及含义：**
- `"offline"` - 服务离线（未连接到K歌进程）
- `"waiting_process"` - K歌客户端未启动，等待用户运行 WeSing.exe
- `"loading"` - 歌曲加载中
- `"playing"` - 播放中
- `"paused"` - 暂停中（play_time 停止推进时自动检测）
- `"waiting_song"` - K歌窗口未打开/焦点丢失，等待用户打开或点击K歌窗口
- `"standby"` - 待机状态，K歌客户端已退出

---

### 3. `/all_lyrics` - 完整歌词列表

**正常响应（有歌词时）：**
```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "song_title": "告白 - 花澤香菜",
    "duration": 236.0,
    "play_time": 1.2,
    "count": 12,
    "lyrics": [
      {
        "index": 0,
        "time": 0.5,
        "text": "いつもそばにいるのに"
      },
      {
        "index": 1,
        "time": 2.1,
        "text": "ふと気付くと遠すぎて"
      },
      {
        "index": 2,
        "time": 3.8,
        "text": "手を伸ばしても届かない"
      },
      {
        "index": 3,
        "time": 5.5,
        "text": "深い森の奥へ迷い込む"
      },
      {
        "index": 4,
        "time": 7.2,
        "text": "君に逢いたい"
      },
      {
        "index": 5,
        "time": 9.0,
        "text": "君に嘘をついていた"
      },
      {
        "index": 6,
        "time": 11.2,
        "text": "心は静かに落ち着かず"
      },
      {
        "index": 7,
        "time": 13.5,
        "text": "何もかもが手から零れ落ちる"
      },
      {
        "index": 8,
        "time": 15.8,
        "text": "ずっと歩いてくよ"
      },
      {
        "index": 9,
        "time": 18.2,
        "text": "迷えるまま"
      },
      {
        "index": 10,
        "time": 20.5,
        "text": "君を探す"
      },
      {
        "index": 11,
        "time": 22.8,
        "text": "その先へ"
      }
    ]
  }
}
```

**说明：**
- `duration` - 歌曲总时长（秒），从 UI 字符串 "mm:ss | mm:ss" 解析，fallback 为最后一行歌词时间+10s
- `play_time` - 发送时的当前播放时间（秒），用于前端插值计时的初始锚点
- `count` - 歌词行数
- `lyrics` - 按index排序的歌词数组

**无歌词时（K歌窗口已关闭或暂无歌词）：**
```json
{
  "code": 0,
  "msg": "success",
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
  "data": {
    "line_index": 5,
    "text": "君に嘘をついていた",
    "timestamp": 9.0,
    "play_time": 9.15,
    "progress": 0.4167
  }
}
```

**说明：**
- `line_index` - 歌词行号
- `timestamp` - 歌词时间戳（秒）
- `play_time` - 实际播放时间（秒）；根据偏移量调整后的时间
- `progress` - 播放进度（0-1）

**无歌词时（歌曲未开始播放 / K歌窗口已关闭）：**
```json
{
  "code": 0,
  "msg": "success",
  "data": {}
}
```

---

### 5. `/status_update` - 播放状态
```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "status": "playing",
    "detail": "告白 - 花澤香菜"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - K歌客户端未启动，等待用户运行 WeSing.exe
- `"loading"` - 歌曲加载中，detail 为歌曲名称
- `"playing"` - 播放中，detail 为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail 为歌曲标题
- `"waiting_song"` - K歌窗口未打开/焦点丢失，等待用户打开或点击K歌窗口
- `"standby"` - 待机状态，K歌客户端已退出

**尚未获取到状态时：**
```json
{
  "code": 0,
  "msg": "success",
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
  "data": {
    "name": "告白",
    "singer": "花澤香菜",
    "title": "告白 - 花澤香菜"
  }
}
```

**无歌曲信息时（K歌窗口已关闭 / 未播放）：**
```json
{
  "code": 0,
  "msg": "success",
  "data": {}
}
```

**说明：**
- K歌窗口关闭时，服务会自动清空歌曲信息缓存并返回 `"data": {}`
- 直接切歌（A→B）时，不会先返回空再返回B，而是直接返回B的信息

---

## SSE 接口（实时推送）

### 7. `/lyric_update-SSE` - 实时歌词推送

**连接建立时：** 始终立即发送一条当前歌词状态（有歌词时发送歌词数据，无歌词时发送 `"data":{}`）。

**初始发送（无歌词时）：**
```
data: {"type":"lyric_update","data":{}}
```

**初始发送（有歌词时）：**
```
data: {"type":"lyric_update","data":{"line_index":5,"text":"君に嘘をついていた","timestamp":9.0,"play_time":9.15,"progress":0.5}}
```

**播放过程中，每当歌词更新时接收：**
```
data: {"type":"lyric_update","data":{"line_index":3,"text":"手を伸ばしても届かない","timestamp":3.8,"play_time":3.85,"progress":0.25}}

data: {"type":"lyric_update","data":{"line_index":4,"text":"深い森の奥へ迷い込む","timestamp":5.5,"play_time":5.6,"progress":0.3333}}

data: {"type":"lyric_update","data":{"line_index":5,"text":"君に逢いたい","timestamp":7.2,"play_time":7.3,"progress":0.4167}}

data: {"type":"lyric_update","data":{"line_index":6,"text":"君に嘘をついていた","timestamp":9.0,"play_time":9.15,"progress":0.5}}
```

**歌曲停止或K歌窗口关闭时（只发送一次）：**
```
data: {"type":"lyric_update","data":{}}
```
- K歌窗口关闭时，主动推送此消息清空歌词状态
- 只发送一次（状态变化时），之后不会重复循环发送
- 客户端接收到 `data: {}` 时应清空显示的歌词内容

**完整生命周期示例：**
```
（连接建立，当前无歌词）
data: {"type":"lyric_update","data":{}}

（用户开始播放歌曲）
data: {"type":"lyric_update","data":{"line_index":0,"text":"男：摘一颗苹果","timestamp":18.326,"play_time":18.15,"progress":0.05}}
data: {"type":"lyric_update","data":{"line_index":1,"text":"男：等你从门前经过","timestamp":20.198,"play_time":20.05,"progress":0.1}}
data: {"type":"lyric_update","data":{"line_index":2,"text":"男：送到你的手中帮你解渴","timestamp":23.162,"play_time":23.0,"progress":0.15}}

（用户关闭K歌窗口）
data: {"type":"lyric_update","data":{}}
```

**特性：**
- ✅ 完全支持 UTF-8 编码（中文、日文、韩文、俄文等所有语言）
- ✅ 服务器设置了 `Content-Type: text/event-stream; charset=utf-8`
- ✅ 连接时始终立即发送当前状态（不会因无数据而不发送）
- ✅ 支持空对象消息用于清空歌词状态（仅在状态改变时发送）
- 实时推送，延迟极低
- 支持跨域（CORS）

**客户端使用示例（JavaScript）：**
```javascript
const eventSource = new EventSource('http://localhost:8765/lyric_update-SSE');

eventSource.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  
  if (!msg.data || !msg.data.text) {
    console.log('歌词已清空');
    // 清空 UI 中的歌词显示
    return;
  }
  
  console.log(`[${msg.data.line_index}] ${msg.data.text} (${msg.data.play_time.toFixed(2)}s)`);
};

eventSource.onerror = (error) => {
  console.error('SSE 连接错误:', error);
  eventSource.close();
};
```

---

### 8. `/song_info-SSE` - 实时歌曲信息推送

**连接建立时：** 始终立即发送一条当前歌曲信息状态（有信息时发送数据，无信息时发送 `"data":{}`）。

**初始发送（无歌曲信息时）：**
```
data: {"type":"song_info_update","data":{}}
```

**初始发送（有歌曲信息时）：**
```
data: {"type":"song_info_update","data":{"name":"告白","singer":"花澤香菜","title":"告白 - 花澤香菜"}}
```

**播放过程中，每当歌曲切换、信息更新（不关K歌窗口直接 A->B）时接收：**
```
data: {"type":"song_info_update","data":{"name":"告白","singer":"花澤香菜","title":"告白 - 花澤香菜"}}

data: {"type":"song_info_update","data":{"name":"Winter Night Fantasy","singer":"Azuki Azusa","title":"Winter Night Fantasy - Azuki Azusa"}}
```

**K歌窗口关闭时（只发送一次）：**
```
data: {"type":"song_info_update","data":{}}
```
- K歌窗口关闭时，主动推送此消息清空歌曲信息
- 直接切歌（A→B）时，不会先发送空再发送B，而是直接发送B的信息

**完整生命周期示例：**
```
（连接建立，当前无歌曲）
data: {"type":"song_info_update","data":{}}

（用户打开歌曲A）
data: {"type":"song_info_update","data":{"name":"有点甜","singer":"汪苏泷/BY2","title":"有点甜 - 汪苏泷/BY2"}}

（用户关闭K歌窗口）
data: {"type":"song_info_update","data":{}}

（用户打开歌曲B）
data: {"type":"song_info_update","data":{"name":"如愿","singer":"王菲","title":"如愿 - 王菲"}}

（用户没有关闭K歌窗口，打开歌曲C）
data: {"type":"song_info_update","data":{"name":"留什么给你","singer":"孙楠","title":"留什么给你 - 孙楠"}}

```

**特性：**
- ✅ 完全支持 UTF-8 编码（中文、日文、韩文、俄文等所有语言）
- ✅ 服务器设置了 `Content-Type: text/event-stream; charset=utf-8`
- ✅ 连接时始终立即发送当前状态（不会因无数据而不发送）
- ✅ 支持空对象消息用于清空歌曲信息（仅在K歌窗口关闭时发送）
- 实时推送，延迟极低
- 支持跨域（CORS）

**客户端使用示例（JavaScript）：**
```javascript
const eventSource = new EventSource('http://localhost:8765/song_info-SSE');

eventSource.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  
  if (!msg.data || !msg.data.title) {
    console.log('歌曲信息已清空');
    // 清空 UI 中的歌曲信息显示
    return;
  }
  
  console.log(`♪ ${msg.data.name} - ${msg.data.singer}`);
};

eventSource.onerror = (error) => {
  console.error('SSE 连接错误:', error);
  eventSource.close();
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

# 完整歌词
curl http://localhost:8765/all_lyrics

# 当前歌词
curl http://localhost:8765/lyric_update

# 播放状态
curl http://localhost:8765/status_update

# 歌曲信息
curl http://localhost:8765/song_info
```

### SSE 接口测试

```bash
# 实时歌词推送（持续连接）
curl -N http://localhost:8765/lyric_update-SSE

# 实时歌曲信息推送（持续连接）
curl -N http://localhost:8765/song_info-SSE
```

---

## WebSocket 接口

### `/ws` - WebSocket 连接

> **统一事件格式：** 所有 WebSocket 和 SSE 推送的消息均使用 `{"type": "事件名", "data": 载荷}` 格式。  
> 所有事件无数据时 `data` 均为 `{}`（空对象）。  
> 下游客户端统一按 `msg.type` 分发，`msg.data` 读取载荷即可。

**连接建立时立即接收以下 4 条消息（始终全部发送，无数据时 data 为 {}）：**

#### 1. `status_update` - 状态更新

**有状态时：**
```json
{
  "type": "status_update",
  "data": {
    "status": "playing",
    "detail": "告白 - 花澤香菜"
  }
}
```

**status 可能的值及含义：**
- `"waiting_process"` - K歌客户端未启动，等待用户运行 WeSing.exe
- `"loading"` - 歌曲加载中，detail为歌曲名称
- `"playing"` - 播放中，detail为歌曲标题（格式: 歌曲名 - 歌手）
- `"paused"` - 暂停中（play_time 停止推进时自动检测），detail为歌曲标题
- `"waiting_song"` - K歌窗口未打开/焦点丢失，等待用户打开或点击K歌窗口
- `"standby"` - 待机状态，K歌客户端已退出

**无状态时（服务刚启动尚未获取到状态）：**
```json
{
  "type": "status_update",
  "data": {}
}
```

#### 2. `song_info_update` - 歌曲信息更新

**有歌曲信息时：**
```json
{
  "type": "song_info_update",
  "data": {
    "name": "告白",
    "singer": "花澤香菜",
    "title": "告白 - 花澤香菜"
  }
}
```

**无歌曲信息时（K歌窗口未打开 / 已关闭）：**
```json
{
  "type": "song_info_update",
  "data": {}
}
```

**清空行为：**
- K歌窗口关闭时，服务主动广播 `data: {}` 清空歌曲信息
- 直接切歌（A→B）时，不会先广播空再广播B，而是直接广播B的信息

#### 3. `lyric_update` - 实时歌词更新

**播放中（有歌词时）：**
```json
{
  "type": "lyric_update",
  "data": {
    "line_index": 5,
    "text": "君に嘘をついていた",
    "timestamp": 9.0,
    "play_time": 9.15,
    "progress": 0.4167
  }
}
```

**无歌词时（歌曲未开始 / K歌窗口已关闭）：**
```json
{
  "type": "lyric_update",
  "data": {}
}
```

**清空行为：**
- K歌窗口关闭时，主动广播 `data: {}` 清空歌词
- 仅在状态变化时发送一次，不会重复发送
- 客户端接收到 `data: {}` 时应清空显示的歌词内容

#### 4. `all_lyrics` - 完整歌词列表

**有歌词时：**
```json
{
  "type": "all_lyrics",
  "data": {
    "song_title": "告白 - 花澤香菜",
    "duration": 236.0,
    "play_time": 1.2,
    "count": 12,
    "lyrics": [
      {"index": 0, "time": 0.5, "text": "いつもそばにいるのに"},
      {"index": 1, "time": 2.1, "text": "ふと気付くと遠すぎて"},
      {"index": 2, "time": 3.8, "text": "手を伸ばしても届かない"}
    ]
  }
}
```

**无歌词时（K歌窗口未打开 / 已关闭）：**
```json
{
  "type": "all_lyrics",
  "data": {}
}
```

#### 5. `lyric_idle` - 空闲消息（歌曲播放结束）

```json
{
  "type": "lyric_idle",
  "data": {}
}
```

> 注：`lyric_idle` 仅在歌曲播放完毕后发送，`data` 始终为 `{}`。

#### 6. `playback_pause` - 暂停播放

当 play_time 连续多次不变时检测为暂停：
```json
{
  "type": "playback_pause",
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
  "data": {
    "play_time": 45.2
  }
}
```

> 注：前端收到 `playback_pause` 应停止时间插值，收到 `playback_resume` 应以 `play_time` 为锚点重新开始插值。

---

### WS 完整生命周期示例

```
（客户端连接，K歌窗口未打开）
← {"type":"status_update","data":{"status":"waiting_song","detail":"等待打开K歌窗口"}}
← {"type":"song_info_update","data":{}}
← {"type":"lyric_update","data":{}}
← {"type":"all_lyrics","data":{}}

（用户打开K歌窗口，选择歌曲A）
← {"type":"status_update","data":{"status":"loading","detail":"有点甜"}}
← {"type":"song_info_update","data":{"name":"有点甜","singer":"汪苏泷/BY2","title":"有点甜 - 汪苏泷/BY2"}}
← {"type":"status_update","data":{"status":"playing","detail":"有点甜 - 汪苏泷/BY2"}}
← {"type":"all_lyrics","data":{"song_title":"有点甜 - 汪苏泷/BY2","duration":236.0,"play_time":0.5,"count":28,"lyrics":[...]}}
← {"type":"lyric_update","data":{"line_index":0,"text":"男：摘一颗苹果","timestamp":18.326,"play_time":18.15,"progress":0.05}}
← {"type":"lyric_update","data":{"line_index":1,"text":"男：等你从门前经过","timestamp":20.198,"play_time":20.05,"progress":0.1}}
...

（用户关闭K歌窗口）
← {"type":"status_update","data":{"status":"waiting_song","detail":"等待打开K歌窗口"}}
← {"type":"song_info_update","data":{}}
← {"type":"all_lyrics","data":{}}
← {"type":"lyric_update","data":{}}

（用户重新打开K歌窗口，选择歌曲B）
← {"type":"status_update","data":{"status":"loading","detail":"如愿"}}
← {"type":"song_info_update","data":{"name":"如愿","singer":"王菲","title":"如愿 - 王菲"}}
← {"type":"status_update","data":{"status":"playing","detail":"如愿 - 王菲"}}
← {"type":"all_lyrics","data":{"song_title":"如愿 - 王菲","duration":280.0,"play_time":0.3,"count":35,"lyrics":[...]}}
← {"type":"lyric_update","data":{"line_index":0,"text":"我在时间尽头等你","timestamp":25.5,"play_time":25.3,"progress":0.03}}
...

（用户不关闭K歌窗口，直接从歌曲B切到歌曲C）
← {"type":"status_update","data":{"status":"paused","detail":"如愿"}}
← {"type":"playback_pause","data":{"play_time":9.2}}
← {"type":"lyric_idle","data":{}}

← {"type":"status_update","data":{"status":"loading","detail":"留什么给你"}}
← {"type":"song_info_update","data":{"name":"留什么给你","singer":"孙楠","title":"留什么给你 - 孙楠"}}
← {"type":"status_update","data":{"status":"playing","detail":"留什么给你 - 孙楠"}}
← {"type":"all_lyrics","data":{"song_title":"留什么给你 - 孙楠","duration":255.0,"play_time":0.4,"count":32,"lyrics":[...]}}
← {"type":"lyric_update","data":{"line_index":0,"text":"最初的爱给了你","timestamp":19.2,"play_time":19.0,"progress":0.04}}
...

（用户暂停播放）
← {"type":"playback_pause","data":{"play_time":45.2}}

（用户恢复播放）
← {"type":"playback_resume","data":{"play_time":45.2}}
← {"type":"lyric_update","data":{"line_index":5,"text":"在时间里等你","timestamp":46.0,"play_time":46.1,"progress":0.16}}
...

（歌曲播放完毕）
← {"type":"lyric_idle","data":{}}

（用户关闭程序）
← {"type":"status_update","data":{"status":"standby","detail":"K歌客户端已退出"}}
← {"type":"status_update","data":{"status":"waiting_process","detail":"K歌客户端未启动"}}
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
        console.log(`状态: ${msg.data.status} - ${msg.data.detail}`);
      }
      break;
      
    case 'song_info_update':
      if (msg.data && msg.data.title) {
        console.log(`♪ ${msg.data.name} - ${msg.data.singer}`);
      } else {
        console.log('歌曲信息已清空');
      }
      break;
      
    case 'all_lyrics':
      if (msg.data && msg.data.lyrics) {
        console.log(`共 ${msg.data.count} 行歌词`);
      } else {
        console.log('歌词列表已清空');
      }
      break;
      
    case 'lyric_update':
      if (msg.data && msg.data.text) {
        console.log(`[${msg.data.line_index}] ${msg.data.text}`);
      } else {
        console.log('当前歌词已清空');
      }
      break;
      
    case 'lyric_idle':
      console.log('歌曲播放结束');
      break;
  }
};

ws.onerror = (error) => {
  console.error('WebSocket 错误:', error);
};
```

---

## 空数据判断规则速查

| 事件类型 | 有数据时 `data` | 无数据时 `data` | 客户端判断有无数据 |
|---|---|---|---|
| `status_update` | `{"status":"...","detail":"..."}` | `{}` | `msg.data && msg.data.status` |
| `song_info_update` | `{"name":"...","singer":"...","title":"..."}` | `{}` | `msg.data && msg.data.title` |
| `all_lyrics` | `{"song_title":"...","duration":N,"play_time":N,"count":N,"lyrics":[...]}` | `{}` | `msg.data && msg.data.lyrics` |
| `lyric_update` | `{"line_index":N,"text":"...","timestamp":N,...}` | `{}` | `msg.data && msg.data.text` |
| `lyric_idle` | — | `{}`（始终） | 收到即为空闲 |
| `playback_pause` | `{"play_time":N}` | — | 收到即为暂停 |
| `playback_resume` | `{"play_time":N}` | — | 收到即为恢复 |

---

## 错误响应格式

所有 HTTP 接口在出错时返回：
```json
{
  "code": -1,
  "msg": "error message",
  "data": {}
}
```

当前实现中，HTTP 接口总是返回 code 为 0 和对应的数据。
