# 音频波形采集接入遥测：设计

issue #120（A 与 B）；后续项见 #121。状态：设计稿 v3（吸收第一、二轮审查），待复审。

## 1. 要解决的问题

4.0 的音频波形靠一个独立的 helper 子进程（`--audio-capture-helper`，同一个 exe）做 WASAPI 捕获，按需拉起。它在各种声卡、驱动、Windows 版本上的表现目前没有任何现场数据；开发机上出现过一次 RME 驱动蓝屏。

读代码得到的现状（均为 main@5ee29e8）：

1. **helper 的遥测用空选项初始化。** `audiocap/helper/helper_windows.go:84` 调 `telemetry.Init(telemetry.Options{})`。`Init` 里 `Release: opts.Version`、`Environment: environmentFor(opts.IsRelease)`（`telemetry/telemetry.go`），空选项意味着事件没有版本号、环境为 `development`；`collectAppInfo(opts.Version, opts.Config, …)` 在空配置下还会把 `playercap.config` 写成一组零值，读起来像「用户配置全是默认」。helper 内每个 goroutine 都挂了 `Guard`（`helper_windows.go:85,93,103,118,141`），所以 Go panic 会上报，但归错了环境。
2. **不经 panic 的崩溃没有记录。** Go 致命错误（`fatal error: …`）与原生访问违例不会被 `Guard` 的 `recover` 抓到。manager 一侧：`cmd.Stderr = os.Stderr`（`audiocap/manager/manager_windows.go:412`）把 helper 的 stderr 直接交给控制台，manager 看不到内容；`demux` 末尾 `hp.cmd.Wait()` 的结果被丢弃（`:511`，注释写「退出码这里不需要」）；`reapHelper`（`:520`）、`noteSpawnFailure`（`:560`）、`noteCaptureFailure`（`:399`）在放弃时只写日志。
3. **蓝屏抓不到**（内核崩溃，进程来不及发）。不在本设计内，见 #121 的 D。

遥测现有的纪律（`telemetry/report.go` 包注释）：只报两类东西——崩溃和「预期之外」的事件；`ReportOnce` 每个 key 每进程只报一次，key 同时是 fingerprint；**不得接到日志上**（实测过 10～55 条/秒的刷屏，免费版 5k/月且服务端没有限流手段）；`ReportOnce` 不调 `Flush`（主进程长期存活）。

## 2. 同类做法（一手出处）

- **父进程观察子进程并分类。** Electron 的 `app` 事件 `child-process-gone` 给出 `reason`（`clean-exit`、`abnormal-exit`、`killed`、`crashed`、`oom`、`launch-failed` 等）与 `exitCode`，并区分「被外部终止（killed）」与「自己异常退出」。来源：https://www.electronjs.org/docs/latest/api/app#event-child-process-gone 。采纳：manager 只在 helper **自行退出**时上报，manager 自己终止的（看门狗、拆除）与随主服务一起关停的不报。
- **崩溃报告尽量不在崩溃的进程里做。** Crashpad 设计文档：处理器「runs in a separate process from the client or clients」，并说「it's therefore important to generate crash reports with as little execution in the crashed process as possible」，因为崩溃进程常因全局状态已损坏而不可靠。来源：https://chromium.googlesource.com/crashpad/crashpad/+/main/doc/overview_design.md 。采纳：Go 致命错误由父进程（manager）读 helper 的 stderr 尾部来上报，helper 内不加新的上报逻辑。
- **Go 自带的崩溃输出重定向。** `runtime/debug.SetCrashOutput` 把未处理的 panic 与致命错误「printed」到**额外**的一个文件（stderr 之外），官方示例就是父进程拉起监视子进程来做崩溃报告。来源：https://pkg.go.dev/runtime/debug#SetCrashOutput 。**不采用**：致命错误默认已打印到 stderr，而 manager 将在本设计里接管 helper 的 stderr，已能拿到同样的文本，多一条管道只增加复杂度。

## 3. 设计

### 3.1 A：helper 的遥测带上版本与环境

- `telemetry.Options` 新增 `Process string`。非空时：给 scope 设标签 `process=<值>`（经新函数 `applyProcess`，使隐私门禁能构造到它），且**不写** `playercap` 上下文里的 `config` 与 `overwritten`（该进程不加载配置，零值会误导）；版本号、`exe_path`、环境照常。主服务不设，行为不变。
- `main.go` 的 helper 分支（`:65-69`）在调 `helper.Run` 之前调 `telemetry.Init(telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: "audio-helper"})`；`helper.Run` 里原有的 `telemetry.Init(telemetry.Options{})` 删除，`Run` 的签名不变。`Run` 内各 goroutine 的 `Guard` 保持不变（未启用遥测时 `Guard` 零开销直接 re-panic）。
- 为什么放在 `main.go`：`isReleaseVersion` 与 `Version` 在 `main` 包，helper 包不应反向依赖；`main.go:65-66` 的注释写明 helper 分支绕过一切初始化。

### 3.2 B：manager 一侧的三类上报

纯逻辑放在 `audiocap/manager` 包的无构建约束新文件 `telemetry.go`（与 `gating.go` 同样处理，Linux 上可测）；Windows 专有的接线在 `manager_windows.go`。

#### 3.2.1 接管 helper 的 stderr：自己读管道，不让 `os/exec` 替我们拷贝

**不能**把 `cmd.Stderr` 设成一个会同步写 `os.Stderr` 的写入器（第一轮审查 P0）。`os/exec` 在 `cmd.Stderr` 不是 `*os.File` 时起拷贝 goroutine，`Wait` 要等它结束；`WaitDelay` 到期后虽会关管道，但 `awaitGoroutines` 仍然 `_ = <-c.goroutineErr` 等拷贝 goroutine 返回（读 `$GOROOT/src/os/exec/exec.go`）。控制台被卡住时（例如用户在控制台里选中文字的 QuickEdit 暂停）`os.Stderr.Write` 阻塞，`Wait` 永不返回，`demux` 不 `close(exited)`，`teardownHelper` 的 `<-hp.exited` 永久阻塞 `Run`。现状「继承」不会这样：卡的是 helper 自己，不是 manager。

做法：

- **自建管道，`Wait` 碰不到读端**：`pr, pw, _ := os.Pipe()`，`cmd.Stderr = pw`。`cmd.Stderr` 是 `*os.File`，`os/exec` 直接把这个句柄交给子进程，**不起拷贝 goroutine**，`Wait` 也不会关闭 `pr`。`Start` 成功后 manager 立即关闭自己手里的 `pw`（只留子进程持有写端）；`Start` 失败则两端都关。子进程退出后写端关闭，读端读完管道里剩下的字节才见 EOF——**内容不会因为 `Wait` 先返回而丢**（第三轮审查：`StderrPipe` 方案下 `Wait` 会关读端，需要先等读泵，等待时间又会被记进存活时长）。
- manager 为每个 helper 起一个读 goroutine：循环 `Read` 到 EOF，每块数据**顺序**做两件事：(1) 写入该 helper 的 `tailBuffer`（内存，不会阻塞）；(2) **同步**写 `os.Stderr`（写入器可注入，测试用；写错误忽略，继续读）。读到 EOF 后关闭 `pr` 并退出。**不丢、不乱序、有背压**——与现状「helper 直接继承控制台」逐项等价：控制台卡住时，读 goroutine 卡在第 (2) 步，管道写满后卡的是 helper 自己的 stderr 写入，和现在一样（第二轮审查 P2：不放宽「stderr 行为不变」的验收条件，所以**不做**有损的异步转发）。
- **监管路径完全不等这个读 goroutine**：`demux` 与现在一样，stdout 读到 EOF 后立即 `cmd.Wait()`，`close(exited)` 的时机、`reapHelper`/`teardownHelper` 里 `time.Since(hp.start)` 的取值点都**不变**（第三轮审查 P2：任何「等 stderr」的步骤都会把等待时间计进存活时长，使 8.5 秒后崩溃的 helper 被误判为稳定运行、清零快速失败计数；也会让等待期间的看门狗对已死进程追加失败）。读 goroutine 在 `Wait` 之后仍可能还在读剩下的字节，这是预期。
- **事件里的 `stderr_tail` 取上报触发时的快照**：上报本来就延后 3 秒（§3.2.2），读 goroutine 通常早已读完；没读完（控制台卡住）则取当时已有的内容，事件里不单独标注。读 goroutine 在控制台恢复或进程退出前一直存在，每个 helper 至多一个；`Manager.Stop` / `teardownHelper` 不等它。
- 路径内不再需要「读完才能 `Wait`」的约束：管道不归 `os/exec` 管。
#### 3.2.2 退出信息与「自行退出」的判定

- 每个 `helperProc` 一把互斥锁，记录：`exit`（`{known bool; code uint32; at time.Time}`，由 `demux` 在 `cmd.Wait()` 返回后、`close(exited)` 之前写入；`known` 取自 `cmd.ProcessState != nil`；**`at` 取进程真实的退出时刻**——`ProcessState.SysUsage()` 在 Windows 上返回 `*syscall.Rusage`，其 `ExitTime`（`syscall.Filetime`）经 `Nanoseconds()` 转成时间——**不是** `demux` 观察到退出的时刻）、`killMarkedAt time.Time`（manager 主动终止——`killHelperForRestart`（看门狗）、`teardownHelper`——**调用时刻**，从未终止则为零）。`watchdogKill` 等既有字段与语义**一行不动**。
- **自行退出** = `exit.known` 且（`killMarkedAt` 为零，或 `exit.at` **严格早于** `killMarkedAt`——实现时修订：Windows 的 `time.Now()` 与进程 `ExitTime` 都按时钟中断步进，manager 下手与进程被杀常落在同一步进内、读数相等，相等必须算 manager 所杀，否则每次拆除都会误报）。因为 `exit.at` 是进程真实死亡时刻，所以第一、二轮审查 P2 担心的窗口（进程已死、`demux` 尚未返回、manager 恰在此刻下手）被消除：只要进程死在 manager 下手之前，不论 `demux` 何时返回，都算自行退出；反过来，manager 先下手、进程随后才死的，`exit.at` 晚于 `killMarkedAt`，不报。剩下的唯一漏报是 `exit.known=false`（`Wait` 没走到，见下），宁可漏报，不误报。`SysUsage` 取不到 `ExitTime` 时退回观察时刻，并在 `extra` 里不单独标注。时钟域：两者都是本机墙钟，`killMarkedAt` 取 `time.Now()` 的墙钟部分。
- `demux` 因 panic 被本地恢复而没走到 `Wait`：`exit.known=false`，不上报（属 #121 的 C）。
- **结算函数 `settleExit(hp)`**：`reapHelper` 与 `teardownHelper` 共用（第一轮审查 P2：`teardownHelper` 直接摘掉 helper、不经 `reapHelper`，原设计会漏掉它此前已发生的崩溃）。`teardownHelper` 里在 `<-hp.exited` 之后调用同一个函数。
- **随主服务一起关停不报**（第一轮审查 P1）：Ctrl+C 时 helper 与主服务同在一个控制台，没有 `signal.Notify` 的 helper 被系统默认处理终止（Go 的 `ctrlHandler` 在 `sigsend` 为假时返回 0，见 `$GOROOT/src/runtime/os_windows.go`），退出码按 Windows 默认控制台处理应为 `STATUS_CONTROL_C_EXIT`（`0xC000013A`；实现时在测试里用真子进程确认，不凭记忆）；而主服务的信号处理只做 `Unpark`、`RestoreChrome`（要等 CDP 回执）再 `os.Exit(0)`，不调 `audioMgr.Stop`（`main.go:281-288`，注释 `:263-264` 写明「无需在信号处理里显式 Stop」）。三层处理：
  1. 退出码 `0xC000013A` 一律不报（确定性排除 Ctrl+C / Ctrl+Break）。
  2. `Manager.BeginShutdown()`（幂等，原子标志），由 `main.go` 的信号处理在**最开头**调用，`Stop()` 也置位。
  3. 上报**延后 3 秒**（`time.AfterFunc`），触发时若 `BeginShutdown` / `Stop` 已发生则丢弃。覆盖关闭控制台窗口、注销、关机等同样杀掉 helper 的事件（`CTRL_CLOSE_EVENT` 等同样会让没有 `Notify` 的 helper 被终止），以及标志与 helper 退出之间的先后竞争。进程若在 3 秒内退出（关停），事件自然不发。
- **事件只经构造函数产生**：`telemetry.go` 里 `type TelemetryEvent struct{ Key, Message string; Tags map[string]string; Extra map[string]any }`，以及三个构造函数 `HelperExitedEvent(…)`、`HelperGaveUpEvent(…)`、`CaptureGaveUpEvent(…)`。接线点只调用这些构造函数、不再就地写 `map` 字面量（AST 门禁守，见 §4）。另导出 `SampleTelemetryEvents()`：用同一批构造函数、把所有可选字段都填上，返回每种事件各一个样本——隐私门禁用它（§3.3），因此新增任何 tag 或 extra 键，门禁里的样本自动带上。
- **注入的上报函数**：`Manager.report func(TelemetryEvent)`，默认实现把事件交给 `telemetry.ReportOnce(ev.Key, ev.Message, ev.Tags, ev.Extra)`（在 Windows 文件里赋值），测试里换成假函数。

#### 3.2.3 三个上报点

| key | 触发 | 条件 | tags | extra |
|---|---|---|---|---|
| `audiocap.helper_exited_abnormally` | `settleExit`（`reapHelper` 与 `teardownHelper` 共用） | 自行退出（见上）且退出码不是 `0xC000013A`，不论是否为 0 | `exit_code`：`0x%X` | `alive_ms`、`quick_fails`、`stderr_tail` |
| `audiocap.helper_gave_up` | `reapHelper` 与 `noteSpawnFailure` 中 `givenUp` 由假转真处 | 快速失败累计达 `maxQuickFails` | `cause`：`quick_exit` 或 `spawn_failure` | `quick_fails`、`last_exit_code`（有则）、`spawn_error`（spawn 失败时）、`stderr_tail`（最近一个 helper 的尾部） |
| `audiocap.capture_gave_up` | `noteCaptureFailure` 中 `f.givenUp` 为真 | 单个播放器的捕获连续失败放弃 | `player`（七个固定值） | `failures`、`stderr_tail`（**当前** helper 的尾部快照；激活失败的 HRESULT 在其中，见 `helper_windows.go:230`） |

- 三个上报都走同一条「延后 3 秒、触发时检查关停标志」的路径（`helper_gave_up`、`capture_gave_up` 同理：关停期间的放弃不报）。
- key 稳定、不含版本号或播放器名；`player` 放 tag。同一 key 每进程只报一次，所以第二个放弃的播放器不会再报（够了：要的是「出现过」）。每个 key 每进程至多一条，一次运行至多三条，对额度可忽略。
- 不报：看门狗杀、正常拆除、`demux` panic 恢复（C）、每次丢段、静音与 ASIO。

#### 3.2.4 `tailBuffer` 与脱敏

并发安全、有界（保留最近 8 KiB 字节）；`Snapshot()` 返回最近至多 40 行，若发生过截断则丢掉开头的不完整行；返回前脱敏：`<盘符>:\Users\<用户名>\` 里的用户名替换成 `<user>`（含空格与非 ASCII 用户名）。

### 3.3 隐私

新增的采集面：退出码、快速失败计数、helper stderr 末至多 40 行。helper 的诊断内容（读代码确认）：HRESULT、播放器名（固定集合）、PID、`session` 包的枚举失败文本；没有设备友好名（`audiocap` 内无 `FriendlyName`/属性存储读取）。用户目录脱敏后，不引入新的个人信息类别，但要在隐私提示里如实交代：

- 中文与英文遥测段各补一行「音频采集：采集进程异常退出或放弃时的退出码与诊断日志末几行（含进程名、PID、系统错误码）」。
- **现有覆盖表不会自动保护这些新事件**（第一轮审查 P2）：`privacynotice_test.go` 只遍历用 `applyOSInfo`/`applyAppInfo`/`applyUserIP` 构造出的事件的 context 与 tag，不会碰 manager 的三个事件，也不会碰 `process` 标签。因此：
  1. `process` 标签：测试里的 scope 构造补上 `applyProcess`，`noticeCoverage` 登记 `"process"`（关键词用新增的音频一行里的词），中英文同样；
  2. manager 的三个事件：门禁用例取 `manager.SampleTelemetryEvents()`（与真实上报**共用同一批构造函数**，不是另写的静态表，所以新增字段不会绕过），对每个样本的每个 tag 键和 extra 键，要求登记表里有它、且登记的关键词同时出现在中文与英文遥测段。用例放在 `telemetry_test` 外部测试包（`telemetry` 的测试里导入 `manager` 会因为 `manager` 的 Windows 文件导入 `telemetry` 而成环；外部测试包不受影响），通过 `export_test.go` 暴露 `privacyNotice` 与 `privacyNoticeEN`；
  3. 单独用例断言中英文遥测段都含「音频采集 / Audio capture」那一行；
  4. 变异：删掉任一语言的那一行即红。
- `telemetry/privacy.go` 头注释的逐条对应表补 `audiocap/manager/telemetry.go → 音频采集那行`。

### 3.4 失败模式与不变量

- 遥测未启用（本地构建）：`ReportOnce` 早退；多出的开销是每 helper 一个 ≤8 KiB 的 tail 与一个读 goroutine。
- 上报绝不改变 helper 的监管语义：退避、放弃、看门狗逻辑一行不动，上报只在原有的判定分支末尾追加（经 `AfterFunc` 延后，不占 `Run` goroutine）。
- helper 的 stderr 行为与现状等价：内容不丢、顺序不变、控制台卡住时背压落在 helper 身上（见 §3.2.1）。多出的只是一次管道拷贝，以及 manager 里的读 goroutine。
- 读 goroutine 卡在 `os.Stderr.Write` 时不影响任何监管路径：`demux`、`reapHelper`、`teardownHelper`、看门狗都不等它，也不因它改变存活时长的记账。
- helper 内 `Guard` 与 manager 的 `helper_exited_abnormally` 对同一次 Go panic 会各报一条：不同 fingerprint、各一条，可接受；致命错误只有后者。

## 4. 测试计划（含反例）

- `tailBuffer`：容量边界（恰好、超出一字节、单次写大于容量）、截断后丢首行、多行与无换行、并发写读（`-race`）、空缓冲。
- 脱敏：`C:\Users\张三\x`、`c:\users\a b\x`（含空格）、正斜杠、多处出现、无匹配不变。
- stderr 读泵（纯函数，注入读端与写端）：内容与顺序逐字节保真（tee 到写端的字节与读到的相同、tail 与之一致）；写端返回错误时继续读；读端 EOF 时返回；写端被屏障卡住时泵阻塞（背压保留）、释放后继续且不丢字节。
- **回收与拆除不被卡死、记账不漂移**（Windows，复刻仓库做法：把测试二进制复制成假 helper 启动，子进程写 stderr 后以指定码退出）：`Manager` 的 stderr 输出端换成被屏障卡住的写入器，子进程退出并断开最后一个订阅后 `teardownHelper` 与 `reapHelper` 必须立即返回（不含任何等读泵的时间），重新订阅能恢复捕获；屏障释放后读泵读完剩余字节并退出、不泄漏；`Wait` 返回后管道里尚未读走的字节在屏障释放后仍被完整读到。
- **存活时长记账**（第三轮审查补）：预置 `quickFails=4`，helper 存活 8.5 秒后非零退出（假时钟），同时 stderr 写端被屏障卡住超过 2 秒：回收后 `quickFails` 必须为 5 并放弃，不得清零；`teardownHelper` 同。读泵卡住期间不得出现对已退出进程追加的看门狗失败。
- 退出分类 `classifyExit`（纯函数）：自行退出、看门狗杀、拆除、`exit.known=false`、`0xC000013A`、退出码 0/1/2/`0xC0000005`、`killMarkedAt` 早于或晚于 `exit.at`、边界相等；`exit.at` 取真实退出时刻（用注入的假退出时刻验证）。
- 时序用例：helper 非零退出且已记录后再触发看门狗或拆除，仍上报；先下手后死，不报；进程已死但 `demux` 尚未返回时下手，按真实退出时刻判为自行退出。
- 关停：`BeginShutdown` 后三个事件均不报；延后窗口内关停不报；窗口外照报；`Stop()` 同。用假时钟或可缩短的延后常量。
- 上报 payload：key、tags、extra 的键与值；脱敏生效；`stderr_tail` 行数与字节上限；once 语义用假函数验证「每个触发点恰好一次、其余路径零次」。
- AST 接线门禁：`reapHelper`、`teardownHelper`、`noteSpawnFailure`、`noteCaptureFailure` 中必须各有对结算或上报函数的调用，且上报点只调用 `TelemetryEvent` 的构造函数、不得就地写 `map` 字面量；`main.go` 的信号处理里必须调用 `BeginShutdown`；删掉任一处即红。
- 隐私门禁：见 §3.3，含「给任一构造函数加一个 tag 或 extra 键而不登记即红」「删掉中文或英文的音频采集行即红」。
- `telemetry`：`Options.Process` 的 tag 与不写 `config`；主服务不设时与旧行为逐项一致（黄金对比）；隐私门禁见 §3.3。
- 变异清单（实现时逐一实测）：去掉 `killMarkedAt` 比较、`exit.at` 改回观察时刻、去掉 `0xC000013A` 排除、去掉关停检查、去掉延后、读泵不 tee 控制台（丢字节）、读泵在 `Wait` 之后才开始读（丢字节）、`demux` 里加回等读泵的步骤（记账漂移）、tail 不脱敏、`BeginShutdown` 不置位、删隐私文案、构造函数加键不登记、once 失效。

## 5. 不做的事

- 不接日志；不报每次丢段、静音与 ASIO；不带设备友好名。
- 不用 `SetCrashOutput`（见 §2）。
- 不改 `maxQuickFails`、退避、看门狗阈值等任何现有数值与既有字段语义。
- C（本地 recover 的音频 panic）与 D（蓝屏线索）见 #121。

## 6. 待复审者重点看的地方

1. §3.2.1 的自建管道加同步读泵：`cmd.Stderr` 为 `*os.File` 时 `os/exec` 是否确实不起拷贝 goroutine、`Wait` 不碰读端；`Start` 失败与成功后各自对 `pw` 的关闭是否正确、有无句柄泄漏；监管路径是否确实不再依赖读泵。
2. §3.2.2 以真实退出时刻（`SysUsage` 的 `ExitTime`）判定自行退出，在 Windows 上是否可靠（时钟域、取不到时的退回）。
3. 关停三层处理是否足以避免 Ctrl+C、关闭控制台窗口时的误报。
4. §3.3 的隐私门禁（事件只经构造函数产生，门禁取同一批构造函数的样本，外部测试包规避导入环）是否真的会在删文案或加新采集面时变红。
