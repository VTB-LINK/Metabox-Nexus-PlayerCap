### 3.3.3

#### 变更 / 改进

- **QQ音乐版本适配**：新增22.71适配。
#### 破坏性变更

- **offset 改为延迟语义**：`offset` 与各 `<player>-offset` 现在是歌词延迟（毫秒），正值=延后，负值=提前，与此前相反。默认值与模板同时取反（全局 200 → -200），同一份歌词的显示时刻不变。
- **config.yml 自动换算**：升级后首次启动时，`config.yml` 里的 `offset` 与各 `<player>-offset` 自动取反，文件带上版本键 `config-version`，原文件留作同目录下的备份 `config.backup-v3-<时间>-<随机段>.yml`。
- **命令行参数不会被换算**：写在快捷方式或脚本里的 `-offset`、`-<player>-offset` 须自行取反（如 `-offset 300` 改为 `-offset -300`）。
- **开关类命令行参数按字面取值，多余的词报错退出**：`-audio-normalize false`、`-<player>-audio-normalize false` 这类写法现在按字面生效（此前被解析为开，且其后的参数全部被忽略）；命令行里多余的词（如漏了横线的 `offset -200`）现在报出该词、打印用法并退出（此前被静默忽略，其后的参数不生效）。
- **config.yml 可能被重建**：版本检查接口要求时，低于指定版本的 `config.yml` 会在启动时被换成当前版本的默认配置并重启，旧文件留作备份，控制台列出需要重新填写的设置。
- **`/service-status` 回显的 offset 变号**：`config`、`config_effective` 里 `offset` 与各 `<player>-offset` 由正变负，键名与类型不变。事件里的 `timestamp`、`play_time`、`position` 取值不变；客户端请直接用 `play_time`，不要拿回显的 offset 自行换算。
- **不支持降回 3.x**：3.x 会按旧语义读取已换算的 offset，歌词偏差为两倍 offset。

#### 已知限制

- Windows 专用。
