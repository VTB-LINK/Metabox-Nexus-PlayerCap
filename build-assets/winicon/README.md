# Windows 可执行文件图标

exe 的图标来自 Go 链接器自动链接的 `resource_windows_amd64.syso`（主包目录 / 仓库根）。
本目录是它的**唯一真源 + 生成器**。

## 为什么要生成器（别再手搓单尺寸 .ico）

Windows 的 `.ico` 是多分辨率容器，应内含覆盖各 DPI 的多档尺寸（本项目 16/20/24/32/40/48/64/96/128/256，
其中 20/40 专为 125%/250% 的标题栏小图标），每张按该分辨率
单独渲染。显示时系统直接取对应尺寸那张。

若 `.ico` 只塞一张大图（早期做法是一张 ~400px），任务栏（~32px）、资源管理器 / 第三方列表
（~16–24px）等小尺寸场景就得由 Windows 在**显示时**把大图现缩——它用的是劣质采样器，
细线条 logo 一缩边缘全是锯齿。electron 类应用清晰，正是因为 electron-builder 在**构建期**
把母版用高质量重采样缩成全套尺寸再打包。本生成器做的就是同一件事。

## 组成

- `masters/metabox5-sqr.png` —— 发版 / 默认图标母版（紫色立方体，2021×2021 正方形）。
- `masters/metabox10-sqr.png` —— 备用 / 开发变体母版（2021×2021）。
- `../../tools/winicon/` —— 纯 Go 生成器：读母版 → Lanczos3 缩全套 DPI 尺寸（10 档）→ unsharp 锐化 → 打多尺寸
  `.ico` → goversioninfo 编成带版本信息的 `.syso`。跨平台，Linux CI 也能产出 windows/amd64 资源。
  `-filter`（lanczos3/catmullrom）与 `-sharpen`（默认 0.6，0 关）可调；小图标发糊就是这两项没做够。

## 生成

发版：`release.yml` 在构建前自动跑，母版用 `metabox5-sqr.png`、版本取 tag：

```
GOOS= GOARCH= go run ./tools/winicon \
  -master build-assets/winicon/masters/metabox5-sqr.png \
  -version "<tag 版本>" -o resource_windows_amd64.syso
```

本地：仓库根已提交一份由母版 5 生成的 `resource_windows_amd64.syso`（version 0.0.0 占位，
供 `go build` 直接用）。改了 logo 或想重生成：

```
# 默认（紫色 5）
go run ./tools/winicon -master build-assets/winicon/masters/metabox5-sqr.png -version 0.0.0 -o resource_windows_amd64.syso
# 开发变体（10）
go run ./tools/winicon -master build-assets/winicon/masters/metabox10-sqr.png -version 0.0.0 -o resource_windows_amd64.syso
```

`-ico <path>` 可额外落盘中间多尺寸 `.ico` 供人工检视。

## 校验

生成后可解析 exe 的 PE 资源确认 `RT_GROUP_ICON` 声明了全部 10 档（含 40 覆盖 250% DPI 标题栏）
——这才是 Windows 实际能看到的尺寸集；只有一档就是退回锯齿老路，缺 DPI 中间档则高分屏发糊。
