# 网易云「特效歌词」镜像捕获 — 设计文档（as-built）

## 背景与目标
网易云音乐桌面版在「歌曲详情页」里有一套官方特效歌词（极光/霓虹/水底/静谧/液态流体…），
渲染在一个 **WebGL canvas `#lyric-effect-canvas-id`** 上，自带歌曲封面背景。
很多主播用 OBS 直播，希望把这套特效画面**引出来放进我们自己的 HTML overlay**，供浏览器源捕获，
并和已有的「多播放器」体系协同（不是网易云活跃输出时淡出等）。

本功能 = 把网易云主渲染的特效 canvas **镜像**到我们的前端页面（`effect_page.html`），供 OBS 捕获。

## 核心方法：纯层捕获（直读 canvas，与 chrome 无关）

> 早期方案用 `Page.startScreencast` 截**整个视口**，再用 CSS 双击隐藏 chrome（工具栏/顶栏/进度条）。
> 缺点：截的是合成画面，要干净就得把 chrome 在主播自己屏幕上也藏掉，二者冲突。

**现方案**：注入页面脚本，直接 `drawImage(#lyric-effect-canvas-id)` 读这一层的像素，编码 JPEG 后经
WebSocket 推回 Go。工具栏/顶栏/进度条是浏览器合成在 canvas **之上**的 DOM 层，读 canvas backing store
**永远只有「封面背景 + 粒子 + 歌词文字」，绝不含 chrome** → **主播可常开工具栏正常用，OBS 仍得纯特效层**。

### ⚠️ 铁律：严格只读，绝不修改源 canvas
`drawImage` 复制到**我们自己的画布**再编码。**绝不**改 `#lyric-effect-canvas-id` 的
`width`/`height`/`style`（曾试图强制提高渲染分辨率）——**实测会让网易云崩溃**，且该 canvas 无 CSS 宽度时
改 `width` 会撑破布局/变形。窗口尺寸本就可变（最大化/手动），任何固定尺寸假设都站不住。

## 已验证的关键事实（实测）
- **取像素**：WebGL canvas 默认 `preserveDrawingBuffer:false`，合成后绘制缓冲被清空，`drawImage`/`toDataURL`
  在帧外读取只有 **~50% 命中**（时灵时不灵）。**注入 `getContext` shim 强制 `preserveDrawingBuffer:true`**
  后变成 **稳定 100%** 命中。shim 必须在 WebGL context 创建前注入。
- **SPA 注入时机**：orpheus(app.html) 是单页应用，开歌在同文档内建 canvas。同文档 `eval` 一次 shim 即对
  「之后创建的 context」生效；**仅当连上时已有歌（context 已建）才需 `Page.reload` 一次**让 shim 生效。
- **封面背景烤进 WebGL canvas**：背景模糊封面 + 粒子 + 文字是同一个 WebGL 场景，DOM/CDP 无法单独关背景或调透明。
  → 纯层必然带封面背景（这就是特效本体）；唯一透明度旋钮是整块画面的 `opacity`（前端参数）。
- **分辨率 = 网易云窗口尺寸**：网易云把特效渲染在 **CSS 分辨率（DPR-unaware）**，例如 1059px 宽窗口 → canvas
  backing 1059×752。在主播屏上由合成器上采样到物理像素（看着清晰），但我们读到的真实细节就是窗口 CSS 尺寸。
  **想更清晰 → 放大网易云窗口**（我们不强制改渲染分辨率，会崩溃/变形）。
- **30fps 瓶颈是「编码并发度」而非编码本身**：单帧串行 `toBlob` 仅 ~23fps；**画布池 + 3 帧并发 `toBlob`** 达
  ~68fps（`toBlob` 异步、不阻塞主线程）。Worker 方案因 bitmap 跨线程传输反而更慢，故走主线程。
  `createImageBitmap` ~179fps、rAF 60fps 都不是瓶颈。
- **细线条 JPEG 色块**：原生分辨率下细对角线易出 JPEG 色度二次采样的色边；**默认 q95 基本消除**（本机回环带宽无忧）。
- **不需要交互式 devtools**：程序化连 CDP WebSocket 即可，无「inspect 光标 / 双击放大 / 卡顿」问题。

### 保活（窗口不可见时是否还出帧）实测矩阵
| 窗口状态 | 出帧 | 结论 |
|---|---|---|
| 前台/可见 | 注入 rAF 循环稳定出帧 | ✅ |
| 被其他窗口遮挡 | 启动 flags 改善但偶发卡，不稳定 | ⚠️ best-effort |
| 真·最小化 | 页面 hidden → rAF 暂停 → 不出帧（**前端冻结最后一帧**，正是 fadeout/freeze 所需） | 设计如此 |
| **off-screen 泊车（移出虚拟桌面包围盒、保持 shown）** | **持续出帧、前台随便用** | ✅✅ park 策略用 |

根因：最小化是 Win32/Chromium「窗口隐藏」判定，启动参数只能部分干预，天生 flaky。
off-screen 让窗口「可见且未遮挡」，合成器/ rAF 持续运行 → 持续出帧。

## 架构

### 1. 捕获器 `player/cloudmusicv3/effect/effect.go`
- 独立开一条到同一页面的 CDP WebSocket（与取词的 `cdp.Client` 分离）。
- 保活：`Emulation.setFocusEmulationEnabled(true)` + `Page.setWebLifecycleState('active')`。
- 注入两段脚本（`Page.addScriptToEvaluateOnNewDocument` + 立即 `eval` 当前文档）：
  - **`captureInjectJS`**：① getContext shim 强制 `preserveDrawingBuffer:true`；② 抓帧循环：rAF 节流 →
    画布池 `drawImage(canvas)` 快照 → 并发 `toBlob(jpeg)` → 经 page→Go 的 ingest WS 推字节。
    无订阅者时 `window.__mbxCapPaused=true` 暂停省 CPU。超大窗口（>`captureOutMaxW`=1920）才降采样保帧率。
  - **`chromeInjectJS`**：双击详情页切换隐藏顶栏/底栏/进度条/边缘渐变（对主播本机视图仍有用；纯层抓帧本就不含 chrome）。
- `purelayerSession`：注入 + 参数热更（gen 变）+ 保活探活 + 仅当已有歌且未启用 preserveDrawingBuffer 才 reload 一次。
- `ingestFrame`：帧门控——仅 `showing && !rawMinimized && now>=gateUntilNano` 时才广播；否则前端冻结最后一帧。
- **兜底**：环境变量 `MBX_EFFECT_MODE=screencast` 回退到旧的 `Page.startScreencast` 像素源（含 chrome）。
- 检测协程：
  - `pollShowingLoop`（~20ms）：读 `#vinyl-page-container` 的 **computed transform ty**（退/进详情页时 style
    属性几乎不变，真正动画的是 computed transform）；|ty|>3px 视为正在滑动 → 不显示。退详情页一开始（主页未露出）
    即可检测到，前端据此冻结，不漏主页。`showing = 详情页在显示 && 未最小化`。
  - `windowStateLoop`（~80ms）：检测最小化、执行 park 策略（见下）。

### 2. 传输 `server/effect.go`
- `/cloudmusicv3/effect-ws`：向 OBS 端推**二进制 JPEG 帧** + 文本状态 JSON。满则丢最新帧（低延迟）。
- `/cloudmusicv3/effect-ingest`：接收注入脚本推来的二进制帧 → 捕获器注册的 `ingestHandler`（门控后广播）。
- `/cloudmusicv3/effect-control`：运行时控制策略 / 手动 park（见下）。
- 状态 JSON：`{"type":"status","cmActive":bool,"showing":bool}`。
  - `cmActive` = 活跃播放器是否网易云（`activePlayer==cloudmusicv3`）。
  - `showing` = 详情页在显示且未最小化。

### 3. off-screen 泊车 `player/cloudmusic/park/park.go`（Windows-only）
- 存 `WINDOWPLACEMENT` → 移到虚拟桌面包围盒外（`SM_XVIRTUALSCREEN+SM_CXVIRTUALSCREEN`）→ 保持 shown+no-activate。
- **`SetWindowPlacement` 会把位置钳到可见工作区**，故还需 `SetWindowPos`（不钳制）强制到屏外。
- 崩溃兜底：泊车状态落盘；启动时若发现遗留泊车文件 → 立即还原。内存态 `parkedMem`（IsParked 不读盘）。
- 泊车时经 CDP 强制隐藏 chrome（屏外保纯净）；unpark 还原到泊车前的 chrome 状态。

### 4. 启动参数 `player/cloudmusic/watchdog/process.go`
注入：`--remote-debugging-port=9222 --disable-backgrounding-occluded-windows --disable-renderer-backgrounding
--disable-background-timer-throttling --disable-features=CalculateNativeWinOcclusion`。
探测按 `keepaliveMarker` 判断是否已注入；未注入则杀掉重启。

### 5. 前端 `effect_display.html`（iframe 入口壳，透传 search + cache-buster）→ `effect_page.html`（实际渲染）
- 连 `/cloudmusicv3/effect-ws` 拿二进制帧 → `createImageBitmap` → 画到 `#view`；`#freeze` 叠层做交叉淡入。
- 单帧解码（保证顺序，避免冻结时回跳）；恢复时（帧间隔 >250ms）冻结帧叠上层淡出 → 交叉淡入。

## 策略与状态机

### 最小化/迷你时策略（`/cloudmusicv3/effect-control?strategy=`）
| strategy | 最小化行为 | 恢复行为 |
|---|---|---|
| **fadeout**（默认） | `showing=false` → 前端淡出（按 offmode） | `showing=true` → 淡入（含 `fadein_delay_ms`） |
| **park** | 按钮最小化（焦点切走）→ 泊车屏外保活 | 冻结(过渡~700ms)+交叉淡入(`resume_ms`)接回，全程实时 |

- park 仅对**按钮最小化**生效（最小化那刻前台是「另一真实 app」才判定）；park 下**任务栏最小化**退回 fadeout
  （焦点在 shell/网易云，无法稳定 park，任务栏最小化的人少）。
- park 后窗口在屏外但**非最小化**（`IsMainMinimized=false`）→ 持续出帧 → 前端保持实时。

### 前端显示状态机（`effect_page.html`）
- `active = cmActive && showing` → 显示实时帧（按 opacity）。
- 切走（cmActive=false / 退详情页 showing=false）→ `showHidden()` 按 `offmode`（默认 fade 到 0，不漏主页）。
- 切回 → 延迟 `fadein_delay_ms` 后淡入（等网易云进详情页冷渲染）。
- **冻结→实时交叉淡入**：帧门控/park 过渡导致帧停发 → 前端定格最后一帧；帧恢复（间隔>250ms）→ 冻结帧叠层
  淡出（时长 `resume_ms`，缺省=`fadein_ms`）→ 平滑接回实时。刚 fadeout 过则跳过交叉淡入，改走延迟淡入（防双重淡入）。

## URL 参数（`effect_page.html` / `effect_display.html`，全程透传）

| 参数 | 默认 | 说明 |
|---|---|---|
| `host` / `port` | localhost / 8765 | 后端地址；或用 `ws=` 直接给完整 ws URL |
| `quality` | 95（后端默认） | JPEG 质量 1–100；细线条色块靠它压（纯层原生分辨率，建议 ≥90） |
| `opacity` | 1 | 整块画面不透明度 0–1（背景烤进 canvas，无法单独关背景） |
| `fit` | contain | `contain`(letterbox) / `cover`(裁切填满) / `fill`(拉伸)。纯层帧是 canvas 原生宽高比，与 OBS 源不同会出黑边，可用 cover/fill 或把网易云窗口调成目标比例 |
| `offmode` | fade | 非活跃输出时：`fade`(淡出全透明) / `hold`(定格) / `pure`(暂等同 fade) |
| `transition` | fade | `fade` / `slide` / `both` |
| `fadein_ms` / `fadeout_ms` | 600 / 600 | 淡入/淡出时长 |
| `fadein_delay_ms` | 1000 | 进详情页后延迟再淡入（等冷渲染） |
| `resume_ms` | =fadein_ms | 「冻结→实时」交叉淡入时长（park/帧门控恢复）；独立于进场淡入 |
| `header_clickable` | 1 | 双击隐藏顶栏后是否保留点击/拖动（透传后端注入） |
| `footer_clickable` | 0 | 双击隐藏底栏后是否保留点击 |

> 注：`scale` 参数已废弃移除（纯层直读 canvas 原生分辨率，缩放无意义）。

## 控制端点
- `GET /cloudmusicv3/effect-control?strategy=park|fadeout` — 切换最小化策略。
- `GET /cloudmusicv3/effect-control?park=1|0` — 一次性手动 park / unpark（任意策略下生效）。

## 约束与已知取舍
- **只读**：绝不改源 canvas（会崩溃）。分辨率受限于网易云窗口尺寸。
- **分辨率偏软**：网易云按 CSS 分辨率渲染特效（DPR-unaware），原生细节有限；放大窗口可改善，无法代码强制。
- **编码帧率**：JPEG 编码（canvas API）受并发度限制，画布池并发已达 ~68fps 余量，目标锁 30fps。
- **真最小化（fadeout 策略）**：页面 hidden → rAF 暂停 → 不出帧 → 前端冻结后淡出（设计如此）。要持续出帧用 park。

## 当前状态
- ✅ 纯层只读捕获、30fps、q95、适配任意窗口、park/fadeout 双策略、退详情防漏主页、双击隐藏 chrome、崩溃恢复。
- ⬜ 黑胶/标准模式下纯层表现与策略（待测）。
- ⬜ 整体代码硬化、合并进 playercap 主框架（Router 驱动 activePlayer、watchdog 接线、main.go）。
- ⬜ `lyric_page.html` 左侧菜单 effect 入口标签页（生成带参 URL）；README / API 文档。
