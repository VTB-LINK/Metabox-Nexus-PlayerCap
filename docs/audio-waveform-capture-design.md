# 音频波形捕获 — Phase 1 设计文档

状态：设计 v2，已实施并合入 main。本文为设计记录，与代码分叉时以代码为准。日期：2026-09-30。
可行性：已验证（`tools/audioprobe`，commit `903c762`，纯 Go WASAPI 进程级 loopback 实测对 QQMusic 抓到非零 RMS）。

本文风格遵循 `AGENTS.md`：严肃、准确、简洁；行号带符号名做冗余（行号会漂，按符号找）。
v2 修订依据：五角度对抗式评审（稳定性/正确性/一致性/过度设计/缺口），采纳项已并入正文。

---

## 0. 背景与范围

歌词样式编辑器改造附带一个**实时音频波形**，作为 OBS overlay 上的一种「播放器样式」视觉元素（不是歌词打轴对齐工具）。波形跟着音乐实时脉动，反映**当下真实输出**的音频。

**分期：**

- **Phase 1（本文）**：端到端**音频捕获 + 交付管线**——从每个在跑播放器的进程级 loopback，到主服务，到 `/audio-ws` 端点，产出一条定义好的**自包含帧流**。含捕获、helper 隔离与生命周期、路由拓扑、帧契约、稳定性。
- **Phase 2（不在本文）**：前端渲染（实时频谱条 + 整首滚动波形）、切换交叉淡化、ASIO/独占的探测与「不支持」提示 UX、sample/true peak 等增补 DSP、编辑器接入、前端向 npm+框架迁移、SPA 交付方式。

**质量优先级继承 `AGENTS.md` §0：稳定 > 正确 > 简洁 > 功能完整度。** 铁律：波形子系统**任何故障都不得拖垮歌词刮取与特效镜像两条主产品线**。宁可波形降级（不显示、静默零帧），绝不影响主服务。

**两个设计前提（brainstorm 共识）：**

1. **一套采集、两种渲染**：实时频谱条与整首滚动波形都从同一路捕获流出，区别仅在前端怎么画（瞬时视图 vs 时间历史「走带器」视图）。Phase 1 只产出帧流，两种渲染都是 Phase 2 拿同一路数据画。
2. **as-live、不套歌词 offset**：波形按真实输出的真实时间记账，不做前移对齐。暂停=断口、回跳=不倒带续录、切歌=接上，全部自然落地，不需源文件、不需歌曲位置。

---

## 1. 总体架构

三段，两条 IPC（stdin 控制 / stdout 数据）：

```
                     stdin(控制: capture <pid> <player> … / stop <player>)
主服务 ───────────────────────────────────────────────► 音频 helper 进程
（fan-out only，                                          （捕获 + DSP，隔离，
  本地 recover 不挂 Guard）                                 同一 exe 子命令）
      ◄───────────────────────────────────────────────
                     stdout(数据: 自包含帧，布局见 §4 末条)
  │
  │ audioHub 按 player 键 fan-out（克隆 effectHub 的 drop-oldest 二进制扇出 + player 键路由）
  ▼
/audio-ws（root，跟随 activePlayer）   /<player>/audio-ws（固定那一家）
  │                                       │
  ▼                                       ▼
        OBS 浏览器源 overlay（Phase 2 渲染）
```

**与特效通路（`server/effect.go`）的类比是骨架**：特效通路里注入脚本算好 JPEG 帧、主服务只广播不解读。音频通路同构——**helper 算好音频帧（DSP 全在 helper），主服务只解出 player 做路由（实现为整帧解码后用同一 `frame.Encode` 重编码），不读 RMS/bins 做任何处理**。把所有音频处理（捕获 + DSP）关进隔离的 helper，主服务（直播关键进程）只做平凡的按键转发，最大化 §0 隔离。

**关键差异**：特效 hub 是单命名空间（只服务 cloudmusicv3），音频要 root（跟随 activePlayer）+ per-player（固定）两种拓扑。audioHub 是**混血**：借 effect 的 drop-oldest 二进制扇出 + 借 subscriber 系统的 player 键路由。

**主服务侧 panic 隔离（承重，评审 blocker）**：isolation 只把 helper 侧崩溃挡住了，但主服务侧仍跑三段自有代码（stdout 读帧+解复用、捕获管理器、stdin 下发）。这些 goroutine **绝不挂 `telemetry.Guard`**（Guard 会 re-panic 杀主进程 = 直播事故），而用**本地 recover**（记日志 → 触发 helper 退避重启 → 继续降级）。这是对 `AGENTS.md` §1.2「每个 go 都挂 Guard」的**有意豁免**，因为波形故障必须降级而非拖垮主线。且解复用把 helper 输出当**不可信数据**：长度前缀设上限、坏头/短读只丢帧不 panic。

---

## 2. 捕获层（音频 helper 进程）

### 2.1 进程形态与生命周期（承重）

同一 exe + 子命令，照 `--kugou-patch-helper`（`main.go:56-59` 短路入口，在 `defer telemetry.Guard()` 之后、`main()` 内一切初始化之前；包级 `init()` 仍先于它执行）：

```
if len(os.Args) >= 2 && os.Args[1] == "--audio-capture-helper" {
    helper.Run(os.Stdin, os.Stdout)   // 读控制、写帧
    return
}
```

拉起端 `exec.Command(exe, "--audio-capture-helper")` + `StdinPipe`/`StdoutPipe`，**`cmd.Stderr = os.Stderr`**（helper 日志并入主控制台——§0.5 日志是直播出事的唯一现场证据，不接则全丢）。**无需 UAC 提权**。进程隔离由「独立进程」提供，不由「独立二进制」提供：helper 崩不带崩主服务；打包流水线零改动、无第二产物、无新依赖。

**孤儿防护（评审 major，本仓库首个长命子进程、零先例）**——主服务多条退出路径都 `os.Exit(0)` 不跑 defer（信号退出 `main.go:283`、`restartSelf` `main.go:785`、`ensureCanonicalName` `main.go:919`），Guard re-panic 崩溃路径同样不清理，Windows 不自动回收子进程。三重防护：

1. **Windows Job Object + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`**：父进程无论怎么死（崩溃/os.Exit/被 kill），内核随之回收 helper。这是主防线。
2. **helper 在 stdin EOF（父进程关闭管道）时立即自杀退出**——写进控制协议契约，作 Job Object 之外的兜底。
3. **启动清扫**：主服务启动时清扫遗留的 audio-helper 进程（照 `park.RestoreOrphaned` 先例，`main.go:267`/§1.2 step 14），防上次异常退出的遗留。

**可观测性（评审 major，helper 是全项目最深盲区）**：helper 子命令短路早于 `telemetry.Init`（`main.go:117`），故 helper 内须**独立调 `telemetry.Init`**（DSN 是编译期注入的包级变量，helper 与主服务同一 exe，无需传递）+ `defer Guard`。**COM 回调内的错误跨 C/COM 边界，Go recover 接不到 → 硬崩**：故 WASAPI/回调层的错误一律以**返回码**上抛，记 stderr 后结束该路捕获，不靠 panic；stdout 只有帧。

### 2.2 捕获机制：纯 Go 进程级 WASAPI loopback

复用 `audiocap/wasapi/wasapi_windows.go` 的已验内核（spike 原在 `tools/audioprobe`）：`ActivateAudioInterfaceAsync` + `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK` + 手写 `IActivateAudioInterfaceCompletionHandler` 回调 vtable（要点：VT_BLOB=`0x0041`、结构体扁平化手排偏移、`NewCallback` 全部包级 var 无捕获合 §0.4、pin map 钉 GC、`KeepAlive`、超时 ABANDON 不 UAF；详见该文件注释）。

- 格式硬编码 48000Hz/2ch/float32（进程 loopback 虚拟设备 `GetMixFormat` 返回 `E_NOTIMPL`；引擎把播放器侧任意采样率/位深/声道转成这个格式——**播放器「输出格式/频率转换/输出声道」怎么设都无所谓**）。
- **`INCLUDE_TARGET_PROCESS_TREE` 是硬需求**：Chromium 系（cloudmusic/kugou/sodamusic）真正出声的是 **audio utility/service 子进程**（`--type=utility --utility-sub-type=audio`，主进程无 `--type=` 的那个的子进程），不是主进程本身。抓主进程 + 整棵树才覆盖到出声子进程。**Phase 1 需逐家实测**两点：① 主进程 + tree 确实覆盖出声子进程；② **先激活 loopback、后开始播放**时，晚生成的音频子进程能否被 tree 动态纳入。（结果见 §9）
- **反复 activate/release 对称释放（评审 minor，spike 只走过一次性抓取）**：Phase 1 会因订阅增减/切歌/播放器重启在同一长命 helper 内反复 activate/release。每次 release 必须彻底释放 WASAPI 客户端（Stop/Release）、completion handler、**pin map 条目**、`KeepAlive` 边界，否则几小时直播里句柄/COM/pin map 单调增长。

### 2.3 DSP 在 helper（Phase 1 只出 RMS + 全频谱）

helper 捕获 PCM 后就地算帧、只把帧写 stdout；主服务零音频处理（§0 隔离：DSP 的 CPU 与崩溃风险全在 helper）。

- **Phase 1 帧只含 RMS + 全频谱 bins**。RMS 已足够驱动基础脉动；全频谱原样送、前端自己选频段做 high/low-pass。
- **sample peak / true peak 推迟到 Phase 2**（评审 YAGNI + 正确性）：视觉波形肉眼不辨 inter-sample 峰值；且合规 true peak 必须用 BS.1770 多相 FIR 插值系数做 ≥4× 过采样，naive 过采样≠真峰、字段名对值不对反误导前端；它还是最贵 DSP × 每路 × cadence。帧 payload 可版本化，Phase 2 渲染器真需要再补字段成本低。

### 2.4 控制协议（主 → helper，stdin）

行式命令：`capture <pid> <player> <norm:0|1> <target_dbfs> <maxgain_db>` / `stop <player>`（后三字段由[音量归一化设计](audio-waveform-normalization-design.md) §3.4 扩入）。helper 据此增删捕获路，纯执行者（不做进程发现、不做订阅/播放判断——策略全在主服务）。**cadence 是编译期常量（~30fps），Phase 1 无运行时改 cadence 命令**。stdin EOF 即退出（见 §2.1）。

### 2.5 helper 内部：stdout 只写帧 + 采集与写解耦（承重）

- **stdout 只承载帧、所有诊断一律走 stderr（红线）**：移植 audioprobe 内核时必须清除/改向其全部 `fmt.Printf`→stdout 的打印，否则任何杂散字节冲垮长度前缀分帧、主服务解析 desync/OOM/崩。分帧协议带**同步魔数**（布局见 §4 末条），主服务解析出界时可重新对齐——把污染降级为「丢几帧后自恢复」而非「解析器卡死」。
- **采集排空与 stdout 写解耦（评审：helper→主背压是唯一未分析方向）**：helper 的采集是「醒来即排空所有 packet」的循环，若 stdout 写阻塞（主服务读端停顿或 Windows 管道 ~64KB 满）会卡住排空 → WASAPI 环缓冲溢出。故 helper 内用一个小的**有界 channel + drop-oldest** 把「采集排空」与「写 stdout」解耦；写端是单 goroutine 阻塞写，阻塞只卡它自己——父进程停顿或死亡时排空循环永不被冻住。

---

## 3. 交付层（主服务）

### 3.1 捕获管理器：进程发现 + 门控 + 预热

- **进程发现**：用 `CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)` 一次快照拿进程名（`x/sys/windows` 已在），**比 gopsutil 逐进程 `Name()` 便宜得多**（`park.go` 有「`process.Processes()` 很贵、需缓存」的明确先例）。**不碰 5 个播放器包**：5 个目标进程名尽量走**单一真源**——能导出的（含把 kugou 的未导出 `targetProcess` 导出成 `TargetProcessName`）就 import 复用；实在要集中 hardcode，则加一个测试断言音频侧副本与各播放器包常量 `EqualFold` 相等，防两份真源分叉。
- **门控 + 预热（承重，评审 major：root 门控映射 + 切换空窗 + 按需节能）**。玩家 X 被捕获，当且仅当：

  ```
  进程 X 在跑  AND  ( 存在 X 的 per-player 订阅  OR  ( 存在 root 订阅 AND ( X 是 activePlayer OR X 正在播放 ) ) )
  ```

  - **按需**：无任何 `/audio-ws` 订阅者 → 不扫描、不捕获、helper 拆除（懒汉）。端点无连接即空转节能（用户要求）。
  - **root 预热消除切换空窗**：root 跟随 activePlayer，而路由切到 B 正因为 B 开唱。若只抓 active，切到 B 时才起 B 的捕获 → WASAPI 激活延迟造成 root 死-snap。故 root 有订阅时**抓所有『正在播放』的播放器**（读 `playerStates`/`GetPlayerStatus` 判在播——这是主服务侧状态、非帧内数据），谁被选为 active 都已热 → root 帧到帧无缝接管。稳态只 1 个在播=只抓 1 路，仅切换重叠瞬间短暂 2 路。
  - **切换即时重新门控**：`activePlayer` 变更（§3.3 钩子）时**唤醒管理器立即重算门控**（而非等下一 tick），钩子本身只唤醒、不在 `r.mu` 内做捕获动作（§3.4）。
- **扫描节流**：tick 用**秒级保守间隔**（如 1–2s，不亚秒），复用/缓存 PID、只在 diff 时动作。扫描本身也受「有无 `/audio-ws` 订阅者」门控。
- **PID 复用 TOCTOU（评审 minor）**：扫描拿 PID 与 helper 激活之间目标可能退出、PID 被复用。交 PID 前用一次新快照**复核该 PID 仍在且进程名匹配**（`procscan.Alive`）。未比对创建时间：Toolhelp 快照不含创建时间，逐进程取需 `OpenProcess`，Phase 1 未做。
- **并发上限与高负载退化**：并发捕获软上限（root 预热在播集合 + 被订阅的 per-player）；高负载下允许 cadence 降级（如降到 15fps）作退路——波形降帧远优于抢主线 CPU。（软上限与降帧均未实现。）
- **编排范式**照特效捕获器（`main.go` `effect.New(...)` + `go func(){ defer telemetry.Guard(); ... }`）——但注意音频侧读/解复用 goroutine 用**本地 recover 不挂 Guard**（§1）。

### 3.2 audioHub（克隆 effectHub + player 键路由）

新建 `server/audio.go`，克隆 `server/effect.go` 结构：

- `audioSub{ ch chan []byte; player string }`（比 effectSub 多 `player` 键：root sub 为 `""`，per-player sub 为固定名）。
- `audioHub{ mu sync.Mutex; subs map[*audioSub]struct{}; active string }`（`active` 为 §3.3 钩子写入的 activePlayer 缓存；剥掉 effectHub 的截帧参数负担）。
- 通道缓冲 **8**、**drop-oldest**（照 `BroadcastEffectFrame` `effect.go:183-203`）。**主服务侧一切发送非阻塞**（§2.1 承重不变量）。
- `BroadcastAudioFrame(player string, frame []byte)`：发给该 player 的 per-player sub；**若 `player == h.active`（读 h.mu 缓存），发给 root sub**。切勿在持 `h.mu` 时回读 `s.activePlayer`（`s.mu`），以免 `h.mu→s.mu` 反向锁序。
- `HasSubscribers(name string) bool` = 存在 name 的 per-player sub **OR**（存在 root sub **AND** `name == h.active` 缓存）。这是「root 只连时也要抓 active」的关键，缺了 OBS 主用法永远收不到帧。
- `handleAudioWS(playerName string)`：照 `handleEffectWS`（`effect.go:328-414`）订阅生命周期——Upgrade（共用 `s.upgrader`）→ 登记 → writer goroutine（**写失败必须显式 `conn.Close()`**，否则读循环永阻塞、sub 成僵尸）→ read loop 保活 → 清理（`delete`→`close(ch)`→`Close`→`<-done`）。

`Server` 加 `audioHub *audioHub`（`server.go:99` 旁），`NewServer` 构造 `audioHub: newAudioHub()`（`server.go:120` 旁）。

**主服务侧读/解复用 goroutine 用本地 recover、不挂 Guard；helper 输出当不可信数据**（长度上限、坏帧丢弃、按 magic 重对齐），见 §1、§2.5。

### 3.3 路由拓扑 + 端点注册

- **root `/audio-ws` 跟随 activePlayer**：`activePlayer` 唯一权威是 `Router.activePlayer string`（`router.go:46`），经 `SetActivePlayer`（`server.go:334-343`，所有变更的唯一漏斗、边沿触发）镜像。在 `SetActivePlayer` 的 `if changed` 块（`server.go:339-342`，与既有 `BroadcastEffectStatus` 并列）加钩子 `s.audioHub.onActivePlayerChanged(name)`。
- **per-player `/<player>/audio-ws` 固定**：连接时把 player 名焊死进 sub，转发只比名字。
- **端点注册**：`/audio-ws` 以 kind `"audio"` 进 `server.go` `Start()` 的声明式 `routes` 表，由 `registerRoutes` 对根（`playerName == ""`，跟随 active）与每个播放器（固定那一家）各注册一遍。最初照 effect 硬编码、手写 per-player 循环实现（`52a4622`），后经 `b46e441` 收进 routes 表。
  `/audio-ws` 共用 `s.upgrader`（`NewServer` 里，恒真 `CheckOrigin`，多端点共用）——**绝不为「修安全」改它**，否则 ingest 与 OBS 源一起断。

### 3.4 SetActivePlayer 钩子的三条戒律（承重）

钩子跑在**持 `r.mu` 写锁的同步 router Run 循环**上（`router.go` `range merged`→`updateRouting`→`switchTo`→`SetActivePlayer`；`watchExpire` goroutine 亦然）：

1. **收缩成单个受 `h.mu` 保护的字段写**（`h.active = name`）+ 唤醒捕获管理器（非阻塞，如向一个带缓冲/select-default 的 channel 发信号）。root 路由判定挪到 `BroadcastAudioFrame` 在音频读 goroutine 上读缓存——**r.mu 主线上不跑任何扇出/分配**。
2. **不得回调 Router**（`r.GetActivePlayer()` `router.go:489` 取 `r.mu.RLock` → 同 goroutine 重入死锁）。要用就用传入的 `name`。
3. **不得 panic、不得阻塞**：这是音频代码唯一在 r.mu 写锁主线上执行的点，一旦 panic/阻塞就沿主线拖垮或经 Guard 杀进程。钩子平凡到不可能 panic。

锁序 `r.mu → h.mu`（`SetActivePlayer` 调钩子前已释放 `s.mu`），无环。

---

## 4. 帧契约（frame contract）

**自包含帧**（drop-oldest 只保最新，丢帧无害）。

- **helper→主 stdout 分帧**：自包含帧以 Magic 开头供 desync 重对齐（§2.5），布局见本节末条。主服务按帧内 `player` 解复用做路由。
- **payload（发往客户端）字段**：时间戳 + **RMS** + **全频谱 bins**（Phase 1；peak 系列 Phase 2 增补）。
- **客户端可见的 epoch/player 标识（承重，评审 minor→为「走带器」正确性提级）**：payload 头保留一个轻量 `player + epoch`。epoch 是 helper 内的全局计数，每路每次成功激活自增一次：root 切换时帧内 `player` 改变、`epoch` 随之换成新那一路的值；同一 player 在 helper 内重新激活，epoch 也变；跨 helper 进程重启后从 1 重新计数、可能与重启前的值相同，此时断点靠 QPC 时间戳（系统单调、不倒退）区分。前端据此识别断点：Phase 2 的「走带器」时间历史视图据此**另起走带段**（不把 B 的波形无缝错拼到 A 的时间线），并可做切换交叉淡化。
- **时间戳语义**：用**系统级单调源（QPC）**，而非进程起点相对——否则 helper 重启后单调钟归零、时间戳倒退，走带器无法处理。
- **cadence**：编译期 ~30fps（§3.1 的高负载降帧未实现）；仅在有订阅者时计算（§3.1 门控）。
- **空闲帧**（`d064e59`）：root 有订阅且无 activePlayer 时，Manager 每 100ms 向 root 推 `player=""`、epoch 为 0、RMS 为 0、无 bins 的帧（时间戳同为系统 QPC），使纯 `/audio-ws` 消费者无需订阅 `/ws` 也能判清空。有 active 但捕获未就绪或降级时可能无帧，判活不应仅凭有无帧。
- 字节布局以 `frame.Encode` / `frame.Decoder` 的实现为准，对外文档为 `doc/openapi.yaml` 的 `/audio-ws`；频谱 bins 为 512 个（1024 点 FFT、周期 Hann 窗，`audiocap/helper` 与 `audiocap/dsp`）。

---

## 5. ASIO / WASAPI 独占：Phase 1 只如实流出

进程 loopback 只能抓**走 Windows 音频引擎**的输出（WaveOut/DirectSound/WASAPI 共享 ✅、且引擎白转 48k/2ch/float）；**ASIO / WASAPI 独占绕过引擎，按机制推断抓到的是数字静音或什么都没有（未实测）** ❌。

**Phase 1：如实把（零值）帧流出去即可**——零帧在管线层已是诚实降级（不崩、不造假、不画平线骗人），§9 的「ASIO → 不崩」照样能验。**不在主服务侧读帧内 RMS 做探测**：那会违反「主服务不读 RMS/bins 做任何处理」（§1/§4）并给直播关键进程加处理面。

**推迟到 Phase 2**：「playing 且持续静音 → 判 ASIO/独占 → 明确提示」的探测与 UX。届时须定义静音窗口长度、RMS 阈值、**滞回与恢复**（真实播放的安静前奏/尾奏/氛围段也 RMS≈0，绝不允许在安静段翻成「不支持」并锁死），并考虑 playing 状态本身领先/滞后真实音频的时序容差。若届时要在管线内判，判定应下沉到 helper（helper 自算持续静音、经 stdout 发一条显式「降级」控制消息，主服务原样转发，保持不读 RMS/bins）。

**不做端点 loopback 兜底**（ASIO/独占用户在这些播放器占比极小，YAGNI）。

---

## 6. 错误处理与稳定性（§0 对齐）

| 失败模式 | 处理 |
|---|---|
| **helper 崩溃** | 主服务**封顶退避**重启（1s 起翻倍、封顶 30s；连续 5 次快速失败即放弃，实际只到 8s，先稳定运行过则 16s）；**连续 N 次快速失败即放弃**，本会话把波形标降级（不再无限 spawn 烧 CPU），下次主服务重启再试。 |
| **helper 假死（非崩溃）** | 帧活性看门狗：某 player 处于 playing 且被订阅、却持续 N 个 cadence 周期收不到其帧 → 判 hang → 主动 kill + 退避重启。helper **静音也照发零帧**（有静音包时按 cadence，引擎无数据时约 100ms 一帧），使「无帧」明确等价于故障而非静音。 |
| **helper 孤儿** | Job Object KILL_ON_JOB_CLOSE + stdin-EOF 自杀 + 启动清扫（§2.1）。 |
| **helper 崩因不可诊断** | helper 内 `telemetry.Init` + Guard；COM 错误以返回码上抛、记 stderr，不靠 panic；stderr 继承父进程（§2.1）。 |
| **主服务侧音频 goroutine panic** | 本地 recover，不挂 Guard（§1）；坏帧当不可信数据只丢不崩（§2.5/§3.2）。 |
| **helper→主 管道背压** | 采集侧经有界 channel 丢最旧入队、永不阻塞；写端单 goroutine 阻塞写，阻塞只卡它自己（§2.5）。 |
| **主→客户端 慢客户端** | audioHub drop-oldest 非阻塞（§3.2，§2.1 承重不变量）。 |

其它：纯 Go / `CGO_ENABLED=0` 保持（不引入 cgo：本机 MinGW 缺 `wrl/implements.h` 的 `RuntimeClass` 与 `audioclientactivationparams.h`，cgo 又不认 MSVC，近抄参考 C++ 不成立；纯 Go 手搓 COM 回调是本仓库既有范式）；helper 的 WASAPI/COM 代码 `//go:build windows`、不被 `config/`/`logger/` import → **不触 §0.3**（`GOOS=linux` 自检保持绿）；IPC 走 stdout 管道（私有父子、无端口、无 token、不碰 `0.0.0.0:8765`），**不复用 effect-ingest 回环 WS**（那是因特效 producer 是浏览器注入 JS 只会说 WS；音频 helper 是我方 Go 子进程，走管道即可，避免继承 effect-ingest「无 Origin/回环校验、默认绑 0.0.0.0、第二条连接行为未定义」那组开放边界）。

---

## 7. 配置

Phase 1 尽量内置默认、少开阀：按需门控恒开、cadence 编译期常量。若确需开关（如全局启停、cadence、并发上限），按 `AGENTS.md` §3.3 同改（纯标量、不引 windows 符号 → 不触 §0.3）：Config 字段 + `mergeYAML` 分支（**类型断言失败必须 `log.Warn` 键名与类型**）+ 模板注释行；有非零默认再改 `DefaultConfig()`；CLI 可覆盖再加 flag 注册（+ `globalFlags` 白名单）+ `flag.Visit` 赋值分支（**绝不直读 flag 变量**）。

---

## 8. 构建 / 打包

同一 exe 子命令 → 两条流水线**零改动**（无第二产物）。无新依赖（`x/sys/windows`、`syscall`、`unsafe` 均在；进程扫描用 `CreateToolhelp32Snapshot` 亦在 `x/sys/windows`）。需补 `doc/openapi.yaml` 的 `/audio-ws`、`/<player>/audio-ws` 端点（`AGENTS.md` §3.5：openapi 是唯一真源、加端点必先补）。

---

## 9. 测试

- **helper**：结构体偏移/大小断言（`audiocap/wasapi/wasapi_test.go`）；帧编码往返；RMS 计算；**反复 capture/stop N 轮、句柄/内存不增长的 soak**（评审：spike 未走 release 路径）；stdout 分帧 magic 重对齐（喂坏字节能自恢复不卡死）。
- **主服务**：**坏帧不崩**（解复用把 helper 输出当不可信数据）；audioHub drop-oldest；`HasSubscribers` 对 root sub 计入 activePlayer 的映射；帧活性看门狗（playing+订阅无帧 → 重启）；`SetActivePlayer` 钩子非阻塞/不重入/不 panic。
- **手工/实机**：放歌抓 RMS + WAV 人耳（已验，QQMusic）；五家逐一验「主 PID + tree」覆盖出声子进程（含先激活后播放）；切歌时 root 预热无空窗；ASIO 输出时零帧不崩。**结果**：2026-10-05 逐家真机验证（RMS 非零、坏帧 0；产物未入库）——五家「主 PID + tree」均覆盖出声进程（QQ 音乐为单主进程 + 异名子进程，WeSing 为单进程，网易云/酷狗/汽水为多进程 CEF），root 跟随切换与预热无空窗已验；「先激活后播放」与 ASIO 零帧未见单独验证记录。
- **CI 局限**：Windows-only 包在现有 self-hosted Linux runner 上跑不了（`AGENTS.md` 既有缺口），helper/capture 相关**靠人在本机跑**，如实标注、别假装有覆盖。

每个非平凡逻辑（帧编码/重对齐、RMS、drop-oldest、门控+预热、活性看门狗、退避封顶）留一个可跑的最小检查。

---

## 10. 开放问题 / 已知限制 / Phase 2

**原开放问题（writing-plans 定，均已在实现中落地）**：helper 控制协议精确格式；帧字节布局与 bin 数（FFT 窗/窗函数）；**非 Electron 播放器（QQ/WeSing）的主 PID 判定启发式**（无 `--type=` 判据，评审指出 Phase 1 会真卡这里；实现为 `procscan.MainPID`：同名集合中父进程不在集合内者，多个取 PID 最小，结果见 §9）；Job Object / 启动清扫的具体 API 落地。

**已知限制**：同一播放器多实例时只捕获单实例，且可能与歌词侧跟踪的实例不一致（歌词侧 PID 是会话级局部变量、无 getter，共享判据是更大的接缝，Phase 1 不做）。

**root 单播放器内容是刻意的，切换抹平不需要双路**（评审后澄清，防 Phase 2 走弯路）：观众同一时刻只听一首，root 若同时输出 A+B 的实时内容会与音频失同步，故 root 永远只出当前 active 一路。切换的**内容跳变**由 Phase 2 前端抹平，用「旧的冻结/衰减 + 新的实时」，**只需单路流、不需要继续接收 A 的实时尾巴**：① 运动惯性（每帧朝目标值阻尼滑动，一招覆盖 A→B/暂停/清除——暂停=帧近零衰减、清除=停帧衰减到零）；② 冻结淡化（切换点冻结 A 最后一帧淡出、实时 B 淡入，照 `effect_page.html` 的 freeze→live 姿态）。前端识别「何时切换/清除」用 §4 的 epoch 标记 +（可选）照歌词 `player_switch`/`player_clear` 补一个轻量 switch/clear 标记。Phase 1 后端不受影响：仍单内容、契约已备 epoch。而**捕获空窗**（死-snap）已由 §3.1 预热在后端消除、保证帧到帧无缝接管。

**Phase 2**：前端渲染（频谱条 + 滚动带子）、切换交叉淡化（用 §4 epoch 标识，照 `effect_page.html` 的 freeze→live 姿态）、ASIO 探测与「不支持」提示 UX、sample/true peak 增补、编辑器接入、npm+框架迁移、SPA 交付方式。

---

## 附：与既有子系统的接缝一览（file:line 带符号，会漂按符号找）

| 接缝 | 位置 | 用途 |
|---|---|---|
| activePlayer 唯一漏斗 | `SetActivePlayer` `server.go:334-343`（`if changed` 块 `:339-342`） | root 跟随 activePlayer 的挂钩点（钩子只写 h.mu 缓存 + 唤醒管理器） |
| activePlayer 权威源 | `Router.activePlayer` `router.go:46` | 钩子只用传入 `name`，**不得**回 `router.go:489` `GetActivePlayer` |
| 二进制扇出模板 | `BroadcastEffectFrame` `effect.go:183-203`、`handleEffectWS` `effect.go:328-414` | audioHub / handleAudioWS 克隆源 |
| 按需门控 | `HasEffectSubscribers` `effect.go:175` | audioHub.HasSubscribers 范本 |
| 共用 upgrader（红线） | `s.upgrader`（`NewServer`） | `/audio-ws` 共用，绝不改 CheckOrigin |
| 端点注册位置 | `Start()` 的声明式 `routes` 表（kind `"audio"`） | `/audio-ws`、`/<player>/audio-ws` 由 `registerRoutes` 注册 |
| Server 字段 / 构造 | `server.go:99` / `:120` | 加 `audioHub` |
| 捕获器编排范式 | `main.go` `effect.New(...)`+`go ...Run()`（`:271` 一带） | 音频捕获管理器接线（但音频读 goroutine 本地 recover 不挂 Guard） |
| helper 子命令入口范式 | `main.go:56-59`（`--kugou-patch-helper`） | `--audio-capture-helper` 短路入口 |
| 孤儿清扫先例 | `park.RestoreOrphaned` `main.go:267`/§1.2 step 14 | helper 启动清扫参照 |
| 退出路径（孤儿风险） | `os.Exit` `main.go:283`/`:785`/`:919` | Job Object 覆盖这些不跑 defer 的路径 |
| 捕获内核 | `audiocap/wasapi/wasapi_windows.go` | helper 复用 |
| 进程名（单一真源） | 五家均已导出 `TargetProcessName`（缺的在 `cb8bba3` 补齐），映射集中在 `audiocap/procscan/names_windows.go` | 进程扫描；`names_windows_test.go` 锁死五对 |
| 播放状态（判在播） | `GetPlayerStatus` `server.go` 一带 / `playerStates` | root 预热「在播集合」判据 |
