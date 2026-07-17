# Metabox-Nexus-PlayerCap 硬化报告

> 基于 109 个 agent 的风险加权审计 + 逐条对抗性证伪
> 生成于 2026-07-16

## 总评

> **2026-07-16 更新**：本条阻断性缺陷已修复（`1532052` / `951a361` / `7ab6ee6`），见「修复进度」。
> 下文保留原始判断以存档，但**订正其中一处错误断言**（见段末）。

有阻断性缺陷，且只有一条：`player/kugou/watchdog/watchdog.go:583` 与 `:617` 把 `CheckPatchStatus` 的 `canAutoFix` 丢进 `_`——酷狗一旦自动更新偏离钉死的 `knownVersion`，`:606` 的版本告警只警告不返回，代码在已经证明「目标指令不在那里」之后照样向 88.8MB 的 DLL 盲写 9 处补丁，无 `.bak`、无 unpatch；而 `libcefPatches[0].data` 与 `p1Patched` 逐字节相同（`:45` vs `:62`），Patch1 一落笔就把版本哨兵改写成「已确认」，于是**打错版本的损坏态与健康的已 patch 态从此不可区分**、CDP 永不起来、2s 一轮永久重试，用户唯一出路是重装酷狗——这同时踩中崩、卡、不可恢复三项。其余 HIGH 项是发版前必修但不阻断：它们要么需要特定输入，要么坏了还能重启服务。

**订正**：原文此处写「`:621` 的写后校验恒真」。该断言经对抗性证伪被推翻（3 名证伪者 2 判推翻）。
写后校验**并非恒真**——杀软静默回滚写入时，字节仍为 orig，`CheckPatchStatus` 走 orig 分支返回
false，`:621` 会正确报错。准确的说法是：该校验只验证「我写的字节落盘了吗」，不验证「该不该写」，
故对「打错版本」零保护。真正的不可检测性来自哨兵自我覆盖（上句），发生在**版本识别层**而非校验的
恒真性上。另需注意：原文设想的「部分写入/杀软拦截 → 校验抓住」路径其实多不可达——`patchDLLBytes`
快速失败，任何 Write 出错都会让 `TriggerAndWait` 提前返回，根本走不到 `:617`。

这个仓库的稳定性姿态可以概括为:诊断能力已经写完,闸门没接上。守卫函数、校验器、错误值大量存在且逻辑正确(`CheckPatchStatus` 的哨兵比对、`reader.go:145` 的 `isValidLyricText`、`mem.go:773` 的 `ReadSSOString`),但消费者是零或被 `_`/`default:` 静默丢弃——真正的风险不在「没想到」,而在「想到了、算出来了、然后扔了」。所以补救成本极低(多为一行 `if !canAutoFix { return }`、一个 `else { log.Warn }`),但在修好之前,系统对自己的损坏态是结构性不可见的,这比缺陷本身更值得警惕。

数据溯源:本报告基于 109 个 agent 的风险加权审计,但 workflow 的 report 阶段三次死于 API 错误,数据系从 journal 按「标题+位置」事后重建(内部统计 102/52/47,重建为 130/51/63,偏差约 20%,来源是同名发现被合并与一次 resume 的重复记录)——**实质内容经回代码复核可信,精确计数不可引用**。

---

核实完毕，9 条全部回到真实代码逐行验证过。以下是该节。

---

## 总览索引

**本节是全部发现的唯一总账。修掉任何一条，必须在同一个 commit 里把它在这里的状态改掉**
（规矩见 AGENTS.md §11「沉淀」）。本文 900+ 行，没有这张表就只能靠翻。

**计数的可信度**：见「总评」末段的数据溯源说明——实质内容可信，**精确计数不可引用**
（报告由 109 个 agent 的审计事后重建，内部统计与重建结果偏差约 20%）。下表按切片口径。

| 类别 | 数量 | 状态 | 详见 |
|---|---|---|---|
| 更早批次 | 6 | ✅ 全部修完 | 下方「已修复并提交」 |
| **HIGH** | 8 + 1 接受 | ✅ 全部修完 | [§HIGH](#high--发版前必修) |
| **MED** | 20 切片 → 18 实际（剔 1、半剔 1），其中 3 条已由 HIGH 顺带修掉 → 约 15 条待修 → **2026-07-16 已修 6 条、证伪 1 条（§4）、新增 1 条（§1d）→ 剩 4 条 + 1 暂缓** | 🔄 见下表 | [§MED](#med--能便宜修就修) |
| **LOW** | 11 切片 → 10 独立缺陷 | ⬜ 2 条建议顺手修，8 条记档不排期 | [§LOW](#low--观察项) |
| **未判决** | ~~15~~ → **实为 3**（2026-07-16 考古恢复内容；原文只有计数、无内容） | 1 ✅ 修 · 1 ⏸ 暂缓 · **1 ⚠️ 安全问题待用户定** | 下方说明 |
| **被证伪淘汰** | 12 + 1（§4 于 2026-07-16 追加） | ❌ 记档，别再「发现」一遍 | [§被证伪淘汰](#被证伪淘汰的择要) |

### MED 逐条状态

| 组 | 条目 | 状态 |
|---|---|---|
| §1a | `effect.go:316` setEffectParams 跑在 Upgrade **之前** —— 敲 URL 探活即全局改画质，无 per-connection 状态可恢复，只能重启 | ✅ `46386f3`。**⚠️ 本条原修法里「最好并进 `:327` 的同一临界区」是错的，照做会自死锁**：`setEffectParams` 内部自取 `h.mu`（`effect.go:135`），Go 的 sync.Mutex 不可重入 → handler 抱着 `h.mu` 永久卡死，连带冻死 `BroadcastEffectFrame`/`HasEffectSubscribers`/`EffectCaptureParams`/`handleEffectIngest` **整个特效子系统**，只能重启进程。三视角独立确认。实修法：整块移到 Upgrade 之后、`h.subs[sub]` 登记**之前**，不并进临界区 |
| §1b | `effect.go:340` + `server.go:490` 写 goroutine 不关连接 | ✅ `9cb446e` |
| §1c | `effect/effect.go:320` park 条件不含订阅者检查 —— 无人看时网易云无法被最小化 | ✅ `46386f3`（与 §1a 同 commit）。修法即笔记原判（加一个 `&&`），无调整。考古确认非有意设计：`:275` 那句「始终运行（不随订阅者启停）」是 `33585d1` 连同兜底一起**新增**的，下半句自陈动机是「以便任何时候都能把遗留泊车的窗口救回」——讲的是**循环生命周期**，不是 park 入口。且若「无订阅者也 park」是有意的，同 commit 的兜底会在 80ms 后撤销它，设计不会自相矛盾至此。**衍生出一条新 MED（§1d，见下），本轮未做** |
| §1d | **新发现**（§1c 的核实中浮现，非原切片）：`windowStateLoop` 顶部的**无订阅者兜底**（`!HasEffectSubscribers() && IsParked() → doUnpark`）复用了 `park.Unpark()`，而后者会把 `savedPlacement` 的 `swShowMinimized` 改写成 `swShowNormal` —— 直播中 OBS 切场景导致订阅者掉线时，已泊车的网易云会**弹回屏幕并抢焦点** | ⬜ 单独立项（用户已定本轮不动）。修法：加 `park.UnparkQuiet()`（跳过 ShowCmd 改写），**仅那条无订阅者兜底**改用它；另两个 unpark 出口——**手动命令**（`TakeManualParkCmd` 的 `case 0`）与**切回前台自动 unpark**——保持 `Unpark()`。**⚠ 按符号找，别按行号**：本条原写的 `:282`/`:316`/`:324` 已被 `46386f3` 自己的 diff 打漂（三个 `doUnpark()` 实为 `:287`/`:320`/`:340`，而 `:316` 现在是 `switch TakeManualParkCmd()`）—— 行号还会再漂。**framing 必须写对**：不是原改写错了（`park.go` 的 ShowCmd 改写带作者显式注释「还原成正常显示而非再最小化」，对用户主动切回窗口是**对的**），而是那条**非用户发起**的救援复用错了。**顺序强制**：必须在 §1c 之后做——只做本条不加订阅者门控会 12.5Hz 无限震荡 |
| §2 | HTTP 无超时 **7 个点**：`qqmusic/api.go:149/:222/:443/:374`、`cloudmusic/effect/effect.go:509`、`cdp/client.go:244` | ✅ 取词 6 处修完：`fetch.go` 三处 `71fe2f1`、`qqmusic/api.go` 四处 `a4b5319`（可达性最高的那组，poll 循环内同步调用 + router 对卡死的 playing 永不过期 → 永久幽灵）。**剩 `effect.go:509` / `cdp/client.go:244` 两处 localhost CDP 探测**未修（非取词、localhost 挂起概率低，按 MED §2 原判缓修，建议 2s） |
| §3a | `config.go:110` config.yml 按 **CWD** 解析（与 `README.md:210` 直接矛盾）+ `:348` 吞掉生成失败 | ✅ `1be577d`。**本条的「保留 CWD 作兼容回退」是假承诺**：CI 打包必然把 config.yml 放进 exe 同目录，zip 用户那儿永远有 → 回退分支对真实用户**永不触发**，它保护不了任何人，还恰好掩盖了唯一的真实回归。真正的兼容保护是两条日志（加载时打绝对路径 + 双份并存时 Warn）。另：笔记 `:594` 的权限判断**方向反了** |
| §3b | `config.go:211` 钳制只作用全局 Poll，per-player 直通（`wesing-poll: 5` 即可触发） | ✅ `aa78297`。**结论对，论据基本全错**：`wesing-poll: 5` 推不出 165ms（笔记的算式自相矛盾，见正文订正），真正会咬人的是 `poll <= 0` 的无界忙等；「四个播放器自保冗余但无害」四条**全部证伪**（kugou 零自保、钳制是它唯一保护）。且本修法**反转了 AGENTS.md §3.3/§13 的显式契约**「per-player poll 不夹紧」，已同 commit 改掉 |
| §4 | NaN 剩余：`reader.go:57` 不 break；`server.go:490` 把 marshal 错误当连接错误；**`server.go:328` 把 NaN 持久化进缓存 → 新连接全中毒** | ❌ **不修，记档**（`98030ae`）。3/3 判推翻、**0/3 判可达**。笔记的自我怀疑不但成立，还有**正面反证**：观测到的 `time=0` = 零填充内存 → float32 恒为 0.0，**永不可能是 NaN**。四条产 float 路径无一可达（qqmusic 读 **uint32** 不是 float —— 本条前提就错了）。且照 §4 修**会打红 `ws_zombie_test.go`**（NaN 是它唯一的触发载体，钉的是已证实并修掉的僵尸订阅者 bug）。`:58` 挂的「与 `9cb446e` 冲突」结清：**不是冲突，是 §4 输了** |
| §5a | `router.go:247` `range map` 定 overlay 归属 → 每 ~5s 抛硬币硬切（实测 1507:493） | ✅ `f516f22`。**判据被三视角一致否决并换掉**：不用笔记提的「配置顺序」（normal 组那个顺序硬编码在 main.go、用户改不了；prior 组的顺序是模板「取消注释」的产物、不是用户表达；且与作者声明的「最后播放者优先」相竞争），改用 **activeAt（recency 补全）**——lastPlaying 本就是它的标量版。另：多处行号错（kugou.go:53 实为 **:190**，错位 137 行）、漏了 wesing.go:50 也在循环内、「无条件」措辞失准、触发前提比笔记暗示的**窄得多** |
| §5b | `server.go:595` 兜底循环返回随机 player | ✅ `0ab8f61`。**本条的位置与范围原判都错了**：不是一个循环而是**两个**（:598 找 `Status=="playing"`、:603 找任意 `Status != nil`），行号已漂到 :597-607，「删 5 行」实为 11 行。四路调查 + 三视角证伪 0/3 推翻、一致投「两个都删」。决定性证据是笔记原先没有的：孪生体 `findAnyNonPriorPlayer()` 已被 `9090f0b` 从 router 删除，commit body 原话「**歌词冻结 bug 的根本原因**」「不检查播放状态」，:603 那个是它逐字的同型残株 → 删它与作者意图**同向** |
| §6a | `qqmusic/api.go:330` 兜底无内容校验 → QRC XML 标签铺满整首歌 | ⬇️ **降级 LOW·记档，修法整个作废**（`ba210c4`）。**招牌被实测证伪**：正常 QRC XML 喂 `parseLRC` 吐的是**正确歌词**（内层逐字行命中 `qqRe` → `len(lines)!=0` → 兜底根本不触发）。真实危害只在**空壳 QRC**（`LyricContent=""`），而它是否真从服务端回来**未能证实**（`api.go:252` 有 `rawLyric==""` 早退）。**推荐的修法是恒绿假门禁**：`isValidLyricText` 实测对 6/6 个 XML/hex 样本**全返回 true**，且**会误杀俄语歌词**。行号 5 个错 4 个（`api.go:202-209` 漂 71 行，指到了完全无关的代码）|
| §6b | `cloudmusic/lyric/fetch.go:47` 压缩型 LRC 正则贪心 | ✅ `f9500c3` **+ `d63a67d`（返工，别漏读）**。`f9500c3` 修了压缩型，但**同时把「吞掉 intentional blank」写成了测试、钉死成「预期行为」**——用户当场抓出「空行歌词会不会是 intentional blank？我们应该照着原文原样吐出」。`d63a67d` 才是完整的：保留 blank + 用 `hasRealText` 替代 `len(parsed)==0`。**这是本轮最有教训价值的一次返工**（「把 bug 固化成门禁」的实例），AGENTS.md §9 的那句「抓不住目标形状的门禁比没有门禁更糟」就是从它来的。另：`d63a67d` 改了 ParseLRC 的契约（行数 ≠ 有歌词）却**只更新了两个消费方中的一个**，漏掉的那条由本轮的 LOW review 抓出并修（见下方「LOW review 抓出的自伤」）。**真机取样复核了本条**（网易云 API 实取两首）：id=5277704 实测 24 行里 **16 行压缩型**（如 `[02:04.45][00:47.19]…`，网易云按降序排），id=156736 则 0 行 —— 即压缩型是**低频但真实**存在，笔记的可达性判断成立。网易云客户端截图确认其语义是「这句在这几个时刻各显示一次」。修法：行首连续时间戳全剥出、**每个各生成一条**（不限两个）、正文取其后、结果按时间升序并重排 Index。两首真实歌词已存入 `lyric/testdata/` 做锚定 |
| §6c | `qqmusic/mem.go:1005` 堆/内联判定与 length 校验挤在同一 `&&` → 乱码标题；**QQ 音乐每次自动更新都会踩** | ✅ `ba210c4`。3/3 判官一致「修」。**本轮少见的「笔记基本写对了」**——坐标 5/5 全对、机制实测坐实。但**严重性论证反了**：主触发是 **clear()**（实测 96.9% 出乱码），不是本条力推的「版本偏移错位」（实测仅 8.5% 乱码、91.5% 静默返回空）。另：本条问的「更好的降级」**早已存在**（`qqmusic.go:133` 的 `meta.Name != ""` 门 → 空名不 emit → overlay 保留上一首标题），不需要新增 |
| §7a | `park.go:65` 负结果不缓存 —— **实测烧 41.3% 个核心**，且 100% 花在无需工作的时段，Win11 上 park 禁用了照烧 | ✅ `48a6f54`。mainWindow 现在正负结果都缓存：找不到主窗时记 missUntil，1s 内不再枚举全进程。核实无误——注释自己写着「避免每次轮询都做进程枚举」，即**意图明确、实现漏了负结果**，非有意设计；且 `IsMainMinimized`/`IsMainForeground` 各调一次 mainWindow（一轮两次），windowStateLoop 注释明写「始终运行，不随订阅者启停」→ 无门控。park 包 Linux 编译不了（CI 跑不了），本机门禁 |
| §7b | `watchdog.go` `IsKuGouRunning` 用 `tasklist` → 2 次/秒 CreateProcess，用户关不掉。**阻塞本身是对的，别一起改掉** | ⬇️ **降级 LOW·顺手修**（3/3 判官一致，`3457832` 记档）。机制全部属实，但**实际用户可见后果 ≈ 零**（不闪窗——这是控制台程序，tasklist 继承父控制台；任务管理器也捕捉不到）→ 是清洁工作不是事故修复。**警告成立且理由比笔记写的更硬**。修法比笔记窄：`watchdog.go:18` **已 import** `x/sys/windows`，自带 CreateToolhelp32Snapshot 原生封装 → **约 15 行**，别手搓 NewProc、别 import wesing/proc（跨播放器耦合）、**更别抄 gopsutil**（那正是 §7a 的病灶）。需真机验证 → 下次触碰 kugou/watchdog 时做 |
| §8a | `MergeYRC` 贪心对齐 | ✅ 一行 tie-break（`3457832`），**其余降级 LOW / 记档**。3/3 判官一致「撤出 MED」。**本条三个前提全错**：① 不在 qqmusic，在 **`cloudmusic/lyric/fetch.go`（网易云）**，全仓 `player/qqmusic/` 对 YRC 零命中；② 与 QQ 版本策略**无关**（`knownVersions` 里没有任何 YRC 偏移），不能按「老版本低优先级」降级；③ `cloudmusic/lyric` 包 Linux vet **exit 0**、完全可测。**「三行止血」的第 2 条被仓库自己的绿色测试证伪**（详见正文），笔记连阈值都没给 |
| §8b | qqmusic lastName 提前更新 | ✅ `cb9810e` + `b445269`（后者订正前者引入的无界等待回归）。**自我复查另发现「陈旧非空 mid」缺口（守卫只认空、不认切歌窗口期读到的上一首 mid），经核实只影响 v20.05：`knownVersions` 表里 `SongMidParamsOff`/`StreamURLOff` 仅 20.05 非 0，22.xx 走 songID 主路径不碰 mid。用户实测 22.xx 从未 mid stale，且版本策略推 22.xx → 不修，记档于此。** |
| §8c | `finder.go:59` `FindLyricHost` 取 `results[0]` —— 同源 bug 在隔壁被修过两次，这是漏掉的第三处 back-port | ⏸ 排到下个 wesing 触碰窗口，需真机手测 |

**MED 剔除 2 条**（切片时去重漏了）：`server.go:610` 锁外读（已由 `0f83220` 修）、
`timer.go:121` NaN 半边（已由 `6667a01` 修）。

### 切片外但已修（本轮）

| 条目 | 状态 |
|---|---|
| `qqmusic/api.go` 的 LRC 正则分钟位写死两位 → 超 99 分钟的音频**整行静默丢弃** | ✅ `5d824f8`。**不在原切片里**（本轮核实 §6a 时顺带发现）。`\[(\d+):(\d{2})\.(\d{2,3})\]` 的分钟位 `\d{2}` → `\d+`；长混音 / DJ 串烧 / 有声书类的 `[100:05.00]` 整行匹配不上、被静默丢掉且无日志。与歌词来源格式无关，任何长音频都中。测试 `qqmusic/lrctimestamp_test.go`。**同一 commit 的 body 承载了一条用户决定**：QQ 音乐的**压缩型 LRC 与 intentional blank** 明确**不修**——探针实测 QQ 音乐歌词库无此格式样本（酷狗 5 首 226 行、QQ 50 行，压缩型与 blank 均为 0；只有网易云有），按版本策略不投入。**将来若真出现，照抄 cloudmusic 的修法**（`f9500c3` + `d63a67d`）即可。 |

### LOW review 抓出的自伤（2026-07-16）

用 LOW 视角回头审本轮 11 条 MED 修复，28 个 agent、5 个 lens（假门禁 / 回归 / 文档分叉 / 过度修复 / 遗留），
对抗性证伪后存活 19 条。**其中最重的一条是本轮修复自己引入的**：

| 条目 | 状态 |
|---|---|
| **`d63a67d` 改了 `ParseLRC` 的契约却只更新了两个消费方中的一个** —— `cloudmusic.go` 的 PureMusic 分支（API 二次确认）仍用 `len(apiLyrics) > 0` 判「有没有歌词」，而 blank 保留后行数 ≠ 有歌词。后果：CDP 误判纯音乐 + API 返回「只有 blank」的 lrc → 置 `cdpLyricsOK` 关掉 Redux fallback → **OBS 整首空白且不走清屏路径**。**加重项**：`resolveCDPLyrics` 的文档注释把这条漏改的判据**立成了「正确参照系」**（写着「与它保持一致」），下一个人照它核对会得出「两边一致、没问题」。 | ✅ `1df3cff`。判据换成 `hasRealText`（签名精确匹配、零新增代码），注释订正。**讽刺的是**：`d63a67d` 自己在 `fetch.go` 里写下了「判断一首歌有没有歌词要看有没有实词行，别用 `len(结果)`」——同一个 commit 里立的契约，在 200 行外自己违反了。**教训：改共享函数的语义时，`grep` 全部消费方，别只改手头这个。** 门禁缺口诚实记录：该分支在 `pollLoop` 里，要打桩 CDP+HTTP 才测得到，变异（判据退回 `len()`）实测**存活** —— 没为它重构主路径（动 `isPureMusic`/`songDuration`/`matchedRedux` 的控制流，风险 > 收益）。 |

**其余存活项的处置**：文档分叉类（AGENTS.md 的 `config.go:211` 锚点被同轮 diff 打漂 70 行、
§1d 的三个坐标全错、两处「AGENTS.md §13」指错节、`:1187` 漏网的「唯一有真实 CI 覆盖」、
「没有 router_test.go」已过时）**均已随本条订正**。**根因是一个的**：本轮往代码里加了大量注释，
**注释自己把注释里引用的行号打漂了** —— 已把关键锚点改成**符号引用**。

### LOW 处置

**结论：没有一条该升级**——后果全部落在正确性/功能完整度，无一条有稳定性含义。但两条性价比
不成比例：

> ⚠️ **「下次碰到该文件时顺手修」这个触发器已经失效过一次**（`ba210c4` 碰了 `mem.go` 却没做
> `mem.go:267`）。原因是它太粗：**碰文件 ≠ 碰函数** —— `ba210c4` 动的是 `ssoFromBuf`，与模块枚举
> 既不同函数也不同调用链。触发器已换成明确排期，别再指望「顺手」。

| 条目 | 处置 |
|---|---|
| `mem.go:267` 模块枚举无截断 → 越界 panic | ✅ `5728078`（2026-07-16，单独 commit）。一行截断 + Warn。**没用 MSDN 的两遍模式**：漏找 DLL 的降级后果是 ConnectQQMusic 返错 → 重试循环继续转，已够用。无测试（触发需模块数 >1024 的真实 32 位进程），靠 review 守。原为10 条里唯一后果是**进程死亡**（全仓 `recover()` 零命中 → 取词服务当场消失）。是 LOW 因为触发概率，不是因为后果 |
| `main.go:646` `io.Copy` 丢 err | ⬜ **顺手修**。唯一会留下**持久磁盘损伤**的点（截断的 canonical exe + 干掉原进程 → 主播下次双击起不来） |
| `effect.go:609` 日志打请求值而非生效值 | ⬜ 本组唯一有间接稳定性含义：主播调低画质、日志确认「q60 已启动」、实际仍 q95 |
| `config.go:60/:241` | ❌ **不改，且不应按其方向改**——DefaultConfig() 不带 per-player 值是刻意的哨兵层，按其修法反而打破模板自述契约 |
| `timer.go:108` 最低地址优先 | ⏸ 需真机证据；核心前提是断言非证据 |
| 其余（`effect.go:602/:619`、`fetch.go:332`、`cdp/client.go:130`、`qrc_decrypt.go:18`、`main.go:521`） | 记档不排期。**`qrc_decrypt.go` 值得加两行「勿修」注释**——S-box 偏离 FIPS 是与 QQ 音乐位级兼容的承重前提 |

### 未判决 ~~15 条~~ → **实为 3 条**（2026-07-16 考古恢复）

> **原文写「15 条」且从未记下它们是什么 —— 两处都错了。** 计数错了，内容也一直没落盘：
> 这半年里任何人想动这批，都只能重跑一遍 109-agent 审计。数据其实一直躺在磁盘上。

**考古结论**（从 `fe72727d` 会话的 `wf_cb3de80d-752` journal + agent transcript 恢复）：
那轮 workflow **204 started / 199 result → 死了 5 个 agent**，逐个查 transcript 后身份是：

| agent | 身份 | 说明 |
|---|---|---|
| `a184117c` / `ab7cb47d` / `ace0cb62` | **证伪 agent** | **这 3 条才是真正的「未判决」** |
| `ab2967cb` / `a554880b` | **report agent** | 「存活的发现（52 条 / 54 条）」—— 正是总评说的「report 阶段死于 API 错误」，笔记就是从它们的 prompt 重建的 |

即「15」大概率是重建时把 report agent 的死亡也算成了未判决条目。总评自己写着「精确计数不可引用」，
这里是又一个实例。

**真正未判决的 3 条**（原始标题/位置/等级/失败场景已全文恢复，出处见上；**行号是 2026-07-15 的，已漂**）：

| # | 条目 | 2026-07-16 复核 |
|---|---|---|
| 1 | **`isValidLyricText` 自初次提交起零调用**，LoadLyrics 的垃圾过滤全是时间维度且对第 0 行完全失效（`wesing/lyric/reader.go:145`，MED） | **仍存在**（grep 确认只有定义+注释，零调用方）。⏸ 暂缓：它的上游是 §8c（`finder.go:59`），而 wesing 按用户指示整体暂缓。**注意**：本轮 §6a 的核实实测了这个函数 —— 它是个**码点区间占比检查器**，不做任何结构判断，对 XML/hex **6/6 全放行**，且**会误杀俄语歌词**（白名单无西里尔字母）。所以「把它接上去」不是修法，见 §6a |
| 2 | **非 socket 类 Extract 错误永不退出会话**（`cloudmusic.go:174`，MED） | ✅ **已修 `dc41170`，但只修了日志层 —— 原发现的修法方向整个是错的**，详见下方专段 |
| 3 | **`PatchRegistryAutoStart` 把 `--remote-debugging-port=9222` 永久写进用户自启动，无卸载路径**（`cloudmusic/watchdog/registry.go:35`，MED） | **仍存在且未核实**。grep 确认：`cloudmusic.go:103` 无条件调用、`DebugFlag` 常量在、**卸载/还原入口零命中**。⚠️ **这条的性质是安全问题，不是普通缺陷**：首次运行即改写 `HKCU\...\Run`，此后每次开机网易云都带无认证 CDP 端口启动 —— **与 PlayerCap 是否在跑、是否已卸载无关**；本机任意非特权进程 GET `127.0.0.1:9222/json` 即可在已登录页面里执行任意 JS（读 cookie/token、以用户身份发请求；仓库自己的 `cdp/client.go` 就是这个范式）。按 AGENTS §14 已接受的 effect-ingest 无认证的口径，**这条更重 —— 它改的是用户的持久化状态且卸载后仍在**。**要动先问用户**（涉及产品行为：加卸载入口 / 改按需注入）|

**这三条的性质**：证伪 agent 死了 = **既未证实也未推翻**。**绝不混进 HIGH/MED 清单**——
实测约半数原始发现经不起对抗性证伪，而 #2 正是活例：核实后它的**四个承重论断被证伪**、
修法方向被三个视角一致否决。**要用先补做证伪。**

### 未判决 #2 的核实结果（`dc41170`）—— 机制成立，但修法方向整个是错的

> **这是「约半数原始发现经不起证伪」的活标本，也是「照发现的字面去修 = 把 MED 修成
> 直播事故」的实例。留全文，别再照原发现动手。**

**成立的部分**：非 socket 错误 → `continue` → `runSession` 永不返回（全函数只有 `StopCh`
与 `IsClosed()` 两个 return，生产中 `Stop()` 零调用方、signal handler 直接 `os.Exit`）；
默认配置下 poll 30 → 被 `<50ms→100ms` 抬到 100ms → **10 行/秒**，且 `log.Warn` 无 level 门控。

**被证伪的四条承重论断**：

1. **报错串是错的**。实测（假 CDP 端点驱动真实 `Extract()`）：`"null: store not found (depth 80)"`
   是**非空且 != "null"**，走的是 **json parse error** 分支，不是发现说的 `extraction returned null`。
   两个承重判据（`IsClosed()`=false、不含 `"no root"`）结果一致，所以机制仍成立，只是证据描述不准。
2. **「connected 位持续说谎」证伪 —— 而且照它修会打断特效镜像**。该场景下网易云在跑、
   socket 活着、canvas 健康，坏的只有 fiber 里 `memoizedProps.store` 一个私有形状；而
   `connected` 的契约白纸黑字是「**CDP 会话是否在进行（网易云已连上）**」—— **字面为真**。
   让它归 0 → `effect.go` 的 `setShowing(false)` + 停止截帧 → **前端特效淡出**，把一条当时
   完全健康的独立产品线（AGENTS §1.1 明列）一起打断。
3. **「唯一能修复网易云的机制永久停摆」严重夸大**。`EnsureDebugMode()` 在 headline 场景下
   **本来就是 no-op**：`process.go` 的 `if hasFlags { return false, nil }` —— 网易云在跑且带着
   我们注入的保活参数就直接早退。「机制停了」≠「修复没了」：fiber 形态真变了是**我们的 JS
   与新版不兼容**，重启一万次网易云也没用。
4. **两个触发前提都无证据，且论证方向反了**。「网易云自动更新后 fiber 形态变」——
   3.5 个月、11 个 `client.go` commit、34 个网易云相关 commit 里**零次**因网易云更新而坏；
   walk 依赖的全是 React/Redux 通用内部形状（`#root` / `__reactContainer` 前缀 /
   `memoizedProps.store` / `st['playing']`），**不含任何随构建变化的 minified 名**；真正脆的
   DOM 选择器全在 try/catch 里优雅降级。**更关键**：网易云自动更新 = 进程重启 = socket 断
   = `IsClosed()` 为真 = **已经会正常退出，压根不触发本条**。而唯一代码可确证的永久失效源
   （`client.go` 的 `pages[0]` 兜底连错 target）实测返回 `null: no root` → **正好被降噪过滤
   → 静默卡死、不刷屏**，症状与发现所述**相反**。

**⚠️ 发现隐含的修法（加连续失败计数 → 退出会话）会造成比原缺陷严重得多的后果，别做**：
- 退出会话 → 外层 for 重跑 `EnsureDebugMode()`，而它在「`Cmdline()` 读失败」与「进程无保活
  marker」之间**不可区分** → `taskkill /F /IM cloudmusic.exe`。**今天「永不退出」反而让这个雷
  最多踩一次；加失败计数 = 提高频率 = 直播中反复杀主播的网易云。**
- `store not found` 是**冷启动必经串**（要求 Redux 的 playing 分片已 hydrate），纯次数阈值
  必然误杀启动期；而 poll 可被 per-player 覆盖，同一个 N 对应的真实时长能差 20 倍
  （`b445269` 有同型判例）。
- Start() 成功路径零 sleep + Connect 恒成功 → 退出会话会变成**热循环**，刷屏速率**高于现状**，
  外加 `ClearSongData` 反复清屏 + connected 抖动 → 路由翻转 + 特效淡出闪烁。

**所以只修了日志层**（三个视角一致）：`extractErrLog` 降噪 —— 同一错误首次打一次、之后每
60s 复读一次「仍在重试」、Extract 成功后重置。**控制流一行未动。**

**真正的危害只剩这一条，但它比原发现说的更硬**（发现完全没提）：日志走 **stderr 且不落盘**
（AGENTS §0），10 行/秒几分钟就把控制台回滚缓冲冲干净，**连带冲掉另外三个播放器的全部日志**
—— 摧毁的是**直播出问题时全系统唯一的现场证据通道**。所以也**不能彻底静默**：「一次都不打」
和「10 行/秒」一样坏。

**没扩大 `"no root"` 过滤面**（有 lens 建议删掉它，未采纳）：三个错误串
（`null: no root` / `null: no react container` / `null: store not found`）**同生于 `fa59084`**，
作者在知道另两串存在的情况下**只过滤了这一个** —— 那是**选择，不是遗漏**。加了周期性降噪后，
漏网的 `no react container`（启动期今天就在刷屏）自然被治住，无需动那个有意图的判断。

**等级订正：MED → LOW-MED**（触发前提未证实，真实危害只剩可观测性）。

配套 `extracterrlog_test.go`；变异四方向全红：恒返回 true（= 原始缺陷）→ 2 条红；去掉复读
（永久静默）→ RepeatsAfterInterval；持续时长从上次复读起算 → 同上；reset 空实现 →
ResetAfterSuccess。cloudmusic 包 Linux vet FAIL，本机门禁。

### 文档内部的两处张力（别当成矛盾）

1. **%TEMP% 提权**：被证伪段判为「冗余向量、非最薄弱环节」，但 `7ab6ee6` 仍修了并开了
   issue #41。纵深防御下修它是对的，但笔记的判断更保守——知道这个差异。
2. **setEffectParams**：同时出现在被证伪段与 MED §1a。不矛盾——被推翻的是攻击叙事
   （「任意网站一个 GET 即可降级画质」），保留的是运维 footgun（主播自己敲 URL 探活）。

---

## 修复进度（截至 2026-07-16）

### 已修复并提交（分支 `fix/toolchain-gates`，未 push）

| 缺陷 | commit | 回归测试 | 变异自证 |
|---|---|---|---|
| park 的 NewCallback 配额泄漏 | `20edf97` + `ef3d11d` | `player/callback_lint_test.go` | 已做，覆盖三种漏报形状（包级 FuncLit / 别名 import / NewCallbackCDecl） |
| 四个 HTTP handler 锁外读 PlayerState | `0f83220` | `server/race_test.go` | 已做，回退旧写法必现撕裂 panic |
| QRC 解密失败被吞，密文推上 OBS | `b4d9fda` + `6446f82` | `player/qqmusic/api_test.go` | 已做，去掉两类失败的区分逻辑即红 |
| validateTimeAddr 用德摩根拒绝式致 NaN 通过 | `6667a01` + `b4f530e` | `player/wesing/lyric/timer_test.go` | 已做，12 个非 NaN 值上两种写法等价、唯 NaN 相反 |
| VerQueryValue 的 uintptr→Pointer 潜伏 UAF | `4044aff` + `0474158` | `player/qqmusic/mem_test.go`、`player/kugou/watchdog/watchdog_test.go` | 已做，注入 `&buf[off+4]` 必红 |
| kugou coverCancel 泄漏 | `ca253cd` | — | 由 vet 的 lostcancel 守；注意其盲区，见 AGENTS.md §3 |
| WS 写失败只 return 不关连接，订阅者僵死仍在册（HIGH #2） | `9cb446e` | `server/ws_zombie_test.go` | 已做，两方向：删 `conn.Close()` → 僵尸测试红；改无条件关闭 → 防过度测试红 |
| kugou 单点哨兵被 patch 自我覆盖，未知版本盲写（HIGH #1，判定层） | `1532052` | `player/kugou/watchdog/patchstatus_test.go` | 已做，退回单点哨兵 → 污染 P2–P9 全红；改错 orig 字节 → 真机锚定红 |
| kugou 选中旧版残留目录的 libcef.dll（HIGH #1，错配子问题） | `951a361` | `player/kugou/watchdog/findinstall_test.go` | 真机端到端：现选 20.1.22.27795，不再是 10.1.94.25498。**注**：951a361 修对了「按 exe 版本选文件」（保留），但同时删了 KuGou8——那半是过度修复，见下行 |
| kugou 删 KuGou8 注册表选址（951a361 的过度修复订正） | `01b058f` | `player/kugou/watchdog/findinstall_test.go`（TestKugou8LibcefIfDir） | 恢复 KuGou8 为首选 + IsDir/有 libcef.dll 守卫，findLibcefForExe 降兜底。真机确认走 KuGou8 选中 20.1.22.27795；变异去掉 Stat 检查即红。951a361 当时确实读到 KuGou8 指向 exe（真实观测，非假实测），但正确动作是守卫而非删除——KuGou8 大多数时候（含本机现在）就是权威答案。**这是本轮第二个 kugou 选址过度修复**（第一个是 10.x 黑名单 `0e129ae`），同源：都在 kugou 选址删了别人有意的东西 |
| kugou patch 无备份/无复检/%TEMP% 路径提权（HIGH #1，写盘层） | `7ab6ee6` | `player/kugou/watchdog/patchwrite_test.go` | 真机端到端（真实 DLL 副本）：patch→自校验→删备份→幂等；破坏字节→复检拒绝且不盲写 |
| kugou 10.x 黑名单被 `1532052` 误删（**过度修复的订正**） | `0e129ae` | `player/kugou/watchdog/blacklist_test.go` | 边界用例（100.x 不得被误判为 10.x） |
| 封面异步回写无代次守卫——换歌时（HIGH #3 场景1，三家） | `18f6cfd` | `player/songgen_test.go` | 已做，两方向：删代次比较 / 无条件丢弃 各红一测。~~**player 包 Linux 可编译 → CI 真门禁**~~ ← **这句是错的，见本表下方「关于『CI 门禁』的订正」** |
| 封面 goroutine 会话结束不作废代次，复活已清空 SongInfo（HIGH #3 场景2，cloudmusic/qqmusic） | `eb47c86` | `player/songgen_test.go` | 已做。`18f6cfd` 只作废换歌时的在飞回写，本 commit 加 `InvalidateSongGen` 补会话结束半边。变异：空实现即红。**wesing 半边 + wesing handle use-after-close 按「wesing 先别管」暂缓，仍开着** |
| kugou UAC 被拒后无退避（HIGH #4，安全桌面每 2s 夺焦） | `2f790be` | `player/kugou/elevation_test.go` | 已做，两方向：去掉上限 / reset 不清零 各红一测 |
| kugou 封面 b64 失败吞掉整首歌 song_info（HIGH #5） | `1c518ed` | `player/kugou/cover_test.go` | 已做。**变异测试抓出了我自己写的装饰性测试**（cancel 用例在调用前 cancel，顶部 select 随机命中即 return，删掉要守的检查照样绿），已改用可阻塞 httptest 钉死时序 |
| qqmusic songMid 未就绪致上一首歌词滚新歌（HIGH #6） | `cb9810e` + `b445269` | `player/qqmusic/songmid_test.go`、`midwait_test.go` | 已做。**返工**：`cb9810e` 的无界 continue 在 songMid 持久为空时（本地文件播放）让整首歌一个事件都不发——MED §8b 早已警告过该修法，我当时没读 MED 段。`b445269` 改为有界等待窗 |
| cloudmusic cdpLyricsOK 早于 ParseLRC 置真（HIGH #7） | `0ee8f6f` | `player/cloudmusic/cdplyrics_test.go` | 已做，两方向。~~**cloudmusic 包 Linux 可编译 → CI 真门禁**~~ ← **这句是错的，见本表下方「关于『CI 门禁』的订正」**（且 cloudmusic 包 `GOOS=linux go vet` 实为 **FAIL**，本身也标错了——可编译的是 `cloudmusic/lyric` 子包） |
| cloudmusic 取词 API 无 HTTP 超时（HIGH #8） | `71fe2f1` | `player/cloudmusic/lyric/httptimeout_test.go` | cloudmusic/lyric/fetch.go 三处改用带 10s 超时的 httpClient |
| qqmusic 取词/封面 API 无 HTTP 超时（HIGH #8 补完 / MED §2） | `a4b5319` | `player/qqmusic/httptimeout_test.go` | 已做，`api.go` 四处远程调用（`:149/:222/:443` 裸 http.Client + `:374` http.Get）改用带超时的 httpClient。变异两方向：Timeout=0 / 改回裸 client 各红一测。**这是可达性最高的那组**——poll 循环内同步调用，卡死则 router 永久幽灵。qqmusic 包 CI 编译不了，本机门禁 |

附带产出：`.gitattributes`（全局 LF，修复前 gofmt 假阳性率 88%）、AOB 滑块探针关停（#39）、
四道门禁（gofmt / build / vet / test）首次全绿。

### 关于「CI 门禁」的订正（2026-07-16，横切上表）

**上表里凡写「→ CI 真门禁」的都是错的，本文其余各处的「【CI 可测】」也容易被同样误读。
把这条读进去再看上表：**

**CI 从不跑测试。** 实测四个 workflow 全文，`go test` 零命中：`build-windows.yml` 是
`push: main` 触发、`GOOS=windows`、self-hosted 的**纯构建**；release/sync 触发于 tag；
purge 触发于 release 事件。**无任何 `pull_request` 触发器、无质量门禁。**

即：**本仓库当前没有任何一条测试是 CI 门禁，全部是本机门禁**，靠人在本地跑。

错在哪：AGENTS.md **§8**「写完跑什么」里的「CI 的真实能力」那张表列的是**哪些包 `GOOS=linux go vet` 通得过**，
语义是「CI 若接上门禁，能覆盖的范围」——**能测 ≠ 在测**。我把前者读成了后者。AGENTS.md
自己在同节末尾已经写明「当前 CI 无任何 pull_request 触发器、无质量门禁……全靠人在本地跑」，
是我没读到那句就先用了上面那张表。

顺带纠出一处事实错误：上表 HIGH #7 那行写「cloudmusic 包 Linux 可编译」——**假的**。实测
`GOOS=linux go vet ./player/cloudmusic` **FAIL**（可编译的是 `./player/cloudmusic/lyric`
子包）。即那一行是双重错误：可编译性标错 + 门禁性质标错。

**本文「【CI 可测】」的准确含义**：该条**能**写成自动化测试且落在 Linux 可编译的包里
（= 将来接上 CI 门禁时能被覆盖），**不是**「CI 已经在跑它」。

这条缺口本身是已知的、且属原始需求 #2（「补完整的改后预运行流程」）的未完成部分，
不在 MED 清单内，别混进来当缺陷修。

**返工记录**：上表中带两个 commit 的条目，第二个是返工——首次修复经对抗性复查发现问题
（1 个回归：QRC 硬失败把明文歌词变成无歌词；2 个物理上不可能失败的测试；lint 漏报了自己
要堵的形状）。教训已写入 AGENTS.md §9 与 §11。

**审计报告本身的两处错误判断**（均因只读 commit 标题、未读 body / 未走通控制流，已就地订正）：
1. HIGH #5 称 800ms「被一个 chore 提交砍掉」（暗示疏忽）。实际 `d448816` 的 body 明写
   `perf(kugou): reduce cover API fallback timeout to 800ms`，是用户实测调优。真正的缺陷是
   「为 b64 失败而 return 吞掉歌曲信息」，800ms 只是提高了触发概率。保留 800ms，修 return。
2. HIGH #1 的修复 `1532052` 误删了 `80e9182` 有意加的 10.x 黑名单，commit message 错称它
   「不设防」——实则它 `return`，我把它和下面不 return 的 `else` 搞混了。经 git 溯源恢复
   （`0e129ae`）。教训：删/覆盖既有代码前先 `git log -S` 读全 commit info，新增零风险、
   删除才会撤销他人有意设计。

**每条 HIGH 修复都做了变异自证**（隔离 worktree 注入变异、确认测试变红），且尽量做双向
（僵尸测试 + 防过度修复测试）。kugou 判定/写盘层另做了真机端到端（真实 139MB DLL 副本，
绝不碰真实安装）。诚实标注了测试盲区：kugou 包 CI 的 Linux runner 编译不了（依赖
x/sys/windows/registry），本机是唯一门禁；qqmusic/cloudmusic 的 runSession 轮询循环依赖
真实进程内存，只测得了抽出的纯函数，循环本身靠代码审查。

### 未修复 —— 下一步的工作面

**HIGH 全部修完（8/8）+ 1 条已决定接受。** 下表保留每条的落地记录：

| # | 缺陷 | 状态 |
|---|---|---|
| ~~1~~ | ~~`kugou/watchdog` 的 `canAutoFix` 被空白标识符丢弃~~ | ✅ `1532052`+`951a361`+`7ab6ee6`+issue #41。**实际范围远超原判断**：证伪推翻「接住 canAutoFix 即足够」（3/3），暴露闸门装在进程边界错误一侧、跨 UAC 的 TOCTOU、`--kugou-patch` 孤儿写原语、`%TEMP%` 路径本地提权。钥匙从版本号改为 9 处 orig 字节指纹。另 `0e129ae` 恢复被 `1532052` 误删的 10.x 黑名单（自查发现的过度修复，见「返工记录」） |
| ~~2~~ | ~~WS 写 goroutine 遇错即 return 但不关连接~~ | ✅ `9cb446e`。NaN 因果链已证伪（写超时同样触发），要害是「写失败不蕴含连接已断」 |
| 3 | 封面 goroutine 无代次守卫 | 🔶 **换歌半边** ✅ `18f6cfd`（BaseEmitter 代次计数 + EmitForGen，两方向变异）；**会话结束半边** ✅ `eb47c86`（cloudmusic/qqmusic）。18f6cfd 只作废了「换歌」时的在飞回写，漏了「会话结束/进程退出」——迟到封面会在 ClearSongData 之后复活已清空的 SongInfo，之后每个新连接经 buildInitEvents 都拿到复活数据（HIGH #3「场景2」）。`eb47c86` 加 InvalidateSongGen 补上。**wesing 那半按「wesing 先别管」暂缓，仍开着**；wesing handle use-after-close（复查另发现）一并暂缓 |
| ~~4~~ | ~~kugou UAC 被拒后无退避、无上限~~ | ✅ `2f790be`。elevationGate 退避 30s→2min，用尽终态放弃；提权成功 reset |
| ~~5~~ | ~~kugou 封面超时即整首歌不发 song_info_update~~ | ✅ `1c518ed`。拿到 URL 后无条件发（b64 空也发）。**审计对 800ms 的指控是错的**：`d448816` body 写着 `perf(kugou)`，是用户实测调优，非疏忽——见「返工记录」 |
| ~~6~~ | ~~qqmusic v20.05 songMid 未就绪~~ | ✅ `cb9810e`。songMid 守卫提到 lastName 认领之前，未就绪清 currentLyrics + continue 不认领，下次重试 |
| ~~7~~ | ~~cloudmusic `cdpLyricsOK` 早于 ParseLRC 置真~~ | ✅ `0ee8f6f`。抽 resolveCDPLyrics，只在解析出非空行才置真，0 行交给 Redux fallback |
| ~~8~~ | ~~取词 API 无 HTTP 超时~~ | ✅ `71fe2f1`（cloudmusic/lyric 三处）+ `a4b5319`（qqmusic/api.go 四处，可达性最高的那组）。取词/封面 6 处全部改用带 10s 超时的 httpClient。剩 effect.go:509 / cdp/client.go:244 两处 localhost CDP 探测未纳入（非取词、localhost 挂起概率低，MED §2 原判缓修）。**订正 `71fe2f1` 当时的判断**：残留面不止「localhost 两处」，qqmusic 那四处才是可达性最高的，当时以「scope 蔓延」推掉判反了优先级——现已补完 |
| — | effect-ingest 无认证 | **已决定接受现状**，不在待修之列。边界与约束见 AGENTS.md §14——特别注意「不要去掉 CheckOrigin」 |

**MED 20 条 / LOW 11 条仍未处理**（见下文各节）；**未判决 ~~15~~ 实为 3 条，需先补做证伪才能动**。这两批是下一步的工作面。

**MED 20 条 / LOW 11 条**：见下文各节。

**未判决 ~~15~~ 实为 3 条**（见上方专段，内容已考古恢复）：证伪 agent 因 API 错误中途死亡，既未证实也未推翻。**绝不混进 HIGH/MED
清单**——实测约半数原始发现经不起对抗性证伪。要用先补做证伪。

### 与其他文档的关系

- **硬规则已全部迁入 AGENTS.md**（§0 / §3 / §9 / §11），本文不复述。两份真源必然分叉。
- issue 分级见 `doc/issue-triage-2026-07.md`（**快照**，以 issue tracker 为准）。
- 本文的数据溯源说明见「总评」末段：实质内容可信，精确计数不可引用。

---

## HIGH —— 发版前必修

> 校正说明：审计数据中 `watchdog.go` 行号整体偏移 +7、`wesing.go` 偏移 +5（切片时的树与当前 HEAD 不同）。下文全部使用**我在当前 HEAD 上实测确认的行号**。其余引用逐字属实。

### 1. `canAutoFix` 被两个调用点全部丢弃：版本漂移后照样盲打 9 处补丁，且盲写会抹掉版本哨兵本身，损坏从此不可检测

**位置** `player/kugou/watchdog/watchdog.go:583`（`allPatched, _, err := CheckPatchStatus(libcefPath)`）、`:617`（`verified, _, verifyErr := ...`）

**失败场景**
酷狗自动更新（`knownVersion` 钉死 `20.1.22.27795` 这一个 build，漂移是必然不是偶然）→ 偏移失效 → `0x58C63EB` 处既非 `p1Orig` 也非 `p1Patched` → `CheckPatchStatus` 落 `:104-107` 的 default 分支返回 `(false, false, nil)`。**err 是 nil**，不触发 `:584` 早退；`canAutoFix` 被丢进 `_` → `allPatched=false` → 直落 `:598` 弹 UAC → 向 88.8MB 的未知 DLL 的 9 个失效偏移盲写。无 `.bak`、无写前校验、无 unpatch（`player/kugou/` 全目录确认）。

第二级后果是我逐字节复核确认的、比原报告更硬的部分：`libcefPatches[0].data` 与 `p1Patched` **完全相同**（均为 `{0xC7,0x06,0xC9,0x2F,0x00,0x00}`，watchdog.go:45 vs :62）。所以 Patch1 一写完，哨兵就变成「版本已确认」。于是 `:617` 的写后校验重读的正是自己刚写的字节 —— `verified` 恒为 true，`:621` 的 `if !verified` 对错版本损坏**结构性不可能触发**（它只抓得到杀软静默回滚，正如 `:622` 文案所说）。损坏被记为 patch 成功，重启酷狗 → CDP 永不起来 → `waitForCDP` 空等 → 2s 重来，永久循环。用户只能重装酷狗。

`:78-80` 的文档注释声明了 `canAutoFix` 的契约，`:106` 自己写着 "cannot safely patch" —— 代码先证明了目标指令不在那里，再因为调用方把证明丢弃而照写不误。这是矛盾，不是设计（`80e9182` 只孤立地加了 10.x 前缀守卫，是被烧到后的症状级补丁，20.2.x 会掉进同一个洞）。

**修法**
`EnsurePatched:583` 接住返回值：`allPatched, canAutoFix, err := ...`；`if !allPatched && !canAutoFix { return fmt.Errorf("libcef.dll 版本不受支持，拒绝盲打补丁") }`，并让 `kugou.go:63` 把它与 `ErrInstallNotFound` 同级处理（return，不重试）。**另需把哨兵与 patch 目标解耦** —— 改用一处补丁不覆盖的字节做指纹，或直接校验 `libcef.dll` 的 FileVersion/SHA256，否则打完补丁就永远认不出版本。

**验证** 【CI 跑不了 —— Windows-only 包】`GOOS=linux go vet ./player/kugou` 实测 FAIL（经 watchdog 依赖 `golang.org/x/sys/windows/registry`）。但 `CheckPatchStatus` 是纯文件 IO，**可以**抽成一个吃 `io.ReaderAt` 的纯函数并在 CI 上用合成字节流覆盖三条分支（orig / patched / unknown）—— 这是本条唯一能自动化的部分，值得做。端到端（UAC、真 DLL、真酷狗）只能真机，且需先备份 libcef.dll。

---

### 2. effect-ingest 无认证 + `CheckOrigin` 恒 true + 绑 0.0.0.0：任意网页或局域网设备可向直播画面注入任意图像

**位置** `server/server.go:104`（`CheckOrigin: func(r *http.Request) bool { return true }`）、`server/server.go:431`（路由注册）、`server/effect.go:256-279`（handler）、`config/config.go:62`（`Addr: "0.0.0.0:8765"`）

**失败场景**
主播机浏览器打开任意网页（含广告 iframe）→ 页面 JS `new WebSocket('ws://127.0.0.1:8765/cloudmusicv3/effect-ingest')` 并 send 任意 JPEG。WebSocket 不受同源策略约束，`CheckOrigin` 恒 true 移除了唯一防线，握手必成。帧经 `effect.go:264-278`（除 `mt != BinaryMessage || len==0` 外零校验，直接 `fn(data)`）→ `ingestFrame` → `BroadcastEffectFrame` → 所有 OBS 订阅者。门控只有 `isShowing() && !rawMinimized && 过 gate` —— 即**主播正常放特效的整个窗口，门都是开的**。绑 0.0.0.0 使局域网任意设备无需浏览器直连。

两处我复核时补到的加重项：
- 端点被**无认证地主动广告出去** —— `main.go:114` 把 `effect-ingest` 地址塞进 `/service-status`，攻击者一个 GET 就拿到写入端点，零侦察成本。
- 不是「与真帧交错」而是**压制**：`BroadcastEffectFrame` 是「满则丢旧、写入最新」，高频灌帧基本占满通道。

`effect.go:255` 注释「单一生产者（注入脚本只开一条），故不做并发保护」—— ingest 的全部安全性挂在这句注释上，代码里零强制。git 史显示这是**继承**不是决策：`CheckOrigin=true` 出自 `e984873` 初次提交，早于 ingest 端点几个月；`0785d51` 引入 ingest 时直接复用了 `s.upgrader`，commit message 只字未提信任边界。宽松 origin 是为**只读广播**端点设的，一个**写入**端点静默继承了它。

**修法（关键约束：不要照着「去掉 CheckOrigin」改，会炸）**
注入脚本跑在 `orpheus://orpheus/pub/app.html`，恢复 gorilla 默认 `checkSameOrigin` 会比较 `u.Host("orpheus")` vs `r.Host("127.0.0.1:8765")` → 403 → ingest 直接断，OBS 浏览器源也可能一起断。**这条 CheckOrigin 对两条正路都是承重的。**
零风险修法只动 `handleEffectIngest`：
1. 专用 upgrader，校验 `r.RemoteAddr` 为回环 —— `effect.go:251` 里 `EffectIngestWSURL` 硬编码 `ws://127.0.0.1:`，注入脚本永远从回环连入，此校验不破坏任何现有行为，直接消灭局域网向量；
2. 本机浏览器向量需 nonce：URL 由 `EffectIngestWSURL()` 生成、经 `effect/effect.go:600` 的 `fmt.Sprintf` 嵌进注入脚本，加个 per-run 随机串比对即可，成本近零；
3. `effectHub` 加单生产者互斥，把注释里的假设变成强制；
4. 默认 `Addr` 改 `127.0.0.1:8765`。

**验证** 【CI 可测】`GOOS=linux go vet ./server` 实测 OK。用 `httptest.NewServer` + `gorilla/websocket.Dialer` 写回归测试：非回环 RemoteAddr 应被拒、错误/缺失 nonce 应被拒、第二条 ingest 连接应被拒。这是 9 条里自动化性价比最高的一条。

---

### 3. WS 写 goroutine 遇错即 return 但不关连接，读循环无 deadline → 订阅者永久僵死却仍在册，此后所有事件静默丢弃

**位置** `server/server.go:486-494`（写 goroutine）、`:497-501`（读循环）、`:503-507`（清理）

**失败场景**
OBS 浏览器源冻结 >5s（场景集加载 / CEF GC / 局域网 OBS 的 Wi-Fi 抖动 —— bind 是 0.0.0.0，远端 OBS 是支持场景）→ 正好推一条 all_lyrics（逐字歌词几百 KB）→ 内核发送缓冲填满 → `WriteJSON` 撞上 `:489` 的 5s deadline 返回 error → 写 goroutine 在 `:491` **只 return**，defer 仅 `close(done)`，既不 `conn.Close()` 也不 unsubscribe。读循环 `:498` 阻塞在 `ReadMessage()`，全程无 `SetReadDeadline`，对端 CEF 只是卡住不会发 close frame。于是连接永久保持、sub 永久留在 `s.subscribers`、`sub.ch` 填满 64 格后 `NotifySubscribers` 的 `select{default:}` 对它永远命中 default —— 所有 lyric_update/song_info/player_switch 静默丢弃。OBS 源解冻后 JS 看到的仍是一条 open 的 socket，没有 onclose、不会重连（`lyric_page.html:1735-1739` 的重连只挂在 onclose，无消息空闲看门狗）→ 歌词 overlay 从此定格，必须手动刷新 OBS 源。`/service-status` 的 `wsCount()`/`ClientAddrs()` 还把它算作在线 —— **运维看到绿灯**。

三条反驳路径我都试过并被真实代码挡回：
- 「gorilla 会自己关」不成立 —— 读了 modcache 里 `websocket@v1.5.3` 源码，`conn.go:360` `writeFatal` 只把 err 记进 `c.writeErr`，不碰 `c.conn`；`Close()` 必须由应用显式调用；读路径用独立的 `readErr`。写失败确实不会唤醒阻塞中的 `ReadMessage`。
- 「有心跳收尸」不成立 —— 全仓 grep 无任何 `SetPongHandler`/`PingMessage`/`WriteControl`，server/ 下无 `SetReadDeadline`。TCP keepalive 也救不了：CEF 卡住时内核仍正常回 ACK。
- 「是故意的」无证据 —— `:503` 的注释「清理：停止接收 → 关闭通道 → 关闭连接 → 等待写入完成」恰恰证明作者心智模型是「读循环是唯一出口」，而写侧 return 违反了这个前提。`effect.go:336-344` 是同一 pattern 的复制，属一致的疏漏。

**这条明确推翻架构地图 §7.8**：「WS writes carry a 5s deadline 所以 hard-hung peer self-evicts」是错的。deadline 只杀死了写 goroutine，没有杀死连接；hard-hung peer 恰恰不自我驱逐，反而变成永久在册、永久收不到数据、还被计入在线数的僵尸。请这一轮改掉地图这条。

**补充（原报告低估了自己的触发面）**：`:479-482` 的 `buildInitEvents` 在**新连接建立时**就写 all_lyrics 且**完全丢弃返回的 error**。由于 `beginMessage` 会 latch 住 `writeErr` 并在后续每次写时重新返回，这里一旦超时，写 goroutine 的第一次 `WriteJSON` 即刻失败 → **连接一出生就是僵尸**。而这条路径恰好命中 CEF 最容易卡的时刻（场景集加载、浏览器源刚启动）。

**修法**
写 goroutine 改为 `defer func(){ conn.Close(); close(done) }()`。关闭连接会让 `:498` 的 `ReadMessage` 立即出错返回 → 读循环 break → 既有的 `:504-507` 清理路径自然跑完（`close(sub.ch)` 时写 goroutine 已退出、range 早已结束，无 send-on-closed 风险；`<-done` 立即返回）。同处给读循环加 `SetReadDeadline` + Pong handler 保活，覆盖对端半开。`:481` 的裸 `conn.WriteJSON(evt)` 也要接住 error。`effect.go:336-344` 同步修。

**验证** 【CI 可测】`server` 包 Linux 可编译，且 Linux runner 上 gcc 现成 → `go test -race ./server` 可行。测法：起 httptest WS server，客户端连上后**不读**（不是断开），灌一条大 payload 直到写超时，断言 `wsCount()` 在超时后归零、`sub.ch` 被关闭。这条能写出确定性单测，不需要真 OBS。

---

### 4. 封面 goroutine 无取消/无代次守卫：迟到的 `song_info_update` 用上一首的标题+封面覆盖当前歌曲，且不会自愈（wesing / cloudmusic / qqmusic 三处）

**位置** `player/wesing/wesing.go:209`（+`:227` emit）、`player/cloudmusic/cloudmusic.go:283`（+`:285`）、`player/qqmusic/qqmusic.go:202`（+`:207`）。承接侧无防护：`server/server.go:337-343`

**失败场景**
歌曲 A 播放中，封面 goroutine 起飞（wesing 最长 5×1s 重试 + 5s HTTP ≈ 10s；cloudmusic/qqmusic 为 5s HTTP）。主播在窗口内切到 B → 主循环发出 B 的 song_info_update（wesing.go:194 / cloudmusic.go:277 / qqmusic.go:152）→ 随后 A 的 goroutine 下载完成，发出携带 A 的 Name/Singer/Title/Cover 的 song_info_update。`UpdatePlayerState` 无条件覆盖 `ps.SongInfo`，无版本/时序比对。

**关键放大器**：song_info_update 每首歌只在切歌时发一次（全仓仅 8 个 emit 点，我已 grep 确认），所以 overlay 会在**整首 B 的时长里**持续显示 A 的歌名和封面，直到下次切歌。

三条我试过并失败的证伪：
- 「有上游守卫」—— 不存在。我专门去查了 `server/dedup.go`（原报告没提，我本指望它是守卫），结果**反向支持**：`dedup.go:17` 注释明说异步封面补发 hash 不同「不会被 switchSkip 抑制」。链路上唯一的 dedup 是设计来放行这条迟到 emit 的。
- 「前端会自愈」—— 不会，且比报告说的更糟。`lyric_page.html:1797-1798` 显示 titleMode 完全忽略 all_lyrics（「title 模式仅由 song_info_update 驱动」），无任何纠正信号。非 titleMode 下 all_lyrics(B) 在 `:1805-1807` 设标题，但它**早于**迟到的 A emit，随后被 `:1756` 覆盖回去、封面被 `:1758` 覆盖。
- 「窗口不可达」—— 这是我最指望能证伪的一条，输了。`wesing.go:211` 注释明写重试路径是**预期情况**（「K歌客户端可能延迟加载封面数据」），所以 ~10s 的 goroutine 生命期是 wesing 的**常规路径**而非边角。触发只需主播试听/切歌。具体：A 于 t0 开始，t0+2s 切歌，B 的 AOB 扫描于 ~t0+4s emit，A 的 goroutine 于 t0+3s 找到 URL、t0+7s 下载完 → 覆盖 B 的整首时长。

**对照组证明这是遗漏**：`kugou.go:226-229` 恰好实现了正确模式（`if coverCancel != nil { coverCancel() }` + 四处 `ctx.Done()` 检查后才 Emit）。诚实修正一点：`git show 0a430db` 显示 kugou 的 ctx 是因为新设计要阻塞等待迟到 URL、**结构上必须**可取消才引入的，不是「作者想到了 stale emit」；但 Emit 前那个 `select { case <-ctx.Done(): return; default: }` 无歧义就是 stale-emit 守卫，结论仍成立。

场景 2（进程退出后复活已清空的 SongInfo）需要一处修正：`wesing.go:50-51` 在 `runSession` 返回后 ~2s 会发第**二**条 `EventClearSongData`，故 2s 内落地的 emit 会被清掉；只有 >2s 后落地的才永久黏住 —— 在 5s/10s 预算下仍然可达。

**修法**
两条路，推荐后者：
1. 把 kugou 模式推广到三处（切歌时 `coverCancel()` 再起新 goroutine，Emit 前查 `ctx.Done()`）；
2. **更稳且更省事**：在 `BaseEmitter` 之上加代次守卫 —— 切歌时 `atomic.AddUint64(&p.songGen,1)`，goroutine 捕获 gen，Emit 前比对不等则丢弃。同时把 `wesing.go:55` 的 `CloseProc` 纳入取消范围。
兜底可在 `UpdatePlayerState` 加「Title 与当前 `AllLyrics.Title` 不符则丢弃 song_info_update」。

**顺带发现的相邻 bug（同一根因，建议一并修）**：`wesing.go:55` 调 `proc.CloseProc(handle)` 时，封面 goroutine 可能在其后最长 5s 内仍调 `lyric.FindCoverURL(handle, mid)`（`wesing.go:215`）。Windows 会回收 handle 值，被回收的 handle 会静默 `ReadProcessMemory` 一个无关进程。走方案 2 时记得把 CloseProc 一起纳入。

**验证** 【CI 跑不了 —— Windows-only 包】`GOOS=linux go vet` 实测 `./player/wesing`、`./player/cloudmusic`、`./player/qqmusic` 全部 FAIL。**但方案 2 的代次守卫落在 `./player` 包（实测 vet OK）**，可以在 CI 上对 `BaseEmitter` 写纯单测：起 gen=1 的 emit、bump gen、断言旧 gen 的 Emit 被丢弃。这是选方案 2 而非方案 1 的一个额外理由。三个播放器各自的接线只能真机验证（切歌后 2s 内再切，看 overlay 是否回退）。

---

### 5. 用户拒绝 UAC 后无退避、无放弃、无上限：安全桌面提权弹窗每 ~2 秒刷一次，直播画面被反复夺焦变灰

**位置** `player/kugou/kugou.go:68`（`log.Warn` 后继续）、`:81`（唯一节流 `time.Sleep(2s)`）、`:114-116`（酷狗未运行则 waitForCDP 立即返回）、`watchdog.go:598-601`（UAC 失败只包成普通 error）

**失败场景**
libcef.dll 未 patch 且酷狗未运行 → `EnsurePatched` → `:598` `launchElevatedHelper` → UAC → 用户点「否」→ `:600` 返回 error → `kugou.go:63` 的 `errors.Is(ErrInstallNotFound)` 为 false（错误链是 `fmt.Errorf(...%w, syscall.Errno(1223))`）→ `:68` 只 log.Warn 落下 → `waitForCDP` → `cdp.Connect()` 失败 → `:114` `!IsKuGouRunning()` 为真 → **立即**返回 → `:81` sleep 2s → continue → 再弹 UAC。UAC 安全桌面会让整个屏幕变暗并夺焦 —— 直播中等同于事故。若酷狗正在运行，`waitForCDP` 走满 90s deadline，退化为每 ~92s 弹一次，整场直播不停。全程无重试计数、无 `ERROR_CANCELLED` 识别、无退避。

三条证伪尝试均失败：
- 「是故意的」—— `:69` 注释讲的是「等用户手动启动酷狗」，不覆盖 UAC 拒绝；且 `2a13ec3`「stop retry loop when installation not found」证明作者正在修同一类无界重试 bug，只补了 install-not-found 一个口子。属遗漏。
- 「没选 kugou 就不会启动」—— **反而更糟**：`main.go:184` `go kp.Start()` 是无条件的，只用网易云但装了酷狗的主播照样被弹。
- 「README 要求管理员运行则 runas 不弹窗」—— 唯一有实质力度的缓解，但不足以证伪：`README.md:174` 确实写了需管理员，然而 syso 内 grep 不到 `requestedExecutionLevel`，默认 asInvoker，纯文档建议，代码不检查自身提权态。

**比原报告更广**：由于第 1 条（`canAutoFix` 被丢弃），酷狗**每次自动更新后**都会重新落进「未 patch」分支 → 重新弹 UAC。不是只有首次运行会撞上。

两点措辞需修正（不影响成立）：(a)「最常见的开播前状态」夸大 —— patch 后持久生效；(b) 弹窗不是定时驱动 —— `info.fMask` 含 `SEE_MASK_NOASYNC`，`ShellExecuteExW` 阻塞至用户应答，实际是「每次拒绝后 2s 再弹」，同时只有一个窗。但用户一旦点「否」即**无退路**，除杀进程外无法摆脱。

**修法**
`launchElevatedHelper` 里检查 `GetLastError()==ERROR_CANCELLED(1223)` 并包成 `ErrUACDeclined` 哨兵；`kugou.go:63` 扩展为 `if errors.Is(err, watchdog.ErrInstallNotFound) || errors.Is(err, watchdog.ErrUACDeclined)` → 发 error 状态并 return（与「未找到安装」同级）。若坚持重试，至少指数退避 + N 次后停手。

**验证** 【CI 跑不了 —— Windows-only 包】`./player/kugou` vet FAIL。`ErrUACDeclined` 的错误链归类（`errors.Is` 能否穿过 `%w` 包住的 `syscall.Errno(1223)`）**可以**抽成纯函数在 CI 测。UAC 交互本身只能真机：装酷狗、还原未 patch 的 libcef.dll、拒绝 UAC、观察是否只弹一次并转入 error 状态。

---

### 6. KuGou 快路径缺少裸 SongInfo 兜底：封面下载超时即整首歌不发 `song_info_update`；且 b64 预算被一个 chore 提交从 3s 悄悄砍到 800ms

**位置** `player/kugou/kugou.go:306`（`if b64 == "" { return }`）、`:247`（`const totalBudget = 800 * time.Millisecond`）、`:260`（唯一兜底入口）

**失败场景**
KuGou 播放一首带封面的普通曲目（`info.Cover` 非空 = 绝大多数曲目）→ `:232-233` 把 `currentCover` **在 goroutine spawn（:245）之前**就预塞进容量 1 的 `coverCh` → `:252-257` 的 select 立刻命中 `case coverURL = <-ch`，此处 `time.Since(start)` ≈ 0 → `:260` 的 `if coverURL == ""` 为假，**跳过 `:267` 那个唯一的裸 SongInfo 兜底** → `:300` 算出 `b64Timeout ≈ 800ms` → 上行被推流占满或 CDN 抖动 → 下载 >800ms → `FetchCoverBase64` 返回 `""` → `:306-308` `return`。

**该曲目全程没有任何 song_info_update 被发出**。`server.go:339` 是 `ps.SongInfo` 的唯一写入点，只有 `EventClearSongData` 会清空它，而 KuGou 只在 `kugou.go:54`（进程未就绪）和 `:90`（会话结束）发该事件，**从不按曲发**。故会话内一次下载失败 → 上一首的 name/singer/cover 继续挂在 `/kugou/ws`、根订阅者、以及 `/song_info` HTTP 端点上，直到下一首成功为止。`all_lyrics` 只喂 `ps.Title`，于是 overlay 自相矛盾：歌词组件显示新标题，歌曲信息组件显示上一首的歌名/歌手/封面。无重试路径 —— `player/cover.go:18-27` 无内部重试；`kugou.go:361-369` 的「封面延迟到达」只往 `coverCh` 推 URL，但快路径 goroutine 已消费过 ch 且此后不再读。

**慢路径（200ms 内没拿到 URL）反而有 `:267` 兜底 —— 兜底只覆盖了罕见路径，常见路径裸奔。**

**四个播放器里 KuGou 是唯一异类**（我 grep 确认）：`cloudmusic.go:277`、`qqmusic.go:152`、`wesing.go:194` **全都**在 spawn 异步 goroutine 之前无条件同步发一条裸 SongInfo（带 Cover URL），由 goroutine 补发 base64；且三者的 `FetchCoverBase64` 超时**都是 5s**（cloudmusic.go:284 / qqmusic.go:206 / wesing.go:226）。`cloudmusic.go:276` 甚至直接写着注释「先发不带 base64 的 songinfo，异步下载封面后补发」。

**800ms 是非预期副作用，不是权衡**：`git log -L 245,250` 显示 `d448816`「chore: prepare v3.0.0-beta.11 release」把 `3 * time.Second` 改成 `800ms`，body 写的是 `perf(kugou): reduce cover API fallback timeout to 800ms` —— 作者本意是缩短 `:271` 那处「调 API 兜底前的 URL 等待」。但 **`totalBudget` 被重载了**，它同时是 `:300` 的 b64 下载预算，快路径下载超时被 3.75 倍收紧。`:237-244` 的注释块至今仍写着「3s 预算内 / 3s 总预算 / 3s 内取到 / 3s 超时」—— 注释没同步正是第二处用法被漏看的铁证。

**修法**（两处独立修）
1. 把 `:267` 的裸 SongInfo emit 提到 `:260` 的 if **之外**，切歌拿到 name/singer 后无条件先发一次（含已知 coverURL），与另外三个播放器对齐。这样 `:306` 的 return 只损失 base64，不损失整条歌曲信息。
2. 拆开被重载的 `totalBudget`：URL 等待预算与 b64 下载预算分开命名，b64 恢复 3s（或对齐其余三个播放器的 5s），并同步 `:237-244` 注释。`:306` 处加 `log.Warn` —— 这条失败目前在全仓库不产生任何日志。

**验证** 【CI 跑不了 —— Windows-only 包】`./player/kugou` vet FAIL。只能真机：跑 KuGou，用防火墙/限速把封面 CDN 打到 >800ms，断言 overlay 仍显示当前曲目的歌名歌手（封面可以缺）。修法 1 的正确性可以靠代码走查 + 与三个对照播放器的 diff 对齐来保障，不必强求自动化。

---

### 7. 轮询循环内同步调用网易云 API 且无任何 HTTP 超时，网络挂起时整个取词 goroutine 卡死，而 `IsConnected()` 仍谎报在线

**位置** `player/cloudmusic/lyric/fetch.go:62`（FetchSongDetail）、`:118`（FetchLyrics）、`:169`（SearchSongID）—— 三处均为裸 `http.DefaultClient.Do(req)`。调用点在轮询循环体内：`cloudmusic.go:255`、`cloudmusic.go:300`

**失败场景**
`http.DefaultClient` 的 `Timeout` 为零值。主播机在国内网络下 music.163.com 被劫持网关/挂死透明代理接管（完成 TLS + 正常回 keepalive + 响应永不返回）时，`Do()` 与随后的 `io.ReadAll(resp.Body)` 无限阻塞。`runSession` 的 for 循环就此停摆：不再 Extract、不再发 status/lyric/seek。

**静默失败链才是这条的要害**：`p.connected` 仅在 `runSession` 返回后才清零（`cloudmusic.go:130-132`），而特效捕获器门控在 `IsConnected`（`effect.go:394/:430`，`main.go:191` 接线）。于是**特效层继续正常镜像画面，主播看到画面还活着，误以为一切正常**，只有歌词永远停在切歌前那一句。全仓无任何 heartbeat/liveness/stall 看门狗（grep 全空）。恢复需重启进程。

我替这条补验了它自己没查的关键前提：全仓 grep `DefaultClient`/`DefaultTransport`，**无任何 init 期改写**，仅各调用点自建 client → `Timeout` 确为零值坐实，且 Go 的 `DefaultTransport` 从无 `ResponseHeaderTimeout`。

**三处需修正（均为外围，不影响成立）**
- 「Ctrl+C 也解不开」**错误**。`main.go:196-203` 的 signal handler 在独立 goroutine 里 `os.Exit(0)`，不等 StopCh，进程正常退出。
- 「Redux 未对齐是常态」**无依据**。重试逻辑存在只证明该情况被处理过，不证明高频。
- 「无限期」**范围被夸大**。`DefaultTransport` 的 30s dial timeout / 10s TLSHandshakeTimeout 已覆盖无 SYN-ACK 与 TLS 停顿；Dialer KeepAlive 30s 对真死对端约 40s 拆链。真正无界只在上述「ACK 但不响应」场景 —— 但退化情形（~40s 全冻结 + 特效仍活）在直播中同样是事故。

**这条反而应该扩大**：`player/qqmusic/api.go:149/:222/:443` 是同样的 `&http.Client{}` 无 Timeout。而 `player/kugou/lyric/lyric.go:25` 写的是 `var httpClient = &http.Client{Timeout: 8*time.Second}`、`kugou.go:530` 是 `kugouAPIClient = &http.Client{Timeout: 5*time.Second}`、`player/cover.go:23` 显式收 timeout 参数 —— 作者明知该模式，属遗漏，涉及两个包共 6 处。

**修法**
`fetch.go` 加包级 `var httpClient = &http.Client{Timeout: 5 * time.Second}`，替换 `:62/:118/:169`；`qqmusic/api.go:149/:222/:443` 同样处理。更稳的做法是给 `FetchLyrics`/`SearchSongID` 加 context 参数，由 `runSession` 传入绑 StopCh 的 ctx。

**验证** 【CI 可测（cloudmusic 部分）】`GOOS=linux go vet ./player/cloudmusic/lyric` 实测 OK —— 这是 9 条里唯一落在 CI 可测包内的播放器侧修复。测法：`httptest` 起一个「接受连接、写完 header 后永不写 body」的 handler，断言 `SearchSongID` 在 ~5s 内返回 error 而非挂死。qqmusic 那三处在 `./player/qqmusic`（vet FAIL），**CI 跑不了**，但如果把 client 抽到一个跨平台的小包里就能一起测。

**等级说明**：维持 HIGH 的依据是「静默失败 + 需重启恢复 + 涉及 6 处 + 修复成本近零」。若严格按触发场景标定，HIGH 依赖「ACK 但不响应」的对端 —— **标 MED 也是可辩护的**，我不掩饰这一点。

---

### 8. `cdpLyricsOK` 在 `ParseLRC` 之前就置 true：lrc 非空但解析出 0 行时，前端整首歌停留在上一首的歌词

**位置** `player/cloudmusic/cloudmusic.go:325`（`cdpLyricsOK = true` 紧接 `:326` 的 `parsed := lyric.ParseLRC(...)`，中间无 `len(parsed)` 检查）

**失败场景**
切歌 → `activeSongID` 拿到 → `FetchLyricsViaCDP` 返回 `d.lrc.lyric` 非空但无任何可解析的计时行 → `PureMusic`/`NoLyric` 均 false → 走 `:324` else 分支：`cdpLyricsOK` 先置 true，随后 `parsed` 为空 → `activeLyrics` 保持空 →
- `:350` 的 `len(activeLyrics) > 0` 门槛不成立 → 不发 all_lyrics；
- `:376` 的 Redux 兜底要求 `!cdpLyricsOK` → 封死；
- `:404` 的 3 秒超时清屏也要求 `!cdpLyricsOK` → 封死。

净结果：`:277` 已经把新歌标题推给前端，而 all_lyrics/lyric_update 仍是上一首的 → OBS overlay 显示「新歌名 + 上一首的歌词」，持续整首歌，**结构上不可能自愈**。`:329` 还会打出 `歌词加载完成(CDP): 0 行` 这条误导性的成功日志。

**非对称是决定性证据**：同一函数 `:300` 的 API 路径写的是 `err2 == nil && len(apiLyrics) > 0` 才置 `cdpLyricsOK`。`git blame` 显示 `:325` 出自 `5c4f8a6b`「[Fix]使用Redexid」—— **同一次提交**里 API 分支写了守卫、CDP 分支没写。无辩护性注释。且 `:152` 该标志自身声明为「CDP 已成功获取歌词，跳过 Redux fallback」，0 行场景恰恰违反其自述不变量。

我复核时把触发面查得比原报告**更宽**：`cdp/client.go:513` 只靠 JS 真值判断 `if (d.lrc && d.lrc.lyric)`，仅排除精确空串 → 纯空白 lrc（`"\n"`、`" "`）同样进 else 分支。`ParseLRC` 有三条丢行路径：`fetch.go:47` 的 `\[(\d+):(\d+)\.(\d+)\](.*)` **强制毫秒段**（我已直接读到该正则确认），加上 `fetch.go:471-473` 跳过 text 为空的行 → 纯 `[00:00.000]` 无文本的 lrc 也返回 0 行。`MergeTlyric`/`MergeYRC` 均按值接收 `[]LyricLine`，无法 append，救不回空 parsed。

前端确认一致：`lyric_page.html:1796-1826` 的 all_lyrics 是**唯一**重置 allLyrics/currentIndex 并清屏的处理器；`:1754` 的 song_info_update 在非 title 模式下只改 songTitle；`:1831-1832` 的空文本 lyric_update 被显式忽略（注释「保留当前封面/歌词状态」）。故 overlay 保留上一首的 allLyrics 并继续按旧锚点插值滚动。

**修法**
```go
parsed := lyric.ParseLRC(lrcResult.Lrc)
lyric.MergeTlyric(parsed, lrcResult.Tlyric)
lyric.MergeYRC(parsed, lrcResult.Yrc, offsetSec)
if len(parsed) == 0 {
    log.Warn("CDP 歌词解析为 0 行(ID=%s)，回退 Redux/超时清屏", activeSongID)
} else {
    cdpLyricsOK = true
    // ... append 到 activeLyrics
}
```
与 `:300` 的 API 路径写法保持一致。

**验证** 【混合】`ParseLRC` 侧在 `./player/cloudmusic/lyric`（vet **OK**）→ **CI 可测**：表驱动单测断言 `ParseLRC("[ti:x]\n[by:y]\n")==0`、`ParseLRC("[00:12]第一行")==0`（无毫秒）、`ParseLRC("[99:00.00]纯音乐，请欣赏")==1`（这条**能**正常解析，不是本 bug —— 别在修复时误伤）。但 `cdpLyricsOK` 这个状态机在 `./player/cloudmusic`（vet **FAIL**）→ **CI 跑不了**，只能真机或把该分支抽成纯函数。

**诚实缺口**：我未能证明网易云 `song/lyric/v1` 真实会返回「lrc 非空 + 零可解析行 + pureMusic/nolyric 均 false」的组合 —— 报告只在合成字符串上测过 `ParseLRC`，真实触发频率未知。这是**暴露面问题而非成立性问题**：代码层确无守卫，lrc 为用户投稿且零校验，一旦触发即静默、持续整首、不可自愈。修复是一行 if，不值得为频率争论而拖。

---

### 9. QQ音乐 v20.05：`songMid` 未就绪时 `currentLyrics` 不被重置，且因 `lastName` 已推进而**永不重试** —— 上一首的歌词在新歌上滚动

**位置** `player/qqmusic/qqmusic.go:134`（守卫分支只设 err）、`:117`（`lastName = meta.Name` 排在守卫之前）、`:80`（`currentLyrics` 声明在循环外）、`:288-293`（匹配循环无条件读它）

**失败场景**
v20.05（`mem.go` 中**唯一** `SongMidParamsOff != 0` 的版本，我已逐版本确认 21.81/22.16/22.22 均留零）下切歌：poll 命中 `meta.Name` 变化 → `:117` **立刻** `lastName = meta.Name` → `:132` 判定 `meta.SongMid == ""`（StreamURL/params 两路堆字符串尚未随切歌更新）→ `:133-134` 只 log.Warn + 设 err，**`currentLyrics` 未赋值，仍是上一首的行**（`:136` 的 else 分支才写它）。因为 `lastName` 已等于新歌名，`:116` 的切歌条件下次轮询不再成立 —— **`:133` 注释承诺的「等待下次轮询」永远不会发生**。

这半条结论纯静态可判、不依赖任何时序假设：grep `lastName` 全文件仅四处（`:79` 声明、`:116` 比较、`:117` 赋值、`:263` `!= ""`），会话内从不重置。

随后 `:156` 的 `err != nil` 分支 Emit 一个空 AllLyrics，但 `:118` 已把 `lastLineIdx` 重置为 -1，`:288-293` 的匹配循环继续在**旧歌的 currentLyrics** 上跑，`:304` 每 tick Emit `LyricUpdate{Text: 上一首的歌词行}`。

**后果比原报告更硬（我追到了消费端）**：`lyric_page.html:1828-1840` 的 lyric_update 分支走 `updateDisplay(data.data)`，**直接渲染 `data.data.text`**，不用 index 去 allLyrics 里取行。所以 `:162` 把 all_lyrics 清成 `[]` 并**挡不住**旧文本上屏（只会让行号显示成「第 N / 0 行」）。`server/dedup.go:32-40` 只对连续完全相同的 payload 去重，逐行不同的旧歌词不会被抑制。净结果：新歌播放 + 旧歌词滚动 + 封面空，直到下次切歌。

**非故意，有反证**：`git show db011f7`（「[fix]在20版本中 大多使用songid而不是mid」）显示守卫是**后加**在既有的 `lastName = meta.Name` 之上，作者没意识到重试永不触发。决定性佐证：这是**唯一**会留下陈旧 `currentLyrics` 的路径 —— `fetchLRC`/`fetchLRCBySongMid` 的所有错误返回都是 `nil, "", "", err`（api.go 多处），会把 `currentLyrics` 重置为 nil。「只有守卫路径不重置」这种不对称是疏漏特征，不是本仓库那种反直觉但正确的设计。

**修法**（两处都要改，只改一处不够）
1. `:132` 的 if 分支里显式 `currentLyrics = nil`；
2. **不要在 `:117` 就无条件推进 `lastName`** —— songMid 未就绪时应保持 `lastName` 不变（或引入 `pendingName`），让下次轮询真正重试。否则注释里承诺的重试语义就是假的。

**验证** 【CI 跑不了 —— Windows-only 包】`./player/qqmusic` vet FAIL（依赖 mem/AOB）。真机也难：需要装 v20.05 并在切歌瞬间命中「Name 已刷新但 `0x80`/`0xAC` 两路仍解不出 mid」的窗口。**建议改为断言式验证**：把切歌分支的状态转移抽成一个不碰内存的纯函数（吃 `meta` 出 `(shouldFetch, newLastName, resetLyrics)`），在 CI 上表驱动覆盖「mid 未就绪 → lastName 不推进 + currentLyrics 置空」。这比追一个真机时序窗口现实得多。

**等级说明**：这是 9 条里**范围最窄**的一条（只影响 v20.05 一个旧版本）。我保留一点：无法静态证明该窗口的**发生频率**（需挂真机 v20.05 抓 struct1 字段刷新时序）。但守卫本身 + 那句 `log.Warn("v20.05 songMid 未就绪")` 的存在，就是作者观测到过该状态的证据；且频率是等级问题不是成立性问题。补一个值得注意的推论：若该窗口期 URL 里残留的是**上一首**的 mid（非空），则走 else 分支拿旧 mid 请求，**同样出旧歌词** —— 这个窗口无论哪种落法都是错的，只是路径不同。故维持 HIGH。

---

**关于本节的覆盖诚实说明**：9 条里只有第 2、3 条（server 包）和第 7 条的 cloudmusic 部分（`player/cloudmusic/lyric`）能在现有 self-hosted Linux runner 上真正跑测试；第 3 条还能吃到 `go test -race ./server`（runner 上 gcc 现成，本机无 C 编译器跑不了）。第 1、4、5、6、9 条全部落在 `player/kugou`、`player/wesing`、`player/qqmusic`、`player/cloudmusic` —— 我实测 `GOOS=linux go vet` 均 FAIL，**CI 永远跑不了，不要在 PR 里假装有覆盖**。对这五条，可行的折中是把纯逻辑（`CheckPatchStatus` 的字节判定、`BaseEmitter` 的代次守卫、错误链归类、切歌状态转移）抽成不碰 Windows API 的纯函数放进可编译包 —— 这既是可测性收益，也恰好是第 4 条选方案 2、第 9 条选断言式验证的独立理由。

---

## MED —— 能便宜修就修

切片 20 条，复核后剔 1 条、半剔 1 条，实际待修 18 条。剔除说明放在最后。

---

### 1. 三个「移一行 / 加一行」的修法，建议合成一个 commit

这三条的共同点：机制已核实、修法不改任何正常路径的语义、无新增状态。

**1a. `server/effect.go:316` — setEffectParams 跑在 Upgrade 之前**

已核实：`s.setEffectParams(...)` 在 :316，`s.upgrader.Upgrade(w, r, nil)` 在 :318，:319-322 早退不回滚。effectHub 的 quality/scale 是**全局单例**，`h.gen++` 会被 `player/cloudmusic/effect/effect.go:619` 的 400ms 轮询捡到并热更 `__mbxCapCfg.q`。

失败场景（**不是攻击，是运维 footgun**）：主播在浏览器地址栏敲 `http://localhost:8765/cloudmusicv3/effect-ws?quality=60` 想探活 → 普通 GET 无升级头 → Upgrade 必败 → 但参数已经写进去了，直播特效画质当场改掉，且没有 per-connection 状态可供恢复，只能重启服务。

修法：把 :316 移到 :322 之后（最好并进 :327 登记 `h.subs[sub]` 的同一临界区）。

代价：**近乎零**。sub 本来就在 Upgrade 之后才注册（:327），捕获器只认 `HasEffectSubscribers()`，所以对正常 WS 路径完全等价。

> 溯源里两条别抄进 commit message：CORS 全开是红鲱鱼（副作用在请求抵达即发生，攻击者不需要读响应）；「参数棘轮」的举例自相矛盾（`effect_page.html:59` 只在 URL 显式带 quality 时才透传，所以举的双源例子实为「后连的赢+刷新即翻转」）。

> **2026-07-16 已修（`46386f3`）。上面的括号里那句是错的，且危险，别照做：**
>
> **⚠️「最好并进 `:327` 登记 `h.subs[sub]` 的同一临界区」会自死锁。** `setEffectParams` 内部自己
> 取 `h.mu`（`server/effect.go:135` Lock / `:145` Unlock），Go 的 `sync.Mutex` **不可重入**。在
> `:326-329` 的临界区内调用它 = handler goroutine 当场死锁，且是**持锁死锁**——它会一直抱着
> `h.mu`，连带把 `BroadcastEffectFrame`/`HasEffectSubscribers`/`EffectCaptureParams`/
> `BroadcastEffectStatus`/`handleEffectIngest` 全部堵死，**整个特效子系统连同已有订阅者一起冻死，
> 只能重启进程**。这比原缺陷严重得多。三个视角独立确认。原子性本就无收益，见下。
>
> **正确修法**：`:296-316` 整块（query 解析 + setEffectParams）移到 Upgrade 成功之后、
> `h.subs[sub]` 登记**之前**。两个位置都是硬约束：
>   - 在 Upgrade 之后 → 治本条（探活式 GET 不再留副作用）。
>   - 仍在 `h.subs` 登记之前 → 捕获器唯一的感知入口是 `HasEffectSubscribers()`，session 开头才读
>     `EffectCaptureParams` 并锁 `startGen`。排在登记前 → gen++ 严格 happens-before 订阅者可见，
>     **零窗口**，这已提供「原子」想要的全部保证；排在登记后 → 新开一个「捕获器以旧参数启动
>     session」的竞态，而 `:619` 的 400ms 热更只重写 quality/fps，header/footer 的注入只在会话
>     启动时做一次（本文 §LOW-B 已记录该缺口）→ 本次连接请求的 header/footer 静默失效。
>   - 整块搬而非只搬一行：`:298` 读默认值与 `:316` 写入之间有个既存小 TOCTOU，只搬 `:316` 会被
>     Upgrade 握手的 IO 耗时把它撑大。不是本次引入的，但没必要放大。
>
> **实测**（agent 真机跑的，两条路径都验证）：探活式 GET `?quality=60` —— 修前 `quality 95→60`、
> `scale 0.70→0.10`、gen `0→1`；修后 `95→95`、gen `0→0`。恶意网页 `new WebSocket(?quality=1)` ——
> 修前修后**逐字相同**（都是 `95→1`）。即被证伪段 #4 的判断成立：**这条修法攻击面一分不减**，
> commit message 绝不能写成安全修复。它治的是运维 footgun，不治攻击面（根因是 `server.go:104`
> CheckOrigin 恒 true，那是 AGENTS.md §14 有意接受的现状）。
>
> **一处范围细节**（agent 补的，值得知道）：AGENTS.md §14 明文接受的是 **effect-ingest（写入端点）**
> 无认证；**effect-ws 的 hub 级全局参数无客户端身份判定并没有单独立条接受**。严格说这一面是
> 「未记档接受」而非「已接受」。要真治得走 per-connection 参数或专用 upgrader，不在本条范围。
>
> **配套**：`server/effectparams_test.go`。其中钉「顺序」那条**必须用 AST 门禁**，别写成运行时的：
> 前身就是运行时版（拨号 → `waitFor` 等 `HasEffectSubscribers` → 断言参数已就位），**实测抓不住
> 「挪到 subs 登记之后」这个变异**——`waitFor` 10ms 轮询，而那两行只隔几微秒，够不着。那是装饰品。
> 该不变量本来就是源码顺序的不变量，钉源码顺序才确定。同类先例：`player/callback_lint_test.go`。
> 变异矩阵：原始缺陷 → `IgnoredWithoutUpgrade` + AST 双红；挪到 subs 后 → AST 红；删掉
> setEffectParams → `AppliedOnWSConnect` 红。

**1b. `server/effect.go:340` + `server/server.go:490` — 写 goroutine `return` 但不关连接**

已核实两处同构：写协程 `if err := conn.WriteMessage(...); err != nil { return }`，defer 只有 `close(done)`，`conn.Close()` 只在读循环 break 之后（effect.go:358 / server.go:506）。读循环两处均**无** SetReadDeadline、无 ping/pong。gorilla 的 writeFatal 只写 `c.writeErr`、不碰底层 net.Conn，所以写超时后 TCP 仍 established → 读循环永久阻塞 → sub 永留 subs 集合。

effect.go 这条是真正可达的那个（q95 JPEG，`server/effect.go:61` 纯层不缩放，一帧就能填满 socket 缓冲，5s deadline 打得到）；server.go 那条是 NaN 事件的汇点（见 §4）。

修法：写 goroutine 改 `defer func(){ conn.Close(); close(done) }()`，让读循环立刻出错、走完既有摘除路径。

代价：**一行 × 2**，复用现有清理路径。

> 后果别写过头：OBS 进程关掉 / 刷新 browser source 都会 FIN → 读循环出错 → sub 摘除 → `HasEffectSubscribers` 转 false → 兜底 unpark 恢复。实际后果是「特效镜像冻结到主播刷新源」，不是「网易云永久卡屏外」。且 `player/cloudmusic/effect/effect.go:324` 的「前台即 unpark」不依赖订阅者、仍然有效——三条救援废了一条，不是全废。

**1c. `player/cloudmusic/effect/effect.go:320` — park 条件不含订阅者检查**

已核实：`if parkAllowed && minimized && !fg && !park.IsParked() { c.doPark() }`，而 :282 的兜底是 `if !c.sink.HasEffectSubscribers() && park.IsParked() { c.doUnpark() }`。park 成功即 `parkedMem=true`（park.go:331）→ 下一 tick（80ms）兜底条件成立 → doUnpark → `park.go:364-366` 把 savedPlacement 的 `swShowMinimized` 改写成 `swShowNormal`。park 只在 minimized 时可达，所以 savedPlacement 必然是最小化态——链条闭合。

后果：strategy=park 的 Win10 机器上，无特效订阅者时网易云**无法被最小化**，每次点最小化都在 ~160ms 内自己弹回来。

修法：:320 条件前加 `c.sink.HasEffectSubscribers() &&`。park 的唯一目的就是给订阅者供帧，无人收看时本就不该 park。

代价：**一个 `&&`**。

> 两点修正：跨进程 `SetWindowPlacement(SW_SHOWNORMAL)` 受前台锁约束，「抢焦点」不保证——但窗口必然取消最小化并可见，这半边是承重的。可达面窄：默认 strategy 是 fadeout（config.go:67），且 main.go:81-84 在 Win11 强制降级，命中人群 = 显式选 park 的 Win10 主播。

> **2026-07-16 已修（`46386f3`，与 §1a 同 commit）。修法即原判，无调整。** 补三点：
>
> **(a) 考古：确认非有意设计。** 我动手前的怀疑是「`:275` 注释『始终运行（不随订阅者启停）』说明
> 作者想过订阅者，也许 `:320` 不查是有意的」——查了，结论相反：**那句注释是 `33585d1` 连同 `:282`
> 兜底一起新增的**，且下半句自陈动机「以便任何时候都能把遗留泊车的窗口救回」讲的是**循环生命周期
> 为何不随订阅者启停**（为了救援），不是 park 入口为何不查订阅者。引入 `:320` 的 `0b93e8e` 全 body
> 一字未提订阅者——当时也无 `:282`，无订阅者时 park 无害。6 小时后 `33585d1` 加了兜底却没碰 `:320`。
> **决定性反证**：若「无订阅者也要 park」是有意的，同 commit 的兜底会在 80ms 后无条件撤销它——
> 设计不会自相矛盾至此。加门控与 `33585d1` 的意图同向。
>
> **(b) 不丢任何 park 能力**：`parkAllowed` 只在 `minimized && !prevMin` 边沿 latch，所以「先最小化、
> 后接入订阅者」时它仍为 true，订阅者一接入下一 tick 即 park。且三条 unpark 出口（`:282`/`:316`/
> `:324`）一行未动——门控只收窄「进入 parked」的集合，不碰任何退出路径，故不可能「卡在屏外」。
> 加了它之后 `:282` 与 `:320` 的谓词**互斥**，同 tick 不可能都成立，震荡不可达（现状恰恰相反，
> 现状才是震荡源）。
>
> **(c) 测试缺口，诚实记录**：本条**无自动化测试**。`effect` 包 Linux 编译不了（经 park →
> `golang.org/x/sys/windows`），CI 跑不了；且 `windowStateLoop` 是无限循环 + Win32 调用，要测得把
> park 的 Win32 抽到接口 + 给 FrameSink 打桩——那是重构 park 包，成本远超「一个 `&&`」，且改的
> 正是「一错就窗口卡屏外」的路径。按「稳定 > 正确」判为不值。**若将来做 §1d（要碰 park 包），
> 在那一轮一起抽接口并补 table-driven**，覆盖「无订阅者+按钮最小化不 park」「park 中订阅者掉线 →
> 退出且保持最小化」「N tick 内 Park 调用 ≤1（不震荡）」——最后这条极易写成恒绿，必须变异自证。

---

### 2. HTTP 无超时：一个包级 var 覆盖七个点，本仓库最划算的一条

已核实全部调用点（**注意行号已随 QRC 修复漂移，审计数据里的 api.go:175 现在是 :180 附近**）：

| 位置 | 现状 |
|---|---|
| `player/qqmusic/api.go:149 / :222 / :443` | 裸 `&http.Client{}` |
| `player/qqmusic/api.go:374` | 裸 `http.Get` |
| `player/cloudmusic/lyric/fetch.go:62 / :118 / :169` | `http.DefaultClient` |
| `player/cloudmusic/effect/effect.go:509` | 裸 `http.Get("http://127.0.0.1:9222/json")` |

`http.DefaultTransport` 只有 DialContext 30s + TLSHandshakeTimeout 10s，**没有 ResponseHeaderTimeout**；`Client.Timeout` 零值。握手成功但对端不吐数据 → `io.ReadAll` 无界阻塞。

三条后果，可达性递减：

- **qqmusic**：`qqmusic.go:136` 在 poll 循环体内同步调 fetchLRC → 卡死即整个 qqmusic goroutine 再也回不到 StopCh 检查。更坏的是审计漏掉的那一层：`router.go:371/:378` 只对 loading 和 paused 做超时，**卡死的 playing 永不过期** → qqmusic 变成 activeNames 里的永久幽灵，每当真实播放器转 idle 就把 overlay 交还给那份冻结状态。
- **cloudmusic**：`cloudmusic.go:300` 的 `lyric.FetchLyrics` 也在 poll 循环里同步调（我复核时发现的，审计只标了「范围外线索」）——但只在 CDP 返回纯音乐的二次确认路径上，面窄。
- **effect.go:509**：卡在 doPark 内则 windowStateLoop 全冻。

修法：每个包一个 `var httpc = &http.Client{Timeout: 10 * time.Second}`（effect/cdp 那条用 2s）。

代价：**极低**，且这是回归仓库既有规范而非新增约定——`kugou.go:530`(5s)、`kugou/lyric/lyric.go:25`(8s)、`kugou/watchdog/watchdog.go:67`(2s)、`player/cover.go:23`、`main.go:238/342/530` 全都显式设了超时，qqmusic/cloudmusic 是漏网的。

> 两处叙事修正：(a)「TCP 黑洞 → 永久」不准，真黑洞会被 Windows keepalive 在 ~5 分钟内拆链；真正无界的是「中间设备照常 ACK 但不转发」/强制门户/HTTP2 流挂起。5 分钟冻结 overlay 仍是事故。(b) effect.go:509 那条的溯源引用了 74de387/546bf63 当证据，但那两个 commit 恰恰是**删掉** Page.reload 的——引用无效，别照抄。

---

### 3. 配置层两条，同一个函数里就能改完

**3a. `config/config.go:110` — config.yml 按 CWD 解析；:348 吞掉生成失败**

已核实：:110 是裸 `os.ReadFile("config.yml")`；:347-350 是 `if err := os.WriteFile(...); err == nil { log.Info(...) }`，**无 else**，写失败零输出；对照 `main.go:336-340` 用的是 `os.Executable()+filepath.Dir`。全仓 grep `os.Chdir|Getwd` 零命中，无人修正 CWD；`ensureCanonicalName()` 与 `restartSelf()` 用 exec.Command 且不设 cmd.Dir → 两条 re-exec 路径都继承坏 CWD。

最硬的证据是 **README.md:210 明写「自动加载同目录下的 config.yml」——代码与自家文档直接矛盾**。

修法：`base := filepath.Dir(os.Executable())`，路径统一为 `filepath.Join(base, "config.yml")`，保留 CWD 作兼容回退；给 generateDefaultConfig 补 else 打 Warn。

代价：**低**。`os.Executable`/`filepath` 是 stdlib，config 包不引入任何 Windows API，CI 的 Linux runner 上 genconfig 照跑。

> 后果被夸大两处：(a) 不是「纯粹的静默」——`main.go:70` 每次启动都打 `播放器: %s (offset=%dms poll=%dms)`，错的 200 在控制台可见。(b) 「整场偏 200~300ms」只是两个分支之一：若计划任务勾了「使用最高权限运行」，写 System32 会**成功**，此时 offset 反而是对的（模板值 500/400/430），代价变成「用户调过的 config.yml 被忽略 + System32 被污染」。

> **2026-07-16 已修（`1be577d`）。缺陷与方向都成立（README 是真源、代码是唯一偏离方），
> 但修法描述里最关键的那句是错的：**
>
> **(a)「保留 CWD 作兼容回退」是假承诺，别把它当兼容保证。** CI 打包必然在 exe 目录放一份
> config.yml（`release.yml` / `build-windows.yml` 的 `go run ./tools/genconfig
> "${PACKAGE_DIR}/config.yml"`）→ zip 用户的 exe 目录**永远**有 config.yml → **回退分支
> 对真实用户永不可达**。它既保护不了任何人，还恰好掩盖了唯一的真实回归：
>
> > 计划任务/注册表自启用户的 CWD 是 System32，当前代码在那儿生成并读取 config.yml，
> > 他一直编辑的是那份；改成 exe 目录优先后，**exe 目录里 CI 那份默认配置赢，他的全部
> > 自定义静默归零**。而 `config.go:115` 的 `log.Info("已加载 config.yml")` **不打路径**，
> > 用户和排障者都无从发现。
>
> **真正的兼容保护不是回退，是两条日志**（本次的承重部分）：(i) 加载时打**绝对路径**——
> 没有这一行，任何「读错了那一份」都无法诊断，这是最高性价比的一行；(ii) `CWD != exeDir`
> 且两处都有 config.yml 时 `log.Warn("发现两份…使用 X，已忽略 Y")`——精确命中上述人群，
> 把静默失效变成一行可见告警。回退本身只对「开发者 go build 到临时目录再从别处跑」有意义，
> 按此定位保留即可。
>
> **(b) `:594` 那条「权限」判断方向反了。** 写失败概率是**变低**不是变高：exe 目录可写
> **已是本程序的既有硬假设**——`ensureCanonicalName` 往 exe 目录 `os.Create`（main.go:638）、
> `performUpdateAll` 往 exeDir 写 `.new`/`.old` 并 rename 覆盖自身 exe（main.go:341-364）；
> exe 目录若不可写，自动更新整条链早就坏了。且分发是 zip 解压（非 MSI 装 Program Files）、
> zip 里已带 config.yml → 压根不触发生成。反而**当前的 CWD 写更危险**：提权计划任务下会往
> `C:\Windows\System32` 写 config.yml 污染系统目录，不提权则失败且无 else 零输出。
>
> **(c) re-exec 比笔记说的更糟。** `ensureCanonicalName()` 在 `main.go:50`、`config.Load()`
> 在 `main.go:55` —— **re-exec 发生在 config 加载之前**，子进程是带着坏 CWD 去 Load 的。
> 两个 `exec.Command`（main.go:513 / :647）都不设 `cmd.Dir`，Go 在 `Dir==""` 时子进程沿用
> 调用者 CWD，故坏 CWD 跨 re-exec 传染。全仓 `os.Chdir/Getwd` 仅测试文件命中，无人修正。
>
> **(d) 读写点是三处不是一处**：`:110`（读）、`:122`（生成后重读）、`:348`（写）必须**同改**，
> 且 `:122` 要读回刚写的那个 path，否则重蹈 `28b095c` 修过的「写了读不到」。**明确不动
> `tools/genconfig`**——它靠 argv 显式传路径，改了破 CI 打包。`os.Executable()` 返回 err 时
> 回退到 CWD 原行为，不 panic：配置读不到最多用默认值，让服务起不来是把小问题升级成事故。
>
> **配套**：`config/configpath_test.go`（`osExecutable`/`osGetwd` 抽成可注入 var —— 真实的
> exe 路径与 CWD 是进程级全局状态，测试里没法安全地改）。变异五方向全红：原始缺陷（裸
> config.yml 只看 CWD）→ 4 条红；CWD 优先 → `PrefersExeDir`；Executable 失败即 panic →
> `SurvivesExecutableError`；删掉 CWD 回退 → `FallsBackToCwd`；写入点回到裸 config.yml →
> `WritesToGivenPath`。注：config 包 Linux 可编译，但 **CI 不跑测试**（见「关于『CI 门禁』的
> 订正」），本条同样是本机门禁。

**3b. `config/config.go:211` — 钳制只作用于全局 Poll，per-player 覆盖直通**

已核实：:210-215 只钳 `cfg.Poll`；`GetPlayerPoll`(:82-87) 命中覆盖时原样返回 `*pc.Poll`；`main.go:168-171` 直接把它传进各 New()，中间无守卫。

失败场景不需要 `poll: 0` 这种极端值——**`wesing-poll: 5`（「想让歌词更跟手」这种合理动作）就够**：`wesing.go:315-318` 有个 `if pollMs < 1 { pollMs = 30 }`，但它**只写进一个本地 int 当除数用（:320 的 1000/pollMs、:372 的 3000/pollMs），从不回写 pollInterval`**，所以 `time.Sleep(pollInterval)` 照睡 5ms，而 `windowCheckInterval = 1000/30 = 33` → 每 165ms 就调一次 `CreateToolhelp32Snapshot`（全进程表枚举）+ `EnumWindows`。README.md:196-202 列的四个 per-player poll 全标「(沿用全局)」、完全没标范围——文档陷阱是真的。

修法：Load() 返回前遍历 `cfg.Players`，对非 nil 的 Poll 应用同一 [10,2000] 钳制，越界打 Warn。

代价：**一个 for 循环**，复用已有边界。四个播放器包里的自保逻辑变成冗余但无害，新增播放器不会再踩。

> 一处纠正：kugou 那半边说过头了。kugou 每圈都走 `client.GetPlayInfo()`（kugou.go:176），是阻塞式 CDP 往返，poll=0 只是按往返速率猛捶 KuGou 的 CEF，不会在用户态打满一核。真正凶的是 wesing。

> **2026-07-16 已修（`aa78297`）。结论对，但上面的论据基本全错，别拿它去说服谁：**
>
> **(a) 「`wesing-poll: 5` 就够」的算式自相矛盾。** poll=5 → `pollMs=5`，`wesing.go:326` 的守卫
> **不触发**（5 >= 1）→ `windowCheckInterval = 1000/5 = 200` → 窗口检查 = 200 × 5ms = **1000ms**，
> 不是 165ms。笔记同时假设守卫**触发**（才有 pollMs=30 → wci=33）又**不触发**（才有 5ms sleep）——
> 两者互斥。而且 `1000/pollMs` 是**自归一化**的：任何 poll >= 1，窗口检查恒为 ~1s（实测
> poll ∈ {1,5,10,30,50} 全是 1s）。poll=5 的真实代价只是 ReadProcessMemory 速率 6x
> （~200/s vs ~33/s），真实但温和，**没有快照风暴**。165ms 实际对应 poll=**-5**。
>
> **真正的最坏情况是 `poll <= 0`**：`time.Sleep(<=0)` 立即返回 → **无界忙等**，wesing 每 33 圈
> 自旋一次 `CreateToolhelp32Snapshot`（全进程表）+ `EnumWindows`。这才是快照风暴。修法该由它
> 来论证。另外 `wesing.go:326` 那个守卫的真实用途是**防 `1000/pollMs` 除零 panic**，根本不是
> poll 自保——笔记把它当自保读了。
>
> **(b) 「四个播放器的自保逻辑变成冗余但无害」——四条全部证伪，一个都别删：**
>
> | 播放器 | 实际 | 钳制后 |
> |---|---|---|
> | `wesing.go:326` | `pollMs<1→30`，**只写本地**、供 `1000/pollMs` 用 → 是**除零守卫**非自保 | 不可达但**必须保留**（`pollLyrics` 是包内函数，纵深） |
> | `kugou.go:295` | **完全没有自保**，pollMs 直通 `:360`/`:557` 的 `time.Sleep` | 钳制是它**唯一**保护 → **承重，非冗余** |
> | `qqmusic.go:110` | `<30ms→50ms`，**回写** | poll ∈ [10,30) 仍触发 → **非冗余** |
> | `cloudmusic.go:157` | `<50ms→100ms`，**回写**，喂 `NewTicker` | poll ∈ [10,50) 仍触发 → **非冗余**，且兼防 `NewTicker(0)` panic（实测） |
>
> **(c) 本修法反转了 AGENTS.md 的显式契约**（笔记完全没提这一点，是最容易踩的坑）：
> §3.3 与 **§5** 两处都写着「**per-player poll 不夹紧——加播放器时自己防**」❌，由 `7ceb21b`
> 写下。改了代码不改它，两份真源当场分叉——正是 CLAUDE.md 反复告诫的那件事。已同 commit 改掉。
>
> **(d) 边界的定位要说清**：[10,2000] 是**安全下限**不是调参下限。各播放器自身的下限
> （qq 50 / cloud 100）都**高于** pollMin，两层不冲突：本条只挡忙等，调参下限留给播放器。
>
> **配套**：`config/clamppoll_test.go`。抽了 `clampPoll`/`clampPolls` 作可测入口——`Load()`
> 往全局 `flag.CommandLine` 注册 flag，**二次调用 panic `flag redefined: poll`**，表驱动测试
> 没法反复调它，只能手搓 Config。**关键**：用例必须断言 `cfg.Players["kugou"].Poll` 真被改写，
> 只测 `clampPoll(0)==10` 是假门禁——把 per-player 循环整个删掉那种测试照样绿（已实测）。
> 变异四方向全红：per-player 直通 / nil 被实体化 / 顺带钳了 Offset / 删掉全局钳制。
>
> **另开单子，本轮没做**（`aa78297` 的核实中浮现）：全局默认 `Poll=30` < cloudmusic 的
> 50ms 阈值 → **cloudmusic 在默认配置下恒跑 100ms**，而模板里原写的是
> `# cloudmusicv3-poll: 30`（**已订正**：2026-07-16 改为 `100` 并注明「低于 50 会被抬到 100」——
> 模板两处 `config/config.go` 的 `defaultConfigContent` 与 `config.yml`。对照组 `# qqmusic-poll: 50`、
> `# wesing-poll: 30` 本来就自洽，只有这条在骗用户）（**按符号找**：原写 `config.go:332` 当时就错，现在那行是 `defaultConfigContent` 里的模板文本，行号随本轮 diff 漂过两次）——模板注释是假的。且 `cloudmusicv3-poll: 20` 会过钳制（在
> [10,2000] 内 → 不 Warn）却被 cloudmusic 静默抬到 100：Warn 不响，值也没生效。

---

### 4. NaN：timer.go 半边已修，另外两个洞还开着

`timer.go:133` 现在是 `IsPlausiblePlayTime(v) = v >= 0 && v < 100000`（接受式），validateTimeAddr 已改用它——**这半边已修，且注释写得很好，别动**。

剩下两个：

- **`player/wesing/lyric/reader.go:57`**：`if len(lyrics) > 0 && timeVal <= 0 { break }`，:61 同款。`NaN <= 0` = false → 不 break → `LyricLine{Time: NaN}` 被 append。修法：读到 timeVal 后立刻 `if math.IsNaN(float64(timeVal)) { break }`。全仓 grep `IsNaN|IsInf` = 0 命中。
- **`server/server.go:490`**：`if err := conn.WriteJSON(evt); err != nil { return }` —— 把**序列化错误**当成了**连接级致命错误**。`json: unsupported value: NaN` 会杀掉写协程，而连接不关（见 §1b）。修法：区分两类错误，marshal 失败只 log.Error + 跳过该事件。

审计漏掉的一层（比它自己写的更严重）：`server.go:328` UpdatePlayerState 无条件把 NaN 写进 playerStates 缓存，`dedup.go:22` 的 `fmt.Fprintf(h,"%.3f")` 对 NaN 不报错 → 毒事件被**持久化**，此后每个新连接的 buildInitEvents 都带毒。「刷新 OBS 源即恢复」是乐观的。

代价：两处都是几行。**但触发前提没被证明**——「0xFFFFFFFF 是内存中最常见哨兵」是断言无证据，且与仓库内唯一经验证据相悖（reader.go:56/60 注释记录实际观测到的垃圾是 `time=0`，而 :57 恰好拦得住）。这条修的是「一致性」（同一份检查的 NaN 安全写法仓库里已经有了，只是没一致应用），不是「已知会咬人」。

> **2026-07-16 结案（`98030ae`）：❌ 不修，本条转为结论记录，别再当 TODO 捡起来。**
> 3 个视角 **3/3 判推翻、0/3 判可达**。上面那句自我怀疑不但成立，而且证据比它自己说的强。
>
> **(a) 有正面反证，不只是「断言无证据」。** `reader.go:60` 记录的观测垃圾是 `time=0`，
> 即看到的是**零填充内存**、不是随机位 —— 零填充重解释为 float32 **恒为 0.0，永不可能是
> NaN**。另实测全部常见 Windows 堆填充模式无一是 NaN（`0xCDCDCDCD`→-4.3e8、
> `0xFEEEFEEE`→-1.588e38、`0xCCCCCCCC`、`0xDDDDDDDD`、`0xBAADF00D`、`0xABABABAB`；且这些
> debug fill 只存在于 debug CRT，WeSing 出的是 release）。随机 32 位字是 NaN 的概率
> 实测 0.39%（1/256）。「`0xFFFFFFFF` 是 NaN」本身为真（negative quiet NaN），但「它是内存中
> **最常见**哨兵」这个叙事**已撤下**，别再「发现」一遍。
>
> **(b) 四条产 float 路径全判，无一可达 —— 而且本条的前提就是错的：**
>
> | 路径 | 判决 |
> |---|---|
> | **qqmusic** | **结构性不可达**。`mem.go:768` 的 `ProgressMs`/`DurationMs` 是 **uint32 不是 float**。`ReadFloat32`(`mem.go:481`) 是**零调用死代码** —— 「qqmusic 从内存读 float」这个印象正是它造成的。四处除法全被 `currentDurationSec > 0`（接受式）挡住 |
> | **cloudmusic** | **结构性不可达**。`DomTimeSec` 是 int；`CurrentProgress` 走 encoding/json，而 **JSON 标准不支持 NaN**（裸 NaN→解析错误、null→零值、`"NaN"`→类型错误） |
> | **wesing** | `wesing.go:464` 的 `playTime/songDuration` 确实漏了 `songDuration > 0` 守卫（同函数 `:199` 加了，一对拷贝一个加一个没加），但**互斥条件使其不可达**：`songDuration==0` ⟺ FindSongDuration 失败且 `len(lyrics)==0`，而后者 → FindCurrentLine 返 -1 → `:455` 的 `currentIdx >= 0` 为假 → `:464` 根本不执行 |
> | **kugou** | 链路**通**但**首环未证明**（见 (d)） |
>
> **(c) 照 §4 修会打红一个真门禁 —— 这是决定性的。** `ws_zombie_test.go` 以 `math.NaN()` 为
> **唯一触发载体**，钉的是「僵尸订阅者」：一个**真实发生过、已被 `9cb446e` 修掉**的 bug。
> §4 的出口修法（marshal 失败 → log+skip）一落地，NaN 就不再算「写失败」→ 该测试必红
> （实测 FAIL 5.01s）→ 要保住它只能改用注入 deadline，**正是仓库记忆点名的「手动改 deadline」
> 假门禁模式**。**拿已证实缺陷的门禁去换未证实缺陷的纵深防御，在「稳定 > 正确」下是负收益。**
>
> **(d) 本条盯错了源头。** §4 完全没提 kugou，而 kugou 才是唯一有可信度的入口：
> `kugou.go:364` `progressRaw, _ := strconv.ParseFloat(info.Progress, 64)` —— **err 被 `_` 丢弃**，
> 而 `cdp.PlayInfo.Progress` 声明为 **string**；Go 的 `ParseFloat("NaN")` → `NaN, err=nil`。
> 且 `isProgressValid`(`kugou.go:591`) 是**德摩根拒绝式** —— 正是 `AGENTS.md:326` 已立为规矩、
> `timer.go` 已修过（`6667a01`/`b4f530e`）的同一 bug 类，**规矩没被一致应用到 kugou**。
> 实测 `isProgressValid(NaN,180)=true` → NaN 直达事件。
> **但首环同样未证明**：JS 侧 `JSON.stringify(NaN)` → `null` → Go 得空串 → ParseFloat 报错 → 0，
> **最可能的那条 JS 路线是安全的**；只有酷狗自己 `String(nan)` 才中招。**同一类未证明，
> 不是更高一级的证据。**
> → 将来若要动 `kugou.go:364` 的 `_` 与 `:591` 的拒绝式，**必须作为独立条目**，理由写
> 「静默失败 + AGENTS.md §3 的接受式规矩未一致应用」，**绝不写「修 NaN 可达性」** —— 那不是
> 留着 §4 的理由。
>
> **(e) 后果被夸大，「每个新连接都带毒」是错的：**
> - `server.go:480-482` 的 init 补发循环是**裸 `conn.WriteJSON(evt)`、错误完全丢弃**（对比
>   `:496` 的 `if err := ...`）。`9cb446e` 的 `conn.Close()` 只在写协程里 →
>   **init 循环结构上不可能关连接** → 「毒缓存 → 新连接一出生就被关 → overlay 黑屏」为假。
>   实际是毒事件**静默降级丢失**，其余事件照常送达。
> - 毒性窗口 **≤ 当前这首歌**：`EventAllLyrics` 无条件重写 Duration/PlayTime/Progress，
>   `EventClearSongData` 六字段全清 → 3 分钟自愈。不是「此后每个新连接都带毒」。
> - 真发现一条（**审计和笔记都没有**）：`EventLyricUpdate` 分支不碰 `ps.Duration`/`ps.Progress`
>   → all_lyrics 的 Duration 毒能撑满整首歌。但仍有界、仍自愈。
>
> **(f) `:58` 挂的「`9cb446e` 的修法与本条判断有冲突，需复核」到此结清：不是冲突，是 §4 输了。**
> `server.go:490` 早已不是本条写的裸 `return`（现状是 `conn.Close()` + Warn）。且本文 `:183`
> 早就写着「✅ `9cb446e`。NaN 因果链已证伪（写超时同样触发）」—— **§4 与笔记自己的那一行
> 自相矛盾，`:183` 才是对的**。
>
> **两条反向警告，防下一个人重犯：**
> 1. `reader.go:56` 的 `len(lyrics) > 0` **是有意设计**，删它会让合法的 `[00:00.00]` 开场行
>    产出零歌词。此处将来若要动，只能换成 `IsPlausiblePlayTime`（接受 0、拒绝 NaN），
>    **不能**用 `<= 0`。
> 2. 「`songDuration==0` 与 `currentIdx>=0` 互斥」这个论证**有个洞**（首条 `Time == -10` 即破），
>    别把它当已证结论到处引用 —— 它只是让 `wesing.go:464` 在**当前**代码下不可达。

---

### 5. 两处 `range map` 决定 overlay 归属 —— 确定性排序，修法同构

**5a. `server/router.go:247`** — `for name, ps := range states` 构建 activeNames，:265/:308 `target := activeNames[0]`，lastPlaying 失配时直接抛硬币（实测 2000 次：cloudmusicv3:1507 / kugou:493）。失效前置条件是「lastPlaying 永不清除」：写入点只有 :179(playing) 和 :188(loading)，paused/idle/clearActivePlayer/forceGroupInert 都不碰。

审计低估了频率——这是它唯一的事实错误，但方向对修复者重要：`qqmusic.go:45` 和 `kugou.go:53` 的 `waiting_process` 发在**外层 for 循环体内**（每 ~2s 重发），而 `router.go:229-231` 对每条 status_update **无条件**调 evaluateNormalGroup（没有 watchExpire 里的 changed 门控）→ 硬币每 2 秒重掷一次 → 约每 ~5s 触发一次 `old != new` → switchTo → NotifySubscribersFullState 全量重放 → OBS 根 overlay 硬切。cloudmusic.go:105 的 waiting_process 在循环**外**，只有 qqmusic/kugou 有这个放大效应。

修法：给 Router 加 `orderedNames []string`（prior 组取 `cfg.PriorPlayer` 书写顺序，normal 组取 NewRouter 收到的顺序），evaluateGroup 改签名按 order 遍历。`activeNames[0]` 退化为「配置顺序里第一个活跃播放器」——既确定又可写进文档。

代价：**中低**。改一个函数签名 + 两个调用点，`grep -rn "sort\." server/` 零命中说明没有既有排序可复用，得自己传 order。

> **2026-07-16 已修（`f516f22`）。缺陷成立（实测复现），但上面的修法被三个视角一致否决，
> 且本条的事实错误是全笔记最多的一条：**
>
> **(a) 判据换了：不用「配置顺序」，用 `activeAt`（recency 补全）。** 三条理由：
> - normal 组的所谓「配置顺序」**根本不可配置** —— 它来自 `main.go:58` 的硬编码字面量
>   （不是 `registeredPlayers`，那个只喂 CLI flag / YAML 键）。用户既无法表达偏好，也无法
>   修正偏斜。且与 AGENTS.md「新播放器**追加**进 playerNames」的规定冲突：每个新播放器会被
>   **静默赋予最低优先级**；哪天为展示原因重排 Banner = 静默改路由。
> - prior 组的顺序是 **config 模板「取消注释」的产物，不是用户的表达**（模板把 4 个播放器按
>   同序预先列出、注释掉 3 个）。把它解释成优先级，等于**给用户从未做过的动作追认意图**。
>   全仓 `cfg.PriorPlayer` 的消费点（IsPriorPlayer 线性查找、NewRouter 建 map、banner、
>   service-status 回显）**零处消费顺序**；文档也零处说顺序=优先级。
> - 它与作者**白纸黑字声明的语义相竞争**：`796d69c` body 原话「切到 priorLastPlaying
>   （焦点竞争，**最后播放者优先**）」，README 与 config 模板也写「切换到最后一个普通播放器」。
>
> **正确的修法是把 recency 补全**：`lastPlaying` 本就是「最近播放者」的标量版，给 playerState
> 加 `activeAt time.Time` 是它的自然全序推广 —— 于是「lastPlaying 永不清除会陈旧」这个失效
> 前提**从根上消失**（陈旧者不在 activeNames 里，压根不参与比较）。与作者设计同向、与既有
> 文档一致、用户无需学任何新概念。
>
> **(b) 实现上有个比原缺陷更严重的陷阱，别踩**：**不要**按上文说的「evaluateGroup 改签名按
> order 遍历」。order 里漏掉任何一个 key，那个播放器就整个从 activeNames 消失 ——
> 不只丢 hasHolding（撞 AGENTS.md §2.3「holding 只能由超时释放」），**漏掉唯一活跃者即
> `len==0` → clearActivePlayer → OBS 空屏**，比它要修的问题严重一级。实际修法：
> `for name, ps := range states` **一字不改**，只在 return 前按 activeAt 降序排 —— 排序只重排
> 已收集到的名字，不可能丢成员。两个调用点也一字不改（它们本来就取 `activeNames[0]`）。
>
> **(c) 行号错了四处**（其一错位 137 行）：`kugou.go:53`→**`:190`**（:53 是封面预算注释）、
> `qqmusic.go:45`→**`:49`**（:45 是 return）、`router.go:265`→**`:267`**（:265 是注释行）、
> `main.go:62`→**`:58`**（**AGENTS.md §3.2/§5.1 同错，已一并订正**）。
>
> **(d) 「放大器」的判据不完备，照它审会误判**：笔记用「Emit 在不在 for 循环体内」当判据，
> 但 **`wesing.go:50` 也在外层 for 体内**（笔记漏列）。wesing 实际不是放大器，因为
> `waitForProcess`(:64-83) 是内部阻塞循环，进程不出现外层循环根本不转。**正确判据是「外层
> 循环的转动周期」，不是 Emit 的词法位置。** 另：`router.go:229-231` 称「无条件」失准 ——
> 那里有 `if !r.priorGroupBlocking()` 门控（原意「无 changed 门控」成立，但措辞会与 §2.2
> 「prior 组绝不加门控」打架）。
>
> **(e) 触发前提比笔记暗示的窄得多，别写成「常态每 5s 硬切」**：需要三个 normal 播放器
> **全部安装且本场都用过**、其中两个同时无人值守 playing、第三个最后播完且**进程已退出**。
> 而且 necessity 视角还纠正了两份材料共有的场景错误：「主播切播放器后关掉旧的」**不触发**
> —— 关旧的时 lastPlaying 早已是新的那个且仍 active，不失配。真正的条件是**反过来的**：
> 最后播放的那个死掉，另外两个还在放。
>
> **(f) 考古挖到比笔记强得多的证据**：`9090f0b` 把 `for _, p := range r.players`（**切片，
> 确定序**）换成了 `for name, ps := range states`（**map，随机序**）—— 即**它把这条路径从
> 确定序回归成了随机序**，而 body 的 11 条改动清单里一个字都没提顺序（删除理由只是「不检查
> 播放状态」，纯状态维度）。同一作者在 `468ea5c` 里专门建了 OrderedMap 类型，body 原话
> 「保证 JSON 序列化时保持插入顺序，**彻底消除 map 随机排列**」—— 他显然知道 map 随机序是
> 问题。所以这次是无声的附带损伤，**恢复确定序是修回归，不是发明新判据**。
>
> **实测**（复现了笔记的数字，并解释了它）：2000 次 → cloudmusicv3:1488 / kugou:512
> （笔记记的 1507/493）。**偏斜成因笔记没解释**：不是 hash 随机，而是小 map（单 bucket 8 slot）
> 按插入顺序填槽、`mapiterinit` 只随机化起点 offset∈[0,8) → 6/8 vs 2/8 = 75%/25%。
> **推论：分布跨进程稳定，不是每次启动都变。** 相邻重掷结果改变概率实测 0.3701（理论 0.375）
> → 每 2s 一掷 → 期望硬切间隔 **5.40s**，笔记的「~5s」可推导可复现。
>
> **别顺手修「放大器」本身**：把 waiting_process 移出循环会让「进程退出后前端收不到
> waiting_process」；给 status_update 加 changed 门控极易被误推广到 prior 路径、撞 §2.2。
> 只修 order 是零风险的那一个 —— 且 order 固定后失配场景下 `old == new` → switchTo 返回
> false → 不推 FullState → 硬切自然消失。
>
> **配套**：`server/routerorder_test.go`（**server/ 此前没有 router_test.go，路由是零守卫的**）。
> 变异四方向全红：去掉 sort（= 原始缺陷）→ 3 条红；排序方向反 → PrefersMostRecent；
> activeAt 打在守卫外（退化成「谁最后发事件」）→ ActiveAtOnlyOnEnteringActive；去掉同刻
> tiebreak → TieBreakIsStable。注：server 包 Linux 可编译，但 **CI 不跑测试**，本机门禁。

**5b. `server/server.go:595`** — `for name, ps := range s.playerStates { if ps.Status != nil { return name } }`，条件只要求「曾经发过任何状态」，waiting_process/standby/offline 全部满足。四个根 HTTP 端点（server.go:417 `registerRoutes("", "")`）全经此路。

关键：`router.go:31` normalizeStatus 把 waiting_process/standby 归为 idle，router 永不为其设 activePlayer → **`activePlayer==""` 是长期默认稳态**（开机没放歌），不是启动瞬态。而 `buildInitEvents`(:218-224) 对同一状态返回干净的 `player_clear` —— HTTP 与 WS 对同一逻辑状态给出不同答案，其中一条还不确定。`doc/API_RESPONSE_EXAMPLES.md:523` 原文写「根端点（活跃播放器）」。

修法：**删掉 :595-599 整个兜底循环**，返回 `""`。四个 handler 已有 `ps == nil` 分支，会干净返回 `Data:{}`。

代价：**删 5 行**。这是本节最便宜的一条。

> **2026-07-16 已修（`0ab8f61`）。上面保留原始判断存档，但它有三处错，动手前必须知道：**
>
> **(a) 位置与范围错了**：不是一个循环，是**两个**——`:598` 找 `Status.Status=="playing"`、`:603` 找任意
> `Status != nil`。上文只点了后者。行号也已漂移，实际是 `:597-607`，「删 5 行」实为 11 行（含注释）。
>
> **(b) 漏了本条最强的证据**：`9090f0b` 的 body 原话——「删除 `findAnyNonPriorPlayer()`（**歌词冻结bug的
> 根本原因**）」，理由是「**不检查播放状态**」。那是 router 侧的孪生体，`:603` 是它逐字的同型残株。
> 即**作者本人已对同一反模式做出裁决**，删它是同向而非撤销设计。溯源另证：两个循环诞生于 `65d9e80`
> ——当时 `Server` 结构体**根本没有 activePlayer 字段**，它们是唯一的选址逻辑；`6087be4` 叠加
> activePlayer 优先分支时没清旧路径，「兜底」是**事后追认的标签**，无任何 commit body 为其辩护。
> 跨重构溯源也堵死了：`65d9e80^` 的前身 `ws/server.go` 是单播放器架构，grep 无任何祖先。
>
> **(c) 「只删一个」是最差方案**（上文完全没提，但会误导修复者）：`:603` 的条件（`Status != nil`）
> **严格弱于** `:598`（`Status.Status=="playing"` 蕴含 `Status != nil`），所以删 `:598` 在数学上
> **不可能**让返回值变成 `""`，只会把「确定挑正在放的那个」降级成「map 随机 4 选 1」——实测命中率
> 100% → ~14%，保留了不一致还加进不确定性。**若只能删一个，必须删 `:603`。**
>
> **实测数据**（三个证伪者独立复现）：同一待机态 `resolvePlayer("")` 400 次 →
> `wesing:260 / qqmusic:53 / cloudmusicv3:49 / kugou:38`，**连续两次答案不一致 214/400**。
> `GET /song_info` 连打 6 次在「陈年歌名」与空之间随机抖动。**新发现的一层**（原判没有）：四个
> handler **各自独立**调 resolvePlayer，map 序每次重新随机 → 同一秒内 `/status_update` 与
> `/song_info` 可解析到不同播放器，拼出「A 的状态 + B 的歌名」——这是兜底独有的故障模式。
>
> **删除的真实代价**（诚实记录，不足以保留）：`router.go:104` UpdatePlayerState 与 `:107`
> updateRouting 是两次独立加锁，中间 `s.mu` 空闲 → 存在「raw playing 已落盘 + activePlayer 仍为
> ''」的微秒级窗口，`:598` 在此窗口能返回正确答案，删后该窗口内单次请求返回 `{}`。但同一窗口
> WS 侧 `buildInitEvents` 本来就返回 player_clear——删除是让 HTTP 与 WS 在这个窗口里**一致**。
>
> **配套**：`resolveplayer_test.go`（此前该路径**零覆盖**，正是兜底能潜伏三个月的原因）+
> `API_RESPONSE_EXAMPLES.md` 补了与 WS 的 `:127` 对称的 HTTP 明文条款（原先只有隐含契约，
> 不写明下一个人会把兜底加回来）。**变异自证**：worktree 停在修改前的 HEAD、只拷进新测试
> ——变异即原始缺陷本身，4 条全红且 `/song_info` 实测吐出残留 SongInfo；单独还原 `:598` 只红
> `IgnoresPlaying`（精确锁定其领地）、单独还原 `:603` 红 4 条（实证其严格更弱）；反方向假修复
> 「直接 return ""」红 `FollowsActive` + `PerPlayerPath`。
>
> 上文 `:556` 那两条「站不住的子结论」维持原判，别抄。另注：`:958` 记的 `server.go:606-608`
> 锁外读是**另一条独立问题**（已由 `0f83220` 修），与本次删除无关，别混。

> 两条子结论站不住，别抄：(a) 招牌场景「启动后 /song_info 在空白与上一首歌间闪烁」**不可达**——wesing.go:51/qqmusic.go:46/kugou.go:54 在 waiting_process 旁配对发了 EventClearSongData，启动态四个播放器全返回 `Data:{}`，只有 player 字段随机跳。陈旧数据走的是另一条路径：暂停只发 EventPlaybackPause、不发 ClearSongData，等 `router.go:378-381` 超时 clearActivePlayer 后该播放器仍持有 SongInfo。(b)「判断是否有歌在放的消费者拿到随机答案」**错误**——:595 只在 :590 没找到 playing 时才可达，消费者对「有没有在放」得到的是一致的「没有」。

---

### 6. 垃圾文本上屏：三条同源，且仓库里现成的闸门是死代码

`player/wesing/lyric/reader.go:145` 的 `isValidLyricText` 是**完整的垃圾文本校验器，零调用方**（我 grep 过，只有定义）。下面三条缺的正是它。

**6a. `player/qqmusic/api.go:330`（原审计标 :286，已漂移）** — `if len(lines) == 0 && len(rawTextLines) > 0` 兜底，无任何内容校验。`api.go:324` 收集条件只是 `!strings.HasPrefix(line, "[")`。

QRC XML 抵达 parseLRC 是**主力路径**：`mem.go` 版本表里 v21.81 和 v22.16 的 `SongMidParamsOff` 与 `StreamURLOff` 均为 0 → `meta.SongMid` 恒为 `""` → `qqmusic.go:132` 的守卫短路 → `api.go` 的 `if songMid != ""` 不成立 → 走 musicu.fcg + 硬编码 `"crypt": 1` → qrcDecrypt → QRC XML。全仓 grep `QrcInfos|LyricContent|Lyric_1` 零命中，**没有任何地方剥 XML 外壳**。后果：`<?xml version="1.0"?>` / `<QrcInfos>` 这些标签被按 durationMs 均分铺满整首歌，`qqmusic.go:187` 还打「歌词加载完成: 5 行」；`len(currentLyrics)==0` 的纯音乐守卫接不住（len=5）。另一张脸：hex 解码失败时（api.go:202-209 只 log.Warn 不 return）整坨未解密 blob 被当歌词铺出去。

修法：兜底路径加内容校验，直接复用 `isValidLyricText`（拒绝以 `<` 开头 / 含 `<...>` / 纯 hex）。

代价：**低**（一个 if + 把死代码提成导出函数或复制过去）。真正剥 XML 外壳是另一件事，可以后做。

> **2026-07-16 结案（`ba210c4`）：⬇️ 降级 LOW·记档。招牌被实测证伪，上面的修法整个作废。**
>
> **(a) 「标签铺满整首歌」在主力路径上不可达。** 实测：拿真实形状的 QRC XML（`LyricContent`
> 里有 `[start,dur]text(...)` 逐字行）喂**真实的 `parseLRC`** → 吐出的是**正确歌词**，XML 外壳
> **零泄漏**。机制：QRC 内层逐字行命中 `qqRe`（`^\[(\d+),(\d+)\](.+)`）→ `len(lines) != 0`
> → **兜底那个 `len(lines)==0` 短路，根本不触发**；外壳行进了 `rawTextLines` 但被静默丢弃。
> `tiRe`/`arRe` 未锚定，还顺手把歌名歌手正确抠了出来。
> → 本条「QRC XML 抵达 parseLRC 是主力路径 → 标签铺满」：**前半句对，后半句错。**
>
> **(b) 真实危害只剩一条窄缝，且可达性未证实。** 只有**空壳 QRC**（`LyricContent=""`，纯音乐）
> 才会把 7 行标签按 durationMs 均分铺出去。本条自己写的「5 行」正是空壳的行数量级 ——
> **它描述的一直是空壳场景，却安在了主力路径的标题下。** 而空壳到底会不会从服务端回来
> **未能证实**：`api.go:252` 有 `if rawLyric == ""` 早退，若 QQ 对纯音乐直接回空，这条**整个是
> 理论问题**。无法发网络请求验证 → 这是降级的主要依据，也是最大的诚实缺口。
>
> **(c) ⚠ 推荐的修法是恒绿假门禁。** `isValidLyricText` 实测：
> `<?xml version="1.0"?>` → **true**；`<QrcInfos>` / `<QrcHeadInfo .../>` / `<Lyric_1 .../>` /
> `</QrcInfos>` → **全 true**；纯 hex blob → **true**。**6/6 全放行。**
> 原因很朴素：它是**码点区间占比检查器**（统计落在 ASCII 可打印 / CJK / 假名 / 谚文等白名单的
> rune 占比 > 0.6 即过），**不做任何结构判断**；而 `<`(0x3C)、`>`(0x3E)、`?`、`/`、`"`、hex 字符
> 全在 `0x20..0x7E` 里 → 占比 100% → 恒 true。
> **本条括号里那句「（拒绝以 `<` 开头 / 含 `<...>` / 纯 hex）」是凭空写的 —— 那三条它一条都不做。**
> 照着接上去，空壳的 7 行标签一行不少地照样上屏。
> **而且它会误杀**：实测 `Я тебя люблю` → **false**（白名单里没有西里尔字母，也没有希腊/泰/
> 阿拉伯/希伯来）→ **静默丢弃俄语等歌词**。它零调用方或许不全是疏忽。
> → 三个选项全否：复制过去 ❌（复制的是个不管用还误杀的东西）；提到公共包 ❌（同上，还污染
> `player/` 根包）；跨包 import ❌（`wesing/lyric` → qqmusic 是跨播放器耦合，与 §7b 判例同构）。
>
> **(d) 「另一张脸」（hex 解码失败只 Warn 不 return）是陈旧的，而且改它 = 重新引入回归。**
> 本条写的 `api.go:202-209` 漂了 **71 行**（今天那里是 musicu.fcg 的 payload map，**完全无关的
> 代码**）。当前真实位置 `api.go:131-134`，且那个 Warn 是 **`6446f82` 专门返工修回来的**：
> `b4d9fda` 曾把 qrcDecrypt 的**全部**失败一律转 error，导致「crypt=1 但服务端实际回明文」
> 从「正常显示歌词」退化成「0 行」。现由哨兵 `errNotCiphertext` 区分（hex 解不开 = **它根本
> 不是密文** → 原样放行 + Warn；3DES/zlib 失败 = 是密文但坏了 → error），并有测试钉死。
> **今天它是设计不是缺陷。** 本条把它塞进「另一张脸」是错的 —— 那是独立的另一条线。
>
> **(e) 落点判断也反了。** 本条说「真正剥 XML 外壳是另一件事，可以后做」——实际剥外壳**更便宜
> 更正确**（~10 行字符串裁剪，且把治标那条整个消掉）。**但 `b4d9fda` 的 commit message 里，
> 本仓库已经就「防线放哪」下过判决**（原话）：「**这不是 parseLRC 的 bug——对真正无时间戳的
> 纯文本歌词，均分兜底是有意为之；防线只能在上游。**」`6446f82` 又补刀撤回了「真实 LRC 一定
> 以 `[` 开头」的说法。→ **在兜底里加内容校验 = 在仓库已判定「不该设防」的层设防。**
> 若将来要做，落点是 `fetchLRC` 内 `decryptIfNeeded` 之后、`html.UnescapeString` **之前**
> （⚠ 顺序陷阱：先 unescape 会把歌词里的 `&quot;` 变成裸 `"`，属性边界检测当场错位）。
>
> **(f) 行号 5 个错 4 个**：`api.go:330`→**`:343`**、`api.go:324`→**`:337`**、
> `api.go:202-209`→**`:131-134`**（漂 71 行）、`qqmusic.go:187`→**`:224`**（漂 37 行）；
> `qqmusic.go:132` 的真正守卫是 `:141` 的 `shouldDeferForSongMid`。唯一没漂的是 `reader.go:145`。
> 本条自己标着「原审计标 :286，已漂移」—— 果然又漂了一轮。
>
> **(g) 可达性链条本身属实，且比本条写的更广**：`SongMidParamsOff` 为 0 的不止 21.81/22.16，
> **22.22 也是**（本条漏了），加上三处「未知版本回退 22.16」→ 确实命中当前推荐的全部 22.xx。
> 「全仓 grep `QrcInfos|LyricContent|Lyric_1` 零命中」也属实。**但链条通 ≠ 危害可达**（见 a/b）。

**6b. `player/cloudmusic/lyric/fetch.go:47`** — ✅ **已修（`f9500c3`）**，下文保留原始分析。
`\[(\d+):(\d+)\.(\d+)\](.*)`，`(.*)` 从第一个 `]` 贪心吃到行尾。压缩型 LRC（同一句配多个时间戳，标准 LRC 特性）直接漏进正文。

**这条有真实歌曲实证，是本节可达性最硬的一条**：真实歌曲 ID 5277704 跑过真实生产管线 FetchLyrics → 20 行里 **16 行**带着裸 `[00:47.19]` 前缀发给 OBS，该曲 overlay 直接废掉。315 首真实采样中 2 首（~0.6%）命中。且网易云把压缩时间戳按**降序**排（`[02:04.45][00:47.19]`），所以取的是较晚那个——首次副歌完全没歌词，复唱显示 `[00:47.19]`。附带后果四也成立：`SameLyricText("[00:45.670]我爱你","我爱你")` 实测 false → YRC 逐字静默丢失。

修法：先用 `^(?:\[(\d+):(\d+)\.(\d+)\])+` 把行首连续时间戳全剥出，正文取最后一个 `]` 之后，每个时间戳各生成一条 LyricLine（生成后按 Time 排序、重排 Index）。

代价：**低到中**。有 `yrc_test.go` 这类既有测试，且 **cloudmusic-lyric 包 Linux 能编译 → CI 的 self-hosted runner 能跑回归**，这是少数几个能被真正测到的修复之一。最低限度版本（识别到正文仍以 `[` 开头就剥掉、只留一个时间戳）是 3 行，至少不把时间戳打到屏幕上。

**6c. `player/qqmusic/mem.go:1005`** — `if capacity > 15 && length > 0 && length < 1000` 把「堆/内联判定」和「length 合法性」挤进同一个 `&&`，任一 length 校验失败就把**已知是堆字符串**的对象交给内联读路径 → `buf[0:4]` 装的是堆指针不是文本 → 返回指针原始字节当歌名。`len(name1) > 1` 过关，sanitizeString(:826) 按 rune 过滤、`0xB1` 解成 RuneError 且 >=32 被保留 → overlay 闪乱码标题。

对照组证明是疏漏：`mem.go:788` 的 `ReadSSOString` 写法正确（先分堆/栈，再在堆分支内 reject），但**零调用方，是死代码**。

比审计的 clear() 假设更硬的触发路径：`detectVersion`(mem.go:352-353) 对任何未知版本回退到 22.16 偏移 → 偏移错位时 length 是随机 DWORD → `length < 1000` 几乎必然为假 → 必进 else → 乱码。**QQ 音乐每次自动更新都会踩。**

修法：照抄 ReadSSOString 的两层结构——`if capacity > 15 { if length == 0 || length >= 1000 { return "" }; ... }`，然后才是内联分支。

代价：**低**。但**别盲目收紧**：else 分支对短名是正确且已验证的路径（mem.go:155 的验证用例「无地自容」= 12 UTF-8 字节 → cap==15）。只应在 `capacity > 15`（堆模式确诊）时 reject。

> **2026-07-16 已修（`ba210c4`）。本条是本轮少见的「笔记基本写对了」——坐标 5/5 全对、
> 机制实测坐实、修法照抄可用、「别盲目收紧」的警告有效。三处订正：**
>
> **(a) 严重性论证反了。** 本条力推的触发路径（版本偏移错位 → length 是随机 DWORD）实测
> **只有 8.5% 出乱码**：随机 24 字节不含 0 的概率 (255/256)^24 ≈ 91% → `inlineIdx == -1`
> → 静默返回空。**真正的主力是 clear()**（capacity 保留、length 归 0）—— 实测 **96.9% 出乱码**，
> 因为 `length==0` 使 `buf[16:20]` 恒为 0、**保证** inlineIdx 命中。本条称偏移错位「比审计的
> clear() 假设更硬」**恰好说反了**。定级依据要换成 clear()。
>
> **(b) 本条问的「更好的降级」早就存在，别新增。** `qqmusic.go:133` 已有 `meta.Name != ""` 门
> → 空名**根本不 emit 切歌事件** → overlay **保留上一首标题**。本条设想的三个备选（空标题 /
> 保留上一首 / 不 emit）其实是同一个且已落地。修法只需 `return ""`，不写任何降级代码。
> 另：本条说的「`len(name1) > 1` 过关」**定位错了** —— `:1050` 那个 `meta.Name = name1` 是
> **死存储**（return 用的是 `name`，`meta` 只有 `SongMid` 被读）。真正的门是 `:1141` 的
> `name := name2; if len(name) < 2 { name = name1 }`，而 22.16 的 `Struct2Ptr` 非零 → Struct2
> 的 WCHAR* 路径**优先于** extractSSO → 乱码只在 name2 空/过短时才浮到 overlay。
>
> **(c) 两个「别动」，本条没提：**
> - **别抄 `ReadSSOString` 的常量**：它用 `strLen > 2048`，这里是 `< 1000`，照抄 = 顺带放宽
>   两倍上界（已加变异用例钉死这条）。
> - **别「修好」内联分支里那个算完即弃的 `idx`**：它对短名是已验证正确的路径，动它没有收益、
>   只有回归风险。
>
> **顺带做的一处重构**：把逻辑抽成包级纯函数 `ssoFromBuf(buf, readHeap)` —— 原 `extractSSO`
> 是 `ReadAllMetadata` 里的闭包，测不到，而这是「一错就往 overlay 推乱码标题」的路径。抽出来
> 顺带消掉了三次多余的 `ReadUint32`（length/capacity/ptr 在 24 字节的 `buf` 快照里本来就有，
> 重读一遍既浪费又引入 TOCTOU 窗口）—— `ReadSSOString` 本来就是直接从 buf 解的。
>
> **考古（3/3 判官一致）**：`ReadSSOString`(`:773`)、`extractString`(`:802`)、`extractSSO`
> **三者同生于唯一的 commit `3bb0453`**（body 全文只有「[FEAT]qq音乐！」，一次性灌入 824 行）。
> 所以那个写法正确的版本**不是「被取代的旧代码」，而是出生即死**：作者同一次提交里写了三个
> SSO 读法，只接了最差的那个。~~建议一并删掉两个零调用方死函数（`ReadSSOString` / `extractString`）~~ → **2026-07-16 LOW review 改判：别删。**
> 理由不是「删了会坏」，而是**它现在是活的参照物**：`ssoFromBuf` 的注释明写「同文件的 ReadSSOString
> 用的是 2048，别顺手抄过来」—— 删掉它，那句注释当场悬空，又一次文档分叉（本轮反复踩的坑）。
> 而删死代码的唯一收益是「简洁」，在本仓库的优先级里排最后一位。
> 同理**别用 ReadSSOString 直接替换 extractSSO** —— 它的内联分支用 `strLen`
> 而非 null-scan、上限 2048、且不做 `bytes.Trim`，是更大的行为变更。
>
> **配套**：`ssostring_test.go`。变异四方向全红：塌回单层 `&&`（= 原始缺陷）→ 2 条红；
> 去掉 length 校验 → HeapWithBadLength；废掉堆读 → HappyPath + TrimsTrailingNul；
> 上界 1000→2048 → HeapWithBadLength。qqmusic 包 Linux 编译不了，本机门禁。

---

### 7. 两条效率，都有实测数字，修法都是「换个 API」

**7a. `player/cloudmusic/park/park.go:65`** — `cachedHwnd = findMainAny()` 把 0 写回缓存，下轮 `cachedHwnd != 0` 为 false 必然再枚举 →「查无此进程」这个结果从不被缓存。`effect.go:286-287` 一轮调两次（IsMainMinimized + IsMainForeground），`effect.go:329` sleep 80ms。

实测（本机 511 进程，用 GetProcessTimes 量内核+用户时间，排除墙钟假象）：**7.9 轮/秒，本进程 CPU 占 41.3% 个核心**；缓存命中路径对照组 0.0%。单次 `cloudmusicPIDs()` 平均 34.8ms —— gopsutil 的 `Processes()` = EnumProcesses + 逐 PID OpenProcess，`p.Name()` 再开一次 OpenProcess，约 8k 次句柄打开/秒，全在内核。

且这笔开销 **100% 花在「什么都不需要做」的时段**：网易云一起来缓存就命中、开销归零。windowStateLoop 不受任何门控（无 config 门、无订阅者门、无 netEaseUp 门），**连 park 被彻底禁用的 Win11 机器上这笔开销照烧**。

不是故意的：park.go:43-44 注释明写缓存存在就是为「避免每次轮询都做进程枚举」。

修法：findMainAny 返回 0 时缓存负结果 + 时间戳，1-2s 内不重复枚举。

代价：**~5 行**。

**7b. `player/kugou/watchdog/watchdog.go:519` + `:210`** — `IsKuGouRunning()` 是 `exec.Command("tasklist", ...)`，waitForKuGou 每 500ms 调一次，两个退出条件都不可达（酷狗不启动 + `Stop()` 全仓零调用者，`main.go:197-203` 信号处理直接 os.Exit(0)）。

可达性：`main.go:184` 无条件 `go kp.Start()`，config 无 kugou 开关（RegisterPlayer 只生成 offset/poll）。装过酷狗 + patch 过 + 今天不用酷狗 → allPatched=true → `:583 waitForKuGou(stopCh)` → 整场直播 2 次/秒 CreateProcess，每次过一遍杀软的同步 CreateProcess 内核回调，用户没有任何办法关掉。

**这是孤例，不是房规**：其余全部走进程内 Win32（`wesing/proc/memory.go:288`、`qqmusic/mem.go:227` 用 CreateToolhelp32Snapshot，`park.go:197` 用 EnumWindows），连它自称模仿的网易云 watchdog（`cloudmusic/watchdog/process.go:66`）也是进程内枚举。

修法：`IsKuGouRunning` 换成 CreateToolhelp32Snapshot + Process32Next 比对进程名（该包已在用 `golang.org/x/sys/windows`）；waitForKuGou 加退避（500ms 起、封顶 5s）。顺带把 killKuGou(:200-205) 的轮询也换掉。

代价：**低**，所需原语仓库里现成就有。注意 `:582` 注释「仿网易云逻辑，不自动启动」——**阻塞本身是对的**（不阻塞的话外层 kugou.go:72-83 会 2s 空转刷屏），错的只是探测手段与频率，别把阻塞一起改掉。

> 修辞降级：2 次/秒称「风暴」夸张，这条不会直接造成直播事故。

> **2026-07-16 处置（`3457832` 记档）：⬇️ 降级 LOW·顺手修，撤出 MED（3/3 判官一致）。
> 机制全部属实，但坐标无一处对，且修法比本条写的窄：**
>
> **(a) 行号全线过期**（机制无一处错、坐标无一处对）：`:519`→**`:284`**、`:210`→**`:274-279`**、
> `:582`/`:583`→**`:731`**、`main.go:184`→**`:180`**、`main.go:197-203`→**`:193-199`**。
> 「2 次/秒」属实（`waitForKuGou` 在 `:657-673`，`:671` sleep 500ms），但**漏了两个调用者**：
> `kugou.go:276` 的 `waitForCDP` 每 2s 一次；`killKuGou:274-279` 每 500ms 一次、上限 10 次。
>
> **(b) 可达性四环全部核实成立**：`main.go:180` 无条件 `go kp.Start()`；config 确无 kugou 开关
> （只有 `kugou-offset`/`kugou-poll`）；`Stop()`(`player.go:198`) 全仓**零调用者**；信号处理直接
> `os.Exit(0)`、从不关 StopCh。
>
> **(c) 但「用户关不掉」的后果 ≈ 零，这条是清洁工作不是事故修复。** 三个常见猜测全不成立：
> **不是** CMD 窗口一闪一闪 —— 这是**控制台程序**（`build-windows.yml` 的 `go build` 无
> `-H=windowsgui`），tasklist 子进程**继承父控制台、不创建新窗口**；**不是**任务管理器里看到
> 大量进程（tasklist 毫秒级退出，刷新率捕捉不到）；**就是字面的「没有开关」**。实际代价是一笔
> 看不见但关不掉的后台税（2/s CreateProcess，每次触发杀软的同步内核回调）。**别按 P0 排期。**
> ⚠ 附带发现：全仓 `SysProcAttr`/`HideWindow`/`CREATE_NO_WINDOW` **零命中** —— 现在无害，
> 但哪天二进制改成 `-H=windowsgui`，这里就会开始闪窗。
>
> **(d) 「阻塞本身是对的」成立，且理由比本条写的更硬。** 去掉阻塞的链路：`EnsurePatched`
> 返回 nil → `waitForCDP`(`kugou.go:260`) → `cdp.Connect` 失败 → **`:276` 的
> `IsKuGouRunning()` 为 false 立即返回** → `kugou.go:242-243` Warn + sleep 2s + continue →
> 外层每 2s 重发 `waiting_process` + `ClearSongData` 并刷一条 Warn，永不停歇。
> **讽刺的是**：让「去掉阻塞」变有害的那个快速失败(`:276`)，正是 `bc5790b` **有意加的**。
> 且 `:731` 注释「仿网易云逻辑，不自动启动」表明阻塞是设计意图。**别碰阻塞。**
>
> **(e) 溯源：tasklist 不是为规避权限的有意设计。** `14a94ba`（引入点）**commit body 全空**，
> 只有标题；`bc5790b` 只导出了 `IsKuGouRunning` 并加存活检测、**没碰实现手段**。「这是孤例，
> 不是房规」经 grep 全仓进程枚举点核实**属实** → 换掉不撤销任何他人设计。
>
> **(f) 修法比本条写的窄，三个「别」：** `watchdog.go:18` **已经 import 了
> `golang.org/x/sys/windows`**，该库自带 `CreateToolhelp32Snapshot`/`Process32First`/`Next`/
> `ProcessEntry32`/`TH32CS_SNAPPROCESS` 原生封装 → **约 15 行，零新依赖、零跨包耦合、
> 零 NewProc**（`park.go:200` 记着 NewCallback 上限 2000 且永不回收的教训 —— toolhelp 路径
> 不涉回调，安全）。
> - **别**手搓 `NewProc`（库里现成有）。
> - **别** import `wesing/proc`（虽有现成的 `FindProcess`，但引入 kugou→wesing 跨播放器耦合）。
> - **别**抄 `park.go`/`cloudmusic/watchdog` 的 gopsutil `process.Processes()` —— **那正是 §7a
>   的病灶本身**（逐 PID OpenProcess，实测 34.8ms/次）。
> - 本条自己提的「**waitForKuGou 加退避**」那一半，3/3 判官**否掉**：只做叶子函数置换，
>   别动频率、别动阻塞。
>
> **(g) 为什么这次不做**：需真机验证（watchdog 包 Linux 不可编译，且 CI 根本不跑测试），
> 而改的是「一错就整场没歌词 + 牵扯 UAC 提权」的路径。**排到下次触碰 kugou/watchdog 的窗口，
> 与别的 kugou 改动一起手测。** 若届时要加测试，照 `park.go:68 var findMainAnyFn = findMainAny`
> 的可注入样板测 `waitForKuGou` 的退出逻辑（syscall 路径本身需真 Windows + 活进程，不值得硬测）。

---

### 8. 不建议现在做

**8a. `player/cloudmusic/lyric/fetch.go:332` MergeYRC 贪心对齐 —— 只做止血，不改算法**

机制成立，我复核时实测复现（LRC 10.0/12.0/14.0 同文本 + YRC 仅 12000/14000 → line0 抢 12.0、line1 抢 14.0、line2 逐字为空；ties 随机性实测 200 次得 `map[10:174 14:22]`）。

但支撑 HIGH 的「后果二」（LyricDisplayStart 提前跳行 → 高亮行错一行）**用它自己的输入走不到**：`player.go:225` 只在 `detail.Timestamp < lineTime` 时才替换，贪心按升序跑、行只会抢更晚的条目，`12.0 < 10.0` 为假 → displayStart 仍是 10.0，trueLineIdx 不跳。剩下成立的部分是优雅降级：不崩、不 panic、行级歌词仍按正确时刻上屏，损伤仅限逐字高亮层 + 少一行 lyrics_detailed。

触发需要四个条件合取（有 YRC + 存在完全同文本重复行 + 间距 < 3s + YRC 恰好漏掉其中一条），而「常见」这个流行度断言**全程无真实歌曲样本佐证**——这是本条最大的证据缺口，与 6b 的 315 首采样形成鲜明对比。

建议：**别做全局最优匹配**（改算法、动的是直播主路径、收益未证）。只做三行止血：
1. `:343` 改成 `if diff < bestDiff || (diff == bestDiff && yrcTimeMs < bestKey)` —— 消除 map 顺序不确定性，让故障至少可复现。
2. 抢到的 `detail.Timestamp` 与 `lyrics[i].Time` 差值超阈值时 `log.Warn` —— 让故障可诊断。

收紧 toleranceMs 也**不建议**：`366ea00` 是主动从 1250 放宽到 3000 的，说明作者踩过对面那个坑，收紧要有新证据。

> **2026-07-16 处置（`3457832`）：止血第 1 条已做；「三行」这个说法与止血第 2 条撤销；
> 本条整体降级 LOW、撤出 MED（3/3 判官一致）。上面的三个前提全错，别再照抄：**
>
> **(a) 位置错、归组错。** 不在 qqmusic —— `MergeYRC` 在 **`player/cloudmusic/lyric/fetch.go`
> （网易云）**，全仓 `player/qqmusic/` 对 YRC **零命中**。本条被归进 §8（qqmusic 那组）本身就是
> 切片时的错误。
>
> **(b) 与 QQ 版本策略无关，不能用它降级。** `knownVersions` 只存在于 qqmusic 的 `mem.go`，
> 表里**没有任何 YRC 相关偏移**。这条不是老版本专属，是**另一个播放器**。
>
> **(c) CI 完全可测。** `GOOS=linux go vet ./player/cloudmusic/lyric` 实测 **exit 0**
> （FAIL 的是 qqmusic）。加回归测试零障碍。
>
> **(d) 行号已漂**：`:332`/`:343` 在 2026-07-16 的三个修复（`71fe2f1`/`f9500c3`/`d63a67d`）
> 之前是准确的，现已漂到 `:326`/`:359`。
>
> **(e) ⚠ 止血第 2 条（差值超阈值 log.Warn）不可实现，已撤销。** 笔记从未给出阈值，而且
> 这个**方向本身被仓库自己的绿色测试证伪**：把现有测试断言的合法漂移全量算一遍 ——
> `yrc_test.go` 的 Goodbye L34 合法漂移 **2470ms**（测试断言它必须匹配），而**坏抢占的漂移
> 只有 2000ms，比合法的还小**。→ **任何能抓住坏抢占的阈值，都会在绿色用例上误报。**
> 调查提的替代方案（无阈值信号：末尾若「某行 Words 为空 且 details 仍有未 used 条目」则
> Warn）3/3 判官**也不采纳**，一并记档。
>
> **(f) 「三行止血」这个数字没有依据**，原文写「只做三行止血：」后只列了 2 条。最可能指代码
> 行数（1 + 2 ≈ 3），但既然第 2 条已撤销，这个说法直接删掉，别让下一个人去凑那三行。
>
> **已做的只有第 1 条**：`fetch.go:359` 的 tie-break `if diff < bestDiff || (diff == bestDiff
> && yrcTimeMs < bestKey)`。无初值缺陷：`bestDiff` 初值 `toleranceMs+1`，首个候选要么走
> `diff < bestDiff` 更新 bestKey，要么其 diff 已 > toleranceMs、最终被 `bestDiff > toleranceMs`
> 拦掉，bestKey 不会停在 0 被误用。它治的是**可观测性**不是稳定性：把随机故障变成确定故障。
>
> **「别改算法」的判断成立，已核实**：不崩不 panic（末行拿零值 `LyricTextDetailed{}`，
> `MergeYRC` 只写 `lyrics[i].TextDetailed`，无索引风险），行级歌词仍按正确时刻上屏，损伤仅限
> 逐字高亮层；触发需四条件合取；「常见」这个断言无真实样本佐证。**「不收紧 toleranceMs」也
> 成立且有了新证据**：Goodbye L34 的 2470ms 合法漂移证明容差确实需要这么宽。
>
> **配套**：`yrctiebreak_test.go`。其中 `TestMergeYRCGreedyIsUnchanged` 是**记录现状**的用例 ——
> 它钉死「贪心抢占仍然存在」这个**有意的取舍**；哪天真上了全局最优匹配它会红，那时应当
> **更新它而不是删掉它**。变异三方向全红：无 tie-break（= 原始缺陷）→ TieIsDeterministic；
> 假修复「永远取时间戳最小」→ PrefersNearest；tie-break 方向反 → TieIsDeterministic。

> 溯源里有一条错的：它称 `yrc_test.go:115` 的 Na-na 用例只因「重复行间距 3.2~3.8s 大于容差」而通过。实测同文本重复行间距是 **7.10s**，而 3.2~3.8s 发生在**不同文本**的行之间（10 个 na vs 7 个 na），SameLyricText 实测已把它们隔开。结论（该用例未覆盖相邻同文本重复）仍成立，但理由是两重因素。

**8b. `player/qqmusic/qqmusic.go:117` lastName 提前更新 —— 修法陷阱，别按审计写的改**

机制核实无误：`lastName = meta.Name` 在 :117，gate 在 :132-134 只设 err、不回滚；grep 全文 lastName 只有 79/116/117/263 四处，116 确是唯一重试入口。「等待下次轮询」结构上不可能发生——这条断言为真。

但两条降级理由：(a) 触发概率未经证明——0x70(名)/0x80(URL) 是同一「当前曲目」记录的相邻字段，若 QQ 音乐在 URL 解析完成后一次性填充，写入间隙是微秒级，对 50ms 轮询近乎不可命中。旁证：若窗口真有数百毫秒，v20.05 将**任何歌都没有歌词**，作者做 db011f7 时必然当场发现。(b) 后果是「单一 legacy 版本上单曲丢歌词」，静默、不崩溃。

**关键警告**：审计给的修法（「gate 分支显式 `lastName = ""` 并 continue」）是**稳定性倒退**。若 SongMid 持久为空（本地文件播放：0x80 是本地路径，basename 过不了 `mem.go:1099` 的 10-20 字符校验），回滚会让该块每 50ms 重入 → `mem.FindCookie()` 每轮执行，而 `mem.go:839-842` **只在成功时缓存**，未命中即用 VirtualQueryEx+ReadProcessMemory **全量重扫 2GB 地址空间**；再叠加 20Hz 的 EventSongInfoUpdate/EventAllLyrics 推给 OBS。按本仓库 稳定 > 正确 的排序，这个「修复」比 bug 本身危害大得多。

正确修法是有界重试（独立的 `pendingMidName` + 尝试计数，比如 3 次后放弃），但那不是「便宜修」。建议**暂缓**，等有 v20.05 实际丢词报告再动。

**8c. `player/wesing/lyric/finder.go:59` FindLyricHost 取 results[0] —— 值得修，但不是一行**

这条的 git 历史是本切片里最有说服力的：finder.go:59 出自 `e984873f`(2026-03-18) 从未改过；timer.go 的逐个校验循环出自 `ef81a425`(**次日**);songinfo.go 的「取最高地址=最新分配」启发式出自 `a4957aa0`(2026-03-28)，commit 标题「**[feat]获取封面&尝试修复歌手不准确**」，diff 明确把 `// 返回第一个有效结果` 改成 `// 返回最高地址的有效结果（最新分配的数据）`。

即：**「取最低地址会拿到过期数据」是在 WeSing 这个进程上被实测踩出来的 bug，并被显式修掉了两次，finder.go 是漏掉的第三处 back-port**。三处同源代码，只有它既不遍历也不校验，还取了与结论**相反**的一端。

且 `isValidLyricText` 是死代码 → LoadLyrics 下游没有任何乱码过滤（见 §6）。

修法：从 `len(results)-1` 倒序遍历 + 结构校验（4 字节对齐；读 `candidate+0x0C+0x48/+0x50` 得 begin/end，要求 `begin != 0`、`end > begin`、`(end-begin)%4 == 0`、`(end-begin)/4 <= 1000`），取第一个通过的。

代价：**中**。逻辑不难（等价于把 LoadLyrics 的前置条件提升为选址判据），但——**wesing 包 CI 永远跑不了**（Windows-only，只有 self-hosted Linux runner），只能靠本机对着真实 WeSing 进程手测。改的是内存选址这种一错就整场歌词报废的路径，验证成本远高于编码成本。

不列进「便宜修」的另一个理由：finder.go:59 已上线约 4 个月，而同类问题在 songinfo.go 上 10 天内就被发现修掉——说明实际频率低（合理推测：LFH 大概率把新 LyricHost 分配回刚 free 的同一地址，通常只有 1 个命中）。建议排到下一个 wesing 触碰窗口，和别的 wesing 改动一起手测。

> 后果措辞纠偏：随机 4 字节误命中大概率死在 `reader.go:32` 的 `endPtr <= beginPtr` 或 `:37` 的 `numEntries > 1000` → 落到「整首无歌词 + 静默 1s 重试」而非乱码。真正能走通指针链的是**结构完整的已 free 旧 LyricHost**，结果是**连贯的上一首歌词**——比乱码更糟，因为看起来完全合理，主播不会立刻察觉。

---

### 剔除说明

- **`server/server.go:610` 四个 HTTP handler 锁外读 PlayerState** —— 切片里仍在，但**已修复并提交**（6667a01 前一批）。我核实了当前代码：:602-624 已有完整的纪律注释（「快照必须在锁内取完」/「绝不在持锁期间 writeJSON」）+ `server/race_test.go` 复现测试。~~这条 self-hosted Linux runner 上 `go test -race ./server` 能跑，是切片里唯一有真实 CI 覆盖的。~~ ← **这句是错的**（`889c891` 漏掉的第三份拷贝）：CI 从不跑测试，见上方「关于『CI 门禁』的订正」。准确说法是「**能**跑」而非「**有**覆盖」——server 包 Linux 可编译，接上门禁后可覆盖，但当前无人跑。切片时的去重漏了它。
- **NaN 条目的 `timer.go:121` 半边** —— 已修（当前 `timer.go:133` 是接受式 `IsPlausiblePlayTime`，注释把德摩根陷阱写清楚了）。该条目的另两个洞（reader.go:57 / server.go:490）仍开着，见 §4。

---

## LOW / 观察项

11 条复核后实为 **10 个独立缺陷**——#1（effect.go:602）与 #7（effect.go:619）是同一处代码的两个面，标题与行号都不同，按「标题+位置」去重合并不掉。这是本轮计数近似性最直接的例证，切片里未必只有这一处。

结论先行：**没有一条该升级**。它们的共性不是「优先级低」，而是后果全部落在正确性/功能完整度上，无一条有稳定性含义。但等级低 ≠ 不修，末尾单列两条性价比不成比例的。

### A. 诊断信号已经存在（config.go:60、config.go:241）

这两条的 MED 全部押在「用户无从察觉」「零日志」「对着同一个版本号报不同 bug」上——**这个前提是错的**。核实 main.go:64-73：`config.Load()` 之后无条件打印开播横幅，:70 逐播放器打印 **resolved** 值 `播放器: kugou (offset=200ms poll=30ms)`，:72 打印 resolved `优先播放器: [wesing]`。两条发现的失败场景（老 config.yml 缺 `kugou-offset` 回落 200；注释掉 `- wesing` 反而仍优先）的结果，每次开播都怼在播控机控制台上。

config.go:60 的「双真源」还是一次误读。核实模板文本：`# wesing-offset: 0`（:326，注释）vs `cloudmusicv3-offset: 500`（:330，未注释）——这个区分是刻意的。DefaultConfig() 不带 per-player 值，正是让「注释掉 = 用全局」这条模板自述语义成立的哨兵层。按其隐含修法把 430 塞进 DefaultConfig()，用户注释掉 `kugou-offset` 将静默得到 430 而非全局值，**反而打破模板自己写的契约**。

且其修法方向与本仓优先级相反：加迁移/补键意味着 main.go:76 的强制自动更新期间，程序去改写主播已调过的 config.yml（注释、顺序、自留行都得保）。开播前被自动改配置，比 230ms 偏移危险一个数量级。**这条不仅不改，且不应按其方向改。**

唯一值得做的是 config.go:230/236/241/273/279 断言失败处补 `else { log.Warn }`（约 5 行），把静默丢弃变成告警。属可诊断性增强，不是修 bug。

### B. 同一处 bug 的两半，且两个证伪者各看到一半（effect.go:602 + :619）

这是我复核出的交叉结论。purelayerSession 的参数下发有两条路径，各丢一半：

- **会话存活 + gen 变**（:620-627）：`q, _, _, _, gen := c.sink.EffectCaptureParams()` 三个下划线丢弃 header/footer → **header/footer 失效**，且 :621 已把 gen 推平，不会重试。
- **会话重建**（订阅者断开重连）：:595-597 重注入 chromeSrc → header/footer 生效；但 :602 `eval(capSrc)` 撞上 :93 `if(window.__mbxCapStarted)return;` → **quality 失效**（页面不重载，:605-607 明写 reload 已移除）。

#7 的证伪者写「日常工作流（把参数固化进 OBS 源 URL）本身就会新建连接、天然正确」——对 header/footer 成立，对 quality 恰好相反：新建连接正是 q 失效的那个分支。两人各读了一半发现，各自得出「另一半没事」。合并后真相是两个旋钮**互斥失效，没有任何一条路径两个都对**。

仍不改行为：effect.go:585 注释明写「抓的是 canvas 像素与 chrome 无关」（已核实）。header/footer 点击性对 OBS 输出零影响，只作用于主播本机网易云窗口；quality 只影响 JPEG 编码质量 → 回环带宽/CPU，画面像素正确、不断流。

**但 effect.go:609 那行日志该修**：`log.Success("纯层捕获已启动（注入抓帧 q%d ...）", quality, ...)` 打印的是 :576 的**请求值**而非页面**生效值**。主播为省 CPU 调低画质、日志确认「q60 已启动」、实际仍在 q95 编码——这是本组唯一有间接稳定性含义的点：CPU 吃紧时主播的缓解措施无声失效，日志还把他引向错误方向。修法是 eval 后回读 `window.__mbxCapCfg.q` 再打印。

顺带：架构地图 §5「scale 递增 gen counter（forcing a capture restart）」对 screencast 成立（:745-749 走 conn.Close() 重建），对 **purelayer（默认模式）是错的**——地图该改。

### C. 触发条件从未被观测（mem.go:267、timer.go:108）

**mem.go:267**：核实 :254-270，`cbNeeded` 与 `unsafe.Sizeof(modules)` 之间确实**没有任何截断**，MSDN 语义（缓冲区不足仍返 TRUE、lpcbNeeded 为「所需」字节数）成立。不改的理由不是代码没错——是 amd64 下 8192 字节缓冲 = 1024 模块，触发需 32 位 QQMusic.exe 加载 >1024 个模块，约为实测最坏情况（重度 hook 的 CEF 应用 150-350 模块）的 3 倍。见末尾，这条我单独拎出来。

**timer.go:108**：首个可解析命中即返回、最低地址优先、只有词法校验（:91-99 冒号/空格位置 + isDigit）无语义判据、与兄弟代码 songinfo.go:50-56（注释明写「返回最高地址的有效结果（最新分配的数据）」）策略相反——全部属实。不改：核心前提「内存里同时存在多个 `NN:NN | NN:NN` 且旧歌的在更低地址」是**断言而非证据**。反向信号有分量：若以可观概率触发，症状（总时长错、进度条卡满、最小化后歌词冻死）在每个会话第 2 首起就刺眼可见，而上线至今无 issue、无 commit、无注释提及。

要坐实需要真机证据：在 FindSongDuration 补一行 log 打印 `len(results)` 与每个命中的完整 text + 地址，连续切歌（尤其第 2、3 首）看是否出现右半段不一致的多命中。**在拿到这个证据前不动它是对的**——照 songinfo.go 改成最高地址优先，本身也只是另一个无证据的猜。

### D. 影响有界，修法风险高于问题本身（fetch.go:332、cdp/client.go:130）

**fetch.go:332**：抢占场景在真实代码上复现（LRC 三行同文本 + YRC 两行 → 三行全错）。不改：影响面仅限卡拉 OK 填色动画，行文本与行时间由 LRC 驱动始终正确，被饿死的行优雅退化成纯文本。而 3000ms 容差是**必需**的（真实用例实测 LRC 214.26 vs YRC 211.79 漂移达 2470ms），换匈牙利/DP 要替换一个正在工作的启发式、冒回归 5 条已调好用例的风险。

低风险的那一半值得改：:343 `if diff < bestDiff` → `|| (diff == bestDiff && yrcTimeMs < bestKey)`。实测同一 tie 输入跑 2000 次得 1739/261 分布——**同一首歌重启两次可能得到不同的逐字时间轴**。这一行把「不确定」变成「确定」，不触碰任何已工作的用例。

**cdp/client.go:130**：核实 :126-140，:130 确实只有 `window[jname] = null`（保留槽位不 delete），:136/:138 两条路径确实连 null 都不置。不改：泄漏在**酷狗自己的渲染进程**里，约 110 字节/条，8 小时 ≈ 100MB，对 PlayerCap 稳定性零影响。原发现的三条放大论证都不成立：「V8 会把 global object 打入 dictionary mode」——V8 global 从诞生起就是 dictionary mode（GlobalDictionary + PropertyCell），机制不存在；「超时路径额外驻留等量闭包」——超时路径被 3s setTimeout 自我节流到 ~0.33/s 且与快路径互斥；catch 路径要 `external.SuperCall` 同步抛异常，那样歌词从第一次轮询起就完全不工作，是 fail-fast 不是静默劣化。

### E. 当前代码无缺陷，只欠注释（qrc_decrypt.go:18）

这条的**形态**与其他 10 条不同：它描述的不是一个失败输入或时序，而是一次未来的误编辑。任何微妙代码都具备这个性质。当前代码对照权威参考实现逐字节正确。

值得做的是硬化：S-box 偏离（S2 row1 缺 14 重 15 在 :18、S4 row3 缺 1 重 10 在 :28）是与 QQ 音乐客户端位级兼容的**承重前提**，出处是 wangqr/QQMusicDES——其自述实现的正是 QQ 音乐用的 "BUGGY DES"，S-box 与本仓库逐字节相同。在 :12 表上方加两行注释（「故意偏离 FIPS 46-3，复刻 QQMusicCommon.dll 的 buggy DES，改回标准值会解出垃圾，勿修」），并把文件头 :3-5 的错误归因（"bit representations"）改成真实原因。

但发现称文件头注释「在为这个改动背书」是误读：:3-5 的结论 "so we port the exact algorithm for byte-perfect compatibility" 是「别动」的告示——理由错了，结论指向正确。

### F. 可诊断性缺口，不是新增事故成因（main.go:521）

核实 :511-523：`cmd.Start()` 确实无 err 检查紧跟 `os.Exit(0)`。三条放大器全假：(1)「杀软隔离高频，正是 renameWithRetry 存在的理由」是 non sequitur——renameWithRetry 重试的是 `os.Rename` 的 sharing violation（AV 持读句柄），读句柄不会让 CreateProcess 失败，「被锁」≠「被隔离」；(2) 最可能的隔离时序已被上游拦住——.new 若在 close 时被隔离，rename 失败 → 回滚旧 exe → 打印手动下载 URL + `fmt.Scanln()` 阻塞让主播确实看得到 → exit(1)；要走到本路径，隔离必须精准落在 rename 成功到 cmd.Start() 之间的 ~1s 窗口；(3)「退出码 0，守护脚本不会重拉」——守护脚本不存在，`git ls-files` 无任何 .bat/.ps1/服务定义，这是双击运行的控制台程序。且 exit(0) 是既有刻意约定：:513-516 在 `os.Executable()` 失败时**已经**打印了 Error 却仍 `os.Exit(0)`。

真正残留的内核只有 main.go:646——见下。

### 该升级的：没有。该顺手修的：两条

无一条有稳定性后果，MED 撑不住。但两条性价比不成比例，不必等排期，下次碰到该文件时顺手修掉：

**1. mem.go:267**，一行：`if numModules > uint32(len(modules)) { numModules = uint32(len(modules)) }`。这是 11 条里**唯一后果是进程死亡**的（越界 panic，全仓库 `recover()` 0 命中，我 grep 过 → 直播中取词服务当场消失）。它是 LOW 是因为触发概率，不是因为后果——「低概率 × 进程死亡 × 一行修复」在 稳定 > 正确 的仓库里不该躺在待办里。建议用 min 截断而**不是**原 fix 提的 MSDN 两遍模式：两遍更「正确」（模块超限时仍能找到 DLL），但多一次动态分配和一次 syscall；截断的降级行为（漏找 DLL → ConnectQQMusic 返错 → qqmusic.go:48 的 2 秒重试循环继续转）已经足够好，且更简洁。

**2. main.go:646** 的 `io.Copy` err 检查，失败时 `os.Remove(canonicalPath)` 后 return（继续用当前名字跑）。这是 F 组三处丢 err 里**唯一会留下持久磁盘损伤**的点——磁盘满时拷出一个截断的 canonical exe，:655 启动它、:656 干掉唯一还能跑的原进程，且主播下次双击依然起不来。另两处（:521、:655 的 cmd.Start）修好也只是多一行日志，服务照样是停的。

其余 9 条：记档，不排期。

---

All key claims verified against current code. Note the audit's line numbers have drifted ~7 lines in `watchdog.go` since the recent fix commits — I'm using current ones below.

---

## 被证伪淘汰的（择要）

以下条目**已被逐条对抗性证伪，并经我回到当前代码复核**。记录它们的唯一目的，是让后来者别再「发现」一遍。凡标注为「故意」的，改动前请先读本节。

（注：切片数据里的行号有约 7 行漂移——`watchdog.go` 因近期修复提交位移。下列行号为**当前 HEAD 实测**。）

### 1. 「提权助手跨 UAC 边界从 %TEMP% 读目标路径」——冗余向量，非最薄弱环节

原发现（HIGH，架构地图列为第二大脆弱点）：`watchdog.go:476` 写 `kugou_cdp_patch_input.txt` → UAC → `:425` 提权侧读回 → `:359` `os.OpenFile(libcefPath, O_RDWR)` 零校验，可劫持成管理员任意路径文件破坏。文件名固定、无 nonce。

**机械事实全对，安全结论站不住。** 攻击前提是「同用户 medium IL 代码执行」。而 `launchElevatedHelper:488` 的 `lpFile` 就是 `os.Executable()`——UAC 要提权的正是**自己这个 exe**；而 `main.go:642` `ensureCanonicalName` 用 `os.Create(filepath.Join(filepath.Dir(exePath), canonicalExeName))` 把运行中的 exe 复制进**自己所在目录**——这是自管理便携 exe，其目录**按设计就是用户可写的**。所以同一个攻击者直接覆写那个 exe，UAC 弹的还是同一个程序、用户照样点「是」，拿到的是**管理员任意代码执行**——比「固定字节破坏」强得多，且无需赛跑。修掉 temp 文件不减少攻击者任何能力。

真相：这不是可修的边界，因为**这里根本没有边界**。UAC AAM 不是安全边界（微软自己的立场）。且旗舰例子不可达——运行中服务的映像被 loader 以 image section 映射、不授予写共享，`:359` 会直接 ERROR_SHARING_VIOLATION。

**残留的真问题（LOW，健壮性非安全）**：`CheckPatchStatus`（`:81`，含哨兵比对）只在 `:583`/`:617` 被调用，**两处都在 medium IL 侧**；真正落笔的 `patchDLLBytes:365-372` 是裸 `Seek`+`Write` 循环，**提权侧零校验**——守卫站在了边界的错误一侧。补一次哨兵读校验是 5 行，能挡住陈旧 input、路径 bug 之类**非安全**误伤（盲写 93MB 偏移进错文件 = 无备份无 unpatch，对直播稳定性有实价）。但这是加固，不是提权漏洞。

### 2. 「Unpark 在 hwnd==0 前 clearState」——故意，且反方向更危险

原发现（HIGH/MED 两处重复上报，架构地图列为严重问题）：`park.go:406-408` `parkedMem = false; clearState(); if hwnd == 0 { return false }`，`RestoreOrphaned:420` 先伪造 `parkedMem=true` 再调 `Unpark()`，故启动期救援必定删掉唯一救援记录。

**整条因果链断在第一环：playercap 根本不会拉起网易云。** `watchdog/process.go:75` `if !hasNetease { return false, nil }`——唯一的 `exec.Command(exePath, LaunchFlags...)` 在 `hasNetease == true` 分支内，语义是「已在跑但没带调试参数 → taskkill 后带参重启」。「几秒后 watchdog 拉起网易云」是臆造的。

更糟的是证据引错了播放器：原文写「`main.go:184` `go kp.Start()` → `:187` RestoreOrphaned」，但 `kp` 是**酷狗**，网易云是 `cp`。发现者把酷狗的启动器当成了网易云的启动器——这正是误判的来源。

真相：`hwnd==0` ⇔ 网易云没运行 ⇔ **屏外窗口根本不存在，没有窗口需要救**。真实救援场景（playercap 崩溃、网易云仍泊车运行）走的是 `hwnd!=0`，还原正常执行。且反方向才制造真 bug：没有任何其它代码清这个文件，一条指向已死窗口的孤儿记录一旦保留就永久存在，之后每次启动都会把陈旧 `savedPlacement` 拍到当时存在的任何网易云窗口上——「每次开 PlayerCap 网易云就跳回旧位置」，永不自愈。原始提交 `d2d5953` 把 clearState 显式写在 `hwnd==0` 分支**里面**，`df7a45d` 重构上提去重，语义未变；注释「窗口没了，状态已清」就是这个设计的遗留痕迹。

### 3. 这两条说明什么——该信任什么

**两条都来自架构地图，两条都错，且错法相同：地图给的是「叙事」，不是代码。**

- 「提权助手」那条：地图排出「第二大脆弱点」的名次，靠的是**边界看起来很吓人**（跨 UAC + temp 文件 + 零校验），而没有问「攻击者到这一步已经拥有什么」。安全结论**不能从局部代码形状读出来**，必须先钉死威胁模型的起点。
- 「Unpark」那条：地图讲了一个流畅的故事（启动 → watchdog 拉起网易云 → 窗口还没出现 → 记录被删），链条每一环都合理，**除了第一环在代码里不存在**。

这不是孤例。同一批数据里，条目 5（`prior-player`）的发现者引用「地图第 77 条 wesing loading 语义」来论证抢占 thrash，而真实代码（`wesing.go:135-152`）**与地图该条相反**，且那段 NOTE 恰恰是踩过这个坑后专门修的；`instruction.md:97`「播放/加载时立即抢占」同样是过期文档。

**结论：地图和 instruction.md 是索引，不是事实来源。** 任何 HIGH 定级在写下之前必须做两件事：(a) 把因果链的**第一环**拉回代码验证——绝大多数假 critical 死在这里，不是死在细节；(b) 跑一次 `git log -S` / `git blame`——本仓库大量反直觉设计带着解释性注释和踩坑提交（`df7a45d` harden、`273c04d` fix #29、`d8c34ec` fix #8、`0b93e8e` park 冷却），**看起来像疏漏的地方，多数是有据可查的取舍**。

顺带一个可校准的信号：本轮 HIGH 里被推翻的比例相当高，而**推翻它们的依据几乎总是「同一个包里另一行代码」**——不是深奥的运行时行为。这说明假阳性的成因是**读得不够宽**，不是读得不够深。

### 4. 「setEffectParams 在 Upgrade 之前调用」——诊断错了缺陷本身

原发现（MED）：`server/effect.go:316` 在 `:318` Upgrade 之前调用 `setEffectParams`，任意网站一个 GET 即可全局降级直播画质。

**「在 Upgrade 之前」与可利用性无关。** `server/server.go:104` `CheckOrigin: func(r *http.Request) bool { return true }`（已复核）——WebSocket 握手不受同源策略/CORS 约束，任意网页 `new WebSocket('ws://127.0.0.1:8765/...?quality=1')` 的 Upgrade 会**成功**，`setEffectParams` 照样执行。把 `:316` 挪到 `:318` 之后，攻击面一分不减，只会让人以为修好了。**标题、位置、隐含修法三者全错。**

且一半后果是假的：默认模式是 purelayer，`effect.go:577` 注释写明「scale 对纯层无意义（直读 canvas 原生分辨率，不缩放）；仅 quality 影响 JPEG 编码」，`:576`/`:619` 用 `_` 显式丢弃 scale——`scale=0.1` 在默认部署下是**完全的 no-op**。

真相：真正的边界是 `server.go:104` 的 CheckOrigin 恒 true + hub 级全局参数无客户端身份判定，与报告位置不是同一处。要治改那里，或把参数改为 per-connection。以「排序缺陷」形式进报告会把工程师引向一个无效修复。

### 5. 「switchSkipHashes 导致全体根订阅者被永久抑制」——抑制是一次性的

原发现（MED）：`server.go:180` 基于「打算发送」而非「实际发送」构建 sentHashes，单个慢订阅者丢帧 → 全体根订阅者被永久抑制。

`router.go:113-115`（已复核）：

```go
if oldHash, ok := r.switchSkipHashes[evt.Type]; ok {
    newHash := hashEventData(evt.Type, evt.Data)
    delete(r.switchSkipHashes, evt.Type)   // ← 无条件删除，先于比对
```

`delete` 在比对**之前**无条件执行 → 每个事件类型至多抑制 **1 条**。「A 永远收不到」是把「一次性去重令牌」误读成了「持续抑制窗口」。且发现点名的 cloudmusicv3 自带自愈：每首歌必发两条 song_info_update（`cloudmusic.go:277` 无 base64 + `:285` 封面补发），`dedup.go:18` 的哈希**显式**把 `msg.CoverBase64 != ""` 编入，注释明写「异步封面补发含 base64，哈希不同，不会被 switchSkip 抑制」——所选例子恰恰是最不可能出现该后果的那个。

真相：`server.go:169` 注释措辞确实不准（是「打算发送」）。**只改注释，不动机制**——per-subscriber 抑制需重构抑制模型，改动风险远高于收益，违反「稳定 > 正确」。

### 6. 「prior-player 全注释掉 → 静默保留 [wesing]」——护栏已在，且是踩坑产物

原发现（HIGH）：`config.go:242` YAML `null` 走不进 `[]interface{}` 断言 → 静默保留默认 `["wesing"]` → WeSing 挂着待机/loading 反复抢占 → 歌词窗口反复跳。

解析机制属实（`prior-player:` 后全是注释 → `value=<nil>` → 断言失败 → 保留默认），**但这是唯一站得住的部分**。事故场景在真实代码里被专门的反 thrash 护栏挡死（`wesing.go:135-152`，已复核）：

```go
case proc.PhaseLoading:
    // NOTE: 不向 router 发送 loading 事件。
    // wesing 的 loading 可能持续很久（超过 prior-player-expire），
    // 期间无音频/歌词输出，若触发优先组抢占会导致普通组被中断并出现空白窗口，
    // loading 超时回退后又会再次切换，观感不佳。
```

**发现描述的那个事故，代码里已经踩坑后专门修掉了。** 待机同理：`wesing.go:126` 发 `waiting_song` → router 归一为 `idle` → 不抢占。且「诊断端点也跟着撒谎」是错的——`main.go:124` 把 `prior-player` 灌进 `/service-status`，如实显示 `["wesing"]`，启动时控制台也打印。

真相：`null → 用默认` 是标准配置语义，且 `mergeYAML` 每个分支都是同一形状。最多一条 LOW 文档问题：模板该加一行「不需要优先播放器请写 `prior-player: []`」。动解析逻辑反而引入「null 该不该等于空」的新语义风险。

### 7. 「MergeTlyric 要求毫秒级精确相等」——零容差是数据结构决定的

原发现（MED）：`fetch.go:304` 精确相等且零日志，翻译对不上就整首静默消失；而 MergeYRC 容忍 3000ms + 模糊文本。

核心论据事实错误：MergeYRC 全段（`fetch.go:310-353`）**同样零 log**，日志不对称不存在。更要紧的是**它暗示的修法本身不安全**：MergeYRC 的 3000ms 窗口安全，仅因被 `fetch.go:336` 的 `player.SameLyricText()` 文本闸门把守（该 helper 注释 `player.go:261-264` 明写用途是对齐 line-level 与 word-level 歌词，yrc 专用）。而**译文按定义就与原文文本不同**，这道闸门对 tlyric 根本无法存在——只剩纯时间就近匹配就会把**错误的译文**静默挂到某行上。直播 overlay 上「显示错译」严格劣于「不显示译文」。

真相：`applyTlyricMap` 匹配的是网易云**自己的** lyricLines 与**自己的** tlyricLines，同出一个 `async:lyric` Redux 切片。网易云自身的双语渲染就依赖这同一个时间键配对；若真漂移，网易云自己先炸。精确匹配是在**复刻上游契约**。对比 yrc 有真实漂移样本进测试（`yrc_test.go:42/71/84`）——**有坑才有容差，无容差正说明无坑**。

### 8. 「park 后 fg 上升沿被 1200ms 冷却吃掉 → 特效层永久冻结」——泊车态窗口不是 iconic

原发现（HIGH）：`effect.go:324-328` 冷却未过时仍无条件推进 `wasForeground`，上升沿被吃掉后永不重来 → 帧被永久门控。

沿丢失本身真实，**但「特效层永久冻结」的前提是假的**。`park.go:387` `off.ShowCmd = swShowNoActivate`（已复核）——SetWindowPlacement 会**取消最小化**，随后 SetWindowPos 只移屏外、不激活。故泊车态下 `IsIconic` = **false** → `effect.go:300-304` rawMinimized = **0**（不是发现说的 1）→ `:231` ingestFrame **帧照常广播**。这恰恰是 park 的设计目标态：屏外保活、特效继续出帧。OBS overlay 完全健康。

真相：残留问题只有——strategy=park 时主播 1.2s 内反悔，第一次点任务栏窗口不飞回，需再点两下。期间特效帧照常流。LOW 级 UX 瑕疵。且前提极窄且非默认：`config.go:67` 默认 `fadeout`，`main.go:81` 在 Win11 强制降级回 fadeout（均已复核）——park 只在「Win10 且手动开启」时存在。

### 9. 「SSE 订阅者收不到 player_clear，停播后永久定格」——文档化的 API 契约

原发现（MED）：`server.go:302` 按 eventTypes 过滤，`/lyric_update-SSE` 结构性地永远收不到 player_clear/player_switch/clear_song_data。

机制描述对，**但这是故意的**。`doc/API_RESPONSE_EXAMPLES.md:985`：「服务端**不会**主动发送清空歌词的消息。前端应根据以下事件自行决定何时重置显示」。player_clear/player_switch 被设计成 **WS 根订阅者专属控制面事件**（doc:738/773 两节标题均为「（仅根订阅者收到）」）。SSE 是单类型数据流，端点名就是事件名；放 player_clear 进去反而**破坏契约**。

且失败场景的消费者不存在：全仓库**零 `EventSource`**。真正的 OBS overlay 是 `lyric_page.html`，走 WebSocket，且已正确处理 player_switch(:1865) 与 player_clear(:1898-1908)。「用 SSE 做 overlay 源」是发现自己假设的用法。

### 10. 「digest 为空时跳过 SHA 校验」——有文档记载的向后兼容决策

原发现（HIGH）：`main.go:413` `if expectedDigest != ""` 无 else 无告警，截断的 exe 直接覆盖运行中程序。

引入该校验的提交 `adfc12f`（#19）同时提交了 `SHA256_VERIFICATION.md`（后被删除，`git show adfc12f:SHA256_VERIFICATION.md` 可读回），「向后兼容性」节原文：「如果 API 响应中没有 `digest` 字段，会跳过验证（仅记录警告）」。**空 digest 跳过是明确决策**，不是疏忽。且该文档给出的真实生产响应样例里 exe 资产**带 digest**——整条链需要一次从未发生过的网关回归才能启动。

另：发现称「镜像返回缓存了一半的对象 → io.Copy 无错返回」不成立——Content-Length 声明 N 而交付 <N 时 Body.Read 返回 `io.ErrUnexpectedEOF`，`main.go:408-411` 会删除 destPath 并报错，**exe 根本不会被动**。

真相：唯一残留是文档承诺的「仅记录警告」从未实现（`:413` 无 else）——LOW 级可观测性缺口，加一行 `mainLog.Warn` 即可。

### 11. 「FindLyricHost 取 results[0]，issue #8 疑似根因」——归因完全反了

原发现（HIGH）：`finder.go:59` `hostAddr = results[0]`（最低地址）且无候选回退，扫描确定性使失败永不自愈。

**「issue #8 根因」被仓库历史直接证伪。** `git log --all --grep="#8"` 全仓只有一条：`d8c34ec [FIX] #8 内存中有多个json 现在会进行匹配来确认歌手`，改的是 `lyric/songinfo.go`，与 `finder.go` 无关。issue #8 = 歌手取错，2026-03 已修。而发现引为反证的 `songinfo.go:34-56`，**恰恰就是 issue #8 的修复代码本身**——把某 bug 的修复代码当成「另一个 bug 是该 issue 根因」的证据。

「零日志无限循环」也是假的：`logger.go:36-38` 的 Detail 是无条件 `log.Printf`，无 level 门控；`FindLyricHost` 每次调用固定打 6 行，其中 `:61` 直接打印选中的 `LyricHost 实例: 0x%08X`——1 秒一轮的重试是**日志刷屏**，不是静默。「确定性 → 永不自愈」同样不成立：AOBScan 每轮 ReadProcessMemory 读**活进程**、每轮重新枚举 region，WeSing 是持续分配/释放的 UI 进程，重试之间堆布局在变。

真相：`results[0]` 无候选回退 + `wesing.go:254` 吞掉 err，是真实但 **LOW** 的健壮性缺口（加个 for + LoadLyrics 试探即可）。注意此包 CI 永远跑不了（Windows-only），改动需实机验证。

### 12. 「CDP 三处 WriteJSON 无写超时 → 取词 goroutine 永久锁死」——同步读闸门排除了堆积

原发现（HIGH）：`cdp/client.go:331/391/434` 全无 SetWriteDeadline，渲染进程挂起时写请求堆积填满 TCP 发送窗口。

三处确实没有 SetWriteDeadline，**但堆积前提被代码结构排除**：`client.go:316-317` `c.mu.Lock(); defer c.mu.Unlock()` 包住整个写+读周期，`:331` 写 → `:336` SetReadDeadline(5s) → `:339` ReadJSON，函数在读完成或超时前不可能返回；任何读错误置 `c.closed = true`。真实时序是：写约 4KB → 5s 后读超时 → closed → 重连。**在不 drain 的对端面前，拆会话前总共只推了一条约 4KB 的消息**，而 Windows 环回 SO_SNDBUF ≥64KB。写要阻塞必须缓冲**已经**满，本代码做不到这件事。

且速率算错：`config.go:63` Poll=30，但 `cloudmusic.go:155-158` `if pollInterval < 50ms { pollInterval = 100ms }` → 实际 10/s，不是 33/s。「effect.go 有写超时所以团队知道要设」也把事实弄反了——`effect.go:359` 是**例外**，`:247/450/489/555/655/665/783` 全都没有；`:353-365` 是三条连发中间不读的**不同结构**，那里写阻塞确实可达。

真相：唯一残留是 `client.go:244` 用 `http.DefaultClient` 无超时——但它由浏览器进程的 HTTP 线程服务，不是渲染进程，发现自己给的触发条件卡不住它。顺手加 `http.Client{Timeout}` 是 LOW 级卫生措施。

### 13. 「0xFFFFFFFF 是内存中最常见哨兵 → NaN 毒化事件流」——前提有正面反证（2026-07-16 追加）

原发现（MED §4，见上文「### 4. NaN」）：内存里读到的垃圾会是 NaN，透过拒绝式检查毒化事件流与缓存。

**这条不是「无证据」，是有反证。** 仓库内唯一的经验观测（`reader.go:60` 注释「垃圾数据经常 `time=0`」）说明看到的是**零填充内存**、不是随机位；零填充重解释为 float32 **恒为 0.0，永不可能是 NaN**。另实测常见 Windows 堆填充模式（`0xCDCDCDCD` / `0xFEEEFEEE` / `0xCCCCCCCC` / `0xDDDDDDDD` / `0xBAADF00D` / `0xABABABAB`）**无一是 NaN**，且它们只存在于 debug CRT，而 WeSing 出的是 release。随机 32 位字是 NaN 的概率仅 0.39%。

四条产 float 路径逐个判决后**无一可达**，其中两条是**结构性**的：qqmusic 读的是 **uint32 不是 float**（`ReadFloat32` 是零调用死代码，「qqmusic 读 float」这个印象就是它造成的）；cloudmusic 走 encoding/json 而 **JSON 标准不支持 NaN**。详见上文 §4 的结案框。

**别再「发现」一遍**：想动 `kugou.go:364` 的 `_`（ParseFloat 丢弃 err）或 `:591` 的德摩根拒绝式是可以的，但理由是「静默失败 + `AGENTS.md:326` 的接受式规矩未一致应用」，**不是「修 NaN 可达性」**——那条链的首环（酷狗自己吐出字符串 `"NaN"`）同样未被证明，而最可能的 JS 路线（`JSON.stringify(NaN)` → `null` → 空串 → ParseFloat 报错 → 0）是安全的。

---

**关于本节的一条诚实说明**：上述证伪我复核了其中的承重环节（`process.go:75` 早退、`park.go:387` swShowNoActivate、`park.go:406-408` 顺序、`server.go:104` CheckOrigin、`router.go:113-115` delete 先于比对、`wesing.go:135-152` NOTE、`watchdog.go:488` lpFile=os.Executable()、`main.go:642` exe 目录可写、`patchDLLBytes:365-372` 无哨兵），均成立。**证伪者本身也会想当然**——第 11 条对「堆分配模型」的推理、第 8 条对 float32 精度的论证，都属于「合理但未实测」；它们不改变结论（因为结论另有承重点），但如果未来有人要在这些地方翻案，该重新验的是那部分，不是我复核过的那部分。

---

以下为两节最终产品。

## 发现的模式

### 模式一（证实，且合并）：诊断做完了，闸门没接上

候选 (a) 的「守卫半边」与候选 (c) 是**同一个病**，应合并成一条。理由是它们的失败结构完全一致：代码先算出「这不对」，然后把这个结论丢掉，继续走 happy path。信号是 `error`、是 `bool`、还是 `select` 的 `default:`，只是载体不同。

已核实的实例：

- `watchdog.go:81` 具名返回 `canAutoFix`，`:100` 赋值，`:80` 注释写明契约。全仓 grep 只有 4 处命中（注释 ×2、声明、赋值），**消费者为零**：`:576` 与 `:610` 两个调用点都写 `allPatched, _, err :=`。后果不是「少一道校验」——我确认了 `libcefPatches[0].data` 与 `p1Patched` 逐字节相同，所以盲写 Patch1 后哨兵被自己改写成「版本已确认」，损坏态与正常态从此不可区分。
- `cloudmusic.go:325` `cdpLyricsOK = true` 排在 `:326` `ParseLRC` **之前**。解析出 0 行时，`:350`/`:376`/`:404` 三条兜底同时失效。同一函数 `:300` 的 API 路径写的是 `err2 == nil && len(apiLyrics) > 0`。同一个函数里，两条路径一对一错。
- `reader.go:145` `isValidLyricText` 是一个写完的乱码校验器，**零调用方**。`mem.go:773` `ReadSSOString` / `mem.go:802` `extractString` 是「先分堆/栈、再在堆分支内 reject」的正确写法，**零调用方**；而活代码 `mem.go:1005` `extractSSO` 恰好犯了 `ReadSSOString` 已经规避掉的错。
- `server.go:161-164` fan-out 的 `select { case sub.ch <- wsEvt: default: }`：通道满即丢事件，无计数无日志。对照 `effect.go:171-181` 是「满则丢旧、写入最新」——对帧正确，对事件才是裸丢。
- `config.go:347-351` `generateDefaultConfig` 只有 `if err == nil { log.Info(...) }`，无 `else`。写失败时全局零输出。
- `server.go:490` `if err := conn.WriteJSON(evt); err != nil { return }`：把「NaN 序列化不了」和「TCP 断了」当成同一件事，而且两件都不 `Close()`。

本轮已修的三条（api.go:205 QRC 解密失败被吞、park.go:181、timer.go:121）是同一形状，说明这不是零星失误。

**这条的诊断价值**：本仓库不缺检测能力——它到处都在检测。缺的是**从检测到拒绝的那根线**。所以「加校验」类的修复建议基本都是浪费，校验往往已经写好了，甚至已经跑过了。要查的是「这个 bool/err 有几个读它的地方」。

### 模式二（证实，但必须降级，不要和模式一混为一谈）：投机取数

候选 (a) 的另一半是独立的、危害低得多的病：取了数据但消费方从未存在。

- `api.go:201` 请求 `"trans": 1`，`api.go:45`/`:160` 声明了 `Trans` 字段。全仓 `.Trans` 零读取（`kugou.go:556` 的 `TransParam` 是同名不同物）。
- `mem.go:770` `SliderVal` 声明、`:1170` 赋值、零读取。注意：**这条团队自己已经发现了**——`qqmusic.go:67` 的注释白纸黑字写着「它的产出 SliderVal 全仓库零消费方」，并据此关掉了 AOB 滑块探针（#39）。它是先例，不是活的病例。
- `reader.go:83-108`：逐 `CharElement` 遍历（注释 `:81` 明确「中文每个 CharElement 是单个汉字」），每次迭代都精确知道「这是第 c 个」，但只把 rune 追加进同一个 `text`，边界在 `:112` `string(text)` 处消失。**修正原描述**：这里没有「扔掉时间轴」——代码只读了 `CharElement+0x00` 的 `RenderData` 指针，压根没探过里面有没有时间字段。扔掉的是边界，不是时间。

只有 QRC 那条真正够格，且它比原描述更值得写：

- 删除点是 `api.go:312` `qqCharTimingRe.ReplaceAllString(rawText, "")`（`:268` 只是编译正则），删掉的是真实的 `(charTs,charDur)`。
- **消费方是存在的**：`player.go:106-122` `BuildLyricsDetailed` 在**共享 player 包**里，对所有播放器生效，`lyrics_detailed` 是 `doc/openapi.yaml` 与 `API_RESPONSE_EXAMPLES.md:198` 的书面公开 API。
- 最刺眼的一步：`API_RESPONSE_EXAMPLES.md:249` 写着「其他播放器的 `text_detailed` 始终为 `{}`，`lyrics_detailed` 始终为 `[]`」。**扔掉之后，文档把「扔掉」追认成了契约。** 这是本轮最值得记住的一条：一次删除，经由文档，变成了永久的产品事实。

### 模式三（部分证伪）：不是「首个命中即胜」，是「没定义什么叫最好，就当第一个是最好」

原描述把 `timer.go:108` 列为病例，**这条站不住**。`timer.go:100-110` 是 `FindSongDuration`，它先验证 8 个数字位、再要求 `duration > 0`，然后才返回——那是「首个通过真实校验者」，是正确写法。更要命的是 `timer.go:41-46` 恰恰是本仓库**最好的对照组**：`for _, hitAddr := range results { if addr, ok := validateTimeAddr(...); ok { return } }`。把对照组当病例列，会把修复者引向错误方向。

病的真正形状是「候选集合无序或排序语义无关 + `[0]` + 零校验」：

- `finder.go:59` `hostAddr = results[0]`，上方只有 `len(results) == 0` 一个检查。`memory.go:454-468` + `:496` 保证 `results` 按地址升序且**逐字节扫描无 4 字节对齐**，故 `[0]` 恒为最低地址命中。
- `router.go:247` `for name, ps := range states` 构建 `activeNames`，`:267`/`:308` `target := activeNames[0]`。`grep -rn "sort\." server/` 零命中。
- `server.go:590` 与 `:595` 两个 `range s.playerStates` 取首个。
- `kugou/lyric/lyric.go:121` `c := sr.Candidates[0]`（原始审计未覆盖）。

而写对的地方，是因为**显式定义了「最好」**：

- `songinfo.go:50-56`：注释「返回最高地址的有效结果（最新分配的数据）」+ 从 `len(results)-1` 倒序 + 逐个 `tryParseFromSongNameAddr`。
- `kugou/lyric/lyric.go:180-186`：`// Rank: official desc, delta asc, score desc` + 显式比较循环。**与同文件 `:121` 的裸 `[0]` 相隔 60 行。**
- `handleServiceStatus:529` 用确定序切片 `PlayerSupport` 遍历，而 60 行外的 `resolvePlayer:590/595` 用 `range map`。

所以规则不该是「不要取 [0]」——`timer.go:41` 和 `lyric.go:181` 都在取第一个，都是对的。规则是「取之前必须能说出你按什么排序、按什么校验」。

### 模式四（新，我认为这是本轮最值钱的一条）：正确的那份就在隔壁

几乎**每一条**发现都有一个在同仓库、常常在同文件、有时相隔几十行的正确实现。这不是巧合的密度：

| 错的 | 对的（同仓库已有） |
|---|---|
| `fetch.go:62/118/169` 裸 `http.DefaultClient`；`api.go:149/222/443` 裸 `&http.Client{}` | `kugou/lyric/lyric.go:25`、`kugou.go:530`、`watchdog.go:67`、`cover.go:23`、`main.go:238/342/530` 七处全设 Timeout |
| `wesing.go:204/222`、`cloudmusic.go:283/285`、`qqmusic.go:202/207` 封面 goroutine 无守卫 | `kugou.go:226-229` coverCancel + 四处 `ctx.Done()` |
| `finder.go:59` 无序无校验取 `[0]` | `songinfo.go:51` 倒序+校验、`timer.go:41` 逐个校验 |
| `cloudmusic.go:325` CDP 路径无 `len>0` 守卫 | `cloudmusic.go:300` API 路径有（**同一函数**） |
| `server.go:606-608` 锁外读 `PlayerState` | `buildInitEvents:215-216` `defer s.mu.RUnlock()` |
| `wesing.go:41`、`kugou.go:133` poll 无下限 | `cloudmusic.go:156`、`qqmusic.go:93` 有下限 |
| `kugou.go:306` 快路径不发裸 SongInfo | `cloudmusic.go:277`、`qqmusic.go:152`、`wesing.go:189` 三处都先发（`cloudmusic.go:276` 还写了注释解释为什么） |
| `mem.go:1005` `extractSSO` 回退而不拒绝 | `mem.go:773` `ReadSSOString` 拒绝（死代码） |
| `resolvePlayer:590/595` range map | `handleServiceStatus:529` 确定序切片 |
| `timer.go:121` 拒绝式（已修） | `wesing.go:265` 接受式 |

**本仓库自己已经写下了这条模式的最佳注脚**——`timer.go:130-135` 的注释：

> 抽成共享函数是为了消灭拷贝漂移：调用方 wesing.go 在校验缓存地址时本来就写的是接受式（正确），而这里写成了拒绝式（错误）——同一个判定两份拷贝、一对一错。

而 git 史证明这不只是「拷贝漂移」，更是**修复不回灌**：`a4957aa0`（标题「[feat]获取封面&尝试修复歌手不准确」）的 diff 把 `songinfo.go` 的注释从「返回第一个有效结果」改成「返回最高地址的有效结果（最新分配的数据）」——即「取第一个会拿到过期数据」是**在 WeSing 这个进程上实测踩出来的、并被显式修掉的 bug**。`finder.go:59` 是同一个包、同一类扫描、同一种堆，至今停在 2026-03-18 的原始写法。同样地，`2a13ec3`（「stop retry loop when installation not found」）只补了 install-not-found 一个口子，UAC 被拒的同型无界重试原封不动。

**这条的价值**：它把「审计」变成了「可执行的搜索」。不需要理解业务，只要问「这个仓库里同一件事有几种写法」，就能机械地找出剩下的洞。也意味着单条修复的性价比被系统性低估了——每修一条，应该顺手 grep 出 2-4 条同型的。

### 模式五（新）：状态只进不出——这里没有任何东西会过期

这条解释了一个否则无法解释的巧合：**每一条 HIGH 的伤害描述都是同一句话**——「持续整首歌 / 整场直播、无日志、无自愈、只能重启进程或手动刷新 OBS 源」。

因为置位有路径，复位没有：

- `normalLastPlaying` / `priorLastPlaying`：唯一写入点是 `router.go:179`（playing）与 `:188`（loading）。paused(`:190-196`)、idle(`:198-204`)、`clearActivePlayer(:350-363)`、`forceGroupInert(:388-394)` 全都不清。**永不复位。**
- `expireGroupPlayers(router.go:367-385)`：只对 `loading` 和 `paused` 做超时，**`playing` 永不过期**。所以一个卡在网络 I/O 里的 player 变成 `activeNames` 里的永久幽灵。
- `cdpLyricsOK`：置 true 后整首歌不复位。
- `ps.SongInfo`：无 TTL，只有 `EventClearSongData` 能清，而 kugou 从不按曲发。
- WS 订阅者：写协程 `return` 后连接不 `Close()`、读循环无 `SetReadDeadline`、全仓 grep 无任何 `SetPongHandler`/`WriteControl`。僵尸永久在册，还被 `wsCount():722` 计入在线。
- `effectHub` 的 quality/scale：全局单例，最后一个连接者独裁，且**最后一个客户端断开后不恢复默认**。
- `qqmusic.go:117` `lastName = meta.Name` 排在守卫之前，于是 `:133` 注释承诺的「等待下次轮询」在结构上不可能发生。

还有一层反直觉的因果，值得单独指出：**这个仓库的防御性写法，把「崩溃」换成了「永久静默错误」。** `ReadProcessMemory` 失败返回零值而不 panic、`select default:` 丢弃而不阻塞、`log.Warn` 后继续而不退出、`net/http` 的 recover 只断一条连接——每一处单看都是「稳定优先」的正确取向。但合起来的效果是：系统**永远不会用崩溃告诉你出事了**，它只会安静地、持续一整首歌地显示错的东西。按「稳定 > 正确」的排序，一个会崩的 bug 反而比这些好——崩了会被发现。

**所以「稳定 > 正确」在本仓库需要一条脚注**：稳定不等于不崩。对直播中间件，「卡住且没人知道」是比崩溃更严重的稳定性事故，因为它没有恢复路径。当前的优先级排序正在被用来为静默降级辩护，这是它被误读的方式。

---
