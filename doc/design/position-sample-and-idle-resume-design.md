# 隐藏解除时的整套补发与播放位置采样设计

本文覆盖里程碑 4.0 的两项服务端改动，二者都落在 `server/server.go` 的状态缓存与补发上：

- issue #69：指定播放器的通道解除无活跃隐藏时，向该通道的连接重发歌曲信息与歌词。
- issue #70：连接补发与 HTTP 快照的位置外推在无歌词歌曲上累计超前；由播放器周期性报位置采样校准。

位置外推本身见 `PlayerState.livePosition` 与 `/service-status` 的 `capabilities` 项 `init_position_live`，无活跃自动隐藏见 `sweepIdleHide`。

状态：已实施（2026-10-09），§9 的真机确认项待验。

## 1. 目标与非目标

**目标**：
- 通道由隐藏态恢复时，该播放器的 WS 与 SSE 连接拿到与「一直在线的连接」相同的数据，不必等到下一次换歌。
- 播放中，服务端缓存的位置与播放器一侧时钟的偏差有上界，不随播放时长累计。
- 根路径（`/ws`、根 SSE、根 HTTP）的事件内容与顺序不变。

**非目标**：
- 不把位置采样广播给客户端。一直在线、自行按墙钟插值的前端同样会超前，本设计不处理（#70 已定只校准缓存）。
- 不改各播放器判定暂停、跳转的门槛，不改各家本地时钟的算法。
- 不改「任何对外事件都解除隐藏」的现行判据。

## 2. 现状

### 2.1 解除无活跃隐藏（#69）

- `sweepIdleHide` 把静默达阈值的通道置为 `IdleHidden`，并在持 `s.mu` 时经 `notifyPerPlayerIdleClear` 推清除：WS 收 `player_clear`，类型过滤的 SSE 收 `lyric_update`（index:-1）或空的 `song_info_update`。
- 隐藏态期间新连入的连接，`buildInitEvents` 只给一条 `player_clear`；SSE 连接因类型过滤什么都收不到。
- `UpdatePlayerState` 遇到任何非内部事件即把 `IdleHidden` 置回 `false`，不做别的。触发解除的那条事件由 `router.Run` 随后调 `NotifySubscribers` 照常下发。

离线回放（按 `router.Run` 的顺序先 `UpdatePlayerState` 后 `NotifySubscribers`；缓存里有状态、歌曲信息、整首歌词与当前行，隐藏后连入，再以 `playback_resume` 恢复）：

| 连接 | 解除时收到 |
|---|---|
| `/<player>/ws` | 仅 `playback_resume` |
| `/<player>/lyric_update-SSE` | 无 |
| `/<player>/song_info-SSE` | 无 |

隐藏之前已在线的 SSE 连接同样缺：进入隐藏态时当前行与歌曲信息已被清掉，解除后没有重发。

### 2.2 位置外推的漂移（#70）

`livePosition` 在状态为 `playing` 时给出「最近一次采样的位置 + 经过的墙钟时间」。采样点只有 `lyric_update`、`all_lyrics`、`playback_pause` / `playback_resume` 的到达，以及状态进出 `playing`。无歌词的歌发过一次 index:-1 后不再有采样。

各播放器的本地时钟都会在不发事件的情况下偏离墙钟：

| 播放器 | 本地位置的来源 | 不发事件的偏离 |
|---|---|---|
| wesing | 每轮直读内存里的播放时间 | 停顿不足 1s（`pauseDuration`）不判暂停；前跳没有检测 |
| qqmusic | 内存进度变化时落锚，其间按墙钟插值 | 停顿不足 1s 不判暂停；2s 以内的跳变不算跳转 |
| applemusic | `clock`：整秒读数每跳一次把位置夹回允许范围 | `clock.observe` 在范围外 3s 以内只重锚、返回 `evNone`；停顿不足 1.6s（开播或跳转后 8s 内 3s）不判暂停，`clock.live` 其间停在上限 |
| kugou | 每轮以平台进度落锚，其间按墙钟插值 | 3s 以内的偏差每轮直接吸收 |
| sodamusic | 平台进度变化时（约 1Hz）落锚，其间按墙钟插值 | 3s 以内的偏差直接吸收；缓冲中（`isLoading`）状态仍为 playing、位置停住 |
| cloudmusicv3 | 纯墙钟（`baseRealClock`），与服务端外推同式 | 无：偏差超过 1.0s（回跳）或 1.5s（前跳）即发 `playback_resume` |

离线回放 #70 的推演（时长 300s，位置读数每 1.5s 才前进一格、连续 40 次，短于暂停门槛）：

| 回放 | 段内带位置的事件 | 第 40 格时播放器一侧 | 服务端外推 | 超前 |
|---|---|---|---|---|
| 只驱动 `clock`：从读数 100、位置 100.5 接上，50ms 轮询 | 不适用 | 140.68s | 160.50s | 19.82s |
| 驱动整个会话：无歌词歌曲正常播放到 index:-1 发出后进入上述读数，100ms 一轮 | 0 条 | 156.73s | 176.73s | 20.00s |

服务端一侧按 `livePosition` 的算式由最近一条带位置的事件算出，未经真实的 `Server`：会话夹具用假时钟，而 `UpdatePlayerState` 的采样时刻取自 `time.Now()`。

## 3. 参考

消费方在两次上报之间自行外推、生产方在不连续处通知，是通行做法；生产方无法保证每次不连续都被察觉时，另加周期校准：

- W3C Media Session 的 position state：用户代理按「上次上报的位置 + 经过时间 × 实际播放速率」计算当前位置并钳到时长；规范的示例只在加载完成、开始播放等处调用 `setPositionState`（[Media Session §4.5 与 §12](https://www.w3.org/TR/mediasession/)）。
- MPRIS：`Position` 变化不发 `PropertiesChanged`，客户端按 `Rate` 自行推进；位置的变化与当前播放态不一致时发 `Seeked`（[MPRIS Player 接口](https://specifications.freedesktop.org/mpris/latest/Player_Interface.html)）。
- Windows 系统媒体传输控件：建议播放中约每 5 秒更新一次时间线属性，并在暂停、跳转等状态变化时再更新一次（[Microsoft Learn：Update the system media transport controls timeline properties](https://learn.microsoft.com/en-us/windows/apps/develop/media-playback/system-media-transport-controls)）。
- RTCP：发送端报告携带时钟对应关系供接收端同步，固定最小发送间隔的推荐值为 5 秒（[RFC 3550 §6.2](https://www.rfc-editor.org/rfc/rfc3550.txt)）。

前两条即现行 `livePosition` 的模型。本项目的播放器靠读内存与 CDP 取数，短于判定门槛的停顿与小幅跳变察觉不到（§2.2），符合后两条加周期校准的前提。

## 4. 设计：解除隐藏时的整套补发（#69）

### 4.1 行为

解除隐藏的判据不变：任何对外事件到达即把 `IdleHidden` 置回假，该事件照常下发。在此之后的第一条**触发补发的事件**上，该播放器的每个 per-player 订阅者先收到一次整套缓存，再收到该事件。

**哪些事件触发补发**

| 事件 | 是否触发 |
|---|---|
| `status_update`，状态按 `normalizeStatus` 归为 playing 或 loading | 触发 |
| `song_info_update`、`all_lyrics`、`lyric_update`、`playback_pause`、`playback_resume` | 触发 |
| `status_update` 的其余状态（paused、standby、waiting_process、waiting_song、error 及未知值） | 不触发 |
| `lyric_idle` 及此表未列的类型 | 不触发 |

不触发的事件照常解除隐藏、照常下发，补发留给其后第一条触发的事件。分两类的原因：六家在进程退出、未启动、无歌处都是先发状态事件、后发 `clear_song_data`，wesing 在轮询结束时还先发一条 `lyric_idle`。这些事件若触发补发，推出去的是即将被清掉的上一首；`clear_song_data` 不外发，两条 SSE 也收不到状态事件，旧歌名与旧歌词会留在画面上，进程未启动时周期重发的 `waiting_process` 还使通道不再隐藏。按事件本身判定，不依赖各家的发出顺序。

**整套的内容**

- 内容与顺序同 `buildInitEvents` 对未隐藏通道给出的结果：`status_update` → `song_info_update` → `all_lyrics` → `lyric_update`，只含缓存中已有的类型；`all_lyrics` 与 `lyric_update` 带同一个按发送时刻外推的 `position` / `progress`。这也是 `NotifySubscribersFullState` 推给根订阅者的那一套。
- 整套取自触发事件写入缓存**之后**的状态。取触发前的缓存时，触发事件若是 `all_lyrics`，会先推上一首的歌词再推新歌词。
- 补发的是那一刻的缓存。暂停超过阈值后直接换歌时，先到的事件（多为 playing 状态）触发补发，缓存里的歌曲信息与歌词还是上一首的，随后由新歌的事件覆盖；`lyric_update-SSE` 上一首的末行会留到新歌的第一行。根路径切换时的补发、从未隐藏的连接在换歌时的所见都是如此，不另处理。

**触发事件自身**，比照根路径 `switchSkip` 的规则：

- `song_info_update`、`all_lyrics`、`lyric_update`：与整套里同类型那条的 `hashEventData` 相同即不再单发。缓存刚由它写入，正常情况下相同；按哈希比而不是按类型跳过，缓存里没有该类型的条目或内容不同时照发。
- `status_update`：不比对，整套之后照常再发一次，订阅者连续收到两条相同的状态。`switchSkip` 同样从不抑制 `status_update`。
- `playback_pause`、`playback_resume`：在整套之后发。

根路径上引起切换的那条事件本身由 `skipRoot` 跳过，其后的事件才经 `switchSkip` 比对；这里把同一套规则用在触发事件本身，与 #69「先推完整缓存，再下发触发解除的事件」一致。

**范围**

- WS 与 SSE 走同一段代码，SSE 按 `matchesType` 过滤：`lyric_update-SSE` 收到缓存的当前行，`song_info-SSE` 收到缓存的歌曲信息。
- 推给该播放器的全部 per-player 订阅者，不区分连入时刻：隐藏前已在线的 SSE 连接同样需要恢复被清掉的内容；WS 连接多收一套与手中数据相同的事件，与根路径切换时的做法一致。
- 根订阅者不在推送范围内，照旧只按 `activePlayer` 与 `skipRoot` 收触发事件。

### 4.2 实现落点

- `PlayerState` 增加 `IdleResumePending bool`：隐藏已解除、整套尚未补发。
- `UpdatePlayerState`：非内部事件到达且 `IdleHidden` 为真时，清 `IdleHidden` 并置 `IdleResumePending`。其余不变，仍然只写缓存、不向订阅通道投递。
- `NotifySubscribers`：取 `activePlayer` 时在 `s.mu` 下查看该播放器的 `IdleResumePending`；标志为真且本事件触发补发（`idleResumeTrigger`，即 §4.1 的表）时取走标志，调 `buildInitEvents(evt.PlayerName, false)` 组装整套。扇出循环里，对 `sub.player == evt.PlayerName` 的订阅者先逐条投递整套（按 `matchesType` 过滤），再按 §4.1 决定是否投递触发事件。本事件不触发补发时标志保留，行为与现在相同。
- 本项不动 `router.go`。`router.Run` 对每条事件无条件先调 `UpdatePlayerState`、末尾调 `NotifySubscribers`，其间没有提前返回，`switched` 只影响根订阅者。

标志可以跨多条事件保留（例如进程未启动时一直是 `waiting_process`），其间通道若再次隐藏，下一条对外事件重新置位，结果相同。

不新增 `capabilities` 项：4.0 前端不需要按此分支（#69 写明前端无需配合），旧前端多收一套事件与根路径切换时相同。

### 4.3 并发与加锁

- 标志的读写都在 `s.mu` 下；整套由 `buildInitEvents` 在 `s.mu.RLock` 下组装；投递在 `s.subMu` 下、`s.mu` 之外，沿用 `NotifySubscribers` 现有的「`s.mu` 取快照 → 释放 → `subMu` 扇出」。新增的投递同为 `select default` 非阻塞（AGENTS §2.1），该处发送点由八处变九处。
- 整套与触发事件在同一次 `subMu` 临界区内入队，二者之间不会插入别的事件。
- 与 `sweepIdleHide` 的先后：隐藏与清除在 `s.mu` 下原子完成，`UpdatePlayerState` 取同一把锁，故清除总在整套之前入队。解除后 `LastEventAt` 刚被刷新，巡检在阈值（至少 1s）内不会再次隐藏；路由 goroutine 在 `UpdatePlayerState` 与 `NotifySubscribers` 之间停顿超过阈值时，订阅者会依次收到清除、整套与触发事件，而服务端记为隐藏。这一窗口现已存在于每条事件上，下一条事件到达即再次解除并补发，不另行处理。
- 与新连接的先后：`handleWS` / `handleSSE` 先 `subscribe` 再 `buildInitEvents`。连接在解除前注册、解除后取初始态时，会先写出完整初始态，再从通道收到一次整套，内容相同。普通事件上现有同样的重叠。

### 4.4 边界情形

| 情形 | 结果 |
|---|---|
| 隐藏态期间到达内部事件（`clear_song_data`、`position_sample`） | 照常写缓存，不解除、不补发 |
| 隐藏中进程退出：`status_update`(standby) → `clear_song_data`，其后周期性的 `waiting_process` + `clear_song_data` | 各条状态照常下发，都不补发；缓存已清空，标志保留 |
| 隐藏中 wesing 关闭 K 歌窗口：`lyric_idle` → `status_update`(waiting_song) → `clear_song_data` → `lyric_idle` | 同上，四条都不补发 |
| 上两行之后新歌开播：`status_update`(playing) → `song_info_update` → `all_lyrics` | 状态事件触发补发，整套只有这条状态，随后状态再发一次；新歌的歌曲信息与歌词照常下发 |
| 隐藏态期间缓存被 `clear_song_data` 清空，之后触发事件到达 | 整套只含剩下的类型 |
| 触发事件是 `all_lyrics` | 写缓存时 `LyricUpdate` 已置空，整套为 `status_update` → `song_info_update` → `all_lyrics`，不带上一首的当前行；触发事件与整套里那条哈希相同，不再单发 |
| 触发事件是 index:-1 的 `lyric_update` | 由整套带出，不再单发，「无歌词」在线上仍只出现一次 |
| 触发事件的类型在缓存里没有条目（如 `Lyrics` 为 nil 的 `all_lyrics`） | 整套不含该类型，触发事件照常下发 |
| 未处于隐藏态 | 标志不置位，`NotifySubscribers` 的行为与现在逐事件相同 |
| 同一条 playing 状态事件既触发根切换、又触发该播放器的补发 | 根订阅者收 `player_switch` 与整套（`NotifySubscribersFullState`），per-player 订阅者收整套与这条状态；两路互不影响 |
| 订阅通道已满 | 丢弃，不阻塞 |

## 5. 设计：播放位置采样（#70）

### 5.1 事件

新增事件类型 `player.EventPositionSample = "position_sample"`，载荷复用 `*player.PlaybackTimeInfo`（`Position` 为整曲实时位置，`Progress` 由 `ClampProgress` 算出），含义是「此刻正在播放，位置是这个值」。

它是内部事件，登记进 `internalEvents`：

- `NotifySubscribers` 开头即返回，不到达任何 WS 或 SSE 订阅者，对外文档不记载。
- `UpdatePlayerState` 不刷新 `LastEventAt`、不解除 `IdleHidden`，不计作播放器活动。
- `updateRouting` 开头对 `position_sample` 直接返回 `false`，按事件类型本身判断，不查 `internalEvents`。`clear_song_data` 的现有路径不动：它仍进入 `default` 分支的普通组重评估。该重评估是否恒为空操作只有推断，不为两种内部事件待遇一致而改；差别及原因写进注释。
- `switchSkip`：`router.Run` 只在 `switchSkipHashes` 含该事件类型时比对，表里的类型来自 `buildInitEvents`，不含内部事件，故不比对、不消耗。

### 5.2 服务端

`UpdatePlayerState` 把 `position_sample` 并入 `playback_pause` / `playback_resume` 的分支，写四个字段：`Position`、`Progress`、`PositionAt`（置为此刻）、`PositionFromPause`（置假）。其余缓存（`Status`、`SongInfo`、`AllLyrics`、`LyricUpdate`、`Duration`）不动。

- 与 `PositionFromPause`：按 §5.3 的条件，暂停事件之后、恢复事件之前不会有采样到达。清标记的作用在事件被 `Emit` 丢弃时：`paused` 状态与 `playback_resume` 都丢失时，状态始终是 `playing`、标记一直为真，位置冻结在暂停点；下一条采样把它清掉。
- 与状态冻结：进出 `playing` 时仍按 `livePosition` 冻结或重启时钟，采样只让冻结值的起点更近。状态不是 `playing` 时到达的采样写入位置但不外推。
- 尚无歌词缓存时到达的采样只写位置字段，`buildInitEvents` 不因此多出条目。

### 5.3 播放器侧：何时发

`BaseEmitter` 增加方法：

```go
// 播放中每轮调用；距上一条不足 PositionSampleInterval 时不发。
func (b *BaseEmitter) EmitPositionSample(now time.Time, position, duration float32)
```

内部记上一条采样的时刻（只由轮询循环调用，单写者）。`position` 须为有限的非负数（接受式判定，NaN 不通过），否则不发、也不记时刻。时刻由调用方传入，applemusic 的会话测试用假时钟。

各播放器在轮询循环末尾、歌词行匹配处调用，条件一律为：**本曲的 `all_lyrics` 已发出，且本地判定为播放中（已向服务端报过 `playing`）**。不发的情形：

- 暂停中：位置已由 `playback_pause` 给定，`livePosition` 不外推。
- 未宣告或换歌尚未发出 `all_lyrics`：缓存里的时长与歌词还是上一首的。
- 加载中、位置源不可用（applemusic 的时长为 0 或位置结构丢失，wesing 读播放时间失败的那一轮）。

固定间隔、不看有没有歌词行在推：有歌词的歌在长间奏里同样没有采样，统一处理不需要各家再判断「此刻有没有行可推」。暂停恢复、换歌后的第一轮会紧跟着带位置的事件再发一条采样，位置相同，无害。

sodamusic 缓冲中状态仍为 playing、本地位置停住，照发：服务端仍按墙钟外推，超前量被限制在一个间隔以内，不能消除。

### 5.4 间隔取值

`player.PositionSampleInterval = 5 * time.Second`，包级常量，只在 `player/player.go` 定义一次。

- 依据：系统媒体传输控件的建议值与 RTCP 的最小间隔都是 5 秒（§3），二者的场景与本设计相近：消费方在两次上报之间外推，上报用于校准。
- 采样把偏差限制为一个间隔内累计的量。会话回放（§2.2 第二行）按「播放中每隔 N 秒取一次播放器一侧的位置」推算；5s 一行在实施后由同一回放实测（`TestSessionPositionSamplesBoundDrift`，段内 12 条采样，段末超前 0.50s），结果相同：

| 间隔 | 段内最大超前 |
|---|---|
| 不采样（现状） | 20.00s，随时长增长 |
| 10s | 3.50s |
| 5s | 1.90s |
| 2s | 0.90s |
| 1s | 0.50s |

- 回放的读数速率是正常的三分之二，属极端情形。按位置停更判暂停的三家，门槛（wesing、qqmusic 1s，applemusic 1.6s、开播或跳转后 8s 内 3s）以内的单次停顿本来就不产生事件，这一部分偏差与间隔无关，间隔只决定它保留多久。
- 采样期间受影响的是无歌词歌曲与长间奏里新连入的连接和 HTTP 快照，位置在那里只驱动进度条；有歌词行时每行都会重新采样。5 秒的残余偏差在此可接受，取更小的值换不来可见的差别。

### 5.5 各播放器接入点

六家全部接入，共用 `EmitPositionSample`。只接按位置停更判暂停的三家不够：kugou 与 sodamusic 同样静默重锚（§2.2）。cloudmusicv3 的本地时钟与服务端外推同式，采样对它现在没有校准作用；仍接入是为了让「每个播放器包都调用它」成为可以机检的规矩（§8），其时钟日后改动时不必再回头补。

| 播放器 | 调用点 | 传入的位置 | 调用条件 |
|---|---|---|---|
| wesing | `pollLyrics` 循环末尾 | `playTime` | `!paused` |
| qqmusic | `runSession` 歌词行匹配处 | `progressSec` | `lastName != "" && !isPaused` |
| applemusic | `session.tick` 末尾的 `emitPositionSample` | `clk.live(now)` | `announced && !loading && clk.playing`（位置结构丢失时 `dropTwin` 已把时钟置为不在走） |
| kugou | `runSession` 歌词行匹配处 | `interpSec` | `isPlaying` |
| sodamusic | `runSession` 歌词行匹配处 | `interpSec`（`livePos()`） | `isPlaying` |
| cloudmusicv3 | `runSession` 循环末尾 | `clock.GetCurrent()` | `isPlaying` 且本曲的 `all_lyrics` 已发出（`activeLyrics` 非空或 `isPureMusic` 为真） |

applemusic 全扫途中的 `scanTick` 不发：全扫 0.12 ~ 0.24s，远小于间隔。

传入的位置必须与该播放器对外事件里的 `position` 同一口径：整曲实时位置，单位秒，不含 offset。口径不一致时采样会把缓存位置整体平移一个 offset。逐家核对如下，六家的 offset 都只加在歌词行匹配用的时间上，不进任何事件的 `position`：

| 播放器 | `lyric_update` 的 `position` | `playback_pause` / `playback_resume` 的 `position` | `all_lyrics` 的 `position` | offset 加在 |
|---|---|---|---|---|
| wesing | `BuildLyricUpdate` 的 `playPos` 实参 `playTime` | `playTime` | `initialPlayTime`（同一地址的 `ReadPlayTime`） | `adjustedTime := playTime + offsetSec`，只喂 `FindCurrentLine` |
| qqmusic | `playPos` 实参 `progressSec` | `meta.ProgressMs / 1000`、`anchorProgressMs / 1000`（`progressSec` 的锚点值） | `meta.ProgressMs / 1000` | `matchTime := progressSec + offsetSec` |
| applemusic | `playPos` 实参 `interp`（`clk.live`） | `emitTime` 取 `clk.live` | `emitAllLyrics` 取 `clk.live` | `nextLine` 的实参 `interp + s.offsetSec` |
| kugou | `playPos` 实参 `interpSec` | `anchorProgressSec`、`progressSec`（`interpSec` 的锚点值） | `initPlay`（`anchorProgressSec`） | `matchTime := interpSec + offsetSec` |
| sodamusic | `playPos` 实参 `interpSec` | `anchorProgressSec`、`progressSec` | `initPlay`、`livePos()` | `matchTime := interpSec + offsetSec` |
| cloudmusicv3 | `playPos` 实参 `clock.GetCurrent()` | `clock.GetCurrent()` | `clock.GetCurrent()` | `currentTime := clock.GetCurrent() + offsetSec` |

### 5.6 事件量与开销

每个播放中的播放器每 5 秒一条，即 0.2 条/秒，与歌词行同一量级。一条采样经过 `Emit`、fan-in 与 `UpdatePlayerState` 在 `s.mu` 写锁内的四个字段赋值，路由评估与广播两步提前返回；不序列化、不进订阅通道、不打日志。`EventCh` 满时与其他事件一样被丢弃，下一条在 5 秒后。

## 6. 放弃的备选

**#69**

| 备选 | 不取的原因 |
|---|---|
| `UpdatePlayerState` 返回解除标志或整套事件，由 `router.Run` 传给 `NotifySubscribers` | 数据流显式，但要改两个函数签名并动 `router.Run` 的循环体；该循环是承重路径且没有测试 |
| 在 `UpdatePlayerState` 内持 `s.mu` 直接投递（与 `sweepIdleHide` 对称） | 触发事件的去重仍要把「已含在整套里」传给 `NotifySubscribers`；并打破 `UpdatePlayerState` 只写缓存的约定 |
| 任何对外事件都在解除时立即补发 | 终止类状态与 `lyric_idle` 先于 `clear_song_data` 到达，推出去的是即将被清掉的上一首（§4.1） |
| 六家把 `clear_song_data` 挪到状态事件之前，服务端不分事件 | 要动十几处发出点并加门禁防回退，新接入的播放器仍可能写反；wesing 轮询结束时先发的 `lyric_idle` 不在其列，仍会触发 |
| 整套取触发前的缓存，触发事件照发 | 触发事件是 `all_lyrics` 时先推上一首歌词 |
| 触发事件一律照发 | 整首歌词与带封面的歌曲信息紧接着各发两遍；根路径对相同内容是抑制的 |
| 触发事件的类型已在整套里就一律不单发 | `status_update` 也被吞掉，偏离根路径「状态从不抑制」与 #69 的原文 |
| 整套里去掉触发事件的类型，由触发事件补位 | 状态事件触发时顺序变成状态在最后，偏离 `buildInitEvents` 的顺序 |
| 只推给隐藏期间连入的连接 | 要逐连接记账；隐藏前在线的 SSE 连接同样需要恢复 |

**#70**

| 备选 | 不取的原因 |
|---|---|
| 维持现状，只在文档写明局限（#70 方向 2） | 已定取方向 1 |
| 广播采样，让在线前端一并校准 | 已定只校准缓存 |
| 复用 `playback_resume` 作采样 | 它是对外事件，下游据它重置歌词时钟；周期性地发会违反「seek 与恢复只发一次」的契约（AGENTS §3.2） |
| 只在本地时钟被校正超过阈值时发 | 六家的时钟各不相同，要逐家定义「校正」。可统一为「本地位置与按上一条采样外推的值相差超过阈值即发」，偏差上界更紧，但多出阈值与最短间隔两个常数，缓冲停顿时退化为按最短间隔发。固定间隔的残余偏差不够用时再换 |
| 仅在没有歌词行可推时发 | 各家要多维护一个「上一条带位置事件的时刻」，省下的只是每 5 秒一条内部事件 |
| 高频上报（每轮或每秒数次），服务端不再外推 | 事件量高两个数量级，且两次上报之间仍需外推 |
| 给 `player.Event` 加发出时刻，服务端以它为采样时刻 | 通道内的延迟在微秒量级 |

## 7. 文档与契约影响

| 文件 | #69 | #70 |
|---|---|---|
| `doc/openapi.yaml` | `/{player}/ws` 说明中「歌恢复后随新事件自然复原」改为解除时先补发整套；`/ws` 事件说明第 9 条与 `player_clear` 的 webhook 说明两处 per-player 段同步；`/{player}/lyric_update-SSE`、`/{player}/song_info-SSE` 补解除时重发缓存 | 不改（内部事件不入对外文档） |
| `doc/API_RESPONSE_EXAMPLES.md` | `player_clear` 一节的 per-player 说明补解除时的补发 | 不改 |
| `AGENTS.md` | §2.1 发送点计数与所列函数；§3.4「per-player 订阅者」条目补解除时的补发；§4 门禁表 | §3.1 补 `position_sample` 不参与路由评估；§3.2 增一条「播放中按间隔调 `EmitPositionSample`，位置与对外事件同口径」；§4 门禁表；§5.1 接入步骤 |
| `doc/core.md` | 不涉及 | 各播放器事件说明中列举发出事件的地方补一项（实施时核对） |
| 代码注释 | `PlayerState` 中「已在线的 WS/SSE 由恢复后的实时事件自然复原」一句 | `internalEvents`、`PositionAt` 的写入点列表、`PositionFromPause` |

两项都不新增 `capabilities` 项：`init_position_live` 的含义（外推到发送时刻）不变，采样只改善其精度；#69 见 §4.2。事件线格式、HTTP 响应结构、配置项均不变。

## 8. 测试

每条先写成失败的测试再实现；每个测试做一次变异自证并记入注释（AGENTS §9）。

**#69**（`server/idleresume_test.go`，按 `router.Run` 的顺序调 `UpdatePlayerState` 与 `NotifySubscribers`）：

1. 隐藏期间连入的 WS：`playback_resume` 到达时依次收到 `status_update`、`song_info_update`、`all_lyrics`、`lyric_update`、`playback_resume`；`lyric_update-SSE` 只收到缓存的当前行，`song_info-SSE` 只收到缓存的歌曲信息；根订阅者与另一个播放器的 per-player 订阅者零事件。
2. 触发事件自身：playing 状态在整套之后再发一次；`song_info_update`、`all_lyrics`、`lyric_update` 与整套里那条哈希相同时该类型恰好一条，`all_lyrics` 触发时整套不含上一首的 `lyric_update`；`playback_pause` 在整套之后恰好一条；触发事件的类型在缓存里没有条目时照发。
3. 终止序列不补发（WS 与两条 SSE 各断言）：缓存齐全、暂停至隐藏后连入，依次灌入 `status_update`(standby)、`clear_song_data`，再灌三组 `waiting_process` + `clear_song_data`，全程没有 `song_info_update`、`all_lyrics`、`lyric_update` 推出，四个 per-player HTTP 端点与之相符（状态为最新、其余为空）；随后新歌开播（playing 状态、`song_info_update`、`all_lyrics`），各连接拿到的歌曲信息与歌词是新歌的。
4. 同上，序列换成 applemusic 的 `waiting_song` + `clear_song_data`，以及 wesing 的 `lyric_idle` → `waiting_song` → `clear_song_data` → `lyric_idle`。
5. paused 状态与 `lyric_idle` 解除隐藏但不补发，其后的 `playback_resume` 才补发。
6. 根订阅者：该播放器是活跃播放器时只收到触发事件一条。
7. 未隐藏时普通事件只下发一条，不带整套。
8. 补发后的下一条事件不再带整套；再次隐藏、再次解除时再补发一次。
9. 隐藏态下 `clear_song_data` 与 `position_sample` 不解除、不补发。
10. 缓存只有状态、或完全为空：整套只含已有类型，没有 `data:{}` 的占位事件。
11. 整套里 `all_lyrics` 与 `lyric_update` 的 `position` / `progress` 相同，`playing` 时外推到发送时刻。
12. 隐藏前已在线的 WS 订阅者依次收到清除、整套、触发事件。
13. 订阅通道已满时 `NotifySubscribers` 立即返回。
14. 同一条 playing 状态事件既切根又触发补发：根收到 `player_switch` 与整套，per-player 收到整套与这条状态。

**#70 服务端**（`server/positionsample_test.go`）：

1. 采样写入位置、进度与采样时刻：其后的补发与 HTTP 快照从采样值起外推。
2. 回放：播放器一侧以正常速率的三分之二前进 60 秒，每 5 秒灌一条采样，每条采样到达前补发位置的超前量不超过界；不灌采样时超前约 20 秒。
3. 不广播：根订阅者（该播放器为活跃播放器）与 per-player 订阅者、两条 SSE 都收不到。
4. 不计活动：`LastEventAt` 不变；隐藏态下到达不解除隐藏；非播放状态下持续到达也不推迟隐藏。
5. 不影响路由：构造「无活跃播放器、该普通播放器在路由里为 playing」的状态，`updateRouting` 对采样返回 `false`、`activePlayer` 不变；对照的 `lyric_update` 与 `clear_song_data` 照旧触发切换。
6. `buildInitEvents` 给出的事件类型与 `internalEvents` 不相交（`switchSkip` 不比对内部事件的前提）。
7. `playback_pause` 之后的采样清除暂停标记，其后照常外推。
8. 状态不是 `playing` 时采样写入但不外推。
9. 采样不改动 `Status`、`SongInfo`、`AllLyrics`、`LyricUpdate`、`Duration`。
10. `clear_song_data` 之后的采样不使补发多出 `all_lyrics` 或 `lyric_update`。
11. 载荷为 nil 或类型不符：忽略，不 panic，缓存不变。

**#70 播放器侧**：

- `player/positionsample_test.go`：首次调用即发；间隔内不发，差 1 纳秒不发、恰好满间隔即发；载荷的进度由 `ClampProgress` 算出，时长为 0 时进度为 0；位置为 NaN、±Inf、负数时不发且不占用间隔；事件类型与载荷指针类型；`EventCh` 满时不阻塞。
- `player/positionsample_lint_test.go`：AST 门禁，凡调用 `NewBaseEmitter` 的包必须调用 `EmitPositionSample`；附门禁自测。
- `player/applemusic/sessionposition_test.go`：§2.2 的会话回放改为断言——卡顿段内按间隔出现 `position_sample`，位置等于当时的 `clk.live`（offset 设为 2s 以区分），由事件重建的服务端位置超前 `clk.live` 不超过界；暂停中、未宣告、加载中、位置结构丢失后不发；恢复后继续发且不早于 `playback_resume`。现有会话测试中按事件总数断言的两处（`TestSessionStartsWithNoSong`、`TestSessionNoEventsWhileLoading`）不受影响，后者同时守着「加载中不发」。

## 9. 需真机确认

离线测试守不住的部分：

- wesing、qqmusic、kugou、sodamusic、cloudmusicv3 五家的轮询循环没有离线夹具，调用点与调用条件只能读代码核对，门禁只查「有没有调用」。每家各验一次：播放中没有歌词行在推的一段（无歌词的歌；wesing 取长间奏），把进度往回拖约 1 秒、重复五次，其后 5 秒内 `GET /<player>/all_lyrics` 的 `position` 与播放器显示的时间相差在 2 秒以内；暂停后该值不再变化。cloudmusicv3 的拖动会被判为跳转并发 `playback_resume`，这一步对它只验证没有回归。
- applemusic：真机上短于暂停门槛的停顿无法按需制造，按上一条的拖动方式验（3 秒以内的跳变只重锚、不发事件）。
- #69：为某播放器设 `<player>-idle-hide: 15`，播放后暂停超过 15 秒，此时新开 `/<player>/ws` 的页面与 `/<player>/song_info-SSE`，再恢复播放：页面立即出现歌名、封面与整首歌词，SSE 立即给出歌曲信息。
