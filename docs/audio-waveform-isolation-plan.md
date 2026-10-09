# 音频捕获故障隔离修复计划

> **状态：已全部实施。** 实施后经整分支对抗审查又做了两处修订，与下文不同处以代码为准：看门狗所杀的 helper 不抬退避、不推迟重启（重试由逐播放器退避控制）；按需拆除稳定运行过的 helper 时同样清零计数与退避。本文为历史实施计划。

> 修复音频波形文档审查中发现的三个代码层缺陷。实施前经过一轮设计对抗审查（23 条成立，已全部并入本文）。范围只限这三项；helper 遥测参数、i18n、`/service-status` 回显等其余待决问题不在本计划内。

**目标**：
- 静默返回型的单路捕获故障（某一路激活或读取失败后不再出帧、helper 进程仍在运行）只让该播放器降级，不再升级为全局放弃。
- Job Object 赋入失败、panic 恢复之后，helper 进程与订阅都不残留。

**已知缺口（不在本计划内）**：使 helper 进程崩溃的单路故障（COM 回调硬崩，或 captureLoop panic 经 Guard re-panic）无法归因到具体播放器，仍计入 helper 快速失败，可导致全局放弃。原因是重启后所有 want 在同一轮一起下发，无法按下发时间归因。真正隔离需要「失败播放器推迟单独下发」或「每个播放器一个 helper」，另立计划。

**基线**：分支 `fix/audiocap-isolation`，基于 `73f9bb6`。

## 全局约束

- 质量优先级 稳定 > 正确 > 简洁 > 功能（AGENTS.md §0）。波形是可降级的附加能力，任何改动不得把故障引向歌词与特效主线。
- 主服务侧音频 goroutine 一律本地 recover，绝不挂 `telemetry.Guard`（AGENTS.md §1.2 例外边界）。helper 子进程内照常挂 Guard。
- `audiocap/manager/gating.go` 无 build tag，不得 import `config`、`logger` 或任何 Windows 专属包，保持 `GOOS=linux` 可编译、可测（AGENTS.md §8 †）。新增的纯判定逻辑放这里。`audiocap/helper/helper.go` 同理。
- `Manager` 的 `helper` / `dispatched` / `backoff` / `quickFails` / `givenUp` / `respawnAfter`，以及本计划新增的逐播放器状态，只由 `Run` goroutine 访问，不加锁。`lastFrame` 仍由 `mu` 保护，`m.mu` 与 server `s.mu` 保持单向锁序。
- 不改帧线格式，不改 helper 控制协议（`capture <pid> <player> <norm> <target> <maxgain>` / `stop <player>`）。
- 不新增依赖；不为测试新增生产代码 hook。确实无法在不加 hook 的前提下测试的，在报告中写明原因。
- 每个非平凡逻辑留一个可运行的最小检查；测试须在去掉修复后变红，报告中给出变异结果。
- 门禁：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`GOOS=linux GOARCH=amd64 go vet ./audiocap/manager/ ./audiocap/helper/` 通过；改动涉及的包另跑 `go test -race`（`CGO_ENABLED=1`，AGENTS.md §8）。
- `telemetry/goroutineguard_test.go` 扫描根包、`server/`、`player/` 的每个 `go` 语句：入口须 `defer telemetry.Guard()`，或直接 defer 调用 `localRecover` / `audioRecover`，且只认函数体开头连续的 defer 段。在这些目录调整 goroutine 的 defer 段时须保持该门禁为绿；`audiocap/` 不在其扫描范围。
- 代码注释与提交信息用中文，严肃、准确、简洁；提交信息不含任何 AI 信息。每个 Task 单独提交。
- 不做真机捕获验证（RME 蓝屏告诫）；全部改动以单测与代码审查验证。

## 背景（现状）

- helper 内某一路 `captureLoop` 在 `CoInitializeEx`、`wasapi.Activate` 或 `Read` 失败时直接返回，helper 进程照跑但不通知 Manager。helper 的 `active` 表仍保留该 player，后续同名 `capture` 会被当作「已在捕获」忽略，必须先 `stop` 才能重新捕获。也可能卡在一个不返回的 COM 调用里（如 `Close` 里的 Stop/Release、`Activate` 里的 Initialize），此时 `stop` 收不回该 goroutine。
- helper 只要还活着，任一路的捕获无论有无音频，每 ≤100ms 都会出一帧（有声为真帧，无声为保活零帧），与播放状态无关。所以一路已下发、2s 内无帧，就说明这一路或整个 helper 已死。
- `Manager.applyControls` 下发 capture 时把 `lastFrame[player]` 置为当前时刻，作为看门狗的宽限基线；此后 `demux` 每收到一帧就刷新它。
- `Manager.checkWatchdog` 只要发现某个 want 中、状态为 playing 的 player 超过 `hangThreshold`（2s）无帧，就杀整个 helper。`reapHelper` 规定 helper 存活不足 `quickFailWindow`（10s）计一次快速失败，存活不少于 10s 则把 `quickFails` 与 `backoff` 清零；连续 `maxQuickFails`（5）次即 `givenUp`，所有播放器失去波形。
- `spawnHelper` 在 `createKillOnCloseJob` 成功后先 `job = j`；随后 `OpenProcess` 或 `assignToJob` 失败只记日志，`job` 保持非 0，但 helper 不在 Job 内。`killHelperForRestart` / `teardownHelper` 只关 job 句柄，杀不掉它；卡死的 helper 会让 `teardownHelper` 的 `<-hp.exited` 永久阻塞 `Run`。
- `reapHelper` 假定进程已死，只关 job 句柄。但 `demux` panic 被恢复时也会 `close(hp.exited)`，此时进程仍活着。
- helper `Run` 收到 stdin EOF 后 `capWG.Wait()` 无限等待所有采集 goroutine；有 goroutine 卡在 COM 调用里时，helper 永不退出，`job == 0` 时成为孤儿。
- `Run` 由 `main.go` 以 `defer localRecover("audio-manager")` 包裹。`Run` panic 被恢复后，已在跑的 helper 不会被拆除，捕获持续到主进程退出，违反按需捕获。（`idleKeepalive` 是独立 goroutine，继续按契约推空闲帧，这一点应保持。）
- `handleAudioWS` 的写 goroutine panic 被 `audioRecover` 恢复后不关连接，读循环不返回，订阅仍在 hub 登记，持续维持捕获，直到客户端断开。

## Task 1：看门狗区分「helper 假死」与「单路捕获失败」，逐播放器重试与放弃

**文件**：`audiocap/manager/gating.go`、`audiocap/manager/gating_test.go`、`audiocap/manager/manager_windows.go`、`audiocap/manager/manager_windows_test.go`

**常量**：`hangThreshold`、`quickFailWindow`、`maxQuickFails`、`backoffBase`、`backoffCap` 移到 gating.go，manager_windows.go 引用同一常量，不得复制数值。

**判定（纯函数，放 gating.go）**

1. 把 `frameSnap` 移到 gating.go，并增加本次下发时刻字段：`player`、`last`（lastFrame，下发时为基线）、`has`（lastFrame 是否存在）、`dispatchedAt`（本次下发时刻；无记录时为零值）。
2. 用新的纯函数替换 `shouldRestart`，**不再接收 status**：
   - `stale`：所有满足「`has` 且 `now - last > hangThreshold`」的 player，按 rows 顺序。与播放状态无关。
   - `alive`（helper 存活证据）：存在某行满足「`has`，且 `last` 晚于 `dispatchedAt`（即本次下发后收到过真帧，不是基线），且 `now - last <= hangThreshold`」。`dispatchedAt` 为零值时视为没有存活证据。
   - `killHelper`：`stale` 非空且没有 `alive`。`stale` 为空时 `killHelper` 必为 false。
3. 逐播放器失败记账（纯函数，放 gating.go）。状态至少含：连续失败次数、下次可重试时刻、是否已放弃、失败时的 PID。记一次失败的规则：
   - 若「失败时刻 − 本次下发时刻 ≥ `quickFailWindow`」，次数重置为 1；否则次数加 1。本次下发时刻缺失（零值）时按「不足窗口」处理，即加 1。
   - 次数达到 `maxQuickFails` 即标记放弃，直到该播放器进程重启（PID 变化）为止。
   - 未放弃时，下次可重试时刻 = 失败时刻 + `min(backoffBase << (次数-1), backoffCap)`，即 1s、2s、4s、8s。
   - 记下失败时的 PID。
4. 成功清账判定（纯函数，放 gating.go）：给定本次下发时刻与 lastFrame，若「lastFrame − 下发时刻 ≥ `quickFailWindow`」，说明本次捕获已跑稳，应清除该播放器的记账。
5. want 过滤（纯函数，放 gating.go）：对 `map[player]pid` 的 want，剔除「已放弃」或「当前时刻早于下次可重试时刻」的 player；若该 player 当前 PID 与记账 PID 不同（播放器已重启），清除其记账且不剔除。

**接线（manager_windows.go）**

6. `Manager` 新增（仅 `Run` 访问）：逐播放器记账表；每个 player 的本次下发时刻表，在 `applyControls` 下发 capture 成功时写入，在下发 stop 或移除时删除。`helperProc` 新增「由看门狗发起退出」标记（挂在 helperProc 上，随进程丢弃，不放 Manager）。
7. `reconcile` 在第 4 步构造 want 之后、第 5 步之前，经第 5 条的过滤函数过滤 want。
8. `checkWatchdog`：
   - 在 `mu` 内快照每个 want 中 player 的 lastFrame，与下发时刻合成 rows；锁外判定。
   - 先做成功清账：对每个有记账的已下发 player，第 4 条判定为真即删除其记账。
   - `killHelper == false` 且 `stale` 非空：对每个 stale player，**先**按其下发时刻记一次失败，**再**下发 `stop <player>`，并从 `dispatched` 与下发时刻表删除、在 `mu` 内删除其 `lastFrame`。helper 不动。
   - `killHelper == true`：对每个 stale player 先记一次失败，再设置 helperProc 的看门狗标记，然后 `killHelperForRestart()`。
9. `reapHelper`：存活不足 `quickFailWindow` 时，仅当看门狗标记为假才 `quickFails++`；存活不少于 `quickFailWindow` 时，不论由谁发起，照现状 `quickFails = 0; backoff = 0`。之后的 `givenUp` 判定、`bumpBackoff` 与 `respawnAfter` 不变。
10. 日志：单路重试与单路放弃各打一条 Warn，含 player 名与次数；放弃时写明「放弃该播放器的波形，直到其进程重启；其他播放器不受影响」。

**测试**

- gating_test.go（无 build tag）：
  - 判定：单个 stale 且无其他行 → kill；stale 加另一行本次下发后有真帧 → 不 kill，stale 列表正确；stale 加另一行只有基线（`last == dispatchedAt`）且未超阈值 → kill；stale 加另一行 `has=false` → kill；暂停中超阈值的行列入 stale（变异：恢复 status 条件须变红）；`has=false` 超阈值不列入 stale；多个 stale 加一行真帧 → 全部列入 stale、不 kill；全部有真帧 → 无动作。
  - 记账：「每次下发 3s 后失败」连续模拟，前 4 次未放弃、退避依次 1/2/4/8s，第 5 次放弃（变异：阈值改成 3 须变红）；窗口外失败重置为 1；下发时刻缺失时加 1；退避不超过 `backoffCap`。
  - 成功清账：下发后跑满窗口为真，不足为假。
  - 过滤：退避期内剔除、到期放行、已放弃剔除、PID 变化清除记账并放行。
- manager_windows_test.go：
  - 原 `TestShouldRestart` 删除或迁移，不得留下对已删函数的引用。
  - reapHelper：看门狗标记且存活不足窗口 → `quickFails` 不变、`givenUp` 为假、backoff 抬高；看门狗标记且存活不少于窗口 → `quickFails == 0`、backoff 回到 `backoffBase`（变异：去掉稳定复位须变红）；`TestReapHelperStableResetsBackoff` 保留。
  - checkWatchdog 单路路径：recWriter 断言下发了 `stop <player>`，`dispatched`、下发时刻、`lastFrame` 均已清除，记账次数为 1，helper 未被杀。
  - checkWatchdog kill 路径端到端：job 用 `createKillOnCloseJob()` 建的空 job，stdin 用记录 Close 的 recWriter，`cmd` 为 nil；断言 stdin 已关、job 已置 0、记账次数为 1；随后把 exited 置为已关、调用 reapHelper，断言 `quickFails` 不变（变异：删去设置看门狗标记的那句须变红）。
  - reconcile 中对过滤函数的调用依赖真实进程快照，不加 hook 测不了；在报告中说明，由审查核对调用位置。

## Task 2：Job Object 赋入失败退回 `Process.Kill`，reap 与 helper 收尾不留进程

**文件**：`audiocap/manager/manager_windows.go`、`audiocap/manager/manager_windows_test.go`、`audiocap/helper/helper_windows.go`、`audiocap/helper/helper.go`、`audiocap/helper/helper_test.go`

1. `spawnHelper`：`OpenProcess` 或 `assignToJob` 任一失败时，`CloseHandle(job)` 并把 `job` 置 0，使后续 `killHelperForRestart` / `teardownHelper` 走 `Process.Kill` 兜底。日志说明已降级为「stdin EOF + Process.Kill」。
2. `reapHelper`：回收前确保进程已死。关闭 `stdin`；`job != 0` 时关 job 句柄（KILL_ON_JOB_CLOSE 强杀），否则 `cmd.Process.Kill()`，已退出时的错误忽略。对 `stdin`、`cmd`、`cmd.Process` 为 nil 的 helperProc（现有单测这样构造）要安全。注释删去「进程已死」的假定，说明 demux panic 恢复后进程可能仍活。
3. helper `Run` 收尾：关闭全部 `done` 后，至多等待 `shutdownGrace = 3 * time.Second`。超时则记 stderr 日志、不 `close(frames)`（卡住的 goroutine 若恢复会向其发送），直接 `os.Exit(0)`。等待上限函数做成纯函数，放在无 build tag 的 helper.go。

**测试**

- helper_test.go：等待上限函数——未 Done 的 WaitGroup 在短时限下返回 false，Done 后返回 true（变异：改成无限等待须超时或变红）。
- manager_windows_test.go：用 Go 标准的「测试二进制自身充当子进程」写法写一个真子进程用例。`TestHelperProcess` 在环境变量 `AUDIOCAP_TEST_HELPER=1` 时 `time.Sleep(time.Hour)` 且不读 stdin，模拟不响应 stdin EOF 的卡死 helper；未设该变量时直接 return。主用例用 `exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")` 加该环境变量启动子进程，构造 `helperProc{cmd, stdin, job: 0, start: time.Now(), exited: closedChan()}` 后调用 `reapHelper`，再在 goroutine 里 `cmd.Wait`，主协程最多等 5s，超时即 Fatal；用 `t.Cleanup` 兜底 Kill，防止泄漏。变异：删去 `job == 0` 时的 `Process.Kill` 须变红。
- Job 赋入失败路径无法在不加 hook 的前提下注入，在报告中说明，交审查确认。

## Task 3：panic 恢复后不残留 helper 与订阅

**文件**：`audiocap/manager/manager_windows.go`、`audiocap/manager/manager_windows_test.go`、`server/audio.go`

1. `Manager.Run`：在函数开头注册 defer，panic 展开时（以及正常返回时）执行 `m.teardownHelper()`；正常 stop 路径已调用过，重复调用须为空操作。**不调 `m.Stop()`**：`idleKeepalive` 须在 `Run` 恢复后继续按契约向 root 推空闲帧。defer 自身不 recover，recover 仍由 `main.go` 的 `localRecover` 负责，日志不重复。
2. `handleAudioWS` 写 goroutine：panic 被 `audioRecover` 恢复后关闭连接，使读循环返回、走既有清理路径注销订阅。执行顺序须为：先 `audioRecover`，再关连接，最后 `close(done)`。三条 defer 连续写在 goroutine 函数体最前面，注册顺序为 `close(done)`、关连接、`audioRecover`，其前不得有任何非 defer 语句（goroutineguard 门禁要求）。写失败路径原有的显式 `conn.Close()` 可并入该 defer。`audioRecover` 须被 defer 直接调用，不得包闭包。
3. 注释同步：函数头与相关注释写明上述行为。

**测试**：`Run` 的 panic 拆除用单测覆盖。以会 panic 的假 sink 驱动 `Run`，m.helper 用 Task 2 的真子进程写法构造，断言 panic 展开后 helper 进程已被回收、`m.helper == nil`、`stopCh` 未关闭；注意 `Run` 起手的 `sweepOrphans` 只清命令行含 `--audio-capture-helper` 的同名进程，测试子进程不得带该参数。写 goroutine 的连接关闭若无合法注入点，在报告中说明。

## Task 4：文档同步

**文件**：`AGENTS.md`、`doc/openapi.yaml`、`doc/API_RESPONSE_EXAMPLES.md`、`doc/core.md`、`README.md`

按 Task 1–3 的实际行为改写原句。就地改写，不叠加，篇幅只减不增。

- AGENTS.md §1.2 例外边界段的「代价」：删去「`Run` 被恢复后 helper 不拆除」「写 goroutine 被恢复后不关连接」两条，改为新行为（Run 恢复后拆 helper、idleKeepalive 照常；写 goroutine 恢复后关连接、订阅注销）。
- AGENTS.md §3.6：
  - Job Object 条：删去「已知缺口」，写明赋入失败也退回 `Process.Kill`、reap 时确保进程已死、helper 收到 stdin EOF 后至多等 `shutdownGrace` 即退出。
  - 退避与看门狗条：写明看门狗不再看播放状态；能证明 helper 存活时逐播放器 stop 重试，退避 1/2/4/8s，连续 5 次放弃该播放器、直到其进程重启；看门狗发起的快速退出不计入快速失败、稳定运行后照常复位；自行崩溃的连续快速失败仍是 1/2/4/8s 后全局放弃；看门狗重启不计数，退避可翻倍到 30s 封顶。删去「30s 封顶不可达」。写明已知缺口：使 helper 崩溃的单路故障无法归因，仍走全局计数。门禁说明按新测试更新。
  - §4 门禁表、§8 † 注：纯逻辑清单补上看门狗判定、逐播放器记账、成功清账、want 过滤；Windows-only 一侧收窄为看门狗接线（checkWatchdog / reapHelper）。
- openapi `/audio-ws`「可能无帧的情况」：
  - 退避一条改为「1 秒起翻倍、最长 30 秒」。
  - 单路失败一条限定为「捕获子进程内某一路启动或读取失败而子进程仍在运行」：任一已下发的播放器（含暂停中）约 2s 无帧即单独重试，退避 1/2/4/8s，连续 5 次后放弃该播放器、直到其进程重启，其他播放器不受影响。删去「若其状态为 playing」。
  - 全局放弃一条写明只针对捕获子进程自身连续崩溃，包括某一个播放器的捕获反复导致子进程崩溃的情况。
- API_RESPONSE_EXAMPLES「音频波形」节、core.md「故障隔离」行：同步上述语义。
- 波形支持范围一律写「全部播放器」，不写「五家」或其他数量（维护者要求，避免新增播放器时反复改文档）。涉及：README.md 能力对照的「音频波形」说明（现「五家相同」）、功能特性「实时音频波形」（现「五家均支持」）、core.md 小节标题（现「五家通用」）、AGENTS.md 中两处「`/<player>/audio-ws` 自动五家齐全」。能力对照表按播放器逐行的 ✔ 保留；验证记录（如「现有五家的验证见…」、docs/ 下设计与计划中的逐家结果）是历史事实，保留原样；歌词能力的「五家」不在本条范围。
- grep 现有文档中描述旧行为的其余句子（「放弃本会话」「若其状态为 playing」「1/2/4/8s 后放弃」「30s 封顶不可达」等），一并改正。
