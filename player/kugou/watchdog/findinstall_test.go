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
	ver := effectiveKuGouVersion(readExeVersion(exePath), exePath, libcefPath)
	if ver == "" {
		t.Skip("readExeVersion 返回空，无法断言版本匹配")
	}
	// libcef.dll 的父目录名必须等于有效版本 —— 即选中了酷狗实际加载的那一份
	gotDir := filepath.Base(filepath.Dir(libcefPath))
	if gotDir != ver {
		t.Errorf("选中的 libcef.dll 版本目录 %q 与酷狗有效版本 %q 不符（可能又选了旧版残留目录）", gotDir, ver)
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

// notRunning 是 runningKuGouFn 在没有任何酷狗进程运行时返回的错误形状（同时满足 ErrConfirmNotReady 与 ErrNotRunning）。
var notRunning = errors.Join(ErrConfirmNotReady, ErrNotRunning)

// TestFindLibcefPrefersRunningChildDir 钉死「以运行中的进程为准」：酷狗在运行时，exe 取主进程的映像（支持 KuGou.exe
// 与 HD_KuGou.exe），libcef.dll 只取其唯一的 CEF 子进程目录下的那份，注册表指向别处也不用。
func TestFindLibcefPrefersRunningChildDir(t *testing.T) {
	setup := func(t *testing.T) (a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795", "20.1.61.28014")
		return a, b
	}
	reg := func(a, kugou8 string) map[string]string {
		return map[string]string{"AppPath": a, "KuGou8": kugou8}
	}
	runningB := func(b, exeName string, childDirs ...string) runningKuGou {
		return runningKuGou{PID: 100, ExePath: filepath.Join(b, exeName), ChildDirs: childDirs}
	}

	t.Run("子进程目录有 libcef.dll、注册表指向另一份安装 → 选运行中的那份", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, TargetProcessName, filepath.Join(b, "20.1.22.27795")), nil, reg(a, filepath.Join(a, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, TargetProcessName), filepath.Join(b, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("HD_KuGou.exe 主进程 + 唯一版本子目录 → 选运行中的那份", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, runningB(b, HDTargetProcessName, filepath.Join(b, "20.1.61.28014")), nil, reg(a, filepath.Join(a, "20.1.22.27795")))
		wantInstall(t, filepath.Join(b, HDTargetProcessName), filepath.Join(b, "20.1.61.28014", "libcef.dll"))
	})

	t.Run("注册表与写死路径都没有（任意路径安装）→ 全取自运行中的进程", func(t *testing.T) {
		_, b := setup(t)
		stubLocate(t, runningB(b, HDTargetProcessName, filepath.Join(b, "20.1.61.28014")), nil, nil)
		wantInstall(t, filepath.Join(b, HDTargetProcessName), filepath.Join(b, "20.1.61.28014", "libcef.dll"))
	})
}

// TestFindInstallRunningReadiness 钉死运行态与冷态严格分离（设计 §5.3）：酷狗在运行时绝不回退注册表 KuGou8 或按版本
// 目录猜测，避免启动器交接或 CEF 子进程尚未就绪时误选残留旧目录、反复弹 UAC 或提前结束启动器。
//
// 变异自证（实测变红）：resolveRunningInstall 在 len(ChildDirs) == 0 时退回 libcefFromKuGou8() →
// KuGou.exe 与 HD_KuGou.exe 两条「运行态尚无 CEF 子进程」子用例均红。
func TestFindInstallRunningReadiness(t *testing.T) {
	setup := func(t *testing.T) (a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795", "20.1.61.28014")
		return a, b
	}
	regBoth := func(a, b string) map[string]string {
		return map[string]string{"AppPath": a, "KuGou8": filepath.Join(b, "20.1.22.27795")}
	}

	for _, exeName := range []string{TargetProcessName, HDTargetProcessName} {
		t.Run("运行态尚无 CEF 子进程（"+exeName+"）→ 零回退，返回 ErrRunningNotReady", func(t *testing.T) {
			a, b := setup(t)
			run := runningKuGou{PID: 25940, ExePath: filepath.Join(b, exeName)}
			stubLocate(t, run, nil, regBoth(a, b), a, b)

			for _, fn := range []struct {
				name string
				call func() (string, string, error)
			}{
				{"FindKuGouInstall", FindKuGouInstall},
				{"locateRunningInstall", locateRunningInstall},
			} {
				exe, lc, err := fn.call()
				if !errors.Is(err, ErrRunningNotReady) || exe != "" || lc != "" {
					t.Fatalf("%s = (%q, %q, %v)，应返回空路径与 ErrRunningNotReady", fn.name, exe, lc, err)
				}
			}
		})
	}

	t.Run("运行态 CEF 子进程分处两个目录 → 零回退，返回 ErrVersionUnsupported", func(t *testing.T) {
		a, b := setup(t)
		run := runningKuGou{
			PID:       25940,
			ExePath:   filepath.Join(b, HDTargetProcessName),
			ChildDirs: []string{filepath.Join(b, "20.1.22.27795"), filepath.Join(b, "20.1.61.28014")},
		}
		stubLocate(t, run, nil, regBoth(a, b), a, b)

		for _, fn := range []struct {
			name string
			call func() (string, string, error)
		}{
			{"FindKuGouInstall", FindKuGouInstall},
			{"locateRunningInstall", locateRunningInstall},
		} {
			exe, lc, err := fn.call()
			if !errors.Is(err, ErrVersionUnsupported) || exe != "" || lc != "" {
				t.Fatalf("%s = (%q, %q, %v)，应返回空路径与 ErrVersionUnsupported", fn.name, exe, lc, err)
			}
		}
	})

	t.Run("运行态唯一子进程目录缺少普通文件 libcef.dll → 零回退，返回 ErrInstallNotFound", func(t *testing.T) {
		a, b := setup(t)
		emptyDir := filepath.Join(b, "21.0.0.1")
		if err := os.MkdirAll(emptyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dirAsFile := filepath.Join(b, "21.0.0.2")
		if err := os.MkdirAll(filepath.Join(dirAsFile, "libcef.dll"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, childDir := range []string{emptyDir, dirAsFile, filepath.Join(b, "gone")} {
			run := runningKuGou{PID: 25940, ExePath: filepath.Join(b, HDTargetProcessName), ChildDirs: []string{childDir}}
			stubLocate(t, run, nil, regBoth(a, b), a, b)
			for _, fn := range []struct {
				name string
				call func() (string, string, error)
			}{
				{"FindKuGouInstall", FindKuGouInstall},
				{"locateRunningInstall", locateRunningInstall},
			} {
				exe, lc, err := fn.call()
				if !errors.Is(err, ErrInstallNotFound) || exe != "" || lc != "" {
					t.Fatalf("%s(childDir=%q) = (%q, %q, %v)，应返回空路径与 ErrInstallNotFound", fn.name, childDir, exe, lc, err)
				}
			}
		}
	})

	t.Run("运行态定位报错（非 ErrNotRunning：多主进程/交接中/路径查询失败）→ 绝不退回冷态，返回 ErrRunningNotReady", func(t *testing.T) {
		a, b := setup(t)
		garbage := runningKuGou{PID: 100, ExePath: filepath.Join(b, TargetProcessName), ChildDirs: []string{filepath.Join(b, "20.1.22.27795")}}
		for _, rerr := range []error{
			errors.Join(ErrConfirmNotReady, errors.New("2 KuGou.exe main processes are running")),
			errors.Join(ErrConfirmNotReady, errors.New("KuGou launcher is handing off to HD_KuGou.exe")),
			errors.Join(ErrConfirmNotReady, errors.New("access denied")),
		} {
			stubLocate(t, garbage, rerr, map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")}, a)
			exe, lc, err := FindKuGouInstall()
			if !errors.Is(err, ErrRunningNotReady) || exe != "" || lc != "" {
				t.Fatalf("rerr=%v 时 FindKuGouInstall = (%q, %q, %v)，应返回空路径与 ErrRunningNotReady", rerr, exe, lc, err)
			}
		}
	})
}

// TestFindLibcefFallsBackWhenNotRunning 钉死仅在确知没有任何酷狗进程运行（ErrNotRunning）时进入冷态预选：
// KuGou.exe 取注册表 AppPath、再取写死的安装根；libcef.dll 取注册表 KuGou8（须经 underDir 守卫，同属该安装目录）、
// 不可用时再按版本推导。
func TestFindLibcefFallsBackWhenNotRunning(t *testing.T) {
	t.Run("注册表 AppPath 与同目录 KuGou8 → 用注册表", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a, "10.1.94.25498", "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"))
	})

	t.Run("冷态 KuGou8 与 exe 目录仅差大小写 → 仍算在其下，照用", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a, "20.1.22.27795", "20.1.23.1")
		upper := filepath.Join(strings.ToUpper(a), "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": upper})
		wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(upper, "libcef.dll"))
	})

	t.Run("反例：冷态 KuGou8 不在 AppPath 之下（另一安装或兄弟前缀目录 A2）→ underDir 拒绝，退回按版本推导", func(t *testing.T) {
		root := t.TempDir()
		a, d, a2 := filepath.Join(root, "A"), filepath.Join(root, "D"), filepath.Join(root, "A2")
		mkInstall(t, a, "20.1.23.1")
		mkVersionDir(t, d, "20.1.22.27795")
		mkInstall(t, a2, "20.1.22.27795")
		for _, other := range []string{filepath.Join(d, "20.1.22.27795"), filepath.Join(a2, "20.1.22.27795")} {
			stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": other})
			wantInstall(t, filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.23.1", "libcef.dll"))
		}
	})

	t.Run("KuGou8 指向 KuGou.exe（文件）→ 按版本推导，取最大而非旧版残留", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "A")
		mkInstall(t, a, "10.1.94.25498", "20.1.22.27795")
		stubLocate(t, runningKuGou{}, notRunning, map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "KuGou.exe")})
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

// TestEffectiveKuGouVersion 钉死冷态外层启动器壳（如 8.9.8.9）的版本核准规则，并确保 10.x 黑名单不被绕过。
func TestEffectiveKuGouVersion(t *testing.T) {
	const (
		exe   = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
		lc20  = `C:\Program Files\KuGou\KGMusic\20.1.61.28014\libcef.dll`
		lc10  = `C:\Program Files\KuGou\KGMusic\10.1.94.25498\libcef.dll`
		lcOut = `D:\Other\20.1.61.28014\libcef.dll`
		lcBad = `C:\Program Files\KuGou\KGMusic\backup\libcef.dll`
	)
	cases := []struct {
		name   string
		exeVer string
		exe    string
		lc     string
		want   string
	}{
		{"8.9.8.9 外层启动器壳 + 同目录 20.1.61.28014 → 采用子目录版本", "8.9.8.9", exe, lc20, "20.1.61.28014"},
		{"大小写不同的同目录 8.9.8.9 + 20.x → 采用子目录版本", "8.9.8.9", exe, `c:\program files\kugou\kgmusic\20.1.61.28014\libcef.dll`, "20.1.61.28014"},
		{"10.x 黑名单绝不被 20.x 目录覆盖", "10.1.94.25498", exe, lc20, "10.1.94.25498"},
		{"20.x 主程序版本保持自身版本", "20.1.22.27795", exe, lc20, "20.1.22.27795"},
		{"8.x 启动器 + 10.x 目录（首段 < 20）→ 不覆盖", "8.9.8.9", exe, lc10, "8.9.8.9"},
		{"8.x 启动器 + 异目录 20.x → 不覆盖", "8.9.8.9", exe, lcOut, "8.9.8.9"},
		{"8.x 启动器 + 非版本号目录 → 不覆盖", "8.9.8.9", exe, lcBad, "8.9.8.9"},
		{"读不出 exe 版本（空串）→ 回落子目录名", "", exe, lc20, "20.1.61.28014"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveKuGouVersion(c.exeVer, c.exe, c.lc)
			if got != c.want {
				t.Fatalf("effectiveKuGouVersion(%q, %q, %q) = %q，应为 %q", c.exeVer, c.exe, c.lc, got, c.want)
			}
		})
	}
	if !isUnsupportedMajor(effectiveKuGouVersion("10.1.94.25498", exe, lc20)) {
		t.Fatal("10.x 经过 effectiveKuGouVersion 后仍必须命中 isUnsupportedMajor 黑名单")
	}

	// 顺带核对 findLibcefForExe 在 8.9.8.9 启动器壳下能选到 20.1.61.28014\libcef.dll。
	base := t.TempDir()
	mkVersionDir(t, base, "10.1.94.25498")
	mkVersionDir(t, base, "20.1.61.28014")
	want := filepath.Join(base, "20.1.61.28014", "libcef.dll")
	if got := findLibcefForExe(base, "8.9.8.9"); got != want {
		t.Fatalf("findLibcefForExe(%q, \"8.9.8.9\") = %q，应为 %q", base, got, want)
	}
}
