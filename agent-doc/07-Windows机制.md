## 7. Windows 机制

### 7.1 原语（全在 `player/wesing/proc/memory.go`）

| 能力 | 入口 | 注意 |
|---|---|---|
| 进程查找 | `FindProcess(name)` | `CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)` |
| 打开句柄 | `OpenProc(pid)` | 只申请 `PROCESS_VM_READ\|PROCESS_QUERY_INFORMATION`——**只读，别加 VM_WRITE** |
| 模块枚举 | `EnumModules(pid)` | `TH32CS_SNAPMODULE\|SNAPMODULE32`，32 位快照 |
| 内存读取 | `ReadBytes` / `ReadUint32` / `ReadInt16` / `ReadUint16` / `ReadFloat32` / `ReadString` | **没有 UTF-16 读取器**。`ReadString` 只认 null 结尾的 ASCII；宽字符在调用层自解（`lyric/songinfo.go` 的 `utf16LEBytesToString` / `utf16LEBytesToStringUntilNull`） |
| 可写区域枚举 | `EnumWritableRegions` | 只走到 `0x7FFF0000`（32 位用户空间上限） |
| AOB 扫描 | `AOBScan` / `ParseAOBPattern` | 见下方两个坑 |
| 窗口状态 | `GetPlayState(pid)` | 一次 `EnumWindows` 拿全 Phase/标题/最小化/拖动 |

**AOB 的两个坑：**

- **`AOBScan` 的文档注释是错的。** 它写「支持 0xFF 作为通配符」——不是。通配符是 `ParseAOBPattern` 里的 `??`（产出 `mask[i]=false`），`0xFF` 就是字面量 0xFF。别照这句注释写模式串。 ❌ 靠人
- **`AOBScan` 对 >64MB 的区域是整块 `continue` 跳过（`if size > 64*1024*1024`），不是分块读。** 目标落在大区域里 = 静默漏命中，无日志、无错误。扫不到时先怀疑这里。 ❌ 靠人
- `isWritable` 只认 `PAGE_READWRITE` 与 `PAGE_EXECUTE_READWRITE`，**刻意排除** `WRITECOPY` 两档（函数内有注释）。别「补全」它。

**窗口状态检测**：`GetPlayState` 靠窗口标题做三态判定（标题判别在包级回调 `getPlayStateCallback` 里，Phase 赋值在 `GetPlayState` 末尾）——「全民K歌 - xxx」与 `CLyricRenderWnd` 都在 = `PhasePlaying`，只有前者 = `PhaseLoading`，没有前者 = `PhaseStandby`。拖动检测走 `GetGUIThreadInfo` 的 `GUI_INMOVESIZE`（`GetPlayState` 内），用于抑制拖窗时的假冻结。

**「只读」的边界**：wesing 与 applemusic 只读扫描（applemusic 见 §7.8）、网易云/酷狗走 CDP。**qqmusic 不是只读**——`mem.go` 的 `ConnectQQMusic` 用 `PROCESS_ALL_ACCESS` 开句柄，`WriteProcessMemory` 在 `mem.go` 的 `WriteBytes`，`InjectSliderAOB` 本体在 `mem.go`。它只是**运行时不写**：调用点已因 issue #39 注释关闭（`qqmusic.go` 的 `runSession` 开头）。**绝不恢复它**——产出 `SliderVal` 零消费方，代价是 codecave + E9 跳转，属杀软重点盯的注入签名；补丁写进 QQ 音乐地址空间后**本程序退出不撤销**（全仓无 `VirtualFreeEx`），恢复后做 A/B 必须每轮重启 QQ 音乐本身。理由见 `runSession` 开头那段注释块。

**wesing 的内存内容不保证与当前歌曲同步。** 逆向时实测：**换歌之后，内存里的东西常常还是上一首的**，且严重程度因机器而异（本地数据库状态可能是变量之一）。这不是我们的 bug，是 wesing 自己的行为——**别试图「修」它，要假设它会发生**。

这一条解释了一整族症状，遇到时先想到它：

- **封面经常拿到上一首**（`[!] 封面 URL 获取失败` 或字节数与上一首完全相同）。`FindCoverURL` 靠 mid 做内存 AOB 匹配，匹配到的可能是陈旧的那份。封面 goroutine 的代次守卫（`EmitForGen`，§3.8）只拦换歌后迟到的回写，拦不住这种在当前代次内取到的陈旧封面。
- **播放时间地址选中一个「永远是 0」的实例**（issue #44）。AOB 命中的很可能是**上一次会话/上一首歌的残留结构体**——它字体 `1E`、行高 `2D` 一应俱全，`+0x10` 的指针也有效，**只是不再被更新了**。

**关键推论，动手前必读：陈旧实例与真实例在静态上无法区分。** 它们的每一个字节都合法。所以以下方向全都无效，别浪费时间：

- 往 `validateTimeAddr` 里加更多特征字节 —— 陈旧实例同样满足
- 排除「可疑」的值（比如 0）—— 0 是歌曲开头的合法值，排除它会在最常见的时机拒掉真实例（issue #44 的第一个错误方向）
- 加强 `ptr10` 的指针校验 —— 陈旧实例的指针也是有效的

**唯一能区分的信号是「它会不会动」。** 真实例至多在开头逗留一瞬；陈旧实例永远不变。`wesing.go` 的 `sawProgress` + `exitDeadAddr` 就是这个判据的实现（issue #44）。**任何新的内存读取都该问一句：如果它是陈旧的，我怎么发现？**

**发现了还得换得掉。** 重新选址若仍按顺序取第一个通过校验的，选中的还是那个陈旧实例：2026-10-09 真机，全民K歌开着音译（歌词双行排版）时在走的是「双行(28/42)」结构，进程里同时留着一个时间恒为 0 的「单行(30/45)」结构、排在前面，`exitDeadAddr` 重扫一遍又选回它，整首一行歌词都不推、状态一直报暂停。（关掉音译后，在走的那个结构原地变回 30/45，两个候选都算单行、在走的那个地址更小排在前面——旧代码在单行排版下能用是碰巧。）现行：`FindPlayTimeAddr` 把两种模式里通过校验的都算候选，挑读数正在走的那个（`pickAdvancing`，最多采样 0.32s）；选址那一刻歌若暂停着、哪个都不走，取读数不为 0 的第一个（真机上那个不走的旧结构恒为 0，暂停中的真结构停在暂停的位置上；都是 0 就取第一个——这只是没有更好凭据时的取舍，不是把 0 当非法值），落选的交给 `altWatch` 约每秒看一眼——当前地址停着而某个候选在走，就改用它（`exitAltAddr`，同一首歌重新宣告一次，一首至多换一次）。✅ `player/wesing/lyric/picktimeaddr_test.go`、`player/wesing/altwatch_test.go`（注入缺陷均红，实测）；采样与检查的节奏、阈值是按真机上「每秒涨约 1 秒」定的，离线守不住数值。**候选要跟着缓存地址一起沿用到之后的歌（`runSession` 的 `cachedAltAddrs`），不能只在扫描的那一首里盯。** 换歌不清缓存地址，下一首 `initSong` 走缓存路径；候选若不带过去，暂停时凭读数猜错的那一次就整场换不回来——`altWatch` 无物可盯，那个读数又大于 0、`sawProgress` 为真，#44 的重扫也不触发，此后每一首的位置与歌词行都停在那个读数上（issue #86）。换地址时被换下的那个进候选名单（`swapAlt`）✅ `player/wesing/altcarry_test.go`。带过去的候选是几首歌之前扫到的，改用之前拿 `lyric.IsPlayTimeStruct` 复核它此刻仍是一个播放时间结构（排版参数是已知的两组之一）——读数合理、会走的 float 到处都是，那块内存另作他用了分不出来 ✅ `player/wesing/lyric/timestruct_test.go`（只测字节比对）。**复核要在 `altWatch.check` 里逐候选做，复核不过的跳过、接着看下一个**；等它返回第一个在走的候选之后再复核，排前的那个一旦是被复用的内存，后面真在走的就永远轮不到 ✅ `player/wesing/altwatch_test.go`。`initSong` 缓存路径把候选带回、`pollLyrics` 把 `IsPlayTimeStruct` 接进 `check`、「一首至多换一次」这三处接线没有离线用例，❌ 靠真机。

### 7.2 NewCallback 必须包级 —— 唯一有自动门禁的一条

**`syscall.NewCallback` / `windows.NewCallback` / `NewCallbackCDecl` 只能出现在包级 `var` 的初始化表达式里。函数体内一律违规。** ✅ **`player/callback_lint_test.go` AST 门禁**

为什么：上限 **2000**（`runtime/zcallback_windows.go` 的 `cb_max`），**永不回收**，超限是 `throw("too many callback functions")`——不可 recover，整进程死。x/sys 与 syscall 共用同一个池。runtime 按 funcval 指针去重，**只有静态 funcval 才命中去重**——函数内新建的闭包每次调用都分配新 funcval，代码文本一样也不去重。

为什么它是门禁而不是一段文字：这条规矩曾只以文字形式存在，park 包依然违反了（§3.0）。

写法样板：包级 `var xxxCallback = syscall.NewCallback(...)` + 包级变量传参/收结果 + 包级 `sync.Mutex` 串行化。Mutex 不是装饰——回调只能靠包级变量回传（lParam 未用），包级变量就必须串行化。

- 样板：`proc/memory.go` 的 `enumWindowsCallback` + `enumWinMu`（供 `EnumProcessWindows`）、`getPlayStateCallback` + `getPlayStateMu`（供 `GetPlayState`）
- 已修：`park.go` 的 `findMainCallback`、`debugListCallback`——原本在 `findMainAny`/`DebugList` 里逐次新建。真实敞口见 `findMainCallback` 上方的注释：`effect.go` 的 `windowStateLoop` 每 80ms 调 `IsMainMinimized` + `IsMainForeground`，缓存未命中时每 tick 泄漏 2 个（25 个/秒），跨网易云重启累积，约 8~20 次重启后打死进程。
- 门禁覆盖三种曾经漏报的形状（`callback_lint_test.go` 中 `TestNoNewCallbackInsideFunc` 的文档注释）：包级 var 内的 FuncLit、别名 import、`NewCallbackCDecl`。`TestLintCatchesKnownHoles` 钉死它们，**别在重构 scanner 时把洞改回去**。
- `go vet` 不检查 NewCallback 计数，这条只能靠门禁挡（见 §8）。

### 7.3 park：屏外泊车（`player/cloudmusic/park/`）

把网易云主窗移到**虚拟桌面包围盒之外**（park.go 的 `offscreenX()`，下限 30000）但保持 shown 且未遮挡，以维持 Chromium 出帧；用途是最小化后仍能给 OBS 特效镜像供帧。

**三条必须知道的：**

1. **Win11 一律强制降级 fadeout，绝不放行 park。** `main()` 以 `park.IsWindows11()` 调用 `effectiveEffectStrategy`，无条件覆盖 config。理由在 `park.go` 的 `IsWindows11` 文档注释：Win11（尤其 24H2）的 DWM 会停止合成「完全不可见/被遮挡」的 Chromium 窗口——实测一泊车就冻结、不再出新帧，保活失效。判据 `IsWindows11()` = major>10 或 (major==10 && build≥22000)。降级规则 ✅ `bannerconfig_test.go`，`main()` 里的调用点 ❌ 靠人（§1.2）
2. **崩溃落盘兜底不能省。** `Park()` 在移窗**之前**先 `writeState`，落盘到 exe 同目录 `mbx-cloudmusic-park.json`（`stateFilePath`；取不到 exe 路径时退回 `os.TempDir()`）。`main()` 启动时调 `park.RestoreOrphaned()`——发现遗留泊车状态即刻还原。没有这步，PlayerCap 崩溃一次，主播的网易云就一直停在屏幕外，且无从知道窗口去了哪里。
3. **找主窗必须用 `WINDOWPLACEMENT.rcNormalPosition` 的面积，不能用当前 rect。** 见 `findMainAny` 的文档注释：最小化时当前 rect 是 (-32000)，只有还原态尺寸才能识别出主窗（~1058×752 最大），并避开迷你播放器/桌面歌词/边框小窗。

移窗要 `SetWindowPlacement` + `SetWindowPos` **两步**（`Park()` 末尾的 `setPlacement` 与 `procSetWindowPos.Call`）：前者一步做「取消最小化 + 不激活」，但会把位置**钳到可见工作区**；后者不钳制，真正移到屏外。少一步就泊不出去。

### 7.4 preserveDrawingBuffer shim 的 document-start 注入（`player/cloudmusic/effect/effect.go`）

WebGL canvas 默认 `preserveDrawingBuffer:false`，合成后绘制缓冲被清空，帧外 `drawImage` 只有 **~50% 命中**。修法是注入 `getContext` shim 强制 `preserveDrawingBuffer:true`（effect.go 的 `glShimJS` 常量，`__mbxGLHook` 守卫幂等）。

**为什么必须早于页面加载**：shim 只对**之后创建**的 context 生效。等页面加载完、歌已经开了，context 早建好了，只能靠 `Page.reload` 补救——而 reload 在网易云启动期会把它卡死/中断播放（`purelayerSession` 里「不再在此 reload」那段注释来自实测）。所以 `shimInjectLoop` 在 9222 一可达就 `Page.addScriptToEvaluateOnNewDocument` + 对当前文档 `Runtime.evaluate` 兜底，然后**阻塞读住连接**（循环 `conn.ReadMessage()` 直到出错）以维持 addScript 注册，连接断（网易云重启）后自动重注。

**为什么 `shimInjectLoop` 故意不受 `netEaseUp` 门控** ❌ 靠人：`Run()` 的抓帧主循环与它起的 `pollShowingLoop` 都门控在 `netEaseUp()`（取词 player 的 `IsConnected`），`shimInjectLoop` 却在 `Run()` 开头无条件起飞。这是刻意的，注释在 `shimInjectLoop` 的文档注释（「关键：**不门控在取词 player 的 IsConnected**」）——player 要等 watchdog 重启 + 5s 才连上，那时页面早加载完、甚至已恢复了上一首歌，context 已建、来不及注入。给它加上门控 = 静默退回 50% 命中率，症状是「时灵时不灵」，最难查。**别为了「一致性」把它并进门控。**

`glShimJS` 与 `captureInjectJS` 开头 `if(!window.__mbxGLHook){...}` 的同段是**两份拷贝，必须保持一致**（`glShimJS` 的文档注释已注明）。

### 7.5 wesing 的 vtable 定位链（`player/wesing/lyric/finder.go`）

`FindLyricHost` 五步，任何一步换 WeSing 版本都可能断：

1. 模块表里找 `KSongsLyric.dll`
2. **解析 PE 导出表**找 `CreateLyricHost`（`findExportFunction`）——手工走 `base+0x3C` → PE 头 → Optional Header `DataDirectory[0]`（`+0x78`）→ 名称表/序号表/函数表（`+0x18/+0x20/+0x24/+0x1C`）
3. **反汇编**：在 `CreateLyricHost` 前 128 字节里找第一条 `CALL rel32`（`0xE8`）→ 构造函数（`findFirstCall`）
4. 在构造函数前 200 字节里找 `mov [edi], imm32`（`C7 07`）→ **vtable 地址**（`findVtableAssignment`）
5. **AOB 扫描**：把 vtable 值当 4 字节小端模式在可写区域搜（`EnumWritableRegions` + `Uint32ToAOB` + `AOBScan`）→ 首个命中 = LyricHost 实例；歌词子结构 = `hostAddr + 0x0C`

**这条链上的每个魔数（`0x3C`/`0x78`/`0xE8`/`C7 07`/`0x0C`）都是逆向出来的，不是标准。** 改 wesing 取词前先读完这个函数——它失败时只返回 error（五步各一处 `return 0, 0, i18n.Errorf(...)`），wesing 会跳过本轮重试，不会有任何提示告诉你是哪一步断的，只有前四步成功后各打的那条 `log.Detail` 地址能定位。排查时把日志级别看全。

**子结构之后的内存布局（`reader.go` `LoadLyrics` 读的，同样全是逆向魔数）：**

```
子结构 +0x48 / +0x50   = vector<LyricEntry*> 的 begin / end
LyricEntry +0x00       = float 行起始秒
LyricEntry +0x08 / +0x0C = vector<CharElement*> 的 begin / end（字级单元：中文一字、英文一词）
CharElement +0x00      = RenderData*（+0x00 → null 结尾 UTF-16LE 文本）
CharElement +0x04      = float 字起始秒（绝对）   ← 逐字
CharElement +0x08      = float 字时长秒           ← 逐字
```

**LyricHost 里不止一条歌词轨**（`roma.go` 头注释，2026-10-09 真机）：`FindLyricHost` 返回的子结构是轨 0（主歌词），其后每隔 `0x70` 是下一条，虚表相同；每条轨 `+0x04` 是一个 CString，内容是这条轨的歌词文件路径（主歌词 `…\<mid>.qrc`，音译 `…\<mid>_Roma.qrc`），`+0x48`/`+0x4C` 是行向量的 begin / end。音译轨与主歌词逐行对应（行数相同、行起始时间逐位相等），纯英文行有的是单独一个空格、有的照抄原文；这条轨在不在与界面上的音译开关无关（关掉后照样在，同日实测）。`LoadRoma` 按路径认轨、按行号 + 行起始时间逐行配到 `RomaText`，任何一步对不上都只是这一行没有音译 ✅ `roma_test.go`（实测）。**认轨要核对它是不是这首歌的**：路径以 `_Roma.qrc` 结尾还不够，去掉 `_Roma` 后得与主歌词轨的路径相同。音译轨会是上一首留下的（§7.1 的又一例）——2026-10-09 真机，从带音译的《红日》切到不带音译的《爱丫爱丫》，主歌词轨换成了新歌，音译轨原封不动留着《红日》的路径与 113 行；只看后缀会认下它，行号相同、起始时间也相同的行（不少歌的第 0 行都从 0 秒起）就配上了上一首的音译，随 `all_lyrics` 留到整首结束。一行里有单元读不出（元素指针、文本指针、文本本身）就整行不配，不把其余的拼成缺字的音译——歌词每首只加载一次，配上了不会重读。宣告前的等待（`wesing.go` 的 `waitRoma`，最多 1 秒）只留给「这首歌的音译轨登记了、行向量却还是空的」：它卡在 `status_update` / `song_info_update` / `all_lyrics` 之前，同一次真机上旧实现见音译轨在、配上 0 行就等，白等满了 1 秒；没有音译轨、是别的歌的、有行而一行没配上（每行只有一个空格的纯英文歌）都立刻返回 ✅ `romawait_test.go`（实测）。「主歌词先解析出来、音译稍后」这种时序本身没有在真机上见过。音译轨自己的字级单元连同时间进 `RomaDetailed` → 对外的 `roma_text_detailed`（形状同 `text_detailed`，其余五家恒为 `{}`）：它与主歌词的字级单元**不一一对应**（「僕」一个单元，音译是 `bo`、`ku` 两个；主歌词带尾空格的字，音译里空格单独占一个单元、时长对半分），所以是独立的一条数组，不能挂到主歌词的每个字上；时间轴不合法的行只留行级的 `RomaText`。这个字段在 `player.LyricLine` / `player.LyricUpdate` 上，`BuildLyricLine` / `BuildLyricUpdate` 不填，由播放器构造后赋值；服务端缓存的手复制（`UpdatePlayerState`）与内容哈希（`dedup.go`）都要带上它 ✅ `server/romatext_cache_test.go`、`server/dedup_test.go`、`player/eventjson_test.go`（实测）。**两处别再踩**：① 向量是 `{begin, end, 容量末尾}`，`+0x50` 是容量末尾——`LoadLyrics` 一直把它当 end 读（真机 69 行读成 94 项），多出来的堆残留全靠后面的合法性判据挡掉（issue #46 就是这么来的），`LoadRoma` 用的是 `+0x4C`；② `LyricEntry` 只有 `0x20` 字节，`+0x30` 已经是相邻的下一个 `LyricEntry`，不是「第二条字符向量」。CString 是 ATL 式的：指针指向字符，字符数在指针前 12 字节。轨 1 在实测的三首歌上都是空的，是不是翻译轨没有验证过。

`+0x04`/`+0x08` 就是 wesing 逐字的来源（见 §6.2）。**2026-07-18 实测跨进程重启一致**：基址随 ASLR 变（无所谓，链的第 1、5 步靠模块枚举 + AOB 扫堆重新定位），但上面所有**结构体内偏移不变**。合法性判据：首字起始 ≈ 行起始、行内 `+0x04` 单调不减、`+0x08` ∈ [0,100)——任一破即整行退回行级（`reader.go` 的 `wordsOK`），不推半截错位的逐字。垃圾防护复用行级同一个 `IsPlausiblePlayTime`（接受式，拒 NaN/Inf，见 §3.7）。

### 7.6 网易云 canvas 严格只读

**绝不修改 `#lyric-effect-canvas-id` 的 width / height / style。改它会让网易云崩溃。** ❌ 靠人

注释钉在 effect.go 的四处：包注释（「严格只读，绝不改源 canvas」）、`captureOutMaxW` 的注释（「绝不改网易云 canvas」）、`captureInjectJS` 的文档注释（「严格只读，绝不修改网易云 canvas —— 改其尺寸会导致网易云崩溃」）、`captureInjectJS` 脚本体内快照处（「绝不改源 canvas」）。

capture 只能 `drawImage` 把源 canvas 拷进**我们自己的**画布池快照（`captureInjectJS` 中的 `slot.x.drawImage(c,0,0,ow,oh)`），降采样也只作用于快照（`captureOutMaxW = 1920`，且仅当源宽超过它才缩）。分辨率由主播的窗口尺寸决定——用户放大窗口即更清晰，我们不强制改渲染分辨率（见 `captureInjectJS` 的文档注释）。想提画质就调 quality/outw，**不要动源 canvas**。

> 归属 §3.5 的一条事实，暂存于此待并入：`semver.IsValid("v0.0.0")` 为**真**，所以开发版自更新的 kill switch 不是 semver 合法性，而是 `main.go` 的 `isReleaseVersion` 里的手写特例 `normalized == "v0.0.0"`。

### 7.7 sodamusic：绕原生反调试开 inspector + transport 提取（`player/sodamusic/`）

汽水音乐是 Electron，**有原生反调试**：启动 argv 里带 `--remote-debugging-port` 会被它在 ~2s 内自杀。所以 cloudmusic 那套「杀掉重启 + 加 argv」在这里**不通**；`NODE_OPTIONS=--inspect` 也被打包版 Electron 过滤（实测 app 存活但 inspector 不开、零监听端口）。

**唯一可行且非破坏性的路子 = 复刻 Node 的 `process._debugProcess(pid)`**（`watchdog/watchdog.go`）。Windows 机制：目标 Node/Electron 主进程启动时会建一个命名映射 `node-debug-handler-<pid>`（十进制 pid），内含一个指针 = 目标地址空间里 `StartIoThreadWrapper` 的函数地址。激活 = `OpenFileMappingW` 读出该地址 → `CreateRemoteThread` 到目标 → 目标**自己**拉起 inspector I/O 线程（默认 9229）。这条路不碰 argv，反调试扫不到；实测 inspector 开着后长会话稳定，反调试**不检测 9229**。全程只读映射 + 建一个远程线程跑目标自带的激活函数，**不改汽水任何内存/状态**（§0.1 红线）。映射不存在（禁用了 inspector）→ 返回错误让上层降级重试，绝不盲写。

- `OpenFileMappingW` / `CreateRemoteThread` 提为包级 `var`（syscall 过程句柄不在轮询路径反复建）。读映射里的 8 字节用 `RtlMoveMemory` 拷进 Go 缓冲 + `binary.LittleEndian` 读出——**避开 `unsafe.Pointer(uintptr)` 触发 vet unsafeptr**（addr 作 syscall 源参数是安全的）。
- node v16 caller 能对 Electron 36(node 22) 目标生效 → 映射名格式跨大版本一致。

**取数**（`cdp/client.go`）：连 9229（主进程 Node inspector，端点 `/json/list`）→ `Runtime.evaluate` **必须带 `includeCommandLineAPI:true`**（Node 经 Command Line API 暴露 `require`，缺了它主进程桥里 `require('electron')` 会 `require is not defined`）→ 在主进程里 `webContents.executeJavaScript(...)` 桥进 rendererMain 主窗口（主进程无 DOM，故一律 `awaitPromise:true`）。桥内探针用「patch `MessagePort.prototype.postMessage` 抓闭包里的 `channel.port1` → 发 `method.invoke` 请求 `sharedState.get('player')` → 截 `method.return`」拿全量播放态。播放态字段：`progressSeconds`（**1Hz 采样**，见下条）、`mediaDetail.playable`（名/歌手/id/时长/`cover_url`）、`mediaDetail.lyrics`（`type:'krc'` 明文 + `translations.cn` 独立 tlyric LRC，按时间戳合并进 SubText）。封面 URL 由 `cover_url={uri,urls[],template_prefix}` 拼 `urls[0]+uri+'~'+template_prefix+'-crop-center:800:800.jpg'`（公网可取、无需鉴权）。

#### 7.7.1 `progressSeconds` 是 1Hz 采样 —— 落锚必须边沿触发 ❌ 靠人

**这是照 kugou 骨架时唯一不能照抄的一处。**

真机实测（2026-08-08，连采 60 次变化）：`progressSeconds` 每 **970~1060ms** 才刷新一次，每次
`+1.0000±0.01`；而值本身是采样瞬间的**真实位置**（形如 `45.519`，毫秒精度，不是整秒量化）。
也就是说两次刷新之间它一直是旧值，陈旧程度 0~1s。

对照：酷狗的进度是 100ns 计时器直读（`kugou.go` 的 `runSession` 中 `progressRaw / 1e7`），**每轮都是新鲜值**，
所以它那句「每轮 `anchorProgressSec = progressSec; anchorTime = sampleTime`」（`sampleTime` 是本轮读数的时刻）落不落锚没差别。
汽水照抄这句，外推就被清零 —— `position` / `progress` / 歌词行匹配一起退化成 1s 阶梯。

实证指纹（不需要外部基准，看输出本身就够）：`lyric_update.position` 的小数位。
- 每轮落锚：连续 10 条事件的 position 全是 `142.925 / 144.924 / 147.920 / 150.918 …`
  ——**小数位恒 `.92`，全落在同一条 1Hz 采样网格上**，说明时钟只在采样落地时前进。
- 边沿落锚：`193.007 / 195.856 / 199.923 / 201.548 …` ——小数位随行阈值散开，时钟是连续的。

现行实现见 `player/sodamusic/sodamusic.go` 的 `lastRawSec` 与 `livePos`。**别退回每轮落锚。**

> ⚠️ 换个测法会看不见这个缺陷。拿 `position` 与真实位置比对，两个版本的误差都只有 ~100ms
> ——因为**换行只可能发生在时钟前进的那一刻**，而阶梯时钟前进的那一刻采样恰好是新鲜的。
> 要看见它，得看「行阈值落在台阶中间时要等多久才跳」，或者直接看上面那个小数位指纹。

#### 7.7.2 窗口最小化久了，进度源掉到 1/60Hz —— 用 `setBackgroundThrottling(false)` 消除 ❌ 靠人

Chromium 对隐藏页面有 intensive wake-up throttling。对照实验（2026-08-08，汽水主窗口全程最小化，
两轮除了「有没有调那一行」之外条件相同）：

**对照组（不调）**：闲置约 4.8 分钟后节流生效，之后恒为一分钟一次。

```
[  288.1s] progress= 58.021 dv= +1.001 dt=  1064.6ms   ← 前 4.8 分钟正常
[  316.2s] progress= 86.021 dv=+28.000 dt= 28027.8ms   ← 节流生效
[  376.2s] progress=146.022 dv=+60.000 dt= 60031.3ms
[  436.1s] progress=206.019 dv=+59.997 dt= 59934.0ms
```

**处理组**：在**已被节流**的状态下调用，300ms 内恢复 1Hz，窗口仍是最小化。

```
setBackgroundThrottling(false) -> [[1,false]]
[    0.3s] progress=263.941 dv= +0.200 dt=  303.6ms
[    1.3s] progress=264.940 dv= +0.998 dt=  915.6ms
```

随后不再动它、继续最小化观察 8 分钟（远超对照组 4.8 分钟的触发点）：**478 个采样，最大间隔
1121.5ms，超过 2000ms 的 0 次**——节流没有重新装填。

**「Electron 里 setBackgroundThrottling 加载后再设需 reload 才生效」这条传闻对本版本不成立**
——上面那两行就是反例。别因为看到那个说法就把这条实现改成「加载前设置」或加 reload：
reload 汽水的渲染器会打断播放。

症状识别：日志里的 `forward seek detected`（中文环境显示「检测到前跳」）全部落在整分钟的同一秒、间隔恰好 60s、跳幅恰好等于间隔。
**那不是 seek，是节流。**

现行实现：`cdp/client.go` 的 `bridgeExpr` 在每次取数（`Extract`）时对所读的那个主窗口
webContents 重申一次 `setBackgroundThrottling(false)`，并把重申后的 `backgroundThrottling`
经 `ExtractionData.Throttled` 带回。**不能只在连上时设一次**——开关随 webContents 生命周期存在，
而连的是主进程 inspector：汽水重建主窗口时 CDP 会话不断，新 webContents 回到默认的节流态。
尽力而为：设不上只由 `sodamusic.go` 的 `runSession` `log.Warn` 一次，不阻断取数。

**为什么不照网易云加启动参数**：网易云那条路是 watchdog **杀掉进程 + 带 argv 重启**
（`player/cloudmusic/watchdog/process.go`），对汽水等于**在直播中杀掉主播的播放器**；
而且汽水有原生反调试，argv 里加东西是否触发自杀未经验证（§7.7 当初正因它自杀才放弃重启加参数）。
运行时 API 只是对它自己的 webContents 调一个官方 Electron 接口：不写内存、不碰 argv、
不改注册表，进程退出即失效。

> **这是 §0.1「不改汽水任何内存/状态」的一处刻意例外**，边界写在这里：只允许
> `setBackgroundThrottling(false)` 这一个调用。要再加别的「顺手也设一下」的 API 之前，
> 先回来读这一段——例外之所以安全，靠的是它窄。

即使这条失效（旧版 Electron、API 被移除），7.7.1 的边沿落锚仍是兜底，两者不是二选一：
- 每轮落锚 + 节流 → 进度**整整冻结一分钟**再跳 60s。实测日志 `检测到前跳: 0.00s → 28.01s`，
  歌词在第 0 行卡了 28 秒。直播里这是事故。
- 边沿落锚 + 节流 → 歌词照常平滑推进（60s 内音频时钟与墙钟只差 0.4ms），只在会话刚建立时
  带一个「首个采样有多旧」的固定偏移，下一个采样到达即被 seek 判据纠正。

#### 7.7.3 多个消费者会互相饿死 ❌ 靠人

同时开两个 PlayerCap（或 PlayerCap + `cdpexplore` + 临时探针）连同一个汽水时，实测出现过
某一路的 `Extract` 连续数十秒拿不到可用数据、而另一路正常。桥内探针会 patch
`MessagePort.prototype.postMessage` 再还原，两路并发时还原顺序会把对方的 wrapper 永久留在原型链上；
transport 侧能否并发承载多个 `method.invoke` 也未验证。

**结论：调试时别一边开着正式服务一边开探针**，测出来的数会是假的（`5070b5d` 的排查中就踩过一次：三路并发
下量到的「误差」全是并发伪影）。生产只跑一个实例，故未按缺陷处理。

---

### 7.8 applemusic：纯进程内存扫描（`player/applemusic/`）

Apple Music for Windows（`AppleMusic.exe`，微软商店 MSIX）是 WinUI3 原生外壳 + 内嵌 JavaScriptCore，**不是 Node**——soda 那套 `_debugProcess` 开 9229 的路子不通。用户要求**纯内存、零 WinRT**（与 wesing/qqmusic 一路），SMTC（`GlobalSystemMediaTransportControlsSessionManager`）这条本可直接给元数据的路被显式否掉。`mem/` 纯只读：`OpenProcess(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ)` + `ReadProcessMemory`/`VirtualQueryEx`，绝不写目标一个字节（§0.1）。**无需管理员**：同用户、同完整性级别，普通权限即可读（2026-10-08 未提权会话实测）；拒绝访问只报 `no_admin` 并重试。

分工：三样数据的形态判据是 `mem/` 里的纯函数（`propstore.go`、`ttmlscan.go`、`twin.go`，可离线喂字节测）；「哪份属于当前歌、什么时候扫、现在播到哪」在 `applemusic.go` 的 `session` 与 `clock`。**三样都不带歌曲标识，每一样都要回答「它属于当前歌吗」**——PR #60 两轮真机复测出的问题（乱码标题、错配歌词、锁在静止位置；没封面、接管不了、歌词晚到当纯音乐、用旧标题宣告新歌、歌词晚 0.8s）大半是这一问没答好，其余是把整秒读数当成了位置。下文 ✅ 后标「实测」的，是在隔离副本里注入对应缺陷、该用例确实变红（注入的写法见各测试的「变异自证」；2026-10-08 用脚本逐条跑过，其后新增的用例由引入它的提交逐条注入，结果记在提交说明里）。

**1. 两个便宜的信号源：位置结构与「正在播放」存储。** 整个会话靠这两样撑着，全扫只在它们说「变了」或还缺东西时才做。

- **位置结构**（`twin.go` 头注释）：`[时长][位置][位置]` 三个 float64，两份位置逐位相等，同一进程内地址不变、跨歌复用，确认后每轮只读这 24 字节。时长是整数秒；手动切歌约 0.15~0.2s 后归零，再过 0.25s~4.4s 写入新时长（已缓存的歌恰好 0.25 或 0.5s，联网加载的更久，审查方见过 12~43s）；自然播完切到下一首时归零只有约 55ms；暂停中切歌时长一直是 0，按了播放才写入。**换歌 = 时长字段变化**（`onTwin` → `beginSong`）。
- **存储**（`propstore.go` 头注释）：带媒体类别头的完整链（`Full`）分带封面与不带封面两种。带封面的总是当前歌（2026-10-09 真机逐秒采样 10 分钟、7 次换歌，全部采样如此）：每次换歌约 0.1~0.25s 后在**新地址**重建，或把原地址就地改写成新歌名（封面要换的，这一份还带着上一首的图、约 1s 内释放，同时另建一份带新封面的），早于时长变正；一首歌期间地址与内容不变。不带封面的会残留（同日见过一份留了 2 分 40 秒、跨三次换歌），所以「完整」不等于「当前歌」（见 4）；2026-10-08 所见「任一时刻完整链只有一份」不成立。`session` 记下选中那份的地址与开头 512 字节（`NowPlaying.Addr` / `Sig`），每 `watchInterval`（300ms）复读比对（`watchStore`），对不上即 `storeDirty`、尽快全扫。时长相同的两首自然衔接时位置结构看不出换歌，靠的是它；封面晚到也是它报的。✅ `TestSessionSameDurationChangeDetectedByStore`（删掉盯梢与跳回开头时的核对即红，实测）。存储一直在变时因它触发的全扫间隔翻倍、封顶 5s ✅ `TestScanBudgetFlappingStore`（实测）。

加载中（时长为 0）不宣告：这时内存里是上一首或「正在连接…」占位（`maybeScan` 的 `songReady` 含 `!s.loading`）✅ `TestSessionNoEventsWhileLoading`（两处 `songReady` 的 `!s.loading` 都去掉即红，实测）。加载中存储换了也会全扫，但只用来记歌词归属（见 5）。加载超过 `loadingGrace`（6s）才清屏：取 3s 时当晚 10 次切歌有 4 次会先清屏再宣告，根路径上被别的播放器插进来一下 ✅ `TestSessionClearThenSameSongReannounced`、`TestSessionQuickSwitchNoClear`（改回 3s / 改成 1s 即红，实测）。新歌起手一律按在放（时长变正就是开播，暂停中切歌时时长不会变正）：旧实现沿用上一首的播放态，暂停后切歌先报 paused、等位置走两格才改口，而路由只认 playing ✅ `TestSessionNewSongAfterPauseStartsPlaying`（实测）。

**2. 时钟：整秒读数不是位置**（`clock` 上方的实测注释：一边抓 `AMPLibraryAgent.exe` 的出声，一边每 5ms 读一次位置结构）。读数跳到 N 的时刻，真实位置已是 N+0.38~N+0.64：Apple Music 约每 256ms 才刷新一次位置、且本身带约 0.38s 的固定滞后，相邻两跳多为 1.02~1.03s，攒够一个刷新周期来一次约 0.77s 的追平。时长变正那一刻也不是精确的开播时刻（真实位置在 -0.05~+0.18s）。旧实现把读数 N 当成位置 N、每跳落锚，300ms 轮询又在「看见跳变」与「推歌词行」两处各晚 0~0.3s——审查方要把 offset 从 400 调到 1200 才对得上。现行：

- 位置 = 锚点 + 墙钟流逝；读数每跳一次，只把估计**夹**回 `[N+flipLagMin, N+flipLagMax(+距上次读数)]`，在范围内不动；读数不跳时不越过 `lastRaw+1+flipLagMax`。起点（开播、中途接上、恢复、跳转）只是估计，靠之后的夹在一轮追平（约 10 秒）内收敛。✅ `TestClockRunsFromStart`（改回每跳落锚即红）、`TestClockClampedIntoFlipWindow`、`TestClockDoesNotRunPastNextFlip`、`TestClockClampsToDuration`，均实测。
- 轮询下限 `minPollInterval` 50ms（`EffectivePollInterval`，登记在 `main.go` 的 `playerPollIntervals`）✅ `TestEffectivePollInterval`、`TestConfigEchoPlayerPollFloors`（实测）。**别以为位置源 1Hz 就可以调回 300ms**：跳变要在这一轮被看见、歌词行也在这一轮推出。
- 全扫的零点几秒里照常读位置、推歌词行（`mem.Reader.Scan` 的 `during` 回调 → `scanTick`），扫完按那一刻的状态用这份快照：换歌本身会触发全扫，真机见过开播晚 0.29s 才被看见。✅ `TestSessionSongStartSeenDuringScan`、`TestSessionSkipDuringScanNoLyricsForOldSong`（传 nil、或沿用扫描前的 `songReady` 即红，实测）。
- 停更 `pauseThreshold`（1.6s）判暂停，冻结在 `lastRaw+1`；开播或播放中跳转确认后 `startWindow`（8s）内门槛放宽到 `startPauseThreshold`（3s），起点记在 `clock.relaxAt`。跳转后的下一跳与开播后的第一跳一样来得晚：真机播放中拖动一次进度条，音频与波形都没断，却在约 2s 后报暂停、再过约 1s 报恢复，其间接着的拖动都被当成暂停中拖动、不发 `playback_resume`。代价是这 8s 里的真暂停最多晚 1.4s 发现。暂停中位置前进一跳（≤ 距上次读数的墙钟秒数 + `tickSlackSec`）只重锚，`resumeConfirm`（2.5s）加一个读数间隔内再来一跳才判恢复——只凭一跳会把暂停中往前拖一两秒报成 playing、让路由把根输出切过来；代价是恢复晚报 1~2s。其余变化是暂停中拖动，确认后（见下条）只重锚、不发事件（§3.2）。✅ `TestClockPauseResume`、`TestClockSmallSeekWhilePausedIsNotResume`、`TestClockResumeWithSlowPoll`、`TestClockStartBufferingNotPause`、`TestClockSeekRelaxesPauseThreshold`、`TestSessionSeekThenSlowFirstFlipNoPause`、`TestSessionRepeatedSeeksStayPlaying`，均实测（后三个：去掉跳转确认处的 `c.relaxAt = now` 即红；放宽期改成永不过期，「8s 之后回到常规门槛」一例红）。
- 跳转要目标读数**持续不短于 `jumpHold`（300ms）才认**，播放中的跳转（偏出范围超过 `seekJumpSec`，3s）与暂停中的拖动（不是上一条那种「一跳」的变化）共用 `clock.holdJump`：期间每次读数都须仍指向该目标（允许随播放推进，落点比目标少一、约 250ms 后进一格的仍是同一个），任一次不指向它即作废，确认前读数与位置都不动。拖动进度条时读数先闪一下目标值又弹回原来的时间线、之后才真正落下去。2026-10-09 真机每 2ms 读一次的采样（播放中拖动 13 次）：闪现持续到 Apple Music 下一次刷新位置为止，11 次是 10~14ms、2 次是 219 与 225ms；弹回是 9~101ms 或约 515ms。`jumpHold` 因此取得比一个刷新周期（约 256ms）长；看持续多久而不看读到几次，是因为约 220ms 的闪现按 50ms 轮询能连着读到五次，全扫途中 `scanTick` 约每 20ms 读一次位置时更多。不经确认、或门槛短于闪现的后果（均真机）：播放中一次拖动连发三条 `playback_resume`（285.5 → 24.5 → 285.5；`jumpHold` 还是 40ms 时，闪现约 220ms 的拖动仍如此：52.35 → 160.35 → 52.35，`lyric_update` 的 index 6 → 23 → 6）；暂停在 212s 拖到约 63s，依次推出目标行、原位置的行、目标行（index 6 → 29 → 6）。代价：跳转在读数稳定约 0.3s 后才报（`jumpHold` 为 40ms 时约 0.05s），`playback_resume` 与目标位置的歌词行都晚这么多。播放中确认后发一次 `playback_resume`，位置从第一次读到目标值的时刻按「目标 + `seekLag`（0.3s）」推算，持续期间读数进了一格的不低于「读数 + `flipLagMin`」；暂停中确认后只重锚并复位行号（推出目标位置的那一行），不发 `playback_resume`。已知现象（不处理，读数序列见 `clock.observe` 的注释）：暂停中拖动一次可能先后推出目标与实际落点两处的歌词行——同一采样里的三次，目标值的读数都先停约 1.1s（长于 `jumpHold`，分不出它是过渡），之后才落到靠前 3~10s 处；暂停不足 `pauseThreshold` 就拖动时仍按播放中处理，这两处各发一条 `playback_resume`。拖完立刻播放、读数在这 0.3s 内又进了一格的，这一格并入待确认的目标、不算恢复的第一跳，恢复晚一跳才判出（采样里没有这样的时序）。✅ 播放中 `TestClockSeekWhilePlaying`、`TestClockSeekFlickerIgnored`、`TestClockSeekFlickerDenseSampling`、`TestClockSeekConfirmedAfterHold`、`TestClockSeekHoldBoundary`、`TestClockSeekSampleOneSeekPerDrag`、`TestClockPendingJumpCleared`、`TestSessionSeekFlickerSingleResume`、`TestSessionSeekFlickerDuringScanSingleResume`、`TestSessionSeekSampleSingleResume`、`TestSessionPendingSeekThenPauseOrSongChange`；暂停中 `TestClockSeekWhilePaused`、`TestClockPausedSeekFlickerIgnored`、`TestClockPausedSeekConfirmed`、`TestClockPausedSeekPendingThenResume`、`TestClockPausedSeekSampleReplay`、`TestSessionSeekWhilePausedSingleResume`、`TestSessionPausedSeekFlickerSingleLine`、`TestSessionPausedSeekPushesLineOnce`、`TestSessionPausedSeekSample`、`TestSessionPausedSeekThenPlayAtOnce`（删掉 `observe` 里待确认那一段、把 `holdJump` 末尾的持续判定改成恒真、把 `jumpHold` 改回 40ms、或把暂停分支改回变化即重锚，各有用例变红，实测；名字带 Sample 的四例与 `TestSessionPausedSeekThenPlayAtOnce` 按 `seeksample_test.go` 里的采样时间线回放）。
- 推送行号只往前走，跳转（含暂停中拖动）、换歌、同一首歌找回位置结构（见 3）后才重置 ✅ `TestNextLine`（实测）。

新旧两版对同一段播放并排跑过一次（2026-10-08，旧 8 行、新 13 行）：歌词行相对真实位置，旧版平均晚 0.80s（0.57~0.94），新版 +0.05s。默认 offset 400 不用动。

**3. 发现位置结构靠活性，不靠形态；确认之前不宣告任何歌。** 同形态的静止结构很多（本机 400~530 个，含随换歌改写时长、位置恒 0 的 `[时长][0][0]`）。全扫收集候选（`isTwinCandidate`：时长为 [15,7200] 的整数秒；超过 4096 个截断并记日志），每 300ms 复读（`watchCandidates` → `cand.observe`）：位置是整数、每次只走一格、相邻两格相隔 `liveMinGap`~`liveMaxGap`（0.5~2.5s）、连着 `liveSteps`（2）次才算在走；并且**先见到存储才认**。只看「在不在走」不够：Apple Music 刚启动的头一两秒有个 `[100][进度][进度]` 的进度条形态相同、也在走，旧实现把它认成时长 100s 的位置结构，真开始放歌后要等它停住 20 秒才换过去。多个同时成立取地址最小（§3.8）。✅ `TestDiscoveryStartupProgressBarNotConfirmed`（去掉最小间隔、或去掉「一下跳了好几格就清零」，各有一例变红）、`TestDiscoveryNoStoreNoConfirm`、`TestSessionPausedStartNotAnnounced`（形态对就认即红），均实测。

候选时长变了（含归零）不删、重新计；一次候选扫描没扫到已在观察的（它正在加载）也不丢；存储换了且还没认出位置结构时把候选扫描重排到开头几档。前两条与第三条各自都能让「暂停中启动后直接点另一首」「加载中赶上候选扫描」在 3s 内认出，两条一起断才红 ✅ `TestDiscoveryPausedStartThenDifferentSong`、`TestDiscoveryCandidateKeptWhileLoading`（实测，单断一条仍绿，注释里写明）。

暂停中附加（或附加时没在放）一直报 `waiting_song` + `no_song`。未确认位置结构时的周期全扫有两种节奏，间隔表按当时的状态选定（`twinScanTable`），状态变化处（附加、位置结构作废、存储出现或消失）经 `restartTwinScan` 重排，存储抖动时只经 `retableTwinScan` 换表（见本段末）。**见到了存储**：每次全扫都收候选，按 `twinScanSteps`（立即，然后 2/3/5/5s，封顶 15s）。**没见到存储**（Apple Music 启动后还没放过歌：真结构尚未分配、内存里也没有存储可盯）：按 `idleScanSteps`（立即，然后 2/3/5s，封顶 5s）只找存储，不收候选、也不碰候选观察表——`watchCandidates` 这时本就不认任何候选；本会话第一次全扫例外，照收候选（附加时歌可能正在放，存储与候选要同一次拿到）。没收候选的全扫认出了存储，立即（受 `minScanGap` 约束）补一次收候选的，之后按 `twinScanSteps`；存储消失而位置结构未确认，回到 `idleScanSteps`，不等原先排下的那一档走完。这两种重排只在存储的「出现 / 消失」距上一次这种翻转不短于 `storeFlapQuiet`（10s，按认出翻转的那两次全扫算）时做；不足 10s 的再次翻转算抖动，只换间隔表：档位不清零、不立即补扫，已排定的下一次全扫照旧、但不晚于新表的封顶（`retableTwinScan`）。存储还在、只是换了歌名不算翻转，照旧重排到开头几档。不设这一条时，相邻两次全扫交替认出与认不出存储（不收候选的认出 → 立即补扫 → 收候选的没认出 → 2s 后再扫）会让周期全扫停在约每 1.15s 一次、到不了任何一张表的封顶（PR #60 修复轮复审）：离线回放 10 分钟 521 次，现为 123 次，与空闲时的 121 次相当；只换表而不收到封顶的话仍是 121 次，但约每 30s 又清零补扫一轮（消失后按 `twinScanSteps` 排下的 15s 一档超出安静期）。代价（离线回放）：存储消失不足 10s 又回来时不立即补扫，回来的同时开播由 3.9s 认出变成 5.6~7.7s；抖动中存储稳定下来并开播，最坏 11.8s（同时开播）、16.8s（稳定 1s 后才写入时长，落到 15s 一档），不设抖动保护时是 4.0s 与 3.8s；抖动停止、存储消失 20s 后放第一首，与下文的冷启动相同（7.0s / 6.0s）。✅ `TestScanBudgetStoreFlapping`（去掉抖动保护、凡翻转都当抖动、换表时不收到封顶、不换表、抖动时回到第一档，各自变红）、`TestDiscoveryStoreFlipOutsideQuietPeriod`、`TestDiscoveryStoreBackWithinQuietPeriod`、`TestDiscoverySongAfterStoreFlapping`、`TestDiscoveryTitleChangeIsNotStoreFlap`（换歌名也算翻转即红），均实测。

冷启动的数字来自离线回放（假内存的全扫不耗时、位置每秒整一格），真机端到端 2026-10-09 量到一次约 4s：空闲附加后放第一首，把开播落在周期全扫的各个相位扫一遍，从开播到宣告最坏 7.0s（存储与时长同时出现）、6.0s（存储早 1s 出现）；改动前（一律按 `twinScanSteps`、每次都收候选）同一回放是 17.0s 与 16.0s，当时真机冷启动两次量到约 19s 与 5s。附加时歌已在放，认出时间前后都是 2.8s。空闲 10 分钟全扫 121 次、除第一次外都不收候选（改动前 43 次、每次都收），开销见 6。存储在而时长迟迟不写入（暂停中点了歌、几十秒后才按播放）的仍走封顶 15s 那档，按下播放后 ≤19s 认出。✅ `TestDiscoveryFirstSongAfterIdleAttach`（断言 7s / 6s；`idleScanSteps` 封顶改回 15s、或认出存储后不立即补扫即红）、`TestDiscoveryIdleScansSkipCandidates`、`TestDiscoveryAttachWhilePlayingCollectsAtOnce`、`TestDiscoveryStoreSeenLongBeforePlay`、`TestDiscoverySupplementScanBeforeDuration`、`TestDiscoveryIdleScanKeepsCandidates`、`TestDiscoveryTwinAndStoreLost`、`TestScanBudgetNoSong`、`TestScanBudgetPausedAttach`，均实测。

已确认的位置结构连续读不出超过 `twinFailFor`（0.9s）即作废，按暂停报，重新发现后同一首按恢复报、不重新宣告 ✅ `TestDiscoveryTwinLostThenRediscovered`（实测）。停住超过 `staleAfter`（20s）、或确认了却 `unannouncedAfter`（5s）还宣告不出歌，按退避找另一个在走的 ✅ `TestDiscoveryStaleTwinReplaced`（删掉 `tick` 里的 `checkStale` 即红，实测）。同一首歌找回位置结构（读不出后重新发现，或停住后换成另一处在走的）时复位歌词行号（`watchCandidates` 的同曲目分支）：找回的位置可能在已推的行之前，不复位则 `nextLine` 一直拒发，歌词停到播放位置越过原先那一行；找回时位置未变的，当前行至多重推一次 ✅ `TestDiscoveryRefoundTwinResetsLyricLine`（删掉复位，前两例红；把复位挪到 `watchCandidates` 开头，三例都因重复推行而红，实测）。**没有已确认的位置结构时绝不报 playing**：路由对 playing 没有超时，误报会一直占着根输出。

**4. 歌名 / 歌手 / 专辑 / 封面：整条解析属性存储。** Apple Music 交给 SMTC 的是 Windows 序列化属性存储（MS-PROPSTORE），链形态固定（`propstore.go` 头注释）：`64440492` 媒体类别（可缺）> `F29F85E0` 标题（#2）+ 封面（#27，VT_STREAM）> `56A3372E`「歌手 — 专辑」> `6FE41453` 应用名「Apple Music」> …，以 StorageSize=0 收尾。只认同一条链里同时有 System.Music 段与应用名段、且完整收尾的；`System.Music.Artist` 按第一个 ` — `（U+2014）拆。**为什么不用锚点 + 众数**：`"1SPS"`+Title 锚点在同进程还命中 Windows 设置搜索的属性存储（审查方真机 18 个候选里 17 个是「聚焦;Windows 聚焦;图像;…」这类关键词串）。✅ `nowplaying_test.go`（只去掉应用名段判据、只去掉 Music 段判据、或去掉收尾判据，均红，实测）。

封面流是 `[流名字节数][流名 UTF-16][补齐到 4 字节][长度][图片字节]`；只收首尾标记对得上的 JPEG / PNG、不超过 5MB，拷出扫描缓冲再用（缓冲在下一块内存时被覆盖）。随 `song_info_update` 的 `cover_base64`（data URI）发出，`cover` 恒为空串。✅ `mem/cover_test.go`（去掉补齐、直接引用缓冲、去掉结尾校验或大小上限、去掉长度检查，均红，实测）。`server/dedup.go` 的 song_info 哈希因此要含 `cover_base64` 的**内容**：没有 URL，只看「有无」会把补发的封面判成重复 ✅ `TestSongInfoHashIncludesCoverContent`（实测）。

多份存储时 `pickNowPlaying` 按（标题, 歌手, 专辑）归并，依次比：不是上一首期间就在的完整副本数（`prevChains` 记着上一首期间见过的地址与内容；就地改写的内容变了，同样算新的）、带封面的完整副本数、完整副本数、歌手非空（加载占位的歌手是空的）、不是上一首期间就在的副本数、票数、是否当前曲目、字典序。完整副本排在票数前是真机逼出来的：残留着早先一首的 2 份非完整副本、当前歌只有 1 份完整副本时，按票数挑三次换歌三次宣告错。带封面那一档也是（2026-10-09）：《过火》播放中内存里另有一份《crossing field》不带封面的完整副本，服务这时重启——确认位置结构之前的全扫两份都算新的，确认之后两份又都是上一首期间就在的，都是各一份完整副本、各一票——落到字典序，宣告成 crossing field；连续运行时同一首从头再放、加载超时清屏后重新宣告同理。它不排第一位：换歌的头一瞬间可能只有新歌不带封面的副本是新的，上一首带封面的还没被改写，这时该取新的。✅ `TestPickNowPlaying`（每一档各有一例，删掉即红，实测）、`TestPickNowPlayingCoveredFull`、`TestSessionCoveredStoreAnnouncedOverCoverlessResidue`（删掉带封面那一档、挪到第一位、写成替换完整副本数一档、计数不要求完整、判新只看地址，各有用例变红，实测）。换歌 `confirmAt`（1.5s）后再全扫一次核对标题；`titleConfirmWindow`（3s）内读到另一个标题按「首扫读到的是上一首」纠正并把这期间记到旧标题名下的歌词文档转给新标题，3s 之外则是时长相同的下一首、按新歌重来 ✅ `TestSessionCorrectedTitleKeepsLyrics`、`TestSessionSameDurationChangeDetectedByStore`（实测）。

封面的两处时序（均真机见过）：宣告时存储里还没有图 → 先发不带图的，图到了补发一条 ✅ `TestSessionLateCoverReemitted`；换歌后新存储先带着新标题建出来、图却还是上一首的，约 0.4s 后才换 → 与上一首发出的那张逐字节相同而专辑不同时先不发（`staleCover`），图换了补发，过了 `coverHold`（2.5s）仍相同就照发 ✅ `TestSessionStaleCoverNotSent`、`TestSessionSameAlbumCoverSentAtOnce`、`TestSessionSameImageDifferentAlbumSentAfterHold`（实测）。加载超时清屏（`clearSong`）不清掉「最近一次实际发出的封面及其专辑」：清屏后的新歌同样可能先带着上一首的图建出存储，清掉则 `staleCover` 无从比对、新歌带着上一首的封面宣告；清掉的那首没发过封面时沿用更早的记录 ✅ `TestSessionCoverIdentityKeptAcrossClear`（五例；改回清零三例红，只记图不记专辑、没发过也照记各有一例红，实测）。一直没有封面的歌按 `coverTries`（3/7/12/20s）找几次就放弃 ✅ `TestSessionNoCoverGivesUp`（实测）。

**5. 歌词：归属跟着「文档最近一次出现新副本时存储里是哪首歌」走。** 锚点是 UTF-16 的 `lyric-ttml-internal`（Apple 歌词专属命名空间），前后夹出 `<tt>…</tt>`，只收含 `<p ` 的完整文档（`findTTML`）；按全文去重，每份副本记下进程内的绝对地址（`addDocs` → `mem.TTMLDoc.Addrs`）。TTML 里没有歌曲标识，`<body dur>` 又只能当上限，归属只能靠「什么时候出现的」。真机（2026-10-08，歌词面板关闭，每秒采样各文档的副本地址）：每次一首歌成为当前曲目，它的文档都会出现新地址的副本（五次观察都是；2026-10-09 见到一次没有出现的，见下文「已知的缝」④）——手动点播时与新存储同一采样出现（细量是比新存储晚 0.2~0.7s，加载中因此隔 `tagScanAfter` 补扫一次）、早于时长写入约 1s，自然播完切歌时在开播之后才出现（见过 0.3~0.5s，也见过约 2s）；会预取（播放列表起播约 7s 时，当前歌与列表里另一首的文档在同一采样里一起出现）；不再是当前曲目后，副本在几十秒到几分钟内陆续回收，可能整份消失、重放时重新出现；副本数分不出新旧（审查方：新旧各 4 份）。所以 `noteDocs` 在一份文档出现新副本时——这次全扫里它有上一次全扫没有的地址，含第一次出现、整份消失后重新出现——把它记到那一刻存储里的（标题, 歌手）名下，**覆盖原归属**。只在第一次见到时记的话，附加前就在内存里的文档除第一首外永不采用，预取的文档记在上一首名下、轮到它时被拒，两种都是等满 `lyricGrace` 报无歌词（同日真机都复现了）。两种情形不改记：本会话第一次全扫只建基线，内存里已有的一律记 `preAttachOwner`；这次没认出任何存储时，第一次出现的记 `""`，已有归属的不动。来历不明的文档连续两次全扫都不在内存里就忘掉，之后再出现按第一次出现算。记在具体歌名下的文档，全文留在 `session.docKept`，离开内存后仍可用（来历不明的两种不留）：挑歌词时留存的与当次全扫到的一起交给 `pickTTML`（`lyricCandidates`），归属照旧以 `docOwner` 为准、随改记与标题纠正转移；`clearSong`、换歌都不清，新会话从空开始；上限 `keptDocsMax`（48 份，一份几千到几万字节），超出丢掉最久没在全扫里见到的。这是真机逼出来的（2026-10-09，逐秒采样）：存储换成《信仰》的同一秒它的文档出现 2 份，1 秒后整份消失，这一次播放期间再没出现过，而这首歌起播慢（时长写入约 1s 后又归零，其后约 9s 才开始走，走了清屏），就绪时文档早已不在内存里——归属在加载中的那次全扫就记下了，当时却只从当次快照里挑，等满 `lyricGrace` 报了无歌词。文档的寿命差别很大：同一首再放时留了 42 秒，另一首的一直留着。留存的副本只配给采用它时同一时长的曲目：`applyLyrics` 把那时的曲目时长记进 `session.docDur`，`lyricCandidates` 把记过时长而与当前曲目时长不同的留存副本挡在候选之外（位置结构的时长是整数秒，同一首歌每次放都一样——2026-10-09 的记录里三首歌各放两三次，每次相同——按相等比）；文档改记到另一首歌名下时这条记录作废，同一歌名下出现新副本、标题纠正、留存被淘汰时保留（淘汰时作废的话，这份文档日后只在加载中被重新见到时没有时长可核对，会被同名的另一个版本采用）；没被采用过的无从核对、照旧并入；这次全扫里扫到的文档不经这道核对。归属键是（标题, 歌手），分不出同名同歌手的两个版本，`<body dur>` 又只有上限：不核对时，先放过 200 秒的短版、再放 300 秒的长版，留下的短版文档首扫就被采用，长版自己的文档随后出现也换不上（已有歌词就不再挑），整首用短版的字和时间轴（PR #60 审查 R21）。`pickTTML` 的规则：

- 记在当前歌名下的（还在内存里的与留存的一样）：用。只要求 `<body dur>` 不比位置结构时长长出 `lyricDurSlack`（6s）、最后一行的起点不晚于音频结束。**`<body dur>` 只能当上限**：它是歌词正文的长度，尾奏长的歌比音频短得多（231.752 对 245、130.820 对 159），旧实现要求它不短于音频时长，这些歌整首被当成没有歌词。
- 来历不明的（本会话第一次全扫时就在内存里的记 `preAttachOwner`，出现时没认出存储的记 `""`）：只有 `<body dur>` 比时长长 0~6s 才用，附加前就在的还只给本会话第一首。
- 记在别的歌名下的：不用。都不满足就按（暂时）没有歌词——宁可没有，不配错。

✅ `TestPickTTMLOwnership`、`TestPickTTMLPlausibility`、`TestPickTTMLOrder`、`TestSessionShortBodyDurLyricsAccepted`、`TestSessionPreviousSongLyricsNotReused`、`TestSessionSkippedSongLyricsStayWithIt`、`TestSessionPreAttachLyricsNotReused`；归属随新副本改记：`TestAddDocsAddresses`、`TestSessionNewCopyReassignsPreAttachDoc`、`TestSessionPrefetchedDocFollowsNewCopy`、`TestSessionFirstScanIsBaselineOnly`、`TestSessionNewCopyOfOtherDocKeepsCurrentLyrics`、`TestSessionCorrectedTitleMovesReassignedDoc`、`TestSessionNewCopyWithoutStoreKeepsOwner`；离开内存后仍可用：`TestSessionLyricsSeenOnlyWhileLoading`、`TestSessionKeptLyricsFollowOwner`、`TestSessionCorrectedTitleMovesKeptDoc`、`TestSessionUnownedLyricsNotKept`、`TestSessionKeptLyricsCap`（候选不并入留存、清屏时清掉留存、凡扫到的都留、不设上限、淘汰最近见到的、再见到不更新时刻，各有用例变红）；留存只配给同一时长：`TestSessionKeptLyricsFollowOwner` 的「同名同歌手的更长版本」「短版文档还在内存里时长版仍采用它」（记录现状）「改记后不按原先的时长核对」「同一歌名下再出现新副本不作废时长记录」四例（去掉时长核对、采用时不记时长、改记时不删时长、同一歌名下出现新副本也删、核对也加到当次扫到的文档上、没记过时长的也不并入，各有用例变红），均实测。

宣告不等歌词：换歌时歌词已在内存（或留存里有）就在同一次全扫里配好再宣告（`song_info_update` → `all_lyrics` → 状态）✅ `TestSessionAnnounceWithLyricsOnce`；不在就先发空的 `all_lyrics`，到了补发 ✅ `TestSessionLateLyricsReemitted`、`TestSessionLateLyricsSeenOnceThenGone`。**「无歌词」（`index:-1`）在换歌 `lyricGrace`（15s）后才发**，每首至多一次，之后歌词到了照常补发：旧实现首扫没找到就发，下游当场按纯音乐处理，而审查方见过 10 秒以上才到的 ✅ `TestSessionNoLyricsVerdictAfterGrace`（实测）。TTML 只有一行「纯音乐，请欣赏」时按无歌词：`all_lyrics` 为空、提示随 `index:-1` 在宣告时发出，不再推 index 0 的 `lyric_update` ✅ `TestSessionPureMusicHintNotPushedAsLine`（实测）。

已知的缝（不用代码去堵，`noteDocs` 注释）：① 当前歌没有自己的歌词时，其间出现新副本的别的文档（预取的下一首、很晚才到的上一首）只要过得了时长上限，就会被当前歌采用，并留在它名下，直到这份文档在别的歌名下再出现新副本。② 预取的文档轮到自己那首时若没有再出现新副本，仍记在别的歌名下、配不上（2026-10-08 真机五次观察都出现了新副本；`TestSessionPrefetchedDocFollowsNewCopy` 的「没有再出现新副本」一例记录这一现状）。③ 服务附加时正在放的那首，文档在第一次全扫建基线时就在，`<body dur>` 若比音频短就配不上，下一首起正常（真机见过；例外见 ④）。④ 留存只在本会话内，服务重启后是空的：重启前播过的歌本会话重播时，出现新副本即配上，副本地址没变的仍按来历不明处理（2026-10-09 真机：《crossing field》重启后再放，文档 3 份的地址都没变，它的 `<body dur>` 239.054 比曲目时长 248 短 9 秒，严格窗口也配不上，报了无歌词；`TestSessionNewCopyReassignsPreAttachDoc` 的「地址集合不变」一例记录这一现状）。⑤ 同名同歌手的两个版本只靠时长分开，且只管留存的：时长相同的两个版本分不开；短版的文档还留在内存里时（由全扫带入、不经时长核对），长版仍可能采用它，留存之前就是这样；留存的文档从未被采用过（只在加载中见过）时没有时长可核对；两个时长不同的版本共用逐字相同的文档时，留存的那份只配给最近采用它的那个时长，另一个版本要等文档在内存里再出现。

解析（`lyric/ttml.go`），只处理 Apple 下发格式里有出处的写法：

- 时间表达式（`parseTS`）两种写法：时钟写法 `[h:][m:]ss.mmm`，至多三段，冒号超过三段的（如带帧号的 `hh:mm:ss:ff`）拒绝；不含冒号的偏移写法可带度量后缀 `h` / `m` / `min` / `s` / `ms`，数值与后缀之间不得有空白。真机见过整份文档写成 `begin="1.127s"` 的，不认后缀时每一行都被跳过、等满 `lyricGrace` 报无歌词。✅ `TestParseTS`、`TestParseTSMetricSuffix`、`TestParseTSMinAndSegmentLimit`、`TestParseTTMLOffsetTimeLineFixture`、`TestParseTTMLOffsetTimeWords`（实测）。
- 逐字：`itunes:timing="Word"` 才有 `<span>` 逐字，Line 型无。解不出 `begin` 的 `<span>`（背景人声 `ttm:role="x-bg"`）当容器，内层逐字 span 各自成词；外层自带 `begin` 的仍整段作一个词。span 之间的空白并入前一个词、行尾空白去掉——overlay 的逐字效果与 `lyrics_detailed[].text` 直接拼接 words，否则英文显示成「HelloWorld」。行文本与译文里连续的 XML 空白（空格、制表、回车、换行）折成一个空格，全角空格不折。✅ `TestParseTTMLWordTiming`、`TestParseTTMLWordSpacing`、`TestParseTTMLBackgroundVocals`（实测）。
- 译文取 `<translations><translation><text for="Lk">` 内的全部文字（含内嵌 `<span>`），按 `itunes:key` 与正文行配对（非 kugou 的行序对齐，也非 soda/cloudmusic 的时间戳对齐，§6.2）；多个 `<translation>` 时同一 key 先到先得，按 key 逐个算——第一种语言缺的 key 由后面的语言补上；`roma_text` 恒空。✅ `TestParseTTMLTranslationNestedSpan`、`TestParseTTMLMultipleTranslations`（实测）。按 key 而非行序取译文由后者守住：2026-10-09 在隔离副本里改成按行序取，它的第 2、3 行取错而红；`TestParseTTMLLineFixture` 仍绿（《晴天》夹具的 key 与行序一致，它只守「有译文」）。
- 只记录现状、不作承诺：`itunes:timing="None"`（`<p>` 没有时间）解析为空；`<p>` 直属 `<body>` 或在嵌套 `<div>` 里时不收（Apple 下发的文档未见这两种结构）✅ `TestParseTTMLTimingNone`、`TestParseTTMLParagraphPlacement`（实测）。

**6. 全扫只在需要时发生，都有节流。** 全扫 = `Reader.Scan`，只读已提交、`MEM_PRIVATE` 且保护恰为 `PAGE_READWRITE` 的区（单区 ≥256MB 跳过）。读缓冲由 `Reader` 持有、跨全扫复用（`Reader` 因此不可并发调用；`Scan` 的返回值不引用它），容量超过 `maxKeptBuf`（64MB）的在全扫结束时丢掉、下一次重新分配——稳态播放零全扫，留着的缓冲会在整个附加期间常驻。逐窗找位置候选（每 8 字节一个窗）先按时长字段的原始位预筛（`twinDurInRange`，与「15 ≤ 时长 ≤ 7200」等价），过了才走 `isTwinCandidate`（`appendTwinCandidates`）。

全扫耗时（真机，本机）：2026-10-09，Apple Music 暂停中，可读区 436MB、5511 个区、最大区 31.3MB——收候选的 `Scan(true)` 0.19~0.24s，不收的 `Scan(false)` 0.12~0.16s；逐窗找候选连同读内存，不带预筛 0.31~0.39s、带预筛 0.14~0.17s，候选数相同；每次 `Scan(true)` 分配约 16MB，都来自结果本身（TTML 文本、封面与签名的拷贝），5 次全扫触发 4 次 GC。改动前（2026-10-08，可读区 476MB、2316 个区；没有预筛，读缓冲只在一次遍历内复用）：`Scan(true)` 0.40s（只读 88ms、属性存储搜索 +40ms、TTML 搜索 +40ms、逐窗找候选 +240ms），`Scan(false)` 0.15s，每次全扫分配约 66MB，5 次全扫触发 11 次 GC；Apple Music 启动期间或多方同时扫时见过 0.7~0.8s，改动后未复测。区筛选的对照同样是改动前量的：只排除 MAPPED/IMAGE 时 2.3s，读全部可读区 3.2~4.2s。旧实现每区新分配缓冲、全扫近乎连续，审查方真机单核 CPU 约 100%、150 秒累计分配约 210GB。✅ 预筛与逐窗收候选 `TestTwinDurInRangeMatchesFloatCompare`、`TestTwinPrefilterNeverDropsCandidate`、`TestAppendTwinCandidates`、`TestAppendTwinCandidatesCap`；缓冲留用上限 `TestDropOversizedBuf`、`TestEachRegionDropsOversizedBufOnReturn`；结果不引用读缓冲 `TestFindNowPlayingResultDoesNotAliasInput`，均实测。❌ 区筛选无离线测试（放宽筛选全绿，实测），收窄后漏读只有真机能看出；缓冲跨全扫复用、`eachRegion` 遍历真进程走完的那个出口、`Scan` 对 `appendTwinCandidates` 的调用依赖真进程，无离线测试；预筛只省时间，整个去掉时测试全绿（实测），省了多少只有真机量得出。

| 需求（`maybeScan`） | 何时 | 间隔 |
|---|---|---|
| 存储换了 | 盯梢发现（任何时候，含加载中） | 立即；连着触发时翻倍，封顶 5s |
| 取标题 | 换歌后（位置结构已确认、不在加载中） | 立即，然后 0.5/1/2/4/8/15/30/60s |
| 找歌词 | 有标题、还没有歌词 | 换歌后头 3s 约每 0.4s，到 15s 约每 1.2s，之后 5/10/20/30/60s |
| 核对标题 / 找封面 | 换歌后 1.5s 一次；宣告时没有封面的在 3/7/12/20s 各一次 | — |
| 只找存储（不收候选） | 尚未确认位置结构、也没见到存储（本会话第一次全扫照收候选） | 立即，然后 2/3/5s，封顶 5s；存储抖动（出现 / 消失相隔不足 10s）时不回到第一档 |
| 收集位置候选 | 见到了存储而尚未确认位置结构（这时任何需求触发的全扫都顺带收；认出存储的那次没收则立即补一次，存储抖动时不补），或确认了 5s 还宣告不出歌 | 立即，然后 2/3/5/5s，封顶 15s；存储抖动时不回到第一档 |
| 找替代的位置结构 | 已确认的停住超过 20s | 立即，然后 30/60/120/300s |

一次全扫满足所有到期需求，任意两次之间至少隔 `minScanGap`（0.3s，从扫完起算）；稳态播放（位置已确认、标题歌词封面到手）零全扫。✅ `scanbudget_test.go`（六例，实测）、`TestSessionNoCoverGivesUp`。未确认位置结构时周期全扫的占空比（按上面 2026-10-09 的复测值）：没见到存储约 0.14s / 5s，约占单核 3%；见到存储约 0.22s / 15s，约占单核 1.5%。前者与改动前每 15s 一次 0.40s 的全扫相当，但读内存的总量约为改动前的 3 倍；这个状态只在 Apple Music 启动后、放第一首歌之前，或位置结构丢失且存储也消失时存在。

真机（2026-10-08，最小组合服务 + Apple Music，不含音频转发）：带另一个每 40 秒重报一次 playing 的播放器、5 次切歌，新歌都在切歌后 0.8~1.0s 到达根输出（其中两次是从那个播放器手里接回来）；这 100 秒主进程平均占单核 3.5%。

**7. 状态**（非歌名态的 Detail 均过 `i18n.T`；下表反引号里是调用点上的英文原文，「」里是中文环境下显示的文字）：

| 情形 | 状态 |
|---|---|
| 找不到进程（及 `Attach` 的其他失败） | `waiting_process` + `process_not_running`，`Apple Music not running`「Apple Music 未启动」 |
| `OpenProcess` 拒绝访问 | `waiting_process` + `no_admin`，`Cannot read the Apple Music process (Apple Music may be running as administrator)`「无权读取 Apple Music 进程（Apple Music 可能以管理员身份运行）」 |
| 附加后、确认位置结构之前（没在放、暂停中附加，或正在放但还没认出，约 2~3s）；加载中（时长为 0）超过 `loadingGrace`（6s）；换歌后 `titleGiveUp`（10s）仍取不到标题（每轮都判，不等下一次扫描） | `waiting_song` + `no_song`，`Apple Music not playing`「Apple Music 未在播放」，后两种附带 `clear_song_data` |
| 有歌 | `playing` / `paused`，Detail =「歌手 - 歌名」 |
| 进程退出 | `standby` + `process_exited`，`Apple Music has exited`「Apple Music 已退出」+ `clear_song_data`，2s 后重新 `Attach` |

`Attach` 失败的两种都附带 `clear_song_data`，3s 后重试。`no_song`（`player.ReasonNoSong`）是为 applemusic 新增的原因码：进程在跑、没有歌在播放或已加载。加载超时清屏时**整个复位当前曲目**（`clearSong`；「最近一次发出的封面」除外，见 4）：不复位则 `song.dur` 仍 > 0，加载期间每轮都重进 `clearSong`、重发 `clear_song_data`；同名同歌手在加载结束后重新宣告，靠的是 `onTwin` 在加载结束时无条件进 `beginSong`（PR #60 审查 P1）。✅ `TestSessionClearThenSameSongReannounced`（去掉复位即红）、`TestSessionTitleGiveUpOnTime`（删掉判定即红）、`TestSessionStartsWithNoSong`，均实测。这些非歌名态的 `reason` 也随服务端缓存进 HTTP `/status_update`、连接补发与 player_switch 的 FullState（§6.4 ④）。

**8. 音频波形的目标进程是 `AMPLibraryAgent.exe`，不是 `AppleMusic.exe`。** 出声的是同一安装包里的 `AMPLibraryAgent.exe`，由系统 DCOM 服务（svchost）拉起，不在 `AppleMusic.exe` 进程树内；WASAPI 进程 loopback 只抓目标进程树，故 `procscan.PlayerProcess["applemusic"]` 是 `applemusic.AudioProcessName`，读内存仍是 `mem.TargetProcessName`，两者各管各的（§5.1 第 4 步的例外）。2026-10-08 真机：`AppleMusic.exe` 树内帧在流动但 RMS 恒为 0；改捕 `AMPLibraryAgent.exe` 后放歌时 `/applemusic/audio-ws` 与根 `/audio-ws` 约 25 帧/秒、RMS 非零。映射值 ✅ `TestPlayerProcessMapping`（实测）；出声的是哪个进程 ❌ 只能真机验，Apple Music 换版本后要复核。

**其他。** Apple Music 常自重启（pid、地址随之变），`Reader.Valid()`（`GetExitCodeProcess` 为 259）在每轮开头探到退出即结束会话、回外层重新 `Attach`，位置结构重新发现。真机测试 `TestLiveSmoke`（`mem` 包）与 `TestLivePlayer`（本包）默认跳过，`PLAYERCAP_LIVE=1` 才跑：开发机常开着 Apple Music，不加开关时 `go test ./...` 会去扫别的进程内存、结果随本机状态变化。其余测试全部离线（本包与 `mem` 包 Windows-only，`lyric` 包 Linux 可跑，§8），会话场景用 `sessionharness_test.go` 的假内存 + 假时钟回放。真机上的时序只能真机验：SMTC 的「下一首 / 暂停 / 播放」可以驱动 Apple Music，拖进度条它不响应、要走 UI 自动化（滑块 `LCDScrubber`）。

---

### 7.9 cloudmusicv3：位置时钟按 DOM 跳秒对表（`player/cloudmusic/domsync.go`）

网易云的位置时钟是「锚点 + 墙钟」（`cloudmusic.go` 的 `baseRealClock`），进度来源只有 DOM 上 `.curtime-thumb` 的整秒文本（`ExtractionData.DomTimeSec`）。原先时钟只在换歌、暂停恢复、seek 检测三处被写，每处各带一个不固定的偏差，各首歌早晚不一（issue #77）。现行：在 DOM 秒数跳变的那一拍把时钟夹进一个区间，其余时候照旧按墙钟走。设计、数据与否决过的方案见 `doc/design/cloudmusic-dom-clock-sync-design.md`。

**实测**（2026-10-09 两次真机：18 次换歌、2 次启动附着、3 次暂停恢复。探针在秒数变化的那一拍打印「DOM 秒数 − 时钟」，下称读数）：

- 同一首歌里各次读数相差约 1ms：跳秒的时刻很稳，可以拿来对表。整秒值本身不是位置（真实位置在该秒内任意处），可用的只有它变化的那一刻——与 §7.7.1 的 `progressSeconds`（采样瞬间的真实位置，可以直接落锚）不同。
- 换歌后的落后量 = 换歌分支耗时（459～696ms）− 起播延迟（186～451ms），两者都不固定：13 首常态换歌的读数 233～399ms，起播延迟最长的一首只有 0.10～0.12s。按分支耗时补偿只去掉了其中一侧，分支偏慢的歌反而偏早（上一版实现因此未合入）。
- 暂停：读到 `playingState != 2` 之后 DOM 进度还走约 0.5s，恢复后读数 0.73～0.88s，保持到曲终。
- seek 检测把时钟置成整秒值，其后落后 0～1s。

**规则**（`domSync.observe`）：相邻两拍都可用（读得到进度、时钟在走、不在换歌前后 500ms 之内、本拍没有判 seek）且秒数恰好加 1 时，把这一拍 `got` 时刻的时钟夹进 `[D − L, D − L + u]`，`u = min(w, 1 + r)`：`D` 是本拍秒数，`w` 是本拍 `got` 减上一拍 `sent`，`r` 是本拍 `Extract` 的往返耗时。在区间内不动。换歌、暂停恢复、跳转之后不另设规则，三者都只是让落后量偏出区间，下一个可信的跳秒夹回。

三处不要改：

- **`L`（`domLagSec`，0.30s）不是物理量。** DOM 跳秒与出声的先后没有量过。它是为保持默认 `cloudmusicv3-offset: 500` 下的现有观感而留的：13 首常态换歌较大一档读数的均值是 302ms。别把它当「界面比出声晚 0.3s」引用；也别为了「更准」改成 0——整体提前 0.3s，现有 config.yml 里的偏移都要重调（仓库所有者已否决），落后量的下沿还会贴上回跳的触发线。观感整体偏早或偏晚时只改这一个常量。
- **别退回每跳落锚，也别夹到区间中点。** 轮询相位决定「看见跳秒时它已经过去多久」（0～u），实测 4 首歌的相位在两档之间交替。落锚或夹到中点时落后量随相位来回，每个跳秒校一次；夹到最近的边界时区间恒含「界面进度 − L」，落后量只朝 `L` 移动且不越过。✅ `TestObserveClampsOnCredibleFlip`、`TestReplayAlternatingPhaseConverges`（两种写法各自变红，实测）
- **上界里的 `1 + r` 不能写成 1。**「界面进度不到 D+1」只在页面读 DOM 的那一刻成立；`Extract` 返回得晚时 `got` 时刻可以超过它，封顶在 D+1 会把正确的时钟回拨。✅ `TestObserveClampsOnCredibleFlip` 的「Extract 返回得晚」一例（实测）

**回拨与行号。** 时钟超前时回拨到上界。时钟只有一条时间线：各对外事件的位置、seek 检测与 DOM 比的量、冻结、位置采样都取回拨后的读数（`at`）；只有行匹配取「读数与回拨前位置的较大者」（`lineAt`），否则回拨跨过行边界时上一行会被再推一次、片刻后又推回。读数取在哪个时刻分两种，不能合成一个：与 DOM 读数成对的（行匹配、seek 检测、起走与冻结、对表）取本拍 `Extract` 返回的时刻 `got`；对外事件的位置与位置采样取发出那一刻的——下游按事件到达的时刻落锚，而 `got` 与发出之间可以隔着阻塞调用（Redux 歌词晚到时读翻译歌词的 `ExtractTlyricLines`），带 `got` 时刻的读数会多落后一个调用耗时。写成「回拨时停走」会出现两条时间线，漏掉一处对外位置就把前端锚回旧的那条（设计 §9.6）。✅ `domsyncrollback_test.go` 各例、`domsynclatelyrics_test.go` 的 `TestReplayBlockedTickEmitsLivePosition`（实测）；`runSession` 的各使用点取的是哪个读数、哪个时刻 ❌ 靠人（§4）。

**通知。** 前端只在带位置的对外事件上落锚，`position_sample` 不广播。校准改动了时钟而前端尚不知道的量（`untold`）达到 `syncNotifySec`（0.2s）时，在一拍末尾另发一条 `playback_resume`；两条之间至少隔 `PositionSampleInterval`，未到则顺延、不丢弃；任何带位置的对外事件发出时清零（经 `emitPos`，✅ AST 门禁）；这些事件要么发在本拍的对表之后（非换歌拍补发的歌词事件为此推迟到对表之后），要么所在的拍不对表，所以它不与那些事件同拍（§3.2）。常态换歌的微调（实测单首累计不超过 99ms）不发，暂停恢复后的那次（0.43～0.48s）发。日志 `synced to UI progress: … → …`（中文环境显示「按界面进度对表: … → …」）只在单次校准量达到 0.2s 时打，不受限频约束；它反复出现说明跳秒时刻不稳。✅ `untold_test.go`、`domsyncnotice_test.go`、`domsynclatelyrics_test.go`（实测）

**轮询间隔配大后校准自动变弱**，不设上限也不报错：250ms 时落后量在 `L` ± 0.25s；1000ms 时 `u` 约 1s，只纠正落在区间之外的偏差；2000ms 时每拍秒数加 2，没有跳秒，不校准，同改动前。✅ `TestReplayPollIntervals`（实测）

**前提与实现后的回归**（2026-10-09，数据在设计 §10.2 末与 issue #77 的评论）。本方案的前提是跳秒时刻稳：前台与窗口最小化时读数都稳定在一档（±1ms）或 200 / 300ms 两档之间，最长 140 秒；窗口被遮挡、泊在屏外时没有量过（`watchdog.LaunchFlags` 关了后台定时器节流与渲染降级，按理不变，推断）。前提不成立时限频只保证不刷事件，不保证位置准。`Extract` 的往返耗时常态 0～2ms，换歌那一拍 39～156ms；页面卡顿时见过一次约 1.3s，其间 DOM 停在 0。拖动之后 DOM 有时 0.1s 内就跳秒、有时停住约 1 秒（后者时钟走到整秒会误报一次回跳，其后的跳秒校回）。单曲循环没有测过。`L`、通知门槛与限频间隔的取值来自实现前的两次实测，回归里未见需要调整；离线测试只守逻辑。网易云页面里有更细的进度来源（原生回调 `audioplayer.onPlayProgress`），取用它要往页面里挂东西（§0 第 1 条），另见 issue #82。

---

