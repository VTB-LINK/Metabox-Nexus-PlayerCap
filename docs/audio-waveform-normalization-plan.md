# 音频波形音量归一化 实现计划

> **状态：代码任务已全部合入 main（截至 `326e1cc`），复选框未回填——不要据此重新执行。末尾的「真机视觉实测与调参」在本文中无执行记录；已知只对网易云与 QQ 音乐做过（结果未入库），不能视为逐家已验证。** 本文为历史实施计划，与代码分叉时以代码为准；Global Constraints 中「提交到 `audio-waveform-normalization` 分支」指当时的工作分支，已合入并删除。

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给音频波形加跨播放器音量归一化，使前端波形视觉幅度在不同播放器/音量下一致。

**Architecture:** 后端烘焙——helper 捕获循环内每家维护能量 EMA、算全频段扁平增益、作用到帧的 RMS+bins；参数（开关/target/max-gain）由 main 按 config 解析、经 capture 控制命令下发。默认关闭 = 对 Phase 1 零行为变化。

**Tech Stack:** Go；纯逻辑（EMA/dbfs/增益）在 helper 包无平台依赖处，CI 可测；捕获编排在 `//go:build windows`。

**Spec:** [docs/audio-waveform-normalization-design.md](audio-waveform-normalization-design.md)

## Global Constraints

- §0 最高约束：稳定>正确>简洁>功能；音频故障绝不拖垮歌词/特效主线；音频 goroutine 本地 recover、绝不挂 telemetry.Guard。
- §0.3：`config`/`logger`/`tools/genconfig` 必须 `GOOS=linux` 可构建；归一化纯逻辑放 helper 包无平台依赖文件（`helper.go`，与 `parseControl`/`downmix` 并列），`import dsp` 纯包、不碰 windows。
- **默认关闭**：`audio-normalize` 默认 `false`；关闭路径必须与当前 helper 出帧逐位相同（passthrough），不惊动已验收的 Phase 1。
- 控制协议是 helper/main 同一 exe 的内部约定；capture 命令从 3 字段扩为 6 字段，两端（manager 构造、helper 解析）必须一致。
- git commit 严肃准确简洁、不含任何 AI 信息；提交到 `audio-waveform-normalization` 分支，不 push。
- 门禁（每任务末）：`gofmt -l`、`go vet ./...`、`go build ./...`、相关包 `go test`、`GOOS=linux` 构建（§0.3）。

**控制协议（Task 3/5 共用，定义一次）：** `capture <pid> <player> <norm:0|1> <target_dbfs:int> <maxgain_db:int>` / `stop <player>` 不变。

## Review Focus

- **keepalive/静音帧不得更新 EMA**：静音若衰减 EMA，恢复播放时增益过冲 ~1s。Apply 只在真音频帧调用（batch 非空）→ 测在 Task 4（wiring）+ Task 1（Apply 更新 EMA 的纯测试）。
- **非法 target/max-gain 被钳制**：target 应钳到 [−60,0]、max-gain 到 [0,45]；越界不得使增益爆掉 → 测在 Task 2。
- **归一化后 bins/RMS 可能 >1**：后端不钳、保留信息，消费者显示自行 clamp → 测在 Task 1（响输入+高增益断言存在 >1）。
- **关闭=逐位 passthrough**：`norm=0` 时帧值与输入完全相同 → 测在 Task 1。
- **per-player 覆盖 nil 跟随全局**：`<player>-audio-normalize` 未设回退全局 `audio-normalize` → 测在 Task 2。

---

### Task 1: Normalizer 与纯增益数学（helper 包，CI 可测）

**Files:**
- Modify: `audiocap/helper/helper.go`（新增纯函数与 Normalizer，与 parseControl/downmix 并列）
- Test: `audiocap/helper/helper_test.go`（新增）

**Interfaces:**
- Consumes: `dsp.Result{RMS float32, Bins []float32}`（既有）。
- Produces:
  - `const normAlpha = 0.03`（τ≈1s @ ~30fps；真机可调）、`const dbfsEpsilon = 1e-10`。
  - `func dbfs(x float64) float64` —— `20*log10(max(x, dbfsEpsilon))`。
  - `func gainFactor(rmsEst float64, targetDBFS, maxGainDB int) float64` —— `pow(10, min(float64(targetDBFS)-dbfs(rmsEst), float64(maxGainDB))/20)`。
  - `type Normalizer struct{ ... }`（私有字段：enabled bool、targetDBFS/maxGainDB int、emaMS float64、seeded bool）。
  - `func newNormalizer(enabled bool, targetDBFS, maxGainDB int) *Normalizer`。
  - `func (nz *Normalizer) Apply(r *dsp.Result)` —— enabled 时：首帧 `emaMS = r.RMS²` 播种、其后 `emaMS = normAlpha*r.RMS² + (1-normAlpha)*emaMS`；`g = gainFactor(sqrt(emaMS), target, maxGain)`；`r.RMS *= g`，每个 `r.Bins[i] *= g`。disabled 时直接返回（不动 r、不更新 EMA）。

- [ ] **Step 1: 写失败测试** `helper_test.go`：
  - `TestDbfsFloor`：`dbfs(0) == dbfs(dbfsEpsilon)`，且为有限值（非 Inf/NaN）。
  - `TestGainFactorCap`：极安静 `rmsEst` 下 `gainFactor` == `pow(10, maxGain/20)`（封顶生效）。
  - `TestGainFactorAttenuates`：`rmsEst` 对应 dBFS 高于 target 时 `gainFactor < 1`。
  - `TestNormalizerSeedAndApply`：enabled，首帧 `RMS=0.1` → Apply 后 `emaMS≈0.01`、RMS 按 g 缩放；连续同输入多帧收敛稳定。
  - `TestNormalizerDisabledPassthrough`：disabled，Apply 后 `r.RMS`、`r.Bins` 与输入逐位相同。
  - `TestNormalizerBinsMayExceedOne`：enabled、低响度输入（需高增益）、bins 含接近 1 的值 → Apply 后存在 bin >1（确认不钳）。
- [ ] **Step 2: 跑测试确认失败**（`go test ./audiocap/helper/ -run TestNormalizer -v`，FAIL：未定义）。
- [ ] **Step 3: 在 `helper.go` 实现** 上述常量/函数/类型（签名见 Interfaces；body 为标准 EMA+log/pow，实现者按签名与测试写）。
- [ ] **Step 4: 跑测试确认通过** + `gofmt`/`go vet`/`GOOS=linux go build ./audiocap/helper/`（纯文件须 linux 可构建）。
- [ ] **Step 5: 提交** `feat(audiocap): 音量归一化纯逻辑（EMA-AGC Normalizer + dbfs/增益）`。

### Task 2: config 归一化字段/flag/解析器/钳制

**Files:**
- Modify: `config/config.go`（PlayerConfig、Config、DefaultConfig、CLI flag、yaml 解析、解析器、钳制）
- Test: `config/` 下新增测试（如 `audionorm_test.go`）
- Modify（如需）：`tools/genconfig` 模板，使生成的 config 含新字段注释

**Interfaces:**
- Produces:
  - `PlayerConfig.AudioNormalize *bool`（nil=跟随全局，套 `IdleHide *int`）。
  - `Config.AudioNormalize bool`（yaml `audio-normalize`）、`.AudioNormalizeTarget int`（yaml `audio-normalize-target`）、`.AudioNormalizeMaxGain int`（yaml `audio-normalize-max-gain`）。
  - `DefaultConfig`：`AudioNormalize:false, AudioNormalizeTarget:-14, AudioNormalizeMaxGain:30`。
  - `func (c *Config) GetPlayerAudioNormalize(playerName string) bool`（套 `GetPlayerIdleHide`：per-player `*bool` 优先、否则全局 `AudioNormalize`）。
  - CLI：全局 `audio-normalize`(bool)/`audio-normalize-target`(int)/`audio-normalize-max-gain`(int)；per-player `<player>-audio-normalize`(bool)（playerCLI 加 `audioNormalize *bool`，用 `flag.Bool`）。
  - 钳制：target 钳到 [−60,0]、max-gain 钳到 [0,45]（全局；越界打 Warn，照 `clampPolls` 惯例）。

- [ ] **Step 1: 写失败测试**：
  - `TestGetPlayerAudioNormalize`：全局 true、某家 override false → 该家 false、其余 true；全局 false、某家 override true → 该家 true。
  - `TestClampAudioNormalizeTarget`/`MaxGain`：越界（如 target=10、max-gain=99）→ 钳到边界。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现**：按 `idle-hide` 的每一处（struct 字段、DefaultConfig、CLI flag 注册与 Visit 赋值、yaml 手动解析段 `m["audio-normalize"]`/`m[name+"-audio-normalize"]`、解析器、钳制函数）逐一补 `audio-normalize` 三件套。全局字段带 yaml tag 自动 unmarshal；per-player 走手动解析段（照 idle-hide）。
- [ ] **Step 4: 跑测试确认通过** + `GOOS=linux go build ./config/... ./tools/genconfig`（§0.3）+ `go vet`。
- [ ] **Step 5: 提交** `feat(config): 音量归一化配置（全局 + per-player 开关覆盖 + 钳制）`。

### Task 3: 控制协议——parseControl 结构体化 + 6 字段 capture

**Files:**
- Modify: `audiocap/helper/helper.go`（parseControl 新签名）
- Modify: `audiocap/helper/helper_test.go`（更新 parseControl 测试）
- Modify: `audiocap/helper/helper_windows.go`（parseControl 调用点解构 `control` + captureLoop 签名透传 norm 参数，**本任务暂不应用**，仅保持构建绿）

**Interfaces:**
- Produces:
  - `type control struct{ op string; pid uint32; player string; norm bool; target, maxGain int }`
  - `func parseControl(line string) (control, error)` —— capture 解析 6 字段（`norm` 用 `fields[3]=="1"`，target/maxGain `strconv.Atoi`）；字段数不符/非法数字报 error；`stop <player>` → `control{op:"stop", player:...}`。
  - captureLoop 签名加 `norm bool, target, maxGain int`（本任务透传、不使用；Task 4 消费）。
- Consumes（Task 5 对齐）：控制协议格式见 Global Constraints。

- [ ] **Step 1: 更新/新增测试**：`TestParseControlCapture`（`capture 123 kugou 1 -14 30` → control{capture,123,kugou,true,-14,30}）、`norm=0` 关、字段数不符报错、pid/target 非法报错、`TestParseControlStop`。
- [ ] **Step 2: 跑测试确认失败**（旧 3 字段断言失败）。
- [ ] **Step 3: 实现** parseControl 新签名（见 Interfaces）；同步更新 helper_windows.go 的调用点（从 `control` 取 op/pid/player 分发，capture 分支把 norm/target/maxGain 传入 captureLoop 新签名，captureLoop 暂不使用这三参）——保持 windows 构建绿。
- [ ] **Step 4: 跑测试确认通过** + `go build ./...`（windows）+ `GOOS=linux go build ./audiocap/helper/` + `go vet ./...`。
- [ ] **Step 5: 提交** `refactor(audiocap): 控制命令 capture 扩 6 字段携带归一化参数`。

### Task 4: helper 捕获循环应用 Normalizer

**Files:**
- Modify: `audiocap/helper/helper_windows.go`（控制分发把 control 的 norm 参数传入 captureLoop；captureLoop 构造 Normalizer；encodeFrame 应用）

**Interfaces:**
- Consumes: Task 1 的 `newNormalizer`/`Apply`；Task 3 的 `control` 字段。
- captureLoop 签名加 `norm bool, target, maxGain int`（或传 `*Normalizer`）；在 Activate 成功后 `nz := newNormalizer(norm, target, maxGain)`。
- `encodeFrame` 加 `nz *Normalizer` 参数：**仅在 batch 非空（真音频）分支**，`res := a.Analyze(w)` 后 `nz.Apply(&res)`，再填 `f.RMS/f.Bins`；keepalive 分支（batch 空）**不调 Apply**（不更新 EMA、零帧原样）。

- [ ] **Step 1: 接线**：captureLoop 收 norm 参数、构造 Normalizer，传入 encodeFrame；encodeFrame 真音频分支调 Apply。
- [ ] **Step 2: 构建校验**：`go build ./...`（windows）+ `go vet ./...`。
- [ ] **Step 3: 回归**：`go test ./audiocap/...`（Task 1 纯测试仍绿；确认 helper 包编译与既有测试不破）。
- [ ] **Step 4: 提交** `feat(audiocap): helper 捕获循环按控制参数应用音量归一化（仅真音频帧更新 EMA）`。
- 验证：端到端在 Task 6 后真机实测（本任务为 windows 接线，正确性由 Task 1 纯测试 + 真机视觉保证）。

### Task 5: manager 构造带归一化参数的 capture 命令

**Files:**
- Modify: `audiocap/manager/manager_windows.go`（applyControls 命令格式）、`audiocap/manager/gating.go`（NormParams 类型）、`audiocap/manager/manager_windows.go` 或 `jobobject_windows.go`（New 签名）
- Modify: `main.go`（New 接线传入解析器）
- Modify: `audiocap/manager/manager_windows_test.go`（TestApplyControls 更新为 6 字段断言）
- Modify（untracked，非提交）：`tools/smoke/main.go` 的 `manager.New` 调用随签名更新

**Interfaces:**
- Produces:
  - `type NormParams struct{ Enabled bool; Target, MaxGain int }`（gating.go，无平台依赖）。
  - `func New(sink AudioSink, status func(name string) string, normResolver func(player string) NormParams, exe string) *Manager`（在现签名加 `normResolver`）。
  - applyControls 的 capture 命令改为 `fmt.Sprintf("capture %d %s %s %d %d", pid, player, boolToBit(np.Enabled), np.Target, np.MaxGain)`（np=normResolver(player)）；stop 不变。
- Consumes: Task 2 的 `GetPlayerAudioNormalize` + 全局 target/maxGain（main 侧组装 normResolver）。

- [ ] **Step 1: 更新测试** `TestApplyControls`：构造带 normResolver 的 Manager，断言 capture 行含 6 字段（如 `capture 100 kugou 1 -14 30`）；PID 变更/移除序列同理带参数。
- [ ] **Step 2: 跑测试确认失败**（旧 3 字段断言失败）。
- [ ] **Step 3: 实现** NormParams、New 加参、applyControls 格式化；main.go 用 `func(p string) manager.NormParams{ return manager.NormParams{cfg.GetPlayerAudioNormalize(p), cfg.AudioNormalizeTarget, cfg.AudioNormalizeMaxGain} }` 接线；更新 tools/smoke 的 New 调用（传一个恒 `{false,...}` 的 resolver 即可）。
- [ ] **Step 4: 跑测试确认通过** + `go build ./...` + `go vet ./...` + `GOOS=linux go build ./config/... ./logger/... ./tools/genconfig`。
- [ ] **Step 5: 提交** `feat(audiocap): 捕获管理器按 config 下发每家归一化参数`。

### Task 6: openapi 契约说明

**Files:**
- Modify: `doc/openapi.yaml`（`/audio-ws` 与 `/{player}/audio-ws` 描述）

**Interfaces:** 无代码接口；文档契约。

- [ ] **Step 1: 补说明**：两端点描述加「归一化（`audio-normalize`）开启时，帧的 RMS 与 bins 为**已归一化**值，可能 >1；消费者显示时自行 clamp」。
- [ ] **Step 2: 校验** yaml 有效（`python -c "import yaml; yaml.safe_load(open('doc/openapi.yaml',encoding='utf-8'))"`）。
- [ ] **Step 3: 提交** `docs(openapi): 补音量归一化对 audio-ws 帧值的契约说明`。

---

**实现后（非 TDD 任务）：真机视觉实测与调参** —— 开启归一化，逐家放不同音量的歌，确认波形填充一致；据此调 `audio-normalize-target`/`-max-gain`/`normAlpha`。短跑、守蓝屏告诫、与邻座错开 RME。
