# config.yml 配置示例

程序首次启动时在 exe 同目录自动生成 `config.yml`，内容即下面这一份（`config-version` 的取值随程序版本而定）。
各键与同名的命令行参数一一对应，命令行参数的优先级更高，见 [README](README.md#命令行参数)。

```yaml
# Metabox-Nexus-PlayerCap 配置文件
# 优先级：命令行参数 > config.yml > 内置默认值

# 配置文件版本，由程序维护，请勿手动修改
config-version: "v4.0.0"

# HTTP/WebSocket/SSE 监听地址
addr: "0.0.0.0:8765"

# 歌词延迟（毫秒），正值=延后，负值=提前
offset: -200

# 轮询间隔（毫秒），范围 10~2000
poll: 30

# 优先播放器；按需取消注释以分配
prior-player:
- wesing
# - cloudmusicv3
# - qqmusic
# - kugou
# - sodamusic
# - applemusic

# 优先播放器暂停超过n秒，自动切换到最后一个普通播放器
prior-player-expire: 15

# 「指定播放器」通道（/<player>/ws 等）无活跃自动隐藏：静默超过 n 秒后向该通道推一次清屏，
# 隐藏停留的末行歌词。0 = 关闭（默认）。仅作用于 per-player 端点，不影响跟随活跃播放器的根路径
# （根路径的自动隐藏另由 prior-player-expire 控制）。每个播放器可用 <name>-idle-hide 单独覆盖：
# 不写=跟随此全局值，0=该播放器关闭，n=该播放器用 n 秒（示例见下方 cloudmusicv3 段）。
per-player-idle-hide: 0

# 音频波形音量归一化：只作用于音频波形帧（/audio-ws 与 /<player>/audio-ws）的 RMS、左右声道峰值与频谱，
# 使各播放器的波形幅度趋于一致，不改变实际播放音量。每个播放器可用 <name>-audio-normalize 单独覆盖
# （true/false），不写=跟随此全局开关。
audio-normalize: true
# 目标响度（dBFS），范围 [-60,0]，越界自动钳到边界
audio-normalize-target: -14
# 最大增益（dB），范围 [0,45]，越界自动钳到边界
audio-normalize-max-gain: 30

# 音频波形帧率（帧每秒）：有音频数据时 /audio-ws 与 /<player>/audio-ws 每秒推送的帧数，
# 范围 [15,60]，越界自动钳到边界
audio-frame-rate: 25

# 全民K歌 配置
# wesing-offset: 0
# wesing-poll: 30
# wesing-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# wesing-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 网易云音乐 v3 配置
cloudmusicv3-offset: -500
# cloudmusicv3-poll: 100   # 低于 50 会被网易云自身的下限抬到 100，写 30 也是跑 100
# cloudmusicv3-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# cloudmusicv3-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize
cloudmusicv3-effect-strategy: fadeout # 特效最小化策略：park 自动屏外渲染保活 / fadeout 自动淡出

# QQ 音乐 配置
qqmusic-offset: -400
# qqmusic-poll: 50
# qqmusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# qqmusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 酷狗音乐 配置
kugou-offset: -430
# kugou-poll: 30
# kugou-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# kugou-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 汽水音乐 配置
sodamusic-offset: -340
# sodamusic-poll: 200     # 低于 200 会被静默抬到 200（每次 Extract 是主进程→渲染器桥 + transport 往返，较重）
# sodamusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# sodamusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# Apple Music 配置（纯内存扫描接入，与自身同用户的 AppleMusic.exe 普通权限即可读，无需管理员）
applemusic-offset: -400
# applemusic-poll: 50     # 低于 50 会被静默抬到 50（每轮只读 24 字节；位置的跳变与歌词行都在这一轮里发现、推出，调慢歌词跟着晚）
# applemusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# applemusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize
```

## 版本与自动迁移

- `config-version` 由程序维护，取值跟随程序版本；没有这个键的文件按 v3 对待。
- 4.0 起 offset 是延迟量，方向与之前相反。升级后首次启动时，`config.yml` 里的 `offset` 与各 `<player>-offset` 自动取反，
  原文件留作同目录下的备份 `config.backup-<版本>-<时间>-<随机段>.yml`。
- 写在快捷方式或脚本里的命令行参数不会被换算，须自行取反。
- 文件只读、不是 UTF-8 编码或写法特殊而无法改写时，文件保持原样，每次启动在内存里换算并告警。
- 迁移只换算文件里已有的键，不补新版模板新增的键。

## 重建

- 发布版本启动时会按服务端下发的版本门槛判断：配置版本低于门槛时不询问，旧文件留作备份，写入当前版本的默认配置后重启，
  控制台列出备份的文件名与需要重新填写的设置。
- 手动重建：删除 `config.yml` 后重启。
- 改写中途被打断时，目录里可能留下 `config.yml.*.tmp` 或空的备份文件，可在程序未运行时手动删除；
  此时若 `config.yml` 不在了，程序会按模板重新生成并提示备份的文件名，原有设置在备份里。
- 4.0 的配置不适用于 3.x，不支持降回 3.x。
