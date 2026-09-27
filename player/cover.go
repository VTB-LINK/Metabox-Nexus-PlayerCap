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

// FetchCoverBase64 下载封面图片并返回 base64 编码的 data URI。
// playerName 用于日志前缀标识调用来源；timeout 控制 HTTP 下载超时，超时则返回空字符串（不阻塞主流程）。
//
// SSRF 加固（issue #12）：只接受 http/https scheme；并在拨号回调里对「实际要连的 IP」拒绝
// 回环/内网/链路本地/unspecified 地址。校验落在 DNS 解析之后、connect 之前，对每个候选 IP 逐一
// 检查，天然避开「先 LookupIP 再 Get」的 TOCTOU / DNS-rebinding 窗口。出错一律返回空串（降级，
// 沿用本函数既有风格）。不设代理：封面走公网 CDN 直连，经代理会绕过下面的内网 IP 校验。
func FetchCoverBase64(playerName string, coverURL string, timeout time.Duration) string {
	if coverURL == "" {
		return ""
	}

	// scheme 白名单：挡掉 file://、gopher:// 等可被利用的 scheme。
	if u, err := url.Parse(coverURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		coverLog.Detail("[%s] 封面 URL 非 http/https，跳过: %q", playerName, coverURL)
		return ""
	}

	client := &http.Client{
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

	encoded := base64.StdEncoding.EncodeToString(body)
	coverLog.Detail("[%s] 封面已获取 (%d bytes → base64)", playerName, len(body))
	return "data:" + mimeType + ";base64," + encoded
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
