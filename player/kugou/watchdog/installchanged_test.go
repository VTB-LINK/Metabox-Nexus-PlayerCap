package watchdog

// 本文件只测 killAndPatch：酷狗启动之后、结束它之前，重新定位安装并与修补前预选的 exe、libcef.dll 比较，
// 不同或定位出错就不结束酷狗、不触发 helper。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/launch"
)

// TestKillAndPatchRechecksInstall 钉死「先确认、再结束」：EnsurePatched 在酷狗没运行时按注册表预选安装、弹 UAC，
// 等到的只是「任意 KuGou.exe 出现」。主播打开的若是另一份安装，照旧结束它、给预选那份打补丁、再启动预选那份，
// 就把主播的酷狗换掉了。路径比较 Clean 后不分大小写。kill 与 trigger 一律是桩，不结束任何进程。
//
// 变异自证（均实测变红）：去掉比较；只比 exe 不比 libcef.dll；重新定位出错时照旧比较、修补；路径比较区分大小写；
// 先结束酷狗、后确认；重新定位的错误按 %w 包装（会被认作 ErrInstallNotFound）。
func TestKillAndPatchRechecksInstall(t *testing.T) {
	const (
		exe = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
		lc  = `C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`
	)
	locateErr := errors.Join(ErrInstallNotFound, errors.New("stub"))
	cases := []struct {
		name               string
		gotExe, gotLc      string
		locateErr          error
		triggerErr         error
		wantPatch          bool // 是否结束酷狗并触发 helper
		wantChanged        bool // 错误是否为 ErrInstallChanged
		wantErr            bool // 是否返回错误
		wantNotInstallMiss bool // 错误不得被认作 ErrInstallNotFound（否则 Start 按找不到安装分流）
	}{
		{name: "相同 → 结束并修补", gotExe: exe, gotLc: lc, wantPatch: true},
		{name: "只差大小写与未 Clean 的写法 → 视为相同", gotExe: `c:\program files\kugou\kgmusic\.\KUGOU.EXE`, gotLc: `C:\PROGRAM FILES\KuGou\KGMusic\20.1.22.27795\\libcef.dll`, wantPatch: true},
		{name: "反例：exe 不同（另一份安装）→ 不结束、不修补", gotExe: `D:\Apps\KuGou\KuGou.exe`, gotLc: lc, wantChanged: true, wantErr: true},
		{name: "反例：libcef.dll 不同（同一安装的另一版本目录）→ 不结束、不修补", gotExe: exe, gotLc: `C:\Program Files\KuGou\KGMusic\20.1.23.1\libcef.dll`, wantChanged: true, wantErr: true},
		{name: "反例：重新定位出错（一并返回的路径恰与预选相同）→ 不结束、不修补，且不被认作找不到安装", gotExe: exe, gotLc: lc, locateErr: locateErr, wantChanged: true, wantErr: true, wantNotInstallMiss: true},
		{name: "反例：重新定位出错、路径为空 → 不结束、不修补", locateErr: locateErr, wantChanged: true, wantErr: true, wantNotInstallMiss: true},
		{name: "相同而 helper 修补失败 → 已结束酷狗，返回修补错误", gotExe: exe, gotLc: lc, triggerErr: errors.New("ERROR: x"), wantPatch: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubGateRunning(t, nil) // 商店版门禁另见 storegate_test.go；这里不读本机的进程
			var calls []string
			locate := func() (string, string, error) {
				calls = append(calls, "locate")
				return c.gotExe, c.gotLc, c.locateErr
			}
			kill := func() { calls = append(calls, "kill") }
			trigger := func() error { calls = append(calls, "trigger"); return c.triggerErr }

			err := killAndPatch(exe, lc, locate, kill, trigger)

			want := []string{"locate"}
			if c.wantPatch {
				want = append(want, "kill", "trigger")
			}
			if len(calls) != len(want) {
				t.Fatalf("调用序列 = %v，应为 %v", calls, want)
			}
			for i := range want {
				if calls[i] != want[i] {
					t.Fatalf("调用序列 = %v，应为 %v", calls, want)
				}
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v，应%s出错", err, map[bool]string{true: "", false: "不"}[c.wantErr])
			}
			if got := errors.Is(err, ErrInstallChanged); got != c.wantChanged {
				t.Fatalf("errors.Is(err, ErrInstallChanged) = %v，应为 %v（err = %v）", got, c.wantChanged, err)
			}
			if c.wantNotInstallMiss && errors.Is(err, ErrInstallNotFound) {
				t.Fatalf("重新定位出错的错误不得被认作 ErrInstallNotFound：%v", err)
			}
		})
	}
}

// TestKillAndPatchLocatesByRunningProcess 钉死修补前重新确认安装须按进程定位成功：生产的 locate 是
// locateRunningInstall，进程来源出错时原样返回错误、不退回注册表。反例：冷启动按注册表预选 A，主播启动的是 B，
// 重新快照时 B 的某个子进程恰好退出、映像路径取不到；若退回注册表就又得到 A，比较通过，结束 B、修补并启动 A。
// 经 stubLocate 注入进程、注册表与安装根，不查询本机，kill 与 trigger 是桩。
//
// 变异自证（均实测变红）：locateRunningInstall 在进程来源出错时退回注册表（即改用 FindKuGouInstall）；
// 进程来源出错时返回 nil 错误。
func TestKillAndPatchLocatesByRunningProcess(t *testing.T) {
	setup := func(t *testing.T) (a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795")
		return a, b
	}
	regA := func(a string) map[string]string {
		return map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")}
	}
	running := func(dir string) runningKuGou {
		return runningKuGou{PID: 100, ExePath: filepath.Join(dir, "KuGou.exe"), ChildDirs: []string{filepath.Join(dir, "20.1.22.27795")}}
	}
	imageGone := errors.Join(ErrConfirmNotReady, errors.New("query the image path of KuGou.exe (PID 7001): access denied"))

	cases := []struct {
		name      string
		run       func(a, b string) runningKuGou
		runErr    error
		wantPatch bool
	}{
		{"进程来源给出与预选相同的安装 → 照常修补", func(a, _ string) runningKuGou { return running(a) }, nil, true},
		{"反例：进程来源给出另一份安装 B → 不结束、不修补", func(_, b string) runningKuGou { return running(b) }, nil, false},
		{"反例：进程来源出错、注册表指向预选的 A → 不退回注册表，不结束、不修补", func(_, b string) runningKuGou { return running(b) }, imageGone, false},
		{"反例：进程来源出错且没有返回内容 → 不结束、不修补", func(_, _ string) runningKuGou { return runningKuGou{} }, imageGone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := setup(t)
			stubLocate(t, c.run(a, b), c.runErr, regA(a))
			stubGateRunning(t, nil)
			var calls []string
			err := killAndPatch(filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"), locateRunningInstall,
				func() { calls = append(calls, "kill") },
				func() error { calls = append(calls, "trigger"); return nil })
			if c.wantPatch {
				if err != nil || len(calls) != 2 {
					t.Fatalf("err = %v，调用 = %v；应无错误并依次结束、触发", err, calls)
				}
				return
			}
			if !errors.Is(err, ErrInstallChanged) || len(calls) != 0 {
				t.Fatalf("err = %v，调用 = %v；应为 ErrInstallChanged 且不结束、不触发", err, calls)
			}
		})
	}

	t.Run("locateRunningInstall：进程来源出错时原样返回错误、路径为空", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, running(b), imageGone, regA(a))
		exe, lc, err := locateRunningInstall()
		if !errors.Is(err, ErrConfirmNotReady) || exe != "" || lc != "" {
			t.Fatalf("locateRunningInstall = (%q, %q, %v)，应为空路径与进程来源的错误", exe, lc, err)
		}
	})
}

// TestWaitUntilRunningReady 钉死冷态已提权 helper 在启动交接期的保活等待：
// 多拍瞬态收敛期间保持 helper 存活不触发 kill；等待中途或最后一拍 locate 成功同时 helper 退出或 stopCh 关闭时
// 必须拒绝放行；非瞬态错误立即返回不重试；超时原样返回最后一次错误交 killAndPatch 包装 ErrInstallChanged。
//
// 变异自证（实测变红）：waitUntilRunningReady 去掉 err == nil 返回前的 helperAlive() 复查 →
// 「最后一拍 locate 成功同时 helper 已退出」子用例红。
func TestWaitUntilRunningReady(t *testing.T) {
	t.Run("三阶段快照收敛后经 sameKuGouExe 一次完成 kill 与 trigger", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		hdExe := filepath.Join(dir, HDTargetProcessName)
		launchExe := filepath.Join(dir, TargetProcessName)
		verDir := filepath.Join(dir, "20.1.61.28014")
		lc := filepath.Join(verDir, "libcef.dll")

		stubGateRunning(t, nil)
		// 模拟 4 拍快照：空窗（ErrNotRunning+ErrConfirmNotReady）→ 交接中（ErrConfirmNotReady）→ 尚无 CEF 子进程（ErrRunningNotReady）→ 稳定 HD_KuGou.exe
		step := 0
		oldRun := runningKuGouFn
		t.Cleanup(func() { runningKuGouFn = oldRun })
		runningKuGouFn = func() (runningKuGou, error) {
			step++
			switch step {
			case 1:
				return runningKuGou{}, errors.Join(ErrConfirmNotReady, ErrNotRunning)
			case 2:
				return runningKuGou{}, errors.Join(ErrConfirmNotReady, errors.New("KuGou launcher is handing off to HD_KuGou.exe"))
			case 3:
				return runningKuGou{PID: 25940, ExePath: hdExe}, nil
			default:
				return runningKuGou{PID: 25940, ExePath: hdExe, ChildDirs: []string{verDir}}, nil
			}
		}

		stopCh := make(chan struct{})
		var calls []string
		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, time.Second, locateRunningInstall)
		}
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil })
		if err != nil {
			t.Fatalf("killAndPatch 失败: %v", err)
		}
		if step != 4 {
			t.Fatalf("快照轮询次数 = %d，应为 4", step)
		}
		if len(calls) != 2 || calls[0] != "kill" || calls[1] != "trigger" {
			t.Fatalf("calls = %v，应为 [kill trigger]", calls)
		}
	})

	t.Run("等待中途 helper 退出 → 立即返回同构错误且不调 kill/trigger", func(t *testing.T) {
		stopCh := make(chan struct{})
		attempts := 0
		alive := true
		exe, lc, err := waitUntilRunningReady(stopCh, func() bool { return alive }, time.Millisecond, time.Second, func() (string, string, error) {
			attempts++
			if attempts == 2 {
				alive = false
			}
			return "", "", errors.Join(ErrRunningNotReady, errors.New("no CEF child"))
		})
		if err == nil || exe != "" || lc != "" {
			t.Fatalf("got = (%q, %q, %v)，期望返回错误", exe, lc, err)
		}
		if attempts != 2 {
			t.Fatalf("helper 退出后应立即停止重试，实际调用 locate %d 次", attempts)
		}
		wantErr := readyToKill(kugouStartedNoHelper, func() bool { return false }).Error()
		if err.Error() != wantErr {
			t.Fatalf("err = %q，应与 readyToKill 同构 (%q)", err.Error(), wantErr)
		}
	})

	t.Run("最后一拍 locate 成功同时 helper 已退出 → 拒绝放行且 kill/trigger 零调用", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		hdExe := filepath.Join(dir, HDTargetProcessName)
		launchExe := filepath.Join(dir, TargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")
		stubGateRunning(t, nil)

		stopCh := make(chan struct{})
		alive := true
		attempts := 0
		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return alive }, time.Millisecond, time.Second, func() (string, string, error) {
				attempts++
				if attempts == 1 {
					return "", "", errors.Join(ErrRunningNotReady, errors.New("still starting"))
				}
				// 最后一拍 locate 返回成功的同时 helper 恰好退出
				alive = false
				return hdExe, lc, nil
			})
		}
		var calls []string
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil })
		if !errors.Is(err, ErrInstallChanged) || len(calls) != 0 {
			t.Fatalf("err = %v, calls = %v；应拒绝放行且 kill/trigger 零调用", err, calls)
		}
		// 直接测 waitUntilRunningReady 返回值与 readyToKill 同构
		alive2 := true
		_, _, werr := waitUntilRunningReady(stopCh, func() bool { return alive2 }, time.Millisecond, time.Second, func() (string, string, error) {
			alive2 = false
			return hdExe, lc, nil
		})
		wantErr := readyToKill(kugouStartedNoHelper, func() bool { return false }).Error()
		if werr == nil || werr.Error() != wantErr {
			t.Fatalf("最后一拍成功同时 helper 退出应返回 %q，实得 %v", wantErr, werr)
		}
	})

	t.Run("最后一拍 locate 成功同时 stopCh 已关闭 → 返回停止错误且不放行", func(t *testing.T) {
		stopCh := make(chan struct{})
		exe, lc, err := waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, time.Second, func() (string, string, error) {
			close(stopCh)
			return `C:\KG\KuGou.exe`, `C:\KG\20.1.61.28014\libcef.dll`, nil
		})
		if err == nil || exe != "" || lc != "" {
			t.Fatalf("got = (%q, %q, %v)，应返回停止错误", exe, lc, err)
		}
		wantStop := readyToKill(kugouWaitStopped, func() bool { return true }).Error()
		if err.Error() != wantStop {
			t.Fatalf("err = %q，应与 readyToKill 停止错误同构 (%q)", err.Error(), wantStop)
		}
	})

	t.Run("等待期间或开始前关闭 stopCh → 立即返回停止错误", func(t *testing.T) {
		closed := make(chan struct{})
		close(closed)
		called := 0
		_, _, err := waitUntilRunningReady(closed, func() bool { return true }, time.Hour, time.Hour, func() (string, string, error) {
			called++
			return "", "", ErrRunningNotReady
		})
		if err == nil || called != 0 {
			t.Fatalf("开始前 stopCh 已关应立即返回错误且不调 locate，实得 err=%v called=%d", err, called)
		}

		stopCh := make(chan struct{})
		start := time.Now()
		_, _, err = waitUntilRunningReady(stopCh, func() bool { return true }, time.Hour, time.Hour, func() (string, string, error) {
			close(stopCh)
			return "", "", ErrRunningNotReady
		})
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("轮间关闭 stopCh 应立即返回错误，实得 err=%v elapsed=%v", err, time.Since(start))
		}
	})

	t.Run("超时 → 原样返回最后一次瞬态错误且 kill/trigger 零调用", func(t *testing.T) {
		stubGateRunning(t, nil)
		lastErr := errors.Join(ErrRunningNotReady, errors.New("still no CEF child"))
		stopCh := make(chan struct{})
		exe, lc, err := waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, 5*time.Millisecond, func() (string, string, error) {
			return "", "", lastErr
		})
		if !errors.Is(err, ErrRunningNotReady) || exe != "" || lc != "" {
			t.Fatalf("超时应原样返回最后一次错误，实得 (%q, %q, %v)", exe, lc, err)
		}
		var calls []string
		kerr := killAndPatch(`C:\KG\KuGou.exe`, `C:\KG\20.1.61.28014\libcef.dll`, func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, 5*time.Millisecond, func() (string, string, error) {
				return "", "", lastErr
			})
		}, func() { calls = append(calls, "kill") }, func() error { calls = append(calls, "trigger"); return nil })
		if !errors.Is(kerr, ErrInstallChanged) || len(calls) != 0 {
			t.Fatalf("超时经 killAndPatch 应返回 ErrInstallChanged 且零调用，实得 err=%v calls=%v", kerr, calls)
		}
	})

	t.Run("非瞬态错误（如 ErrVersionUnsupported / ErrInstallNotFound）→ 立即原样返回不重试", func(t *testing.T) {
		for _, fatal := range []error{
			errors.Join(ErrVersionUnsupported, errors.New("two dirs")),
			errors.Join(ErrInstallNotFound, errors.New("missing libcef")),
		} {
			attempts := 0
			_, _, err := waitUntilRunningReady(make(chan struct{}), func() bool { return true }, time.Millisecond, time.Second, func() (string, string, error) {
				attempts++
				return "", "", fatal
			})
			if !errors.Is(err, fatal) || attempts != 1 {
				t.Fatalf("非瞬态错误 %v 应只调 1 次 locate 并原样返回，实得 attempts=%d err=%v", fatal, attempts, err)
			}
		}
	})
}

// TestKillAndPatchSameKuGouDirHDSwitch 钉死 sameKuGouExe 同安装目录判定：
// 冷态预选根目录 KuGou.exe、运行态落定为同目录 HD_KuGou.exe 且 libcef.dll 一致时放行；
// 跨目录或非白名单文件名仍拒杀并返回 ErrInstallChanged。
func TestKillAndPatchSameKuGouDirHDSwitch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "KGMusic")
	mkInstall(t, dir, "20.1.61.28014")
	launchExe := filepath.Join(dir, TargetProcessName)
	hdExe := filepath.Join(dir, HDTargetProcessName)
	lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")

	cases := []struct {
		name      string
		gotExe    string
		gotLc     string
		wantPatch bool
	}{
		{"同目录 KuGou.exe → HD_KuGou.exe 且 libcef 一致 → 放行", hdExe, lc, true},
		{"同目录大小写与未 Clean 差异下的 HD_KuGou.exe → 放行", filepath.Join(strings.ToUpper(dir), ".", "hd_kugou.EXE"), lc, true},
		{"反例：跨目录 D:\\KG\\HD_KuGou.exe → 拒杀返回 ErrInstallChanged", `D:\KG\HD_KuGou.exe`, lc, false},
		{"反例：版本子目录下的 KuGou.exe（目录不同）→ 拒杀返回 ErrInstallChanged", filepath.Join(dir, "20.1.61.28014", TargetProcessName), lc, false},
		{"反例：同目录非白名单进程名 KuGouConcept.exe → 拒杀返回 ErrInstallChanged", filepath.Join(dir, "KuGouConcept.exe"), lc, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubGateRunning(t, nil)
			var calls []string
			err := killAndPatch(launchExe, lc,
				func() (string, string, error) { return c.gotExe, c.gotLc, nil },
				func() { calls = append(calls, "kill") },
				func() error { calls = append(calls, "trigger"); return nil })
			if c.wantPatch {
				if err != nil || len(calls) != 2 || calls[0] != "kill" || calls[1] != "trigger" {
					t.Fatalf("err=%v calls=%v，期望无错且依次 kill、trigger", err, calls)
				}
				return
			}
			if !errors.Is(err, ErrInstallChanged) || len(calls) != 0 {
				t.Fatalf("err=%v calls=%v，期望 ErrInstallChanged 且 kill/trigger 零调用", err, calls)
			}
		})
	}
}

// TestResolveLaunchExeAndPrecheck 钉死重启启动器前置预检：
// HD_KuGou.exe 要求同目录存在普通文件 KuGou.exe 且通过 launch.Gate，绝不回退直启 HD_KuGou.exe；
// 缺失或位于 WindowsApps 时，在弹 UAC 之前与 killAndPatch 结束酷狗之前均返回错误、不杀进程、不弹 UAC；
// allPatched == true 时无需重启，不受缺 KuGou.exe 影响。
//
// 变异自证（实测变红）：resolveLaunchExe 在缺同目录 KuGou.exe 时退回返回 HD_KuGou.exe →
// 规则矩阵、killAndPatch 拦截与弹 UAC 前预检三条子用例均红。
func TestResolveLaunchExeAndPrecheck(t *testing.T) {
	t.Run("resolveLaunchExe 规则矩阵", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		hdExe := filepath.Join(dir, HDTargetProcessName)
		launcher := filepath.Join(dir, TargetProcessName)
		storeHD := `C:\Program Files\WindowsApps\KuGou.Music_20.1\HD_KuGou.exe`
		storeLauncher := `C:\Program Files\WindowsApps\KuGou.Music_20.1\KuGou.exe`

		// 1. 普通 KuGou.exe 过门禁原样返回
		got, err := resolveLaunchExe(launcher, func(string) bool { return false })
		if err != nil || got != launcher {
			t.Fatalf("KuGou.exe = (%q, %v)，应原样返回 %q", got, err, launcher)
		}
		// 2. WindowsApps 下的 KuGou.exe 被 launch.Gate 拦截
		var pe *launch.PackagedError
		if _, err := resolveLaunchExe(storeLauncher, func(string) bool { return true }); !errors.As(err, &pe) {
			t.Fatalf("WindowsApps KuGou.exe 应返回 *PackagedError，实得 %v", err)
		}
		// 3. HD_KuGou.exe 存在同目录普通文件 KuGou.exe → 返回 KuGou.exe
		got, err = resolveLaunchExe(hdExe, func(p string) bool { return samePath(p, launcher) })
		if err != nil || !samePath(got, launcher) {
			t.Fatalf("HD_KuGou.exe 有同目录启动器 = (%q, %v)，应返回 %q", got, err, launcher)
		}
		// 4. HD_KuGou.exe 缺少同目录普通文件 KuGou.exe → 返回 ErrInstallNotFound，绝不回退 HD_KuGou.exe
		got, err = resolveLaunchExe(hdExe, func(string) bool { return false })
		if !errors.Is(err, ErrInstallNotFound) || got != "" {
			t.Fatalf("HD_KuGou.exe 缺同目录启动器 = (%q, %v)，应返回 ErrInstallNotFound 且路径为空", got, err)
		}
		// 5. 同目录 KuGou.exe 是目录而非普通文件 → 真实 isRegularFileFn 返回 false → ErrInstallNotFound
		dirLauncher := filepath.Join(t.TempDir(), "KGDirLauncher")
		if err := os.MkdirAll(filepath.Join(dirLauncher, TargetProcessName), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err = resolveLaunchExe(filepath.Join(dirLauncher, HDTargetProcessName), isRegularFileFn)
		if !errors.Is(err, ErrInstallNotFound) || got != "" {
			t.Fatalf("同目录 KuGou.exe 为目录时 = (%q, %v)，应返回 ErrInstallNotFound", got, err)
		}
		// 6. WindowsApps 下的 HD_KuGou.exe → 先过 launch.Gate 返回 *PackagedError
		pe = nil
		if _, err := resolveLaunchExe(storeHD, func(string) bool { return false }); !errors.As(err, &pe) || !samePath(pe.ExePath, storeLauncher) {
			t.Fatalf("WindowsApps HD_KuGou.exe 应返回 *PackagedError(%s)，实得 %v", storeLauncher, err)
		}
	})

	t.Run("killAndPatch 在 kill 之前拦截缺启动器与 WindowsApps 启动器", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkVersionDir(t, dir, "20.1.61.28014")
		hdExe := filepath.Join(dir, HDTargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")
		stubGateRunning(t, nil)

		var calls []string
		err := killAndPatch(hdExe, lc,
			func() (string, string, error) { return hdExe, lc, nil },
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil })
		if !errors.Is(err, ErrInstallNotFound) || errors.Is(err, ErrInstallChanged) || len(calls) != 0 {
			t.Fatalf("缺同目录 KuGou.exe：err=%v calls=%v，应返回 ErrInstallNotFound（非 ErrInstallChanged）且零调用", err, calls)
		}

		storeHD := `C:\Program Files\WindowsApps\KuGou.Music_20.1\HD_KuGou.exe`
		storeLC := `C:\Program Files\WindowsApps\KuGou.Music_20.1\20.1.61.28014\libcef.dll`
		var pe *launch.PackagedError
		err = killAndPatch(storeHD, storeLC,
			func() (string, string, error) { return storeHD, storeLC, nil },
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil })
		if !errors.As(err, &pe) || len(calls) != 0 {
			t.Fatalf("WindowsApps 启动器：err=%v calls=%v，应返回 *PackagedError 且零调用", err, calls)
		}
	})

	t.Run("弹 UAC 前预检与 allPatched == true 放行（零真机进程、零 UAC）", func(t *testing.T) {
		stubEnv := func(t *testing.T, allPatched bool, withLauncher bool) {
			t.Helper()
			dir := filepath.Join(t.TempDir(), "KGMusic")
			mkVersionDir(t, dir, "20.1.61.28014")
			hdExe := filepath.Join(dir, HDTargetProcessName)
			if err := os.WriteFile(hdExe, []byte("MZ"), 0o644); err != nil {
				t.Fatal(err)
			}
			if withLauncher {
				if err := os.WriteFile(filepath.Join(dir, TargetProcessName), []byte("MZ"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			verDir := filepath.Join(dir, "20.1.61.28014")
			stubGateRunning(t, nil)
			stubLocate(t, runningKuGou{PID: 25940, ExePath: hdExe, ChildDirs: []string{verDir}}, nil, nil)

			oldCDP, oldCheck, oldSnap := isCDPAvailableFn, checkPatchStatusFn, snapshotProcsFn
			oldInstallLog, oldFPWarn := installLogged, fingerprintWarned
			t.Cleanup(func() {
				isCDPAvailableFn, checkPatchStatusFn, snapshotProcsFn = oldCDP, oldCheck, oldSnap
				installLogged, fingerprintWarned = oldInstallLog, oldFPWarn
			})
			isCDPAvailableFn = func() bool { return false }
			checkPatchStatusFn = func(string, bool) (bool, bool, error) { return allPatched, true, nil }
			snapshotProcsFn = func() ([]proc, error) {
				return []proc{{PID: 25940, PPID: 4, Name: HDTargetProcessName}}, nil
			}
		}

		t.Run("!allPatched 且缺同目录 KuGou.exe → 弹 UAC 前返回 ErrInstallNotFound", func(t *testing.T) {
			stubEnv(t, false, false)
			err := EnsurePatchedNoElevate(make(chan struct{}))
			if !errors.Is(err, ErrInstallNotFound) {
				t.Fatalf("EnsurePatchedNoElevate = %v，应在提权前返回 ErrInstallNotFound", err)
			}
		})

		t.Run("!allPatched 且同目录存在 KuGou.exe → 通过启动器预检到达提权门槛", func(t *testing.T) {
			stubEnv(t, false, true)
			err := EnsurePatchedNoElevate(make(chan struct{}))
			if !errors.Is(err, ErrElevationNeeded) {
				t.Fatalf("EnsurePatchedNoElevate = %v，应通过启动器预检后返回 ErrElevationNeeded", err)
			}
		})

		t.Run("allPatched == true 且缺同目录 KuGou.exe → 无需重启，照常放行", func(t *testing.T) {
			stubEnv(t, true, false)
			if err := EnsurePatchedNoElevate(make(chan struct{})); err != nil {
				t.Fatalf("allPatched == true 时不应受缺 KuGou.exe 影响，实得 %v", err)
			}
		})
	})
}

// TestKillAndPatchPreKillChecksAfterFinalPrechecks 钉死 killAndPatch 在 gateRunningFn 与 resolveLaunchExe
// 最终预检之后、kill() 之前仍须经 preKill（checkReadyToKill）复查 stopCh 与 helperAlive：
// 即便 locate 经 waitUntilRunningReady 已成功返回，若在 gateRunningFn 进程快照或 resolveLaunchExe 磁盘 stat
// 期间提权 helper 退出或收到停止信号，仍必须中止、原样返回错误且 kill/trigger 零调用；仍存活且未停止时正常放行。
//
// 变异自证（实测变红）：注释掉 killAndPatch 中的 preKill 复查循环 → 前三条负向子用例均以
// "calls = [kill trigger]，应为零调用" 变红，第四条顺序子用例亦以缺少 preKill 变红（4/4 全红）。
func TestKillAndPatchPreKillChecksAfterFinalPrechecks(t *testing.T) {
	wantHelperExited := errHelperExited().Error()
	wantStopped := readyToKill(kugouWaitStopped, func() bool { return true }).Error()

	t.Run("gateRunningFn 执行期间 helper 退出 → 返回与 errHelperExited 同构错误且 kill/trigger 零调用", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		launchExe := filepath.Join(dir, TargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")

		stopCh := make(chan struct{})
		alive := true
		gateCalls := 0
		oldGate := gateRunningFn
		t.Cleanup(func() { gateRunningFn = oldGate })
		gateRunningFn = func() error {
			gateCalls++
			alive = false
			return nil
		}

		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return alive }, time.Millisecond, time.Second, func() (string, string, error) {
				return launchExe, lc, nil
			})
		}
		var calls []string
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil },
			func() error { return checkReadyToKill(stopCh, func() bool { return alive }) })

		if gateCalls != 1 {
			t.Fatalf("gateRunningFn 调用 %d 次，应为 1", gateCalls)
		}
		if len(calls) != 0 {
			t.Fatalf("calls = %v，应为零调用", calls)
		}
		if err == nil || err.Error() != wantHelperExited {
			t.Fatalf("err = %v，应与 errHelperExited 同构 (%q)", err, wantHelperExited)
		}
		if errors.Is(err, ErrInstallChanged) {
			t.Fatalf("preKill 失败不得包装成 ErrInstallChanged：%v", err)
		}
	})

	t.Run("resolveLaunchExe（isRegularFileFn）执行期间 helper 退出（HD_KuGou.exe）→ 返回与 errHelperExited 同构错误且 kill/trigger 零调用", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		launchExe := filepath.Join(dir, TargetProcessName)
		hdExe := filepath.Join(dir, HDTargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")

		stubGateRunning(t, nil)
		stopCh := make(chan struct{})
		alive := true
		statCalls := 0
		oldIsRegular := isRegularFileFn
		t.Cleanup(func() { isRegularFileFn = oldIsRegular })
		isRegularFileFn = func(path string) bool {
			statCalls++
			if !samePath(path, launchExe) {
				t.Errorf("isRegularFileFn 收到路径 %q，应为同目录启动器 %q", path, launchExe)
			}
			alive = false
			return true
		}

		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return alive }, time.Millisecond, time.Second, func() (string, string, error) {
				return hdExe, lc, nil
			})
		}
		var calls []string
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil },
			func() error { return checkReadyToKill(stopCh, func() bool { return alive }) })

		if statCalls != 1 {
			t.Fatalf("isRegularFileFn 调用 %d 次，应为 1", statCalls)
		}
		if len(calls) != 0 {
			t.Fatalf("calls = %v，应为零调用", calls)
		}
		if err == nil || err.Error() != wantHelperExited {
			t.Fatalf("err = %v，应与 errHelperExited 同构 (%q)", err, wantHelperExited)
		}
		if errors.Is(err, ErrInstallChanged) {
			t.Fatalf("preKill 失败不得包装成 ErrInstallChanged：%v", err)
		}
	})

	t.Run("gateRunningFn 执行期间 stopCh 关闭 → 返回停止错误且 kill/trigger 零调用", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		launchExe := filepath.Join(dir, TargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")

		stopCh := make(chan struct{})
		gateCalls := 0
		oldGate := gateRunningFn
		t.Cleanup(func() { gateRunningFn = oldGate })
		gateRunningFn = func() error {
			gateCalls++
			close(stopCh)
			return nil
		}

		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, time.Second, func() (string, string, error) {
				return launchExe, lc, nil
			})
		}
		var calls []string
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil },
			func() error { return checkReadyToKill(stopCh, func() bool { return true }) })

		if gateCalls != 1 {
			t.Fatalf("gateRunningFn 调用 %d 次，应为 1", gateCalls)
		}
		if len(calls) != 0 {
			t.Fatalf("calls = %v，应为零调用", calls)
		}
		if err == nil || err.Error() != wantStopped {
			t.Fatalf("err = %v，应与停止错误同构 (%q)", err, wantStopped)
		}
		if errors.Is(err, ErrInstallChanged) {
			t.Fatalf("停止错误不得包装成 ErrInstallChanged：%v", err)
		}
	})

	t.Run("gateRunningFn 与 resolveLaunchExe 后仍存活且未停止 → 按序调用 kill 与 trigger 并返回 nil", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "KGMusic")
		mkInstall(t, dir, "20.1.61.28014")
		launchExe := filepath.Join(dir, TargetProcessName)
		hdExe := filepath.Join(dir, HDTargetProcessName)
		lc := filepath.Join(dir, "20.1.61.28014", "libcef.dll")

		stopCh := make(chan struct{})
		var calls []string
		oldGate, oldIsRegular := gateRunningFn, isRegularFileFn
		t.Cleanup(func() {
			gateRunningFn = oldGate
			isRegularFileFn = oldIsRegular
		})
		gateRunningFn = func() error {
			calls = append(calls, "gate")
			return nil
		}
		isRegularFileFn = func(path string) bool {
			calls = append(calls, "stat")
			return samePath(path, launchExe)
		}

		locate := func() (string, string, error) {
			return waitUntilRunningReady(stopCh, func() bool { return true }, time.Millisecond, time.Second, func() (string, string, error) {
				return hdExe, lc, nil
			})
		}
		err := killAndPatch(launchExe, lc, locate,
			func() { calls = append(calls, "kill") },
			func() error { calls = append(calls, "trigger"); return nil },
			nil,
			func() error {
				calls = append(calls, "preKill")
				return checkReadyToKill(stopCh, func() bool { return true })
			})

		if err != nil {
			t.Fatalf("killAndPatch 失败: %v", err)
		}
		want := []string{"gate", "stat", "preKill", "kill", "trigger"}
		if len(calls) != len(want) {
			t.Fatalf("调用序列 = %v，应为 %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("调用序列 = %v，应为 %v", calls, want)
			}
		}
	})
}
