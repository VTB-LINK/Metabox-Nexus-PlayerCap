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
├─ React / Redux 状态 → 歌曲 ID、歌词、播放状态
└─ DOM → 歌名、歌手、封面、进度文本

PlayerCap (cloudmusicv3 模块)
├─ Watchdog 确保进程带调试端口启动（注册表注入自启参数）
├─ WebSocket CDP 客户端连接浏览器
├─ JS 求值 → React Fiber 遍历 → 提取 Redux + DOM 状态
├─ 切歌时强制 Redux 刷新歌词，优先使用当前歌曲 Redux ID
├─ DOM / Redux 双重校验，避免旧歌词、旧封面串到新歌
├─ 网易云 API / CDP 获取歌词（LRC 解析）+ 封面
├─ 本地时钟锚定 + seek 检测
└─ play_time 停滞检测 → 暂停/恢复事件
```

### QQMusic（进程内存读取 + AOB Hook）

```
QQMusic.exe 进程
├─ QQMusic.dll + 0xC87C80 → 歌曲元数据（歌名/歌手/SongID/进度/时长）
├─ QQMusic.dll + 0xC157D8 → 快速计时器指针（~1秒更新）
├─ QQMusic_GFWrapper.dll → 伴奏滑块控件（AOB Hook 捕获 ESI/EDI）
└─ QQMusic.dll + 0x488B75 → 精确进度写入点（AOB Hook + KUSER 时间戳）

PlayerCap (qqmusic 模块)
├─ 进程内存扫描定位 QQMusic.dll + QQMusic_GFWrapper.dll
├─ AOB Hook 注入（伴奏滑块）
├─ 快速计时器锚点 + 本地时钟实时线性插值
├─ QQ 音乐 API 获取歌词（QRC 3DES 解密）+ 专辑封面
├─ 快速计时器异常跳变检测 seek（支持前跳 / 回跳）
└─ 快速计时器停滞检测 → 暂停/恢复事件
```

### KuGou（CDP 远程调试 + libcef patch）

```
KuGou.exe 进程
├─ libcef.dll → CEF / Chromium 内核
├─ DevTools 端口 12233（由 PlayerCap 自动 patch libcef.dll 打开）
├─ desktop-popup 页面 → external.SuperCall(864) 播放信息接口
└─ PlayInfo → hash、filename、cover、progress、duration、playStatus

PlayerCap (kugou 模块)
├─ Watchdog 自动定位酷狗安装目录（注册表 + Program Files 兜底）
├─ 检测 libcef.dll patch 状态，必要时终止酷狗 → 提权 helper patch → 重启酷狗
├─ WebSocket CDP 客户端连接 desktop-popup 页面（端口 12233）
├─ JS 求值调用 SuperCall 获取播放信息
├─ hash 变化检测切歌，filename 拆分歌名 + 歌手
├─ 酷狗歌词 API 获取歌词（hash 优先，必要时按歌名/歌手/时长解析 canonical hash）
├─ CDP 封面 URL + 酷狗公开 API 兜底获取封面，异步下载 Base64
├─ 本地时钟锚定 + 100ns progress 单位换算 + seek 检测
├─ 伴唱/伴奏等同曲 hash 变化时复用同名同歌手歌词
└─ play_time 停滞检测 → 暂停/恢复事件，CDP 断开后自动回到等待状态
```

### SodaMusic（CDP 远程调试 + 明文 KRC）

```
SodaMusic.exe 进程（Electron）
├─ 原生反调试：启动参数带 --remote-debugging-port 会被它自杀，故不能改 argv
├─ 命名映射 node-debug-handler-<pid> → 主进程里 inspector 激活函数的地址
├─ Node inspector 端口 9229（激活后由目标自己开，反调试不检测它）
├─ rendererMain 主窗口 → 字节的 transport 服务 → sharedState.get('player')
└─ 播放态：progressSeconds（1Hz 采样）、mediaDetail.playable（名/歌手/id/时长/封面）、
           mediaDetail.lyrics（明文 KRC + translations.cn 独立译轨）

PlayerCap (sodamusic 模块)
├─ Watchdog 找主进程 pid（命令行不含 --type= 的那个），复刻 process._debugProcess 开 9229
│  —— 只读命名映射 + 让目标跑它自带的激活函数，不改汽水任何内存/状态
├─ CDP 连 9229 主进程 Node context，executeJavaScript 桥进 rendererMain 取全量播放态
├─ 取数时顺带关掉该窗口的后台节流：汽水最小化且约 5 分钟无人交互后，Chromium 会把进度
│  刷新从 1Hz 压到 1/60Hz；这一步让它最小化时也保持 1Hz，设不上则告警一次而非静默降级
├─ mediaId 变化检测切歌；mediaDetail 未就绪（歌名为空）时跳过本轮，不发半成品
├─ 时长比歌名晚到时给 2 秒宽限，避免 all_lyrics 带着 duration=0 出去让进度条整首归零
├─ 歌词是平台已解密的明文 KRC，复用 player/krc 解析出逐字，连解密都省
├─ 翻译来自 translations.cn 的独立 tlyric LRC，按绝对时间戳（±10ms）合并进 sub_text
├─ 封面 URL 由 cover_url 的 {urls,uri,template_prefix} 拼成 800×800，异步下载 Base64
└─ 本地时钟外推：进度源只有 1Hz，仅在采样变化时落锚、两次采样之间按墙钟推进
   —— 每轮都落锚等于没有外推，position 会退化成 1 秒阶梯、歌词整体滞后
```
