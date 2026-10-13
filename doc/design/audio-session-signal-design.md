# WASAPI 音频会话信号 — 设计文档（issue #115）

状态：设计 v2，已过两轮 Codex 设计审查，已实现并经真机核对（§2.5、§4.4）。日期：2026-10-13。本文是设计记录，与代码分叉时以代码为准。
前端承接：VTB-LIVE/VTB-TOOLS_Metabox_Nexus_PlayerCap_Config#17。

风格遵循 `AGENTS.md`。质量优先级继承其 §0：稳定 > 正确 > 简洁 > 功能完整度。音频子系统的铁律不变：任何故障只许降级成「没有这个信号」，不得拖垮歌词与特效两条主线（见 [audio-waveform-capture-design.md](audio-waveform-capture-design.md) §0、§1）。

---

## 0. 背景与目标

前端（编辑器波形页、卡片页预览、连接面板）要提示「疑似独占输出」。它现在只能靠 `/service-status` 报该播放器在播放、`/audio-ws` 连续 10 秒只有静音帧来推断，分不清：

- 播放器自身静音或音量为 0；
- 歌曲里较长的静音段；
- 播放器报告在播放而实际卡在缓冲；
- 同一播放器多开时捕获到的是另一个实例；
- 状态更新延迟。

ASIO 与 WASAPI 独占输出绕开共享模式的混音，进程级 loopback 捕获不到声音（[捕获设计](audio-waveform-capture-design.md) §5 已按机制推断，未实测）。能不能捕获到，取决于目标进程在共享模式下有没有正在运行的播放流。issue 要求后端按播放器给出这一信号，至少区分：

1. 目标进程在共享模式下有活跃的音频会话：捕获有效，静音即真实静音。
2. 目标进程在播放而共享模式下没有活跃会话：疑似独占或 ASIO。

给出方式二选一，由后端定：`/audio-ws` 帧头 flags 增一位，或 `/service-status` 按播放器给出状态。判定所用的系统接口须真机验证。

**不在本文范围**：前端提示文案与阈值（Config#17 管）；独占 / ASIO 输出的实际捕获（[捕获设计](audio-waveform-capture-design.md) §5 已定「不做端点 loopback 兜底」）。

---

## 1. 结论摘要

| 项 | 决定 |
|---|---|
| 信号来源 | 系统音频会话：对每个活跃渲染端点取 `IAudioSessionManager2` 的会话枚举，看目标进程树内有没有 `AudioSessionStateActive` 的会话。不看帧内 RMS / bins。 |
| 探测位置 | helper 子进程。主服务不新增任何 COM 调用。 |
| 给出方式 | `/audio-ws` 帧头 `flags` 增两位（帧格式 v1 兼容新增）；`/service-status` 不动。 |
| 两位的含义 | bit1 `FlagSessionKnown`：本帧所属的捕获已有一次新鲜的会话探测结果。bit2 `FlagSessionActive`：该结果为「目标进程树在某个渲染端点上有 Active 的 WASAPI 会话」（共享或独占，二者无法区分，见 §2.5）。bit2 只在 bit1 为 1 时有意义。 |
| 探测方式 | 轮询，不用通知回调。无捕获在跑时不探测。 |
| 失效处理 | 结果带时间戳，超过存活期没更新就把两位都清 0（未知）。 |
| **能力边界（已实测）** | 能区分「没有 WASAPI 播放流」（ASIO 等非 WASAPI 输出、播放器暂停）与「有 WASAPI 播放流」。**区分不了 WASAPI 独占**：独占流同样出现 Active 会话，而会话峰值、端点峰值表、进程 loopback 对它全为零（§2.5）。后端只报会话事实，不判「独占」。 |

---

## 2. 调研

### 2.1 Windows 文档（一手）

| 事实 | 出处 |
|---|---|
| 会话是一组相关音频流；每个渲染会话只属于一个渲染端点；流在初始化时归入会话，终生不变。 | [Audio Sessions](https://learn.microsoft.com/en-us/windows/win32/coreaudio/audio-sessions) |
| 会话含至少一个运行中的流即 active；最后一个运行流停止即 inactive；inactive 一段时间后 expired；会话终止是最后一个流被释放。 | 同上 |
| 会话默认是进程内的，也可以跨进程。会话 GUID 在进程内唯一，系统以 GUID + PID 区分。 | 同上 |
| `IAudioSessionManager2` 在调用 `IMMDevice::Activate` 的那个端点上枚举会话；Windows 7 起可用；线程须已初始化 COM。 | [IAudioSessionManager2](https://learn.microsoft.com/en-us/windows/win32/api/audiopolicy/nn-audiopolicy-iaudiosessionmanager2) |
| 枚举器可能不知道通过 `IAudioSessionNotification` 刚报告的新会话。 | [GetSessionEnumerator](https://learn.microsoft.com/en-us/windows/win32/api/audiopolicy/nf-audiopolicy-iaudiosessionmanager2-getsessionenumerator) |
| 会话音量接口（`ISimpleAudioVolume` 等）对独占流无效；独占流改用端点音量。 | [Exclusive-Mode Streams](https://learn.microsoft.com/en-us/windows/win32/coreaudio/exclusive-mode-streams) |
| loopback 只能用于共享模式流，独占流不能在 loopback 下工作。 | [Loopback Recording](https://learn.microsoft.com/en-us/windows/win32/coreaudio/loopback-recording) |
| 进程 loopback 不绑定具体端点；目标进程没有渲染流时，捕获方收到静音。 | [Application loopback audio capture](https://learn.microsoft.com/en-us/samples/microsoft/windows-classic-samples/applicationloopbackaudio-sample/) |

**文档没写的**：独占流是否出现在共享模式的会话枚举里。上表只说「会话音量对独占流无效」。所以「独占 ⇒ 无活跃会话」是推断，不是事实，须真机验证。ASIO 不经 WASAPI，按机制不应有 WASAPI 会话，同样须验证。

### 2.2 类比

| 系统 | 做法 | 对本设计的启示 |
|---|---|---|
| Windows 音量合成器（Sndvol） | 枚举各渲染会话，显示 active 与 inactive 的会话（见 Audio Sessions 一文）。 | 「会话枚举」是系统自己判断谁在出声的第一手来源，与读取样本值无关。 |
| PulseAudio | 服务端维护 sink input 的注册表，每路有 `corked`（被暂停）与 drained（无数据）等状态，与流里的样本值无关（[sink-input.h 状态枚举](https://code.delx.au/pulseaudio/blob/61b07768c2f7fcc38a32ba31db837a57335ed664:/src/pulsecore/sink-input.h)，经搜索结果核对，未读全文）。 | 「查服务端登记的流状态」比「分析样本」更不容易把真静音误判成故障。 |
| Android | `AudioManager.getActivePlaybackConfigurations()` 与 `AudioPlaybackCallback` 报告当前活跃的播放配置（[API 参考](https://developer.android.com/reference/android/media/AudioManager.AudioPlaybackCallback)，仅核对到类名与方法名）。 | 同上；它提供拉取与回调两种形态，我们取拉取（§4.4）。 |

### 2.3 同类产品

OBS 的「应用音频捕获」建立在同一套进程 loopback 上。论坛里有多条用户报告：应用走 ASIO 或 WASAPI 独占后，该捕获源没有声音。这些是用户报告，不是官方说明。它们与 Windows 文档一致，也说明这一局限是平台决定的，所有同类工具都有，没有「绕过」可借鉴。OBS 没有提供「为什么没声音」的诊断信号。

### 2.4 本机探针（一手，2026-10-13）

`tools/sessionprobe/main.go`（构建标签 `ignore`，只读，不开音频流）在本机枚举渲染端点与会话：

- 本机 RME 驱动暴露 **21 个活跃渲染端点**（每对输出一个）。同一个应用在**每个端点上都留有会话**，绝大多数是 Inactive（如 `cloudmusic.exe`、`steam.exe` 在 21 个端点上各一个 Inactive 会话）。只认 `Active`。
- 当时唯一的 Active 会话是 `chrome.exe`（pid 60596），在默认端点上，会话峰值随播放跳动（0.0017 → 0.0337）。`Spotify.exe`（pid 5488）在默认端点上有 Inactive 会话，与它暂停一致。
- 非提权进程可以枚举其他进程的会话并取 PID、状态、峰值，无需管理员权限。
- 不做缓存时，一次完整枚举（进程快照 + 21 个端点逐个 Activate + 枚举）约 **94 ms**。
- 同一端点上还会出现 Metabox 自己的捕获进程（`Metabox-Nexus-PlayerCap.exe` 的 `--audio-capture-helper`）的 Active 会话：进程 loopback 的捕获会话会挂在目标所在的渲染端点上。它不在目标进程树内，按 §4.2 的树成员判定不会被算进来；但实现与测试必须覆盖「同端点有 helper 自己的 Active 会话」这一反例。
- 网易云（pid 79052，进程树根即自身）在 RME ADAT (01+02) 上有 Active 会话，会话峰值 0.9 左右，同一时刻 helper 的 loopback 会话也有非零峰值（共享模式的正例；用户已把网易云输出设备选为「Wasapi:ADAT (01+02)」，网易云列表里没有标「独占」的条目，这一项是共享还是独占由本数据判断：有 Active 会话且 loopback 有数据，按文档属共享）。
- 会话 PID 与播放器进程树的对应关系：Chromium 系的出声进程是音频子进程，沿父链上溯到主进程可对上（探针里 `root=` 一列）。

**这些观察不回答独占 / ASIO 问题**（当时没有应用在独占或 ASIO 下播放），后续结果见 §2.5。

### 2.5 真机结果（2026-10-13，只读探针 + 短时捕获）

**ASIO（QQ 音乐 22.71，用户在播放器里选了 ASIO 输出）**：Metabox 的状态接口报 QQ 音乐 `playing`；对 `/qqmusic/audio-ws` 收 8 秒，201 帧全部是 N > 0 的静音零帧（rms、peak 全 0，flags=1，epoch 不变，没有空闲帧，捕获本身有效）；同一时段探针在 21 个活跃渲染端点上**找不到 QQ 音乐的任何会话**，连 Inactive 的也没有。即 ASIO 下 (known, active) = (1, 0)，与设计一致。局限：「在播放」来自 Metabox 读到的播放器状态，未单独确认耳机里确有声音。

**共享模式（网易云 3.1.38，输出设备选「Wasapi:ADAT (01+02)」）**：`cloudmusic.exe` 在 RME ADAT (01+02) 上有 Active 会话，会话峰值 0.6～0.9，同时 helper 的 loopback 会话峰值非零。网易云的设备列表里没有标「独占」的条目，该「Wasapi:」选项实测为共享模式（有 Active 会话且 loopback 有数据）。

**共享模式（QQ 音乐，输出改选 WASAPI）**：QQMusic.exe 在某个端点上有 Active 会话，会话峰值 0.37；对 `/qqmusic/audio-ws` 收 8 秒，201 帧全部非静音（最大 rms 0.47）。该「WASAPI」选项同样是共享模式。

**WASAPI 独占（`tools/exclusiverender` 合成）**：用户的各播放器设置里都没有独占选项，改由合成渲染器在没接设备的 RME ADAT (29+30) 上以 WASAPI 独占（44.1 kHz / 16 位，设备拒绝 48 kHz 格式）开流：

- 静音流 6 秒：探针在流存续期间（约 5.8 秒）看到 `exclrender.exe` 的 **Active 会话**，流结束后会话消失。**独占流在会话枚举里是 Active 的**，R1 的假设（独占 ⇒ 无 Active 会话）对 WASAPI 独占不成立。
- 低幅度音调流（440 Hz，−50 dBFS，首个缓冲核对到样本最大值 103，即确实写入了非零数据）9 秒：同一拍里该独占会话的**会话峰值为 0.0000**（同一拍共享模式的 QQ 音乐为 0.3726）；该端点自己的**端点峰值表全程 0.00000**；对渲染进程开进程 loopback 捕 4 秒，**399 个 packet 全部为零样本**（与文档「独占流不能 loopback」一致）。
- 即：独占下引擎一侧的所有可读量（会话状态除外）都是零，Active 会话 + 全零与「共享模式的真静音」在这些量上无法区分。
- 另：首次尝试因 RME 端点整批换 GUID 在 `Activate` 就失败，没有开流（见下）。


**RME 端点整批换 GUID**：同一晚探针观察到活跃渲染端点数从 21 变为 17、19，再回到 21；注册表里旧的一批 RME 端点（如 ADAT (01+02)）变为 `DeviceState=4`（不存在），出现一批同名的新端点（名称还从 `ADAT (01+02)` 变成 `ADAT (1+2)`）。触发原因推测是用户切换 ASIO / 启动 TotalMix 使 RME 驱动重新枚举，未单独验证。含义：端点 GUID 不稳定、设备会在几秒内成批出现与消失，端点与会话管理器缓存必须能丢弃重建（§4.4、R9），按 GUID 记住端点（如黑名单）不可靠。


---

## 3. 备选方案与取舍

| 方案 | 结论 | 理由 |
|---|---|---|
| A. `/service-status` 按播放器给出捕获状态 | 否 | 捕获是按需的：无 `/audio-ws` 连接就不捕获（[捕获设计](audio-waveform-capture-design.md) §3.1）。要让 `/service-status` 常有这个状态，就得为一个只读的 HTTP 轮询常开捕获与探测，违背按需节能；不常开则读到的是「未知」，形同虚设。 |
| B. **`/audio-ws` 帧头 flags 增位**（采用） | 是 | 信号只在有捕获时才有意义，而有捕获必有订阅者，订阅者本来就在收帧。flags 的其余位已声明保留、消费者应忽略（openapi），新增不破坏 v1。旧后端不置位，前端按「未知」处理即可（Config#17 已写明）。 |
| C. 主服务读帧内 RMS 判「持续静音」 | 否 | 违背「主服务不读 RMS / bins」（[捕获设计](audio-waveform-capture-design.md) §1、§5），且分不清真静音。这正是 issue 要解决的问题。 |
| D. helper 内判持续静音后发「降级」控制消息 | 否 | 同样靠样本值推断，分不清真静音与独占；还要给 stdout 加非帧消息，破坏「stdout 只写帧」的红线（§2.5）。 |
| E. 会话通知回调（`RegisterSessionNotification`、`IAudioSessionEvents`） | 否 | 需要手写回调 vtable 并经 `syscall.NewCallback` 回到 Go；回调内的错误跨 COM 边界，Go 的 recover 接不到，会硬崩（[捕获设计](audio-waveform-capture-design.md) §2.1）。文档还说枚举器可能不知道刚报告的新会话，仍得配合枚举。轮询更简单且无此风险。 |
| F. 在主服务里做探测 | 否 | 直播关键进程里不新增 COM 面。helper 已有 COM 线程模型、隔离、看门狗。 |
| G. 额外加一位「会话存在但不活跃」 | 暂不 | 区分「卡缓冲」与「独占」有价值，但 §5 的残留会话问题使它不可靠，且不是 issue 要求。留作后续，见 §8。 |

---

## 4. 设计

### 4.1 信号定义

`audiocap/frame`：

```go
// FlagSessionKnown 是 flags 的 bit1：本帧所属的这路捕获已有一次新鲜的 WASAPI 会话探测结果。
const FlagSessionKnown uint32 = 1 << 1
// FlagSessionActive 是 flags 的 bit2：探测结果为「目标进程树在某个活跃渲染端点上有 Active 的 WASAPI 会话」（共享或独占）。
// 只在 FlagSessionKnown 置位时有意义。
const FlagSessionActive uint32 = 1 << 2
```

| bit1 known | bit2 active | 含义 | 前端可以得出 |
|---|---|---|---|
| 0 | 0 | 未知：旧后端、空闲帧、探测未完成、结果过期、探测失败 | 沿用现行推断 |
| 1 | 1 | 目标进程树有 Active 的 WASAPI 会话（共享或独占） | 播放器在用 WASAPI 输出。若是共享模式，捕获有效，静音即真实静音（播放器静音、音量为 0、静音段）；若是独占，捕获不到声音而本信号无法发现。**不能据此排除独占** |
| 1 | 0 | 探测成功，目标进程树没有 Active 的 WASAPI 会话 | 当前没有任何 WASAPI 播放流：ASIO 等非 WASAPI 输出、播放器暂停、卡在缓冲或进程树未识别。与「播放中」同时出现且持续，说明捕获**一定**抓不到声音，输出多半走 ASIO 等非 WASAPI 路径 |
| 0 | 1 | 不会出现 | 消费者应按未知处理 |

**后端不判「独占」二字，只报事实。** 「(1,0) 且播放器在播放」说明没有 WASAPI 流，捕获必然抓不到，是确定的，但原因不一定是 ASIO：播放器缓冲卡住时流已停，也是这个组合。「(1,1)」不能排除 WASAPI 独占（§2.5 实测）。文案由前端定，文档如实写这两条边界。

置位对象：该路捕获发出的**每一帧**，含静音的 keepalive 帧，因为静音帧恰是前端要判别的场景。无捕获时主服务推的空闲帧 flags 恒为 0（未知），不变。

### 4.2 判据

对一路捕获（目标 PID = `capture` 命令的 pid，捕获时 `INCLUDE_TARGET_PROCESS_TREE`）：

```
active = 存在 活跃渲染端点 E、E 上的会话 S，使得
           S.state == Active  AND  S.pid ∈ 目标进程树（目标 PID 及其全部后代）
```

- 「活跃渲染端点」取 `EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE)` 的全部端点，不只默认端点：播放器可以按应用单独指定输出设备，本机有 21 个端点。
- 进程树用与捕获相同的口径：目标 PID 及后代。后代关系取一次进程快照（`CreateToolhelp32Snapshot`）的 PPID 链，自会话 PID 上溯至目标 PID，深度上限 16。
- 会话 PID 为 0（系统声音与跨进程会话）不计。
- 状态只看 `Active`，不看会话峰值：会话 Active 而样本全 0 正是播放器静音或静音段，要判为 (1,1)。

#### 4.2.1 结果的发布规则：负结果要求枚举完整

一拍对一个槽位只有三种结果，**不对称**：

| 结果 | 条件 | 发布 |
|---|---|---|
| 正 | 找到至少一个满足 §4.2 的 Active 会话 | (1,1)。其他端点或会话的查询失败不影响，已经找到了。 |
| 负 | 没找到，**且本拍的枚举是完整的** | (1,0)。 |
| 失败 | 没找到，且枚举不完整 | 不更新槽位，让它按存活期自然过期成 (0,0)。 |

「枚举完整」指：进程快照成功；`EnumAudioEndpoints` 成功；每个端点的 `Activate`、`GetSessionEnumerator`、`GetCount` 成功；每个会话的 `GetSession`、`GetState` 成功；对每个 `Active` 的会话 `QueryInterface` 与 `GetProcessId` 成功，且 `GetProcessId` 不报 `AUDCLNT_S_NO_SINGLE_PROCESS`（成功码，表示会话跨进程，返回的 PID 只是最初创建者，当前出声的流可能属于树外的进程，同样无法归属；系统声音会话除外，它由 `IsSystemSoundsSession` 排除）。无法取 PID 或无法归属，就无法判它是否在目标树里。**任何一步失败都使本拍不完整**，不得「跳过失败的端点、用其余端点的空结果发布负结果」：目标唯一的 Active 会话可能恰在查询失败的那个端点上，跳过会让每拍都刷新出 (1,0)，存活期永远不触发，静音或音量为 0 时会在前端 10 秒窗口后误报疑似独占。对 Inactive 会话不必取 PID，其失败不影响完整性。

### 4.3 位置

新包 `audiocap/session`，Windows 专属，纯 Go、无 cgo，沿用 `audiocap/wasapi` 的手写 vtable 调用（`comCall`，含 `//go:uintptrescapes`）。只用同步 COM 调用，不写任何回调。

helper 内新增一个 **session watcher goroutine**（全 helper 一个，不是每路一个）：

- 自己 `LockOSThread` + `CoInitializeEx(MTA)`，与采集 goroutine 同一套线程约束，但**独立线程**：探测卡在 COM（如音频服务停滞）只冻结信号，不冻结采集与出帧。
- 持有「被监视集合」，元素是**一次 capture 的槽位**（`*slot`），不是 player 名。每条 `capture` 命令新建一个槽位（含目标 PID，以及 `atomic` 存放的 `{active bool, at int64 纳秒}`），登记进集合；`stop`、或同名 `capture` 替换时摘掉旧槽位。集合由 Run 的控制循环在 `active` 映射变更处同步增删，加锁保护，集合很小。
- 槽位绑定一次 capture 的理由：manager 在播放器 PID 变化时紧接着发 `stop`、`capture`（`manager_windows.go` 的 `applyControls`），helper 随即允许同名新建（`helper_windows.go` 的 `Run` 控制循环）。若槽位按 player 名定位，旧 PID 的在途探测可能在替换后写回同名槽，把旧目标的结果标给新捕获。槽位按 capture 分配后，旧探测只会写回已摘掉的旧槽位，新槽位在自己的首次有效探测之前保持未知。采集 goroutine 在激活时拿到自己这一路槽位的引用，`encodeFrame` 只读它。
- 集合为空时不探测，goroutine 阻塞等待。符合「无捕获不探测」。
- 每拍（§4.4）：持锁拷出当前槽位切片，锁外做一次进程快照、一次全端点会话枚举，按 §4.2 与 §4.2.1 对切片里每个槽位算出结果，只写这些槽位。
- 采集侧 `encodeFrame` 读本路槽位：`at` 距今不超过存活期（默认 3 秒，约三拍）且槽位已被写过，则置 known，并按 `active` 置 bit2；否则两位都为 0。槽位读写无锁（`atomic.Pointer`），不增加采集路径的阻塞点。

helper 与主服务的接口**零新增**：不加 stdin 命令、不加 stdout 消息，不改 manager。manager 的 `demux` 已把帧整体解码再 `frame.Encode` 转发，`Flags` 原样透传（`manager_windows.go` 的 demux，`frame.go` 的 `Flags` 字段）。

### 4.4 探测节奏与成本

- 拍间隔 **1 秒**（前端的判据窗口是 10 秒，一拍的延迟可忽略；存活期 3 秒）。
- 端点列表缓存，**每 5 秒**刷新一次（`EnumAudioEndpoints`）；每个端点的 `IAudioSessionManager2` 缓存复用，端点消失或 `GetSessionEnumerator` 报 `AUDCLNT_E_DEVICE_INVALIDATED` 一类错误时丢弃重建。热插拔在 5 秒内反映。
- 每拍对每个缓存的端点调 `GetSessionEnumerator` → `GetCount` → 逐个 `GetSession` / `GetState` / `QueryInterface(IAudioSessionControl2)` / `GetProcessId`。先看状态，只对 Active 的会话再取 PID，省去对大量 Inactive 会话的 `QueryInterface`。
- 进程快照（PID → PPID）缓存 **5 秒**；缓存期内出现缓存里没有的 Active 会话 PID（新起的进程）时立即重取。理由见下方实测：取快照是一拍里最贵的一项。
- 成本（真机实测，2026-10-13，21 个端点、43 个会话、约数百个进程）：不缓存时一次完整枚举约 94 ms（含每个端点的 `Activate`）。实现后分项量得（`PLAYERCAP_LIVE=1 go test ./audiocap/session/ -run TestLiveTickCost -v`）：缓存命中时一拍的会话枚举约 3 ms，进程快照约 17 ms，端点缓存每次重建约 30 ms CPU。原设想「逐拍取快照」会占去一拍八成的 CPU，故加了上面的快照缓存。独立 helper 对同样的两路捕获（QQ 音乐、explorer）跑 12 秒与 30 秒，与不带 watcher 的主线 helper 比较：多出 0.33 s 与 0.51 s 的 CPU，斜率约 **每秒 10 ms，即约 1% 单核**，另有 helper 启动时一次性的约 0.2 s（COM 初始化与首次建缓存）。低于本设计原定的 30 ms 一拍的门槛，不降频。可再省的约一半在端点缓存重建：改为按 `IMMDevice::GetId` 复用会话管理器（只对新端点 `Activate`），需要时再做。
- 按需：集合为空时 goroutine 休眠；`/audio-ws` 无连接时主服务本就不起 helper 捕获，探测随之不存在。

### 4.5 失败与降级

| 情形 | 行为 |
|---|---|
| `CoCreateInstance` / `Activate` / 枚举失败 | 该拍视为探测失败，不更新槽位；槽位过期后帧回到 (0,0)。日志限频（首次必报，之后每 60 秒至多一条），走 helper 既有的 `diag` 通道，不直接写 stderr。 |
| 单个端点或会话查询失败 | 该拍不完整：已找到目标 Active 会话则照常发布正结果，否则不发布（§4.2.1）。 |
| watcher 卡死在 COM | 槽位停止更新，3 秒后帧回到 (0,0)；采集与出帧不受影响。helper 关停时，watcher 与采集 goroutine 一样受 `shutdownGrace` 约束，超时直接退出进程。 |
| watcher panic | helper 内 goroutine 挂 `telemetry.Guard`（与 helper 内其他 goroutine 一致，helper 崩只影响波形，主服务由 Job Object 与重启兜底）。 |
| 进程快照失败 | 该拍失败，同上。 |
| 目标 PID 已退出 | 树里无会话，得 (1,0)；捕获本身会因进程退出而被主服务拆除，不在本设计处理。 |

### 4.6 文档与契约

- `doc/openapi.yaml` 的 `/audio-ws` 帧格式：`flags` 一行改为 bit0 / bit1 / bit2；新增「会话信号」一节，写 §4.1 的四种组合与前端可得结论；帧形态表里的 flags 列同步。`/{player}/audio-ws` 引用同一说明。
- `doc/API_RESPONSE_EXAMPLES.md` 的音频小节、`audiocap/frame/frame.go` 包注释、`agent-doc` 的音频相关条目同步。
- 版本演进：只用 flags 的保留位，`version` 仍为 1，`headerLen` 仍为 48。

---

## 5. 风险与反例

| # | 反例 | 影响 | 对策 |
|---|---|---|---|
| R1 | **独占流在会话枚举里也有 Active 会话** | **已证实**（§2.5）：WASAPI 独占得 (1,1)，本信号对 WASAPI 独占无判别力 | ASIO 已验证无会话，可判别。独占不可判别，文档如实写明（§4.1）；本机的播放器里没有一家提供独占选项，实际影响限于用户自行把输出设成独占的场景。要判别独占另需新的信号，见 §7。 |
| R2 | 播放器从共享切到独占后，旧的共享会话仍以 Inactive 残留 | (1,0) 仍成立（Inactive 不算 Active），不受影响。若改为判「无会话」才会误判，这是不加 §3 方案 G 的原因 | 只判 Active。 |
| R3 | 播放器在共享模式输出到某个**非默认端点** | 只看默认端点会漏 | 枚举全部活跃渲染端点（§4.2）。 |
| R4 | 出声进程不在目标进程树内 | 得 (1,0)，被前端当疑似独占 | 与捕获同口径：捕获不到的进程，本信号也不应算它。Apple Music 的捕获目标是 `AMPLibraryAgent.exe`，已由 procscan 处理；会话 PID 须在真机核对（§6）。 |
| R5 | 同一播放器多开 | 目标 PID 只有一个，只看其树 | 与捕获同口径。 |
| R6 | PID 复用：目标退出后 PID 被别的进程占用 | 极短窗口内可能把别的进程的会话算进来 | 捕获随进程退出被拆，窗口 ≤ 一拍；忽略，写入注释。 |
| R7 | 会话状态变化滞后于播放状态 | 开始播放后的头 1～2 秒可能是 (1,0) | 前端按 10 秒窗口判，不受影响；文档写明「须持续」。 |
| R8 | 21 个端点上每拍枚举的 CPU / RPC 成本 | 影响直播机器 | 缓存 + 按需 + 实测（§4.4）；超标则降频。 |
| R9 | 探测期间端点热插拔使 COM 对象失效 | 一拍失败 | 丢弃缓存重建（§4.4、§4.5）。 |
| R10 | RME 驱动：蓝屏先例 | 真机验证期间风险 | 会话枚举是控制面查询，不开流，风险低；验证里涉及开流的步骤（独占渲染）按 §6 的规矩处理。 |

---

## 6. 验证计划

### 6.1 离线（Linux 可跑的纯逻辑，Windows 专属的另标）

- `frame`：新旗标的编码 / 解码往返；(0,1) 不由编码器产生；未知位被 Decoder 原样透传（反例：旧消费者忽略）。
- `session` 纯函数（拆出，不碰 COM）：进程树成员判定（含深度上限、环、PID 0、断链）；探测结果 → 帧标志的映射（含过期、从未探测、探测失败）；存活期边界。
- 发布规则（§4.2.1）用注入的假枚举器测，反例必写：端点 E1 含目标 Active 会话但每拍查询失败、E2 成功返回空，连续 12 拍不得持续产生 (1,0)；E1 失败而 E2 找到目标 Active，应产生 (1,1)；对 Active 会话 `GetProcessId` 失败，不得发布负结果；Inactive 会话的 PID 查询失败不影响负结果。
- 槽位按 capture 绑定：用屏障暂停对 PID A 的探测，对同一 player 执行 stop、capture PID B，再放行 A 的探测并让它发布相反状态；B 在自己的首次有效探测之前必须保持 (0,0)。
- helper：`encodeFrame` 在槽位新鲜 / 过期 / 空三种状态下的 flags，keepalive 帧同样带位；与 `FlagNormalized` 的位不冲突。
- manager：`demux` 对带新旗标的帧不改 flags（Windows 专属）。
- 变异：删掉槽位过期判断、把 `Active` 条件改成「存在会话」、掉了 keepalive 路径的置位，各自变红。

### 6.2 真机（Windows，逐步预告、短跑）

先做无需改动用户设置的部分：

1. 共享模式：每个播放器放歌，用探针与 `/audio-ws` 同时读，确认 (1,1)；暂停后 (1,0)；播放器内静音与音量 0、Windows 混音器里静音，仍是 (1,1)。
2. 会话 PID 与进程树的对应：Chromium 系（网易云、酷狗、汽水）、Spotify、QQ 音乐、Apple Music（`AMPLibraryAgent.exe`）、全民K歌。
3. 记录实现后的每拍耗时与进程 CPU。

再做需要用户配合的部分（先问）：

4. 独占：**已做**（2026-10-13，§2.5）。用户的各播放器设置里都没有独占选项，改用合成渲染器；本机实际使用的输出是 RME ADAT 1+2（默认端点），独占实验不得选它，用的是没接设备的 29+30。结论：独占流有 Active 会话，R1 成立。
5. ASIO：**已做**（QQ 音乐 22.71，§2.5），无会话。
6. 合成渲染器：已用于第 4 步。

每个步骤短跑（≤ 10 秒）、一次一个、不并发开流，开始前在对话里写明做什么、多久、会听到 / 看到什么。

---

## 7. 判别 WASAPI 独占的备选（R1 已成立，本设计不做）

「有没有 Active 会话」不能区分独占。已实测不可用：会话峰值（独占下为 0）、端点峰值表（0）、进程 loopback（全零），它们在独占与真静音下读数相同（§2.5）。未验证的候选，每条都要先真机验证再入设计，且都比本设计侵入得多：

1. 对目标 Active 会话所在的端点试开一条共享模式的 `IAudioClient`（只 `Initialize`、不 `Start`），独占流占着时应返回 `AUDCLNT_E_DEVICE_IN_USE`。需要在 RME 等直播机的声卡上额外开流，并会在端点上留下本进程的会话，风险与影响须评估。
2. 放弃判别，如实写「后端无法区分独占」，前端维持「疑似」。当前选这一条。

---

## 8. 推迟与不做

- 「会话存在但不活跃」位（方案 G）：不做，理由见 §3、§5 R2。需要时另开 issue。
- `/service-status` 的按播放器状态：不做（方案 A）。
- 通知回调（方案 E）：不做。
- 前端文案与判据窗口：前端会话负责（Config#17）。
