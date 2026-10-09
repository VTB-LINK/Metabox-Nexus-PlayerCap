# AGENTS.md — Metabox-Nexus-PlayerCap 开发指南（给 AI agent / 协作者）

本文件是在本仓库工作的**强制流程**，取代 `instruction.md`——后者的内容已逐条裁决后合并至本文，
文件本身已删除，原文见 git 历史。下文若干处引用它，是为了说明某条规矩的来历或某条错误规范的
剔除理由，不代表该文件仍然存在。

这个服务在主播直播时跑在播控机上，OBS 从它拉歌词、特效与音频波形 overlay。**它挂了 = 直播事故。**
它对接五个没有公开 API 的国产音乐播放器，手段是内存扫描 / CDP / 二进制补丁 / 代码注入，
因此运行在一组脆弱假设之上。质量优先级：**稳定 > 正确 > 简洁 > 功能完整度**。

---

## 0. 最高约束

读不完全文就只读这五条。每一条都对应已经发生过的故障。

1. **绝不炸主播的机器。** 我们读别人的进程内存、改别人的 DLL、移别人的窗口。任何一步失手，
   代价由主播在直播中承担，且往往**不可自行恢复**（例：酷狗补丁打坏 → 只能重装酷狗）。
   宁可降级（不显示歌词、明确报「不支持」），绝不盲干。
2. **绝不改网易云的特效 canvas。** 改它的 `width`/`height`/`style` 会让网易云**崩溃**。
   捕获只能 `drawImage` 只读拷贝。分辨率上限由网易云窗口尺寸决定，唯一的调节手段是请用户
   放大窗口。
3. **`config/`、`logger/`、`i18n/` 必须保持 `GOOS=linux` 可构建。** 三者恰是 `tools/genconfig`
   的全部内部依赖（`config` 与 `logger` 都 import `i18n`），而两条打包流水线在 Linux runner 上
   原生跑它生成随包 `config.yml`。`i18n` 的 Windows 专有代码只能放在带构建约束的文件里，且
   `!windows` 侧必须有桩（现为 `detect_windows.go` / `detect_other.go`）。破坏它
   = 两条流水线同时挂，且失败点报在「打包」而非「构建」，排查方向容易被带偏。
   **这是 CI 的实现细节，不是平台目标——本项目是纯 Windows。**
   自检：`GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./i18n/... ./tools/genconfig`
4. **`syscall`/`windows.NewCallback` 只能在包级 `var`。** 它有全局上限（2000）且**永不回收**，
   在轮询路径上每 tick 新建会很快耗尽配额并使进程失败。样板见 `player/wesing/proc/memory.go`。
   ✅ 有门禁：`player/callback_lint_test.go`
5. **绝不给构建加 `-H windowsgui`。** 本程序是控制台应用，全部日志经 `log.Printf` 走 stderr
   **且不落盘**。改成 GUI 子系统后 stderr 无处输出，日志全部丢失，直播出问题时没有现场证据。
   （`-H windowsgui` 从未出现在任何构建脚本中；`instruction.md` 曾把它写成既定规范，与实际
   构建配置不符，已剔除。）

---

## 1. 这个项目是什么 / 数据怎么流

### 1.1 定位

纯 Windows Go 服务，跑在主播的**播控机**上，做两条主产品线和一项附加能力：

1. **歌词刮取与推送** —— 从五个没有公开 API 的国产播放器（全民K歌 `wesing` / 网易云v3 `cloudmusicv3` / QQ音乐 `qqmusic` / 酷狗 `kugou` / 汽水音乐 `sodamusic`）刮实时歌词与播放状态，经 WS/SSE/HTTP 推给 OBS overlay。
2. **网易云特效 canvas 画面镜像** —— 把网易云播放页的特效 canvas 逐帧抓成 JPEG，喂给 OBS 浏览器源。**这是独立的第二条产品线，不是歌词的附属功能**，它有自己的通路、自己的进程干预机制（park 屏外泊车、CDP 注入）。
3. **实时音频波形（附加能力）** —— 对在跑的播放器做进程级 WASAPI loopback（只读），在隔离的 helper 子进程（同一 exe 的 `--audio-capture-helper` 子命令）里算 RMS、峰值与频谱帧，经 `/audio-ws`（跟随 activePlayer）与 `/<player>/audio-ws` 推给 OBS overlay。**地位与前两条不同：任何故障只许降级成「没有波形」，绝不许拖垮前两条**——本地 recover 不挂 Guard（§1.2）、helper 隔离与按需捕获（§3.6）都由此派生。

服务中断即直播事故。质量优先级：**稳定 > 正确 > 简洁 > 功能完整度**。

模块名 `Metabox-Nexus-PlayerCap`（`go.mod` 的 `module` 行），import 前缀照此写。Go 1.25.0。

### 1.2 启动顺序（`main.go`，顺序有原因，别重排）

```
main()
├─ defer telemetry.Guard()                    ← 主 goroutine 的 panic 上报；os.Exit 不跑 defer
├─ 0. 子进程模式短路，不走下面任何一步
│     --kugou-patch-helper <libcefPath>：提权 helper，patch 完 DLL 即 os.Exit。
│       （旧的 --kugou-patch / RunPatch 是无闸门裸写原语，已随 7ab6ee6 删除）
│     --audio-capture-helper：音频捕获 helper（Manager 用 exec 拉起），helper.Run(stdin, stdout)
│       读控制、写帧，stdin EOF 即返回；自带 telemetry.Init + Guard     ★ 必须早于 1
├─ 0.5 i18n.SetLanguage(i18n.DetectSystemLanguage())   ← 早于任何用户可见输出，见 §6.4
├─ 1. ensureCanonicalName()
├─ 1.5 cleanupLegacyExe()                     ← 旧版 WesingCap 残留由后台 goroutine 重试删除
├─ 2. config.Load()
├─ 3. playerNames := []string{wesing, cloudmusicv3, qqmusic, kugou, sodamusic}
│     ← 播放器的真开关，见 §5.1
├─ 4. Banner（按 playerNames 循环生成）
├─ 5. telemetry.Init(telemetry.Options{...})  ← 空 DSN 自动禁用；★ 早于 7
├─ 6. telemetry.PrintPrivacyNotice(10s, isReleaseVersion)  ★ 必须早于任何联网动作
│     两段文案各归各的开关：遥测段看 DSN，更新段看 isReleaseVersion。两段都不适用即立即返回
├─ 7. checkAndUpdate()                        ★ 阻塞，在服务起来之前
├─ 7.5 startDailyHeartbeat()                  ★ 必须晚于 7；只发请求、绝不做更新动作
├─ 8. server.NewServer(playerNames)
├─ 9. effectStrategy: Win11 强制降级 park → fadeout
├─ 10. go srv.Start(cfg.Addr, readyCh); <-readyCh   ★ 等端口就绪
├─ 11. 五个播放器 New(offset, poll)
├─ 12. server.NewRouter + router.Register ×5
├─ 13. go wp/cp/qp/kp/sp.Start()              ← 五个采集 goroutine
├─ 14. park.RestoreOrphaned()                 ★ 在捕获器之前
├─ 15. effect.New(srv, cp.IsConnected); go .Run()
├─ 15.5 manager.New(srv.AudioHub(), …); srv.SetAudioWake(audioMgr.Wake); go audioMgr.Run()
│                                             ★ 晚于 7 与 10；本地 recover，不挂 Guard（见下）
├─ 16. signal handler: park.Unpark + RestoreChrome + os.Exit(0)（helper 由 Job Object 回收，§3.6）
└─ 17. router.Run()                           ← 阻塞主循环
```

**出货代码（根包、`server/`、`player/`）起的每个 `go` 都要带 `defer telemetry.Guard()`**，`main.go` 里即上树的 1.5/7.5/10/13/15/16；**唯一例外是音频子系统**（15.5 的管理器，以及下段列出的主服务侧音频 goroutine）。
挂法：goroutine 入口函数体的第一句。`go wp.Start()` 这种调用外部包方法的形式没地方挂 defer，所以包一层闭包：`go func() { defer telemetry.Guard(); wp.Start() }()`；`go x.m(args)` 调同包方法时，挂在被调方法的第一句，**别改写成闭包**——闭包按引用捕获变量，而原写法在 go 语句处就对实参求值，改写可能改变语义。
Guard 捕获 → 上报 → Flush → **原样 re-panic**，进程照常死——采集 goroutine 带着未知的损坏状态
继续跑，会把错位歌词一直推上 OBS 且骗过所有人（进程还活着，没人会去重启）。漏挂不改变任何行为，只是这次崩溃不进 Sentry，所以只能靠门禁发现。
✅ 有门禁：`telemetry/goroutineguard_test.go`（出货代码的每个 `go` 语句入口都挂了 Guard 或列名豁免）、`telemetry/guarddirect_test.go`（Guard 必须 `defer` 直接调用，包闭包会让 recover 静默失效）。

**例外边界：主服务侧的音频 goroutine 一律本地 recover、绝不挂 Guard——别「照上面的规矩修好」它。**
恰为四处：`main.go` 15.5 的 `defer localRecover("audio-manager")`；`Manager.idleKeepalive` 与 `Manager.demux`
（`audiocap/manager`，内联 recover）；`handleAudioWS` 的写 goroutine（`server/audio.go`，`audioRecover`）。理由与上条
相反：波形是可降级的附加能力（§1.1），re-panic = 为它杀掉歌词与特效两条主线；而这些 goroutine recover 后直接结束，
不带损坏状态继续输出。代价：只留一行 Warn、不进 Sentry；`idleKeepalive`、写 goroutine 与 `Run` 不自动重启（`demux` 仍 `close(hp.exited)`，随 helper 重启重建）；`Run` panic 展开时经起手的 defer 拆除 helper，`idleKeepalive` 照常推空闲帧；写 goroutine 被恢复后经 defer 关连接，读循环返回、订阅注销（删对应 defer 即红，实测）。helper 子进程内**照常**挂 Guard——那里 re-panic 只杀 helper。
自检：`git grep -n -e 'defer localRecover(' -e 'defer audioRecover(' -e 'recover()' -- main.go server audiocap ':!*_test.go'` 应只命中上述四处与 `localRecover` / `audioRecover` 两个定义。
门禁覆盖分两层看。`goroutineguard_test.go` 只扫根包、`server/`、`player/`，在那里只认 `localRecover` / `audioRecover` 两个具名豁免：把 `main.go` 或 `server/audio.go` 的豁免包一层闭包（recover 失效，panic 未上报即杀主进程）即红（实测）。`audiocap/` 不在其扫描范围，`Manager.idleKeepalive` 与 `Manager.demux` 的内联 recover 不经这两个具名函数、不受它约束。
❌ 靠人：把上述任一 goroutine 改挂 `defer telemetry.Guard()`，两条 Guard 门禁都绿（Guard 本身合规；`guarddirect_test.go` 只查形状、不查位置，实测）；`audiocap/` 内两处的任何改动都没有门禁。

各 ★ 的**为什么**（改动前必读）：

| 约束 | 为什么 |
|---|---|
| **`--audio-capture-helper` 短路必须留在步骤 0（早于 1）** | helper 是同一 exe 的子进程，stdout 就是帧通道。往后挪任何一步都会出事：`ensureCanonicalName` 可能复制 exe 并重启自身；`config.Load()` 的 `flag.Parse()` 不认此参数会直接退出（推断，未实测），helper 起一个死一个、5 次后本会话放弃；Banner 与隐私提示写 stdout，污染帧流；`checkAndUpdate` 会让子进程联网、换 exe。 |
| **`telemetry.Init()` 必须早于 `checkAndUpdate()`** | 自动更新本身就是故障点：下载/校验失败会 `os.Exit(1)` 结束进程。Init 在它之前，那条路上的崩溃才在上报范围内。（未注入 DSN 时 Init 自动禁用，本地构建无副作用。） |
| **`PrintPrivacyNotice()` 必须早于 `checkAndUpdate()`** | 那一步会联网、可能替换 exe 并重启进程。告示得出现在**任何联网动作之前**，否则就是先斩后奏。未启用遥测时它立即返回——不打印也不等待，本地调试不受影响（`telemetry/privacy.go`）。 |
| **`checkAndUpdate()` 必须早于 `srv.Start()`** | 它可能替换自身 exe 并调用 `restartSelf()` → `os.Exit(0)`。端口先绑上，重启的新进程会撞端口冲突。 |
| **`startDailyHeartbeat()` 必须晚于 `checkAndUpdate()`** | 同上那条的推论：`checkAndUpdate` 可能 `os.Exit(0)`，在它之前起心跳等于给一个马上要死的进程挂后台 goroutine。**心跳只发请求、不解析响应、不做任何更新动作**——运行期换 exe 就是直播事故。✅ 有门禁：`heartbeatinert_test.go` |
| **播放器必须等 `<-readyCh` 之后再起** | 端口未就绪时，网易云注入脚本回连 `ws://127.0.0.1:<port>/cloudmusicv3/effect-ingest` 会失败；采集侧也会对着一个还没监听的服务发事件。 |
| **`park.RestoreOrphaned()` 必须早于 `effect.New()`** | 上次崩溃可能把网易云窗口遗留在屏外泊车位。捕获器一旦开始跑 park 状态机（80ms 一 tick），会在一个未知的遗留状态上做决策。先还原，再交给捕获器。 |
| **音频捕获管理器（15.5）必须晚于 `checkAndUpdate()` 与 `<-readyCh`** | 前者同心跳那条：`checkAndUpdate` 可能 `os.Exit`/换 exe，不该先 spawn helper（Manager 无订阅不 spawn 是双保险，不是可以挪的理由）。后者是 `Manager.sweepOrphans` 的前提：它清杀「同名 exe 且命令行含 `--audio-capture-helper`」的进程，只在单实例时安全，而同端口的第二实例必在 `srv.Start` 撞端口退出、到不了 15.5。不同 `addr` 的双实例不在此保护内（推断）。 |

Win11 门控（步骤 9）：**绝不删掉 `park.IsWindows11()` 的强制降级。** Win11 的 DWM 停止合成不可见窗口，park 屏外保活失效，主播看到的是画面黑掉。❌ 靠人。

### 1.3 四条数据通路

**关键心智模型：特效帧与音频帧都完全不经过事件总线。** 二者与 ① 的接点都是只读的：都经 `SetActivePlayer` 钩子接收选址结果（§2.1）；特效另在 `effectStatusJSON` 读 `s.activePlayer`、经 `cp.IsConnected` 读网易云连接态；音频另经 `GetPlayerStatus` 读状态缓存；都不回调 Router。此外只共用同一个 `http.Server` 和同一个 `websocket.Upgrader`。

```
① JSON 事件流（歌词/状态）
   player.Start() ──EventCh(128, 满则丢)──> fan-in goroutine ──merged(256, 阻塞)──> router.Run()
                                              (每播放器一个)
   router.Run(): UpdatePlayerState(缓存) → updateRouting(选 activePlayer) → dedup(内容哈希)
                 → NotifySubscribers ──sub.ch(64, select default 丢弃)──> WS / SSE

② 特效帧流（网易云 canvas 镜像）—— 不经过 router，不经过 merged，不产生任何 player.Event
   网易云进程 (CDP 9222)
     └─ Page.addScriptToEvaluateOnNewDocument 注入抓帧脚本
          └─ 页面内 JS ──WS BinaryMessage(JPEG)──> /cloudmusicv3/effect-ingest
               └─ ingestHandler = Capturer.ingestFrame(门控)
                    └─ BroadcastEffectFrame ─effectHub─> /cloudmusicv3/effect-ws

③ per-player API（逃生舱）
   /wesing/ws、/kugou/all_lyrics、/qqmusic/song_info-SSE …
   直接读 playerStates 快照，与 ① 的路由决策（activePlayer）完全无关。
   「我就要盯这个播放器」，调试与镜像通道靠它。

④ 音频帧流（实时波形）—— 不经过 router，不经过 merged，不产生任何 player.Event
   播放器进程树 ──WASAPI 进程 loopback（INCLUDE_TARGET_PROCESS_TREE，只读）──> helper 子进程
     └─ stdout 自包含帧（布局见 doc/openapi.yaml 的 /audio-ws）──> Manager.demux（按 Magic 重对齐，坏帧只丢）
          └─ BroadcastAudioFrame ─audioHub─> /audio-ws（跟随 active 缓存）、/<player>/audio-ws（固定）
   控制反向走 stdin（capture / stop），由 Manager 按「订阅门控 + root 在播预热」下发；无订阅即拆 helper。
   Manager.idleKeepalive 每 100ms 直推空闲帧（不捕获）：root 有订阅且无 active 时 player=""；
   某播放器 HasSubscribers 为真而 300ms 内未转发过它的真帧时 player=<名>（root 与 per-player 同一规则）。
   与 ① 的接点（只读）：SetActivePlayer 钩子写 audioHub 的 active 缓存 + Manager.Wake；
   Manager 经 GetPlayerStatus 判在播。不回调 Router。
```

关于 ②：入口是 `/cloudmusicv3/effect-ingest`，写端有**进程内一次性随机 token 鉴权**（`?t=`，注入脚本经 `EffectIngestWSURL()` 内部取址获得，`handleEffectIngest` 在 Upgrade 前常量时间比较、不匹配即 403），但**仍共用恒真的 `CheckOrigin`（`server.go` 的 `NewServer` 中的 `upgrader`）、无 Origin/回环校验、默认绑 `0.0.0.0:8765`**——写端安全靠 token，不靠 Origin。`/service-status` **不再广告 ingest 地址**（端点仍在，注入脚本内部取址）。边界与未保障的假设记在 §14 —— 改它之前先读那一节。特别是：**不要「去掉 CheckOrigin」，那会把 ingest 和 OBS 浏览器源一起打断**。

### 1.4 目录结构

```
├─ main.go              启动编排、自动更新、每日在线心跳、ensureCanonicalName、提权 helper 与音频 helper 入口
├─ config/config.go     ★ 必须 GOOS=linux 可构建（CI 依赖，见 §0）
├─ logger/logger.go     ★ 同上
├─ clientid/            匿名客户端标识 + 版本检查请求头（供网关统计 DAU/MAU）
│                       ★ 绝不依赖 telemetry —— 反过来依赖会让隐私提示门禁成环，见 §3.5
├─ telemetry/           Sentry 故障上报、隐私提示、系统信息采集（空 DSN 自动禁用）
├─ i18n/                中英文案表 + 显示语言探测（探测隔离在 detect_windows.go，见 §6.4）
│                       ★ logger/、config/ 依赖它，同须 GOOS=linux 可构建（见 §0）
├─ taskbar/             控制台任务栏进度（OSC 9;4 / ITaskbarList3），仅在启动期 checkAndUpdate 中使用
├─ server/
│   ├─ server.go        HTTP/WS/SSE、声明式路由表、PlayerState 缓存、订阅者
│   ├─ router.go        优先级路由 + 超时状态机（★ 无 HandleFunc，不管端点注册）
│   ├─ effect.go        effectHub、ingest、effect-ws —— 独立于事件总线
│   ├─ audio.go         audioHub、/audio-ws —— 独立于事件总线；root 经 SetActivePlayer 钩子跟随 active
│   ├─ dedup.go         switchSkip 的事件内容 FNV 哈希
│   └─ types.go
├─ player/
│   ├─ player.go        Player 接口、BaseEmitter、事件常量与载荷类型
│   ├─ cover.go         封面下载（显式 timeout 参数）
│   ├─ krc/             公共包：酷狗 KRC 明文解析（ParsePlainKRC，kugou 与 sodamusic 共用单一真源）
│   ├─ wesing/          模式：进程内存只读扫描（proc/ + lyric/，PE 导出表定位 vtable）
│   ├─ cloudmusic/      模式：CDP + Redux
│   │   ├─ cdp/  lyric/  watchdog/   保活标记 + 注册表自启项修补
│   │   ├─ effect/effect.go   特效捕获器：注入、门控、park 决策
│   │   └─ park/park.go       屏外泊车（含崩溃落盘兜底、Win11 探测）
│   ├─ qqmusic/         模式：进程内存扫描（mem.go，运行时不写，见 §7.1）、QRC 解密、AOB 探针已关停(#39)
│   ├─ kugou/           模式：CDP + libcef.dll patch + 提权 helper
│   └─ sodamusic/       模式：CDP（复刻 _debugProcess 绕原生反调试开 9229）+ 明文 KRC；见 §7.7
│       └─ cdp/  watchdog/   watchdog=找主进程 pid + _debugProcess 激活 inspector
├─ audiocap/            实时音频波形（§1.1 第 3 条，可降级附加能力）；纯 Go，零 cgo
│   ├─ wasapi/          进程级 WASAPI loopback 捕获器（手搓 COM vtable，回调包级 var；Windows-only）
│   ├─ helper/          helper 子进程本体：捕获 + DSP + 归一化 + 分帧 + 解耦写（★ stdout 只写帧）
│   ├─ manager/         主服务侧：门控 + 预热 + Job Object + 退避 + 看门狗 + 解复用（★ 本地 recover）
│   ├─ procscan/        Toolhelp 进程快照、MainPID、播放器名 → 进程名（★ 新增播放器要改，§5.1）
│   └─ dsp/  frame/     RMS + 频谱、帧编解码（Magic 重对齐、长度上限）—— 纯 Go，Linux 可测
├─ tools/               全部是独立 CLI，不进出货二进制（各自用途见 §13.4）
│   ├─ genconfig/       ★ CI 在 Linux runner 上原生跑它生成随包 config.yml
│   ├─ winicon/         CI 两条流水线构建前原生跑它生成多尺寸图标 .syso
│   ├─ audioprobe/  cdpexplore/  devserver/  docsample/  parktest/  watchdogtest/  wsrecord/
│   ├─ kugouprobe/  qqprobe/  qqromaprobe/  sodaprobe/  wesingprobe/   只读探测工具
├─ doc/                 API 与参考文档
│   ├─ openapi.yaml            ★ API 唯一真源（线上 apifox 是它的手动导入产物）
│   ├─ API_RESPONSE_EXAMPLES.md
│   └─ core.md                 各播放器取数与音频波形捕获原理
└─ docs/                设计与实施计划（音频波形）；历史记录，与代码分叉时以代码为准
```

**`doc/openapi.yaml` 是 API 文档的唯一真源，线上 apifox 是下游副本，不是权威。** player enum 五家齐全（`components.parameters.PlayerParam` 的 `enum`），读端点 effect-ws 与 audio-ws（根与各 per-player）也已列入 `/service-status` 响应示例的端点表；唯 effect-ingest 未进 openapi —— 它是内部写端（token 鉴权、`/service-status` 不广告），有意不对外文档化，仅在 `doc/API_RESPONSE_EXAMPLES.md` 作内部端点记述（见 §14）。**加端点仍须同步补 `openapi.yaml`。** ❌ 靠人。

**`server/router.go` 不注册任何端点**（`grep -c HandleFunc server/router.go` = 0）。除下述例外，端点全在 `Server.Start` 的声明式路由表 `routes` 里（含二进制的 `/audio-ws`，kind `"audio"`）—— 这张表就是「新增播放器零代码注册」的实现，也是加 kugou 时不必碰路由、`/<player>/audio-ws` 自动覆盖全部播放器的原因。例外是 `Start()` 里紧随其后直接 `mux.HandleFunc` 的四个端点：`/health-check`、`/service-status`、`/cloudmusicv3/effect-ws`、`/cloudmusicv3/effect-ingest`。（按符号找：`routes := []routeDef`、`// 仅根路径的内部端点`。）

本节的顺序与路由约束没有测试守卫。上面的 ❌ 标记据此而来，不从相邻测试借用可信度。（**理由已更新**：本轮新增了 `server/routerorder_test.go`，但它只守 `evaluateGroup` 的**选址确定性**——`activeNames` 按 activeAt 降序、平局稳定、不丢成员——**不碰本节的任何一条**：prior/normal 组切换、holding 倒计时、组级释放、per-player 超时全部仍是零守卫。`server/race_test.go` 同样不涉路由。）

---

## 2. 承重设计原则

改这一节涉及的代码前，先读懂 why。这些设计看起来像可以顺手清理的偷懒或不对称，实际不是。

> **本节除 §2.1 表中音频两处丢最旧外全员 ❌**（那两处有单测守丢弃策略本身，见 §4）：`server/routerorder_test.go`（本轮新增）只守 `evaluateGroup` 的选址确定性，`server/race_test.go` 只覆盖 HTTP 快照原子性 —— **两者都不碰本节的任何一条**。下面每条只靠人守，破坏它们不会被任何门禁拦下。

### 2.1 fan-out 非阻塞 是 fan-in 阻塞可证明不停滞的前提

**通道与满时行为**：

| 通道 | 缓冲 | 满时行为 | 位置 |
|---|---|---|---|
| 播放器 `EventCh` | 128 | `Emit` 非阻塞，**直接丢事件** | `player/player.go` 的 `NewBaseEmitter`（建通道）、`Emit`（`select default`） |
| Router `merged` | 256 | fan-in **阻塞等待** | `server/router.go` 的 `Run`：`merged := make(chan player.Event, 256)` 与 fan-in goroutine 里的 `merged <- evt` |
| 订阅者 `sub.ch` | 64 | `select default` **丢弃** | `server/server.go` 的 `subscribe`（建通道）；发送点为表下所列四个函数 |
| 特效 `effectSub.ch` | 8 | 丢最旧帧后写入最新帧，保低延迟 | `server/effect.go` 的 `handleEffectWS`（建通道）、`BroadcastEffectFrame`（丢旧写新） |
| 音频 `audioSub.ch` | 8 | 丢最旧帧后写入最新帧（同 effect） | `server/audio.go` 的 `handleAudioWS`（建）、`BroadcastAudioFrame`（扇出） |
| helper `frames` | 64 | `enqueue` 丢最旧再写、仍满丢本帧，**采集 goroutine 永不阻塞** | `audiocap/helper/` 的 `Run`（建）、`enqueue` |

**绝不把 `NotifySubscribers` / `NotifySubscribersFullState` / `NotifySubscribersClear` / `notifyPerPlayerIdleClear` 里那八处 `select { case sub.ch <- ...: default: }` 改成阻塞发送；`BroadcastAudioFrame` 的丢最旧同理**——它全程持 `h.mu` 扇出，而钩子里的 `onActivePlayerChanged` 取同一把锁（见下）。helper 的 `enqueue` 也不许改成阻塞，但理由不同：阻塞会冻住采集排空（`docs/audio-waveform-capture-design.md` §2.5）。

**为什么**：`router.go` 的 `Run` 里 fan-in goroutine 的 `merged <- evt` 是阻塞的，而 `merged` 的**唯一消费者**是同一函数里的 `for evt := range merged` 循环——该循环在末尾**同步**调用 `r.server.NotifySubscribers(evt, switched)`。这条阻塞发送可证明不停滞的唯一前提，就是消费路径上不存在任何可能长时间阻塞的操作：订阅者发送一律非阻塞，网络 I/O 全部隔离在每连接的 WS 写 goroutine 里（`server.go` 的 `handleWS`）。

**`SetActivePlayer` 也在这条路径上**：`switchTo` 与 `watchExpire` 持 `r.mu` 调它，它的 `if changed` 块里挂着 `BroadcastEffectStatus`、`audioHub.onActivePlayerChanged`（取 `h.mu` 写一个字段，而 `BroadcastAudioFrame` 扇出全程持同一把锁）与 `audioWake`（向缓冲 1 的 Manager `wakeCh` 做 `select default` 投递，多次唤醒合并成一次重算）。往这里加钩子同样只许非阻塞，且不得回调 Router（`GetActivePlayer` 取 `r.mu.RLock`，同 goroutine 重入即死锁）、不得 panic（经 `router.Run` 时由 main 的 Guard、经 `watchExpire` goroutine 时由它自己的 Guard 上报后 re-panic，进程照样死）。音频的捕获动作因此全在 Manager 自己的 goroutine 上做。helper → Manager 的 stdout 管道不在这条路径上：写端阻塞只卡 helper 的写 goroutine，采集侧由 `frames` 隔开。❌ 钩子接线无测试：`server/audio_test.go` 直调 hub 方法，删掉 `SetActivePlayer` 里的 `onActivePlayerChanged` 测试全绿（实测）。

**破坏它会怎样**：一个卡住的 OBS 浏览器源 → `sub.ch` 写阻塞 → Run 循环停住 → `merged` 塞满 → 每个播放器的 fan-in goroutine（`router.go` 的 `Run` 里 `for _, p := range r.players` 起的那些）阻塞 → 各播放器 `EventCh` 塞满 → `Emit` 开始静默丢弃。整条流水线停摆，且不产生任何日志。反压从一个慢客户端一路传导回五个播放器 goroutine。

那个裸 `default:` 分支看起来像没写完的错误处理，它是承重的。要保证慢客户端送达必须换机制（per-sub 有界队列 + 主动断开慢客户端），**不能把丢弃改成阻塞发送**。

### 2.2 优先组评估绝不加门控；两组的不对称是刻意的

**绝不给优先组（prior）的评估加任何门控条件。**

| 组 | 状态事件路径 | 超时路径 |
|---|---|---|
| 优先组 | `updateRouting` 中 `switched = r.evaluatePriorGroup()` —— **无条件** | `watchExpire` 中优先组 per-player 超时与组级超时 —— **无条件** |
| 普通组 | `updateRouting` 中过 `if !r.priorGroupBlocking()`（状态事件分支与 `default` 分支各一处） | `watchExpire` 中普通组 per-player 超时与组级超时，均过 `priorGroupBlocking()` |

**为什么**：K 歌一开唱必须立即切主输出。任何延迟或条件化都会导致直播事故。

**破坏它会怎样**：以「对称化」为名给 prior 路径补上 `priorGroupBlocking()` 之类的门控，开唱后画面不切——这正是这套路由存在的全部理由。**这个不对称是设计，不是遗漏，不要重构掉。**

### 2.3 holding 只能由 `prior-player-expire` 超时释放

**绝不让其他播放器的事件抢走 holding。**

holding = `paused && activated`（`router.go` 的 `evaluateGroup` 中 `case "paused"` 分支）。它是「优先播放器暂停期间，画面不要被别的播放器抢走」的唯一机制。释放路径**只有超时**：per-player 暂停超时（`expireGroupPlayers` 的 paused 分支）与组级倒计时（`watchExpire` 中优先组、普通组各自的组级超时）。

**破坏它会怎样**：优先播放器暂停期间，其他播放器立刻抢走主输出。

### 2.4 优先组释放时必须强制清空普通组 holding —— 两条路径都要做

**改一处就是 bug。** 两条路径必须都把普通组 holding 清成 inert（一处调 `forceGroupInert(r.normalStates)`，一处是与之等效的内联循环）：

| 释放路径 | 位置 |
|---|---|
| 优先组全员 inert | `router.go` 的 `evaluatePriorGroup` 第 3 步：`for _, ns := range r.normalStates` 内联循环 + `r.normalGroupPaused = false` |
| 优先组组级超时 | `router.go` 的 `watchExpire` 优先组组级超时分支：`forceGroupInert(r.normalStates)` + `r.normalGroupPaused = false` |

**为什么**：没有这步，K 歌唱完释放的一瞬间，一个几十分钟前暂停的普通组播放器仍满足 `paused && activated` → 被判为 holding → 抢到主输出。

**破坏它会怎样**：唱完的瞬间，OBS 上显示几十分钟前的陈旧歌词。`watchExpire` 里 `forceGroupInert(r.normalStates)` 上方的注释「与 evaluatePriorGroup 全员 inert 路径一致」就是在提醒这两处必须同改。

### 2.5 `prior-player-expire: 0` 会关掉两个组的全部超时

**绝不把 `prior-player-expire` 设为 0，也绝不移除 `router.go` 的 `watchExpire` 里那个 `> 0` 判断而不重新设计超时门控。**

`watchExpire` 的 `if r.cfg.PriorPlayerExpire > 0` 是**整个 `watchExpire` 的总闸**——包在它里面的是：优先组 per-player 超时、优先组组级超时、**普通组 per-player 超时、普通组组级超时、loading 超时**（`expireGroupPlayers` 的 loading 分支）。设为 0 → 全部静默关闭 → holding 永不释放，activePlayer 永久卡死。

两个加重项：

1. **普通组的倒计时用的就是这个「优先播放器」配置项**——`evaluateNormalGroup` 启动组级倒计时的日志、`watchExpire` 中普通组的 per-player 超时与组级超时，全部读 `r.cfg.PriorPlayerExpire`。名字里的 `prior-player` 不提示它管着普通组。
2. **没有钳制**。`config/config.go` 中 `mergeYAML` 的 `prior-player-expire` 分支只做 `if i, ok := v.(int); ok`，0 是合法 int，直接落地。默认值 15（`DefaultConfig()` 与模板 `defaultConfigContent`）。

### 2.6 `normalizeStatus` 的 default → idle 是开放式契约

**绝不把 `router.go` 的 `normalizeStatus` 里的 `default:` 改成 panic、报错或拒绝注册。**

```go
default: // "standby", "waiting_process", "waiting_song", 以及未来任何未知值
    return "idle"
```
（`server/router.go` 的 `normalizeStatus`）

**为什么**：这是 fail-safe。新播放器发任何拼错或新造的状态，只会退化成 idle——不抢占、不阻塞优先组——而不是让 router 崩。它是「播放器的状态枚举可以不严格」这一前提的唯一支撑。

**破坏它会怎样**：`player/` 下一个拼错的状态字符串会带崩整个采集进程，直播中歌词与状态全部断供。注释里那句「以及未来任何未知值」是契约，不是描述。

### 2.7 缓存更新与广播必须解耦：先无条件 `UpdatePlayerState`，再做路由决策

**`router.go` 的 `Run` 循环里 `r.server.UpdatePlayerState(evt)` 必须无条件跑在 `r.updateRouting(evt)` 之前，绝不因「这个播放器不活跃/不会广播」而跳过。**

**为什么**：`server.go` 的 `buildInitEvents` 从 `playerStates` 缓存里组装 all_lyrics 等初始事件。不回写缓存，切换或中途连入的前端就会拿到一首歌**开头**的进度，歌词跳位。这也是 per-player API（`/cloudmusicv3/ws` 等）在播放器非活跃时仍能返回正确数据的前提。

**破坏它会怎样**：「反正不广播，省一次写」的优化会让后连入的 OBS 源整首歌歌词错位，且不自愈。

---

## 3. 硬规则：禁止触碰

> **图例**：✅ = 有自动门禁挡着（注明是哪个测试）；❌ = 只靠人读到这一行。诚实标注，别把 ❌ 写成 ✅。
> 四道门禁：`gofmt -l` / `go build` / `go vet ./...` / `go test ./...`。门禁全绿的含义有边界，见 3.8。

### 3.0 元规则：授权型措辞必须带边界

**凡写「可以直接 X」「应当加上 Y」这类授权型措辞，必须同时给出边界与自检命令；给不出就不要写。** ❌

本仓库最危险的文档条款不是过时的事实，而是**以规范语气给出的许可**。过时的事实只让人白跑一趟；许可会让人照着它做出破坏性改动，而且沿途前提都是真的、推理都是顺的，不会有任何异常提示。instruction.md 被剔除的三条都是这个形状（许可用 windows 包 / 许可注入内存 / 许可 windowsgui），这是文档腐烂的典型形状。

**推论：规矩不在被读到的地方，等于不存在。** instruction.md 共 604 行，§8.3 明确写了 NewCallback 的规矩（规矩本身见 3.6），wesing 也照做了样板，但 6 月新增的 park 包依然违反了它——park 是新写的，而那份文档从 5 月起没人再打开。**新增规矩优先考虑做成门禁，做不成再写字。**

---

### 3.1 路由与优先级

- **`router.Run()` 必须无条件先调 `srv.UpdatePlayerState(evt)`，再做任何路由决策。** ❌ — 缓存更新与「是否广播」必须解耦：`buildInitEvents` 用 `ps.Position` 组装 all_lyrics，不回写会让中途连入的前端拿到一首歌开头的进度，歌词跳位。 — `server/router.go` 的 `Run`
- **优先组（prior）的评估绝不加任何门控条件。** ❌ — K 歌一开唱必须秒切主输出，任何延迟或条件化都是直播事故。`evaluatePriorGroup` 与 `watchExpire` 里的 prior 路径都是无条件的——**不要以「对称化」为名重构掉**。 — `server/router.go` 的 `updateRouting`（`switched = r.evaluatePriorGroup()`）、`evaluatePriorGroup`、`watchExpire`
- **普通组的 `priorGroupBlocking()` 门控绝不删。两组不对称是刻意的。** ❌ — 普通组要过门、优先组不过门，正是「K 歌盖住一切」的实现。 — `server/router.go` 的 `priorGroupBlocking` 及其四个调用点（`updateRouting` 两处、`watchExpire` 两处）
- **优先播放器暂停后的 holding 只能由 `prior-player-expire` 超时释放。** ❌ — 这是「唱歌中途暂停不要被网易云抢走画面」的唯一机制。注意整套超时（含普通组）被 `if r.cfg.PriorPlayerExpire > 0` 一把总闸门控——**设为 0 会静默关闭全部超时，holding 永不释放**。 — `server/router.go` 的 `watchExpire`
- **优先组释放时必须强制清空所有普通组 holding；两条释放路径都要做。** ❌ — 少做一处，K 歌唱完释放的瞬间，一个几十分钟前暂停的网易云会被判为 holding 抢到主输出，推送陈旧歌词。 — `server/router.go` 的 `evaluatePriorGroup`（全员 inert 分支）、`watchExpire`（优先组组级超时分支）
- **普通组全员 inert 时，必须向根订阅者推 `player_switch(to="")` **和** `player_clear` 两条。** ❌ — 前端既定契约：前者触发切换动画，后者触发清屏。缺一条会留残影。

### 3.2 播放器契约

- **`normalizeStatus` 的 default 分支必须归 idle，绝不改成 panic / 报错 / 拒绝注册。** ❌ — 这是 fail-safe 的开放式契约：新播放器发任何拼错或新造的状态只会退化成 idle（不抢占、不阻塞优先组），而不是让 router 崩。它是「状态枚举可以不严格」的前提。 — `server/router.go` 的 `normalizeStatus`
- **事件 `Data` 一律传结构体指针**（nil 载荷除外）。 ❌ — `router.go` 用 `evt.Data.(*player.StatusInfo)` 做断言。传值类型**不报错、不 panic、vet 抓不到**，只让断言 ok=false 被静默跳过：该播放器永远不参与路由。
- **新播放器必须把 `PlayerName` 追加进 `playerNames` 切片。** ❌ — 这是真开关：Banner、`server.NewServer`、NewRouter 建表、端点表（含 `/<player>/audio-ws`）、config 回显全由它派生。**漏了不会报错**，表现为播放器一切正常（有日志、有事件）但永远切不成 activePlayer。`config.RegisterPlayer` 的 init() 自动注册会给人「注册是自动的」错觉——这个字面量切片恰恰是手动的。**音频捕获不由它派生**：`audiocap/procscan` 的 `Players` / `PlayerProcess` 是另一张手写表，漏了同样静默（§5.1）。 — `main.go` 的 `playerNames := []string{...}`（按符号找）
- **`Name()` 必须与配置键前缀逐字一致。** ❌ — 拼错会静默退回全局 offset。目录名不必等于 Name()（`cloudmusic` → `"cloudmusicv3"`）。
- **seek 与恢复在同一 tick 同时发生时，只允许发一次 `playback_resume`。** ❌ — 参考实现：seek 分支 Emit 后置 `seeked = true`，状态变化分支以 `if !seeked` 守卫。违反 → 下游重复重置歌词时钟 → 直播中歌词跳字/闪烁。 — `player/cloudmusic/cloudmusic.go` 的 `runSession`（`seeked := false` 声明、seek 分支的 `seeked = true`、状态变化分支的 `if !seeked`）
- **通道的丢弃/阻塞策略绝不对调。** ❌ — 承重不变量：fan-in 阻塞（`merged`，`router.go` 的 `Run`）之所以永不停滞，唯一前提是消费路径上对订阅者通道**一律 `select default` 非阻塞发送**。把任何一处改成阻塞发送，一个卡住的 WS 客户端就会阻塞 Run → `merged` 塞满 → 各播放器 `EventCh`（128，`player/player.go` 的 `NewBaseEmitter`）塞满 → `Emit` 开始静默丢弃（`player.go` 的 `Emit`），**整条流水线停摆且无任何日志**。要保证送达必须换机制（per-sub 有界队列 + 主动断开慢客户端），**不能把丢弃改成阻塞**。

### 3.3 配置

- **CLI flag 覆盖必须靠 `flag.Visit`（只对显式传入的 flag 赋值），绝不直接读 flag 变量。** ❌ — 直接读会让未传的 flag 以零值覆盖 config.yml，**静默清空用户配置**。这是三层优先级成立的唯一原因。 — `config/config.go` 中 `Load()` 的 `flag.Visit`
- **`PlayerConfig` 的覆盖字段必须保持指针（`Offset`/`Poll`/`IdleHide` 为 `*int`、`AudioNormalize` 为 `*bool`），绝不改成值类型。** ❌ — `wesing-offset: 0` 是合法且有意义的配置（模板里 `# wesing-offset: 0` 那行就是这个示例），`<player>-audio-normalize: false` 同理是合法覆盖。改成值类型后零值与「未设置 = 跟随全局」不可区分，wesing 会静默继承全局 200ms 偏移。CLI 赋值必须取局部拷贝地址，直接取指针会让所有播放器共享同一地址。 — `config/config.go` 的 `PlayerConfig`
- **运行时取 per-player 值必须走 `GetPlayerOffset` / `GetPlayerPoll` / `GetPlayerIdleHide` / `GetPlayerAudioNormalize`，绝不直读 `cfg.Players[x].Offset` 这类字段。** ❌ — 未在 config.yml 设置的播放器其 PlayerConfig 是空壳，覆盖字段均为 nil，直接解引用即 panic。 — `config/config.go` 的 `GetPlayer*`
- **新增配置项必须同时改三处：Config 加字段、`mergeYAML` 加分支、模板加注释行。** ❌ — 前两处缺一即**静默失效**：yaml struct tag 是**装饰性的**，解码走 `map[string]interface{}` 再由 `mergeYAML` 手工逐字段搬运，Config 结构体从不被 yaml 直接解码，只加字段和 tag 编译通过、无警告、值永远是默认值。模板是手写 const，缺了它用户看不到这个键。**按需另有四处，漏了同样不报错**：非零默认值进 `DefaultConfig()`（全局键与模板不一致由 `config/templatedefaults_test.go` 拦，见「三个静默陷阱」之三）；要 CLI 覆盖就注册 flag 并在 `flag.Visit` 里加分支（全局 flag 另加进 `globalFlags`，它只影响 `-h` 的分组顺序）；有边界就写钳制函数并在 `Load()` 返回前调用（调用点无门禁，见下文两条钳制），钳制或拒收前用 `noteWritten` 记下写下的原值，否则 `/service-status` 的 `config` 会把运行值当写下值回显；要在 `/service-status` 回显就在 `main.go` 的 `buildConfigEcho` 里加一行（同时进 `config`、`config_effective` 与 `config_overwritten`，`configecho_test.go` 钉着键序），并同步 `doc/openapi.yaml` 与 `doc/API_RESPONSE_EXAMPLES.md` 的 config 示例。现有配置键全部在回显内。 — `config/config.go` 的 `Config` / `mergeYAML` / `defaultConfigContent`（按符号找）
- **新增 `mergeYAML` 分支时，类型断言失败必须 `log.Warn` 出键名与实际类型。** ❌ — 早期分支（`addr`/`offset`/`poll`/`prior-player`/`prior-player-expire` 与每播放器的 `-offset`/`-poll`）内层是无 else 的类型断言：`offset: 200.0`（float64）或 `offset: "200"`（string）会被**静默丢弃**，不报错、不记日志，用户看不出配置没生效。此后新增的分支（如 `per-player-idle-hide`）与已补上的 `cloudmusicv3-effect-strategy` 已带 else 告警，照它们写。 — `config/config.go` 的 `mergeYAML`
- **改播放器默认偏移必须同时改模板和 `DefaultConfig()` 的回退值。** ❌ — 两个真源值不一致：`DefaultConfig().Players` 是空 map，缺键回退全局 **200**；模板写的是 cloudmusicv3=500。用户注释掉某行、或从旧版 config.yml 升级，**静默变 200**——全新安装与升级安装的偏移差 300ms。（注：`# wesing-offset: 0` 注释掉、`cloudmusicv3-offset: 500` 不注释，这个区分是刻意的，别把值塞进 DefaultConfig 抹平。） — `config/config.go` 的模板 `defaultConfigContent`（`wesing-offset` / `cloudmusicv3-offset` 两行）
- **改轮询边界只改 `pollMin`/`pollMax` 两个常量，全局与 per-player 都必须走 `clampPolls`。** ✅ **部分门禁**：`config/clamppoll_test.go` **只覆盖 `clampPolls` 自身**（钳制逻辑、nil 跳过、不碰 Offset）；**`Load()` 里那一句 `clampPolls(&cfg)` 的调用点无门禁 —— 删掉它测试全绿**（实测），而那等于把本条整条回退到原缺陷。原因是 `Load()` 往全局 `flag.CommandLine` 注册 flag，二次调用 panic（`flag redefined: poll`），测试没法反复调它。改这块时**调用点靠人守**。 — 钳位 [10,2000] 现在对全局 `cfg.Poll` **与每个 per-player 覆盖**同时生效（`clampPolls` 在 `Load()` 返回前跑）；钳制前的原值经 `noteWritten` 记入 `cfg.WrittenValues`，只供 `/service-status` 的 `config` 回显写下的值，运行值不变（✅ `config/writtenvalues_test.go`）。**注意这条边界是「安全下限」不是「调参下限」**：它只挡 `poll <= 0` 那种忙等（`time.Sleep(<=0)` 立即返回 → 主循环空转；wesing 还会每 33 圈自旋一次全进程表快照），各播放器自身另有更高的下限（qqmusic `<30ms→50ms`、cloudmusic `<50ms→100ms`、sodamusic `<200ms→200ms`，即 `minPollInterval`），各自只在本包导出的 `EffectivePollInterval` 定义一次，轮询循环与 `main.go` 的 `buildConfigEcho`（经 `playerPollIntervals` 表）共用，都在 pollMin 之上、不冲突。**别删播放器侧那些守卫**：kugou 压根没有（`kugou.go` 的 `runSession` 里 `pollInterval` 直通 `time.Sleep`，clampPolls 是它唯一的保护）；qqmusic/cloudmusic/sodamusic 的在 poll∈[10,30)/[10,50)/[10,200) 时仍会触发；wesing 的 `pollMs<1→30`（`wesing.go` 的 `pollLyrics` 开头）是 `1000/pollMs` 的**除零守卫**，不是 poll 自保，钳后虽不可达也必须留着。 — `config/config.go` 的 `clampPolls` / `pollMin` / `pollMax`（**按符号找，别按行号**：本条原写 `:211`，被同一轮 diff 打漂 70 行）
- **改音量归一化边界只改 `audioNormTargetMin/Max`、`audioNormMaxGainMin/Max` 四个常量，全局 target / max-gain 必须走 `clampAudioNormalize`。** ✅ **部分门禁**：`config/audionorm_test.go` 覆盖 clamp 自身、`mergeYAML` 对全局与 per-player 键的解析、`GetPlayerAudioNormalize` 的回退；`Load()` 里的调用点删掉测试全绿（实测），原因同上，**调用点靠人守**。per-player 覆盖只是开关，不参与钳制。越界原值同样记入 `cfg.WrittenValues`（`config/writtenvalues_test.go`）。 — `config/config.go` 的 `clampAudioNormalize`

### 3.4 服务层与 wire

- **`HTTPResponse.Data` 绝不返回 null；无数据时必须显式写 `Data: struct{}{}`。** ❌ — 「Data 永远不是 null」是前端免判空的硬前提，四个 handler 的空数据分支全靠这条纪律撑着。写成 `Data: nil` 就会给 OBS 前端喂 null。 — `server/server.go` 的 `handleAllLyrics` / `handleLyricUpdate` / `handleStatusUpdate` / `handleSongInfo`
- **HTTP handler 必须锁内取快照、锁外序列化；绝不在持锁期间 `writeJSON`。** ✅`server/race_test.go` — 四个读端点曾在锁外读 `PlayerState`，构成真实数据竞争。 — commit `0f83220`
- **SSE 的类型过滤必须在 `routes` 表里用 `sseTypes` 声明出来。** ❌ — `eventTypes` 为空 = 全通。新增 SSE 端点漏填不会报错，只会静默变成全量推送。**过滤是声明出来的，不是默认的。** — `server/server.go` 的 `subscriber.eventTypes`、`matchesType`、`routeDef.sseTypes`
- **根订阅者只收 activePlayer 的事件；per-player 订阅者绝不受 `activePlayer` 与 `skipRoot` 影响。** ❌ — 根路径 = 单一「当前该播什么」的视图，破坏它 → OBS 根源收到多播放器歌词交错 → 直播串词。per-player 命名空间是「我就要盯这个播放器」的逃生舱，误把 switchSkip 作用上去会让 `/cloudmusicv3/ws` 莫名丢事件。
- **`/audio-ws` 只收 active 缓存那一家的帧（无 active 时收 `player=""` 空闲帧），`/<player>/audio-ws` 只比名字、与 active 无关；某播放器有订阅（`HasSubscribers`）而 `noCaptureAfter`（300ms）内未转发过它的真帧时，两者都收 `player=<名>` 的空闲帧，N=0 即无捕获，前端两端点一套代码。** ✅ `server/audio_test.go`（只测 hub 路由；钩子接线 ❌，见 §2.1；空闲帧由 `Manager.idleKeepalive` 产生，root 空闲帧的内容（`player=""`、`flags=0`、`epoch` 与 rms / peak 为 0、QPC 毫秒时间戳、v1 头）由 `TestIdleKeepaliveFrameV1` 守，按播放器空闲帧的内容（`player=<名>`，其余同上）由 `TestIdleKeepalivePlayerFrame` 守，`noCaptureAfter` 内转发过即不推由 `TestIdleKeepaliveSkipsForwarded` 守，demux 转发即写 `lastForward`、下发基线不写由 `TestDemuxRecordsForward` 与 `TestApplyControls` 守（以上 ✅，Windows-only）；判定 `idlePlayers` 由 `gating_test.go` 的 `TestIdlePlayers` 守（✅，Linux 可跑）；去掉按播放器分支或 root 分支、判定忽略 `hasSub`、阈值比较写反或边界改为大于等于即推、改读含下发基线的 `lastFrame`、demux 不写或下发也写 `lastForward`，均红；改读 `lastFrame` 只由 `TestIdleKeepaliveSkipsForwarded` 拦住，因该用例只写 `lastForward`；`reapHelper` 或 `teardownHelper` 清空 `lastForward` 全绿（以上实测；推断其影响仅为 helper 退出后空闲帧至多提前 300ms 出现）；helper 真帧与保活帧的 QPC 毫秒时间戳由 `TestEncodeFrameTimestampQPC` 守（✅，Windows-only），改用墙钟、不经 `qpcFrequency` 换算或去掉其包级初始化即红；两条时间戳测试都以 `readQPC` 与系统 QPC 频率为参照，`readQPC` 退化为常量、或 helper 包级初始化 / 空闲帧取用的频率写死为与本机相同的值时拦不住（实测）；`Run` 中 `go m.idleKeepalive()` 的启动接线 ❌，删掉测试全绿，实测）
- **`audiocap/frame` 的线格式同时是 `/audio-ws` 的对外 wire 契约，改它等于改公开 API。** ✅ **部分门禁**：`audiocap/frame/frame_test.go` 的 `TestRoundTripAllFields` 按 spec §3 逐偏移钉住线上字节，magic、version、headerLen、flags bit0 的取值以字面量钉住，`TestForwardCompatExtraHeader` 钉住向前兼容；改字段顺序、改上述任一常量的值或不跳过未知字段即红（实测）；同步 `doc/openapi.yaml` 与前端靠人。 — helper → Manager 的 stdout 帧与推给 OBS 的 WS 二进制消息是**同一格式**：`Manager.demux` 解出帧后用同一个 `frame.Encode` 重编码转发。改字段顺序、Magic 或长度语义，前端静默解错、不报错。兼容的新增只追加在定长头末尾并增大 `headerLen`，不兼容的改动才升 `version`。字节布局以 `frame.Encode` / `frame.Decoder` 的实现为准；对外文档只有 `doc/openapi.yaml` 的 `/audio-ws` 一处，`frame.go` 包注释与之同改，并通知前端。归一化开启时 RMS、peak 与 bins 可能 >1（peak 不截断，未开归一化时也可能略超 1）、后端不钳；采样中的 NaN 与 ±Inf 由 helper 的 `window.add` 按 0 计入，不会使这些值成为 NaN 或无穷大；二者也是契约的一部分。
- **绝不为了「修安全」去掉 `upgrader.CheckOrigin` 的恒真。** ❌ — 注入脚本跑在 `orpheus://` 协议的源上，恢复 gorilla 默认 `checkSameOrigin` 会比较 `orpheus` vs `127.0.0.1:8765` → 403 → **ingest 与 OBS 浏览器源可能一起断**。这条 CheckOrigin 对两条正路都是承重的。要收紧只能在 `handleEffectIngest` 里前置鉴权或用专用 upgrader，不能就地改这个共用的。（现状：写端已在 `handleEffectIngest` 里前置 token 校验收紧，未动共用 upgrader。） — `server/server.go` 的 `NewServer` 里那个 `upgrader`（被 `/ws`、`/<player>/ws`、`/audio-ws`、`/<player>/audio-ws`、effect-ws、effect-ingest 共用）
- **effect-ingest 写端已加 token 鉴权；其余现状仍是有意接受的，改之前先读边界。** ❌ — 写端要求进程内随机 token（`handleEffectIngest` Upgrade 前常量时间比较、不匹配 403），挡住本机其它软件伪造帧；但**仍无 Origin 校验、无回环检查，默认绑 `0.0.0.0:8765`**（写端安全靠 token，读端 effect-ws 不带 token）。`/service-status` 已不再广告 ingest 地址。`effect.go` 注释称「单一生产者（注入脚本只开一条），故升级后不做并发保护」——**这是假设，零机制保证；token 通过后的第二条 ingest 连接进来时行为未定义**。 — `server/server.go`、`server/effect.go`、`config/config.go`（`DefaultConfig()` 与模板中的 `addr`）

### 3.5 自动更新与发版

- **版本判据只认 `tag_name`；`name` 只用于 -force 判定。写反就是线上误降级。** ✅ **部分门禁**：`updatedecision_test.go` 的 `TestDecideUpdate` **只守 `decideUpdate` 函数内部**——内部拿 `releaseName` 当版本比、或拿 tag 判 `-force`，均当场红（变异实测）；**接线无门禁**——`checkAndUpdate` 中的调用点把 `release.TagName` 与 `release.Name` 传反、或把 `releaseInfo` 两个字段的 json 标签对调，根包测试全绿（变异实测）。改这块时**调用点与字段映射靠人守**。 — 按完整 semver 比较（x/mod/semver）。 — `main.go` 的 `releaseInfo.TagName` 与 `checkAndUpdate` 中的 `decideUpdate(Version, release.TagName, release.Name)` 调用
- **`Version == "0.0.0"` 或非 semver 格式的构建必须跳过更新检查；绝不给 dev 构建打上 semver 版本号。** ✅ **部分门禁**：判据本身有守卫——`TestIsReleaseVersion`（`0.0.0` 与非 semver 开发版号均须判 false）、`TestDecideUpdate` 的 `invalid current` 用例（删掉 `decideUpdate` 开头对当前版本的 `isReleaseVersion` 校验即红，变异实测）；**`checkAndUpdate` 开头的 `if !isReleaseVersion(Version)` 早退无门禁**，删掉它根包测试全绿（变异实测）——此时 `decideUpdate` 仍对开发版返回 error、不会更新，但开发构建会照常联网做版本检查；**「绝不给 dev 构建打上 semver 版本号」是 CI 侧约束（`build-windows.yml` 的 `Build Windows executable` 步骤），无门禁**。 — 这是开发版自更新的 kill switch：本地 `go build` 与 CI dev 构建都不会被网关的正式版覆盖掉。 — `main.go` 的 `Version` 默认值、`isReleaseVersion` 与 `checkAndUpdate` 开头的早退
- **强制降级只认 Release 标题 `name` 以 `-force` 结尾；`-force` 绝不写进 `tag_name`。** ❌ — 这条禁令有具体后果：`semver.IsValid("v3.0.0-force")` **为真**——force 是合法的 prerelease 标识符，不报错，而是被当成 v3.0.0 的**预发布版**参与比较、排序低于正式版，静默制造反向升级。 — `main.go` 的 `isForceReleaseName`、`decideUpdate`
- **打 tag 前必须在 Windows 开发机上跑完 §8 的四道门禁与 `go test -race ./...`，全绿才打。** ❌（靠人，无门禁）— CI 不跑任何测试（无 `pull_request` 触发器），且 `server`、`telemetry`、`player/` 大部分包是 Windows-only，Linux runner 接上门禁也测不到它们；包括两条 Guard 门禁在内的全部测试只能在开发机上跑。发版前这一遍是它们唯一必经的检查点，省掉它就等于整套测试从未存在。`-race` 需本机有 C 编译器（cgo）。
- **目标 Release 必须已发布（非 Draft），且绝不勾选 GitHub pre-release**，即使 tag 带 -alpha/-beta/-rc。 ❌ — `/releases/latest` 语义上排除 draft 与 pre-release，勾了就等于该版本对客户端不存在。CI 产出的两个 Release 都是 draft，**必须人工发布**。 — `.github/workflows/release.yml` 的 `Create Release Draft (本仓库)`（`draft: true`）与 `Create Release Draft (dotcom mirror)`（`gh release create --draft`）两个步骤
- **发版必须发布 dotcom（VTB-LINK）那个 draft。** ❌ — client-version 与两个 CDN **全部指向 dotcom 的 release**，vlink.dev 不对外暴露。漏发 = 版本号出去了但下载 404。vlink.dev 的 draft 是内部留档。清缓存由镜像过去的 workflow 在 dotcom 侧触发（`.github` 被 sync-source-to-dotcom 全量镜像，无排除）。 — `.github/workflows/release.yml` 的 job 级 `env.DOTCOM_OWNER: VTB-LINK` 与 `Create Release Draft (dotcom mirror)` 步骤
- **更新下载 SHA256 校验失败必须删除损坏文件并终止更新流程。** ❌ — updater 会用下载物替换自身 exe，放过一个损坏或被篡改的二进制 = 把客户端刷成砖。注意仅当 digest 非空才校验，依赖网关下发。 — `main.go` 的 `downloadFile`
- **dev 构建刻意不用 canonical 名，不要把它「修」成 canonical。** ❌ — 那会让开发构建带上被自动更新识别的身份，破坏「开发构建跳过更新」的既有设计。 — `main.go` 的 `canonicalExeName`
- **每日在线心跳只发请求，绝不解析响应、绝不做更新动作。** ✅ `heartbeatinert_test.go` — 心跳与启动时的版本检查打的是同一个端点、组装的是同一个请求，响应体里就是完整的 `releaseInfo`。「顺手解析一下、有新版就更新」离得只有几行远且看起来完全合理，后果是**直播进行到一半进程替换自身 exe 并重启**。自动更新只允许发生在启动时，那时 OBS 还没在拉流。门禁只查直接调用，跨函数的间接路径它看不见。 — `main.go` 的 `sendHeartbeat`
- **客户端标识走请求头，绝不拼进 query。** ❌ — proxy-cache 的默认缓存键是「方法 + URI + query」，**不含请求头**；标识拼进 query 会让缓存键按客户端打散，缓存等于没开。⚠️ **这是面向将来的约束，不是对现状的描述**：2026-08-10 实测这条 route 上**没有** proxy-cache（连发三次条条带 `X-Kong-Upstream-Latency`、无 `X-Cache-Status`、无 `Age`；`Via: kong/3.4.1.0-enterprise-edition`）。§12 那条 purge workflow 的存在很容易让人推断出「这里挂着缓存」——**那是推论不是事实**，且它本身还是 disabled 的。别拿它当缓存存在的证据（§3.0）。 — `main.go` 的 `newVersionCheckRequest`
- **`clientid` 绝不依赖 `telemetry`。** ✅ 编译器 — 依赖方向是 `telemetry` 的**测试**引用 `clientid`（`telemetry/gatewaynotice_test.go` 拿 `clientid.HeaderNames()` 核对隐私提示）。反过来加一条 `clientid → telemetry` 就会成环，那条门禁直接编译不出来。系统信息经 `telemetry.OSSummary()` 由 `main` 取好再传进 `clientid.Env`，就是为了保持这个方向。
- **心跳判「今天报过没有」用本地日历日，不是 24h ticker。** ✅ `pingdate_test.go` — ticker 走单调时钟，机器休眠期间是否推进依系统而定；一台每天休眠十小时的播控机会让 24 小时周期不断后漂，漂满一天就整天不上报。而这种漏报**只表现为 DAU 曲线偏低**，无报错、无日志，不会有人去查。
- **新增版本检查请求头必须同时改隐私提示。** ✅ `telemetry/gatewaynotice_test.go` — 登记在 `clientid.HeaderNames()` 的每一项都要在 `noticeUpdateSection` 里有对应那句话，反向也查（登记了却不再发的要清掉）。这是 `privacynotice_test.go` 那条门禁在版本检查这条通路上的同位物。
- 请求头清单见 `clientid/headers.go` 的 `HeaderNames()`，标识粒度的取舍见 `clientid` 包注释。网关侧算 DAU/MAU 与启动次数的口径原记在 `doc/gateway-client-metrics.md`，该文件已于 `5d22d80` 删除，原文见 git 历史；**它是下游描述，与代码分叉时以代码为准。**
- **`doc/openapi.yaml` 是 API 文档的唯一真源；线上 apifox 是它的手动导入产物（下游副本，不是权威）。** ❌ — 加端点必须先改 openapi.yaml。（此前记的两处缺口——player enum 缺 kugou、effect 端点未进文档——已在 issue #43 补齐。）
- **`RELEASE_BODY.md` 与 `README.md` 写下的每一句能力 / 版本描述，都必须反映当下代码——新增或修改时逐句回代码核，打 tag 发版前再通篇对一遍。** ❌（靠人守，无门禁）— **过时是这两份文书的头号敌人，也是最难自查的一种错：动手的人往往照脑子里的旧印象写，而代码早已变了。本条就是为防这个而立的。** 它们没有 `tools/docsample` 那样的门禁盯着，只能靠动手时逐句核代码，不能凭记忆。实例：3.0 rc 阶段 `RELEASE_BODY.md` 仍停在 `beta.14`（落后十几个 tag，「逐字仅网易云」早已不成立），`README.md` 功能特性写着「各播放器支持逐字/翻译」「输出音译」「支持所有语言」——全是旧印象，而代码是逐字/翻译三家有、wesing 无，音译（KRC `type=0` 罗马音）被**显式丢弃**，UTF-8 也不该当卖点吹。**文风等同 `doc/`：严肃、准确、简洁，不写营销话术（「极大提升」「完美」「强大」「毫秒级」）或 AI 腔的形容词堆砌；能力按播放器如实列，别用「各播放器」抹平差异。**

### 3.6 Windows 机制

- **`NewCallback` 只能出现在包级 `var` 上，且回调必须无捕获。** ✅`player/callback_lint_test.go`（AST 门禁，全仓强制，覆盖 `audiocap/`；已覆盖包级 FuncLit、别名 import、`NewCallbackCDecl` 三种漏报形状） — 全局池上限 2000，超限是 `throw`，**不可 recover，整进程死**，且注册的回调永不回收。runtime 按 funcval 指针去重，**只有静态 funcval 才命中去重**：函数内 `NewCallback(func(){捕获局部变量})` 每次调用都分配新 funcval，代码文本相同也不去重，每次真占一个槽位。`windows.NewCallback`（x/sys）与 `syscall.NewCallback` 共用同一个池。范本：`player/wesing/proc/memory.go` 的 `enumWindowsCallback` / `getPlayStateCallback`；park 已修（`player/cloudmusic/park/park.go` 的 `findMainCallback` / `debugListCallback`，commit `20edf97`/`ef3d11d`）；COM vtable 形态见 `audiocap/wasapi/wasapi_windows.go` 的四个 `completionHandler*` 包级 var（per-instance 状态经 `this` 指针取，不靠闭包捕获）。这条规矩曾长期只以文字形式存在而未被遵守，见 3.0。
- **回调结果只能经包级变量 + 包级 Mutex 回传。** ✅ 同上（门禁管位置，Mutex 靠人） — Mutex 不是可选项：回调只能靠包级变量回传（lParam 未用），包级变量就必须串行化。
- **音频捕获与 DSP 只许在 `--audio-capture-helper` 子进程里跑，且 helper 的 stdout 只许写帧、一切诊断走 stderr。** ❌ — WASAPI/COM 回调的错误跨 C/COM 边界，Go 的 recover 接不到、只能硬崩；放在子进程里，崩的是 helper（由 Manager 退避重启）。主服务侧 `Manager.demux` 完整解帧取 `player` 路由，再用同一个 `frame.Encode` 重编码转发，不做音频处理、不在主进程里激活 WASAPI。控制走 stdin、帧走 stdout 私有管道，**不复用 effect-ingest 回环 WS、不开端口**，不继承那组开放边界（§14）。stdout 就是帧通道：`frame.Decoder` 按 Magic 重对齐、坏帧只丢不崩（✅ `audiocap/frame/frame_test.go`）是兜底，不是许可；`helper.Run` 起手 `log.SetOutput(os.Stderr)`，Manager 以 `cmd.Stderr = os.Stderr` 把 helper 日志并入主控制台（§0.5）。同一 exe 的全部包级 `init()` 都在短路之前执行——**`init()` 里绝不写 stdout**。 — `audiocap/helper/helper_windows.go` 的 `Run`；`audiocap/manager/manager_windows.go` 的 `spawnHelper` / `demux`
- **helper 必须赋入 `KILL_ON_JOB_CLOSE` 的 Job Object，job 句柄只由 Manager 持有。** ❌ — 主服务的退出路径（信号处理的 `os.Exit(0)`、Guard re-panic、被任务管理器结束）都不跑 defer，Windows 也不随父进程回收子进程。父进程退出时 helper 收到 stdin EOF，至多等 `shutdownGrace` 让采集 goroutine 收尾，超时即 `os.Exit`。helper **卡死**（控制 goroutine 自身阻塞、走不到上述退出，或整个进程无响应）且父进程已退出、Manager 无从调用 `Process.Kill` 时，Job 是唯一能强杀它的机制。`Manager.sweepOrphans` 启动时另清杀遗留 helper（前提见 §1.2）。建 Job 或赋入失败时 `job == 0`，`killHelperForRestart`、`teardownHelper`、`reapHelper` 退回 `Process.Kill`——**别当死代码删**：teardown 缺了它，卡死的 helper 会让 `<-hp.exited` 永久阻塞 `Run`（`313b961`）。`reapHelper` 不假定进程已死（`demux` panic 恢复后也会 `close(hp.exited)`），删其 `Process.Kill` 即红（实测）。 — `audiocap/manager/jobobject_windows.go`；`manager_windows.go` 的 `spawnHelper` / `teardownHelper` / `reapHelper`；`helper_windows.go` 的 `Run`
- **helper 重启保持「spawn 失败或自行退出后指数退避（1s 起翻倍）+ 累计 5 次快速失败即全局放弃」。** 快速失败的来源：spawn 失败、启动后不足 10s 自行退出（含 `demux` panic 恢复后的回收）。存活满 10s 的 helper 结束时（自行退出、被看门狗杀或按需拆除）即计数清零、退避从 1s 重新起算；看门狗所杀的 helper 一律不抬退避、不推迟重启，存活不足 10s 时既不计数也不清零——每次 kill 至少有一路超时、已逐播放器记账，这几路的重试由各自 `retryAt` 控制；同一 helper 内未超时的路（只有基线、尚未出真帧）不记账，随新 helper 立即重新下发。自行崩溃序列因此是 1/2/4/8s 后全局放弃；仅当序列由一个存活满 10s 后**自行退出**的 helper 开头时，多一档 16s（`reapHelper` 的稳定分支清零后仍 `bumpBackoff`，看门狗所杀或按需拆除的只清零、不抬退避）；`maxQuickFails = 5` 时 `backoffCap`（30s）不可达，只作上限保护。✅ 部分门禁：`manager_windows_test.go`——删 `reapHelper` 里的 `m.givenUp = true` 或 `!hp.watchdogKill` 判定、看门狗路径恢复 `bumpBackoff`、删 `teardownHelper` 的稳定复位，均红；`noteSpawnFailure` 里的 `m.givenUp = true` 删掉全绿（spawn 失败路径无用例），均实测。**Windows-only，CI 跑不了**（§8）。已知缺口：使 helper 崩溃的单路故障（如 COM 回调硬崩）无法归因到播放器，仍走全局计数。改成无限重启 = 启动即崩的 helper 每秒 spawn 一次、与主线抢 CPU。 — `manager_windows.go` 的 `reapHelper` / `teardownHelper` / `noteSpawnFailure` / `bumpBackoff`
- **帧活性看门狗对每个已下发的 player 判定、不看播放状态，阈值 `hangThreshold`（2s）必须远高于 keepalive 节拍（暂停或静音时约 100ms 一帧）。** 超时的 player 逐个记失败，按播放器退避 1/2/4/8s 重试，连续 5 次放弃该播放器、直到其进程重启，跑满 10s 清账。另有一路在本次下发后出帧（基线不算）即证明 helper 存活，只 `stop` 超时的路；否则判 helper 假死、kill 重启（不下发 stop、不退避，见上条）。✅ 把基线算作存活证据、改放弃阈值、去掉 PID 变化清账或跑稳清账、未跑稳也清账、不停该路、kill 路径也下发 stop、记账挪到 `stopCapture` 之后、删 `reconcile` 里的 `filterWant` 调用或把传给它的时刻改为零值，均红（`gating_test.go` Linux 可跑；`TestCheckWatchdog*` 与 reconcile 级用例 `TestReconcileFilterWant` Windows-only，后者把 sodamusic 的进程名临时改为本测试进程独有的名字、把测试二进制复制为该名字启动，以进入真实进程快照且不与真实播放器同名；实测）。阈值压到接近 keepalive 节拍 = 静音段被误判、捕获被反复停或 helper 被反复杀。 — `gating.go` 的 `judgeWatchdog` / `recordCaptureFailure` / `filterWant`；`manager_windows.go` 的 `checkWatchdog`
- **捕获必须保持按需：无任何 `/audio-ws` 订阅即拆 helper，不扫描（启动时 `sweepOrphans` 的一次进程枚举除外）、不捕获；root 订阅下的预热只覆盖「正在播放」的播放器。** ❌ — 预热是为了消切歌空窗，不是常开。常开 = 直播全程对主播的声卡驱动做持续 loopback，而持续捕获与声卡驱动蓝屏的关联至今未排除（§14 未决 3）。「为了首帧更快」改成常开、或预热全部在跑的播放器，都属于这一条。 — `Manager.reconcile`、`desiredCaptures`
- **绝不把 `uintptr` 转回 `unsafe.Pointer` 去取地址；要算偏移就在切片上算。** ❌ — 潜伏 UAF：GC 不认识 uintptr，对象可能已被移动/回收。当前全仓代码中 `unsafe.Pointer(uintptr` **零命中**（唯一的文本命中是 `player/sodamusic/watchdog/watchdog.go` 里说明为何避开它的注释），保持它。 — `player/qqmusic/mem.go`、`player/kugou/watchdog/watchdog.go`
- **读 VS_FIXEDFILEINFO 必须先校验 `Signature == 0xFEEF04BD`。** ❌ — 不校验就是拿垃圾内存当版本号。 — `player/kugou/watchdog/watchdog.go` 的 `readExeVersion`、`player/qqmusic/mem.go` 的 `getFileVersion`、`player/cloudmusic/watchdog/version.go` 的 `ReadExeVersion`
- **绝不恢复 `InjectSliderAOB()`。** ❌ — 见 issue #39。它与同文件的 `InjectProgressAOB`（调用点已删、本体仍在）是全项目仅有的两个写外部进程内存的实现（装 codecave + 打 E9 跳转），是杀软重点盯的注入签名，而其产出 `SliderVal` 至今零消费方。实现体原样保留在 `player/qqmusic/mem.go` 的 `InjectSliderAOB`，调用已在 `player/qqmusic/qqmusic.go` 的 `runSession` 开头注释关闭。**恢复的前置条件是先证明有消费方**，不是「照模板补全」。
- **22.31 起各宽字符版本（现为 22.31/22.41/22.52/22.60/22.71，同一套模型）的 `SongIDDurCheckOff` 交叉核对绝不能删——它不是多余的校验。** ❌ — 这些版本的数字 songID 都不在 now-playing 显示对象里，而在另一处「播放会话」结构（`knownVersions` 对应条目的 `SongIDOff`，按符号找）。这两处**在换歌瞬间会短暂不同步**：显示对象先更新（切歌检测因此触发），会话结构还留着上一首的 songID。`ReadAllMetadata` 用会话结构自带的时长与显示时长精确核对，不一致即把 songId 归 0，交给下面的补取重试。删掉这步 = 换歌瞬间拿**上一首**的 songID 去请求 → **整首推错歌词，比空白更糟且不自愈**。稳态下两处时长恒等（CE 多次连读实测），核对不会误杀。
- **22.31 起各宽字符版本换歌后的歌词补取（`lyricsPending`）绝不能退回「等不到就认账」。** ❌ — songID（会话结构）与 songMid（堆上报 JSON）**都滞后显示对象数秒**，而显示对象先更新、先触发切歌检测。原逻辑等 500ms 拿不到就认领 lastName 并按「无歌词」收场，可认领后不再进换歌分支 → **永不重试 → 整首空白**（真机实测约 80% 的切歌命中此路径）。现行：立即认领并发标题（overlay 不停在上一首），随后在 `lyricRetryWindow` 内每 `lyricRetryInterval` 重试，songID / songMid 谁先落位用谁，取到即中途替换 all_lyrics。**`FindSongMid` 是全内存扫描**，故只在 songID 缺位时才跑且受节流约束——**别把它挪进 poll 热路径**。
- **22.31 起各宽字符版本的三条反直觉事实，别照 22.16/22.22 的样子「补全」。** ❌ —（宽字符切换发生在 22.22→22.31 之间，此后各版同一套模型，`knownVersions` 各条注释均记为字段布局一致、仅静态地址迁移）① 歌名/歌手是 UTF-16 `WCHAR*` 不是窄 SSO（GBK 字节全内存零命中、UTF-16 多命中，CE 实测），故 `UseWideStrings=true`；按窄 SSO 去读会把指针字节当文本。② 客户端上报的 `songid` **恒为 0**（QQ 改用 songmid 做主键），别据此断定「没有 songID」——真 ID 在会话结构里，见上两条。③ `FastTimerPtr` 留 0 是对的不是漏填：22.16/22.22 要另找快速计时器，是因为它们结构体内的 `ProgressOff` 数秒一跳；22.31/22.41 的 `ProgressOff` 实测亚秒级高分辨率（快速连读每次都变），本地时钟插值以它为锚已足够，补一个 FastTimer 只是多一层可断的间接；此后各版同样不设 FastTimer。
- **网易云 canvas 严格只读，绝不改尺寸。** ❌ — 改尺寸会让网易云崩溃。
- **网易云 watchdog 的保活标记只能用 `--disable-backgrounding-occluded-windows`。** ❌ — 换成 `--remote-debugging-port=9222` 会漏判「有调试口但没保活参数」的实例，让它逃过重启 → 窗口被遮挡时渲染器降帧 → 特效镜像掉帧；换成 `--disable-features=CalculateNativeWinOcclusion` 更糟，那是**网易云子进程自带的**，会误判成已注入。 — `player/cloudmusic/watchdog/process.go`
- **注册表注入仅当自启项已存在时才修补，不代为创建。** ❌ — 代为创建 = 替用户改开机启动，越权。 — `player/cloudmusic/watchdog/registry.go`
- **Win11 下 park 一律强制降级为 fadeout，别「修」掉这个门。** ❌ — Win11 的 DWM 停止合成不可见窗口，park 屏外保活在 Win11 上失效。 — `main.go` 的 `main()` 中 `park.IsWindows11()` 判定处

### 3.7 错误处理与构建

- **`player/` 下绝不 panic。** ❌ — 内存读取/CDP 失败 → 跳过本轮、下轮重试。实证：递归 grep `panic(` 于 `player/`，非测试代码**零命中**（命中只在 `player/krc` 的两个 `_test.go`）。轮询循环跑在直播全程，一次 panic 带崩整个采集进程 = 歌词/状态直接断供。这不是风格偏好，是运行时生存约束。
- **解密/解析失败必须区分「不是密文」与「是密文但坏了」，绝不整坨吞掉。** ✅`player/qqmusic/qrcdecrypt_test.go` — QRC 曾把解密失败吞掉，密文被当歌词推上 OBS。现行判据：hex 解不开 = 不是密文 → 放行；3DES/zlib 失败 = 是密文但坏了 → 报错。 — `player/qqmusic/api.go` 的 `decryptIfNeeded`，commit `b4d9fda`/`6446f82`
- **合法性判定写成接受式，绝不用德摩根反写。** ✅`player/wesing/lyric/plausibleplaytime_test.go` — 反写的拒绝式让 **NaN 通过校验**成为播放时间地址（`NaN <= 0` 为 false）。现行 `IsPlausiblePlayTime(v) = v >= 0 && v < 100000` 是共享判定，已贴到 30ms 热路径。 — `player/wesing/lyric/timer.go` 的 `IsPlausiblePlayTime`；调用点：`player/wesing/wesing.go` 的 `runSession`（初始播放时间）、`initSong`（复用缓存地址前）、`pollLyrics`（轮询热路径），以及 `lyric` 包内的 `validateTimeAddr` 与 `LoadLyrics`，commit `6667a01`/`b4f530e`
- **绝不给构建加 `-H windowsgui`。** ❌ — 本程序是控制台应用，全部日志经 `log.Printf` 走 stderr **且不落盘**（`logger/logger.go` 的五个级别方法全部直调标准库 `log.Printf`，无文件 sink）。改 GUI 子系统 = **日志全部消失、直播中零可观测性**。该标志从未进入过任何构建脚本（两条流水线 ldflags 只有 `-X`：`release.yml` 与 `build-windows.yml` 的 `Build Windows executable` 步骤），是 instruction.md 凭空发明的。真要无窗口运行，前置条件是先给 logger 加文件 sink。
- **`config/`、`logger/`、`i18n/` 必须保持 `GOOS=linux` 可构建。** ❌ — 这三个包恰是 `tools/genconfig` 的**全部**内部依赖（`config` 与 `logger` 都 import `i18n`），而两条打包流水线在 Linux runner 上**原生**跑它生成随包 config.yml（两个 workflow 的 `Prepare files for packaging` 步骤，`GOOS= GOARCH=` 是刻意清空 job 级的 windows/amd64）。`config/`、`logger/` 引入 x/sys/windows、syscall 或任何 `_windows.go` 专有符号会同时炸掉两条流水线，且失败点在「Prepare files for packaging」而非 build——排查时第一反应会去看构建配置，不会去看 config 包的 import。`i18n/` 的 Windows 专有代码（语言探测）只能放在带 `windows` 构建约束的文件里（`detect_windows.go`），且 `!windows` 侧必须有同签名的桩（`detect_other.go`），漏掉任一项同样炸。
  自检：`GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./i18n/... ./tools/genconfig`
  **这是 CI 的实现细节，不是平台目标**——本项目是纯 Windows，`main.go`、`server/`、`player/` 各包该用 windows API 就用，不要为跨平台做抽象。
- **`.gitattributes` 里那段注释是防回退的制度性注释，别删。** ❌ — 没有全局 `eol=lf`，开发机的 `core.autocrlf=true` 会让 `gofmt -l` 把全部 .go 误报成格式不对，真违规藏在噪音里 → gofmt 长期无法作为门禁。`*.syso binary` 同理：被当文本做行尾转换会直接损坏，链出来的 exe 图标/版本信息全废。

### 3.8 门禁的盲区：vet 绿不等于什么

这一小节记录门禁的已知盲区。**别把 `go vet ./...` 绿当成安全信号。**

- **`lostcancel` 看不穿 defer 闭包与 helper 函数，只认对变量的直接调用。** ❌ — 实测：把 `coverCancel()` 包在 defer 闭包里做 nil 检查再调用，**仍被报为泄漏**。反过来说明它只做局部的语法匹配。因此 **kugou 的三条早退路径必须保持显式调用**（`player/kugou/kugou.go` 的 `runSession` 中 `<-p.StopCh`、`client.IsClosed()`、`GetPlayInfo` 失败三处 `return` 之前；另切歌分支在 `context.WithCancel` 新建 ctx 前先取消上一首）——保持显式，vet 才能抓住将来新增的 return。`var coverCancel` 声明下方的注释就是为这条守的。sodamusic 的 `runSession` 照同一写法，同样保持显式。
- **vet 绿 ≠ context 无泄漏。** ❌ — cloudmusic / qqmusic / wesing 三家的封面 goroutine **不带 context**（非测试代码里 `context.WithCancel` 只有 kugou 与 sodamusic 的 `runSession` 两处）。`lostcancel` **结构性看不见它们**：没有 CancelFunc 可丢，分析器就无从下手。这三家的迟到回写改由代次守卫拦截（`BaseEmitter` 的 `NewSongGen` / `EmitForGen`，`18f6cfd`），vet 同样看不见某处是否漏接。 — `player/wesing/wesing.go`、`player/cloudmusic/cloudmusic.go`、`player/qqmusic/qqmusic.go` 各自 `runSession` 中捕获 `coverGen := p.NewSongGen()` 的那段 `go func`
- **vet 根本不检查 `NewCallback` 计数。** ❌→✅ — 那条只能靠 `player/callback_lint_test.go`。这正是它被做成 AST 门禁而不是写成一段文字的原因，见 3.0。
- **`gofmt -l` 不检查 import 分组。** ❌ — 绿 ≠ 分组对。`main.go` 是既有例外，不必整改。
- **vet 不检查 map 迭代序，而 Go 的 map 迭代序是随机的。** ❌ — **本仓库已踩三次**：`config` 的 `clampPolls`（`range` 配置 map 决定 clamp 顺序）、`server` 的 `evaluateGroup`（`range` 状态 map 决定谁当选活跃播放器）、`telemetry` 的 `collectAppInfo`（`range ExplicitKeys` 决定上报里 overwritten 的顺序）。三次都是同一个形状：**map 迭代的结果流到了对外可见的地方**。
  判据：`range` 一个 map，其产物若影响**输出顺序、选举结果、或任何被比对的东西**，就必须显式排序（`sort.Strings` / `sort.SliceStable`），或者换成有序结构。
  这类错误极难自然发现——小 map 的随机起点让它在开发机上常常「看着是对的」，测试跑一次也多半绿。**测它得跑多次采样**（`telemetry/appconfig_test.go` 用 14 个 key × 100 次），或者干脆用 AST/评审兜。
  ⚠️ 反例别误伤：`range` map 只为**聚合**（求和、计数、找最大值）时顺序无关，不必排序。上面三条的共同点是顺序**逃出了**那个循环。

**已知未修，不要当成新发现，也不要顺手改**：

- wesing 的封面 goroutine 在会话结束时不作废代次。换歌半边已由 `18f6cfd` 的代次守卫修复（三家均在 spawn 时捕获 `NewSongGen()`、回写走 `EmitForGen`），cloudmusic / qqmusic 的会话结束半边由 `eb47c86` 修复（`Start` 在会话结束发 `EventClearSongData` 前调 `InvalidateSongGen`）；wesing 的 `Start` 至今没有这一步，进程退出后迟到的封面回写仍可能复活已清空的 SongInfo。同一 goroutine 在 `proc.CloseProc(handle)` 之后仍可能拿该 handle 调 `FindCoverURL` 扫内存（`eb47c86` 的 message 记为 use-after-close），同样未修。

---

## 4. 规矩的守卫状况

本节是本文档的诚实性检查，也是路线图。

规矩不在被读到的地方就等于不存在（见 §3.0）。**结论：能变成测试的规矩，就不要只写成散文。** 下表右边每个 ❌ 都是「下一条该变成门禁的规矩」。

### 已有自动门禁 ✅

| 规矩 | 守卫 | 是否做过变异验证 |
|---|---|---|
| NewCallback 只能在包级 `var` | `player/callback_lint_test.go`（AST 扫全仓） | ✅ 塞入违规立刻红；已覆盖包级 FuncLit、别名 import、`NewCallbackCDecl` 三种漏报形状；覆盖 `audiocap/`（往 `Capturer.Close` 体内塞一句即红，实测） |
| HTTP handler 必须锁内取快照 | `server/race_test.go` | ✅ 回退旧写法必现 panic |
| QRC 两类失败必须区分 | `player/qqmusic/qrcdecrypt_test.go` | ✅ 去掉区分逻辑立刻红 |
| 播放时间校验必须用接受式（NaN 不得通过） | `player/wesing/lyric/plausibleplaytime_test.go` | ✅ 12 个非 NaN 值上两种写法等价，唯 NaN 相反 |
| 版本探测的指针算术 | `player/qqmusic/getfileversion_test.go`、`player/kugou/watchdog/readexeversion_test.go` | ✅ 变异（`&buf[off]` → `&buf[off+4]`）必红 |
| 行尾一律 LF | `.gitattributes` + `gofmt -l` | ✅ |
| `telemetry.Guard` 只能 `defer` 直接调用 | `telemetry/guarddirect_test.go`（AST 扫全仓） | ✅ 包一层闭包立刻红；门禁自带零命中自检（扫不到任何调用点也红）。**只查形状、不查位置**——位置由 `goroutineguard_test.go` 管，见 §1.2 |
| 出货代码每个 goroutine 入口都挂 Guard | `telemetry/goroutineguard_test.go`（AST 扫根包、`server/`、`player/`） | ✅ 删掉封面 goroutine 或被调方法首句的 Guard、新加裸 `go func(){}()`、`go` 外部包函数，均红；自带零命中自检。**已知盲区**：同包方法只按名字匹配、不做类型检查，见该文件头 |
| 更新决策的版本判据 | `updatedecision_test.go` | ✅ 删掉 `isReleaseVersion` 的 `v0.0.0` 特判、`decideUpdate` 内部拿 `releaseName` 当版本比，均红。**只守函数内部**：`checkAndUpdate` 的调用点传参与早退无门禁，见 §3.5 |
| Sentry 的 DSN 注入符号名与代码一致 | `telemetry/ldflagssymbol_test.go` | ✅ 改名 `dsn`、或改 workflow 的 `-X` 符号，任一侧漂移即红 |
| Sentry 的 ClientOptions 接线（含配额闸门） | `telemetry/initwiring_test.go` | ✅ 7 个字段逐个变异必红（读 sentry 实际生效的 `Client().Options()`，非我方副本） |
| 系统版本必须读免疫兼容层的 `RtlGetNtVersionNumbers` | `telemetry/compatshim_test.go` | ✅ 两个 API 可注入，四种 shim 形状必红。**`RtlGetVersion` 受兼容层影响，`park.IsWindows11()` 正建立在它之上——那是条现存缺陷线索，见该文件头** |
| 时区必须用非本地化的 `TimeZoneKeyName` | `telemetry/timezonekey_test.go` | ⚠️ **有条件**：靠「值含非 ASCII」判断，中文机上有效，**英文机器上抓不到**（两个字段恰好相同），CI 跑不了 |
| 每日在线心跳绝不做更新动作 | `heartbeatinert_test.go`（AST 扫心跳函数体） | ✅ 心跳里塞一句 `restartSelf()` 立刻红。**只查直接调用**——套一层中间函数它看不见，这是刻意的取舍 |
| 版本检查请求真的带着客户端标注 | `clientwiring_test.go` | ✅ 删掉 `clientid.Apply` 那行、或把 `client.Do(req)` 退回 `client.Get(url)`，两种都红。**这几条专打接线**：clientid 包自己那二十来条用例全部直接调 `Apply`/`derive`，接线断了它们照样绿 |
| 新增请求头必须同时改隐私提示 | `telemetry/gatewaynotice_test.go` | ✅ 往 `HeaderNames()` 加一项而不改文案即红；反向（登记了却不再发）也红 |
| 心跳跨日判据 | `pingdate_test.go` | ✅ 判据改成恒 false 即红；含一条 UTC+8 用例钉死走本地日历日而非 UTC |
| 解帧把 helper 输出当不可信数据（坏字节/超长前缀/越界只丢不 panic，按 Magic 重对齐） | `audiocap/frame/frame_test.go` | ✅ 删掉 Magic 前垃圾的丢弃、或去掉 `MaxPayload` 判定，均红。纯 Go，Linux 可跑 |
| 帧线格式即 `/audio-ws` 对外 wire 契约（逐偏移布局与常量取值、向前兼容） | `audiocap/frame/frame_test.go` 的 `TestRoundTripAllFields`、`TestForwardCompatExtraHeader` | ✅ **部分**：编解码两侧同时对调 `peakL` / `peakR` 偏移、bins 不从 `headerLen` 处读、或改 `Magic` / `Version` / `HeaderLen` / `FlagNormalized` 任一常量的值，均红；同步 openapi 与前端靠人（§3.4）。纯 Go，Linux 可跑 |
| 音频两处丢最旧（helper `enqueue`、audioHub）与 root/per-player 路由 | `audiocap/helper/helper_test.go`、`server/audio_test.go` | ✅ 删掉任一处「丢最旧」即红。只测 hub 自身，钩子接线见 §2.1 |
| 门控预热、看门狗判定、逐播放器失败记账与成功清账、want 过滤、按播放器空闲帧判定；退避 / 放弃 / 拆除复位、看门狗与空闲帧接线 | `audiocap/manager/gating_test.go`、`manager_windows_test.go` | ✅ 去掉 `hasRoot && playing[n]` 即红；其余覆盖边界见 §3.6，空闲帧见 §3.4。后者 **Windows-only**，守 `checkWatchdog` / `reapHelper` / `teardownHelper` / `spawnHelper` 接线、`reconcile` 的 want 过滤与 `idleKeepalive` / `demux` 的空闲帧接线 |
| 归一化只作用于真音频帧、关闭即逐位直通、非有限 RMS 不进 EMA；非有限采样（NaN、±Inf）进入 helper 窗口时按 0 计入；归一化配置解析与钳制 | `audiocap/helper/helper_windows_test.go`、`helper_test.go`、`config/audionorm_test.go` | ✅ 删掉 `encodeFrame` 里的 `nz.Apply`、或删掉 `Normalizer.Apply` 的非有限值守卫，均红；`window.add` 去掉清洗、只清洗 NaN、改为降混后只清洗 batch、只清洗声道 0、或把接受式判定反写为拒绝式，`TestWindow` 与 `TestEncodeFrameNonFiniteSamples` 均红；判定改为严格不等、阈值收窄（[-1, 1] 与 ±1e30 两例）、或把次正规数冲为 0，仅 `TestWindow` 红（就地清洗与先拷贝再清洗两种写法下均红）（以上实测）。`TestEncodeFrameNonFiniteSamples` 为 Windows-only，`TestWindow` 为纯 Go。⚠️ 钳制调用点无守卫（§3.3）；`captureLoop` 对 `win.add` 的调用同样无门禁，接线靠人守（把 `captureLoop` 改为绕过 `add`、自行内联降混与取峰，全仓测试全绿，实测） |
| 模板全局键取值与 `DefaultConfig()` 一致 | `config/templatedefaults_test.go` | ✅ `DefaultConfig()` 的 `AudioNormalize` 改回 `false`、或模板的 `offset` 改为 300，均红（实测）。per-player 键不在覆盖内（「三个静默陷阱」之三）。纯 Go，Linux 可跑 |
| 播放器名 → 进程名五对锁死 | `audiocap/procscan/names_windows_test.go` | ✅ 改一家映射值即红。不比对 `playerNames`（§5.1）。Windows-only |
| 播放器层轮询下限的边界 | `player/{qqmusic,cloudmusic,sodamusic}/pollfloor_test.go` | ✅ 阈值比较改为含等号（qqmusic）、改抬高值（cloudmusic）、去掉下限（sodamusic），均红（实测）。只守 `EffectivePollInterval` 本身：把 qqmusic 轮询循环改回直接换算间隔，该包测试全绿（实测），三家循环的调用点靠人守。Windows-only |
| `/service-status` 的 `config` 回显写下的值、`config_effective` 回显实际生效的值，两表键集合与顺序一致 | `configecho_test.go`、`config/writtenvalues_test.go` | ✅ 组装处写下值改读运行值、去掉播放器层下限、生效策略改读 `cfg.EffectStrategy`、删掉一个键，或钳制 / YAML 侧拒收处不记原值、per-player 不继承全局写下值，均红（实测）。`Load()` 的钳制调用点、CLI 侧效果策略分支的原值记录、`playerPollIntervals` 漏登记新下限，均无门禁 |

### 靠人守 ❌

| 规矩 | 为什么没门禁 | 风险 |
|---|---|---|
| **`config/` + `logger/` + `i18n/` 保持 Linux 可构建** | 无检查、无 PR 门禁（`i18n` 的构建约束只隔离探测代码，不校验可构建性） | 破坏它 = 两条打包流水线同时挂。`logger` 被二十余个文件引用，是最容易有人为了 ANSI 颜色加一句 `SetConsoleMode` 的地方 |
| 日志文案跨播放器一致 | 无法机检 | 实测 `instruction.md` §3.7 规定的连接文案符合率 **0/2** |
| 封面 goroutine 的代次守卫接线 | `player/songgen_test.go` 只测 `BaseEmitter` 的代次判定本身；各播放器是否在 spawn 时捕获代次、回写走 `EmitForGen`、会话结束调 `InvalidateSongGen`，无门禁（另见下「结构性盲区」） | 漏接一处，迟到的封面回写会用**上一首**的标题 + 封面覆盖当前歌曲且整首歌不自愈；wesing 的会话结束半边现仍未接（§3.8） |
| 「不得硬编码状态值」 | 无法机检 | `逐字：否` 在三家播放器是写死的字面量 |
| **主服务侧音频 goroutine 本地 recover、不挂 Guard** | `goroutineguard_test.go` 只拦「豁免写坏」（包闭包即红），拦不住「改挂 Guard」；`audiocap/` 不在其扫描范围（§1.2） | 任一波形 panic 经 re-panic 杀掉直播主进程 |
| **`SetActivePlayer` 的音频钩子非阻塞、不回调 Router、不 panic** | 接线零测试（§2.1） | 路由主循环停摆，或同 goroutine 重入死锁 |
| **`--audio-capture-helper` 短路留在 `main()` 最前** | 无 | 见 §1.2 ★ 表 |
| **新增播放器同步 `procscan.Players` / `PlayerProcess` 并真机验出声进程在树内** | 无跨表比对；出声只能真机验 | 该播放器永远没有波形，不报错 |

### 门禁的结构性盲区（**绿 ≠ 安全**）

详见 §3.8，此处只列结论——不知道这三条就会把绿灯当成安全：

1. **`go vet` 的 `lostcancel` 看不穿任何间接层**（defer 闭包、helper 函数均不认），只认对变量的直接调用。所以 kugou 的早退路径必须保持**显式**调用，vet 才能抓住将来新增的 return。
2. **`go vet` 对「压根不带 context 的 `go func`」结构性失明。** 没有 `CancelFunc` 可丢，分析器就无从下手。vet 绿不代表这类问题清了。
3. **`go vet` 不检查 NewCallback 计数**（只能靠 `player/callback_lint_test.go`），**`gofmt` 不检查 import 分组**。

---

## 5. 怎么加东西

### 5.1 加播放器

**照抄一个现成的**。四种参考实现，按目标播放器的形态挑最近的那个：

| 模式 | 参考 | 取数手段 |
|---|---|---|
| 内存只读扫描 | `player/wesing/` | `OpenProcess` + `ReadProcessMemory`（`proc/memory.go`），PE 导出表定位 vtable |
| CDP + React fiber 遍历 | `player/cloudmusic/` | 注入 JS 遍历 fiber 树（`cdp/client.go` 的 `ForceFetchLyricsInRedux`） |
| 内存偏移表 | `player/qqmusic/` | 按 exe 版本查偏移表（`mem.go` 的 `knownVersions`，现覆盖 `20.05`/`21.81`/`22.16`/`22.22`/`22.31`/`22.41`/`22.52`/`22.60`/`22.71`）。**取词主键分三种，别假定统一**：`21.81`/`22.16`/`22.22`（窄 SSO）只用数字 songID；`20.05` 用 songMid（`SongMidParamsOff`/`StreamURLOff`，从结构体内 URL 串解析）；`22.31` 起各版（宽字符，同一套模型）主用会话结构里的 songID（`SongIDOff` + `SongIDDurCheckOff` 核对），songMid 仅作兜底（`SongMidFromHeap` → `FindSongMid` 扫堆 JSON）。未知版本按版本号距离回退到最近的已知版本（`nearestKnownVersion`，`9a0aa76` 起；读不到版本号时退到 `latestKnownVersion`），这是赌，读出的多半是垃圾——见 `nearestKnownVersion` 注释 |
| CDP + 二进制补丁 | `player/kugou/` | patch `libcef.dll` 开 CDP（`watchdog/`）+ 提权 helper |

**必须实现 `player.Player` 四方法：`Name`/`Start`/`Stop`/`Events`。嵌入 `BaseEmitter` 后只需自行写 `Start()`。**
`router.Run()` 只认这个接口，对每个 Register 进来的 player 直接 `range p.Events()`。 — `player/player.go` 的 `Player` 接口与 `BaseEmitter`（`NewBaseEmitter`、`Name`/`Events`/`Stop`/`Emit`）
✅ 编译期强制（不实现则 `router.Register` 传参不过）

#### 两个静默失败点

**① `Event.Data` 必须传结构体指针（nil 载荷除外）。**
`router.go` 的 `updateRouting` 做的是 `evt.Data.(*player.StatusInfo)`。传值类型**不会编译报错、不会 panic、`go vet` 抓不到**，只让断言 `ok=false` 被整个 `if` 静默跳过 —— 表现为播放器一切正常（有日志、有事件、per-player 端点有数据）但**永远切不成 activePlayer**。
❌ 无门禁。当前 `player/` 下 33 处 `Emit(player.EventStatusUpdate, ...)` 全部是 `&player.StatusInfo{...}`，纪律无破口，别打破它。

**② `main.go` 的 `playerNames` 字面量是真开关，必须手动追加。**
```go
playerNames := []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName, kugou.PlayerName, sodamusic.PlayerName}
```
Banner、`server.NewServer`、`NewRouter` 的 normalStates 建表、端点表、config 回显、`PlayerSupport` 全由它派生。**漏了不报错**：`updateRouting` 里的 `ns := r.normalStates[evt.PlayerName]` 取到 nil，被紧随的 `if ns != nil` 静默跳过。
**认知陷阱**：`config.RegisterPlayer` 在 init() 里自动注册（见 5.2）会给人「注册是自动的」的错觉 —— 那是**另一个注册表**，只管 per-player 配置键。**音频捕获还有第三张**：`audiocap/procscan/names_windows.go` 的 `Players` 与 `PlayerProcess`（播放器名 → 目标进程名）。漏了它同样不报错：`/<name>/audio-ws` 照样注册却永远没有帧；该播放器当上 activePlayer 时 root `/audio-ws` 整段无帧（`Manager.reconcile` 与 `idleKeepalive` 的按播放器空闲帧都只遍历 `procscan.Players`，`player=""` 空闲帧只在无 active 时推）。`names_windows_test.go` 锁死的是现有五对，不比对 `playerNames`。**三处都要改，漏一个静默失效。**
❌ 无门禁。

#### 接入步骤

1. 定义 `const PlayerName = "xxx"` + `func init() { config.RegisterPlayer(PlayerName) }`（范本 `player/kugou/kugou.go` 的 `PlayerName` 常量与 `init`）。
   **目录名不必等于 `Name()`**（`player/cloudmusic/` → `"cloudmusicv3"`），但 **`Name()` 必须与配置键前缀逐字一致** —— 拼错则静默退回全局 offset。
2. `New(offset, poll int)`，内部 `NewBaseEmitter(PlayerName)`。
3. `main.go`：import → `xxx.New(cfg.GetPlayerOffset(...), cfg.GetPlayerPoll(...))` → **追加进 `playerNames`** → `router.Register(...)` → `go func() { defer telemetry.Guard(); xp.Start() }()`（理由见 §1.2）。Banner 无需手改。播放器若自带轮询下限，写成本包导出的 `EffectivePollInterval` 供轮询循环调用，并登记进 `main.go` 的 `playerPollIntervals`；漏登记不报错，只是 `/service-status` 的 `config_effective` 显示钳制后而非实际的间隔。
4. `audiocap/procscan/names_windows.go`：`Players` 追加 `PlayerName`，`PlayerProcess` 加「`PlayerName` → `TargetProcessName`」一对（wesing / qqmusic 的常量在播放器包，cloudmusic / kugou / sodamusic 的在各自 watchdog 子包），同步 `names_windows_test.go`。**再真机验证**放歌时 `/<name>/audio-ws` 的 RMS 非零：WASAPI 只抓 `MainPID` 及其子进程树，出声进程不在树内即静默零帧。现有五家的验证见 `docs/audio-waveform-capture-design.md` §9；真机验证须短跑，见本文 §9。

#### 其他硬规则

**状态枚举可以不严格 —— `normalizeStatus` 的 default 分支必须归 idle，绝不改成 panic/报错/拒绝注册。**
这是 fail-safe 的开放式契约：新播放器发任何拼错或新造的状态（如 kugou 实发的 `"error"`，见 `kugou.go` 的 `Start`）只会退化成 idle —— 不抢占、不阻塞优先组 —— 而不是让 router 崩。 — `server/router.go` 的 `normalizeStatus`
✅ 编译期无关，但 default 分支本身即门禁；改掉它才是破坏

**seek 复用 `playback_resume`；seek 与状态恢复同 tick 时只允许发一次。**
范本：seek 分支 Emit 后置 `seeked = true`，状态变化分支以 `if !seeked` 守卫。违反 → 下游重复重置歌词时钟 → 直播中歌词跳字/闪烁。 — `cloudmusic.go` 的 `runSession`（`seeked := false` 声明于 seek 检测之前）
❌ 无门禁。

**`player/` 下绝不 panic。** 内存读取/CDP 失败 → 跳过本轮、下轮重试。轮询循环跑在直播全程，一次 panic 带崩整个采集进程 = 歌词直接断供。
✅ `grep -rn "panic(" player/` 当前只命中 `_test.go`，非测试代码零命中（非自动门禁，但可一行自查）

**`EventPlayerSwitch` / `EventPlayerClear` 不是播放器事件**，由 Server 直接构造、Router 或 Server 自身巡检触发；新播放器不需要、也不应该 Emit 这两个。（`EventPlayerSwitch` 只走根订阅者流；`EventPlayerClear` 除根流外，per-player 通道的无活跃自动隐藏也会单发，见 `notifyPerPlayerIdleClear`。） — `player/player.go` 的事件类型常量块定义，发射在 `server/`

**新播放器的用户可见文案要中英双语，详见 §6.4。** 最低限度：状态 `Detail` 非歌名态必带 `Reason` 码且 `Detail` 过 `i18n.T`；错误用 `i18n.Errorf`（不是 `fmt.Errorf`）；新日志把中文 format 串补进 `i18n/messages*.go` 的 `english` 表（调用点不改，logger 边界已过 `i18n.T`）；注入 `%s` 的中文值（如 detailedFlag「是/否」）在源头包 `i18n.T`。 ✅ `i18n/placeholders_test.go`（校验占位符），❌ 覆盖漏译靠人。

---

### 5.2 加配置项

**三层优先级：命令行 > config.yml > 内置默认。**

**CLI 覆盖必须靠 `flag.Visit`（只对显式传入的 flag 赋值），绝不直接读 flag 变量。**
直接读会让未传的 flag 以零值覆盖 config.yml，**静默清空用户配置**。这是三层优先级成立的唯一原因。 — `config/config.go` 中 `Load()` 的 `flag.Visit` 回调
❌ 无门禁。

**`PlayerConfig` 的覆盖字段必须保持指针（`Offset`/`Poll`/`IdleHide` 为 `*int`、`AudioNormalize` 为 `*bool`），绝不改成值类型。**
`wesing-offset: 0`、`<player>-audio-normalize: false` 都是合法且有意义的覆盖（模板里 `# wesing-offset: 0` 就是示例）。改成值类型后零值与「未设置」不可区分，wesing 会静默继承全局 200ms。
另：CLI 赋值必须取**局部拷贝地址**（`v := *cliPlayers[name].offset; ... = &v`）—— 直接取 `cliPlayers` 指针会让所有播放器共享同一地址。 — `config.go` 的 `PlayerConfig` 与 `flag.Visit` 块
❌ 无门禁。

**运行时取值必走 `GetPlayerOffset` / `GetPlayerPoll` / `GetPlayerIdleHide` / `GetPlayerAudioNormalize`，绝不直读 `cfg.Players[x].Offset` 这类字段。**
未在 config.yml 设置的播放器，其 `PlayerConfig` 是空壳，覆盖字段均为 nil，直接解引用即 panic。 — `config.go` 的 `GetPlayer*`
❌ 无门禁。

**`config.RegisterPlayer(name)` 只登记播放器名；每个 per-player 配置维度都是手写的。**
现有四个维度：`offset` / `poll` / `idle-hide`（`*int`）与 `audio-normalize`（`*bool`）。每加一个要同改六处：`PlayerConfig` 加指针字段（nil = 跟随全局）；`Load()` 的 `playerCLI` 结构与 flag 生成循环各加一项；`flag.Visit` 的 per-player 分支取**局部拷贝地址**赋值；`mergeYAML` 的 per-player 循环加分支（类型断言失败 `log.Warn`）；加 `GetPlayerXxx(name)` 解析器；模板各播放器段补一行注释示例。范本是 `IdleHide` 与 `AudioNormalize`。 — `RegisterPlayer` / `Load` / `mergeYAML`（按符号找）
现存反例：`EffectStrategy`（键 `cloudmusicv3-effect-strategy`）只对网易云一家有意义，没有走上述 per-player 维度，而是作为全局项手写：`yaml:"-"`，在 `Load()` 里单独注册同名 CLI flag，在 `mergeYAML` 里单独写一个分支。单个播放器专属的配置就长这样。
❌ 无门禁。

#### 三个静默陷阱

**陷阱一：yaml struct tag 是装饰性的，不要相信它。**
解码走 `yaml.Unmarshal(data, &m)` 到 `map[string]interface{}`，再由 `mergeYAML()` 手工逐字段搬运（均在 `config.go` 的 `Load()` 中调用）。**Config 结构体从不被 yaml 直接解码**，模板也不是结构体序列化产物（`defaultConfigContent` 是手写 const）。
**新增配置项必须同时改三处**：① Config 加字段；② `mergeYAML` 加一个 `if v, ok := m["key"]; ok` 分支；③ `defaultConfigContent` 模板加行。①② 缺一即**静默失效** —— 编译通过、无警告、值永远是默认值；③ 缺了不影响生效，但用户看不到这个键。按需另有四处，清单见 §3.3。
❌ 无门禁。

**陷阱二：`mergeYAML` 静默吞类型错误。**
早期分支（`addr`/`offset`/`poll`/`prior-player`/`prior-player-expire` 与每播放器的 `-offset`/`-poll`）内层是**无 else** 的类型断言。用户把 `offset: 200` 误写成 `offset: "200"`（string）或 `200.0`（float64），该键被**静默丢弃** —— 不报错、不记日志、`ExplicitKeys` 也不标记，Banner 上看不出配置没生效。`Load()` 里的 `log.Warn("解析 %s 失败: %v", ...)` 只在整份 YAML 语法错时触发，捕不到单字段类型错。此后新增的分支（如 `per-player-idle-hide`）与已补上的 `cloudmusicv3-effect-strategy` 已带 else 告警。
**新增 `mergeYAML` 分支时，类型断言失败必须 `log.Warn` 出键名与实际类型。**
❌ 无门禁。

**陷阱三：`DefaultConfig()` 与模板是两个真源，值不一致。**

| | 全局 | cloudmusicv3 | qqmusic | kugou | sodamusic |
|---|---|---|---|---|---|
| `DefaultConfig()`（`Players` 为空 map） | offset **200** | 回退 200 | 回退 200 | 回退 200 | 回退 200 |
| 模板 `defaultConfigContent` | offset 200 | **500** | **400** | **430** | **340** |

只有「模板生成的 config.yml 原样存在」时用户才拿到 500/400/430/340。用户注释掉某行、或从旧版 config.yml 升级 → **静默变 200** —— 同一份代码在「全新安装」与「升级安装」下播放器偏移差 300ms，老用户拿不到调优值且升级无效。
**改播放器默认偏移必须同时改模板，并评估 `DefaultConfig()` 的回退值。**
全局键 ✅ `config/templatedefaults_test.go`：模板中全局键的取值与 `DefaultConfig()` 不一致即红（把 `DefaultConfig()` 的 `AudioNormalize` 改回 `false`、或把模板的 `offset` 改为 300，均红，实测）。`b613d73` 只在模板中把 `audio-normalize` 改为 `true`，升级用户仍为关，即属此类。per-player 键 ❌：测试二进制不注册播放器，模板中的专属键被跳过，上表的差异仍在。

**另注：per-player poll 现已夹紧**（`clampPolls`，按符号找；此前只夹全局，`-kugou-poll 1` 能拿到未钳制的 1ms 热循环）。**这条边界是安全下限、不是调参下限**——只挡 `poll <= 0` 的忙等，各播放器自身更高的下限（qqmusic 50ms / cloudmusic 100ms / sodamusic 200ms）在其上另行生效，两层不冲突。播放器侧的守卫一个都别删，理由见 §3.3 那条。

---

### 5.3 加端点

**三类路由，能进表就进表：**

| 类 | 位置 | 条数 | 新增播放器时 |
|---|---|---:|---|
| 声明式路由表 `routes` | `Server.Start` 的 `routes := []routeDef` | 8 | **零代码**，`registerRoutes` 对根 + 每个播放器各注册一遍 |
| 仅根路径的内部端点 | `Start()` 里 `// 仅根路径的内部端点` 之下 | 2 | 手写（`/health-check`、`/service-status`） |
| effect 硬编码前缀 | `Start()` 里两行 `/cloudmusicv3/effect-*` | 2 | 手写 |

**新端点优先进 `routes` 表** —— 这正是加 kugou 时不必碰路由、`/<player>/audio-ws` 自动覆盖全部播放器的原因。**但进表只管注册、不管广告**：`/service-status` 的端点表是 `main.go` 里手写的 `endpointsOM`（根段 + per-player 循环），**任何新端点（进表与否）都要在那里补，并同步 `doc/openapi.yaml` 的 `/service-status` 示例**。漏了不报错：端点存在但不出现在 `/service-status` 里（`/audio-ws` 就漏过一次，`0e4ecb0` 补上），或反之广告了不存在的端点。进不了表的，`server.go` 的 `Start()` 里再多一处手写注册。 — `routeDef` 定义见 `server/server.go`

**SSE 的类型过滤必须在 `routes` 表里用 `sseTypes` 声明出来。**
`eventTypes` 为空 = **全通**（`server/server.go` 的 `matchesType`）—— 新增 SSE 端点漏填不会报错，只会静默变成全量推送。过滤是声明出来的，不是默认的。 — 现有两条：`/lyric_update-SSE`、`/song_info-SSE`
❌ 无门禁。

**`HTTPResponse.Data` 绝不返回 null；无数据时必须显式写 `Data: struct{}{}`。**
「Data 永远不是 null」是前端免判空的硬前提，四个 handler 的空数据分支全靠这条纪律撑着（`server.go` 的 `handleAllLyrics` / `handleLyricUpdate` / `handleStatusUpdate` / `handleSongInfo`）。随手写 `Data: nil` 就会给 OBS 前端喂 null。 — 结构体 `server/types.go` 的 `HTTPResponse`
❌ 无门禁。

**`HTTPResponse.Code` 恒为 0，没有错误路径 —— 这是既成事实，别指望它报错。**
`server.go` 内全部 `writeJSON(w, HTTPResponse{...})` 写出点（现 14 处）无一例外都是 `Code: 0, Msg: "success"`。新端点**不要**发明 `Code: 500` 这类约定 —— 下游没人在读它，单方面引入非 0 值只会让老前端把错误当成功。要改成真错误码，先确认下游消费方。
❌ 无门禁。

**新增 HTTP 读端点：锁内取快照、锁外序列化。**
既有四个 handler 的写法是「`RLock` → 组装快照 → `RUnlock` → `writeJSON`」（范本 `server.go` 的 `handleAllLyrics`）。两条纪律：**快照必须在锁内取完**（slice header 与 Title 等字段同一次持锁读出，否则是非原子快照）；**绝不在持锁期间 `writeJSON`**（json.Marshal + 网络写会被慢客户端拖住，持 RLock 写网络 = 一个慢下游阻塞 `UpdatePlayerState`，进而阻塞整个路由器）。
✅ 部分门禁：`server/race_test.go` 的 `TestAllLyricsSnapshotIsAtomic` —— **只覆盖 `handleAllLyrics` 一个 handler**（测试体内硬编码 `s.handleAllLyrics("")`）。新端点不在它的覆盖内，靠人。

#### 改端点必须同步改 `doc/openapi.yaml`

**`doc/openapi.yaml` 是 API 文档的唯一真源。线上 apifox 是它的手动导入产物（下游副本，不是权威）。**
改端点 → 改 `doc/openapi.yaml` → 重新导入 apifox。反过来做（先改 apifox）会让唯一真源失准，且下次导入把改动冲掉。
❌ 无门禁 —— 没有任何测试比对路由表与 openapi.yaml。

**曾记的债已补齐**（issue #43）：`doc/openapi.yaml` 现含 kugou 与 sodamusic —— player enum 五家齐全（`components.parameters.PlayerParam` 的 `enum`），`/service-status` 响应示例的端点表列出各家 per-player 路径（含 audio-ws）与 effect-ws。唯 effect-ingest 未入 openapi：它是内部写端，有意不对外文档化（记在 `doc/API_RESPONSE_EXAMPLES.md` 与 §14）。

---

---

## 6. 日志与文案规范

规范分三层，**别把三层混为一谈**。三层平铺成一张表时，编译期约束和符合率 0/2 的措辞惯例长得一模一样，读者只能整表当教条或整表无视。分层的意义就是让「违反即 bug」与「参考现状」可区分。

### 6.1 强制（违反即 bug）

| 规矩 | 为什么 | 谁在守 |
|---|---|---|
| **运行时代码必须用 `logger` 包，绝不 `fmt.Println` / `log.Printf` 直出状态**。例外见 6.2 末；音频 helper 子进程不走 logger，用标准 `log.Printf` 加 `[helper]` 前缀写 stderr，由 Manager 的 `cmd.Stderr = os.Stderr` 并入主控制台（§3.6）。 | 模块名 + 级别图标是直播中定位故障的唯一线索，裸 print 没有这两样。 | ❌ 靠人 |
| **同一事件在五个播放器里文案必须逐字一致**；不一致必须有**机制上**的理由（见 6.2），「懒得改」不是理由。 | 运营在同一屏日志里横跨五个播放器排查，措辞漂移 = 无法 grep。 | ❌ 靠人 |
| **日志里的状态值必须来自真检测，绝不写死字面量。** | 见下文。 | ❌ 靠人 |
| **一个 package 只有一个 `log` 变量**；同包多文件共用（`player/wesing/lyric/finder.go` 的包级 `var log` 声明，`reader.go`/`timer.go` 共用）。同包确需两个 → 换名（`server` 包的 `serverLog`（server.go）/ `routerLog`（router.go））。 | Go 编译期约束，不是风格偏好——同包重复声明 `log` 直接编译不过。 | ✅ 编译器 |
| **绝不在模块内自行 `log.SetFlags`**。时间戳由 `main()` 开头的 `log.SetFlags` 统一设定。 | 各模块各设 = 同一屏日志出现两种时间格式。 | ❌ 靠人 |

**「逐字：否」是断言，不是报告。**

全仓 `逐字：` 日志现有 **13 处：11 处真检测、2 处仍写死 `否`**。本节初写时是「8 写死、3 真检测」——qqmusic（QRC）、kugou（KRC，`97f60d8`）、wesing（内存 `CharElement`）的逐字先后接入后都改用了真检测，见下方实证。

| 写法 | 位置 |
|---|---|
| ✅ 真检测（`detailedFlag()` / `lyricDetailedFlag()`，遍历 `TextDetailed.Words`） | `player/cloudmusic`（CDP / API / fetch 共 3 处）、`player/qqmusic`（4 处）、`player/kugou`（3 处，`97f60d8` 起）、`player/wesing`（`detailedFlag()`）——**按符号找，行号已漂** |
| ❌ 写死 `否` | `player/cloudmusic/cloudmusic.go` 两条 Redux 分支（同文件里 `lyricDetailedFlag()` 就在手边却没用） |

后果：**就算有人把逐字接通了，日志还是会说「否」**——它不是在报告状态，是在断言状态，接线者会据此以为自己没做成、回去改一个本来就对的实现。

**这条已被三次兑现、且证明有用**：qqmusic、kugou、wesing 接通逐字时，接线者都改用了真检测（qqmusic/kugou 还在代码里留注释显式引用本节「绝不写死——AGENTS.md §6.1」），没踩坑。**尤其 wesing 那处**——本节曾断言它「确无逐字源、尚可辩护」，正是这类写死最危险的形状：把「还没去读」误当成「机制上没有」，反而拦住了接线。实测证明字级时间就在 `CharElement` 里。剩下 2 处写死是 `cloudmusic` 的两条 Redux 分支——同一文件里 `lyricDetailedFlag()` 就在手边却没用，仍是隐患。

**新增任何 `<状态>：<值>` 形态的日志，值必须来自函数调用。** 想不出怎么检测，就把这个字段从日志里删掉：不报好过报错。

### 6.2 例外（机制不同，须写明理由）

**五家的能力不一致。** 逐字五家全有；翻译 cloudmusicv3 / qqmusic / kugou / sodamusic 四家有，只有 wesing 无来源。**这是逐步挖出来的、不是天然分界**：kugou 一度也被归进「都无」（见 §6.1 的写死日志与下文 ①），先后两个 commit 才把它的逐字与翻译从 KRC 挖通。wesing 现在的翻译「无」同样可能只是「还没挖」，别倒果为因（下文 ①②）。以下为代码真源 + WS 录音双证：

| | `sub_text`（翻译） | `text_detailed`（逐字） | 纯音乐 |
|---|---|---|---|
| **wesing** | 无 | **有**（内存直读 `CharElement`，非 HTTP） | **情况不存在** |
| **cloudmusicv3** | **有** | **有**（YRC） | 平台返回「纯音乐，请欣赏」→ 归一为 `index:-1` |
| **qqmusic** | **有** | **有**（QRC） | API 返回零行 → `index:-1` |
| **kugou** | **有**（KRC 内嵌 `[language:]`） | **有**（KRC，`97f60d8` 起） | 同 cloudmusic（两家文案一字不差） |
| **sodamusic** | **有**（tlyric 式独立 LRC，取自 `translations.cn`，按绝对时间戳对齐——非酷狗的行号对齐） | **有**（明文 KRC，与 kugou 同格式） | 无歌词 → `index:-1`（同 cloudmusic/kugou） |

**逐字现在五家全有；翻译四家有（cloudmusicv3 / qqmusic / kugou / sodamusic）、只有 wesing 无来源。** 本节几经订正，两条曾经的「过时」都已翻案：

- **kugou 逐字**（`97f60d8`）：从 `krcs.kugou.com` 拿 `fmt=krc` 解出字级时间轴。**kugou 翻译**（`447686b`）：同一份 KRC 里内嵌的 `[language:]` 标签（base64 JSON，`type=1` 中文翻译轨，按 `[start,dur]` 行号对齐，非网易云那种时间戳匹配）→ 赋进 `SubText`。二者同一次 `fmt=krc` 请求、同一次解密，白搭车。kugou 的逐字/翻译都是**条件性**的：拿不到 KRC、回落到行级 `fmt=lrc` 时 `text_detailed`/`sub_text` 都空，由 `detailedFlag()` 如实反映。
- **wesing 逐字**（内存直读）：**曾断言「机制上不成立」，实测推翻**。全民K歌是卡拉OK，字级时间就在 `CharElement` 里——`+0x04` 字起始秒、`+0x08` 字时长秒（2026-07-18 实测：首字起始 = 行时间，行内严格递增，末字终点≤下一行起点，53/53 行吻合；**跨进程重启一致**，只有模块基址随 ASLR 变、结构体内偏移全不变）。`reader.go` 的 `LoadLyrics` 此前遍历 `CharElement` 只读了文本（`+0x00`→RenderData），这两个时间字段整个略过了——不是「没有逐字源」，是「没去读」。时间轴不合法的行退回行级（`Detailed` 为空 `{}`），由 `detailedFlag()` 如实反映。
- **sodamusic 逐字**（明文 KRC）：字节的 transport 直接给**已解密的明文 KRC**（`lyrics.type==='krc'`，格式与酷狗字级完全同构 `[行起ms,行长ms]<字偏ms,字长ms,0>字`），故复用 `player/krc` 公共包解析、连解密都省。逐字是**条件性**的：`type` 非 krc / 内容为空时按无歌词处理，由 `detailedFlag()` 如实反映。KRC 解析已抽 `player/krc` 单一真源，kugou 与 sodamusic 共用（`ParsePlainKRC`）。

**只剩 wesing 翻译仍无来源**：`player/wesing/` 包零 HTTP，渲染内存里只有演唱歌词、无翻译轨；它手里的 mid（`songinfo.go` 从内存 JSON 刮的）是**全民K歌曲库 mid，不是 QQ 音乐 songmid**（2026-07-18 实测：拿去 QQ `fcg_query_lyric_new` 返 `retcode=-1901`、`musicu.fcg` 返 `songID=0/qrc=0`），故走不通 QQ 那条翻译源。要上 wesing 翻译得先找到可用的外部翻译源。

#### 「没有」分两种，别混

**① 无来源 —— 还没挖出取词路径**（现只剩 **wesing 的翻译**一项）

> 这一类正在逐条被挖通：kugou 逐字（`97f60d8`，KRC）、kugou 翻译（`447686b`，KRC 内嵌 `[language:]`）、wesing 逐字（内存 `CharElement`）先后挖通——应验了本节稍后仓库所有者那句「将来挖出来了就该补上」。**只剩 wesing 翻译无来源**。

wesing 扫的是内存里**已渲染完成**的歌词：`player/wesing/lyric/reader.go` 的 `LyricLine` 是 `{Index, Time, Text, Detailed}`（`Detailed` 即逐字，`SubText` 尚无——无翻译源）。整个 `player/wesing/` 包**零 HTTP 引用**（实测 grep `net/http` 无命中）——这正是翻译无来源的根：翻译要么来自外部 API（与 zero-HTTP 设计冲突），要么内存里有翻译轨（渲染内存只有演唱词，没有）。**注意「零 HTTP」不再等于「零逐字」**：逐字来自内存字级时间，不需要 HTTP。

**但这只等于「我们目前拿不到」，不等于「平台没有」。** 仓库所有者原话：「逐字和翻译，是因为我们还没挖出来。」将来挖出来了就该补上，那不是破坏一致性，是补齐。

⚠️ **本节此前写的是「这些概念对 wesing 根本不成立」——那是把「没找到」写成了「不存在」**，正是本文档反复警告的那个动作（见 §3.0）。一个未经验证的推论被写进规范，就成了后人不再质疑的前提。

它还与 §6.1 自相矛盾：那里写着「**就算有人把逐字接通了**，日志还是会说『否』」——那句话的前提正是「逐字可以被接通」。同一份文档，两处打架，而没人发现。

（精确一点：wesing 有 mid，`lyric/songinfo.go` 从内存 JSON 里刮出来的，但它只用于 `FindCoverURL` 的内存 AOB 匹配，不用于任何取词请求。别把「有 mid」误读成「能查歌词」。）

**② 情况不存在 —— 领域约束**（wesing 的纯音乐）

wesing 是 K 歌平台，**曲库内所有歌都带词**。所以「纯音乐」这个状态对它不成立，`index:-1` 结构上永远不会发出。这不是实现限制，改不改代码都一样。

（`initSong` 里 `len(lyrics)==0` 直接返回失败、一个事件都不发——那描述的是「假如出现纯音乐会怎样」，而那个前提不成立。别把实现细节当理由写。）

> sodamusic 与 kugou 都出 KRC 逐字、但翻译机制不同：kugou 的译文内嵌在同一份 KRC 的
> `[language:]` 轨里、**按行号对齐**；sodamusic 的译文是独立的 tlyric LRC（`translations.cn`）、
> **按绝对时间戳对齐**（同 cloudmusic 的 `MergeTlyric`）。别把两者的对齐口径搞混
> （见 `player/sodamusic/translation.go` 与 `player/krc`）。
>
> **sodamusic 的时间戳对齐带 ±10ms 容差，不是精确相等——那不是放宽，是补上精度差。**
> 2026-08-08 真机实测：译轨是厘秒制（`[00:26.56]`），而 KRC 行起始是毫秒制，两者精度不同源。
> 《We Are The World》105 行**全部整 10ms 对齐**、精确相等 105/105（放宽后结果一字不变）；
> 但同日《听海》32 行的行起始是 `45519 / 53253 / 60457` 这种毫秒值，**整 10ms 对齐只占 1/32**。
> 也就是说「KRC 可以是毫秒精度」是真实形态，一旦它与厘秒译轨相遇，精确相等会**整轨静默丢译文**
> （无日志、无报错，只是 `sub_text` 全空）。现有样本里有译轨的歌恰好都整 10ms 对齐，
> 那是相关性不是保证。容差取 10 = 一个厘秒格，误配需要两行歌词起始相隔 ≤10ms，不可能。

| | 「无来源」 | 「逐字：否」写死 |
|---|---|---|
| 决定者 | 我们还没挖出那条路径 | 实现偷懒（有检测器不用） |
| 定性 | **合法现状**，但标注为待办 | **bug** |
| 该怎么办 | 写明「目前无来源」，别写成「不可能」 | 修 |

判据：**问「把它做成一致需要什么」。**答案是「换一套数据来源」→ 现状，如实标注。答案是「调一下已有的那个函数」→ bug。

**方括号走私是故意约定，别当 typo 修。** `logger.New("CloudMusic] [CDP")` 这类写法，靠把 `] [` 塞进模块名伪造二级标签，渲染成 `[CloudMusic] [CDP]`，按惯例带 `// 渲染为 [X] [Y]` 注释。logger 只认单个模块名（`logger.go` 的 `Logger` 结构体只有一个 `module` 字段），这是在不改 logger 的前提下拿到层级前缀的唯一办法。

**当前模块名**（均为包级 `var ... = logger.New(...)`，在声明文件内搜 `logger.New` 即得）：

| 模块名 | 声明文件 |
|---|---|
| `Main` | main.go |
| `Config` | config/config.go |
| `Telemetry` | telemetry/telemetry.go |
| `ClientID` | clientid/clientid.go |
| `Server` / `Router` | server/server.go / server/router.go |
| `Cover` | player/cover.go |
| `Wesing` | player/wesing/wesing.go、player/wesing/lyric/finder.go |
| `CloudMusic` | player/cloudmusic/cloudmusic.go、player/cloudmusic/lyric/fetch.go |
| `CloudMusic] [CDP` | player/cloudmusic/cdp/client.go |
| `CloudMusic] [Effect` | player/cloudmusic/effect/effect.go |
| `CloudMusic] [Watchdog` | player/cloudmusic/watchdog/process.go |
| `CloudMusic] [Park` | player/cloudmusic/park/park.go |
| `KuGou` | player/kugou/kugou.go |
| `KuGou] [CDP` | player/kugou/cdp/client.go |
| `KuGou] [Watchdog` | player/kugou/watchdog/watchdog.go |
| `QQMusic` | player/qqmusic/qqmusic.go |
| `Soda` | player/sodamusic/sodamusic.go |
| `Soda] [CDP` | player/sodamusic/cdp/client.go |
| `Soda] [Watchdog` | player/sodamusic/watchdog/watchdog.go |
| `AudioCap] [Manager` | audiocap/manager/manager_windows.go（`managerLog`） |

**级别图标**（`logger.go` 的 `Info`/`Success`/`Warn`/`Error`/`Detail` 五个方法）：`Info [*]` / `Success [✓]` / `Warn [!]` / `Error [✗]` / `Detail [+]`。

**允许 `fmt` 的 7 类例外**：启动 Banner（`main()` 中按 `playerNames` 循环打印的那段）、更新通知框、进度条（`\r` 覆写）、用户交互 prompt、SSE/HTTP 协议输出（`server.go` 的 `handleSSE` 写 `http.ResponseWriter`；`config.go` 的 `flag.Usage` 与 `printFlag` 写 usage 到 stderr）、**`tools/` 下的调试 CLI**（§13.4 所列各子命令：genconfig / winicon / audioprobe / cdpexplore / parktest / watchdogtest / docsample / wsrecord / kugouprobe / qqprobe / qqromaprobe / sodaprobe / wesingprobe 都是一次性命令行工具，不是运行时；devserver 不用 fmt）、**隐私提示**（`telemetry/privacy.go`：给主播读的整块告示 + `\r` 倒计时，套上 `[Telemetry] [*]` 前缀反而会淹没它）。另：`server/dedup.go` 的 `hashEventData` 里的 `fmt.Fprintf` 写的是 hash 而非终端，不属日志。

### 6.3 惯例（参考，非强制）

**这一层不是规矩，是现状快照。** `instruction.md` §3.7 把它当强制，实测符合率 0/2——最典型的是「`<方式>已连接，开始轮询...`」这条格式，**全仓一次都没出现过**，而代码自发收敛出了更准确、且三处逐字一致的措辞：`CDP 连接成功，开始监听播放状态`（cloudmusic / kugou / sodamusic 三家的 `Start`）。CDP 是事件监听不是轮询，**代码比文档准**。把这层当强制，只会让人去改三条本来就对的日志。

新接播放器时**对齐代码现状**，不是对齐这张表：

| 场景 | 现状措辞 | 覆盖 |
|---|---|---|
| 切歌 | `♪ 歌曲: %s` | 五家全有（均在各自的 `runSession` 里），但参数不同：wesing 只有 title，kugou 多带 hash，sodamusic 多带 id |
| 暂停 / 恢复 | `暂停 @ %.2fs` / `恢复 @ %.2fs` | cloudmusic / qqmusic / kugou / sodamusic 四家的 `runSession` |
| seek | `检测到回跳: %.2fs → %.2fs` / `检测到前跳: ...` | cloudmusic / qqmusic / kugou / sodamusic 四家的 `runSession` |
| 歌词加载完成 | `歌词加载完成[(来源)]: %d 行；逐字：<否\|是>` | 五家全有；来源后缀只有 cloudmusic 用（`(CDP)`/`(Redux)`/`(API)`），qqmusic 的补取路径为 `(补取)`，其余无后缀 |
| 进程等待 | `等待 <进程名> 启动...` | wesing.go 的 `waitForProcess`、kugou/watchdog/watchdog.go 的 `waitForKuGou` |

**已知不一致（属 6.1 的「靠人」缺口，不是机制例外）**：wesing 发 `EventPlaybackPause`/`Resume`（wesing.go 的 `pollLyrics` 冻结判定处）却**不打任何 `暂停 @`/`恢复 @` 日志**，也没有前跳检测（`pollLyrics` 里只有回跳检测）。wesing 手里有 playTime，做得到——这是漏的，不是机制决定的。

---

### 6.4 i18n（中英双语）：新增用户可见文案的约定

运行期输出按 **Windows 显示语言**中/英切换：`main` 启动早期 `i18n.SetLanguage(i18n.DetectSystemLanguage())` 一次性设定（主语言是中文即中文、否则英文；**只看显示语言，不看区域/时区**）。中文是源语言，`i18n/messages*.go` 的 `english` 表是「中文原文 → 英文」。**新增任何用户可见的中文都要按下面接线，否则英文系统上那条就漏成中文**（安全回退、但不完整，且不报错）。
✅ 有门禁：`i18n/placeholders_test.go`（逐条校验译文的 `%`-占位符多重集与原文一致，防漏写/错写导致 `%!s(MISSING)`）。

- **① logger 调用（`.Info/.Warn/...`）：只补表，不改调用点。** logger 五方法在边界已对 format 串过 `i18n.T`。新增一条日志 → 把它的中文 format 串**逐字**加进 `english` 表即可（`messages_p4_log.go` 或 `messages.go`），调用点不动。英文语序不同时用带序号的 `%[n]` 重排，由 fmt 处理。
- **② 错误文案：用 `i18n.Errorf`，别用 `fmt.Errorf`。** ❌ — `fmt.Errorf(i18n.T(...), ...)` 过不了 `go vet`（「non-constant format string」）。改用 `i18n.Errorf("中文%w", err)`（`i18n/errorf.go`，写成 vet 认得的 printf wrapper：`format = T(format); fmt.Errorf(format, a...)`，于是 vet 转去校验**调用点常量 format** 的占位符）。`errors.New("中文")` → `errors.New(i18n.T("中文"))`。补对应英文进表。
- **③ 包级 sentinel 错误（`var Err = errors.New("中文")`）：保持中文，别包 i18n.T。** ❌ — 包级 var 在 **init 求值、早于 SetLanguage**，包了会冻结成中文；且它们靠 `errors.Is` 按**身份**比较、文本只是附带。用户可见的呈现要在别处（如 StatusInfo）本地化，不在 sentinel 上。现存 5 个（kugou watchdog 3 + qqmusic 2），别新增会外显的。
- **④ `StatusInfo`：非歌名状态必带 `Reason` 稳定码 + 本地化 `Detail`。** ❌ — playing/paused 时 `Detail`=歌名、无 `Reason`；其余状态一律 `Detail: i18n.T("中文"), Reason: player.ReasonXxx`。`Reason` 是**播放器无关的稳定机器码**（`player.Reason*`，7 个），消费方与 `openapi.yaml` 按它判断，**取代对中文 Detail 的字面量匹配**。没有合适现成码就在 `player/player.go` 加一个并同步补 openapi 的 `reason` 枚举。
- **⑤ 被当数据注入日志的中文值：在源头包 i18n.T。** 翻 format 串对**注入进 `%s` 的中文值**无效——如 `detailedFlag` 返回的「是/否」、组名「优先/普通」、`nonEmpty(x, "未知")` 的回退值。在**返回/传入处**包 `i18n.T` 并补表。
- **⑥ 不该翻译的中文（翻了反而出错）**：用于**匹配/比较**的中文常量（窗口名如「全民K歌」、歌词过滤词「著作权/版权」、平台提示查找集「纯音乐，请欣赏」、歌名后缀「(伴奏)」）是数据不是 UI；注入 OBS 的 JS 里的中文注释；**仅上报 Sentry** 的 dev-only 文案（`telemetry.ReportOnce(...)`、Sentry scope payload）按项目决策不译。
- **⑦ §0.3 边界**：语言探测走 kernel32（`GetUserDefaultUILanguage`），Windows-only，隔离在 `i18n/detect_windows.go`（`!windows` 侧是桩）。`i18n` 包主体纯 Go，故 `logger`/`config` 可安全依赖它、仍 `GOOS=linux` 可构建。**绝不把语言探测塞进 logger/config；locale 只能由 main 注入。**

**自检（❌ 无门禁，靠人）**：新增中文后扫一遍「有没有中文字符串字面量落进用户输出却不在译表」。上一轮用一个一次性 AST 覆盖检查器（遍历出货 `.go`，收集含汉字的 `BasicLit`/字符串相加，减去译表 key）做过；剩下的应全部落在 ⑥ 的有意保留类。漏一条只表现为**英文系统上那一行/那个值仍是中文**，不报错。

---

## 7. Windows 机制

### 7.1 原语（全在 `player/wesing/proc/memory.go`）

| 能力 | 入口 | 注意 |
|---|---|---|
| 进程查找 | `FindProcess(name)` | `CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)` |
| 打开句柄 | `OpenProc(pid)` | 只申请 `PROCESS_VM_READ\|PROCESS_QUERY_INFORMATION`——**只读，别加 VM_WRITE** |
| 模块枚举 | `EnumModules(pid)` | `TH32CS_SNAPMODULE\|SNAPMODULE32`，32 位快照 |
| 内存读取 | `ReadBytes` / `ReadUint32` / `ReadInt16` / `ReadUint16` / `ReadFloat32` / `ReadString` | **没有 UTF-16 读取器**。`ReadString` 只认 null 结尾的 ASCII；宽字符在调用层自解（`lyric/songinfo.go` 的 `utf16LEBytesToString` / `utf16LEBytesToStringUntilNull`） |
| 可写区域枚举 | `EnumWritableRegions` | 只走到 `0x7FFF0000`（32 位用户空间上限） |
| AOB 扫描 | `AOBScan` / `ParseAOBPattern` | 见下方两个坑 |
| 窗口状态 | `GetPlayState(pid)` | 一次 `EnumWindows` 拿全 Phase/标题/最小化/拖动 |

**AOB 的两个坑：**

- **`AOBScan` 的文档注释是错的。** 它写「支持 0xFF 作为通配符」——不是。通配符是 `ParseAOBPattern` 里的 `??`（产出 `mask[i]=false`），`0xFF` 就是字面量 0xFF。别照这句注释写模式串。 ❌ 靠人
- **`AOBScan` 对 >64MB 的区域是整块 `continue` 跳过（`if size > 64*1024*1024`），不是分块读。** 目标落在大区域里 = 静默漏命中，无日志、无错误。扫不到时先怀疑这里。 ❌ 靠人
- `isWritable` 只认 `PAGE_READWRITE` 与 `PAGE_EXECUTE_READWRITE`，**刻意排除** `WRITECOPY` 两档（函数内有注释）。别「补全」它。

**窗口状态检测**：`GetPlayState` 靠窗口标题做三态判定（标题判别在包级回调 `getPlayStateCallback` 里，Phase 赋值在 `GetPlayState` 末尾）——「全民K歌 - xxx」与 `CLyricRenderWnd` 都在 = `PhasePlaying`，只有前者 = `PhaseLoading`，没有前者 = `PhaseStandby`。拖动检测走 `GetGUIThreadInfo` 的 `GUI_INMOVESIZE`（`GetPlayState` 内），用于抑制拖窗时的假冻结。

**「只读」的边界**：wesing 只读扫描、网易云/酷狗走 CDP。**qqmusic 不是只读**——`mem.go` 的 `ConnectQQMusic` 用 `PROCESS_ALL_ACCESS` 开句柄，`WriteProcessMemory` 在 `mem.go` 的 `WriteBytes`，`InjectSliderAOB` 本体在 `mem.go`。它只是**运行时不写**：调用点已因 issue #39 注释关闭（`qqmusic.go` 的 `runSession` 开头）。**绝不恢复它**——产出 `SliderVal` 零消费方，代价是 codecave + E9 跳转，属杀软重点盯的注入签名；补丁写进 QQ 音乐地址空间后**本程序退出不撤销**（全仓无 `VirtualFreeEx`），恢复后做 A/B 必须每轮重启 QQ 音乐本身。理由见 `runSession` 开头那段注释块。

**wesing 的内存内容不保证与当前歌曲同步。** 逆向时实测：**换歌之后，内存里的东西常常还是上一首的**，且严重程度因机器而异（本地数据库状态可能是变量之一）。这不是我们的 bug，是 wesing 自己的行为——**别试图「修」它，要假设它会发生**。

这一条解释了一整族症状，遇到时先想到它：

- **封面经常拿到上一首**（`[!] 封面 URL 获取失败` 或字节数与上一首完全相同）。`FindCoverURL` 靠 mid 做内存 AOB 匹配，匹配到的可能是陈旧的那份。封面 goroutine 的代次守卫（`EmitForGen`，§3.8）只拦换歌后迟到的回写，拦不住这种在当前代次内取到的陈旧封面。
- **播放时间地址选中一个「永远是 0」的实例**（issue #44）。AOB 命中的很可能是**上一次会话/上一首歌的残留结构体**——它字体 `1E`、行高 `2D` 一应俱全，`+0x10` 的指针也有效，**只是不再被更新了**。

**关键推论，动手前必读：陈旧实例与真实例在静态上无法区分。** 它们的每一个字节都合法。所以以下方向全都无效，别浪费时间：

- 往 `validateTimeAddr` 里加更多特征字节 —— 陈旧实例同样满足
- 排除「可疑」的值（比如 0）—— 0 是歌曲开头的合法值，排除它会在最常见的时机拒掉真实例（issue #44 的第一个错误方向）
- 加强 `ptr10` 的指针校验 —— 陈旧实例的指针也是有效的

**唯一能区分的信号是「它会不会动」。** 真实例至多在开头逗留一瞬；陈旧实例永远不变。`wesing.go` 的 `sawProgress` + `exitDeadAddr` 就是这个判据的实现（issue #44）。**任何新的内存读取都该问一句：如果它是陈旧的，我怎么发现？**

### 7.2 NewCallback 必须包级 —— 唯一有自动门禁的一条

**`syscall.NewCallback` / `windows.NewCallback` / `NewCallbackCDecl` 只能出现在包级 `var` 的初始化表达式里。函数体内一律违规。** ✅ **`player/callback_lint_test.go` AST 门禁**

为什么：上限 **2000**（`runtime/zcallback_windows.go` 的 `cb_max`），**永不回收**，超限是 `throw("too many callback functions")`——不可 recover，整进程死。x/sys 与 syscall 共用同一个池。runtime 按 funcval 指针去重，**只有静态 funcval 才命中去重**——函数内新建的闭包每次调用都分配新 funcval，代码文本一样也不去重。

为什么它是门禁而不是一段文字：如上文提到（§3），这条规矩早已写在 instruction.md §8.3，park 包依然违反了。

写法样板：包级 `var xxxCallback = syscall.NewCallback(...)` + 包级变量传参/收结果 + 包级 `sync.Mutex` 串行化。Mutex 不是装饰——回调只能靠包级变量回传（lParam 未用），包级变量就必须串行化。

- 样板：`proc/memory.go` 的 `enumWindowsCallback` + `enumWinMu`（供 `EnumProcessWindows`）、`getPlayStateCallback` + `getPlayStateMu`（供 `GetPlayState`）
- 已修：`park.go` 的 `findMainCallback`、`debugListCallback`——原本在 `findMainAny`/`DebugList` 里逐次新建。真实敞口见 `findMainCallback` 上方的注释：`effect.go` 的 `windowStateLoop` 每 80ms 调 `IsMainMinimized` + `IsMainForeground`，缓存未命中时每 tick 泄漏 2 个（25 个/秒），跨网易云重启累积，约 8~20 次重启后打死进程。
- 门禁覆盖三种曾经漏报的形状（`callback_lint_test.go` 中 `TestNoNewCallbackInsideFunc` 的文档注释）：包级 var 内的 FuncLit、别名 import、`NewCallbackCDecl`。`TestLintCatchesKnownHoles` 钉死它们，**别在重构 scanner 时把洞改回去**。
- `go vet` 不检查 NewCallback 计数，这条只能靠门禁挡（见 §8）。

### 7.3 park：屏外泊车（`player/cloudmusic/park/`）

把网易云主窗移到**虚拟桌面包围盒之外**（park.go 的 `offscreenX()`，下限 30000）但保持 shown 且未遮挡，以维持 Chromium 出帧；用途是最小化后仍能给 OBS 特效镜像供帧。

**三条必须知道的：**

1. **Win11 一律强制降级 fadeout，绝不放行 park。** `main()` 中 `park.IsWindows11()` 那段无条件覆盖 config。理由在 `park.go` 的 `IsWindows11` 文档注释：Win11（尤其 24H2）的 DWM 会停止合成「完全不可见/被遮挡」的 Chromium 窗口——实测一泊车就冻结、不再出新帧，保活失效。判据 `IsWindows11()` = major>10 或 (major==10 && build≥22000)。 ❌ 靠人
2. **崩溃落盘兜底不能省。** `Park()` 在移窗**之前**先 `writeState`，落盘到 exe 同目录 `mbx-cloudmusic-park.json`（`stateFilePath`；取不到 exe 路径时退回 `os.TempDir()`）。`main()` 启动时调 `park.RestoreOrphaned()`——发现遗留泊车状态即刻还原。没有这步，PlayerCap 崩溃一次，主播的网易云就一直停在屏幕外，且无从知道窗口去了哪里。
3. **找主窗必须用 `WINDOWPLACEMENT.rcNormalPosition` 的面积，不能用当前 rect。** 见 `findMainAny` 的文档注释：最小化时当前 rect 是 (-32000)，只有还原态尺寸才能识别出主窗（~1058×752 最大），并避开迷你播放器/桌面歌词/边框小窗。

移窗要 `SetWindowPlacement` + `SetWindowPos` **两步**（`Park()` 末尾的 `setPlacement` 与 `procSetWindowPos.Call`）：前者一步做「取消最小化 + 不激活」，但会把位置**钳到可见工作区**；后者不钳制，真正移到屏外。少一步就泊不出去。

### 7.4 preserveDrawingBuffer shim 的 document-start 注入（`player/cloudmusic/effect/effect.go`）

WebGL canvas 默认 `preserveDrawingBuffer:false`，合成后绘制缓冲被清空，帧外 `drawImage` 只有 **~50% 命中**。修法是注入 `getContext` shim 强制 `preserveDrawingBuffer:true`（effect.go 的 `glShimJS` 常量，`__mbxGLHook` 守卫幂等）。

**为什么必须早于页面加载**：shim 只对**之后创建**的 context 生效。等页面加载完、歌已经开了，context 早建好了，只能靠 `Page.reload` 补救——而 reload 在网易云启动期会把它卡死/中断播放（`purelayerSession` 里「不再在此 reload」那段注释来自实测）。所以 `shimInjectLoop` 在 9222 一可达就 `Page.addScriptToEvaluateOnNewDocument` + 对当前文档 `Runtime.evaluate` 兜底，然后**阻塞读住连接**（循环 `conn.ReadMessage()` 直到出错）以维持 addScript 注册，连接断（网易云重启）后自动重注。

**为什么 `shimInjectLoop` 故意不受 `netEaseUp` 门控** ❌ 靠人：`Run()` 的抓帧主循环与它起的 `pollShowingLoop` 都门控在 `netEaseUp()`（取词 player 的 `IsConnected`），`shimInjectLoop` 却在 `Run()` 开头无条件起飞。这是刻意的，注释在 `shimInjectLoop` 的文档注释（「关键：**不门控在取词 player 的 IsConnected**」）——player 要等 watchdog 重启 + 5s 才连上，那时页面早加载完、甚至已恢复了上一首歌，context 已建、来不及注入。给它加上门控 = 静默退回 50% 命中率，症状是「时灵时不灵」，最难查。**别为了「一致性」把它并进门控。**

`glShimJS` 与 `captureInjectJS` 开头 `if(!window.__mbxGLHook){...}` 的同段是**两份拷贝，必须保持一致**（`glShimJS` 的文档注释已注明）。

### 7.5 wesing 的 vtable 定位链（`player/wesing/lyric/finder.go`）

`FindLyricHost` 五步，任何一步换 WeSing 版本都可能断：

1. 模块表里找 `KSongsLyric.dll`
2. **解析 PE 导出表**找 `CreateLyricHost`（`findExportFunction`）——手工走 `base+0x3C` → PE 头 → Optional Header `DataDirectory[0]`（`+0x78`）→ 名称表/序号表/函数表（`+0x18/+0x20/+0x24/+0x1C`）
3. **反汇编**：在 `CreateLyricHost` 前 128 字节里找第一条 `CALL rel32`（`0xE8`）→ 构造函数（`findFirstCall`）
4. 在构造函数前 200 字节里找 `mov [edi], imm32`（`C7 07`）→ **vtable 地址**（`findVtableAssignment`）
5. **AOB 扫描**：把 vtable 值当 4 字节小端模式在可写区域搜（`EnumWritableRegions` + `Uint32ToAOB` + `AOBScan`）→ 首个命中 = LyricHost 实例；歌词子结构 = `hostAddr + 0x0C`

**这条链上的每个魔数（`0x3C`/`0x78`/`0xE8`/`C7 07`/`0x0C`）都是逆向出来的，不是标准。** 改 wesing 取词前先读完这个函数——它失败时只返回 error（五步各一处 `return 0, 0, i18n.Errorf(...)`），wesing 会跳过本轮重试，不会有任何提示告诉你是哪一步断的，只有前四步成功后各打的那条 `log.Detail` 地址能定位。排查时把日志级别看全。

**子结构之后的内存布局（`reader.go` `LoadLyrics` 读的，同样全是逆向魔数）：**

```
子结构 +0x48 / +0x50   = vector<LyricEntry*> 的 begin / end
LyricEntry +0x00       = float 行起始秒
LyricEntry +0x08 / +0x0C = vector<CharElement*> 的 begin / end（字级单元：中文一字、英文一词）
CharElement +0x00      = RenderData*（+0x00 → null 结尾 UTF-16LE 文本）
CharElement +0x04      = float 字起始秒（绝对）   ← 逐字
CharElement +0x08      = float 字时长秒           ← 逐字
```

`+0x04`/`+0x08` 就是 wesing 逐字的来源（见 §6.2）。**2026-07-18 实测跨进程重启一致**：基址随 ASLR 变（无所谓，链的第 1、5 步靠模块枚举 + AOB 扫堆重新定位），但上面所有**结构体内偏移不变**。合法性判据：首字起始 ≈ 行起始、行内 `+0x04` 单调不减、`+0x08` ∈ [0,100)——任一破即整行退回行级（`reader.go` 的 `wordsOK`），不推半截错位的逐字。垃圾防护复用行级同一个 `IsPlausiblePlayTime`（接受式，拒 NaN/Inf，见 §3.7）。

### 7.6 网易云 canvas 严格只读

**绝不修改 `#lyric-effect-canvas-id` 的 width / height / style。改它会让网易云崩溃。** ❌ 靠人

注释钉在 effect.go 的四处：包注释（「严格只读，绝不改源 canvas」）、`captureOutMaxW` 的注释（「绝不改网易云 canvas」）、`captureInjectJS` 的文档注释（「严格只读，绝不修改网易云 canvas —— 改其尺寸会导致网易云崩溃」）、`captureInjectJS` 脚本体内快照处（「绝不改源 canvas」）。

capture 只能 `drawImage` 把源 canvas 拷进**我们自己的**画布池快照（`captureInjectJS` 中的 `slot.x.drawImage(c,0,0,ow,oh)`），降采样也只作用于快照（`captureOutMaxW = 1920`，且仅当源宽超过它才缩）。分辨率由主播的窗口尺寸决定——用户放大窗口即更清晰，我们不强制改渲染分辨率（见 `captureInjectJS` 的文档注释）。想提画质就调 quality/outw，**不要动源 canvas**。

> 归属 §3.5 的一条事实，暂存于此待并入：`semver.IsValid("v0.0.0")` 为**真**，所以开发版自更新的 kill switch 不是 semver 合法性，而是 `main.go` 的 `isReleaseVersion` 里的手写特例 `normalized == "v0.0.0"`。

### 7.7 sodamusic：绕原生反调试开 inspector + transport 提取（`player/sodamusic/`）

汽水音乐是 Electron，**有原生反调试**：启动 argv 里带 `--remote-debugging-port` 会被它在 ~2s 内自杀。所以 cloudmusic 那套「杀掉重启 + 加 argv」在这里**不通**；`NODE_OPTIONS=--inspect` 也被打包版 Electron 过滤（实测 app 存活但 inspector 不开、零监听端口）。

**唯一可行且非破坏性的路子 = 复刻 Node 的 `process._debugProcess(pid)`**（`watchdog/watchdog.go`）。Windows 机制：目标 Node/Electron 主进程启动时会建一个命名映射 `node-debug-handler-<pid>`（十进制 pid），内含一个指针 = 目标地址空间里 `StartIoThreadWrapper` 的函数地址。激活 = `OpenFileMappingW` 读出该地址 → `CreateRemoteThread` 到目标 → 目标**自己**拉起 inspector I/O 线程（默认 9229）。这条路不碰 argv，反调试扫不到；实测 inspector 开着后长会话稳定，反调试**不检测 9229**。全程只读映射 + 建一个远程线程跑目标自带的激活函数，**不改汽水任何内存/状态**（§0.1 红线）。映射不存在（禁用了 inspector）→ 返回错误让上层降级重试，绝不盲写。

- `OpenFileMappingW` / `CreateRemoteThread` 提为包级 `var`（syscall 过程句柄不在轮询路径反复建）。读映射里的 8 字节用 `RtlMoveMemory` 拷进 Go 缓冲 + `binary.LittleEndian` 读出——**避开 `unsafe.Pointer(uintptr)` 触发 vet unsafeptr**（addr 作 syscall 源参数是安全的）。
- node v16 caller 能对 Electron 36(node 22) 目标生效 → 映射名格式跨大版本一致。

**取数**（`cdp/client.go`）：连 9229（主进程 Node inspector，端点 `/json/list`）→ `Runtime.evaluate` **必须带 `includeCommandLineAPI:true`**（Node 经 Command Line API 暴露 `require`，缺了它主进程桥里 `require('electron')` 会 `require is not defined`）→ 在主进程里 `webContents.executeJavaScript(...)` 桥进 rendererMain 主窗口（主进程无 DOM，故一律 `awaitPromise:true`）。桥内探针用「patch `MessagePort.prototype.postMessage` 抓闭包里的 `channel.port1` → 发 `method.invoke` 请求 `sharedState.get('player')` → 截 `method.return`」拿全量播放态。播放态字段：`progressSeconds`（**1Hz 采样**，见下条）、`mediaDetail.playable`（名/歌手/id/时长/`cover_url`）、`mediaDetail.lyrics`（`type:'krc'` 明文 + `translations.cn` 独立 tlyric LRC，按时间戳合并进 SubText）。封面 URL 由 `cover_url={uri,urls[],template_prefix}` 拼 `urls[0]+uri+'~'+template_prefix+'-crop-center:800:800.jpg'`（公网可取、无需鉴权）。

#### 7.7.1 `progressSeconds` 是 1Hz 采样 —— 落锚必须边沿触发 ❌ 靠人

**这是照 kugou 骨架时唯一不能照抄的一处。**

真机实测（2026-08-08，连采 60 次变化）：`progressSeconds` 每 **970~1060ms** 才刷新一次，每次
`+1.0000±0.01`；而值本身是采样瞬间的**真实位置**（形如 `45.519`，毫秒精度，不是整秒量化）。
也就是说两次刷新之间它一直是旧值，陈旧程度 0~1s。

对照：酷狗的进度是 100ns 计时器直读（`kugou.go` 的 `runSession` 中 `progressRaw / 1e7`），**每轮都是新鲜值**，
所以它那句「每轮 `anchorProgressSec = progressSec; anchorTime = time.Now()`」落不落锚没差别。
汽水照抄这句，外推就被清零 —— `position` / `progress` / 歌词行匹配一起退化成 1s 阶梯。

实证指纹（不需要外部基准，看输出本身就够）：`lyric_update.position` 的小数位。
- 每轮落锚：连续 10 条事件的 position 全是 `142.925 / 144.924 / 147.920 / 150.918 …`
  ——**小数位恒 `.92`，全落在同一条 1Hz 采样网格上**，说明时钟只在采样落地时前进。
- 边沿落锚：`193.007 / 195.856 / 199.923 / 201.548 …` ——小数位随行阈值散开，时钟是连续的。

现行实现见 `player/sodamusic/sodamusic.go` 的 `lastRawSec` 与 `livePos`。**别退回每轮落锚。**

> ⚠️ 换个测法会看不见这个缺陷。拿 `position` 与真实位置比对，两个版本的误差都只有 ~100ms
> ——因为**换行只可能发生在时钟前进的那一刻**，而阶梯时钟前进的那一刻采样恰好是新鲜的。
> 要看见它，得看「行阈值落在台阶中间时要等多久才跳」，或者直接看上面那个小数位指纹。

#### 7.7.2 窗口最小化久了，进度源掉到 1/60Hz —— 用 `setBackgroundThrottling(false)` 消除 ❌ 靠人

Chromium 对隐藏页面有 intensive wake-up throttling。对照实验（2026-08-08，汽水主窗口全程最小化，
两轮除了「有没有调那一行」之外条件相同）：

**对照组（不调）**：闲置约 4.8 分钟后节流生效，之后恒为一分钟一次。

```
[  288.1s] progress= 58.021 dv= +1.001 dt=  1064.6ms   ← 前 4.8 分钟正常
[  316.2s] progress= 86.021 dv=+28.000 dt= 28027.8ms   ← 节流生效
[  376.2s] progress=146.022 dv=+60.000 dt= 60031.3ms
[  436.1s] progress=206.019 dv=+59.997 dt= 59934.0ms
```

**处理组**：在**已被节流**的状态下调用，300ms 内恢复 1Hz，窗口仍是最小化。

```
setBackgroundThrottling(false) -> [[1,false]]
[    0.3s] progress=263.941 dv= +0.200 dt=  303.6ms
[    1.3s] progress=264.940 dv= +0.998 dt=  915.6ms
```

随后不再动它、继续最小化观察 8 分钟（远超对照组 4.8 分钟的触发点）：**478 个采样，最大间隔
1121.5ms，超过 2000ms 的 0 次**——节流没有重新装填。

**「Electron 里 setBackgroundThrottling 加载后再设需 reload 才生效」这条传闻对本版本不成立**
——上面那两行就是反例。别因为看到那个说法就把这条实现改成「加载前设置」或加 reload：
reload 汽水的渲染器会打断播放。

症状识别：日志里的「检测到前跳」全部落在整分钟的同一秒、间隔恰好 60s、跳幅恰好等于间隔。
**那不是 seek，是节流。**

现行实现：`cdp.Client.DisableBackgroundThrottling()`，在每次 CDP 连上后调一次
（`sodamusic.go` 的 `Start`）。**每次重连都要重设**——它随 webContents 生命周期存在。
尽力而为：失败只 `log.Warn`，不阻断取数。

**为什么不照网易云加启动参数**：网易云那条路是 watchdog **杀掉进程 + 带 argv 重启**
（`player/cloudmusic/watchdog/process.go`），对汽水等于**在直播中杀掉主播的播放器**；
而且汽水有原生反调试，argv 里加东西是否触发自杀未经验证（§7.7 当初正因它自杀才放弃重启加参数）。
运行时 API 只是对它自己的 webContents 调一个官方 Electron 接口：不写内存、不碰 argv、
不改注册表，进程退出即失效。

> **这是 §0.1「不改汽水任何内存/状态」的一处刻意例外**，边界写在这里：只允许
> `setBackgroundThrottling(false)` 这一个调用。要再加别的「顺手也设一下」的 API 之前，
> 先回来读这一段——例外之所以安全，靠的是它窄。

即使这条失效（旧版 Electron、API 被移除），7.7.1 的边沿落锚仍是兜底，两者不是二选一：
- 每轮落锚 + 节流 → 进度**整整冻结一分钟**再跳 60s。实测日志 `检测到前跳: 0.00s → 28.01s`，
  歌词在第 0 行卡了 28 秒。直播里这是事故。
- 边沿落锚 + 节流 → 歌词照常平滑推进（60s 内音频时钟与墙钟只差 0.4ms），只在会话刚建立时
  带一个「首个采样有多旧」的固定偏移，下一个采样到达即被 seek 判据纠正。

#### 7.7.3 多个消费者会互相饿死 ❌ 靠人

同时开两个 PlayerCap（或 PlayerCap + `cdpexplore` + 临时探针）连同一个汽水时，实测出现过
某一路的 `Extract` 连续数十秒拿不到可用数据、而另一路正常。桥内探针会 patch
`MessagePort.prototype.postMessage` 再还原，两路并发时还原顺序会把对方的 wrapper 永久留在原型链上；
transport 侧能否并发承载多个 `method.invoke` 也未验证。

**结论：调试时别一边开着正式服务一边开探针**，测出来的数会是假的（本轮就先踩了一次：三路并发
下量到的「误差」全是并发伪影）。生产只跑一个实例，故未按缺陷处理。

---

---

## 8. 写完跑什么

四道门禁。**提交前四条全跑，不挑；打 tag 发版前在 Windows 开发机上再全跑一遍，并加跑 `go test -race ./...`**（CI 不跑任何测试，发版前这一遍是唯一必经的检查点，见本节末）：

```bash
gofmt -l .        # 必须无输出
go build ./...
go vet ./...
go test ./...
go test -race ./...   # 仅发版前；需本机有 C 编译器（cgo）
```

`gofmt -l .` 有输出就是没过——它列的是待格式化文件，不是警告。行尾靠 `.gitattributes` 的 `* text=auto eol=lf` 兜住；删掉那行，`gofmt -l` 会把全部 .go 误报成格式错（88% 假阳性），门禁当场失效。

### 三条盲区：绿 ≠ 安全

| 盲区 | 实测 | 谁在守 |
|---|---|---|
| `go vet` 的 lostcancel 只认「对变量的直接丢弃」。cancel 存进结构体字段、经 helper 释放 → **看不见**；**压根不带 context 的 `go func`** → **结构性失明**，无对象可查 | 三例合成测试：直接丢弃报，helper 释放与不带 context 的 `go func` 均静默通过 | ❌ 靠人 |
| `go vet` **不检查 NewCallback 计数** | vet 无此 analyzer | ✅ `player/callback_lint_test.go`（AST 门禁：全仓 NewCallback 只能在包级 var；已覆盖包级 FuncLit、别名 import、NewCallbackCDecl 三种漏报形状） |
| `gofmt` **不检查 import 分组**。它只在块**内**排序，不强制 stdlib 与三方分组 | 单块内 `fmt`/`github.com/...`/`os` 字典序排列 → `gofmt -l` 无输出 | ❌ 靠人 |

### 改了 `config/`、`logger/` 或 `i18n/` 必跑

```bash
GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./i18n/... ./tools/genconfig
```

它们是打包流水线的编译期依赖：CI 在 Linux runner 上原生跑 `GOOS= GOARCH= go run ./tools/genconfig`（两个 workflow 的 `Prepare files for packaging` 步骤）生成随包 config.yml，而 genconfig 的内部依赖恰为这三个包（`GOOS=linux go list -deps ./tools/genconfig` 实测）。`config/`、`logger/` 引入 x/sys/windows 或 syscall，或 `i18n/` 的 Windows 专有代码漏了构建约束 / `!windows` 桩 → 两条流水线同时挂，且失败点在「Prepare files for packaging」而非 build。

**必须用多包形式。** 多包 `go build` 丢弃输出、不产物；写成单包（只 `go build ./tools/genconfig`）会在仓库根目录留下一个 **2.9MB 的 Linux 二进制 `genconfig`**——而 `.gitignore` 只有 `*.exe`，挡不住无扩展名产物（`git check-ignore` 实测：NOT IGNORED，会被 `git add -A` 一起提交）。

### CI 的真实能力

**只有 self-hosted Linux runner，Windows runner 不可用。**

| 包 | `GOOS=linux go vet` 实测 | CI 接上门禁后 |
|---|---|---|
| `config` `logger` `i18n` `player` `player/krc` `player/cloudmusic/cdp` `player/cloudmusic/lyric` `player/kugou/cdp` `player/kugou/lyric` `player/sodamusic/cdp` `taskbar` `audiocap/dsp` `audiocap/frame` `audiocap/helper`† `audiocap/manager`† `audiocap/procscan`† | exit=0 | 能测（**注意：当前并没在测**，见本节末） |
| 根包 `clientid` `server` `telemetry` 及 `player/` 其余各包（含 `player/cloudmusic`、`player/cloudmusic/effect`、`park`、各 `watchdog`、`player/qqmusic`、`player/wesing`、`player/kugou`、`player/sodamusic`）、`audiocap/wasapi` | FAIL | **永远跑不了** |

† 只编进无 build tag 的纯逻辑文件（helper 的控制行解析/降混/按声道峰值累加与窗口/丢最旧/归一化/限时等待，manager 的 `desiredCaptures`、看门狗判定、逐播放器失败记账、成功清账、want 过滤、空闲帧判定 `idlePlayers`，procscan 的 `MainPID`；helper 与 manager 共用的 QPC 毫秒换算在 frame 的 `QPCMillis`）；捕获、`encodeFrame` 的峰值与 flags 写入、非有限采样经窗口到帧的端到端用例（`TestEncodeFrameNonFiniteSamples`）、helper 真帧时间戳（`TestEncodeFrameTimestampQPC`）、空闲帧的内容与推送接线、Job Object、退避与拆除复位、`checkWatchdog` / `reapHelper` / `teardownHelper` / `spawnHelper` 接线、`reconcile` 的 want 过滤、进程名映射的测试都在 `*_windows_test.go`，CI 永远跑不了。

Windows-only 包的测试 CI 跑不了——**别在 PR 里假装有覆盖**。

`telemetry` 自 `sysinfo.go` 引入 `x/sys/windows` 起也进了这一行（`GOOS=linux go vet ./telemetry`
实测：`build constraints exclude all Go files`）；`server` 与 `player/` 下起 goroutine 的各包为挂
`defer telemetry.Guard()`（§1.2）import 了 `telemetry`，随之一并进了这一行。代价要说清楚：这些包的
测试**全靠本机跑**，其中包括 `guarddirect_test.go`、`goroutineguard_test.go` 两条扫全仓的 AST 门禁——
它们恰恰是最需要自动化的，因为守的是「有人在别的包里漏挂 Guard 或把它写成闭包」这种远程违规。
这是有意接受的取舍（本项目本就是纯 Windows），补偿手段是上面那条「发版前在 Windows 上全跑」，
但**别以为它有 CI 在守**。

这不是「为 Linux 设计」（本项目是纯 Windows 服务），只是**用现有 runner 测它能测的**。`-race` 不必靠 Linux runner：
本机有 C 编译器即可在 Windows 上跑（2026-10-05 实测 gcc 15.2.0，`go test -race` 对除 `tools/` 外的全部包通过）。
`server/race_test.go` 用合法状态对交替的手法、不依赖 `-race`，是写它时本机还没有 C 编译器的遗留，照常有效。

> ⚠️ **上表的「能测」是潜力，不是现状。别把它读成「CI 在跑」。** 实测教训：`doc/hardening-notes-2026-07.md`（已于 `5d22d80` 删除，原文见 git 历史）
> 的修复表里因此写出了「player 包 Linux 可编译 → **CI 真门禁**」这种标注，把一条纯本机门禁
> 记成了自动化门禁（还顺带把 `player/cloudmusic` 误标成可编译，实为 FAIL）。**能测 ≠ 在测**，
> 差的正是下面这段说的那个触发器。

**当前 CI 无任何 `pull_request` 触发器、无质量门禁**（四个 workflow 全读过：build-windows 触发于 push main、release 触发于 tag、sync 触发于 tag 与手动 `workflow_dispatch`、purge 触发于 release 事件）。这是已知缺口——上面的门禁命令目前**全靠人在本地跑**，发版前那一遍不得省。

---

## 9. 验证方法学

「我改完了，怎么知道它真的对」。以下每条对应一类真实发生过的错误。

### 写测试时必须问「它在什么情况下会红」，并真的让它红一次

实例：kugou 的 `TestReadExeVersion` 用正则 `^\d+\.\d+\.\d+\.\d+$` 做断言，而被测函数是
`fmt.Sprintf("%d.%d.%d.%d", 四个 uint32)`——同义反复，物理上不可能失败。变异实测（把
`&buf[off]` 改成 `&buf[off+4]`，正是该 commit 要防的指针算错）：qqmusic 测试 FAIL，kugou 测试
PASS 并返回 `"19041.5915.10.0"`——一个格式合法的错误版本号，正是该测试注释里自称要拦截的
失败模式。

**规矩：变异测试是验收标准，不是可选项。** 写完测试，把它要防的缺陷注入回去，确认它变红，
再恢复。做不到这一步的测试等同于没写，并且会造成「已经守住了」的错觉。

### 测试文件命名：一个文件一个关注点，文件名即关注点

Go 要求 `_test.go` 与被测包**同目录**（实测：挪到 `test/` 子目录后 `undefined: 被测函数`——
未导出标识符跨包不可见）。所以测试无法集中存放，只能靠命名维持秩序。

**规矩：文件名描述该文件测的那一个关注点。禁止用包名或源文件名做兜底名。**

`mem_test.go` / `watchdog_test.go` / `api_test.go` 这类名字承诺的是整个源文件，实际往往只测
其中一个点，且会给后来者发出「往这儿堆」的邀请函——`mem.go` 逾 1400 行、`watchdog.go` 约
800 行，镜像命名等于宣布这个测试文件将来要装下全部。已按关注点重命名：

| 旧（兜底名） | 新（关注点名） |
|---|---|
| `main_test.go` | `updatedecision_test.go` |
| `player/player_test.go` | 拆为 `eventjson_test.go` + `lyrictextmatch_test.go` |
| `player/cloudmusic/cloudmusic_test.go` | `songidentity_test.go` |
| `player/qqmusic/api_test.go` | `qrcdecrypt_test.go` |
| `player/qqmusic/mem_test.go` | `getfileversion_test.go` |
| `player/kugou/watchdog/watchdog_test.go` | `readexeversion_test.go` |
| `player/wesing/lyric/timer_test.go` | `plausibleplaytime_test.go` |

判据：**文件顶部能不能用一句话说清「本文件只测 X」**。说不清、或者要用「和」连接两件无关的
事，就该拆（`player_test.go` 当初就同时装着事件 JSON 形状与 `SameLyricText` 文本匹配，已拆开）。
缺陷回归测试用失败模式命名是合规的（`ws_zombie_test.go` / `race_test.go`）——失败模式就是它的
关注点。改测试文件名对 Go 工具链零风险：它只认 `_test.go` 后缀与包名。

### 破坏性验证必须在隔离环境做，绝不在工作区

实例：为验证一处注释的承诺，在活的工作区里做「备份 → 改 → 还原」实验，拿到 `vet exit=0`，
据此几乎得出「vet 抓不住、注释不成立」的结论。该结果无效，两个原因叠加：
（a）另一个 agent 当时正在回退同一个文件做自己的实验，「备份」备份到的是被改过的版本；
（b）退出码取自管道末端的 `head` 而不是 `go vet`。
在隔离克隆里重做后结论相反：`vet exit=1`，注释里的承诺成立。

**规矩**：破坏性验证一律 `git clone` 到临时目录或写最小复现，不碰工作区。工作区是共享资源
——有 agent 在跑、有编辑器开着、有别人在读 `git status`。

**推论：管道会吃掉退出码。** `go vet ... | head` 的 `$?` 是 `head` 的。

**推论：真机捕获验证同理，声卡也是共享资源**（蓝屏事件见 §14 未决 3）。只起验证需要的最小组合
（server + Manager，不拉起播放器侧 watchdog），短跑，测完断开所有 `/audio-ws` 让 helper 拆除，别与其他
对同一声卡施压的实验并行。`tools/audioprobe` 同理。

### 「已验证」有使用门槛：推理出来的和实测过的必须用不同的词

容易编造的几类断言：

- 写「已用测试**穷举**确认」——实际测试里只覆盖了 12 个值。
- 写「**实测**确认无回归」——从未跑过新旧二进制对照。
- 写「非原子快照**稳定复现**」——回退旧代码跑 6 次，6/6 全死于撕裂 panic，那个「非原子快照」
  检测器一次都没触发过。频率写反了。

**规矩**：说「已验证」之前先问「验证的产物在哪」。拿不出产物就写「推断」。写「实测」就要能
说出跑了几次、看到了什么。

### 给函数写注释或 commit message 前，把调用方读完

同样容易编造的几类断言：

- 声称「垃圾版本号可能意外匹配上哨兵并触发盲打补丁」——方向反了。哨兵不是闸门（只把日志从
  Warn 降成 Detail），唯一真闸门是 `HasPrefix(ver, "10.")`。真实危险是：垃圾版本号只要不以
  `10.` 开头，反而绕过唯一的拒绝闸门。
- 声称「`tools/parktest` 是循环调用它的」——`for _, s := range park.DebugList()` 遍历的是
  返回的 slice，每进程只调一次。把 `for ... range f()` 读成了「循环调用 f」。
- 声称「这是本程序唯一会写外部进程内存的地方」——同一个文件里就有第二个注入器
  （`InjectProgressAOB`），只是调用点被删了、函数本体还在。排查粒度是「按播放器包」而非
  「按文件」，给一个函数写注释却没往下翻 20 行。

**规矩**：凡是描述「后果」的注释，必须读完调用方再写。凡是用「唯一」「总是」「永远」这类
全称量词的，必须 grep 全仓验证。

### 任何上一轮的产出都是线索，不是真理

实例：架构探索地图里标为「critical」的结论被后续证伪（提权助手 TOCTOU、`Unpark` 的
`clearState` 顺序），而它们当时的表述相当确定。审计数据的行号也存在 +7/+5 的偏移（切片时的
树与当前 HEAD 不同）。

**规矩**：拿上一轮的结论当线索去查代码，不要当事实去引用。**尤其是行号。**

---

## 10. 提交前

### 跑完 §8 的四道门禁

不用等人催。改了 `config/`、`logger/` 或 `i18n/` 再加跑那条 Linux 自检。

### 提交策略

- **不自动提交。** 流程是：改完 → 跑门禁 → **用户测试通过后** → 用户说「提交」才提交。
- **绝不在 `main` 上直接改/提交。** 先开分支（`fix/...` / `feat/...`）。
- **commit 不加任何 co-author / `Co-Authored-By` / AI 署名。** 只写正文。
- **用显式文件路径 `git add`，不要 `git add .`。** 工作区可能有并行的 agent 或自动化工具写入的临时文件（`zz_*_test.go` 之类），这些文件未经审查——混进提交轻则污染历史，重则带进一个会让 `go test` 挂死到超时的死锁探针。
- **一个独立的既有 bug 修复单独拆 commit**，别混进 feature。
- 多行 message 用 `git commit -F -` 配 heredoc，避免 shell 转义把 `@` 之类混进标题。

### commit message 的门槛

**写「为什么」，不只是「改了什么」。** 尤其是那些会被后人「顺手优化」掉的东西——把原因写进
message 和代码注释，否则下一个人会把 bug 装回去。实例：`coverCancel` 的三处显式调用看起来
啰嗦，不写清「改成 defer 闭包会让 vet 失明」就一定会被改掉（出处见 §3.8）。

**但 §9 的「已验证」门槛同样适用于 message。** message 与注释里最常见的编造有三类：

- **全称量词**——「唯一 / 总是 / 永远」，写的时候只查了当前文件。
- **验证声明**——「已验证 / 已复现 / 无回归」，实际是推理出来的，没有产物。
- **后果描述**——声称改动会导致什么，而没有读过调用方；把控制流读错（例如把
  `for ... range f()` 当成循环调用 `f`）也归在这类。

这三类的共同点：写的时候语气笃定，读的时候没人会去核。**message 里的错误比代码里的错误更
持久**——代码会被重写，message 永远躺在 `git log` 里误导人。

提交前自问三句：
1. 我写的每个「唯一 / 总是 / 永远」，grep 过全仓吗？
2. 我写的每个「已验证 / 已复现」，产物在哪？
3. 我描述的后果，读过调用方吗？

### 工作树卫生

提交前 `git status --porcelain` 应当只剩你打算提交的文件。若有 `zz_*`、`nantest_tmp/`、
无扩展名的 Linux 编译产物（`genconfig`）——那是 agent 或误用单包 `go build` 留下的，清掉。

---

---

## 11. 硬化与复查

§9 回答「我改的这个对不对」；**这一节回答「我们怎么找出我们根本没想到的」**。前者每次改动都做，
后者周期性做。

### 什么时候做

- **发版前必做，别跳。**
- 碰过这几类之后必做：并发 / goroutine 生命周期 / 提权 / 写外部进程 / 窗口生命周期 / 子进程生命周期 / COM 回调。

### 方法：风险加权，不是均匀铺开

两万余行非测试代码里风险密度差得很远。均匀铺开的结果是每块都浅，反而漏掉真东西。

| 面 | 为什么高危 | 建议视角数 |
|---|---|---|
| `player/kugou/watchdog` | UAC 提权 + 盲打补丁 + `%TEMP%` 文件 IPC，且无备份 | 3~4（安全/失败模式/并发） |
| `player/cloudmusic/effect` + `park` | churn 最密、碰 DWM 与窗口生命周期、承重 | 3~4（状态机/窗口生命周期/并发） |
| `player/qqmusic/mem` | 写外部进程 + 手工逆向的版本偏移表 | 3（内存安全/版本回退/密码学） |
| `audiocap/`（manager + helper + wasapi） | 本仓库首个长命子进程（Job Object、退避、看门狗、孤儿清扫）+ 手搓 COM vtable 跨 C 边界 + 挂在 `r.mu` 主线上的钩子 + 直接压在声卡驱动上 | 3（子进程生命周期/COM 与内存安全/锁序与阻塞） |
| `server/` | 四层锁序 + 静默丢弃 + 公开 wire 契约 | 4（竞态/丢弃/非确定性/契约） |
| `logger/`（42 行） | —— | 0，不值得占一个 agent |

### 对抗性证伪是必需工序，不是可选

实测中约半数原始发现经不起对抗性证伪。

不加这道工序，硬化报告里会有相当比例的幻影发现。**这比没有报告更糟**：未经证伪的发现一旦写进
「硬规则」，之后就没人敢质疑，后人只会绕着它走。

证伪者的指令要点：
- **明确偏向枪毙**：「拿不准就判 refuted=true」。宁可漏掉一条真问题，也不要让站不住的发现
  进入报告。
- **必须专门检查「这个 bug 会不会其实是故意的」**。本仓库有大量反直觉但正确的设计：
  QQ 的 DES S-box 故意偏离 FIPS 46-3、`shimInjectLoop` 故意不受 `netEaseUp()` 门控、
  fan-out 的 `default:` 丢弃正是 fan-in 不停滞的前提。
- 每条发现单独派一个证伪 agent，视角互不相同。

### 自我复查不管用，必须找对手

自查与对抗性复查的产出差距很大：自查倾向于只承认无害的小问题，而独立的对抗性复查能在同一批
改动里找出回归、无效测试与未经验证的断言。

**这不是不够认真——是自查在结构上就做不到。**

复查者要专找这几类：
- **未经验证的断言**：注释/message 里声称的事实，有没有测试或实测支撑？尤其「已复现」
  「已验证」「不会更糟」这类措辞。
- **新代码零覆盖**：改动引入的分支，有没有测试真的走到过？
- **假测试**：新增的测试在缺陷存在时真能变红吗？（变异测试）
- **修复不完整**：同样的模式在别处是否还有？（本仓库有大量复制粘贴的缺陷）
- **过度自信的注释**：写死的结论，是不是猜的？

### 判级与处置

| 级 | 判据 | 处置 |
|---|---|---|
| **HIGH** | 会崩 / 会卡 / 会写坏用户的东西 / 用户无法自行恢复 / 提权面被滥用 / 违反承重约束 | **发版前必修** |
| **MED** | 矛盾态 / 数据丢失 / 自愈型泄漏 / 静默失败 | 能便宜修就修，否则记录 |
| **LOW** | 理论路径 / 诊断可读性 / 受控例外 | 写进硬化笔记，**必须写清为什么不改**——「优先级低」不算理由 |
| **未判决** | 证伪 agent 挂了，既未证实也未推翻 | **绝不混进 HIGH/MED 清单**，单列。可信度等同未复核的原始发现（约半数站不住） |

### 沉淀

结论写成 `doc/<主题>-hardening-notes.md`：**开头一张总览索引**（全部发现 + 状态 + 指向详情）
+ 总评（有无阻断缺陷）+ 修复清单 + 观察项（已分析、为何不改）+ 验证步骤。
**标明哪些只能真机验、哪些 CI 永远跑不了**（Windows-only 包）。

### 修一条，就在同一个 commit 里订正笔记

**硬性要求：修复 commit 必须同时更新硬化笔记的总览索引。** 不是「修完统一更新」——那等于
不更新。笔记与代码分叉之后，它就从资产变成了负债：后来者按它去修已经修过的东西，或者
绕开它自己重查一遍。

订正内容不止于「打勾」，还包括：

- **原判断错了就写下来。** 审计报告不是圣经，它对「意图」的判断尤其不可靠（实例：把一个
  commit 标题的 `chore:` 前缀当成「顺手砍的」证据，而 body 里明写 `perf(...)` 与实测理由）。
- **修法与笔记的建议不同，写明为什么。** 尤其当笔记明确警告过某个修法（实例：MED §8b 警告
  「gate 分支显式回滚 + continue」是稳定性倒退，而 HIGH #6 的首版修复正是那个方向）。
- **修出回归又返工的，两个 commit 都留在表里。** 返工记录比干净的修复清单有用得多——它
  是唯一能让人看出「这里踩过什么」的东西。

### 改共享函数的语义，grep 全部消费方

**改了一个函数「返回什么意思」，就必须 grep 它的每一个调用方，逐个确认判据还成立。**
不是「改手头这个」——改完的那一刻，其余调用方全都在按**旧契约**读新数据。

实例（`d63a67d`，本轮由 LOW review 抓出）：`ParseLRC` 原本吞掉 intentional blank，于是
「行数 > 0」等价于「有歌词」。该 commit 让它保留 blank —— **契约变成「行数 ≠ 有歌词」** ——
但只改了 `resolveCDPLyrics` 一个消费方，漏掉 `cloudmusic.go` PureMusic 分支里同样按
`len() > 0` 判定的那条。后果是 OBS 整首空白且不自愈。**最刺的是**：同一个 commit 在
`fetch.go` 里亲手写下了「判断一首歌有没有歌词要看有没有实词行，别用 `len(结果)`」——
契约立在那儿，200 行外自己违反。

**连带要查注释**：漏改的那条被 `resolveCDPLyrics` 的文档注释**立成了「正确参照系」**
（「与它保持一致」）。改契约时，凡是引用「另一条分支怎么做」的注释全部要复核 —— 它们
会把错的固化成对的，比代码本身更难发现。

### 注释里引用位置，用符号名不用行号

**同文件内的交叉引用写符号名（函数名 / 常量名 / 特征代码），不写 `:行号`。**

理由不是洁癖：**加注释这个动作本身会把注释里的行号打漂**。本轮实测——`aa78297` 往
`config.go` 插入 121 行，把 AGENTS.md 里约 20 处 `config.go:NNN` 引用全部打漂，**包括它
自己在同一个 commit 里新写的那处锚点**（`config.go:211` → 实为 `:280`，漂 70 行）；
`46386f3` 的注释引用 `:282`，被它自己的 diff 推到 `:286`。而这些锚点存在的**唯一目的**
就是防止两份真源分叉 —— 它们自己先分叉了。

跨文件引用行号仍可接受（改动频率低），但**必须带上符号名做冗余**，例如
「`kugou.go:295`（`pollMs` 直通 `time.Sleep` 那处）」—— 行号漂了还能靠符号找回来。
硬化笔记的行号引用已被实测证明大面积过期（本轮一条错位 137 行、一条错位 71 行、
指到了完全无关的代码），**按符号找，别按行号抄**。

**动一处代码之前，先把这份笔记里所有提到它的地方读完**，不只是你正在修的那一级。HIGH / MED /
LOW 三段常对同一处代码给出不同的、逐级更准的判断——只读自己那一级的代价，实测是两次返工。

---

## 12. 发版

### 客户端判据（改这些之前先读 `main.go` 的 `decideUpdate` / `isReleaseVersion` / `isForceReleaseName`）

| 规矩 | 为什么 | 谁在守 |
|---|---|---|
| **版本判据只认 `tag_name`**；`name` 只用于 -force 判定 | `checkAndUpdate` 中的 `decideUpdate(Version, release.TagName, release.Name)`；字段见 `releaseInfo`。写反 = 线上误降级 | ✅ 部分：`updatedecision_test.go` 只守 `decideUpdate` 内部；调用点传参与 json 字段映射靠人（§3.5） |
| **绝不给开发版打 semver 版本号** | `Version == "0.0.0"` 或非 semver → 跳过更新检查，这是开发版自更新的 **kill switch**（`Version` 默认值、`isReleaseVersion`）。CI 靠打**非 semver** 版本号利用它（`build-windows.yml` 的 `Build Windows executable` 步骤中 `VERSION="${OUTPUT_NAME}-${BUILD_TIME}-${COMMIT_HASH}"`）。「修」CI 让它打真 semver = 静默给开发版装上更新器 | ❌ 靠人（CI 侧）；判据本身由 `updatedecision_test.go` 守（§3.5） |
| **绝不删 `isReleaseVersion` 里的 `normalized == "v0.0.0"` 特判** | 实测 `semver.IsValid("v0.0.0")` = **true**——kill switch 不是 semver 校验兜住的，就是这行手写特判。以为 `IsValid` 覆盖了它而「简化」掉 = 开发版静默获得更新器 | ✅ `updatedecision_test.go`（删掉特判，`TestIsReleaseVersion`、`TestDecideUpdate` 均红，变异实测） |
| **`-force` 只进 release 标题，绝不进 tag** | 实测 `semver.IsValid("v3.0.0-force")` = **true**，`Compare("v3.0.0-force","v3.0.0")` = **-1**——不报错，被当成 v3.0.0 的预发布版、排序低于正式版，构成静默降级陷阱。判定见 `isForceReleaseName`（ToLower + TrimSpace + 后缀） | ✅ `updatedecision_test.go` 的 `TestIsForceReleaseName`、`TestDecideUpdate` |
| **不得勾选 pre-release**，即使 tag 带 -alpha/-beta/-rc | `/releases/latest` 语义上排除 draft 与 pre-release，勾了 = 该版本对客户端不存在 | ❌ 靠人 |
| **CI 默认 `name == tag_name`、不追加 `-force`，别改** | release.yml 的 draft 创建步骤落在 `isForceReleaseName` 为 false 一侧——默认不可能触发强制降级 | ❌ 靠人 |
| SHA256 **仅在 digest 非空时**校验，失败即删文件并终止 | `main.go` 的 `downloadFile`：`expectedDigest == ""` 时**静默跳过**，校验与否取决于网关下发。放过损坏二进制 = 把客户端刷成砖 | ❌ 靠人（网关侧） |
| 缓存 purge **只在 published-as-latest 或 make_latest 变更时**触发 | 见 purge-release-cache-on-latest-change.yml 的 `if` 条件。**仅改标题不刷缓存**。回退顺序：先改标题 → 再设 latest | ❌ 靠人 |

### 两个 draft：必须发 dotcom 那个

`release.yml` 每次 tag 创建**两个 draft**：

| draft | 由哪个步骤创建 | 角色 |
|---|---|---|
| 本仓库（vlink.dev） | `softprops/action-gh-release` 步骤 | **内部留档**，不对外暴露 |
| **dotcom（VTB-LINK/Metabox-Nexus-PlayerCap）** | `gh release create --draft` 步骤 | **client-version 与两个 CDN 全部指向它** |

**必须发布 dotcom 那个 draft。漏了 = 版本号出去了但下载 404。**（网关 client-version 地址见 `main.go` 的
`versionCheckURL`；两个 CDN 前缀即 `releaseInfo` 的 `GlobalCDN` / `ChinaCDN`，均由网关下发，代码里无常量可查。）

### 隐式依赖链：清缓存在 dotcom 侧触发

`.github` 目录被 sync-source-to-dotcom.yml **全量镜像、无排除**（`Sync tagged snapshot to github.com main`
步骤先 `find ... ! -name .git -exec rm -rf` 再 `cp -R`），所以 purge workflow 是**镜像过去的那一份、
在 dotcom 侧**跑的。

以下任一断裂 → **缓存永不清 → 版本推出去了用户仍拿旧包**：

- 镜像不带 `.github`
- dotcom 未配 `RELEASE_CACHE_PURGE_URL` 或 `KONG_ADMIN_TOKEN`（purge workflow 的 `Purge release API cache` 步骤缺任一即 `::error::` 退出）
- dotcom Actions 关着

失败发生在 dotcom 的 Actions 里，vlink.dev 这边看不到。

> ⚠️ **现状：网关侧的响应缓存关着，purge 因此也一并停用。发版时不必查它。**
>
> **因果是「缓存关 → purge 关」，两者是配对的**（2026-08-10 仓库所有者原话：「因为我把缓存关了，
> 所以同步关闭 purge 的」）。缓存不在，purge 无事可做，留着它跑只会每次发版留一条无意义的记录。
>
> 2026-08-08 实测：dotcom 的四个 workflow 全是 `disabled_manually`
> （`gh workflow list --repo VTB-LINK/Metabox-Nexus-PlayerCap --all`），最近一次 purge 运行停在
> 2026-04-10 的 v2.0.3；两个 secret 仍在。网关侧 `proxy-cache-advanced` 插件挂在
> `Metabox-Nexus-PlayerCap` 这条 route 上、`enabled: false`。
>
> ⚠️ 本节此前把停用理由写成「`release.yml` / `build-windows.yml` / `sync-source-to-dotcom.yml`
> 被镜像过去后不该在 dotcom 重跑，purge 随那一批一起关了」——**那是从「四个都 disabled」倒推的，
> 是错的**（§3.0：别把推论写成事实）。那三个确实不该在 dotcom 重跑，但 purge 关掉与它们无关。
>
> **要重开就必须两件一起开：**先启用网关的 `proxy-cache-advanced`，再单独启用
> `Purge Release Cache On Latest Change`（**别把另外三个一起打开**）。单开任何一件都是坏组合：
>
> - 只开缓存 → `cache_ttl: 43200`（12 小时）+ 无 purge = **发版后最长 12 小时客户端才看得到新版**。
>   而且发版流程里「CI 建 draft → 人工发布」那段窗口，`/releases/latest` 返回的是**上一个版本**、
>   200 且完全合法，一旦被缓存就冻结 12 小时——客户端安静地判定「已是最新」，任何监控都看不出异常。
> - 只开 purge → 它无事可做。
>
> 上面那条依赖链与三种断裂形态是重开时的检查表，照旧成立。但**现在别再照着「发版后去 dotcom
> 确认 purge job 绿了」去查**——它不会有新记录，会被当成故障白查一轮（本文档没写这条之前，
> 已经发生过一次）。

---

---

## 13. 仓库速览

### 13.1 模块与构建事实

| 项 | 值 | 备注 |
|---|---|---|
| 模块名 | `Metabox-Nexus-PlayerCap` | `go.mod` 的 `module` 行；import 前缀照此写 |
| Go 版本 | `1.25.0` | 以 `go.mod` 为准，勿手改降级 |
| 出货二进制 | `Metabox-Nexus-PlayerCap.exe`（Windows/amd64） | canonical 名，自动更新链路依赖 |
| CI 构建命令 | `go build -ldflags "-X 'main.Version=${VERSION}' -X 'Metabox-Nexus-PlayerCap/telemetry.dsn=${SENTRY_DSN}'" -o "${EXE_NAME}"` | `release.yml` 与 `build-windows.yml` 的 `Build Windows executable` 步骤，两边逐字相同（两个 `-X` 参数外层都有单引号；`SENTRY_DSN` 取自 step 级 `env` 的 secret） |

### 13.2 直接依赖（9 项）

| 依赖 | 用途 |
|---|---|
| `github.com/getsentry/sentry-go v0.48.0` | 遥测与崩溃上报（`telemetry`；空 DSN 自动禁用） |
| `github.com/go-ole/go-ole v1.2.6` | ITaskbarList3 COM 调用（`taskbar`，conhost 任务栏进度） |
| `github.com/gorilla/websocket v1.5.3` | WS 订阅端、CDP 客户端、effect 与 audio 帧通道 |
| `github.com/josephspurrier/goversioninfo v1.7.0` | 生成带版本信息的多尺寸 Windows 图标 .syso（`tools/winicon`） |
| `github.com/shirou/gopsutil/v3 v3.24.2` | 进程发现（含 `audiocap/manager` 的孤儿 helper 清扫） |
| `golang.org/x/image v0.44.0` | 图标缩放（Lanczos3，`tools/winicon`） |
| `golang.org/x/mod v0.37.0` | `semver` 版本比较，自动更新用（`main.go` 的 `decideUpdate` / `isReleaseVersion`） |
| `golang.org/x/sys v0.45.0` | Windows API |
| `gopkg.in/yaml.v3 v3.0.1` | 配置解析 |

`golang.org/x/sys/windows` 主包直接用于 12 个出货源文件：`taskbar/taskbar_windows.go`、`telemetry/sysinfo.go`、
`i18n/detect_windows.go`、`player/cloudmusic/park/park.go`、`player/cloudmusic/watchdog/version.go`、
`player/sodamusic/watchdog/watchdog.go`、`player/kugou/watchdog/watchdog.go`、`audiocap/helper/helper_windows.go`、
`audiocap/wasapi/wasapi_windows.go`、`audiocap/manager/{manager,jobobject}_windows.go`、
`audiocap/procscan/procscan_windows.go`（另有工具 `tools/audioprobe/main.go`）；`registry` 子包另 3 处：
`clientid/clientid.go`、`player/cloudmusic/watchdog/registry.go`、`player/kugou/watchdog/watchdog.go`
（测试另见 `telemetry/compatshim_test.go`、`audiocap/wasapi/wasapi_test.go`）。
**`config/` 与 `logger/` 内禁止出现；`i18n/` 内只许出现在带 `windows` 构建约束的文件里（现为 `detect_windows.go`），且 `!windows` 侧须有同签名的桩（`detect_other.go`）**（见「最高约束」§0）。

### 13.3 目录树（对 HEAD `0654b41` 核实）

```
├── main.go                     # 入口：启动、自动更新、每日在线心跳、播放器调度、事件路由主循环
├── updatedecision_test.go      # 版本比较与自动更新决策（semver 归一化、发布版判定、-force、decideUpdate）
├── heartbeatinert_test.go      # ✅ AST 门禁：心跳函数体内禁止出现更新/退出调用
├── clientwiring_test.go        # ✅ 接线门禁：版本检查请求真的带着客户端标注
├── pingdate_test.go            # 心跳跨日判据（含 UTC+8 用例）
├── configecho_test.go          # /service-status 的 config / config_effective / config_overwritten 组装
├── config/config.go            # YAML + CLI flag + 默认值三层合并；RegisterPlayer 注册表
├── logger/logger.go            # 统一日志包（5 级别）；无文件 sink，全部走 stderr
├── i18n/                       # ★ 运行期中英文案切换；纯 Go，config/、logger/ 依赖它，须 GOOS=linux 可构建（§0）
│   ├── i18n.go, errorf.go      # SetLanguage / T / Errorf
│   ├── messages*.go            # 「中文原文 → 英文」的 english 表
│   ├── detect_windows.go       # DetectSystemLanguage（显示语言探测，//go:build windows）
│   └── detect_other.go         # DetectSystemLanguage 的 !windows 桩，仅供 Linux 构建
├── telemetry/                  # ★ Sentry 故障上报（dsn 由 ldflags 注入，空 DSN 自动禁用）；sysinfo.go 引入 x/sys/windows，仅 Windows 构建
│   ├── telemetry.go            # Init / Enabled
│   ├── panic.go                # Guard：捕获 → 上报 → Flush → 原样 re-panic
│   ├── report.go               # ReportOnce：「预期之外」事件的低频上报
│   ├── privacy.go              # PrintPrivacyNotice：启动隐私提示
│   ├── sysinfo.go, sysscope.go, appinfo.go, userip.go   # 系统信息、程序自身信息、来源 IP 的采集与挂载
│   ├── goroutineguard_test.go  # ✅ AST 门禁：出货代码每个 goroutine 入口都挂 Guard（§1.2）
│   └── guarddirect_test.go     # ✅ AST 门禁：Guard 只能 defer 直接调用（其余门禁见 §4）
├── clientid/                   # 匿名客户端标识（MachineGuid 单向哈希）+ 版本检查请求头
│   ├── clientid.go             # ID()/derive()；★ 只读注册表，不写任何键
│   └── headers.go              # HeaderNames() —— 隐私提示门禁的真源
├── player/
│   ├── player.go               # Player 接口、BaseEmitter、公共类型、ClampFloat32
│   ├── cover.go                # 公共封面下载（HTTP → base64）
│   ├── callback_lint_test.go   # ✅ AST 门禁：全仓 NewCallback 只准出现在包级 var
│   ├── krc/krc.go              # ★ 酷狗 KRC 明文解析（ParsePlainKRC：字级时间轴 + 内嵌翻译/音译轨），kugou 与 sodamusic 共用
│   ├── wesing/                 # 全民K歌 —— 内存读取
│   │   ├── wesing.go
│   │   ├── proc/memory.go      # Windows API 封装：进程/内存/AOB/窗口枚举
│   │   └── lyric/{finder,reader,timer,songinfo}.go   # plausibleplaytime_test.go 覆盖 IsPlausiblePlayTime
│   ├── cloudmusic/             # 网易云 v3 —— CDP
│   │   ├── cloudmusic.go
│   │   ├── cdp/client.go       # CDP WS 客户端、JS 求值、React Fiber 遍历
│   │   ├── lyric/fetch.go      # 网易云 API + LRC/YRC 解析 + MergeTlyric
│   │   ├── effect/effect.go    # ★ 特效 canvas 纯层捕获（注入脚本直读像素，严格只读）
│   │   ├── park/park.go        # ★ 主窗口移出屏幕保活 + 崩溃兜底还原（Windows-only）
│   │   └── watchdog/{process,registry,version}.go   # 保活标记重启、注册表自启项修补、按 exe 版本号识别 v2/v3
│   ├── kugou/                  # ★ 酷狗 —— CDP（libcef 打补丁）
│   │   ├── kugou.go
│   │   ├── cdp/client.go
│   │   ├── lyric/lyric.go      # hash/关键词双路搜索 + 时长匹配
│   │   └── watchdog/watchdog.go  # CheckPatchStatus / RunPatchHelper / 选址 / 版本读取（有 _test）
│   ├── qqmusic/                # QQ音乐 —— 进程内存扫描（运行时不写，但句柄为 PROCESS_ALL_ACCESS，见 §7.1）
│   │   ├── qqmusic.go, mem.go, api.go, qrc_decrypt.go
│   │   └── *_test.go           # 按关注点命名（§9），如 qrcdecrypt_test.go、getfileversion_test.go
│   └── sodamusic/              # ★ 汽水音乐 —— CDP（复刻 _debugProcess 开 9229）+ 明文 KRC；见 §7.7
│       ├── sodamusic.go
│       ├── translation.go      # translations.cn 独立译轨按时间戳（±10ms）对齐进 SubText
│       ├── roma.go             # 从酷狗借音译，按主歌词文本对齐补入 RomaText
│       ├── cdp/client.go       # 连主进程 Node inspector，executeJavaScript 桥进 rendererMain 取 sharedState
│       └── watchdog/watchdog.go  # 找主进程 pid + 复刻 process._debugProcess 激活 inspector
├── server/
│   ├── server.go               # 订阅者管理、状态缓存、广播分发、声明式路由表
│   ├── router.go               # 多播放器优先级路由 + 超时状态机
│   ├── types.go                # WSEvent / HTTPResponse / OrderedMap / player 类型别名
│   ├── dedup.go                # ★ hashEventData：FNV-1a 内容比对去重（取代时间窗口抑制）
│   ├── effect.go               # ★ effectHub：特效帧广播、策略、参数、状态 JSON
│   ├── audio.go                # ★ audioHub：音频帧扇出，root 经 SetActivePlayer 钩子跟随 active
│   └── race_test.go            # ✅ HTTP 端点锁内取快照的竞态门禁
├── audiocap/                   # ★ 实时音频波形（可降级附加能力，§1.1）
│   ├── wasapi/                 # 进程级 WASAPI loopback 捕获器（手搓 COM vtable；Windows-only）
│   ├── helper/                 # helper 子进程本体（★ stdout 只写帧）
│   ├── manager/                # 主服务侧门控 / Job Object / 退避 / 看门狗 / 解复用（★ 本地 recover）
│   ├── procscan/               # 进程快照、MainPID、播放器名 → 进程名（新增播放器要改）
│   └── dsp/, frame/            # RMS + 频谱、帧编解码 —— 纯 Go，Linux 可测
├── taskbar/                    # 控制台任务栏进度条子系统
│   ├── taskbar.go              # 跨平台抽象；OSC 9;4 + conhost 后端注入点（非 Windows 空实现）
│   └── taskbar_windows.go      # ITaskbarList3 COM + FlashWindowEx（//go:build windows）
├── tools/                      # ★ 全部为本地开发/CI 工具，非出货路径；各自用途见 13.4
│   └── {genconfig,winicon,devserver,cdpexplore,parktest,watchdogtest,docsample,wsrecord,kugouprobe,qqprobe,qqromaprobe,sodaprobe,wesingprobe}/
├── doc/                        # openapi.yaml、API_RESPONSE_EXAMPLES.md、core.md（滞后点见 13.6）
├── docs/                       # ★ 设计与实施计划（音频波形）；历史记录，与代码分叉时以代码为准
├── build-assets/winicon/masters/{metabox5,metabox10}-sqr.png # 图标母版（5=发版/默认紫，10=dev）
├── build-assets/winicon/README.md # 图标子系统说明
├── build-assets/{metabox5-sqr.ico, metabox10-sqr.ico, metabox5.svg} # 未被引用的旧图标源文件，仅归档（4faaae2）
├── .github/workflows/          # build-windows / release / sync-source-to-dotcom / purge-release-cache-on-latest-change（见 §8、§12）
├── resource_windows_amd64.syso # ★ 已入库；由 tools/winicon 从母版5生成的多尺寸变体
├── config.yml, effect_page.html, effect_display.html, lyric_page.html, lyric_display.html
└── .gitattributes              # 全局 eol=lf + *.syso binary（注释是防回退的，别删）
```

★ = instruction.md 原目录树完全没有的条目；其中 `i18n/`、`telemetry/`、`player/krc/`、`player/sodamusic/` 等非音频条目对 `3f22ccc` 逐项核实补入，`audiocap/`、`server/audio.go`、`docs/` 三处对 `326e1cc` 补入。

### 13.4 tools/ 子命令

| 子命令 | 干什么 | 谁在跑 |
|---|---|---|
| `tools/genconfig` | 把 `config.DefaultConfigContent()` 写成 clean 的 config.yml，使发版不受开发者本地 config.yml（park 等个人参数）影响 | **CI**，Linux runner 原生 `GOOS= GOARCH= go run`（`release.yml`、`build-windows.yml`） |
| `tools/winicon` | 从高分母版 PNG 生成多尺寸 Windows 图标 `.syso`（16..256 每档 Lanczos3 缩 + unsharp 锐化 → 多尺寸 .ico → goversioninfo 带版本信息）。纯 Go 跨平台，Linux 出 win/amd64 资源 | **CI**，两个 workflow 构建前 `GOOS= GOARCH= go run`；也可本地跑（见 §13.5） |
| `tools/devserver` | 只起 server + 特效捕获器的最小组合（不启其他播放器，避开 watchdog/提权副作用）；`:8766` 控制口 `GET /active?p=` 切活跃播放器 | 人（本地） |
| `tools/cdpexplore` | 一次性 CDP 探针：`eval <jsfile>` / `evals "<expr>"` / `screencast <n> [ms]` | 人（本地） |
| `tools/parktest` | 手测 park 包：`park` / `unpark` / `restore` / `list` / `status` | 人（本地） |
| `tools/watchdogtest` | 调 `watchdog.EnsureDebugMode()`，测「网易云未带保活参数 → 杀掉重启注入」冷启动路径 | 人（本地） |
| `tools/wsrecord` | 同时连多个 WS 端点，把收到的事件带统一时间原点原样录成 jsonl；文档示例的真实流量来源（issue #43） | 人（本地） |
| `tools/docsample` | 校验 `doc/API_RESPONSE_EXAMPLES.md` 与 `doc/openapi.yaml` 示例里的每个数值都能在 wsrecord 录音中找到，挡住编造的示例值：`-rec <录音目录> <文档...>` | 人（本地）；`yaml_test.go` 随 `go test ./...` 跑 |
| `tools/kugouprobe` | 直连酷狗歌词 API 端到端验证 KRC `type=0` 音译接线，打印每行 text/sub_text/roma_text；只读 | 人（本地） |
| `tools/qqprobe` | 探测 `knownVersions` 尚未收录的 QQ 音乐版本的内存偏移，输出可粘进 `player/qqmusic/mem.go` 的 `dllOffsets` 字面量；句柄只申请读权限。仅 Windows 构建 | 人（本地） |
| `tools/qqromaprobe` | 连 QQ 音乐取当前歌 songID + cookie，复刻 `musicu.fcg`（roma=1）打印 lyric/trans/roma 原始形态，用于判定音译格式；只读。仅 Windows 构建 | 人（本地） |
| `tools/sodaprobe` | 连汽水主进程 inspector 取明文 KRC 经 `krc.ParsePlainKRC` 解析打印，验证是否内嵌 `type=0` 音译轨；只读。仅 Windows 构建 | 人（本地） |
| `tools/wesingprobe` | 全民K歌音译歌词的内存探测：dump 指定行的 LyricEntry 与 CharElement；只读。仅 Windows 构建 | 人（本地） |
| `tools/audioprobe` | WASAPI 进程级 loopback 可行性探针（`903c762`），现复用 `audiocap/wasapi`：对指定 PID/exe 捕获 N 秒、周期打印 RMS，可选落 WAV | 人（本地）。**会真的对声卡做 loopback，短跑**（§9） |

**`tools/genconfig` 是打包流水线的编译期依赖，绝不能让它变成 Windows-only。** 其内部依赖恰为
`config/`、`logger/`、`i18n/`（`GOOS=linux go list -deps ./tools/genconfig` 实测）。❌ 靠人（自检：
`GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./i18n/... ./tools/genconfig`）。

### 13.5 图标 .syso —— 构建期从母版生成，别再手搓单尺寸

exe 图标由 Go 链接器自动链接根目录 `resource_windows_amd64.syso`。唯一真源 =
`build-assets/winicon/masters/` 的高分母版（2021×2021）+ 生成器 `tools/winicon`
（详见 `build-assets/winicon/README.md`）。**已不再有 `winicon/{main,release}/*.syso` 静态副本。**

| 环境 | 母版 | 版本 | 图标怎么来 |
|---|---|---|---|
| 发版（`release.yml`） | `metabox5-sqr`（紫） | tag 版本 | 构建前 `GOOS= GOARCH= go run ./tools/winicon` 生成 |
| main dev（`build-windows.yml`） | `metabox10-sqr` | `0.0.0` 占位 | 同上（保留 dev≠release 图标区分） |
| 本地 `go build .` | 根目录已入库的 syso（母版5生成，version 0.0.0） | — | 无人；Go 自动链接 |

**为什么改**：旧做法是手搓一张 ~400px **单尺寸** .ico → .syso；小尺寸场景（任务栏/资源管理器
列表 16–32px）由 Windows 在**显示时**劣质降采样 → 细线条 logo 锯齿。现按 electron-builder 思路
在**构建期**从母版把全套 DPI 尺寸（16/20/24/32/40/48/64/96/128/256，其中 20/40 专为 125%/250%
的标题栏小图标）各自用 Lanczos3 缩好、再补 unsharp 锐化后打包，Windows 直接取对应尺寸那张。
校验：解析 exe PE 的 `RT_GROUP_ICON` 应声明 **10 档**；只有一档是退回锯齿老路，缺 DPI 中间档
（如 250% 缺 40）则高分屏标题栏发糊。小尺寸偏软则调 `-filter`/`-sharpen`（默认 lanczos3 + 0.6）。

**改图标别忘**：两个 workflow 的图标步骤都依赖 `tools/winicon` + 对应母版——删/改母版或生成器要
同步二者。换 logo 后本地重生成根 syso（命令见 winicon README）并提交；`.gitattributes` 的
`*.syso binary` 保留。❌ 靠人。

### 13.6 doc/ 文档的已知滞后点（具名）

| 文档 | 确定滞后点（已实测） |
|---|---|
| `doc/openapi.yaml` | **API 文档的唯一真源**（线上 apifox 是它的手动导入产物，是下游副本、不是权威）。曾缺的 kugou / effect 已补齐（issue #43）：player enum 五家齐全、`/service-status` 端点表含 effect-ws 与 audio-ws。唯 effect-ingest 有意不入文档（内部写端，见 §14），非滞后 |
| `doc/API_RESPONSE_EXAMPLES.md` | 仅为离线快速参考。曾记的「整个 kugou 缺失」已不成立（kugou 与 sodamusic 均有示例） |

**加端点/加播放器时必须同步 `doc/openapi.yaml`，绝不改线上 apifox 当作数。** apifox 是手动导入的
副本，改它不会回流；两边分叉时以 openapi.yaml 为准。❌ 靠人。

### 13.7 声明式路由表 = 新增播放器零代码注册

`server/server.go` 的 `Start()` 中的 `routes []routeDef` 是**设计意图，不是巧合**（instruction.md 从未写出
这点，反而错记为「router.go 负责端点注册」）:

```go
// 声明式路由表：新增播放器零代码注册
routes := []routeDef{
    {"/ws", "ws", nil, nil},
    {"/all_lyrics", "http", s.handleAllLyrics, nil},
    ...
}
```

注册链路：端点由 `main.go` 的 `playerNames` 派生 —— `NewServer(playerNames)` 建 `playerStates`
→ `Start()` 对根路径注册一遍、再对每个 `playerStates` key 注册一遍前缀路径。播放器包 `init()` 里的
`config.RegisterPlayer(PlayerName)`（五家播放器各一处）只管配置维度，不参与端点注册（见 §5.1 ②）。

**新增播放器时绝不手写 `mux.HandleFunc`，只做 §5.1 的接入步骤（`RegisterPlayer` + 追加 `playerNames`）。**
手写会绕过 per-player 前缀的对称性，导致新播放器只有根端点、没有 `/<name>/*`。❌ 靠人。

**新增端点类型时必须同时改 `registerRoutes` 里的 `switch rd.kind`**，现有四种 kind
（`http`/`ws`/`sse`/`audio`）之外的值会被静默忽略——switch 无 default 分支。❌ 靠人。

仅根路径的例外端点（不进表、无 per-player 版本）：`/health-check`、`/service-status`、
`/cloudmusicv3/effect-ws`、`/cloudmusicv3/effect-ingest`（均在 `server.go` 的 `Start()` 里直接注册）。

---

---

## 14. 已知并接受的风险 / 未决事项

### 已知并接受（**有意的，不是待办**）

**effect-ingest 写端有 token、无 Origin 校验。** `/cloudmusicv3/effect-ingest` 写端要求进程内
一次性随机 token（`?t=`，`crypto/rand` 16B、仅内存不落盘；`handleEffectIngest` 在 Upgrade 前用
`crypto/subtle` 常量时间比较、不匹配即 403）。仍共用恒真的 `CheckOrigin`、无回环校验、默认绑
`0.0.0.0:8765`——**写端安全靠 token，不靠 Origin**：WebSocket 不受同源策略约束，局域网设备或主播机
网页能发起连接，但没有 token 过不了鉴权。token 明文存在于注入网易云页面的抓帧脚本里，能读该进程
内存或网易云 CDP/页面者仍可提取——但具此能力者本就能自行抓同一 canvas，无新增暴露。

- **前提**：token 防的是**本机其它软件**（能连端口、伪造 Origin、读用户文件，但无 CDP/页面 JS 权）。
  **多机部署 / 公网暴露 / 播控机在不可信网络时必须重新评估**（0.0.0.0 绑定 + token 明文在页面脚本里）。
- **未保障的假设**：`effect.go` 的注释称「单一生产者（注入脚本只开一条），故不做并发保护」
  ——**没有任何机制保证它**；token 通过后的第二条 ingest 连接进来时行为**未定义**。
- **进程重启后 token 会变**：注入脚本 `captureInjectJS` 有热接管（`__mbxCapV2` 版本门 +
  `window.__mbxCapSetIngest` 切地址、不新起 rAF 循环）；升级前注入的旧脚本无法热接管，靠网易云
  reload/切歌重建 canvas 恢复（一次性升级过渡瑕疵，不为它引双 loop 或 reload 网易云）。
- **`/service-status` 不再广告 ingest 地址**（端点仍在，注入脚本经 `EffectIngestWSURL()` 内部取址）。
- **硬约束：别照着「去掉 `CheckOrigin`」改，会炸。** 注入脚本跑在 `orpheus://` 源上，恢复 gorilla
  默认的 `checkSameOrigin` 会比较 `orpheus` vs `127.0.0.1:8765` → 403 → ingest 断，OBS 浏览器源
  也可能一起断。**这条恒真的 CheckOrigin 对两条正路都是承重的。** 收紧写端的正确做法是像现在这样
  在 `handleEffectIngest` 里前置 token 校验，不要动共用的 upgrader。

### 未决

| # | 事项 | 卡在哪 |
|---|---|---|
| 1 | CI 无任何 `pull_request` 触发器、无质量门禁 | 现有 Linux runner 只能覆盖 §8 表中 exit=0 的那一行（`server` 已因挂 Guard 移出）；`-race` 在 Windows 开发机上即可跑，不再依赖它。现行补偿是 §3.5「打 tag 前在 Windows 上全跑」，靠人 |
| 2 | Windows-only 包的测试 CI 永远跑不了 | 只有 self-hosted Linux runner，Windows runner 留给别的项目。**如实记录，别假装有覆盖** |
| 3 | 持续 WASAPI 进程 loopback 与专业声卡驱动蓝屏的关联 | 2026-09-30 开发机（RME Digiface）蓝屏、报 RME 驱动；持续捕获是候选诱因之一，未证实也未排除。主播机器同样可能用这类声卡，现有缓解只有按需门控（§3.6）。**无法稳定复现，卡在无法定性** |
| 4 | 同一播放器多实例时只捕获 `MainPID` 选中的一个，可能与歌词侧跟踪的实例不一致 | 歌词侧 PID 是会话级局部变量、无 getter；共享判据是更大的接缝，未做（`docs/audio-waveform-capture-design.md` §10 已知限制） |
