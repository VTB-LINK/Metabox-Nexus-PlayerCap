package watchdog

// 本文件只测 libcef.dll 的选址：必须选中 KuGou.exe 实际加载的那个版本目录，而不是升级
// 残留的旧版本目录（拿错版本的偏移表去打 = 写坏 DLL）。含两级——注册表 KuGou8 首选（守卫）
// 与 exe 版本推导兜底。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKugou8LibcefIfDir 钉死 KuGou8 首选信号的守卫。
//
// KuGou8 是酷狗自己维护、指向当前版本目录的权威指针，是「选中实际加载的那份 libcef.dll」
// 最直接的信号，故作首选。但它会变——实测有的机器/时刻它指向 KuGou.exe（文件）而非版本
// 目录，此时必须视为不可用、回落 exe 版本推导，否则会 Join 出无效路径。951a361 当年因为
// 读到它指向 exe 就把整个 KuGou8 删了（过度修复）；正确做法就是这个守卫。
func TestKugou8LibcefIfDir(t *testing.T) {
	t.Run("目录且有 libcef.dll → 返回该路径", func(t *testing.T) {
		base := t.TempDir()
		verDir := filepath.Join(base, "20.1.22.27795")
		if err := os.MkdirAll(verDir, 0o755); err != nil {
			t.Fatal(err)
		}
		lc := filepath.Join(verDir, "libcef.dll")
		if err := os.WriteFile(lc, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := kugou8LibcefIfDir(verDir); got != lc {
			t.Errorf("应返回 %q，实得 %q", lc, got)
		}
	})

	t.Run("目录但无 libcef.dll → 空", func(t *testing.T) {
		if got := kugou8LibcefIfDir(t.TempDir()); got != "" {
			t.Errorf("目录下无 libcef.dll 应返回空，实得 %q", got)
		}
	})

	t.Run("指向文件（KuGou8=KuGou.exe 的已知情况）→ 空", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "KuGou.exe")
		if err := os.WriteFile(f, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := kugou8LibcefIfDir(f); got != "" {
			t.Errorf("KuGou8 指向文件时应返回空（否则回落逻辑不触发），实得 %q", got)
		}
	})

	t.Run("空串 / 不存在的路径 → 空", func(t *testing.T) {
		if got := kugou8LibcefIfDir(""); got != "" {
			t.Errorf("空串应返回空，实得 %q", got)
		}
		if got := kugou8LibcefIfDir(filepath.Join(t.TempDir(), "nope")); got != "" {
			t.Errorf("不存在的路径应返回空，实得 %q", got)
		}
	})
}

// mkVersionDir 在 base 下造一个 <name> 版本目录并放一个假的 libcef.dll。
func mkVersionDir(t *testing.T, base, name string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libcef.dll"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFindLibcefForExe 钉死版本目录选择逻辑。
//
// 现实动机：一台机器上 KGMusic 下同时有 10.1.94.25498 与 20.1.22.27795 两个版本目录
// （升级残留），而 KuGou.exe 是 20.1.22.27795。旧实现按 os.ReadDir 字典序取第一个含
// libcef.dll 的目录，选中 10.x —— 拿 20.x 的偏移表打 10.x 的 DLL。必须按 exe 版本选中
// 酷狗实际加载的那一份。
func TestFindLibcefForExe(t *testing.T) {
	t.Run("精确匹配 exe 版本目录（即便旧版排字典序更前）", func(t *testing.T) {
		base := t.TempDir()
		mkVersionDir(t, base, "10.1.94.25498") // 旧版残留，字典序在前
		mkVersionDir(t, base, "20.1.22.27795") // exe 实际版本
		got := findLibcefForExe(base, "20.1.22.27795")
		want := filepath.Join(base, "20.1.22.27795", "libcef.dll")
		if got != want {
			t.Errorf("应选中 exe 版本目录 %q，实得 %q", want, got)
		}
	})

	t.Run("exe 版本读取失败时兜底取版本号最大（不取字典序第一/旧版）", func(t *testing.T) {
		base := t.TempDir()
		mkVersionDir(t, base, "10.1.94.25498")
		mkVersionDir(t, base, "20.1.22.27795")
		got := findLibcefForExe(base, "") // ver 空模拟 readExeVersion 失败
		want := filepath.Join(base, "20.1.22.27795", "libcef.dll")
		if got != want {
			t.Errorf("兜底应取版本号最大的 %q，实得 %q", want, got)
		}
	})

	t.Run("exe 版本目录不存在时兜底最大", func(t *testing.T) {
		base := t.TempDir()
		mkVersionDir(t, base, "10.1.94.25498")
		mkVersionDir(t, base, "20.1.22.27795")
		got := findLibcefForExe(base, "99.9.9.9") // 无同名目录
		want := filepath.Join(base, "20.1.22.27795", "libcef.dll")
		if got != want {
			t.Errorf("无匹配目录时兜底取最大 %q，实得 %q", want, got)
		}
	})

	t.Run("无任何含 libcef.dll 的目录 → 空", func(t *testing.T) {
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "common"), 0o755); err != nil { // 无 libcef.dll
			t.Fatal(err)
		}
		if got := findLibcefForExe(base, "20.1.22.27795"); got != "" {
			t.Errorf("无 libcef.dll 目录应返回空，实得 %q", got)
		}
	})
}

// TestFindKuGouInstallRealMachine 在真机上端到端验证 FindKuGouInstall 选中的是酷狗
// 实际加载的版本目录（CI 无此安装且编译不了本包，Skip）。
//
// 本机 KuGou.exe 是 20.1.22.27795，旁边残留 10.1.94.25498 —— 旧实现会误选后者。
func TestFindKuGouInstallRealMachine(t *testing.T) {
	if _, err := os.Stat(`C:\Program Files\KuGou\KGMusic\KuGou.exe`); err != nil {
		t.Skip("未找到真机酷狗安装，跳过")
	}
	exePath, libcefPath, err := FindKuGouInstall()
	if err != nil {
		t.Fatalf("FindKuGouInstall 失败: %v", err)
	}
	ver := readExeVersion(exePath)
	if ver == "" {
		t.Skip("readExeVersion 返回空，无法断言版本匹配")
	}
	// libcef.dll 的父目录名必须等于 exe 版本 —— 即选中了酷狗实际加载的那一份
	gotDir := filepath.Base(filepath.Dir(libcefPath))
	if gotDir != ver {
		t.Errorf("选中的 libcef.dll 版本目录 %q 与 KuGou.exe 版本 %q 不符（可能又选了旧版残留目录）", gotDir, ver)
	}
	if !strings.HasSuffix(strings.ToLower(libcefPath), "libcef.dll") {
		t.Errorf("libcefPath 不是 libcef.dll: %q", libcefPath)
	}
	t.Logf("FindKuGouInstall → exe=%s (ver %s), libcef=%s", exePath, ver, libcefPath)
}
