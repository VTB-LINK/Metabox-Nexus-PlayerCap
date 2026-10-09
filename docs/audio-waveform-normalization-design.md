# 音频波形音量归一化设计

本文是音频波形特性的**增强能力**设计：跨播放器的音量归一化，使前端波形视觉幅度在不同播放器、不同播放音量下保持一致。它建立在 Phase 1 捕获管线之上（见 [audio-waveform-capture-design.md](audio-waveform-capture-design.md)），是一条独立、默认关闭、可叠加的能力。

状态：已实施并合入 main（`bb3abe4`…`8ed57c9`，截至 `326e1cc`）；依赖的 Phase 1 捕获管线（原分支 `phase1-audio-waveform`，已删除）亦已合入。本文为设计记录，与代码分叉时以代码为准。

## 1. 目标与非目标

**要解决的问题**：五家播放器各自以不同音量播放，导致前端波形整体大小不一致——安静的播放器缩成一条线、响的顶满。希望无论各家实际音量如何，波形视觉幅度一致。

**目标**：
- 跨播放器的**整体响度**归一化，前端波形幅度一致。
- 全局开关 + per-player 开关覆盖。
- 默认关闭，开启后对 Phase 1 现有行为零破坏。

**非目标（明确不做）**：
- **分频段归一化**（每频段各自拉到目标、压平频谱）。那会抹掉音乐的频谱性格与逐曲差异，是「频谱白化/多段压缩」的创意 EQ 效果，与「如实展示音乐」相悖。本特性只做**全频段（broadband）扁平增益**，保留频谱形状。
- **运行时 API**（直播中实时调参）。走 config，与 `offset`/`poll`/`idle-hide` 同款。运行时调参是额外面，Phase-2-if-ever。
- 精确 LUFS 响度计量。视觉一致不需要广播级响度标准。

## 2. 参考

- **phandasm/waveform**（OBS 频谱插件，优先参考）：其 `normalize_volume` 即本特性原型——用 ~1s 滚动 RMS 测输入响度，`volume_compensation = min(volume_target_dBFS − dbfs(input_rms), max_gain_dB)`，作为**单标量**在 dB 域加到所有频谱 bin（全频段、可正可负）。默认 target `-8 dBFS`、max_gain `30 dB`、默认关。其 floor/ceiling 是**显示映射**、与归一化无关。
- **Widdit/now-playing-service**：**不做**音量归一化（仅用进程音量判暂停）。非本特性参考。

## 3. 设计

### 3.1 落点：后端烘焙

归一化在 **helper 的捕获循环内**完成，直接作用到帧的 `RMS` 与 `Bins`；前端拿到即一致幅度、零改动。

理由：归一化是**跨播放器**的，helper（知道每家样本、预热期已在测量每家）是 root 切歌时唯一能拿到**已收敛**增益的地方；纯前端在 root 场景下收不到非活跃家的帧、无法预先测其响度，架构上做不动。扁平增益不改频谱形状，前端既有的频率滤波照常工作。

### 3.2 算法：每家独立的 EMA-AGC

每路捕获维护独立的响度估计与增益：

1. **能量 EMA**（每家一个 `float`）：每帧 `ms = rms²`；首帧播种 `ema_ms = ms`，其后 `ema_ms = α·ms + (1−α)·ema_ms`。响度估计 `rms_est = sqrt(ema_ms)`。
   - `α` 对应 τ≈1s 的时间常数，起手 `α≈0.03`（τ≈1s @ ~30fps 活跃帧节拍），真机调。
   - *ponytail：这是对 waveform「1s 样本环形缓冲精确 RMS」的简化——一个 float、无环形缓冲，对驱动一条缓慢增益足够平滑。ceiling：帧节拍变化时 α 不随 Δt 自适应；真静音期增益漂移无所谓（作用于静音仍是静音）。若日后要精确可改时间感知 α。*
2. **增益**：`gain_dB = min(target_dBFS − dbfs(rms_est), max_gain_dB)`，其中 `dbfs(x) = 20·log10(max(x, ε))`，`ε≈1e-10`（≈−200 dBFS，照 waveform 的 `DB_MIN`，防 `log10(0)` 下溢）。
   - 可正可负：响的播放器（`rms_est > target`）→ 负增益**衰减到目标**；只封上限 `max_gain`、不设下限。
   - 真静音 `rms_est→0` → `gain_dB = max_gain`，而 `max_gain × 近静音 ≈ 0`，**不会把静音放大成满屏**。
3. **作用**：`g = 10^(gain_dB/20)`，乘到帧的 `RMS` 与**所有 bins**（走带器用 RMS、频谱用 bins，须一致）。**全频段扁平增益**，频谱形状不变。

关闭时（`norm=0`）：helper **完全跳过** EMA 与增益、原样出帧 = Phase 1 当前行为，零开销零变化。

### 3.3 配置（走 config，套 offset/poll/idle-hide 覆盖模式）

**全局**（yaml 字段 + CLI flag）：

| 键 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `audio-normalize` | bool | `false` | 全局开关 |
| `audio-normalize-target` | int | `-14` | 目标响度（dBFS） |
| `audio-normalize-max-gain` | int | `30` | 增益上限（dB） |

**per-player 覆盖**（`*bool`，nil=跟随全局，套 `IdleHide *int` 模式）：

| 键 | 类型 | 含义 |
|---|---|---|
| `<player>-audio-normalize` | *bool | 该家开关覆盖；不设=跟随全局 |

**仅开关可 per-player 覆盖**，`target`/`max-gain` 保持全局：归一化的目的是拉到**共同目标**，target 本质全局；per-player 改 target 等于故意让某家更响/更轻、反破坏一致性，罕见。YAGNI：真有某家需单独调 target，再按同模式补 `<player>-audio-normalize-target`（几行）。

**解析器**：`GetPlayerAudioNormalize(name) bool`（套 `GetPlayerIdleHide`），命中 per-player 覆盖优先、否则回退全局。`target`/`max-gain` 经 `clampPolls` 同款钳制到合理区间（target 建议 [−60, 0]、max-gain [0, 45]，与 waveform 滑块一致）；per-player 覆盖只有开关，不参与钳制。

默认值 `target −14 / max-gain 30` 为起手值，具体填多少让波形填充好看由真机视觉实测决定。

### 3.4 下发链路：控制协议扩展

main 单一真源持有 config，manager reconcile 时解析每家参数、经 capture 控制命令下发；helper 不读 config。

- 现 `capture <pid> <player>` → 扩成 `capture <pid> <player> <norm:0|1> <target_dbfs:int> <maxgain_db:int>`。
- main 侧总是带上三个参数（`norm = GetPlayerAudioNormalize(player)`，target/maxgain 取全局；`norm=0` 时 target/maxgain 占位、helper 忽略）。
- helper/main 是**同一个 exe**（manager spawn 同版本），无跨版本兼容问题——直接改成 6 字段；`parseControl` 对 capture 解析 6 字段、字段数不符按现有逻辑报坏命令丢弃。`stop <player>` 不变。

### 3.5 契约影响

- 归一化**开启**时，帧的 `RMS` 与 `bins` 为**已归一化**值，且可能 **>1**（响处被抬）。后端**不钳、保留信息**；消费者显示时自行 clamp（检查页本就 `min(1,…)`）。
- `openapi.yaml` 的 `/audio-ws`、`/{player}/audio-ws` 描述须注明：归一化开启时 RMS/bins 为归一化值、可能 >1。
- 帧**线格式不变**（仍 `…|rms|…|bins`），只是数值语义随归一化开关变化。epoch/player/ts 语义不变。

## 4. §0 与安全

- **默认关闭 → 零行为变化**：不惊动已验收的 Phase 1 捕获链。
- 全部在 helper 侧、纯算术（EMA + log/pow + 乘法），**无新 goroutine、无锁、无阻塞主线、不碰 RME 更多**。
- 增益数学有界（`max(x,ε)` 防下溢、`min(…,max_gain)` 封顶），无 NaN/Inf 外泄。
- helper 仍是隔离子进程：归一化若有 bug，最坏是波形数值异常（可降级能力），经 Phase 1 既有的坏帧降级/本地 recover 隔离，**不拖垮歌词/特效主线**（§0）。
- `config`/`logger` 的归一化字段须保持 `GOOS=linux` 可构建（§0.3）；helper 侧应用代码在 windows 捕获路径内，不影响 linux 构建面。

## 5. 测试

**纯逻辑（CI 可测，无平台依赖）**——归一化增益数学抽为纯函数 + 一个 `Normalizer` 持状态结构，置于 helper 包无平台依赖处（如 `helper.go`，与 `parseControl`/`downmix` 并列，CI 可测；import dsp 纯包、不碰 windows），测：
- `dbfs` 下溢：`dbfs(0) == dbfs(ε)`、不产 Inf/NaN。
- 增益封顶：极安静输入 → `gain_dB == max_gain`。
- 衰减方向：输入响于 target → `gain_dB < 0`。
- EMA 播种：首帧 `rms_est == 首帧 rms`（无 0 冷启动过冲）；多帧收敛趋向稳态输入。
- 关闭路径：`norm=0` → 帧值与输入逐位相同（passthrough）。
- bins>1：响输入 + 高增益 → 存在 bin >1（确认后端不钳）。

**config**：`GetPlayerAudioNormalize` 的 per-player 覆盖优先/回退全局；target/max-gain 钳制（仅全局）。

**控制协议**：`parseControl` 解析 6 字段 capture（norm/target/maxgain）、字段数不符报坏命令。

**真机视觉实测**：开启归一化，逐家放不同音量的歌，确认波形填充一致；在此调 `target`/`max-gain`/`α`。

## 6. 落地顺序（供 writing-plans）

1. 纯归一化数学 + `Normalizer`（helper 包纯函数：dbfs / 增益 / EMA 步进 / 应用）+ 单测。
2. config 字段/flag/解析器/钳制 + 单测。
3. helper：EMA 状态 + 应用增益；`parseControl` 扩 6 字段 + 单测更新。
4. manager：reconcile 解析每家参数、拼进 capture 控制命令。
5. openapi：`/audio-ws` 契约补「归一化值、可能 >1」。
6. 真机视觉实测与调参（短跑，守蓝屏告诫）。
