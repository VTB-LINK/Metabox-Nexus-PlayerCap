package wesing

// 本文件只测「PCM 不可用」原因的日志级别（设计 §4.4「日志」）：每个原因本会话 Warn 一次，之后降为 Detail。

import (
	"bytes"
	stdlog "log"
	"strings"
	"testing"
)

// 变异自证（实测）：不记已 Warn 过的原因（每次都 Warn），第二、四、五条红；按会话只 Warn 一次、不分原因，
// 第三条（新原因）红。
func TestPCMNotesWarnOncePerReason(t *testing.T) {
	var buf bytes.Buffer
	w, flags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	stdlog.SetFlags(0)
	defer func() { stdlog.SetOutput(w); stdlog.SetFlags(flags) }()

	n := pcmNotes{}
	for _, reason := range []string{"A", "A", "B", "A", "B"} {
		n.unavailable(reason)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{"[!]", "[+]", "[!]", "[+]", "[+]"}
	if len(lines) != len(want) {
		t.Fatalf("应有 %d 行日志，得到 %q", len(want), lines)
	}
	for i, l := range lines {
		if !strings.Contains(l, want[i]) {
			t.Errorf("第 %d 行应为 %s：%q", i+1, want[i], l)
		}
	}
}
