# Metabox-Nexus-PlayerCap 接手 Roadmap

> 编制时间：2026-07-15 · 基线 commit `04a8676` + 本轮未提交的工作树改动
> 面向对象：即将接手本仓库的资深工程师
> 所有结论带 `file:line` 或 issue 号。凡证据不足处，明确标注「需运行时验证」及验证方法。

---

> **这是 2026-07-15 的一次性快照**，基于当时的 11 个 open issue。issue 状态以 tracker 为准；
> 本文的价值在于「每个 issue 对照代码核实后的结论与依赖关系」，不在于状态本身。
> 硬化审计的发现见 doc/hardening-notes-2026-07.md。


## 1. 现状判断

**这个仓库最大的问题不是 bug，是子系统没有能力说「我干不了这活」——于是每个降级点各自发明一套反应，其中三处对外撒谎。** QQ 遇到未知版本时套用 22.16 偏移继续解引用（`player/qqmusic/mem.go:352`），对象的 `m.version="22.31"` 而 `m.offsets` 是 22.16 的；KuGou 已经正确算出 `canAutoFix=false` 然后一个 `_` 扔掉（`player/kugou/watchdog/watchdog.go:576`），继续 UAC 提权盲写 93MB DLL；`main.go:85` 把 park 降级成 fadeout，`main.go:126` 上报的却是未降级的 `cfg.EffectStrategy`。

**第二大问题是「已经拿到手的数据被主动删掉」，而文档对外宣称这些能力已经存在。** QQ 逐字时间轴在 `player/qqmusic/api.go:268` 被一行 `qqCharTimingRe.ReplaceAllString(rawText, "")` 销毁；QQ 翻译在 `api.go:154` 已请求 `"trans":1`、`api.go:35` 已解析成 `Trans` 字段、全仓零读取；而 `README.md:132-133` 写着「各播放器支持逐字解析 / 音译」——四个播放器里逐字 1/4、翻译 1/4、音译 **0/4**。

**第三，地基刚刚被修好，时间窗正在打开。** 本轮已实测确认 `GOOS=windows go vet ./...` exit=0、`gofmt -l .` 零输出——这是本仓库历史上第一次两者同时为绿。而四条 workflow 至今**无任何 `pull_request` 触发器、无 `go test`/`go vet`/`gofmt` 门禁**（已核实：`build-windows.yml:3-6` 与 `release.yml:3-4` 均为 `push` 触发）。**门禁首次具备可落地条件，这个窗口不抓住，两周内会重新变红。**

---

## 2. 分期

### P0 — 焊死地基（本周，全部 S，全部 CI 可验）

已完成的部分**不要重排**：

| 事项 | 状态 | 证据 |
|---|---|---|
| `.gitattributes`（`text=auto eol=lf` + `.syso/.ico/.png/.exe` 显式 binary） | ✅ 已完成（未提交） | 工作树 `?? .gitattributes`；此前 `core.autocrlf=true` 导致 `gofmt -l` 24 个里 22 个是 CRLF 假阳性；`git ls-files --eol` 证明 index 本来就 100% LF，修复零 blob 变更 |
| 5 处真实 gofmt 违规（`effect.go` / `wesing/proc/memory.go` / `server/effect.go`） | ✅ 已完成 | 实测 `gofmt -l .` 现零输出 |
| 4 处 vet 违规 | ✅ 已完成 | 实测 `GOOS=windows go vet ./...` exit=0 |
| `player/qqmusic/mem_test.go` + `player/kugou/watchdog/watchdog_test.go` | ✅ 已完成（未提交） | 两包此前零测试；覆盖包数 5→7 |

**P0 剩余动作：**

**P0-1｜提交上述工作树改动。** 现在有 9 个 modified + 3 个 untracked 悬在工作树上。这是全场最高优先级——地基修好了但没入库，等于没修。

**P0-2｜加 CI 门禁。** 新增 `.github/workflows/check.yml`，`on: [pull_request, push]`，三步：
```bash
gofmt -l . | tee /dev/stderr | (! read)
GOOS=windows GOARCH=amd64 go vet ./...
GOOS= GOARCH= go test ./player ./player/cloudmusic/lyric ./server ./config
```
注意两个坑（均已核实）：
- 现有 workflow 在 **job 级**设了 `env: GOOS: windows`（`build-windows.yml:12-13`），跑在 Linux self-hosted runner 上（`build-windows.yml:10` `runs-on: self-hosted`，`release.yml:14` `[self-hosted,Linux]`）——所以 `go test` 在这个 env 下**架构上无法运行**。新 workflow 必须显式 `GOOS= GOARCH=`（照抄 `build-windows.yml:84` 的既有写法）。
- `go vet` 必须带 `GOOS=windows` 才有意义（大部分代码带 Windows API）。

**P0-3｜config 双真源 drift test（S，约 30 行）。** 已核实 `config/` 目前**零测试文件**，且 Linux-buildable、CI 跑得动。
- 根因已坐实：`config.go:60-71` 的 `DefaultConfig()` 里 `Players: make(map[string]*PlayerConfig)` 是**空的** → `GetPlayerOffset`（`config.go:74-78`）回退 `c.Offset=200`；而模板 `config.go:330/335/339` 写的是 `cloudmusicv3-offset: 500` / `qqmusic-offset: 400` / `kugou-offset: 430`。
- 后果：`Load()` 只在文件缺失时才写模板，文件存在就只走 `mergeYAML`（只覆盖物理存在的键）→ **在 `3bb0453` / `49133ae` 之前装的用户，这三个播放器永远跑 200ms，升级多少次都好不了。**
- 修法：解析 `DefaultConfigContent()`，逐键断言与 `DefaultConfig()` + per-player 默认值相等。双源漂移从此不可能再发生。

> **结构性约束（写下来免得后人重复踩）**：模板**没法**从播放器注册表推导。已核实 `tools/genconfig/main.go` 只 import `config`、从不 import 播放器包 → `registeredPlayers` 在 CI 渲染模板时是空的；而播放器包的 `init()` 带 Windows API，import 进来直接破坏 Linux 构建（`release.yml:93` / `build-windows.yml:84` 的 `GOOS= GOARCH= go run ./tools/genconfig` 依赖这一点）。**「让 RegisterPlayer 带默认值然后渲染模板」这条路是堵死的。** per-player 默认值只能留在 `config/` 内做成纯数据表供两个消费者共读，或靠 drift test 兜住。

**P0-4｜日志落盘（S，独立，无依赖）。** `logger/logger.go` 全部是 `log.Printf` 裸包装；全树无 `SetOutput`；`main.go:51` 只有 `log.SetFlags`。加一个 file sink（`os.Executable()` + `os.OpenFile` 都不带 Windows API，**不破坏 config/logger 的 Linux 约束**），由 `main.go:51` 调用。
路径必须相对 exe 目录解析，不是 CWD——`config.go:110` 用 CWD 而 `main.go:340` 写在 exe 旁，这个坑已经存在，别再复制一遍。

> **为什么这一项排在所有 feature 前面**：#30 的 body 是**一张零文字的截图**，#8 靠录屏和两张截图描述 bug。这不是作者偷懒，是**没有日志文件可附**的直接产物。仅此一项就把所有 bug 报告从「请录屏」变成「请附日志」。

**P0 完成定义：** `.gitattributes` + fmt/vet 修复 + 两个新测试文件已入库；CI 有 `pull_request` 触发的 fmt/vet/test 三道门禁且通过；`config/` 有 drift test 且 `DefaultConfig()` 与模板对齐；程序启动后在 exe 目录旁产出可轮转的日志文件。

---

### P1 — 让系统能说出「我不行」（下周，全部 S，wire 零破坏）

**这是整个 roadmap 的枢纽。P2/P3 的一半工作依赖它，而且它把 #33 从一个不可执行的备忘变成一个可执行的任务。**

**P1-1｜新建 `compat/` 包 —— 一个裁决类型 + 一个落点。**

```go
// compat/compat.go —— 纯数据，零 Windows import
package compat

type Verdict int
const (
    Supported    Verdict = iota // 已验证：偏移/补丁与探测到的版本匹配
    Unsupported                 // 探测到版本，但无匹配条目 → 拒绝挂载
    Undetectable                // 无法探测版本 → 拒绝挂载
)

type Report struct {
    Subsystem string   // "qqmusic" / "kugou" / "cloudmusicv3-effect"
    Detected  string   // 原样字符串："22.31" / "20.1.22.27795" / ""
    Known     []string // 已验证支持的版本（让用户知道该降到哪一版）
    V         Verdict
    Reason    string   // 中文，用户可读
}

func (r Report) OK() bool { return r.V == Supported }
func Publish(r Report)     // 后端探测后调用一次
func Snapshot() []Report   // /service-status 读取
```

**明确不要做的事：不要抽「版本协商层」。** 四个后端的版本身份机制不可约地不同：
- QQ：`fmt.Sprintf("%d.%02d", major, minor)`（`mem.go:346`）从 PE 版本资源 → map key
- KuGou：精确 4 段字符串 `"20.1.22.27795"`（`watchdog.go:33`）**外加** DLL 偏移处的字节哨兵 `p1Orig`（`watchdog.go:61`）
- CloudMusic：**没有版本概念**——CDP target 发现，版本隐含在 React fiber 形状里
- WeSing：**没有版本概念**——PE 导出表解析 + AOB，按构造自定位

在「解析 PE 版本查表」和「读某偏移处 6 个字节」之间抽接口，会得到一个空抽象。**可共享的不是身份机制，是裁决结果和拿它做什么。**

**P1-2｜QQ 拒绝挂载（S）。** `mem.go:346-354` 的 `detectVersion` 改为返回 `compat.Report`；`ConnectQQMusic`（`mem.go:299`）在 `!OK()` 时返回 error，让 `qqmusic.go:48-52` 的重连循环把它当「未就绪」。**删掉 `m.offsets = knownVersions["22.16"]` 兜底。**

> **wire 零破坏，这是关键**：不需要新状态值。`waiting_process` **已经在 enum 上**（`doc/openapi.yaml:1698`）、**已经归一化为 `idle`**（`server/router.go:31`），而 `idle` 的播放器**永远抢不走 overlay**。路由器今天就做对了，缺的只是后端从不进入这个状态。所以：不支持 → 发 `waiting_process` + `Detail` 带原因。零前端改动、零契约破坏、零 enum 变更。
> 顺带收编 `kugou.go:65` 那个 off-enum 的 `"error"`——它不在 `openapi.yaml:1698` 的 enum 里，且被 `router.go:23-34` 的 `default:` 塌缩成 `idle`（注释原文「以及未来任何未知值」）。**全仓唯一一次真正的拒绝，既不在 wire 契约上，又在内部被抹平。**

**P1-3｜KuGou 收编 `canAutoFix`（S，一行 `_` 改成变量）。** `watchdog.go:576` 现在是 `allPatched, _, err := CheckPatchStatus(...)`，而 `CheckPatchStatus` 在 `:107` 已经正确算出了 `canAutoFix`。改成走已有的 `ErrInstallNotFound` 同款终态路径（新增 `ErrVersionUnsupported`），复用 `kugou.go:63-66` 的形状。
> 顺带注意 `watchdog.go:565` 那个 `strings.HasPrefix(ver, "10.")` 是硬编码**黑名单**——它证明「拒绝」的直觉早就有了，只是做成了黑名单而不是白名单。
> **这一步同时关掉「UAC 提权盲写未知 93MB DLL 且无 `.bak`」这个头号脆弱点——它其实是同一个洞最贵的实例。**

**P1-4｜`/service-status` 加 `player_compat` + 修 `main.go:126` 的 park 谎报（S）。**
- `main.go:85` 用降级后的 `effectStrategy`，`main.go:126` 上报 `cfg.EffectStrategy`（未降级）——**已实测确认**。
- `main.go:62` 的 `playerNames` → `server.go:551` 的 `player_support` 是**写死的**：QQ 装了 22.31 它照样宣称支持。这是 `player_compat` 该 reconcile 的地方。
- `server.ServiceInfo`（`server.go:65-73`）+ `handleServiceStatus`（`server.go:544-556`）新增字段。

**P1-5｜静默丢弃加计数器（S）。** `player/player.go:195-200` 的 `BaseEmitter.Emit` 用裸 `default:` 丢事件，无计数无日志；`server/server.go:160-163` 的 fan-out 同理。加计数器挂到 `/service-status`。
> **不要动 fan-out 的 `default:` 本身**——它正是 `router.go:95` 阻塞式 `merged <- evt` 可证明不停滞的前提。只加计数，不改语义。

**P1-6｜`#8a` WeSing 时长串歌（S~M，P0 的日志落盘之后）。** 见 §3。

**P1 完成定义：** `compat/` 存在且加入 CI 测试白名单（`GOOS= GOARCH= go test ... ./compat`）；QQ 遇未知版本发 `waiting_process` 而非套 22.16 偏移；KuGou 遇未验证版本拒绝 patch 而非盲写；`/service-status` 的 `player_compat` 与运行时探测结果对账；`main.go:126` 报的是实际生效的 strategy。

**为什么是这个顺序：** QQ 22.31 用户今天的真实痛点**不是「QQ 没歌词」，是「垃圾串通过 `qqmusic.go:106` 的 `meta.Name != ""` 判定 → `qqmusic.go:141` Emit `playing` → `router.go:107` 翻转 `activePlayer` → 从正在正常工作的 WeSing 手里抢走 overlay」**。这个 S 级改动**一次性覆盖所有未来的 QQ 版本**，价值高于 #38 全做完（L）。而 #33 要收集的事件今天**还不存在**——必须先由 P1 把它创造出来。

---

### P2 — 兑现文档已经吹出去的承诺（M）

排序原则：**先收回假承诺，再实现。**

1. **改 `README.md:132-133`**（S，chore，先于一切实现）—— 它和 `doc/API_RESPONSE_EXAMPLES.md:248-249`（「目前仅网易云音乐提供逐字数据」）直接打架，现在会**直接生产误报 bug**。
2. **修 `instruction.md:275`**（S）—— 它说 sub_text「当前始终为空字符串」，自 `4c4ff8f`（2026-05-14）起是假的，而它是 normative spec，同时与 `doc/openapi.yaml:316` 的非空示例矛盾。顺带改 `API_RESPONSE_EXAMPLES.md:818,826`。
3. **合并逻辑提级**（M）—— `MergeTlyric`/`MergeYRC` 现困在 `player/cloudmusic/lyric` 包内。QQ 要用就得复制 → 立刻产生第二个真理来源（这仓库已经有两个播放器注册表、两套配置默认值了，别再加一个）。提到 `player/` 时**别照抄 `MergeTlyric` 的毫秒精确匹配**（`fetch.go:299-306`）——用 `MergeYRC` 的容差匹配（`fetch.go:329`），前者匹配失败是完全静默的。
4. **QQ 逐字 + QQ 翻译**（S+S，同一次改动）—— 见 §3 的 #18/#22。
5. **修「逐字：否」硬编码**（S，与 4 同批）—— `qqmusic.go:187` / `kugou.go:330,333,336` / `wesing.go:262` 全是**字面量断言**，只有 cloudmusic 用 `lyricDetailedFlag()` 真检测。**接通逐字后日志还是会说「否」，实现者会以为自己没做成。**
6. **网易云 romalrc + QQ roma**（S+S）—— 见 §3 的 #34。
7. **`#8b` WeSing 歌词串歌** + **`#8c` 歌手 fallback**。

**P2 完成定义：** README/instruction/API_EXAMPLES 三处文档与运行时一致；QQ 逐字与翻译在非 v20.05 版本上产出真实数据；四个播放器的「逐字：X」日志是检测结果而非断言；合并逻辑在 `player/` 且带容差 + 失败日志。

---

### P3 — 逆向与大件（无期限，需实验室）

- **#38 本体**（L）—— 必须先答 32/64 位问题。
- **#18 WeSing 逐字**（L）—— 必须排在 `#8b` 之后。
- **#34c WeSing 发音**（XL）—— 零知识区。
- **#39 滑块路由器改造**（L）—— 路由器改造，不是接线活。
- **#16 任务栏进度条**（M）—— 需双路径 + 可测手段。
- **#31 Metabox 生态**（XL）—— 阻塞在仓库外的规范文档上。

---

## 3. 每个 issue 的去向

| # | 标题 | 结论 | 分期 | 依赖 | 工作量 |
|---|---|---|---|---|---|
| **38** | QQ音乐 22.31/41 支持 | **拆**：拒绝挂载 → P1-2；补偏移表 → P3 | P1 + P3 | ①32/64 位确认 ②实机 + CE 逆向 ③作者补正文 | S + L |
| **35** | 三播放器本地文件歌词/封面 | **关**（前提被作者自己评论推翻）→ 重开窄 issue「QQ 本地/无 mid 曲目回退」 | P2 | P1-2（硬依赖） | M |
| **34** | wesing 粤语/韩语发音 | **拆 4 份**：a 契约 / b 网易云 / c wesing / d QQ | a→P1, b/d→P2, c→P3 | c 依赖 #8b | S/S/XL/S |
| **33** | 新增运行检测与错误收集 | **改写**为「日志落盘 + 丢弃计数」；OTel/Sentry 框架选型**关掉** | P0-4 + P1-5 | 无 | S + S |
| **32** | 新增 config.yml 强制更新 | **关**（机制不成立）→ 换成 P0-3 drift test | P0-3 | 无 | S |
| **31** | 适配Metabox生态接口规范 | **等**（降级为「写规范」）+ **立刻摘 `good first issue`** | P3 | PRD.md:89 的 WS/gRPC 二选一决策 | XL |
| **30** | BUG：网易云无法连接 | **关** → 重开「网易云连接路径可观测性」 | P2 | 无 | S |
| **22** | 关于第二行歌词（副歌词） | **stale，关掉已完成部分** → QQ 半边并入 P2 | P2 | 合并逻辑提级 | S（非 M） |
| **18** | [feat] 增加逐字歌词 | **拆三份**（量级差两个数量级） | QQ→P2, KuGou→P2调查, WeSing→P3 | WeSing 依赖 #8b | S / ? / L |
| **16** | 下载进度条同步到任务栏 | **做，但重划范围**（双路径） | P3 | 需先有可测手段 | M |
| **8** | 反复播放后标题/歌手/时长识别错误 | **摘 invalid**，改标题，拆 8a/8b/8c | 8a→P1-6, 8b/8c→P2 | 实机复现环境 | M |
| **39** | AOB 滑块探针（本轮已建） | **等** | P3 | 路由器动态成员改造 | L |

### stale / already-done 的证据

**#35 — stale。** 标题点名 3 个播放器，2 个早已实现，且并非近期偷偷修好，本来就是这么设计的：
- 网易云双保险：`cloudmusic.go:250-261` 在 Redux ID 为空时直接 `lyric.SearchSongID(songName, songArtist, targetDurationMs)`（实现 `lyric/fetch.go:155`）；另有 Redux 镜像路径 `:339-342` 与后到路径 `:376-383`。
- 酷狗更彻底：`kugou.go:308` → `kugou/lyric/lyric.go:62-96` 三级回退（hash → 曲库反查 canonical hash → `searchByKeyword`），name/singer 来自 `kugou.go:458-464` 的 `splitFilename` 拆 `PlayInfo.Filename`——**这就是评论所说的「文件名+搜索」，代码里一字不差地实现着**。
- 作者 2026-06-14 评论已推翻正文前提：「这些播放器……并没有读取本地音频文件内置歌词的能力」。**只剩 QQ 一个真空缺**（`api.go` 全部函数只认 mid/ID，无任何搜索函数）。

**#22 — stale。** 正文画的三种 wire 已全部落地：`player/player.go:49`（`LyricLine.SubText`）、`:72`（`LyricUpdate.SubText`），共用同一组 struct 经 `server/server.go:347` 出站，一次改动三处全中。前端 `lyric_page.html:1343/2138-2140/2161-2163/2487/2977-2978` 齐了。网易云落地于 `4c4ff8f`，两条路径（`MergeTlyric` @ `fetch.go:291-308` / `ExtractTlyricLines` @ `cdp/client.go:574`）都通。
jiahao 那条「无时间戳直接丢弃」**已满足，但是靠结构撞对的**：`ParseLRC` 在 `fetch.go:456-459` 对匹配不上 `[mm:ss.xx]` 的行 `continue`，`:470-473` 再丢空文本行。**没有任何注释或测试把这个行为钉住**——谁重写 ParseLRC 都会无声破坏它。

**#18 — 三件性质完全不同的事，triage 判 L 是误判。**

> **必须处理的矛盾：triage 把 #18 整体判为 L；我实测核实后，QQ 这一半远没有 L 那么大。**

已实测确认（`sed -n '218,275p' player/qqmusic/api.go`）：
- `api.go:263` 注释写明格式 `[startMs,durationMs]text(charTs,charDur)...`
- `api.go:224` 已编译 `qqCharTimingRe = regexp.MustCompile(`\(\d+,\d+\)`)`——**精确定位逐字时间**
- `api.go:268` `plainText := qqCharTimingRe.ReplaceAllString(rawText, "")`——**逐字时间轴在这里被显式销毁**
- 目标结构 `player.LyricTextDetailedWord`（`player/player.go:9-15`，字段 `Timestamp/PlayTime/Duration/Text`）**早已存在**
- 真机日志佐证：「QRC 解密成功 (6910 字符)」→「歌词加载完成: 44 行」——6910 字符解出 44 行，逐字数据确实在手上

**结论：对 QQ 而言这是「把删除改成解析」的接线活（S），不是逆向活。**

> **但有一个 issue 和架构地图都没提的限制**：逐字只存在于 `musicu.fcg` 路径（`api.go:150-153` 发 `qrc:1, crypt:1`）。v20.05 走 `fetchLRCBySongMid`（`api.go:94`，URL 带 `nobase64=1`）打的是老 fcg，**返回纯 LRC，本来就没有逐字**。QQ 逐字是「四个版本里三个能吃到」，不是全量。

- **KuGou = 待验证参数。** `lyric.go:242` 显式请求 `fmt=lrc`，`fmt=krc` 是另一个选项。**但全仓 grep `krc` 只命中 `krcs.kugou.com` 域名，零 KRC 解密代码。**「`fmt=krc` 确实带逐字和翻译」是外部知识，**在本仓库中无法证实**。**需运行时验证**：花 30 分钟实拉一个 `fmt=krc` 响应确认结构，再决定评级。不要因为「另外两个都做了」就凑一个。
- **WeSing = 真逆向（L），但比 issue 想的近。** `reader.go:83-108` **已经在逐字遍历**（`numChars := (charEnd - charBegin) / 4`，`reader.go:81` 注释「中文歌词每个 CharElement 是单个汉字」），切分现成，只是在 `reader.go:106/:114` 被拼成扁平字符串扔掉。缺的只有逐字时间——`CharElement` 里唯一解出的偏移是 `+0x00 → RenderData*`（`reader.go:89-90`），其余是**零知识区**。issue 正文说的「读 `WeSingCache\WeSingDL\Res` 的 note 文件」是错的（那是音高/打分数据）；「复用 QQ 逐字」措辞也反了（QQ 的不是要去复用，是已经在手里被删掉）。

**#8 — 真根因已坐实，invalid 是误标。**

扫描顺序可证明是地址升序：`proc/memory.go:454` region 升序 + `:505` region 内 i 升序 → `results[0]` 恒为**最低地址 = 最早分配**。

- **(b) 时长——从未修过。** `lyric/timer.go:66` `for _, addr := range results` 取第一个能解析的就 `return`（`:108`），跟当前歌曲**零绑定**——无 MID、无歌名、不参考 timeAddr。`wesing.go:178` 每首歌只读一次。**精确对上作者 2026-03-20 的评论「总时长错误好像是留在前面的某一首歌不变了。实时进度没问题」**（进度走 `FindPlayTimeAddr` @ `timer.go:41-46`，读活 float 且有 `validateTimeAddr`，所以正常）。
- **(c) 歌词——从未修过。** `lyric/finder.go:59` `hostAddr = results[0]`。
- **(a) 标题/歌手——部分修复，这是 invalid 标签的来源。** 时间线：03-19 14:42 `d8c34ec`「[FIX] #8」只动了 `songinfo.go` → 03-19 18:56 `fcac0fa` 动了 `FindSongDuration` 但**没给它同样的匹配逻辑** → **03-20 00:51/01:02 作者带截图再次报告时长错，晚于那个 [FIX]** → 03-28 `a4957aa`「**尝试**修复歌手不准确」。
- `git log -S 'hostAddr = results[0]'` 只有 `a830e0c`（改目录名）；`git log -L '/func FindSongDuration/,/^}/'` 无实质修改。**(b)(c) 的选择逻辑自诞生起从未被改过。**
- 三处启发式**互相矛盾**：时长/歌词取最低地址，`songinfo.go:50` 取最高地址且注释「最新分配的数据」——**这个假设本身是错的**，Windows LFH 复用已释放块，分配不单调向上。

> ### ⚠️ 必须纠正的一处前提错误
>
> 交接材料里「**关于 #8 —— 真机已复现**」那一段描述的是：播放本地文件时歌名形如「`1_张惠妹 - 分生_(Instrumental) -`」，随即报「歌词获取失败: API error code: 24001」，且 `api.go:188` 对该错误码无处理。
>
> **`api.go` 是 QQ 音乐的。这条真机证据支持的是 #35 的 QQ 半边（本地文件无 songID → 24001），不是 #8。** #8 的标题是全民K歌（WeSing），根因在 `finder.go:59` / `timer.go:66` 的堆实例选择，与 24001 无关。
>
> 两者被混为一谈会导致：修完 QQ 的本地文件搜索回退后，有人以为 #8 修好了并关掉它——而 WeSing 的时长串歌一行没动。**#8 的 invalid 该摘，但摘它的理由是静态分析证据，不是这条真机日志。**

**#30 — unclear，但很可能不是「连不上」，是「连错了还不吭声」。**
- 全仓 grep `无法连接|连接失败|连不上` 只有 4 处（`main.go:385` 自动更新、`main.go:534` CDN 测速、`effect/effect.go:417`、`:474` 注释），**没有一处在网易云取词路径上**——所以截图里的「无法连接」**在代码里没有对应文案**，截图内容不可解读。
- `cdp/client.go:291-298` 三级降级最后一级是 `wsUrl = pages[0].WebSocketDebuggerUrl`——**无条件抓第一个**，全文件**无任何一处校验 target 属于 cloudmusic.exe**（仅 `:267`/`:272`/`:284` 提及 `orpheus`）。9222 被任何其他 CDP 进程占用 → 连上陌生标签页 → `jsPayload` 恒返回 `null: no root`（`client.go:130`）。**表现为「连不上」，实为「连错了」**——这是 `mem.go:352` 反模式在网易云侧的孪生兄弟。
- `cloudmusic.go:123-127` 把 `err` **完全丢弃、零日志、2s 静默重试**；唯一状态播报在循环**外面**只发一次（`cloudmusic.go:105`「网易云音乐未启动」）。**这解释了为什么 #30 的 body 是一张没有文字的截图——当时根本没有文字可截。**
- 时间线否证「已被悄悄修好」：#30 提于 2026-04-15，而 `effect/`（`3549ad6`，6-19）、`park/`（`d2d5953`，6-20）目录**当时不存在**；6 月的 `74de387`/`546bf63` 修的是 6 月自己引入的 bug。
- **`cdp.Connect()` 的三级降级逻辑一行都没动过 → 如果根因在这，它今天 100% 仍然存在。**

**#31 — unclear，且是全仓最危险的标签。**
- 「Metabox 生态系统接口规范」**在任何仓库里都不存在**。唯一书面表述在 `Metabox-MDT-Media-Sequencer/PRD.md:89`：「基于 WebSocket **或** gRPC」——连二选一都没定。两仓均无 `.proto`，无 gRPC 依赖；`Warudo-Ws-middleware` 全仓 grep「metabox|nexus|UAR|生态」零命中。
- 反向证据（为何 unclear 而非 valid）：若「适配」= 统一 envelope + WebSocket，则 `types.go:54` `WSEvent{type,player,data}` + `types.go:61` `HTTPResponse{code,msg,player,data}` **可能早已满足**。同一句话既可判「已完成」也可判「XL 未开工」——这本身就是 unclear 的硬证据。

---

## 4. 建议关掉的 issue

| # | 理由 |
|---|---|
| **#32** | **机制不成立。** 真实动机（新 offset 到不了老用户）根因在 `config.go:60-71` 与 `config.go:330/335/339` 双源漂移——删掉一个源即可，不需要版本键、不需要网关、不需要删用户文件。且 `main.go:217-227` 的 `releaseInfo` **只能消费 GitHub release 既有字段**，本仓库单方面改不出这个信号；照抄 `main.go:593-594` 的 `isForceReleaseName`（`HasSuffix(...,"-force")`）会直接破坏现有 force-downgrade 语义（标题写成 `v3.1.0-force-resetconfig` 就不再匹配）。**关闭时把 P0-3 的链接写进 closing comment。** |
| **#35** | **前提被作者自己 2026-06-14 的评论推翻**，标题点名 3 个播放器 2 个早已实现。留着它会让人以为有个「本地歌词」大功能待做。**关闭时必须把评论那句结论抄进 closing comment**，否则这个认知会丢失——这正是 #8 的教训。 |
| **#30** | 截图所示的「无法连接」在代码里**没有对应日志文案**（全仓仅 4 处，无一在网易云路径），故不可解读；4 月的现场已被 6 月的 effect/park 子系统整体重写；未记录网易云版本 / 9222 占用 / PlayerCap 版本，三者缺一即无法归因。**留着它不产生信息，只产生「有个已知 bug 没修」的错觉。** |
| **#22（已完成部分）** | 正文画的 schema 全部落地。剩余的 QQ 半边应作为窄 issue 重开。 |
| **#33（OTel/Sentry 那一半）** | **两个方案都不成立。** 已核实全树：**零 `recover()`、零生产 `panic()`**（仅 `tools/devserver/main.go:26`、`tools/cdpexplore/main.go:44,49`）。**Sentry 的招牌能力在这个仓库的表面积是零**——QQ 22.31 不会 panic，它自信地返回垃圾，Sentry 会报告一个**完全健康**的进程。OTel 是范畴错误：单进程、四个本地轮询循环、无分布式 span。另：全部退出走 `os.Exit` 绕过 defer（`main.go:162,202,323,515,522,656` + `watchdog.go:322,328,331,410,430,435,438`），`main.go:195-196` 的 `signal.Notify` 只收 Ctrl+C/SIGTERM——**最想要的崩溃恰恰永远 flush 不到**。 |

**⚠️ 关于 `invalid` 标签的处置约定（重要，别误伤）：**

`invalid` 在本仓库**不是** GitHub 语义上的「报告无效」，而是**作者的私人分诊标记**，且含义至少有两种：
- **#38 型 =「正文为空，按现状不可执行」**——需求真实（`mem.go:129-192` 的 `knownVersions` 确无 22.31/41）。
- **#8 型 =「诊断发散 / 修了三分之一就宣告完成」**——根因可见、至今未修。

两者都 open 是因为**打了标签却没人关**，仓库缺一条「invalid = 关闭」的处置约定。**外人据此关闭会误伤。** 建议：#8 **摘掉 invalid**（根因已坐实）；#38 保留（正文确实为空，需作者补齐）；#31 **反而最该挂 invalid，它却拿到了 `good first issue`**。

---

## 5. 建议新开的 issue

| 标题 | 正文（一句话） |
|---|---|
| `arch: 新建 compat/ 包，统一「不支持」的裁决与上报` | 四个降级点（`qqmusic/mem.go:352`、`kugou/watchdog/watchdog.go:576`、`kugou/kugou.go:63`、`main.go:81-84`）各自发明了一套反应且三处对外撒谎；用一个 `compat.Report` 裁决类型 + `/service-status` 落点收编，wire 零破坏（复用已在 enum 上的 `waiting_process`）。 |
| `fix(qqmusic): 未知版本拒绝挂载，删除静默回退 22.16` | `mem.go:351-354` 对未知版本 `log.Warn` 后套用 22.16 偏移继续解引用，`m.version` 与 `m.offsets` 互相撒谎，垃圾串通过 `qqmusic.go:106` 的弱判定翻转 `router.go:107` 的 `activePlayer`，**从正常工作的 WeSing 手里抢走 overlay**；改为返回 error 让重连循环发 `waiting_process`。 |
| `fix(kugou): CheckPatchStatus 的 canAutoFix 被丢弃，导致盲写未知 DLL` | `watchdog.go:576` 写成 `allPatched, _, err :=`——`:107` 已正确算出 `canAutoFix=false` 然后扔掉，于是继续 UAC 提权对 93MB libcef.dll 做 9 处盲写，**且无 `.bak` 备份**。 |
| `chore(config): 加 drift test，钉死 DefaultConfig() 与模板一致` | `config.go:60-71` 的 `Players` 是空 map → `GetPlayerOffset` 回退 200ms，而模板 `config.go:330/335/339` 是 500/400/430；`3bb0453`/`49133ae` 之前装的用户**永远拿不到调优值且升级无效**；`config/` 零测试但 Linux-buildable，约 30 行即可根治。 |
| `ci: 加 pull_request 门禁（gofmt / go vet / go test）` | 四条 workflow 无任何 `pull_request` 触发器、无质量检查，且 job 级 `env: GOOS: windows`（`build-windows.yml:12`）导致 `go test` 架构上无法运行；vet/gofmt 刚变绿，**这是唯一的时间窗**。 |
| `feat(logger): 日志落盘到 exe 目录（含轮转）` | 全树无 `log.SetOutput`，`main.go:51` 只有 `SetFlags`；**#30 的 body 是零文字截图、#8 靠录屏，都是没有日志可附的直接产物**；`os.Executable()`+`os.OpenFile` 不带 Windows API，不破坏 config/logger 的 Linux 构建约束；注意别踩 `config.go:110`(CWD) vs `main.go:340`(exeDir) 的既有坑。 |
| `fix: 「逐字：否」是硬编码字面量，不是检测结果` | `qqmusic.go:187` / `kugou.go:330,333,336` / `wesing.go:262` 全是断言，只有 cloudmusic 用 `lyricDetailedFlag()` 真检测——**接通逐字后日志仍会说「否」，实现者会以为自己没做成**。 |
| `fix(qqmusic): parseLRC 销毁了已解密的逐字时间轴` | `api.go:268` 一行 `qqCharTimingRe.ReplaceAllString(rawText, "")` 删掉 `(charTs,charDur)`，而正则 `api.go:224` 和目标结构 `player.LyricTextDetailedWord`（`player.go:9`）都已存在；**注明只覆盖非 v20.05 版本**（`api.go:94` 的老 fcg 路径本来就没有逐字）。 |
| `fix(qqmusic): Trans 字段已请求已解析零读取` | `api.go:154` 发 `"trans":1`、`api.go:35` 声明 `Trans string`、`api.go:112` fcg 路径也有，**全仓 grep `\.Trans\b` 零命中**——根因是 `fetchLRC` 返回签名 `(lines, name, singer, error)` 没给翻译留出口。 |
| `fix(cloudmusic): CDP target 选择无条件降级到 pages[0]` | `cdp/client.go:291-298` 在匹配不到 `orpheus://` 时抓第一个页面，全文件**无一处校验 target 属于 cloudmusic.exe**；9222 被 Chrome/Edge/任何 Electron 占用即静默连上陌生标签页；同时 `cloudmusic.go:123-127` 把 error 完全丢弃、零日志、2s 静默重试。 |
| `fix(qqmusic): 本地文件无 songID → API 24001 无处理` | 真机日志：本地文件歌名从文件名切出（形如 `1_张惠妹 - 分生_(Instrumental) -`，歌手为空）→ `歌词获取失败: API error code: 24001`；`api.go:188` 对该错误码原样包成 error 抛出；对照组流媒体 `songID=856694` 一切正常。这是 #35 的 QQ 残余。 |
| `docs: README:132-133 对未实现的能力做了承诺` | 「各播放器支持逐字解析 / 音译」——实际逐字 1/4、翻译 1/4、**音译 0/4**，且与 `doc/API_RESPONSE_EXAMPLES.md:248-249` 自己的说明直接打架，现在会直接生产误报 bug。 |
| `docs: instruction.md:275 对 sub_text 的描述自 4c4ff8f 起为假` | 它说「当前始终为空字符串」，而它是 normative spec，同时与 `doc/openapi.yaml:316` 的非空示例矛盾；三份文档三个说法。 |
| `fix(server): kugou 的 "error" 状态不在 openapi enum 上` | `kugou.go:65` emit `Status:"error"`，但 `doc/openapi.yaml:1698` 的 enum 无此值，且被 `router.go:23-34` 的 `default:` 塌缩成 `idle`——**全仓唯一一次真正的拒绝，既不在契约上又被内部抹平**。 |
| `fix(main): /service-status 上报未降级的 effect-strategy` | `main.go:85` 用降级后的 `effectStrategy`（Win11 强制 fadeout），`main.go:126` 上报 `cfg.EffectStrategy`；同理 `main.go:62`→`server.go:551` 的 `player_support` 是硬编码，QQ 装 22.31 照样宣称支持。 |
| `fix: BaseEmitter.Emit 与 fan-out 静默丢弃无计数` | `player/player.go:195-200` 和 `server/server.go:160-163` 用裸 `default:` 丢事件；**只加计数器挂到 `/service-status`，不要动 `default:` 本身**——它是 `router.go:95` 阻塞式发送可证明不停滞的前提。 |
| `test: ParseLRC 丢弃无时间戳行的行为无测试保护` | `fetch.go:456-459`/`:470-473` 满足了 #22 jiahao 的要求，但**是靠结构撞对的**，无注释无测试——谁重写都会无声破坏。 |
| `refactor: MergeTlyric/MergeYRC 提级到 player/` | 现困在 `player/cloudmusic/lyric` 内，QQ 要用就得复制 → 第二个真理来源；提级时**别照抄 `MergeTlyric` 的毫秒精确匹配**（`fetch.go:299-306`），用 `MergeYRC` 的容差（`fetch.go:329`）+ 匹配失败打日志（现在是完全静默）。 |
| `chore: 摘掉 #31 的 good first issue 标签` | 它是跨仓协议设计任务，实际落点是 wire 契约 + router fan-out（全仓最重的两处承重结构），而规范文档尚不存在。 |
| `docs: instruction.md:603 的 -H windowsgui 与 CI 矛盾` | `release.yml:74` 和 `build-windows.yml:61` 都没有该标志（控制台子系统）；谁拿文档去「订正」代码，`GetConsoleWindow()` 返回 0，**现存的文本进度条也会一起静默消失**。 |

> ### ❌ 一条**不该**开的 issue（已核实为假）
>
> 交接材料建议开「effect_page.html 重连已坏」。**已实测证否**：`effect_page.html:296-317` 有完整重连——`connect()` 里 `try/catch → scheduleReconnect()`（`:297`）、`ws.onclose → scheduleReconnect()`（`:308-312`）、`ws.onerror → ws.close()` 触发 onclose（`:311`）、`scheduleReconnect()` 1500ms 退避（`:315-318`）。对比 `lyric_page.html:1735-1738` 是 3000ms，两者形状一致。**重连没坏，不要开这个 issue。**
> （唯一可议的细节：`effect_page.html:287` 的 `cmActive/showing` 在重连后仍是上次的值而非重置——影响是重连瞬间可能短暂显示陈旧状态。**需运行时验证**：手动 kill/restart 后端，观察 overlay 在 1.5s 重连窗口内是否闪一帧陈旧特效。量级 XS，不值得单独开单，可在别的改动里顺手带上。）

---

## 6. 风险登记

| 期 | 动作 | 碰到的承重路径 | 威胁 | 验证方法 |
|---|---|---|---|---|
| **P0-2** | 加 CI 门禁 | 两条打包流水线 | 新 workflow 若继承 job 级 `env: GOOS: windows`（`build-windows.yml:12`），`go test` 直接不可运行；若漏掉 `GOOS= GOARCH=` 则 `go run ./tools/genconfig` 在 Linux runner 上炸 | 在分支上先跑一次 workflow_dispatch，确认三步全绿再合并；本地预演 `GOOS=windows go vet ./...` + `GOOS= go test ./player ./server ./config` |
| **P0-3** | 对齐 `DefaultConfig()` 与模板 | **shipped defaults ≠ DefaultConfig()** 这条 landmine | 老用户的 cloudmusicv3 offset 会从 200 → 500（歌词位置**会变**）。这是**预期的修复**，但对已经手动把 offset 调到 200 的用户是行为变更——他们的 config.yml 里若显式写了值，`mergeYAML` 会保留，不受影响；只有**没写这个键**的用户会变 | 三组手测：①无 config.yml 冷启动 ②有旧 config.yml（缺 per-player 键）③有旧 config.yml（显式写了 `cloudmusicv3-offset: 200`）。第③组必须保持 200 |
| **P0-4** | 日志落盘 | **`config/` 和 `logger/` 绝不可 import Windows API** | 该约束由**零机制**保障（无 build tag、无检查、无 PR gate），却被 `release.yml:93` / `build-windows.yml:84` 真实依赖。`logger/logger.go:3` 当前 import **只有 `"log"`**，被 17 个文件引用。一旦有人为了「日志写到 exe 旁」伸手去拿 `windows.GetModuleFileName`，两条流水线在 genconfig 步骤炸掉且无 gate 拦得住 | **`os.Executable()` 是 stdlib、跨平台，不引入 Windows API。** 验证：`GOOS=linux go build ./config/... ./logger/...` 必须 exit=0（已实测当前为 0）——**这一条应当直接加进 P0-2 的 CI 门禁** |
| **P1-2** | QQ 拒绝挂载 | `router.go:107` 的 `activePlayer` 翻转 | 这是**收窄**，方向正确。但需确认 `qqmusic.go:48-52` 的重连循环真的把 error 当「未就绪」而非退出会话 | **需运行时验证**：装一个非 22.05/21.81/22.16/22.22 的 QQ 音乐，或临时改 `mem.go:346` 的 `fmt.Sprintf` 伪造版本号。断言：①日志出现「暂不支持」②`/service-status` 的 `player_compat` 显示 Unsupported ③**WeSing 的 overlay 不被抢走**（这才是真正的验收点） |
| **P1-3** | KuGou 拒绝 patch | **UAC 提权 + libcef.dll 盲写** | 这是本仓库风险最高的代码路径。改动方向是**减少写入**，但若 `canAutoFix` 的语义理解错，可能让**已验证版本**也拒绝 patch → 酷狗功能全挂 | **需运行时验证**：①装 `20.1.22.27795`（`watchdog.go:33` 的 knownVersion）→ 必须正常 patch ②装任意其他版本 → 必须拒绝且日志说明该降级到哪一版。**先备份 libcef.dll 再测**（现在代码不做备份，这正是要修的东西之一） |
| **P1-6 / P2** | #8a/#8b/#8c WeSing | ①**`syscall.NewCallback` 单例**（`proc/memory.go:113-137`、`:198-239`）②**WeSing 是默认 prior player**（`config.go:65`）③**只读句柄** | ①Go 对存活 callback 有硬上限且永不释放，30ms 轮询里 per-call `NewCallback` 会**直接崩进程**——修 #8 时改窗口标题解析（`memory.go:230`）极易顺手新建 ②WeSing 回归直接打在默认 overlay 上 ③**不要**为定位对象往 WeSing 里写内存（QQ 后端就是这么干的，`mem.go:534`）；WeSing 全程只读、无需提权，是它唯一比 QQ/KuGou 干净的地方 | **CI 无 Windows runner**（`build-windows.yml:10` self-hosted 但 Linux，见 `:84` 的注释），`player/wesing` 不在可测白名单内 → **只能实机手测**。建议先写 `tools/wesing-probe`（对标已有的 `tools/cdpexplore`），dump 某次扫描的**全部候选命中 + 地址 + 解析结果**——否则「取哪个才对」只能靠猜，改完也无法自证 |
| **P2** | QQ 逐字/翻译 | **QQ 的 DES S-box 是故意偏离 FIPS 46-3 的**（`qrc_decrypt.go:18`、`:28`，且 `:4-6` 的注释给的理由是**错的**） | 要取 trans 就必须动 `api.go`。**任何人在「反正都要改这个文件了，顺手换成 crypto/des」的念头下动手，会让 QQ 全部歌词——不只是翻译——一起死掉。** 这是本轮唯一的真实地雷 | 在 `api.go` 顶部加一条中文注释指向 `qrc_decrypt.go:18` 的约束；改完后实机对拉一首已知歌曲，断言解密后字符数与改前一致（真机日志基线：「QRC 解密成功 (6910 字符)」→「44 行」） |
| **P2** | 修改 `parseLRC` | 它是**共用函数** | `api.go:218` 同时处理标准 LRC 分支（`:244-261`）和 QQ 私有 QRC 分支（`:264-277`）。改 QRC 分支时不能碰标准 LRC 分支，也不能动 `:286-300` 的「无时间戳纯文本按时长均分」兜底 | `player/qqmusic/` 现在**有测试了**（本轮新增 `mem_test.go`）；给 `parseLRC` 补表驱动测试：标准 LRC / QRC 带逐字 / QRC 无逐字 / 纯文本兜底 四组。这个包 Linux-buildable → **能进 CI** |
| **P2** | 提级 MergeTlyric | `server/dedup.go:25`、`:35` 的 FNV 哈希已包含 `SubText` | 若新增字段（而非复用 sub_text）漏进哈希，**歌词变化不触发推送**。#22 这一点做对了，别在提级时弄坏 | `server/` 在可测白名单内；加一个断言「SubText/TextDetailed 变化必须产生不同哈希」的单测 |
| **全期** | 任何改动 | **无回归网** | 四条 workflow 里 `go test` 出现 **0 次**、无 `pull_request` 触发器 = **没有任何合并门禁** | **这正是 P0-2 存在的理由。在 P0-2 落地前，P1 及之后的所有改动都只能靠手测——这是把 P0 排在最前面的全部理由。** |

---

## 附：一句话总结

**先让系统能说出「我不行」（P1），再谈收集它说了什么（#33）——两者是同一个地基的两面。** #32 只是共用了「版本」这个词，它的正解是一个 30 行的 drift test，不是一个抽象。而在此之前，先把已经修好的地基提交并焊上门禁（P0）——vet 和 gofmt 现在是绿的，这个窗口只有一次。