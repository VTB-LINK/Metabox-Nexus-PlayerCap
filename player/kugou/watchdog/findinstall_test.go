package watchdog

// 本文件只测找安装的选址：KuGou.exe 与它加载的 libcef.dll。libcef.dll 必须选中酷狗实际加载的那个版本目录，
// 而不是升级残留的旧版本目录（拿错版本的偏移表去打 = 写坏 DLL）。libcef.dll 含三级——运行中主进程的
// 唯一子进程目录、注册表 KuGou8（守卫）、exe 版本推导兜底；KuGou.exe 含三级——运行中主进程的映像、
// 注册表 AppPath、写死的安装根。

import (
	"errors"
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

// TestFindLibcefForExeComparesVersionsNumerically 钉死兜底取「版本号最大」按数值比较：按点分段逐段比数值，
// 段数不同时缺段按 0；有非纯数字段（或空段）的目录不是版本目录，跳过、不参与比较。与 exe 版本同名的目录
// 仍然优先。按字符串比较时 20.1.9.1 排在 20.1.22.27795 之后、backup 排在所有数字开头的目录之后，会选中旧版
// 或非版本目录。
//
// 变异自证（均实测变红）：改回字符串比较；段数不同时短的算大；不跳过非版本目录；去掉精确匹配优先。
func TestFindLibcefForExeComparesVersionsNumerically(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		ver  string
		want string // 空串表示应返回空
	}{
		{"20.1.9.1 与 20.1.22.27795 → 后者（第三段 9 < 22）", []string{"20.1.9.1", "20.1.22.27795"}, "", "20.1.22.27795"},
		{"20.1.22 与 20.1.22.1 → 后者（缺段按 0）", []string{"20.1.22", "20.1.22.1"}, "", "20.1.22.1"},
		{"2.0.0.0 与 10.0.0.0 → 后者（首段 2 < 10）", []string{"2.0.0.0", "10.0.0.0"}, "", "10.0.0.0"},
		{"反例：backup、20.1.x、20..1 不是版本目录 → 跳过", []string{"backup", "20.1.x", "20..1", "20.1.22.27795"}, "", "20.1.22.27795"},
		{"反例：只有非版本目录 → 空", []string{"backup", "20.1.x"}, "", ""},
		{"exe 版本目录存在 → 精确匹配优先，不取更大的", []string{"20.1.9.1", "20.1.22.27795"}, "20.1.9.1", "20.1.9.1"},
		{"exe 版本目录不存在 → 按数值取最大", []string{"20.1.9.1", "20.1.22.27795", "backup"}, "20.1.30.0", "20.1.22.27795"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			for _, d := range c.dirs {
				mkVersionDir(t, base, d)
			}
			want := ""
			if c.want != "" {
				want = filepath.Join(base, c.want, "libcef.dll")
			}
			if got := findLibcefForExe(base, c.ver); got != want {
				t.Fatalf("findLibcefForExe(%v, %q) = %q，应为 %q", c.dirs, c.ver, got, want)
			}
		})
	}
}

// TestFindKuGouInstallRealMachine 在真机上端到端验证 FindKuGouInstall 选中的是酷狗
// 实际加载的版本目录（没装酷狗的机器上 Skip）。
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

// stubLocate 替换找安装的三个来源：运行中的酷狗（run、runErr）、注册表 HKCU\Software\KuGou 的值（reg）、
// 写死的安装根（roots），用例结束时还原。经它的用例不查询本机的进程与注册表，也不看本机的安装目录。
func stubLocate(t *testing.T, run runningKuGou, runErr error, reg map[string]string, roots ...string) {
	t.Helper()
	oldRun, oldReg, oldRoots := runningKuGouFn, regKuGouFn, kugouInstallRoots
	t.Cleanup(func() { runningKuGouFn, regKuGouFn, kugouInstallRoots = oldRun, oldReg, oldRoots })
	runningKuGouFn = func() (runningKuGou, error) { return run, runErr }
	regKuGouFn = func(name string) string { return reg[name] }
	kugouInstallRoots = roots
}

// mkInstall 在 base 下造一份安装：KuGou.exe（假文件，读不出版本）与各版本目录下的假 libcef.dll。
func mkInstall(t *testing.T, base string, versions ...string) {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "KuGou.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		mkVersionDir(t, base, v)
	}
}

// wantInstall 断言 FindKuGouInstall 成功且选中 (exe, libcef)。
func wantInstall(t *testing.T, exe, libcef string) {
	t.Helper()
	gotExe, gotLibcef, err := FindKuGouInstall()
	if err != nil {
		t.Fatalf("FindKuGouInstall 失败: %v", err)
	}
	if gotExe != exe || gotLibcef != libcef {
		t.Fatalf("FindKuGouInstall = (%q, %q)，应为 (%q, %q)", gotExe, gotLibcef, exe, libcef)
	}
}

// notRunning 是 runningKuGouFn 在酷狗没在运行时返回的错误形状。
var notRunning = errors.Join(ErrConfirmNotReady, errors.New("KuGou.exe main process not found"))

// TestFindLibcefPrefersRunningChildDir 钉死「以运行中的进程为准」：酷狗在运行时，KuGou.exe 取主进程的映像，
// libcef.dll 取它唯一的 CEF 子进程目录下的那份，注册表指向别处也不用（注册表可能是旧数据，或指向另一份安装）。
// 子进程目录不可用（没有、不止一个、其下没有 libcef.dll）时退回现行顺序，不放宽任何一级的守卫；此时 KuGou8
// 须在运行中 KuGou.exe 的目录之下才用（Clean 后逐段比较、不分大小写），否则在该目录下按版本推导，免得 exe 与
// libcef.dll 分属两份安装。
//
// 运行中的安装 B 有 20.1.22.27795 与 20.1.23.1 两个版本目录（假 KuGou.exe 读不出版本，按版本推导取最大即
// 20.1.23.1），注册表指向的安装 A 有 20.1.22.27795：三级各自选出的路径互不相同，用例能分辨用的是哪一级。
//
// 变异自证（均实测变红）：libcef.dll 先看 KuGou8、再看子进程目录；子进程目录不止一个时取第一个；
// 子进程目录不检查 libcef.dll 是否存在；KuGou.exe 先看 AppPath、再看运行中的映像；去掉 KuGou8 须在 exe 目录
// 之下的限制；判「在其下」时区分大小写；判「在其下」时只比字符串前缀、不看路径分隔符。
func TestFindLibcefPrefersRunningChildDir(t *testing.T) {
	setup := func(t *testing.T) (a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795", "20.1.23.1")
		return a, b
	}
	// reg 构造注册表：AppPath 指向 A，KuGou8 指向 kugou8。
	reg := func(a, kugou8 string) map[string]string {
		return map[string]string{"AppPath": a, "KuGou8": kugou8}
	}
	runningB := func(b string, childDirs ...string) runningKuGou {
		return runningKuGou{PID: 100, ExePath: filepath.Join(b, "KuGou.exe"), ChildDirs: childDirs}
	}
	inB8 := func(b string) string { return filepath.Join(b, "20.1.22.27795") }               // KuGou8 指向 B 下的版本目录
	derivedB := func(b string) string { return filepath.Join(b, "20.1.23.1", "libcef.dll") } // B 下按版本推导的结果

	t.Run("子进程目录有 libcef.dll、注册表指向另一份安装 → 选运行中的那份", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, filepath.Join(b, "20.1.22.27795")), nil, reg(a, filepath.Join(a, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(b, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("注册表与写死路径都没有（任意路径安装）→ 全取自运行中的进程", func(t *testing.T) {
		_, b := setup(t)
		stubLocate(t, runningB(b, filepath.Join(b, "20.1.22.27795")), nil, nil)
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(b, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("反例：子进程目录下没有 libcef.dll → 退回注册表 KuGou8（在酷狗目录下）", func(t *testing.T) {
		a, b := setup(t)
		empty := filepath.Join(b, "21.0.0.1")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			t.Fatal(err)
		}
		stubLocate(t, runningB(b, empty), nil, reg(a, inB8(b)))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(inB8(b), "libcef.dll"))
	})

	t.Run("反例：子进程目录不存在 → 退回注册表 KuGou8（在酷狗目录下）", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, filepath.Join(b, "gone")), nil, reg(a, inB8(b)))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(inB8(b), "libcef.dll"))
	})

	t.Run("反例：子进程分处两个目录 → 不取其一，退回注册表 KuGou8（在酷狗目录下）", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, filepath.Join(b, "20.1.23.1"), filepath.Join(b, "20.1.22.27795")), nil, reg(a, inB8(b)))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(inB8(b), "libcef.dll"))
	})

	t.Run("尚无子进程、KuGou8 指向酷狗目录下的版本目录 → 照用", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b), nil, reg(a, inB8(b)))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(inB8(b), "libcef.dll"))
	})

	t.Run("尚无子进程、KuGou8 与酷狗目录只差大小写 → 仍算在其下，照用", func(t *testing.T) {
		a, b := setup(t)
		upper := filepath.Join(strings.ToUpper(b), "20.1.22.27795")
		stubLocate(t, runningB(b), nil, reg(a, upper))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), filepath.Join(upper, "libcef.dll"))
	})

	t.Run("反例：尚无子进程、KuGou8 指向另一份安装 → 不用，在酷狗目录下按版本推导", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b), nil, reg(a, filepath.Join(a, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), derivedB(b))
	})

	t.Run("反例：尚无子进程、KuGou8 在名字以酷狗目录为前缀的兄弟目录（B 与 B2）→ 不算在其下", func(t *testing.T) {
		a, b := setup(t)
		b2 := b + "2"
		mkInstall(t, b2, "20.1.22.27795")
		stubLocate(t, runningB(b), nil, reg(a, filepath.Join(b2, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), derivedB(b))
	})

	t.Run("反例：子进程目录不可用、KuGou8 指向另一份安装 → 不用，在酷狗目录下按版本推导", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, filepath.Join(b, "gone")), nil, reg(a, filepath.Join(a, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, "KuGou.exe"), derivedB(b))
	})

	t.Run("尚无子进程、注册表也没有 → 在运行中映像的目录下按版本推导", func(t *testing.T) {
		_, b := setup(t)
		mkVersionDir(t, b, "10.1.94.25498") // 旧版残留：推导取最大，不取它
		stubLocate(t, runningB(b), nil, nil)
		wantInstall(t, filepath.Join(b, "KuGou.exe"), derivedB(b))
	})
}

// TestFindLibcefFallsBackWhenNotRunning 钉死酷狗不在运行（或运行中的主进程定位不了）时沿用现行顺序：
// KuGou.exe 取注册表 AppPath、再取写死的安装根；libcef.dll 取注册表 KuGou8（守卫）、再按版本推导。
// 结果与 TestKugou8LibcefIfDir、TestFindLibcefForExe 的现行用例一致。runningKuGouFn 报错时不用它一并
// 返回的任何内容。
//
// 变异自证（均实测变红）：runningKuGouFn 报错时仍用其 ExePath 或 ChildDirs；AppPath 不检查其下有无 KuGou.exe；
// 都找不到时返回空路径而不是 ErrInstallNotFound；exe 不是取自进程时也要求 KuGou8 在 exe 目录之下。
func TestFindLibcefFallsBackWhenNotRunning(t *testing.T) {
	t.Run("注册表 AppPath 与 KuGou8 → 用注册表", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a, "10.1.94.25498", "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("KuGou8 不在 AppPath 之下 → 照用（exe 不是取自运行中的进程，不加「须在 exe 目录之下」的限制）", func(t *testing.T) {
		root := t.TempDir()
		a, d := filepath.Join(root, "A"), filepath.Join(root, "D")
		mkInstall(t, a, "20.1.23.1")
		mkVersionDir(t, d, "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": filepath.Join(d, "20.1.22.27795")})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(d, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("KuGou8 指向 KuGou.exe（文件）→ 按版本推导，取最大而非旧版残留", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a, "10.1.94.25498", "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "KuGou.exe")})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("反例：运行中的主进程定位不了（报错）→ 不用它一并返回的路径", func(t *testing.T) {
		root := t.TempDir()
		a, b := filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795")
		garbage := runningKuGou{PID: 100, ExePath: filepath.Join(b, "KuGou.exe"), ChildDirs: []string{filepath.Join(b, "20.1.22.27795")}}
		stubLocate(t, garbage, errors.Join(ErrConfirmNotReady, errors.New("2 KuGou.exe main processes are running")),
			map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("反例：AppPath 下没有 KuGou.exe → 取写死的安装根", func(t *testing.T) {
		root := t.TempDir()
		stale, missing, c := filepath.Join(root, "stale"), filepath.Join(root, "missing"), filepath.Join(root, "C")
		if err := os.MkdirAll(stale, 0o755); err != nil {
			t.Fatal(err)
		}
		mkInstall(t, c, "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": stale}, missing, c)
		wantInstall(t, filepath.Join(c, "KuGou.exe"), filepath.Join(c, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("反例：哪里都没有 KuGou.exe → ErrInstallNotFound", func(t *testing.T) {
		root := t.TempDir()
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": root, "KuGou8": root}, filepath.Join(root, "missing"))
		exe, lc, err := FindKuGouInstall()
		if !errors.Is(err, ErrInstallNotFound) || exe != "" || lc != "" {
			t.Fatalf("FindKuGouInstall = (%q, %q, %v)，应为 ErrInstallNotFound", exe, lc, err)
		}
	})

	t.Run("反例：有 KuGou.exe 而哪里都没有 libcef.dll → ErrInstallNotFound", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a)
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a})
		exe, lc, err := FindKuGouInstall()
		if !errors.Is(err, ErrInstallNotFound) || exe != "" || lc != "" {
			t.Fatalf("FindKuGouInstall = (%q, %q, %v)，应为 ErrInstallNotFound", exe, lc, err)
		}
	})
}
