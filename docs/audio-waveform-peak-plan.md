# 音频波形 sample peak 与帧格式版本化 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 帧线格式升级为带版本与定长头长度的 v1，加入左右声道 sample peak 与归一化标志。

**Architecture:** `audiocap/frame` 定义 v1 布局与校验，并作为 QPC 毫秒换算的唯一实现；helper 在降混前按声道累加峰值，并用窗口结构管理两次出帧之间的样本与峰值，归一化增益同乘到 peak，flags 标记归一化；manager 的空闲帧改用毫秒时间戳。server 不改。

**Tech Stack:** Go 1.25，纯 Go，无新依赖；Windows（WASAPI 进程级 loopback）。

**Spec:** `docs/audio-waveform-peak-design.md`

## Global Constraints

- 质量优先级 稳定 > 正确 > 简洁 > 功能（AGENTS.md §0）。波形是可降级的附加能力，任何改动不得把故障引向歌词与特效主线。
- 帧 v1 布局逐字照 spec §3：小端；`version` = 1；`headerLen` = 48；`flags` bit0 = 已启用归一化；`ts` 为开机起算毫秒（f64，来源 QPC）；`peakL` / `peakR` 在 36 / 40；`playerLen` 为 u32 在 44；bins 从 `headerLen` 开始，`player` 在 bins 之后。
- 坏帧判定逐字照 spec §3「校验」，按序：帧长 < 48；`version` ≠ 1；`headerLen` < 48；`headerLen` 非 4 的倍数；`headerLen` 大于帧长；`headerLen + playerLen` 大于帧长；帧长 − `headerLen` − `playerLen` 非 4 的倍数。坏帧丢弃并按 magic 重对齐，任何输入不 panic、不 OOM。
- `MaxPayload`（1<<16）不变；magic 不变。
- `audiocap/frame` 与 `audiocap/helper/helper.go` 无 build tag，保持 `GOOS=linux` 可编译、可测（AGENTS.md §8 †）。
- 主服务侧音频 goroutine 本地 recover，绝不挂 `telemetry.Guard`（AGENTS.md §1.2）；`telemetry/goroutineguard_test.go` 保持为绿。
- 不新增依赖；不为测试新增生产代码 hook。错误分类用的未导出哨兵错误不算 hook。
- 测试须在去掉对应实现后变红；报告中给出变异内容与结果。
- 门禁：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`GOOS=linux GOARCH=amd64 go vet ./audiocap/frame/ ./audiocap/manager/ ./audiocap/helper/` 通过；改动包另跑 `CGO_ENABLED=1 go test -race`。
- 代码注释与提交信息用中文，严肃、准确、简洁；提交信息不含任何 AI 信息。每个 Task 单独提交。
- 不做真机捕获验证（RME 蓝屏告诫）。
- 文档就地改写原句，不叠加；波形支持范围写「全部播放器」，不写具体数量。

## Review Focus

- **消费者用带偏移的视图解析**：`new Float32Array(buf, 32, 3)` 与 bins 视图要求 `buf` 是从 0 开始的独立 ArrayBuffer。传入 Node 的 `Buffer` 或其切片时，`DataView` / `Float32Array` 构造会抛 TypeError，或在 `byteOffset` 非 0 时读错位置。Task 3 在 openapi 示例旁写明这一前提；审查时用 node 跑示例核对。
- **未来的兼容追加被当成坏帧**：`headerLen` 大于 48 的帧必须照常解出已知字段、bins 与 player。Task 1 的 `TestForwardCompatExtraHeader` 钉住。
- **峰值窗口不清零**：漏清零会让 peak 退化成自激活以来的历史最大值，电平表卡在顶部。Task 2 的 `TestWindow` 钉住取出即清零；`encodeFrame` 保活分支不写 peak，由 `TestEncodeFramePeaksAndFlags` 钉住。
- **空闲帧与真帧时间基不一致**：二者必须同为 QPC 毫秒，否则 active 与 idle 切换时时间轴跳变。Task 1 的 `TestIdleKeepaliveFrameV1` 用夹逼断言钉住。
- **短帧或伪 magic 让解码越界**：payload 小于 40 字节时读字段越界 panic，会在 demux 里被恢复并计为 helper 快速失败。Task 1 的 `TestShortPayloadRejected` 钉住。

---

### Task 1: 帧线格式 v1 与 QPC 毫秒换算

**Files:**
- Modify: `audiocap/frame/frame.go`
- Test: `audiocap/frame/frame_test.go`
- Modify: `audiocap/helper/helper.go`（删除 `qpcNanos`，文件头列举同步）、`audiocap/helper/helper_windows.go`（`encodeFrame` 的时间戳改用 `frame.QPCMillis`）
- Test: `audiocap/helper/helper_test.go`（删除 `TestQPCNanos`）
- Modify: `audiocap/manager/manager_windows.go`（删除 `qpcNanos`，`idleKeepalive` 改用 `frame.QPCMillis` 与 `TSMillis`）
- Test: `audiocap/manager/manager_windows_test.go`

**Interfaces:**
- Produces（`audiocap/frame`）：
  - `const Version uint32 = 1`、`const HeaderLen = 48`、`const FlagNormalized uint32 = 1 << 0`；`MaxPayload` 不变。现有内部常量 `headerLen`（8 字节 magic+payloadLen）改名为 `prefixLen`。
  - `type Frame struct { Player string; Flags uint32; Epoch uint32; TSMillis float64; RMS, PeakL, PeakR float32; Bins []float32 }`（删除 `TSNanos`）。
  - `func Encode(dst []byte, f Frame) []byte`：签名不变，输出 v1。
  - `Decoder` / `Next()` 签名与三态契约不变，按 v1 解析与校验；`headerLen` > 48 时跳过偏移 48..headerLen 之间不认识的字段；bins 个数为 0 时 `Frame.Bins` 为 nil。
  - 未导出哨兵错误，每条校验一个：`errShort`、`errVersion`、`errHeaderShort`、`errHeaderAlign`、`errHeaderOverrun`、`errPlayerOverrun`、`errBinsAlign`。需要附带数值时用 `fmt.Errorf("...: %w", errX)` 包装，`Next` 原样返回。
  - `func QPCMillis(counter, freq int64) float64`：`freq <= 0` 返回 0，否则返回 `counter` 换算成的毫秒数。
- Consumes：无。

- [ ] **Step 1: 改写 frame_test.go，写出失败测试**

把现有用例中的 `TSNanos` 换成 `TSMillis`（如 `123.456`）。删除 `TestBadPlayerLenRejected`（它构造的是旧布局，v1 下由 `TestBadHeaderRejected` 覆盖），其余现有用例保留。新增：
- `TestRoundTripAllFields`：`Frame{Player: "kugou", Flags: FlagNormalized, Epoch: 3, TSMillis: 123456.789, RMS: 0.42, PeakL: 0.9, PeakR: 1.25, Bins: []float32{0, 0.5, 1, 0.25}}` 编码后断言：
  - `binary.LittleEndian.Uint32(b[4:])` == `len(b) − 8`；
  - `b[8:12]` 为 1，`b[12:16]` 为 48；
  - bins 从偏移 48 开始，player 字节位于 `48 + 4*4` 处；
  - 解码结果与输入 `reflect.DeepEqual`。
- `TestIdleFrameRoundTrip`：`Frame{TSMillis: 1}` 编码长度为 48，解码结果与输入 `reflect.DeepEqual`（Bins 为 nil）。
- `TestBadHeaderRejected`：表驱动，好帧取 `Frame{Player: "kugou", Bins: 4 个值}`（帧长 69）。每个子用例把好帧改坏，使其恰好违反一条校验，后面再接一个好帧。断言：坏帧那次 `Next` 返回的错误满足 `errors.Is(err, want)`，随后取到的是后面那个好帧。子用例与取值：
  - `version = 2` → `errVersion`；
  - `headerLen = 44` → `errHeaderShort`；
  - `headerLen = 50` 且 `playerLen = 3` → `errHeaderAlign`（剩余字节 16，避免被 bins 对齐校验兜住）；
  - `headerLen = 72` → `errHeaderOverrun`；
  - `playerLen = 帧长 − 48 + 4`（25）→ `errPlayerOverrun`（剩余为 −4，避免被 bins 对齐校验兜住）；
  - `playerLen = 6` → `errBinsAlign`。
- `TestShortPayloadRejected`：
  - 对 n = 0..39，各构造容量恰为 n 的 `p := make([]byte, n)`，在放得下的位置写 `version = 1`、`headerLen = 48`，直接调用 `decodePayload(p)`，断言 `errors.Is(err, errShort)` 且不 panic；
  - Decoder 级用例：magic + `payloadLen = 4`（后跟 `version = 1`），再接一个好帧，断言坏帧那次 `Next` 返回 `errShort`，随后取到好帧。
- `TestForwardCompatExtraHeader`：手工构造 `headerLen = 56` 的帧，在偏移 48..56 写入 8 个非零字节（例如 `0xAA`），其余字段与 bins/player 照 v1 填写，`payloadLen` 相应增大。断言已知字段、bins、player 均解对，且 bins 不含 `0xAA` 拼出的值。
- `TestQPCMillis`（f = 10_000_000）：
  - `QPCMillis(123, 0) == 0`；
  - `QPCMillis(f, f) == 1000`；
  - `QPCMillis(2*f+f/4, f) == 2250`；
  - `QPCMillis(1e14, 1e7)` 与 `1e10` 的差小于 `1e-3`；
  - `QPCMillis(c+1, f) > QPCMillis(c, f)`（c = 1e14）。

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./audiocap/frame/`
Expected: 编译失败（`TSMillis`、`PeakL`、`QPCMillis`、哨兵错误等未定义）。

- [ ] **Step 3: 实现 v1 编解码与 `QPCMillis`**

在 `frame.go` 中按 Interfaces 实现。包注释的线格式描述改为 v1，`Frame` 字段注释按 spec §3 写明含义。
- `Encode` 中 `payloadLen = HeaderLen − prefixLen + 4*len(Bins) + len(Player)`。
- `decodePayload(p)`（`p` 为 magic 与 payloadLen 之后的字节）按 Global Constraints 的顺序逐项判定，第一项为 `len(p) < HeaderLen − prefixLen` → `errShort`，在读任何字段之前判定。
- `headerLen` 与 `playerLen` 按 u32 读取后转为 int64 再比较，避免相加回绕。
- bins 个数为 0 时 `Bins` 保持 nil。

- [ ] **Step 4: 运行 frame 测试**

Run: `go test ./audiocap/frame/ -v`
Expected: PASS。

- [ ] **Step 5: 迁移调用点（helper / manager）**

- `helper.go`：删除 `qpcNanos` 及其注释；文件头的内容列举删去「QPC 纳秒换算」。`helper_test.go` 删除 `TestQPCNanos`。
- `helper_windows.go` 的 `encodeFrame`：`TSNanos: qpcNanos(...)` 改为 `TSMillis: frame.QPCMillis(readQPC(), qpcFrequency)`。签名在本 Task 不变。
- `manager_windows.go`：删除 `qpcNanos`；`idleKeepalive` 改为 `frame.Frame{TSMillis: frame.QPCMillis(readQPC(), qpcFreq)}`。全文件中「TSNanos」「纳秒」的说法改为 `TSMillis`、毫秒。

- [ ] **Step 6: 为空闲帧写测试**

在 `manager_windows_test.go` 新增 `TestIdleKeepaliveFrameV1`：
- Manager 用 `New(sink, nil, nil, "")` 构造（保证 `stopCh` 非 nil）。
- 假 sink：`HasRootSubscriber` 为真，`ActivePlayer` 为空，`BroadcastAudioFrame` 用 `select { case ch <- b: default: }` 非阻塞写入一个带缓冲的通道。这样 Stop 之后多余的调用既不阻塞，也没有数据竞争。
- 测试先经 `procQPFreq` 取频率 `freq`，记下 `before := frame.QPCMillis(readQPC(), freq)`，再 `go m.idleKeepalive()`。收到首帧后记 `after := frame.QPCMillis(readQPC(), freq)`，然后 `m.Stop()`。最长等 2s，超时 Fatal。
- 用 `frame.Decoder` 解码该帧，断言：player 为空，`Flags == 0`，`len(Bins) == 0`，`before <= TSMillis <= after`；原始字节中 `version` 为 1，`headerLen` 为 48。

- [ ] **Step 7: 全量门禁与残留扫描**

Run:
- `gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./...`；
- `$env:GOOS='linux'; $env:GOARCH='amd64'; go vet ./audiocap/frame/ ./audiocap/manager/ ./audiocap/helper/`，跑完恢复环境变量；
- `$env:CGO_ENABLED='1'; go test -race ./audiocap/...`；
- 在 `audiocap/` 下 grep `TSNanos`、`qpcNanos`、`纳秒`。

Expected：
- 门禁全部通过；grep 只剩 `procscan.go` 中与帧无关的「Windows 文件时间纳秒」。
- 报告中给出变异结果，以下每一项都须变红：
  - 删去任一项校验，`TestBadHeaderRejected` 或 `TestShortPayloadRejected` 的对应子用例变红（断言的是 `errors.Is`）；
  - `decodePayload` 不跳过未知字段时，`TestForwardCompatExtraHeader` 变红；
  - 空闲帧时间戳改成 `float64(time.Now().UnixMilli())` 时，`TestIdleKeepaliveFrameV1` 变红。

- [ ] **Step 8: Commit**

```bash
git add audiocap/frame audiocap/helper/helper.go audiocap/helper/helper_test.go audiocap/helper/helper_windows.go audiocap/manager/manager_windows.go audiocap/manager/manager_windows_test.go
git commit -m "feat(audiocap): 帧线格式 v1：版本号、定长头长度与毫秒时间戳"
```

---

### Task 2: sample peak、归一化增益与 flags

**Files:**
- Modify: `audiocap/helper/helper.go`
- Test: `audiocap/helper/helper_test.go`
- Modify: `audiocap/helper/helper_windows.go`
- Test: `audiocap/helper/helper_windows_test.go`

**Interfaces:**
- Consumes：Task 1 的 `frame.Frame`（`Flags`、`PeakL`、`PeakR`、`TSMillis`）、`frame.FlagNormalized`、`frame.QPCMillis`。
- Produces（`audiocap/helper`）：
  - `func accumulatePeaks(acc [2]float32, interleaved []float32, channels int) [2]float32`：对每个样本取绝对值，声道 0 累加进 `acc[0]`、声道 1 累加进 `acc[1]`；`channels == 1` 时同一值同时累加到两侧；`channels > 2` 时只看声道 0 和 1。NaN 不计入（比较为假）；超过 1 照常保留。
  - `type window struct { batch []float32; peaks [2]float32 }`，用于累积两次出帧之间的非静音样本：
    - `func (w *window) add(interleaved []float32, channels int, silent bool)`：`silent` 或 `interleaved` 为空时不做任何事；否则把 `downmix(interleaved, channels)` 追加到 `batch`，并用 `accumulatePeaks` 更新 `peaks`。
    - `func (w *window) take() ([]float32, [2]float32)`：返回当前 batch 与 peaks，并清零：`batch` 截为 `[:0]`，peaks 归零。返回的 batch 与内部缓冲共用底层数组，在下次 `add` 之前有效。
  - `func (nz *Normalizer) Apply(r *dsp.Result) float32`：返回本次施加的线性增益；未启用时不碰 `r`、不更新 EMA，返回 1。
  - `func encodeFrame(a *dsp.Analyzer, nz *Normalizer, player string, epoch uint32, batch []float32, peaks [2]float32) []byte`：
    - `nz` 启用时 `Flags = frame.FlagNormalized`，保活帧同样置位；
    - 有音频时，用 `Apply` 返回的增益乘 `peaks`，写入 `PeakL` / `PeakR`；
    - 保活帧（batch 为空）不写 peak，`PeakL` / `PeakR` 恒为 0，不论传入值。

- [ ] **Step 1: 写 helper.go 纯函数的失败测试**

在 `helper_test.go` 新增：
- `TestAccumulatePeaks`：
  - `accumulatePeaks([2]float32{}, []float32{0.1, -0.7, -0.4, 0.2}, 2) == [2]float32{0.4, 0.7}`；
  - 初值 `{0.5, 0.5}`、输入 `{0.2, 0.3}` 时结果仍为 `{0.5, 0.5}`；
  - NaN 被忽略：`{float32(math.NaN()), 0.3}` 得 `{0, 0.3}`；
  - 超过 1 保留：`{1.5, -2}` 得 `{1.5, 2}`；
  - `channels == 1`：`{0.3, -0.6}` 得 `{0.6, 0.6}`。
- `TestWindow`：
  - 对新窗口 `add` 一个**非空**的静音包（`{0.9, 0.9}`，`silent = true`），`take` 得到空 batch、零 peaks；
  - 对新窗口 `add` 非静音的 2 声道输入 `{0.2, -0.4, 0.6, 0}`，`take` 得到 batch `{-0.1, 0.3}`、peaks `{0.6, 0.4}`；
  - 紧接着再 `take` 一次，得到 `len(batch) == 0` 且 peaks 为零值。
- `TestNormalizerApplyReturnsGain`：
  - 未启用：返回 1，且 `r` 逐位不变；
  - 启用（`newNormalizer(true, -14, 30)`）：首帧 RMS 为 0.01 时，返回值等于 `float32(gainFactor(0.01, -14, 30))`，且 `r.RMS` 等于 `0.01 * 返回值`。

现有 `Apply` 调用点因返回值被忽略，照常编译，不需要改。

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./audiocap/helper/ -run "AccumulatePeaks|Window|ApplyReturnsGain"`
Expected: 编译失败（函数或类型未定义、`Apply` 无返回值）。

- [ ] **Step 3: 实现 `accumulatePeaks`、`window`，并让 `Apply` 返回增益**

放在 `helper.go`（无 build tag）。文件头：
- 第 2 行「DSP（RMS + 频谱）」改为包含左右声道峰值；
- 内容列举补上「按声道峰值累加」与「两次出帧之间的样本与峰值窗口（window）」。

- [ ] **Step 4: 运行，确认通过**

Run: `go test ./audiocap/helper/`
Expected: PASS。

- [ ] **Step 5: 写 encodeFrame 的失败测试**

在 `helper_windows_test.go` 中，现有 `encodeFrame` 调用补传 `[2]float32{}`。新增 `TestEncodeFramePeaksAndFlags`，用 `frame.Decoder` 解码后断言：
- 归一化启用、有音频、peaks 为 `{0.5, 0.25}`：`Flags == frame.FlagNormalized`；`PeakL / RMS` 与 `0.5 / 未归一化 RMS` 在 1e-6 内相等（即 peak 与 RMS 乘了同一增益），`PeakR` 同理。
- 归一化启用、保活帧（batch 为 nil，peaks 传入 `{0.7, 0.3}`）：`Flags == frame.FlagNormalized`，且 `PeakL == 0 && PeakR == 0`。
- 归一化关闭、有音频：`Flags == 0`，`PeakL == 0.5`，`PeakR == 0.25`，RMS 与 bins 和 `dsp.Analyze` 的结果逐位相同。

- [ ] **Step 6: 运行，确认失败**

Run: `go test ./audiocap/helper/ -run EncodeFrame`
Expected: 编译失败或断言失败。

- [ ] **Step 7: 改 `encodeFrame` 与 `captureLoop`**

`encodeFrame` 按 Interfaces 实现：`PeakL` / `PeakR` 只在 `len(batch) > 0` 分支里赋值。

`captureLoop` 的改法：
- 用 `win := window{batch: make([]float32, 0, wasapi.SampleRate/10)}` 取代原有的 `batch` 变量；
- 每次 `Read` 之后调用 `win.add(readBuf[:n*wasapi.Channels], wasapi.Channels, silent)`；
- 出帧时写 `b, p := win.take(); enqueue(frames, encodeFrame(analyzer, nz, player, epoch, b, p))`；
- 删除原有的 `batch = batch[:0]`。

同步修改 `captureLoop`、`encodeFrame` 的函数注释：peak 窗口为两帧之间的全部非静音样本，与 RMS、bins 只用最后至多 `fftSize` 个降混样本不同。

- [ ] **Step 8: 全量门禁与变异**

Run: 同 Task 1 Step 7。
Expected: 全部通过。报告中给出以下变异结果，每一项都须变红：
- `window.add` 不判断 `silent` → `TestWindow`；
- `window.take` 不清零 peaks → `TestWindow`；
- `encodeFrame` 不把增益乘到 peak 上 → `TestEncodeFramePeaksAndFlags`；
- 保活分支不置 flags，或透传 peaks → `TestEncodeFramePeaksAndFlags`；
- `accumulatePeaks` 先降混再取峰 → `TestAccumulatePeaks`。

报告中还须写明：`captureLoop` 调用 `win.add` / `win.take` 的接线依赖 `wasapi.Activate`，无法变异验证，靠人守。

- [ ] **Step 9: Commit**

```bash
git add audiocap/helper
git commit -m "feat(audiocap): 帧加入左右声道 sample peak 与归一化标志"
```

---

### Task 3: 文档同步

**Files:**
- Modify: `doc/openapi.yaml`（info.description 的多播放器架构句；`/audio-ws` 与 `/{player}/audio-ws` 的 description）
- Modify: `doc/API_RESPONSE_EXAMPLES.md`（文首多播放器架构句；音频波形一节）
- Modify: `README.md`（功能特性「实时音频波形」；接口清单 `/audio-ws` 行；`-audio-normalize` 参数行；配置示例的归一化注释）
- Modify: `config/config.go`（默认配置模板 `defaultConfigContent` 的 audio-normalize 注释）、`config.yml`（同一段注释）
- Modify: `doc/core.md`（音频波形一节的 DSP 行）
- Modify: `AGENTS.md`（§3.4 线格式契约条；§3.4 中 `/audio-ws` 路由条关于空闲帧「❌ 无测试」的括注；§8 † 注的纯逻辑列举）
- Modify: `docs/audio-waveform-peak-design.md`（状态行）

**Interfaces:**
- Consumes：Task 1、Task 2 的实际行为。每一句都回到代码核对，不照抄 spec。

- [ ] **Step 1: openapi**

- info.description：「音频波形帧在二进制头内携带 `player`」改为「音频波形帧内携带 `player`」。
- `/audio-ws` 帧格式表与字段说明改为 v1 布局。
- 写明版本演进规则与坏帧校验，含最小帧长。
- JS 示例替换为 spec §3 的参考解析，并在旁边写明：`buf` 须为从 0 开始的独立 ArrayBuffer（`binaryType = 'arraybuffer'`）；传入 Node 的 `Buffer` 时须先复制成独立 ArrayBuffer。
- 删去「`bins` 的起始偏移不保证 4 字节对齐……」一条。
- 帧形态表加 peak 列（音频帧为本窗口实测峰值，保活帧与空闲帧为 0），并写明 flags。
- 归一化一节写明：peak 与 RMS、bins 同乘增益；flags bit0 对该路每一帧置位，空闲帧为 0；归一化开启时 peak 大于 1 不代表真实削波。删去「帧内不携带归一化状态」。
- epoch 与 ts 的说明改为毫秒。
- `/{player}/audio-ws` 只同步引用到的说法。

- [ ] **Step 2: 其余文档与配置注释**

- **API_RESPONSE_EXAMPLES**：文首同一句照 openapi 改；音频节的内容概述加入左右声道 peak 与 flags。
- **README**：
  - 功能特性「音频帧含 RMS 与 512 点频谱」改为「音频帧含 RMS、左右声道峰值与 512 点频谱」；
  - 接口清单 `/audio-ws` 行「二进制：RMS + 频谱」加入峰值。
- **归一化作用范围**：README 的 `-audio-normalize` 参数行、README 配置示例、`config/config.go` 模板、`config.yml` 四处，「RMS 与频谱」逐字改为「RMS、左右声道峰值与频谱」，四处保持逐字一致。
- **core.md DSP 行**：写明降混前按声道取峰值。
- **AGENTS.md §3.4 线格式契约条**：
  - 写明「兼容的新增只追加在定长头末尾并增大 `headerLen`，不兼容的改动才升 `version`」；
  - 字节布局以 `frame.Encode` / `frame.Decoder` 为准，对外文档只有 openapi 一处；
  - 「RMS/bins 可能 >1」改为「RMS、peak 与 bins 可能 >1（peak 不截断，未开归一化时也可能略超 1）」。
- **AGENTS.md §3.4 `/audio-ws` 路由条**的括注，改为：空闲帧内容（`player=""`、`flags=0`、v1 头）由 `TestIdleKeepaliveFrameV1` 守（✅，Windows-only）；`Run` 中 `go m.idleKeepalive()` 的启动接线 ❌，删掉测试全绿。须按「实测」惯例实际跑一次删除接线的变异，确认测试全绿。
- **AGENTS.md §8 † 注**：helper 的纯逻辑列举补上峰值累加与窗口；frame 补上 QPC 毫秒换算。
- **spec 状态行**：改为「已实施」。

- [ ] **Step 3: 全仓残留扫描**

Run:
- 在 `doc/`、`README.md`、`AGENTS.md`、`config.yml`、`config/config.go` 中 grep：`TSNanos`、`纳秒`、`u16`、`i64`、`不携带归一化`、`头内`、`对齐`、`RMS 与频谱`、`RMS + 频谱`，逐个确认已改或仍然成立；「字段间无对齐填充」在 v1 下仍成立，保留。
- `go build ./config/ ./tools/genconfig/`。
- 用 genconfig 生成模板，与根 `config.yml` 做 diff。

Expected：对外文档中不再有旧布局的描述；模板、`config.yml` 与 README 的配置块逐字一致。`docs/` 下其余设计与计划文档是历史记录，不改。

- [ ] **Step 4: Commit**

```bash
git add doc README.md AGENTS.md config/config.go config.yml docs/audio-waveform-peak-design.md
git commit -m "docs: 同步音频帧线格式 v1、sample peak 与归一化标志"
```
