# 音频波形捕获 Phase 1 实现计划

> **状态：代码任务已全部合入 main（截至 `326e1cc`），复选框未回填——不要据此重新执行。计划中的手工验证步骤（如 Task 4 Step 5 的 soak，Task 10 Step 7 的杀 helper / 杀主服务 / 假死）是否执行无记录，不能视为已验证。** 本文为历史实施计划，与代码分叉时以代码为准。已知偏离：Task 5 的 `procscan.Names` 改为 `Players` + `PlayerProcess`（`65b6867`）；Task 9 端点注册改走声明式 `routes` 表（`b46e441`）；capture 控制命令后由归一化计划扩为 6 字段；另增 root 空闲帧（`d064e59`）与退避稳定复位（`20badfd`）。

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现。步骤用 `- [ ]` 复选框跟踪。

**Goal:** 把每个在跑播放器的音频输出经进程级 WASAPI loopback 捕获、算成自包含帧，经 `/audio-ws`（root 跟随 activePlayer）与 `/<player>/audio-ws`（固定）推给 OBS overlay，且任何故障都不拖垮歌词/特效主线。

**Architecture:** 独立 helper 进程（同一 exe `--audio-capture-helper` 子命令）做捕获+DSP，stdin 收控制、stdout 发帧；主服务的捕获管理器用进程快照发现 PID、按订阅门控+在播预热、经 Job Object 监管 helper，读 stdout 解复用后交 audioHub（克隆 effect hub + player 键路由）扇出。DSP 与帧编解码抽成纯 Go 包以便 CI 测试。

**Tech Stack:** Go 1.23，`golang.org/x/sys/windows`（COM/WASAPI/Job Object/Toolhelp 快照，纯 Go 手搓 vtable，不引 cgo），标准库 `testing`。

**Spec:** `docs/audio-waveform-capture-design.md`（v2，经对抗式评审修订）。计划从 spec 论证，执行者两份都读。

## Global Constraints

（每条逐字取自 spec / AGENTS.md，所有任务的要求隐含包含本节。）

- **纯 Go，不引入 cgo；`CGO_ENABLED=0` 保持可交叉编译。** helper 的 WASAPI/COM/Job Object 代码一律 `//go:build windows`，且**绝不被 `config/`、`logger/` import**（否则破坏 §0.3 的 `GOOS=linux go build ./config/... ./logger/... ./tools/genconfig`）。自检：`GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./tools/genconfig` 必须绿。
- **`NewCallback` 只能在包级 `var`、闭包无捕获（§0.4）**，per-instance 状态经 this 指针；受 `player/callback_lint_test` AST 门禁约束（全仓强制）。
- **主服务侧音频 goroutine 用本地 recover，绝不挂 `telemetry.Guard`**（Guard re-panic 会杀主进程=直播事故）。这是对 §1.2 的**有意豁免**，注释里写明。helper 侧另有自己的 telemetry.Init+Guard。
- **音频捕获只在正式启动后接线（启动时序，承重）**：manager 接线必须在 `checkAndUpdate` **之后**（照 §1.2 特效捕获器位置，step 15 一带），**绝不早于隐私提示（`PrintPrivacyNotice`）与更新检查（`checkAndUpdate`）**——`checkAndUpdate` 可能 `os.Exit`/替换 exe/`restartSelf`，在它之前起音频（乃至 spawn helper 子进程）是错的（10s 隐私、启动更新期间都不该拉起音频捕获）。helper 子进程入口（Task 7）短路早于一切 init，本就不跑隐私/更新。且 manager 懒汉：无 `/audio-ws` 订阅不 spawn helper——双保险。
- **helper 模式 stdout 只写帧、所有诊断走 stderr。** 移植 `tools/audioprobe` 内核时清除其全部 `fmt.Printf`→stdout。
- **绝不改共用 `s.upgrader` 的 `CheckOrigin`（`server.go:115` 恒真）**；`/audio-ws` 直接共用。
- **主服务侧发送一律非阻塞（drop-oldest）**（§2.1 承重不变量）。
- 四道门禁必须绿：`gofmt -l` / `go build ./...` / `go vet ./...` / `go test ./...`。文风：严肃准确简洁，无 AI 信息，commit 不含 AI 字样。
- 帧格式：进程 loopback 固定 48000Hz/2ch/float32（`GetMixFormat` 返回 `E_NOTIMPL`，硬编码）。

## Review Focus

（spec 隐含、但无单个任务测试覆盖、最可能咬到用户的输入/失败模式；每条已把测试落到拥有该代码的任务。）

1. **helper 吐坏字节/半帧/超大长度前缀** → 主服务 demux 只丢帧不 panic、按 magic 重对齐（Task 8 的解码器 fuzz/坏输入测试）。
2. **只连 root `/audio-ws`（OBS 主用法）、无 per-player 订阅** → helper 仍捕获当前 activePlayer、root 收得到帧（Task 7 `HasSubscribers` 单测：仅 root sub 时对 active 返回 true）。
3. **activePlayer 在安静前奏/暂停时** → Phase 1 如实流零帧、不崩、不误判（Task 4 静音帧仍按 cadence 发 keepalive；Task 6/9 不因零 RMS 停捕获）。
4. **helper 假死（进程活着不产帧）** → 帧活性看门狗 kill+重启（Task 9 watchdog 单测：注入「playing+订阅但无帧」→ 触发重启）。
5. **主服务崩溃/os.Exit 退出** → helper 随 Job Object 被内核回收，不成孤儿（Task 9 Job Object；无法在 CI 测，手工验证项写进任务）。

---

## 文件结构

**新增包（纯 Go，CI 可测）：**
- `audiocap/dsp/dsp.go` — RMS + 频谱（FFT）分析。无 windows 依赖。
- `audiocap/frame/frame.go` — 帧编解码：`magic|len|player-id|epoch|ts|payload(rms+bins)`，含重对齐解码器。无 windows 依赖。

**新增包（`//go:build windows`）：**
- `audiocap/wasapi/wasapi_windows.go` — 可复用进程级 loopback 捕获器（从 `tools/audioprobe` 抽取）：`Capturer` 含 Activate/Read/Close 对称释放。
- `audiocap/procscan/procscan_windows.go` — `CreateToolhelp32Snapshot` 进程发现 + 主进程定位 + PID 身份复核。
- `audiocap/helper/helper_windows.go` — helper 进程逻辑：控制循环、捕获管理、DSP、分帧、解耦写、keepalive、telemetry。
- `audiocap/manager/manager_windows.go` — 主服务侧：门控+预热、Job Object 监管、退避、活性看门狗、stdout 解复用、stdin 控制。

**新增（主服务，非 windows 门控）：**
- `server/audio.go` — audioHub（克隆 effect hub + player 键路由）。

**修改：**
- `main.go` — `--audio-capture-helper` 子命令入口；manager 接线。
- `server/server.go` — `audioHub` 字段、`NewServer` 构造、端点硬编码、`SetActivePlayer` 钩子。
- `player/kugou/kugou.go`、`player/wesing/wesing.go`、`player/qqmusic/qqmusic.go` — 导出进程名常量（单一真源）。
- `doc/openapi.yaml` — 新增 `/audio-ws`、`/<player>/audio-ws`。

**任务依赖顺序：** 1(名常量) → 2(dsp) → 3(frame) → 4(wasapi 抽取) → 5(procscan) → 6(helper) → 7(子命令入口) → 8(audioHub) → 9(server 接线) → 10(manager) → 11(main 接线) → 12(openapi)。2/3/8 之间无强依赖可并行；其余按接口依赖串。

---

## Task 1: 导出五家进程名常量（单一真源）

**Files:**
- Modify: `player/kugou/watchdog/watchdog.go`（`targetProcess` → 导出 `TargetProcessName`）
- Modify: `player/wesing/wesing.go`（内联 `"WeSing.exe"` → 包级导出 const）
- Modify: `player/qqmusic/qqmusic.go` 或 `player/qqmusic/mem.go`（内联 `"qqmusic.exe"` → 包级导出 const）
- Test: `audiocap/procscan/names_test.go`（下一任务建包后补；本任务仅导出）

**Interfaces:**
- Produces: `kugou.TargetProcessName`、`wesing.TargetProcessName`、`qqmusic.TargetProcessName`（string 常量）。cloudmusic/sodamusic 已有 `watchdog.TargetProcessName`。

- [ ] **Step 1: 导出 kugou 进程名**

把 `player/kugou/watchdog/watchdog.go` 的 `const targetProcess = "KuGou.exe"` 改为 `const TargetProcessName = "KuGou.exe"`，更新包内引用。

- [ ] **Step 2: 导出 wesing 进程名**

`player/wesing/wesing.go` 加包级 `const TargetProcessName = "WeSing.exe"`，把 `proc.FindProcess("WeSing.exe")` 处改用该常量。

- [ ] **Step 3: 导出 qqmusic 进程名**

`player/qqmusic/mem.go`（`"qqmusic.exe"` 出处）加包级 `const TargetProcessName = "qqmusic.exe"` 并改用。

- [ ] **Step 4: 构建自检**

Run: `go build ./... && go vet ./...`
Expected: 通过（纯重命名/提取常量）。

- [ ] **Step 5: Commit**

```bash
git add player/kugou/watchdog/watchdog.go player/wesing/wesing.go player/qqmusic/mem.go
git commit -m "refactor(player): 导出五家目标进程名常量供音频捕获复用"
```

---

## Task 2: DSP 包（RMS + 频谱），纯 Go 可测

**Files:**
- Create: `audiocap/dsp/dsp.go`
- Test: `audiocap/dsp/dsp_test.go`

**Interfaces:**
- Produces:
  ```go
  package dsp
  // Analyze 接收单声道 float32 样本（调用方已把交织立体声降混/取一路），返回该窗的分析结果。
  // bins 为幅度谱（0..1 归一），长度 = fftSize/2；rms 为该窗 RMS。
  type Result struct { RMS float32; Bins []float32 }
  type Analyzer struct { /* 持有 fftSize、窗函数、复用缓冲 */ }
  func New(fftSize int) *Analyzer
  func (a *Analyzer) Analyze(mono []float32) Result   // len(mono) 应 == fftSize；不足补零
  ```

- [ ] **Step 1: 写失败测试（RMS）**

```go
// audiocap/dsp/dsp_test.go
package dsp

import ("math"; "testing")

func TestRMSFullScaleSine(t *testing.T) {
    const n = 1024
    a := New(n)
    s := make([]float32, n)
    for i := range s { s[i] = float32(math.Sin(2*math.Pi*float64(i)/64)) } // 满幅正弦
    got := a.Analyze(s).RMS
    want := float32(math.Sqrt2 / 2) // ≈0.707
    if math.Abs(float64(got-want)) > 0.02 { t.Fatalf("RMS=%v want≈%v", got, want) }
}

func TestRMSSilence(t *testing.T) {
    a := New(1024)
    if r := a.Analyze(make([]float32, 1024)).RMS; r != 0 { t.Fatalf("silence RMS=%v want 0", r) }
}
```

- [ ] **Step 2: 写失败测试（频谱峰值在正确 bin）**

```go
func TestSpectrumPeakBin(t *testing.T) {
    const n = 1024
    a := New(n)
    s := make([]float32, n)
    // 频率 = 采样率/周期；这里周期 16 样本 → bin = n/16 = 64
    for i := range s { s[i] = float32(math.Sin(2*math.Pi*float64(i)/16)) }
    bins := a.Analyze(s).Bins
    peak := 0
    for i := range bins { if bins[i] > bins[peak] { peak = i } }
    if peak < 60 || peak > 68 { t.Fatalf("peak bin=%d want≈64", peak) }
    if len(bins) != n/2 { t.Fatalf("len(bins)=%d want %d", len(bins), n/2) }
}
```

- [ ] **Step 3: 运行验证失败**

Run: `go test ./audiocap/dsp/ -run TestRMS -v`
Expected: FAIL（包/函数未定义）。

- [ ] **Step 4: 实现 dsp.go**

实现 `New`/`Analyze`：应用窗函数（Hann）后做基-2 FFT（纯 Go 递归或迭代 Cooley-Tukey），取前 `n/2` bin 的模、按 `n/2` 归一到 0..1；RMS = `sqrt(mean(x^2))`（在加窗前的原始样本上算，避免窗衰减 RMS）。复用内部缓冲避免每帧分配。fftSize 非 2 的幂时向下取整或 panic（构造期校验）。

- [ ] **Step 5: 运行验证通过**

Run: `go test ./audiocap/dsp/ -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add audiocap/dsp/
git commit -m "feat(audiocap): 新增 DSP 包（RMS + 频谱），纯 Go 可测"
```

---

## Task 3: 帧编解码包，纯 Go 可测

**Files:**
- Create: `audiocap/frame/frame.go`
- Test: `audiocap/frame/frame_test.go`

**Interfaces:**
- Produces:
  ```go
  package frame
  const Magic uint32 = 0x56544157 // "VTAW"，用于 desync 重对齐
  const MaxPayload = 1 << 16       // 长度前缀上限，超过即判坏帧丢弃（防 OOM）
  type Frame struct {
      Player string   // 空串=root/降级；非空=具体播放器
      Epoch  uint32   // 切换/helper 重启递增
      TSNanos int64   // 系统单调（QPC）纳秒
      RMS    float32
      Bins   []float32
  }
  func Encode(dst []byte, f Frame) []byte              // 追加编码后字节，返回新 slice
  // Decoder 从字节流增量解帧，能在坏字节后按 Magic 重同步。
  type Decoder struct { /* 内部缓冲 */ }
  func (d *Decoder) Push(b []byte)                     // 喂入 stdout 读到的字节
  func (d *Decoder) Next() (Frame, bool, error)        // 取一帧；bool=有帧；error=坏帧已跳过（调用方记日志继续）
  ```

- [ ] **Step 1: 写失败测试（编解码往返）**

```go
// audiocap/frame/frame_test.go
package frame
import ("reflect"; "testing")

func TestRoundTrip(t *testing.T) {
    f := Frame{Player:"kugou", Epoch:3, TSNanos:123456789, RMS:0.42, Bins:[]float32{0,0.5,1,0.25}}
    var d Decoder
    d.Push(Encode(nil, f))
    got, ok, err := d.Next()
    if err != nil || !ok { t.Fatalf("Next ok=%v err=%v", ok, err) }
    if !reflect.DeepEqual(got, f) { t.Fatalf("got %+v want %+v", got, f) }
}
```

- [ ] **Step 2: 写失败测试（坏字节后按 Magic 重对齐、超长丢弃）**

```go
func TestResyncAfterGarbage(t *testing.T) {
    good := Encode(nil, Frame{Player:"a", Bins:[]float32{1}})
    var d Decoder
    d.Push([]byte{0xDE,0xAD,0xBE})   // 垃圾前缀
    d.Push(good)                      // 紧跟一帧好的
    // 第一次 Next 可能报坏帧(跳过垃圾)或直接对齐到 good；循环取直到拿到好帧
    var got Frame; var ok bool
    for i:=0; i<4 && !ok; i++ { got, ok, _ = d.Next() }
    if !ok || got.Player != "a" { t.Fatalf("resync 失败 got=%+v ok=%v", got, ok) }
}

func TestOversizedLengthRejected(t *testing.T) {
    var d Decoder
    // Magic + 一个超过 MaxPayload 的长度前缀
    buf := make([]byte, 8)
    putU32(buf[0:], Magic); putU32(buf[4:], uint32(MaxPayload+1))
    d.Push(buf)
    _, ok, err := d.Next()
    if ok || err == nil { t.Fatalf("超长前缀应判坏帧: ok=%v err=%v", ok, err) }
}
```

（`putU32` 为测试辅助，或用 `encoding/binary`。）

- [ ] **Step 3: 运行验证失败**

Run: `go test ./audiocap/frame/ -v`
Expected: FAIL。

- [ ] **Step 4: 实现 frame.go**

`Encode`：`Magic(u32) | payloadLen(u32) | epoch(u32) | ts(i64) | rms(f32) | playerLen(u16) | player | bins(f32×N)`（bins 数由 payloadLen 推出）。`Decoder.Next`：先在缓冲里找 `Magic`（找不到丢弃到末 3 字节保留可能的半 Magic）；读 len，若 `>MaxPayload` 判坏帧（跳过这 4 字节 Magic 继续找下一个）、返回 err；缓冲不足则等更多字节（返回 ok=false）；足够则解出 Frame。用 `encoding/binary.LittleEndian`。

- [ ] **Step 5: 运行验证通过 + Commit**

Run: `go test ./audiocap/frame/ -v` → PASS
```bash
git add audiocap/frame/
git commit -m "feat(audiocap): 新增帧编解码（含 Magic 重对齐、超长防护），纯 Go 可测"
```

---

## Task 4: 抽取可复用 WASAPI 捕获器

**Files:**
- Create: `audiocap/wasapi/wasapi_windows.go`（从 `tools/audioprobe/wasapi_windows.go` 抽取重构）
- Create: `audiocap/wasapi/wasapi_test.go`（结构体偏移断言，移自 audioprobe）
- Modify: `tools/audioprobe/main.go`、删除 `tools/audioprobe/wasapi_windows.go`/`wasapi_test.go`（改为 import 新包）
- Test（手工，windows）：反复 activate/close 无泄漏 soak

**Interfaces:**
- Produces:
  ```go
  //go:build windows
  package wasapi
  type Capturer struct { /* client, captureClient, event, pinned handler */ }
  // Activate 对 pid（含/不含子进程树）激活进程 loopback，格式固定 48k/2ch/f32。
  func Activate(pid uint32, includeTree bool) (*Capturer, error)
  // Read 阻塞等一批样本，写入 dst（交织 f32），返回帧数、是否静音标志、错误。
  func (c *Capturer) Read(dst []float32) (frames int, silent bool, err error)
  func (c *Capturer) Close() error   // 对称释放：Stop/Release client+captureClient、CloseHandle event、解 pin map、置 nil
  const SampleRate = 48000; const Channels = 2
  ```

- [ ] **Step 1: 移动并重构为 Capturer**

把 `tools/audioprobe/wasapi_windows.go` 的常量/GUID/结构体/回调/激活链移到 `audiocap/wasapi/wasapi_windows.go`。把 `activateProcessLoopbackClient` 包装进 `Activate() (*Capturer,...)`，把采集循环拆成 `Read()`（单次 `WaitForSingleObject`+`GetBuffer`/`ReleaseBuffer`），新增 `Close()` **对称释放**（当前 audioprobe 无 release 路径——补全：`IAudioClient::Stop`、Release client 与 captureClient、CloseHandle 事件、`handlerRelease` 归零解 pin）。保留 §0.4 的 NewCallback 包级 var 形态。

- [ ] **Step 2: 移动偏移断言测试**

把 `wasapi_test.go`（AUDIOCLIENT_ACTIVATION_PARAMS=12B / PROPVARIANT=24B / WAVEFORMATEXTENSIBLE=40B 等）移入新包。

- [ ] **Step 3: audioprobe 改用新包**

`tools/audioprobe/main.go` 改为 `import "Metabox-Nexus-PlayerCap/audiocap/wasapi"`，用 `wasapi.Activate`/`Read`/`Close` 替换原内联调用；删除 audioprobe 里的 COM 文件。

- [ ] **Step 4: 构建 + 门禁**

Run: `go build ./... && go vet ./... && go test ./audiocap/wasapi/`
Expected: 通过；`player/callback_lint_test` 仍绿（回调仍包级 var）。

- [ ] **Step 5: 手工 soak（windows，记录靠人）**

Run（本机放歌）：`go run ./tools/audioprobe -pid <QQ pid> -seconds 5`
Expected: RMS 非零、正常退出。补一个临时循环脚本 activate/close ×200 观察句柄数（任务管理器/handle 计数）不增长——**CI 跑不了 Windows-only，如实标注靠人跑**。

- [ ] **Step 6: Commit**

```bash
git add audiocap/wasapi/ tools/audioprobe/
git commit -m "refactor(audiocap): 抽取可复用 WASAPI 进程 loopback 捕获器（补对称释放），audioprobe 改用之"
```

---

## Task 5: 进程发现（Toolhelp 快照 + 主进程定位 + PID 身份）

**Files:**
- Create: `audiocap/procscan/procscan_windows.go`
- Create: `audiocap/procscan/names.go`（非 windows；集中五家进程名，含与各包常量对齐的守卫）
- Test: `audiocap/procscan/names_test.go`（纯 Go）；手工 windows 快照测试

**Interfaces:**
- Produces:
  ```go
  package procscan
  // Names 是要扫描的五家进程名（引自各 player 包常量，单一真源）。
  var Names = []string{kugou.TargetProcessName, wesing.TargetProcessName, qqmusic.TargetProcessName, cloudmusic工.., sodamusic工..}
  //go:build windows
  type Proc struct { PID uint32; Name string; PPID uint32; CreateTime int64 }
  func Snapshot() ([]Proc, error)                       // CreateToolhelp32Snapshot 一次枚举
  func MainPID(procs []Proc, name string) (uint32, bool) // 定位「主进程」：同名中无同名父的那个（Chromium 主进程无 --type=）
  func Alive(pid uint32, name string) bool               // 复核 PID 仍是该名进程（防 PID 复用 TOCTOU）
  ```
  （`cloudmusic工..`/`sodamusic工..` 指其 watchdog 包已导出的 `TargetProcessName`；实际写全 import 路径。）

- [ ] **Step 1: 写失败测试（Names 与各包常量一致）**

```go
// audiocap/procscan/names_test.go
package procscan
import ("strings";"testing"; "Metabox-Nexus-PlayerCap/player/kugou/watchdog")
func TestNamesMatchPackages(t *testing.T) {
    if !contains(Names, watchdog.TargetProcessName) { t.Fatalf("Names 缺 kugou 进程名") }
    // ... 五家逐一断言（EqualFold），防音频侧副本与 player 包分叉
}
```

- [ ] **Step 2: 运行失败 → 实现 names.go**

`Names` 直接引用五家导出常量（cloudmusic/sodamusic 从各自 watchdog 包，kugou/wesing/qqmusic 从 Task 1 导出的常量）。若某家不便 import（循环依赖）则在此 hardcode 并靠本测试守卫。

- [ ] **Step 3: 实现 procscan_windows.go**

`Snapshot` 用 `windows.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS,0)` + `Process32First/Next` 收集 PID/Name/PPID（`th32ParentProcessID`）；CreateTime 可选（需 OpenProcess+GetProcessTimes，失败置 0 不阻断）。`MainPID`：同名集合里选「其 PPID 不在同名集合中」的那个（Chromium 主进程的父不是同名浏览器）；多个则取 PID 最小/最早。`Alive`：快照里该 PID 仍存在且名匹配。

- [ ] **Step 4: 门禁 + 手工**

Run: `go test ./audiocap/procscan/`（names 测试，CI 可跑）；`go build ./...`。
手工（windows）：小程序打印 `Snapshot()`/`MainPID(procs,"cloudmusic.exe")`，核对拿到网易云主进程 PID。

- [ ] **Step 5: Commit**

```bash
git add audiocap/procscan/
git commit -m "feat(audiocap): 进程发现（Toolhelp 快照+主进程定位+PID 身份复核）"
```

---

## Task 6: helper 进程逻辑（捕获+DSP+分帧+解耦写+keepalive+telemetry）

**Files:**
- Create: `audiocap/helper/helper_windows.go`
- Create: `audiocap/helper/helper_test.go`（控制解析、降混、keepalive 逻辑——尽量抽成非 windows 可测的纯函数）
- 手工 windows：端到端抓帧

**Interfaces:**
- Consumes: `wasapi.Activate/Read/Close`（Task 4）、`dsp.Analyzer`（Task 2）、`frame.Encode`（Task 3）。
- Produces: `func Run(stdin io.Reader, stdout io.Writer, dsn string)`（阻塞直到 stdin EOF）。控制协议（stdin，行式 UTF-8）：`capture <pid> <player>\n`、`stop <player>\n`。数据（stdout）：`frame.Encode` 帧流，**仅帧**。

- [ ] **Step 1: 写失败测试（控制行解析）**

```go
// helper_test.go —— 把解析抽成纯函数便于测试
func TestParseControl(t *testing.T) {
    op, pid, player, err := parseControl("capture 1234 kugou")
    if err!=nil || op!="capture" || pid!=1234 || player!="kugou" { t.Fatal(...) }
    if _,_,_,e := parseControl("garbage"); e==nil { t.Fatal("应报错") }
}
```

- [ ] **Step 2: 写失败测试（立体声降混为单声道）**

```go
func TestDownmixStereo(t *testing.T) {
    // 交织 [L0,R0,L1,R1] → mono[i]=(L+R)/2
    mono := downmix([]float32{1,-1, 0.5,0.5}, 2)
    want := []float32{0, 0.5}
    // 断言逐元素近似
}
```

- [ ] **Step 3: 运行失败 → 实现 helper_windows.go**

结构：
- `Run` 先 `telemetry.Init`（用传入 dsn）+ `defer telemetry.Guard()`（这是 helper 进程自己的，允许 re-panic 杀 helper——隔离在此进程）。
- 起 goroutine 读 stdin 逐行 `parseControl`：`capture` → `wasapi.Activate(pid,true)` 存进 `map[player]*captureState`（含 Capturer、dsp.Analyzer、epoch）；`stop` → `Close` 并删除。stdin EOF → 关所有捕获、退出（防孤儿兜底）。
- 每路 `captureState` 一个采集 goroutine：循环 `Read`→`downmix`→按 cadence（累积到 fftSize）`Analyzer.Analyze`→`frame.Encode`（player/epoch/QPC-ts/rms/bins）→**推入有界 channel（drop-oldest）**；即使 `silent` 也照发（keepalive）。
- 一个写 goroutine：从有界 channel 取帧、写 stdout（**只写帧**）；写用非阻塞/deadline，父进程停顿不冻采集（§2.5）。
- QPC 时间戳：`windows.QueryPerformanceCounter`。

- [ ] **Step 4: 运行纯 Go 测试通过**

Run: `go test ./audiocap/helper/` （parseControl/downmix，CI 可跑）；`go build ./...`；`go vet ./...`。

- [ ] **Step 5: 手工端到端（windows）**

临时用主服务或一个小 driver 起 `Run`，stdin 喂 `capture <QQ pid> qqmusic`，放歌，确认 stdout 有帧、RMS 非零、暂停时 keepalive 仍来。

- [ ] **Step 6: Commit**

```bash
git add audiocap/helper/
git commit -m "feat(audiocap): helper 进程（捕获+DSP+分帧+解耦写+keepalive+独立 telemetry）"
```

---

## Task 7: main.go 子命令入口

**Files:**
- Modify: `main.go`（`--audio-capture-helper` 短路，在 `defer telemetry.Guard()` 之后、任何 init 之前，照 `--kugou-patch-helper` `main.go:56-59`）

**Interfaces:**
- Consumes: `helper.Run`（Task 6）。

- [ ] **Step 1: 加短路入口**

在 `main()` 现有 helper 短路旁加（**位置：`defer telemetry.Guard()` 之后、`ensureCanonicalName`/`config.Load`/`telemetry.Init`/隐私提示/`checkAndUpdate` 等一切 init 之前**，照 `--kugou-patch-helper` `main.go:56-59`）。helper 是独立进程，短路即返回——**永不跑隐私提示/更新检查**，符合「音频只在正式启动接线」的时序约束：
```go
if len(os.Args) >= 2 && os.Args[1] == "--audio-capture-helper" {
    dsn := os.Getenv("AUDIO_HELPER_DSN") // 父进程注入
    helper.Run(os.Stdin, os.Stdout, dsn)
    return
}
```

- [ ] **Step 2: 构建 + 手工**

Run: `go build ./...`；手工 `Metabox-Nexus-PlayerCap.exe --audio-capture-helper`（stdin 立即 EOF → 干净退出）。

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat(main): 新增 --audio-capture-helper 子命令入口"
```

---

## Task 8: audioHub（克隆 effect hub + player 键路由）

**Files:**
- Create: `server/audio.go`
- Test: `server/audio_test.go`（drop-oldest、HasSubscribers 映射、按键路由——纯 Go 可测）

**Interfaces:**
- Consumes: `frame` 帧字节（原样转发 payload）。
- Produces:
  ```go
  type audioSub struct { ch chan []byte; player string } // player=""=root
  type audioHub struct { mu sync.Mutex; subs map[*audioSub]struct{}; active string }
  func newAudioHub() *audioHub
  func (h *audioHub) onActivePlayerChanged(name string)   // 只写 h.active（h.mu），不阻塞不 panic
  func (h *audioHub) HasSubscribers(name string) bool      // per-player sub(name) OR (root sub AND name==h.active)
  func (h *audioHub) BroadcastAudioFrame(player string, payload []byte) // drop-oldest；发 per-player(player) + 若 player==h.active 发 root
  func (h *audioHub) AnySubscribers() bool                 // 供扫描门控
  ```

- [ ] **Step 1: 写失败测试（HasSubscribers 把 root 计入 active）**

```go
func TestHasSubscribersRootFollowsActive(t *testing.T) {
    h := newAudioHub()
    root := &audioSub{ch: make(chan []byte,8), player:""}
    h.mu.Lock(); h.subs[root]=struct{}{}; h.mu.Unlock()
    h.onActivePlayerChanged("kugou")
    if !h.HasSubscribers("kugou") { t.Fatal("仅 root sub 时 active=kugou 应为 true") }
    if h.HasSubscribers("wesing") { t.Fatal("非 active 应为 false") }
}
```

- [ ] **Step 2: 写失败测试（drop-oldest + 按键路由）**

```go
func TestBroadcastDropOldestAndRouting(t *testing.T) {
    h := newAudioHub(); h.onActivePlayerChanged("kugou")
    root := &audioSub{ch: make(chan []byte,1), player:""}
    per := &audioSub{ch: make(chan []byte,1), player:"wesing"}
    h.mu.Lock(); h.subs[root]=struct{}{}; h.subs[per]=struct{}{}; h.mu.Unlock()
    h.BroadcastAudioFrame("kugou", []byte("a")) // → root(active)
    h.BroadcastAudioFrame("kugou", []byte("b")) // 通道满 → 丢旧留新
    if got := <-root.ch; string(got)!="b" { t.Fatalf("root 应保最新 b, got %s", got) }
    if len(per.ch)!=0 { t.Fatal("wesing per-player 不该收到 kugou 帧") }
}
```

- [ ] **Step 3: 运行失败 → 实现 server/audio.go**

照 `server/effect.go` 的 `effectSub`/`effectHub`/`BroadcastEffectFrame` drop-oldest（buffer 8），加 `player` 键与 `active` 缓存。`BroadcastAudioFrame` 在 `h.mu` 内遍历：`sub.player==player`（per-player）或 `sub.player=="" && player==h.active`（root）才投递，投递用 select-default drop-oldest。`handleAudioWS(playerName)` 照 `handleEffectWS`（Upgrade 共用 `s.upgrader`、登记、writer goroutine 写失败必 `conn.Close()`、清理）——**writer/read goroutine 用本地 recover 不挂 Guard**。

- [ ] **Step 4: 运行通过 + Commit**

Run: `go test ./server/ -run TestHasSub -v && go test ./server/ -run TestBroadcast -v` → PASS
```bash
git add server/audio.go server/audio_test.go
git commit -m "feat(server): audioHub（克隆 effect 扇出 + player 键路由 + root 跟随 active）"
```

---

## Task 9: server 接线（字段/构造/端点/钩子）

**Files:**
- Modify: `server/server.go`（`audioHub` 字段 `:99` 旁、`NewServer` 构造 `:120` 旁、端点硬编码 `:621-623` 旁、`SetActivePlayer` 钩子 `:339-342`）
- Modify: `server/server.go` 暴露 `AudioHub()` getter 或让 manager 经 Server 拿到 hub 与 `GetPlayerStatus`

**Interfaces:**
- Consumes: `newAudioHub`、`handleAudioWS`（Task 8）。
- Produces: `func (s *Server) AudioHub() *audioHub`；端点 `/audio-ws`、`/<player>/audio-ws`。

- [ ] **Step 1: 加字段与构造**

`Server` 加 `audioHub *audioHub`；`NewServer` 加 `audioHub: newAudioHub()`。

- [ ] **Step 2: 端点硬编码**

在 `server.go` effect 端点旁：
```go
mux.HandleFunc("/audio-ws", s.handleAudioWS(""))
for name := range s.playerStates {
    mux.HandleFunc("/"+name+"/audio-ws", s.handleAudioWS(name))
}
```

- [ ] **Step 3: SetActivePlayer 钩子**

在 `SetActivePlayer` 的 `if changed` 块（与 `BroadcastEffectStatus` 并列）加 `s.audioHub.onActivePlayerChanged(name)`。**该钩子只写 h.mu 缓存、非阻塞、不回调 router、不 panic**（§3.4）。捕获管理器的「切换即重新门控」由 manager 侧订阅/轮询实现（Task 10），钩子不在 r.mu 内触发捕获动作。

- [ ] **Step 4: 门禁**

Run: `go build ./... && go vet ./... && go test ./server/`
Expected: 通过。

- [ ] **Step 5: Commit**

```bash
git add server/server.go
git commit -m "feat(server): 接入 audioHub 字段/构造/端点/SetActivePlayer 钩子"
```

---

## Task 10: 捕获管理器（门控+预热+Job Object 监管+退避+看门狗+解复用）

**Files:**
- Create: `audiocap/manager/manager_windows.go`
- Create: `audiocap/manager/gating.go`（非 windows；门控+预热的纯决策函数，可测）
- Create: `audiocap/manager/joblobject_windows.go`（Job Object 封装）
- Test: `audiocap/manager/gating_test.go`（纯 Go）；手工 windows

**Interfaces:**
- Consumes: `procscan`（Task 5）、`server.AudioHub()`、`GetPlayerStatus`、`frame.Decoder`（Task 3）、helper 子命令（Task 7）。
- Produces: `func New(hub *audioHub, status func(name string) string, exe string, dsn string) *Manager`；`func (m *Manager) Run()`（阻塞）；`func (m *Manager) Wake()`（activePlayer 变更时唤醒重算门控）。

- [ ] **Step 1: 写失败测试（门控决策：应捕获集合）**

```go
// gating_test.go
func TestDesiredCaptureSet(t *testing.T) {
    // 有 root sub、active=kugou、在播集合={kugou,wesing}、per-player 订阅={qqmusic}
    got := desiredCaptures(hasRoot(true), active("kugou"),
        playing([]string{"kugou","wesing"}), perPlayerSubs([]string{"qqmusic"}))
    // 期望：qqmusic(有per订阅) + 在播集合(kugou,wesing，因 root 订阅要预热)；
    want := set("qqmusic","kugou","wesing")
    assertSetEqual(t, got, want)
}
func TestNoSubsNoCapture(t *testing.T) {
    if len(desiredCaptures(hasRoot(false), active(""), playing([]string{"kugou"}), perPlayerSubs(nil)))!=0 {
        t.Fatal("无任何订阅应捕获空集")
    }
}
```

- [ ] **Step 2: 运行失败 → 实现 gating.go**

`desiredCaptures` 纯函数：`set = perPlayerSubs`；若 `hasRoot` 则 `set ∪= playing`（预热在播集合，保证 active 恒热）；返回 set。（active 本身若在播已含在 playing；若 active 未在播则不预热，root 自然收零帧。）

- [ ] **Step 3: 实现 Job Object 封装**

`joblobject_windows.go`：`CreateJobObject` → `SetInformationJobObject(JobObjectExtendedLimitInformation, {BasicLimitInformation.LimitFlags: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE})` → 返回 handle；`Assign(job, procHandle)`。用 `x/sys/windows`（有 `CreateJobObject`/`AssignProcessToJobObject`/`SetInformationJobObject`）。

- [ ] **Step 4: 实现 manager_windows.go**

`Run` 循环（秒级 tick + `Wake()` 唤醒，二选一先到）：
- **门控扫描**：`hub.AnySubscribers()` 为假 → 确保 helper 已拆除、跳过（按需节能）。为真 → `procscan.Snapshot`，对 `desiredCaptures(...)` 里每个 player 用 `MainPID` 求 PID（`Alive` 复核防复用），下发 `capture <pid> <player>`；不再需要的下发 `stop`。
- **helper 监管**：需要捕获且 helper 未起 → spawn（`exec.Command(exe,"--audio-capture-helper")`，设 `AUDIO_HELPER_DSN` env、`cmd.Stderr=os.Stderr`、StdinPipe/StdoutPipe），创建 Job Object 并 `Assign`（防孤儿）。stdout 起读 goroutine：`frame.Decoder` 增量解帧→`hub.BroadcastAudioFrame(f.Player,payload)`；**本地 recover 不挂 Guard**，坏帧靠 Decoder 跳过。
- **退避 + 放弃**：helper 退出 → 封顶指数退避重启（cap 30s）；连续 N 次快速失败 → 放弃本会话（标降级、停重试）。
- **活性看门狗**：记录每 player 最后收帧时刻；某 player 在 desired 集合且 helper 在跑却持续 N 个 cadence 无帧 → 判 hang → kill helper（Job Object close 或 Process.Kill）走退避重启。
- `Wake()`：activePlayer 变更时由 server 侧调用（经 Server 暴露的回调或 manager 注册），立即重算门控消除切换空窗。

- [ ] **Step 5: 启动孤儿清扫**

`Run` 起始：`procscan` 找残留的本 exe `--audio-capture-helper` 进程（按 exe 路径+命令行）并 kill，照 `park.RestoreOrphaned` 先例。

- [ ] **Step 6: 纯 Go 测试通过 + 门禁**

Run: `go test ./audiocap/manager/ -run TestDesired -v && go test ./audiocap/manager/ -run TestNoSubs -v`（gating，CI 可跑）；`go build ./... && go vet ./...`。

- [ ] **Step 7: 手工（windows）**

主服务起后连 `/audio-ws`，放歌验波形帧到达；切歌验无空窗；杀 helper 验退避重启；杀主服务验 helper 随之消失（Job Object）；`taskkill` 制造 helper 假死验看门狗重启。**均 CI 跑不了，靠人跑并记录。**

- [ ] **Step 8: Commit**

```bash
git add audiocap/manager/
git commit -m "feat(audiocap): 捕获管理器（门控+预热+Job Object 监管+退避+活性看门狗+解复用）"
```

---

## Task 11: main.go 接线捕获管理器

**Files:**
- Modify: `main.go`（在特效捕获器接线旁 `:271` 一带构造并起 manager）

**Interfaces:**
- Consumes: `manager.New/Run/Wake`（Task 10）、`server` 的 hub 与 `GetPlayerStatus`。

- [ ] **Step 1: 构造并起 manager（位置承重：必须在 checkAndUpdate 之后）**

**接线点在特效捕获器旁（`main.go:271` 一带，§1.2 step 15），即 `checkAndUpdate`（step 7）之后。绝不放到隐私提示/更新检查之前**（见 Global Constraints 的启动时序条）。

```go
audioMgr := manager.New(srv.AudioHub(), srv.GetPlayerStatus, selfExe, telemetryDSN)
go func() { defer localRecover("audio-manager"); audioMgr.Run() }() // 本地 recover，非 telemetry.Guard
```
把 manager 的 `Wake` 注册给 server，`SetActivePlayer` 钩子里除写 audioHub 缓存外也调 `Wake`（非阻塞：`Wake` 只向带缓冲/select-default channel 发信号）。`localRecover` 为音频子系统的本地恢复包装（记日志、不 re-panic）。

- [ ] **Step 2: 门禁 + 手工**

Run: `go build ./... && go vet ./... && go test ./...`
手工：整体起服务，走 Task 10 Step 7 的实机项。

- [ ] **Step 3: Commit**

```bash
git add main.go server/server.go
git commit -m "feat(main): 接入音频捕获管理器（本地 recover 不挂 Guard，切换唤醒）"
```

---

## Task 12: openapi 补端点

**Files:**
- Modify: `doc/openapi.yaml`（新增 `/audio-ws`、`/<player>/audio-ws`；说明二进制帧 WS、payload 结构）

- [ ] **Step 1: 补端点定义**

照现有 effect-ws 在 `/service-status` 端点表与 paths 的记法，加 root 与 per-player 的 `/audio-ws`（WS、二进制帧）。注明帧 payload = epoch+ts+rms+bins（Phase 1）。

- [ ] **Step 2: 校验 + Commit**

Run: openapi 可用 `swagger-cli validate` 或人工核 YAML 合法。
```bash
git add doc/openapi.yaml
git commit -m "docs(openapi): 补 /audio-ws 与 per-player 音频波形端点"
```

---

## 自检（写完计划后对 spec 复核）

- **覆盖**：spec §2 捕获→Task 4/6；§2.3 DSP→Task 2/6；§2.5 分帧/解耦→Task 3/6；§3.1 发现+门控+预热→Task 5/10；§3.2 audioHub→Task 7/8；§3.3 端点+钩子→Task 9；§3.4 钩子戒律→Task 9；§4 帧契约→Task 3；§5 ASIO 零帧→Task 6（keepalive 静音照发）+ Task 10（零 RMS 不停捕获）；§6 崩/假死/孤儿/可观测→Task 6(telemetry)/Task 10(Job Object+退避+看门狗+启动清扫)；§7 config→Phase 1 内置默认，未建 config 项（如需再按 §3.3）；§8 打包→同一 exe 零改动（无任务）；§9 测试→散落各任务；openapi→Task 12。
- **Review Focus 五条**：坏帧不崩→Task 3 解码器测试 + Task 10 本地 recover；仅 root→Task 8 HasSubscribers 测试；安静/暂停→Task 6 keepalive；假死→Task 10 看门狗；主服务退出→Task 10 Job Object（手工验）。
- **类型一致**：`Capturer.Activate/Read/Close`、`dsp.Analyzer.Analyze/Result`、`frame.Encode/Decoder.Next/Frame`、`audioHub.BroadcastAudioFrame/HasSubscribers/onActivePlayerChanged`、`manager.New/Run/Wake` 全计划一致引用。
- **已知留白（非占位，属 spec §10 开放项，执行时定）**：非 Electron 主 PID 启发式（Task 5 `MainPID` 先用「无同名父」通用规则，QQ/WeSing 单进程天然命中）；FFT 窗大小/cadence 具体值（Task 2/6 取 fftSize=1024、cadence≈30fps 起）；Job Object/QPC 的确切 `x/sys/windows` 符号（Task 10/6 执行时以该库为准）。
