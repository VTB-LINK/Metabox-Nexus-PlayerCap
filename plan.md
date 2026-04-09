# Issue #25 编辑器面板实现计划

## 概要

将 `lyric_display.html` 默认页面改为「编辑器模式」：左侧配置面板 + 右侧实时预览（iframe），无参数访问即进入编辑器。现有 `pure`/`one_line` 等参数行为不变。

---

## Phase 1: 编辑器骨架 + 模式切换

**目标：** 无参数打开 = 编辑器页面；加 `default` 参数 = 原始默认展示页

- 新增 URL 参数 `default`：加了 `default` 则走原来的歌词展示逻辑
- 无参数（也无 `pure`/`one_line`/`default`）时进入编辑器模式
- 编辑器布局：
  - 左侧面板（~350px）：配置区域
  - 右侧区域（flex 剩余空间）：iframe 实时预览（加载自身 + 当前参数）
- 编辑器页面自身样式独立，不影响 iframe 内的展示页

**交付：** 能看到左右分栏，iframe 能加载展示页

---

## Phase 2: 配置面板 — 基础参数

**目标：** 可视化调整传给 iframe 的 URL 参数

面板控件：
| 控件 | 类型 | 对应参数 |
|------|------|----------|
| 显示模式 | 下拉 (default / pure) | `default` / `pure` |
| 单行模式 | 开关 | `one_line` |
| 播放器 | 下拉 (跟随活跃 / wesing / cloudmusicv3) | `player` |
| 歌词颜色 | 颜色选择器 | `color` |
| 发光效果 | 开关 | `glow` |
| 发光颜色 | 颜色选择器（glow 开启时显示） | `glow_color` |
| 描边效果 | 开关 | `stroke` |
| 描边厚度 | 数字输入（支持小数） | `stroke_width` |
| 描边颜色 | 颜色选择器（stroke 开启时显示） | `stroke_color` |
| 预览背景色 | 颜色选择器（pure 模式时显示） | `bg` |

- 每次控件变化 → 重新拼接 URL → 更新 iframe.src
- 面板底部显示生成的 URL（只读）

**交付：** 调整参数后 iframe 实时反映效果

---

## Phase 3: 字体选择器

**目标：** 支持本地字体 + 在线字体

- 使用 `window.queryLocalFonts()` API（需 HTTPS 或 localhost）获取本地字体列表
  - CEF 低版本不支持时 fallback：隐藏本地字体列表，仅保留手动输入
- 字体来源切换：本地字体 / 手动输入（Google Fonts 或系统字体名）
- 本地字体列表：可搜索/筛选的下拉
- 选择后更新 `font` 参数 → iframe 刷新
- 字体预览：面板内小预览文字显示当前字体效果

**CEF 兼容策略：**
```
if ('queryLocalFonts' in window) → 显示本地字体列表
else → 隐藏本地字体区域，仅显示手动输入框
```

**交付：** 能选择本地/在线字体并预览

---

## Phase 4: 封面预览组件

**目标：** 独立的封面展示组件 + 配置

面板新增「封面设置」区域：
| 控件 | 类型 | 说明 |
|------|------|------|
| 封面形状 | 下拉 (方形 / 圆形) | `cover_shape` |
| 碟片样式 | 开关（圆形时可用） | `cover_disc` — 中心挖孔效果 |
| 旋转模式 | 下拉（不旋转 / 持续旋转 / 跟随播放状态） | `cover_rotate` |
| 旋转速度 | 滑块 (4~30s 一圈，默认 12s) | `cover_spin_speed` |
| 封面尺寸 | 滑块 (40~200px) | `cover_size` |

旋转动效要求：
- 启动旋转时缓入（ease-in），停止旋转时缓出（ease-out）至静止，而非瞬间停转
- 实现方式：不依赖 `animation-play-state`，改用 JS 追踪当前角度 + CSS `transition` 控制
  - 播放 → 暂停：读取当前旋转角度，移除 animation，设置 `transform: rotate(当前角度)` + `transition: transform 1s ease-out`
  - 暂停 → 播放：从当前角度恢复 animation，用 `animation-delay` 负值偏移起始角度 + `ease-in` 过渡

封面 URL 接口：
- 新增参数 `cover_only`：仅显示封面组件（供 OBS 单独作为浏览器源）
- 封面数据来自 WS song_info_update 事件的 cover / cover_base64
- 切换封面时淡入淡出过渡效果（crossfade）

**交付：** iframe 中能看到独立封面组件，编辑器能调整封面参数

---

## Phase 5: URL 复制 + 收尾

**目标：** 快速复制配置好的 URL

- 「复制歌词 URL」按钮 → 复制歌词展示页 URL（含所有参数）
- 「复制封面 URL」按钮 → 复制 `?cover_only&...` 的 URL
- 复制后显示短暂的 ✓ 提示
- 自动检测当前 host：
  - `file://` → 基于当前文件路径拼接 URL
  - `http(s)://` → 基于 `location.origin + location.pathname` 拼接 URL
  - 无需用户手动改地址，后续页面托管到服务器后自动适配
- 地址栏自动检测：如果是 `file://` 协议，提示用户字体接口可能受限
- URL 编码处理：中文字体名 → encodeURIComponent

**交付：** 用户可一键复制调好的 URL 给 OBS

---

## Phase 6: 样式打磨

- 编辑器面板深色主题（与歌词展示页风格统一）
- 面板区域可折叠分组（基础设置 / 字体 / 封面 / 导出）
- 响应式：窄屏时面板堆叠到上方
- iframe 边框圆角 + 阴影
- 面板滚动支持（内容超出时）

---

## 实现顺序

```
Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 → Phase 6
  骨架      参数面板    字体选择    封面组件    URL复制     打磨
```

每个 Phase 完成后可独立测试，不依赖后续 Phase。
