// Package cover resolves Kuwo album artwork by RID without calling the player.
// The request shape comes from 9.6.0.0's singleComment.js setCoverLink.
package cover

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

const endpoint = "http://artistpic.kwcdn.kuwo.cn/pic.web"

var ErrNoCover = errors.New("kuwo: no cover")

// Result retains the RID so a caller can discard results arriving after a song change.
type Result struct {
	RID     string
	URL     string
	DataURI string
}

// Fetch resolves and downloads artwork, including the shared 800x800 normalization.
// Run it in the caller's cancellable cover job, not its playback polling loop.
// The shared downloader has a timeout rather than a context; an in-flight download
// can therefore finish after cancellation, but its result is discarded here.
func Fetch(ctx context.Context, rid string, timeout time.Duration) (Result, error) {
	return fetch(ctx, rid, timeout, endpoint, player.FetchCoverBase64)
}

func fetch(ctx context.Context, rid string, timeout time.Duration, endpointURL string, download func(string, string, time.Duration) string) (Result, error) {
	id, err := numericRID(rid)
	if err != nil {
		return Result{}, err
	}
	result := Result{RID: "MUSIC_" + id}
	if timeout <= 0 {
		return result, i18n.Errorf("kuwo: cover timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Timeout: timeout,
		// This endpoint returns a URL in its body; HTTP redirects are not part
		// of the observed protocol and must not silently change the API target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	result.URL, err = resolve(ctx, client, endpointURL, id)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return result, context.DeadlineExceeded
	}
	result.DataURI = download("kuwo", result.URL, remaining)
	if err := ctx.Err(); err != nil {
		result.DataURI = ""
		return result, err
	}
	if result.DataURI == "" {
		return result, i18n.Errorf("kuwo: cover download failed")
	}
	return result, nil
}

func numericRID(rid string) (string, error) {
	id := strings.TrimPrefix(rid, "MUSIC_")
	if id == "" || len(id) > 20 || strings.Trim(id, "0") == "" {
		return "", i18n.Errorf("kuwo: invalid cover RID")
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return "", i18n.Errorf("kuwo: invalid cover RID")
		}
	}
	return id, nil
}

func resolve(ctx context.Context, client *http.Client, endpointURL, id string) (string, error) {
	u, err := url.Parse(endpointURL)
	if err != nil {
		return "", i18n.Errorf("kuwo: invalid cover endpoint: %w", err)
	}
	u.RawQuery = url.Values{"type": {"rid_pic"}, "pictype": {"url"}, "size": {"500"}, "rid": {id}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", i18n.Errorf("kuwo: failed to build cover request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", i18n.Errorf("kuwo: cover lookup failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", i18n.Errorf("kuwo: cover lookup HTTP %d", resp.StatusCode)
	}
	const maxResponse = 4096
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return "", i18n.Errorf("kuwo: failed to read cover response: %w", err)
	}
	if len(body) > maxResponse {
		return "", i18n.Errorf("kuwo: cover lookup response too large")
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" || raw == "NO_PIC" {
		return "", ErrNoCover
	}
	imageURL, err := url.Parse(raw)
	if err != nil || (imageURL.Scheme != "http" && imageURL.Scheme != "https") || imageURL.User != nil || imageURL.Port() != "" || imageURL.Fragment != "" {
		return "", i18n.Errorf("kuwo: invalid cover URL")
	}
	host := strings.ToLower(imageURL.Hostname())
	if !strings.HasSuffix(host, ".kuwo.cn") || imageURL.Path == "" || strings.ContainsAny(raw, "\r\n\t ") {
		return "", i18n.Errorf("kuwo: unexpected cover URL")
	}
	return imageURL.String(), nil
}
