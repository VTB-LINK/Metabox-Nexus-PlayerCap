package player

import (
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FetchCoverBase64 下载封面图片并返回 base64 编码的 data URI。
// timeout 控制 HTTP 下载超时，超时则返回空字符串（不阻塞主流程）。
func FetchCoverBase64(coverURL string, timeout time.Duration) string {
	if coverURL == "" {
		return ""
	}

	client := &http.Client{Timeout: timeout}
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
	return "data:" + mimeType + ";base64," + encoded
}
