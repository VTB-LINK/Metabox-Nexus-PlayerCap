# 酷我封面取数

`Fetch(ctx, rid, timeout)` 接受数字 RID 或 `MUSIC_<数字>`，返回关联 RID、原图 URL 和经
`player.FetchCoverBase64` 规范化的 800×800 data URI。由播放器的独立封面任务调用；换歌后
必须按 RID/会话代次丢弃迟到结果。此包没有注册播放器，也没有把封面事件接入主循环。

## 请求依据与实测（2026-10-22）

安装版 9.6.0.0 的 `bin/html/webdata/netsong/static/common.js` 定义：

```text
getCover: http://artistpic.kwcdn.kuwo.cn/pic.web
```

同目录 `singleComment.js` 的 `setCoverLink` 发起 GET，参数为
`type=rid_pic&pictype=url&size=500&rid=<数字>`，读取纯文本 URL，将 `NO_PIC` 当作无图。
实现沿用该形态，不调用宿主桥，不需要 CDP、账户信息或重启播放器。

分析时的文件 SHA-256：

- common.js：`e963872371e71a16ef55fbc67cead224a952e1c3d28d3815335c2128c40442d1`
- singleComment.js：`d1331f12662a12a31287ebb78d5fceea13fa8af7aadfb89a4c56c46578799d43`

本机日志中出现的 RID `MUSIC_568213041`，线上完整链路实测一次：

```text
HTTP 200
http://img1.kwcdn.kuwo.cn/star/albumcover/500/s4s67/92/1349231143.jpg
下载 JPEG 500×500，69529 字节
规范化 JPEG 800×800，120734 字节
SHA-256 38a212d5f473c25c23860f4685a902dcca9849d566f718ac83a60e598c0e2546
```

这证明该 RID 的封面可取，不代表所有 RID 都有封面，也不构成当前歌曲识别的验证。

## 可重复验证

```powershell
go test ./player/kuwo/cover
$env:PLAYERCAP_LIVE = '1'
$env:KUWO_RID = 'MUSIC_568213041'
go test ./player/kuwo/cover -run TestLiveFetch -v -count=1
Remove-Item Env:PLAYERCAP_LIVE, Env:KUWO_RID
```

离线测试覆盖请求字段、无图/坏响应、RID 校验及取消后丢弃结果；真机用例核对生产下载链路的
图片解码和尺寸。已在独立临时克隆中去掉取消后的 DataURI 清空，确认
`TestCancelledCoverResultDiscarded` 失败，工作区原实现通过。

边界：共享图片下载器暂不接受 context，取消不能立即中断已开始的图片下载；下载总超时受
剩余预算限制，结束时检查 context 并丢弃结果。调用方仍需保护“旧曲任务晚于新曲完成”以及
“同 RID 跨会话”的事件发布。接口及返回图片域名变化会返回错误降级，需要重新验证。
