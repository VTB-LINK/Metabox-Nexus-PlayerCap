package effect

// 本文件守注入网易云的抓帧脚本体积：网易云 3.1.38 的 CDP 连接收到单条超过约 4096 字节的消息即断开
// （2026-10-13 真机二分：整条消息 3989 字节通过、4039 字节断开；见 agent-doc 07 §7.6）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// cdpMessageBytes 按 purelayerSession 的写法（gorilla 的 WriteJSON：encoding/json 转义 < > &，末尾带换行）
// 算一条 CDP 请求的字节数。id 取六位数，比会话里实际用到的大。
func cdpMessageBytes(t *testing.T, method string, params map[string]any) int {
	t.Helper()
	b, err := json.Marshal(map[string]any{"id": 100000, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return len(b) + 1
}

// TestCaptureScriptFitsCDPMessage 抓帧脚本经 Runtime.evaluate 与 Page.addScriptToEvaluateOnNewDocument
// 发出，整条消息须不超过 3900 字节（离约 4096 的断开点留余量）。
//
// 变异自证：captureScript 不经 compactJS（保留注释与缩进），两条均红。
func TestCaptureScriptFitsCDPMessage(t *testing.T) {
	url := "ws://127.0.0.1:8765/cloudmusicv3/effect-ingest?t=" + strings.Repeat("0123456789abcdef", 2)
	src := captureScript(url, 95)
	for _, m := range []struct {
		method string
		params map[string]any
	}{
		{"Runtime.evaluate", map[string]any{"expression": src, "returnByValue": true}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": src}},
	} {
		n := cdpMessageBytes(t, m.method, m.params)
		t.Logf("%s: %d bytes", m.method, n)
		if n > 3900 {
			t.Errorf("%s carrying the capture script is %d bytes (> 3900): NetEase Cloud Music 3.1.38 drops "+
				"the CDP connection on a single message over about 4096 bytes, see agent-doc 07 §7.6", m.method, n)
		}
	}
}

// TestCompactJS 只去整行注释、行首缩进与空行；行内的 // 与行尾空白不动。
//
// 变异自证：改成去掉每行第一个 // 之后的内容，「行内 URL」「行尾注释」两例红（抓帧脚本里 ws:// 被截断，
// node 用例与 TestCaptureScriptFormatsCleanly 也红）；不去行首缩进，四例红——此时整条消息约 3766 字节，
// TestCaptureScriptFitsCDPMessage 的 3900 拦不住，要靠本例。
func TestCompactJS(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"whole-line comment", "a();\n  // note\nb();", "a();\nb();"},
		{"indentation", "  a();\n\t\tb();", "a();\nb();"},
		{"blank lines", "a();\n\n   \nb();", "a();\nb();"},
		{"URL inside a string", `  var u="ws://h/p";`, `var u="ws://h/p";`},
		{"trailing comment kept", "  a(); // tail", "a(); // tail"},
		{"regex not at line start", `  if(/webgl/i.test(x))y();`, `if(/webgl/i.test(x))y();`},
		{"comment marker with leading tab", "\t// c\na();", "a();"},
		{"no trailing newline added", "a();\n", "a();"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactJS(tc.in); got != tc.want {
				t.Fatalf("compactJS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
