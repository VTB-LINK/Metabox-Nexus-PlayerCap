# 酷我音乐 接入侦察记录（第八个播放器，**未定型**）

侦察笔记，不是设计定稿。证据等级：**实测** = 在本机 `9.6.0.0_W1` 实例上跑过/量过；**推断** = 由实测推导；**待验证** = 必须真机再跑一次。侦察期间**没有**修改酷我的任何文件；有一次 `GetLyricByRID` 探测把 `kwmusic.exe` 打崩（见 §6，教训写在里面）。

**2026-10-22 更新**：封面与高频客户端时钟已实现，详见 §11。§7 保留的是旧四秒快照的历史记录，
不能再作为主时钟设计。默认 `pos.Open` 已使用播放核心，旧方案仅由 `OpenSnapshot` 显式调用。

**2026-10-23 更新**：逐字时间公式已从客户端恢复并经五份歌词样本核对，见协议文档 §1.3、§6。
用户确认允许依赖 CDP，音频链路不作为当前阻塞项；无需先实现全原生元数据/状态才能接入。

**2026-10-23 集成更新**：`feat/kuwo-integration` 已实现播放器主循环、逐字输出、项目接线及离线门禁。
已规避静态分析中与现场吻合的 CDP 尾换行形状，未动态重放致崩帧；限定版本的本机管线与 HTTP/WS/SSE 测试已通过。
历史崩溃原始请求未保存在 dump 中，不能声称唯一根因已经证明；完整证据与边界见 §12 和[硬化记录](kuwo-cdp-hardening-notes.md)。

---

## 0. 结论摘要

| 能力 | 状态 |
|---|---|
| 歌曲元数据（歌名/歌手/专辑/albumid/artistid/rid/tpay） | ✅ 实测可用，走 CEF 桥命令 `GetNowplaySong` |
| 播放/空闲状态、当前 SongId | ✅ 桥命令实测可用（`GetPlayStatus` → `Status=1` 播放中 / `Status=2` 暂停 / `Status=3` 空闲）；Go 传输整链验收见 §12 |
| 换歌信号 | ✅ `SetPlayMark(<rid>)` 推送 + `GetNowplaySong` 变化 |
| **播放位置（ms）** | ✅ `KwService.exe` 内 `PlayerCore.dll` 的底层进度缓存；50ms 采样连续变化，暂停冻结、拖动与循环归零已观测。`pos.OpenCore` / `CoreClock`，见 §11；四秒快照仅为旧方案 |
| **封面** | ✅ `player/kuwo/cover.Fetch` 按 RID 获取，通过公共处理链规范化为 800×800；不依赖 CDP，见 §11 |
| **歌词（整首 LRC + 行时间）** | ✅ **已逆出并实现**：请求/响应两层的加密都破了（循环密钥 `yeelion`），不依赖客户端、不需要签名。协议真源见 [kuwo-lyric-protocol.md](kuwo-lyric-protocol.md)，实现见 `player/kuwo/lyric/` |
| 逐字歌词 | ✅ **时间公式及解析器已实现**：由 `[kuwo:]` 解码两个系数，将 `<a,b>` 还原为片段开始/时长（ms）。客户端静态证据 + 1705 标记交叉核对，见 [kuwo-lyric-protocol.md](kuwo-lyric-protocol.md) §6；无效字级数据整行降级为逐行 |
| 拿到 CDP 的前提 | 必须「杀掉 + 带 `--remote-debugging-port` 重启」（单实例，不能热附加）——实测可行，见 §5 |
| 内存读取 | 只有当酷我以**中完整性**运行时可用（本机它被管理员启动 → VM_READ 被拒）；插件自己拉起时是中完整性，可读（§2） |

**当前状态**：主循环、事件、项目注册及逐字解析已实现；限定版本本机接入验收已完成（§12），非跨版本兼容或长期直播稳定性保证。
状态走 CDP，1/2/3 已实测；未知枚举拒绝，不推断缓冲态。`CoreClock.Advancing` 仅表示最近是否观察到进度变化。


---

## 1. 环境事实（实测）

| 项 | 值 |
|---|---|
| 版本 | 酷我音乐 `9.6.0.0`（内部串 `MUSIC_9.6.0.0_W1`） |
| 安装目录 | `C:\Program Files (x86)\kuwo\kuwomusic\9.6.0.0_W1`（32 位程序） |
| 启动器 | `9.6.0.0_W1\Kwmusic.exe`（455KB，`asInvoker`） |
| 主程序 | `9.6.0.0_W1\bin\kwmusic.exe`（645KB，`asInvoker`，FileVersion `9.6.0.0`） |
| 子进程 | `bin\KwWebKit.exe`（CEF 子/渲染进程）、`bin\KwService.exe`（P2P + 播放核 + 本机 HTTP 服务） |
| 自启项 | `HKLM\...\WOW6432Node\Microsoft\Windows\CurrentVersion\Run` → `kwmusic = "<安装目录>\Kwmusic.exe" /autorun` |
| 选址键 | `…\Uninstall\酷我音乐`：`DisplayVersion=9.6.0.0`、`InstallLocation=…\9.6.0.0_W1`（首选，比扫目录稳） |
| 用户态目录 | `%LOCALAPPDATA%\kwmusic\cefcache`（CEF cache + `http_h5app.kuwo.cn_0.localstorage`） |
| 配置 | `bin\Conf\default\config.ini`（`[lyric]` 里 `newserver=http://newlyric.kuwo.cn`、`newstrategy=1`；另有 `[DeskLyric]`、`[NowPlayLyricWnd]`、`[NowNewPlayingPageWnd]`、`[MiniLyricWnd]`） |

进程树（实测，`ParentProcessId`）：`kwmusic.exe` → `KwService.exe` + `KwWebKit.exe`（后两者在启动同秒被拉起）。

## 2. 权限（实测，决定机制选型）

- 本机这份实例是**管理员（高完整性）**：子进程 `KwService.exe` 的 `TokenIntegrityLevel` RID = `0x3000`；对照 `explorer.exe` 与本 shell 为 `0x2000`。
- 对 `kwmusic.exe` 的逐权限试探（调用方 Medium）：

| 权限 | 结果 |
|---|---|
| `PROCESS_TERMINATE (0x1)` / `SYNCHRONIZE` / `QUERY_LIMITED_INFORMATION` | OK |
| `VM_READ` / `VM_WRITE` / `VM_OPERATION` / `CREATE_THREAD` / `DUP_HANDLE` / `SET_INFO` / `SUSPEND` / `QUERY_INFORMATION` / `WRITE_DAC` | 拒（err 5） |

- 同机上 `explorer`/`obs64`/`DingTalk`/`QQ`/`steamwebhelper` 的 `VM_READ` 全部 OK ⇒ 只有酷我这一家被挡（高完整性 + 令牌 DACL 的组合，不是它自设安全描述符）。
- **中完整性运行时**（插件自己拉起，实测）：三个进程 RID 均为 `0x2000`，`VM_READ`/`QUERY_INFORMATION` 放行，`ReadProcessMemory` 可读。
- 可执行的副作用动作：`TerminateProcess(TERMINATE)` 从 Medium **可以**杀（实测成功），而 `taskkill /F` 会被拒（它需要 `QUERY_INFORMATION`）。
- 拿不到：命令行（WMI/`Win32_Process.CommandLine` 空）、主模块路径、模块列表（`tasklist /m` 空）——高完整性下同样是被同一组权限卡住。

## 3. 三条通路

### 3.1 原生 UI（DuiLib）——没有可刮的文本（实测）

窗口：`kwmusicmaindlg`（主窗口）、`KwDeskLyricWnd`（桌面歌词，`[DeskLyric] DLLineCount=1`）、`KwDeskLyricTopWnd`/`KwDeskLyricUnlockWnd`、`PlayerVolumnWnd`、`MvWnd`…；歌词由 `KwModLyricShow.dll`/`UIDrawLyricMgr.dll`（`Module.xml`）自绘，**窗口标题为空**，无 `WM_GETTEXT` 可刮（推断：GDI/D2D 自绘，无 accessibility 树）。截屏验证也因窗口透明/坐标叠加（截到的是别的窗口）不可靠。

### 3.2 CEF（Chromium 49）——主通路（实测）

- `bin\libcef.dll` 43.31MB / x86 / `ImageBase=0x23450000` / 版本串 `3.2623.1401.gb90a3be` ⇒ **CEF 3.2623 ≡ Chromium 49**（`/json/version` 实测：`Chrome/49.0.2623.110`、`Protocol-Version 1.1`）。
- **浏览器进程 = `kwmusic.exe`**：加载 `KwWebKitDll.dll`（含 `CefApp`/`CefClient`/`CefBrowserProcessHandler`/`CefRenderHandler`(OSR)/`CefWebCallMusic`/`IKwWebKitMgr`，动态解析 `cef_initialize`、`cef_browser_host_create_browser_sync`）；窗口枚举可见 `Chrome_WidgetWin_0`、`Chrome_SystemMessageWindow`、`KwBrowserMgrWnd`。
- **`KwWebKit.exe` = 纯子进程**：只解析 `cef_execute_process`/`cef_api_hash`/`cef_v8context_*`/`cef_v8value_create_*`，**没有** `cef_initialize`/`cef_browser_host_create_browser`。
- 页面 = `bin\html\webdata\netsong\*`（Vue webpack SPA；`static/common.js` 1.5MB）且**会自更新**（`bin\html\webpacket\netsong.zip` 6.3MB，侦察当日被重写过；日志 `NETSONGVER:9.6.0.0_202605271030`）⇒ 实现不得依赖本地 JS 文件内容，只依赖 CDP 里的全局函数/桥命令。
- 页面导航：每个频道一个 HTML（`nav.txt`：`songlib→index.html`、`channel_*`、`content_*`…），另有单曲页 `singleComment.html`（"单曲评论页"，含歌词区 + 评论）与 `singleCommentGuide.html`（原生播放页上的"查看歌词和歌手写真"提示）。

### 3.3 共享内存桥（`KwShareMemMgr.dll`，实测字符串）

- 对象名 `KwShareMemory--201703031800`，锁 `KwLockMutex-201703031800`；导入 `CreateFileMappingW`/`MapViewOfFile`/`CreateMutexW`/`WaitForSingleObject`/`ReleaseMutex`；失败串 `CreateFileMapping fail: 0x%08x`、`MapViewOfFile fail: 0x%08x`、`KwLockMutex fail:`。
- 导出：`KwShareMemMgr::InitShareMem()`、`ExitShareMem()`、**`GetDataByCmd(const std::wstring& cmd, std::wstring& out) -> uint32`**、`SetConfig/SetVer/SetUserState/SetOpenChargeSong`。
- 写端 = `KwWebKit.exe`（渲染侧实现网页的 `window.external.callkwmusic(cmd)`）；分发端 = `KwMusicDLL.dll`（宿主）。
- 桥的固定命令（同 DLL 字符串）：`GetCache/SetCache/GetConfig/SetConfig/GetVer/SetVer/UserState/OpenChargeSong/Netsong/domComplete/showwindowex/resize/IsOpenImage/webproxyip/LogRealTime`。

## 4. 已验证的桥命令（**实测**，全部从 CDP 里的页面注入调用）

注入方式：`window.external.callkwmusic("<cmd>")`（返回值）或 `(...,0)`（不取值）。

| 命令 | 实测返回 | 备注 |
|---|---|---|
| `GetVer` | `MUSIC_9.6.0.0_W1` | 版本串 |
| `GetCurChannel` | `songlib` | 当前网页频道 |
| `GetPlayingProgress` | `""`（空串） | 在"推荐"页与直接导航出的"单曲页"上都是空 ⇒ **位置拿不到** |
| `GetPlayStatus` | `Status=1&SongId=658118183`（播放中）/ `Status=3&SongId=`（空闲） | 状态 + 当前 rid |
| `GetNowplaySong` | `playtype=0&quality=2&song=list=正在播放&rid=MUSIC_658118183&name=湮灭&album=湮灭&albumid=104916954&artist=萨吉&artistid=222244&mvid=0&tpay=0&` | **双层 URL 编码**；元数据全套 |
| `SetPlayMark(<rid%3D…>)` | —— | 宿主→页面的推送，实测在"推荐"页能收到 |
| `PlayCtrl?playState=0/1` | 无可见效果 | 状态未变化（语义未定，**未验证**） |
| `PlaySongByRid?asnext=1&needplay=1&type=single&rid=<rid>&playsrc=<enc>&websrc=<enc>` | —— | 客户端自己的"播放"调用形态（`checkComment.js` 同形），实测能起播 |

其它从 JS 里读出的相关命令（未逐个验证）：`GetPlayingWebSource`、`GetSongPos?albumid=`、`GetLyricByRID?param=`、`InitComplete`、`ShowSearchLyricDlg`、`ShowLyricMenu`、`MoveLyricForward/Backward`、`ResetLyric`、`observe?webID=&observer=&callback=`。

## 5. CDP 的开启方式（**实测**）

```
1) 杀：TerminateProcess(kwmusic.exe)      ← Medium 权限即可；taskkill /F 不行
2) 起：bin\kwmusic.exe --remote-debugging-port=9224   （不经提权；WMI CreateProcess 可脱离父 shell）
3) 探：GET http://127.0.0.1:9224/json → 一个 page target
        title "推荐" / url file:///<安装目录>/bin/html/webdata/netsong/index.html
        ws://127.0.0.1:9224/devtools/page/<id>
```

- **只认启动时命令行**：先在已运行实例上执行不会开端口，且酷我有单实例逻辑（`KwLib::Process::KillOtherInstance`，日志 `ACT:AppStart … Num:617 Exit:1`）——实测第二次拉起会立刻退出。
- 静态依据（字节级实测）：`libcef.dll` 里 `remote-debugging-port`（VA `0x255418A0`）有 3 处引用，其中一处是 `HasSwitch` 门控 + `GetSwitchValue` + 端口范围检查 + 以该端口构造 DevTools HTTP handler；另一处是 `CefSettings.remote_debugging_port`（1024..65535）校验后补开关。`KwWebKitDll.dll` 自己也在拼命令行（`--enable-system-flash`、`OpenWebDebug`、`&Show DevTools`）。
- 端口预算（全机监听实测）：`9222` 网易云、`9223` Spotify、`9229` 汽水、`12233` 酷狗 ⇒ 酷我用 **9224**。

## 6. 歌词：已逆出（协议真源见 [kuwo-lyric-protocol.md](kuwo-lyric-protocol.md)）

**结论先说**：歌词的请求与响应两层编码都已破，**用我们自己拼的请求就能取到任意歌曲的整首 LRC**，
不依赖客户端、不需要签名、不需要 CDP。实现与夹具在 `player/kuwo/lyric/`。

一句话规格：`GET http://newlyric.kuwo.cn/newlyric.lrc?base64(xor("yeelion", 明文))`，
明文是 `user=…&requester=localhost&req=1 &songname=<GBK>&artist=<GBK>&rid=MUSIC_<id>&lrcx=1&olrc=1`；
响应是「文本头 + CRLFCRLF + zlib(base64(xor("yeelion", GBK 的 LRC)))」。完整的字段表、LRC 格式、
以及**逐条证据链**（两条请求的公共前缀 → 重放 → 客户端内存里的明文 → 异或得到 `yeelion` →
跨歌验证 → 反推请求 → 自造请求闭环）都在协议文档里。

以下是侦察阶段留下的、仍然成立的旁证（不必再看，但能解释当时为什么绕了远路）：

- 宿主会造 `SetPlayInfo` / `SetCurLyric` / `SetLyricTime`（`KwMusicDLL.dll` 里的载荷构造表与网页
  解析逐键对上），但**在可达页面上一次都没投递**——用它取歌词这条路本来就走不通。
- `KwLib.dll` 导出的 `Entrypt::Encrypt/Decrypt`（按长度整体移位 + 循环异或，
  密钥 `KoOtOiTvINGwd` / `_Y8g2E6n0E1i7L5t2IoOoNk`）是**红鲱鱼**：歌词通道不用它。
  协议文档 §2 末尾专门写了这一条，免得后来者再走一遍。
- **事故点（务必记住）**：`GetLyricByRID?param=<我猜的值>` 一调就把 `kwmusic.exe` 打崩了（`DumpReport.exe … /logname CRASH` 起、`act.log` 刷 `MODULE:kwservice|ERR:PROCESSLOSE|SUBERR:KWMUSIC`）。
  ⇒ **纪律：桥命令只准用客户端自己写过的形态与参数；任何自造参数都是直播事故候选**（AGENTS §0.1）。
  反请求格式时改走「抓客户端自己的请求 + 内存里的明文」这条路，就没有再碰客户端。


## 7. 旧位置方案：四秒快照（历史记录，已由 §11 取代）

### 7.1 为什么不用 CDP（实测三次，两个会话）

| 命令 | 返回 | 结论 |
|---|---|---|
| `GetPlayingProgress`（酷我自己的单曲页就用它取位置） | `""`（空串），即使 `GetPlayStatus` 是 `Status=1`（播放中） | 宿主不给我们能连上的这些窗口回时间 |
| `GetPlayStatus` | `Status=1&SongId=204419549` / `Status=3&SongId=` | 只有状态 + rid，**没有毫秒** |
| `GetPlayingWebSource` | `playState=1&rid%3D204419549` | 有 `playState`，不是时间 |
| `GetNowplaySong` | 歌名/歌手/专辑/ids/rid/tpay | 元数据，无时长无位置 |
| 页面 DOM | `innerText` 里 `\d{1,2}:\d{2}` 零命中；`[role=progressbar]`/`progress`/`time` 类元素零命中 | 时间轴在原生播放条上，网页没有可刮的时间 |

`GetSongPos?albumid=` 也不是时间（它回的是「这张专辑里正在播哪一首」）。所以位置只能走内存。

### 7.2 位置在哪（实测布局，x86，9.6.0.0）

「当前歌曲信息」结构，偏移相对 **rid 字符串起点**：

| 偏移 | 类型 | 实测值 | 含义 |
|---|---|---|---|
| +0 | char[16] | `MUSIC_204419549\0` | rid（≤15 字节走 std::string 的 SSO，内联在这里） |
| +16 | int32 | 15 | 质量/码率档 |
| +20 | int32 | 15 | 同上 |
| +24 | int32 | 29000 | **时长（毫秒）** |
| +28 | int32 | 21220 | **位置（毫秒）** ← 要的 |
| +32 | int32 | 1 | 未验证语义（播放标志？） |

### 7.3 它是低频快照，不是实时值

实测（20 秒、200ms 采样）：**每 4.000 秒刷新一次，每次恰好 +4000ms**，rate 严格 1:1；
单曲循环回到 0 时该槽直接变 0。所以直接把它当位置发出去最多滞后 4 秒，
正确用法是**锚点 + 墙钟插值**（仓库已有的 `player.LivePosition` 就是干这个的），
并且快照在 `4s + 0.5s` 内没再更新时视为不再推进（暂停/缓冲），位置冻结在锚点。

### 7.4 实现与限制（`player/kuwo/pos`）

- `OpenSnapshot(pid, rid, durationMs)`：在 `kwmusic.exe` 私有可读写内存里按 **rid 字符串**定位结构，
  旧时长回退仍只解析内联 RID，**不支持长 RID 指针**，也不能靠时长确认当前歌曲；不得作为可靠兜底。`Validate` 校验 rid+NUL、
  质量档 0..100、时长 3s..1h、位置 ≤ 时长；
- `Reader.Position()` 读一次毫秒；`Tracker.Observe/Estimate/Stalled` 做锚点、插值、冻结，
  以及「正常刷新 vs 跳变（seek/循环）」的区分；
- 真机用例 `TestLiveRead`（`PLAYERCAP_LIVE=1 KUWO_RID=MUSIC_…`）实测：定位到
  `dur=29000ms @0x98B24A8`，10 秒内 3 次正常刷新（13.1→17.1→21.1→25.1s），插值推到 26.7s，
  `seek` 误判 0 次。

写播放器前必须知道的限制：

1. **暂停检测最慢 4.5 秒**（靠"快照停更"判断）。CDP 的 `Status` 在暂停时是否变化**未验证**；
   若会变，播放器层应优先用 CDP 判播放/暂停，快照只提供位置。
2. **向前 seek 可能漏报**：判据只抓「变小」和「大过 经过时间+一个周期+容差」两种。首次读到的
   快照相位未知（实测 +4000 可能只隔 1.2 秒就到），所以判据刻意宽松——漏报只影响 overlay
   动画重置，位置本身总是重锚，不会错。
3. **需要中完整性**：酷我被管理员启动时 `VM_READ` 被拒（实测 err=5），`Open` 返回
   `ErrNotFound`，播放器应降级为"没有位置"。
4. **结构地址每次启动都变**（堆），每首歌都要重新定位；定位是一次全私有内存扫描
   （实测 0.5~1 秒），只在换歌时做，不进轮询。
5. **结构里的时长可能小于平台时长**：只缓存了一部分的歌实测 `dur=29000`（29 秒），
   位置与 clamp 都以结构里的时长为准。

### 7.5 其它信号（仍成立的旁证）

- 桥命令 `GetPlayingProgress` 的失败方式见 §4；`act.log` 每首歌开始有一行
  `ACT:PLAY_MUSIC … DUR:<秒> … PT/NPT:<毫秒>`，只有换歌时刻的快照，且 `DUR` 会跟着缓存进度变
  （实测同一首歌从 198 变成 29）。
- 内存里还有一串 ASCII 进度记录 `…,0,<时长ms>,<位置ms>,1,MUSIC_<id>,1…`（上报用的环形缓冲），
  区域会漂移，不要拿它当数据源。
- 逆歌词时顺带确认：**已解码的 LRC 明文确实躺在 `kwmusic.exe` 内存里**（ASCII，`[kuwo:…` 开头），
  但那也是堆上的临时缓冲（多处拷贝、地址随换歌变化）⇒ 歌词走 §6 的网络路径，别读内存。

## 8. 机制选型与下一步

以下表格保留早期候选方案；最终采用 A + C + §11 高频核心位置，未采用 B 或 D 的四秒快照。

| 方案 | 拿到什么 | 代价 / 风险 | 判断 |
|---|---|---|---|
| **A. CDP + 桥命令（元数据/状态）** | 歌名/歌手/专辑/ids/rid、播放-空闲状态、换歌 | 一次用户可见的重启（带 flag）；之后零提权 | ✅ 已验证，作为基座 |
| **B. 让宿主把歌词推给我们** | 整首 LRC（含逐字）+ 偏移 + 位置（`SetPlayPos` 触发） | 需找到宿主认定的"歌词页"窗口/打开方式（候选：`PageJump`/`ShowPublicWebUIWnd`——**参数未验证，有崩客户端风险**）；窗口可能是用户可见的 | 待攻关，必须先证明参数安全 |
| **C. 自实现歌词请求** | 任意 rid 的整首 LRC + 行时间，与播放器内部完全解耦 | 已完成，无需签名；协议见 [kuwo-lyric-protocol.md](kuwo-lyric-protocol.md) | ✅ **已实现**（`player/kuwo/lyric/`） |
| **D. 内存快照 + 插值（位置）** | 位置（客户端结构里 4 秒一次的快照，1:1），插值后连续 | 需中完整性；结构地址每次启动变，换歌要重新定位（~0.5-1s）；暂停检测最慢 4.5s | ✅ **已实现**（`player/kuwo/pos`，见 §7） |
| **E. 降级**：`act.log` 尾巴 | 只有换歌时刻的歌名/歌手/rid/时长 | 无位置、无歌词正文 | 只作交叉验证 |

**该阶段待办已完成**：§11 封面和高频位置已接入 `player/kuwo` 主循环、事件与项目注册，状态使用 CDP。
出声进程 RMS 按用户要求不作为本次阻塞项；最新验收与剩余边界见 §12。


## 9. 接入落点（照 `AGENTS.md §5.1`，三处硬编码都要改）

1. `player/kuwo/`：`const PlayerName = "kuwo"` + `init(){config.RegisterPlayer(PlayerName)}`、`New(offset,poll)`、`Start()`；模板补 `kuwo-offset/-poll/-idle-hide/-audio-normalize` 四行 + README 参数表（否则 `config/keyplaces_test.go` 红）。CDP 客户端建议照 `player/cloudmusic/cdp` 与 `player/kugou` 的写法，`watchdog` 照 `player/spotify/watchdog`（带 flag 重启 + 端口探测 + 只重启一次）。
2. `main.go`：import、`New(...)`、**追加进 `playerNames`**、`router.Register`、`go func(){ defer telemetry.Guard(); kw.Start() }()`；自带轮询下限则登记 `playerPollIntervals`。
3. `audiocap/procscan/names_windows.go`：`Players` + `PlayerProcess`（+ `names_windows_test.go`）。**出声进程尚未验证**：酷我有 `KwService.exe`（`CPlayCoreStub`/`GetPlayerCore`）与 `KwVirtualizer.dll` 在音频链路上，到底是不是 `kwmusic.exe` 出声必须真机看 `/<player>/audio-ws` 的 RMS。

另需同步：`doc/openapi.yaml`（player enum、端点表）、`doc/API_RESPONSE_EXAMPLES.md`、`i18n/messages*.go`。

## 10. 待验证清单

- **T1** ~~宿主认定的"歌词页"窗口是哪一种~~ —— 已被方案 C 取代，不再需要（歌词不再经宿主推送）。
- **T2** ~~让 `GetPlayingProgress` 返回毫秒的条件~~ —— 不再追：位置已由 §7 的内存快照解决；
  若哪天发现让宿主回时间的方法，`pos` 可以整体退化成 CDP 实现（接口不变）。
- **T3** ~~`sig` 算法与 lrcx 解压~~ —— **已完成**：无签名，密钥是循环串 `yeelion`，
  响应是 zlib + base64 + 同一把异或，见 [kuwo-lyric-protocol.md](kuwo-lyric-protocol.md)。
- **T4** ~~位置的稳定内存锚点~~ —— **已完成**（§7）：按 rid 字符串锚（>16 字节退回时长锚），
  `player/kuwo/pos` 与真机用例都在。剩下要验的是**跨版本**：换 9.6 以后的版本要先跑一遍
  `TestLiveRead`，结构布局/快照节奏都可能变。
- **T5** 出声进程（`kwmusic.exe` / `KwService.exe` / 虚拟声卡）。
- **T6** 歌词覆盖度：逐字解析与 `text_detailed` 已接线；2026-10-24 从《アイロニ - 4円》取得
  `[ml:1.0]` 双语 lrcx，并接入 `sub_text`（协议文档 §7）。音译未提供，其他双语格式未验证。
  纯 LRC 仍需降级；《湮灭》只是片头信息均匀，正文存在非均匀逐字。
- **T7** 暂停/恢复/seek 的可判据（`GetPlayStatus` 的完整枚举 + 是否存在 seek 事件），本项目 `player/` 契约要求 seek 复用 `playback_resume` 且每 tick `EmitPositionSample`。
- **T8** 重启后是否保持提权（本机那份实例是管理员启动的；插件自己拉起时是中完整性——酷我的虚拟声卡/音效等提权相关功能是否降级未验证）。

## 11. 2026-10-22：运行实例、封面与高频时钟

### 当前实例（本轮只读实测）

- 主进程 `kwmusic.exe` PID 50140，直接子进程 `KwService.exe` PID 47688，安装版仍为 `9.6.0.0_W1`。
- 主进程命令行只有 `/autorun`，9224 无监听；本轮没有重启播放器或写目标进程内存。
- 当前歌曲 `等风也等你 / 苏晚清`，RID `MUSIC_568213041`，客户端时长 154 秒。
  RID/时长来自播放核心，歌名/歌手由核心中的 GBK 字符串只读核对（尚未做元数据公共 API）。
- 主进程和服务进程均可用 `PROCESS_QUERY_INFORMATION | PROCESS_VM_READ` 读取。
  **没有重新测令牌完整性级别**，不把“可读”直接等同于“已测中完整性”。
- SMTC 枚举只见 QQ Music，未见酷我。用户授权后也没有向 QQ Music 发送控制命令；暂停与拖动由用户在酷我完成。

### 封面

客户端 `singleComment.js` 自己使用的请求：

```text
http://artistpic.kwcdn.kuwo.cn/pic.web?type=rid_pic&pictype=url&size=500&rid=568213041
```

实测返回 `http://img1.kwcdn.kuwo.cn/star/albumcover/500/s4s67/92/1349231143.jpg`。
`cover.Fetch(ctx, rid, timeout)` 已完成 URL 解析、无图/失败处理及公共封面下载规范化。
原图 500×500 JPEG、69529 字节；输出 800×800 JPEG、120734 字节。
详见 [`player/kuwo/cover/README.md`](../../player/kuwo/cover/README.md)。

### 时钟证据与实现

`PlayerCore.dll`（`9.6.0.0_W1` 侦察机实测记录）SHA-256：
`4ef97f6a18b00fcf5a9331b7a067871e3fa36aa1bcba09c2c97e33fd3e8f3b9a`。
PE 时间戳 `0x6a1fa2a8`，映像大小 `0x2f000`，x86。**这些是 W1 的历史测量值，不再作为准入条件**：
准入改按结构匹配（见下文实现入口），时间戳只进日志，映像大小取 Toolhelp 的 `ModBaseSize`。

| 定位 | 含义 |
|---|---|
| 导出 `GetPlayerCore` RVA `0x12890` | `mov eax,<模块基址+0x25750>; ret`，直接返回静态单例 |
| 单例 `+0x84` | 当前 RID，MSVC `std::string`（16 字节内联区、长度、容量） |
| 单例 `+0xa8` | 客户端时长，秒；每次读取刷新，不沿用换歌时缓存 |
| 单例 `+0x188` | 当前底层播放器选择；本轮仅验证值 1 的 native audio 分支 |
| 单例 `+0x2a0` | 播放位置，毫秒 |
| 函数 RVA `0x14e70` | 调用底层播放器进度 getter，将返回值写入 `+0x2a0` |

静态链进一步落到 `CKuwoPlayer.dll` 的 `0xa710 → 0x10be0 → 0x14090`，
最终调用音频插件的时间查询；**本轮只反汇编并读数据，没有调用这些客户端函数**。
这与主进程四秒快照不是同一槽位。原笔记把 std::string 的长度/容量 15/15 当作质量档，不适用于新布局解析。

实现入口：`pos.OpenCore(mainPID)` 根据父子关系选服务，按结构匹配 PlayerCore——解析 PE 头与节表、
导出表，把唯一的 `GetPlayerCore` 导出解析成单例，再要求进度 getter 的机器码逐字节出现在该单例虚表的
某个槽位（槽位序号不钉死）；对 `9.6.0.0` 的任意渠道后缀放行，不比时间戳、不要求固定映像大小。
`Read(expectedRID)` 读取 RID/位置/时长并复核歌曲身份，首个 dword（vtable 值）与打开时不一致即拒绝。
长 RID 解引用字符串指针；不再按相同时长猜歌曲。结构不符、未验证 backend、短读、越界数据和换歌撕裂
都返回错误，不自动退回旧四秒源。
`pos.Open` 已接新源，保留其原参数签名；`OpenSnapshot` 仅供诊断对比。

`CoreClock` 直接输出采到的真实毫秒，不用四秒墙钟外推；同值样本立即保持原位，
前后 seek/循环重新落到真实值，换 RID 清状态，旧时间戳样本被丢弃。
`Advancing` 是 350ms 内是否见过变化，**不是原生 paused/buffering 的枚举**。

实测结果：

| 场景 | 观测 |
|---|---|
| 正常播放，Go 真机用例 | 100 次、50ms 采样全部变化；墙钟 5.0351528s，位置 +5030ms，最大变化间隔 50.6098ms |
| 用户多次拖动 | 59420→103000ms、104390→59914ms、59914→85000ms，均被只读采样捕获 |
| 单曲循环 | 153800→0ms；重启播放过渡会短暂出现 backend=0，读取器拒绝过渡样本 |
| 用户暂停/恢复 | 20940ms 固定约 12.766s，恢复后继续推进 |
| 用户确认最小化后 | 再跑 Go 用例：100 次全部变化；墙钟 5.0327368s，位置 +5030ms，最大变化间隔 50.6232ms |
| 封面＋时钟组合探针 | `go run ./tools/kuwoprobe -duration 5s -cover`，封面成功；100 次变化，0 次跳过 |

原始只读采样保存在本机临时调查目录 `kuwo-clock-edef31f2d38549eabd80056bb13ffefc` 的
`clock-samples.jsonl` 与 `clock-samples-1792683207.jsonl`。表格保留关键结果供临时目录清理后复查。
这里验证的是**客户端进度时钟与墙钟速率**，没有测声卡/OBS 的端到端音频延迟，不能把数值差当作绝对听感误差。

### 验收与剩余工作

离线用例覆盖字段单位、错误 RID、长 RID、动态时长、撕裂、未知版本、指令指纹、暂停不漂移、seek/循环和迟到样本。
隔离副本中已分别注入位置偏移错误、暂停继续外推、跳过 getter 指纹、旧样本跨 RID 重置四类缺陷，相关测试均变红。
真机用例在无 seek 时另核对位置增量与墙钟之差不超过 200ms（需保持播放）；有 seek 则明确不做稳态速率断言。

本节原待办的最新状态（2026-10-23）：

1. 已实现主循环、歌曲/歌词/封面事件、`EmitPositionSample`、换歌代次与资源释放，并有离线回归及隔离变异验证。
2. CDP 元数据及状态 API 已实现，原始嵌套 query 有真实夹具；发送端规避尾换行形状后，Go 真机传输与管线通过，见 §12。
3. 已接入 `main.go`、配置、音频映射与文档；音频波形 RMS 于 2026-10-24 在 `9.6.0.0_UG1` 真机核对通过（独立测试栈连 `/kuwo/audio-ws`，RMS 非零、帧率约 26、坏帧 0）。
4. 已实现逐字解析与输出；翻译于 2026-10-24 补充实测 `[ml:1.0]` 格式支持，音译、其他版本/backend 不提供；本轮事件链已覆盖暂停/恢复、前后拖动、两首歌切换、客户端重新连接及瞬态失败恢复。多版本与长时间运行另行验收。

复跑（只读，`-cover` 会访问酷我封面服务）：

```powershell
go run ./tools/kuwoprobe -duration 10s -cover
$env:PLAYERCAP_LIVE='1'
go test ./player/kuwo/pos -run '^TestLiveCoreClock$' -v -count=1
Remove-Item Env:PLAYERCAP_LIVE
```

## 12. 主循环接入与验收记录（2026-10-23）

- 开发分支：`feat/kuwo-integration`。
- 元数据/状态每 200ms 读 CDP；位置按配置轮询，最低 50ms。读数须匹配 RID，结构不符的核心或非原生后端拒绝（对 `9.6.0.0` 任意渠道后缀放行）。
- 封面和歌词各一条 worker，待办容量各 1；换歌取消旧请求，结果同时检查代次与 RID。
- 逐字行按实际显示起点稳定排序、重编索引；文件 offset 与用户 offset 分开处理，播放位置不套歌词 offset。
- 暂停 seek 发一次 resume 校正位置后补 pause，防止前端继续计时；seek 与恢复同拍只发一次 resume。
- 失联先清空公开歌曲状态再等待后台任务退出；不会自动启动原本没运行的酷我。受控重启仅允许白名单内构建（`knownBuilds` 的 EXE 哈希，当前 W1、UG1）、精确进程身份，每个服务进程最多一次；白名单外不重启。

### 本轮实际观察

用户授权重启及暂停/恢复/拖动。重启后手动播放 RID `398013033`《白鸽乌鸦相爱的戏码》/潘成（皮卡潘）。
Node 桥探针读取 `Status=1`；执行源码已确认的 `PlayCtrl?playState=pause` 后为 `Status=2`。
Go 只读核心探针 3 秒内位置恒为 33600ms、变化 0、拒绝 0；恢复后 3 秒观察到 60 次变化、拒绝 0。
同曲封面请求成功，原图 500×500、26235 字节，经公共处理得到 800×800、51409 字节。

首次 Go CDP live 用例超时，随后 `/json` 返回空数组，`KwWebKit.exe` 已退出，主进程与音频核心仍在运行。
`act.log` 的 00:38:55.978 记录 `WKCRASH / KWWEBKIT`；相应 dump 显示 `libcef.dll+0xA5DA88`，
异常 `0xC0000005` 读取空地址。时间相关不等于已证明 Go 请求是根因。
当前已将请求收敛为成功 Node 探针的 `JSON.stringify` 对象形状及无尾换行单个 WS 文本帧，
这是当时的临时处置记录。随后静态分析得到与现场吻合的尾换行拒绝及空指针路径，未动态重放；发送形状规避后的真机结果如下；
“裸桥返回导致崩溃”的假设已被崩溃位置证据否定，不应继续引用。

### 离线验收与剩余门槛

已跑通 `gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`，以及
Linux 多包构建 `config/logger/i18n/tools/genconfig`。后续修改仍需重跑门禁。
隔离变异覆盖旧代次回写、换歌漏取消、重复 resume、位置误套 offset、未知状态未拒绝、
逐字系数与文件 offset、解压上限、CDP 身份复核、EXE 指纹与单次重启预算，均实际变红。
首次因缺少 C 编译器而未能执行 race；2026-10-23 安装 WinLibs GCC 16.2.0 / MinGW-w64 14.0.0 UCRT 后，
在 Go 1.26.5 windows/amd64、CGO=1 下，`go test -race ./player/kuwo/... -count=1` 与
`go test -race ./... -count=1` 均通过（含 `tools/`，未开启真机 opt-in 用例）。
未检测到数据竞争或测试失败，无需为此修改业务代码；环境与复跑命令见 `doc/design/kuwo-cdp-hardening-notes.md`。

后续本机复测（同日 22:43–22:51）：

- 最小 Go CDP 快照通过；12 秒完整管线通过，《梦的光点》66 行、474 个字级片段、封面完整。
- 仅启动 Kuwo + Router + Server，分别运行 180 秒和 90 秒，没有启动其他播放器或音频捕获。
- 根路径与 `/kuwo` 的 HTTP、WS、两类 SSE 全部返回正确的 `player=kuwo`；WS 共记录 135 条事件并跑断言。
- 前后拖动捕获到约 129 秒与 30 秒位置；换成《光辉岁月》后取得 44 行、349 片段和新封面，无旧歌曲回写。
- 确认进入暂停后 HTTP 位置 27.610 秒持续 3 秒不变；恢复后继续推进。切歌中一次无效快照触发清空，约 3 秒自动重连恢复，保留为观察项（2026-10-24 在 UG1 上复测为每次切歌必现，成因与处理见[硬化记录](kuwo-cdp-hardening-notes.md)的“构建匹配与真机复测”，现已改为重试）。
- 核心时钟再次测试：100/100 次变化，墙钟 5.0349327 秒、进度 +5030ms，最大变化间隔 51.737ms。
- 两次服务均正常 Stop 退出，测试端口 18765 已关闭；网页进程仍在，CDP 页面仍存在，没有新增崩溃 dump。

准确结论是已完成上述限定环境功能验收；不是长期 soak、全部歌曲/后端或所有进程生命周期的穷举。
客户端画面逐字高亮及声卡/OBS 延迟未做同步校准；音频 RMS 按用户决定不作为本次阻塞项。
完整管线只读测试入口为 `PLAYERCAP_KUWO_PIPELINE_LIVE=1 go test ./player/kuwo -run '^TestLivePipeline$' -v -count=1`，
要求酷我已开启 CDP 且播放有逐字歌词的歌曲；该用例不启动、重启或控制播放器。
