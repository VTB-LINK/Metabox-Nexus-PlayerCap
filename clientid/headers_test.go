package clientid

// 本文件守接线：Apply 到底往请求上写了什么。
//
// 最要紧的一条是 TestApplySetsOnlyRegisteredHeaders —— HeaderNames() 是隐私提示门禁的
// 输入，一旦 Apply 偷偷多写一个头而没登记，那条门禁就在核对一份不完整的清单，
// 我们对主播的承诺会安静地落后于代码。

import (
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

// fullEnv 是「什么都采到了」的环境，用来观察 Apply 的完整输出面。
var fullEnv = Env{
	Version:   "3.0.0-rc.14",
	OSVersion: "10.0.19045",
	OSEdition: "Enterprise",
	Arch:      "x64",
	Ping:      PingStart,
}

func newTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/client-version", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// registeredKeys 把 HeaderNames() 转成 net/http 实际使用的规范化键。
//
// Go 的 textproto 会把 "X-Client-OS" 规范化成 "X-Client-Os"（每段首字母大写、其余小写），
// 所以不能拿常量原值去比 req.Header 的键。这不是 bug：HTTP 头名大小写不敏感，
// nginx/Kong 那侧看到的一律是小写。
func registeredKeys() map[string]bool {
	keys := make(map[string]bool, len(HeaderNames()))
	for _, name := range HeaderNames() {
		keys[textproto.CanonicalMIMEHeaderKey(name)] = true
	}
	return keys
}

// TestApplySetsOnlyRegisteredHeaders 钉死「Apply 写的每一个头都登记在 HeaderNames()」。
//
// 变异自证：在 Apply 里加一句 req.Header.Set("X-Client-Secret", "x") 而不改 HeaderNames()，
// 本用例当场红。
func TestApplySetsOnlyRegisteredHeaders(t *testing.T) {
	resetID(t, func() (string, error) { return sampleGUID, nil })

	req := newTestRequest(t)
	Apply(req, fullEnv)

	allowed := registeredKeys()
	for key := range req.Header {
		if !allowed[key] {
			t.Errorf("Apply 写了未登记的请求头 %q —— 把它加进 HeaderNames()，"+
				"否则隐私提示门禁核对的是一份不完整的清单", key)
		}
	}
}

// TestApplySetsEveryRegisteredHeader 反向：登记了就得真的发出去。
//
// 登记一个从不出现的头会让隐私提示写着我们其实没发的东西——同样是文案与代码分叉，
// 只是方向相反。
func TestApplySetsEveryRegisteredHeader(t *testing.T) {
	resetID(t, func() (string, error) { return sampleGUID, nil })

	req := newTestRequest(t)
	Apply(req, fullEnv)

	for _, name := range HeaderNames() {
		if req.Header.Get(name) == "" {
			t.Errorf("HeaderNames() 里登记了 %q，但环境齐备时 Apply 并没有发它", name)
		}
	}
}

func TestApplyCarriesValues(t *testing.T) {
	resetID(t, func() (string, error) { return sampleGUID, nil })

	req := newTestRequest(t)
	Apply(req, fullEnv)

	for _, c := range []struct{ header, want string }{
		{HeaderID, derive(sampleGUID)},
		{HeaderVersion, "3.0.0-rc.14"},
		{HeaderOS, "10.0.19045"},
		{HeaderEdition, "Enterprise"},
		{HeaderArch, "x64"},
		{HeaderPing, PingStart},
	} {
		if got := req.Header.Get(c.header); got != c.want {
			t.Errorf("%s = %q，期望 %q", c.header, got, c.want)
		}
	}
}

// TestApplyOmitsEmptyValues 钉死「采不到就不发这个头」，而不是发一个空值。
//
// 空值头在网关侧看着像「采到了，是空的」，缺头才是「没采到」。两者在报表里是不同的结论。
//
// 变异自证：把 Apply 里 set 的 `if value != ""` 判据去掉，本用例当场红。
func TestApplyOmitsEmptyValues(t *testing.T) {
	resetID(t, func() (string, error) { return "", nil }) // 读得到，但值是空的

	req := newTestRequest(t)
	Apply(req, Env{Version: "3.0.0", Ping: PingDaily}) // 只有版本与时机，系统信息全缺

	for _, absent := range []string{HeaderID, HeaderOS, HeaderEdition, HeaderArch} {
		if _, ok := req.Header[textproto.CanonicalMIMEHeaderKey(absent)]; ok {
			t.Errorf("%s 采不到时仍被写进了请求（值 %q）—— 应当整个不发这个头",
				absent, req.Header.Get(absent))
		}
	}
	if got := req.Header.Get(HeaderPing); got != PingDaily {
		t.Errorf("%s = %q，期望 %q", HeaderPing, got, PingDaily)
	}
}

func TestUserAgentShape(t *testing.T) {
	got := userAgent(fullEnv)
	want := "Metabox-Nexus-PlayerCap/3.0.0-rc.14 (Windows NT 10.0.19045; x64)"
	if got != want {
		t.Errorf("UA = %q，期望 %q", got, want)
	}
	if strings.Contains(got, "Go-http-client") {
		t.Error("UA 里仍带着 Go 默认 UA")
	}
}

// TestUserAgentDegradesGracefully 系统信息缺失时 UA 仍要合法，别拼出
// "Windows NT ; " 这种带空洞的串。
func TestUserAgentDegradesGracefully(t *testing.T) {
	got := userAgent(Env{Version: "3.0.0"})
	want := "Metabox-Nexus-PlayerCap/3.0.0 (Windows)"
	if got != want {
		t.Errorf("UA = %q，期望 %q", got, want)
	}
}

// TestUserAgentEmptyWithoutVersion 没有版本号就整个不发 UA。
//
// 只有产品名、没有版本的 UA 在网关日志里认得出是我们，却回答不了任何问题；
// 保持 Go 默认值反而一眼能看出这是条异常请求。
func TestUserAgentEmptyWithoutVersion(t *testing.T) {
	if got := userAgent(Env{OSVersion: "10.0.19045", Arch: "x64"}); got != "" {
		t.Errorf("没有版本号时 UA = %q，期望空串", got)
	}
}
