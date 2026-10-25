# 酷我构建匹配设计：放开渠道后缀，PlayerCore 按结构匹配

状态：设计，待实现。关联 #51、PR #124。

## 1. 背景

PR #124 把酷我的读取准入钉死在一个构建 `9.6.0.0_W1` 上。2026-10-24 在另一台机器上，酷我目录由 `9.3.9.0_BD1` 变成 `9.6.0.0_UG1`（推测为客户端自升级，未核实），被全部拒绝。逐项对比后，差别只有三处钉死值：

| 钉死值 | 位置 | W1（作者记录） | UG1（实测） |
|---|---|---|---|
| CDP 桥版本串 | `cdp/client.go:25,103` | `MUSIC_9.6.0.0_W1` | `MUSIC_9.6.0.0_UG1` |
| CDP 页面目录 | `cdp/client.go:124` | `/9.6.0.0_w1/bin/html/webdata/netsong/` | `/9.6.0.0_ug1/bin/html/webdata/netsong/` |
| PlayerCore PE 时间戳 | `pos/core_windows.go:19,91` | `0x6a1fa2a8` | `0x6a2641e4` |

UG1 的 PlayerCore 其余特征与 W1 一致（静态比对磁盘文件）：映像大小 `0x2f000`；导出 `GetPlayerCore` 在 RVA `0x12890`，机器码 `mov eax,0x10025750; ret`，即单例 RVA `0x25750`；单例的 vtable 在 RVA `0x1e3c8`；读位置函数的机器码在 RVA `0x14e70` 逐字出现，并且它是该 vtable 的第 38 个槽位。把三个钉死值临时改成 UG1 后，PR 的完整流程在 UG1 上端到端跑通（歌词、译文、封面、位置、1:1 走时）。

结论：后缀（W1、UG1）是发行渠道标签，不同用户会拿到不同后缀，不应写死。

## 2. 目标与非目标

目标：

1. 读取路径（CDP 与 PlayerCore 内存只读）对 `9.6.0.0` 的任意渠道后缀放行。
2. PlayerCore 的准入由「结构证据」取代「时间戳加固定 RVA」：重新编译后偏移变化但结构仍成立的构建，可以接入。
3. 保持 fail-closed：任何一项结构证据缺失，一律降级，不猜测。
4. 拒绝时的状态要说真话，并且可操作（见 §3.4）。

非目标：

- 不放开数字版本 `9.6.0.0`。其他数字版本没有任何证据，继续拒绝。
- 不做「只靠位置字段在走」的行为兜底：暂停时位置不动，无法判断；字段偏移没有结构证据时，行为一致不能证明语义一致。
- 不放开受控重启。重启是破坏性动作，仍只对已知构建放行（见 §3.3）。
- 不动其他播放器，不动 README 首表（#125）。

## 3. 设计

### 3.1 CDP 准入（`player/kuwo/cdp`）

- 桥版本串必须匹配 `^MUSIC_9\.6\.0\.0_([A-Za-z0-9]{1,16})$`，数字部分固定，后缀取第一个捕获组。
- 页面选择：`file://` 页面路径（小写、`path.Clean` 后）必须含 `/9.6.0.0_<后缀>/bin/html/webdata/netsong/` 且以 `.html` 结尾，后缀同样限定 `[a-z0-9]{1,16}`。仍优先 `index.html`，仍无「第一个页面」兜底。
- 一致性：连接后桥报告的后缀与所选页面目录里的后缀必须相等（不分大小写），否则拒绝。这是后缀放开后保留的一条交叉校验。
- 桥版本、目录、一致性三类拒绝统一返回 `ErrVersionUnsupported`（`cdp` 包内新增，区别于「连不上」的 `ErrUnavailable`），以便 §3.4 把它与连接故障区分开。`selectTarget` 需要把页面后缀带出来，签名可以改，测试同步。

### 3.2 PlayerCore 结构匹配（`player/kuwo/pos`）

`coreModule` 取得模块基址与映像大小（Toolhelp 的 `ModBaseSize`），不再要求等于 `0x2f000`。`validateCoreImage(read, module, imageSize)` 返回单例地址与 vtable 值，步骤如下。任何一步不满足返回 `ErrUnsupportedCore`；读失败返回原错误；对象尚未构造返回 `ErrInvalidSample`（可重试）。

**统一的受检区间规则。** 所有来自远端内存的偏移、长度、计数先用 `uint64` 做减法式检查再转换：`inImage(rva, n)` 当且仅当 `n > 0`、`rva < imageSize`、`n <= imageSize - rva` 才为真；绝对地址同理对 `[module, module+imageSize)` 检查（`module` 与 `imageSize` 之和不得溢出 32 位目标地址空间）。任何 `read(...)` 调用前必须先过这条检查；计数乘法先限幅再乘。读到的长度与请求不符一律视为失败。

1. 头页：读 `module` 起 `0x1000` 字节，必须读满。`MZ`；`e_lfanew` 满足 `0x40 <= e_lfanew <= 0x1000-0x108`；`PE\0\0`；Machine `0x14c`；可选头魔数 `0x10b`；`SizeOfOptionalHeader >= 0x70` 且 `NumberOfRvaAndSizes >= 1`；`SizeOfImage` 等于 `imageSize` 且在 `[0x4000, 4 MiB]`。**不比较时间戳**（只用于日志）。同时解析节表：`NumberOfSections` 在 `[1, 16]`，节表整体落在头页内；每一节的区间（`VirtualAddress` 起，长度取 `VirtualSize`，为 0 时取 `SizeOfRawData`）必须 `inImage`；特征位含 `IMAGE_SCN_MEM_EXECUTE (0x20000000)` 的节记为「可执行区」，至少要有一个，否则拒绝。
2. 导出表：取 DataDirectory[0] 的 RVA 与大小，`inImage` 成立，且大小在 `[40, 64 KiB]`（`IMAGE_EXPORT_DIRECTORY` 本身 40 字节，更小的目录无法承载头部），整块一次读入（读满）。从这块内存解析：`NumberOfNames <= 256`、`NumberOfFunctions <= 4096`；`AddressOfNames`、`AddressOfNameOrdinals`、`AddressOfFunctions` 三张表各自按条目数乘 4 或 2 做 `inImage` 检查，并且必须完整落在导出目录这块内存内（拒绝指向目录之外的表）；每个名字从目录内存里取，长度同时受「目录内剩余字节」与 64 字节上限约束，且必须有结束符；按名字精确匹配 `GetPlayerCore`，必须恰好出现一次；序号小于 `NumberOfFunctions`；函数 RVA 的入口 6 字节满足 `inImage`，且不落在导出目录范围内（排除转发导出）。
3. 单例：读函数入口 6 字节，必须是 `b8 imm32 c3`。`imm32` 是加载后已重定位的绝对地址，必须 4 字节对齐，且 `[imm32, imm32+coreSize)` 落在 `[module+0x1000, module+imageSize)` 内。
4. 绑定读位置函数：读单例对象首个 dword 得 vtable 指针 `V`。`V == 0`（对象尚未构造）返回 `ErrInvalidSample`；`V` 必须 4 字节对齐且落在映像内，并且 `V-4` 处（MSVC 的 RTTI 位置）也在映像内，其值是一个落在映像内、**不在可执行区**的指针（数据区指针），否则拒绝：这是「`V` 确是一张虚表的起点」的证据。**虚表边界由「槽位指向可执行区」界定**：从 `V` 起逐槽读取（最多 256 槽，受映像末尾限制，一次读入），第一个不指向可执行区的槽位即表尾；MSVC 在下一张虚表之前放的是指向数据区的 RTTI 指针，所以遍历不会滑进相邻类的虚表。对表内每个槽位指向的地址，读 `len(coreGetterCode)` 字节（先过 `inImage`）并与 `coreGetterCode` 逐字节比较，**至少一个槽位相等才通过**；表为空也拒绝。命中的槽位序号不钉死、只记录。这条取代「在整个映像里搜机器码」：读位置函数必须是**当前单例所属类的虚方法**，从而证明对象内 `+0x188` 读后端、`+0x2a0` 写位置的偏移属于这个对象的布局，而不是映像里某处遗留的旧函数或相邻旧类。模糊的是槽位序号，内容仍是逐字节精确。UG1 实测（磁盘文件与运行中的进程一致）：`V-4` 指向 `.rdata`，表长 65 槽，读位置函数是第 38 槽，第 65 个值不是代码指针，遍历在此结束。
5. 稳定性：打开时记住 `V`。之后每次 `Read` 读到的首个 dword 必须等于它，否则返回 `ErrUnsupportedCore`；为 0 返回 `ErrInvalidSample`。

不变的部分：`coreSize`、RID `+0x84`、时长 `+0xa8`、后端 `+0x188`、位置 `+0x2a0` 四个偏移，以及 `readCoreSample` 的全部动态校验（RID 格式并等于 CDP 的 RID、后端为 1、时长与位置范围、前后两次读取比对、250ms 与 50ms 的撕裂判定）。RID 与 CDP 这个独立来源逐样本对账，是偏移 `+0x84` 成立的持续证据。

读取量：打开时约 4 KiB（头页）加导出目录（本构建很小）加至多 256 个槽位（1 KiB）与每槽 94 字节；之后每个采样周期与现在一样读 `coreSize` 两次，另加 4 字节 vtable。不再扫描整个映像，不存在块重叠与累计预算问题。

### 3.3 受控重启（`player/kuwo/watchdog`）

重启资格只由「已知构建表」决定，目录后缀不独立放行：

```
knownBuilds = { "w1": SHA256(W1 的 kwmusic.exe), "ug1": SHA256(UG1 的 kwmusic.exe) }
```

`verifyExecutable(exe)` 通过当且仅当：路径是绝对路径；文件名、父目录名、祖父目录名按 Windows 语义**不分大小写**比较，依次为 `kwmusic.exe`、`bin`、`9.6.0.0_<s>`；`<s>`（小写）在 `knownBuilds` 里，并且文件的 SHA256 等于该条目的哈希。其余一律 `ErrVersionUnsupported`：未知后缀；已知后缀但哈希不符；已知哈希放在未知后缀目录下；路径含 `..`；父目录不是 `bin`。文件大小上限与「是普通文件」检查保持不变。

其余逻辑（每个服务运行最多一次、重启前后重新枚举、PID 创建时间核对）不变。重启仍在端口未就绪、未被外来监听占用、命令行未带调试参数时才考虑。

后果：未知构建（例如新的渠道后缀）不会被自动重启，用户需要自己带 `--remote-debugging-port=9224` 启动；只要调试口已开，读取路径照常放行。新增一个已知构建 = 往 `knownBuilds` 加一行，并在 README 与 §14 里记录来源与取证方式。

### 3.4 状态与错误映射

需要区分的几种情形，以及它们到达状态输出（`kuwo.go` 的 `Start`）的路径：

| 情形 | 错误 | 状态 `reason` | 文案 |
|---|---|---|---|
| 酷我没在运行 | `ErrNotRunning` | `process_not_running` | 沿用 |
| 调试口通了，但桥版本 / 页面目录 / 后缀一致性不通过 | `cdp.ErrVersionUnsupported` | `version_unsupported` | 沿用「暂不支持此酷我播放核心」 |
| PlayerCore 结构不匹配 | `pos.ErrUnsupportedCore` | `version_unsupported` | 沿用 |
| 调试口没开，且该构建不在 `knownBuilds`，所以不会被自动重启 | 新增 `watchdog.ErrRestartNotAuthorized` | `cdp_disconnected` | 新增（中英双语）：「酷我需要以调试端口 9224 启动；自动重启只对已验证的构建开放，请手动带 `--remote-debugging-port=9224` 启动酷我」 |
| 其余连不上 | `ErrPortNotReady` 等 | `cdp_disconnected` | 沿用 |

要点：`watchdog.debugReady` 目前把一切连接失败压成 `false`，端口仍被占用时最终落成 `ErrPortNotReady`。实现需要让 `cdp.ErrVersionUnsupported` 穿过 `debugReady` 与 `ensure`，原样到达 `Start` 的 `errors.Is` 分支；不能被「端口被占用」覆盖。外来端口占用时不重启的保护保持不变。

接入成功时打一条 Detail 级日志，记录后缀、PlayerCore 时间戳与命中的 vtable 槽位序号（英文 format 串，补中文译文），直播出问题时能看到是哪个构建在跑。

### 3.5 活性校验（结构通过之后的第二道证据）

静态结构无法精确给出虚表长度（「连续代码指针」只是近似边界），所以在会话层再加一道行为证据，只补充结构匹配，**不替代**它：

- 对每首歌（RID 变化即重置）：CDP 状态为 playing（`Status == 1`）的累计时间达到 20 秒，而这首歌从第一个有效样本起核心位置从未变化过，则返回 `pos.ErrClockStalled`，会话结束并 fail-closed。
- 条件刻意收窄为「整首歌一次都没动过」：用户暂停（`Status == 2`）会使计时清零；拖动、缓冲在歌曲中途停顿，位置此前动过，不触发；换歌重置。因此正常使用里不会误判。
- 触发时打一条 Warn（英文 format 串，补中文译文）说明「播放中但核心位置不走」，状态走现有 `cdp_disconnected` 通路，不冒充「版本不支持」。阈值是包级变量，测试可调。
- 它抓的是这类场景：结构证据都通过，但位置字段实际已经移到别处，旧偏移上留着一个合法的静止值（位置恒为某个常数，RID、时长、后端都合法）。

## 4. 与现有机制的对照

- 按名字解析远端 PE 导出表有先例：`player/wesing/lyric/finder.go:67` 的 `findExportFunction`。本设计在 `pos` 内基于 `memoryRead` 另写一份，便于用合成镜像做离线测试，不引入对 wesing 包的依赖。
- 纯解析与校验逻辑放在不带构建约束的文件里，让它在 `!windows` 下也能编译和测试；只有 Toolhelp、`OpenProcess` 这类系统调用留在 `_windows.go`。

## 5. 反例清单（测试必须覆盖）

CDP：后缀 `W1`、`UG1`、`X9` 通过；数字部分 `9.6.0.1`、`9.7.0.0`、`10.6.0.0`、缺后缀、空后缀、后缀超长、含非 ASCII、含 `/` 与 `..`、前缀大小写错误、空串均拒绝；页面目录后缀与桥后缀不一致拒绝；页面目录后缀合法但路径其余部分不符拒绝。传输用例的假服务页面后缀要参数化（`W1`、`UG1`），不再固定 W1。拒绝类型：版本 / 目录 / 一致性不通过得到 `ErrVersionUnsupported`，连不上得到 `ErrUnavailable`。

PlayerCore（合成镜像，用 `memoryRead` 假实现）：基线通过；时间戳不同通过；映像大小不同（与 `imageSize` 一致）通过；加载基址不同通过；读位置函数在虚表的不同槽位通过；`imageSize` 与 `SizeOfImage` 不一致、`SizeOfImage` 过大或过小、Machine 不对、魔数不对、`e_lfanew` 为 `0x7fffffff` 或越界、`SizeOfOptionalHeader` 过小、`NumberOfRvaAndSizes` 为 0 拒绝；节表：`NumberOfSections` 为 0 或 17、节表越出头页、某节区间越出映像、没有任何可执行节拒绝；导出表缺失、导出目录越界、导出目录大小为 1 到 39 字节（头部放不下，合成镜像里该地址有完整可读内存）、名字的 64 字节上限与目录内剩余长度冲突（名字顶到目录末尾且无结束符）、`AddressOfNames` 指向映像外（即使假内存在该地址可读）、三张表指向导出目录之外、`NumberOfNames` 或 `NumberOfFunctions` 超限（如 `0x40000001`）、名字缺失、名字重复、名字无结束符、序号越界、转发导出、入口 RVA 为 `imageSize-1`（6 字节越界）拒绝；入口不是 `b8 .. c3`、`imm32` 越界或不对齐拒绝；**关联反例**：完整的 `coreGetterCode` 存在于映像里但不在单例 vtable 的任何槽位里，必须拒绝（对应真实场景：旧函数遗留、新布局把位置移到 `+0x2a4`）；**相邻旧表反例**：当前类的虚表很短（例如只有两个槽位），紧随其后是一个数据区的 RTTI 指针和另一个旧类的虚表，旧读位置函数位于后者，必须拒绝（遍历须在 RTTI 指针处停止，而不是继续走满 256 槽）；`V-4` 越出映像、`V-4` 的值落在可执行区、`V-4` 读失败拒绝；表首槽位就不指向可执行区（表为空）拒绝；vtable 里槽位指向的函数字节差一位拒绝；`V` 为 0 返回可重试错误；`V` 越界、不对齐、`V+槽位区` 越过映像末尾（按剩余槽位读取，不 panic）拒绝或按规则截断；打开后 `V` 变化拒绝；任一读失败、短读按规定处理且不 panic。

重启：`(W1 目录, W1 哈希)`、`(UG1 目录, UG1 哈希)` 通过；已知目录加未知哈希、未知后缀目录加已知哈希、未知后缀加未知哈希、路径含 `..`、父目录不是 `bin`、文件名不是 `kwmusic.exe`、相对路径均拒绝；目录与文件名大小写变化（`9.6.0.0_w1\BIN\KWMUSIC.EXE`）通过；没有任何真实安装被读取（测试用临时文件，哈希表通过可注入变量替换）。

活性校验：playing 满 20 秒且整首歌位置从未变化，返回 `ErrClockStalled` 并结束会话（用可调的阈值做快速测试）；playing 不足 20 秒不触发；暂停期间累计清零（先 playing 15 秒、暂停、再 playing 15 秒不触发）；位置动过一次之后再停顿 60 秒不触发（缓冲、拖动）；换歌后重置；`Status == 3` 或 RID 为空重置；位置每次都变的正常流不触发；触发后状态走 `cdp_disconnected`，不是 `version_unsupported`。

状态映射：CDP 版本不通过但端口仍被占用，得到 `version_unsupported`，不得变成 `cdp_disconnected`；未知构建、端口关闭，得到「需要手动带参数启动」，并且断言没有 stop、start 调用；外来监听占用端口时依旧不重启。

## 6. 需要同步的文档与夹具

文档里所有把准入写成「仅 W1」「已验证指纹含时间戳」的陈述按新规则改写，并区分三类内容：历史实测记录（保留，标明是哪一台机器、哪个构建）、读取准入规则、自动重启白名单。落点：README 酷我小节，`doc/openapi.yaml`（含第 10 行附近的接入边界说明）、`doc/API_RESPONSE_EXAMPLES.md`（位置读取限定 W1 的陈述），`doc/design/kuwo-cdp-hardening-notes.md`、`doc/design/kuwo-integration-recon.md`、`doc/design/kuwo-lyric-protocol.md` 中涉及钉死值的地方，`agent-doc/14-已知风险与未决.md` 的酷我条目，`config/config.go` 模板里酷我段的注释，以及 `player/kuwo/pos/core.go` 与 `cdp/client.go` 的包注释。测试夹具：`cdp/transport_test.go`、`cdp/snapshotparse_test.go`、`cdp/targetselection_test.go` 里的 W1 常量，`pos/corefingerprint_windows_test.go`、`watchdog/exefingerprint_test.go`、`watchdog/restartdecision_test.go`。README 首表与折叠区的精简另见 #125，不在本次范围。

## 7. 风险与未决

1. 重新编译后读位置函数的机器码变了，或它不再是虚方法：准入失败，用户看到「版本不支持」。需要新增一条指纹，是有意的 fail-closed。
2. 读位置函数在虚表里、对象布局在 `+0x84`、`+0xa8` 变了：由逐样本的 RID 对账与范围校验兜住，不由静态证据证明。「连续代码指针」不能精确界定虚表长度（理论上虚表后可紧邻无 RTTI 前缀的函数指针数组，旧读位置函数恰在其中且位置字段已搬家），这一残余由 §3.5 的活性校验在 20 秒内兜住：位置字段整首歌不动即 fail-closed。风险低，不为零。
3. W1 无法在评审机上复测：W1 的行为由 PR 作者的真机记录与按其钉死值构造的合成回归支撑。W1 的 vtable 槽位序号未能独立核验，依据是 W1 与 UG1 的映像大小、导出、单例、vtable、读位置函数的 RVA 全部相同；合入后须请作者在 W1 上复跑一次真机。
4. 其他数字版本（如 9.3.9.0）仍不支持，需要单独取证。
