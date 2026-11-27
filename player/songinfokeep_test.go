package player

// 本文件只测同一次宣告内歌曲信息字段（name、singer）的补正取舍函数与全局开关
// （SetSongInfoKeepFirst / SupplementField，#229）。

import "testing"

// initialSupplementOnZeroValue 在包初始化阶段（任何测试调用 SetSongInfoKeepFirst 之前）
// 采集 SupplementField 的结果，用于钉死「零值即开启」语义而不依赖未导出状态变量名。
var (
	initialKeepNonEmpty = SupplementField("A", "B")
	initialKeepNoClear  = SupplementField("A", "")
	initialFillEmpty    = SupplementField("", "B")
)

// TestSongInfoKeepFirstZeroValueAndToggle 校验未调用 SetSongInfoKeepFirst 时零值即开启，
// 以及 SetSongInfoKeepFirst(false) / SetSongInfoKeepFirst(true) 可正确切换开关语义（U2）。
//
// 变异自证（隔离副本，实测）：存放不取反（Store(on) + !Load()，零值即关闭）→ 红；
// 开启一律取 A / 开启一律取 R / 关闭仍按开启取舍 → 各自红。
func TestSongInfoKeepFirstZeroValueAndToggle(t *testing.T) {
	t.Cleanup(func() { SetSongInfoKeepFirst(true) })

	if initialKeepNonEmpty != "A" {
		t.Errorf("零值状态 SupplementField(\"A\", \"B\") = %q，期望 \"A\"（零值应为开启）", initialKeepNonEmpty)
	}
	if initialKeepNoClear != "A" {
		t.Errorf("零值状态 SupplementField(\"A\", \"\") = %q，期望 \"A\"（零值应为开启）", initialKeepNoClear)
	}
	if initialFillEmpty != "B" {
		t.Errorf("零值状态 SupplementField(\"\", \"B\") = %q，期望 \"B\"", initialFillEmpty)
	}

	SetSongInfoKeepFirst(false)
	if got := SupplementField("A", "B"); got != "B" {
		t.Errorf("关闭开关后 SupplementField(\"A\", \"B\") = %q，期望 \"B\"", got)
	}
	if got := SupplementField("A", ""); got != "" {
		t.Errorf("关闭开关后 SupplementField(\"A\", \"\") = %q，期望 \"\"", got)
	}

	SetSongInfoKeepFirst(true)
	if got := SupplementField("A", "B"); got != "A" {
		t.Errorf("重新开启开关后 SupplementField(\"A\", \"B\") = %q，期望 \"A\"", got)
	}
	if got := SupplementField("A", ""); got != "A" {
		t.Errorf("重新开启开关后 SupplementField(\"A\", \"\") = %q，期望 \"A\"", got)
	}
	if got := SupplementField("", "B"); got != "B" {
		t.Errorf("重新开启开关后 SupplementField(\"\", \"B\") = %q，期望 \"B\"", got)
	}
}

// TestSupplementField 表驱动覆盖开关（开/关）× announced ∈ {"", "  ", "A"} × read ∈ {"", " ", "A", "B"}
// 全部 24 种组合（U1）。
//
// 变异自证（隔离副本，实测）：开启一律取 A → 开/空/A、开/空/B、开/空白/A、开/空白/B 红；
// 开启一律取 R → 开/空/空白、开/空白/空、开/空白/空白、开/A/空、开/A/空白、开/A/B 红；
// 为空不去空白（announced != ""）→ 开/空白/A、开/空白/B 红；
// R 为空白也补（删去 TrimSpace(read) == ""）→ 开/空/空白、开/空白/空、开/空白/空白 红；
// 关闭仍按开启取舍（忽略开关）→ 本用例与 U2 红。
func TestSupplementField(t *testing.T) {
	t.Cleanup(func() { SetSongInfoKeepFirst(true) })

	cases := []struct {
		name      string
		keepFirst bool
		announced string
		read      string
		want      string
	}{
		// 开关开启（12 例）
		{"开/空/空", true, "", "", ""},
		{"开/空/空白", true, "", " ", ""},
		{"开/空/A", true, "", "A", "A"},
		{"开/空/B", true, "", "B", "B"},
		{"开/空白/空", true, "  ", "", "  "},
		{"开/空白/空白", true, "  ", " ", "  "},
		{"开/空白/A", true, "  ", "A", "A"},
		{"开/空白/B", true, "  ", "B", "B"},
		{"开/A/空", true, "A", "", "A"},
		{"开/A/空白", true, "A", " ", "A"},
		{"开/A/A", true, "A", "A", "A"},
		{"开/A/B", true, "A", "B", "A"},

		// 开关关闭（12 例）：一律取 read
		{"关/空/空", false, "", "", ""},
		{"关/空/空白", false, "", " ", " "},
		{"关/空/A", false, "", "A", "A"},
		{"关/空/B", false, "", "B", "B"},
		{"关/空白/空", false, "  ", "", ""},
		{"关/空白/空白", false, "  ", " ", " "},
		{"关/空白/A", false, "  ", "A", "A"},
		{"关/空白/B", false, "  ", "B", "B"},
		{"关/A/空", false, "A", "", ""},
		{"关/A/空白", false, "A", " ", " "},
		{"关/A/A", false, "A", "A", "A"},
		{"关/A/B", false, "A", "B", "B"},
	}

	if len(cases) != 24 {
		t.Fatalf("用例总数 = %d，期望 24", len(cases))
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			SetSongInfoKeepFirst(c.keepFirst)
			if got := SupplementField(c.announced, c.read); got != c.want {
				t.Errorf("keepFirst=%v SupplementField(%q, %q) = %q，期望 %q",
					c.keepFirst, c.announced, c.read, got, c.want)
			}
		})
	}
}
