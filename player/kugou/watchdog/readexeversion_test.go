package watchdog

// 本文件只测 readExeVersion：从 PE 版本资源读 FileVersion 字符串。
// 断言必须锚定实值——只验格式是同义反复，抓不住指针算错（详见函数注释）。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// parseVer 把 "a.b.c.d" 拆成四个整数，供实值锚定断言使用。
func parseVer(t *testing.T, s string) [4]int {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		t.Fatalf("版本串 %q 不是 a.b.c.d 形式", s)
	}
	var v [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("版本串 %q 的第 %d 段不是整数: %v", s, i+1, err)
		}
		v[i] = n
	}
	return v
}

// TestReadExeVersion 用系统自带的 PE 文件验证版本探测。
//
// **断言必须锚定实值，不能只验格式。** 此前这里写的是正则 `^\d+\.\d+\.\d+\.\d+$`
// ——那是对 fmt.Sprintf("%d.%d.%d.%d", 四个 uint32) 的同义反复，任何取值都必然匹配，
// 物理上不可能失败。变异实测证明过：把 &buf[off] 改成 &buf[off+4]（正是要防的那类
// 指针算错），该测试照样 PASS 并返回 "19041.5915.10.0" 这种格式合法的垃圾版本号。
//
// 现在锚定 kernel32 的 FileVersionMS 主版本号：微软为应用兼容把它冻结在 6.2，
// Win8/10/11 皆然（实测本机为 6.2.19041.5915）。偏移一旦算错，Signature 校验会先
// 拦下并返回空串，此断言即失败。
func TestReadExeVersion(t *testing.T) {
	sysDir := os.Getenv("SystemRoot")
	if sysDir == "" {
		sysDir = `C:\Windows`
	}
	path := filepath.Join(sysDir, "System32", "kernel32.dll")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("找不到 %s，跳过：%v", path, err)
	}

	got := readExeVersion(path)
	if got == "" {
		t.Fatalf("readExeVersion(%s) 返回空——版本资源解析失败或 Signature 校验未通过", path)
	}
	v := parseVer(t, got)

	// 实值锚定：kernel32 的 FileVersionMS 恒为 6.2（微软冻结以保应用兼容）
	if v[0] != 6 || v[1] != 2 {
		t.Errorf("kernel32 FileVersionMS 应为 6.2，实得 %d.%d（版本 %q）——疑似读到了错误偏移", v[0], v[1], got)
	}
	// build 号是真实的 Windows 构建号，五位数；垃圾数据几乎不可能落在这个区间
	if v[2] < 10000 || v[2] > 99999 {
		t.Errorf("kernel32 build 号 %d 不像真实 Windows 构建号（版本 %q）", v[2], got)
	}
	t.Logf("kernel32.dll FileVersion = %s", got)
}

// TestReadExeVersionBadInput 确认坏输入返回空串。
//
// 关于「为什么空串是安全的」——这里曾经写过一段完全相反的叙事，说「返回垃圾但格式
// 合法的版本号可能意外匹配上哨兵并触发盲打补丁」。**那是编的，写的时候没读调用方。**
// 真实的 EnsurePatched（watchdog.go:567-581）：
//   - ver == "" → 回落到 libcef 父目录名继续跑，目录名通常就是真版本，所以空串安全；
//   - ver == knownVersion → 只把日志从 Warn 降成 Detail，**哨兵不是闸门**，不匹配照样 patch；
//   - 唯一的真闸门是 strings.HasPrefix(ver, "10.")。
//
// 所以真实危险方向是反的：垃圾版本号只要不以 "10." 开头，反而**绕过唯一的拒绝闸门**。
// 空串因为有目录名兜底，反倒是三者中最安全的结果——这正是 Signature 校验的价值：
// 把「格式合法的垃圾」变成「空串 → 目录名兜底 → 正确版本」。
func TestReadExeVersionBadInput(t *testing.T) {
	notPE := filepath.Join(t.TempDir(), "notpe.dll")
	if err := os.WriteFile(notPE, []byte("definitely not a PE file"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"不存在的路径":  filepath.Join(t.TempDir(), "nope.dll"),
		"空路径":     "",
		"非 PE 文件": notPE,
	}
	for name, p := range cases {
		if got := readExeVersion(p); got != "" {
			t.Errorf("%s: readExeVersion(%q) = %q，应返回空串", name, p, got)
		}
	}
}
