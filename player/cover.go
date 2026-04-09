package player

import (
	"encoding/base64"
	"io"
	"net/http"
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

	// 限制最大 2MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return ""
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
