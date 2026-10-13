package lyric

// 本文件测 Fetch 对非 200 答复带出的状态码（StatusError）与 IsNotFound 的判定。
// 经替换 http.DefaultTransport 拦下请求，不联网；本文件的用例不并行。

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubTransport 让本测试里的 Fetch 都由 f 答复；测试结束还原。
func stubTransport(t *testing.T, f roundTripFunc) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func reply(status int, body string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	}
}

// 非 200 一律返回 StatusError（状态码可由 errors.As 取出），文案与此前逐字相同；只有 404 是 IsNotFound。
// 变异自证（均实测）：Fetch 改回普通错误 → 各个非 200 例红；IsNotFound 不看状态码 → 各个非 404 例与 TestIsNotFound 的 401、0 红；
// IsNotFound 改成类型断言 → TestIsNotFound 的「wrapped 404」红。
func TestFetchStatusError(t *testing.T) {
	i18n.SetLanguage(i18n.English)
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })

	for _, status := range []int{404, 401, 403, 400, 500, 502} {
		stubTransport(t, reply(status, ""))
		resp, err := Fetch("4DVJ7B6zpk0gOt0P5ahN1P", "Bearer x", "", "", time.Second)
		var se *StatusError
		if resp != nil || !errors.As(err, &se) || se.Status != status {
			t.Errorf("HTTP %d: got (%v, %v), want a StatusError with that status", status, resp, err)
			continue
		}
		if want := fmt.Sprintf("lyrics endpoint returned HTTP %d", status); err.Error() != want {
			t.Errorf("HTTP %d: message %q, want %q (unchanged)", status, err.Error(), want)
		}
		if IsNotFound(err) != (status == http.StatusNotFound) {
			t.Errorf("HTTP %d: IsNotFound = %v", status, IsNotFound(err))
		}
	}

	// 200：照常解析，没有错误。
	stubTransport(t, reply(200, `{"lyrics":{"syncType":"LINE_SYNCED","lines":[]}}`))
	if resp, err := Fetch("id", "Bearer x", "", "", time.Second); err != nil || resp == nil || SyncType(resp) != "LINE_SYNCED" {
		t.Errorf("HTTP 200: got (%v, %v)", resp, err)
	}

	// 网络错误、没有 token：不是 StatusError。
	stubTransport(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
	if _, err := Fetch("id", "Bearer x", "", "", time.Second); err == nil || IsNotFound(err) || errors.As(err, new(*StatusError)) {
		t.Errorf("network error: got %v, want a plain error", err)
	}
	if _, err := Fetch("id", "", "", "", time.Second); err == nil || errors.As(err, new(*StatusError)) {
		t.Errorf("no token: got %v, want a plain error", err)
	}
}

// IsNotFound 只认 StatusError 的状态码：包了一层照样认，文案里写着 404 的普通错误不认。
func TestIsNotFound(t *testing.T) {
	nf := &StatusError{Status: 404, Err: errors.New("x")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"404", nf, true},
		{"wrapped 404", fmt.Errorf("layer: %w", nf), true},
		{"401", &StatusError{Status: 401, Err: errors.New("x")}, false},
		{"0", &StatusError{Status: 0, Err: errors.New("x")}, false},
		{"plain error saying 404", errors.New("lyrics endpoint returned HTTP 404"), false},
	}
	for _, c := range cases {
		if got := IsNotFound(c.err); got != c.want {
			t.Errorf("%s: IsNotFound = %v, want %v", c.name, got, c.want)
		}
	}
}
