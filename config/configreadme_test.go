package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigReadmeMatchesTemplate 钉住 CONFIG.README.md 里的 yaml 代码块与 DefaultConfigContent("v4.0.0")
// 逐字节一致（issue #242），防止新增播放器、新增配置键或调整默认偏移与注释时示例滞后于模板。
//
// 变异自证（隔离 worktree 实测）：CONFIG.README.md 删去 kuwo 段 → 红；kuwo-offset 改写为 -300 → 红；
// 在文末再加第二个 ```yaml 代码块 → 红。
func TestConfigReadmeMatchesTemplate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "CONFIG.README.md"))
	if err != nil {
		t.Fatalf("读取 CONFIG.README.md 失败: %v", err)
	}
	const openFence = "\n```yaml\n"
	const closeFence = "\n```\n"

	doc := string(raw)
	before, rest, ok := strings.Cut(doc, openFence)
	if !ok {
		t.Fatal("CONFIG.README.md 里找不到 ```yaml 代码块")
	}
	_ = before
	block, after, ok := strings.Cut(rest, closeFence)
	if !ok {
		t.Fatal("CONFIG.README.md 的 ```yaml 代码块缺少闭合围栏")
	}
	if strings.Contains(after, "```yaml") {
		t.Fatal("CONFIG.README.md 里不应有多个 ```yaml 代码块")
	}

	got := block + "\n"
	want := DefaultConfigContent("v4.0.0")
	if got != want {
		t.Errorf("CONFIG.README.md 的配置示例与 DefaultConfigContent(\"v4.0.0\") 不一致:\n--- CONFIG.README.md ---\n%s\n--- DefaultConfigContent ---\n%s", got, want)
	}
}
