# AGENTS.md — Metabox-Nexus-PlayerCap 开发指南（给 AI agent / 协作者）

本文件与 [agent-doc/](agent-doc/) 下的各章是在本仓库工作的**强制流程**，也是仓库内唯一的开发规范。
本文件只放最高约束与各章索引，第 1～14 章各在 `agent-doc/` 下的一个文件里。

这个服务在主播直播时跑在播控机上，OBS 从它拉歌词、特效与音频波形 overlay。**它挂了 = 直播事故。**
它对接六个没有公开歌词 API 的音乐播放器（五个国产播放器与 Apple Music for Windows），手段是内存扫描 / CDP / 二进制补丁 / 代码注入，
因此运行在一组脆弱假设之上。质量优先级：**稳定 > 正确 > 简洁 > 功能完整度**。

---

## 0. 最高约束

读不完全文就只读这五条。每一条都对应已经发生过的故障。

1. **绝不炸主播的机器。** 我们读别人的进程内存、改别人的 DLL、移别人的窗口。任何一步失手，
   代价由主播在直播中承担，且往往**不可自行恢复**（例：酷狗补丁打坏 → 只能重装酷狗）。
   宁可降级（不显示歌词、明确报「不支持」），绝不盲干。
2. **绝不改网易云的特效 canvas。** 改它的 `width`/`height`/`style` 会让网易云**崩溃**。
   捕获只能 `drawImage` 只读拷贝。分辨率上限由网易云窗口尺寸决定，唯一的调节手段是请用户
   放大窗口。
3. **`config/`、`logger/`、`i18n/` 必须保持 `GOOS=linux` 可构建。** 三者恰是 `tools/genconfig`
   的全部内部依赖（`config` 与 `logger` 都 import `i18n`），而两条打包流水线在 Linux runner 上
   原生跑它生成随包 `config.yml`。`i18n` 与 `config` 的 Windows 专有代码只能放在带构建约束的文件里，且
   `!windows` 侧必须有桩（现为 `i18n/detect_windows.go` / `detect_other.go`、
   `config/replacefile_windows.go` / `replacefile_other.go`）。破坏它
   = 两条流水线同时挂，且失败点报在「打包」而非「构建」，排查方向容易被带偏。
   **这是 CI 的实现细节，不是平台目标——本项目是纯 Windows。**
   自检：`GOOS=linux GOARCH=amd64 go build ./config/... ./logger/... ./i18n/... ./tools/genconfig`
4. **`syscall`/`windows.NewCallback` 只能在包级 `var`。** 它有全局上限（2000）且**永不回收**，
   在轮询路径上每 tick 新建会很快耗尽配额并使进程失败。样板见 `player/wesing/proc/memory.go`。
   ✅ 有门禁：`player/callback_lint_test.go`
5. **绝不给构建加 `-H windowsgui`。** 本程序是控制台应用，全部日志经 `log.Printf` 走 stderr
   **且不落盘**。改成 GUI 子系统后 stderr 无处输出，日志全部丢失，直播出问题时没有现场证据。
   （`-H windowsgui` 不在任何构建脚本中：两条流水线的 ldflags 只有 `-X`。真要无窗口运行，
   前置条件是先给 logger 加文件 sink，见 §3.7。）

---

## 各章索引

第 1～14 章各成一个文件，放在 [agent-doc/](agent-doc/)。章号、节号与标题同拆分之前：
代码注释与文档里写的「AGENTS.md §x.y」，即第 x 章那个文件里的 x.y 节。

| 章 | 文件 | 内容 |
|---|---|---|
| §1 这个项目是什么 / 数据怎么流 | [agent-doc/01-项目与数据流.md](agent-doc/01-项目与数据流.md) | 定位、启动顺序、四条数据通路、目录结构 |
| §2 承重设计原则 | [agent-doc/02-承重设计原则.md](agent-doc/02-承重设计原则.md) | 路由与优先组的设计前提：fan-out 非阻塞、holding 与超时、缓存更新与广播解耦 |
| §3 硬规则：禁止触碰 | [agent-doc/03-硬规则.md](agent-doc/03-硬规则.md) | 各处的禁区：路由与优先级、播放器契约、配置、服务层与 wire、自动更新与发版、Windows 机制、错误处理与构建 |
| §4 规矩的守卫状况 | [agent-doc/04-守卫状况.md](agent-doc/04-守卫状况.md) | 哪些规矩有自动门禁、哪些靠人守、门禁的盲区 |
| §5 怎么加东西 | [agent-doc/05-怎么加东西.md](agent-doc/05-怎么加东西.md) | 加播放器、加配置项、加端点的步骤 |
| §6 日志与文案规范 | [agent-doc/06-日志与文案.md](agent-doc/06-日志与文案.md) | 日志格式、允许直接打印的例外、i18n 约定 |
| §7 Windows 机制 | [agent-doc/07-Windows机制.md](agent-doc/07-Windows机制.md) | 内存读写原语、NewCallback、park、canvas 只读，以及 wesing、sodamusic、applemusic、cloudmusicv3 各自的机制 |
| §8 写完跑什么 | [agent-doc/08-写完跑什么.md](agent-doc/08-写完跑什么.md) | 四道门禁、改了 config / logger / i18n 必跑的、CI 实际覆盖什么 |
| §9 验证方法学 | [agent-doc/09-验证方法学.md](agent-doc/09-验证方法学.md) | 测试怎样才算数、破坏性验证的做法、「已验证」的使用门槛 |
| §10 提交前 | [agent-doc/10-提交前.md](agent-doc/10-提交前.md) | 提交策略、commit message 的门槛、工作树卫生 |
| §11 硬化与复查 | [agent-doc/11-硬化与复查.md](agent-doc/11-硬化与复查.md) | 何时做复查、对抗性证伪、判级与沉淀 |
| §12 发版 | [agent-doc/12-发版.md](agent-doc/12-发版.md) | 客户端的更新判据、两个 draft、清缓存的依赖链 |
| §13 仓库速览 | [agent-doc/13-仓库速览.md](agent-doc/13-仓库速览.md) | 模块与依赖、目录树、tools/ 工具、图标、doc/ 的滞后点、路由表 |
| §14 已知并接受的风险 / 未决事项 | [agent-doc/14-已知风险与未决.md](agent-doc/14-已知风险与未决.md) | 有意接受的风险与未决事项 |
