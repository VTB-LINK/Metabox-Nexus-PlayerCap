# 版本检查请求的客户端标注 —— 网关侧统计口径

本文档描述客户端向 `client-version` 端点发出的请求上带了什么、为什么，以及网关侧
怎么用它算 DAU/MAU 与启动次数。

代码真源：`clientid/headers.go`（`HeaderNames()`）、`main.go` 的 `newVersionCheckRequest`
与 `sendHeartbeat`。**本文档是下游描述，与代码分叉时以代码为准。**

---

## 1. 客户端行为

| 时机 | 频率 | 触发条件 |
|---|---|---|
| 启动 | 每次进程启动一次 | `isReleaseVersion(Version)` 为真（dev 构建不发） |
| 每日在线心跳 | 每个本地日历日一次 | 同上，且启动那次已经过去 |

两者打的是**同一个 URL**（`.../v2/client-version`）、走同一套请求头，靠 `X-Client-Ping`
区分。共用 URL 是有意的：网关侧不必为心跳单独配 route 就能开始统计。

心跳**只发请求、丢弃响应体**，不解析、不下载、不重启——运行期换 exe 就是直播事故。
有 AST 门禁守着（`heartbeatinert_test.go`）。

心跳的唤醒间隔是 1 小时，但只有**本地日历日变了**才真的发。不用 24h ticker 的理由：
ticker 走单调时钟，机器休眠期间是否推进依系统而定，一台每天休眠十小时的播控机会让
周期不断后漂，漂满一天就整天不上报——而这只表现为 DAU 曲线偏低，无报错、无日志。

---

## 2. 请求头

| 头 | 例 | 说明 |
|---|---|---|
| `X-Client-Id` | `9f2a…`（32 位十六进制） | 匿名设备标识。`SHA256(域前缀 + MachineGuid)` 取前 128 位。**去重就靠它** |
| `X-Client-Version` | `3.0.0-rc.14` | 客户端版本号（ldflags 注入的 `main.Version` 原值） |
| `X-Client-OS` | `10.0.19045` | Windows 真实版本（免疫兼容性 shim） |
| `X-Client-Edition` | `Enterprise` | Windows edition |
| `X-Client-Arch` | `x64` | CPU 架构 |
| `X-Client-Ping` | `start` / `daily` | 启动 / 每日在线 |
| `User-Agent` | `Metabox-Nexus-PlayerCap/3.0.0-rc.14 (Windows NT 10.0.19045; x64)` | 取代 Go 默认的 `Go-http-client/1.1` |

**采不到的头整个不发，不发空值。** 空值在日志里像「采到了，是空的」，缺头才是「没采到」，
两者在报表里是不同的结论。

`X-Client-Id` 采不到的唯一情形是 MachineGuid 读不出来（客户端会打一条 `[ClientID] [!]` 告警）。
这时客户端**不会编一个随机值**：那会让同一台机器每次启动都算成新设备，把 DAU 灌成启动次数，
而且曲线只会变好看，没人会去查。

### 标识为什么是这个粒度

`MachineGuid` 是 Windows 装机时生成的 GUID，只读、不需要管理员权限、换目录/重下载/重装本程序
都不变。代价：

- 重装系统 → 算成一台新机器
- 同机多个 Windows 用户 → 算成一台
- 克隆的虚拟机 → 共用同一个 GUID，算成一台

对「今天有多少台播控机开过」这个问题，这个粒度是对的。要「多少个人」得有账号体系，我们没有。

哈希是必须的：原样发 MachineGuid 等于给每台机器发一张可跨服务关联的身份证。域前缀让这个标识
只在本用途下有意义——换用途换前缀，两边哈希对不上，跨用途关联在机制上就不成立。

**域前缀改了 = 全体客户端换一批新 ID**，报表会看到一次性的「老用户全部流失 + 等量新增」，
且跨越那次改动的留存曲线永久断档。

---

## 3. 为什么走请求头而不是 query

### 现状：这条 route 上没有响应缓存（2026-08-10 实测）

连发三次，每次都带 `X-Kong-Upstream-Latency`（336~373ms），**没有** `X-Cache-Status`、
**没有** `Age`：请求条条回源，proxy-cache 没有生效在这条 route 上。

```
Via: kong/3.4.1.0-enterprise-edition
Cache-Control: max-age=60, s-maxage=60, private     ← 来自上游，不是网关加的
X-Kong-Upstream-Latency: 373
X-Kong-Proxy-Latency: 16
```

仓库里那个 `purge-release-cache-on-latest-change.yml` 容易让人推断出「这条 route 挂着
proxy-cache」——**那是推论，不是事实**，而且它自己也是 disabled 状态（见 AGENTS §12）。
别把它当成缓存存在的证据。

两个直接后果：

- 每台客户端每天那一两次请求都会打到上游（~350ms）。量级小的时候无所谓，值得知道。
- 日志插件不存在「缓存命中时会不会跳过 log 阶段」的问题——**每条请求都必然被记录**。

### 但请求头仍然是对的选择

这是**面向将来**的约束，不是对现状的描述。一旦有人给这条 route 开了 proxy-cache：

- 走请求头 → 默认缓存键是「方法 + URI + query」，不含请求头，所有客户端共用一个缓存条目
- 走 query → 每台机器一个缓存键，缓存等于没开

⚠️ 真开缓存时另有两件事要处理：上游回的 `Cache-Control: private` 会让 proxy-cache 默认
不存（需 `config.cache_control=false` 或改上游）；且**别把上面任何一个头加进 `vary_headers`**，
那等价于走 query。同时第 4 节那条「缓存命中还记不记日志」的验证就重新变成必做项。

---

## 4. 网关侧怎么出数

三个数都从同一份访问日志里出：

| 指标 | 算法 |
|---|---|
| **DAU** | 当天 `X-Client-Id` 的去重计数（`start` 与 `daily` 都算——只要出现过就是在线） |
| **MAU** | 近 30 天 `X-Client-Id` 的去重计数 |
| **每日启动次数** | 当天 `X-Client-Ping = start` 的**请求条数**，不去重 |
| **人均启动次数** | 上面两个相除 |
| **版本分布 / 升级渗透率** | 按 `X-Client-Version` 分组去重计数 |

**人均启动次数反常地高不是「主播勤快」**，通常是崩溃重启或播放器接不上在反复重试——
它是个能提前发现故障的信号，值得单独做一条曲线。

### Kong Manager / Vitals 出不了 DAU

Vitals 的聚合维度是 service / route / consumer / 状态码，**没有按自定义请求头做去重计数的能力**；
Prometheus 插件同理（counter/histogram 做不了 distinct，把高基数的设备 ID 塞进 label 更是灾难）。

所以路线只有一条：**日志插件把这几个头打出去 → 落进一个能算 `count(distinct)` 的存储 → 出图**。

### 落地路径（按新增依赖从少到多）

1. **零新增依赖**：`file-log` 插件把 JSON 行写到 Kong 所在 VM 的磁盘，用一个每日脚本
   （`jq` + `sort -u | wc -l`）出 CSV。够回答「昨天多少台」，不够做趋势图。
2. **一个存储 + Grafana**：`http-log` 插件推到 ClickHouse（`uniqExact(client_id)` 正是
   为这种查询存在的）或 OpenObserve（单二进制、自带面板），Grafana 出图。**MAU 需要
   30 天窗口内的去重，这条路是唯一稳妥的做法。**

两条都建议用 `custom_fields_by_lua` 把日志裁到只剩需要的字段——默认序列化会带上完整的
请求/响应头与 latency 明细，量大且没用。

### 需要在 3.4.1 上自己验的

本文档写的是通用做法，下面这点请在实例上确认，别照抄：

- **`custom_fields_by_lua` 能否靠返回 nil 删掉顶层字段。** 不同版本对此处理不一致。
  验法：打一条日志出来看 `request` 还在不在。不行就退回默认序列化，先跑起来再优化——
  字段多只是费磁盘。

「缓存命中时日志插件还发不发」这条**目前不适用**：这条 route 上没有 proxy-cache（见 §3），
每条请求都回源、都会被记录。哪天开了缓存，这条就重新变成必做的验证项。

---

## 5. 改动这套东西时的规矩

- **改请求头名 = 报表断档。** 旧版客户端仍在发旧名，要等自动更新一轮一轮淘汰干净才会消失。
  真要改，网关侧必须同时接受新旧两个名字，直到旧版本清零。
- **新增请求头必须同时改隐私提示**（`telemetry/privacy.go` 的 `noticeUpdateSection`）。
  有门禁：`telemetry/gatewaynotice_test.go` 拿 `clientid.HeaderNames()` 逐项核对，加了头
  不改文案即红。反向也查——登记了却不再发的要清掉。
- **`clientid` 绝不依赖 `telemetry`。** 依赖方向是 telemetry 的**测试**引用 clientid；
  反过来加一条就成环，那条门禁直接编译不出来。
