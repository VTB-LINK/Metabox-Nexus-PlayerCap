package player

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/logger"
)

var coverLog = logger.New("Cover")

// FetchCoverBase64 下载封面图片，经 CoverDataURI 规范为 800×800 后返回 data URI。
// playerName 用于日志前缀标识调用来源；timeout 控制 HTTP 下载超时，超时则返回空字符串（不阻塞主流程）。
//
// SSRF 加固（issue #12）：只接受 http/https scheme；并在拨号回调里对「实际要连的 IP」拒绝
// 回环/内网/链路本地/unspecified 地址。校验落在 DNS 解析之后、connect 之前，对每个候选 IP 逐一
// 检查，天然避开「先 LookupIP 再 Get」的 TOCTOU / DNS-rebinding 窗口。下载出错一律返回空串（降级，
// 沿用本函数既有风格）；规范化失败时原字节照发（见 CoverDataURI）。不设代理：封面走公网 CDN 直连，
// 经代理会绕过下面的内网 IP 校验。
func FetchCoverBase64(playerName string, coverURL string, timeout time.Duration) string {
	if coverURL == "" {
		return ""
	}

	// scheme 白名单：挡掉 file://、gopher:// 等可被利用的 scheme。
	if u, err := url.Parse(coverURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		coverLog.Detail("[%s] cover URL is not http/https, skipping: %q", playerName, coverURL)
		return ""
	}

	// 每次下载都是一个独立的客户端（独立的 Transport）：不回收的话，对端保活时空闲连接与它的两个 goroutine 会一直留着
	// （Transport 默认没有空闲超时，Body.Close 与 Client.Timeout 都不清理空闲连接）。
	client := newCoverClient(timeout)
	defer client.CloseIdleConnections()
	resp, err := client.Get(coverURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	// 限制最大 5MB
	const maxSize = 5 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return ""
	}
	if int64(len(body)) > maxSize {
		return "" // 超过上限，放弃 base64，让前端用 URL 加载
	}
	// 校验 Content-Length（如有）防止被 LimitReader 静默截断
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if expected, err := strconv.ParseInt(cl, 10, 64); err == nil && int64(len(body)) < expected {
			return "" // 下载不完整
		}
	}

	// 根据 URL 后缀或 Content-Type 确定 MIME
	mimeType := "image/jpeg"
	if strings.HasSuffix(coverURL, ".png") {
		mimeType = "image/png"
	} else if ct := resp.Header.Get("Content-Type"); ct != "" && strings.HasPrefix(ct, "image/") {
		mimeType = ct
	}

	coverLog.Detail("[%s] cover fetched (%d bytes → base64)", playerName, len(body))
	return CoverDataURI(playerName, body, mimeType)
}

// newCoverClient 构造封面下载用的 HTTP 客户端。做成包级变量只为测试能换成直连 httptest 回环服务的客户端
// （生产值的拨号检查拒绝回环地址），生产代码不改它。
var newCoverClient = func(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Control 在 DNS 解析之后、connect 之前对每个候选 IP 调一次：命中内网/非法地址即拒绝。
			DialContext: (&net.Dialer{
				Timeout: timeout,
				Control: func(_, address string, _ syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}
					if ip := net.ParseIP(host); ip == nil || isBlockedCoverIP(ip) {
						return fmt.Errorf("封面拒绝连接内网/非法地址: %s", address)
					}
					return nil
				},
			}).DialContext,
		},
	}
}

// CoverDataURI 把封面图片规范为 800×800 后编成 data URI（issue #101），b 为空时返回空串。cover_base64 一律经它
// 生成。已是 800×800 的 JPEG / PNG 原字节发出、MIME 按格式改正；其余 JPEG / PNG 居中裁成正方形（非正方形裁去
// 长边两端）再缩放到 800×800、编码为 JPEG。处理不了的（读不出头、格式不在白名单、尺寸超限、CMYK、JPEG 扫描次数超限、解码或编码失败）原字节、原 mime
// 发出，并打一条警告。在调用方的 goroutine 里同步执行：本机缩放一张 1200² 的 JPEG 约 75ms（BenchmarkNormalizeCover）。
func CoverDataURI(playerName string, b []byte, mime string) string {
	if len(b) == 0 {
		return ""
	}
	start := time.Now()
	out, outMIME, w, h, err := normalizeCover(b, mime)
	if err != nil {
		coverLog.Warn("[%s] cover not normalized, sent as is: %v", playerName, err)
	} else if w != coverSide || h != coverSide {
		coverLog.Detail("[%s] cover resized %dx%d → 800x800 (%d → %d bytes, %v)", playerName, w, h, len(b), len(out), time.Since(start))
	}
	return "data:" + outMIME + ";base64," + base64.StdEncoding.EncodeToString(out)
}

// isBlockedCoverIP 判断目标 IP 是否属于封面下载 SSRF 防护必须拒绝的范围（issue #12）：
// 回环（127/8、::1）、私有网（RFC1918、IPv6 ULA）、链路本地（169.254/16、fe80::/10）、
// unspecified（0.0.0.0、::）；以及 net 标准库判定之外的两段保留地址：CGNAT 100.64.0.0/10
// （RFC6598，运营商级 NAT，不属 IsPrivate 但同为内网面）、"本网络" 0.0.0.0/8（0.0.0.0 已由
// IsUnspecified 覆盖，此处补 0.x.x.x 其余）。命中即拒绝，防封面 URL 被用来探/打本机与内网服务。
func isBlockedCoverIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10：首字节 100、次字节高 2 位为 01（即 64–127）
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8 "本网络"保留段
		if ip4[0] == 0 {
			return true
		}
	}
	return false
}
