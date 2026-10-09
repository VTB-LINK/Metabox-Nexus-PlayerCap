# 音频波形 sample peak 与帧格式版本化设计

本文是音频波形的一项增强设计：在帧中加入左右声道的 sample peak，并给帧格式加上版本与定长头长度，使以后增加字段不再破坏消费者。它建立在 Phase 1 捕获管线（[audio-waveform-capture-design.md](audio-waveform-capture-design.md)）与音量归一化（[audio-waveform-normalization-design.md](audio-waveform-normalization-design.md)）之上。

状态：已实施（2026-10-05）。

## 1. 目标与非目标

**目标**：
- 每帧提供左、右声道的 sample peak，同时服务两种前端用法：滚动波形的包络（前端取两声道最大值）与左右分声道电平表。
- 帧格式带版本号与定长头长度。兼容的新增字段不需要前端同步升级。
- 帧内携带「本路已启用归一化」标志，消除「根端点切换前后数值尺度不同、前端无从得知」这一已知限制。
- 简化前端解析：数值类型只用 u32、f64、f32 三种，字段 4 字节对齐，bins 可直接映射为 `Float32Array`。

**非目标**：
- true peak（ITU-R BS.1770 的 4 倍以上多相 FIR 过采样）。视觉用途不需要采样点之间的峰值，代价也最高。
- 多于两个声道。进程级 loopback 的捕获格式固定为 48000Hz / 2ch / float32（`audiocap/wasapi` 的 `SampleRate`、`Channels`）。
- 兼容旧的无版本布局。`/audio-ws` 未在任何正式版本中发布（最新 tag v3.3.3 不含音频波形），不存在外部消费者。

## 2. 现状

- 帧线格式（`audiocap/frame/frame.go`）：`Magic(u32) | payloadLen(u32) | epoch(u32) | ts(i64，QPC 纳秒) | rms(f32) | playerLen(u16) | player | bins(f32×N)`，没有版本字段；bins 个数由 `payloadLen` 反推，bins 起点随 `playerLen` 变化，不保证 4 字节对齐。
- helper 每次读到非静音包，就把交错样本按声道取平均降混成单声道，追加到本帧的 batch 中；出帧时用 batch 的最后至多 1024 个样本做 RMS 与 FFT（`encodeFrame`）。帧间隔约 33–40ms，对应 1600–1900 个样本，因此约四成样本不参与 RMS 与 bins 的计算。
- 降混取平均会让左右反相的峰值相互抵消。
- 音量归一化（`Normalizer.Apply`）把同一增益乘到 RMS 与全部 bins 上，响度估计只看 RMS；帧内不携带归一化状态。
- 线格式同时用于 helper 到主服务的 stdout 通道，以及 `/audio-ws` 推给客户端的 WebSocket 消息：`Manager.demux` 解码后用同一个 `frame.Encode` 重编码转发。

## 3. 帧线格式 v1

全部字段为小端序，无对齐填充。

| 偏移 | 类型 | 字段 | 说明 |
|---|---|---|---|
| 0 | u32 | `magic` | `0x56544157`（线上字节 `57 41 54 56`），不变 |
| 4 | u32 | `payloadLen` | 本字段之后的全部字节数。服务端内部分帧用；WebSocket 消费者可直接用消息长度 |
| 8 | u32 | `version` | `1` |
| 12 | u32 | `headerLen` | 定长头总长，即 bins 在帧内的起始偏移，恒为 4 的倍数。v1 为 `48` |
| 16 | u32 | `flags` | bit0：本路已启用音量归一化。其余位保留，发送方置 0，接收方忽略 |
| 20 | u32 | `epoch` | 语义不变 |
| 24 | f64 | `ts` | 开机起算的毫秒数，来源仍为系统 QPC；与墙钟无关，仅用于帧间相对时间 |
| 32 | f32 | `rms` | 语义不变 |
| 36 | f32 | `peakL` | 左声道（声道 0）sample peak，见 §4 |
| 40 | f32 | `peakR` | 右声道（声道 1）sample peak，见 §4 |
| 44 | u32 | `playerLen` | `player` 的字节数 |
| `headerLen` | f32×N | `bins` | 幅度谱；N = (帧长 − `headerLen` − `playerLen`) / 4 |
| `headerLen` + 4N | bytes | `player` | 播放器标识名；空闲帧为空 |

**演进规则**：
- **兼容的新增**：新字段一律追加在 `playerLen` 之后、`headerLen` 之前，同时增大 `headerLen`（保持 4 的倍数），`version` 不变。旧消费者读自己认识的字段，再从 `headerLen` 处读 bins 与 player，不受影响。
- **不兼容的改动**（改变已有字段的偏移、类型或语义）才把 `version` 升到 2。消费者遇到不认识的 `version`，整帧丢弃。

**校验**（接收方，含服务端内部的 Go 解码器）：按以下顺序判定，任一成立即为坏帧：
- 帧长 < 48（即 `payloadLen` < 40），在读取任何字段之前判定；
- `version` ≠ 1；
- `headerLen` < 48，或不是 4 的倍数，或大于帧长；
- `headerLen + playerLen` 大于帧长；
- 帧长 − `headerLen` − `playerLen` 不是 4 的倍数。

**前端参考解析**：

```javascript
// buf 为一条 WebSocket 消息的 ArrayBuffer（ws.binaryType = 'arraybuffer'）
const v = new DataView(buf);
if (v.getUint32(0, true) !== 0x56544157 || v.getUint32(8, true) !== 1) return;
const hdr = v.getUint32(12, true), flags = v.getUint32(16, true);
const epoch = v.getUint32(20, true), ts = v.getFloat64(24, true);
const [rms, peakL, peakR] = new Float32Array(buf, 32, 3);
const playerLen = v.getUint32(44, true);
const n = (buf.byteLength - hdr - playerLen) / 4;
const bins = new Float32Array(buf, hdr, n);
const player = new TextDecoder().decode(new Uint8Array(buf, hdr + 4 * n, playerLen));
```

帧大小约 2.1KB，`MaxPayload`（64KB）不变。

## 4. sample peak

**计算位置**：helper 的采集循环（`captureLoop`），在降混之前。

**口径**：
- 每次读到非静音包，就对该包的交错样本按声道取绝对值的最大值：声道 0 累加进 `peakL`，声道 1 累加进 `peakR`。
- 窗口为两次出帧之间读到的全部非静音样本，出帧后清零。这一点与 RMS、bins 不同（二者只用最后至多 1024 个降混样本），因为 peak 要抓住瞬态，RMS 与 bins 则受 FFT 窗长约束，维持现状。
- 静音包（`AUDCLNT_BUFFERFLAGS_SILENT`）不计入。
- 非有限采样（NaN、±Inf）在进入窗口时按 0 计入，等同于该位置的采样为 0：照常参与降混、RMS、bins 与归一化 EMA，不抬高 peak；batch 与 peak 不会因此出现 NaN 或无穷大。
- 不截断。float32 PCM 通常在 0–1 之间，引擎混音后可能略超 1，超出部分保留，作为削波信息。
- 保活帧（两帧之间没有非静音样本）的 `peakL`、`peakR` 为 0。manager 推送的空闲帧同样为 0。

## 5. 音量归一化与 flags

- 归一化开启时，`peakL`、`peakR` 乘以与 RMS、bins **同一个增益**，所有幅度值保持同一尺度，跨播放器的视觉一致性同样覆盖包络与电平表。
- 响度估计（EMA）仍只看 RMS，peak 不参与。
- 代价：归一化开启时，peak 大于 1 不代表真实削波。需要判断真实削波的消费者，依据 flags bit0 区分。
- flags bit0 在一路捕获启用归一化时，对该路的**每一帧**都置位（包括保活帧），使消费者看到稳定的每路状态。空闲帧 flags 为 0。
- 根端点切换到另一个播放器时，消费者可据 bit0 的变化得知数值尺度改变。

## 6. 改动面

- `audiocap/frame`：
  - `Frame` 增加 `Flags`、`PeakL`、`PeakR`；时间戳字段改为 float64 毫秒（如 `TSMillis`），与线格式一致，不做纳秒与毫秒的来回换算。
  - `Encode` 输出 v1 布局；`Decoder` 按 §3 的校验解析，`headerLen` 大于 48 时跳过不认识的定长字段，作为消费者兼容行为的参考实现。
  - 导出 `Version`、`HeaderLen`（48）、`FlagNormalized` 常量。现有内部常量 `headerLen`（magic 与 payloadLen 的 8 字节）改名，避免与线格式字段同名。
- `audiocap/helper`：
  - 新增按声道累加峰值的纯函数，以及累积「两次出帧之间的降混样本与峰值」的窗口结构（并入读包、取出即清零），均放在无 build tag 的 `helper.go`，可在 Linux CI 上测试。清零逻辑因此有单测守住，不只依赖 `captureLoop` 的接线。
  - `Normalizer.Apply` 改为返回本次施加的增益（未启用时为 1），由调用方乘到 peak 上；dsp 包不改。
  - `encodeFrame` 接收本帧峰值与归一化状态，写入 `PeakL`、`PeakR`、`Flags`；保活帧不写 peak，恒为 0。
  - `captureLoop` 每次读包后并入窗口，出帧时取出窗口。
  - QPC 换算由纳秒改为毫秒。
- `audiocap/manager`：`idleKeepalive` 的空闲帧改用毫秒时间戳，flags 为 0；`demux` 仍是「解码再编码」，逻辑不变。QPC 换算与 helper 保持同一公式。
- `server`：不改。帧对它是不透明的字节。

## 7. 错误处理

- 解码器遇到 §3 所列任一坏头，按坏帧丢弃并按 magic 重对齐；任何输入都不 panic、不 OOM，与现行 Decoder 的契约一致。
- helper 侧峰值计算是纯算术，没有错误路径。

## 8. 测试

每个关键改动都要做变异验证：去掉修复后测试变红，改回再绿。

- **frame**：
  - 全字段往返编解码，包括空闲帧（无 player、无 bins）；
  - §3 每一种坏头各一个用例，并验证坏帧之后能接上下一个好帧；
  - 向前兼容：构造 `headerLen = 56`、中间带 8 字节未知字段的帧，已知字段、bins、player 均解对；
  - `headerLen` 与 bins 起点对齐（4 的倍数）。
- **helper**：
  - 峰值按声道分别累加；
  - 静音包不计入；非有限采样（NaN、±Inf）按 0 计入；超过 1 保留；
  - 保活帧 peak 为 0，且归一化开启时 flags 仍置位；
  - 归一化开启时 peak 与 RMS 乘同一增益；关闭时 peak 与 RMS、bins 逐位不变，flags 为 0；
  - QPC 毫秒换算。
- **manager**：空闲帧的布局与 flags。
- **审查时**：用 Go 生成真实帧，再用 node 实跑 openapi 中的 JS 示例，核对每个字段（不入库）。
- **真机**：本设计不要求真机捕获（RME 蓝屏告诫）。合并后如需观察实际效果，可约一次短跑 smoke，事先与邻座会话协调。

## 9. 文档

就地改写原句，不叠加：
- `doc/openapi.yaml` 的 info.description：「音频波形帧在二进制头内携带 `player`」改为「音频波形帧内携带 `player`」（v1 的 player 在定长头之外）。
- `doc/openapi.yaml` 的 `/audio-ws`：
  - 帧格式表与说明改为 §3；
  - 帧形态表加 peak 列；
  - JS 示例改为 §3 的参考解析；
  - 归一化一节写明 peak 同乘增益、flags bit0 的含义；
  - 写明版本演进与校验规则；
  - 删去「帧内不携带归一化状态」，以及「bins 的起始偏移不保证 4 字节对齐」的告诫（v1 下 bins 对齐）。
- `doc/API_RESPONSE_EXAMPLES.md`：文首的多播放器架构句照 openapi 同改；音频波形一节的内容概述加入 peak 与 flags。
- `README.md`：功能特性「RMS 与 512 点频谱」改为「RMS、左右声道峰值与 512 点频谱」；接口清单「二进制：RMS + 频谱」同步加入峰值。
- 归一化作用范围的说法（README 的 `-audio-normalize` 参数行与配置示例、`config/config.go` 的默认配置模板、`config.yml`）：「RMS 与频谱」改为「RMS、左右声道峰值与频谱」，四处逐字一致。
- `doc/core.md` 音频波形一节的 DSP 行：加入降混前的按声道峰值。
- `AGENTS.md` §3.4 的线格式契约条：写明版本演进规则（兼容的新增只追加并增大 `headerLen`；不兼容才升 `version`）；「RMS/bins 可能 >1」补上 peak。

## 10. 兼容性

helper 与主服务是同一个 exe，二者的线格式总是一致；`/audio-ws` 未发布，不需要迁移。帧格式变化只影响仓库外尚未存在的消费者，以及 scratchpad 中的调试检查页（不入库）。
