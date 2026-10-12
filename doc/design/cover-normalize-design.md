# 封面统一为 800×800 的设计

本文覆盖里程碑 4.0 的 issue #101：各播放器发出的 `cover_base64` 统一为 800×800。

状态：设计已过 Codex 前置审查第 1、2 轮并按其意见修订（2026-10-12）；§13 记录各点的决定，其中第 1 条（非正方形）按需求原文「拉伸」做，裁切是向用户提的建议、未获采纳前不做。

## 1. 需求与范围

需求原文（issue #101，标题「apple music & spotify: base64封面是1200x1200；spotify是640」）：

> 所有都应统一成800x800。我们编码base64的时候应该吐出正确的。
> 我们可能要抽象化一个通用的封面处理，挂在封面获取最后面，如果原始就是800x800则直接bypass，如果不是则我们拉伸到800x800然后才能吐出base64.

顺序约束（用户补充）：先合入 Spotify PR #100，#101 与它不冲突就同步做、先后合并。本设计不改 `player/spotify/*`；Spotify 的 `cover_base64` 经共享的 `player.FetchCoverBase64` 生成，共享层改完即覆盖。

**目标**：
- 所有生成 `cover_base64` 的路径经同一个函数：已是 800×800 的图原字节发出；其余 JPEG / PNG 拉伸到 800×800、编码为 JPEG 后发出。
- 处理失败一律退回今天的行为（原字节原样发出），不吞 `song_info_update`，不阻塞对外事件。
- 由门禁保证日后新增的播放器不绕开这个函数。

**非目标**：
- 不改 `cover` 字段（URL）的含义，不保证 URL 指向的图是 800×800。
- 不做色彩管理、EXIF 方向校正（§11）。
- 不新增配置项与 `capabilities` 项。

## 2. 现状

### 2.1 生成 cover_base64 的入口

`player/` 下的非测试代码里拼 `data:…;base64,` 的只有两处（`git grep ';base64,'`，2026-10-12；`tools/cdpexplore` 另有一处是解码用的前缀）：

| 入口 | 位置 | 调用方 |
|---|---|---|
| `player.FetchCoverBase64` | `player/cover.go:27-94`：下载 ≤5MB（`:67-75`），MIME 取 URL 后缀 `.png` 或 `Content-Type`（`:83-89`），原样 base64（`:91-93`） | wesing（`wesing.go:277`）、cloudmusicv3（`cloudmusic.go:410`）、qqmusic（`qqmusic.go:292`、`:370`）、kugou（`kugou.go:137`，经 seam `kugou.go:67`）、sodamusic（`sodamusic.go:579`）；Spotify 分支 `fix/4.0-spotify-review:player/spotify/spotify.go:778` |
| applemusic `session.setCover` | `player/applemusic/applemusic.go:1147-1151`：内存里的图片字节直接拼 data URI（`:1149`） | `announce`（`:1158-1161`）与 `applyCover`（`:1135`） |

`FetchCoverBase64` 的全部调用点都在已挂 `telemetry.Guard` 的封面 goroutine 里（`wesing.go:259-260`、`cloudmusic.go:408-409`、`qqmusic.go:286-297` 与 `:364-375`、`kugou.go:83-84`、`sodamusic.go:563-564`）。applemusic 的 `setCover` 在轮询 goroutine 里同步执行：`runSession`（`:259`）→ `tick`（`:462`）→ `maybeScan`（`:471`）→ 全扫 `Scan`（`:745`）→ `applySnapshot`（`:775`）→ `announce` / `applyCover`（`:867`、`:869`）。全扫期间由 `scanTick`（`:500-503`）每约 20ms 读位置、推歌词行；全扫返回后的 `applySnapshot`（含这里的封面处理）期间不推行，处理完成后由同一轮 `tick` 末尾的 `emitLyricLine`（`:473`）按当前时钟补推。

### 2.2 各家来源与尺寸

录音 `Repos/VTB-LIVE/playercap-recordings` 里共 1206 处带 URL 的 `cover`，全部是 800 档：网易云 1114 处 `param=800y800`、QQ 28 处 `R800x800`、wesing 21 处 `mid_album_800`、酷狗 19 处 `/800/`、汽水 24 处 `crop-center:800:800`（2026-10-12 逐条正则统计）。录音里未裁剪的 `cover_base64` 共 11 张，解码读 SOF / IHDR 全部是 800×800：网易云 9 张（7 张 JPEG，2 张 PNG 1.32MB、1.41MB，都标 `image/jpg`），汽水 2 张 JPEG。

| 播放器 | 来源 | 尺寸怎么定 | 尺寸 | 依据 |
|---|---|---|---|---|
| wesing | 内存 AOB 扫出的 imgcache URL（`lyric/songinfo.go:113-188` `FindCoverURL`） | `/mid_album_500/` 换成 `/mid_album_800/`（`wesing.go:267`、`:464`） | 800×800 JPEG | 录音；`FindCoverURL` 返回非 500 档时替换不生效，尺寸未知 |
| cloudmusicv3 | 三条路径：DOM 底栏图（`cdp/client.go:185-193`，改写成 `thumbnail=300y300`）；Redux `Album.PicUrl`（`cloudmusic.go:316-318`、`:386-388`，无尺寸参数）；`FetchCoverViaCDP`（`cdp/client.go:553-578`，无 `param=` 时补 `param=800y800`，`:561-562`），三者依次覆盖 | 只有第三条请求 800 | 常见路径 800×800，JPEG 或 PNG | 录音全部来自第三条；前两条只在 `activeSongID` 为空或 CDP 取不到时生效（`cloudmusic.go:389-393`），出现频率未知 |
| qqmusic | `buildCoverURL`（`api.go:902-904`），调用处 size 恒为 800 | URL 里的 `R800x800` | 800×800 JPEG | 录音；歌手图兜底（T001）未取样 |
| kugou | `getPlayInfo` 的 `cover`；兜底 `fetchCoverFromAPI` 把 `{size}` 换成 800（`kugou.go:763`） | 换歌时 `/stdmusic/120/` 换成 `/stdmusic/800/`（`kugou.go:408`） | 800×800 JPEG | 录音。**封面晚到分支 `kugou.go:518-526` 不做这个替换**，读代码推断 VIP 首播封面晚到时取到 120 档（未真机复现） |
| sodamusic | 注入脚本拼 `crop-center:800:800.jpg`（`cdp/client.go:317-319`）；`cover_url` 是字符串时原样用 | URL 模板 | 800×800 JPEG | 录音；字符串分支未见样本 |
| applemusic | SMTC 属性存储的缩略图流，只收 JPEG / PNG、≤5MB（`mem/propstore.go:75-76`、`:183-195`） | 无 URL、无参数 | 未知 | issue 标题称 1200×1200；仓库与录音里没有样本 |
| spotify（#100） | 页面 `[data-testid="cover-art-image"]` 的 `currentSrc \|\| src`（`fix/4.0-spotify-review:player/spotify/cdp/client.go:499-500`） | 页面渲染尺寸与 DPR 决定取到 srcset 的哪一档，代码不改写 | **300×300 JPEG**（真机实测 5 首：URL 为 `ab67616d00001e02…`，17～35KB） | issue 标题称 640，实测不是；调研所得 i.scdn.co 的档位为 64 / 300 / 640 / 原图，没有 800 档（只在一张图上实测，未复核） |

### 2.3 已知偏差

- applemusic 与 spotify 不是 800，是 #101 的直接起因（尺寸均待真机确认，§12）。
- 网易云 PNG 封面标 `image/jpg`：`FetchCoverBase64` 原样采用 `Content-Type`（`cover.go:87-88`），CDN 回的就是 `image/jpg`（录音中 9 张全是）。
- 网易云 Redux 路径取的是无参数原图，调研抽样里有 2048×2048、6,169,855 字节的 PNG，超过 5MB 上限（`cover.go:68`）时 `cover_base64` 为空（调研所得，未复核）。
- 酷狗封面晚到分支取到 120 档（推断，同上）。

### 2.4 消费方

- `server/dedup.go:16-25`：`song_info` 的哈希含 `cover_base64` 全文。处理是确定性的（同输入同输出，§6.4），去重行为不变。
- `lyric_page.html`：封面框 80×80 圆形（`:105-111`），`<img>` 为 `object-fit: cover`（`:132`、`:141`），即非正方形图在显示时被居中裁切；用 `cover` URL 判断是否同一首歌（`:3043-3047`），只接受 `data:image/` 与 http(s) 前缀（`:2656-2659`）。
- `doc/openapi.yaml:2547-2553`：`cover_base64` 的描述与示例不承诺尺寸与格式。

## 3. 参考

| 系统 | 做法 | 出处 |
|---|---|---|
| Navidrome | 请求尺寸大于原图时压到原图尺寸，注释大意「放大浪费资源，也不增加信息」；用 `xdraw.CatmullRom.Scale`；`square=true` 时等比缩进正方形画布、透明留边并输出 PNG | [core/artwork/resize.go@2dc0983](https://github.com/navidrome/navidrome/blob/2dc09836291a1eadaced27cbeb938d875635d9ef/core/artwork/resize.go)（2026-10-12 复核） |
| Jellyfin | `GetNewImageSize` 依次 `Resize`、`ResizeFill`、`ScaleDownToFit`，注释原文 "Never encode larger than the source." | [ImageHelper.cs@82ecfde](https://github.com/jellyfin/jellyfin/blob/82ecfde2f7bdd9d91ec403749c124ef96f64cea3/MediaBrowser.Controller/Drawing/ImageHelper.cs)（2026-10-12 复核） |
| Go `image` 包 | 处理任意图片应先 `DecodeConfig`，按头部尺寸决定能否安全 `Decode`；`Decode` 产出超大图像不算安全问题 | 本机 `GOROOT/src/image/image.go:25-37`（go1.26.1） |
| x/image/draw | `ApproxBiLinear` 缩小时只混 4 个最近像素、耗时与源像素数无关；`BiLinear` 慢但质量高；`CatmullRom` 很慢、质量很高；核插值器临时缓冲为 目标宽×源高 个 `[4]float64` | 本机模块缓存 `golang.org/x/image@v0.44.0/draw/scale.go:155-188`、`:204-206` |
| imgproxy / sharp / Cloudflare Images | 解码前按像素数设上限（默认 50MP / 约 268MP / 100MP）；处理失败退回原图或回退图 | 调研所得（[imgproxy 配置](https://docs.imgproxy.net/configuration/options)、[sharp](https://sharp.pixelplumbing.com/api-constructor)、[Cloudflare 限制](https://developers.cloudflare.com/images/get-started/limits/)），未复核 |

两点结论：同类系统普遍不放大、保持宽高比，本设计按需求放大并拉伸，属有意偏离，后果见 §11；「先读头限尺寸、失败退回原图」是通行做法，本设计照用。

## 4. 备选方案

| 方案 | 内容 | 取舍 |
|---|---|---|
| A 共享层统一处理 | 下载或取到字节之后、编 base64 之前，经同一个函数规范为 800×800 | 覆盖全部七家（含 spotify），与需求原文「挂在封面获取最后面」一致；代价是小图放大发糊、每次非 800 的封面多 20～130ms CPU（§6） |
| B 只在源头请求 800 档 | 各家 URL 改成 800 档，不做本地处理 | applemusic 没有 URL，做不到；spotify 没有 800 档；网易云对小于 800 的原图不放大（调研所得）、QQ 的档位外返回 404（调研所得）。单独做达不到「统一成 800」 |
| C = A + 有选择的源头修正 | A 之外，把已知会取到小图或超大原图的源头改为请求 800 档 | 源头请求到 800 时 A 直接放行，省去放大造成的模糊与重编码；只在有现成 800 档、改动只有几行的地方做 |

**决定：A 为主体；C 的源头修正逐项列出，不并入 #101、另建 issue（§13 第 2、3 条）。**

C 的逐家评估：

| 播放器 | 源头修正 | 是否值得 |
|---|---|---|
| kugou | `kugou.go:518-519` 晚到的封面 URL 同样做 `/stdmusic/120/` → `/stdmusic/800/`，再与 `currentCover` 比较 | 值得：一行；否则 VIP 首播晚到的封面是 120 档，A 会把它放大 6.7 倍。按读代码推断，同时消除换歌后第二拍把 `currentCover` 改回 120 档、打一条「Cover URL updated」日志的现象（未真机复现） |
| cloudmusicv3 | 换歌分支定下 `songCover` 后（`cloudmusic.go:393` 之后）统一处理：`*.music.126.net` 且无 `param=` 的 URL，去掉 `thumbnail=`、补 `param=800y800` | 值得：一个十行以内的函数。Redux 无参原图可能超过 5MB 导致没有 `cover_base64`；DOM 的 300 档会被放大。两条路径频率未知 |
| spotify | 把 `currentSrc` 的尺寸前缀改为原图档或 640 档 | 改动在 `player/spotify/*`，不在 #101 内做；#100 合入后另建 issue，先真机确认 `currentSrc` 取到哪一档（§13 第 3 条） |
| wesing、qqmusic、sodamusic | 已请求 800 | 不改 |
| applemusic | 无 URL | 不适用 |

## 5. 设计

### 5.1 接口

在 `player/cover.go` 增加：

```go
// CoverDataURI 把封面图片规范为 800×800 后编成 data URI。b 为空时返回空串。
// 已是 800×800 的 JPEG / PNG 原字节发出；其余 JPEG / PNG 拉伸到 800×800、编码为 JPEG。
// 读不出头、格式不在白名单、尺寸超限、解码或编码失败时原字节、原 mime 发出，并打一条警告。
func CoverDataURI(playerName string, b []byte, mime string) string

// normalizeCover 是 CoverDataURI 的纯函数部分，不打日志，供测试直接断言。
func normalizeCover(b []byte, mime string) (out []byte, outMIME string, w, h int, err error)
```

- 导出 `CoverDataURI`：applemusic 在另一个包里调用。`normalizeCover` 不导出。
- 放在 `player/cover.go`：与 `FetchCoverBase64` 同文件、共用 `coverLog`（`agent-doc/06-日志与文案.md:106` 的模块名表不变）。
- 接入点只有两处：`FetchCoverBase64` 末尾（`cover.go:91-93`）改为 `return CoverDataURI(playerName, body, mimeType)`；applemusic `setCover`（`applemusic.go:1149`）改为 `s.song.coverURI = player.CoverDataURI("AppleMusic", np.Cover, np.CoverMIME)`。其余六家（含 spotify）不改。
- `coverHash`、`staleCover` 仍按原始字节算（`applemusic.go:1131-1132`、`:1144`、`:1159`），换图判定不受影响。
- 为了能测到 `FetchCoverBase64` 的成功路径（现有的拨号检查会拒绝 httptest 的回环地址），把 HTTP 客户端的构造抽成包级变量 `newCoverClient func(timeout time.Duration) *http.Client`，生产值就是现在 `cover.go:38-56` 的那段（SSRF 校验原样保留），测试里换成直连 httptest 服务器的客户端。该校验今天没有任何测试覆盖（全仓 `*_test.go` 里没有 `isBlockedCoverIP` 的用例；酷狗的封面用例连的是无服务的 `127.0.0.1:1`，删掉拨号校验它照样通过），所以这次抽取要把它补上（§9 第 22 条），免得抽取时丢了校验而无人知道。

### 5.2 算法

依次：

1. `len(b) == 0`：返回空串（今天会得到 `data:image/jpeg;base64,` 这样的空 data URI）。
2. `image.DecodeConfig`。失败 → 原样。
3. 格式名不是 `jpeg` 或 `png` → 原样。生产代码只注册这两种（只 import `image/jpeg`、`image/png`，不 import gif、webp、tiff、bmp），这一条防将来有依赖注册了别的格式。
4. 宽高都是 800 → **放行**：原字节，MIME 按格式名改写为 `image/jpeg` 或 `image/png`，不解码、不打日志。只改标签不改字节：网易云标 `image/jpg` 的 PNG 由此得到正确的 `image/png`。
5. 宽或高不在 1～1600 → 原样（像素上限，§6.2）。
5a. JPEG 且 `DecodeConfig` 报的颜色模型是 `color.CMYKModel`（4 分量，含 Adobe YCCK）→ 原样，**不解码**。Go 对 4 分量 JPEG 一律解成 `*image.CMYK`、按固定公式处理（`GOROOT/src/image/jpeg/reader.go:675` 起的 `applyBlack`），不做色彩管理，重编码后颜色偏（调研用标准库测试图对比，PSNR 15.27dB，未复核），原样交给浏览器显示更好；而渐进式 CMYK 在解码期间同时持有四组系数数组、图像与黑色平面、CMYK 输出（约 100MB，Codex 设计审查推演），在解码前拦下才不必为它付内存。
5b. **JPEG 预检扫描次数**：按字节扫描 SOS 标记（`FF DA`），个数超过 `maxJPEGScans`（16，最终取值按 §5.3 的实测校正）→ 原样，打警告。理由见 §5.3「渐进式 JPEG 炸弹」。只对 `jpeg` 做；PNG 的解码工作量由 IHDR 的尺寸界定。
6. `image.Decode`。失败 → 原样（截断、损坏、4 分量但没有 Adobe APP14 的 JPEG）。
7. 解出的图像类型再兜底一次：`*image.CMYK` → 原样（正常情况下已在 5a 拦下）。
8. `dst := image.NewRGBA(800×800)`，`xdraw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)`。源矩形取整幅图，非正方形被拉伸（需求原文；§13 第 1 条）。
9. `jpeg.Encode(dst, Quality: 90)`，MIME 为 `image/jpeg`。失败 → 原样。

**插值器选 `BiLinear`**（本机实测见 §6.1）：
- 比 `CatmullRom` 快约 20～30%（1200 源 76ms 对 97ms，1080 源 50ms 对 74ms），放大与缩小的质量差别在调研的对照里不可测。
- 比 `ApproxBiLinear` 慢约 20～25ms，但后者缩小时只取 4 个最近像素，1.5 倍缩小（1200→800）就有混叠：调研用周期 2.5px 的条纹测输出的灰度标准差，ApproxBiLinear 40.4、BiLinear 16.8（越小越好，调研所得，未复核）。放大时两者等价。
- 代价是核插值器的临时缓冲，1200 源约 30MB（§6.2）。

**输出一律 JPEG，质量 90**：
- 缩放本身已重采样，编码为 PNG 没有保真上的收益。800×800 的不透明照片：PNG 默认压缩 759KB / 107ms，JPEG q90 147KB / 21ms（本机实测，同一张图）。
- 不按「有无透明」分流：调研下载的网易云 1414×1414 PNG 原图有 1414 个非不透明像素（相当于一行），按透明分流会输出 1.25MB、246ms 的 PNG。透明像素按黑底合成：`jpeg.Encode` 对 `*image.RGBA` 直接读预乘后的 `Pix`、忽略 alpha（`GOROOT/src/image/jpeg/writer.go:420-436`）。
- 质量 90 的 800×800 输出为 71～204KB（本机 12 张样本，§6.1），base64 后约 95～272KB。Go 的 JPEG 编码器只写 4:2:0 baseline（`writer.go:314-332`、`:573`），同一张 Go 产出的图再以同一质量重编码几乎无损（调研所得）。

**原样退回时打一条警告**：`coverLog.Warn("[%s] cover not normalized, sent as is: %v", playerName, err)`。缩放成功打一条 `coverLog.Detail("[%s] cover resized %dx%d → 800x800 (%d → %d bytes, %v)", …)`，带耗时，供真机评估 applemusic 的阻塞（§12）。英文原文与中文逐字补进 `i18n/messages_p4_log.go` 的「封面与音译」段（`:296` 起）；`normalizeCover` 返回的错误用 `i18n.Errorf`（`agent-doc/06-日志与文案.md:154-155`）。

### 5.3 已知的坑与处理

| 情形 | 处理 |
|---|---|
| 解码炸弹：把 1×1 PNG 的 IHDR 改为 30000×30000 并重算 CRC，共 71 字节 | 第 5 步拦下，不解码。本机实测 `DecodeConfig` 立即返回该尺寸、无错误 |
| 渐进式 JPEG 炸弹：尺寸与字节数都在上限内，但文件里重复放上万次只带几字节熵数据的 AC 扫描（Codex 设计审查的构造：约 4.1MB 的 2048×2048 灰度 SOF2，重复 20 万次扫描，约 131 亿次块处理）。Go 的 `image/jpeg` 对每个 SOS 都调用 `processSOS`，没有扫描次数上限（`GOROOT/src/image/jpeg/reader.go:620-624` 附近；2026-10-12 在 go1.26.1 上核对，未发现任何相关限制），尺寸与字节上限都拦不住 | 第 5b 步：解码前数 SOS 标记，超过 `maxJPEGScans`（16）直接原样；正常渐进式 JPEG 通常 10 个扫描上下（libjpeg、mozjpeg 的默认脚本都约 10 个），个数是保守的上界（EXIF 缩略图里的 SOS 也会被数进去，只会多数、不会漏数）。**放行后的最坏工作量不能只按熵位数算**：AC refinement 扫描对零系数的遍历不消耗熵位（`scan.go` 的 `refineNonZeroes`），Codex 推演 2048² 灰度、1 个 DC 扫描加 63 个 `Ss=1,Se=63,Ah=1,Al=0` 的 refinement 扫描（共 64 个 SOS，文件不足 10KB）会做约 65536×63×63≈2.6 亿次零系数检查；16 个扫描的上限把它压到约 1/4，仍是推演、没有实测。故 `maxJPEGScans` 的最终取值以 §9 第 18 条(b)(c)(d)的实测为准（只测灰度 refinement 一种形态不够：重复的基线扫描每次都要重做反量化与 IDCT，成本在另一处）：最坏耗时须在本机 ≤100ms，不满足就继续调低。applemusic 在轮询 goroutine 里同步处理，所以这条是必须的 |
| CMYK / YCCK JPEG（含渐进式） | 第 5a 步原样、不解码（`DecodeConfig` 的颜色模型为 CMYK）；4 分量无 APP14 的同样报 CMYK 模型，同样在 5a 拦下 |
| 800×800 但数据截断 | 第 4 步放行、不解码，与今天相同（`FetchCoverBase64` 已按 `Content-Length` 拒收不完整下载，`cover.go:76-81`） |
| GIF、WebP 等 | 第 2 或第 3 步原样。现有来源没有这两种格式（§2.2；applemusic 只收 JPEG / PNG） |
| EXIF 方向 | 不处理（§11）：Go 的 JPEG 解码器忽略 APP1（`reader.go:635-637`），重编码后方向标签丢失 |
| ICC 配置文件 | 不处理（§11）：解码时忽略，重编码后丢失，广色域图会偏灰 |

### 5.4 门禁

规矩：**`cover_base64` 一律经 `player.CoverDataURI` 生成。** 按 `agent-doc/03-硬规则.md:12`「新增规矩优先做成门禁」，新增 `player/coverdatauri_lint_test.go`：AST 扫 `player/` 下的非测试 `.go` 文件，字符串字面量含 `;base64,` 的只允许出现在 `player/cover.go`。只看字面量、不看注释（`player/wesing/lyric/songinfo.go:17` 的注释含该串）。附门禁自测：一段含 applemusic 旧写法的源码必被拦。变异验证：把 `applemusic.go:1149` 改回旧写法即红。登记进 `agent-doc/04-守卫状况.md` 的「已有自动门禁」表，规矩写进 `agent-doc/03-硬规则.md` §3.2。

盲区：用 `fmt.Sprintf` 之外的方式拼出不含该字面量的 data URI（如分段拼 `";base"+"64,"`）查不到。

## 6. 性能与安全预算

### 6.1 耗时

本机实测（i9-11900KF，go1.26.1 windows/amd64，五次取最快；源为调研下载的 CDN 样本与由其合成的 q92 JPEG；整条流水线单 goroutine）：

| 源 | 读头 | 解码 | 缩放 BiLinear | 编码 q90 | 合计 | 累计分配 | 输出 |
|---|---|---|---|---|---|---|---|
| 800² JPEG，174KB | 2.8µs | — | — | — | 放行 | 约 0 | 原字节 |
| 1080² JPEG，79KB | 0.0ms | 8.1ms | 30.5ms | 11.7ms | 50.3ms | 31.7MB | 85KB |
| 1200² JPEG（合成），532KB | 0.0ms | 22.7ms | 36.5ms | 17.1ms | 76.3ms | 35.3MB | 167KB |
| 1414² PNG，3.68MB | 0.0ms | 64.6ms | 45.5ms | 15.5ms | 125.6ms | 46.9MB | 171KB |
| 640² JPEG（合成），134KB | 0.0ms | 4.9ms | 16.4ms | 14.7ms | 36.1ms | 19.8MB | 146KB |
| 500² JPEG，82KB | 0.0ms | 5.0ms | 14.0ms | 13.0ms | 31.9ms | 16.2MB | 111KB |
| 120² JPEG，8KB | 0.0ms | 1.0ms | 7.0ms | 11.0ms | 19.0ms | 6.0MB | 71KB |

对照：1200 源 ApproxBiLinear 53.1ms / 5.2MB，CatmullRom 96.6ms / 35.4MB；1080 源分别为 38.2ms、73.6ms。主播机 CPU 可能更慢，绝对值仅供参考；实现时把同一套合成样本写成 `BenchmarkNormalizeCover` 入库，便于复测。

### 6.2 内存与像素上限

- 峰值主要是 `BiLinear` 的临时缓冲：目标宽 × 源高 × 32 字节（`scale.go:204-206`），1200 源 30.7MB。另有解码出的源图与 800² 的 RGBA 2.56MB；源图按解码出的类型算：JPEG 4:2:0 约 1.5 字节/像素，8 位 PNG 的 `NRGBA` 4 字节/像素，**16 位 PNG 的 `NRGBA64` 8 字节/像素**（`image/png` 的 `DecodeConfig` 接受 16 位，`Decode` 返回 64 位类型）。
- 上限取单边 ≤ 1600（已知来源都在内：applemusic 据称 1200，网易云 1414 的 PNG，酷狗、QQ、wesing、汽水均 ≤ 800，Spotify 640 档；超过的退回原样、与今天相同）。按 1600×1600 逐类型算活跃峰值（源图 + `BiLinear` 临时缓冲 + 目标图 2.56MB，临时缓冲 = 目标宽 × 源高 × 32 字节 = 40.96MB）：基线 JPEG 4:2:0 约 47MB，8 位 PNG（`NRGBA` 4 字节/像素，10.2MB）约 54MB，16 位 RGBA PNG（`NRGBA64` 8 字节/像素，20.5MB）约 64MB（Adam7 另持有 pass 图，同量级），**渐进式 YCbCr 4:4:4 JPEG** 约 80MB（解码期间的系数数组每分量 40000 块 × 256 字节 = 10.2MB，三分量 30.7MB，加图像平面 7.7MB；这些数组在 `Decode` 返回后即不可达，但 GC 未必已回收，按与缩放缓冲同时存在算）；再加输入字节（≤5MB）与行缓冲，最坏约 85MB，用完即为垃圾，不常驻。CMYK 在 5a 就拦下，不解码。不为这些形态再收紧上限；§9 补 16 位 PNG、Adam7、渐进式 4:4:4 的夹具，断言能正常缩放，累计分配量记入基准。
- 超过 1600 的退回原样、不是 800：网易云无参原图（调研见到 2048、4000）属此类，这是 §4 网易云源头修正（#109）的另一个理由。
- 字节上限不变：下载仍 ≤5MB（`cover.go:68`），applemusic 仍 ≤5MB（`propstore.go:75-76`）；缩放后的输出远小于此。
- 不注册 tiff、bmp：x/image/tiff 历史上有 `DecodeConfig` 即耗尽内存的漏洞（GO-2023-1572，调研所得）；只注册 jpeg、png 也缩小了攻击面。

### 6.3 在哪个 goroutine 里做

- 五家与 spotify：在已有的封面 goroutine 里，紧接下载之后，不新开 goroutine，`telemetry.Guard` 门禁不受影响。kugou 的 `coverTotalBudget = 800ms`（`kugou.go:50-57`）只约束下载超时；常见路径是 800 档、放行耗时微秒级；晚到的 120 档若不做 §4 的源头修正，放大约 19ms。
- applemusic：在轮询 goroutine 里同步做，**每次换封面阻塞一轮约 50～80ms（本机按 1080～1200 源估计）**。这一拍正跟在 0.12～0.24s 的全扫之后（`applemusic.go:18`、`doc/core.md:147`），全扫期间 `scanTick` 推行，缩放期间不推行，处理完后当轮 `tick` 末尾的 `emitLyricLine` 按当前时钟补推：落在缩放期间的歌词行晚到最多同样长；时钟按墙钟走（`clk.live`），位置不受影响。每首歌通常一到两次（宣告一次，图晚到或换图再一次）。不做异步的理由见 §11；真机耗时超过 100ms 时再按 §11 的异步方案改。
- `player/` 下不 panic（`agent-doc/03-硬规则.md:111`）：本设计的代码不调用 `panic`，所有错误走返回值退回原样。标准库解码器若在恶意输入上 panic，按现行规矩由 Guard 上报后进程退出；是否给这一个纯函数加局部 recover 属规矩取舍，默认不加（§13 第 6 条）。

### 6.4 确定性

同一输入两次调用输出逐字节相同（`BiLinear` 与 `jpeg.Encode` 都没有随机性，单 goroutine）。`server/dedup.go` 按 `cover_base64` 全文去重的行为不变，测试里断言。

## 7. 对下游的影响

| 对象 | 影响 |
|---|---|
| 事件 | 条数、顺序、时机不变；`cover_base64` 的内容变为 800×800；MIME 可能由 `image/jpg` 变为 `image/jpeg` 或 `image/png`，由 PNG 变为 `image/jpeg`（缩放过的） |
| `server/dedup.go` | 确定性输出，去重不变；`dedup_test.go:55-73` 用字面量，不受影响 |
| 前端 `lyric_page.html` | 同首歌判定用 `cover` URL，不受影响；`data:image/` 前缀不变。非正方形源图在 80px 圆框里由 `object-fit: cover` 显示：今天是居中裁切的效果，拉伸后显示变形的图 |
| 体积 | 缩放过的封面 71～204KB；小图放大后比原图大（120² 8KB → 71KB） |
| `doc/openapi.yaml`、`API_RESPONSE_EXAMPLES.md` 示例 | 示例均为 `data:image/jpeg;base64,…`，不必改；字段描述补尺寸约定（§10） |
| `tools/docsample` | 不校验 `cover_base64`（`tools/docsample/main.go:74-76`），不受影响 |

## 8. 对各播放器的影响面

| 播放器 | 常见路径 | 变化 |
|---|---|---|
| wesing | 800 档放行 | 字节不变；`FindCoverURL` 返回非 500 档时被拉伸到 800 |
| cloudmusicv3 | 800 档放行 | 字节不变；PNG 的 MIME 改为 `image/png`、JPEG 的改为 `image/jpeg`；Redux / DOM 兜底路径被缩放（或做了源头修正后放行） |
| qqmusic | 800 档放行 | 字节不变；MIME 为 `image/jpeg`（今天取 `Content-Type`，录音里同为 `image/jpeg`） |
| kugou | 800 档放行 | 晚到的 120 档：不做源头修正则放大到 800（发糊），做了则放行 |
| sodamusic | 800 档放行 | 字节不变；`cover_url` 为字符串的分支若非 800 则缩放 |
| applemusic | 预计缩放（尺寸待确认） | 换封面那一轮多 50～80ms；`cover_base64` 为 800×800 JPEG |
| spotify（#100 合入后） | 预计缩放（档位待确认） | `cover_base64` 为 800×800；若取到 64 / 300 档，放大后发糊，需 §4 的后续 issue |

`player/spotify/*` 不改。与 PR #100 的重叠只在文档与译表（§10）。

## 9. 测试

每条先写成失败的测试再实现；每个测试做一次变异自证并记入注释（AGENTS §9）。夹具在测试里用 `image/jpeg`、`image/png` 现场生成，不入库二进制；CMYK 样本从 `GOROOT/src/image/testdata/video-001.cmyk.jpeg`（19,477 字节）拷进 `player/testdata/`。

**`player/covernormalize_test.go`（`normalizeCover` 与 `CoverDataURI`）**：

1. 800² JPEG：输出与输入 `bytes.Equal`，MIME `image/jpeg`。
2. 800² JPEG，熵编码段截断：仍原字节放行（证明放行不解码）。删掉第 4 步即红。
3. 800² PNG、入参 mime 为 `image/jpg`：原字节，MIME `image/png`。
4. 1200²、1080² JPEG：输出解码为 800×800，MIME `image/jpeg`。
5. 640²、300²、120²、1×1：放大到 800×800。
6. 800×600、600×800、801×800、800×799：都缩放（只有两边都是 800 才放行）。
7. 非正方形 1078×875，左 1/4 红、其余蓝：输出的红蓝分界在 x≈200（拉伸；居中裁切会在 x≈154），首行与末行是图像内容而不是留边；875×1078 竖向同理。
8. 1600×1600 缩放；1601×10、10×1601 原样。
9. IHDR 改为 30000²、CRC 重算的 PNG；SOF 改为 65000² 的 JPEG：原样，`testing.AllocsPerRun` 或分配量断言证明没有解码。删掉第 5 步即红。
10. 截断的非 800 JPEG（头完整）：原样，返回错误。
11. CMYK JPEG（基线与渐进式各一，渐进式用手工拼的 4 分量 SOF2 头）：原样，且用 `testing.AllocsPerRun` 或分配量断言证明没有进入解码；去掉 APP14 的同一文件：原样。删掉第 5a 步即红。
12. GIF（测试里 import `image/gif` 生成并注册）：第 3 步原样。删掉格式白名单即红。
13. 只有 RIFF/WEBP 头的字节、`"图片字节"` 这类非图片字节：原样，MIME 不变（applemusic 现有夹具依赖这一条）。
14. 0 字节：`CoverDataURI` 返回空串。
15. 带透明区的 PNG（非 800）：输出 JPEG，透明区解码后接近黑色。
16. 同一输入两次：输出逐字节相同。
17. `BenchmarkNormalizeCover`：1200、640、120 三档。
18. 渐进式 JPEG 炸弹（两种）：(a) 1600×1600 灰度 SOF2，一个全零 DC 扫描后重复 20 万次只带几字节熵数据的 AC 扫描（总长约 4MB，在 5MB 与尺寸上限之内）：原样返回，且用 `testing` 超时（200ms 内返回）断言没有进入解码，删掉第 5b 步即超时；(b) **refinement 炸弹**：1600×1600 灰度 SOF2，1 个全零 DC 扫描加 `maxJPEGScans-1` 个 `Ss=1,Se=63,Ah=1,Al=0` 的 AC refinement 扫描（每次用 EOB runs 覆盖全部块，文件不足 10KB，恰好卡在扫描上限）：这只是一种压力夹具（零系数遍历），不代表所有放行形态的最坏成本；(c) **重复基线扫描**：1600×1600 SOF0、三分量 4:4:4（采样均 0x11），重复 `maxJPEGScans` 个三分量 SOS（每块用两个一位 Huffman 码表示零 DC 与零 EOB，文件约 480KB）——Go 接受重复扫描，每次基线扫描都重新反量化、做 IDCT 并写像素（Codex 推演 16 个扫描约 192 万次块重建）；(d) **高熵大文件**：接近 5MB 字节上限的 1600×1600 基线 JPEG（真实照片内容，高质量编码）。(b)(c)(d) 用来实测 `CoverDataURI` 的耗时并记入基准，**`maxJPEGScans` 的取值以三者中最慢的为准：本机最坏耗时须 ≤100ms（主播机可能更慢），不满足就继续调低；若只靠扫描个数压不下来，再增加按「块数 × 扫描数」加权的工作量限制。**
19. 扫描次数边界：真实的渐进式 JPEG（`image/jpeg` 不能编码渐进式，夹具用 `GOROOT/src/image/testdata/video-001.progressive.jpeg` 拷进 `player/testdata/`，或在测试里手工拼 SOS 段）正常缩放；恰好 `maxJPEGScans` 个 SOS 缩放、`maxJPEGScans+1` 个原样（取值改了测试不必改；夹具尺寸避开 800×800 的放行）。
20. 16 位 RGBA PNG、Adam7 隔行 PNG、渐进式 YCbCr 4:4:4 JPEG（1600×1600 与 1200×1200 各一）：缩放成功、输出 800×800、不 panic；累计分配量记入基准，核对 §6.2 的估算。
21. **共享下载路径的接入测试**（`FetchCoverBase64` 本身）：经 `newCoverClient` 换成直连 httptest 服务器的客户端，服务器回 640×640 JPEG、800×800 JPEG、非图片字节、超过 5MB 的体；断言返回的 data URI 分别解码为 800×800、与输入逐字节相同的 800×800、原字节（MIME 不变）、空串。变异自证：把 `FetchCoverBase64` 末尾恢复为直接 base64 的旧写法，640 的用例即红。这条是 AST 门禁补不到的（门禁允许整个 `cover.go` 拼 data URI）。
22. **SSRF 校验的补测**（`player/cover.go` 今天没有）：`isBlockedCoverIP` 的分类表（回环、RFC1918、链路本地、unspecified、CGNAT、0.x、IPv6 回环/ULA/链路本地，以及公网地址与边界 100.63.255.255、100.128.0.0 不拦）；用**未替换**的生产客户端（`newCoverClient` 的默认值）去连监听在 127.0.0.1 的 httptest 服务，断言返回空串且服务端收到的请求数为零。变异自证：删掉 `Dialer.Control`（或让 `isBlockedCoverIP` 恒返回 false）即红。

**`player/coverdatauri_lint_test.go`**：§5.4 的门禁与自测。

**applemusic**：现有会话测试的封面是假字节（`sessionharness_test.go:269-281`、`sessioncover_test.go:28`、`sessionstore_test.go:37`），走第 2 步原样退回，断言不变、不改夹具。新增一条：存储里放一张真实的 1200² JPEG，宣告的 `cover_base64` 解码为 800×800；同一张图再次出现时不重复补发（`coverHash` 按原始字节）。

**kugou**：`cover_test.go:110-120` 的 seam 桩绕过 `FetchCoverBase64`，不受影响。做 §4 源头修正时补一条：晚到的 `/stdmusic/120/` URL 进入 `coverCh` 前被换成 800 档；换歌后第二拍 `info.Cover` 为 120 档时不再触发「Cover URL updated」。

**cloudmusicv3**（做源头修正时）：URL 处理函数的反例——无参数、已有 `param=`、`thumbnail=300y300`、`thumbnail` 在中间、非 `music.126.net` 主机、非 http(s)、解析失败。

## 10. 文档与注释订正

改写原句、在现有表或列表里加行，不另起专节。

| 文件 | 改动 |
|---|---|
| `doc/openapi.yaml:2549-2552` | `cover_base64` 描述补：800×800；已是 800×800 的原样（JPEG 或 PNG）；其余缩放为 JPEG，非正方形被拉伸；无法处理的原样发出、尺寸不保证 |
| `doc/API_RESPONSE_EXAMPLES.md:730` | applemusic 的 MIME 说明随之改写 |
| `doc/core.md:152`、`:218` | applemusic 封面经 `player.CoverDataURI` 规范后发出 |
| `README.md:79` | 「封面 Base64」补 800×800（§3.5 逐句回代码核） |
| `agent-doc/01-项目与数据流.md:153`、`agent-doc/13-仓库速览.md:95` | `cover.go` 的职责改为「封面下载与规范化（→ 800×800 → data URI）」 |
| `agent-doc/07-Windows机制.md:252` | applemusic 封面发出前经 `CoverDataURI` |
| `agent-doc/03-硬规则.md` §3.2、`agent-doc/04-守卫状况.md` 门禁表 | §5.4 的规矩与门禁 |
| `agent-doc/14-已知风险与未决.md` | 「已知并接受」补 EXIF 方向与 ICC 不处理（§11） |
| `i18n/messages_p4_log.go:296` 起 | 新日志与错误文案的中文 |
| `player/cover.go` | `FetchCoverBase64` 文档注释补「经 `CoverDataURI` 规范」 |
| `player/kugou/kugou.go:742` | 注释「替换为 400」与代码（`:763`，800）不符，订正 |
| `player/wesing/lyric/songinfo.go:17` | `Cover` 注释写「封面 base64」，该字段从不赋值、全仓无读取，订正为死字段说明或删字段（实现时定） |
| #100 合入后 | `agent-doc/07-Windows机制.md` 的 spotify 封面条目、`doc/API_RESPONSE_EXAMPLES.md` 的 spotify 段、openapi `cover` 描述里的 spotify 句，按合入后的原文核对 |

**与 PR #100 的合并**：#100 改了 `README.md`、`agent-doc/01/03/04/07/13`、`doc/openapi.yaml`、`doc/API_RESPONSE_EXAMPLES.md`、`i18n/messages_p4_log.go`（`git diff --name-only main...fix/4.0-spotify-review`），与上表重叠；代码文件（`player/cover.go`、`player/applemusic/applemusic.go`、`player/kugou/kugou.go`、`player/cloudmusic/cloudmusic.go`、新测试）不重叠。代码与测试可与 #100 并行；文档与译表在 #100 合入 main 后基于新 main 改，避免冲突，也便于把 spotify 一并写进去。

## 11. 不做的范围

| 不做 | 理由 |
|---|---|
| 保持宽高比（留边或裁切） | 需求原文是「拉伸」。后果：非正方形源图变形，1078×875 的图横向压缩约 19%；而今天随包前端以 `object-fit: cover` 显示的是居中裁切的效果。已知来源的专辑封面都是正方形（录音与调研样本），非正方形只可能来自兜底路径（酷狗歌手头像、QQ 歌手图、网易云 DOM 图、汽水字符串分支、applemusic 缩略图），频率未知。改为裁切只需把第 8 步的源矩形换成居中的正方形，本设计按需求原文拉伸，裁切作为建议向用户提出（§13 第 1 条） |
| 不放大小图 | 需求原文要求统一为 800。后果：120 档放大 6.7 倍、64 档放大 12.5 倍后明显发糊；以 §4 的源头修正减少这种情形 |
| 800×800 的 PNG 转 JPEG | 需求原文要求放行。网易云 800×800 的 PNG 为 1.3～1.4MB，base64 后约 1.8MB，照旧 |
| EXIF 方向校正 | 要解析 APP1；专辑封面几乎不带方向标签（未证实）。重编码后带方向标签的图会以未转正的样子显示 |
| ICC 色彩管理 | Go 不做色彩管理，要引入 cgo 或自写转换。若 applemusic 的缩略图带 Display P3 配置文件，重编码后偏灰；需真机确认（§12）后再定 |
| applemusic 改为异步缩放 | 要在会话里加结果通道、按 `coverHash` 回填，宣告变为「先无图、后补图」两条 `song_info_update`，现有封面会话测试的条数断言都要改，前端会先清空封面再显示。同步阻塞 50～80ms 可接受时不做；真机超过 100ms 再做 |
| 引入 imaging、bimg 等第三方库 | 标准库加已有的 `golang.org/x/image`（`go.mod:11`）够用；bimg 需要 cgo |
| 在 `cover` URL 上保证 800 | URL 是平台给的，前端以它判断同首歌 |

## 12. 开放问题与需真机确认

- `maxJPEGScans`（初值 16）与放行后最坏耗时：用 §9 第 18 条(b)(c)(d)的三类夹具在本机实测，最坏须 ≤100ms，取值以实测为准；主播机的 CPU 可能更慢，上限宁低勿高（低了只是更多渐进式 JPEG 原样放行）。
- applemusic 缩略图的实际尺寸、格式，以及是否带 ICC（APP2 `ICC_PROFILE`）：播放时取 `/applemusic/song_info` 的 `cover_base64` 解码读头；实现后也可直接看 `cover resized` 日志。
- applemusic 在主播机上的缩放耗时：看 `cover resized` 日志的耗时，超过 100ms 按 §11 改异步。
- spotify `currentSrc` 取到的档位：已测，300×300（§13 第 3 条）；是否改取 640 档或原图档另议。
- 酷狗晚到分支取到 120 档、换歌后第二拍改回 120 档：只有读代码推断，未真机复现。
- 网易云 Redux 与 DOM 兜底路径的出现频率；`param=800y800` 对非正方形原图是裁切、对小于 800 的原图不放大，为调研的实测与肉眼判断，无官方文档。
- QQ、wesing、汽水在原图小于 800 时 CDN 的行为（放大、返回小图还是 404）：未见样本；本设计在本地统一处理，不依赖它。
- 引入 `image/jpeg`、`image/png`、`x/image/draw` 后主程序的体积增量：实现时用同一提交前后 `go build` 的产物比较（调研在最小程序上测得约 +0.6MB，未在本仓库复测）。
- 4.0 新前端（另一仓库）如何使用 `cover_base64`：本设计只核对了随包的 `lyric_page.html`。

## 13. 决定

用户（项目负责人）的原话与决定：需求原文「拉伸」；顺序「先 Spotify（#100），#101 不冲突就同步做、先后合并」。其余点由主会话按推荐定，记在这里，用户可推翻：

1. 非正方形源图：**拉伸**（需求原文）。设计者推荐居中裁成正方形再缩放（与随包前端 `object-fit: cover` 的显示效果一致、不变形），实现上只差第 8 步的源矩形；作为建议向用户提出，用户未改口前不做。
2. 网易云兜底路径与酷狗晚到封面的源头修正：**不并入 #101**，另建 issue（两处都只有读代码的推断、没有真机复现，不在 #101 里做没验证的改动）。
3. spotify 源头改档：#100 合入后另建 issue。真机实测（2026-10-12，Spotify 1.3.4.258，连续 5 首）`currentSrc` 取到的是 `i.scdn.co/image/ab67616d00001e02…`，解码为 **300×300 JPEG，17～35KB**，不是 issue 标题说的 640；放大到 800 会发糊，建议改取 `ab67616d0000b273`（640）或原图档，见 §12。
4. 缩放后一律 JPEG、质量 90，透明处按黑底。
5. applemusic 同步缩放，换封面那一轮歌词行最多晚 50～80ms（本机）；实现后看真机日志，超过 100ms 再改异步。
6. 标准库解码器 panic 不加局部 recover，遵守现行 Guard 规矩。
7. EXIF 方向、ICC 配置文件不处理，写进 `agent-doc/14` 的「已知并接受」。